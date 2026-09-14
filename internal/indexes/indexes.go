// Package indexes manages a small set of extra SQLite indexes on the Plex
// library database ("managed indexes").
//
// Plex never creates these; we do, optionally, at container start while Plex
// Media Server is not running. Every name carries the zz_plexapi_ prefix so a
// future Plex migration can never collide with one.
//
// The one real risk is that SQLite refuses ALTER TABLE DROP COLUMN on a column
// that an index mentions. A future Plex migration dropping metadata_items.
// title_sort or added_at would therefore fail while our index exists, so the
// ensure step drops every managed index when the PMS version changes and
// recreates it on the following start, leaving migrations a clean schema.
package indexes

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pwsh/plex_api/internal/sqlite"
)

// Prefix is required on every managed index name.
const Prefix = "zz_plexapi_"

// Index is one managed index.
type Index struct {
	Name   string `json:"name"`
	Table  string `json:"table"`
	Create string `json:"-"`
	// Why documents the query the index exists for.
	Why string `json:"why,omitempty"`
}

// Managed is the complete set. Adding one here is all it takes: the ensure
// step, /health and /v1/indexes are all driven from this slice.
//
// The title_sort index must be declared COLLATE icu_root, because that is the
// collation /v1/tables/metadata_items sorts with, and an index is only usable
// for an ORDER BY whose collation it matches. icu_root exists only in the Plex
// engine, which is the engine this package drives.
var Managed = []Index{
	{
		Name:  Prefix + "section_type_title",
		Table: "metadata_items",
		Create: `CREATE INDEX IF NOT EXISTS ` + Prefix + `section_type_title ` +
			`ON metadata_items(library_section_id, metadata_type, title_sort COLLATE icu_root)`,
		Why: "library listings sorted the way PMS sorts them (7.7 ms -> 0.2 ms on 116k rows)",
	},
	{
		Name:   Prefix + "type_added",
		Table:  "metadata_items",
		Create: `CREATE INDEX IF NOT EXISTS ` + Prefix + `type_added ON metadata_items(metadata_type, added_at)`,
		Why:    "recently added listings per media type",
	},
}

// IndexStatus is one managed index and whether the database has it.
type IndexStatus struct {
	Name    string `json:"name"`
	Table   string `json:"table"`
	Present bool   `json:"present"`
	Why     string `json:"why,omitempty"`
}

// Status is the answer to "what does this database look like right now".
type Status struct {
	Managed []IndexStatus `json:"managed"`
	// Unknown lists zz_plexapi_ indexes present in the database that are not
	// in Managed, i.e. left over from an older version of this service.
	Unknown []string `json:"unknown,omitempty"`
	// SchemaVersion is PRAGMA schema_version, which changes on every DDL.
	SchemaVersion int64 `json:"schema_version"`
	// MaxMigration is max(version) of schema_migrations.
	MaxMigration string `json:"max_migration,omitempty"`
}

// AllPresent reports whether every managed index exists.
func (s Status) AllPresent() bool {
	for _, m := range s.Managed {
		if !m.Present {
			return false
		}
	}
	return len(s.Managed) > 0
}

// AnyPresent reports whether at least one managed (or leftover) index exists.
func (s Status) AnyPresent() bool {
	if len(s.Unknown) > 0 {
		return true
	}
	for _, m := range s.Managed {
		if m.Present {
			return true
		}
	}
	return false
}

// PresentNames lists the managed indexes that exist.
func (s Status) PresentNames() []string {
	var out []string
	for _, m := range s.Managed {
		if m.Present {
			out = append(out, m.Name)
		}
	}
	return out
}

// statusStatements is one script, so Status costs a single round trip.
var statusStatements = []sqlite.Statement{
	{SQL: "SELECT name FROM sqlite_master WHERE type = 'index' AND name LIKE '" + Prefix + "%' ORDER BY name"},
	{SQL: "PRAGMA schema_version"},
	{SQL: "SELECT max(version) AS max_version FROM schema_migrations"},
}

// GetStatus reads the current state of the managed indexes. It is one read
// round trip (three statements in one script) and is safe while PMS is up.
func GetStatus(ctx context.Context, drv *sqlite.Driver, dbPath string) (Status, error) {
	res, err := drv.Query(ctx, dbPath, statusStatements)
	if err != nil {
		return Status{}, err
	}
	present := map[string]bool{}
	if len(res) > 0 {
		for _, row := range res[0].Rows {
			if n, ok := row["name"].(string); ok {
				present[n] = true
			}
		}
	}
	st := Status{Managed: make([]IndexStatus, 0, len(Managed))}
	known := map[string]bool{}
	for _, m := range Managed {
		known[m.Name] = true
		st.Managed = append(st.Managed, IndexStatus{
			Name: m.Name, Table: m.Table, Present: present[m.Name], Why: m.Why,
		})
	}
	for name := range present {
		if !known[name] {
			st.Unknown = append(st.Unknown, name)
		}
	}
	sortStrings(st.Unknown)
	if len(res) > 1 && len(res[1].Rows) == 1 {
		st.SchemaVersion = asInt(res[1].Rows[0]["schema_version"])
	}
	if len(res) > 2 && len(res[2].Rows) == 1 {
		st.MaxMigration = asString(res[2].Rows[0]["max_version"])
	}
	return st, nil
}

// CreateResult reports how long each phase of Create took.
type CreateResult struct {
	Created  []string      `json:"created"`
	Build    time.Duration `json:"-"`
	Check    time.Duration `json:"-"`
	QuickChk string        `json:"quick_check"`
}

// Create creates every missing managed index, runs ANALYZE so the planner has
// statistics for them, and then verifies the database with
// PRAGMA quick_check(1).
//
// The CREATE INDEX statements and the ANALYZE go through the driver's Exec
// path, which wraps them in BEGIN IMMEDIATE ... COMMIT with -bail: any failure
// exits the shell before COMMIT, so a half-built set of indexes cannot be
// left behind. Both are legal inside a transaction.
//
// quick_check is deliberately a SEPARATE, read-only call: it is a whole
// database scan that has no business holding a write transaction open, and
// running it after COMMIT is what actually verifies the committed file.
func Create(ctx context.Context, drv *sqlite.Driver, dbPath string) (CreateResult, error) {
	var out CreateResult
	stmts := make([]sqlite.Statement, 0, len(Managed)+1)
	for _, m := range Managed {
		stmts = append(stmts, sqlite.Statement{SQL: m.Create})
		out.Created = append(out.Created, m.Name)
	}
	stmts = append(stmts, sqlite.Statement{SQL: "ANALYZE"})

	start := time.Now()
	if _, err := drv.Exec(ctx, dbPath, stmts); err != nil {
		return out, fmt.Errorf("creating managed indexes: %w", err)
	}
	out.Build = time.Since(start)

	start = time.Now()
	chk, err := quickCheck(ctx, drv, dbPath)
	out.Check = time.Since(start)
	out.QuickChk = chk
	if err != nil {
		return out, err
	}
	return out, nil
}

// Drop removes every managed index, and any leftover zz_plexapi_ index the
// database still carries from an older version of this service.
func Drop(ctx context.Context, drv *sqlite.Driver, dbPath string, extra ...string) ([]string, error) {
	names := make([]string, 0, len(Managed)+len(extra))
	for _, m := range Managed {
		names = append(names, m.Name)
	}
	for _, e := range extra {
		if strings.HasPrefix(e, Prefix) {
			names = append(names, e)
		}
	}
	stmts := make([]sqlite.Statement, 0, len(names))
	for _, n := range names {
		stmts = append(stmts, sqlite.Statement{SQL: `DROP INDEX IF EXISTS "` + n + `"`})
	}
	if len(stmts) == 0 {
		return nil, nil
	}
	if _, err := drv.Exec(ctx, dbPath, stmts); err != nil {
		return nil, fmt.Errorf("dropping managed indexes: %w", err)
	}
	return names, nil
}

// quickCheck runs PRAGMA quick_check(1) read-only and returns its single row.
func quickCheck(ctx context.Context, drv *sqlite.Driver, dbPath string) (string, error) {
	res, err := drv.Query(ctx, dbPath, []sqlite.Statement{{SQL: "PRAGMA quick_check(1)"}})
	if err != nil {
		return "", fmt.Errorf("quick_check: %w", err)
	}
	got := ""
	if len(res) > 0 && len(res[0].Rows) > 0 {
		for _, v := range res[0].Rows[0] {
			got = asString(v)
			break
		}
	}
	if got != "ok" {
		return got, fmt.Errorf("quick_check returned %q, want \"ok\"", got)
	}
	return got, nil
}

func asString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		return fmt.Sprint(t)
	}
}

func asInt(v any) int64 {
	var n int64
	switch t := v.(type) {
	case nil:
		return 0
	case float64:
		return int64(t)
	case int64:
		return t
	default:
		fmt.Sscan(asString(v), &n)
	}
	return n
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
