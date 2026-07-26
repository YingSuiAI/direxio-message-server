package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestCanonicalDigestAndSecretRedaction(t *testing.T) {
	params := map[string]any{"message": "hello", "count": int64(2), "nested": map[string]any{"z": true, "a": "x"}}
	if got := canonicalDigest(params); got != "ece5d75a9ae2933698910252626e4046bf3f2f4a042865e3d69c0cee34184dd0" {
		t.Fatalf("digest = %s", got)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"do-not-forward-secret","code":"safe"}`))
	}))
	defer server.Close()
	d, err := NewDriver(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.action(context.Background(), "test", nil); err == nil || strings.Contains(err.Error(), "do-not-forward-secret") {
		t.Fatalf("secret-bearing error escaped: %v", err)
	}
}

func TestSummaryContainsOnlyAnswerDigestAndLength(t *testing.T) {
	value, err := json.Marshal(Summary{Flow: "deepseek", CoreReady: true, AnswerHash: "abc", AnswerLen: 11})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(value), "real answer") || strings.Contains(string(value), "answer\"") {
		t.Fatalf("summary contains raw model output: %s", value)
	}
	if !strings.Contains(string(value), "answer_sha256") || !strings.Contains(string(value), "answer_length") {
		t.Fatalf("summary omitted sanitized answer markers: %s", value)
	}
}

func TestConfirmationOrderingAndTaskEvents(t *testing.T) {
	confirmed := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		action, _ := request["action"].(string)
		w.Header().Set("Content-Type", "application/json")
		switch action {
		case "agent.core.confirmations.get":
			if confirmed {
				_, _ = w.Write([]byte(`{"confirmation":{"confirmation_id":"c","task_id":"t","state":"confirmed","revision":4}}`))
			} else {
				_, _ = w.Write([]byte(`{"confirmation":{"confirmation_id":"c","task_id":"t","state":"pending","revision":3}}`))
			}
		case "agent.core.confirmations.confirm":
			confirmed = true
			_, _ = w.Write([]byte(`{"confirmation":{"confirmation_id":"c","task_id":"t","state":"confirmed","revision":4}}`))
		case "agent.core.tasks.get":
			if !confirmed {
				t.Fatalf("task was read before confirmation")
			}
			_, _ = w.Write([]byte(`{"task":{"task_id":"t","status":"succeeded","revision":2}}`))
		case "agent.core.tasks.events":
			_, _ = w.Write([]byte(`{"events":[{"task_id":"t","sequence":1}]}`))
		default:
			t.Fatalf("unexpected action %s", action)
		}
	}))
	defer server.Close()
	d, err := NewDriver(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.confirmTask(context.Background(), "c", "t"); err != nil {
		t.Fatal(err)
	}
	if !confirmed {
		t.Fatal("confirmation was not issued")
	}
}

func TestStreamChecksDigestAndRealAnswer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_p2p/ws" {
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close(websocket.StatusNormalClosure, "")
			var hello map[string]any
			if wsjson.Read(r.Context(), conn, &hello) != nil {
				return
			}
			_ = wsjson.Write(r.Context(), conn, map[string]any{"type": "server.ready"})
			var frame map[string]any
			if wsjson.Read(r.Context(), conn, &frame) != nil {
				return
			}
			params, _ := frame["params"].(map[string]any)
			if frame["request_digest"] != canonicalDigest(params) {
				t.Errorf("request digest mismatch")
			}
			turnID, _ := frame["turn_id"].(string)
			params, _ = frame["params"].(map[string]any)
			conversationID, _ := params["conversation_id"].(string)
			_ = wsjson.Write(r.Context(), conn, map[string]any{"type": "server.agent_core_stream.accepted", "turn_id": turnID, "core_turn_id": "core-turn", "conversation_id": conversationID})
			_ = wsjson.Write(r.Context(), conn, map[string]any{"type": "server.agent_core_stream.event", "turn_id": turnID, "seq": int64(1), "event": "delta", "data": map[string]any{"text": "real answer"}})
			_ = wsjson.Write(r.Context(), conn, map[string]any{"type": "server.agent_core_stream.event", "turn_id": turnID, "seq": int64(2), "event": "done", "data": map[string]any{}})
			return
		}
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ticket":"test-ticket"}`))
	}))
	defer server.Close()
	d, err := NewDriver(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	answer, err := d.stream(context.Background(), "hello", "profile")
	if err != nil || answer != "real answer" {
		t.Fatalf("stream answer=%q err=%v", answer, err)
	}
}

func TestWorkloadPlanRejectsSecretValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, []byte(`{"summary":"x","secret_value":"must-not-be-used"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readWorkloadPlan(path); err == nil {
		t.Fatal("secret-bearing workload plan was accepted")
	}
}

func TestDeepSeekContractFixture(t *testing.T) {
	var conversationID string
	var clientProfileID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_p2p/ws" {
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close(websocket.StatusNormalClosure, "")
			var hello map[string]any
			if wsjson.Read(r.Context(), conn, &hello) != nil {
				return
			}
			_ = wsjson.Write(r.Context(), conn, map[string]any{"type": "server.ready"})
			var frame map[string]any
			if wsjson.Read(r.Context(), conn, &frame) != nil {
				return
			}
			params, _ := frame["params"].(map[string]any)
			conversationID, _ = params["conversation_id"].(string)
			turn, _ := frame["turn_id"].(string)
			_ = wsjson.Write(r.Context(), conn, map[string]any{"type": "server.agent_core_stream.accepted", "turn_id": turn, "core_turn_id": "core-turn", "conversation_id": conversationID})
			_ = wsjson.Write(r.Context(), conn, map[string]any{"type": "server.agent_core_stream.event", "turn_id": turn, "seq": 1, "event": "delta", "data": map[string]any{"text": "deepseek fixture"}})
			_ = wsjson.Write(r.Context(), conn, map[string]any{"type": "server.agent_core_stream.event", "turn_id": turn, "seq": 2, "event": "done", "data": map[string]any{}})
			return
		}
		var req map[string]any
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			return
		}
		action, _ := req["action"].(string)
		w.Header().Set("Content-Type", "application/json")
		switch action {
		case "agent.backends.get":
			_, _ = w.Write([]byte(`{"core":{"configured":true,"status":"ready","api_version":"v1","instance_id":"instance","capabilities":["agent.info","model.profile","conversation"]}}`))
		case "agent.core.model_profiles.sync":
			params := req["params"].(map[string]any)
			entries := params["entries"].([]any)
			entry := entries[0].(map[string]any)
			id := entry["client_profile_id"].(string)
			clientProfileID = id
			_, _ = w.Write([]byte(`{"default_client_profile_id":"` + id + `","profiles":[{"profile_id":"profile-1","client_profile_id":"` + id + `","provider":"openai_compatible","base_url":"https://api.deepseek.com","model":"deepseek-chat","api_key_configured":true,"revision":1}]}`))
		case "agent.core.model_profiles.get":
			_, _ = w.Write([]byte(`{"profile":{"profile_id":"profile-1","client_profile_id":"` + clientProfileID + `","provider":"openai_compatible","base_url":"https://api.deepseek.com","model":"deepseek-chat","api_key_configured":true,"revision":1}}`))
		case "realtime.ws_ticket.create":
			_, _ = w.Write([]byte(`{"ticket":"fixture-ticket"}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":"unexpected"}`))
		}
	}))
	defer server.Close()
	d, err := NewDriver(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	answer, err := d.deepseek(context.Background(), "fixture-key")
	if err != nil || answer != "deepseek fixture" {
		t.Fatalf("answer=%q err=%v", answer, err)
	}
}

func TestDualExtensionContractFixture(t *testing.T) {
	confirmed := map[string]bool{"mcp-install": false, "skill-install": false}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			return
		}
		action, _ := req["action"].(string)
		w.Header().Set("Content-Type", "application/json")
		kind := "mcp"
		if strings.Contains(action, "skills") {
			kind = "skill"
		}
		if action == "agent.core.confirmations.get" || action == "agent.core.confirmations.confirm" {
			if params, ok := req["params"].(map[string]any); ok && strings.Contains(fmt.Sprint(params["confirmation_id"]), "skill") {
				kind = "skill"
			}
		}
		id := kind + "-install"
		cid := kind + "-confirmation"
		tid := kind + "-task"
		candidate := map[string]any{"id": kind + "-candidate", "kind": kind, "source": "github", "name": kind, "description": "fixture", "transport": "streamable-http", "pin": map[string]any{"registry_version": "v1", "registry_sha256": strings.Repeat("a", 64), "git_commit": strings.Repeat("b", 40), "git_sha256": strings.Repeat("c", 64)}}
		inspection := map[string]any{"candidate": candidate, "content_digest": strings.Repeat("d", 64), "manifest_digest": strings.Repeat("e", 64), "execution_digest": strings.Repeat("f", 64), "network_schema_digest": strings.Repeat("1", 64), "secret_schema_digest": strings.Repeat("2", 64), "execution": map[string]any{}, "network_grants": []any{}, "secret_grants": []any{}}
		installation := func() map[string]any {
			state := "installing"
			active := ""
			proposed := "version-1"
			if confirmed[id] {
				state, active, proposed = "installed", "version-1", ""
			}
			return map[string]any{"installation_id": id, "kind": kind, "candidate_id": candidate["id"], "revision": 1, "state": state, "active_version_id": active, "proposed_version_id": proposed, "versions": []any{map[string]any{"version_id": "version-1", "content_digest": inspection["content_digest"], "manifest_digest": inspection["manifest_digest"], "execution_digest": inspection["execution_digest"], "network_schema_digest": inspection["network_schema_digest"], "secret_schema_digest": inspection["secret_schema_digest"]}}}
		}
		switch action {
		case "agent.backends.get":
			_, _ = w.Write([]byte(`{"core":{"configured":true,"status":"ready","api_version":"v1","instance_id":"i","capabilities":["agent.info","mcp","skill","task","confirmation"]}}`))
		case "agent.core.mcp.discover", "agent.core.skills.discover":
			_, _ = w.Write(mustJSONValue(map[string]any{"candidates": []any{candidate}}))
		case "agent.core.mcp.inspect", "agent.core.skills.inspect":
			_, _ = w.Write(mustJSONValue(map[string]any{"inspection": inspection}))
		case "agent.core.mcp.install", "agent.core.skills.install":
			_, _ = w.Write(mustJSONValue(map[string]any{"installation": installation(), "confirmation_id": cid, "task_id": tid}))
		case "agent.core.mcp.get", "agent.core.skills.get":
			_, _ = w.Write(mustJSONValue(map[string]any{"installation": installation()}))
		case "agent.core.mcp.list_tools":
			tools := []any{}
			if confirmed[id] {
				tools = []any{map[string]any{"name": "ping"}}
			}
			_, _ = w.Write(mustJSONValue(map[string]any{"tools": tools}))
		case "agent.core.mcp.execute", "agent.core.skills.execute":
			_, _ = w.Write(mustJSONValue(map[string]any{"task_id": tid + "-execute"}))
		case "agent.core.confirmations.get":
			state := "pending"
			rev := int64(1)
			if confirmed[id] {
				state, rev = "confirmed", 2
			}
			_, _ = w.Write(mustJSONValue(map[string]any{"confirmation": map[string]any{"confirmation_id": cid, "task_id": tid, "state": state, "revision": rev, "binding": map[string]any{"operation_domain": "extension", "target_id": id, "target_revision": 1, "content_digest": inspection["content_digest"], "parameter_digest": strings.Repeat("3", 64), "network_digest": strings.Repeat("4", 64), "secret_grant_digest": strings.Repeat("5", 64), "network_grants": []any{}, "secret_grants": []any{}}}}))
		case "agent.core.confirmations.confirm":
			confirmed[id] = true
			_, _ = w.Write(mustJSONValue(map[string]any{"confirmation": map[string]any{"confirmation_id": cid, "task_id": tid, "state": "confirmed", "revision": 2}}))
		case "agent.core.tasks.get":
			taskID, _ := req["params"].(map[string]any)["task_id"].(string)
			_, _ = w.Write([]byte(`{"task":{"task_id":"` + taskID + `","status":"succeeded"}}`))
		case "agent.core.tasks.events":
			taskID, _ := req["params"].(map[string]any)["task_id"].(string)
			_, _ = w.Write([]byte(`{"events":[{"task_id":"` + taskID + `","sequence":1}]}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":"unexpected"}`))
		}
	}))
	defer server.Close()
	d, err := NewDriver(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := d.extensions(context.Background(), ""); err != nil || got != 2 {
		t.Fatalf("extensions=%d err=%v", got, err)
	}
}

func TestBindingDescriptorFixtureRejectsDrift(t *testing.T) {
	digest := strings.Repeat("a", 64)
	inspection := map[string]any{
		"network_grants": []any{map[string]any{"scheme": "https", "host": "example.test", "port": float64(443), "path_prefix": "/mcp", "digest": digest}},
		"secret_grants":  []any{map[string]any{"reference_id": "ref", "purpose": "mcp_credential", "binding_digest": digest}},
	}
	if err := validateBindingDescriptors(map[string]any{"network_grants": []any{"https://example.test:443/mcp:" + digest}, "secret_grants": []any{map[string]any{"reference_id": "ref", "purpose": "mcp_credential", "binding_digest": digest}}}, inspection); err != nil {
		t.Fatalf("valid descriptors rejected: %v", err)
	}
	if err := validateBindingDescriptors(map[string]any{"network_grants": []any{"https://drift.example.test:443/mcp:" + digest}, "secret_grants": []any{}}, inspection); err == nil {
		t.Fatal("descriptor drift was accepted")
	}
}

func mustJSONValue(value any) []byte { encoded, _ := json.Marshal(value); return encoded }

func TestWorkloadApplyDestroyContractFixture(t *testing.T) {
	applyConfirmed, destroyConfirmed := false, false
	workloadID := "11111111-1111-4111-8111-111111111111"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			return
		}
		action, _ := req["action"].(string)
		params, _ := req["params"].(map[string]any)
		w.Header().Set("Content-Type", "application/json")
		plan := map[string]any{"plan_id": "22222222-2222-4222-8222-222222222222", "revision": 1, "digest": strings.Repeat("a", 64), "target_kind": "core-runner"}
		op := func(destroy bool) map[string]any {
			status := "pending"
			actual := any(nil)
			if destroy && destroyConfirmed {
				status, actual = "succeeded", map[string]any{"workload_id": workloadID, "state": "destroyed", "applied_plan_id": plan["plan_id"], "applied_plan_digest": plan["digest"], "readback_digest": strings.Repeat("b", 64), "provider_version": "fixture", "identity": map[string]any{"kind": "core-runner"}}
			}
			if !destroy && applyConfirmed {
				status, actual = "succeeded", map[string]any{"workload_id": workloadID, "state": "ready", "applied_plan_id": plan["plan_id"], "applied_plan_digest": plan["digest"], "readback_digest": strings.Repeat("b", 64), "provider_version": "fixture", "identity": map[string]any{"kind": "core-runner"}}
			}
			revision := int64(1)
			if status == "succeeded" {
				revision = 2
			}
			task, conf := "33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444"
			if destroy {
				task, conf = "55555555-5555-4555-8555-555555555555", "66666666-6666-4666-8666-666666666666"
			}
			opID := "77777777-7777-4777-8777-777777777777"
			if destroy {
				opID = "88888888-8888-4888-8888-888888888888"
			}
			kind := "apply"
			if destroy {
				kind = "destroy"
			}
			return map[string]any{"operation_id": opID, "workload_id": workloadID, "plan_id": plan["plan_id"], "kind": kind, "plan_revision": 1, "plan_digest": plan["digest"], "target_kind": "core-runner", "task_id": task, "confirmation_id": conf, "status": status, "revision": revision, "actual": actual}
		}
		confirmation := func(destroy bool) map[string]any {
			state, rev := "pending", int64(1)
			if (destroy && destroyConfirmed) || (!destroy && applyConfirmed) {
				state, rev = "confirmed", 2
			}
			task, conf, domain := "33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444", "workload:apply"
			if destroy {
				task, conf, domain = "55555555-5555-4555-8555-555555555555", "66666666-6666-4666-8666-666666666666", "workload:destroy"
			}
			return map[string]any{"confirmation_id": conf, "task_id": task, "state": state, "revision": rev, "binding": map[string]any{"operation_domain": domain, "target_id": workloadID, "target_revision": 1, "content_digest": plan["digest"], "parameter_digest": strings.Repeat("c", 64), "network_digest": strings.Repeat("d", 64), "secret_grant_digest": strings.Repeat("e", 64)}}
		}
		switch action {
		case "agent.backends.get":
			_, _ = w.Write([]byte(`{"core":{"configured":true,"status":"ready","api_version":"v1","instance_id":"i","capabilities":["agent.info","task","confirmation","workload.core_runner"]}}`))
		case "agent.core.workloads.plan", "agent.core.workloads.get":
			_, _ = w.Write(mustJSONValue(map[string]any{"plan": plan}))
		case "agent.core.workloads.quote":
			_, _ = w.Write(mustJSONValue(map[string]any{"quote": map[string]any{"plan_id": plan["plan_id"], "plan_digest": plan["digest"]}}))
		case "agent.core.workloads.apply":
			_, _ = w.Write(mustJSONValue(map[string]any{"operation": op(false), "confirmation": confirmation(false), "task_id": "33333333-3333-4333-8333-333333333333"}))
		case "agent.core.workloads.destroy":
			_, _ = w.Write(mustJSONValue(map[string]any{"operation": op(true), "confirmation": confirmation(true), "task_id": "55555555-5555-4555-8555-555555555555"}))
		case "agent.core.workloads.operations.get":
			operationID, _ := params["operation_id"].(string)
			value := op(operationID == "88888888-8888-4888-8888-888888888888")
			_, _ = w.Write(mustJSONValue(map[string]any{"operation": value}))
		case "agent.core.workloads.operations.events":
			operationID, _ := params["operation_id"].(string)
			_, _ = w.Write(mustJSONValue(map[string]any{"events": []any{map[string]any{"operation_id": operationID, "sequence": 1, "kind": "completed", "status": "succeeded", "message": "fixture"}}}))
		case "agent.core.workloads.actual.get":
			state := "ready"
			if destroyConfirmed {
				state = "destroyed"
			}
			_, _ = w.Write(mustJSONValue(map[string]any{"workload": map[string]any{"workload_id": workloadID, "revision": 1, "state": state, "identity": map[string]any{"kind": "core-runner"}, "applied_plan_id": "22222222-2222-4222-8222-222222222222", "applied_plan_digest": strings.Repeat("a", 64), "readback_digest": strings.Repeat("b", 64), "provider_version": "fixture"}}))
		case "agent.core.confirmations.get":
			_, _ = w.Write(mustJSONValue(map[string]any{"confirmation": func() map[string]any {
				return confirmation(strings.Contains(fmt.Sprint(params["confirmation_id"]), "6666"))
			}()}))
		case "agent.core.confirmations.confirm":
			if strings.Contains(fmt.Sprint(params["confirmation_id"]), "6666") {
				destroyConfirmed = true
			} else {
				applyConfirmed = true
			}
			taskID := "33333333-3333-4333-8333-333333333333"
			if strings.Contains(fmt.Sprint(params["confirmation_id"]), "6666") {
				taskID = "55555555-5555-4555-8555-555555555555"
			}
			_, _ = w.Write(mustJSONValue(map[string]any{"confirmation": map[string]any{"confirmation_id": params["confirmation_id"], "task_id": taskID, "state": "confirmed", "revision": 2}}))
		case "agent.core.tasks.get":
			taskID, _ := params["task_id"].(string)
			_, _ = w.Write(mustJSONValue(map[string]any{"task": map[string]any{"task_id": taskID, "status": "succeeded"}}))
		case "agent.core.tasks.events":
			taskID, _ := params["task_id"].(string)
			_, _ = w.Write(mustJSONValue(map[string]any{"events": []any{map[string]any{"task_id": taskID, "sequence": 1}}}))
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":"unexpected"}`))
		}
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "workload.json")
	if err := os.WriteFile(path, []byte(`{"summary":"fixture"}`), 0600); err != nil {
		t.Fatal(err)
	}
	d, err := NewDriver(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.workload(context.Background(), path); err != nil {
		t.Fatal(err)
	}
}
