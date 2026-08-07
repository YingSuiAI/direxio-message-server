package agentgateway

import (
	"errors"
	"testing"
)

const (
	attachmentIdempotencyID = "11111111-1111-4111-8111-111111111111"
	attachmentTurnRequestID = "22222222-2222-4222-8222-222222222222"
	attachmentUploadID      = "33333333-3333-4333-8333-333333333333"
	attachmentSourceID      = "44444444-4444-4444-8444-444444444444"
	attachmentSHA256        = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
)

var attachmentAuthority = actionResultAuthority{ownerID: "@owner:example.test", accountGeneration: 7}

func validAttachmentUploadProjection(revision, received int64) map[string]any {
	return map[string]any{
		"upload_id": attachmentUploadID, "source_id": attachmentSourceID,
		"turn_request_id": attachmentTurnRequestID, "status": "receiving",
		"received_size": received, "max_chunk_bytes": int64(maxChatAttachmentChunkBytes),
		"revision": revision, "expires_at": "2030-01-02T03:04:05Z",
	}
}

func validCommittedAttachment() map[string]any {
	return map[string]any{
		"source_id": attachmentSourceID, "revision": int64(1),
		"turn_request_id": attachmentTurnRequestID, "kind": "image", "name": "image.png",
		"mime_type": "image/png", "size_bytes": int64(5), "sha256": attachmentSHA256,
		"status": "committed", "expires_at": "2030-01-02T03:04:05Z",
	}
}

func TestChatAttachmentResultsValidateAgentShapeAndProjectPublicEnvelope(t *testing.T) {
	beginRequest := map[string]any{
		"idempotency_key": attachmentIdempotencyID,
		"turn_request_id": attachmentTurnRequestID,
	}
	begin := validAttachmentUploadProjection(1, 0)
	if got, err := adaptActionResultForRequestWithAuthority("agent.chat.attachment.begin", beginRequest, begin, attachmentAuthority); err != nil || got["upload_id"] != attachmentUploadID {
		t.Fatalf("begin projection = %#v, err %v", got, err)
	}

	appendRequest := map[string]any{
		"idempotency_key": attachmentIdempotencyID, "upload_id": attachmentUploadID,
		"expected_revision": int64(1), "offset_bytes": int64(0), "data_base64": "aGVsbG8=",
	}
	appendResult := validAttachmentUploadProjection(2, 5)
	if got, err := adaptActionResultForRequestWithAuthority("agent.chat.attachment.append", appendRequest, appendResult, attachmentAuthority); err != nil || got["received_size"] != int64(5) {
		t.Fatalf("append projection = %#v, err %v", got, err)
	}

	commitRequest := map[string]any{
		"idempotency_key": attachmentIdempotencyID, "upload_id": attachmentUploadID,
		"expected_revision": int64(2), "content_sha256": attachmentSHA256,
		"kind": "image", "name": "image.png", "mime_type": "image/png", "declared_size": int64(5),
	}
	commitResult := validCommittedAttachment()
	got, err := adaptActionResultForRequestWithAuthority("agent.chat.attachment.commit", commitRequest, commitResult, attachmentAuthority)
	if err != nil {
		t.Fatalf("commit result rejected: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("commit public envelope has extra fields: %#v", got)
	}
	attachment, ok := got["attachment"].(map[string]any)
	if !ok || attachment["source_id"] != attachmentSourceID {
		t.Fatalf("commit public envelope = %#v", got)
	}
}

func TestChatAttachmentCommitAcceptsCurrentKindAndMIMEContract(t *testing.T) {
	request := map[string]any{"content_sha256": attachmentSHA256}
	for name, fields := range map[string]map[string]any{
		"image": {
			"kind": "image", "name": "image.webp", "mime_type": "image/webp",
		},
		"file": {
			"kind": "file", "name": "task.json", "mime_type": "application/json",
		},
		"structured suffix file": {
			"kind": "file", "name": "events.data", "mime_type": "application/vnd.example+json",
		},
		"workspace archive": {
			"kind": "workspace_archive", "name": "workspace.tar.gz",
			"mime_type": "application/vnd.dirextalk.workspace+tar+gzip",
		},
	} {
		t.Run(name, func(t *testing.T) {
			result := validCommittedAttachment()
			for field, value := range fields {
				result[field] = value
			}
			result["size_bytes"] = int64(maxChatAttachmentBytes)
			if _, err := adaptActionResultForRequestWithAuthority(
				"agent.chat.attachment.commit", request, result, attachmentAuthority,
			); err != nil {
				t.Fatalf("valid %s result rejected: %v", name, err)
			}
		})
	}

	for name, fields := range map[string]map[string]any{
		"unknown kind":       {"kind": "archive"},
		"image as file":      {"kind": "file", "mime_type": "image/png"},
		"parameterized MIME": {"kind": "file", "mime_type": "text/plain; charset=utf-8"},
		"generic gzip": {
			"kind": "workspace_archive", "mime_type": "application/gzip",
		},
	} {
		t.Run("reject "+name, func(t *testing.T) {
			result := validCommittedAttachment()
			for field, value := range fields {
				result[field] = value
			}
			if _, err := adaptActionResultForRequestWithAuthority(
				"agent.chat.attachment.commit", request, result, attachmentAuthority,
			); !errors.Is(err, ErrInvalidActionResult) {
				t.Fatalf("invalid %s result accepted: %v", name, err)
			}
		})
	}
}

func TestChatAttachmentResultsRejectMissingAuthorityAndIdentityDrift(t *testing.T) {
	beginRequest := map[string]any{"turn_request_id": attachmentTurnRequestID}
	if _, err := adaptActionResultForRequestWithAuthority("agent.chat.attachment.begin", beginRequest, validAttachmentUploadProjection(1, 0), actionResultAuthority{}); !errors.Is(err, ErrInvalidActionResult) {
		t.Fatalf("missing prepared authority accepted: %v", err)
	}

	beginDrift := validAttachmentUploadProjection(1, 0)
	beginDrift["turn_request_id"] = "55555555-5555-4555-8555-555555555555"
	if _, err := adaptActionResultForRequestWithAuthority("agent.chat.attachment.begin", beginRequest, beginDrift, attachmentAuthority); !errors.Is(err, ErrInvalidActionResult) {
		t.Fatalf("begin turn identity drift accepted: %v", err)
	}

	appendRequest := map[string]any{
		"upload_id": attachmentUploadID, "expected_revision": int64(1),
		"offset_bytes": int64(0), "data_base64": "aGVsbG8=",
	}
	for name, mutate := range map[string]func(map[string]any){
		"upload":        func(value map[string]any) { value["upload_id"] = "55555555-5555-4555-8555-555555555555" },
		"revision":      func(value map[string]any) { value["revision"] = int64(3) },
		"received size": func(value map[string]any) { value["received_size"] = int64(4) },
		"extra field":   func(value map[string]any) { value["diagnostics"] = "private" },
	} {
		t.Run(name, func(t *testing.T) {
			result := validAttachmentUploadProjection(2, 5)
			mutate(result)
			if _, err := adaptActionResultForRequestWithAuthority("agent.chat.attachment.append", appendRequest, result, attachmentAuthority); !errors.Is(err, ErrInvalidActionResult) {
				t.Fatalf("drifted append result accepted: %v", err)
			}
		})
	}

	commitRequest := map[string]any{"content_sha256": attachmentSHA256, "kind": "image", "name": "image.png", "mime_type": "image/png", "declared_size": int64(5)}
	for name, mutate := range map[string]func(map[string]any){
		"digest": func(value map[string]any) {
			value["sha256"] = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		},
		"revision":    func(value map[string]any) { value["revision"] = int64(2) },
		"extra field": func(value map[string]any) { value["s3_url"] = "private" },
	} {
		t.Run("commit "+name, func(t *testing.T) {
			result := validCommittedAttachment()
			mutate(result)
			if _, err := adaptActionResultForRequestWithAuthority("agent.chat.attachment.commit", commitRequest, result, attachmentAuthority); !errors.Is(err, ErrInvalidActionResult) {
				t.Fatalf("drifted commit result accepted: %v", err)
			}
		})
	}
}
