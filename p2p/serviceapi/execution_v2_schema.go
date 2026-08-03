package serviceapi

func executionV2Name(name string) string { return "agent.execution.v2." + name }

func executionV2Field(kind string, required bool) ActionFieldSchema {
	return ActionFieldSchema{Type: kind, Required: required}
}

func executionV2PageRequest() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		"page_size":  {Type: "integer", Presence: &ActionPresenceSchema{Omitted: "server_default_100", Present: "integer_1_to_200"}},
		"page_token": {Type: "string", Presence: &ActionPresenceSchema{Omitted: "first_page"}},
	}
}

func executionV2PageResponse(item string) map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		item:              {Type: "array", Required: true, Items: &ActionFieldSchema{Type: "object"}},
		"next_page_token": {Type: "string", Required: true},
	}
}

func executionV2OwnedObject(name string) map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{name: {
		Type:       "object",
		Required:   true,
		Properties: map[string]ActionFieldSchema{"owner_id": {Type: "string", Required: true}},
	}}
}

func executionV2OwnedPageResponse(item string) map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		item: {
			Type:     "array",
			Required: true,
			Items: &ActionFieldSchema{
				Type:       "object",
				Properties: map[string]ActionFieldSchema{"owner_id": {Type: "string", Required: true}},
			},
		},
		"next_page_token": {Type: "string", Required: true},
	}
}

func executionV2MutationBase() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		"idempotency_key": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
	}
}

func executionV2RevisionMutationBase() map[string]ActionFieldSchema {
	fields := executionV2MutationBase()
	fields["expected_revision"] = ActionFieldSchema{Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer"}}
	return fields
}

func executionV2EventsRequest(id string) map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		id:               {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}},
		"after_sequence": {Type: "integer", Presence: &ActionPresenceSchema{Omitted: "from_beginning", Present: "nonnegative_integer"}},
		"limit":          {Type: "integer", Presence: &ActionPresenceSchema{Omitted: "server_default_100", Present: "integer_1_to_200"}},
	}
}

func executionV2EventsResponse() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{
		"events":        {Type: "array", Required: true, Items: &ActionFieldSchema{Type: "object"}},
		"next_sequence": {Type: "integer", Required: true},
	}
}

func executionV2PlanSelection(fields map[string]ActionFieldSchema) map[string]ActionFieldSchema {
	fields["intent"] = ActionFieldSchema{Type: "string", Required: true}
	fields["recipe_id"] = ActionFieldSchema{Type: "string", Required: true}
	fields["target_id"] = ActionFieldSchema{Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}}
	fields["target_revision"] = ActionFieldSchema{Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer"}}
	fields["purpose"] = ActionFieldSchema{Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "one_of:service|job"}}
	fields["ai_configuration"] = ActionFieldSchema{Type: "object", Properties: map[string]ActionFieldSchema{
		"mode": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "one_of:api_key|auth_gate"}}, "provider": {Type: "string", Required: true},
		"secret_ref": {Type: "string"}, "secret_revision": {Type: "integer"}, "secret_purpose": {Type: "string"},
		"secret_binding_digest": {Type: "string"}, "status": {Type: "string"},
	}}
	return fields
}

func executionV2SecretResponse() map[string]ActionFieldSchema {
	return map[string]ActionFieldSchema{"secret": {Type: "object", Required: true, Properties: map[string]ActionFieldSchema{
		"secret_ref": {Type: "string", Required: true}, "revision": {Type: "integer", Required: true},
		"purpose": {Type: "string", Required: true}, "provider": {Type: "string", Required: true},
		"binding_digest": {Type: "string", Required: true}, "status": {Type: "string", Required: true},
		"created_at": {Type: "string", Required: true}, "updated_at": {Type: "string", Required: true},
	}}}
}

// executionV2Schema is intentionally action-specific. ProductCore does not
// enforce ActionSchema at runtime, so this file and the strict boundary
// decoder are tested for exact key parity.
func executionV2Schema(name string) *ActionSchema {
	object := func(name string) map[string]ActionFieldSchema {
		return map[string]ActionFieldSchema{name: {Type: "object", Required: true}}
	}
	get := func(id, result string) *ActionSchema {
		return &ActionSchema{
			Request:  map[string]ActionFieldSchema{id: {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}}},
			Response: object(result),
		}
	}

	switch name {
	case "projects.analyze":
		request := executionV2MutationBase()
		request["project_id"] = ActionFieldSchema{Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}}
		request["source"] = ActionFieldSchema{Type: "object", Required: true, Properties: map[string]ActionFieldSchema{
			"kind": {Type: "string", Required: true}, "location": {Type: "string"},
			"commit": {Type: "string"}, "artifact_id": {Type: "string"},
			"credential_ref": {Type: "string"}, "credential_revision": {Type: "integer"},
			"immutable": {Type: "boolean", Required: true},
		}}
		return &ActionSchema{Request: request, Response: executionV2OwnedObject("analysis")}
	case "analyses.get":
		return &ActionSchema{
			Request:  map[string]ActionFieldSchema{"analysis_id": {Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}}},
			Response: executionV2OwnedObject("analysis"),
		}
	case "targets.list":
		return &ActionSchema{Request: executionV2PageRequest(), Response: executionV2OwnedPageResponse("targets")}
	case "targets.get":
		request := map[string]ActionFieldSchema{"target_id": {Type: "string", Required: true}, "revision": {Type: "integer"}}
		return &ActionSchema{Request: request, Response: executionV2OwnedObject("target")}
	case "targets.import":
		request := executionV2MutationBase()
		request["credential_id"] = ActionFieldSchema{Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}}
		request["credential_revision"] = ActionFieldSchema{Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer"}}
		request["instance_id"] = ActionFieldSchema{Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "aws_ec2_instance_id"}}
		response := executionV2OwnedObject("target")
		response["observation_id"] = ActionFieldSchema{Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}}
		response["observation"] = ActionFieldSchema{Type: "object", Required: true, Properties: map[string]ActionFieldSchema{"owner_id": {Type: "string", Required: true}}}
		return &ActionSchema{Request: request, Response: response}
	case "targets.reserve":
		request := executionV2MutationBase()
		request["credential_id"] = ActionFieldSchema{Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "canonical_uuid"}}
		request["credential_revision"] = ActionFieldSchema{Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "positive_integer"}}
		request["instance_type"] = ActionFieldSchema{Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "aws_instance_type"}}
		request["volume_gib"] = ActionFieldSchema{Type: "integer", Required: true, Presence: &ActionPresenceSchema{Present: "range_8_16384"}}
		return &ActionSchema{Request: request, Response: executionV2OwnedObject("target")}
	case "targets.observe":
		request := executionV2MutationBase()
		request["target_id"] = ActionFieldSchema{Type: "string", Required: true}
		request["target_revision"] = ActionFieldSchema{Type: "integer", Required: true}
		return &ActionSchema{Request: request, Response: executionV2OwnedObject("observation")}
	case "plans.create":
		request := executionV2PlanSelection(executionV2MutationBase())
		request["project_id"] = ActionFieldSchema{Type: "string", Required: true}
		request["analysis_id"] = ActionFieldSchema{Type: "string", Required: true}
		return &ActionSchema{Request: request, Response: executionV2OwnedObject("plan")}
	case "plans.revise":
		request := executionV2PlanSelection(executionV2RevisionMutationBase())
		request["plan_id"] = ActionFieldSchema{Type: "string", Required: true}
		return &ActionSchema{Request: request, Response: executionV2OwnedObject("plan")}
	case "plans.get":
		request := map[string]ActionFieldSchema{"plan_id": {Type: "string", Required: true}, "revision": {Type: "integer"}}
		return &ActionSchema{Request: request, Response: executionV2OwnedObject("plan")}
	case "plans.list":
		return &ActionSchema{Request: executionV2PageRequest(), Response: executionV2PageResponse("plans")}
	case "deployments.list":
		request := executionV2PageRequest()
		request["project_id"] = ActionFieldSchema{Type: "string"}
		return &ActionSchema{Request: request, Response: executionV2PageResponse("deployments")}
	case "deployments.get":
		return get("deployment_id", "deployment")
	case "deployments.events":
		return &ActionSchema{Request: executionV2EventsRequest("deployment_id"), Response: executionV2EventsResponse()}
	case "runs.create":
		request := executionV2MutationBase()
		request["plan_id"] = ActionFieldSchema{Type: "string", Required: true}
		request["plan_revision"] = ActionFieldSchema{Type: "integer", Required: true}
		request["operation"] = ActionFieldSchema{Type: "string", Required: true}
		request["trigger_kind"] = ActionFieldSchema{Type: "string"}
		request["rollback_of_run_id"] = ActionFieldSchema{Type: "string"}
		response := object("run")
		response["stages"] = ActionFieldSchema{Type: "array", Required: true, Items: &ActionFieldSchema{Type: "object"}}
		return &ActionSchema{Request: request, Response: response}
	case "runs.get":
		response := object("run")
		response["stages"] = ActionFieldSchema{Type: "array", Required: true, Items: &ActionFieldSchema{Type: "object"}}
		return &ActionSchema{Request: map[string]ActionFieldSchema{"run_id": {Type: "string", Required: true}}, Response: response}
	case "runs.list":
		request := executionV2PageRequest()
		request["project_id"] = ActionFieldSchema{Type: "string"}
		request["deployment_id"] = ActionFieldSchema{Type: "string"}
		return &ActionSchema{Request: request, Response: executionV2PageResponse("runs")}
	case "runs.cancel", "runs.retry":
		request := executionV2RevisionMutationBase()
		request["run_id"] = ActionFieldSchema{Type: "string", Required: true}
		return &ActionSchema{Request: request, Response: object("run")}
	case "runs.reconcile":
		request := executionV2RevisionMutationBase()
		request["run_id"] = ActionFieldSchema{Type: "string", Required: true}
		request["stage_id"] = ActionFieldSchema{Type: "string", Required: true}
		return &ActionSchema{Request: request, Response: object("run")}
	case "runs.events":
		return &ActionSchema{Request: executionV2EventsRequest("run_id"), Response: executionV2EventsResponse()}
	case "confirmations.get":
		return get("confirmation_id", "confirmation")
	case "confirmations.list":
		request := executionV2PageRequest()
		request["states"] = ActionFieldSchema{Type: "array", Items: &ActionFieldSchema{Type: "string"}}
		return &ActionSchema{Request: request, Response: executionV2PageResponse("confirmations")}
	case "confirmations.confirm", "confirmations.reject":
		request := executionV2RevisionMutationBase()
		request["confirmation_id"] = ActionFieldSchema{Type: "string", Required: true}
		return &ActionSchema{Request: request, Response: object("confirmation")}
	case "artifacts.get":
		return get("artifact_id", "artifact")
	case "service_bindings.list":
		request := executionV2PageRequest()
		request["project_id"] = ActionFieldSchema{Type: "string"}
		return &ActionSchema{Request: request, Response: executionV2PageResponse("bindings")}
	case "service_bindings.get":
		return get("binding_id", "binding")
	case "service_bindings.invoke":
		request := executionV2RevisionMutationBase()
		request["binding_id"] = ActionFieldSchema{Type: "string", Required: true}
		request["operation"] = ActionFieldSchema{Type: "string", Required: true}
		request["input"] = ActionFieldSchema{Type: "object", Required: true}
		return &ActionSchema{Request: request, Response: object("result")}
	case "secrets.create":
		request := executionV2MutationBase()
		request["provider"] = ActionFieldSchema{Type: "string", Required: true}
		request["purpose"] = ActionFieldSchema{Type: "string", Required: true, Presence: &ActionPresenceSchema{Present: "exact:ai_provider_api_key"}}
		request["value"] = ActionFieldSchema{Type: "string", Required: true, WriteOnly: true}
		return &ActionSchema{Request: request, Response: executionV2SecretResponse()}
	case "secrets.get":
		return &ActionSchema{Request: map[string]ActionFieldSchema{"secret_ref": {Type: "string", Required: true}, "revision": {Type: "integer"}}, Response: executionV2SecretResponse()}
	case "secrets.list":
		return &ActionSchema{Request: executionV2PageRequest(), Response: map[string]ActionFieldSchema{"secrets": {Type: "array", Required: true, Items: &ActionFieldSchema{Type: "object", Properties: executionV2SecretResponse()["secret"].Properties}}, "next_page_token": {Type: "string", Required: true}}}
	case "secrets.revoke":
		request := executionV2RevisionMutationBase()
		request["secret_ref"] = ActionFieldSchema{Type: "string", Required: true}
		return &ActionSchema{Request: request, Response: executionV2SecretResponse()}
	default:
		return nil
	}
}
