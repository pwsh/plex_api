package indexes_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pwsh/plex_api/internal/config"
	"github.com/pwsh/plex_api/internal/indexes"
	"github.com/pwsh/plex_api/internal/sqlite"
)

// These tests need a real PMS install and a real library database:
//
//	PLEX_API_TEST_PMS_DIR=/usr/lib/plexmediaserver
//	PLEX_API_TEST_DB=/path/to/com.plexapp.plugins.library.db
//
// PLEX_API_TEST_DB is never written to: it is copied into a temporary
// directory first, because building indexes is a write.
func testEnv(t *testing.T) (pmsDir, dbPath string) {
	t.Helper()
	pmsDir = os.Getenv("PLEX_API_TEST_PMS_DIR")
	dbPath = os.Getenv("PLEX_API_TEST_DB")
	if pmsDir == "" || dbPath == "" {
		t.Skip("set PLEX_API_TEST_PMS_DIR and PLEX_API_TEST_DB to run integration tests")
	}
	if _, err := os.Stat(filepath.Join(pmsDir, sqlite.ShellName)); err != nil {
		t.Skipf("no Plex SQLite in %s: %v", pmsDir, err)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Skipf("no database at %s: %v", dbPath, err)
	}
	return pmsDir, dbPath
}

func scratchConfig(t *testing.T) *config.Config {
	t.Helper()
	pmsDir, dbPath := testEnv(t)
	dir := t.TempDir()
	start := time.Now()
	copyFile(t, dbPath, filepath.Join(dir, config.MainDBName))
	t.Logf("copied the library database in %s", time.Since(start).Round(time.Millisecond))
	state := t.TempDir()
	return &config.Config{
		PMSDir:        pmsDir,
		DBDir:         dir,
		PMSAddr:       "127.0.0.1:1", // nothing listens here, so "PMS is stopped"
		BusyTimeoutMS: 5000,
		QueryTimeout:  10 * time.Minute,
		Indexes:       true,
		IndexBackup:   true,
		StateDir:      state,
		BackupDir:     filepath.Join(state, "backups"),
		BackupKeep:    3,
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
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestIntegrationEnsureLifecycle walks the whole managed-index life cycle on a
// scratch copy of a real library: missing -> created (with a backup) ->
// present -> cheap no-op -> dropped because the PMS version changed.
func TestIntegrationEnsureLifecycle(t *testing.T) {
	cfg := scratchConfig(t)
	ctx := context.Background()
	drv := sqlite.New(cfg.PMSDir, cfg.BusyTimeoutMS, cfg.QueryTimeout, 0, 0)
	defer drv.Close()

	logf := func(format string, args ...any) { t.Logf(format, args...) }

	// 1. Nothing there yet.
	start := time.Now()
	st, err := indexes.GetStatus(ctx, drv, cfg.MainDB())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("GetStatus (cold, pool off) took %s", time.Since(start).Round(time.Millisecond))
	if st.AnyPresent() {
		t.Fatalf("a fresh copy should carry no managed indexes: %+v", st)
	}
	if st.MaxMigration == "" {
		t.Error("GetStatus should report max(schema_migrations.version)")
	}
	if st.SchemaVersion == 0 {
		t.Error("GetStatus should report PRAGMA schema_version")
	}

	// 2. Ensure creates them and takes a backup first.
	start = time.Now()
	if err := indexes.Ensure(ctx, cfg, logf); err != nil {
		t.Fatal(err)
	}
	t.Logf("first Ensure (backup + create + ANALYZE + quick_check) took %s", time.Since(start).Round(time.Millisecond))

	backups, err := filepath.Glob(filepath.Join(cfg.BackupDir, "*", config.MainDBName))
	if err != nil || len(backups) != 1 {
		t.Fatalf("want exactly one backup, got %v (%v)", backups, err)
	}
	orig, _ := os.Stat(cfg.MainDB())
	bak, err := os.Stat(backups[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("backup %s is %d bytes (source %d)", backups[0], bak.Size(), orig.Size())
	if bak.Size() == 0 {
		t.Fatal("the backup is empty")
	}

	state, err := indexes.LoadState(cfg.StateDir)
	if err != nil || state == nil {
		t.Fatalf("state file: %+v, %v", state, err)
	}
	if state.PMSVersion == "" || state.PMSVersion == indexes.UnknownVersion {
		t.Errorf("state should record the real PMS version, got %q", state.PMSVersion)
	}
	if len(state.Indexes) != len(indexes.Managed) {
		t.Errorf("state records %v, want %d indexes", state.Indexes, len(indexes.Managed))
	}

	// 3. Everything is present now.
	st, err = indexes.GetStatus(ctx, drv, cfg.MainDB())
	if err != nil {
		t.Fatal(err)
	}
	if !st.AllPresent() {
		t.Fatalf("after Ensure: %+v", st)
	}
	if len(st.Unknown) != 0 {
		t.Errorf("unexpected leftover indexes %v", st.Unknown)
	}

	// 4. The second run is the fast path: no backup, no DDL, one read round
	// trip plus the PMS --version call.
	start = time.Now()
	if err := indexes.Ensure(ctx, cfg, logf); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	t.Logf("second Ensure (no-op fast path) took %s", elapsed.Round(time.Millisecond))
	if elapsed > 2*time.Second {
		t.Errorf("the no-op path took %s; it should be well under a second", elapsed)
	}
	if got, _ := filepath.Glob(filepath.Join(cfg.BackupDir, "*")); len(got) != 1 {
		t.Errorf("the no-op path must not take another backup, got %v", got)
	}
	if after, err := indexes.LoadState(cfg.StateDir); err != nil || after.CreatedAt != state.CreatedAt {
		t.Errorf("the no-op path must not rewrite the state file: %+v, %v", after, err)
	}

	// 5. A PMS upgrade: rewrite the recorded version and ensure again. The
	// indexes must come out so Plex's migrations see a clean schema.
	state.PMSVersion = "v0.0.0-pretend-older"
	if err := indexes.SaveState(cfg.StateDir, state); err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	if err := indexes.Ensure(ctx, cfg, logf); err != nil {
		t.Fatal(err)
	}
	t.Logf("version-change Ensure (drop) took %s", time.Since(start).Round(time.Millisecond))

	st, err = indexes.GetStatus(ctx, drv, cfg.MainDB())
	if err != nil {
		t.Fatal(err)
	}
	if st.AnyPresent() {
		t.Fatalf("the version change should have dropped everything: %+v", st)
	}
	if got, err := indexes.LoadState(cfg.StateDir); err != nil || got != nil {
		t.Fatalf("the state file should be gone: %+v, %v", got, err)
	}

	// 6. And the next start rebuilds them.
	if err := indexes.Ensure(ctx, cfg, logf); err != nil {
		t.Fatal(err)
	}
	if st, err = indexes.GetStatus(ctx, drv, cfg.MainDB()); err != nil || !st.AllPresent() {
		t.Fatalf("rebuild after a version change: %+v, %v", st, err)
	}

	// 7. Explicit removal, the documented way out.
	if err := indexes.DropAll(ctx, cfg, logf); err != nil {
		t.Fatal(err)
	}
	if st, err = indexes.GetStatus(ctx, drv, cfg.MainDB()); err != nil || st.AnyPresent() {
		t.Fatalf("after DropAll: %+v, %v", st, err)
	}
}

// TestIntegrationEnsureDisabled checks that the feature being off neither
// creates anything nor removes what is already there.
func TestIntegrationEnsureDisabled(t *testing.T) {
	cfg := scratchConfig(t)
	cfg.Indexes = false
	ctx := context.Background()
	drv := sqlite.New(cfg.PMSDir, cfg.BusyTimeoutMS, cfg.QueryTimeout, 0, 0)
	defer drv.Close()

	if err := indexes.Ensure(ctx, cfg, t.Logf); err != nil {
		t.Fatal(err)
	}
	st, err := indexes.GetStatus(ctx, drv, cfg.MainDB())
	if err != nil {
		t.Fatal(err)
	}
	if st.AnyPresent() {
		t.Fatalf("PLEX_API_INDEXES off must not create anything: %+v", st)
	}
	if got, err := indexes.LoadState(cfg.StateDir); err != nil || got != nil {
		t.Fatalf("no state file should be written: %+v, %v", got, err)
	}
	if _, err := os.Stat(cfg.BackupDir); !os.IsNotExist(err) {
		t.Errorf("no backup directory should be created: %v", err)
	}
}

// TestIntegrationBackupPrunes checks that Backup produces a usable copy and
// that Prune keeps only the newest ones.
func TestIntegrationBackupPrunes(t *testing.T) {
	cfg := scratchConfig(t)
	ctx := context.Background()
	drv := sqlite.New(cfg.PMSDir, cfg.BusyTimeoutMS, cfg.QueryTimeout, 0, 0)
	defer drv.Close()
	shell := indexes.DriverShell{ShellPath: drv.ShellPath, LibDir: drv.LibDir}

	start := time.Now()
	path, err := indexes.Backup(ctx, shell, cfg.MainDB(), cfg.BackupDir)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf(".backup of %d bytes took %s", fi.Size(), time.Since(start).Round(time.Millisecond))

	// The copy must be a valid database the engine can open.
	res, err := drv.Query(ctx, path, []sqlite.Statement{{SQL: "PRAGMA quick_check(1)"}})
	if err != nil {
		t.Fatal(err)
	}
	got := ""
	for _, v := range res[0].Rows[0] {
		got, _ = v.(string)
	}
	if got != "ok" {
		t.Fatalf("quick_check on the backup = %q", got)
	}

	// Four backups, keep three.
	for i := 0; i < 3; i++ {
		stampDir := filepath.Join(cfg.BackupDir, time.Now().Add(time.Duration(i+1)*time.Second).Format("20060102-150405"))
		if err := os.MkdirAll(stampDir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := indexes.Prune(cfg.BackupDir, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || !strings.HasPrefix(removed[0], cfg.BackupDir) {
		t.Fatalf("Prune removed %v, want the single oldest", removed)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the oldest backup should be gone: %v", err)
	}
}
