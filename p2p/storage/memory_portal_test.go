package storage

import (
	"context"
	"sync"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
)

func TestMemoryStorePortalCASAndCopies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()

	if _, ok, err := store.LoadPortal(ctx); err != nil || ok {
		t.Fatalf("LoadPortal before save = (_, %v, %v), want (_, false, nil)", ok, err)
	}

	state := portalState{
		Initialized:    true,
		MatrixDeviceID: "DEVICE_A",
		AgentConfig: dirextalkdomain.AgentConfig{
			MCPBlockedRoomIDs: []string{"!blocked:example.com"},
		},
		ClientBuild: clientBuild{Version: "1.0.0", BuildNumber: "1"},
	}
	if err := store.SavePortal(ctx, state); err != nil {
		t.Fatalf("SavePortal: %v", err)
	}

	state.AgentConfig.MCPBlockedRoomIDs[0] = "mutated"
	loaded, ok, err := store.LoadPortal(ctx)
	if err != nil || !ok {
		t.Fatalf("LoadPortal after save = (_, %v, %v), want (_, true, nil)", ok, err)
	}
	if got := loaded.AgentConfig.MCPBlockedRoomIDs[0]; got != "!blocked:example.com" {
		t.Fatalf("stored blocked rooms aliased caller input: %q", got)
	}

	loaded.AgentConfig.MCPBlockedRoomIDs[0] = "returned mutation"
	reloaded, _, err := store.LoadPortal(ctx)
	if err != nil {
		t.Fatalf("LoadPortal after returned-value mutation: %v", err)
	}
	if got := reloaded.AgentConfig.MCPBlockedRoomIDs[0]; got != "!blocked:example.com" {
		t.Fatalf("LoadPortal returned aliased blocked rooms: %q", got)
	}

	updated, err := store.SaveClientBuild(ctx, "WRONG_DEVICE", clientBuild{Version: "2.0.0"})
	if err != nil || updated {
		t.Fatalf("SaveClientBuild wrong device = (%v, %v), want (false, nil)", updated, err)
	}
	updated, err = store.SaveClientBuild(ctx, "DEVICE_A", clientBuild{Version: "2.0.0", BuildNumber: "2"})
	if err != nil || !updated {
		t.Fatalf("SaveClientBuild matching device = (%v, %v), want (true, nil)", updated, err)
	}

	loaded, _, _ = store.LoadPortal(ctx)
	if loaded.ClientBuild.Version != "2.0.0" {
		t.Fatalf("client build version = %q, want 2.0.0", loaded.ClientBuild.Version)
	}

	// SavePortal preserves client build while the device is unchanged, matching
	// the durable store's compare-and-swap boundary.
	loaded.Password = "rotated"
	loaded.ClientBuild = clientBuild{Version: "stale"}
	if err := store.SavePortal(ctx, loaded); err != nil {
		t.Fatalf("SavePortal with unchanged device: %v", err)
	}
	loaded, _, _ = store.LoadPortal(ctx)
	if loaded.ClientBuild.Version != "2.0.0" {
		t.Fatalf("unchanged-device SavePortal replaced client build: %q", loaded.ClientBuild.Version)
	}

	loaded.MatrixDeviceID = "DEVICE_B"
	loaded.ClientBuild = clientBuild{Version: "3.0.0"}
	if err := store.SavePortal(ctx, loaded); err != nil {
		t.Fatalf("SavePortal with changed device: %v", err)
	}
	loaded, _, _ = store.LoadPortal(ctx)
	if loaded.ClientBuild.Version != "3.0.0" {
		t.Fatalf("changed-device SavePortal kept old client build: %q", loaded.ClientBuild.Version)
	}
}

func TestMemoryStoreSaveClientBuildRequiresPortal(t *testing.T) {
	t.Parallel()
	updated, err := NewMemoryStore().SaveClientBuild(context.Background(), "DEVICE", clientBuild{Version: "1"})
	if err != nil || updated {
		t.Fatalf("SaveClientBuild without portal = (%v, %v), want (false, nil)", updated, err)
	}
}

func TestMemoryStoreReadMarkersAdvanceMonotonicallyAndListDeterministically(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()

	for _, marker := range []readMarker{
		{RoomID: "!z:example.com", EventID: "$z-new", OriginServerTS: 200, TopologicalPosition: 2, StreamPosition: 20},
		{RoomID: "!a:example.com", EventID: "$a-current", OriginServerTS: 100, TopologicalPosition: 5, StreamPosition: 10},
		{RoomID: "!a:example.com", EventID: "$a-stale", OriginServerTS: 999, TopologicalPosition: 4, StreamPosition: 99},
		{RoomID: "!a:example.com", EventID: "$a-equal", OriginServerTS: 100, TopologicalPosition: 5, StreamPosition: 11},
		{RoomID: "!a:example.com", EventID: "$a-replay", OriginServerTS: 9999, TopologicalPosition: 5, StreamPosition: 10},
	} {
		if err := store.SaveReadMarker(ctx, marker); err != nil {
			t.Fatalf("SaveReadMarker(%#v): %v", marker, err)
		}
	}

	marker, found, err := store.GetReadMarker(ctx, "!a:example.com")
	if err != nil || !found {
		t.Fatalf("GetReadMarker = (%#v, %v, %v), want current marker", marker, found, err)
	}
	if marker.EventID != "$a-equal" || marker.OriginServerTS != 100 ||
		marker.TopologicalPosition != 5 || marker.StreamPosition != 11 {
		t.Fatalf("authoritatively older marker replaced current marker: %#v", marker)
	}
	if _, found, err := store.GetReadMarker(ctx, "!missing:example.com"); err != nil || found {
		t.Fatalf("missing GetReadMarker = (_, %v, %v), want (_, false, nil)", found, err)
	}

	markers, err := store.ListReadMarkers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(markers) != 2 || markers[0].RoomID != "!a:example.com" || markers[1].RoomID != "!z:example.com" {
		t.Fatalf("ListReadMarkers = %#v, want room-id order", markers)
	}
}

func TestMemoryStoreReadMarkerConcurrentWritesDoNotRegress(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	markers := []readMarker{
		{RoomID: "!room:example.com", EventID: "$newest", OriginServerTS: 1, TopologicalPosition: 9, StreamPosition: 30},
		{RoomID: "!room:example.com", EventID: "$older-depth", OriginServerTS: 999, TopologicalPosition: 8, StreamPosition: 99},
		{RoomID: "!room:example.com", EventID: "$older-stream", OriginServerTS: 999, TopologicalPosition: 9, StreamPosition: 29},
	}

	var wg sync.WaitGroup
	for _, marker := range markers {
		marker := marker
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := store.SaveReadMarker(ctx, marker); err != nil {
				t.Errorf("SaveReadMarker(%s): %v", marker.EventID, err)
			}
		}()
	}
	wg.Wait()

	marker, found, err := store.GetReadMarker(ctx, "!room:example.com")
	if err != nil || !found || marker.EventID != "$newest" {
		t.Fatalf("concurrent marker = (%#v, %v, %v), want newest", marker, found, err)
	}
}
