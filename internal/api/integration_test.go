package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pwsh/plex_api/internal/api"
	"github.com/pwsh/plex_api/internal/config"
)

// These tests need a real PMS install and a real library database:
//
//	PLEX_API_TEST_PMS_DIR=/usr/lib/plexmediaserver
//	PLEX_API_TEST_DB=/path/to/com.plexapp.plugins.library.db
//
// The read tests never write to PLEX_API_TEST_DB. The write test copies it to
// a temporary directory first.
func testEnv(t *testing.T) (pmsDir, dbPath string) {
	t.Helper()
	pmsDir = os.Getenv("PLEX_API_TEST_PMS_DIR")
	dbPath = os.Getenv("PLEX_API_TEST_DB")
	if pmsDir == "" || dbPath == "" {
		t.Skip("set PLEX_API_TEST_PMS_DIR and PLEX_API_TEST_DB to run integration tests")
	}
	if _, err := os.Stat(filepath.Join(pmsDir, "Plex SQLite")); err != nil {
		t.Skipf("no Plex SQLite in %s: %v", pmsDir, err)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Skipf("no database at %s: %v", dbPath, err)
	}
	return pmsDir, dbPath
}

func newTestServer(t *testing.T, cfg *config.Config) *httptest.Server {
	t.Helper()
	apiSrv := api.New(cfg, "test")
	srv := httptest.NewServer(apiSrv.Handler())
	t.Cleanup(func() {
		srv.Close()
		apiSrv.Close()
	})
	return srv
}

// testPool reads the pool settings from the environment so the whole suite can
// be run both pooled (the default) and with PLEX_API_POOL=0.
func testPool(t *testing.T) (size, maxUses int) {
	t.Helper()
	size, maxUses = config.DefaultPoolSize, config.DefaultPoolMaxUses
	if v := os.Getenv("PLEX_API_POOL"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("PLEX_API_POOL=%q: %v", v, err)
		}
		size = n
	}
	if v := os.Getenv("PLEX_API_POOL_MAX_USES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("PLEX_API_POOL_MAX_USES=%q: %v", v, err)
		}
		maxUses = n
	}
	return size, maxUses
}

func readCfg(t *testing.T) *config.Config {
	pmsDir, dbPath := testEnv(t)
	poolSize, poolMaxUses := testPool(t)
	return &config.Config{
		PMSDir:        pmsDir,
		DBDir:         filepath.Dir(dbPath),
		PMSAddr:       "127.0.0.1:1", // nothing listens here
		BusyTimeoutMS: 5000,
		QueryTimeout:  60 * time.Second,
		PoolSize:      poolSize,
		PoolMaxUses:   poolMaxUses,
		// A scratch state directory so the suite never reads or writes a
		// real /config/plex-api.
		StateDir:  t.TempDir(),
		BackupDir: filepath.Join(t.TempDir(), "backups"),
	}
}

// TestIntegrationIndexes checks the read-only managed-index view. The test
// database is opened read-only here, so nothing is ever created: the point is
// that the status is reported, and reported identically by /health.
func TestIntegrationIndexes(t *testing.T) {
	srv := newTestServer(t, readCfg(t))

	out := getJSON(t, srv, "/v1/indexes", http.StatusOK)
	if out["enabled"] != false {
		t.Errorf("enabled = %v, want false by default", out["enabled"])
	}
	if out["state"] != nil {
		t.Errorf("state = %v, want null with no state file", out["state"])
	}
	managed, _ := out["managed"].([]any)
	if len(managed) == 0 {
		t.Fatalf("managed = %v, want the index definitions", out["managed"])
	}
	for _, m := range managed {
		e := m.(map[string]any)
		name, _ := e["name"].(string)
		if !strings.HasPrefix(name, "zz_plexapi_") {
			t.Errorf("managed index %q must carry the zz_plexapi_ prefix", name)
		}
		if _, ok := e["present"]; !ok {
			t.Errorf("managed index %q has no present flag", name)
		}
		if tbl, _ := e["table"].(string); tbl == "" {
			t.Errorf("managed index %q has no table", name)
		}
	}
	if v, _ := out["pms_version"].(string); v == "" {
		t.Log("pms_version is empty (the PMS binary may not be next to the shell in this test install)")
	}

	health := getJSON(t, srv, "/health", http.StatusOK)
	hi, _ := health["indexes"].(map[string]any)
	if hi == nil {
		t.Fatal("/health carries no indexes section")
	}
	hm, _ := hi["managed"].([]any)
	if len(hm) != len(managed) {
		t.Errorf("/health reports %d managed indexes, /v1/indexes reports %d", len(hm), len(managed))
	}
}

func getJSON(t *testing.T, srv *httptest.Server, path string, want int) map[string]any {
	t.Helper()
	return doJSON(t, srv, http.MethodGet, path, nil, want)
}

func doJSON(t *testing.T, srv *httptest.Server, method, path string, body any, want int) map[string]any {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	start := time.Now()
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	t.Logf("%s %s -> %d in %s (%d bytes)", method, path, resp.StatusCode, time.Since(start).Round(time.Millisecond), len(raw))
	if resp.StatusCode != want {
		t.Fatalf("%s %s: status %d, want %d; body: %s", method, path, resp.StatusCode, want, truncate(string(raw), 400))
	}
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%s %s: bad JSON: %v; body: %s", method, path, err, truncate(string(raw), 400))
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func TestIntegrationHealth(t *testing.T) {
	srv := newTestServer(t, readCfg(t))
	out := getJSON(t, srv, "/health", http.StatusOK)
	if out["status"] != "ok" {
		t.Fatalf("health status = %v (%v)", out["status"], out["error"])
	}
	if v, _ := out["sqlite_version"].(string); v == "" {
		t.Error("health did not report sqlite_version")
	}
	mig, _ := out["schema_migrations"].(map[string]any)
	if mig == nil || mig["count"].(float64) <= 0 {
		t.Errorf("schema_migrations = %v", out["schema_migrations"])
	}
	if mv, _ := mig["max_version"].(string); mv == "" {
		t.Error("schema_migrations.max_version is empty")
	}
	dbs, _ := out["databases"].(map[string]any)
	main, _ := dbs["main"].(map[string]any)
	if main == nil || main["exists"] != true {
		t.Errorf("main database not reported as existing: %v", dbs)
	}
	t.Logf("sqlite=%v migrations=%v pms_running=%v write=%v",
		out["sqlite_version"], mig, out["pms_running"], out["write_policy"])
}

func TestIntegrationHealthNeedsNoAuth(t *testing.T) {
	cfg := readCfg(t)
	cfg.Token = "s3cret"
	srv := newTestServer(t, cfg)
	getJSON(t, srv, "/health", http.StatusOK)
	getJSON(t, srv, "/v1/schema", http.StatusUnauthorized)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/schema", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("bearer token: status %d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/v1/schema", nil)
	req.Header.Set("X-Plex-Api-Token", "s3cret")
	resp, err = srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("header token: status %d", resp.StatusCode)
	}
}

func TestIntegrationSearch(t *testing.T) {
	srv := newTestServer(t, readCfg(t))

	start := time.Now()
	out := getJSON(t, srv, "/v1/search?q=star&limit=10", http.StatusOK)
	elapsed := time.Since(start)

	results, _ := out["results"].([]any)
	if len(results) == 0 {
		t.Fatal("no search results for q=star")
	}
	first, _ := results[0].(map[string]any)
	for _, key := range []string{"docid", "id", "metadata_type", "guid", "title", "fts_title"} {
		if _, ok := first[key]; !ok {
			t.Errorf("search result missing %q: %v", key, first)
		}
	}
	if first["docid"] != first["id"] {
		t.Errorf("docid %v should equal metadata_items.id %v", first["docid"], first["id"])
	}
	t.Logf("LATENCY /v1/search?q=star&limit=10: %s (%d results)", elapsed.Round(time.Millisecond), len(results))

	getJSON(t, srv, "/v1/search", http.StatusBadRequest)

	start = time.Now()
	tags := getJSON(t, srv, "/v1/tags/search?q=star&limit=10", http.StatusOK)
	t.Logf("LATENCY /v1/tags/search?q=star: %s", time.Since(start).Round(time.Millisecond))
	if tr, _ := tags["results"].([]any); len(tr) == 0 {
		t.Error("no tag search results for q=star")
	}
}

func TestIntegrationItem(t *testing.T) {
	srv := newTestServer(t, readCfg(t))

	// Find an item that actually has media hanging off it.
	q := doJSON(t, srv, http.MethodPost, "/v1/query", map[string]any{
		"sql": "SELECT metadata_item_id AS id FROM media_items WHERE metadata_item_id IS NOT NULL LIMIT 1",
	}, http.StatusOK)
	rows, _ := q["rows"].([]any)
	if len(rows) == 0 {
		t.Skip("no media_items rows in the test database")
	}
	id := int64(rows[0].(map[string]any)["id"].(float64))

	start := time.Now()
	out := getJSON(t, srv, fmt.Sprintf("/v1/items/%d", id), http.StatusOK)
	elapsed := time.Since(start)

	item, _ := out["item"].(map[string]any)
	if item == nil || int64(item["id"].(float64)) != id {
		t.Fatalf("item = %v", out["item"])
	}
	for _, key := range []string{"media_items", "media_parts", "media_streams", "taggings", "settings", "extras"} {
		if _, ok := out[key]; !ok {
			t.Errorf("item response missing %q", key)
		}
	}
	if mi, _ := out["media_items"].([]any); len(mi) == 0 {
		t.Error("expected at least one media_item")
	}
	if mp, _ := out["media_parts"].([]any); len(mp) == 0 {
		t.Error("expected at least one media_part")
	}
	t.Logf("LATENCY /v1/items/%d: %s (media=%d parts=%d streams=%d taggings=%d)",
		id, elapsed.Round(time.Millisecond),
		len(out["media_items"].([]any)), len(out["media_parts"].([]any)),
		len(out["media_streams"].([]any)), len(out["taggings"].([]any)))

	getJSON(t, srv, "/v1/items/999999999", http.StatusNotFound)
	getJSON(t, srv, "/v1/items/abc", http.StatusBadRequest)
}

func TestIntegrationTables(t *testing.T) {
	srv := newTestServer(t, readCfg(t))

	start := time.Now()
	out := getJSON(t, srv, "/v1/tables/metadata_items?limit=2", http.StatusOK)
	t.Logf("LATENCY /v1/tables/metadata_items?limit=2 (icu_root order): %s", time.Since(start).Round(time.Millisecond))

	rows, _ := out["rows"].([]any)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	cols, _ := out["columns"].([]any)
	if len(cols) == 0 {
		t.Error("no columns reported")
	}

	// Explicit ordering, validated against table_info.
	out = getJSON(t, srv, "/v1/tables/metadata_items?limit=3&order=-id", http.StatusOK)
	rows, _ = out["rows"].([]any)
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	prev := int64(1) << 62
	for _, r := range rows {
		id := int64(r.(map[string]any)["id"].(float64))
		if id > prev {
			t.Errorf("rows not in descending id order: %d after %d", id, prev)
		}
		prev = id
	}

	getJSON(t, srv, "/v1/tables/metadata_items?order=nope", http.StatusBadRequest)
	getJSON(t, srv, "/v1/tables/no_such_table", http.StatusNotFound)

	// limit is capped.
	out = getJSON(t, srv, "/v1/tables/metadata_items?limit=99999", http.StatusOK)
	if out["limit"].(float64) != 1000 {
		t.Errorf("limit not capped: %v", out["limit"])
	}
}

func TestIntegrationBlobsAreBase64(t *testing.T) {
	_, dbPath := testEnv(t)
	blobs := filepath.Join(filepath.Dir(dbPath), config.BlobsDBName)
	if _, err := os.Stat(blobs); err != nil {
		t.Skip("no blobs database beside the test database")
	}
	srv := newTestServer(t, readCfg(t))
	out := getJSON(t, srv, "/v1/tables/blobs?limit=1&db=blobs", http.StatusOK)
	bc, _ := out["blob_columns"].([]any)
	if len(bc) == 0 {
		t.Fatal("blobs.blob should be reported as a blob column")
	}
	rows, _ := out["rows"].([]any)
	if len(rows) == 0 {
		t.Skip("blobs table is empty")
	}
	v, _ := rows[0].(map[string]any)["blob"].(string)
	if v == "" {
		t.Fatal("blob column empty")
	}
	// gzip magic 1f 8b 08 base64-encodes to a prefix of "H4sI".
	if v[:4] != "H4sI" {
		t.Errorf("blob does not look like base64 gzip: %q", truncate(v, 20))
	}
}

func TestIntegrationSchema(t *testing.T) {
	srv := newTestServer(t, readCfg(t))
	out := getJSON(t, srv, "/v1/schema", http.StatusOK)
	tables, _ := out["tables"].([]any)
	if len(tables) < 50 {
		t.Errorf("got %d tables, expected the full Plex schema", len(tables))
	}
	one := getJSON(t, srv, "/v1/schema/metadata_items", http.StatusOK)
	if cols, _ := one["columns"].([]any); len(cols) < 10 {
		t.Errorf("metadata_items columns = %d", len(cols))
	}
	if idx, _ := one["indexes"].([]any); len(idx) == 0 {
		t.Error("metadata_items should have indexes")
	}
	getJSON(t, srv, "/v1/schema/no_such_table", http.StatusNotFound)
}

func TestIntegrationQueryIsReadOnly(t *testing.T) {
	srv := newTestServer(t, readCfg(t))

	// Bound parameters survive quotes and reach SQLite intact.
	out := doJSON(t, srv, http.MethodPost, "/v1/query", map[string]any{
		"sql":    "SELECT :a AS a, :b AS b, :c AS c, :d AS d",
		"params": map[string]any{"a": "it's a \"test\"", "b": 42, "c": nil, "d": true},
	}, http.StatusOK)
	row := out["rows"].([]any)[0].(map[string]any)
	if row["a"] != "it's a \"test\"" {
		t.Errorf("string parameter round trip failed: %#v", row["a"])
	}
	if row["b"].(float64) != 42 || row["c"] != nil || row["d"].(float64) != 1 {
		t.Errorf("parameter round trip: %#v", row)
	}

	// Several statements come back as "results".
	multi := doJSON(t, srv, http.MethodPost, "/v1/query", map[string]any{
		"sql": "SELECT 1 AS a; SELECT 2 AS b;",
	}, http.StatusOK)
	if sets, _ := multi["results"].([]any); len(sets) != 2 {
		t.Errorf("multi-statement query = %v", multi)
	}

	// -readonly makes the engine refuse writes regardless of the write policy.
	doJSON(t, srv, http.MethodPost, "/v1/query", map[string]any{
		"sql": "UPDATE metadata_items SET summary = 'x' WHERE id = -1",
	}, http.StatusForbidden)

	// Bad SQL is a structured 400.
	bad := doJSON(t, srv, http.MethodPost, "/v1/query", map[string]any{
		"sql": "SELECT * FROM no_such_table",
	}, http.StatusBadRequest)
	e, _ := bad["error"].(map[string]any)
	if e == nil || e["code"] != "sql_error" {
		t.Errorf("error body = %v", bad)
	}
}

func TestIntegrationWritesBlockedByDefault(t *testing.T) {
	srv := newTestServer(t, readCfg(t)) // AllowWrite is false
	out := doJSON(t, srv, http.MethodPost, "/v1/exec", map[string]any{
		"statements": []any{map[string]any{"sql": "UPDATE metadata_items SET summary = 'x' WHERE id = -1"}},
	}, http.StatusForbidden)
	if e, _ := out["error"].(map[string]any); e == nil || e["code"] != "writes_disabled" {
		t.Errorf("error = %v", out)
	}
	doJSON(t, srv, http.MethodPatch, "/v1/settings/plex:%2F%2Fmovie%2Fx", map[string]any{
		"view_count": 1,
	}, http.StatusForbidden)
}

func pathEscape(s string) string { return url.PathEscape(s) }

// TestIntegrationPoolConcurrentItems drives the HTTP surface with more
// simultaneous requests than the pool has shells, and checks that /health
// reports the pool afterwards.
func TestIntegrationPoolConcurrentItems(t *testing.T) {
	cfg := readCfg(t)
	if cfg.PoolSize == 0 {
		t.Skip("pool disabled (PLEX_API_POOL=0)")
	}
	cfg.PoolSize = 4
	srv := newTestServer(t, cfg)

	id := anyItemID(t, srv)
	const n = 16
	var wg sync.WaitGroup
	bodies := make([]string, n)
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := http.Get(srv.URL + "/v1/items/" + id)
			if err != nil {
				codes[i] = -1
				bodies[i] = err.Error()
				return
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			codes[i], bodies[i] = resp.StatusCode, string(b)
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if codes[i] != http.StatusOK {
			t.Fatalf("request %d: status %d: %s", i, codes[i], bodies[i])
		}
		if bodies[i] != bodies[0] {
			t.Errorf("request %d returned a different body than request 0 (cross-talk)", i)
		}
	}

	h := getJSON(t, srv, "/health", http.StatusOK)
	pools, _ := h["pool"].([]any)
	if len(pools) == 0 {
		t.Fatal("/health reported no pool")
	}
	p, _ := pools[0].(map[string]any)
	if num(p["size"]) != 4 {
		t.Errorf("health pool size = %v, want 4", p["size"])
	}
	if num(p["idle"]) != 4 {
		t.Errorf("health pool idle = %v, want 4", p["idle"])
	}
	if num(p["spawned_total"]) <= 0 {
		t.Errorf("health pool spawned_total = %v, want a positive count", p["spawned_total"])
	}
}

// anyItemID returns the id of some metadata_items row.
func anyItemID(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	got := doJSON(t, srv, http.MethodPost, "/v1/query",
		map[string]any{"sql": "SELECT CAST(id AS TEXT) AS id FROM metadata_items ORDER BY id LIMIT 1"}, http.StatusOK)
	rows, _ := got["rows"].([]any)
	if len(rows) != 1 {
		t.Fatal("no metadata_items rows in the test database")
	}
	row, _ := rows[0].(map[string]any)
	id, _ := row["id"].(string)
	if id == "" {
		t.Fatal("no item id")
	}
	return id
}

// num reads a JSON number out of a decoded response.
func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

func TestIntegrationRecent(t *testing.T) {
	srv := newTestServer(t, readCfg(t))
	out := getJSON(t, srv, "/v1/recent?types=1,2&limit=5", http.StatusOK)
	rows, _ := out["results"].([]any)
	if len(rows) != 5 || out["count"].(float64) != 5 {
		t.Fatalf("recent: count=%v rows=%d", out["count"], len(rows))
	}
	prev := float64(1 << 62)
	for _, r := range rows {
		row := r.(map[string]any)
		mt := row["metadata_type"].(float64)
		if mt != 1 && mt != 2 {
			t.Errorf("unexpected metadata_type %v", mt)
		}
		at := row["added_at"].(float64)
		if at > prev {
			t.Errorf("results not newest first: %v after %v", at, prev)
		}
		prev = at
	}
	sec := getJSON(t, srv, "/v1/recent?types=1&section=5&limit=3", http.StatusOK)
	for _, r := range sec["results"].([]any) {
		if r.(map[string]any)["library_section_id"].(float64) != 5 {
			t.Errorf("section filter ignored: %v", r)
		}
	}
	getJSON(t, srv, "/v1/recent?types=x", http.StatusBadRequest)
}
