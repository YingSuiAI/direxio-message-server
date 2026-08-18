package members

import (
	"context"
	"net/http"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
)

type reactivationContractConversation struct{}

func (reactivationContractConversation) Operation(context.Context, string, string, string) (map[string]any, *dirextalkdomain.ConversationView, error) {
	return nil, nil, nil
}

func TestRoomReactivatePreservesMissingMatrixLookupErrorContract(t *testing.T) {
	module := New(nil, Config{
		OwnerMXID: func() string { return "@owner:example.com" },
		LookupMember: func(context.Context, string, string) (dirextalkdomain.MemberRecord, bool, error) {
			return dirextalkdomain.MemberRecord{}, false, nil
		},
		NewMember: func(roomID, channelID, userID string) dirextalkdomain.MemberRecord {
			return dirextalkdomain.MemberRecord{RoomID: roomID, ChannelID: channelID, UserID: userID}
		},
		SaveMember:           func(context.Context, dirextalkdomain.MemberRecord) error { return nil },
		ApplyLocalProfile:    func(*dirextalkdomain.MemberRecord) {},
		SaveRetainedMetadata: func(context.Context, string, dirextalkdomain.MemberRecord, map[string]any) *actionbase.Error { return nil },
		Conversation:         reactivationContractConversation{},
	})

	result, actionErr := module.RoomReactivate(context.Background(), map[string]any{
		"room_id": "!room:example.com", "room_type": "group", "rebuild_generation": "generation-1",
	})
	if result != nil || actionErr == nil || actionErr.Status != http.StatusInternalServerError {
		t.Fatalf("RoomReactivate = (%#v, %#v), want ProductCore 500", result, actionErr)
	}
	const want = "internal error: Matrix member lookup is not configured"
	if actionErr.Error != want {
		t.Fatalf("RoomReactivate error=%q, want %q", actionErr.Error, want)
	}
}
