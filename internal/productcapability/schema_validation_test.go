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
