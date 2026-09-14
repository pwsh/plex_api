package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNeedsIsolation(t *testing.T) {
	yes := []string{
		"PRAGMA journal_mode",
		"  pragma table_info(t)",
		"ATTACH DATABASE 'x' AS y",
		"attach 'x' as y",
		"DETACH y",
		"SELECT 1; PRAGMA cache_size = 1",
		"-- a comment\nPRAGMA foo",
		"PRAGMA(x)",
		"/* hidden */ PRAGMA foo",
		"CREATE TEMP TABLE t(x)",
		"SELECT 1; create temp view v as select 1",
		"BEGIN",
	}
	for _, s := range yes {
		if !NeedsIsolation(s) {
			t.Errorf("NeedsIsolation(%q) = false, want true", s)
		}
	}
	no := []string{
		"SELECT 1",
		"SELECT 'pragma' AS x",
		"SELECT * FROM t WHERE name = 'ATTACH'",
		"WITH a AS (SELECT 1) SELECT * FROM a",
		"/* c */ SELECT 2",
		"EXPLAIN QUERY PLAN SELECT 1",
		"",
	}
	for _, s := range no {
		if NeedsIsolation(s) {
			t.Errorf("NeedsIsolation(%q) = true, want false", s)
		}
	}
}

// poolDriver builds a Driver with a pool of the given size against the real
// test database. It skips unless the integration environment is set.
func poolDriver(t *testing.T, size int, maxUses int, timeout time.Duration) (*Driver, string) {
	t.Helper()
	pmsDir := os.Getenv("PLEX_API_TEST_PMS_DIR")
	dbPath := os.Getenv("PLEX_API_TEST_DB")
	if pmsDir == "" || dbPath == "" {
		t.Skip("set PLEX_API_TEST_PMS_DIR and PLEX_API_TEST_DB to run pool integration tests")
	}
	if _, err := os.Stat(filepath.Join(pmsDir, ShellName)); err != nil {
		t.Skipf("no %s in %s: %v", ShellName, pmsDir, err)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Skipf("no database at %s: %v", dbPath, err)
	}
	d := New(pmsDir, 5000, timeout, size, maxUses)
	t.Cleanup(d.Close)
	return d, dbPath
}

// TestPoolErrorThenSuccess checks that a failing statement, which -bail turns
// into a process exit, is reported as a normal SQL error and that the pool
// spawns a replacement for the next request.
func TestPoolErrorThenSuccess(t *testing.T) {
	d, db := poolDriver(t, 1, 0, 30*time.Second)
	ctx := context.Background()

	if _, err := d.Query(ctx, db, []Statement{{SQL: "SELECT 1 AS n"}}); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	before := d.PoolStats()[0].SpawnedTotal

	_, err := d.Query(ctx, db, []Statement{{SQL: "SELECT * FROM no_such_table_here"}})
	if err == nil {
		t.Fatal("want an error from a missing table")
	}
	var se *Error
	if !asSQLiteError(err, &se) {
		t.Fatalf("want *sqlite.Error, got %T: %v", err, err)
	}
	if !strings.Contains(se.Message, "no such table") {
		t.Errorf("message = %q, want it to mention the missing table", se.Message)
	}
	// The spawn path reports line 4 for the first statement of a one-statement
	// script; the pool must rewrite its running line count to match.
	if se.Line != 5 {
		t.Errorf("line = %d, want 5 (script-relative, as the spawn path reports)", se.Line)
	}

	res, err := d.Query(ctx, db, []Statement{{SQL: "SELECT 7 AS n"}})
	if err != nil {
		t.Fatalf("request after the error failed: %v", err)
	}
	if len(res) != 1 || len(res[0].Rows) != 1 || res[0].Rows[0]["n"].(interface{ String() string }).String() != "7" {
		t.Fatalf("unexpected result %+v", res)
	}
	st := d.PoolStats()[0]
	if st.SpawnedTotal <= before {
		t.Errorf("spawned_total = %d, want more than %d (a replacement shell)", st.SpawnedTotal, before)
	}
	if st.Idle != 1 {
		t.Errorf("idle = %d, want 1", st.Idle)
	}
}

// TestPoolParameterIsolation checks that .parameter clear stops a binding from
// one request being visible in the next on the same shell.
func TestPoolParameterIsolation(t *testing.T) {
	d, db := poolDriver(t, 1, 0, 30*time.Second)
	ctx := context.Background()

	res, err := d.Query(ctx, db, []Statement{{SQL: "SELECT :p AS p", Params: map[string]any{"p": 4242}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := str(res[0].Rows[0]["p"]); got != "4242" {
		t.Fatalf("first request p = %v, want 4242", res[0].Rows[0]["p"])
	}
	res, err = d.Query(ctx, db, []Statement{{SQL: "SELECT :p IS NULL AS unbound"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := str(res[0].Rows[0]["unbound"]); got != "1" {
		t.Fatalf(":p leaked into the next request on the same shell (unbound = %v)", res[0].Rows[0]["unbound"])
	}
}

// TestPoolConcurrency runs more requests at once than the pool has shells.
func TestPoolConcurrency(t *testing.T) {
	d, db := poolDriver(t, 4, 0, 60*time.Second)
	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	vals := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := d.Query(context.Background(), db,
				[]Statement{{SQL: "SELECT :i AS i, count(*) AS c FROM metadata_items", Params: map[string]any{"i": i}}})
			if err != nil {
				errs[i] = err
				return
			}
			vals[i] = str(res[0].Rows[0]["i"])
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("request %d: %v", i, errs[i])
		}
		if vals[i] != itoa(i) {
			t.Errorf("request %d got i=%q, want %d (cross-talk between shells)", i, vals[i], i)
		}
	}
	st := d.PoolStats()[0]
	if st.Size != 4 {
		t.Errorf("size = %d, want 4", st.Size)
	}
	if st.SpawnedTotal > 4 {
		t.Errorf("spawned_total = %d, want at most 4 for a clean run", st.SpawnedTotal)
	}
	if st.Idle != 4 {
		t.Errorf("idle = %d, want 4 after everything finished", st.Idle)
	}
}

// TestPoolTimeoutRecovers checks that a query that outruns QueryTimeout is
// killed, reported as a timeout, and does not poison the pool.
func TestPoolTimeoutRecovers(t *testing.T) {
	d, db := poolDriver(t, 1, 0, 500*time.Millisecond)
	ctx := context.Background()

	slow := "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 2000000000) SELECT count(*) AS n FROM c"
	start := time.Now()
	_, err := d.Query(ctx, db, []Statement{{SQL: slow}})
	if err == nil {
		t.Fatal("want a timeout error")
	}
	var se *Error
	if !asSQLiteError(err, &se) || se.Kind != "timeout" {
		t.Fatalf("want a timeout *sqlite.Error, got %T: %v", err, err)
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("timeout took %s, want it to be enforced promptly", el)
	}

	res, err := d.Query(ctx, db, []Statement{{SQL: "SELECT 3 AS n"}})
	if err != nil {
		t.Fatalf("request after the timeout failed: %v", err)
	}
	if str(res[0].Rows[0]["n"]) != "3" {
		t.Fatalf("unexpected result %+v", res)
	}
	if st := d.PoolStats()[0]; st.Idle != 1 {
		t.Errorf("idle = %d, want 1", st.Idle)
	}
}

// TestPoolIsolatedStatements checks that PRAGMA and ATTACH go to a private
// process: they still work, and they leave no trace on a pooled shell.
func TestPoolIsolatedStatements(t *testing.T) {
	d, db := poolDriver(t, 1, 0, 30*time.Second)
	ctx := context.Background()

	// Prime the pool so a shell exists.
	if _, err := d.Query(ctx, db, []Statement{{SQL: "SELECT 1 AS n"}}); err != nil {
		t.Fatal(err)
	}
	spawned := d.PoolStats()[0].SpawnedTotal

	res, err := d.QueryAll(ctx, db, Statement{SQL: "PRAGMA cache_size = 1234; PRAGMA cache_size"})
	if err != nil {
		t.Fatalf("PRAGMA through the spawn fallback: %v", err)
	}
	last := res[len(res)-1]
	if len(last.Rows) != 1 || str(last.Rows[0]["cache_size"]) != "1234" {
		t.Fatalf("PRAGMA did not take effect in its own process: %+v", res)
	}

	// The pooled shell must be untouched, and must not have been restarted.
	res2, err := d.QueryAll(ctx, db, Statement{SQL: "SELECT 1 AS n"})
	if err != nil {
		t.Fatal(err)
	}
	if str(res2[0].Rows[0]["n"]) != "1" {
		t.Fatalf("unexpected result %+v", res2)
	}
	if got := d.PoolStats()[0].SpawnedTotal; got != spawned {
		t.Errorf("spawned_total = %d, want %d: the isolated statement should not touch the pool", got, spawned)
	}

	if _, err := d.QueryAll(ctx, db, Statement{SQL: "ATTACH DATABASE ':memory:' AS scratch; SELECT count(*) AS n FROM scratch.sqlite_master"}); err != nil {
		t.Fatalf("ATTACH through the spawn fallback: %v", err)
	}
	if _, err := d.QueryAll(ctx, db, Statement{SQL: "SELECT count(*) AS n FROM scratch.sqlite_master"}); err == nil {
		t.Error("the attached database leaked into a later request")
	}
}

// TestPoolRecyclesAfterMaxUses checks PoolMaxUses.
func TestPoolRecyclesAfterMaxUses(t *testing.T) {
	d, db := poolDriver(t, 1, 3, 30*time.Second)
	for i := 0; i < 7; i++ {
		if _, err := d.Query(context.Background(), db, []Statement{{SQL: "SELECT 1 AS n"}}); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	st := d.PoolStats()[0]
	if st.RecycledTotal < 2 {
		t.Errorf("recycled_total = %d, want at least 2 after 7 requests with max_uses=3", st.RecycledTotal)
	}
	if st.SpawnedTotal < 3 {
		t.Errorf("spawned_total = %d, want at least 3", st.SpawnedTotal)
	}
}

// --- small helpers ---------------------------------------------------------

func asSQLiteError(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}

func str(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(interface{ String() string }); ok {
		return s.String()
	}
	return ""
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
