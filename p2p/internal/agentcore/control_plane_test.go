package agentcore

import (
	"context"
	"strings"
	"testing"

	agentv1 "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcorev1"
)

func TestControlPlaneCapabilityGateHidesAbsentOptionalToken(t *testing.T) {
	client, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	result, actionErr := client.taskGet(context.Background(), map[string]any{"task_id": "task-1"})
	if result != nil || actionErr == nil || actionErr.Code != "agent_core_capability_unavailable" {
		t.Fatalf("task gate = %#v, %#v; want capability precondition", result, actionErr)
	}
}

func TestControlPlaneProjectionsNormalizeEnumsAndOmitSensitiveFields(t *testing.T) {
	task := taskMap(&agentv1.CoreTask{TaskId: "task-1", Status: agentv1.CoreTaskStatus_CORE_TASK_STATUS_WAITING_USER, Kind: agentv1.CoreTaskKind_CORE_TASK_KIND_EXTENSION, FailureSummary: "  safe\nsummary ", Revision: 3})
	if task["status"] != "waiting-user" || task["kind"] != "extension" || task["failure_summary"] != "safe summary" {
		t.Fatalf("task projection = %#v", task)
	}
	installation := installationMap(&agentv1.CoreInstallation{InstallationId: "install-1", Versions: []*agentv1.CoreExtensionVersion{{VersionId: "v1", Execution: &agentv1.CoreExecution{Descriptor_: &agentv1.CoreExecution_Stdio{Stdio: &agentv1.CoreStaticEntry{RelativePath: "/private/runtime", Argv: []string{"secret-command"}}}}}}})
	serialized := marshalSafe(installation)
	if strings.Contains(serialized, "private/runtime") || strings.Contains(serialized, "secret-command") {
		t.Fatalf("installation projection leaked execution material: %s", serialized)
	}
}
func TestInspectionProjectionSafeDescriptors(t *testing.T) {
	i := inspectionMap(&agentv1.CoreExtensionInspection{ContentDigest: strings.Repeat("a", 64), Execution: &agentv1.CoreExecution{Descriptor_: &agentv1.CoreExecution_Remote{Remote: &agentv1.CoreRemoteEndpoint{Url: "https://example.test", CredentialReferenceId: "cred"}}}, NetworkGrants: []*agentv1.CoreNetworkGrant{{Scheme: "https", Host: "example.test", Port: 443, Digest: strings.Repeat("b", 64)}}, SecretGrants: []*agentv1.CoreExtensionSecretGrantDescriptor{{ReferenceId: "r", Purpose: agentv1.CoreSecretPurpose_CORE_SECRET_PURPOSE_MCP_CREDENTIAL, BindingDigest: strings.Repeat("c", 64), Configured: true}}})
	s := marshalSafe(i)
	if strings.Contains(s, "secret_value") || strings.Contains(s, "Authorization") {
		t.Fatal(s)
	}
}
func TestInspectionParserRejectsUnknownAndTypeMismatch(t *testing.T) {
	cand := &agentv1.CoreExtensionCandidate{Id: "x", Kind: agentv1.CoreExtensionKind_CORE_EXTENSION_KIND_MCP, Source: agentv1.CoreExtensionSource_CORE_EXTENSION_SOURCE_GITHUB, Name: "x", Description: "x", Transport: agentv1.CoreExtensionTransport_CORE_EXTENSION_TRANSPORT_STREAMABLE_HTTP, Pin: &agentv1.CoreSourcePin{GitCommit: strings.Repeat("a", 40), GitSha256: strings.Repeat("f", 64)}}
	base := map[string]any{"candidate": map[string]any{"id": "x", "kind": "mcp", "source": "github", "name": "x", "description": "x", "transport": "streamable-http", "pin": map[string]any{"registry_version": "", "registry_sha256": "", "git_commit": strings.Repeat("a", 40), "git_sha256": strings.Repeat("f", 64)}}, "content_digest": strings.Repeat("a", 64), "manifest_digest": strings.Repeat("b", 64), "execution_digest": strings.Repeat("c", 64), "network_schema_digest": strings.Repeat("d", 64), "secret_schema_digest": strings.Repeat("e", 64), "execution": map[string]any{"remote": map[string]any{"url": "https://example.test"}}, "network_grants": []any{}, "secret_grants": []any{}, "unknown": true}
	if _, e := inspectionFromParams(map[string]any{"inspection": base}, "inspection", cand.GetKind(), cand); e == nil {
		t.Fatal("unknown accepted")
	}
	delete(base, "unknown")
	base["network_grants"] = "bad"
	if _, e := inspectionFromParams(map[string]any{"inspection": base}, "inspection", cand.GetKind(), cand); e == nil {
		t.Fatal("type mismatch accepted")
	}
}

func TestTaskTemplateParserAcceptsLegalScheduleFields(t *testing.T) {
	template, actionErr := taskTemplateFromParams(map[string]any{"task_template": map[string]any{
		"goal": "run the safe task", "conversation_id": "conversation-1", "model_profile_id": "profile-1", "timeout_seconds": int64(60),
	}}, "task_template")
	if actionErr != nil {
		t.Fatalf("legal task template rejected: %#v", actionErr)
	}
	if template.GetGoal() != "run the safe task" || template.GetConversationId() != "conversation-1" || template.GetModelProfileId() != "profile-1" || template.GetTimeoutSeconds() != 60 {
		t.Fatalf("task template = %#v", template)
	}
}

func TestExtensionCandidateAndInspectionStrictness(t *testing.T) {
	base := map[string]any{"id": "candidate", "kind": "mcp", "source": "github", "name": "name", "description": "", "transport": "streamable-http", "pin": map[string]any{"registry_version": "", "registry_sha256": "", "git_commit": strings.Repeat("a", 40), "git_sha256": strings.Repeat("b", 64)}}
	candidate, actionErr := candidateFromParams(map[string]any{"candidate": base}, "candidate", agentv1.CoreExtensionKind_CORE_EXTENSION_KIND_MCP)
	if actionErr != nil || candidate.GetDescription() != "" {
		t.Fatalf("empty description must remain a valid string: %#v, %#v", candidate, actionErr)
	}
	for _, mutate := range []func(map[string]any){
		func(v map[string]any) { v["git_commit"] = strings.Repeat("A", 40) },
		func(v map[string]any) { v["git_commit"] = strings.Repeat("a", 39) },
		func(v map[string]any) { v["git_sha256"] = strings.Repeat("B", 64) },
		func(v map[string]any) {
			v["registry_version"], v["registry_sha256"] = "latest", strings.Repeat("c", 64)
		},
	} {
		pin := map[string]any{"registry_version": "", "registry_sha256": "", "git_commit": strings.Repeat("a", 40), "git_sha256": strings.Repeat("b", 64)}
		mutate(pin)
		copy := map[string]any{"id": "candidate", "kind": "mcp", "source": "github", "name": "name", "description": "", "transport": "streamable-http", "pin": pin}
		if _, err := candidateFromParams(map[string]any{"candidate": copy}, "candidate", agentv1.CoreExtensionKind_CORE_EXTENSION_KIND_MCP); err == nil {
			t.Fatal("invalid Agent SourcePin accepted")
		}
	}
	for _, invalid := range []map[string]any{
		{"id": "candidate", "kind": "mcp", "source": "skills-sh", "name": "name", "description": "", "transport": "streamable-http", "pin": map[string]any{"registry_version": "2026.07", "registry_sha256": strings.Repeat("c", 64), "git_commit": "", "git_sha256": ""}},
		{"id": "candidate", "kind": "skill", "source": "glama", "name": "name", "description": "", "transport": "skill-static", "pin": map[string]any{"registry_version": "2026.07", "registry_sha256": strings.Repeat("c", 64), "git_commit": "", "git_sha256": ""}},
		{"id": "candidate", "kind": "mcp", "source": "github", "name": "name", "description": "", "transport": "streamable-http", "pin": map[string]any{"registry_version": "2026.07", "registry_sha256": strings.Repeat("c", 64), "git_commit": "", "git_sha256": ""}},
		{"id": "candidate", "kind": "skill", "source": "skills-sh", "name": "name", "description": "", "transport": "skill-static", "pin": map[string]any{"registry_version": "", "registry_sha256": "", "git_commit": strings.Repeat("a", 40), "git_sha256": strings.Repeat("b", 64)}},
	} {
		kind := agentv1.CoreExtensionKind_CORE_EXTENSION_KIND_MCP
		if invalid["kind"] == "skill" {
			kind = agentv1.CoreExtensionKind_CORE_EXTENSION_KIND_SKILL
		}
		if _, err := candidateFromParams(map[string]any{"candidate": invalid}, "candidate", kind); err == nil {
			t.Fatal("invalid Candidate.Validate source/pin matrix accepted")
		}
	}
	inspection := map[string]any{"candidate": base, "content_digest": strings.Repeat("1", 64), "manifest_digest": strings.Repeat("2", 64), "execution_digest": strings.Repeat("3", 64), "network_schema_digest": strings.Repeat("4", 64), "secret_schema_digest": strings.Repeat("5", 64), "execution": map[string]any{"remote": map[string]any{"url": "https://example.test"}}, "network_grants": []any{}, "secret_grants": []any{}}
	if _, err := inspectionFromParams(map[string]any{"inspection": inspection}, "inspection", candidate.GetKind(), candidate); err != nil {
		t.Fatalf("empty grants must be accepted: %#v", err)
	}
	delete(inspection, "network_grants")
	if _, err := inspectionFromParams(map[string]any{"inspection": inspection}, "inspection", candidate.GetKind(), candidate); err == nil {
		t.Fatal("missing network_grants accepted")
	}
	inspection["network_grants"] = []any{}
	inspection["content_digest"] = strings.Repeat("A", 64)
	if _, err := inspectionFromParams(map[string]any{"inspection": inspection}, "inspection", candidate.GetKind(), candidate); err == nil {
		t.Fatal("uppercase inspection digest accepted")
	}
	secret := "  exact write-only bytes  "
	inputs, err := secretInputsFromParams(map[string]any{"secret_inputs": []any{map[string]any{"reference_id": "ref", "purpose": "mcp-credential", "secret_value": secret}}}, "secret_inputs")
	if err != nil || len(inputs) != 1 || inputs[0].GetSecretValue() != secret {
		t.Fatalf("secret input was normalized or rejected: %#v, %#v", inputs, err)
	}
}
