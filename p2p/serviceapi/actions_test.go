package serviceapi

import (
	"reflect"
	"testing"
)

func TestActionSpecsReturnsStableOrderedCopy(t *testing.T) {
	first := ActionSpecs()
	second := ActionSpecs()

	if len(first) != 207 {
		t.Fatalf("ActionSpecs() returned %d actions, want 207", len(first))
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
func TestWorkloadPlanSchemaNestedFields(t *testing.T) {
	s, _ := ActionSpecFor("agent.core.workloads.plan")
	tt := s.Schema.Request["typed_target"]
	if tt.Type != "object" || tt.Properties["identity"].Properties["aws_ecs_subnet_ids"].Items.Type != "string" || tt.Properties["ports"].Items.Properties["port"].Type != "integer" || tt.Properties["network_grants"].Items.Properties["reference_id"].Type != "string" {
		t.Fatal("incomplete workload schema")
	}
	if s.Schema.Request["command_steps"].Items.Type != "string" {
		t.Fatal("command_steps items missing")
	}
	plan := s.Schema.Response["plan"]
	if !plan.Required || plan.Properties["typed_target"].Properties["identity"].Properties["kind"].Type != "string" || plan.Properties["typed_resource_limits"].Properties["output_mb"].Type != "integer" || plan.Properties["typed_secret_grants"].Items.Properties["binding_digest"].Type != "string" {
		t.Fatal("workload plan response schema is incomplete")
	}
	apply, _ := ActionSpecFor("agent.core.workloads.apply")
	op := apply.Schema.Response["operation"]
	if !op.Required || op.Properties["target_kind"].Type != "string" || op.Properties["desired_plan"].Properties["target"].Properties["network_grants"].Items.Properties["reference_id"].Type != "string" || op.Properties["actual"].Properties["identity"].Properties["aws_ecs_image_uri"].Type != "string" {
		t.Fatal("workload operation response schema is incomplete")
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
