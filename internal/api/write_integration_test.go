package api_test

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pwsh/plex_api/internal/config"
)

// writeCfg copies the read-only test database into a temporary directory and
// returns a config that allows writes against the copy. The original is never
// touched.
func writeCfg(t *testing.T) *config.Config {
	pmsDir, dbPath := testEnv(t)
	poolSize, poolMaxUses := testPool(t)
	dir := t.TempDir()
	dst := filepath.Join(dir, config.MainDBName)
	copyFile(t, dbPath, dst)
	// A journal left beside the original would be stale next to the copy, so
	// only the database file itself is copied; both test databases are in
	// delete journal mode with no live journal.
	return &config.Config{
		PMSDir:            pmsDir,
		DBDir:             dir,
		PMSAddr:           "127.0.0.1:1",
		AllowWrite:        true,
		WriteWhileRunning: true,
		BusyTimeoutMS:     5000,
		QueryTimeout:      60 * time.Second,
		PoolSize:          poolSize,
		PoolMaxUses:       poolMaxUses,
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(out, in)
	if err != nil {
		out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("copied %s -> %s (%d bytes)", src, dst, n)
}

// TestIntegrationExecRollback checks that a failing statement inside a
// multi-statement /v1/exec leaves nothing behind: -bail stops the shell before
// COMMIT, so the BEGIN IMMEDIATE transaction is never committed.
func TestIntegrationExecRollback(t *testing.T) {
	srv := newTestServer(t, writeCfg(t))

	pick := doJSON(t, srv, http.MethodPost, "/v1/query", map[string]any{
		"sql": "SELECT id, summary FROM metadata_items WHERE metadata_type = 1 LIMIT 1",
	}, http.StatusOK)
	rows, _ := pick["rows"].([]any)
	if len(rows) == 0 {
		t.Skip("no movies in the test database")
	}
	row := rows[0].(map[string]any)
	id := int64(row["id"].(float64))
	before := row["summary"]

	// First statement succeeds, second fails. Nothing may be committed.
	fail := doJSON(t, srv, http.MethodPost, "/v1/exec", map[string]any{
		"statements": []any{
			map[string]any{
				"sql":    "UPDATE metadata_items SET summary = :s WHERE id = :id",
				"params": map[string]any{"s": "plex-api rollback probe", "id": id},
			},
			map[string]any{"sql": "UPDATE no_such_table SET x = 1"},
		},
	}, http.StatusBadRequest)
	if e, _ := fail["error"].(map[string]any); e == nil || e["code"] != "sql_error" {
		t.Errorf("error body = %v", fail)
	}

	after := doJSON(t, srv, http.MethodPost, "/v1/query", map[string]any{
		"sql":    "SELECT summary FROM metadata_items WHERE id = :id",
		"params": map[string]any{"id": id},
	}, http.StatusOK)
	got := after["rows"].([]any)[0].(map[string]any)["summary"]
	if got != before {
		t.Fatalf("rollback failed: summary is %#v, want the original %#v", got, before)
	}

	// The same first statement on its own does commit.
	ok := doJSON(t, srv, http.MethodPost, "/v1/exec", map[string]any{
		"statements": []any{
			map[string]any{
				"sql":    "UPDATE metadata_items SET summary = :s WHERE id = :id",
				"params": map[string]any{"s": "plex-api commit probe", "id": id},
			},
		},
	}, http.StatusOK)
	if ok["total_changes"].(float64) != 1 {
		t.Errorf("total_changes = %v, want 1", ok["total_changes"])
	}
	stmts := ok["statements"].([]any)
	if stmts[0].(map[string]any)["changes"].(float64) != 1 {
		t.Errorf("per-statement changes = %v", stmts[0])
	}

	after = doJSON(t, srv, http.MethodPost, "/v1/query", map[string]any{
		"sql":    "SELECT summary FROM metadata_items WHERE id = :id",
		"params": map[string]any{"id": id},
	}, http.StatusOK)
	if got := after["rows"].([]any)[0].(map[string]any)["summary"]; got != "plex-api commit probe" {
		t.Fatalf("commit failed: summary is %#v", got)
	}
}

// TestIntegrationSettingsPatch exercises both the update and the insert path of
// PATCH /v1/settings/{guid}.
func TestIntegrationSettingsPatch(t *testing.T) {
	srv := newTestServer(t, writeCfg(t))

	pick := doJSON(t, srv, http.MethodPost, "/v1/query", map[string]any{
		"sql": `SELECT s.guid, s.account_id, s.view_count FROM metadata_item_settings s LIMIT 1`,
	}, http.StatusOK)
	rows, _ := pick["rows"].([]any)
	if len(rows) == 0 {
		t.Skip("no metadata_item_settings rows in the test database")
	}
	existing := rows[0].(map[string]any)
	guid := existing["guid"].(string)
	account := int64(existing["account_id"].(float64))

	// Update path.
	out := doJSON(t, srv, http.MethodPatch, "/v1/settings/"+pathEscape(guid), map[string]any{
		"account_id":     account,
		"view_count":     7,
		"view_offset":    12345,
		"last_viewed_at": 1700000000,
		"rating":         8.5,
	}, http.StatusOK)
	if out["updated"].(float64) != 1 || out["inserted"].(float64) != 0 {
		t.Fatalf("update path: updated=%v inserted=%v", out["updated"], out["inserted"])
	}
	got := out["settings"].(map[string]any)
	if got["view_count"].(float64) != 7 || got["view_offset"].(float64) != 12345 ||
		got["last_viewed_at"].(float64) != 1700000000 || got["rating"].(float64) != 8.5 {
		t.Errorf("settings after update = %v", got)
	}

	// Insert path: a guid that has no settings row for a fresh account.
	newGUID := "plex://movie/plex-api-test-" + t.Name()
	out = doJSON(t, srv, http.MethodPatch, "/v1/settings/"+pathEscape(newGUID), map[string]any{
		"account_id": 4242,
		"view_count": 3,
	}, http.StatusOK)
	if out["updated"].(float64) != 0 || out["inserted"].(float64) != 1 {
		t.Fatalf("insert path: updated=%v inserted=%v", out["updated"], out["inserted"])
	}

	read := getJSON(t, srv, "/v1/settings/"+pathEscape(newGUID), http.StatusOK)
	settings := read["settings"].([]any)
	if len(settings) != 1 {
		t.Fatalf("expected one settings row, got %d", len(settings))
	}
	if settings[0].(map[string]any)["view_count"].(float64) != 3 {
		t.Errorf("settings = %v", settings[0])
	}

	// A second PATCH must update, not insert again.
	out = doJSON(t, srv, http.MethodPatch, "/v1/settings/"+pathEscape(newGUID), map[string]any{
		"account_id": 4242,
		"view_count": 4,
	}, http.StatusOK)
	if out["updated"].(float64) != 1 || out["inserted"].(float64) != 0 {
		t.Fatalf("second patch: updated=%v inserted=%v", out["updated"], out["inserted"])
	}

	// An empty body is a 400.
	doJSON(t, srv, http.MethodPatch, "/v1/settings/"+pathEscape(newGUID), map[string]any{}, http.StatusBadRequest)
	// Unknown fields are rejected rather than silently ignored.
	doJSON(t, srv, http.MethodPatch, "/v1/settings/"+pathEscape(newGUID),
		map[string]any{"summary": "nope"}, http.StatusBadRequest)
	// A guid nobody has ever seen has no settings and no item.
	getJSON(t, srv, "/v1/settings/"+pathEscape("plex://movie/definitely-not-here"), http.StatusNotFound)
}

// TestIntegrationWriteThroughPlexEngine writes to a table that stock SQLite
// cannot touch: metadata_items.title fires the FTS trigger that needs Plex's
// "collating" tokenizer.
func TestIntegrationWriteThroughPlexEngine(t *testing.T) {
	srv := newTestServer(t, writeCfg(t))

	pick := doJSON(t, srv, http.MethodPost, "/v1/query", map[string]any{
		"sql": "SELECT id, title FROM metadata_items WHERE metadata_type = 1 LIMIT 1",
	}, http.StatusOK)
	rows, _ := pick["rows"].([]any)
	if len(rows) == 0 {
		t.Skip("no movies in the test database")
	}
	id := int64(rows[0].(map[string]any)["id"].(float64))

	out := doJSON(t, srv, http.MethodPost, "/v1/exec", map[string]any{
		"statements": []any{map[string]any{
			"sql":    "UPDATE metadata_items SET title = :t WHERE id = :id",
			"params": map[string]any{"t": "Plex API Probe: it's \"quoted\"", "id": id},
		}},
	}, http.StatusOK)
	if out["total_changes"].(float64) != 1 {
		t.Fatalf("total_changes = %v", out["total_changes"])
	}

	// The FTS trigger copies the new title verbatim, so the row is findable
	// but without the trailing space / show-title suffix PMS would write.
	got := doJSON(t, srv, http.MethodPost, "/v1/query", map[string]any{
		"sql":    "SELECT title FROM metadata_items WHERE id = :id",
		"params": map[string]any{"id": id},
	}, http.StatusOK)
	if title := got["rows"].([]any)[0].(map[string]any)["title"]; title != "Plex API Probe: it's \"quoted\"" {
		t.Fatalf("title = %#v", title)
	}
	found := getJSON(t, srv, "/v1/search?q=Probe&limit=20", http.StatusOK)
	results := found["results"].([]any)
	hit := false
	for _, r := range results {
		if int64(r.(map[string]any)["id"].(float64)) == id {
			hit = true
			if fts := r.(map[string]any)["fts_title"]; fts != "Plex API Probe: it's \"quoted\"" {
				t.Errorf("fts_title = %#v (triggers copy the column verbatim)", fts)
			}
		}
	}
	if !hit {
		t.Errorf("item %d not found by FTS after the title change", id)
	}
}
