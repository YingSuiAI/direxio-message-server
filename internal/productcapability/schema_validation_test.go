package productcapability

import (
	"strings"
	"testing"

	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
)

func TestValidateOperationInputEnforcesSizeAndScalarTypes(t *testing.T) {
	operation := &capv1.OperationDescriptor{
		InputSchemaJson:     `{"type":"object","properties":{"room_id":{"type":"string"},"limit":{"type":"integer"}}}`,
		MaxRequestSizeBytes: 64,
	}
	if err := validateOperationInput(operation, []byte(`{"room_id":"!room:test","limit":10}`)); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
	if err := validateOperationInput(operation, []byte(`{"room_id":42}`)); err == nil {
		t.Fatal("invalid scalar type accepted")
	}
	if err := validateOperationInput(operation, []byte(`{"room_id":"`+strings.Repeat("x", 128)+`"}`)); err == nil {
		t.Fatal("oversized input accepted")
	}
}

func TestValidateOperationInputRejectsServerDerivedIdentity(t *testing.T) {
	operation := &capv1.OperationDescriptor{
		InputSchemaJson: `{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"number"}}}`,
	}
	for _, field := range []string{"owner_id", "owner_mxid", "authenticated_owner_id", "sender", "sender_mxid", "account_generation", " Owner_ID "} {
		t.Run(field, func(t *testing.T) {
			if err := validateOperationInput(operation, []byte(`{"`+field+`":"attacker"}`)); err == nil {
				t.Fatalf("server-derived identity field %q was accepted", field)
			}
		})
	}
	if err := validateOperationInput(operation, []byte(`{"query":"alice","limit":10}`)); err != nil {
		t.Fatalf("normal contacts input rejected: %v", err)
	}
}

func TestValidateOperationInputEnforcesRecordKindEnum(t *testing.T) {
	operation := &capv1.OperationDescriptor{InputSchemaJson: `{"type":"object","additionalProperties":false,"properties":{"record_kind":{"type":"string","enum":["cloud_worker"]}}}`}
	if err := validateOperationInput(operation, []byte(`{"record_kind":"cloud_worker"}`)); err != nil {
		t.Fatalf("cloud_worker record kind rejected: %v", err)
	}
	for _, raw := range []string{`{"record_kind":"legacy"}`, `{"record_kind":""}`, `{"record_kind":1}`} {
		if err := validateOperationInput(operation, []byte(raw)); err == nil {
			t.Fatalf("invalid record kind accepted: %s", raw)
		}
	}
	if err := validateOperationInput(operation, []byte(`{}`)); err != nil {
		t.Fatalf("optional record kind omission rejected: %v", err)
	}
}
