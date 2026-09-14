package sqlite

import (
	"encoding/json"
	"testing"
)

func TestParseError(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		kind    string
		line    int
		message string
	}{
		{
			name:    "parse error with source echo",
			in:      "Parse error near line 2: near \"SELCT\": syntax error\n  SELCT 1;\n  ^--- error here\n",
			kind:    "parse",
			line:    2,
			message: `near "SELCT": syntax error`,
		},
		{
			name:    "missing table",
			in:      "Parse error near line 2: no such table: nosuchtable\n",
			kind:    "parse",
			line:    2,
			message: "no such table: nosuchtable",
		},
		{
			name:    "readonly database",
			in:      "Error near line 2: attempt to write a readonly database\n",
			kind:    "error",
			line:    2,
			message: "attempt to write a readonly database",
		},
		{
			name:    "runtime error",
			in:      "Runtime error near line 7: UNIQUE constraint failed: tags.id\n",
			kind:    "runtime",
			line:    7,
			message: "UNIQUE constraint failed: tags.id",
		},
		{
			name:    "no line number",
			in:      "Error: unable to open database \"/nope.db\": unable to open database file\n",
			kind:    "error",
			line:    0,
			message: `unable to open database "/nope.db": unable to open database file`,
		},
		{
			name:    "in prepare prefix stripped",
			in:      "Error: in prepare, no such column: zzz\n",
			kind:    "error",
			line:    0,
			message: "no such column: zzz",
		},
		{
			name:    "unrecognised output",
			in:      "something went sideways\n",
			kind:    "shell",
			line:    0,
			message: "something went sideways",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseError(tc.in)
			if got == nil {
				t.Fatal("ParseError returned nil")
			}
			if got.Kind != tc.kind || got.Line != tc.line || got.Message != tc.message {
				t.Errorf("got kind=%q line=%d msg=%q; want kind=%q line=%d msg=%q",
					got.Kind, got.Line, got.Message, tc.kind, tc.line, tc.message)
			}
			if got.Raw == "" {
				t.Error("Raw should carry the full stderr text")
			}
		})
	}
}

func TestParseErrorEmpty(t *testing.T) {
	for _, in := range []string{"", "\n", "   \n\n"} {
		if got := ParseError(in); got != nil {
			t.Errorf("ParseError(%q) = %v, want nil", in, got)
		}
	}
}

func TestErrorString(t *testing.T) {
	e := &Error{Kind: "parse", Line: 3, Message: "boom"}
	if got, want := e.Error(), "parse error near line 3: boom"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
	e = &Error{Kind: "shell", Message: "died"}
	if got, want := e.Error(), "died"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestSplitResults(t *testing.T) {
	out := sentinel + "\n[{\"a\":1}]\n" + sentinel + "\n" + sentinel + "\n[{\"b\":\"x\"},\n{\"b\":\"y\"}]\n"
	segs, err := splitResults(out, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := decodeAll(segs); err != nil {
		t.Fatal(err)
	}
	if len(segs) != 3 {
		t.Fatalf("got %d segments, want 3", len(segs))
	}
	res := firstOfEach(segs)
	if len(res[0].Rows) != 1 || res[0].Rows[0]["a"].(json.Number).String() != "1" {
		t.Errorf("first result = %#v", res[0])
	}
	if res[1].Rows != nil {
		t.Errorf("empty segment should have nil rows, got %#v", res[1].Rows)
	}
	if len(res[2].Rows) != 2 || res[2].Columns[0] != "b" {
		t.Errorf("third result = %#v", res[2])
	}
}

// One segment can hold several result sets when its statement text held
// several SELECTs; that is what POST /v1/query relies on.
func TestSplitResultsMultipleSetsInOneSegment(t *testing.T) {
	out := sentinel + "\n[{\"a\":1}]\n[{\"b\":2}]\n"
	segs, err := splitResults(out, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := decodeAll(segs); err != nil {
		t.Fatal(err)
	}
	if len(segs) != 1 || len(segs[0]) != 2 {
		t.Fatalf("got %d segments / %v result sets", len(segs), segs)
	}
	if segs[0][1].Rows[0]["b"].(json.Number).String() != "2" {
		t.Errorf("second set = %#v", segs[0][1])
	}
}

func TestSplitResultsPadsMissingSegments(t *testing.T) {
	segs, err := splitResults("", 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 4 {
		t.Fatalf("got %d segments, want 4", len(segs))
	}
	if got := firstOfEach(segs); len(got) != 4 || got[0].Rows != nil {
		t.Errorf("firstOfEach = %#v", got)
	}
}

func TestResultRawHelpers(t *testing.T) {
	r := Result{Raw: json.RawMessage("[{\"b\":\"x\",\"a\":1},\n{\"b\":\"y\",\"a\":2}]")}
	if got := r.Count(); got != 2 {
		t.Errorf("Count = %d, want 2", got)
	}
	if cols := r.ColumnNames(); len(cols) != 2 || cols[0] != "b" || cols[1] != "a" {
		t.Errorf("ColumnNames = %v, want [b a] (shell order, not sorted)", cols)
	}
	els, err := r.Elements()
	if err != nil || len(els) != 2 || string(els[1]) != "{\"b\":\"y\",\"a\":2}" {
		t.Errorf("Elements = %q, %v", els, err)
	}
	var empty Result
	if string(empty.JSON()) != "[]" || empty.Count() != 0 || empty.ColumnNames() != nil {
		t.Errorf("empty result helpers: %q %d %v", empty.JSON(), empty.Count(), empty.ColumnNames())
	}
	if err := r.Decode(); err != nil || len(r.Rows) != 2 || r.Rows[1]["a"].(json.Number).String() != "2" {
		t.Errorf("Decode: %v %#v", err, r.Rows)
	}
}
