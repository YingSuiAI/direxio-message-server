package serviceapi

import (
	"reflect"
	"strings"
	"time"

	agentdatav2 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/agent/data/v2"
)

func agentSessionCreateSchema() *ActionSchema {
	response := generatedAgentSessionResponseSchema()
	setResponsePresence(response, "ticket", "short_lived_compact_eddsa_jws")
	ticket := response["ticket"]
	ticket.WriteOnly = true
	response["ticket"] = ticket
	setResponsePresence(response, "expires_at", "rfc3339_utc")
	setResponsePresence(response, "server_time", "rfc3339_utc")
	setResponsePresence(response, "base_path", string(agentdatav2.AgentSessionResponseBasePathAgentv1))
	setResponsePresence(response, "session_id", "canonical_uuid")

	return &ActionSchema{
		Request: map[string]ActionFieldSchema{
			"session_id": {Type: "string", Presence: &ActionPresenceSchema{Omitted: "new_session", Present: "canonical_uuid;refresh_same_session"}},
		},
		Response: response,
	}
}

// generatedAgentSessionResponseSchema adapts the generated OpenAPI DTO into
// ProductCore's smaller action-metadata shape. ProductCore-only presence and
// write-only metadata is applied separately above; response fields, required
// status, and JSON types come from the shared contract type.
func generatedAgentSessionResponseSchema() map[string]ActionFieldSchema {
	responseType := reflect.TypeFor[agentdatav2.AgentSessionResponse]()
	fields := make(map[string]ActionFieldSchema, responseType.NumField())
	for index := range responseType.NumField() {
		field := responseType.Field(index)
		jsonName := strings.Split(field.Tag.Get("json"), ",")[0]
		if jsonName == "" || jsonName == "-" {
			panic("generated AgentSessionResponse contains an unaddressable JSON field")
		}
		fields[jsonName] = generatedActionField(field.Type)
	}
	return fields
}

func generatedActionField(fieldType reflect.Type) ActionFieldSchema {
	timeType := reflect.TypeFor[time.Time]()
	uuidType := reflect.TypeFor[agentdatav2.SessionId]()
	switch {
	case fieldType == timeType, fieldType == uuidType, fieldType.Kind() == reflect.String:
		return ActionFieldSchema{Type: "string", Required: true}
	case fieldType.Kind() == reflect.Slice && fieldType.Elem().Kind() == reflect.String:
		return ActionFieldSchema{Type: "array", Required: true, Items: &ActionFieldSchema{Type: "string"}}
	default:
		panic("generated AgentSessionResponse contains an unsupported ProductCore field type: " + fieldType.String())
	}
}

func setResponsePresence(fields map[string]ActionFieldSchema, name, present string) {
	field, ok := fields[name]
	if !ok {
		panic("generated AgentSessionResponse is missing required ProductCore field " + name)
	}
	field.Presence = &ActionPresenceSchema{Present: present}
	fields[name] = field
}
