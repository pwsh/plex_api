package api

import (
	"context"
	"net/http"
	"os"

	"github.com/pwsh/plex_api/internal/sqlite"
)

type dbStatus struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	Size   int64  `json:"size_bytes,omitempty"`
}

type healthResponse struct {
	Status        string              `json:"status"`
	Version       string              `json:"version"`
	Shell         string              `json:"shell"`
	ShellExists   bool                `json:"shell_exists"`
	SQLiteVersion string              `json:"sqlite_version,omitempty"`
	Databases     map[string]dbStatus `json:"databases"`
	Migrations    *migrationInfo      `json:"schema_migrations,omitempty"`
	PMSAddr       string              `json:"pms_addr"`
	PMSRunning    bool                `json:"pms_running"`
	Pool          []sqlite.PoolStats  `json:"pool"`
	Indexes       *indexesInfo        `json:"indexes,omitempty"`
	Write         writePolicy         `json:"write_policy"`
	Error         string              `json:"error,omitempty"`
}

type migrationInfo struct {
	Count      int64  `json:"count"`
	MaxVersion string `json:"max_version"`
}

type writePolicy struct {
	Enabled            bool `json:"enabled"`
	AllowWhileRunning  bool `json:"allow_while_running"`
	WritesPermittedNow bool `json:"writes_permitted_now"`
}

// handleHealth reports whether the service can reach the shell and the
// databases. It never requires authentication.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	resp := healthResponse{
		Status:    "ok",
		Version:   s.version,
		Shell:     s.cfg.ShellPath(),
		Databases: map[string]dbStatus{},
		PMSAddr:   s.cfg.PMSAddr,
	}
	if fi, err := os.Stat(s.cfg.ShellPath()); err == nil && !fi.IsDir() {
		resp.ShellExists = true
	}
	for name, path := range map[string]string{"main": s.cfg.MainDB(), "blobs": s.cfg.BlobsDB()} {
		st := dbStatus{Path: path}
		if fi, err := os.Stat(path); err == nil {
			st.Exists = true
			st.Size = fi.Size()
		}
		resp.Databases[name] = st
	}
	resp.PMSRunning = s.pmsRunning()
	resp.Pool = s.drv.PoolStats()
	resp.Write = writePolicy{
		Enabled:            s.cfg.AllowWrite,
		AllowWhileRunning:  s.cfg.WriteWhileRunning,
		WritesPermittedNow: s.cfg.AllowWrite && (s.cfg.WriteWhileRunning || !resp.PMSRunning),
	}

	if resp.ShellExists && resp.Databases["main"].Exists {
		ctx, cancel := context.WithTimeout(r.Context(), s.cfg.QueryTimeout)
		defer cancel()
		res, err := query(ctx, s.drv, s.cfg.MainDB(),
			sqlite.Statement{SQL: "SELECT sqlite_version() AS v"},
			sqlite.Statement{SQL: "SELECT count(*) AS count, max(version) AS max_version FROM schema_migrations"},
		)
		if err != nil {
			resp.Status = "degraded"
			resp.Error = err.Error()
		} else {
			if len(res) > 0 && len(res[0].Rows) == 1 {
				resp.SQLiteVersion = asString(res[0].Rows[0]["v"])
			}
			if len(res) > 1 && len(res[1].Rows) == 1 {
				row := res[1].Rows[0]
				resp.Migrations = &migrationInfo{
					Count:      asInt(row["count"]),
					MaxVersion: asString(row["max_version"]),
				}
			}
		}
		info := s.indexesInfo(ctx)
		resp.Indexes = &info
	} else {
		resp.Status = "degraded"
		if !resp.ShellExists {
			resp.Error = "Plex SQLite shell not found at " + s.cfg.ShellPath()
		} else {
			resp.Error = "library database not found at " + s.cfg.MainDB()
		}
	}

	status := http.StatusOK
	if resp.Status != "ok" {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, resp)
}
