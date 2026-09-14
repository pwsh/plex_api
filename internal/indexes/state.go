package indexes

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// StateFileName is the file inside the state directory that records what was
// created and against which PMS version.
const StateFileName = "indexes.json"

// State is the on-disk record written after a successful create. Its only job
// is to make every later start cheap (and to notice a PMS upgrade).
type State struct {
	PMSVersion string   `json:"pms_version"`
	CreatedAt  int64    `json:"created_at"`
	Indexes    []string `json:"indexes"`
}

// StatePath is the full path of the state file inside dir.
func StatePath(dir string) string { return filepath.Join(dir, StateFileName) }

// LoadState reads the state file. A missing file is not an error: it returns
// (nil, nil), which means "never created".
func LoadState(dir string) (*State, error) {
	buf, err := os.ReadFile(StatePath(dir))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(buf, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", StatePath(dir), err)
	}
	return &s, nil
}

// SaveState writes the state file atomically (temp file plus rename), so a
// crash mid-write cannot leave a truncated file that later starts choke on.
func SaveState(dir string, s *State) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if s.CreatedAt == 0 {
		s.CreatedAt = time.Now().Unix()
	}
	buf, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	buf = append(buf, '\n')
	tmp := StatePath(dir) + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, StatePath(dir)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// RemoveState deletes the state file. A missing file is not an error.
func RemoveState(dir string) error {
	err := os.Remove(StatePath(dir))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
