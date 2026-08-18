package p2p

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultVoiceCallbackMaxBodyBytes = 64 << 10
	maxVoiceCallbackBodyBytes        = 1 << 20
	defaultVoiceCallbackTimeout      = 15 * time.Second
	maxVoiceCallbackTimeout          = 60 * time.Second
	maxVoiceCallbackResponseBytes    = 2 << 20
	voiceCallbackRelayAuthHeader     = "X-Dirextalk-Agent-Voice-Relay-Token"
	voiceCallbackGenerationHeader    = "X-Dirextalk-Account-Generation"
)

// voiceCallbackRelayConfig describes the one-way MS→Agent callback hop.  The
// original provider callback HMAC remains in the request Authorization (or
// X-Voice-Callback-Token) header; AuthToken is a separate deployment secret
// used only to authenticate the relay itself.
type voiceCallbackRelayConfig struct {
	URL               string
	AuthToken         string
	CAFile            string
	ClientCertFile    string
	ClientKeyFile     string
	ServerName        string
	HTTPClient        *http.Client
	MaxBodyBytes      int64
	Timeout           time.Duration
	AccountGeneration int64
}

type voiceCallbackRelay struct {
	baseURL           *url.URL
	authToken         string
	accountGeneration int64
	client            *http.Client
	maxBodyBytes      int64
	timeout           time.Duration
}

func newVoiceCallbackRelay(cfg voiceCallbackRelayConfig) (*voiceCallbackRelay, error) {
	rawURL := strings.TrimSpace(cfg.URL)
	if rawURL == "" {
		return nil, nil
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("agent voice callback URL must be an HTTPS origin without credentials or query")
	}
	if strings.TrimSpace(cfg.AuthToken) == "" {
		return nil, errors.New("agent voice callback relay token is required")
	}
	if len(cfg.AuthToken) > 4096 {
		return nil, errors.New("agent voice callback relay token is too long")
	}
	if cfg.AccountGeneration <= 0 {
		return nil, errors.New("agent voice callback account generation must be positive")
	}
	maxBody := cfg.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = defaultVoiceCallbackMaxBodyBytes
	}
	if maxBody > maxVoiceCallbackBodyBytes {
		return nil, errors.New("agent voice callback body limit is too large")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultVoiceCallbackTimeout
	}
	if timeout > maxVoiceCallbackTimeout {
		return nil, errors.New("agent voice callback timeout is too large")
	}
	client := cfg.HTTPClient
	if client == nil {
		client, err = newVoiceCallbackHTTPClient(cfg)
		if err != nil {
			return nil, err
		}
	}
	if client == nil {
		client = &http.Client{}
	}
	return &voiceCallbackRelay{
		baseURL:           parsed,
		authToken:         strings.TrimSpace(cfg.AuthToken),
		accountGeneration: cfg.AccountGeneration,
		client:            client,
		maxBodyBytes:      maxBody,
		timeout:           timeout,
	}, nil
}

// newVoiceCallbackHTTPClient creates the optional mTLS client for a private
// Agent listener.  A custom HTTP client is accepted by tests and by callers
// that terminate the private listener behind an already-configured transport.
func newVoiceCallbackHTTPClient(cfg voiceCallbackRelayConfig) (*http.Client, error) {
	if strings.TrimSpace(cfg.CAFile) == "" && strings.TrimSpace(cfg.ClientCertFile) == "" && strings.TrimSpace(cfg.ClientKeyFile) == "" {
		return &http.Client{}, nil
	}
	if strings.TrimSpace(cfg.CAFile) == "" || strings.TrimSpace(cfg.ClientCertFile) == "" || strings.TrimSpace(cfg.ClientKeyFile) == "" {
		return nil, errors.New("agent voice callback TLS CA, client certificate, and key are required together")
	}
	caBytes, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read Agent voice callback CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBytes) {
		return nil, errors.New("parse Agent voice callback CA")
	}
	cert, err := tls.LoadX509KeyPair(cfg.ClientCertFile, cfg.ClientKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load Agent voice callback client certificate: %w", err)
	}
	serverName := strings.TrimSpace(cfg.ServerName)
	if serverName == "" {
		serverName = "dirextalk-agent"
	}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, Certificates: []tls.Certificate{cert}, ServerName: serverName},
	}
	return &http.Client{Transport: transport}, nil
}

func (r *voiceCallbackRelay) handle(w http.ResponseWriter, req *http.Request, path string) {
	if req.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost+", "+http.MethodOptions)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r == nil || r.baseURL == nil || r.client == nil {
		http.Error(w, "voice unavailable", http.StatusServiceUnavailable)
		return
	}
	if strings.TrimSpace(req.URL.Query().Get("session_id")) == "" {
		http.Error(w, "voice callback rejected", http.StatusUnauthorized)
		return
	}
	body, err := readVoiceCallbackBody(req.Body, r.maxBodyBytes)
	if err != nil {
		statusCode := http.StatusBadRequest
		if errors.Is(err, errVoiceCallbackBodyTooLarge) {
			statusCode = http.StatusRequestEntityTooLarge
		}
		http.Error(w, "invalid callback", statusCode)
		return
	}
	target := *r.baseURL
	target.Path = strings.TrimRight(target.Path, "/") + "/" + strings.TrimLeft(path, "/")
	target.RawQuery = req.URL.RawQuery
	ctx, cancel := context.WithTimeout(req.Context(), r.timeout)
	defer cancel()
	out, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, "voice unavailable", http.StatusBadGateway)
		return
	}
	for _, name := range []string{"Authorization", "X-Voice-Callback-Token", "Content-Type", "Accept"} {
		if value := req.Header.Get(name); value != "" {
			out.Header.Set(name, value)
		}
	}
	out.Header.Set(voiceCallbackRelayAuthHeader, r.authToken)
	out.Header.Set(voiceCallbackGenerationHeader, strconv.FormatInt(r.accountGeneration, 10))
	response, err := r.client.Do(out)
	if err != nil {
		// Do not reflect transport details or private Agent addresses to the
		// provider.  The provider may retry a generic gateway failure safely.
		http.Error(w, "voice callback unavailable", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	statusCode := response.StatusCode
	if statusCode >= http.StatusInternalServerError {
		statusCode = http.StatusBadGateway
	}
	for _, name := range []string{"Content-Type", "Cache-Control", "X-Content-Type-Options"} {
		if value := response.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.WriteHeader(statusCode)
	_, _ = io.Copy(w, io.LimitReader(response.Body, maxVoiceCallbackResponseBytes))
}

var errVoiceCallbackBodyTooLarge = errors.New("voice callback body too large")

func readVoiceCallbackBody(body io.Reader, max int64) ([]byte, error) {
	if body == nil {
		return nil, errors.New("callback body is required")
	}
	if max <= 0 {
		max = defaultVoiceCallbackMaxBodyBytes
	}
	limited := io.LimitReader(body, max+1)
	value, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(value)) > max {
		return nil, errVoiceCallbackBodyTooLarge
	}
	return value, nil
}
