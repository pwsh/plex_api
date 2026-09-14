package api

import (
	"context"
	"net/http"

	"github.com/pwsh/plex_api/internal/sqlite"
)

// handleSearch runs a full-text search over item titles.
//
// fts4_metadata_titles_icu uses Plex's "collating" tokenizer, which exists only
// inside the PMS binary, so this query works through the Plex shell and nowhere
// else. The MATCH operand must name the virtual table itself, so the FTS scan
// is a subquery that is then joined to metadata_items.
//
// Caveat: PMS stores the title with a trailing space, and for episodes it
// appends the show title to both title and title_sort ("Tape 1, Side A 13
// Reasons Why"). The indexed text is therefore returned alongside the real
// metadata_items title as fts_title / fts_title_sort.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "q is required")
		return
	}
	limit, err := intParam(r, "limit", 50, 1, maxLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.QueryTimeout)
	defer cancel()

	res, err := s.drv.QueryRaw(ctx, s.cfg.MainDB(), []sqlite.Statement{{
		SQL: `SELECT f.docid AS docid, f.title AS fts_title, f.title_sort AS fts_title_sort,
			m.id, m.metadata_type, m.library_section_id, m.year, m.guid,
			m.title, m.title_sort, m.parent_id
		FROM (SELECT docid, title, title_sort FROM fts4_metadata_titles_icu
		      WHERE fts4_metadata_titles_icu MATCH :q LIMIT :limit) f
		JOIN metadata_items m ON m.id = f.docid`,
		Params: map[string]any{"q": q, "limit": limit},
	}})
	if err != nil {
		writeSQLError(w, err)
		return
	}
	writeFields(w, http.StatusOK, []field{
		{Key: "query", Value: q},
		{Key: "limit", Value: limit},
		{Key: "count", Value: res[0].Count()},
		{Key: "results", Raw: res[0].JSON()},
	})
}

// handleTagSearch is the same thing against fts4_tag_titles_icu, which covers
// tag types 0, 1, 2, 4, 6, 207 and 400 (genres, collections, directors, actors).
func (s *Server) handleTagSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "q is required")
		return
	}
	limit, err := intParam(r, "limit", 50, 1, maxLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.QueryTimeout)
	defer cancel()

	res, err := s.drv.QueryRaw(ctx, s.cfg.MainDB(), []sqlite.Statement{{
		SQL: `SELECT f.docid AS docid, f.tag AS fts_tag,
			t.id, t.tag_type, t.tag, t."key" AS tag_key
		FROM (SELECT docid, tag FROM fts4_tag_titles_icu
		      WHERE fts4_tag_titles_icu MATCH :q LIMIT :limit) f
		JOIN tags t ON t.id = f.docid`,
		Params: map[string]any{"q": q, "limit": limit},
	}})
	if err != nil {
		writeSQLError(w, err)
		return
	}
	writeFields(w, http.StatusOK, []field{
		{Key: "query", Value: q},
		{Key: "limit", Value: limit},
		{Key: "count", Value: res[0].Count()},
		{Key: "results", Raw: res[0].JSON()},
	})
}
