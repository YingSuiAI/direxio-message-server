package p2p

import (
	"encoding/json"
	"testing"
)

func reactionHistoryPayloads(t *testing.T, payload map[string]any) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(payload["reactions"])
	if err != nil {
		t.Fatalf("marshal reactions: %v", err)
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatalf("unmarshal reactions: %v", err)
	}
	return items
}

func mapValue(t *testing.T, payload map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := payload[key].(map[string]any)
	if !ok {
		t.Fatalf("expected %s object in %#v", key, payload)
	}
	return value
}

func setServiceOwnerForTest(service *Service, mxid, displayName string) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.ownerMXID = mxid
	service.profile.UserID = mxid
	service.profile.DisplayName = displayName
}

func assertSingleInitializedFlag(t *testing.T, payload map[string]any, initialized bool) {
	t.Helper()
	if payload["initialized"] != initialized {
		t.Fatalf("expected initialized=%v, got %#v", initialized, payload)
	}
	allowed := map[string]bool{
		"access_token":       true,
		"device_id":          true,
		"user_id":            true,
		"homeserver":         true,
		"agent_room_id":      true,
		"system_room_id":     true,
		"password":           true,
		"initialized":        true,
		"store_mode":         true,
		"projector_started":  true,
		"policy_index_mode":  true,
		"policy_index_ready": true,
		"event_stream_ready": true,
	}
	for field := range payload {
		if !allowed[field] {
			t.Fatalf("session exposed unexpected initialization field %s: %#v", field, payload)
		}
	}
}

func jsonList(t *testing.T, value any) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var result []map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func findMember(members []memberRecord, userID string) memberRecord {
	for _, member := range members {
		if member.UserID == userID {
			return member
		}
	}
	return memberRecord{}
}

func findContact(contacts []contactRecord, peerMXID string) contactRecord {
	for _, contact := range contacts {
		if contact.PeerMXID == peerMXID {
			return contact
		}
	}
	return contactRecord{}
}
