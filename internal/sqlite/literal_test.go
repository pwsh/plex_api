package sqlite

import (
	"encoding/json"
	"strings"
	"testing"
)

func num(s string) json.Number { return json.Number(s) }

func TestLiteral(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"nil", nil, "NULL"},
		{"true", true, "1"},
		{"false", false, "0"},
		{"empty string", "", "''"},
		{"plain string", "star wars", "'star wars'"},
		{"unicode", "Ünïcødé", "'Ünïcødé'"},
		{"backslash", `C:\x`, `'C:\x'`},
		{"single quote", "it's", "char(105,116,39,115)"},
		{"double quote", `say "hi"`, "char(115,97,121,32,34,104,105,34)"},
		{"semicolon injection", "'; DROP TABLE tags; --",
			"char(39,59,32,68,82,79,80,32,84,65,66,76,69,32,116,97,103,115,59,32,45,45)"},
		{"int", num("42"), "42"},
		{"negative int", num("-7"), "-7"},
		{"big int", num("9007199254740993"), "9007199254740993"},
		{"float", num("1.5"), "1.5"},
		{"go int", 42, "42"},
		{"go int64", int64(-3), "-3"},
		{"go float", 2.5, "2.5"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Literal(tc.in)
			if err != nil {
				t.Fatalf("Literal(%#v) error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("Literal(%#v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The shell's dot-command tokenizer splits a line on whitespace and treats
// '...' and "..." as quoted spans with no escape mechanism inside, so a
// doubled ” silently ends one token and starts another and the parameter is
// left unbound. Anything that cannot be one plain quoted token must therefore
// use the char() form.
func TestLiteralUnsafeStringsUseCharForm(t *testing.T) {
	for _, in := range []string{"line1\nline2", "a\tb", "a\rb", "\x00", "it's", `a"b`, "'"} {
		got := mustLiteral(t, in)
		if !strings.HasPrefix(got, "char(") || !strings.HasSuffix(got, ")") {
			t.Errorf("Literal(%q) = %q, want the char(...) form", in, got)
		}
		if strings.ContainsAny(got, " \t\r\n'\"") {
			t.Errorf("Literal(%q) = %q must be a single unquoted token", in, got)
		}
	}
	if got, want := mustLiteral(t, "a\nb"), "char(97,10,98)"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
	// Astral characters are encoded by code point, not by UTF-8 byte.
	if got, want := mustLiteral(t, "'\U0001F600"), "char(39,128512)"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

// Every literal must stay on one line, whatever the input.
func TestLiteralIsSingleLine(t *testing.T) {
	for _, in := range []any{"a\nb", "a\r\nb", "plain", "", nil, true, num("1.5")} {
		got := mustLiteral(t, in)
		if strings.ContainsAny(got, "\r\n") {
			t.Errorf("Literal(%#v) = %q contains a line break", in, got)
		}
	}
}

func mustLiteral(t *testing.T, v any) string {
	t.Helper()
	got, err := Literal(v)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestLiteralRejects(t *testing.T) {
	for _, in := range []any{
		map[string]any{"a": 1},
		[]any{1, 2},
		num("nope"),
		struct{}{},
	} {
		if got, err := Literal(in); err == nil {
			t.Errorf("Literal(%#v) = %q, want an error", in, got)
		}
	}
}

func TestValidParamName(t *testing.T) {
	good := []string{"q", "_x", "a1", "account_id", "A_B_9"}
	bad := []string{"", "1a", "a-b", "a b", "a;b", "a'b", ":q", "a.b", "ä"}
	for _, s := range good {
		if !ValidParamName(s) {
			t.Errorf("ValidParamName(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if ValidParamName(s) {
			t.Errorf("ValidParamName(%q) = true, want false", s)
		}
	}
}

func TestQuoteIdent(t *testing.T) {
	if got, want := QuoteIdent("index"), `"index"`; got != want {
		t.Errorf("got %q want %q", got, want)
	}
	if got, want := QuoteIdent(`a"b`), `"a""b"`; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestBuildScriptRejectsBadInput(t *testing.T) {
	if _, err := buildScript(5000, nil, false); err != ErrNoStatements {
		t.Errorf("empty statements: got %v, want ErrNoStatements", err)
	}
	if _, err := buildScript(5000, []Statement{{SQL: "  "}}, false); err == nil {
		t.Error("empty sql: want an error")
	}
	if _, err := buildScript(5000, []Statement{{SQL: ".shell rm -rf /"}}, false); err == nil {
		t.Error("dot-command: want an error")
	}
	if _, err := buildScript(5000, []Statement{{SQL: "SELECT 1", Params: map[string]any{"bad name": 1}}}, false); err == nil {
		t.Error("bad parameter name: want an error")
	}
	if _, err := buildScript(5000, []Statement{{SQL: "SELECT 1", Params: map[string]any{"q": []any{1}}}}, false); err == nil {
		t.Error("bad parameter type: want an error")
	}
}

func TestBuildScriptShape(t *testing.T) {
	got, err := buildScript(1234, []Statement{
		{SQL: "SELECT :q AS q", Params: map[string]any{"q": "x"}},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := ".timeout 1234\n.mode json\n.headers off\n.parameter set :q 'x'\n.print " +
		sentinel + "\nSELECT :q AS q;\n"
	if got != want {
		t.Errorf("read script =\n%q\nwant\n%q", got, want)
	}

	got, err = buildScript(1234, []Statement{{SQL: "DELETE FROM t;"}}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, frag := range []string{"BEGIN IMMEDIATE;\n", "DELETE FROM t;\n", "SELECT changes() AS changes;\n", "COMMIT;\n"} {
		if !strings.Contains(got, frag) {
			t.Errorf("write script missing %q:\n%s", frag, got)
		}
	}
}

// A script line must never be split by a parameter value.
func TestBuildScriptParameterStaysOnOneLine(t *testing.T) {
	got, err := buildScript(5000, []Statement{
		{SQL: "SELECT :a, :b", Params: map[string]any{"a": "x\ny", "b": "it's"}},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, ".parameter set ") && strings.Count(line, " ") < 3 {
			t.Errorf("malformed parameter line %q", line)
		}
	}
	if !strings.Contains(got, ".parameter set :a char(120,10,121)\n") {
		t.Errorf("missing encoded parameter :a in:\n%s", got)
	}
}
