package nativeagent

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestNativeAgentReferencesExtractRoomsAndPosts(t *testing.T) {
	msgs := []*schema.Message{
		schema.ToolMessage(`{"result":{"rooms":[{"room_id":"!r:example","type":"direct","name":"Alice"}]}}`, "rooms", schema.WithToolName("dirextalk_rooms_search")),
		schema.ToolMessage(`{"result":{"room_id":"!r:example","channel_id":"c1","name":"News","posts":[{"post_id":"p1","msg":"hello"}]}}`, "posts", schema.WithToolName("dirextalk_channel_posts_list")),
	}
	refs := nativeAgentReferences(msgs)
	if len(refs) != 2 || refs[0]["kind"] != "room" || refs[1]["kind"] != "channel_post" {
		t.Fatalf("unexpected refs: %#v", refs)
	}
}

func TestNativeAgentReferencesIgnoreMalformedToolResults(t *testing.T) {
	if refs := nativeAgentReferences([]*schema.Message{schema.ToolMessage("not-json", "rooms")}); len(refs) != 0 {
		t.Fatalf("refs = %#v", refs)
	}
}

func TestNativeAgentReferencesRequireCompleteIdentifiers(t *testing.T) {
	msgs := []*schema.Message{
		schema.ToolMessage(`{"result":{"room_id":"","name":"Missing room"}}`, "messages", schema.WithToolName("dirextalk_messages_list")),
		schema.ToolMessage(`{"result":{"room_id":"","channel_id":"c1","posts":[{"post_id":"p1"}]}}`, "posts", schema.WithToolName("dirextalk_channel_posts_list")),
		schema.ToolMessage(`{"result":{"room_id":"!missing-channel:example","posts":[{"post_id":"p2"}]}}`, "posts", schema.WithToolName("dirextalk_channel_posts_list")),
		schema.ToolMessage(`{"result":{"room_id":"!missing-post:example","channel_id":"c1","posts":[{}]}}`, "posts", schema.WithToolName("dirextalk_channel_posts_list")),
		schema.ToolMessage(`{"result":{"room_id":"!room:example","name":"Room"}}`, "messages", schema.WithToolName("dirextalk_messages_list")),
		schema.ToolMessage(`{"result":{"room_id":"!channel:example","channel_id":"c1","posts":[{"post_id":"p1"}]}}`, "posts", schema.WithToolName("dirextalk_channel_posts_list")),
	}

	refs := nativeAgentReferences(msgs)
	if len(refs) != 2 {
		t.Fatalf("references = %#v, want one room and one complete channel post", refs)
	}
	if refs[0]["kind"] != "room" || refs[0]["room_id"] != "!room:example" {
		t.Fatalf("room reference = %#v", refs[0])
	}
	if refs[1]["kind"] != "channel_post" || refs[1]["room_id"] != "!channel:example" || refs[1]["channel_id"] != "c1" || refs[1]["post_id"] != "p1" {
		t.Fatalf("channel post reference = %#v", refs[1])
	}
}

func TestNativeAgentReferencesProjectCanonicalExecutionV2Identities(t *testing.T) {
	planID := "11111111-1111-4111-8111-111111111111"
	runID := "22222222-2222-4222-8222-222222222222"
	stageID := "33333333-3333-4333-8333-333333333333"
	targetID := "44444444-4444-4444-8444-444444444444"
	confirmationID := "55555555-5555-4555-8555-555555555555"
	bindingID := "66666666-6666-4666-8666-666666666666"
	deploymentID := "77777777-7777-4777-8777-777777777777"
	projectID := "88888888-8888-4888-8888-888888888888"
	planDigest := strings.Repeat("a", 64)
	runDigest := strings.Repeat("b", 64)
	stageDigest := strings.Repeat("c", 64)
	targetDigest := strings.Repeat("d", 64)
	serviceDigest := strings.Repeat("1", 64)

	planResult := `{"result":{"plan":{"id":"` + planID + `","revision":4,"digest":"` + planDigest + `"}}}`
	runResult := `{"result":{"run":{"run_id":"` + runID + `","revision":2,"run_digest":"` + runDigest + `","plan_id":"` + planID + `","plan_revision":4,"plan_digest":"` + planDigest + `","deployment_id":"` + deploymentID + `","status":"waiting_user"},"stages":[{"confirmation_id":"` + confirmationID + `","stage_id":"` + stageID + `","stage_revision":1,"stage_digest":"` + stageDigest + `","run_id":"` + runID + `","run_revision":2,"plan_id":"` + planID + `","plan_revision":4,"target_id":"` + targetID + `","target_revision":3,"target_digest":"` + targetDigest + `"}]}}`
	bindingResult := `{"result":{"bindings":[{"binding_id":"` + bindingID + `","deployment_id":"` + deploymentID + `","project_id":"` + projectID + `","run_id":"` + runID + `","target_id":"` + targetID + `","target_revision":3,"target_digest":"` + targetDigest + `","revision":2,"digest":"` + serviceDigest + `"}]}}`
	msgs := []*schema.Message{
		schema.ToolMessage(planResult, "plan", schema.WithToolName("native_agent_execution_v2_plans_create")),
		// A duplicate get result must not create a second plan reference.
		schema.ToolMessage(planResult, "plan-get", schema.WithToolName("native_agent_execution_v2_plans_get")),
		schema.ToolMessage(runResult, "run", schema.WithToolName("native_agent_execution_v2_runs_create")),
		schema.ToolMessage(bindingResult, "bindings", schema.WithToolName("native_agent_execution_v2_service_bindings_list")),
	}

	refs := nativeAgentReferences(msgs)
	if len(refs) != 4 {
		t.Fatalf("references = %#v, want plan/run/confirmation/binding", refs)
	}
	if refs[0]["kind"] != "execution_plan" || refs[0]["plan_id"] != planID || refs[0]["plan_revision"] != uint64(4) || refs[0]["plan_digest"] != planDigest {
		t.Fatalf("plan reference = %#v", refs[0])
	}
	if refs[1]["kind"] != "execution_run" || refs[1]["run_id"] != runID || refs[1]["run_revision"] != uint64(2) || refs[1]["run_digest"] != runDigest {
		t.Fatalf("run reference = %#v", refs[1])
	}
	if refs[2]["kind"] != "execution_confirmation" || refs[2]["confirmation_id"] != confirmationID || refs[2]["stage_digest"] != stageDigest || refs[2]["stage_id"] != stageID {
		t.Fatalf("confirmation reference = %#v", refs[2])
	}
	if refs[3]["kind"] != "service_binding" || refs[3]["binding_id"] != bindingID || refs[3]["binding_revision"] != uint64(2) || refs[3]["binding_digest"] != serviceDigest {
		t.Fatalf("binding reference = %#v", refs[3])
	}
	for _, ref := range refs {
		for _, forbidden := range []string{"action", "route", "auto_confirm", "navigate"} {
			if _, exists := ref[forbidden]; exists {
				t.Fatalf("reference carries active field %q: %#v", forbidden, ref)
			}
		}
		if ref["kind"] == "pending_confirmation" {
			t.Fatalf("legacy actionable confirmation reference leaked: %#v", ref)
		}
	}
}

func TestNativeAgentReferencesRejectForgedOrIncompleteExecutionV2Identities(t *testing.T) {
	planID := "11111111-1111-4111-8111-111111111111"
	runID := "22222222-2222-4222-8222-222222222222"
	otherRunID := "99999999-9999-4999-8999-999999999999"
	digest := strings.Repeat("a", 64)
	msgs := []*schema.Message{
		schema.ToolMessage(`{"result":{"plan":{"id":"`+planID+`","revision":1}}}`, "plan", schema.WithToolName("native_agent_execution_v2_plans_get")),
		schema.ToolMessage(`{"result":{"run":{"run_id":"`+runID+`","revision":2,"run_digest":"`+digest+`","plan_id":"`+planID+`","plan_revision":1,"plan_digest":"`+digest+`"},"stages":[{"confirmation_id":"55555555-5555-4555-8555-555555555555","stage_id":"33333333-3333-4333-8333-333333333333","stage_revision":1,"stage_digest":"`+digest+`","run_id":"`+otherRunID+`","run_revision":2,"plan_id":"`+planID+`","plan_revision":1,"target_id":"44444444-4444-4444-8444-444444444444","target_revision":1,"target_digest":"`+digest+`"}]}}`, "run", schema.WithToolName("native_agent_execution_v2_runs_create")),
		schema.ToolMessage(`{"result":{"binding":{"binding_id":"66666666-6666-4666-8666-666666666666","revision":1,"digest":"`+digest+`"}}}`, "binding", schema.WithToolName("native_agent_execution_v2_service_bindings_get")),
		// Arbitrary invoked service output is never trusted as a binding reference.
		schema.ToolMessage(`{"result":{"binding":{"binding_id":"66666666-6666-4666-8666-666666666666","revision":1,"digest":"`+digest+`"}}}`, "invoke", schema.WithToolName("native_agent_execution_v2_service_bindings_invoke")),
	}
	refs := nativeAgentReferences(msgs)
	if len(refs) != 1 || refs[0]["kind"] != "execution_run" || refs[0]["run_id"] != runID {
		t.Fatalf("forged/incomplete references were accepted: %#v", refs)
	}
}
