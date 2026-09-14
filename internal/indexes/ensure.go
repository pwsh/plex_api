package indexes

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/pwsh/plex_api/internal/config"
	"github.com/pwsh/plex_api/internal/sqlite"
)

// UnknownVersion is recorded when the PMS binary could not be asked for its
// version. The ensure step never drops indexes on an unknown version: it
// cannot tell an upgrade from a broken install.
const UnknownVersion = "unknown"

// PMSVersion runs "<pmsDir>/Plex Media Server --version" and returns its
// trimmed output (e.g. "v1.43.4.10903-e5521bd8c"). The binary needs
// LD_LIBRARY_PATH pointing at <pmsDir>/lib, exactly like the shell does.
// A failure is not fatal anywhere: the caller falls back to UnknownVersion.
func PMSVersion(ctx context.Context, pmsDir string) (string, error) {
	bin := filepath.Join(pmsDir, "Plex Media Server")
	if _, err := os.Stat(bin); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--version")
	cmd.Env = backupEnv(filepath.Join(pmsDir, "lib"))
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		return "", errors.New("empty version output")
	}
	return v, nil
}

// pmsRunning reports whether something is listening on addr. The managed
// indexes are only ever built with Plex stopped, which in the container means
// "before svc-plex starts".
func pmsRunning(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// Ensure is the -ensure-indexes CLI mode: bring the managed indexes in line
// with the configuration, cheaply when there is nothing to do.
//
// It is designed to be run at container start, before Plex Media Server is
// started, so no lock contention and no PMS restart are involved.
//
// The decision order is:
//
//  1. no database yet (fresh install)        -> nothing to do
//  2. PMS version changed since the state    -> drop everything, forget the
//     file was written                          state, let migrations run on a
//     clean schema; recreate next start
//  3. PLEX_API_INDEXES is not true           -> leave whatever is there alone
//  4. every managed index already present    -> nothing to do (the fast path,
//     one read round trip)
//  5. otherwise                              -> optional backup, then create
func Ensure(ctx context.Context, cfg *config.Config, logf func(string, ...any)) error {
	if logf == nil {
		logf = log.Printf
	}
	dbPath := cfg.MainDB()
	if _, err := os.Stat(dbPath); err != nil {
		logf("[plex-api] no library database at %s yet; nothing to index", dbPath)
		return nil
	}

	// A one-shot CLI run has no use for a pool of long-lived shells.
	drv := sqlite.New(cfg.PMSDir, cfg.BusyTimeoutMS, cfg.QueryTimeout, 0, 0)
	defer drv.Close()

	version, verr := PMSVersion(ctx, cfg.PMSDir)
	if verr != nil {
		version = UnknownVersion
		logf("[plex-api] could not read the Plex Media Server version (%v); continuing as %q", verr, UnknownVersion)
	}

	state, err := LoadState(cfg.StateDir)
	if err != nil {
		return fmt.Errorf("reading %s: %w", StatePath(cfg.StateDir), err)
	}

	start := time.Now()
	status, err := GetStatus(ctx, drv, dbPath)
	if err != nil {
		return fmt.Errorf("reading index status: %w", err)
	}
	logf("[plex-api] index status read in %s (schema_version=%d, migrations=%s)",
		time.Since(start).Round(time.Millisecond), status.SchemaVersion, status.MaxMigration)

	// 2. A PMS upgrade is the one case where our indexes are a liability:
	// SQLite refuses ALTER TABLE DROP COLUMN on an indexed column, so a
	// migration dropping metadata_items.title_sort or added_at would fail
	// while ours exist. Drop them first and rebuild on the next start.
	if state != nil && version != UnknownVersion && state.PMSVersion != version && status.AnyPresent() {
		if pmsRunning(cfg.PMSAddr) {
			return fmt.Errorf("Plex Media Server is listening on %s; managed indexes must not be dropped while it runs", cfg.PMSAddr)
		}
		dropped, err := Drop(ctx, drv, dbPath, status.Unknown...)
		if err != nil {
			return err
		}
		if err := RemoveState(cfg.StateDir); err != nil {
			return fmt.Errorf("removing %s: %w", StatePath(cfg.StateDir), err)
		}
		logf("[plex-api] Plex version changed from %s to %s: managed indexes dropped so migrations run "+
			"on a clean schema; they will be recreated on the next start (%s)",
			state.PMSVersion, version, strings.Join(dropped, ", "))
		return nil
	}

	// 3. Turned off. Existing indexes are left alone on purpose: removing
	// them is an explicit action (-drop-indexes), not a side effect of
	// unsetting a variable.
	if !cfg.Indexes {
		if status.AnyPresent() {
			logf("[plex-api] PLEX_API_INDEXES is not set, but managed indexes are present (%s); "+
				"leaving them. Run `plex-api -drop-indexes` to remove them.",
				strings.Join(append(status.PresentNames(), status.Unknown...), ", "))
		}
		return nil
	}

	// 4. The fast path every start after the first takes.
	if status.AllPresent() && len(status.Unknown) == 0 {
		if state == nil {
			// Indexes exist but the state file was lost; write it back so the
			// next PMS upgrade is still noticed.
			if err := SaveState(cfg.StateDir, &State{PMSVersion: version, Indexes: status.PresentNames()}); err != nil {
				return err
			}
		}
		logf("[plex-api] managed indexes present (%s); nothing to do", strings.Join(status.PresentNames(), ", "))
		return nil
	}

	// 5. Something is missing. This must not happen next to a live PMS.
	if pmsRunning(cfg.PMSAddr) {
		return fmt.Errorf("Plex Media Server is listening on %s; stop it before building managed indexes", cfg.PMSAddr)
	}

	if cfg.IndexBackup {
		start = time.Now()
		path, err := Backup(ctx, DriverShell{ShellPath: drv.ShellPath, LibDir: drv.LibDir}, dbPath, cfg.BackupDir)
		if err != nil {
			return err
		}
		size := int64(0)
		if fi, serr := os.Stat(path); serr == nil {
			size = fi.Size()
		}
		logf("[plex-api] backup written to %s (%d bytes) in %s", path, size, time.Since(start).Round(time.Millisecond))
		removed, err := Prune(cfg.BackupDir, cfg.BackupKeep)
		if err != nil {
			logf("[plex-api] pruning old backups: %v", err)
		}
		for _, r := range removed {
			logf("[plex-api] pruned old backup %s", r)
		}
	}

	res, err := Create(ctx, drv, dbPath)
	if err != nil {
		return err
	}
	logf("[plex-api] created %s in %s; quick_check %s in %s",
		strings.Join(res.Created, ", "), res.Build.Round(time.Millisecond),
		res.QuickChk, res.Check.Round(time.Millisecond))

	if err := SaveState(cfg.StateDir, &State{PMSVersion: version, Indexes: res.Created}); err != nil {
		return fmt.Errorf("writing %s: %w", StatePath(cfg.StateDir), err)
	}
	logf("[plex-api] wrote %s (pms_version=%s)", StatePath(cfg.StateDir), version)
	return nil
}

// DropAll is the -drop-indexes CLI mode: remove every managed index and the
// state file. It refuses to run while PMS is up.
func DropAll(ctx context.Context, cfg *config.Config, logf func(string, ...any)) error {
	if logf == nil {
		logf = log.Printf
	}
	dbPath := cfg.MainDB()
	if _, err := os.Stat(dbPath); err != nil {
		logf("[plex-api] no library database at %s; nothing to drop", dbPath)
		return nil
	}
	if pmsRunning(cfg.PMSAddr) {
		return fmt.Errorf("Plex Media Server is listening on %s; stop it before dropping managed indexes", cfg.PMSAddr)
	}
	drv := sqlite.New(cfg.PMSDir, cfg.BusyTimeoutMS, cfg.QueryTimeout, 0, 0)
	defer drv.Close()

	status, err := GetStatus(ctx, drv, dbPath)
	if err != nil {
		return err
	}
	dropped, err := Drop(ctx, drv, dbPath, status.Unknown...)
	if err != nil {
		return err
	}
	if err := RemoveState(cfg.StateDir); err != nil {
		return err
	}
	logf("[plex-api] dropped %s and removed %s", strings.Join(dropped, ", "), StatePath(cfg.StateDir))
	return nil
}
