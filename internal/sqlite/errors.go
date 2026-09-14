package sqlite

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Error is a structured form of one diagnostic line written by the Plex SQLite
// shell to stderr.
type Error struct {
	// Kind is the lower-cased error class reported by the shell: "parse",
	// "runtime", "error" (generic), or "shell" when the message did not match
	// the shell's usual format.
	Kind string `json:"kind"`
	// Line is the 1-based line of the generated script, or 0 if unknown.
	Line int `json:"line,omitempty"`
	// Message is the SQLite error text.
	Message string `json:"message"`
	// Raw is the complete stderr output.
	Raw string `json:"raw,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Line > 0 {
		return fmt.Sprintf("%s error near line %d: %s", e.Kind, e.Line, e.Message)
	}
	if e.Kind != "" && e.Kind != "shell" {
		return fmt.Sprintf("%s error: %s", e.Kind, e.Message)
	}
	return e.Message
}

// "Parse error near line 2: no such table: x"
// "Runtime error near line 3: UNIQUE constraint failed: ..."
// "Error near line 2: attempt to write a readonly database"
// "Error: in prepare, no such table: x"
var errLineRE = regexp.MustCompile(`^(?:([A-Za-z]+) )?[Ee]rror(?: near line (\d+))?: (.*)$`)

// ParseError turns the shell's stderr output into a structured Error. It
// returns nil when out contains nothing that looks like an error. The first
// recognised diagnostic wins, because the driver always runs with -bail.
func ParseError(out string) *Error {
	trimmed := strings.TrimRight(out, "\n")
	if strings.TrimSpace(trimmed) == "" {
		return nil
	}
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		m := errLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		kind := strings.ToLower(m[1])
		if kind == "" {
			kind = "error"
		}
		n := 0
		if m[2] != "" {
			n, _ = strconv.Atoi(m[2])
		}
		msg := strings.TrimSpace(m[3])
		msg = strings.TrimPrefix(msg, "in prepare, ")
		return &Error{Kind: kind, Line: n, Message: msg, Raw: trimmed}
	}
	return &Error{Kind: "shell", Message: strings.TrimSpace(trimmed), Raw: trimmed}
}
