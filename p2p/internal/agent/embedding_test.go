package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

func TestEmbedHTTPOpenAIHeadersAndOrder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("authorization=%q", got)
		}
		var body struct {
			Input []string `json:"input"`
			Model string   `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if strings.Join(body.Input, ",") != "one,two" || body.Model != "embed-model" {
			t.Fatalf("request=%+v", body)
		}
		_, _ = w.Write([]byte(`{"data":[{"embedding":[1,0],"index":1},{"embedding":[0,1],"index":0}]}`))
	}))
	defer server.Close()
	p := storage.ModelProfile{Provider: "openai_compatible", BaseURL: server.URL, Model: "embed-model", APIKey: "secret"}
	vectors, err := embedHTTP(context.Background(), p, []string{"one", "two"}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if vectors[0][0] != 0 || vectors[1][0] != 1 {
		t.Fatalf("ordering=%v", vectors)
	}
}

func TestEmbedHTTPOpenRouterUsesCompatibleEmbeddingsEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Fatalf("path=%q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer openrouter-secret" {
			t.Fatalf("authorization=%q", got)
		}
		var body struct {
			Input []string `json:"input"`
			Model string   `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Input) != 1 || body.Input[0] != "remember this" || body.Model != "openai/text-embedding-3-small" {
			t.Fatalf("request=%+v", body)
		}
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.25,-0.5]}]}`))
	}))
	defer server.Close()

	p := storage.ModelProfile{
		Provider: "openrouter",
		BaseURL:  server.URL + "/v1",
		Model:    "openai/text-embedding-3-small",
		APIKey:   "openrouter-secret",
	}
	vectors, err := embedHTTP(context.Background(), p, []string{"remember this"}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if len(vectors) != 1 || len(vectors[0]) != 2 || vectors[0][0] != 0.25 || vectors[0][1] != -0.5 {
		t.Fatalf("vectors=%v", vectors)
	}
}

func TestEmbedHTTPRejectsOversizeMalformedAndSecretsInErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", int(embeddingMaxBody)+1)))
	}))
	defer server.Close()
	p := storage.ModelProfile{Provider: "openai", BaseURL: server.URL, Model: "m", APIKey: "super-secret"}
	if _, err := embedHTTP(context.Background(), p, []string{"x"}, server.Client()); err == nil || strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("err=%v", err)
	}
	server2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"data":`)) }))
	defer server2.Close()
	p.BaseURL = server2.URL
	if _, err := embedHTTP(context.Background(), p, []string{"x"}, server2.Client()); err == nil {
		t.Fatal("expected malformed response")
	}
}

func TestEmbedHTTPGeminiHeaderAndPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "secret" || !strings.HasSuffix(r.URL.Path, "/v1beta/models/gem:embedContent") {
			t.Fatalf("headers/path=%v %s", r.Header, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"embedding":{"values":[0.5,0.5]}}`))
	}))
	defer server.Close()
	p := storage.ModelProfile{Provider: "gemini", BaseURL: server.URL, Model: "models/gem", APIKey: "secret"}
	vectors, err := embedHTTP(context.Background(), p, []string{"hello"}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if len(vectors) != 1 || len(vectors[0]) != 2 {
		t.Fatalf("vectors=%v", vectors)
	}
}

func TestEmbedHTTPDoesNotFollowRedirectsWithCredentials(t *testing.T) {
	followed := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed++
		if r.Header.Get("Authorization") != "" {
			t.Fatalf("credential leaked on redirected request")
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/embeddings", http.StatusFound)
	}))
	defer origin.Close()
	p := storage.ModelProfile{Provider: "openai", BaseURL: origin.URL, Model: "m", APIKey: "secret"}
	if _, err := embedHTTP(context.Background(), p, []string{"x"}, origin.Client()); err == nil {
		t.Fatal("expected redirect error")
	}
	if followed != 0 {
		t.Fatalf("redirect was followed %d times", followed)
	}
}

func TestEmbedHTTPDoesNotFollowSameHostRedirect(t *testing.T) {
	followed := 0
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/same-target" {
			followed++
			if r.Header.Get("Authorization") != "" {
				t.Fatal("credential leaked")
			}
			return
		}
		http.Redirect(w, r, "/same-target", http.StatusFound)
	}))
	defer origin.Close()
	p := storage.ModelProfile{Provider: "openai", BaseURL: origin.URL, Model: "m", APIKey: "secret"}
	if _, err := embedHTTP(context.Background(), p, []string{"x"}, origin.Client()); err == nil {
		t.Fatal("expected redirect error")
	}
	if followed != 0 {
		t.Fatalf("same-host redirect followed %d times", followed)
	}
}

func TestEmbeddingDimensionBound(t *testing.T) {
	if validVector(make([]float32, 32769)) {
		t.Fatal("accepted oversized vector")
	}
	if !validVector(make([]float32, 32768)) {
		t.Fatal("rejected maximum vector")
	}
}

func TestEmbeddingForStoreUsesExactDefaultAndPinsProfile(t *testing.T) {
	store := storage.NewMemoryStore()
	key := "secret"
	_, err := store.SyncModelProfilesWithDefaults(context.Background(), "owner", "idem", storage.ModelProfileDefaults{EmbeddingClientProfileID: "vector"}, []storage.ModelProfileSyncEntry{{ClientProfileID: "vector", Provider: "openai", Model: "m", BaseURL: "https://example.com", ModelKind: storage.ModelKindEmbedding, APIKey: &key}})
	if err != nil {
		t.Fatal(err)
	}
	session, err := embeddingForStore(store, func() string { return "owner" }, nil)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if session.ProfileID == "" || session.Model != "m" || session.Revision != 1 {
		t.Fatalf("session=%+v", session)
	}
}
