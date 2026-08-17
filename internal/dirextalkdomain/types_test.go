package dirextalkdomain

import (
	"encoding/json"
	"testing"
)

func TestNormalizeMemberMembershipUsesMatrixJoinEnum(t *testing.T) {
	for input, want := range map[string]string{
		" join ":   "join",
		" JOINED ": "join",
		"Joining":  "joining",
		"LEFT":     "left",
	} {
		if got := NormalizeMemberMembership(input); got != want {
			t.Fatalf("NormalizeMemberMembership(%q) = %q, want %q", input, got, want)
		}
	}
	if !MemberMembershipJoined("JOIN") || MemberMembershipJoined("joined") {
		t.Fatal("only Matrix membership=join must be treated as joined")
	}
}

func TestMemberRecordJSONIncludesCompatibilityFields(t *testing.T) {
	raw, err := json.Marshal(MemberRecord{
		RoomID:     "!room:example.com",
		UserID:     "@alice:example.com",
		Membership: "join",
		Role:       "member",
	})
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if got["user_mxid"] != "@alice:example.com" {
		t.Fatalf("expected user_mxid compatibility field, got %#v", got)
	}
	if got["status"] != "join" {
		t.Fatalf("expected status compatibility field, got %#v", got)
	}
	if _, ok := got["RequesterNodeBaseURL"]; ok {
		t.Fatalf("RequesterNodeBaseURL must not be serialized: %#v", got)
	}
	if _, ok := got["requester_node_base_url"]; ok {
		t.Fatalf("requester_node_base_url must not be serialized: %#v", got)
	}
}

func TestAgentConfigJSONContainsOnlyMessageServerOwnedFields(t *testing.T) {
	raw, err := json.Marshal(AgentConfig{
		OnlineAgentIdentity: AgentIdentityConfig{
			DisplayName: "Your Agent",
			AvatarURL:   "mxc://online",
		},
		Enabled:           true,
		MCPBlockedRoomIDs: []string{"!blocked:example.com"},
	})
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var encoded map[string]any
	if err := json.Unmarshal(raw, &encoded); err != nil {
		t.Fatalf("Unmarshal encoded failed: %v", err)
	}
	if _, ok := encoded["display_name"]; ok {
		t.Fatalf("flat display_name must not be serialized: %#v", encoded)
	}
	if _, ok := encoded["avatar_url"]; ok {
		t.Fatalf("flat avatar_url must not be serialized: %#v", encoded)
	}
	if _, ok := encoded["native_agent_identity"]; ok {
		t.Fatalf("Native Agent identity must not be stored by Message Server: %#v", encoded)
	}
	onlineIdentity := encoded["online_agent_identity"].(map[string]any)
	if onlineIdentity["display_name"] != "Your Agent" || onlineIdentity["avatar_url"] != "mxc://online" {
		t.Fatalf("Online Agent identity should round-trip as a known field, got %#v", encoded)
	}

	var decoded AgentConfig
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if !decoded.Enabled || decoded.OnlineAgentIdentity.DisplayName != "Your Agent" {
		t.Fatalf("expected known fields to round-trip, got %#v", decoded)
	}
}

func TestAgentConfigJSONIgnoresFlatIdentityFields(t *testing.T) {
	var decoded AgentConfig
	if err := json.Unmarshal([]byte(`{
		"display_name":"Legacy Agent",
		"avatar_url":"mxc://legacy"
	}`), &decoded); err != nil {
		t.Fatalf("Unmarshal legacy config failed: %v", err)
	}
	if decoded.OnlineAgentIdentity != (AgentIdentityConfig{}) {
		t.Fatalf("flat identity fields must not populate current identities: %#v", decoded)
	}
}

func TestSharedRecordJSONContracts(t *testing.T) {
	raw, err := json.Marshal(struct {
		Marker ReadMarker  `json:"marker"`
		Block  BlockRecord `json:"block"`
	}{
		Marker: ReadMarker{
			RoomID:         "!room:example.com",
			EventID:        "$event:example.com",
			OriginServerTS: 123,
		},
		Block: BlockRecord{
			TargetType:  "contact",
			TargetID:    "@alice:example.com",
			RoomID:      "!direct:example.com",
			PeerMXID:    "@alice:example.com",
			DisplayName: "Alice",
			AvatarURL:   "mxc://avatar",
			CreatedAt:   456,
		},
	})
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var got map[string]map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if got["marker"]["origin_server_ts"] != float64(123) {
		t.Fatalf("expected read marker origin_server_ts contract, got %#v", got["marker"])
	}
	if got["block"]["target_type"] != "contact" || got["block"]["peer_mxid"] != "@alice:example.com" {
		t.Fatalf("expected block JSON contract, got %#v", got["block"])
	}
	if _, ok := got["block"]["channel_id"]; ok {
		t.Fatalf("empty channel_id should be omitted, got %#v", got["block"])
	}
}

func TestSocialCallReportRecordJSONContracts(t *testing.T) {
	raw, err := json.Marshal(struct {
		Call     CallRecord     `json:"call"`
		Favorite FavoriteRecord `json:"favorite"`
		Follow   FollowRecord   `json:"follow"`
		Reaction ReactionRecord `json:"reaction"`
		Report   ReportRecord   `json:"report"`
	}{
		Call: CallRecord{
			CallID:        "call_1",
			RoomID:        "!room:example.com",
			RoomType:      "direct",
			MediaType:     "video",
			CreatedByMXID: "@owner:example.com",
			State:         "connected",
			CreatedAt:     "2026-07-08T00:00:00Z",
		},
		Favorite: FavoriteRecord{
			ID:             7,
			EventID:        "$event:example.com",
			RoomID:         "!room:example.com",
			SenderID:       "@alice:example.com",
			SenderName:     "Alice",
			Content:        "hello",
			MessageType:    "text",
			OriginServerTS: 123,
			CreatedAt:      "2026-07-08T00:00:00Z",
		},
		Follow: FollowRecord{
			Domain:    "remote.example",
			CreatedAt: "2026-07-08T00:00:00Z",
		},
		Reaction: ReactionRecord{
			TargetType: "post",
			TargetID:   "post_1",
			ChannelID:  "channel_1",
			PostID:     "post_1",
			Reaction:   "like",
			UserID:     "@owner:example.com",
			Active:     true,
			CreatedAt:  "2026-07-08T00:00:00Z",
		},
		Report: ReportRecord{
			ReportID:            "report_1",
			TargetType:          "group",
			TargetRoomID:        "!room:example.com",
			TargetName:          "Group",
			ReporterMXID:        "@owner:example.com",
			ReporterDisplayName: "Owner",
			Reason:              "spam",
			Body:                "body",
			ImageURLs:           []string{"mxc://image"},
			SystemRoomID:        "!system:example.com",
			EventID:             "$report:example.com",
			OriginServerTS:      456,
			CreatedAt:           "2026-07-08T00:00:00Z",
		},
	})
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var got map[string]map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if got["call"]["call_id"] != "call_1" || got["call"]["created_by_mxid"] != "@owner:example.com" {
		t.Fatalf("expected call JSON contract, got %#v", got["call"])
	}
	if _, ok := got["call"]["answered_at"]; ok {
		t.Fatalf("empty call answered_at should be omitted, got %#v", got["call"])
	}
	if got["favorite"]["origin_server_ts"] != float64(123) {
		t.Fatalf("expected favorite origin_server_ts contract, got %#v", got["favorite"])
	}
	if got["follow"]["domain"] != "remote.example" {
		t.Fatalf("expected follow JSON contract, got %#v", got["follow"])
	}
	if got["reaction"]["active"] != true || got["reaction"]["post_id"] != "post_1" {
		t.Fatalf("expected reaction JSON contract, got %#v", got["reaction"])
	}
	if got["report"]["reporter_mxid"] != "@owner:example.com" || got["report"]["origin_server_ts"] != float64(456) {
		t.Fatalf("expected report JSON contract, got %#v", got["report"])
	}
	if _, ok := got["report"]["target_channel_id"]; ok {
		t.Fatalf("empty report target_channel_id should be omitted, got %#v", got["report"])
	}
}

func TestTerminalCallStateNormalizesKnownTerminalValues(t *testing.T) {
	for _, state := range []string{"ended", " REJECTED ", "Missed", "failed"} {
		if !TerminalCallState(state) {
			t.Fatalf("TerminalCallState(%q) = false", state)
		}
	}
	for _, state := range []string{"", "ringing", "connected", "cancelled"} {
		if TerminalCallState(state) {
			t.Fatalf("TerminalCallState(%q) = true", state)
		}
	}
}

func TestBlockIdentityHelpersPreserveCompatibility(t *testing.T) {
	for _, alias := range []string{"contact", " FRIEND ", "User", "member"} {
		if got := NormalizeBlockTargetType(alias); got != "contact" {
			t.Fatalf("NormalizeBlockTargetType(%q) = %q", alias, got)
		}
	}
	for _, invalid := range []string{"", "group", "channel", "room"} {
		if got := NormalizeBlockTargetType(invalid); got != "" {
			t.Fatalf("NormalizeBlockTargetType(%q) = %q", invalid, got)
		}
	}
	if got := BlockKey(" friend ", " @alice:example.com "); got != "contact|@alice:example.com" {
		t.Fatalf("BlockKey() = %q", got)
	}
	if got := DisplayNameFromMXID("@alice:example.com"); got != "alice" {
		t.Fatalf("DisplayNameFromMXID() = %q", got)
	}
	if got := DisplayNameFromMXID(""); got != "" {
		t.Fatalf("DisplayNameFromMXID(empty) = %q", got)
	}
	for _, tt := range []struct {
		mxid string
		want string
	}{
		{mxid: "@alice:example.com", want: "example.com"},
		{mxid: "@owner:dendrite-b:8448", want: "dendrite-b:8448"},
		{mxid: "alice:example.com", want: "example.com"},
		{mxid: "@alice:", want: ""},
		{mxid: "alice", want: ""},
		{mxid: " @alice:example.com ", want: "example.com "},
	} {
		if got := DomainFromMXID(tt.mxid); got != tt.want {
			t.Fatalf("DomainFromMXID(%q) = %q, want %q", tt.mxid, got, tt.want)
		}
	}
}

func TestEventAndInviteGrantJSONContracts(t *testing.T) {
	raw, err := json.Marshal(struct {
		Grant  ChannelInviteGrant `json:"grant"`
		Event  Event              `json:"event"`
		Bounds EventBounds        `json:"bounds"`
	}{
		Grant: ChannelInviteGrant{
			GrantID:     "grant_1",
			ChannelID:   "channel_1",
			RoomID:      "!channel:example.com",
			ShareRoomID: "!share:example.com",
			CreatedBy:   "@owner:example.com",
			CreatedAt:   123,
		},
		Event: Event{
			Seq:       7,
			Type:      "channel.updated",
			RoomID:    "!channel:example.com",
			EventID:   "$event:example.com",
			DedupeKey: "secret-dedupe",
			Payload:   map[string]any{"ok": true},
			CreatedAt: "2026-07-08T00:00:00Z",
		},
		Bounds: EventBounds{
			MinSeq: 1,
			MaxSeq: 9,
			Count:  3,
		},
	})
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var got map[string]map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if got["grant"]["grant_id"] != "grant_1" || got["grant"]["share_room_id"] != "!share:example.com" {
		t.Fatalf("expected invite grant JSON contract, got %#v", got["grant"])
	}
	if got["event"]["seq"] != float64(7) || got["event"]["type"] != "channel.updated" {
		t.Fatalf("expected event JSON contract, got %#v", got["event"])
	}
	if _, ok := got["event"]["DedupeKey"]; ok {
		t.Fatalf("DedupeKey must not be serialized, got %#v", got["event"])
	}
	if _, ok := got["event"]["dedupe_key"]; ok {
		t.Fatalf("dedupe_key must not be serialized, got %#v", got["event"])
	}
	if got["bounds"]["min_seq"] != float64(1) || got["bounds"]["max_seq"] != float64(9) || got["bounds"]["count"] != float64(3) {
		t.Fatalf("expected event bounds JSON contract, got %#v", got["bounds"])
	}
}

func TestProductMemberVisibilityAndRoleNormalization(t *testing.T) {
	hidden := []string{"leave", "LEFT", " remove ", "rejected", "ban", "banned"}
	for _, membership := range hidden {
		if !MemberHidden(membership) {
			t.Fatalf("MemberHidden(%q) = false, want true", membership)
		}
	}
	for _, membership := range []string{"", "invite", "pending", "join"} {
		if MemberHidden(membership) {
			t.Fatalf("MemberHidden(%q) = true, want false", membership)
		}
	}
	if !ProductOwnerRole(" OWNER ") || ProductOwnerRole("member") {
		t.Fatal("ProductOwnerRole did not preserve owner-only semantics")
	}
	if got := NormalizeProductMemberRole("OWNER"); got != "owner" {
		t.Fatalf("NormalizeProductMemberRole(owner) = %q", got)
	}
	if got := NormalizeProductMemberRole("administrator"); got != "member" {
		t.Fatalf("NormalizeProductMemberRole(non-owner) = %q", got)
	}
}
