package serviceapi

import (
	"reflect"
	"testing"
)

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

func TestReleaseV2SchemasPublishUnifiedComponentContract(t *testing.T) {
	status, ok := ActionSpecFor("release.v2.status")
	if !ok || status.Schema == nil || !status.Schema.Response["agent"].Required {
		t.Fatalf("release.v2.status schema = %#v", status.Schema)
	}
	agent := status.Schema.Response["agent"]
	for _, field := range []string{"available", "current_version", "latest_version", "minimum_server_version", "update_available", "compatibility", "reasons"} {
		if !agent.Properties[field].Required {
			t.Errorf("release.v2.status agent.%s must be required", field)
		}
	}
	if !status.Schema.Response["active_job"].Properties["component"].Required {
		t.Fatal("release.v2.status active_job.component must be required")
	}
	apply, ok := ActionSpecFor("release.v2.apply")
	if !ok || apply.Schema == nil || !apply.Schema.Request["component"].Required || !apply.Schema.Request["target_version"].Required || !apply.Schema.Response["job_token"].WriteOnly {
		t.Fatalf("release.v2.apply schema = %#v", apply.Schema)
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

func TestBuildActionSpecIndexRejectsDuplicateNames(t *testing.T) {
	specs := []ActionSpec{
		{Name: "duplicate", Auth: ActionAuthPublic, Transport: ActionTransportHTTPOnly},
		{Name: "duplicate", Auth: ActionAuthOwner, Transport: ActionTransportHTTPOnly},
	}

	index, err := buildActionSpecIndex(specs)
	if err == nil {
		t.Fatal("buildActionSpecIndex() accepted duplicate action names")
	}
	if index != nil {
		t.Fatalf("buildActionSpecIndex() returned partial index %#v on error", index)
	}
}
