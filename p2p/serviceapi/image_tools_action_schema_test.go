package serviceapi

import "testing"

func TestImageToolActionsPublishClosedOwnerOnlyTypedContract(t *testing.T) {
	want := map[string]struct {
		request  []string
		response []string
	}{
		"agent.image_tools.upload.begin": {
			request:  []string{"idempotency_key", "image_request_id", "name", "mime_type", "declared_size", "content_sha256"},
			response: []string{"upload_id", "source_id", "image_request_id", "status", "received_size", "max_chunk_bytes", "revision", "expires_at"},
		},
		"agent.image_tools.upload.append": {
			request:  []string{"idempotency_key", "upload_id", "expected_revision", "ordinal", "offset_bytes", "data_base64", "chunk_sha256"},
			response: []string{"upload_id", "source_id", "image_request_id", "status", "received_size", "max_chunk_bytes", "revision", "expires_at"},
		},
		"agent.image_tools.upload.commit": {
			request:  []string{"idempotency_key", "upload_id", "expected_revision", "content_sha256"},
			response: []string{"source_id", "source_revision", "image_request_id", "name", "mime_type", "size_bytes", "sha256", "status", "expires_at"},
		},
		"agent.image_tools.extract_text": {
			request:  []string{"idempotency_key", "source_id", "source_revision"},
			response: []string{"idempotency_key", "source_id", "source_revision", "text"},
		},
		"agent.image_tools.translate_text": {
			request:  []string{"idempotency_key", "source_id", "source_revision", "target_locale"},
			response: []string{"idempotency_key", "source_id", "source_revision", "text", "target_locale"},
		},
	}
	for action, expected := range want {
		spec, ok := ActionSpecFor(action)
		if !ok || spec.Auth != ActionAuthOwner || spec.Transport != ActionTransportHTTPOnly || spec.Schema == nil {
			t.Fatalf("%s is not an owner HTTP/WS typed action: %#v", action, spec)
		}
		assertExactImageToolFields(t, action+" request", spec.Schema.Request, expected.request)
		assertExactImageToolFields(t, action+" response", spec.Schema.Response, expected.response)
	}
	appendSpec, _ := ActionSpecFor("agent.image_tools.upload.append")
	if !appendSpec.Schema.Request["data_base64"].WriteOnly {
		t.Fatal("image upload bytes must be write-only")
	}
}

func assertExactImageToolFields(t *testing.T, label string, fields map[string]ActionFieldSchema, expected []string) {
	t.Helper()
	if len(fields) != len(expected) {
		t.Fatalf("%s fields = %#v, want exactly %v", label, fields, expected)
	}
	for _, name := range expected {
		field, ok := fields[name]
		if !ok || !field.Required {
			t.Errorf("%s missing required field %s", label, name)
		}
	}
}
