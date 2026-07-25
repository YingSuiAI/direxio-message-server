package serviceapi

const ActionContractSchemaVersion = 1

type ActionContractDocument struct {
	SchemaVersion int                  `json:"schema_version"`
	GeneratedFrom string               `json:"generated_from"`
	Actions       []ActionContractSpec `json:"actions"`
}

type ActionContractSpec struct {
	Name      string        `json:"name"`
	Auth      string        `json:"auth"`
	Transport string        `json:"transport"`
	Schema    *ActionSchema `json:"schema,omitempty"`
}

// ActionSchema is intentionally additive: existing actions retain their
// compact metadata while actions with presence-sensitive fields can publish a
// machine-readable request/response shape.
type ActionSchema struct {
	Request  map[string]ActionFieldSchema `json:"request,omitempty"`
	Response map[string]ActionFieldSchema `json:"response,omitempty"`
}

type ActionFieldSchema struct {
	Type       string                       `json:"type"`
	Required   bool                         `json:"required,omitempty"`
	WriteOnly  bool                         `json:"write_only,omitempty"`
	Properties map[string]ActionFieldSchema `json:"properties,omitempty"`
	Items      *ActionFieldSchema           `json:"items,omitempty"`
	Presence   *ActionPresenceSchema        `json:"presence,omitempty"`
}

type ActionPresenceSchema struct {
	Omitted string `json:"omitted,omitempty"`
	Present string `json:"present,omitempty"`
	Empty   string `json:"empty,omitempty"`
}

func cloneActionSchema(schema *ActionSchema) *ActionSchema {
	if schema == nil {
		return nil
	}
	var cloneFields func(map[string]ActionFieldSchema) map[string]ActionFieldSchema
	cloneFields = func(fields map[string]ActionFieldSchema) map[string]ActionFieldSchema {
		if fields == nil {
			return nil
		}
		out := make(map[string]ActionFieldSchema, len(fields))
		for name, field := range fields {
			copyField := field
			if field.Properties != nil {
				copyField.Properties = make(map[string]ActionFieldSchema, len(field.Properties))
				for childName, child := range field.Properties {
					childCopy := child
					if child.Presence != nil {
						presence := *child.Presence
						childCopy.Presence = &presence
					}
					copyField.Properties[childName] = childCopy
				}
			}
			if field.Items != nil {
				item := *field.Items
				if field.Items.Presence != nil {
					presence := *field.Items.Presence
					item.Presence = &presence
				}
				if field.Items.Properties != nil {
					item.Properties = cloneFields(field.Items.Properties)
				}
				copyField.Items = &item
			}
			if field.Presence != nil {
				presence := *field.Presence
				copyField.Presence = &presence
			}
			out[name] = copyField
		}
		return out
	}
	return &ActionSchema{Request: cloneFields(schema.Request), Response: cloneFields(schema.Response)}
}

func cloneActionSpec(spec ActionSpec) ActionSpec {
	spec.Schema = cloneActionSchema(spec.Schema)
	return spec
}

func ActionContract() ActionContractDocument {
	specs := ActionSpecs()
	actions := make([]ActionContractSpec, 0, len(specs))
	for _, spec := range specs {
		actions = append(actions, ActionContractSpec{
			Name:      spec.Name,
			Auth:      string(spec.Auth),
			Transport: string(spec.Transport),
			Schema:    spec.Schema,
		})
	}
	return ActionContractDocument{
		SchemaVersion: ActionContractSchemaVersion,
		GeneratedFrom: "p2p/serviceapi.ActionSpecs",
		Actions:       actions,
	}
}
