package agent

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	agentdatav2 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/agent/data/v2"
	"gopkg.in/yaml.v2"
)

const capabilityAPIModule = "github.com/YingSuiAI/dirextalk-capability-api"

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
	response, ok := result.(agentdatav2.AgentSessionResponse)
	if !ok {
		t.Fatalf("ticket response type = %T, want generated AgentSessionResponse", result)
	}
	if !response.ExpiresAt.Equal(time.Date(2026, 8, 16, 2, 18, 4, 0, time.UTC)) || !response.ServerTime.Equal(issuedAt) || response.BasePath != agentdatav2.AgentSessionResponseBasePathAgentv1 || response.SessionId.String() != sessionID {
		t.Fatalf("unexpected ticket response: %#v", response)
	}
	if response.ServerTime.Location() != time.UTC {
		t.Fatalf("server_time must be UTC: %#v", response.ServerTime)
	}
	gotFields := jsonFieldNames(t, response)
	wantFields := jsonFieldNames(t, loadSharedAgentSessionVector(t))
	if !reflect.DeepEqual(gotFields, wantFields) {
		t.Fatalf("ticket response fields drifted: got=%v want=%v", gotFields, wantFields)
	}
	parts := strings.Split(response.Ticket, ".")
	if len(parts) != 3 {
		t.Fatalf("ticket is not compact JWS: %q", response.Ticket)
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
	if claims.SessionID != sessionID || claims.Nonce == "" || claims.IssuedAt != response.ServerTime.Unix() || claims.ExpiresAt != response.ServerTime.Add(agentSessionTTL).Unix() {
		t.Fatalf("ticket lifetime/session claims drifted: %#v", claims)
	}
	wantScopes := strings.Join(agentDataScopeStrings(agentSessionScopes), ",")
	if strings.Join(claims.Scopes, ",") != wantScopes || strings.Join(agentDataScopeStrings(response.Scopes), ",") != wantScopes {
		t.Fatalf("ticket scopes drifted: claims=%v response=%v", claims.Scopes, response.Scopes)
	}
}

func TestSharedAgentDataPlaneV2SessionVector(t *testing.T) {
	response := loadSharedAgentSessionVector(t)
	if response.BasePath != agentdatav2.AgentSessionResponseBasePathAgentv1 || response.SessionId.String() == "" || len(response.Scopes) == 0 || response.Ticket == "" {
		t.Fatalf("shared session response vector is incomplete: %#v", response)
	}
}

func TestIssuedAgentSessionScopesMatchSharedV2Contract(t *testing.T) {
	path := filepath.Join(sharedCapabilityModuleDir(t), "api", "openapi", "agent-data-plane-v2.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read shared v2 Agent data-plane contract: %v", err)
	}
	var contract struct {
		Components struct {
			Schemas map[string]struct {
				Enum []string `yaml:"enum"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(data, &contract); err != nil {
		t.Fatalf("decode shared v2 Agent data-plane contract: %v", err)
	}
	want := append([]string(nil), contract.Components.Schemas["AgentDataScope"].Enum...)
	got := agentDataScopeStrings(agentSessionScopes)
	sort.Strings(want)
	sort.Strings(got)
	if len(want) == 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("issued Agent session scopes differ from shared v2 contract: got=%v want=%v", got, want)
	}
}

func sharedCapabilityModuleDir(t *testing.T) string {
	t.Helper()
	command := exec.Command("go", "list", "-m", "-f={{.Version}}\n{{.Dir}}", capabilityAPIModule)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("locate shared capability contract module: %v", err)
	}
	parts := strings.SplitN(strings.TrimSpace(string(output)), "\n", 2)
	if len(parts) != 2 || parts[0] != "v1.1.0" || strings.TrimSpace(parts[1]) == "" {
		t.Fatalf("shared capability contract pin = %q, want v1.1.0 and module directory", output)
	}
	return strings.TrimSpace(parts[1])
}

func loadSharedAgentSessionVector(t *testing.T) agentdatav2.AgentSessionResponse {
	t.Helper()
	path := filepath.Join(sharedCapabilityModuleDir(t), "conformance", "agent-data-plane", "v2", "valid", "session_response.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read shared v2 Agent session vector: %v", err)
	}
	var response agentdatav2.AgentSessionResponse
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		t.Fatalf("decode shared v2 Agent session vector with generated DTO: %v", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		t.Fatalf("shared v2 Agent session vector has trailing JSON: %v", err)
	}
	return response
}

func jsonFieldNames(t *testing.T, value any) []string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON field source: %v", err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatalf("decode JSON field source: %v", err)
	}
	fields := make([]string, 0, len(object))
	for field := range object {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	return fields
}

func agentDataScopeStrings(scopes []agentdatav2.AgentDataScope) []string {
	values := make([]string, len(scopes))
	for index, scope := range scopes {
		values[index] = string(scope)
	}
	return values
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
