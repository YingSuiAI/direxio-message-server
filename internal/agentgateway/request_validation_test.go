package agentgateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"testing"

	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
)

func TestValidateChatRequestRequiresImmutableProfilePins(t *testing.T) {
	base := map[string]any{
		"idempotency_key":        "11111111-1111-4111-8111-111111111111",
		"conversation_id":        "22222222-2222-4222-8222-222222222222",
		"message":                "hello",
		"model_profile_id":       "profile-id",
		"model_profile_revision": int64(2),
		"credential_version":     int64(3),
	}
	if err := ValidateActionRequest("agent.chat", base); err != nil {
		t.Fatalf("complete chat request rejected: %v", err)
	}
	jsonNumbers := cloneParams(base)
	jsonNumbers["model_profile_revision"] = json.Number("2")
	jsonNumbers["credential_version"] = json.Number("3")
	if err := ValidateActionRequest("agent.chat", jsonNumbers); err != nil {
		t.Fatalf("JSON-number chat profile pins rejected: %v", err)
	}
	for _, field := range []string{"idempotency_key", "conversation_id", "message", "model_profile_id", "model_profile_revision", "credential_version"} {
		params := cloneParams(base)
		delete(params, field)
		if err := ValidateActionRequest("agent.chat.stream", params); !errors.Is(err, ErrInvalidActionRequest) {
			t.Errorf("missing %s error = %v, want ErrInvalidActionRequest", field, err)
		}
	}
	for _, field := range []string{"model_profile", "tool_credentials", "default_profile", "client_model_profile_id", "messages", "history", "chat_history", "conversation_history"} {
		params := cloneParams(base)
		params[field] = map[string]any{"api_key": "secret"}
		err := ValidateActionRequest("agent.chat", params)
		if !errors.Is(err, ErrInvalidActionRequest) {
			t.Errorf("legacy %s error = %v, want ErrInvalidActionRequest", field, err)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("legacy %s error leaked a secret: %v", field, err)
		}
	}
	for _, value := range []any{0, -1, 1.5, "2", nil} {
		params := cloneParams(base)
		params["model_profile_revision"] = value
		if err := ValidateActionRequest("agent.chat", params); !errors.Is(err, ErrInvalidActionRequest) {
			t.Errorf("revision %#v accepted: %v", value, err)
		}
	}
}

func TestValidateChatAttachmentsUseCommittedIDsOnlyOnDurableStream(t *testing.T) {
	base := map[string]any{
		"idempotency_key":        "11111111-1111-4111-8111-111111111111",
		"conversation_id":        "22222222-2222-4222-8222-222222222222",
		"message":                "inspect the images",
		"model_profile_id":       "profile-id",
		"model_profile_revision": int64(2),
		"credential_version":     int64(3),
	}
	ids := []any{
		"11111111-1111-4111-8111-111111111111",
		"22222222-2222-4222-8222-222222222222",
	}
	stream := cloneParams(base)
	stream["accepted_attachment_ids"] = ids
	if err := ValidateActionRequest("agent.chat.stream", stream); err != nil {
		t.Fatalf("committed attachment IDs rejected: %v", err)
	}
	if err := ValidateActionRequest("agent.chat", stream); !errors.Is(err, ErrInvalidActionRequest) {
		t.Fatalf("unary chat accepted attachment IDs: %v", err)
	}

	for name, value := range map[string]any{
		"duplicate": []any{ids[0], ids[0]},
		"too many": []any{
			"11111111-1111-4111-8111-111111111111",
			"22222222-2222-4222-8222-222222222222",
			"33333333-3333-4333-8333-333333333333",
			"44444444-4444-4444-8444-444444444444",
			"55555555-5555-4555-8555-555555555555",
		},
		"invalid UUID": []any{"attachment-1"},
		"mixed type":   []any{ids[0], 2},
	} {
		t.Run(name, func(t *testing.T) {
			params := cloneParams(base)
			params["accepted_attachment_ids"] = value
			if err := ValidateActionRequest("agent.chat.stream", params); !errors.Is(err, ErrInvalidActionRequest) {
				t.Fatalf("invalid attachment IDs accepted: %v", err)
			}
		})
	}

	for _, field := range []string{"attachment", "attachments", "data_base64"} {
		params := cloneParams(base)
		params[field] = map[string]any{"data_base64": "private-image-canary"}
		err := ValidateActionRequest("agent.chat.stream", params)
		if !errors.Is(err, ErrInvalidActionRequest) {
			t.Errorf("raw %s payload accepted: %v", field, err)
		}
		if strings.Contains(err.Error(), "private-image-canary") {
			t.Errorf("raw %s error leaked payload bytes: %v", field, err)
		}
	}
}

func TestValidateChatStreamExtensionsRequireExactLocalBindings(t *testing.T) {
	base := map[string]any{
		"idempotency_key":        "11111111-1111-4111-8111-111111111111",
		"conversation_id":        "22222222-2222-4222-8222-222222222222",
		"message":                "run the light task locally",
		"model_profile_id":       "profile-id",
		"model_profile_revision": int64(2),
		"credential_version":     int64(3),
	}
	valid := func() map[string]any {
		return map[string]any{
			"kind": "mcp", "id": "33333333-3333-4333-8333-333333333333", "pinned_version": "1.2.3",
			"digest": strings.Repeat("a", 64), "allowed_tools": []any{"write_html"},
		}
	}
	stream := cloneParams(base)
	stream["extensions"] = []any{valid()}
	if err := ValidateActionRequest("agent.chat.stream", stream); err != nil {
		t.Fatalf("exact local extension rejected: %v", err)
	}
	if err := ValidateActionRequest("agent.chat", stream); !errors.Is(err, ErrInvalidActionRequest) {
		t.Fatalf("unary chat accepted extensions: %v", err)
	}

	tests := map[string]any{
		"empty selections": []any{},
		"non object":       []any{"mcp"},
		"duplicate install": []any{
			valid(), valid(),
		},
	}
	for name, extensions := range tests {
		t.Run(name, func(t *testing.T) {
			params := cloneParams(base)
			params["extensions"] = extensions
			if err := ValidateActionRequest("agent.chat.stream", params); !errors.Is(err, ErrInvalidActionRequest) {
				t.Fatalf("invalid extensions accepted: %v", err)
			}
		})
	}
	mutations := map[string]func(map[string]any){
		"unknown field":  func(v map[string]any) { v["extra"] = true },
		"invalid kind":   func(v map[string]any) { v["kind"] = "cloud_worker" },
		"invalid id":     func(v map[string]any) { v["id"] = "installation" },
		"unpinned":       func(v map[string]any) { v["pinned_version"] = " " },
		"invalid digest": func(v map[string]any) { v["digest"] = "sha256:x" },
		"empty tools":    func(v map[string]any) { v["allowed_tools"] = []any{} },
		"duplicate tool": func(v map[string]any) { v["allowed_tools"] = []any{"write_html", "write_html"} },
		"cloud intrinsic": func(v map[string]any) {
			v["allowed_tools"] = []any{"cloud_worker_propose"}
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			selection := valid()
			mutate(selection)
			params := cloneParams(base)
			params["extensions"] = []any{selection}
			if err := ValidateActionRequest("agent.chat.stream", params); !errors.Is(err, ErrInvalidActionRequest) {
				t.Fatalf("invalid selection accepted: %v", err)
			}
		})
	}
}

func TestValidateChatAttachmentUploadRequests(t *testing.T) {
	const (
		idempotencyID = "11111111-1111-4111-8111-111111111111"
		turnRequestID = "22222222-2222-4222-8222-222222222222"
		uploadID      = "33333333-3333-4333-8333-333333333333"
	)
	chunk := []byte("hello")
	chunkDigest := sha256.Sum256(chunk)
	digest := hex.EncodeToString(chunkDigest[:])
	encoded := base64.StdEncoding.EncodeToString(chunk)

	begin := map[string]any{
		"idempotency_key": idempotencyID, "turn_request_id": turnRequestID,
		"kind": "image", "name": "image.png", "mime_type": "image/png", "declared_size": len(chunk), "content_sha256": digest,
	}
	appendRequest := map[string]any{
		"idempotency_key": idempotencyID, "upload_id": uploadID, "expected_revision": 1,
		"ordinal": 0, "offset_bytes": 0, "data_base64": encoded, "chunk_sha256": digest,
	}
	commit := map[string]any{
		"idempotency_key": idempotencyID, "upload_id": uploadID,
		"expected_revision": 2, "content_sha256": digest,
	}
	for action, params := range map[string]map[string]any{
		"agent.chat.attachment.begin":  begin,
		"agent.chat.attachment.append": appendRequest,
		"agent.chat.attachment.commit": commit,
	} {
		if err := ValidateActionRequest(action, params); err != nil {
			t.Errorf("%s canonical request rejected: %v", action, err)
		}
	}

	for name, input := range map[string]map[string]any{
		"ordinary file": {
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
		t.Run("begin "+name, func(t *testing.T) {
			params := cloneParams(begin)
			for field, value := range input {
				params[field] = value
			}
			params["declared_size"] = maxChatAttachmentBytes
			if err := ValidateActionRequest("agent.chat.attachment.begin", params); err != nil {
				t.Fatalf("valid %s rejected: %v", name, err)
			}
		})
	}

	for name, value := range map[string]string{
		"empty":    "",
		"raw":      base64.RawStdEncoding.EncodeToString([]byte("h")),
		"URL safe": base64.URLEncoding.EncodeToString([]byte{0xfb, 0xff}),
		"oversize": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{'x'}, int(maxChatAttachmentChunkBytes)+1)),
	} {
		t.Run(name, func(t *testing.T) {
			params := cloneParams(appendRequest)
			params["data_base64"] = value
			err := ValidateActionRequest("agent.chat.attachment.append", params)
			if !errors.Is(err, ErrInvalidActionRequest) {
				t.Fatalf("non-canonical base64 accepted: %v", err)
			}
			if value != "" && strings.Contains(err.Error(), value) {
				t.Fatal("base64 payload was reflected in the validation error")
			}
		})
	}

	wrongDigest := cloneParams(appendRequest)
	wrongDigest["chunk_sha256"] = strings.Repeat("0", sha256.Size*2)
	if err := ValidateActionRequest("agent.chat.attachment.append", wrongDigest); !errors.Is(err, ErrInvalidActionRequest) {
		t.Fatalf("mismatched chunk digest accepted: %v", err)
	}
	for _, mutation := range []map[string]any{
		{"name": "../image.png"},
		{"kind": ""},
		{"mime_type": "application/octet-stream"},
		{"kind": "file", "mime_type": "image/png"},
		{"kind": "workspace_archive", "mime_type": "application/gzip"},
		{"declared_size": maxChatAttachmentBytes + 1},
		{"content_sha256": strings.ToUpper(digest)},
		{"extra": true},
	} {
		params := cloneParams(begin)
		for field, value := range mutation {
			params[field] = value
		}
		if err := ValidateActionRequest("agent.chat.attachment.begin", params); !errors.Is(err, ErrInvalidActionRequest) {
			t.Errorf("invalid begin mutation %#v accepted: %v", mutation, err)
		}
	}
}

func TestValidateCloudWorkerArtifactDownloadRequest(t *testing.T) {
	const artifactID = "9e728519-ea72-52cc-bb5a-8eb2860722b8"
	valid := map[string]any{
		"record_kind":     "cloud_worker",
		"artifact_id":     artifactID,
		"offset_bytes":    json.Number("0"),
		"max_chunk_bytes": json.Number("524288"),
	}
	if err := ValidateActionRequest("agent.execution.v2.artifacts.download", valid); err != nil {
		t.Fatalf("canonical artifact download request rejected: %v", err)
	}
	lastChunk := cloneParams(valid)
	lastChunk["offset_bytes"] = maxCloudWorkerArtifactBytes - 1
	lastChunk["max_chunk_bytes"] = int64(1)
	if err := ValidateActionRequest("agent.execution.v2.artifacts.download", lastChunk); err != nil {
		t.Fatalf("64 MiB artifact boundary rejected: %v", err)
	}

	for name, mutation := range map[string]map[string]any{
		"missing record kind": {"record_kind": nil},
		"generic route":       {"record_kind": "generic"},
		"bad artifact id":     {"artifact_id": "artifact-1"},
		"negative offset":     {"offset_bytes": -1},
		"oversize offset":     {"offset_bytes": maxCloudWorkerArtifactBytes},
		"fractional offset":   {"offset_bytes": 1.5},
		"zero chunk":          {"max_chunk_bytes": 0},
		"oversize chunk":      {"max_chunk_bytes": maxCloudWorkerArtifactChunkBytes + 1},
		"unknown field":       {"s3_url": "s3://private/internal"},
	} {
		t.Run(name, func(t *testing.T) {
			request := cloneParams(valid)
			for field, value := range mutation {
				if value == nil {
					delete(request, field)
				} else {
					request[field] = value
				}
			}
			err := ValidateActionRequest("agent.execution.v2.artifacts.download", request)
			if !errors.Is(err, ErrInvalidActionRequest) {
				t.Fatalf("invalid artifact download request accepted: %v", err)
			}
			if strings.Contains(err.Error(), "s3://private/internal") {
				t.Fatal("request validation reflected a private storage address")
			}
		})
	}
}

func TestValidateChatRequestRejectsNestedSensitiveKeysButNotPromptValues(t *testing.T) {
	base := map[string]any{
		"idempotency_key":        "11111111-1111-4111-8111-111111111111",
		"conversation_id":        "22222222-2222-4222-8222-222222222222",
		"message":                "A prompt may mention api_key, authorization, and password.",
		"model_profile_id":       "profile-id",
		"model_profile_revision": int64(2),
		"credential_version":     int64(3),
	}
	for _, test := range []struct {
		name string
		key  string
	}{
		{name: "tool credentials", key: "tool_credentials"},
		{name: "credentials plural", key: "credentials"},
		{name: "aws credentials", key: "aws_credentials"},
		{name: "aws credentials compact", key: "awsCredentials"},
		{name: "secrets plural", key: "secrets"},
		{name: "tokens plural", key: "tokens"},
		{name: "model profile", key: " model_profile "},
		{name: "model profile camel variant", key: "modelProfileId"},
		{name: "client profile", key: "CLIENT_MODEL_PROFILE_ID"},
		{name: "client profile camel variant", key: "clientModelProfileId"},
		{name: "credential version camel variant", key: "credentialVersion"},
		{name: "api key", key: " API_KEY "},
		{name: "api key compact", key: "ApiKey"},
		{name: "api keys plural", key: "api_keys"},
		{name: "authorization", key: "authorization"},
		{name: "request headers camel", key: "requestHeaders"},
		{name: "request headers compact", key: "requestheaders"},
		{name: "access token", key: "access_token"},
		{name: "bearer", key: "bearer"},
		{name: "bearer token camel", key: "bearerToken"},
		{name: "db pass camel", key: "dbPass"},
		{name: "user pass camel", key: "userPass"},
		{name: "pass exact", key: "pass"},
		{name: "db passes plural", key: "dbPasses"},
		{name: "http basic camel", key: "httpBasic"},
		{name: "http basic auth camel", key: "httpBasicAuth"},
		{name: "basic exact", key: "basic"},
		{name: "http basics plural", key: "httpBasics"},
		{name: "headers", key: "headers"},
		{name: "cookie", key: "cookie"},
		{name: "password", key: "password"},
		{name: "passwords plural", key: "passwords"},
		{name: "passwds plural", key: "passwds"},
		{name: "passphrases plural", key: "passphrases"},
		{name: "authorizations plural", key: "authorizations"},
		{name: "secret", key: "secret"},
		{name: "private key", key: "private_key"},
		{name: "messages", key: "messages"},
		{name: "history", key: "history"},
		{name: "chat history camel", key: "chatHistory"},
		{name: "conversation history", key: "conversation_history"},
	} {
		t.Run(test.name, func(t *testing.T) {
			params := cloneParams(base)
			params["metadata"] = []any{
				map[string]any{"label": "ordinary value", "nested": map[string]string{test.key: "secret-value"}},
			}
			err := ValidateActionRequest("agent.chat.stream", params)
			if !errors.Is(err, ErrInvalidActionRequest) {
				t.Fatalf("nested key %q accepted: %v", test.key, err)
			}
			if strings.Contains(err.Error(), "secret-value") {
				t.Fatalf("nested key %q error leaked its value: %v", test.key, err)
			}
		})
	}

	for _, value := range []string{
		"api_key",
		"authorization and password",
		"message may discuss a private_key without containing one",
	} {
		params := cloneParams(base)
		params["message"] = value
		if err := ValidateActionRequest("agent.chat", params); err != nil {
			t.Errorf("ordinary value %#v was rejected: %v", value, err)
		}
	}
}

func TestValidateChatRequestUsesOneClosedStartShape(t *testing.T) {
	valid := map[string]any{
		"idempotency_key":        "11111111-1111-4111-8111-111111111111",
		"conversation_id":        "22222222-2222-4222-8222-222222222222",
		"message":                "hello",
		"model_profile_id":       "profile-id",
		"model_profile_revision": int64(2),
		"credential_version":     int64(3),
		"after_seq":              json.Number("0"),
	}
	if err := ValidateActionRequest("agent.chat.stream", valid); err != nil {
		t.Fatalf("canonical stream request rejected: %v", err)
	}
	for _, field := range []string{"prompt", "turn_id", "client_message_id", "request_id", "operation_id"} {
		params := cloneParams(valid)
		params[field] = "33333333-3333-4333-8333-333333333333"
		if err := ValidateActionRequest("agent.chat.stream", params); !errors.Is(err, ErrInvalidActionRequest) {
			t.Errorf("unsupported start field %q accepted: %v", field, err)
		}
	}
	for _, sequence := range []any{-1, 1.5, "1"} {
		params := cloneParams(valid)
		params["after_seq"] = sequence
		if err := ValidateActionRequest("agent.chat.stream", params); !errors.Is(err, ErrInvalidActionRequest) {
			t.Errorf("invalid after_seq %#v accepted: %v", sequence, err)
		}
	}
}

func TestPositiveIntegerUsesExactInt64UpperBound(t *testing.T) {
	max := uint64(math.MaxInt64)
	for _, value := range []any{
		int64(math.MaxInt64),
		max,
		json.Number("2"),
		json.Number("2.0"),
		json.Number("9223372036854775807"),
		json.Number("9223372036854775807.0"),
	} {
		if !positiveInteger(value) {
			t.Errorf("valid positive integer %#v was rejected", value)
		}
	}

	for _, value := range []any{
		uint64(math.MaxInt64) + 1,
		uint(max) + 1,
		float64(math.MaxInt64),
		float32(math.MaxInt64),
		json.Number("9223372036854775808"),
		json.Number("9223372036854775808.0"),
		json.Number("1e100"),
	} {
		if positiveInteger(value) {
			t.Errorf("overflowing positive integer %#v was accepted", value)
		}
	}
}

func TestTurnControlRequestsRequireCanonicalClosedShapes(t *testing.T) {
	mutationID := "33333333-3333-4333-8333-333333333333"
	turnID := "11111111-1111-4111-8111-111111111111"
	conversationID := "22222222-2222-4222-8222-222222222222"
	if err := ValidateActionRequest("agent.chat.turn.stop", map[string]any{
		"idempotency_key":   mutationID,
		"turn_id":           turnID,
		"expected_revision": json.Number("2"),
	}); err != nil {
		t.Fatalf("canonical stop rejected: %v", err)
	}
	if err := ValidateActionRequest("agent.chat.turn.steer", map[string]any{
		"idempotency_key": mutationID, "turn_id": turnID,
		"expected_revision": json.Number("2"), "instruction": "answer the newest constraint first",
	}); err != nil {
		t.Fatalf("canonical steer rejected: %v", err)
	}
	if err := ValidateActionRequest("agent.chat.turns.list", map[string]any{"conversation_id": conversationID, "page_token": "", "limit": json.Number("20")}); err != nil {
		t.Fatalf("canonical list rejected: %v", err)
	}
	if err := ValidateActionRequest("agent.chat.turns.list", map[string]any{"conversation_id": conversationID, "limit": json.Number("1000")}); err != nil {
		t.Fatalf("maximum canonical list limit rejected: %v", err)
	}
	for _, test := range []struct {
		action string
		params map[string]any
	}{
		{"agent.chat.turn.stop", map[string]any{"turn_id": turnID, "expected_revision": 2}},
		{"agent.chat.turn.stop", map[string]any{"idempotency_key": mutationID, "turn_id": "turn-1", "expected_revision": 2}},
		{"agent.chat.turn.stop", map[string]any{"idempotency_key": mutationID, "turn_id": "00000000-0000-0000-0000-000000000000", "expected_revision": 2}},
		{"agent.chat.turn.stop", map[string]any{"idempotency_key": mutationID, "turn_id": turnID, "expected_revision": 0}},
		{"agent.chat.turn.stop", map[string]any{"idempotency_key": mutationID, "turn_id": turnID, "expected_revision": 2, "conversation_id": conversationID}},
		{"agent.chat.turn.steer", map[string]any{"idempotency_key": mutationID, "turn_id": turnID, "expected_revision": 2}},
		{"agent.chat.turn.steer", map[string]any{"idempotency_key": mutationID, "turn_id": turnID, "expected_revision": 2, "instruction": "   "}},
		{"agent.chat.turn.steer", map[string]any{"idempotency_key": mutationID, "turn_id": turnID, "expected_revision": 2, "instruction": "guide", "message": "alias"}},
		{"agent.chat.turns.list", map[string]any{"conversation_id": "conversation-1"}},
		{"agent.chat.turns.list", map[string]any{"conversation_id": conversationID, "next_cursor": "legacy"}},
		{"agent.chat.turns.list", map[string]any{"conversation_id": conversationID, "limit": 0}},
		{"agent.chat.turns.list", map[string]any{"conversation_id": conversationID, "limit": json.Number("1001")}},
	} {
		if err := ValidateActionRequest(test.action, test.params); !errors.Is(err, ErrInvalidActionRequest) {
			t.Errorf("%s %#v error=%v, want ErrInvalidActionRequest", test.action, test.params, err)
		}
	}
}

func TestValidateModelCatalogRequestRejectsInvalidKindAndCredentialMix(t *testing.T) {
	for _, kind := range []any{"", "vision", 1, nil} {
		err := ValidateActionRequest("agent.models.list", map[string]any{"model_kind": kind})
		if !errors.Is(err, ErrInvalidActionRequest) {
			t.Errorf("model_kind %#v accepted: %v", kind, err)
		}
	}
	for _, params := range []map[string]any{
		{"model_profile_id": "profile", "api_key": "secret"},
		{"client_model_profile_id": "profile", "api_key": "secret"},
	} {
		err := ValidateActionRequest("agent.models.list", params)
		if !errors.Is(err, ErrInvalidActionRequest) {
			t.Errorf("profile/API key mix accepted: %v", err)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("profile/API key error leaked a secret: %v", err)
		}
	}
	if err := ValidateActionRequest("agent.models.list", map[string]any{"model_kind": "speech", "api_key": "secret"}); err != nil {
		t.Fatalf("valid speech catalog request rejected: %v", err)
	}
}

func TestCapabilityHTTPStatusUsesStructuredErrorCode(t *testing.T) {
	cases := map[capv1.ErrorCode]int{
		capv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT:    http.StatusBadRequest,
		capv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED:   http.StatusForbidden,
		capv1.ErrorCode_ERROR_CODE_NOT_FOUND:           http.StatusNotFound,
		capv1.ErrorCode_ERROR_CODE_CONFLICT:            http.StatusConflict,
		capv1.ErrorCode_ERROR_CODE_PRECONDITION_FAILED: http.StatusPreconditionFailed,
		capv1.ErrorCode_ERROR_CODE_NOT_READY:           http.StatusServiceUnavailable,
		capv1.ErrorCode_ERROR_CODE_UNAVAILABLE:         http.StatusServiceUnavailable,
		capv1.ErrorCode_ERROR_CODE_UNCERTAIN:           http.StatusConflict,
		capv1.ErrorCode_ERROR_CODE_UPSTREAM_FAILED:     http.StatusBadGateway,
	}
	for code, want := range cases {
		if got := CapabilityHTTPStatus(code); got != want {
			t.Errorf("CapabilityHTTPStatus(%s) = %d, want %d", code, got, want)
		}
	}
	secret := "provider-secret-canary"
	if got := (&CapabilityError{Code: capv1.ErrorCode_ERROR_CODE_UPSTREAM_FAILED}).Error(); strings.Contains(got, secret) {
		t.Fatalf("structured capability error leaked %q: %q", secret, got)
	}
}

func TestValidateCoreMCPManagedNodeContract(t *testing.T) {
	if err := ValidateActionRequest("agent.core.mcp.discover", map[string]any{}); err != nil {
		t.Fatalf("default official registry discovery rejected: %v", err)
	}
	if err := ValidateActionRequest("agent.core.mcp.discover", map[string]any{"source": "npm", "query": "filesystem", "page_size": int64(10)}); err != nil {
		t.Fatalf("npm discovery rejected: %v", err)
	}
	valid := validManagedNodeMCPMutation()
	if err := ValidateActionRequest("agent.core.mcp.install", valid); err != nil {
		t.Fatalf("managed Node MCP install rejected: %v", err)
	}
	if err := ValidateActionRequest("agent.core.mcp.inspect", map[string]any{"candidate": valid["candidate"]}); err != nil {
		t.Fatalf("managed Node MCP inspection request rejected: %v", err)
	}
	update := validManagedNodeMCPMutation()
	update["installation_id"] = "22222222-2222-4222-8222-222222222222"
	update["expected_revision"] = int64(4)
	if err := ValidateActionRequest("agent.core.mcp.update", update); err != nil {
		t.Fatalf("managed Node MCP update rejected: %v", err)
	}
	if err := ValidateActionRequest("agent.core.mcp.remove", map[string]any{
		"idempotency_key":   "11111111-1111-4111-8111-111111111111",
		"installation_id":   "22222222-2222-4222-8222-222222222222",
		"expected_revision": int64(5),
	}); err != nil {
		t.Fatalf("managed MCP removal rejected: %v", err)
	}

	invalid := map[string]func(map[string]any){
		"floating npm tag": func(value map[string]any) {
			value["candidate"].(map[string]any)["pin"].(map[string]any)["registry_version"] = "latest"
			value["inspection"].(map[string]any)["candidate"] = value["candidate"]
		},
		"numeric prerelease leading zero": func(value map[string]any) {
			value["candidate"].(map[string]any)["pin"].(map[string]any)["registry_version"] = "1.2.3-01"
			value["inspection"].(map[string]any)["candidate"] = value["candidate"]
		},
		"wrong transport": func(value map[string]any) {
			value["candidate"].(map[string]any)["transport"] = "stdio_static"
			value["inspection"].(map[string]any)["candidate"] = value["candidate"]
		},
		"missing node runtime": func(value map[string]any) {
			delete(value["inspection"].(map[string]any)["execution"].(map[string]any)["stdio"].(map[string]any), "runtime")
		},
		"network grant": func(value map[string]any) {
			value["inspection"].(map[string]any)["network_grants"] = []any{map[string]any{"host": "example.com"}}
		},
		"client secret": func(value map[string]any) {
			value["secret_inputs"] = []any{map[string]any{"secret_value": "must-not-leak"}}
		},
		"client node receipt": func(value map[string]any) {
			value["inspection"].(map[string]any)["node_artifact"] = map[string]any{}
		},
		"install revision": func(value map[string]any) { value["expected_revision"] = int64(1) },
	}
	for name, mutate := range invalid {
		t.Run(name, func(t *testing.T) {
			value := validManagedNodeMCPMutation()
			mutate(value)
			err := ValidateActionRequest("agent.core.mcp.install", value)
			if !errors.Is(err, ErrInvalidActionRequest) {
				t.Fatalf("invalid managed Node MCP request error = %v, want ErrInvalidActionRequest", err)
			}
			if strings.Contains(err.Error(), "must-not-leak") {
				t.Fatalf("request validation leaked secret material: %v", err)
			}
		})
	}
}

func TestValidateCoreExtensionFamiliesStayDisjoint(t *testing.T) {
	if err := ValidateActionRequest("agent.core.skills.discover", map[string]any{}); err != nil {
		t.Fatalf("default built-in Skill discovery rejected: %v", err)
	}
	if err := ValidateActionRequest("agent.core.skills.discover", map[string]any{"source": "skills_sh", "query": "frontend"}); err != nil {
		t.Fatalf("skills.sh discovery rejected: %v", err)
	}
	for action, params := range map[string]map[string]any{
		"agent.core.mcp.discover":    {"source": "builtin"},
		"agent.core.skills.discover": {"source": "npm", "query": "calculator"},
		"agent.core.mcp.list":        {"source": "skills_sh"},
		"agent.core.skills.list":     {"source": "official_registry"},
		"agent.core.mcp.get":         {"installation_id": "not-a-uuid"},
		"agent.core.skills.get":      {},
	} {
		if err := ValidateActionRequest(action, params); !errors.Is(err, ErrInvalidActionRequest) {
			t.Errorf("cross-family %s request error = %v, want ErrInvalidActionRequest", action, err)
		}
	}

	valid := validManagedSkillMutation()
	if err := ValidateActionRequest("agent.core.skills.inspect", map[string]any{"candidate": valid["candidate"]}); err != nil {
		t.Fatalf("Skill inspection request rejected: %v", err)
	}
	if err := ValidateActionRequest("agent.core.skills.install", valid); err != nil {
		t.Fatalf("Skill install rejected: %v", err)
	}
	update := validManagedSkillMutation()
	update["installation_id"] = "22222222-2222-4222-8222-222222222222"
	update["expected_revision"] = int64(2)
	if err := ValidateActionRequest("agent.core.skills.update", update); err != nil {
		t.Fatalf("Skill update rejected: %v", err)
	}
	if err := ValidateActionRequest("agent.core.skills.remove", map[string]any{
		"idempotency_key":   "11111111-1111-4111-8111-111111111111",
		"installation_id":   "22222222-2222-4222-8222-222222222222",
		"expected_revision": int64(3),
	}); err != nil {
		t.Fatalf("Skill removal rejected: %v", err)
	}

	mcp := validManagedNodeMCPMutation()["candidate"]
	if err := ValidateActionRequest("agent.core.skills.inspect", map[string]any{"candidate": mcp}); !errors.Is(err, ErrInvalidActionRequest) {
		t.Fatalf("MCP candidate crossed into Skills: %v", err)
	}
	skill := valid["candidate"]
	if err := ValidateActionRequest("agent.core.mcp.inspect", map[string]any{"candidate": skill}); !errors.Is(err, ErrInvalidActionRequest) {
		t.Fatalf("Skill candidate crossed into MCP: %v", err)
	}

	invalid := map[string]func(map[string]any){
		"MCP source": func(value map[string]any) {
			value["candidate"].(map[string]any)["source"] = "npm"
			value["candidate"].(map[string]any)["pin"] = map[string]any{"registry_version": "1.0.0", "registry_sha256": strings.Repeat("a", 64)}
			value["inspection"].(map[string]any)["candidate"] = value["candidate"]
		},
		"MCP transport": func(value map[string]any) {
			value["candidate"].(map[string]any)["transport"] = "stdio_node"
			value["inspection"].(map[string]any)["candidate"] = value["candidate"]
		},
		"MCP execution": func(value map[string]any) {
			value["inspection"].(map[string]any)["execution"] = map[string]any{"stdio": map[string]any{"relative_path": "entry", "digest": strings.Repeat("a", 64), "argv": []any{"entry"}}}
		},
		"invalid executable Skill": func(value map[string]any) {
			value["inspection"].(map[string]any)["execution"].(map[string]any)["skill"].(map[string]any)["executable"] = true
		},
		"missing network grants": func(value map[string]any) {
			delete(value["inspection"].(map[string]any), "network_grants")
		},
		"missing secret grants": func(value map[string]any) {
			delete(value["inspection"].(map[string]any), "secret_grants")
		},
	}
	for name, mutate := range invalid {
		t.Run(name, func(t *testing.T) {
			value := validManagedSkillMutation()
			mutate(value)
			if err := ValidateActionRequest("agent.core.skills.install", value); !errors.Is(err, ErrInvalidActionRequest) {
				t.Fatalf("invalid Skill request error = %v, want ErrInvalidActionRequest", err)
			}
		})
	}
}

func validManagedSkillMutation() map[string]any {
	digest := strings.Repeat("a", sha256.Size*2)
	candidate := map[string]any{
		"id": "frontend-design", "kind": "skill", "source": "builtin", "name": "Frontend Design",
		"pin": map[string]any{"registry_version": "1.0.0", "registry_sha256": digest}, "transport": "skill_static",
	}
	inspection := map[string]any{
		"candidate": candidate, "content_digest": digest, "manifest_digest": digest, "execution_digest": digest,
		"network_schema_digest": digest, "secret_schema_digest": digest,
		"execution":      map[string]any{"skill": map[string]any{"relative_path": "SKILL.md", "digest": digest}},
		"network_grants": []any{}, "secret_grants": []any{},
	}
	return map[string]any{
		"idempotency_key": "11111111-1111-4111-8111-111111111111",
		"candidate":       candidate, "inspection": inspection, "secret_inputs": []any{},
	}
}

func validManagedNodeMCPMutation() map[string]any {
	digest := strings.Repeat("a", sha256.Size*2)
	candidate := map[string]any{
		"id": "@modelcontextprotocol/server-filesystem", "kind": "mcp", "source": "npm", "name": "Filesystem MCP",
		"pin": map[string]any{"registry_version": "1.2.3", "registry_sha256": digest}, "transport": "stdio_node",
	}
	inspection := map[string]any{
		"candidate": candidate, "content_digest": digest, "manifest_digest": digest, "execution_digest": digest,
		"network_schema_digest": digest, "secret_schema_digest": digest,
		"execution": map[string]any{"stdio": map[string]any{
			"relative_path": "node_modules/@modelcontextprotocol/server-filesystem/dist/index.js", "digest": digest, "argv": []any{"--stdio"}, "runtime": "node",
		}},
		"network_grants": []any{}, "secret_grants": []any{},
	}
	return map[string]any{
		"idempotency_key": "11111111-1111-4111-8111-111111111111",
		"candidate":       candidate, "inspection": inspection, "secret_inputs": []any{},
	}
}

func TestCapabilityErrorFromProtoPreservesOnlySafeClientCodes(t *testing.T) {
	tests := []struct {
		name, code, message string
		capabilityCode      capv1.ErrorCode
	}{
		{"knowledge quota", KnowledgeQuotaExceededCode, "knowledge quota exceeded", capv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED},
		{"install busy", ExtensionInstallBusyCode, "another extension installation is already in progress", capv1.ErrorCode_ERROR_CODE_PRECONDITION_FAILED},
		{"installation limit", ExtensionInstallationLimitCode, "extension installation limit reached", capv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED},
		{"Node storage quota", ExtensionNodeStorageQuotaCode, "extension Node storage quota exceeded", capv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := capabilityErrorFromProto(&capv1.CapabilityError{Code: test.capabilityCode, Message: "raw private sentinel", Details: map[string]string{"code": test.code, "secret": "drop"}})
			var capabilityErr *CapabilityError
			if !errors.As(err, &capabilityErr) || capabilityErr.ClientCode != test.code || capabilityErr.Error() != test.message {
				t.Fatalf("safe capability error = %#v", err)
			}
			if strings.Contains(capabilityErr.Error(), "sentinel") || strings.Contains(capabilityErr.Error(), "drop") {
				t.Fatalf("capability error leaked protected detail: %q", capabilityErr.Error())
			}
		})
	}
	for _, value := range []*capv1.CapabilityError{
		{Code: capv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED, Details: map[string]string{"code": "untrusted_code"}},
		{Code: capv1.ErrorCode_ERROR_CODE_PRECONDITION_FAILED, Details: map[string]string{"code": ExtensionInstallationLimitCode}},
		{Code: capv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED, Details: map[string]string{"code": ExtensionInstallBusyCode}},
	} {
		other := capabilityErrorFromProto(value)
		var capabilityErr *CapabilityError
		if !errors.As(other, &capabilityErr) || capabilityErr.ClientCode != "" {
			t.Fatalf("untrusted or mismatched client code escaped Agent boundary: %#v", other)
		}
	}
}
