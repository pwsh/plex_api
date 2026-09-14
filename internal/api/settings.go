package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/pwsh/plex_api/internal/sqlite"
)

// settingsColumns are the metadata_item_settings fields a PATCH may set.
// The watch-state tables carry no triggers and no ICU index, so these are the
// safest writes in the database.
var settingsColumns = []string{"view_count", "view_offset", "last_viewed_at", "rating"}

// handleGetSettings returns the metadata_item_settings rows for one guid.
// The per-user tables are keyed by guid rather than by id, so they survive a
// re-match or a library rebuild (docs/PlexDatabaseLayout.md section 4).
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	guid := r.PathValue("guid")
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.QueryTimeout)
	defer cancel()

	res, err := query(ctx, s.drv, s.cfg.MainDB(),
		sqlite.Statement{
			SQL:    `SELECT * FROM metadata_item_settings WHERE guid = :guid ORDER BY account_id`,
			Params: map[string]any{"guid": guid},
		},
		sqlite.Statement{
			SQL:    `SELECT id, title, metadata_type, library_section_id FROM metadata_items WHERE guid = :guid ORDER BY id`,
			Params: map[string]any{"guid": guid},
		},
	)
	if err != nil {
		writeSQLError(w, err)
		return
	}
	rows := rowsOf(res[0])
	if len(rows) == 0 && len(rowsOf(res[1])) == 0 {
		writeError(w, http.StatusNotFound, "not_found", "no settings and no metadata_items row for guid "+guid)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"guid":     guid,
		"settings": rows,
		"items":    rowsOf(res[1]),
	})
}

type patchSettingsRequest struct {
	AccountID    *json.Number `json:"account_id"`
	ViewCount    *json.Number `json:"view_count"`
	ViewOffset   *json.Number `json:"view_offset"`
	LastViewedAt *json.Number `json:"last_viewed_at"`
	Rating       *json.Number `json:"rating"`
}

// handlePatchSettings updates one metadata_item_settings row, inserting it if
// it does not exist yet. Both statements run inside the same BEGIN IMMEDIATE
// transaction, so the UPDATE-then-conditional-INSERT pair is atomic (there is
// no UNIQUE index on (account_id, guid) to hang ON CONFLICT off).
func (s *Server) handlePatchSettings(w http.ResponseWriter, r *http.Request) {
	guid := r.PathValue("guid")
	var req patchSettingsRequest
	if !decodeBody(w, r, &req) {
		return
	}

	fields := map[string]any{}
	if req.ViewCount != nil {
		fields["view_count"] = *req.ViewCount
	}
	if req.ViewOffset != nil {
		fields["view_offset"] = *req.ViewOffset
	}
	if req.LastViewedAt != nil {
		fields["last_viewed_at"] = *req.LastViewedAt
	}
	if req.Rating != nil {
		fields["rating"] = *req.Rating
	}
	if len(fields) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request",
			"set at least one of "+strings.Join(settingsColumns, ", "))
		return
	}
	accountID := json.Number("1")
	if req.AccountID != nil {
		accountID = *req.AccountID
	}
	if _, err := accountID.Int64(); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "account_id must be an integer")
		return
	}

	if !s.checkWritePolicy(w) {
		return
	}

	now := time.Now().Unix()
	params := map[string]any{"guid": guid, "account_id": accountID, "now": now}

	var sets, insCols, insVals []string
	for _, c := range settingsColumns {
		v, ok := fields[c]
		if !ok {
			continue
		}
		params[c] = v
		sets = append(sets, fmt.Sprintf("%s = :%s", sqlite.QuoteIdent(c), c))
		insCols = append(insCols, sqlite.QuoteIdent(c))
		insVals = append(insVals, ":"+c)
	}

	updateSQL := `UPDATE metadata_item_settings SET ` + strings.Join(sets, ", ") +
		`, updated_at = :now, changed_at = :now WHERE guid = :guid AND account_id = :account_id`
	insertSQL := `INSERT INTO metadata_item_settings (account_id, guid, ` +
		strings.Join(insCols, ", ") + `, created_at, updated_at, changed_at) ` +
		`SELECT :account_id, :guid, ` + strings.Join(insVals, ", ") + `, :now, :now, :now ` +
		`WHERE NOT EXISTS (SELECT 1 FROM metadata_item_settings WHERE guid = :guid AND account_id = :account_id)`

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.QueryTimeout)
	defer cancel()

	res, err := s.drv.Exec(ctx, s.cfg.MainDB(),
		[]sqlite.Statement{
			{SQL: updateSQL, Params: params},
			{SQL: insertSQL, Params: params},
		})
	if err != nil {
		writeSQLError(w, err)
		return
	}
	updated := changesOf(res, 0)
	inserted := changesOf(res, 1)

	after, err := query(ctx, s.drv, s.cfg.MainDB(), sqlite.Statement{
		SQL:    `SELECT * FROM metadata_item_settings WHERE guid = :guid AND account_id = :account_id`,
		Params: map[string]any{"guid": guid, "account_id": accountID},
	})
	if err != nil {
		writeSQLError(w, err)
		return
	}
	var row any
	if rows := rowsOf(after[0]); len(rows) > 0 {
		row = rows[0]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"guid":       guid,
		"account_id": accountID,
		"updated":    updated,
		"inserted":   inserted,
		"settings":   row,
	})
}

// changesOf reads the changes() result that Driver.Exec appends after
// statement i.
func changesOf(res []sqlite.Result, i int) int64 {
	if 2*i+1 >= len(res) {
		return 0
	}
	rows := res[2*i+1].Rows
	if len(rows) != 1 {
		return 0
	}
	return asInt(rows[0]["changes"])
}
