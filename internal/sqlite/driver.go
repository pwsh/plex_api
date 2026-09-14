// Package sqlite drives the SQLite shell bundled with Plex Media Server as a
// subprocess. No cgo and no SQLite driver are involved.
//
// The shell binary ("Plex SQLite") is an 11 KB stub that re-executes the
// "Plex Media Server" binary in sqlite3-shell mode. That server binary is the
// only place where Plex's "collating" FTS4 tokenizer and "icu_root" collation
// exist, so it is the only engine that can query fts4_*_icu or write to
// metadata_items / tags. It needs LD_LIBRARY_PATH pointing at the install's
// lib directory.
package sqlite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// sentinel frames statement output. It is emitted with the .print dot-command
// before every statement, so stdout can be split into per-statement segments.
const sentinel = "--plex-api-stmt--"

// ShellName is the file name of the shell inside the PMS install directory.
const ShellName = "Plex SQLite"

// Statement is one SQL statement plus its named parameters.
type Statement struct {
	SQL    string         `json:"sql"`
	Params map[string]any `json:"params,omitempty"`
}

// Result is the output of one statement.
//
// Raw is the JSON array exactly as the shell printed it (nil when the
// statement produced no result set). Rows and Columns are filled in by
// Decode, which Query does eagerly and QueryRaw leaves to the caller, so that
// large result sets can be passed through to HTTP clients without a decode and
// re-encode round trip.
type Result struct {
	Raw     json.RawMessage  `json:"-"`
	Columns []string         `json:"columns,omitempty"`
	Rows    []map[string]any `json:"rows"`
}

// JSON returns the raw array, or "[]" for an empty result set.
func (r Result) JSON() json.RawMessage {
	if len(r.Raw) == 0 {
		return json.RawMessage("[]")
	}
	return r.Raw
}

// Elements splits the raw array into its elements without decoding them.
func (r Result) Elements() ([]json.RawMessage, error) {
	if len(r.Raw) == 0 {
		return nil, nil
	}
	var out []json.RawMessage
	if err := json.Unmarshal(r.Raw, &out); err != nil {
		return nil, fmt.Errorf("decoding shell output: %w", err)
	}
	return out, nil
}

// Count is the number of rows in the result. The shell's JSON mode writes one
// row per line, so for a raw array the count is the number of lines.
func (r Result) Count() int {
	if r.Rows != nil {
		return len(r.Rows)
	}
	if len(r.Raw) == 0 || string(r.Raw) == "[]" {
		return 0
	}
	return bytes.Count(r.Raw, []byte{'\n'}) + 1
}

// ColumnNames returns the keys of the first row in the order the shell wrote
// them, which is the SELECT's column order.
func (r Result) ColumnNames() []string {
	if r.Columns != nil {
		return r.Columns
	}
	if len(r.Raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(r.Raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
		return nil
	}
	if !dec.More() {
		return nil
	}
	_, cols, err := decodeObject(dec)
	if err != nil {
		return nil
	}
	return cols
}

// Decode fills Rows and Columns from Raw. It is idempotent. Numbers are kept
// as json.Number so 64-bit ids survive the round trip.
func (r *Result) Decode() error {
	if r.Rows != nil || len(r.Raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(r.Raw))
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
		return fmt.Errorf("unexpected shell output %q", truncate(string(r.Raw), 120))
	}
	r.Rows = []map[string]any{}
	seen := map[string]bool{}
	for dec.More() {
		row, cols, err := decodeObject(dec)
		if err != nil {
			return err
		}
		for _, c := range cols {
			if !seen[c] {
				seen[c] = true
				r.Columns = append(r.Columns, c)
			}
		}
		r.Rows = append(r.Rows, row)
	}
	return nil
}

func decodeAll(segs [][]Result) error {
	for _, seg := range segs {
		for i := range seg {
			if err := seg[i].Decode(); err != nil {
				return err
			}
		}
	}
	return nil
}

// Driver runs SQL through the Plex SQLite shell. Writes always get a private
// subprocess. Reads go through a pool of long-lived read-only shells when
// PoolSize is greater than zero, which saves the ~16 ms of process startup.
type Driver struct {
	// ShellPath is the absolute path of the "Plex SQLite" binary.
	ShellPath string
	// LibDir is put on LD_LIBRARY_PATH.
	LibDir string
	// BusyTimeoutMS is applied with .timeout on every script.
	BusyTimeoutMS int
	// QueryTimeout kills the subprocess if it runs longer.
	QueryTimeout time.Duration
	// PoolSize is the number of persistent read-only shells per database.
	// Zero disables pooling and spawns a process per request.
	PoolSize int
	// PoolMaxUses recycles a pooled shell after this many requests, bounding
	// any state a request might have left behind. Zero means never.
	PoolMaxUses int

	mu     sync.Mutex
	pools  map[string]*Pool
	closed bool
}

// New builds a Driver for a PMS install directory. poolSize of 0 disables the
// read pool.
func New(pmsDir string, busyTimeoutMS int, queryTimeout time.Duration, poolSize, poolMaxUses int) *Driver {
	return &Driver{
		ShellPath:     filepath.Join(pmsDir, ShellName),
		LibDir:        filepath.Join(pmsDir, "lib"),
		BusyTimeoutMS: busyTimeoutMS,
		QueryTimeout:  queryTimeout,
		PoolSize:      poolSize,
		PoolMaxUses:   poolMaxUses,
	}
}

// pool returns the pool for one database path, creating it on first use.
func (d *Driver) pool(dbPath string) *Pool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.PoolSize <= 0 {
		return nil
	}
	if d.pools == nil {
		d.pools = map[string]*Pool{}
	}
	p := d.pools[dbPath]
	if p == nil {
		p = newPool(d, dbPath, d.PoolSize, d.PoolMaxUses)
		d.pools[dbPath] = p
	}
	return p
}

// Warm pre-starts the pooled shells for one database so the first requests
// do not pay for process start. No-op when pooling is disabled.
func (d *Driver) Warm(dbPath string) {
	if p := d.pool(dbPath); p != nil {
		p.Warm()
	}
}

// PoolStats reports one entry per live pool, for /health.
func (d *Driver) PoolStats() []PoolStats {
	d.mu.Lock()
	pools := make([]*Pool, 0, len(d.pools))
	for _, p := range d.pools {
		pools = append(pools, p)
	}
	d.mu.Unlock()
	out := make([]PoolStats, 0, len(pools))
	for _, p := range pools {
		out = append(out, p.Stats())
	}
	sortStatsByDB(out)
	return out
}

func sortStatsByDB(s []PoolStats) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].DB < s[j-1].DB; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// Close shuts down every pooled shell. Spawn-per-request calls are unaffected.
func (d *Driver) Close() {
	d.mu.Lock()
	d.closed = true
	pools := make([]*Pool, 0, len(d.pools))
	for _, p := range d.pools {
		pools = append(pools, p)
	}
	d.pools = nil
	d.mu.Unlock()
	for _, p := range pools {
		p.Close()
	}
}

// ErrNoStatements is returned when a call carries no SQL.
var ErrNoStatements = errors.New("no statements")

// Query runs statements read-only (-readonly -bail) and returns one Result per
// statement.
func (d *Driver) Query(ctx context.Context, dbPath string, stmts []Statement) ([]Result, error) {
	res, err := d.QueryRaw(ctx, dbPath, stmts)
	if err != nil {
		return nil, err
	}
	for i := range res {
		if err := res[i].Decode(); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// QueryRaw is Query without decoding: each Result carries only Raw. Use it
// when the rows are going straight back out as JSON.
func (d *Driver) QueryRaw(ctx context.Context, dbPath string, stmts []Statement) ([]Result, error) {
	segs, err := d.readSegments(ctx, dbPath, stmts, len(stmts), true)
	if err != nil {
		return nil, err
	}
	return firstOfEach(segs), nil
}

// QueryAll is Query for a single SQL string that may hold several statements.
// It returns every result set the shell produced, in order, decoded.
// Caller-supplied SQL that changes connection state (ATTACH, DETACH, PRAGMA)
// is never run on a pooled shell; it gets a private process instead.
func (d *Driver) QueryAll(ctx context.Context, dbPath string, stmt Statement) ([]Result, error) {
	res, err := d.QueryAllRaw(ctx, dbPath, stmt)
	if err != nil {
		return nil, err
	}
	for i := range res {
		if err := res[i].Decode(); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// QueryAllRaw is QueryAll without decoding.
func (d *Driver) QueryAllRaw(ctx context.Context, dbPath string, stmt Statement) ([]Result, error) {
	segs, err := d.readSegments(ctx, dbPath, []Statement{stmt}, 1, !NeedsIsolation(stmt.SQL))
	if err != nil {
		return nil, err
	}
	var out []Result
	for _, seg := range segs {
		out = append(out, seg...)
	}
	if len(out) == 0 {
		out = []Result{{}}
	}
	return out, nil
}

// Exec runs statements read-write inside BEGIN IMMEDIATE ... COMMIT. With
// -bail the shell exits at the first error before reaching COMMIT, so the
// transaction is rolled back by the process exiting and nothing is committed.
// A "SELECT changes()" is appended after each statement, so the returned slice
// holds 2*len(stmts) results: statement output then its changes count.
func (d *Driver) Exec(ctx context.Context, dbPath string, stmts []Statement) ([]Result, error) {
	script, err := buildScript(d.BusyTimeoutMS, stmts, true)
	if err != nil {
		return nil, err
	}
	segs, err := d.runSegments(ctx, dbPath, script, false, 2*len(stmts))
	if err != nil {
		return nil, err
	}
	if err := decodeAll(segs); err != nil {
		return nil, err
	}
	return firstOfEach(segs), nil
}

// firstOfEach reduces one segment to its first result set, which is all a
// single-statement segment can produce.
func firstOfEach(segs [][]Result) []Result {
	out := make([]Result, len(segs))
	for i, seg := range segs {
		if len(seg) > 0 {
			out[i] = seg[0]
		}
	}
	return out
}

// readSegments runs read-only statements, through the pool when one is
// enabled and pooling is allowed for this SQL, otherwise by spawning.
func (d *Driver) readSegments(ctx context.Context, dbPath string, stmts []Statement, want int, mayPool bool) ([][]Result, error) {
	if mayPool {
		if p := d.pool(dbPath); p != nil {
			body, err := buildBody(stmts, false)
			if err != nil {
				return nil, err
			}
			parts, err := p.run(ctx, body)
			if err != nil {
				return nil, err
			}
			return decodeSegments(parts, want)
		}
	}
	script, err := buildScript(d.BusyTimeoutMS, stmts, false)
	if err != nil {
		return nil, err
	}
	return d.runSegments(ctx, dbPath, script, true, want)
}

func (d *Driver) runSegments(ctx context.Context, dbPath, script string, readonly bool, want int) ([][]Result, error) {
	out, err := d.run(ctx, dbPath, script, readonly)
	if err != nil {
		return nil, err
	}
	return splitResults(out, want)
}

// scriptHeader is the preamble a shell needs once: it is written at the top of
// every spawned script and once at startup for a pooled shell.
func scriptHeader(busyMS int) string {
	return fmt.Sprintf(".timeout %d\n.mode json\n.headers off\n", busyMS)
}

func buildScript(busyMS int, stmts []Statement, write bool) (string, error) {
	body, err := buildBody(stmts, write)
	if err != nil {
		return "", err
	}
	return scriptHeader(busyMS) + body, nil
}

// buildBody is the per-request part of the script: parameters, sentinels and
// the statements themselves.
func buildBody(stmts []Statement, write bool) (string, error) {
	if len(stmts) == 0 {
		return "", ErrNoStatements
	}
	var b strings.Builder
	if write {
		b.WriteString("BEGIN IMMEDIATE;\n")
	}
	for _, st := range stmts {
		sql := strings.TrimSpace(st.SQL)
		if sql == "" {
			return "", errors.New("empty sql statement")
		}
		if strings.HasPrefix(sql, ".") {
			return "", errors.New("dot-commands are not allowed in statements")
		}
		// Parameters are set immediately before their statement so that
		// per-statement maps cannot collide.
		names := make([]string, 0, len(st.Params))
		for k := range st.Params {
			names = append(names, k)
		}
		sortStrings(names)
		for _, name := range names {
			if !ValidParamName(name) {
				return "", fmt.Errorf("invalid parameter name %q", name)
			}
			lit, err := Literal(st.Params[name])
			if err != nil {
				return "", fmt.Errorf("parameter %q: %w", name, err)
			}
			fmt.Fprintf(&b, ".parameter set :%s %s\n", name, lit)
		}
		fmt.Fprintf(&b, ".print %s\n", sentinel)
		b.WriteString(sql)
		if !strings.HasSuffix(sql, ";") {
			b.WriteString(";")
		}
		b.WriteString("\n")
		if write {
			fmt.Fprintf(&b, ".print %s\n", sentinel)
			b.WriteString("SELECT changes() AS changes;\n")
		}
	}
	if write {
		b.WriteString("COMMIT;\n")
	}
	return b.String(), nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func (d *Driver) run(ctx context.Context, dbPath, script string, readonly bool) (string, error) {
	if d.ShellPath == "" {
		return "", errors.New("sqlite: shell path not configured")
	}
	timeout := d.QueryTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{"-bail"}
	if readonly {
		args = append(args, "-readonly")
	}
	args = append(args, dbPath)

	cmd := exec.CommandContext(ctx, d.ShellPath, args...)
	cmd.Env = shellEnv(d.LibDir)
	cmd.Stdin = strings.NewReader(script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if ctxErr := ctx.Err(); errors.Is(ctxErr, context.DeadlineExceeded) {
		return "", &Error{Kind: "timeout", Message: fmt.Sprintf("query exceeded %s", timeout)}
	}
	if err != nil {
		if perr := ParseError(stderr.String()); perr != nil {
			return "", perr
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", &Error{Kind: "shell", Message: "shell exited with status " + strconv.Itoa(ee.ExitCode())}
		}
		return "", &Error{Kind: "shell", Message: err.Error()}
	}
	// -bail means a zero exit status implies no error, but the shell can still
	// warn on stderr; surface a genuine diagnostic if one is there.
	if perr := ParseError(stderr.String()); perr != nil && perr.Kind != "shell" {
		return "", perr
	}
	return stdout.String(), nil
}

func shellEnv(libDir string) []string {
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

// splitResults splits sentinel-framed stdout into per-statement segments. A
// segment holds one result set per SELECT the statement text produced, so it
// is usually empty or of length one.
func splitResults(out string, want int) ([][]Result, error) {
	parts := strings.Split(out, sentinel+"\n")
	if len(parts) > 0 {
		parts = parts[1:] // text before the first sentinel is not statement output
	}
	return decodeSegments(parts, want)
}

// decodeSegments turns per-statement output texts into Results, padding to
// want when the shell produced fewer segments than statements.
func decodeSegments(parts []string, want int) ([][]Result, error) {
	segments := make([][]Result, 0, want)
	for _, p := range parts {
		seg, err := decodeSegment(p)
		if err != nil {
			return nil, err
		}
		segments = append(segments, seg)
	}
	// Pad only if the shell produced fewer segments than statements.
	for len(segments) < want {
		segments = append(segments, nil)
	}
	return segments, nil
}

// decodeSegment splits the text the shell printed for one segment into one
// Result per JSON array, keeping each array as raw bytes. A segment usually
// holds zero or one array; a multi-statement SQL string can produce several.
func decodeSegment(seg string) ([]Result, error) {
	seg = strings.TrimSpace(seg)
	if seg == "" {
		return nil, nil
	}
	// Fast path: one array. The shell writes one row per line and only ever
	// separates two arrays with "]\n[", and a newline inside a value is
	// escaped, so this test cannot misfire on row content.
	if seg[0] == '[' && seg[len(seg)-1] == ']' && !strings.Contains(seg, "]\n[") {
		return []Result{{Raw: json.RawMessage(seg)}}, nil
	}
	dec := json.NewDecoder(strings.NewReader(seg))
	var out []Result
	for {
		var raw json.RawMessage
		err := dec.Decode(&raw)
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("decoding shell output: %w", err)
		}
		if len(raw) == 0 || raw[0] != '[' {
			return nil, fmt.Errorf("unexpected shell output %q", truncate(seg, 120))
		}
		out = append(out, Result{Raw: raw})
	}
}

func decodeObject(dec *json.Decoder) (map[string]any, []string, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, nil, fmt.Errorf("expected object in shell output, got %v", tok)
	}
	row := map[string]any{}
	var cols []string
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		key, ok := kt.(string)
		if !ok {
			return nil, nil, fmt.Errorf("expected column name in shell output, got %v", kt)
		}
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, nil, err
		}
		row[key] = v
		cols = append(cols, key)
	}
	if _, err := dec.Token(); err != nil { // closing }
		return nil, nil, err
	}
	return row, cols, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
