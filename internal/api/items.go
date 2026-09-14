package api

import (
	"context"
	"net/http"
	"strconv"

	"github.com/pwsh/plex_api/internal/sqlite"
)

// handleItem returns one metadata_items row with everything hanging off it.
// The joins follow docs/PlexDatabaseLayout.md section 8; all of them run in a
// single shell subprocess.
func (s *Server) handleItem(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "item id must be an integer")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.QueryTimeout)
	defer cancel()

	p := map[string]any{"id": id}
	res, err := s.drv.QueryRaw(ctx, s.cfg.MainDB(), []sqlite.Statement{
		sqlite.Statement{SQL: `SELECT * FROM metadata_items WHERE id = :id`, Params: p},
		sqlite.Statement{SQL: `SELECT * FROM media_items WHERE metadata_item_id = :id ORDER BY id`, Params: p},
		sqlite.Statement{SQL: `SELECT p.* FROM media_parts p
			JOIN media_items mi ON p.media_item_id = mi.id
			WHERE mi.metadata_item_id = :id ORDER BY p.media_item_id, p.id`, Params: p},
		sqlite.Statement{SQL: `SELECT st.* FROM media_streams st
			JOIN media_parts p ON st.media_part_id = p.id
			JOIN media_items mi ON p.media_item_id = mi.id
			WHERE mi.metadata_item_id = :id ORDER BY st.media_part_id, st.stream_type_id, st."index"`, Params: p},
		sqlite.Statement{SQL: `SELECT tg.id AS tagging_id, tg.tag_id, t.tag_type, t.tag, t."key" AS tag_key,
			tg.text, tg."index", tg.time_offset, tg.end_time_offset, tg.thumb_url, tg.extra_data
			FROM taggings tg JOIN tags t ON tg.tag_id = t.id
			WHERE tg.metadata_item_id = :id
			ORDER BY t.tag_type, tg."index", tg.id`, Params: p},
		sqlite.Statement{SQL: `SELECT ms.* FROM metadata_item_settings ms
			JOIN metadata_items m ON m.guid = ms.guid
			WHERE m.id = :id ORDER BY ms.account_id`, Params: p},
		sqlite.Statement{SQL: `SELECT r.relation_type, r.related_metadata_item_id, e.title, e.metadata_type, e.guid
			FROM metadata_relations r JOIN metadata_items e ON e.id = r.related_metadata_item_id
			WHERE r.metadata_item_id = :id ORDER BY r.id`, Params: p},
	})
	if err != nil {
		writeSQLError(w, err)
		return
	}
	items, err := res[0].Elements()
	if err != nil {
		writeSQLError(w, err)
		return
	}
	if len(items) == 0 {
		writeError(w, http.StatusNotFound, "not_found", "no metadata_items row with id "+strconv.FormatInt(id, 10))
		return
	}
	writeFields(w, http.StatusOK, []field{
		{Key: "item", Raw: items[0]},
		{Key: "media_items", Raw: res[1].JSON()},
		{Key: "media_parts", Raw: res[2].JSON()},
		{Key: "media_streams", Raw: res[3].JSON()},
		{Key: "taggings", Raw: res[4].JSON()},
		{Key: "settings", Raw: res[5].JSON()},
		{Key: "extras", Raw: res[6].JSON()},
	})
}

func rowsOf(r sqlite.Result) []map[string]any {
	if r.Rows == nil {
		return []map[string]any{}
	}
	return r.Rows
}
