package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/pwsh/plex_api/internal/sqlite"
)

// lookupObject returns the sqlite_master row for name, or an error if no such
// object exists. This is the only place a client-supplied identifier becomes
// part of a SQL string, so every table-name parameter must pass through it.
func (s *Server) lookupObject(ctx context.Context, db, name string) (map[string]any, error) {
	res, err := query(ctx, s.drv, db, sqlite.Statement{
		SQL:    "SELECT name, type, sql FROM sqlite_master WHERE name = :name",
		Params: map[string]any{"name": name},
	})
	if err != nil {
		return nil, err
	}
	if len(res) == 0 || len(res[0].Rows) == 0 {
		return nil, errNotFound
	}
	return res[0].Rows[0], nil
}

var errNotFound = errors.New("not found")

type schemaObject struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	TblName string `json:"tbl_name,omitempty"`
	SQL     string `json:"sql,omitempty"`
}

// handleSchema lists every object in sqlite_master, grouped by type.
func (s *Server) handleSchema(w http.ResponseWriter, r *http.Request) {
	db, err := s.dbPath(r.URL.Query().Get("db"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.QueryTimeout)
	defer cancel()
	res, err := query(ctx, s.drv, db, sqlite.Statement{
		SQL: `SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY type, name`,
	})
	if err != nil {
		writeSQLError(w, err)
		return
	}
	out := map[string][]schemaObject{"table": {}, "view": {}, "trigger": {}, "index": {}}
	if len(res) > 0 {
		for _, row := range res[0].Rows {
			o := schemaObject{
				Name:    asString(row["name"]),
				Type:    asString(row["type"]),
				TblName: asString(row["tbl_name"]),
				SQL:     asString(row["sql"]),
			}
			out[o.Type] = append(out[o.Type], o)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tables":   out["table"],
		"views":    out["view"],
		"triggers": out["trigger"],
		"indexes":  out["index"],
	})
}

// handleSchemaTable returns PRAGMA table_info, the indexes and the CREATE
// statement for one table or view.
func (s *Server) handleSchemaTable(w http.ResponseWriter, r *http.Request) {
	db, err := s.dbPath(r.URL.Query().Get("db"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	name := r.PathValue("table")
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.QueryTimeout)
	defer cancel()

	obj, err := s.lookupObject(ctx, db, name)
	if errors.Is(err, errNotFound) {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no such object %q", name))
		return
	}
	if err != nil {
		writeSQLError(w, err)
		return
	}

	res, err := query(ctx, s.drv, db,
		// PRAGMA does not take bound parameters, hence the validated,
		// quoted identifier.
		sqlite.Statement{SQL: "PRAGMA table_info(" + sqlite.QuoteIdent(name) + ")"},
		sqlite.Statement{
			SQL:    "SELECT name, sql FROM sqlite_master WHERE type = 'index' AND tbl_name = :t ORDER BY name",
			Params: map[string]any{"t": name},
		},
		sqlite.Statement{
			SQL:    "SELECT name, sql FROM sqlite_master WHERE type = 'trigger' AND tbl_name = :t ORDER BY name",
			Params: map[string]any{"t": name},
		},
	)
	if err != nil {
		writeSQLError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":     asString(obj["name"]),
		"type":     asString(obj["type"]),
		"sql":      asString(obj["sql"]),
		"columns":  res[0].Rows,
		"indexes":  res[1].Rows,
		"triggers": res[2].Rows,
	})
}

// columnInfo is one row of PRAGMA table_info that we care about.
type columnInfo struct {
	Name string
	Type string
}

func (s *Server) tableColumns(ctx context.Context, db, table string) ([]columnInfo, error) {
	res, err := query(ctx, s.drv, db, sqlite.Statement{
		SQL: "PRAGMA table_info(" + sqlite.QuoteIdent(table) + ")",
	})
	if err != nil {
		return nil, err
	}
	if len(res) == 0 || len(res[0].Rows) == 0 {
		return nil, errNotFound
	}
	cols := make([]columnInfo, 0, len(res[0].Rows))
	for _, row := range res[0].Rows {
		cols = append(cols, columnInfo{Name: asString(row["name"]), Type: asString(row["type"])})
	}
	return cols, nil
}
