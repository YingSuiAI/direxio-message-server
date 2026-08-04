package capability

import (
	"context"
	"testing"
	"time"
)

func TestGrantManager_IssueAndVerify(t *testing.T) {
	secret := []byte("test-secret-key-32-bytes-long!!")
	manager := NewGrantManager(secret)

	grant := &CapabilityGrant{
		RootOperationID:   "op-123",
		OwnerID:           "user-456",
		AccountGeneration: 1,
		GrantedScopes:     []string{"contacts:read", "messages:send"},
		CatalogDigest:     []byte("catalog-hash"),
	}

	ctx := context.Background()

	// 签发 grant
	grantBytes, err := manager.IssueGrant(ctx, grant)
	if err != nil {
		t.Fatalf("IssueGrant failed: %v", err)
	}

	t.Logf("Issued grant: %s", string(grantBytes))

	// 验证 grant
	verified, err := manager.VerifyGrant(ctx, grantBytes)
	if err != nil {
		t.Fatalf("VerifyGrant failed: %v", err)
	}

	// 检查字段
	if verified.RootOperationID != grant.RootOperationID {
		t.Errorf("RootOperationID mismatch: got %s, want %s", verified.RootOperationID, grant.RootOperationID)
	}
	if verified.OwnerID != grant.OwnerID {
		t.Errorf("OwnerID mismatch: got %s, want %s", verified.OwnerID, grant.OwnerID)
	}
	if verified.AccountGeneration != grant.AccountGeneration {
		t.Errorf("AccountGeneration mismatch: got %d, want %d", verified.AccountGeneration, grant.AccountGeneration)
	}
	if len(verified.GrantedScopes) != len(grant.GrantedScopes) {
		t.Errorf("GrantedScopes length mismatch: got %d, want %d", len(verified.GrantedScopes), len(grant.GrantedScopes))
	}
}

func TestGrantManager_VerifyExpired(t *testing.T) {
	secret := []byte("test-secret-key-32-bytes-long!!")
	manager := NewGrantManager(secret)

	grant := &CapabilityGrant{
		RootOperationID:   "op-123",
		OwnerID:           "user-456",
		AccountGeneration: 1,
		GrantedScopes:     []string{"contacts:read"},
		CatalogDigest:     []byte("hash"),
		ExpiryUnixMs:      time.Now().Add(-1 * time.Hour).UnixMilli(), // 已过期
	}

	ctx := context.Background()

	// 签发 grant
	grantBytes, err := manager.IssueGrant(ctx, grant)
	if err != nil {
		t.Fatalf("IssueGrant failed: %v", err)
	}

	// 验证应该失败（已过期）
	_, err = manager.VerifyGrant(ctx, grantBytes)
	if err == nil {
		t.Fatal("Expected error for expired grant, got nil")
	}
	if err.Error() != "grant expired" {
		t.Logf("Got expected error: %v", err)
	}
}

func TestGrantManager_VerifyTamperedGrant(t *testing.T) {
	secret := []byte("test-secret-key-32-bytes-long!!")
	manager := NewGrantManager(secret)

	grant := &CapabilityGrant{
		RootOperationID:   "op-123",
		OwnerID:           "user-456",
		AccountGeneration: 1,
		GrantedScopes:     []string{"contacts:read"},
		CatalogDigest:     []byte("hash"),
	}

	ctx := context.Background()

	// 签发 grant
	grantBytes, err := manager.IssueGrant(ctx, grant)
	if err != nil {
		t.Fatalf("IssueGrant failed: %v", err)
	}

	// 篡改 grant（修改一个字节）
	if len(grantBytes) > 10 {
		grantBytes[10] = grantBytes[10] ^ 0xFF
	}

	// 验证应该失败（签名不匹配）
	_, err = manager.VerifyGrant(ctx, grantBytes)
	if err == nil {
		t.Fatal("Expected error for tampered grant, got nil")
	}
	t.Logf("Got expected error: %v", err)
}

func TestCapabilityGrant_HasScope(t *testing.T) {
	grant := &CapabilityGrant{
		GrantedScopes: []string{"contacts:read", "messages:send", "rooms:write"},
	}

	tests := []struct {
		scope string
		want  bool
	}{
		{"contacts:read", true},
		{"messages:send", true},
		{"rooms:write", true},
		{"contacts:write", false},
		{"admin:all", false},
	}

	for _, tt := range tests {
		t.Run(tt.scope, func(t *testing.T) {
			got := grant.HasScope(tt.scope)
			if got != tt.want {
				t.Errorf("HasScope(%s) = %v, want %v", tt.scope, got, tt.want)
			}
		})
	}
}

func TestCapabilityGrant_HasAllScopes(t *testing.T) {
	grant := &CapabilityGrant{
		GrantedScopes: []string{"contacts:read", "messages:send", "rooms:write"},
	}

	tests := []struct {
		name   string
		scopes []string
		want   bool
	}{
		{
			name:   "has all",
			scopes: []string{"contacts:read", "messages:send"},
			want:   true,
		},
		{
			name:   "missing one",
			scopes: []string{"contacts:read", "admin:all"},
			want:   false,
		},
		{
			name:   "empty",
			scopes: []string{},
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := grant.HasAllScopes(tt.scopes)
			if got != tt.want {
				t.Errorf("HasAllScopes() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestScopeJoinSplit(t *testing.T) {
	original := []string{"contacts:read", "messages:send", "rooms:write"}
	joined := joinScopes(original)
	split := splitScopes(joined)

	if len(split) != len(original) {
		t.Errorf("Length mismatch: got %d, want %d", len(split), len(original))
	}

	for i, scope := range original {
		if split[i] != scope {
			t.Errorf("Scope[%d] mismatch: got %s, want %s", i, split[i], scope)
		}
	}
}
