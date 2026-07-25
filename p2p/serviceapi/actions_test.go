package serviceapi

import (
	"reflect"
	"testing"
)

func TestActionSpecsReturnsStableOrderedCopy(t *testing.T) {
	first := ActionSpecs()
	second := ActionSpecs()

	if len(first) != 188 {
		t.Fatalf("ActionSpecs() returned %d actions, want 188", len(first))
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
