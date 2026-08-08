package agentgateway

import (
	"fmt"
	"unicode/utf8"
)

func validateImageToolActionResult(action string, request, output map[string]any, authority actionResultAuthority) error {
	if !authority.valid() {
		return imageToolResultError("prepared owner authority is missing")
	}
	if request == nil || output == nil {
		return imageToolResultError("request or response is missing")
	}
	switch action {
	case "agent.image_tools.upload.begin":
		if err := validateImageToolUploadProjection(output); err != nil {
			return err
		}
		if output["image_request_id"] != request["image_request_id"] {
			return imageToolResultError("image_request_id does not match the prepared request")
		}
		return nil
	case "agent.image_tools.upload.append":
		if err := validateImageToolUploadProjection(output); err != nil {
			return err
		}
		if output["upload_id"] != request["upload_id"] {
			return imageToolResultError("upload_id does not match the prepared request")
		}
		return nil
	case "agent.image_tools.upload.commit":
		return validateCommittedImageToolSource(output, request)
	case "agent.image_tools.extract_text":
		return validateImageToolExecutionResult(output, request, false)
	case "agent.image_tools.translate_text":
		return validateImageToolExecutionResult(output, request, true)
	default:
		return imageToolResultError("unsupported image tool action")
	}
}

func validateImageToolUploadProjection(output map[string]any) error {
	if err := imageToolExactFields(output, []string{
		"upload_id", "source_id", "image_request_id", "status", "received_size", "max_chunk_bytes", "revision", "expires_at",
	}, "upload projection"); err != nil {
		return err
	}
	for _, field := range []string{"upload_id", "source_id", "image_request_id"} {
		if !canonicalActionUUID(output[field]) {
			return imageToolResultError("%s must be a canonical UUID", field)
		}
	}
	status, _ := output["status"].(string)
	if status != "receiving" && status != "committed" && status != "consumed" {
		return imageToolResultError("status is invalid")
	}
	if !actionIntegerInRange(output["received_size"], 0, maxChatAttachmentBytes) {
		return imageToolResultError("received_size is invalid")
	}
	if !actionIntegerInRange(output["max_chunk_bytes"], maxChatAttachmentChunkBytes, maxChatAttachmentChunkBytes) {
		return imageToolResultError("max_chunk_bytes is invalid")
	}
	if !positiveInteger(output["revision"]) {
		return imageToolResultError("revision is invalid")
	}
	if !canonicalChatAttachmentTime(output["expires_at"]) {
		return imageToolResultError("expires_at is invalid")
	}
	return nil
}

func validateCommittedImageToolSource(output, request map[string]any) error {
	if err := imageToolExactFields(output, []string{
		"source_id", "source_revision", "image_request_id", "name", "mime_type", "size_bytes", "sha256", "status", "expires_at",
	}, "committed source"); err != nil {
		return err
	}
	for _, field := range []string{"source_id", "image_request_id"} {
		if !canonicalActionUUID(output[field]) {
			return imageToolResultError("%s must be a canonical UUID", field)
		}
	}
	if !actionIntegerInRange(output["source_revision"], 1, 1) {
		return imageToolResultError("source_revision must be 1")
	}
	if output["status"] != "committed" {
		return imageToolResultError("status must be committed")
	}
	if !validChatAttachmentName(output["name"]) {
		return imageToolResultError("name is invalid")
	}
	if !validImageToolMIME(output["mime_type"]) {
		return imageToolResultError("mime_type is invalid")
	}
	if !actionIntegerInRange(output["size_bytes"], 1, maxChatAttachmentBytes) {
		return imageToolResultError("size_bytes is invalid")
	}
	if !canonicalActionSHA256(output["sha256"]) || output["sha256"] != request["content_sha256"] {
		return imageToolResultError("sha256 does not match the prepared request")
	}
	if !canonicalChatAttachmentTime(output["expires_at"]) {
		return imageToolResultError("expires_at is invalid")
	}
	return nil
}

func validateImageToolExecutionResult(output, request map[string]any, translate bool) error {
	required := []string{"idempotency_key", "source_id", "source_revision", "text"}
	if translate {
		required = append(required, "target_locale")
	}
	if err := imageToolExactFields(output, required, "execution response"); err != nil {
		return err
	}
	for _, field := range []string{"idempotency_key", "source_id"} {
		if output[field] != request[field] {
			return imageToolResultError("%s does not match the prepared request", field)
		}
	}
	requestRevision, requestOK := actionInteger(request["source_revision"])
	resultRevision, resultOK := actionInteger(output["source_revision"])
	if !requestOK || !resultOK || requestRevision != 1 || resultRevision != requestRevision {
		return imageToolResultError("source_revision does not match the prepared request")
	}
	text, ok := output["text"].(string)
	if !ok || len(text) > maxTextToolOutputBytes || !utf8.ValidString(text) {
		return imageToolResultError("text must be UTF-8 of at most 65536 bytes")
	}
	if translate {
		if output["target_locale"] != request["target_locale"] || !canonicalBCP47Locale(output["target_locale"]) {
			return imageToolResultError("target_locale does not match the prepared request")
		}
	}
	return nil
}

func imageToolExactFields(value map[string]any, required []string, label string) error {
	if value == nil || len(value) != len(required) {
		return imageToolResultError("%s has an invalid field set", label)
	}
	for _, field := range required {
		if _, present := value[field]; !present {
			return imageToolResultError("%s is missing %s", label, field)
		}
	}
	return nil
}

func imageToolResultError(format string, args ...any) error {
	return fmt.Errorf("%w: image tools response "+format, append([]any{ErrInvalidActionResult}, args...)...)
}
