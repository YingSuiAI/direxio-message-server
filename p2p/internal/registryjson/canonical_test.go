package registryjson

import (
	"strings"
	"testing"
)

func TestCanonicalCorpus(t *testing.T) {
	depth := strings.Repeat("[", maxDepth+2) + strings.Repeat("]", maxDepth+2)
	nodes := "[" + strings.Repeat("0,", maxNodes) + "0]"
	cases := []struct {
		content, wantError, wantDigest string
	}{
		{`{"content_digest":"ignored","z":[true,null,"x"],"a":1}`, "", "c4daf0b418c0d35fcffde36252dc87538a868217bc2db0a1e3f8937d1884abfb"},
		{`{"n":1.0,"content_digest":"ignored"}`, "", "3b6b06ecd1c968c8e738e0f11c4bb361fca80a9a694de22fe66a05286afbd081"},
		{`{"id":1,"id":2}`, `duplicate key "id"`, ""},
		{`{"id":1} {"id":2}`, "trailing JSON", ""},
		{depth, "JSON depth cap exceeded", ""},
		{nodes, "JSON node cap exceeded", ""},
	}
	for _, tc := range cases {
		_, err := CanonicalValue([]byte(tc.content))
		if (tc.wantError == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), tc.wantError) {
			t.Fatalf("error = %v, want %q", err, tc.wantError)
		}
		if got := ContentDigest([]byte(tc.content)); got != tc.wantDigest {
			t.Fatalf("digest = %q, want %q", got, tc.wantDigest)
		}
	}
}
