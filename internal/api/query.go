package api

import (
	"context"
	"net/http"

	"github.com/pwsh/plex_api/internal/sqlite"
)

type queryRequest struct {
	SQL    string         `json:"sql"`
	Params map[string]any `json:"params"`
	DB     string         `json:"db"`
}

// handleQuery runs arbitrary read-only SQL.
//
// The shell is started with -readonly, so read-only-ness is enforced by SQLite
// itself rather than by inspecting the SQL. Multiple statements in one body are
// allowed; when the shell reports more than one result set the response carries
// "results" (an array of row arrays) instead of "rows".
func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	var req queryRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.SQL == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "sql is required")
		return
	}
	db, err := s.dbPath(req.DB)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.QueryTimeout)
	defer cancel()

	res, err := s.drv.QueryAllRaw(ctx, db, sqlite.Statement{SQL: req.SQL, Params: req.Params})
	if err != nil {
		writeSQLError(w, err)
		return
	}
	if len(res) == 1 {
		writeFields(w, http.StatusOK, []field{
			{Key: "rows", Raw: res[0].JSON()},
			{Key: "count", Value: res[0].Count()},
			{Key: "columns", Value: res[0].ColumnNames()},
		})
		return
	}
	sets := make([]map[string]any, 0, len(res))
	for _, rr := range res {
		sets = append(sets, map[string]any{"rows": rr.JSON(), "count": rr.Count(), "columns": rr.ColumnNames()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": sets})
}

type execRequest struct {
	Statements []sqlite.Statement `json:"statements"`
	DB         string             `json:"db"`
}

// handleExec runs write statements in one BEGIN IMMEDIATE transaction.
// A "SELECT changes()" is appended after each statement, so every statement
// reports its own row count; -bail means the first failure aborts the shell
// before COMMIT and nothing is written.
func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	var req execRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if len(req.Statements) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "statements is required and must not be empty")
		return
	}
	db, err := s.dbPath(req.DB)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if !s.checkWritePolicy(w) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.QueryTimeout)
	defer cancel()

	res, err := s.drv.Exec(ctx, db, req.Statements)
	if err != nil {
		writeSQLError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(req.Statements))
	var total int64
	for i := range req.Statements {
		entry := map[string]any{"index": i}
		if 2*i < len(res) {
			if rows := rowsOf(res[2*i]); len(rows) > 0 {
				entry["rows"] = rows
			}
		}
		if 2*i+1 < len(res) {
			if rows := res[2*i+1].Rows; len(rows) == 1 {
				n := asInt(rows[0]["changes"])
				entry["changes"] = n
				total += n
			}
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"statements":    out,
		"total_changes": total,
		"committed":     true,
	})
}
