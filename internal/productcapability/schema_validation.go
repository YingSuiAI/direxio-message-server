package productcapability

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
)

// validateOperationInput enforces the bounded JSON object contract advertised
// by each descriptor. It intentionally implements the small JSON-Schema
// subset used by Product capabilities (object/properties/required and scalar
// types) without adding a second schema engine to the message-server.
func validateOperationInput(operation *capv1.OperationDescriptor, raw []byte) error {
	if operation == nil {
		return fmt.Errorf("operation descriptor is required")
	}
	max := operation.GetMaxRequestSizeBytes()
	if max <= 0 {
		max = 1 << 20
	}
	if int64(len(raw)) > max {
		return fmt.Errorf("request exceeds operation size limit")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte(`{}`)
	}
	var input map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&input); err != nil || input == nil {
		return fmt.Errorf("request_json must be a JSON object")
	}
	if err := rejectForgedIdentity(input); err != nil {
		return err
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(operation.GetInputSchemaJson()), &schema); err != nil {
		return fmt.Errorf("operation input schema is invalid")
	}
	if required, ok := schema["required"].([]any); ok {
		for _, value := range required {
			name, _ := value.(string)
			if strings.TrimSpace(name) != "" {
				if _, present := input[name]; !present {
					return fmt.Errorf("required field %q is missing", name)
				}
			}
		}
	}
	properties, _ := schema["properties"].(map[string]any)
	for name, value := range input {
		property, ok := properties[name].(map[string]any)
		if !ok {
			continue
		}
		if expected, _ := property["type"].(string); expected != "" && !jsonSchemaTypeMatches(expected, value) {
			return fmt.Errorf("field %q has invalid type", name)
		}
	}
	return nil
}

func rejectForgedIdentity(params map[string]any) error {
	for key := range params {
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "owner_id", "owner_mxid", "authenticated_owner_id", "sender", "sender_mxid", "account_generation":
			return fmt.Errorf("request field %q is server-derived", key)
		}
	}
	return nil
}

func jsonSchemaTypeMatches(expected string, value any) bool {
	switch expected {
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "number", "integer":
		number, ok := value.(json.Number)
		if !ok {
			return false
		}
		if expected == "integer" {
			_, err := number.Int64()
			return err == nil
		}
		_, err := number.Float64()
		return err == nil
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "null":
		return value == nil
	default:
		return true
	}
}
