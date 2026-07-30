package nativeagent

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
)

const nativeAgentReferencePreviewRunes = 240

func nativeAgentReferences(produced []*schema.Message) []map[string]any {
	references := make([]map[string]any, 0)
	seenRooms := map[string]struct{}{}
	seenPosts := map[string]struct{}{}
	seenConfirmations := map[string]struct{}{}

	addRoom := func(roomID, roomType, title, preview string) {
		roomID = strings.TrimSpace(roomID)
		roomType = normalizedReferenceRoomType(roomType)
		if roomID == "" {
			return
		}
		if _, exists := seenRooms[roomID]; exists {
			return
		}
		seenRooms[roomID] = struct{}{}
		reference := map[string]any{
			"kind":    "room",
			"room_id": roomID,
		}
		if roomType != "" {
			reference["room_type"] = roomType
		}
		if title = strings.TrimSpace(title); title != "" {
			reference["title"] = title
		}
		if preview = referencePreview(preview); preview != "" {
			reference["preview"] = preview
		}
		references = append(references, reference)
	}

	addPost := func(roomID, channelID, postID, title, preview string) {
		roomID = strings.TrimSpace(roomID)
		channelID = strings.TrimSpace(channelID)
		postID = strings.TrimSpace(postID)
		if roomID == "" || channelID == "" || postID == "" {
			return
		}
		key := channelID + "\x00" + postID
		if _, exists := seenPosts[key]; exists {
			return
		}
		seenPosts[key] = struct{}{}
		reference := map[string]any{
			"kind":       "channel_post",
			"room_id":    roomID,
			"channel_id": channelID,
			"post_id":    postID,
		}
		if title = strings.TrimSpace(title); title != "" {
			reference["title"] = title
		}
		if preview = referencePreview(preview); preview != "" {
			reference["preview"] = preview
		}
		references = append(references, reference)
	}

	addPendingConfirmation := func(result json.RawMessage, action string) {
		var envelope struct {
			Operation struct {
				OperationID    string `json:"operation_id"`
				WorkloadID     string `json:"workload_id"`
				PlanID         string `json:"plan_id"`
				TaskID         string `json:"task_id"`
				ConfirmationID string `json:"confirmation_id"`
				Revision       int64  `json:"revision"`
				PlanRevision   int64  `json:"plan_revision"`
				PlanDigest     string `json:"plan_digest"`
				TargetKind     string `json:"target_kind"`
				Summary        string `json:"summary"`
				Kind           string `json:"kind"`
			} `json:"operation"`
			Confirmation struct {
				ConfirmationID string `json:"confirmation_id"`
				TaskID         string `json:"task_id"`
				State          string `json:"state"`
				Revision       int64  `json:"revision"`
				ExpiresAt      string `json:"expires_at"`
				Binding        struct {
					OperationDomain string `json:"operation_domain"`
					TargetID        string `json:"target_id"`
					TargetRevision  int64  `json:"target_revision"`
					ContentDigest   string `json:"content_digest"`
				} `json:"binding"`
			} `json:"confirmation"`
		}
		if json.Unmarshal(result, &envelope) != nil {
			return
		}
		op := envelope.Operation
		confirmation := envelope.Confirmation
		action = strings.ToLower(strings.TrimSpace(action))
		expiresAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(confirmation.ExpiresAt))
		if (action != "apply" && action != "destroy") || strings.ToLower(strings.TrimSpace(op.Kind)) != action ||
			!validReferenceUUID(op.OperationID) || !validReferenceUUID(op.WorkloadID) ||
			!validReferenceUUID(op.PlanID) || !validReferenceUUID(op.TaskID) ||
			!validReferenceUUID(op.ConfirmationID) || !validReferenceUUID(confirmation.ConfirmationID) ||
			op.ConfirmationID != confirmation.ConfirmationID || confirmation.State != "pending" ||
			op.Revision <= 0 || op.PlanRevision <= 0 || confirmation.Revision <= 0 ||
			confirmation.TaskID != op.TaskID || confirmation.Binding.OperationDomain != "workload:"+action ||
			confirmation.Binding.TargetID != op.WorkloadID || confirmation.Binding.TargetRevision != op.PlanRevision ||
			confirmation.Binding.ContentDigest != op.PlanDigest || !validReferenceDigest(op.PlanDigest) ||
			strings.TrimSpace(confirmation.ExpiresAt) == "" || err != nil || !expiresAt.After(time.Now()) {
			return
		}
		id := strings.TrimSpace(op.ConfirmationID)
		if _, exists := seenConfirmations[id]; exists {
			return
		}
		seenConfirmations[id] = struct{}{}
		reference := map[string]any{
			"kind":            "pending_confirmation",
			"confirmation_id": id,
			"operation_id":    strings.TrimSpace(op.OperationID),
			"workload_id":     strings.TrimSpace(op.WorkloadID),
			"task_id":         strings.TrimSpace(op.TaskID),
			"plan_id":         strings.TrimSpace(op.PlanID),
			"action":          action,
			"revision":        confirmation.Revision,
			"expires_at":      strings.TrimSpace(confirmation.ExpiresAt),
		}
		if target := strings.TrimSpace(op.TargetKind); target != "" {
			reference["target_kind"] = target
		}
		if summary := strings.TrimSpace(op.Summary); summary != "" {
			reference["summary"] = referencePreview(summary)
		}
		if digest := strings.TrimSpace(op.PlanDigest); digest != "" {
			reference["plan_digest"] = digest
		}
		references = append(references, reference)
	}

	for _, message := range produced {
		if message == nil || message.Role != schema.Tool {
			continue
		}
		var envelope struct {
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal([]byte(message.Content), &envelope); err != nil || len(envelope.Result) == 0 || string(envelope.Result) == "null" {
			continue
		}

		switch strings.TrimSpace(message.ToolName) {
		case "native_agent_workloads_apply":
			addPendingConfirmation(envelope.Result, "apply")
		case "native_agent_workloads_destroy":
			addPendingConfirmation(envelope.Result, "destroy")
		case "dirextalk_contacts_list", "dirextalk_contacts_search":
			var result struct {
				Contacts []struct {
					RoomID      string `json:"room_id"`
					DisplayName string `json:"display_name"`
				} `json:"contacts"`
			}
			if json.Unmarshal(envelope.Result, &result) != nil {
				continue
			}
			for _, contact := range result.Contacts {
				addRoom(contact.RoomID, "direct", contact.DisplayName, "")
			}
		case "dirextalk_rooms_search":
			var result struct {
				Rooms []struct {
					Type   string `json:"type"`
					Name   string `json:"name"`
					RoomID string `json:"room_id"`
				} `json:"rooms"`
			}
			if json.Unmarshal(envelope.Result, &result) != nil {
				continue
			}
			for _, room := range result.Rooms {
				if normalizedReferenceRoomType(room.Type) == "" {
					continue
				}
				addRoom(room.RoomID, room.Type, room.Name, "")
			}
		case "dirextalk_messages_list":
			var result struct {
				RoomID   string `json:"room_id"`
				Name     string `json:"name"`
				Messages []struct {
					Msg string `json:"msg"`
				} `json:"messages"`
			}
			if json.Unmarshal(envelope.Result, &result) != nil {
				continue
			}
			preview := ""
			if len(result.Messages) > 0 {
				preview = result.Messages[0].Msg
			}
			addRoom(result.RoomID, "", result.Name, preview)
		case "dirextalk_channel_posts_list":
			var result struct {
				RoomID    string `json:"room_id"`
				ChannelID string `json:"channel_id"`
				Name      string `json:"name"`
				Posts     []struct {
					PostID string `json:"post_id"`
					Msg    string `json:"msg"`
				} `json:"posts"`
			}
			if json.Unmarshal(envelope.Result, &result) != nil {
				continue
			}
			for _, post := range result.Posts {
				addPost(result.RoomID, result.ChannelID, post.PostID, result.Name, post.Msg)
			}
		}
	}
	return references
}

func validReferenceUUID(value string) bool {
	value = strings.TrimSpace(value)
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}

func validReferenceDigest(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func normalizedReferenceRoomType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "contact", "direct":
		return "direct"
	case "group":
		return "group"
	case "channel":
		return "channel"
	default:
		return ""
	}
}

func referencePreview(value string) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= nativeAgentReferencePreviewRunes {
		return value
	}
	return string(runes[:nativeAgentReferencePreviewRunes]) + "..."
}
