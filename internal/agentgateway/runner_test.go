package agentgateway

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
	"github.com/YingSuiAI/dirextalk-message-server/internal/agentstream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestActionBindingIsExplicit(t *testing.T) {
	if _, ok := actionBindingFor("agent.chat.stream"); !ok {
		t.Fatal("typed chat stream binding is missing")
	}
	if binding, ok := actionBindingFor("agent.chat.turns.list"); !ok || binding.operation != "list_turns" {
		t.Fatalf("turn listing binding = %#v, want list_turns", binding)
	}
	if binding, ok := actionBindingFor("agent.chat.turn.stop"); !ok || binding.capabilityID != "agent.chat.v1" || binding.operation != "stop_turn" {
		t.Fatalf("turn stop binding = %#v, want agent.chat.v1/stop_turn", binding)
	}
	if binding, ok := actionBindingFor("agent.chat.turn.steer"); !ok || binding.capabilityID != "agent.chat.v1" || binding.operation != "steer_turn" {
		t.Fatalf("turn steer binding = %#v, want agent.chat.v1/steer_turn", binding)
	}
	if _, ok := actionBindingFor("agent.chat.conversations.list.stream"); ok {
		t.Fatal("unknown stream suffix must not use heuristic fallback")
	}
	if binding, ok := actionBindingFor("agent.core.mcp.execute"); !ok || binding.capabilityID != "agent.skills.v1" || binding.operation != "execute_mcp" {
		t.Fatalf("typed MCP execute binding = %#v, want agent.skills.v1/execute_mcp", binding)
	}
	if binding, ok := actionBindingFor("agent.models.list"); !ok || binding.capabilityID != "agent.info.v1" || binding.operation != "list_models" {
		t.Fatalf("provider model catalog binding = %#v, want agent.info.v1/list_models", binding)
	}
	if binding, ok := actionBindingFor("agent.model_profiles.list"); !ok || binding.capabilityID != "agent.models.v1" || binding.operation != "list_models" {
		t.Fatalf("model profile list binding = %#v, want agent.models.v1/list_models", binding)
	}
	for action, operation := range map[string]string{
		"agent.execution.v2.plans.get": "plans_get", "agent.execution.v2.plans.list": "plans_list",
		"agent.execution.v2.runs.get": "runs_get", "agent.execution.v2.runs.list": "runs_list", "agent.execution.v2.runs.cancel": "runs_cancel", "agent.execution.v2.runs.events": "runs_events",
		"agent.execution.v2.artifacts.get": "artifacts_get", "agent.execution.v2.artifacts.download": "artifacts_download", "agent.execution.v2.artifacts.delete": "artifacts_delete",
	} {
		binding, ok := actionBindingFor(action)
		if !ok || binding.capabilityID != "agent.execution.v2" || binding.operation != operation {
			t.Errorf("%s binding = %#v, want agent.execution.v2/%s", action, binding, operation)
		}
	}
	for _, action := range []string{
		"agent.execution.v2.projects.analyze", "agent.execution.v2.analyses.get",
		"agent.execution.v2.targets.list", "agent.execution.v2.targets.get", "agent.execution.v2.targets.import", "agent.execution.v2.targets.reserve", "agent.execution.v2.targets.observe",
		"agent.execution.v2.plans.create", "agent.execution.v2.plans.revise",
		"agent.execution.v2.deployments.list", "agent.execution.v2.deployments.get", "agent.execution.v2.deployments.events",
		"agent.execution.v2.runs.create", "agent.execution.v2.runs.retry",
		"agent.execution.v2.service_bindings.list", "agent.execution.v2.service_bindings.get", "agent.execution.v2.service_bindings.invoke",
		"agent.execution.v2.secrets.create", "agent.execution.v2.secrets.get", "agent.execution.v2.secrets.list", "agent.execution.v2.secrets.revoke",
	} {
		if _, ok := actionBindingFor(action); ok {
			t.Errorf("retired action %s retains a gateway binding", action)
		}
		if _, ok := expectedCatalogSchemaDigests[action]; ok {
			t.Errorf("retired action %s retains a catalog schema pin", action)
		}
	}
	for action, operation := range map[string]string{
		"agent.web_search.config.get":    "get_config",
		"agent.web_search.config.update": "update_config",
		"agent.web_search.test":          "test",
	} {
		binding, ok := actionBindingFor(action)
		if !ok || binding.capabilityID != "agent.web_search.v1" || binding.operation != operation {
			t.Errorf("%s binding = %#v, want agent.web_search.v1/%s", action, binding, operation)
		}
	}
}

func TestMemoryFactReplayMetadataStaysOutsideClosedBusinessResult(t *testing.T) {
	factID := "33333333-3333-4333-8333-333333333333"
	results := map[string]map[string]any{
		"agent.memory.facts.update": {
			"id": factID, "subject": "user", "predicate": "occupation", "value": "architect", "kind": "identity", "confidence": float64(.9),
			"valid_from": "2026-08-12T07:00:00Z", "last_confirmed_at": "2026-08-12T08:00:00Z",
		},
		"agent.memory.facts.delete": {"fact_id": factID, "deleted": true},
	}
	for action, output := range results {
		if actionPublishesReplay(action) {
			t.Fatalf("%s must not publish transport replay metadata", action)
		}
		if _, err := adaptActionResult(action, output); err != nil {
			t.Fatalf("%s closed business result rejected after replay: %v", action, err)
		}
	}
	for _, action := range []string{"agent.chat.conversations.create", "agent.knowledge.upload.start"} {
		if !actionPublishesReplay(action) {
			t.Fatalf("%s must retain its public replay receipt", action)
		}
	}
}

func TestActionBindingsExcludeUnpublishedModelAndKnowledgeActions(t *testing.T) {
	for _, action := range []string{
		"agent.core.model_profiles.sync", "agent.core.model_profiles.list", "agent.core.model_profiles.get", "agent.core.model_profiles.delete", "agent.core.model_profiles.create", "agent.core.model_profiles.update", "agent.core.model_profiles.test",
		"agent.model_profiles.create", "agent.model_profiles.update",
		"agent.knowledge.sources.get", "agent.knowledge.memory.create", "agent.knowledge.memories.list", "agent.knowledge.memories.get", "agent.knowledge.memories.update", "agent.knowledge.memories.delete",
		"agent.knowledge.memory.search", "agent.knowledge.index", "agent.skills.list", "agent.skills.install", "agent.skills.enable", "agent.skills.disable", "agent.skills.uninstall", "agent.skills.registry.search", "agent.mcp.servers.list", "agent.mcp.servers.install", "agent.mcp.servers.enable", "agent.mcp.servers.disable", "agent.mcp.servers.uninstall", "agent.mcp.registry.search",
	} {
		if _, ok := actionBindingFor(action); ok {
			t.Errorf("unpublished ProductCore action %q retains a gateway binding", action)
		}
		if _, ok := expectedCatalogSchemaDigests[action]; ok {
			t.Errorf("unpublished ProductCore action %q retains a catalog schema pin", action)
		}
	}
}

func TestTransformArtifactDownloadRequestPassesExactTypedQuery(t *testing.T) {
	binding, ok := actionBindingFor("agent.execution.v2.artifacts.download")
	if !ok {
		t.Fatal("artifact download binding is missing")
	}
	raw, err := transformCapabilityRequest("agent.execution.v2.artifacts.download", "transport-operation", map[string]any{
		"record_kind": "cloud_worker", "artifact_id": "9e728519-ea72-52cc-bb5a-8eb2860722b8",
		"offset_bytes": int64(8), "max_chunk_bytes": int64(512 << 10),
	}, binding)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte(`{"artifact_id":"9e728519-ea72-52cc-bb5a-8eb2860722b8","max_chunk_bytes":524288,"offset_bytes":8,"record_kind":"cloud_worker"}`)
	if !bytes.Equal(raw, want) {
		t.Fatalf("typed artifact download input = %s, want %s", raw, want)
	}
}

func TestEveryLiveActionBindingHasPinnedCatalogSchema(t *testing.T) {
	for action := range actionBindings {
		requirement := NewCatalogRequirement(action)
		if !requirement.RequireSchemaPin || len(requirement.InputSchemaDigest) != sha256.Size || len(requirement.ResultSchemaDigest) != sha256.Size {
			t.Errorf("live action %q is missing exact schema pins: require=%v input=%d result=%d", action, requirement.RequireSchemaPin, len(requirement.InputSchemaDigest), len(requirement.ResultSchemaDigest))
		}
	}
	stream := NewCatalogRequirement("agent.chat.stream")
	if !stream.RequireEventSchemaPin || len(stream.EventSchemaDigest) != sha256.Size {
		t.Fatalf("durable chat stream event schema pin = require %v length %d, want true/%d", stream.RequireEventSchemaPin, len(stream.EventSchemaDigest), sha256.Size)
	}
}

func TestCatalogLookupRejectsUnpinnedLiveAction(t *testing.T) {
	const action = "agent.test.unpinned"
	actionBindings[action] = actionBinding{capabilityID: "agent.test.v1", operation: "test"}
	defer delete(actionBindings, action)
	if _, err := catalogRequirementForLookup(action); err == nil {
		t.Fatal("live lookup accepted an action without an exact schema pin")
	}
}

func TestModelCatalogDefaultsKindAtGatewayBoundary(t *testing.T) {
	binding, ok := actionBindingFor("agent.models.list")
	if !ok {
		t.Fatal("agent.models.list binding is missing")
	}
	raw, err := transformCapabilityRequest("agent.models.list", "operation-id", map[string]any{"provider": "openrouter"}, binding)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"model_kind":"conversation"`)) {
		t.Fatalf("default model kind was not canonicalized: %s", raw)
	}

	raw, err = transformCapabilityRequest("agent.models.list", "operation-id", map[string]any{"model_kind": "embedding"}, binding)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"model_kind":"embedding"`)) {
		t.Fatalf("explicit model kind was replaced: %s", raw)
	}
}

func TestRunnerOperationIDForTurnIsStableAndCanonical(t *testing.T) {
	runner := &Runner{config: RunnerConfig{OwnerID: func() string { return "@owner:example" }}}
	const idempotencyKey = "11111111-1111-4111-8111-111111111111"
	params := map[string]any{"idempotency_key": idempotencyKey}
	first := runner.operationIDFor(params)
	second := runner.operationIDFor(params)
	if first != idempotencyKey || first != second {
		t.Fatalf("turn operation id is not stable: %q/%q", first, second)
	}
	if stopped := runner.operationIDFor(map[string]any{"idempotency_key": idempotencyKey, "turn_id": "22222222-2222-4222-8222-222222222222"}); stopped != idempotencyKey {
		t.Fatalf("stop operation id = %q, want mutation idempotency key %q", stopped, idempotencyKey)
	}
	if _, ok := actionBindingFor("agent.chat"); !ok {
		t.Fatal("chat binding missing")
	}
}

func TestTransformChatRequestEmitsExactAgentInput(t *testing.T) {
	binding, ok := actionBindingFor("agent.chat.stream")
	if !ok {
		t.Fatal("agent.chat.stream binding is missing")
	}
	raw, err := transformCapabilityRequest("agent.chat.stream", "operation-id", map[string]any{
		"idempotency_key":        "11111111-1111-4111-8111-111111111111",
		"conversation_id":        "22222222-2222-4222-8222-222222222222",
		"message":                "run this",
		"model_profile_id":       "33333333-3333-4333-8333-333333333333",
		"model_profile_revision": int64(3),
		"credential_version":     int64(4),
		"accepted_attachment_ids": []any{
			"44444444-4444-4444-8444-444444444444",
		},
		"extensions": []any{map[string]any{
			"kind": "mcp", "id": "55555555-5555-4555-8555-555555555555", "pinned_version": "1.2.3",
			"digest": strings.Repeat("a", 64), "allowed_tools": []any{"write_html"},
		}},
		"after_seq":         int64(7),
		"turn_id":           "must-not-cross",
		"client_message_id": "must-not-cross",
		"request_id":        "must-not-cross",
		"prompt":            "must-not-cross",
	}, binding)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte(`{"accepted_attachment_ids":["44444444-4444-4444-8444-444444444444"],"conversation_id":"22222222-2222-4222-8222-222222222222","credential_version":4,"extensions":[{"allowed_tools":["write_html"],"digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","id":"55555555-5555-4555-8555-555555555555","kind":"mcp","pinned_version":"1.2.3"}],"idempotency_key":"11111111-1111-4111-8111-111111111111","message":"run this","model_profile_id":"33333333-3333-4333-8333-333333333333","model_profile_revision":3}`)
	if !bytes.Equal(raw, want) {
		t.Fatalf("canonical Agent chat input = %s, want %s", raw, want)
	}
}

func TestTransformTurnStopRequestPassesExactTypedMutation(t *testing.T) {
	binding, ok := actionBindingFor("agent.chat.turn.stop")
	if !ok {
		t.Fatal("agent.chat.turn.stop binding is missing")
	}
	raw, err := transformCapabilityRequest("agent.chat.turn.stop", "transport-operation", map[string]any{
		"idempotency_key":   "11111111-1111-4111-8111-111111111111",
		"turn_id":           "22222222-2222-4222-8222-222222222222",
		"expected_revision": int64(3),
	}, binding)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte(`{"expected_revision":3,"idempotency_key":"11111111-1111-4111-8111-111111111111","turn_id":"22222222-2222-4222-8222-222222222222"}`)
	if !bytes.Equal(raw, want) {
		t.Fatalf("typed turn stop input = %s, want %s", raw, want)
	}
}

func TestTransformTurnSteerRequestPassesExactTypedMutation(t *testing.T) {
	binding, ok := actionBindingFor("agent.chat.turn.steer")
	if !ok {
		t.Fatal("agent.chat.turn.steer binding is missing")
	}
	raw, err := transformCapabilityRequest("agent.chat.turn.steer", "transport-operation", map[string]any{
		"idempotency_key":   "11111111-1111-4111-8111-111111111111",
		"turn_id":           "22222222-2222-4222-8222-222222222222",
		"expected_revision": int64(3),
		"instruction":       "focus on the latest guidance",
	}, binding)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte(`{"expected_revision":3,"idempotency_key":"11111111-1111-4111-8111-111111111111","instruction":"focus on the latest guidance","turn_id":"22222222-2222-4222-8222-222222222222"}`)
	if !bytes.Equal(raw, want) {
		t.Fatalf("typed turn steer input = %s, want %s", raw, want)
	}
}

func TestNativeEventsFromResultProjectsCanonicalChatResponse(t *testing.T) {
	response := durableChatResponseForTest("done", []any{map[string]any{"kind": "room", "room_id": "!room:example"}}, []any{"11111111-1111-4111-8111-111111111111"}, []any{"22222222-2222-4222-8222-222222222222"})
	result, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	events, err := nativeEventsFromResult(result, 17, durableTestStartID, actionResultAuthority{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Event != "done" || events[0].Seq != 17 || events[0].Data["sequence"] != int64(17) {
		t.Fatalf("canonical result events = %#v", events)
	}
	done := events[0].Data
	if done["idempotency_key"] != durableTestStartID || done["conversation_id"] != durableTestConversationID || done["turn_id"] != durableTestStartID || done["revision"] != float64(2) {
		t.Fatalf("done event identity = %#v", done)
	}
	if len(done["references"].([]any)) != 1 || len(done["related_task_ids"].([]any)) != 1 || len(done["related_plan_ids"].([]any)) != 1 {
		t.Fatalf("done event did not promote server-authored linkage: %#v", done)
	}
}

func TestNativeChatStreamPreservesReasoningOnDeltaAndDone(t *testing.T) {
	deltaJSON := []byte(`{"kind":"delta","idempotency_key":"` + durableTestStartID + `","conversation_id":"` + durableTestConversationID + `","turn_id":"` + durableTestTurnID + `","revision":2,"reasoning_content":"partial reasoning"}`)
	delta, terminal, err := nativeEventFromProto(&capv1.WatchOperationEvent{
		OperationId: durableTestStartID,
		Sequence:    6,
		Event:       &capv1.WatchOperationEvent_Progress{Progress: &capv1.ProgressEvent{EventJson: deltaJSON}},
	}, actionResultAuthority{})
	if err != nil || terminal || delta == nil || delta.Event != "delta" || delta.Data["reasoning_content"] != "partial reasoning" {
		t.Fatalf("reasoning delta = event %#v terminal %v err %v", delta, terminal, err)
	}
	if _, present := delta.Data["text"]; present {
		t.Fatalf("reasoning-only delta gained text: %#v", delta.Data)
	}

	response := durableChatResponseForTest("answer", nil, nil, nil)
	response["message"].(map[string]any)["reasoning_content"] = "complete reasoning"
	resultJSON, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	events, err := nativeEventsFromResult(resultJSON, 7, durableTestStartID, actionResultAuthority{})
	if err != nil || len(events) != 1 || events[0].Event != "done" || events[0].Data["reasoning_content"] != "complete reasoning" {
		t.Fatalf("reasoning done events = %#v, err %v", events, err)
	}
}

func TestNativeEventsFromResultProjectsExecutionArtifactReference(t *testing.T) {
	reference := validExecutionArtifactReferenceForTest()
	result, err := json.Marshal(durableChatResponseForTest("done", []any{reference}, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	events, err := nativeEventsFromResult(result, 17, durableTestStartID, actionResultAuthority{ownerID: "@owner:example.test", accountGeneration: 7})
	if err != nil || len(events) != 1 || !reflect.DeepEqual(events[0].Data["references"], []any{reference}) {
		t.Fatalf("artifact result events=%#v err=%v", events, err)
	}
}

func TestCompletedResultUsesDurableTurnRevisionAfterToolRounds(t *testing.T) {
	artifactIDs := []string{
		"10000000-0000-4000-8000-000000000001", "10000000-0000-4000-8000-000000000002", "10000000-0000-4000-8000-000000000003",
		"10000000-0000-4000-8000-000000000004", "10000000-0000-4000-8000-000000000005", "10000000-0000-4000-8000-000000000006",
		"10000000-0000-4000-8000-000000000007", "10000000-0000-4000-8000-000000000008", "10000000-0000-4000-8000-000000000009",
	}
	executionIDs := []string{
		"20000000-0000-4000-8000-000000000001", "20000000-0000-4000-8000-000000000002", "20000000-0000-4000-8000-000000000003",
	}
	references := make([]any, 0, len(artifactIDs))
	for index, artifactID := range artifactIDs {
		reference := validExecutionArtifactReferenceForTest()
		reference["artifact_id"] = artifactID
		reference["execution_id"] = executionIDs[index/3]
		references = append(references, reference)
	}
	result, err := json.Marshal(durableChatResponseForTest("done", references, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	turn := map[string]any{
		"turn_id": durableTestTurnID, "idempotency_key": durableTestStartID,
		"conversation_id": durableTestConversationID, "state": "completed",
		"revision": float64(11), "last_sequence": float64(97), "terminal_code": "", "terminal_summary": "",
		"created_at": "2026-08-14T18:10:42.667862Z", "updated_at": "2026-08-14T18:11:51.943944Z",
	}
	events, err := nativeEventsFromCompletedResult(result, 1601, durableTestStartID, actionResultAuthority{
		ownerID: "@owner:example.test", accountGeneration: 7,
	}, turn)
	if err != nil || len(events) != 1 {
		t.Fatalf("completed result events=%#v err=%v", events, err)
	}
	done := events[0]
	if done.Event != "done" || done.Seq != 1601 || done.Data["revision"] != int64(11) || done.Data["turn_id"] != durableTestTurnID {
		t.Fatalf("completed result projection=%#v", done)
	}
	if len(done.Data["references"].([]any)) != 9 || done.Data["revision"].(int64) <= 10 {
		t.Fatalf("terminal result lost artifact linkage or regressed after tool-round revision 10: %#v", done.Data)
	}
}

func TestNativeEventsFromResultRejectsOperationIdentityMismatch(t *testing.T) {
	result, err := json.Marshal(durableChatResponseForTest("done", nil, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if events, err := nativeEventsFromResult(result, 17, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", actionResultAuthority{}); err == nil || events != nil {
		t.Fatalf("mismatched operation result = events %#v err %v", events, err)
	}
}

func TestNativeChatStreamDoneRejectsMalformedReferenceOnProgressAndResult(t *testing.T) {
	progress := &capv1.WatchOperationEvent{
		OperationId: "operation-1",
		Sequence:    2,
		Event: &capv1.WatchOperationEvent_Progress{Progress: &capv1.ProgressEvent{
			EventJson: []byte(`{"kind":"done","response":{"message":{"content":"done","references":[{"kind":"execution_run"}]},"done":true,"references":[{"kind":"execution_run"}]}}`),
		}},
	}
	if event, terminal, err := nativeEventFromProto(progress, actionResultAuthority{}); event != nil || !terminal || err == nil {
		t.Fatalf("malformed progress done = event %#v terminal %v err %v", event, terminal, err)
	}

	result := []byte(`{"message":{"content":"done","references":[{"kind":"service_binding"}]},"done":true,"references":[{"kind":"service_binding"}]}`)
	if events, err := nativeEventsFromResult(result, 2, durableTestStartID, actionResultAuthority{}); err == nil || events != nil {
		t.Fatalf("malformed result done = events %#v err %v", events, err)
	}
}

func TestNativeChatProgressDonePromotesNestedResponseLinkage(t *testing.T) {
	const startID = "33333333-3333-4333-8333-333333333333"
	const conversationID = "44444444-4444-4444-8444-444444444444"
	progress := &capv1.WatchOperationEvent{
		OperationId: "operation-1",
		Sequence:    2,
		Event: &capv1.WatchOperationEvent_Progress{Progress: &capv1.ProgressEvent{
			EventJson: []byte(`{"kind":"done","idempotency_key":"` + startID + `","conversation_id":"` + conversationID + `","turn_id":"55555555-5555-4555-8555-555555555555","revision":2,"response":{"idempotency_key":"` + startID + `","conversation_id":"` + conversationID + `","revision":3,"model_profile_id":"66666666-6666-4666-8666-666666666666","message":{"content":"finished","related_task_ids":["11111111-1111-4111-8111-111111111111"],"related_plan_ids":["22222222-2222-4222-8222-222222222222"],"references":[{"kind":"channel_post","room_id":"!room:example","channel_id":"!room:example","post_id":"post-1"}]},"done":true,"related_task_ids":["11111111-1111-4111-8111-111111111111"],"related_plan_ids":["22222222-2222-4222-8222-222222222222"],"references":[{"kind":"channel_post","room_id":"!room:example","channel_id":"!room:example","post_id":"post-1"}]}}`),
		}},
	}
	event, terminal, err := nativeEventFromProto(progress, actionResultAuthority{})
	if err != nil || !terminal || event == nil || event.Event != "done" {
		t.Fatalf("progress done = event %#v terminal %v err %v", event, terminal, err)
	}
	if event.Data["text"] != "finished" || len(event.Data["references"].([]any)) != 1 || len(event.Data["related_task_ids"].([]any)) != 1 || len(event.Data["related_plan_ids"].([]any)) != 1 {
		t.Fatalf("progress done linkage = %#v", event.Data)
	}
}

func TestNativeChatProgressProjectsOnlyAgentAuthoredTurnIdentity(t *testing.T) {
	const startID = "11111111-1111-4111-8111-111111111111"
	const conversationID = "22222222-2222-4222-8222-222222222222"
	const internalTurnID = "33333333-3333-4333-8333-333333333333"
	progress := &capv1.WatchOperationEvent{
		OperationId: startID,
		Sequence:    3,
		Event: &capv1.WatchOperationEvent_Progress{Progress: &capv1.ProgressEvent{EventJson: []byte(
			`{"kind":"accepted","idempotency_key":"` + startID + `","conversation_id":"` + conversationID + `","turn_id":"` + internalTurnID + `","revision":1}`,
		)}},
	}
	event, terminal, err := nativeEventFromProto(progress, actionResultAuthority{})
	if err != nil || terminal || event == nil || event.Event != "accepted" {
		t.Fatalf("accepted progress = event %#v terminal %v err %v", event, terminal, err)
	}
	if event.Data["idempotency_key"] != startID || event.Data["turn_id"] != internalTurnID || event.Data["revision"] != float64(1) || event.Data["sequence"] != int64(3) {
		t.Fatalf("accepted progress identity = %#v", event.Data)
	}
	if _, leaked := event.Data["operation_id"]; leaked {
		t.Fatalf("transport operation id leaked into business progress: %#v", event.Data)
	}

	transportAccepted := &capv1.WatchOperationEvent{
		OperationId: startID,
		Sequence:    1,
		Event:       &capv1.WatchOperationEvent_Accepted{Accepted: &capv1.AcceptedEvent{}},
	}
	if projected, terminal, err := nativeEventFromProto(transportAccepted, actionResultAuthority{}); err != nil || terminal || projected != nil {
		t.Fatalf("transport accepted was projected as a business turn: event=%#v terminal=%v err=%v", projected, terminal, err)
	}
}

func TestNativeChatProgressProjectsWaitingConfirmation(t *testing.T) {
	progress := &capv1.WatchOperationEvent{
		OperationId: durableTestStartID,
		Sequence:    4,
		Event: &capv1.WatchOperationEvent_Progress{Progress: &capv1.ProgressEvent{EventJson: []byte(
			`{"kind":"waiting_confirmation","idempotency_key":"` + durableTestStartID + `","conversation_id":"` + durableTestConversationID + `","turn_id":"` + durableTestTurnID + `","revision":3,"confirmation_id":"11111111-1111-4111-8111-111111111111","execution_id":"33333333-3333-4333-8333-333333333333","status":"waiting_confirmation"}`,
		)}},
	}
	event, terminal, err := nativeEventFromProto(progress, actionResultAuthority{})
	if err != nil || terminal || event == nil || event.Event != "waiting_confirmation" || event.Seq != 4 {
		t.Fatalf("waiting confirmation progress = event %#v terminal %v err %v", event, terminal, err)
	}
	if event.Data["confirmation_id"] != "11111111-1111-4111-8111-111111111111" ||
		event.Data["execution_id"] != "33333333-3333-4333-8333-333333333333" ||
		event.Data["status"] != "waiting_confirmation" {
		t.Fatalf("waiting confirmation authority = %#v", event.Data)
	}
	if _, legacy := event.Data["attempt_id"]; legacy {
		t.Fatalf("waiting confirmation exposed superseded attempt authority: %#v", event.Data)
	}
}

func TestNativeChatProgressProjectsWorkerStatusOnce(t *testing.T) {
	progress := &capv1.WatchOperationEvent{
		OperationId: durableTestStartID,
		Sequence:    5,
		Event: &capv1.WatchOperationEvent_Progress{Progress: &capv1.ProgressEvent{EventJson: []byte(
			`{"kind":"worker_status","idempotency_key":"` + durableTestStartID + `","conversation_id":"` + durableTestConversationID + `","turn_id":"` + durableTestTurnID + `","revision":4,"execution_id":"33333333-3333-4333-8333-333333333333","status":"provisioning","created_at":"2026-08-15T03:13:30.123Z"}`,
		)}},
	}
	event, terminal, err := nativeEventFromProto(progress, actionResultAuthority{})
	if err != nil || terminal || event == nil || event.Event != "worker_status" || event.Seq != 5 {
		t.Fatalf("worker status progress = event %#v terminal %v err %v", event, terminal, err)
	}
	if len(event.Data) != 9 || event.Data["kind"] != "worker_status" ||
		event.Data["execution_id"] != "33333333-3333-4333-8333-333333333333" ||
		event.Data["status"] != "provisioning" || event.Data["created_at"] != "2026-08-15T03:13:30.123Z" ||
		event.Data["sequence"] != int64(5) {
		t.Fatalf("worker status payload = %#v", event.Data)
	}
}

func TestNativeChatProgressRejectsWaitingConfirmationSchemaDrift(t *testing.T) {
	base := map[string]any{
		"kind": "waiting_confirmation", "idempotency_key": durableTestStartID,
		"conversation_id": durableTestConversationID, "turn_id": durableTestTurnID, "revision": 3,
		"confirmation_id": "11111111-1111-4111-8111-111111111111",
		"execution_id":    "33333333-3333-4333-8333-333333333333",
		"status":          "waiting_confirmation",
	}
	for name, mutate := range map[string]func(map[string]any){
		"superseded attempt authority": func(event map[string]any) {
			event["attempt_id"] = "22222222-2222-4222-8222-222222222222"
		},
		"mixed text": func(event map[string]any) { event["text"] = "not allowed" },
		"authority on non-waiting": func(event map[string]any) {
			event["kind"] = "delta"
		},
	} {
		t.Run(name, func(t *testing.T) {
			payload := cloneParams(base)
			mutate(payload)
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			progress := &capv1.WatchOperationEvent{
				OperationId: durableTestStartID, Sequence: 4,
				Event: &capv1.WatchOperationEvent_Progress{Progress: &capv1.ProgressEvent{EventJson: raw}},
			}
			if event, terminal, err := nativeEventFromProto(progress, actionResultAuthority{}); err == nil || !terminal || event != nil {
				t.Fatalf("schema drift projected: event=%#v terminal=%v err=%v", event, terminal, err)
			}
		})
	}
}

func TestNativeChatStreamRejectsCloudWorkerReferenceFromForeignPreparedGeneration(t *testing.T) {
	reference := validExecutionReferenceForTest()
	reference["account_generation"] = float64(8)
	result, err := json.Marshal(durableChatResponseForTest("done", []any{reference}, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	authority := actionResultAuthority{ownerID: "@owner:example.test", accountGeneration: 7}
	if events, err := nativeEventsFromResult(result, 1, durableTestStartID, authority); err == nil || events != nil {
		t.Fatalf("foreign result generation = events %#v err %v", events, err)
	}

	progressJSON, err := json.Marshal(map[string]any{
		"kind": "done", "idempotency_key": durableTestStartID,
		"conversation_id": durableTestConversationID, "turn_id": durableTestTurnID, "revision": float64(2),
		"response": durableChatResponseForTest("done", []any{reference}, nil, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	progress := &capv1.WatchOperationEvent{
		OperationId: "operation-1", Sequence: 2,
		Event: &capv1.WatchOperationEvent_Progress{Progress: &capv1.ProgressEvent{EventJson: progressJSON}},
	}
	if event, terminal, err := nativeEventFromProto(progress, authority); err == nil || !terminal || event != nil {
		t.Fatalf("foreign progress generation = event %#v terminal %v err %v", event, terminal, err)
	}

	deltaJSON, err := json.Marshal(map[string]any{
		"kind":       "delta",
		"references": []any{reference},
	})
	if err != nil {
		t.Fatal(err)
	}
	delta := &capv1.WatchOperationEvent{
		OperationId: "operation-1", Sequence: 1,
		Event: &capv1.WatchOperationEvent_Progress{Progress: &capv1.ProgressEvent{EventJson: deltaJSON}},
	}
	if event, terminal, err := nativeEventFromProto(delta, authority); err == nil || !terminal || event != nil {
		t.Fatalf("foreign delta generation = event %#v terminal %v err %v", event, terminal, err)
	}
}

const (
	durableTestStartID        = "66666666-6666-4666-8666-666666666666"
	durableTestConversationID = "77777777-7777-4777-8777-777777777777"
	durableTestTurnID         = "88888888-8888-4888-8888-888888888888"
)

func durableChatResponseForTest(content string, references, taskIDs, planIDs []any) map[string]any {
	response := canonicalChatResponseForTest(content, references, taskIDs, planIDs)
	response["idempotency_key"] = durableTestStartID
	response["conversation_id"] = durableTestConversationID
	response["revision"] = float64(2)
	response["model_profile_id"] = "99999999-9999-4999-8999-999999999999"
	return response
}

func TestNativeEventFromProtoDoesNotForgeIdentityForCapabilityError(t *testing.T) {
	event, terminal, err := nativeEventFromProto(&capv1.WatchOperationEvent{
		OperationId: "operation-1",
		Sequence:    24,
		Event: &capv1.WatchOperationEvent_Error{Error: &capv1.ErrorEvent{Error: &capv1.CapabilityError{
			Code: capv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED,
			Details: map[string]string{
				"code": KnowledgeQuotaExceededCode,
			},
		}}},
	}, actionResultAuthority{})
	var capabilityErr *CapabilityError
	if !terminal || !errors.As(err, &capabilityErr) || capabilityErr.ClientCode != KnowledgeQuotaExceededCode {
		t.Fatalf("knowledge quota event terminal=%v err=%#v", terminal, err)
	}
	if event != nil {
		t.Fatalf("identity-free capability error was projected as a business event: %#v", event)
	}
}

func TestTerminalChatErrorFromAuthoritativeFailedTurn(t *testing.T) {
	turn := map[string]any{
		"turn_id":          durableTestTurnID,
		"idempotency_key":  durableTestStartID,
		"conversation_id":  durableTestConversationID,
		"state":            "failed",
		"revision":         float64(5),
		"last_sequence":    float64(16),
		"terminal_code":    "provider_uncertain",
		"terminal_summary": "model dispatch outcome is unknown",
		"created_at":       "2026-08-11T10:24:42.331Z",
		"updated_at":       "2026-08-11T10:26:44.558Z",
	}
	event, err := terminalChatErrorFromTurn(turn, 17)
	if err != nil {
		t.Fatal(err)
	}
	if event == nil || event.Event != "error" || event.Seq != 17 {
		t.Fatalf("terminal event = %#v", event)
	}
	if event.Data["idempotency_key"] != durableTestStartID || event.Data["conversation_id"] != durableTestConversationID || event.Data["turn_id"] != durableTestTurnID || event.Data["revision"] != int64(5) {
		t.Fatalf("terminal identity = %#v", event.Data)
	}
	if event.Data["error_code"] != "provider_uncertain" || event.Data["error_summary"] != "model dispatch outcome is unknown" || event.Data["sequence"] != int64(17) {
		t.Fatalf("terminal failure = %#v", event.Data)
	}
}

func TestTerminalChatErrorRejectsMissingFailureAuthority(t *testing.T) {
	turn := map[string]any{
		"turn_id":          durableTestTurnID,
		"idempotency_key":  durableTestStartID,
		"conversation_id":  durableTestConversationID,
		"state":            "failed",
		"revision":         float64(5),
		"last_sequence":    float64(16),
		"terminal_code":    "",
		"terminal_summary": "",
		"created_at":       "2026-08-11T10:24:42.331Z",
		"updated_at":       "2026-08-11T10:26:44.558Z",
	}
	if event, err := terminalChatErrorFromTurn(turn, 17); !errors.Is(err, ErrInvalidActionResult) || event != nil {
		t.Fatalf("missing failure authority = event %#v err %v", event, err)
	}
}

func TestDurableObservationPreservesLastAuthoritativeCursor(t *testing.T) {
	observation := durableChatObservation{}
	observation.captureEvent(&agentstream.Event{Seq: 7, Data: map[string]any{
		"idempotency_key": durableTestStartID, "conversation_id": durableTestConversationID,
		"turn_id": durableTestTurnID, "revision": float64(2),
	}})
	observation.captureTurn(map[string]any{
		"turn_id": durableTestTurnID, "idempotency_key": durableTestStartID,
		"conversation_id": durableTestConversationID, "state": "running",
		"revision": float64(4), "last_sequence": float64(9), "terminal_code": "", "terminal_summary": "",
		"created_at": "2026-08-11T10:24:42.331Z", "updated_at": "2026-08-11T10:26:44.558Z",
	})
	err := observation.interrupted(ErrWatchIdleTimeout)
	var interrupted *ObservationInterruptedError
	if !errors.As(err, &interrupted) || !errors.Is(err, ErrWatchIdleTimeout) {
		t.Fatalf("observation error = %#v", err)
	}
	if interrupted.IdempotencyKey != durableTestStartID || interrupted.ConversationID != durableTestConversationID ||
		interrupted.TurnID != durableTestTurnID || interrupted.Revision != 4 || interrupted.Sequence != 9 {
		t.Fatalf("observation authority = %#v", interrupted)
	}
}

func TestObservationAttachmentLossClassification(t *testing.T) {
	for _, err := range []error{
		ErrWatchIdleTimeout,
		io.EOF,
		status.Error(codes.Unavailable, "connection lost"),
		status.Error(codes.DeadlineExceeded, "watch timeout"),
		status.Error(codes.ResourceExhausted, "stream limit"),
	} {
		if !observationAttachmentLoss(err) {
			t.Errorf("observation loss %v was treated as a task failure", err)
		}
	}
	for _, err := range []error{errors.New("invalid result"), status.Error(codes.InvalidArgument, "bad request")} {
		if observationAttachmentLoss(err) {
			t.Errorf("deterministic failure %v was treated as an observation loss", err)
		}
	}
}

func TestNonterminalLedgerTurnIsNotProjectedAsTaskFailure(t *testing.T) {
	turn := map[string]any{
		"turn_id": durableTestTurnID, "idempotency_key": durableTestStartID,
		"conversation_id": durableTestConversationID, "state": "waiting_confirmation",
		"revision": float64(5), "last_sequence": float64(16), "terminal_code": "", "terminal_summary": "",
		"created_at": "2026-08-11T10:24:42.331Z", "updated_at": "2026-08-11T10:26:44.558Z",
	}
	if event := terminalChatEventFromTurn(turn, 17); event != nil {
		t.Fatalf("nonterminal ledger turn became a terminal event: %#v", event)
	}
}

func TestOperationControlPermissionIsFreshAndTargetBound(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{config: RunnerConfig{
		GrantPrivateKey: privateKey,
		GrantCodec:      capv1.GrantCodec{Now: func() time.Time { return time.UnixMilli(1_700_000_100_000) }},
	}}
	base := &capv1.PermissionContext{AuthenticatedOwnerId: "owner-123", AccountGeneration: 7}
	entry := capv1.NewCallContext("018f2f1e-7b5b-7b21-8f2e-4a6f3b1d9c12", "018f2f1e-7b5b-7b21-8f2e-4a6f3b1d9c12", 1_700_000_150_000)
	entry, err = capv1.AppendCallNode(entry, capv1.NodeMessage)
	if err != nil {
		t.Fatal(err)
	}
	permission, err := runner.operationControlPermission(entry, "018f2f1e-7b5b-7b21-8f2e-4a6f3b1d9c12", "watch", base)
	if err != nil {
		t.Fatal(err)
	}
	if string(permission.GetCapabilityGrant()) == string(base.GetCapabilityGrant()) {
		t.Fatal("control grant must not reuse the root permission grant")
	}
	actual, err := capv1.AppendCallNode(entry, capv1.NodeAgent)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := runner.config.GrantCodec.VerifyOperationControlGrant(permission.GetCapabilityGrant(), publicKey, capv1.OperationControlGrantBinding{
		CallContext: actual, OwnerID: base.GetAuthenticatedOwnerId(), AccountGeneration: base.GetAccountGeneration(),
		OperationID: entry.GetRootOperationId(), ControlAction: "watch", ControlScope: "operation:control:watch",
	})
	if err != nil {
		t.Fatalf("control grant did not verify at Agent boundary: %v", err)
	}
	if verified.OperationID != entry.GetRootOperationId() || verified.EntryRoute != capv1.NodeMessage || verified.EntryHop != 1 {
		t.Fatalf("unexpected control grant claims: %#v", verified)
	}
}

func TestPrepareGrantCarriesRootDigestForProductDelegation(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{config: RunnerConfig{GrantPrivateKey: privateKey}}
	operationID := "018f2f1e-7b5b-7b21-8f2e-4a6f3b1d9c12"
	entry := capv1.NewCallContext(operationID, operationID, time.Now().Add(time.Minute).UnixMilli())
	entry, err = capv1.AppendCallNode(entry, capv1.NodeMessage)
	if err != nil {
		t.Fatal(err)
	}
	operation := &capv1.OperationDescriptor{OperationId: "invoke_product", RequiredScopes: []string{"agent:skills:execute"}, InputSchemaJson: `{"type":"object"}`}
	descriptor := &capv1.CapabilityDescriptor{CapabilityId: "agent.skills.v1", SemanticVersion: "1.0.0", ProtocolVersion: 1, Operations: []*capv1.OperationDescriptor{operation}}
	catalog := &capv1.DescribeCapabilitiesResponse{CatalogDigest: bytes.Repeat([]byte{1}, sha256.Size), Capabilities: []*capv1.CapabilityDescriptor{descriptor}}
	permission := &capv1.PermissionContext{AuthenticatedOwnerId: "owner-123", AccountGeneration: 7, GrantedScopes: []string{"agent:product:execute"}}
	rootDigest := sha256.Sum256([]byte("root request"))
	if _, _, err := runner.prepareGrantWithCatalog(catalog, entry, operationID, descriptor, operation, permission, rootDigest[:]); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(permission.GetRootRequestDigest(), rootDigest[:]) {
		t.Fatalf("permission root digest = %x, want %x", permission.GetRootRequestDigest(), rootDigest)
	}
}
