package p2p

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultPortalPasswordPrefersProtectedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "portal-password")
	if err := os.WriteFile(path, []byte("file-only-secret\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	t.Setenv("P2P_PORTAL_PASSWORD_FILE", path)
	t.Setenv("P2P_PORTAL_PASSWORD", "plaintext-fallback")
	if got := defaultPortalPassword(); got != "file-only-secret" {
		t.Fatalf("defaultPortalPassword() = %q, want protected file value", got)
	}
}

func TestDefaultPortalPasswordDoesNotUsePlaintextFallbackWhenFileIsInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "portal-password")
	if err := os.WriteFile(path, []byte("invalid-permissions"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("P2P_PORTAL_PASSWORD_FILE", path)
	t.Setenv("P2P_PORTAL_PASSWORD", "plaintext-fallback")
	got := defaultPortalPassword()
	if got == "plaintext-fallback" || got == "" {
		t.Fatalf("invalid protected file fell back to plaintext/empty password: %q", got)
	}
}
