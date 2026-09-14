// Package config reads the service configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Config holds every tunable of the service.
type Config struct {
	Bind              string
	PMSDir            string
	DBDir             string
	Token             string
	AllowWrite        bool
	WriteWhileRunning bool
	PMSAddr           string
	BusyTimeoutMS     int
	QueryTimeout      time.Duration
	PoolSize          int
	PoolMaxUses       int

	// Managed indexes (see internal/indexes).
	Indexes     bool
	IndexBackup bool
	StateDir    string
	BackupDir   string
	BackupKeep  int
}

// Defaults.
const (
	DefaultBind          = "0.0.0.0:32500"
	DefaultPMSDir        = "/usr/lib/plexmediaserver"
	DefaultDBDir         = "/config/Library/Application Support/Plex Media Server/Plug-in Support/Databases"
	DefaultPMSAddr       = "127.0.0.1:32400"
	DefaultBusyTimeoutMS = 5000
	DefaultQueryTimeout  = 30 * time.Second
	DefaultPoolSize      = 4
	DefaultPoolMaxUses   = 1000
	DefaultStateDir      = "/config/plex-api"
	DefaultBackupKeep    = 3
	// BackupDirName is appended to the state directory when
	// PLEX_API_BACKUP_DIR is unset, giving /config/plex-api/backups.
	BackupDirName = "backups"

	MainDBName  = "com.plexapp.plugins.library.db"
	BlobsDBName = "com.plexapp.plugins.library.blobs.db"
)

// Load builds a Config from the environment, applying defaults.
func Load() (*Config, error) {
	c := &Config{
		Bind:          env("PLEX_API_BIND", DefaultBind),
		PMSDir:        env("PLEX_API_PMS_DIR", DefaultPMSDir),
		DBDir:         env("PLEX_API_DB_DIR", DefaultDBDir),
		Token:         os.Getenv("PLEX_API_TOKEN"),
		PMSAddr:       env("PLEX_API_PMS_ADDR", DefaultPMSAddr),
		BusyTimeoutMS: DefaultBusyTimeoutMS,
		QueryTimeout:  DefaultQueryTimeout,
		PoolSize:      DefaultPoolSize,
		PoolMaxUses:   DefaultPoolMaxUses,
		StateDir:      env("PLEX_API_STATE_DIR", DefaultStateDir),
		BackupKeep:    DefaultBackupKeep,
	}
	c.BackupDir = env("PLEX_API_BACKUP_DIR", filepath.Join(c.StateDir, BackupDirName))
	var err error
	if c.AllowWrite, err = envBool("PLEX_API_WRITE", false); err != nil {
		return nil, err
	}
	if c.WriteWhileRunning, err = envBool("PLEX_API_WRITE_WHILE_RUNNING", false); err != nil {
		return nil, err
	}
	if c.Indexes, err = envBool("PLEX_API_INDEXES", false); err != nil {
		return nil, err
	}
	if c.IndexBackup, err = envBool("PLEX_API_INDEX_BACKUP", true); err != nil {
		return nil, err
	}
	if c.BackupKeep, err = envInt("PLEX_API_BACKUP_KEEP", DefaultBackupKeep); err != nil {
		return nil, err
	}
	if v := os.Getenv("PLEX_API_TIMEOUT_MS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("PLEX_API_TIMEOUT_MS: want a non-negative integer, got %q", v)
		}
		c.BusyTimeoutMS = n
	}
	if c.PoolSize, err = envInt("PLEX_API_POOL", DefaultPoolSize); err != nil {
		return nil, err
	}
	if c.PoolMaxUses, err = envInt("PLEX_API_POOL_MAX_USES", DefaultPoolMaxUses); err != nil {
		return nil, err
	}
	if v := os.Getenv("PLEX_API_QUERY_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("PLEX_API_QUERY_TIMEOUT: want a positive Go duration such as 30s, got %q", v)
		}
		c.QueryTimeout = d
	}
	return c, nil
}

// ShellPath is the path of the "Plex SQLite" binary.
func (c *Config) ShellPath() string { return filepath.Join(c.PMSDir, "Plex SQLite") }

// MainDB is the path of the library database.
func (c *Config) MainDB() string { return filepath.Join(c.DBDir, MainDBName) }

// BlobsDB is the path of the blobs database.
func (c *Config) BlobsDB() string { return filepath.Join(c.DBDir, BlobsDBName) }

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envInt reads a non-negative integer setting.
func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s: want a non-negative integer, got %q", key, v)
	}
	return n, nil
}

func envBool(key string, def bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	switch v {
	case "1", "true", "TRUE", "True", "yes", "on":
		return true, nil
	case "0", "false", "FALSE", "False", "no", "off":
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: want a boolean, got %q", key, v)
	}
	return b, nil
}
