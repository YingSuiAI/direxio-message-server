package serviceapi

func agentSessionCreateSchema() *ActionSchema {
	return &ActionSchema{
		Request: map[string]ActionFieldSchema{
			"session_id": {Type: "string", Presence: &ActionPresenceSchema{Omitted: "new_session", Present: "canonical_uuid;refresh_same_session"}},
		},
		Response: map[string]ActionFieldSchema{
			"ticket":     {Type: "string", Required: true, WriteOnly: true, Presence: &ActionPresenceSchema{Present: "short_lived_compact_eddsa_jws"}},
			"expires_at": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "rfc3339_utc"}},
			"base_path":  {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "/agent/v1"}},
			"session_id": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
			"scopes":     {Type: "array", Required: true, Items: &ActionFieldSchema{Type: "string"}},
		},
	}
}
