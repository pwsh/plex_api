package sqlite

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// paramNameRE restricts parameter names to a safe identifier shape. Names are
// bound as ":name" in the generated script, so anything outside this set could
// break out of the ".parameter set" line.
var paramNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidParamName reports whether name may be used as a bound parameter.
func ValidParamName(name string) bool { return paramNameRE.MatchString(name) }

// Literal renders a JSON-decoded value as a SQL literal expression suitable as
// the VALUE argument of the shell's ".parameter set" dot-command. The shell
// evaluates that argument as a SQL expression, so the result must be a
// self-contained, single-line expression.
//
// Rules:
//   - string  -> single-quoted literal with ” escaping. Strings containing
//     control characters (which would terminate the dot-command line) are
//     rendered as cast(x'<hex>' as text) instead, which is exact for any UTF-8.
//   - number  -> the numeral itself (integers stay integers; NaN/Inf rejected)
//   - bool    -> 1 / 0
//   - nil     -> NULL
//
// Anything else (objects, arrays) is rejected.
func Literal(v any) (string, error) {
	switch t := v.(type) {
	case nil:
		return "NULL", nil
	case bool:
		if t {
			return "1", nil
		}
		return "0", nil
	case string:
		return stringLiteral(t), nil
	case json.Number:
		return numberLiteral(t.String())
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return "", fmt.Errorf("unsupported numeric value %v", t)
		}
		return strconv.FormatFloat(t, 'g', -1, 64), nil
	case int:
		return strconv.Itoa(t), nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	}
	return "", fmt.Errorf("unsupported parameter type %T (only string, number, boolean and null are allowed)", v)
}

func numberLiteral(s string) (string, error) {
	if _, err := strconv.ParseInt(s, 10, 64); err == nil {
		return s, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return "", fmt.Errorf("invalid number %q", s)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "", fmt.Errorf("unsupported numeric value %q", s)
	}
	return strconv.FormatFloat(f, 'g', -1, 64), nil
}

// plainSafe reports whether s can be rendered as a simple single-quoted SQL
// literal on a ".parameter set" line.
//
// The shell splits a dot-command line into whitespace-separated tokens and
// understands '...' and "..." as quoted spans, but it has no escape mechanism
// inside them: a doubled ” ends one token and starts another, which silently
// leaves the parameter unbound. So a quote of either kind, or any control
// character, disqualifies the plain form.
func plainSafe(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == 0x7f || c == '\'' || c == '"' {
			return false
		}
	}
	return true
}

// stringLiteral renders a Go string as a SQL text expression that survives the
// shell's dot-command tokenizer: one token, no whitespace problems.
//
// The fallback form is char(cp, cp, ...) over the string's Unicode code
// points, which is exact for quotes, newlines and astral characters alike.
func stringLiteral(s string) string {
	if plainSafe(s) {
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	var b strings.Builder
	b.WriteString("char(")
	for i, r := range s {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(int(r)))
	}
	b.WriteString(")")
	return b.String()
}

// QuoteIdent renders an SQL identifier as a double-quoted name. Callers must
// still validate identifiers against sqlite_master / table_info; this only
// guards against quoting mistakes.
func QuoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
