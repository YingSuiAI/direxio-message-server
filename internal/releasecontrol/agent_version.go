package releasecontrol

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	AgentVersionUnavailableCode = "agent_version_unavailable"
	AgentVersionInvalidCode     = "agent_version_invalid"
	maxAgentVersionResponseSize = 4 * 1024
)

// AgentVersionSource observes the immutable version reported by the running
// Agent. It is deliberately independent of the host updater and its runtime
// mutation checks.
type AgentVersionSource interface {
	CurrentAgentVersion(context.Context) (string, error)
}

type AgentVersionError struct {
	Code string
}

func (e *AgentVersionError) Error() string {
	if e == nil || e.Code == "" {
		return "agent version observation failed"
	}
	return e.Code
}

func AsAgentVersionError(err error) (*AgentVersionError, bool) {
	var versionErr *AgentVersionError
	if !errors.As(err, &versionErr) {
		return nil, false
	}
	return versionErr, true
}

type HTTPAgentVersionSourceConfig struct {
	URL     string
	Client  *http.Client
	Timeout time.Duration
}

type httpAgentVersionSource struct {
	url    string
	client *http.Client
}

func NewHTTPAgentVersionSource(config HTTPAgentVersionSourceConfig) (AgentVersionSource, error) {
	endpoint := strings.TrimSpace(config.URL)
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Path != "/agent/v1/health" {
		return nil, errors.New("Agent version URL must be an absolute HTTP URL ending in /agent/v1/health")
	}
	client := config.Client
	if client == nil {
		timeout := config.Timeout
		if timeout <= 0 {
			timeout = 3 * time.Second
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		client = &http.Client{
			Transport: transport,
			Timeout:   timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return &httpAgentVersionSource{url: parsed.String(), client: client}, nil
}

func (s *httpAgentVersionSource) CurrentAgentVersion(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return "", &AgentVersionError{Code: AgentVersionInvalidCode}
	}
	response, err := s.client.Do(req)
	if err != nil {
		return "", &AgentVersionError{Code: AgentVersionUnavailableCode}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", &AgentVersionError{Code: AgentVersionUnavailableCode}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxAgentVersionResponseSize+1))
	if err != nil || len(data) > maxAgentVersionResponseSize {
		return "", &AgentVersionError{Code: AgentVersionInvalidCode}
	}
	var health struct {
		ReleaseVersion string `json:"release_version"`
	}
	if err := json.Unmarshal(data, &health); err != nil {
		return "", &AgentVersionError{Code: AgentVersionInvalidCode}
	}
	version, err := CanonicalStableVersion("release_version", health.ReleaseVersion)
	if err != nil {
		return "", &AgentVersionError{Code: AgentVersionInvalidCode}
	}
	return version, nil
}

func AgentVersionReason(err error) string {
	if versionErr, ok := AsAgentVersionError(err); ok {
		switch versionErr.Code {
		case AgentVersionUnavailableCode, AgentVersionInvalidCode:
			return versionErr.Code
		}
	}
	return AgentVersionUnavailableCode
}
