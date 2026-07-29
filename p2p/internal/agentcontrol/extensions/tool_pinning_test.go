package extensions

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func testPinnedTools() []Tool {
	schema := []byte(`{"type":"object","properties":{"name":{"type":"string"}}}`)
	return []Tool{{Name: "hello", Description: "say hello", InputSchemaDigest: DigestBytes(schema), InputSchema: schema}}
}

func installedTestExtension(t *testing.T) (Installation, string) {
	t.Helper()
	i := inspection()
	versionID := uuid.NewString()
	installID := uuid.NewString()
	inst := Installation{ID: installID, OwnerID: "owner", Candidate: i.Candidate, Revision: 4, State: "installed", ActiveVersionID: versionID, Versions: []Version{{VersionID: versionID, Pin: i.Candidate.Pin, ContentDigest: i.ContentDigest, ManifestDigest: i.ManifestDigest, ExecutionDigest: i.ExecutionDigest, NetworkDigest: i.NetworkDigest, SecretDigest: i.SecretDigest, Execution: i.Execution, NetworkGrants: i.NetworkGrants, SecretGrants: i.SecretGrants}}}
	if err := inst.Validate(); err != nil {
		t.Fatalf("test installation invalid: %v", err)
	}
	return inst, versionID
}

func TestMemoryStorePinToolsIsIdempotentAndFenced(t *testing.T) {
	store := NewMemoryStore()
	inst, versionID := installedTestExtension(t)
	if err := store.Put(context.Background(), inst); err != nil {
		t.Fatal(err)
	}
	tools := testPinnedTools()
	got, err := store.PinTools(context.Background(), "owner", inst.ID, versionID, inst.Revision, tools)
	if err != nil || !PinnedToolsEqual(got, tools) {
		t.Fatalf("first pin = %#v, %v", got, err)
	}
	got[0].Description = "caller mutation"
	replay, err := store.PinTools(context.Background(), "owner", inst.ID, versionID, inst.Revision, tools)
	if err != nil || !PinnedToolsEqual(replay, tools) {
		t.Fatalf("replay = %#v, %v", replay, err)
	}
	drift := testPinnedTools()
	drift[0].Description = "different"
	if _, err = store.PinTools(context.Background(), "owner", inst.ID, versionID, inst.Revision, drift); err != ErrConflict {
		t.Fatalf("drift error = %v, want conflict", err)
	}
	if _, err = store.PinTools(context.Background(), "other", inst.ID, versionID, inst.Revision, tools); err != ErrNotFound {
		t.Fatalf("cross-owner error = %v, want not found", err)
	}
	if _, err = store.PinTools(context.Background(), "owner", inst.ID, versionID, inst.Revision-1, tools); err != ErrRevisionConflict {
		t.Fatalf("stale revision error = %v, want revision conflict", err)
	}
}

func TestValidatePinnedToolsRejectsDuplicateAndSchemaDrift(t *testing.T) {
	tools := testPinnedTools()
	if err := ValidatePinnedTools(append(tools, tools[0])); err != ErrInvalid {
		t.Fatalf("duplicate validation = %v, want invalid", err)
	}
	tools[0].InputSchemaDigest = DigestBytes([]byte("other"))
	if err := ValidatePinnedTools(tools); err != ErrInvalid {
		t.Fatalf("schema drift validation = %v, want invalid", err)
	}
	tools = testPinnedTools()
	tools[0].InputSchemaDigest = "ABC"
	tools[0].InputSchema = nil
	if err := ValidatePinnedTools(tools); err != ErrInvalid {
		t.Fatalf("uppercase digest validation = %v, want invalid", err)
	}
}
