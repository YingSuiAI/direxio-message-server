package schedules

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/nativeagent"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
	"github.com/google/uuid"
)

const confirmationTTL = 10 * time.Minute

var gatedScheduleActions = map[string]bool{
	"agent.schedules.create": true, "agent.schedules.update": true,
	"agent.schedules.enable": true, "agent.schedules.delete": true,
	"agent.schedules.run_now": true, "agent.schedules.disable": true,
}

// Tools exposes the bounded schedule surface to interactive Native Agent
// turns. Mutating actions are represented as proposals and require a later
// owner-authored confirmation turn.
func (m *Module) Tools() []nativeagent.Tool {
	if m == nil {
		return nil
	}
	obj := func(keys ...string) map[string]any {
		p := map[string]any{}
		for _, k := range keys {
			p[k] = map[string]any{"type": "string"}
		}
		return map[string]any{"type": "object", "properties": p}
	}
	tool := func(name, desc, action string, schema map[string]any, write bool) nativeagent.Tool {
		return nativeagent.Tool{Name: name, Description: desc, Parameters: schema, Write: write, Handler: func(ctx context.Context, p map[string]any) (any, error) { return m.invokeChatTool(ctx, action, p) }}
	}
	triggerSchema := map[string]any{"type": "object", "properties": map[string]any{"kind": map[string]any{"type": "string"}, "value": map[string]any{"type": "string"}, "timezone": map[string]any{"type": "string"}}}
	createSchema := obj("name", "prompt", "model_profile_id", "skip_if_running")
	createSchema["properties"].(map[string]any)["trigger"] = triggerSchema
	return []nativeagent.Tool{
		tool("native_agent_schedules_list", "List embedded schedules.", "agent.schedules.list", obj("limit", "cursor"), false),
		tool("native_agent_schedules_get", "Get an embedded schedule.", "agent.schedules.get", obj("schedule_id"), false),
		tool("native_agent_schedule_runs_list", "List runs for an embedded schedule.", "agent.schedule_runs.list", obj("schedule_id", "limit", "cursor"), false),
		tool("native_agent_schedule_runs_get", "Get an embedded schedule run.", "agent.schedule_runs.get", obj("schedule_id", "run_id"), false),
		tool("native_agent_schedules_disable", "Disable an embedded schedule immediately.", "agent.schedules.disable", obj("schedule_id", "expected_revision", "idempotency_key"), true),
		tool("native_agent_schedules_create", "Propose creating an embedded schedule; wait for owner confirmation.", "agent.schedules.create", createSchema, true),
		tool("native_agent_schedules_update", "Propose updating an embedded schedule; wait for owner confirmation.", "agent.schedules.update", obj("schedule_id", "name", "prompt", "model_profile_id", "trigger", "skip_if_running", "expected_revision"), true),
		tool("native_agent_schedules_enable", "Propose enabling an embedded schedule; wait for owner confirmation.", "agent.schedules.enable", obj("schedule_id", "expected_revision"), true),
		tool("native_agent_schedules_delete", "Propose deleting an embedded schedule; wait for owner confirmation.", "agent.schedules.delete", obj("schedule_id", "expected_revision"), true),
		tool("native_agent_schedules_run_now", "Propose running an embedded schedule now; wait for owner confirmation.", "agent.schedules.run_now", obj("schedule_id"), true),
		tool("native_agent_schedules_confirm", "Execute a pending schedule proposal only after the owner sends the exact approval phrase in a new turn.", "agent.schedules.confirm", obj("confirmation_id"), true),
	}
}

func (m *Module) invokeChatTool(ctx context.Context, action string, params map[string]any) (any, error) {
	if action == "agent.schedules.confirm" {
		return m.confirm(ctx, params)
	}
	if !gatedScheduleActions[action] {
		return m.InvokeTool(ctx, action, params)
	}
	clean, err := boundedScheduleParams(action, params)
	if err != nil {
		return nil, err
	}
	if action == "agent.schedules.disable" {
		return m.InvokeTool(ctx, action, ensureIdempotency(m.owner(), action, clean))
	}
	owner, conversation, _ := nativeagent.RequestContext(ctx)
	if owner == "" || conversation == "" || m.confirmations == nil {
		return nil, fmt.Errorf("schedule confirmations are unavailable")
	}
	key := ensureIdempotency(owner, action, clean)["idempotency_key"].(string)
	digest := mutationDigest(action, clean)
	codeBytes := make([]byte, 4)
	if _, err = rand.Read(codeBytes); err != nil {
		return nil, err
	}
	code := strings.ToUpper(hex.EncodeToString(codeBytes))
	id := uuid.NewString()
	summary := confirmationSummary(action, clean)
	proposal := storageConfirmation(id, owner, conversation, action, clean, digest, key, summary, code)
	stored, _, err := m.confirmations.ReserveScheduleConfirmation(ctx, proposal)
	if err != nil {
		return nil, err
	}
	return map[string]any{"confirmation_required": true, "confirmation_id": stored.ConfirmationID, "action": stored.Action, "summary": stored.Summary, "expires_at": stored.ExpiresAt.UTC().Format(time.RFC3339), "approval_phrase": "确认执行 " + stored.ApprovalCode, "status": stored.Status, "revision": stored.Revision}, nil
}

// storageConfirmation is kept local to avoid exposing credential-bearing
// schedule implementation details through the tool contract.
func storageConfirmation(id, owner, conversation, action string, p map[string]any, digest [32]byte, key, summary, code string) storage.ScheduleConfirmation {
	b, _ := json.Marshal(p)
	return storage.ScheduleConfirmation{ConfirmationID: id, OwnerID: owner, ConversationID: conversation, Action: action, ParamsJSON: b, RequestDigest: digest, IdempotencyKey: key, Summary: summary, ApprovalCode: code, Status: "pending", Revision: 1, ExpiresAt: time.Now().UTC().Add(confirmationTTL)}
}

func (m *Module) confirm(ctx context.Context, params map[string]any) (any, error) {
	owner, conversation, userText := nativeagent.RequestContext(ctx)
	id := strings.TrimSpace(fmt.Sprint(params["confirmation_id"]))
	if owner == "" || conversation == "" || id == "" || m.confirmations == nil {
		return map[string]any{"confirmation_required": true}, nil
	}
	v, ok, err := m.confirmations.GetScheduleConfirmation(ctx, owner, conversation, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return map[string]any{"confirmation_required": true, "confirmation_id": id}, nil
	}
	if v.Status == "completed" || v.Status == "failed" {
		return terminalConfirmation(v), nil
	}
	if normalizeApproval(userText) != normalizeApproval("确认执行 "+v.ApprovalCode) {
		return map[string]any{"confirmation_required": true, "confirmation_id": id, "summary": v.Summary, "expires_at": v.ExpiresAt.UTC().Format(time.RFC3339)}, nil
	}
	claimed, err := m.confirmations.ClaimScheduleConfirmation(ctx, owner, conversation, id, v.Revision, time.Now().UTC())
	if claimed.Status == "completed" || claimed.Status == "failed" {
		return terminalConfirmation(claimed), nil
	}
	if err != nil {
		if claimed.Status == "completed" || claimed.Status == "failed" {
			return terminalConfirmation(claimed), nil
		}
		if current, ok, ge := m.confirmations.GetScheduleConfirmation(ctx, owner, conversation, id); ge == nil && ok && current.Status == "executing" {
			// A prior worker may have crashed after claiming. Give it a short
			// chance to persist its terminal result, then safely re-enter the
			// idempotent Stage A action using the stored key.
			for i := 0; i < 25; i++ {
				time.Sleep(20 * time.Millisecond)
				current, _, _ = m.confirmations.GetScheduleConfirmation(ctx, owner, conversation, id)
				if current.Status == "completed" || current.Status == "failed" {
					return terminalConfirmation(current), nil
				}
			}
			claimed = current
		} else {
			return map[string]any{"confirmation_required": true, "confirmation_id": id}, nil
		}
	}
	var p map[string]any
	if json.Unmarshal(claimed.ParamsJSON, &p) != nil {
		return nil, fmt.Errorf("invalid confirmation parameters")
	}
	p["idempotency_key"] = claimed.IdempotencyKey
	out, invokeErr := m.InvokeTool(ctx, claimed.Action, p)
	result := map[string]any{"confirmation_id": id, "action": claimed.Action, "status": "completed", "result": out}
	status, errText := "completed", ""
	if invokeErr != nil {
		status, errText = "failed", invokeErr.Error()
		result = map[string]any{"confirmation_id": id, "action": claimed.Action, "status": status, "error": errText}
	}
	b, _ := json.Marshal(sanitizeConfirmationValue(result))
	if completeErr := m.confirmations.CompleteScheduleConfirmation(ctx, owner, conversation, id, claimed.Revision, status, b, errText); completeErr != nil {
		if terminal, ok, readErr := m.confirmations.GetScheduleConfirmation(ctx, owner, conversation, id); readErr == nil && ok && (terminal.Status == "completed" || terminal.Status == "failed") {
			return terminalConfirmation(terminal), nil
		}
		return nil, completeErr
	}
	return result, nil
}
func terminalConfirmation(v storage.ScheduleConfirmation) map[string]any {
	var result any
	_ = json.Unmarshal(v.ResultJSON, &result)
	out := map[string]any{"confirmation_id": v.ConfirmationID, "action": v.Action, "status": v.Status}
	if result != nil {
		out["result"] = result
	}
	if v.Error != "" {
		out["error"] = v.Error
	}
	return out
}
func normalizeApproval(s string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}
func ensureIdempotency(owner, action string, p map[string]any) map[string]any {
	d := mutationDigest(action, p)
	p["idempotency_key"] = uuid.NewSHA1(uuid.Nil, []byte(owner+"\x00"+action+"\x00"+hex.EncodeToString(d[:]))).String()
	return p
}
func boundedScheduleParams(action string, p map[string]any) (map[string]any, error) {
	sets := map[string]map[string]bool{
		"agent.schedules.create":  {"name": true, "prompt": true, "model_profile_id": true, "trigger": true, "skip_if_running": true, "idempotency_key": true},
		"agent.schedules.update":  {"schedule_id": true, "name": true, "prompt": true, "model_profile_id": true, "trigger": true, "skip_if_running": true, "expected_revision": true, "idempotency_key": true},
		"agent.schedules.enable":  {"schedule_id": true, "expected_revision": true, "idempotency_key": true},
		"agent.schedules.disable": {"schedule_id": true, "expected_revision": true, "idempotency_key": true},
		"agent.schedules.delete":  {"schedule_id": true, "expected_revision": true, "idempotency_key": true},
		"agent.schedules.run_now": {"schedule_id": true, "idempotency_key": true},
	}
	allowed := sets[action]
	if allowed == nil {
		return nil, fmt.Errorf("unknown schedule action")
	}
	out := map[string]any{}
	for k, v := range p {
		if !allowed[k] {
			return nil, fmt.Errorf("unknown field: %s", k)
		}
		switch x := v.(type) {
		case string:
			if k == "skip_if_running" || k == "expected_revision" {
				return nil, fmt.Errorf("field %s has invalid type", k)
			}
			if len([]rune(x)) > 2000 {
				return nil, fmt.Errorf("field %s is too long", k)
			}
			out[k] = strings.TrimSpace(x)
		case bool:
			if k != "skip_if_running" {
				return nil, fmt.Errorf("field %s has invalid type", k)
			}
			out[k] = x
		case map[string]any:
			if k != "trigger" {
				return nil, fmt.Errorf("field %s is invalid", k)
			}
			trigger := map[string]any{}
			for tk, tv := range x {
				if tk != "kind" && tk != "value" && tk != "timezone" {
					return nil, fmt.Errorf("trigger field %s is invalid", tk)
				}
				ts, ok := tv.(string)
				if !ok || len([]rune(ts)) > 200 {
					return nil, fmt.Errorf("trigger field %s is invalid", tk)
				}
				trigger[tk] = strings.TrimSpace(ts)
			}
			out[k] = trigger
		case float64:
			if k != "expected_revision" {
				return nil, fmt.Errorf("field %s has invalid type", k)
			}
			if k == "expected_revision" && (x <= 0 || x != float64(int64(x))) {
				return nil, fmt.Errorf("expected_revision must be positive integer")
			}
			out[k] = int64(x)
		case int, int64:
			if k != "expected_revision" {
				return nil, fmt.Errorf("field %s has invalid type", k)
			}
			out[k] = v
		default:
			return nil, fmt.Errorf("field %s is invalid", k)
		}
	}
	if action != "agent.schedules.create" {
		if id, ok := out["schedule_id"].(string); !ok || strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("schedule_id is required")
		}
	}
	if action != "agent.schedules.create" && (action == "agent.schedules.update" || action == "agent.schedules.enable" || action == "agent.schedules.delete" || action == "agent.schedules.disable") {
		if _, ok := out["expected_revision"]; !ok {
			return nil, fmt.Errorf("expected_revision is required")
		}
		if n, ok := out["expected_revision"].(int64); ok && n <= 0 {
			return nil, fmt.Errorf("expected_revision must be positive integer")
		}
	}
	if action == "agent.schedules.create" {
		for _, key := range []string{"name", "prompt", "model_profile_id", "trigger"} {
			if _, ok := out[key]; !ok {
				return nil, fmt.Errorf("%s is required", key)
			}
		}
	}
	if action == "agent.schedules.update" {
		changed := false
		for _, key := range []string{"name", "prompt", "model_profile_id", "trigger", "skip_if_running"} {
			if _, ok := out[key]; ok {
				changed = true
			}
		}
		if !changed {
			return nil, fmt.Errorf("update requires a changed field")
		}
	}
	if key, ok := out["idempotency_key"].(string); ok && strings.TrimSpace(key) != "" {
		parsed, pe := uuid.Parse(key)
		if pe != nil || parsed.String() != key {
			return nil, fmt.Errorf("idempotency_key must be canonical UUID")
		}
	}
	if raw, ok := out["trigger"].(map[string]any); ok {
		kind, kOK := raw["kind"].(string)
		value, vOK := raw["value"].(string)
		tz, tOK := raw["timezone"].(string)
		if !kOK || !vOK || !tOK || value == "" || tz == "" || (kind != "one_time" && kind != "cron") {
			return nil, fmt.Errorf("trigger is invalid")
		}
	}
	if action != "agent.schedules.create" && strings.TrimSpace(fmt.Sprint(out["schedule_id"])) == "" {
		return nil, fmt.Errorf("schedule_id is required")
	}
	return out, nil
}
func confirmationSummary(action string, p map[string]any) string {
	s := action
	if id, ok := p["schedule_id"].(string); ok && strings.TrimSpace(id) != "" {
		s += " schedule " + id
	}
	if name, ok := p["name"].(string); ok && strings.TrimSpace(name) != "" {
		s += " " + name
	}
	r := []rune(s)
	if len(r) > 240 {
		r = r[:240]
	}
	return string(r)
}
func sanitizeConfirmationValue(v any) any {
	b, _ := json.Marshal(v)
	var x any
	_ = json.Unmarshal(b, &x)
	return x
}
