package nativeagent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	coreworkload "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload"
	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
)

func withEC2BindingDigests(result string) string {
	return strings.Replace(result,
		`"template_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","credential_binding_digest"`,
		`"template_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","parameter_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","network_digest":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","secret_grant_digest":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","credential_binding_digest"`,
		1)
}

func TestNativeAgentReferencesExtractRoomsAndPostsInToolOrder(t *testing.T) {
	produced := []*schema.Message{
		schema.ToolMessage(`{"result":{"contacts":[{"display_name":"Ada","room_id":"!direct:example.com"},{"display_name":"Ada duplicate","room_id":"!direct:example.com"}]}}`, "call_contacts", schema.WithToolName("dirextalk_contacts_search")),
		schema.ToolMessage(`{"result":{"rooms":[{"type":"group","name":"Team","room_id":"!group:example.com"},{"type":"channel","name":"News","room_id":"!channel:example.com"}]}}`, "call_rooms", schema.WithToolName("dirextalk_rooms_search")),
		schema.ToolMessage(`{"result":{"room_id":"!group:example.com","name":"Team","messages":[{"msg":"matching message"}]}}`, "call_messages", schema.WithToolName("dirextalk_messages_list")),
		schema.ToolMessage(`{"result":{"room_id":"!channel:example.com","channel_id":"news","name":"News","posts":[{"post_id":"post_1","msg":"first post"},{"post_id":"post_2","msg":"second post"}]}}`, "call_posts", schema.WithToolName("dirextalk_channel_posts_list")),
		schema.ToolMessage(`{"result":{"room_id":"!migrated-channel:example.com","channel_id":"news","name":"News duplicate","posts":[{"post_id":"post_1","msg":"duplicate post"}]}}`, "call_duplicate_posts", schema.WithToolName("dirextalk_channel_posts_list")),
	}

	got := nativeAgentReferences(produced)
	if len(got) != 5 {
		t.Fatalf("references len = %d, want 5: %#v", len(got), got)
	}
	want := []map[string]any{
		{"kind": "room", "room_id": "!direct:example.com", "room_type": "direct", "title": "Ada"},
		{"kind": "room", "room_id": "!group:example.com", "room_type": "group", "title": "Team"},
		{"kind": "room", "room_id": "!channel:example.com", "room_type": "channel", "title": "News"},
		{"kind": "channel_post", "room_id": "!channel:example.com", "channel_id": "news", "post_id": "post_1", "title": "News", "preview": "first post"},
		{"kind": "channel_post", "room_id": "!channel:example.com", "channel_id": "news", "post_id": "post_2", "title": "News", "preview": "second post"},
	}
	for index := range want {
		for key, value := range want[index] {
			if got[index][key] != value {
				t.Fatalf("reference %d %s = %#v, want %#v; full=%#v", index, key, got[index][key], value, got)
			}
		}
	}
}

func TestNativeAgentReferencesUseMessagePreviewAndIgnoreInvalidResults(t *testing.T) {
	produced := []*schema.Message{
		schema.ToolMessage(`{"result":{"room_id":"!room:example.com","name":"Room","messages":[{"msg":" matched text "}]}}`, "call_messages", schema.WithToolName("dirextalk_messages_list")),
		schema.ToolMessage(`{"error":"denied"}`, "call_error", schema.WithToolName("dirextalk_rooms_search")),
		schema.ToolMessage(`not-json`, "call_bad", schema.WithToolName("dirextalk_contacts_list")),
		schema.ToolMessage(`{"result":{"room_id":"","name":"Missing"}}`, "call_missing", schema.WithToolName("dirextalk_messages_list")),
		schema.ToolMessage(`{"result":{"rooms":[{"type":"unknown","name":"Unknown","room_id":"!unknown:example.com"}]}}`, "call_unknown", schema.WithToolName("dirextalk_rooms_search")),
		schema.ToolMessage(`{"result":{"room_id":"!channel:example.com","posts":[{"post_id":"post_without_channel","msg":"body"}]}}`, "call_incomplete_post", schema.WithToolName("dirextalk_channel_posts_list")),
		schema.ToolMessage(`{"result":{"room_id":"!ignored:example.com"}}`, "call_other", schema.WithToolName("runtime__shell")),
	}

	got := nativeAgentReferences(produced)
	if len(got) != 1 {
		t.Fatalf("references = %#v, want one valid room reference", got)
	}
	if got[0]["room_id"] != "!room:example.com" || got[0]["preview"] != "matched text" {
		t.Fatalf("unexpected message reference: %#v", got[0])
	}
}

func TestNativeAgentReferencesExtractOnlyPendingWorkloadProposals(t *testing.T) {
	valid := `{"result":{"operation":{"operation_id":"11111111-1111-4111-8111-111111111111","workload_id":"22222222-2222-4222-8222-222222222222","plan_id":"33333333-3333-4333-8333-333333333333","task_id":"44444444-4444-4444-8444-444444444444","confirmation_id":"55555555-5555-4555-8555-555555555555","kind":"apply","revision":2,"plan_revision":1,"plan_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","target_kind":"aws-ecs","summary":"Deploy API"},"confirmation":{"confirmation_id":"55555555-5555-4555-8555-555555555555","task_id":"44444444-4444-4444-8444-444444444444","state":"pending","revision":1,"expires_at":"2030-01-01T00:00:00Z","binding":{"operation_domain":"workload:apply","target_id":"22222222-2222-4222-8222-222222222222","target_revision":1,"content_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","parameter_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","network_digest":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","secret_grant_digest":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"}}}}`
	produced := []*schema.Message{
		schema.ToolMessage(valid, "call_apply", schema.WithToolName("native_agent_workloads_apply")),
		// Model-authored text and an unrelated tool with the same payload shape
		// must never become actionable confirmation references.
		schema.ToolMessage(valid, "call_other", schema.WithToolName("runtime__shell")),
		schema.ToolMessage(strings.Replace(valid, `"state":"pending"`, `"state":"confirmed"`, 1), "call_confirmed", schema.WithToolName("native_agent_workloads_apply")),
	}
	got := nativeAgentReferences(produced)
	if len(got) != 1 {
		t.Fatalf("references = %#v, want one pending confirmation", got)
	}
	want := map[string]any{
		"kind": "pending_confirmation", "confirmation_id": "55555555-5555-4555-8555-555555555555",
		"operation_id": "11111111-1111-4111-8111-111111111111", "workload_id": "22222222-2222-4222-8222-222222222222",
		"action": "apply", "revision": int64(1), "target_kind": "aws-ecs", "summary": "Deploy API",
	}
	for key, value := range want {
		if got[0][key] != value {
			t.Fatalf("reference %s = %#v, want %#v; full=%#v", key, got[0][key], value, got)
		}
	}
}

func TestNativeAgentReferencesRejectMalformedPendingConfirmation(t *testing.T) {
	base := `{"result":{"operation":{"operation_id":"11111111-1111-4111-8111-111111111111","workload_id":"22222222-2222-4222-8222-222222222222","plan_id":"33333333-3333-4333-8333-333333333333","task_id":"44444444-4444-4444-8444-444444444444","confirmation_id":"55555555-5555-4555-8555-555555555555","kind":"apply","revision":2,"plan_revision":1,"plan_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"confirmation":{"confirmation_id":"55555555-5555-4555-8555-555555555555","task_id":"44444444-4444-4444-8444-444444444444","state":"pending","revision":1,"expires_at":"2030-01-01T00:00:00Z","binding":{"operation_domain":"workload:apply","target_id":"22222222-2222-4222-8222-222222222222","target_revision":1,"content_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","parameter_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","network_digest":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","secret_grant_digest":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"}}}}`
	for _, malformed := range []string{
		strings.Replace(base, `"plan_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`, `"plan_digest":"bad"`, 1),
		strings.Replace(base, `"operation_id":"11111111-1111-4111-8111-111111111111"`, `"operation_id":"not-uuid"`, 1),
		strings.Replace(base, `"expires_at":"2030-01-01T00:00:00Z"`, `"expires_at":"2020-01-01T00:00:00Z"`, 1),
		strings.Replace(base, `"task_id":"44444444-4444-4444-8444-444444444444"`, `"task_id":"66666666-6666-4666-8666-666666666666"`, 1),
		strings.Replace(base, `"operation_domain":"workload:apply"`, `"operation_domain":"workload:destroy"`, 1),
		strings.Replace(base, `"target_revision":1`, `"target_revision":2`, 1),
		strings.Replace(base, `"content_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`, `"content_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"`, 1),
		strings.Replace(base, `"parameter_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"`, `"parameter_digest":"bad"`, 1),
		strings.Replace(base, `"network_digest":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"`, `"network_digest":"bad"`, 1),
		strings.Replace(base, `"secret_grant_digest":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"`, `"secret_grant_digest":"bad"`, 1),
	} {
		got := nativeAgentReferences([]*schema.Message{schema.ToolMessage(malformed, "call", schema.WithToolName("native_agent_workloads_apply"))})
		if len(got) != 0 {
			t.Fatalf("malformed pending reference accepted: %#v", got)
		}
	}
}

func TestNativeAgentReferencesExtractTypedEC2PendingConfirmation(t *testing.T) {
	result := withEC2BindingDigests(`{"result":{"provision":{"provision_id":"11111111-1111-4111-8111-111111111111","plan_id":"33333333-3333-4333-8333-333333333333","plan_revision":1,"revision":2,"credential_id":"77777777-7777-4777-8777-777777777777","credential_revision":4,"target_id":"aws-target:abc","template_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","credential_binding_digest":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"},"change":{"change_id":"22222222-2222-4222-8222-222222222222","plan_id":"33333333-3333-4333-8333-333333333333","provision_id":"11111111-1111-4111-8111-111111111111","task_id":"44444444-4444-4444-8444-444444444444","confirmation_id":"55555555-5555-4555-8555-555555555555","operation":"create","status":"waiting_user","revision":2},"confirmation":{"confirmation_id":"55555555-5555-4555-8555-555555555555","task_id":"44444444-4444-4444-8444-444444444444","state":"pending","revision":1,"expires_at":"2030-01-01T00:00:00Z","binding":{"operation_domain":"aws","target_id":"aws-target:abc","target_revision":1,"content_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","parameter_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","network_digest":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","secret_grant_digest":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","secret_grants":[{"reference_id":"77777777-7777-4777-8777-777777777777","purpose":"aws_credential","secret_revision":4,"binding_digest":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}]}}}}`)
	got := nativeAgentReferences([]*schema.Message{schema.ToolMessage(result, "call_create", schema.WithToolName("native_agent_aws_ec2_provisions_create_request"))})
	if len(got) != 1 {
		t.Fatalf("references = %#v, want one pending confirmation", got)
	}
	for key, want := range map[string]any{
		"kind": "pending_confirmation", "action": "create", "target_kind": "aws-ec2",
		"provision_id": "11111111-1111-4111-8111-111111111111", "operation_domain": "aws",
		"target_id": "aws-target:abc", "target_revision": int64(1),
	} {
		if got[0][key] != want {
			t.Fatalf("reference %s = %#v, want %#v; full=%#v", key, got[0][key], want, got)
		}
	}
	for _, malformed := range []string{
		strings.Replace(result, `"target_id":"aws-target:abc"`, `"target_id":"aws-target:other"`, 1),
		strings.Replace(result, `"plan_id":"33333333-3333-4333-8333-333333333333"`, `"plan_id":"88888888-8888-4888-8888-888888888888"`, 1),
		strings.Replace(result, `"provision_id":"11111111-1111-4111-8111-111111111111"`, `"provision_id":"99999999-9999-4999-8999-999999999999"`, 1),
		strings.Replace(result, `"parameter_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"`, `"parameter_digest":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"`, 1),
		strings.Replace(result, `"network_digest":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"`, `"network_digest":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"`, 1),
		strings.Replace(result, `"secret_grant_digest":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"`, `"secret_grant_digest":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"`, 1),
	} {
		if got := nativeAgentReferences([]*schema.Message{schema.ToolMessage(malformed, "call_create", schema.WithToolName("native_agent_aws_ec2_provisions_create_request"))}); len(got) != 0 {
			t.Fatalf("mismatched EC2 binding produced actionable reference: %#v", got)
		}
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(result), &envelope); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"duplicate":          func(map[string]any) {},
		"non-canonical-uuid": func(g map[string]any) { g["reference_id"] = "{" + g["reference_id"].(string) + "}" },
		"wrong-purpose":      func(g map[string]any) { g["purpose"] = "mcp_credential" },
		"wrong-revision":     func(g map[string]any) { g["secret_revision"] = float64(5) },
		"wrong-binding":      func(g map[string]any) { g["binding_digest"] = strings.Repeat("f", 64) },
	} {
		copyEnvelope, err := json.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
		var candidate map[string]any
		if err := json.Unmarshal(copyEnvelope, &candidate); err != nil {
			t.Fatal(err)
		}
		candidateBinding := candidate["result"].(map[string]any)["confirmation"].(map[string]any)["binding"].(map[string]any)
		candidateGrant := candidateBinding["secret_grants"].([]any)[0].(map[string]any)
		mutate(candidateGrant)
		if name == "duplicate" {
			candidateBinding["secret_grants"] = append(candidateBinding["secret_grants"].([]any), candidateGrant)
		}
		candidateRaw, _ := json.Marshal(candidate)
		if got := nativeAgentReferences([]*schema.Message{schema.ToolMessage(string(candidateRaw), "call_create", schema.WithToolName("native_agent_aws_ec2_provisions_create_request"))}); len(got) != 0 {
			t.Fatalf("%s credential grant accepted: %#v", name, got)
		}
	}
}

func TestNativeAgentReferencesExtractTypedEC2DestroyPendingConfirmation(t *testing.T) {
	provisionID := "11111111-1111-4111-8111-111111111111"
	originalPlanID := "33333333-3333-4333-8333-333333333333"
	deletePlanID := uuid.NewSHA1(uuid.Nil, []byte("ec2-destroy:"+provisionID)).String()
	credentialID := "77777777-7777-4777-8777-777777777777"
	content, parameter, network, aggregate, grantDigest := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("d", 64), strings.Repeat("e", 64)
	result := map[string]any{
		"provision": map[string]any{
			"provision_id": provisionID, "plan_id": originalPlanID, "plan_revision": 1, "revision": 2,
			"credential_id": credentialID, "credential_revision": 4, "target_id": "aws-target:abc",
			"template_sha256": content, "credential_binding_digest": grantDigest,
			"parameter_digest": parameter, "network_digest": network, "secret_grant_digest": aggregate,
		},
		"change": map[string]any{
			"change_id": "22222222-2222-4222-8222-222222222222", "plan_id": deletePlanID, "provision_id": provisionID,
			"task_id": "44444444-4444-4444-8444-444444444444", "confirmation_id": "55555555-5555-4555-8555-555555555555",
			"operation": "delete", "status": "waiting_user", "revision": 2,
		},
		"confirmation": map[string]any{
			"confirmation_id": "55555555-5555-4555-8555-555555555555", "task_id": "44444444-4444-4444-8444-444444444444",
			"state": "pending", "revision": 1, "expires_at": "2030-01-01T00:00:00Z",
			"binding": map[string]any{
				"operation_domain": "aws", "target_id": "aws-target:abc", "target_revision": 1,
				"content_digest": content, "parameter_digest": parameter, "network_digest": network, "secret_grant_digest": aggregate,
				"secret_grants": []any{map[string]any{"reference_id": credentialID, "purpose": "aws_credential", "secret_revision": 4, "binding_digest": grantDigest}},
			},
		},
	}
	encoded, err := json.Marshal(map[string]any{"result": result})
	if err != nil {
		t.Fatal(err)
	}
	tool := func(raw []byte) []*schema.Message {
		return []*schema.Message{schema.ToolMessage(string(raw), "call_destroy", schema.WithToolName("native_agent_aws_ec2_provisions_destroy_request"))}
	}
	got := nativeAgentReferences(tool(encoded))
	if len(got) != 1 || got[0]["action"] != "destroy" || got[0]["plan_id"] != deletePlanID || got[0]["provision_id"] != provisionID {
		t.Fatalf("destroy reference = %#v", got)
	}

	for name, mutate := range map[string]func(map[string]any){
		"derived-plan-drift": func(result map[string]any) {
			result["change"].(map[string]any)["plan_id"] = "88888888-8888-4888-8888-888888888888"
		},
		"provision-binding-drift": func(result map[string]any) {
			result["provision"].(map[string]any)["target_id"] = "aws-target:other"
		},
		"credential-drift": func(result map[string]any) {
			result["provision"].(map[string]any)["credential_id"] = "99999999-9999-4999-8999-999999999999"
		},
		"confirmation-binding-drift": func(result map[string]any) {
			result["confirmation"].(map[string]any)["binding"].(map[string]any)["parameter_digest"] = strings.Repeat("f", 64)
		},
	} {
		candidate, err := json.Marshal(map[string]any{"result": result})
		if err != nil {
			t.Fatal(err)
		}
		var envelope map[string]any
		if err := json.Unmarshal(candidate, &envelope); err != nil {
			t.Fatal(err)
		}
		mutate(envelope["result"].(map[string]any))
		candidate, err = json.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
		if got := nativeAgentReferences(tool(candidate)); len(got) != 0 {
			t.Fatalf("%s accepted destroy drift: %#v", name, got)
		}
	}
}

func TestNativeAgentReferencesExtractGeoLibrePendingConfirmation(t *testing.T) {
	result := `{"result":{"provision_id":"11111111-1111-4111-8111-111111111111","provision_revision":2,"expected_workload_revision":1,"plan":{"plan_id":"44444444-4444-4444-8444-444444444444","revision":1,"digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","typed_target":{"provision_id":"11111111-1111-4111-8111-111111111111","provision_revision":"2","credential_id":"77777777-7777-4777-8777-777777777777","credential_revision":4,"account_id":"123456789012","region":"us-east-1","instance_id":"i-0123456789abcdef0"},"release":{"version":"geolibre-2856ef8-v1","commit":"2856ef8c0b227ad18ecf43d4623cf00013c1740e","image_digest":"bd18a93768087e5619e75e2e8282ce347aed9179987ee8a7f471df862b72d64d","manifest_digest":"849a9977a72efe5b4e70d28517c1edf038d7ca91c0fc4f53183a3ee3b1095a86","command_digest":"f0263e3ae0f0ad857da924ade38f9bd24e6643a0ed37829b4bcaf2152d7bd582"}},"operation":{"operation_id":"22222222-2222-4222-8222-222222222222","workload_id":"33333333-3333-4333-8333-333333333333","plan_id":"44444444-4444-4444-8444-444444444444","task_id":"55555555-5555-4555-8555-555555555555","confirmation_id":"66666666-6666-4666-8666-666666666666","kind":"apply","revision":2,"expected_workload_revision":1,"plan_revision":1,"plan_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","target_kind":"aws-ec2-ssm","summary":"Install GeoLibre","desired_plan":{"secret_grants":[{"reference_id":"77777777-7777-4777-8777-777777777777","purpose":"aws_credential","secret_revision":4,"binding_digest":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}]}},"confirmation":{"confirmation_id":"66666666-6666-4666-8666-666666666666","task_id":"55555555-5555-4555-8555-555555555555","state":"pending","revision":1,"expires_at":"2030-01-01T00:00:00Z","binding":{"operation_domain":"workload:apply","target_id":"33333333-3333-4333-8333-333333333333","target_revision":1,"content_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","parameter_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","network_digest":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","secret_grant_digest":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","secret_grants":[{"reference_id":"77777777-7777-4777-8777-777777777777","purpose":"aws_credential","secret_revision":4,"binding_digest":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}]}}}}`
	aggregate := coreworkload.SecretGrantAggregateDigestForTypedRefs([]coreworkload.SecretGrantRef{{
		ReferenceID:   "77777777-7777-4777-8777-777777777777",
		Purpose:       coreconfirmation.SecretPurposeAWSCredential,
		Revision:      4,
		BindingDigest: coreconfirmation.Digest(strings.Repeat("e", 64)),
	}})
	result = strings.Replace(result,
		`"secret_grant_digest":"`+strings.Repeat("d", 64)+`"`,
		`"secret_grant_digest":"`+aggregate+`"`, 1)
	got := nativeAgentReferences([]*schema.Message{schema.ToolMessage(result, "call_install", schema.WithToolName("native_agent_aws_ec2_geolibre_install_request"))})
	if len(got) != 1 || got[0]["provision_id"] != "11111111-1111-4111-8111-111111111111" || got[0]["operation_domain"] != "workload:apply" {
		t.Fatalf("GeoLibre reference = %#v", got)
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(result), &envelope); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"aggregate-digest-drift": func(binding map[string]any) {
			binding["secret_grant_digest"] = strings.Repeat("f", 64)
		},
		"per-grant-binding-drift": func(binding map[string]any) {
			grant := binding["secret_grants"].([]any)[0].(map[string]any)
			grant["binding_digest"] = strings.Repeat("f", 64)
		},
		"duplicate-grant": func(binding map[string]any) {
			grants := binding["secret_grants"].([]any)
			binding["secret_grants"] = append(grants, grants[0])
		},
	} {
		candidateRaw, err := json.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
		var candidate map[string]any
		if err := json.Unmarshal(candidateRaw, &candidate); err != nil {
			t.Fatal(err)
		}
		binding := candidate["result"].(map[string]any)["confirmation"].(map[string]any)["binding"].(map[string]any)
		mutate(binding)
		candidateRaw, _ = json.Marshal(candidate)
		if got := nativeAgentReferences([]*schema.Message{schema.ToolMessage(string(candidateRaw), "call_install", schema.WithToolName("native_agent_aws_ec2_geolibre_install_request"))}); len(got) != 0 {
			t.Fatalf("%s accepted: %#v", name, got)
		}
	}
}

func TestNativeAgentReferencesDoNotPersistModelToolSecrets(t *testing.T) {
	valid := withEC2BindingDigests(`{"result":{"provision":{"provision_id":"11111111-1111-4111-8111-111111111111","plan_id":"33333333-3333-4333-8333-333333333333","plan_revision":1,"revision":2,"credential_id":"77777777-7777-4777-8777-777777777777","credential_revision":4,"target_id":"aws-target:abc","template_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","credential_binding_digest":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"},"change":{"change_id":"22222222-2222-4222-8222-222222222222","plan_id":"33333333-3333-4333-8333-333333333333","provision_id":"11111111-1111-4111-8111-111111111111","task_id":"44444444-4444-4444-8444-444444444444","confirmation_id":"55555555-5555-4555-8555-555555555555","operation":"create","status":"waiting_user","revision":2},"confirmation":{"confirmation_id":"55555555-5555-4555-8555-555555555555","task_id":"44444444-4444-4444-8444-444444444444","state":"pending","revision":1,"expires_at":"2030-01-01T00:00:00Z","binding":{"operation_domain":"aws","target_id":"aws-target:abc","target_revision":1,"content_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","parameter_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","network_digest":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","secret_grant_digest":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","secret_grants":[{"reference_id":"77777777-7777-4777-8777-777777777777","purpose":"aws_credential","secret_revision":4,"binding_digest":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}]}}},"owner_id":"owner-secret","selected_command":"rm -rf","desired_plan":{"labels":{"private":"secret"}}}`)
	references := nativeAgentReferences([]*schema.Message{schema.ToolMessage(valid, "call_create", schema.WithToolName("native_agent_aws_ec2_provisions_create_request"))})
	if len(references) != 1 {
		t.Fatalf("references = %#v", references)
	}
	serialized := fmt.Sprint(references[0])
	for _, forbidden := range []string{"owner-secret", "selected_command", "rm -rf", "private"} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("persisted native reference retained %q: %s", forbidden, serialized)
		}
	}
}

func TestNativeAgentReferencesRejectGeoLibreReleaseDrift(t *testing.T) {
	result := `{"result":{"provision_id":"11111111-1111-4111-8111-111111111111","plan":{"release":{"version":"geolibre-2856ef8-v1","commit":"bad","image_digest":"bd18a93768087e5619e75e2e8282ce347aed9179987ee8a7f471df862b72d64d","manifest_digest":"849a9977a72efe5b4e70d28517c1edf038d7ca91c0fc4f53183a3ee3b1095a86","command_digest":"f0263e3ae0f0ad857da924ade38f9bd24e6643a0ed37829b4bcaf2152d7bd582"}},"operation":{"operation_id":"22222222-2222-4222-8222-222222222222","workload_id":"33333333-3333-4333-8333-333333333333","plan_id":"44444444-4444-4444-8444-444444444444","task_id":"55555555-5555-4555-8555-555555555555","confirmation_id":"66666666-6666-4666-8666-666666666666","kind":"apply","revision":2,"plan_revision":1,"plan_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"confirmation":{"confirmation_id":"66666666-6666-4666-8666-666666666666","task_id":"55555555-5555-4555-8555-555555555555","state":"pending","revision":1,"expires_at":"2030-01-01T00:00:00Z","binding":{"operation_domain":"workload:apply","target_id":"33333333-3333-4333-8333-333333333333","target_revision":1,"content_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","parameter_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","network_digest":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","secret_grant_digest":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"}}}}`
	for _, malformed := range []string{
		result,
		strings.Replace(result, `"plan_id":"44444444-4444-4444-8444-444444444444"`, `"plan_id":"88888888-8888-4888-8888-888888888888"`, 1),
		strings.Replace(result, `"dirextalk:provision-revision":"2"`, `"dirextalk:provision-revision":"3"`, 1),
	} {
		if got := nativeAgentReferences([]*schema.Message{schema.ToolMessage(malformed, "call_install", schema.WithToolName("native_agent_aws_ec2_geolibre_install_request"))}); len(got) != 0 {
			t.Fatalf("GeoLibre drift produced actionable reference: %#v", got)
		}
	}
}
