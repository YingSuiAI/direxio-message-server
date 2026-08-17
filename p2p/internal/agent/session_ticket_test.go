package agent

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

type agentDataPlaneContractFixture struct {
	Version         int `json:"version"`
	SessionResponse struct {
		RequiredFields   []string `json:"required_fields"`
		BasePath         string   `json:"base_path"`
		TimestampFormat  string   `json:"timestamp_format"`
		TicketTTLSeconds int      `json:"ticket_ttl_seconds"`
	} `json:"session_response"`
	Errors map[string]string `json:"errors"`
	SSE    map[string]string `json:"sse"`
}

func TestCreateAgentSessionSignsOwnerGenerationScopeAndExpiry(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 16, 10, 3, 4, 987000000, time.FixedZone("UTC+8", 8*60*60))
	issuedAt := now.UTC().Truncate(time.Second)
	sessionID := "7d369a99-c600-4a3d-ae3e-81611ddf73a1"
	module := New(Config{
		OwnerID: func() string { return "@owner:example.com" }, AccountGeneration: 9,
		TicketPrivateKey: privateKey, Now: func() time.Time { return now },
	})
	result, actionErr := module.createAgentSession(t.Context(), map[string]any{"session_id": sessionID})
	if actionErr != nil {
		t.Fatalf("issue ticket: %v", actionErr)
	}
	response := result.(map[string]any)
	if response["expires_at"] != "2026-08-16T02:18:04Z" || response["server_time"] != "2026-08-16T02:03:04Z" || response["base_path"] != "/agent/v1" || response["session_id"] != sessionID {
		t.Fatalf("unexpected ticket response: %#v", response)
	}
	serverTime, err := time.Parse(time.RFC3339, response["server_time"].(string))
	if err != nil || serverTime.Location() != time.UTC || !serverTime.Equal(issuedAt) {
		t.Fatalf("server_time must be the RFC3339 UTC ticket issuance time: value=%#v parsed=%v err=%v", response["server_time"], serverTime, err)
	}
	contract := loadAgentDataPlaneContractV1(t)
	gotFields := make([]string, 0, len(response))
	for field := range response {
		gotFields = append(gotFields, field)
	}
	sort.Strings(gotFields)
	wantFields := append([]string(nil), contract.SessionResponse.RequiredFields...)
	sort.Strings(wantFields)
	if !reflect.DeepEqual(gotFields, wantFields) {
		t.Fatalf("ticket response fields drifted: got=%v want=%v", gotFields, wantFields)
	}
	parts := strings.Split(response["ticket"].(string), ".")
	if len(parts) != 3 {
		t.Fatalf("ticket is not compact JWS: %q", response["ticket"])
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(publicKey, []byte(parts[0]+"."+parts[1]), signature) {
		t.Fatal("ticket signature did not verify with the configured public key")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claimFields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &claimFields); err != nil {
		t.Fatal(err)
	}
	wantClaimFields := []string{"account_generation", "aud", "exp", "iat", "iss", "nonce", "scope", "session_id", "sub"}
	if len(claimFields) != len(wantClaimFields) {
		t.Fatalf("ticket claim set drifted: %v", claimFields)
	}
	for _, field := range wantClaimFields {
		if _, ok := claimFields[field]; !ok {
			t.Fatalf("ticket is missing claim %q: %v", field, claimFields)
		}
	}
	var claims agentSessionClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Issuer != "dirextalk-message-server" || claims.Audience != agentSessionAudience || claims.Subject != "@owner:example.com" || claims.AccountGeneration != 9 {
		t.Fatalf("ticket authority claims drifted: %#v", claims)
	}
	if claims.SessionID != sessionID || claims.Nonce == "" || claims.IssuedAt != serverTime.Unix() || claims.ExpiresAt != serverTime.Add(agentSessionTTL).Unix() {
		t.Fatalf("ticket lifetime/session claims drifted: %#v", claims)
	}
	wantScopes := strings.Join(agentSessionScopes, ",")
	if strings.Join(claims.Scopes, ",") != wantScopes || strings.Join(response["scopes"].([]string), ",") != wantScopes {
		t.Fatalf("ticket scopes drifted: claims=%v response=%v", claims.Scopes, response["scopes"])
	}
}

func TestAgentDataPlaneContractV1Fixture(t *testing.T) {
	got := loadAgentDataPlaneContractV1(t)
	want := agentDataPlaneContractFixture{
		Version: 1,
		Errors: map[string]string{
			"expired":         "AGENT_TICKET_EXPIRED",
			"stale":           "AGENT_TICKET_STALE",
			"invalid":         "AGENT_TICKET_INVALID",
			"scope_forbidden": "AGENT_TICKET_SCOPE_FORBIDDEN",
		},
		SSE: map[string]string{"cursor_conflict": "AGENT_CURSOR_CONFLICT"},
	}
	want.SessionResponse.RequiredFields = []string{"ticket", "expires_at", "server_time", "base_path", "session_id", "scopes"}
	want.SessionResponse.BasePath = agentSessionBasePath
	want.SessionResponse.TimestampFormat = "rfc3339_utc"
	want.SessionResponse.TicketTTLSeconds = int(agentSessionTTL / time.Second)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("v1 Agent data-plane contract fixture drifted: got=%#v want=%#v", got, want)
	}
}

func loadAgentDataPlaneContractV1(t *testing.T) agentDataPlaneContractFixture {
	t.Helper()
	data, err := os.ReadFile("testdata/agent_data_plane_contract_v1.json")
	if err != nil {
		t.Fatalf("read v1 Agent data-plane contract fixture: %v", err)
	}
	var fixture agentDataPlaneContractFixture
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode v1 Agent data-plane contract fixture: %v", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		t.Fatalf("v1 Agent data-plane contract fixture has trailing JSON: %v", err)
	}
	return fixture
}

func TestCreateAgentSessionRejectsInvalidRefreshOrMissingAuthority(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	valid := New(Config{OwnerID: func() string { return "@owner:example.com" }, AccountGeneration: 1, TicketPrivateKey: privateKey})
	for _, params := range []map[string]any{
		{"session_id": "not-a-uuid"},
		{"session_id": "7D369A99-C600-4A3D-AE3E-81611DDF73A1"},
		{"session_id": "7d369a99-c600-4a3d-ae3e-81611ddf73a1", "scope": []string{"agent:admin"}},
	} {
		if _, actionErr := valid.createAgentSession(t.Context(), params); actionErr == nil || actionErr.Status != 400 {
			t.Fatalf("expected invalid ticket request to fail with 400, params=%#v err=%#v", params, actionErr)
		}
	}
	missing := New(Config{OwnerID: func() string { return "@owner:example.com" }, AccountGeneration: 1})
	if _, actionErr := missing.createAgentSession(t.Context(), nil); actionErr == nil || actionErr.Status != 503 {
		t.Fatalf("expected missing signing authority to fail closed, got %#v", actionErr)
	}
}
