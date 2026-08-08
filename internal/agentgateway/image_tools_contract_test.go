package agentgateway

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const (
	imageToolRequestID = "11111111-1111-4111-8111-111111111111"
	imageToolMutation  = "22222222-2222-4222-8222-222222222222"
	imageToolUploadID  = "33333333-3333-4333-8333-333333333333"
	imageToolSourceID  = "44444444-4444-4444-8444-444444444444"
	imageToolDigest    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

var imageToolAuthority = actionResultAuthority{ownerID: "@owner:example.test", accountGeneration: 7}

func TestImageToolRequestsRejectAliasesSecretsAndInvalidPayloads(t *testing.T) {
	begin := map[string]any{
		"idempotency_key": imageToolMutation, "image_request_id": imageToolRequestID,
		"name": "photo.png", "mime_type": "image/png", "declared_size": json.Number("2"), "content_sha256": imageToolDigest,
	}
	chunk := []byte("ok")
	digest := sha256.Sum256(chunk)
	appendRequest := map[string]any{
		"idempotency_key": imageToolMutation, "upload_id": imageToolUploadID, "expected_revision": json.Number("1"),
		"ordinal": json.Number("0"), "offset_bytes": json.Number("0"), "data_base64": base64.StdEncoding.EncodeToString(chunk),
		"chunk_sha256": hex.EncodeToString(digest[:]),
	}
	commit := map[string]any{
		"idempotency_key": imageToolMutation, "upload_id": imageToolUploadID,
		"expected_revision": json.Number("2"), "content_sha256": imageToolDigest,
	}
	extract := map[string]any{
		"idempotency_key": imageToolMutation, "source_id": imageToolSourceID, "source_revision": json.Number("1"),
	}
	translate := cloneParams(extract)
	translate["target_locale"] = "zh-Hans"
	valid := map[string]map[string]any{
		"agent.image_tools.upload.begin": begin, "agent.image_tools.upload.append": appendRequest,
		"agent.image_tools.upload.commit": commit, "agent.image_tools.extract_text": extract,
		"agent.image_tools.translate_text": translate,
	}
	for action, params := range valid {
		if err := ValidateActionRequest(action, params); err != nil {
			t.Errorf("valid %s request rejected: %v", action, err)
		}
	}

	invalid := []struct {
		action string
		params map[string]any
	}{
		{"agent.image_tools.upload.begin", withImageToolField(begin, "url", "https://example.test/image.png")},
		{"agent.image_tools.upload.begin", withImageToolField(begin, "mxc", "mxc://example.test/id")},
		{"agent.image_tools.upload.begin", withImageToolField(begin, "path", "/tmp/image.png")},
		{"agent.image_tools.upload.begin", withImageToolField(begin, "data_uri", "data:image/png;base64,eA==")},
		{"agent.image_tools.upload.begin", withImageToolField(begin, "api_key", "must-not-cross")},
		{"agent.image_tools.upload.begin", withImageToolField(begin, "mime_type", "image/gif")},
		{"agent.image_tools.upload.begin", withImageToolField(begin, "declared_size", json.Number("8388609"))},
		{"agent.image_tools.upload.append", withImageToolField(appendRequest, "data_base64", "b2s")},
		{"agent.image_tools.upload.append", withImageToolField(appendRequest, "chunk_sha256", imageToolDigest)},
		{"agent.image_tools.upload.commit", withImageToolField(commit, "content_sha256", strings.ToUpper(imageToolDigest))},
		{"agent.image_tools.extract_text", withImageToolField(extract, "selected_text", "forbidden")},
		{"agent.image_tools.extract_text", withImageToolField(extract, "source_revision", json.Number("2"))},
		{"agent.image_tools.translate_text", withImageToolField(translate, "target_locale", "zh_CN")},
		{"agent.image_tools.translate_text", withImageToolField(translate, "target_locale", "en-us")},
		{"agent.image_tools.translate_text", withImageToolField(translate, "model_profile_id", "forbidden")},
	}
	oversized := make([]byte, maxChatAttachmentChunkBytes+1)
	oversizedDigest := sha256.Sum256(oversized)
	oversizedAppend := cloneParams(appendRequest)
	oversizedAppend["data_base64"] = base64.StdEncoding.EncodeToString(oversized)
	oversizedAppend["chunk_sha256"] = hex.EncodeToString(oversizedDigest[:])
	invalid = append(invalid, struct {
		action string
		params map[string]any
	}{"agent.image_tools.upload.append", oversizedAppend})
	for _, test := range invalid {
		if err := ValidateActionRequest(test.action, test.params); !errors.Is(err, ErrInvalidActionRequest) {
			t.Errorf("invalid %s request error = %v", test.action, err)
		}
	}
}

func TestImageToolBindingsPinCanonicalAgentSchemas(t *testing.T) {
	want := map[string]struct {
		operation string
		input     string
		result    string
	}{
		"agent.image_tools.upload.begin":   {"upload_begin", "2797dbc61371d343647866a9116151065eb633838f917d584c6394f237ea2b5a", "5c1795955a5b5cd63c390c11fa24654389a9c42bea108584831eb7b0fa4f7bc7"},
		"agent.image_tools.upload.append":  {"upload_append", "f3c65322ece7aca25c93e12046b4919aa6b942e5e252b3802ba86184898e013e", "5c1795955a5b5cd63c390c11fa24654389a9c42bea108584831eb7b0fa4f7bc7"},
		"agent.image_tools.upload.commit":  {"upload_commit", "1f5a339393a28aaa60a08d4ba3d8e43d4320eef387d59f3524a4fb598320c875", "2012c595a14cb1a5630822bbf4a033278d10dafccc97254c61ab7375ae21ce11"},
		"agent.image_tools.extract_text":   {"extract_text", "475db24a1d6efb6ddeceeeb5f3bba9214bfa745b7046d65c61e7bb62fc2e7c8f", "a9ae62acacbb5f5417461e9d46dde99eff836ccbabdd8e71c3d7b356a7ef829e"},
		"agent.image_tools.translate_text": {"translate_text", "695285a75e6b3c2d0706d01b50e7b3d57b072599efe362a48cb192ec44735a8d", "b17197b1c57c81abf31fbb19a815d47f2af52d25236d999fb13d5553a2c34599"},
	}
	for action, expected := range want {
		binding, ok := actionBindingFor(action)
		if !ok || binding.capabilityID != "agent.image_tools.v1" || binding.operation != expected.operation {
			t.Errorf("%s binding = %#v", action, binding)
		}
		requirement := NewCatalogRequirement(action)
		if !requirement.RequireSchemaPin || hex.EncodeToString(requirement.InputSchemaDigest) != expected.input || hex.EncodeToString(requirement.ResultSchemaDigest) != expected.result {
			t.Errorf("%s schema pins = input %x result %x", action, requirement.InputSchemaDigest, requirement.ResultSchemaDigest)
		}
	}
}

func TestImageToolResultsAreExactOwnerBoundAndIdentityPinned(t *testing.T) {
	beginRequest := map[string]any{"image_request_id": imageToolRequestID}
	upload := validImageToolUploadProjection()
	if got, err := adaptActionResultForRequestWithAuthority("agent.image_tools.upload.begin", beginRequest, upload, imageToolAuthority); err != nil || got["upload_id"] != imageToolUploadID {
		t.Fatalf("valid begin result = %#v, %v", got, err)
	}
	replayed := cloneParams(upload)
	replayed["status"] = "consumed"
	replayed["revision"] = float64(4)
	replayed["received_size"] = float64(8)
	if _, err := adaptActionResultForRequestWithAuthority("agent.image_tools.upload.begin", beginRequest, replayed, imageToolAuthority); err != nil {
		t.Fatalf("valid replay projection rejected: %v", err)
	}
	appendRequest := map[string]any{"upload_id": imageToolUploadID}
	if _, err := adaptActionResultForRequestWithAuthority("agent.image_tools.upload.append", appendRequest, upload, imageToolAuthority); err != nil {
		t.Fatalf("valid append result rejected: %v", err)
	}
	commitRequest := map[string]any{"content_sha256": imageToolDigest}
	commit := map[string]any{
		"source_id": imageToolSourceID, "source_revision": float64(1), "image_request_id": imageToolRequestID,
		"name": "photo.png", "mime_type": "image/png", "size_bytes": float64(2), "sha256": imageToolDigest,
		"status": "committed", "expires_at": "2026-08-08T10:30:00Z",
	}
	if _, err := adaptActionResultForRequestWithAuthority("agent.image_tools.upload.commit", commitRequest, commit, imageToolAuthority); err != nil {
		t.Fatalf("valid commit result rejected: %v", err)
	}
	executeRequest := map[string]any{
		"idempotency_key": imageToolMutation, "source_id": imageToolSourceID, "source_revision": json.Number("1"),
	}
	extract := map[string]any{
		"idempotency_key": imageToolMutation, "source_id": imageToolSourceID, "source_revision": float64(1), "text": "",
	}
	if _, err := adaptActionResultForRequestWithAuthority("agent.image_tools.extract_text", executeRequest, extract, imageToolAuthority); err != nil {
		t.Fatalf("empty extraction result rejected: %v", err)
	}
	translateRequest := cloneParams(executeRequest)
	translateRequest["target_locale"] = "zh-Hans"
	translate := cloneParams(extract)
	translate["target_locale"] = "zh-Hans"
	translate["text"] = "文字"
	if _, err := adaptActionResultForRequestWithAuthority("agent.image_tools.translate_text", translateRequest, translate, imageToolAuthority); err != nil {
		t.Fatalf("valid translation result rejected: %v", err)
	}

	invalid := []struct {
		action  string
		request map[string]any
		result  map[string]any
	}{
		{"agent.image_tools.upload.begin", beginRequest, withImageToolField(upload, "image_request_id", imageToolSourceID)},
		{"agent.image_tools.upload.append", appendRequest, withImageToolField(upload, "upload_id", imageToolSourceID)},
		{"agent.image_tools.upload.commit", commitRequest, withImageToolField(commit, "sha256", strings.Repeat("b", 64))},
		{"agent.image_tools.extract_text", executeRequest, withImageToolField(extract, "source_id", imageToolUploadID)},
		{"agent.image_tools.extract_text", executeRequest, withImageToolField(extract, "api_key", "must-not-leak")},
		{"agent.image_tools.extract_text", executeRequest, withImageToolField(extract, "text", strings.Repeat("x", maxTextToolOutputBytes+1))},
		{"agent.image_tools.translate_text", translateRequest, withImageToolField(translate, "target_locale", "en-US")},
	}
	for _, test := range invalid {
		if _, err := adaptActionResultForRequestWithAuthority(test.action, test.request, test.result, imageToolAuthority); !errors.Is(err, ErrInvalidActionResult) {
			t.Errorf("invalid %s result error = %v", test.action, err)
		}
	}
	if _, err := adaptActionResultForRequestWithAuthority("agent.image_tools.extract_text", executeRequest, extract, actionResultAuthority{}); !errors.Is(err, ErrInvalidActionResult) {
		t.Fatalf("missing owner authority error = %v", err)
	}
}

func validImageToolUploadProjection() map[string]any {
	return map[string]any{
		"upload_id": imageToolUploadID, "source_id": imageToolSourceID, "image_request_id": imageToolRequestID,
		"status": "receiving", "received_size": float64(0), "max_chunk_bytes": float64(maxChatAttachmentChunkBytes),
		"revision": float64(1), "expires_at": "2026-08-08T10:30:00Z",
	}
}

func withImageToolField(source map[string]any, key string, value any) map[string]any {
	result := cloneParams(source)
	result[key] = value
	return result
}
