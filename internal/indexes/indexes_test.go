package indexes

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestManagedDefinitions(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range Managed {
		if len(m.Name) <= len(Prefix) || m.Name[:len(Prefix)] != Prefix {
			t.Errorf("index %q does not carry the %q prefix", m.Name, Prefix)
		}
		if seen[m.Name] {
			t.Errorf("duplicate index name %q", m.Name)
		}
		seen[m.Name] = true
		if m.Table == "" {
			t.Errorf("index %q has no table", m.Name)
		}
		// The CREATE must be idempotent and must name the index, so that
		// Create can be re-run and Drop matches it.
		want := "CREATE INDEX IF NOT EXISTS " + m.Name + " "
		if len(m.Create) < len(want) || m.Create[:len(want)] != want {
			t.Errorf("index %q: create statement should start with %q, got %q", m.Name, want, m.Create)
		}
	}
}

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()

	got, err := LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState on an empty dir: %v", err)
	}
	if got != nil {
		t.Fatalf("LoadState on an empty dir = %+v, want nil", got)
	}

	want := &State{PMSVersion: "v1.43.4.10903-e5521bd8c", Indexes: []string{"a", "b"}}
	if err := SaveState(dir, want); err != nil {
		t.Fatal(err)
	}
	if want.CreatedAt == 0 {
		t.Fatal("SaveState should have stamped CreatedAt")
	}
	got, err = LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip: got %+v, want %+v", got, want)
	}

	// Creating it in a directory that does not exist yet must work, since
	// /config/plex-api is ours to make.
	nested := filepath.Join(dir, "a", "b")
	if err := SaveState(nested, &State{PMSVersion: "x"}); err != nil {
		t.Fatalf("SaveState into a new directory: %v", err)
	}
	if _, err := os.Stat(StatePath(nested)); err != nil {
		t.Fatal(err)
	}

	if err := RemoveState(dir); err != nil {
		t.Fatal(err)
	}
	if err := RemoveState(dir); err != nil {
		t.Fatalf("RemoveState must tolerate a missing file: %v", err)
	}
	got, err = LoadState(dir)
	if err != nil || got != nil {
		t.Fatalf("after RemoveState: got %+v, %v", got, err)
	}
}

func TestLoadStateCorrupt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(StatePath(dir), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(dir); err == nil {
		t.Fatal("want an error for a corrupt state file")
	}
}

func TestPrune(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	var stamps []string
	for i := 0; i < 5; i++ {
		name := base.Add(time.Duration(i) * time.Hour).Format(backupStampLayout)
		stamps = append(stamps, name)
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Things that are not backups must survive untouched.
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "not-a-stamp"), 0o755); err != nil {
		t.Fatal(err)
	}

	removed, err := Prune(dir, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed %v, want the 2 oldest", removed)
	}
	for _, s := range stamps[:2] {
		if _, err := os.Stat(filepath.Join(dir, s)); !os.IsNotExist(err) {
			t.Errorf("%s should have been pruned", s)
		}
	}
	for _, s := range stamps[2:] {
		if _, err := os.Stat(filepath.Join(dir, s)); err != nil {
			t.Errorf("%s should have been kept: %v", s, err)
		}
	}
	for _, keep := range []string{"README", "not-a-stamp"} {
		if _, err := os.Stat(filepath.Join(dir, keep)); err != nil {
			t.Errorf("%s should not have been touched: %v", keep, err)
		}
	}

	// Idempotent, and keep<=0 means "keep everything".
	if removed, err = Prune(dir, 3); err != nil || len(removed) != 0 {
		t.Fatalf("second Prune: %v, %v", removed, err)
	}
	if removed, err = Prune(dir, 0); err != nil || len(removed) != 0 {
		t.Fatalf("Prune(0): %v, %v", removed, err)
	}
	if removed, err = Prune(filepath.Join(dir, "missing"), 3); err != nil || removed != nil {
		t.Fatalf("Prune on a missing dir: %v, %v", removed, err)
	}
}

func TestEscapeSQL(t *testing.T) {
	if got := escapeSQL("/a/b'c/d.db"); got != "/a/b''c/d.db" {
		t.Fatalf("escapeSQL = %q", got)
	}
}

func TestStatusHelpers(t *testing.T) {
	s := Status{Managed: []IndexStatus{{Name: "a"}, {Name: "b"}}}
	if s.AllPresent() || s.AnyPresent() {
		t.Fatal("none present")
	}
	s.Managed[0].Present = true
	if s.AllPresent() || !s.AnyPresent() {
		t.Fatal("one present")
	}
	s.Managed[1].Present = true
	if !s.AllPresent() {
		t.Fatal("all present")
	}
	if got := s.PresentNames(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("PresentNames = %v", got)
	}
	if (Status{}).AllPresent() {
		t.Fatal("an empty status must not claim everything is present")
	}
	if !(Status{Unknown: []string{"zz_plexapi_old"}}).AnyPresent() {
		t.Fatal("a leftover index counts as present")
	}
}
