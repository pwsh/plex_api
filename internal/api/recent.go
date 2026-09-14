package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/pwsh/plex_api/internal/sqlite"
)

// handleRecent returns the most recently added items of the requested
// metadata types, newest first.
//
// A single "WHERE metadata_type IN (...) ORDER BY added_at DESC" cannot walk
// one index, so SQLite scans every row of those types and sorts them (about
// 8 ms on 116k rows). Instead each type gets its own ordered, limited branch,
// which is an index walk on (metadata_type, added_at) or, with a section
// filter, on Plex's own (library_section_id, metadata_type, added_at) index.
// The branches are merged and cut to the limit: 0.35 ms on the same data.
func (s *Server) handleRecent(w http.ResponseWriter, r *http.Request) {
	limit, err := intParam(r, "limit", 20, 1, maxLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	types, err := intList(r.URL.Query().Get("types"), []int64{1, 2})
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "types must be a comma-separated list of metadata_type codes")
		return
	}
	if len(types) > 16 {
		writeError(w, http.StatusBadRequest, "bad_request", "at most 16 types")
		return
	}
	params := map[string]any{"limit": limit}
	sectionClause := ""
	if sec := r.URL.Query().Get("section"); sec != "" {
		id, err := strconv.ParseInt(sec, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "section must be an integer")
			return
		}
		params["section"] = id
		sectionClause = " AND library_section_id = :section"
	}

	const cols = "id, library_section_id, metadata_type, parent_id, title, title_sort, year, guid, added_at, originally_available_at"
	branches := make([]string, 0, len(types))
	for i, t := range types {
		name := "t" + strconv.Itoa(i)
		params[name] = t
		branches = append(branches, "SELECT * FROM (SELECT "+cols+" FROM metadata_items WHERE metadata_type = :"+name+
			sectionClause+" ORDER BY added_at DESC LIMIT :limit)")
	}
	sql := "SELECT " + cols + " FROM (" + strings.Join(branches, " UNION ALL ") + ") ORDER BY added_at DESC, id DESC LIMIT :limit"

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.QueryTimeout)
	defer cancel()
	res, err := s.drv.QueryRaw(ctx, s.cfg.MainDB(), []sqlite.Statement{{SQL: sql, Params: params}})
	if err != nil {
		writeSQLError(w, err)
		return
	}
	writeFields(w, http.StatusOK, []field{
		{Key: "types", Value: types},
		{Key: "limit", Value: limit},
		{Key: "count", Value: res[0].Count()},
		{Key: "results", Raw: res[0].JSON()},
	})
}

// intList parses "1,2,4" into integers, returning def for an empty string.
func intList(s string, def []int64) ([]int64, error) {
	if strings.TrimSpace(s) == "" {
		return def, nil
	}
	var out []int64
	for _, part := range strings.Split(s, ",") {
		n, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("bad integer %q", part)
		}
		out = append(out, n)
	}
	return out, nil
}
