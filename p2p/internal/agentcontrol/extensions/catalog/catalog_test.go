package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/extensions"
)

func testKey() []byte { return []byte("catalog-test-signing-key-012345678901") }

func TestOfficialRemoteSearchAndInspectionFiltersLocalAndSSE(t *testing.T) {
	var body = `{"servers":[` +
		`{"name":"remote/demo","version":"1.2.3","description":"remote","remotes":[{"type":"streamable-http","url":"https://remote.example/mcp"}]},` +
		`{"name":"stdio/demo","version":"1.0.0","remotes":[{"type":"stdio","command":"node"}]},` +
		`{"name":"sse/demo","version":"1.0.0","remotes":[{"type":"sse","url":"https://remote.example/events"}]},` +
		`{"name":"http/demo","version":"1.0.0","remotes":[{"type":"streamable-http","url":"http://remote.example/mcp"}]}` +
		`],"nextCursor":"cursor-2"}`
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0.1/servers/remote%2Fdemo/versions/1.2.3" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"remote/demo","version":"1.2.3","description":"remote","remotes":[{"type":"streamable-http","url":"https://remote.example/mcp"}]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer s.Close()
	cat, err := NewForTest(Config{Authorities: map[string]string{"official_registry": s.URL}, Client: s.Client(), SigningKey: testKey()})
	if err != nil {
		t.Fatal(err)
	}
	candidates, next, err := cat.Search(context.Background(), "official_registry", "demo", 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].Transport != extensions.TransportRemote {
		t.Fatalf("filtered candidates: %#v", candidates)
	}
	if next == "" {
		t.Fatal("expected signed next token")
	}
	if _, _, err := cat.Search(context.Background(), "official_registry", "other", 10, next); err == nil {
		t.Fatal("page token was not query-bound")
	}
	inspected, err := cat.Inspect(context.Background(), candidates[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := inspected.Validate(); err != nil {
		t.Fatalf("inspection validation: %v", err)
	}
	if inspected.Execution.Remote == nil || inspected.Execution.Remote.URL != "https://remote.example/mcp" {
		t.Fatalf("remote execution: %#v", inspected.Execution)
	}
	if len(inspected.SecretGrants) != 1 || inspected.SecretGrants[0].Configured {
		t.Fatalf("inspection must not claim a credential is configured: %#v", inspected.SecretGrants)
	}
}

func TestCatalogRejectsNonHTTPSAndStdioInspectionBeforeRequest(t *testing.T) {
	called := false
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer s.Close()
	cat, err := NewForTest(Config{Authorities: map[string]string{"official_registry": s.URL}, Client: s.Client(), SigningKey: testKey()})
	if err != nil {
		t.Fatal(err)
	}
	stdio := extensions.Candidate{ID: "demo@1.0.0", Kind: extensions.KindMCP, Source: "official_registry", Name: "demo", Transport: "stdio", Pin: extensions.SourcePin{RegistryVersion: "1.0.0", RegistrySHA256: digest("pin")}}
	if _, err := cat.Inspect(context.Background(), stdio); err == nil {
		t.Fatal("stdio inspection accepted")
	}
	if called {
		t.Fatal("stdio inspection performed source request")
	}
	if _, _, err := cat.Search(context.Background(), "unknown", "", 10, ""); err == nil {
		t.Fatal("unknown source accepted")
	}
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func TestCatalogRejectsTamperedPageToken(t *testing.T) {
	cat, err := NewForTest(Config{Authorities: map[string]string{"official_registry": "http://127.0.0.1:1"}, SigningKey: testKey()})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = cat.Search(context.Background(), "official_registry", strings.Repeat("x", 1), 10, "not-a-token")
	if err == nil {
		t.Fatal("malformed token accepted")
	}
}

func TestCatalogAggregatesAllSourcesWhenSourceIsEmpty(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v0.1/servers":
			_, _ = w.Write([]byte(`{"servers":[{"name":"official/demo","version":"1.0.0","remotes":[{"type":"streamable-http","url":"https://remote.example/official"}]}]}`))
		case r.URL.Path == "/servers":
			_, _ = w.Write([]byte(`{"servers":[{"qualifiedName":"smithery/demo","name":"Smithery Demo","version":"1.0.0","deploymentUrl":"https://remote.example/smithery"}]}`))
		case r.URL.Path == "/api/mcp/v1/servers":
			_, _ = w.Write([]byte(`{"data":{"servers":{"nodes":[{"owner":"glama","name":"demo","version":"1.0.0","url":"https://remote.example/glama"}],"pageInfo":{"hasNextPage":false}}}}`))
		case r.URL.Path == "/search/repositories":
			_, _ = w.Write([]byte(`{"items":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer s.Close()
	authorities := map[string]string{SourceOfficialRegistry: s.URL, SourceSmithery: s.URL, SourceGlama: s.URL, SourceGitHub: s.URL}
	cat, err := NewForTest(Config{Authorities: authorities, Client: s.Client(), SigningKey: testKey()})
	if err != nil {
		t.Fatal(err)
	}
	candidates, next, err := cat.Search(context.Background(), "", "demo", 8, "")
	if err != nil {
		t.Fatal(err)
	}
	if next != "" || len(candidates) != 3 {
		t.Fatalf("aggregate result: %d candidates, next=%q", len(candidates), next)
	}
	if candidates[0].Source != SourceOfficialRegistry || candidates[1].Source != SourceSmithery || candidates[2].Source != SourceGlama {
		t.Fatalf("aggregate order: %#v", candidates)
	}
}

func TestGitHubPinnedRemoteInspection(t *testing.T) {
	commit := strings.Repeat("a", 40)
	manifest := []byte(`{"transport":"streamable-http","url":"https://remote.example/github"}`)
	blobSHA := fullBlobSHA(manifest)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/search/repositories":
			_, _ = w.Write([]byte(`{"items":[{"full_name":"owner/repo","description":"demo","default_branch":"main"}]}`))
		case r.URL.Path == "/repos/owner/repo/commits/main":
			_, _ = w.Write([]byte(`{"sha":"` + commit + `"}`))
		case r.URL.Path == "/repos/owner/repo/git/trees/"+commit:
			payload := map[string]any{"sha": commit, "url": "https://api.github.com/tree", "size": 1, "tree": []map[string]any{{"path": "manifest.json", "mode": "100644", "type": "blob", "sha": blobSHA, "url": "https://api.github.com/blob", "size": len(manifest)}}, "truncated": false}
			_ = json.NewEncoder(w).Encode(payload)
		case r.URL.Path == "/repos/owner/repo/git/blobs/"+blobSHA:
			payload := map[string]string{"content": base64.StdEncoding.EncodeToString(manifest), "encoding": "base64"}
			_ = json.NewEncoder(w).Encode(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cat, err := NewForTest(Config{Authorities: map[string]string{SourceGitHub: server.URL}, Client: server.Client(), SigningKey: testKey()})
	if err != nil {
		t.Fatal(err)
	}
	candidates, _, err := cat.Search(context.Background(), SourceGitHub, "demo", 10, "")
	if err != nil || len(candidates) != 1 {
		t.Fatalf("github search: %#v %v", candidates, err)
	}
	if candidates[0].Transport != extensions.TransportRemote {
		t.Fatalf("github transport: %#v", candidates[0])
	}
	inspected, err := cat.Inspect(context.Background(), candidates[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := inspected.Validate(); err != nil {
		t.Fatalf("github inspection: %v", err)
	}
}
