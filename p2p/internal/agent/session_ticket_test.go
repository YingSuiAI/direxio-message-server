package agent

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCreateAgentSessionSignsOwnerGenerationScopeAndExpiry(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 16, 2, 3, 4, 0, time.UTC)
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
	if response["expires_at"] != "2026-08-16T02:18:04Z" || response["base_path"] != "/agent/v1" || response["session_id"] != sessionID {
		t.Fatalf("unexpected ticket response: %#v", response)
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
	var claims agentSessionClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Issuer != "dirextalk-message-server" || claims.Audience != agentSessionAudience || claims.Subject != "@owner:example.com" || claims.AccountGeneration != 9 {
		t.Fatalf("ticket authority claims drifted: %#v", claims)
	}
	if claims.SessionID != sessionID || claims.Nonce == "" || claims.IssuedAt != now.Unix() || claims.ExpiresAt != now.Add(agentSessionTTL).Unix() {
		t.Fatalf("ticket lifetime/session claims drifted: %#v", claims)
	}
	wantScopes := strings.Join(agentSessionScopes, ",")
	if strings.Join(claims.Scopes, ",") != wantScopes || strings.Join(response["scopes"].([]string), ",") != wantScopes {
		t.Fatalf("ticket scopes drifted: claims=%v response=%v", claims.Scopes, response["scopes"])
	}
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
