package agentembedded

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/extensions"
)

type mcpCatalogStub struct {
	candidate  extensions.Candidate
	inspection extensions.Inspection
}

func (s mcpCatalogStub) Search(context.Context, string, string, int, string) ([]extensions.Candidate, string, error) {
	return []extensions.Candidate{s.candidate}, "next", nil
}

func (s mcpCatalogStub) Inspect(context.Context, extensions.Candidate) (extensions.Inspection, error) {
	return s.inspection, nil
}

func testRemoteMCPCandidate() extensions.Candidate {
	return extensions.Candidate{
		ID:          "owner/repository",
		Kind:        extensions.KindMCP,
		Source:      "github",
		Name:        "demo",
		Description: "",
		Transport:   extensions.TransportRemote,
		Pin: extensions.SourcePin{
			GitCommit: strings.Repeat("a", 40),
			GitSHA256: strings.Repeat("b", 64),
		},
	}
}

func testRemoteMCPInspection() extensions.Inspection {
	candidate := testRemoteMCPCandidate()
	reference := "11111111-1111-4111-8111-111111111111"
	digest := strings.Repeat("c", 64)
	return extensions.Inspection{
		Candidate:       candidate,
		ContentDigest:   digest,
		ManifestDigest:  digest,
		ExecutionDigest: digest,
		NetworkDigest:   digest,
		SecretDigest:    digest,
		Execution: extensions.Execution{Remote: &extensions.Endpoint{
			URL:                   "https://mcp.example/mcp",
			CredentialReferenceID: reference,
		}},
		NetworkGrants: []extensions.NetworkGrant{{
			Scheme: "https", Host: "mcp.example", Port: 443,
			PathPrefix: "/mcp", Digest: digest,
		}},
		SecretGrants: []extensions.SecretGrant{{
			ReferenceID: reference, Purpose: "mcp_credential",
			BindingDigest: digest, Configured: false,
		}},
	}
}

func requireExactKeys(t *testing.T, value map[string]any, expected ...string) {
	t.Helper()
	if !exactKeys(value, expected...) {
		t.Fatalf("keys = %v, want exactly %v", reflect.ValueOf(value).MapKeys(), expected)
	}
}

func TestMCPPublicWireDTOsRemainStable(t *testing.T) {
	candidate := testRemoteMCPCandidate()
	candidateWire := candidateDTO(candidate)
	requireExactKeys(t, candidateWire, "id", "kind", "source", "name", "description", "pin", "transport")
	if candidateWire["transport"] != "streamable-http" || candidateWire["source"] != "github" {
		t.Fatalf("candidate wire enums = %#v", candidateWire)
	}
	pin := candidateWire["pin"].(map[string]any)
	requireExactKeys(t, pin, "registry_version", "registry_sha256", "git_commit", "git_sha256")
	parsedCandidate, actionErr := candidateValue(candidateWire)
	if actionErr != nil || !reflect.DeepEqual(parsedCandidate, candidate) {
		t.Fatalf("candidate round trip = %#v, %v", parsedCandidate, actionErr)
	}

	inspection := testRemoteMCPInspection()
	inspectionWire := inspectionDTO(inspection)
	requireExactKeys(t, inspectionWire,
		"candidate", "content_digest", "manifest_digest", "execution_digest",
		"network_schema_digest", "secret_schema_digest", "execution",
		"network_grants", "secret_grants",
	)
	parsedInspection, actionErr := inspectionParam(map[string]any{"inspection": inspectionWire}, candidate)
	if actionErr != nil || !reflect.DeepEqual(parsedInspection, inspection) {
		t.Fatalf("inspection round trip = %#v, %v", parsedInspection, actionErr)
	}

	versionID := "22222222-2222-4222-8222-222222222222"
	now := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
	installed := extensions.Installation{
		ID: "33333333-3333-4333-8333-333333333333", OwnerID: "secret-owner",
		Candidate: candidate, Revision: 2, State: "updating",
		ActiveVersionID: versionID,
		Versions: []extensions.Version{{
			VersionID: versionID, ContentDigest: inspection.ContentDigest,
			ManifestDigest:  inspection.ManifestDigest,
			ExecutionDigest: inspection.ExecutionDigest,
			NetworkDigest:   inspection.NetworkDigest, SecretDigest: inspection.SecretDigest,
			CreatedAt: now,
		}},
		CreatedAt: now, UpdatedAt: now,
	}
	installationWire := installationDTO(installed)
	requireExactKeys(t, installationWire,
		"installation_id", "kind", "source", "name", "description", "revision",
		"state", "active_version_id", "proposed_version_id", "candidate_id",
		"transport", "versions", "created_at", "updated_at",
	)
	versionWire := installationWire["versions"].([]any)[0].(map[string]any)
	requireExactKeys(t, versionWire,
		"version_id", "content_digest", "manifest_digest", "execution_digest",
		"network_schema_digest", "secret_schema_digest", "created_at",
	)

	toolWire := toolDTO(extensions.Tool{
		Name: "echo", Description: "", InputSchemaDigest: strings.Repeat("d", 64),
	})
	requireExactKeys(t, toolWire, "name", "description", "input_schema_digest")
	executionWire := executeDTO(extensions.ExecuteResult{
		TaskID:         "44444444-4444-4444-8444-444444444444",
		ConfirmationID: "55555555-5555-4555-8555-555555555555",
		InstallationID: installed.ID, VersionID: versionID,
	})
	requireExactKeys(t, executionWire, "task_id", "confirmation_id")
}

func TestMCPInspectWrapsStableDTOAndMutationRechecksAuthority(t *testing.T) {
	candidate := testRemoteMCPCandidate()
	inspection := testRemoteMCPInspection()
	port := &MCPActionPort{
		Service: &extensions.Service{},
		Catalog: mcpCatalogStub{candidate: candidate, inspection: inspection},
	}
	value, actionErr := port.Handle(
		context.Background(),
		"owner",
		"agent.core.mcp.inspect",
		map[string]any{"candidate": candidateDTO(candidate)},
	)
	if actionErr != nil {
		t.Fatal(actionErr)
	}
	response := value.(map[string]any)
	requireExactKeys(t, response, "inspection")

	changed := inspectionDTO(inspection)
	changed["content_digest"] = strings.Repeat("e", 64)
	_, actionErr = port.Handle(
		context.Background(),
		"owner",
		"agent.core.mcp.install",
		map[string]any{
			"idempotency_key": "66666666-6666-4666-8666-666666666666",
			"candidate":       candidateDTO(candidate),
			"inspection":      changed,
			"secret_inputs": []any{map[string]any{
				"reference_id": inspection.SecretGrants[0].ReferenceID,
				"purpose":      "mcp-credential",
				"secret_value": "write-only",
			}},
		},
	)
	if actionErr == nil || actionErr.Code != "mcp_inspection_changed" {
		t.Fatalf("authority mismatch error = %#v", actionErr)
	}
}

func TestMCPListAppliesSourceStateFiltersAndStableCursor(t *testing.T) {
	store := extensions.NewMemoryStore()
	candidate := testRemoteMCPCandidate()
	for _, fixture := range []struct {
		id, source, state string
	}{
		{"11111111-1111-4111-8111-111111111111", "github", "installed"},
		{"22222222-2222-4222-8222-222222222222", "github", "installed"},
		{"33333333-3333-4333-8333-333333333333", "github", "failed"},
		{"44444444-4444-4444-8444-444444444444", "glama", "installed"},
	} {
		itemCandidate := candidate
		itemCandidate.Source = fixture.source
		if err := store.Put(context.Background(), extensions.Installation{
			ID: fixture.id, OwnerID: "owner", Candidate: itemCandidate,
			Revision: 1, State: fixture.state,
		}); err != nil {
			t.Fatal(err)
		}
	}
	port := &MCPActionPort{Service: &extensions.Service{Store: store}}
	value, actionErr := port.Handle(context.Background(), "owner", "agent.core.mcp.list", map[string]any{
		"page_size": 1,
		"source":    "github",
		"state":     "installed",
	})
	if actionErr != nil {
		t.Fatal(actionErr)
	}
	first := value.(map[string]any)
	items := first["installations"].([]any)
	if len(items) != 1 || first["next_page_token"] == "" {
		t.Fatalf("first page = %#v", first)
	}
	value, actionErr = port.Handle(context.Background(), "owner", "agent.core.mcp.list", map[string]any{
		"page_size":  1,
		"page_token": first["next_page_token"],
		"source":     "github",
		"state":      "installed",
	})
	if actionErr != nil {
		t.Fatal(actionErr)
	}
	second := value.(map[string]any)
	if got := second["installations"].([]any); len(got) != 1 || second["next_page_token"] != "" {
		t.Fatalf("second page = %#v", second)
	}
	if _, actionErr = port.Handle(context.Background(), "owner", "agent.core.mcp.list", map[string]any{"source": "stdio"}); actionErr == nil {
		t.Fatal("expected invalid source to be rejected")
	}
}
