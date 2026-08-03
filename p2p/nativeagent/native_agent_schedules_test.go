package nativeagent

import (
	"fmt"
	"strings"
	"testing"
)

func TestSanitizeScheduledTextRedactsCanaryAndCredentialPatterns(t *testing.T) {
	secret := "canary-secret-value"
	got := SanitizeScheduledText(fmt.Sprintf("output %s bearer abcdefghijkl api_key=%s", secret, secret), secret)
	if strings.Contains(got, secret) || strings.Contains(strings.ToLower(got), "bearer abc") {
		t.Fatalf("secret leaked: %q", got)
	}
}
