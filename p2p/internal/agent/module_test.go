package agent

import (
	"errors"
	"net/http"
	"testing"
)

func TestExternalAgentActionErrorClassifiesForgedServerDerivedIdentity(t *testing.T) {
	err := externalAgentActionError(errors.New(`agent operation failed: query error: request field "owner_id" is server-derived`))
	if err == nil || err.Status != http.StatusBadRequest {
		t.Fatalf("forged identity status = %#v, want HTTP 400", err)
	}
}
