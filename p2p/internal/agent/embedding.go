package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/agentmemory"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

const (
	embeddingTimeout        = 30 * time.Second
	embeddingMaxBody  int64 = 4 << 20
	embeddingMaxInput       = 32
	embeddingMaxText        = 1 << 20
)

func embeddingForStore(profiles storage.ModelProfileStore, owner func() string, client *http.Client) agentmemory.KnowledgeEmbeddingSessionFunc {
	return func(ctx context.Context) (agentmemory.KnowledgeEmbeddingSession, error) {
		if profiles == nil {
			return agentmemory.KnowledgeEmbeddingSession{}, agentmemory.ErrNoEmbeddingProfile
		}
		ownerID := ""
		if owner != nil {
			ownerID = owner()
		}
		ownerID = strings.TrimSpace(ownerID)
		profile, err := profiles.ResolveDefaultModelProfile(ctx, ownerID, storage.ModelKindEmbedding)
		if err != nil {
			if errors.Is(err, storage.ErrModelProfileNotFound) {
				return agentmemory.KnowledgeEmbeddingSession{}, agentmemory.ErrNoEmbeddingProfile
			}
			return agentmemory.KnowledgeEmbeddingSession{}, err
		}
		session := agentmemory.KnowledgeEmbeddingSession{ProfileID: profile.ProfileID, Revision: profile.Revision, Model: profile.Model}
		session.ValidateCurrent = func(validateCtx context.Context) error {
			current, validateErr := profiles.ResolveDefaultModelProfilePin(validateCtx, ownerID, storage.ModelKindEmbedding)
			if validateErr != nil {
				return validateErr
			}
			if current.ProfileID != session.ProfileID || current.Revision != session.Revision || current.Model != session.Model {
				return agentmemory.ErrEmbeddingSessionStale
			}
			return nil
		}
		session.Embed = func(embedCtx context.Context, input string) ([]float32, error) {
			vectors, embedErr := embedHTTP(embedCtx, profile, []string{input}, client)
			if embedErr != nil {
				return nil, embedErr
			}
			if len(vectors) != 1 {
				return nil, errors.New("embedding provider returned invalid vector count")
			}
			return vectors[0], nil
		}
		return session, nil
	}
}

func embedHTTP(ctx context.Context, profile storage.ModelProfile, inputs []string, client *http.Client) ([][]float32, error) {
	if len(inputs) == 0 || len(inputs) > embeddingMaxInput || strings.TrimSpace(profile.Model) == "" || profile.APIKey == "" {
		return nil, errors.New("embedding profile is not ready")
	}
	for _, input := range inputs {
		if len(input) > embeddingMaxText || strings.Contains(input, "\x00") {
			return nil, errors.New("embedding input is invalid")
		}
	}
	provider := strings.ToLower(strings.TrimSpace(profile.Provider))
	if provider != "openai" && provider != "openai_compatible" && provider != "openrouter" && provider != "gemini" {
		return nil, errors.New("embedding provider is unsupported")
	}
	if client == nil {
		client = &http.Client{}
	}
	if provider == "gemini" {
		return embedGemini(ctx, profile, inputs, client)
	}
	payload, _ := json.Marshal(map[string]any{"input": inputs, "model": profile.Model})
	endpoint, err := endpoint(profile.BaseURL, "/embeddings")
	if err != nil {
		return nil, errors.New("embedding endpoint is invalid")
	}
	response, err := embeddingRequest(ctx, client, endpoint, profile.APIKey, payload, false)
	if err != nil {
		return nil, err
	}
	defer zero(payload)
	defer zero(response)
	var decoded struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
			Index     *int      `json:"index"`
		} `json:"data"`
	}
	if err := boundedJSON(response, &decoded); err != nil || len(decoded.Data) != len(inputs) {
		return nil, errors.New("embedding provider returned invalid response")
	}
	out := make([][]float32, len(inputs))
	seen := make([]bool, len(inputs))
	for i, item := range decoded.Data {
		idx := i
		if item.Index != nil {
			idx = *item.Index
		}
		if idx < 0 || idx >= len(inputs) || seen[idx] || !validVector(item.Embedding) {
			return nil, errors.New("embedding provider returned invalid vector")
		}
		seen[idx] = true
		out[idx] = append([]float32(nil), item.Embedding...)
	}
	for _, ok := range seen {
		if !ok {
			return nil, errors.New("embedding provider returned invalid indices")
		}
	}
	return out, nil
}

func embedGemini(ctx context.Context, profile storage.ModelProfile, inputs []string, client *http.Client) ([][]float32, error) {
	model := strings.TrimPrefix(strings.TrimSpace(profile.Model), "models/")
	if model == "" || strings.ContainsAny(model, "\r\n?&#") {
		return nil, errors.New("embedding model is invalid")
	}
	out := make([][]float32, len(inputs))
	for i, input := range inputs {
		payload, _ := json.Marshal(map[string]any{"content": map[string]any{"parts": []map[string]string{{"text": input}}}})
		endpoint, err := endpoint(profile.BaseURL, path.Join("/v1beta/models", model+":embedContent"))
		if err != nil {
			return nil, errors.New("embedding endpoint is invalid")
		}
		response, err := embeddingRequest(ctx, client, endpoint, profile.APIKey, payload, true)
		zero(payload)
		if err != nil {
			return nil, err
		}
		var decoded struct {
			Embedding struct {
				Values []float32 `json:"values"`
			} `json:"embedding"`
		}
		if boundedJSON(response, &decoded) != nil || !validVector(decoded.Embedding.Values) {
			zero(response)
			return nil, errors.New("embedding provider returned invalid vector")
		}
		out[i] = append([]float32(nil), decoded.Embedding.Values...)
		zero(response)
	}
	return out, nil
}

func endpoint(base, suffix string) (string, error) {
	base = strings.TrimSpace(base)
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") || strings.ContainsAny(base, "\r\n\x00") {
		return "", errors.New("invalid endpoint")
	}
	if u.Scheme == "http" && u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1" {
		return "", errors.New("insecure endpoint")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + strings.TrimLeft(suffix, "/")
	return u.String(), nil
}
func embeddingRequest(ctx context.Context, client *http.Client, endpoint, key string, body []byte, gemini bool) ([]byte, error) {
	c, cancel := context.WithTimeout(ctx, embeddingTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(c, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("embedding request failed")
	}
	req.Header.Set("Content-Type", "application/json")
	if gemini {
		req.Header.Set("x-goog-api-key", key)
	} else {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	clientCopy := *client
	clientCopy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := clientCopy.Do(req)
	if err != nil {
		return nil, errors.New("embedding request failed")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, embeddingMaxBody+1))
	if err != nil {
		return nil, errors.New("embedding response read failed")
	}
	if int64(len(data)) > embeddingMaxBody {
		return nil, errors.New("embedding response too large")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("embedding provider returned status %d", resp.StatusCode)
	}
	return data, nil
}
func boundedJSON(data []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	if err := d.Decode(target); err != nil {
		return err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("trailing response data")
	}
	return nil
}
func validVector(v []float32) bool {
	if len(v) == 0 || len(v) > agentmemory.MaxEmbeddingDimension {
		return false
	}
	for _, x := range v {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return false
		}
	}
	return true
}
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
