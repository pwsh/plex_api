package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/pwsh/plex_api/internal/sqlite"
)

const (
	defaultLimit = 100
	maxLimit     = 1000
)

// tableMeta is what a page query needs to know about a table, cached per
// (database, table) and validated against PRAGMA schema_version on every
// request so a PMS migration invalidates it automatically.
type tableMeta struct {
	Type          string
	Cols          []columnInfo
	SchemaVersion string
}

type metaCache struct {
	mu sync.Mutex
	m  map[string]*tableMeta
}

func (c *metaCache) get(key string) *tableMeta {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[key]
}

func (c *metaCache) put(key string, v *tableMeta) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]*tableMeta{}
	}
	c.m[key] = v
}

// loadTableMeta fetches sqlite_master type, table_info and schema_version in
// one round trip.
func (s *Server) loadTableMeta(ctx context.Context, db, table string) (*tableMeta, error) {
	res, err := query(ctx, s.drv, db,
		sqlite.Statement{
			SQL:    "SELECT type FROM sqlite_master WHERE name = :name",
			Params: map[string]any{"name": table},
		},
		sqlite.Statement{SQL: "PRAGMA table_info(" + sqlite.QuoteIdent(table) + ")"},
		sqlite.Statement{SQL: "PRAGMA schema_version"},
	)
	if err != nil {
		return nil, err
	}
	if len(res[0].Rows) == 0 || len(res[1].Rows) == 0 {
		return nil, errNotFound
	}
	m := &tableMeta{Type: asString(res[0].Rows[0]["type"])}
	for _, row := range res[1].Rows {
		m.Cols = append(m.Cols, columnInfo{Name: asString(row["name"]), Type: asString(row["type"])})
	}
	if len(res[2].Rows) == 1 {
		m.SchemaVersion = asString(res[2].Rows[0]["schema_version"])
	}
	return m, nil
}

// handleTable pages through one table.
//
// Rows are passed through from the shell's JSON untouched. Blob columns are
// selected as base64 in SQL (the shell's base64() emits MIME line breaks,
// which replace() strips) because the shell's JSON writer escapes raw bytes as
// lossy \uXXXX sequences.
//
// Table metadata is cached; the page script starts with PRAGMA schema_version
// and the page is retried with fresh metadata if the version moved.
func (s *Server) handleTable(w http.ResponseWriter, r *http.Request) {
	db, err := s.dbPath(r.URL.Query().Get("db"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	table := r.PathValue("table")

	limit, err := intParam(r, "limit", defaultLimit, 0, maxLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	offset, err := intParam(r, "offset", 0, 0, 1<<31)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.QueryTimeout)
	defer cancel()

	key := db + "\x00" + table
	meta := s.meta.get(key)
	if meta == nil {
		meta, err = s.loadTableMeta(ctx, db, table)
		if errors.Is(err, errNotFound) {
			writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no such table %q", table))
			return
		}
		if err != nil {
			writeSQLError(w, err)
			return
		}
		s.meta.put(key, meta)
	}
	if meta.Type != "table" && meta.Type != "view" {
		writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("%q is a %s, not a table", table, meta.Type))
		return
	}

	for attempt := 0; attempt < 2; attempt++ {
		orderBy, err := orderClause(r.URL.Query().Get("order"), table, meta.Cols)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}

		selects := make([]string, 0, len(meta.Cols))
		blobCols := []string{}
		for _, c := range meta.Cols {
			q := sqlite.QuoteIdent(c.Name)
			if isBlobType(c.Type) {
				blobCols = append(blobCols, c.Name)
				selects = append(selects, "replace(base64("+q+"), char(10), '') AS "+q)
				continue
			}
			selects = append(selects, q)
		}
		sql := "SELECT " + strings.Join(selects, ", ") + " FROM " + sqlite.QuoteIdent(table) +
			orderBy + " LIMIT :limit OFFSET :offset"

		res, err := s.drv.QueryRaw(ctx, db, []sqlite.Statement{
			{SQL: "PRAGMA schema_version"},
			{SQL: sql, Params: map[string]any{"limit": limit, "offset": offset}},
		})
		if err != nil {
			writeSQLError(w, err)
			return
		}
		if err := res[0].Decode(); err != nil {
			writeSQLError(w, err)
			return
		}
		version := ""
		if len(res[0].Rows) == 1 {
			version = asString(res[0].Rows[0]["schema_version"])
		}
		if version != meta.SchemaVersion && attempt == 0 {
			// A migration ran since the metadata was cached: reload and retry.
			meta, err = s.loadTableMeta(ctx, db, table)
			if errors.Is(err, errNotFound) {
				writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no such table %q", table))
				return
			}
			if err != nil {
				writeSQLError(w, err)
				return
			}
			s.meta.put(key, meta)
			continue
		}

		columnNames := make([]string, len(meta.Cols))
		for i, c := range meta.Cols {
			columnNames[i] = c.Name
		}
		writeFields(w, http.StatusOK, []field{
			{Key: "table", Value: table},
			{Key: "columns", Value: columnNames},
			{Key: "blob_columns", Value: blobCols},
			{Key: "limit", Value: limit},
			{Key: "offset", Value: offset},
			{Key: "count", Value: res[1].Count()},
			{Key: "rows", Raw: res[1].JSON()},
		})
		return
	}
}

// orderClause validates the ?order= parameter against the table's real columns
// and returns an ORDER BY fragment. A leading "-" means descending.
//
// With no explicit order, metadata_items is sorted the way PMS sorts library
// listings: ORDER BY title_sort COLLATE icu_root, which only the Plex engine
// can evaluate (index_title_sort_icu covers it).
func orderClause(order, table string, cols []columnInfo) (string, error) {
	if order == "" {
		if table == "metadata_items" {
			return " ORDER BY title_sort COLLATE icu_root", nil
		}
		return "", nil
	}
	desc := false
	if strings.HasPrefix(order, "-") {
		desc = true
		order = order[1:]
	}
	for _, c := range cols {
		if c.Name == order {
			clause := " ORDER BY " + sqlite.QuoteIdent(c.Name)
			if table == "metadata_items" && (c.Name == "title_sort" || c.Name == "title") {
				clause += " COLLATE icu_root"
			}
			if desc {
				clause += " DESC"
			}
			return clause, nil
		}
	}
	return "", fmt.Errorf("unknown column %q for table %q", order, table)
}
