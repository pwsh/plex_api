package api

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
)

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case nil:
		return ""
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

func asInt(v any) int64 {
	switch t := v.(type) {
	case json.Number:
		n, _ := t.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	case float64:
		return int64(t)
	}
	return 0
}

// isBlobType reports whether a declared column type means BLOB. SQLite's rules
// say any type name containing "BLOB" (and the empty type name) has affinity
// BLOB; Plex only ever declares "BLOB".
func isBlobType(decl string) bool {
	return strings.Contains(strings.ToUpper(decl), "BLOB")
}

// hexToBase64 converts the uppercase hex string produced by SQLite's hex()
// into standard base64. Blob columns are selected as hex("col") because the
// shell's JSON writer emits raw bytes as lossy \uXXXX escapes; base64 is what
// the API returns to clients.
func hexToBase64(v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	raw, err := hex.DecodeString(strings.ToLower(s))
	if err != nil {
		return v
	}
	return base64.StdEncoding.EncodeToString(raw)
}
