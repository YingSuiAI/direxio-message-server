package agentembedded

import (
	"testing"
	"time"

	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
)

func TestCredentialProjectionUsesRedactedConfiguredNamesAndVerification(t *testing.T) {
	now := time.Date(2026, 7, 30, 1, 2, 3, 0, time.UTC)
	unverified := coreaws.CredentialView{ID: "id", Revision: 2, VerifiedRevision: 0, AccessKeyConfigured: true, SecretAccessKeyConfigured: true, UpdatedAt: now}
	got := credentialViewMap(unverified)
	if _, ok := got["tested_at"]; ok {
		t.Fatal("unverified credential exposed tested_at")
	}
	for _, key := range []string{"access_key_configured", "secret_access_key_configured", "session_token_configured", "verified_revision"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("missing %s: %#v", key, got)
		}
	}
	for _, key := range []string{"access_key", "secret_access_key", "session_token"} {
		if _, ok := got[key]; ok {
			t.Fatalf("credential redaction leaked legacy/secret field %s", key)
		}
	}
	verified := unverified
	verified.VerifiedRevision = verified.Revision
	got = credentialViewMap(verified)
	if got["tested_at"] != now.Format(time.RFC3339Nano) {
		t.Fatalf("tested_at = %#v", got["tested_at"])
	}
}
