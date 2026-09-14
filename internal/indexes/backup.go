package indexes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// backupStampLayout names each backup directory by the time it was taken.
const backupStampLayout = "20060102-150405"

// Backup copies the main library database to dir/<timestamp>/<name> using the
// shell's ".backup" dot-command, which drives SQLite's online backup API and
// therefore produces a consistent copy including everything still in the WAL.
// A plain file copy would not.
//
// The shell is executed directly rather than through Driver.Query/Exec,
// because ".backup" is a dot-command and the driver deliberately refuses those
// inside statements.
//
// Only the main database is backed up: it is the only one that gets indexes.
func Backup(ctx context.Context, drv shellRunner, dbPath, dir string) (string, error) {
	stamp := time.Now().Format(backupStampLayout)
	dest := filepath.Join(dir, stamp)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", err
	}
	out := filepath.Join(dest, filepath.Base(dbPath))
	script := fmt.Sprintf(".timeout %d\n.backup '%s'\n", 30000, escapeSQL(out))

	cmd := exec.CommandContext(ctx, drv.Shell(), "-bail", dbPath)
	cmd.Env = backupEnv(drv.Lib())
	cmd.Stdin = strings.NewReader(script)
	var stderr, stdout strings.Builder
	cmd.Stderr = &stderr
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		_ = os.RemoveAll(dest)
		return "", fmt.Errorf("backup: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if msg := strings.TrimSpace(stderr.String()); msg != "" {
		_ = os.RemoveAll(dest)
		return "", fmt.Errorf("backup: %s", msg)
	}
	if fi, err := os.Stat(out); err != nil || fi.Size() == 0 {
		_ = os.RemoveAll(dest)
		return "", errors.New("backup: no output file was produced")
	}
	return out, nil
}

// shellRunner is the little of Driver that Backup needs, so tests can supply a
// stub and so the dependency stays visible.
type shellRunner interface {
	Shell() string
	Lib() string
}

// DriverShell adapts a *sqlite.Driver to shellRunner.
type DriverShell struct {
	ShellPath string
	LibDir    string
}

func (d DriverShell) Shell() string { return d.ShellPath }
func (d DriverShell) Lib() string   { return d.LibDir }

func backupEnv(libDir string) []string {
	env := os.Environ()
	out := env[:0:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, "LD_LIBRARY_PATH=") {
			continue
		}
		out = append(out, kv)
	}
	if libDir != "" {
		out = append(out, "LD_LIBRARY_PATH="+libDir)
	}
	return out
}

// escapeSQL doubles single quotes so a path can be embedded in the
// single-quoted argument of a dot-command.
func escapeSQL(s string) string { return strings.ReplaceAll(s, "'", "''") }

// Prune keeps the keep most recent backup directories under dir and removes
// the rest. keep <= 0 keeps everything. It only ever removes directories whose
// name is a backup timestamp, so an unrelated file in the backup directory is
// never touched.
func Prune(dir string, keep int) ([]string, error) {
	if keep <= 0 {
		return nil, nil
	}
	ents, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var stamps []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if _, err := time.Parse(backupStampLayout, e.Name()); err != nil {
			continue
		}
		stamps = append(stamps, e.Name())
	}
	if len(stamps) <= keep {
		return nil, nil
	}
	sortStrings(stamps) // the layout sorts lexicographically by time
	var removed []string
	for _, name := range stamps[:len(stamps)-keep] {
		p := filepath.Join(dir, name)
		if err := os.RemoveAll(p); err != nil {
			return removed, err
		}
		removed = append(removed, p)
	}
	return removed, nil
}
