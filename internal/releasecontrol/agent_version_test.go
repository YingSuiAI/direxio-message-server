package releasecontrol

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPAgentVersionSourceReadsUnauthenticatedHealthMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/agent/v1/health" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" {
			t.Fatal("Agent health metadata request must not carry authorization")
		}
		_, _ = w.Write([]byte(`{"status":"ok","release_version":"v1.0.168"}`))
	}))
	defer server.Close()

	source, err := NewHTTPAgentVersionSource(HTTPAgentVersionSourceConfig{URL: server.URL + "/agent/v1/health"})
	if err != nil {
		t.Fatal(err)
	}
	version, err := source.CurrentAgentVersion(context.Background())
	if err != nil || version != "v1.0.168" {
		t.Fatalf("version=%q err=%v", version, err)
	}
}

func TestHTTPAgentVersionSourceClassifiesFailures(t *testing.T) {
	for name, response := range map[string]struct {
		status int
		body   string
		code   string
	}{
		"unavailable":          {status: http.StatusServiceUnavailable, body: `{}`, code: AgentVersionUnavailableCode},
		"invalid json":         {status: http.StatusOK, body: `{`, code: AgentVersionInvalidCode},
		"missing version":      {status: http.StatusOK, body: `{"status":"ok"}`, code: AgentVersionInvalidCode},
		"noncanonical version": {status: http.StatusOK, body: `{"release_version":"1.0.168"}`, code: AgentVersionInvalidCode},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(response.status)
				_, _ = w.Write([]byte(response.body))
			}))
			defer server.Close()
			source, err := NewHTTPAgentVersionSource(HTTPAgentVersionSourceConfig{URL: server.URL + "/agent/v1/health"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = source.CurrentAgentVersion(context.Background())
			var versionErr *AgentVersionError
			if !errors.As(err, &versionErr) || versionErr.Code != response.code {
				t.Fatalf("error=%#v, want code %s", err, response.code)
			}
		})
	}
}

func TestHTTPAgentVersionSourceRejectsUnsafeEndpointShapes(t *testing.T) {
	for _, endpoint := range []string{"", "agent:8082/agent/v1/health", "ftp://agent/agent/v1/health", "http://user@agent/agent/v1/health", "http://agent/other", "http://agent/agent/v1/health?token=x"} {
		if source, err := NewHTTPAgentVersionSource(HTTPAgentVersionSourceConfig{URL: endpoint}); err == nil || source != nil {
			t.Fatalf("endpoint %q did not fail closed", endpoint)
		}
	}
}
