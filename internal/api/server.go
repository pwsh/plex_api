// Package api implements the HTTP surface of plex-api.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pwsh/plex_api/internal/config"
	"github.com/pwsh/plex_api/internal/sqlite"
)

// Server wires the configuration, the shell driver and the router together.
type Server struct {
	cfg     *config.Config
	drv     *sqlite.Driver
	version string
	mux     *http.ServeMux
	logger  *log.Logger
	meta    metaCache

	// The PMS version is read from the binary once and then cached: it costs
	// a process start and cannot change without a container restart.
	pmsVerOnce sync.Once
	pmsVer     string
}

// New builds a Server.
func New(cfg *config.Config, version string) *Server {
	s := &Server{
		cfg:     cfg,
		drv:     sqlite.New(cfg.PMSDir, cfg.BusyTimeoutMS, cfg.QueryTimeout, cfg.PoolSize, cfg.PoolMaxUses),
		version: version,
		mux:     http.NewServeMux(),
		logger:  log.New(os.Stdout, "", log.LstdFlags),
	}
	s.routes()
	// Pre-start the read shells for the main database so the first requests
	// do not pay for process start.
	go s.drv.Warm(cfg.MainDB())
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("GET /v1/schema", s.handleSchema)
	s.mux.HandleFunc("GET /v1/schema/{table}", s.handleSchemaTable)
	s.mux.HandleFunc("GET /v1/tables/{table}", s.handleTable)
	s.mux.HandleFunc("GET /v1/items/{id}", s.handleItem)
	s.mux.HandleFunc("GET /v1/recent", s.handleRecent)
	s.mux.HandleFunc("GET /v1/search", s.handleSearch)
	s.mux.HandleFunc("GET /v1/tags/search", s.handleTagSearch)
	s.mux.HandleFunc("GET /v1/indexes", s.handleIndexes)
	s.mux.HandleFunc("POST /v1/query", s.handleQuery)
	s.mux.HandleFunc("POST /v1/exec", s.handleExec)
	s.mux.HandleFunc("GET /v1/settings/{guid}", s.handleGetSettings)
	s.mux.HandleFunc("PATCH /v1/settings/{guid}", s.handlePatchSettings)
}

// Close releases the pooled shells. It is called on shutdown.
func (s *Server) Close() { s.drv.Close() }

// Handler returns the fully decorated HTTP handler (auth + request logging).
func (s *Server) Handler() http.Handler {
	return s.logging(s.auth(s.mux))
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	n      int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.n += n
	return n, err
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		path := r.URL.Path
		if r.URL.RawQuery != "" {
			path += "?" + r.URL.RawQuery
		}
		s.logger.Printf("%s %s %d %s %dB", r.Method, path, rec.status, time.Since(start).Round(time.Microsecond), rec.n)
	})
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Token == "" || r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		tok := r.Header.Get("X-Plex-Api-Token")
		if tok == "" {
			if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
				tok = strings.TrimSpace(strings.TrimPrefix(a, "Bearer "))
			}
		}
		if subtleEqual(tok, s.cfg.Token) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="plex-api"`)
		writeError(w, http.StatusUnauthorized, "unauthorized", "a valid API token is required")
	})
}

// subtleEqual is a constant-time comparison that does not leak the token length
// through early exit.
func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

// --- responses -------------------------------------------------------------

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Line    int    `json:"line,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encode_failed", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(buf)))
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}

// field is one key/value pair of a response envelope written by writeFields.
type field struct {
	Key string
	// Raw, when set, is spliced into the output verbatim; Value is marshalled.
	Raw   json.RawMessage
	Value any
}

// writeFields writes a JSON object without re-encoding large raw values:
// json.Marshal would re-validate and compact every json.RawMessage, which on a
// multi-megabyte page is a full extra pass over the bytes.
func writeFields(w http.ResponseWriter, status int, fields []field) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, _ := json.Marshal(f.Key)
		buf.Write(k)
		buf.WriteByte(':')
		if f.Raw != nil {
			buf.Write(f.Raw)
			continue
		}
		v, err := json.Marshal(f.Value)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "encode_failed", err.Error())
			return
		}
		buf.Write(v)
	}
	buf.WriteByte('}')
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]apiError{"error": {Code: code, Message: msg}})
}

// writeSQLError maps a driver error onto an HTTP status and JSON body.
func writeSQLError(w http.ResponseWriter, err error) {
	var se *sqlite.Error
	if errors.As(err, &se) {
		status := http.StatusBadRequest
		code := "sql_error"
		switch se.Kind {
		case "timeout":
			status, code = http.StatusGatewayTimeout, "timeout"
		case "shell":
			status, code = http.StatusInternalServerError, "shell_error"
		}
		if strings.Contains(se.Message, "database is locked") || strings.Contains(se.Message, "database table is locked") {
			status, code = http.StatusConflict, "database_locked"
		}
		if strings.Contains(se.Message, "readonly database") {
			status, code = http.StatusForbidden, "readonly"
		}
		writeJSON(w, status, map[string]apiError{"error": {Code: code, Message: se.Message, Line: se.Line}})
		return
	}
	writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
}

// --- helpers ---------------------------------------------------------------

// dbPath resolves the ?db= / "db" body field to a file path.
func (s *Server) dbPath(which string) (string, error) {
	switch which {
	case "", "main", "library":
		return s.cfg.MainDB(), nil
	case "blobs":
		return s.cfg.BlobsDB(), nil
	}
	return "", fmt.Errorf("unknown database %q (use \"main\" or \"blobs\")", which)
}

// pmsRunning reports whether something is listening on the PMS address.
func (s *Server) pmsRunning() bool {
	conn, err := net.DialTimeout("tcp", s.cfg.PMSAddr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// checkWritePolicy enforces PLEX_API_WRITE and PLEX_API_WRITE_WHILE_RUNNING.
// It writes the error response itself and returns false when the write must
// not proceed.
func (s *Server) checkWritePolicy(w http.ResponseWriter) bool {
	if !s.cfg.AllowWrite {
		writeError(w, http.StatusForbidden, "writes_disabled",
			"writes are disabled; set PLEX_API_WRITE=true to enable them")
		return false
	}
	if !s.cfg.WriteWhileRunning && s.pmsRunning() {
		writeError(w, http.StatusConflict, "pms_running",
			"Plex Media Server appears to be running on "+s.cfg.PMSAddr+
				"; stop it or set PLEX_API_WRITE_WHILE_RUNNING=true")
		return false
	}
	return true
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func query(ctx context.Context, d *sqlite.Driver, db string, stmts ...sqlite.Statement) ([]sqlite.Result, error) {
	return d.Query(ctx, db, stmts)
}

func intParam(r *http.Request, name string, def, min, max int) (int, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	if n < min {
		n = min
	}
	if n > max {
		n = max
	}
	return n, nil
}
