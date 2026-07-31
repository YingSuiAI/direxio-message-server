package nativeagent

import (
	"encoding/hex"
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
)

const nativeAgentReferencePreviewRunes = 240

// ReferencesFromToolMessages projects only server-authored, immutable
// identities. Execution V2 references are deliberately informational: they do
// not carry an action, route, or an actionable confirmation-card kind and
// therefore can never authorize, auto-confirm, or auto-navigate a run.
func ReferencesFromToolMessages(produced []*schema.Message) []map[string]any {
	return nativeAgentReferences(produced)
}

func nativeAgentReferences(produced []*schema.Message) []map[string]any {
	refs := make([]map[string]any, 0)
	seenRooms := map[string]bool{}
	seenPosts := map[string]bool{}
	seenExecution := map[string]bool{}

	addRoom := func(roomID, typ, title, preview string) {
		roomID = strings.TrimSpace(roomID)
		if roomID == "" || seenRooms[roomID] {
			return
		}
		seenRooms[roomID] = true
		ref := map[string]any{"kind": "room", "room_id": roomID}
		if typ = strings.TrimSpace(typ); typ != "" {
			ref["room_type"] = typ
		}
		if title = strings.TrimSpace(title); title != "" {
			ref["title"] = title
		}
		if preview = referencePreview(preview); preview != "" {
			ref["preview"] = preview
		}
		refs = append(refs, ref)
	}
	addPost := func(roomID, channelID, postID, title, preview string) {
		roomID, channelID, postID = strings.TrimSpace(roomID), strings.TrimSpace(channelID), strings.TrimSpace(postID)
		if roomID == "" || channelID == "" || postID == "" {
			return
		}
		key := roomID + "\x00" + channelID + "\x00" + postID
		if seenPosts[key] {
			return
		}
		seenPosts[key] = true
		ref := map[string]any{"kind": "channel_post", "room_id": roomID, "channel_id": channelID, "post_id": postID}
		if title = strings.TrimSpace(title); title != "" {
			ref["title"] = title
		}
		if preview = referencePreview(preview); preview != "" {
			ref["preview"] = preview
		}
		refs = append(refs, ref)
	}
	addExecution := func(ref map[string]any) {
		kind, _ := ref["kind"].(string)
		id := executionReferenceIdentity(ref)
		if kind == "" || id == "" {
			return
		}
		key := kind + "\x00" + id
		if seenExecution[key] {
			return
		}
		seenExecution[key] = true
		refs = append(refs, ref)
	}

	for _, message := range produced {
		if message == nil || message.Role != schema.Tool {
			continue
		}
		resultRaw, result, ok := nativeToolResult(message.Content)
		if !ok {
			continue
		}
		name := strings.TrimSpace(message.ToolName)
		var roomResult struct {
			RoomID    string `json:"room_id"`
			Name      string `json:"name"`
			ChannelID string `json:"channel_id"`
			Messages  []struct {
				Msg string `json:"msg"`
			} `json:"messages"`
			Posts []struct {
				PostID string `json:"post_id"`
				Msg    string `json:"msg"`
			} `json:"posts"`
			Rooms []struct {
				RoomID string `json:"room_id"`
				Type   string `json:"type"`
				Name   string `json:"name"`
			} `json:"rooms"`
			Contacts []struct {
				RoomID      string `json:"room_id"`
				DisplayName string `json:"display_name"`
			} `json:"contacts"`
		}
		_ = json.Unmarshal(resultRaw, &roomResult)
		switch name {
		case "dirextalk_contacts_list", "dirextalk_contacts_search":
			for _, contact := range roomResult.Contacts {
				addRoom(contact.RoomID, "direct", contact.DisplayName, "")
			}
		case "dirextalk_rooms_search":
			for _, room := range roomResult.Rooms {
				addRoom(room.RoomID, room.Type, room.Name, "")
			}
		case "dirextalk_messages_list":
			preview := ""
			if len(roomResult.Messages) > 0 {
				preview = roomResult.Messages[0].Msg
			}
			addRoom(roomResult.RoomID, "", roomResult.Name, preview)
		case "dirextalk_channel_posts_list":
			for _, post := range roomResult.Posts {
				addPost(roomResult.RoomID, roomResult.ChannelID, post.PostID, roomResult.Name, post.Msg)
			}
		}

		switch name {
		case "native_agent_execution_v2_plans_create", "native_agent_execution_v2_plans_get":
			if ref, ok := executionPlanReference(result); ok {
				addExecution(ref)
			}
		case "native_agent_execution_v2_runs_create", "native_agent_execution_v2_runs_get", "native_agent_execution_v2_runs_status":
			run, container, ref, ok := executionRunReference(result)
			if !ok {
				continue
			}
			addExecution(ref)
			if name == "native_agent_execution_v2_runs_create" {
				confirmations := referenceObjectSlice(container, "confirmations")
				if len(confirmations) == 0 {
					confirmations = referenceObjectSlice(result, "confirmations")
				}
				acceptedConfirmation := false
				for _, confirmation := range confirmations {
					if confirmationRef, valid := executionConfirmationReference(confirmation, run); valid {
						addExecution(confirmationRef)
						acceptedConfirmation = true
					}
				}
				// The public runs.create projection intentionally omits confirmation
				// binding internals. Its server-authored stage projection still
				// carries a safe confirmation identity plus exact stage/run/plan
				// revisions and digests, which is sufficient for an informational
				// reference without creating an actionable confirmation card.
				if !acceptedConfirmation {
					for _, stage := range referenceObjectSlice(result, "stages") {
						if confirmationRef, valid := executionStageConfirmationReference(stage, run); valid {
							addExecution(confirmationRef)
						}
					}
				}
			}
		case "native_agent_execution_v2_service_bindings_list":
			for _, binding := range referenceObjectSlice(result, "bindings") {
				if ref, ok := serviceBindingReference(binding); ok {
					addExecution(ref)
				}
			}
		case "native_agent_execution_v2_service_bindings_get":
			if binding := referenceObject(result, "binding"); binding != nil {
				if ref, ok := serviceBindingReference(binding); ok {
					addExecution(ref)
				}
			}
		}
	}
	return refs
}

func nativeToolResult(content string) (json.RawMessage, map[string]any, bool) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &root); err != nil {
		return nil, nil, false
	}
	raw := json.RawMessage(content)
	if value, exists := root["result"]; exists && len(value) > 0 && string(value) != "null" {
		raw = value
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var result map[string]any
	if err := decoder.Decode(&result); err != nil || result == nil {
		return nil, nil, false
	}
	return raw, result, true
}

func executionPlanReference(result map[string]any) (map[string]any, bool) {
	plan := referenceObject(result, "plan")
	if plan == nil {
		return nil, false
	}
	id := referenceString(plan, "id", "plan_id")
	revision := referenceUint(plan, "revision", "plan_revision")
	digest := referenceString(plan, "digest", "plan_digest")
	if !validReferenceUUID(id) || revision == 0 || !validReferenceDigest(digest) {
		return nil, false
	}
	return map[string]any{"kind": "execution_plan", "plan_id": id, "plan_revision": revision, "plan_digest": digest}, true
}

func executionRunReference(result map[string]any) (map[string]any, map[string]any, map[string]any, bool) {
	container := referenceObject(result, "run")
	if container == nil {
		return nil, nil, nil, false
	}
	run := container
	if nested := referenceObject(container, "run"); nested != nil {
		run = nested
	}
	runID := referenceString(run, "run_id")
	runRevision := referenceUint(run, "revision", "run_revision")
	runDigest := referenceString(run, "run_digest")
	planID := referenceString(run, "plan_id")
	planRevision := referenceUint(run, "plan_revision")
	planDigest := referenceString(run, "plan_digest")
	if !validReferenceUUID(runID) || runRevision == 0 || !validReferenceDigest(runDigest) ||
		!validReferenceUUID(planID) || planRevision == 0 || !validReferenceDigest(planDigest) {
		return nil, nil, nil, false
	}
	ref := map[string]any{
		"kind": "execution_run", "run_id": runID, "run_revision": runRevision, "run_digest": runDigest,
		"plan_id": planID, "plan_revision": planRevision, "plan_digest": planDigest,
	}
	if deploymentID := referenceString(run, "deployment_id"); validReferenceUUID(deploymentID) {
		ref["deployment_id"] = deploymentID
	}
	if status := referenceString(run, "status"); status != "" {
		ref["status"] = status
	}
	return run, container, ref, true
}

func executionConfirmationReference(confirmation, run map[string]any) (map[string]any, bool) {
	binding := referenceObject(confirmation, "binding")
	if binding == nil {
		return nil, false
	}
	id := referenceString(confirmation, "id", "confirmation_id")
	revision := referenceUint(confirmation, "revision", "confirmation_revision")
	bindingDigest := referenceString(binding, "digest", "binding_digest")
	operationDomain := referenceString(binding, "operation_domain")
	planID := referenceString(binding, "plan_id")
	planRevision := referenceUint(binding, "plan_revision")
	planDigest := referenceString(binding, "plan_digest")
	runID := referenceString(binding, "run_id")
	runRevision := referenceUint(binding, "run_revision")
	stageID := referenceString(binding, "stage_id")
	stageRevision := referenceUint(binding, "stage_revision")
	stageDigest := referenceString(binding, "stage_digest")
	targetID := referenceString(binding, "target_id")
	targetRevision := referenceUint(binding, "target_revision")
	targetDigest := referenceString(binding, "target_digest")
	previewDigest := referenceString(binding, "preview_digest")
	if !validReferenceUUID(id) || revision == 0 || !validReferenceDigest(bindingDigest) ||
		!strings.HasPrefix(operationDomain, "execution:v2:") ||
		!validReferenceUUID(planID) || planRevision == 0 || !validReferenceDigest(planDigest) ||
		!validReferenceUUID(runID) || runRevision == 0 ||
		!validReferenceUUID(stageID) || stageRevision == 0 || !validReferenceDigest(stageDigest) ||
		!validReferenceUUID(targetID) || targetRevision == 0 || !validReferenceDigest(targetDigest) ||
		!validReferenceDigest(previewDigest) {
		return nil, false
	}
	if runID != referenceString(run, "run_id") || runRevision != referenceUint(run, "revision", "run_revision") ||
		planID != referenceString(run, "plan_id") || planRevision != referenceUint(run, "plan_revision") ||
		planDigest != referenceString(run, "plan_digest") {
		return nil, false
	}
	ref := map[string]any{
		"kind": "execution_confirmation", "confirmation_id": id, "confirmation_revision": revision,
		"binding_digest": bindingDigest, "plan_id": planID, "plan_revision": planRevision, "plan_digest": planDigest,
		"run_id": runID, "run_revision": runRevision, "stage_id": stageID, "stage_revision": stageRevision,
		"stage_digest": stageDigest, "target_id": targetID, "target_revision": targetRevision,
		"target_digest": targetDigest, "preview_digest": previewDigest,
	}
	if state := referenceString(confirmation, "state"); state != "" {
		ref["state"] = state
	}
	if risk := referenceString(binding, "risk_level"); risk != "" {
		ref["risk_level"] = risk
	}
	if gate := referenceString(binding, "gate_type"); gate != "" {
		ref["gate_type"] = gate
	}
	return ref, true
}

func executionStageConfirmationReference(stage, run map[string]any) (map[string]any, bool) {
	confirmationID := referenceString(stage, "confirmation_id")
	stageID := referenceString(stage, "stage_id")
	stageRevision := referenceUint(stage, "stage_revision")
	stageDigest := referenceString(stage, "stage_digest")
	runID := referenceString(stage, "run_id")
	runRevision := referenceUint(stage, "run_revision")
	planID := referenceString(stage, "plan_id")
	planRevision := referenceUint(stage, "plan_revision")
	targetID := referenceString(stage, "target_id")
	targetRevision := referenceUint(stage, "target_revision")
	targetDigest := referenceString(stage, "target_digest")
	runDigest := referenceString(run, "run_digest")
	planDigest := referenceString(run, "plan_digest")
	if !validReferenceUUID(confirmationID) || !validReferenceUUID(stageID) || stageRevision == 0 || !validReferenceDigest(stageDigest) ||
		!validReferenceUUID(runID) || runRevision == 0 || !validReferenceDigest(runDigest) ||
		!validReferenceUUID(planID) || planRevision == 0 || !validReferenceDigest(planDigest) ||
		!validReferenceUUID(targetID) || targetRevision == 0 || !validReferenceDigest(targetDigest) {
		return nil, false
	}
	if runID != referenceString(run, "run_id") || runRevision != referenceUint(run, "revision", "run_revision") ||
		planID != referenceString(run, "plan_id") || planRevision != referenceUint(run, "plan_revision") {
		return nil, false
	}
	return map[string]any{
		"kind": "execution_confirmation", "confirmation_id": confirmationID,
		"plan_id": planID, "plan_revision": planRevision, "plan_digest": planDigest,
		"run_id": runID, "run_revision": runRevision, "run_digest": runDigest,
		"stage_id": stageID, "stage_revision": stageRevision, "stage_digest": stageDigest,
		"target_id": targetID, "target_revision": targetRevision, "target_digest": targetDigest,
	}, true
}

func serviceBindingReference(binding map[string]any) (map[string]any, bool) {
	id := referenceString(binding, "binding_id")
	revision := referenceUint(binding, "revision")
	digest := referenceString(binding, "digest")
	deploymentID := referenceString(binding, "deployment_id")
	projectID := referenceString(binding, "project_id")
	runID := referenceString(binding, "run_id")
	targetID := referenceString(binding, "target_id")
	targetRevision := referenceUint(binding, "target_revision")
	targetDigest := referenceString(binding, "target_digest")
	if !validReferenceUUID(id) || revision == 0 || !validReferenceDigest(digest) ||
		!validReferenceUUID(deploymentID) || !validReferenceUUID(projectID) || !validReferenceUUID(runID) ||
		!validReferenceUUID(targetID) || targetRevision == 0 || !validReferenceDigest(targetDigest) {
		return nil, false
	}
	return map[string]any{
		"kind": "service_binding", "binding_id": id, "binding_revision": revision, "binding_digest": digest,
		"deployment_id": deploymentID, "project_id": projectID, "run_id": runID,
		"target_id": targetID, "target_revision": targetRevision, "target_digest": targetDigest,
	}, true
}

func executionReferenceIdentity(ref map[string]any) string {
	for _, key := range []string{"plan_id", "run_id", "confirmation_id", "binding_id"} {
		if value, ok := ref[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

func referenceObject(object map[string]any, key string) map[string]any {
	value, ok := referenceValue(object, key)
	if !ok {
		return nil
	}
	result, _ := value.(map[string]any)
	return result
}

func referenceObjectSlice(object map[string]any, key string) []map[string]any {
	value, ok := referenceValue(object, key)
	if !ok {
		return nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if object, ok := item.(map[string]any); ok {
			result = append(result, object)
		}
	}
	return result
}

func referenceString(object map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := referenceValue(object, key); ok {
			if text, ok := value.(string); ok {
				if text = strings.TrimSpace(text); text != "" {
					return text
				}
			}
		}
	}
	return ""
}

func referenceUint(object map[string]any, keys ...string) uint64 {
	for _, key := range keys {
		value, ok := referenceValue(object, key)
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case json.Number:
			n, err := strconv.ParseUint(string(typed), 10, 64)
			if err == nil {
				return n
			}
		case float64:
			if typed > 0 && typed <= math.MaxUint64 && math.Trunc(typed) == typed {
				return uint64(typed)
			}
		case int:
			if typed > 0 {
				return uint64(typed)
			}
		case int64:
			if typed > 0 {
				return uint64(typed)
			}
		case uint64:
			return typed
		}
	}
	return 0
}

func referenceValue(object map[string]any, key string) (any, bool) {
	want := normalizedReferenceKey(key)
	for candidate, value := range object {
		if normalizedReferenceKey(candidate) == want {
			return value, true
		}
	}
	return nil, false
}

func normalizedReferenceKey(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	return strings.NewReplacer("_", "", "-", "", ".", "").Replace(key)
}

func validReferenceUUID(value string) bool {
	_, err := uuid.Parse(strings.TrimSpace(value))
	return err == nil
}

func validReferenceDigest(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func referencePreview(value string) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > nativeAgentReferencePreviewRunes {
		runes = runes[:nativeAgentReferencePreviewRunes]
	}
	return string(runes)
}
