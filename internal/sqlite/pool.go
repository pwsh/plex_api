package sqlite

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// endMarkerPrefix starts the unique line a pooled shell prints when it has
// finished a request. The nonce that follows makes it impossible for query
// output to be mistaken for the marker.
const endMarkerPrefix = "--plex-api-end-"

// maxLine is the largest single line of shell output the reader accepts. The
// JSON writer puts one row per line, so this only needs to survive a very wide
// row.
const maxLine = 64 << 20

// Pool owns a fixed number of long-lived "Plex SQLite" shells for one database
// path. Every shell runs with -readonly -bail, so the pool is for reads only;
// writes keep spawning a private process.
//
// A shell is a slot in a buffered channel. A nil slot means "no process yet":
// processes are started lazily on first use and after a shell is discarded, so
// a pool that is never used costs nothing.
type Pool struct {
	drv     *Driver
	dbPath  string
	maxUses int
	slots   chan *shell

	spawned  atomic.Int64
	recycled atomic.Int64

	closeOnce sync.Once
	closed    atomic.Bool
}

// PoolStats is the /health view of one pool.
type PoolStats struct {
	DB            string `json:"db"`
	Size          int    `json:"size"`
	Idle          int    `json:"idle"`
	SpawnedTotal  int64  `json:"spawned_total"`
	RecycledTotal int64  `json:"recycled_total"`
}

func newPool(d *Driver, dbPath string, size, maxUses int) *Pool {
	p := &Pool{drv: d, dbPath: dbPath, maxUses: maxUses, slots: make(chan *shell, size)}
	for i := 0; i < size; i++ {
		p.slots <- nil
	}
	return p
}

// Stats reports the pool's current state.
func (p *Pool) Stats() PoolStats {
	return PoolStats{
		DB:            p.dbPath,
		Size:          cap(p.slots),
		Idle:          len(p.slots),
		SpawnedTotal:  p.spawned.Load(),
		RecycledTotal: p.recycled.Load(),
	}
}

// shell is one persistent subprocess.
type shell struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  chan string
	nonce  string
	uses   int
	lineNo int // input lines written so far, for error line-number mapping

	mu     sync.Mutex
	stderr strings.Builder
}

func (s *shell) stderrText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stderr.String()
}

// kill terminates the process and releases its pipes. The stdout reader is
// drained so it can reach EOF and exit instead of blocking forever on a full
// channel that nobody reads any more.
func (s *shell) kill() {
	_ = s.stdin.Close()
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	go func() {
		for range s.lines {
		}
	}()
	_ = s.cmd.Wait()
}

// closeGracefully closes stdin so the shell exits on EOF, waits briefly, and
// kills it if it is still there.
func (s *shell) closeGracefully(grace time.Duration) {
	_ = s.stdin.Close()
	done := make(chan struct{})
	go func() { _ = s.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(grace):
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		<-done
	}
}

func (p *Pool) spawn() (*shell, error) {
	d := p.drv
	if d.ShellPath == "" {
		return nil, errors.New("sqlite: shell path not configured")
	}
	cmd := exec.Command(d.ShellPath, "-readonly", "-bail", p.dbPath)
	cmd.Env = shellEnv(d.LibDir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	var buf [8]byte
	_, _ = rand.Read(buf[:])
	s := &shell{
		cmd:   cmd,
		stdin: stdin,
		lines: make(chan string, 64),
		nonce: hex.EncodeToString(buf[:]),
	}
	// One reader goroutine per shell, so a request that times out never leaves
	// a blocked read behind: the goroutine keeps draining until the process
	// dies.
	go func() {
		defer close(s.lines)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), maxLine)
		for sc.Scan() {
			s.lines <- sc.Text()
		}
	}()
	go func() {
		b, _ := io.ReadAll(stderrPipe)
		s.mu.Lock()
		s.stderr.Write(b)
		s.mu.Unlock()
	}()

	hdr := scriptHeader(d.BusyTimeoutMS)
	if _, err := io.WriteString(stdin, hdr); err != nil {
		s.kill()
		return nil, &Error{Kind: "shell", Message: "starting pooled shell: " + err.Error()}
	}
	s.lineNo = strings.Count(hdr, "\n")
	p.spawned.Add(1)
	return s, nil
}

// acquire takes a slot, starting or restarting the process if needed.
func (p *Pool) acquire(ctx context.Context) (*shell, error) {
	if p.closed.Load() {
		return nil, errors.New("sqlite: pool is closed")
	}
	var s *shell
	select {
	case s = <-p.slots:
	case <-ctx.Done():
		return nil, ctxError(ctx)
	}
	if s != nil && p.maxUses > 0 && s.uses >= p.maxUses {
		// Retire the worn shell in the background so this request only pays
		// for the replacement's start, not for the old one's shutdown.
		old := s
		go old.closeGracefully(time.Second)
		p.recycled.Add(1)
		s = nil
	}
	if s == nil {
		fresh, err := p.spawn()
		if err != nil {
			p.slots <- nil
			return nil, err
		}
		s = fresh
	}
	return s, nil
}

// Warm starts shells for every empty slot so the first requests do not pay
// for process start. Slots that are busy or already populated are left alone.
func (p *Pool) Warm() {
	for i := 0; i < cap(p.slots); i++ {
		var s *shell
		select {
		case s = <-p.slots:
		default:
			return
		}
		if s == nil && !p.closed.Load() {
			if fresh, err := p.spawn(); err == nil {
				s = fresh
			}
		}
		p.release(s, true)
	}
}

// release returns a healthy shell to the pool, or gives the slot back empty
// when the shell has to be discarded.
func (p *Pool) release(s *shell, healthy bool) {
	if !healthy && s != nil {
		s.kill()
		p.recycled.Add(1)
		s = nil
	}
	if p.closed.Load() && s != nil {
		s.closeGracefully(time.Second)
		s = nil
	}
	// The channel is buffered to the pool size and every slot is returned
	// exactly once, so this never blocks, even after Close.
	p.slots <- s
}

// Close shuts every idle shell down and marks the pool closed; shells that are
// checked out are closed by release when their request finishes. It is safe
// to call more than once.
func (p *Pool) Close() {
	p.closeOnce.Do(func() {
		p.closed.Store(true)
		deadline := time.After(3 * time.Second)
		for i := 0; i < cap(p.slots); i++ {
			select {
			case s := <-p.slots:
				if s != nil {
					s.closeGracefully(time.Second)
				}
			case <-deadline:
				return
			}
		}
	})
}

// run executes one request body on a pooled shell and returns the stdout text
// of each sentinel-framed statement, split as the lines arrive.
func (p *Pool) run(ctx context.Context, body string) ([]string, error) {
	timeout := p.drv.QueryTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	s, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	s.uses++

	// .parameter clear keeps bindings from leaking into the next request.
	marker := fmt.Sprintf("%s%s-%d--", endMarkerPrefix, s.nonce, s.uses)
	req := ".parameter clear\n" + body + ".print " + marker + "\n"
	base := s.lineNo
	s.lineNo += strings.Count(req, "\n")

	if _, err := io.WriteString(s.stdin, req); err != nil {
		// A write failure means the shell is already gone; its stderr holds
		// the reason.
		err := p.shellFailure(s, base)
		p.release(s, false)
		return nil, err
	}

	var parts []string
	var cur strings.Builder
	started := false // text before the first sentinel is not statement output
	for {
		select {
		case line, ok := <-s.lines:
			if !ok {
				// stdout closed: with -bail the shell exited on an error.
				err := p.shellFailure(s, base)
				p.release(s, false)
				return nil, err
			}
			switch line {
			case marker:
				if started {
					parts = append(parts, cur.String())
				}
				p.release(s, true)
				return parts, nil
			case sentinel:
				if started {
					parts = append(parts, cur.String())
				}
				cur.Reset()
				started = true
			default:
				if started {
					cur.WriteString(line)
					cur.WriteByte('\n')
				}
			}
		case <-ctx.Done():
			cerr := ctxError(ctx)
			p.release(s, false)
			return nil, cerr
		}
	}
}

// shellFailure waits for the dead shell and turns its stderr into an Error,
// rewriting the line number so it refers to the generated script rather than
// to the whole lifetime of the connection.
func (p *Pool) shellFailure(s *shell, base int) error {
	// Give the stderr reader a moment to finish.
	done := make(chan struct{})
	go func() { _ = s.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	if perr := ParseError(s.stderrText()); perr != nil {
		if perr.Line > 0 {
			// Script line = reported line - (base + 1 for ".parameter clear")
			// + 3 for the header the spawn path would have written.
			perr.Line = perr.Line - base + 2
			if perr.Line < 1 {
				perr.Line = 0
			}
		}
		return perr
	}
	return &Error{Kind: "shell", Message: "pooled shell exited without a diagnostic"}
}

func ctxError(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &Error{Kind: "timeout", Message: "query exceeded the query timeout"}
	}
	return ctx.Err()
}

// pooledVerbs are the only statement verbs allowed on a shared pooled shell.
// Everything else (ATTACH, PRAGMA, CREATE TEMP ..., transactions, and any
// write that -readonly would reject anyway) can change connection state, so it
// runs in a private throw-away process instead. An allow-list is used rather
// than a deny-list so that a verb nobody thought of cannot slip through.
var pooledVerbs = []string{"SELECT", "WITH", "VALUES", "EXPLAIN"}

// NeedsIsolation reports whether a caller-supplied SQL string contains any
// statement that must not run on a pooled shell. Splitting on ';' is
// conservative: a ';' inside a string literal only makes the check stricter.
func NeedsIsolation(sql string) bool {
	for _, part := range strings.Split(sql, ";") {
		word := firstWord(part)
		if word == "" {
			continue
		}
		ok := false
		for _, v := range pooledVerbs {
			if strings.EqualFold(word, v) {
				ok = true
				break
			}
		}
		if !ok {
			return true
		}
	}
	return false
}

func firstWord(s string) string {
	s = strings.TrimSpace(s)
	// Skip leading line and block comments so "-- note\nPRAGMA ..." and
	// "/* x */ PRAGMA ..." are still caught.
	for {
		if strings.HasPrefix(s, "--") {
			i := strings.IndexByte(s, '\n')
			if i < 0 {
				return ""
			}
			s = strings.TrimSpace(s[i+1:])
			continue
		}
		if strings.HasPrefix(s, "/*") {
			i := strings.Index(s, "*/")
			if i < 0 {
				return ""
			}
			s = strings.TrimSpace(s[i+2:])
			continue
		}
		break
	}
	i := strings.IndexFunc(s, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '(' || r == ';'
	})
	if i < 0 {
		return s
	}
	return s[:i]
}
