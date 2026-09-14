package api

import (
	"context"
	"net/http"

	"github.com/pwsh/plex_api/internal/indexes"
)

// indexesInfo is the managed-index view shared by /health and /v1/indexes.
type indexesInfo struct {
	Enabled       bool                  `json:"enabled"`
	PMSVersion    string                `json:"pms_version,omitempty"`
	State         *indexes.State        `json:"state"`
	Managed       []indexes.IndexStatus `json:"managed"`
	Unknown       []string              `json:"unknown,omitempty"`
	SchemaVersion int64                 `json:"schema_version,omitempty"`
	MaxMigration  string                `json:"max_migration,omitempty"`
	StateFile     string                `json:"state_file"`
	BackupDir     string                `json:"backup_dir"`
	Error         string                `json:"error,omitempty"`
}

// indexesInfo gathers the managed index status. The database part is a single
// read round trip; the state file is a single small read; the PMS version is
// cached for the life of the process, because the binary cannot change under a
// running container without a restart.
func (s *Server) indexesInfo(ctx context.Context) indexesInfo {
	info := indexesInfo{
		Enabled:    s.cfg.Indexes,
		PMSVersion: s.pmsVersion(ctx),
		StateFile:  indexes.StatePath(s.cfg.StateDir),
		BackupDir:  s.cfg.BackupDir,
		Managed:    []indexes.IndexStatus{},
	}
	if st, err := indexes.LoadState(s.cfg.StateDir); err != nil {
		info.Error = err.Error()
	} else {
		info.State = st
	}
	status, err := indexes.GetStatus(ctx, s.drv, s.cfg.MainDB())
	if err != nil {
		if info.Error == "" {
			info.Error = err.Error()
		}
		// Still report the definitions, all "not present but unknown".
		for _, m := range indexes.Managed {
			info.Managed = append(info.Managed, indexes.IndexStatus{Name: m.Name, Table: m.Table, Why: m.Why})
		}
		return info
	}
	info.Managed = status.Managed
	info.Unknown = status.Unknown
	info.SchemaVersion = status.SchemaVersion
	info.MaxMigration = status.MaxMigration
	return info
}

// pmsVersion is read once per process from the PMS binary.
func (s *Server) pmsVersion(ctx context.Context) string {
	s.pmsVerOnce.Do(func() {
		v, err := indexes.PMSVersion(ctx, s.cfg.PMSDir)
		if err != nil {
			v = ""
		}
		s.pmsVer = v
	})
	return s.pmsVer
}

// handleIndexes reports the managed index status. There are deliberately no
// create/drop endpoints: both need Plex stopped, so they are CLI modes
// (-ensure-indexes, -drop-indexes) run by the s6 oneshot before Plex starts.
func (s *Server) handleIndexes(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.QueryTimeout)
	defer cancel()
	info := s.indexesInfo(ctx)
	status := http.StatusOK
	if info.Error != "" {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, info)
}
