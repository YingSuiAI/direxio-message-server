package serviceapi

import (
	"reflect"
	"testing"
)

func TestAgentConversationAndKnowledgeSchemasMatchHandlerResponses(t *testing.T) {
	byName := make(map[string]ActionSpec)
	for _, spec := range ActionSpecs() {
		byName[spec.Name] = spec
	}
	for _, test := range []struct {
		action string
		fields map[string]string
	}{
		{"agent.knowledge.config.get", map[string]string{"embedding_profile_id": "string", "embedding_profile_revision": "integer", "embedding_model": "string", "dimension": "integer", "collection": "string", "collection_config_digest": "string", "revision": "integer", "updated_at": "string"}},
		{"agent.knowledge.config.update", map[string]string{"embedding_profile_id": "string", "embedding_profile_revision": "integer", "embedding_model": "string", "dimension": "integer", "collection": "string", "collection_config_digest": "string", "revision": "integer", "updated_at": "string"}},
		{"agent.chat.conversations.create", map[string]string{"conversation": "object", "replayed": "boolean"}},
		{"agent.chat.conversations.list", map[string]string{"conversations": "array", "next_cursor": "string"}},
		{"agent.chat.conversations.get", map[string]string{"conversation": "object", "messages": "array", "next_cursor": "string"}},
		{"agent.chat.conversations.rename", map[string]string{"conversation": "object", "replayed": "boolean"}},
		{"agent.chat.conversations.delete", map[string]string{"conversation": "object", "replayed": "boolean"}},
		{"agent.knowledge.memory.create", map[string]string{"memory_id": "string", "title": "string", "content": "string", "tags": "array", "created_at": "string", "replayed": "boolean", "embedding_indexed": "boolean", "embedding_profile_id": "string", "embedding_profile_revision": "integer", "embedding_model": "string"}},
		{"agent.knowledge.search", map[string]string{"items": "array", "next_cursor": "string", "search_mode": "string", "embedding_profile_id": "string", "embedding_profile_revision": "integer", "embedding_model": "string", "embedding_generation": "string", "collection_config_digest": "string"}},
		{"agent.knowledge.status", map[string]string{"supported": "boolean", "count": "integer", "embedding_indexed": "integer", "embedding_stale": "integer", "embedding_profile_id": "string", "embedding_profile_revision": "integer", "embedding_model": "string"}},
	} {
		schema := byName[test.action].Schema
		if schema == nil || len(schema.Response) != len(test.fields) {
			t.Fatalf("%s response schema = %#v", test.action, schema)
		}
		for name, want := range test.fields {
			if got := schema.Response[name].Type; got != want {
				t.Fatalf("%s response.%s = %q, want %q", test.action, name, got, want)
			}
		}
	}
}

func TestKnowledgeActionSchemasPinCurrentUploadAndConfigContract(t *testing.T) {
	start, ok := ActionSpecFor("agent.knowledge.upload.start")
	if !ok || start.Schema == nil || !start.Schema.Request["content_sha256"].Required {
		t.Fatal("knowledge upload start must require content_sha256")
	}
	get, ok := ActionSpecFor("agent.knowledge.config.get")
	if !ok || get.Schema == nil || len(get.Schema.Request) != 0 {
		t.Fatalf("knowledge config get request schema = %#v", get)
	}
	update, ok := ActionSpecFor("agent.knowledge.config.update")
	if !ok || update.Schema == nil {
		t.Fatalf("knowledge config update schema = %#v", update)
	}
	for _, field := range []string{"idempotency_key", "expected_revision"} {
		if !update.Schema.Request[field].Required {
			t.Errorf("knowledge config update %s must be required", field)
		}
	}
	for _, field := range []string{"embedding_profile_id", "embedding_profile_revision", "embedding_model", "collection_config_digest", "revision"} {
		if !get.Schema.Response[field].Required || !update.Schema.Response[field].Required {
			t.Errorf("knowledge config %s must be required in both responses", field)
		}
	}
}

func TestKnowledgeSearchSchemaRequiresQueryAndResultEnvelope(t *testing.T) {
	spec, ok := ActionSpecFor("agent.knowledge.search")
	if !ok || spec.Schema == nil {
		t.Fatal("agent.knowledge.search must publish a schema")
	}
	if !spec.Schema.Request["query"].Required {
		t.Fatal("knowledge search query must be required")
	}
	for _, field := range []string{"items", "next_cursor", "search_mode"} {
		if !spec.Schema.Response[field].Required {
			t.Errorf("knowledge search response.%s must be required", field)
		}
	}
	for _, field := range []string{"embedding_profile_id", "embedding_profile_revision", "embedding_model", "embedding_generation", "collection_config_digest"} {
		if _, ok := spec.Schema.Response[field]; !ok {
			t.Errorf("knowledge search provenance field %s was removed", field)
		}
	}
}

func TestActionSpecsReturnsStableOrderedCopy(t *testing.T) {
	first := ActionSpecs()
	second := ActionSpecs()

	if len(first) == 0 {
		t.Fatal("ActionSpecs() returned no actions")
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("ActionSpecs() did not preserve action order")
	}

	first[0] = ActionSpec{Name: "mutated"}
	if reflect.DeepEqual(first, ActionSpecs()) {
		t.Fatal("ActionSpecs() returned storage shared with its caller")
	}
	if got := ActionSpecs()[0].Name; got != "portal.bootstrap" {
		t.Fatalf("mutating returned specs changed registry: first action = %q", got)
	}
}

func TestActionSpecForFindsEveryRegisteredAction(t *testing.T) {
	for _, want := range ActionSpecs() {
		got, ok := ActionSpecFor(" \t" + want.Name + "\n")
		if !ok {
			t.Errorf("ActionSpecFor(%q) did not find registered action", want.Name)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ActionSpecFor(%q) = %#v, want %#v", want.Name, got, want)
		}
	}
}

func TestActionSpecForRejectsUnknownAndRetiredActions(t *testing.T) {
	for _, action := range []string{
		"", "   ", "portal.missing", "PORTAL.BOOTSTRAP",
		"portal.setup", "agent.status", "apis.list",
		"contacts.export", "contacts.download", "contacts.import",
		"rooms.send", "rooms.send_media", "rooms.messages.delete",
		"rooms.messages.delete_batch", "rooms.messages.delete_range", "rooms.messages.recall",
		"sync.messages", "sync.unread", "search",
	} {
		if got, ok := ActionSpecFor(action); ok {
			t.Errorf("ActionSpecFor(%q) = %#v, true; want zero, false", action, got)
		}
	}
}

func TestModelProfileActionSchemasDescribePresenceSensitiveFields(t *testing.T) {
	for _, action := range []string{"agent.core.model_profiles.sync", "agent.core.model_profiles.list", "agent.core.model_profiles.get", "agent.core.model_profiles.delete"} {
		spec, ok := ActionSpecFor(action)
		if !ok || spec.Schema == nil {
			t.Fatalf("%s must publish an action schema", action)
		}
	}
	syncSpec, _ := ActionSpecFor("agent.core.model_profiles.sync")
	entry := syncSpec.Schema.Request["entries"].Items
	if entry == nil || entry.Properties["client_profile_id"].Presence == nil || entry.Properties["api_key"].Presence == nil {
		t.Fatal("sync schema must expose client reference and API-key presence")
	}
	keyPresence := entry.Properties["api_key"].Presence
	if keyPresence.Omitted != "preserve_existing" || keyPresence.Present != "rotate_write_only" || keyPresence.Empty != "rejected" {
		t.Fatalf("api_key presence schema = %#v", keyPresence)
	}
	refPresence := entry.Properties["client_profile_id"].Presence
	if refPresence.Present != "exact_nonempty_bytes" || refPresence.Empty != "rejected" {
		t.Fatalf("client_profile_id presence schema = %#v", refPresence)
	}
}
func TestAgentCoreFamilySchemaDrift(t *testing.T) {
	c, _ := ActionSpecFor("agent.core.schedules.create")
	if c.Schema.Request["task_template"].Type != "object" || c.Schema.Request["trigger"].Type != "object" {
		t.Fatal("schedule create schema")
	}
	u, _ := ActionSpecFor("agent.core.mcp.update")
	if !u.Schema.Request["installation_id"].Required {
		t.Fatal("mcp update installation_id")
	}
	i, _ := ActionSpecFor("agent.core.mcp.install")
	if i.Schema.Request["installation_id"].Required {
		t.Fatal("mcp install installation_id")
	}
	m, _ := ActionSpecFor("agent.core.mcp.execute")
	if !m.Schema.Request["tool_name"].Required {
		t.Fatal("mcp tool_name")
	}
	s, _ := ActionSpecFor("agent.core.skills.execute")
	if s.Schema.Request["tool_name"].Required {
		t.Fatal("skill tool_name")
	}
	inspect, _ := ActionSpecFor("agent.core.mcp.inspect")
	candidate := inspect.Schema.Request["candidate"]
	if candidate.Type != "object" || !candidate.Required || !candidate.Properties["pin"].Required || candidate.Properties["pin"].Properties["git_sha256"].Type != "string" {
		t.Fatal("inspect candidate schema must publish the immutable pin")
	}
	installation := i.Schema.Request["inspection"]
	if installation.Type != "object" || !installation.Required || installation.Properties["execution"].Properties["remote"].Properties["url"].Type != "string" || installation.Properties["network_grants"].Items.Properties["port"].Type != "integer" || installation.Properties["secret_grants"].Items.Properties["configured"].Type != "boolean" {
		t.Fatal("extension inspection schema is incomplete")
	}
	secret := i.Schema.Request["secret_inputs"].Items.Properties["secret_value"]
	if !secret.Required || !secret.WriteOnly {
		t.Fatal("extension secret value must be write-only")
	}
	deleteCredential, ok := ActionSpecFor("agent.core.aws.credentials.delete")
	if !ok || deleteCredential.Schema == nil {
		t.Fatal("AWS credential delete schema must be published")
	}
	if !deleteCredential.Schema.Request["idempotency_key"].Required ||
		!deleteCredential.Schema.Request["credential_id"].Required ||
		!deleteCredential.Schema.Request["expected_revision"].Required {
		t.Fatal("AWS credential delete must require idempotency, identity, and exact revision")
	}
}

func TestBuildActionSpecIndexRejectsDuplicateNames(t *testing.T) {
	specs := []ActionSpec{
		{Name: "duplicate", Auth: ActionAuthPublic, Transport: ActionTransportHTTPOnly},
		{Name: "duplicate", Auth: ActionAuthOwner, Transport: ActionTransportHTTPAndWS},
	}

	index, err := buildActionSpecIndex(specs)
	if err == nil {
		t.Fatal("buildActionSpecIndex() accepted duplicate action names")
	}
	if index != nil {
		t.Fatalf("buildActionSpecIndex() returned partial index %#v on error", index)
	}
}

func TestNativeAgentReferenceSchemaMatchesStrictProducerShape(t *testing.T) {
	wantFields := map[string]bool{
		"kind": true, "room_id": true, "room_type": true, "title": true,
		"preview": true, "channel_id": true, "post_id": true,
	}
	for _, action := range []string{"agent.chat", "agent.chat.stream"} {
		spec, ok := ActionSpecFor(action)
		if !ok || spec.Schema == nil {
			t.Fatalf("%s must publish a schema", action)
		}
		field := spec.Schema.Response["references"]
		if field.Type != "array" || field.Items == nil || field.Items.Type != "object" {
			t.Fatalf("%s references schema = %#v", action, field)
		}
		got := field.Items.Properties
		if len(got) != len(wantFields) {
			t.Fatalf("%s reference fields = %#v", action, got)
		}
		for name := range got {
			if !wantFields[name] {
				t.Errorf("%s publishes unsupported reference field %q", action, name)
			}
		}
		kind := field.Items.Properties["kind"]
		if !kind.Required {
			t.Errorf("%s reference kind must be required", action)
		}
		roomID := field.Items.Properties["room_id"]
		if !roomID.Required {
			t.Errorf("%s reference room_id must be required", action)
		}
		if field.Presence == nil || field.Presence.Omitted != "No room or channel references were produced." {
			t.Errorf("%s reference presence = %#v", action, field.Presence)
		}
	}
}

func TestNativeAgentWebSearchSchemasUseServerStoredCredential(t *testing.T) {
	getSpec, ok := ActionSpecFor("agent.web_search.config.get")
	if !ok || getSpec.Schema == nil {
		t.Fatal("agent.web_search.config.get must publish a schema")
	}
	updateSpec, ok := ActionSpecFor("agent.web_search.config.update")
	if !ok || updateSpec.Schema == nil {
		t.Fatal("agent.web_search.config.update must publish a schema")
	}
	if !updateSpec.Schema.Request["idempotency_key"].Required || !updateSpec.Schema.Request["expected_revision"].Required {
		t.Fatalf("web search update concurrency fields = %#v", updateSpec.Schema.Request)
	}
	if !updateSpec.Schema.Request["api_key"].WriteOnly {
		t.Fatal("web search update API key must be write-only")
	}
	for _, spec := range []ActionSpec{getSpec, updateSpec} {
		for _, field := range []string{"enabled", "provider", "api_key_configured", "revision"} {
			if !spec.Schema.Response[field].Required {
				t.Errorf("%s response field %s must be required", spec.Name, field)
			}
		}
		if _, exposed := spec.Schema.Response["api_key"]; exposed {
			t.Errorf("%s response must not publish an API key", spec.Name)
		}
	}
	testSpec, ok := ActionSpecFor("agent.web_search.test")
	if !ok || testSpec.Schema == nil || len(testSpec.Schema.Request) != 0 {
		t.Fatalf("agent.web_search.test must use the stored credential without request secrets: %#v", testSpec)
	}
	for _, action := range []string{"agent.chat", "agent.chat.stream"} {
		spec, ok := ActionSpecFor(action)
		if !ok || spec.Schema == nil {
			t.Fatalf("%s must publish a schema", action)
		}
		if _, legacy := spec.Schema.Request["tool_credentials"]; legacy {
			t.Fatalf("%s must not publish request-scoped web-search credentials", action)
		}
	}
}

func TestNativeAgentModelCatalogSchemaIsSeparateFromProfiles(t *testing.T) {
	spec, ok := ActionSpecFor("agent.models.list")
	if !ok || spec.Schema == nil {
		t.Fatal("agent.models.list must publish a provider catalog schema")
	}
	if !spec.Schema.Request["api_key"].WriteOnly {
		t.Fatal("provider catalog API key must remain write-only")
	}
	modelKind := spec.Schema.Request["model_kind"]
	if modelKind.Presence == nil || modelKind.Presence.Omitted != "conversation" {
		t.Fatalf("model_kind omission contract = %#v", modelKind.Presence)
	}
	if !spec.Schema.Response["models"].Required || !spec.Schema.Response["providers"].Required {
		t.Fatalf("provider catalog response = %#v", spec.Schema.Response)
	}
	if !spec.Schema.Response["models"].Items.Properties["id"].Required || !spec.Schema.Response["models"].Items.Properties["provider"].Required {
		t.Fatal("provider catalog model id and provider must be required")
	}
	if _, profiles := spec.Schema.Response["profiles"]; profiles {
		t.Fatal("provider catalog must not publish model profiles")
	}
}
