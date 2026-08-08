package agentgateway

import (
	"encoding/base64"
	"fmt"
	"time"
)

func validateChatAttachmentActionResult(action string, request, output map[string]any, authority actionResultAuthority) error {
	if !authority.valid() {
		return chatAttachmentResultError("prepared owner authority is missing")
	}
	if request == nil || output == nil {
		return chatAttachmentResultError("request or response is missing")
	}
	switch action {
	case "agent.chat.attachment.begin":
		if err := validateChatAttachmentUploadProjection(output); err != nil {
			return err
		}
		if output["turn_request_id"] != request["turn_request_id"] {
			return chatAttachmentResultError("turn_request_id does not match the prepared request")
		}
		received, _ := actionInteger(output["received_size"])
		revision, _ := actionInteger(output["revision"])
		if received != 0 || revision != 1 {
			return chatAttachmentResultError("begin upload projection is not at its initial revision")
		}
		return nil
	case "agent.chat.attachment.append":
		if err := validateChatAttachmentUploadProjection(output); err != nil {
			return err
		}
		if output["upload_id"] != request["upload_id"] {
			return chatAttachmentResultError("upload_id does not match the prepared request")
		}
		expectedRevision, ok := actionInteger(request["expected_revision"])
		if !ok || expectedRevision <= 0 {
			return chatAttachmentResultError("prepared expected_revision is invalid")
		}
		revision, _ := actionInteger(output["revision"])
		if expectedRevision == int64(^uint64(0)>>1) || revision != expectedRevision+1 {
			return chatAttachmentResultError("revision does not advance the prepared request")
		}
		offset, ok := actionInteger(request["offset_bytes"])
		if !ok || offset < 0 || offset > maxChatAttachmentBytes {
			return chatAttachmentResultError("prepared offset_bytes is invalid")
		}
		encoded, ok := request["data_base64"].(string)
		if !ok {
			return chatAttachmentResultError("prepared chunk is missing")
		}
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(decoded) == 0 || int64(len(decoded)) > maxChatAttachmentChunkBytes {
			return chatAttachmentResultError("prepared chunk is invalid")
		}
		defer clear(decoded)
		received, _ := actionInteger(output["received_size"])
		if offset > maxChatAttachmentBytes-int64(len(decoded)) || received != offset+int64(len(decoded)) {
			return chatAttachmentResultError("received_size does not match the prepared chunk")
		}
		return nil
	case "agent.chat.attachment.commit":
		return validateCommittedChatAttachment(output, request)
	default:
		return chatAttachmentResultError("unsupported attachment action")
	}
}

func validateChatAttachmentUploadProjection(output map[string]any) error {
	if err := chatAttachmentExact(output, []string{
		"upload_id", "source_id", "turn_request_id", "status", "received_size", "max_chunk_bytes", "revision", "expires_at",
	}, "upload projection"); err != nil {
		return err
	}
	for _, field := range []string{"upload_id", "source_id", "turn_request_id"} {
		if !canonicalActionUUID(output[field]) {
			return chatAttachmentResultError("%s must be a canonical UUID", field)
		}
	}
	if output["status"] != "receiving" {
		return chatAttachmentResultError("status must be receiving")
	}
	if !actionIntegerInRange(output["received_size"], 0, maxChatAttachmentBytes) {
		return chatAttachmentResultError("received_size is invalid")
	}
	if !actionIntegerInRange(output["max_chunk_bytes"], maxChatAttachmentChunkBytes, maxChatAttachmentChunkBytes) {
		return chatAttachmentResultError("max_chunk_bytes is invalid")
	}
	if !positiveInteger(output["revision"]) {
		return chatAttachmentResultError("revision is invalid")
	}
	if !canonicalChatAttachmentTime(output["expires_at"]) {
		return chatAttachmentResultError("expires_at is invalid")
	}
	return nil
}

func validateCommittedChatAttachment(attachment, request map[string]any) error {
	if err := chatAttachmentExact(attachment, []string{
		"source_id", "revision", "turn_request_id", "kind", "name", "mime_type", "size_bytes", "sha256", "status", "expires_at",
	}, "attachment"); err != nil {
		return err
	}
	for _, field := range []string{"source_id", "turn_request_id"} {
		if !canonicalActionUUID(attachment[field]) {
			return chatAttachmentResultError("attachment %s must be a canonical UUID", field)
		}
	}
	if !actionIntegerInRange(attachment["revision"], 1, 1) {
		return chatAttachmentResultError("attachment revision must be 1")
	}
	if attachment["status"] != "committed" {
		return chatAttachmentResultError("attachment kind or status is invalid")
	}
	if !validChatAttachmentName(attachment["name"]) {
		return chatAttachmentResultError("attachment name is invalid")
	}
	if !validChatAttachmentMIME(attachment["kind"], attachment["mime_type"]) {
		return chatAttachmentResultError("attachment mime_type is invalid")
	}
	if !actionIntegerInRange(attachment["size_bytes"], 1, maxChatAttachmentBytes) {
		return chatAttachmentResultError("attachment size_bytes is invalid")
	}
	if !canonicalActionSHA256(attachment["sha256"]) || attachment["sha256"] != request["content_sha256"] {
		return chatAttachmentResultError("attachment sha256 does not match the prepared request")
	}
	if !canonicalChatAttachmentTime(attachment["expires_at"]) {
		return chatAttachmentResultError("attachment expires_at is invalid")
	}
	return nil
}

func canonicalChatAttachmentTime(value any) bool {
	text, ok := value.(string)
	if !ok || text == "" {
		return false
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	return err == nil && !parsed.IsZero() && parsed.UTC().Format(time.RFC3339Nano) == text
}

func chatAttachmentExact(value map[string]any, required []string, label string) error {
	if value == nil || len(value) != len(required) {
		return chatAttachmentResultError("%s has an invalid field set", label)
	}
	for _, field := range required {
		if _, present := value[field]; !present {
			return chatAttachmentResultError("%s is missing %s", label, field)
		}
	}
	return nil
}

func chatAttachmentResultError(format string, args ...any) error {
	return fmt.Errorf("%w: chat attachment response "+format, append([]any{ErrInvalidActionResult}, args...)...)
}
