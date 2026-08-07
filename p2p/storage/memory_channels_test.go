package storage

import (
	"context"
	"testing"
)

func TestMemoryStoreCompareAndSwapMemberGenerationGuardsState(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	existing := memberRecord{
		RoomID: "!channel:example.com", ChannelID: "channel-a", UserID: "@peer:example.com",
		Membership: "rejected", RequestID: "request-a", JoinedAt: 10,
	}
	if err := store.UpsertMember(ctx, existing); err != nil {
		t.Fatal(err)
	}
	desired := existing
	desired.Membership = "pending"
	desired.RequestID = "request-b"
	desired.JoinedAt = 20
	if saved, err := store.CompareAndSwapMemberGeneration(ctx, desired, existing.RequestID, "pending"); err != nil || saved {
		t.Fatalf("stale member state CAS = (%v, %v)", saved, err)
	}
	if saved, err := store.CompareAndSwapMemberGeneration(ctx, desired, existing.RequestID, existing.Membership); err != nil || !saved {
		t.Fatalf("current member state CAS = (%v, %v)", saved, err)
	}
	got, found, err := store.LookupMember(ctx, existing.RoomID, existing.UserID)
	if err != nil || !found || got.RequestID != desired.RequestID || got.JoinedAt != desired.JoinedAt || got.Membership != desired.Membership {
		t.Fatalf("member after CAS = (%#v, %v, %v)", got, found, err)
	}
}

func TestMemoryStoreCanonicalizesJoinedMembershipToJoin(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	member := memberRecord{RoomID: "!room:example.com", UserID: "@owner:example.com", Membership: " JOINED "}
	if err := store.UpsertMember(ctx, member); err != nil {
		t.Fatal(err)
	}
	got, found, err := store.LookupMember(ctx, member.RoomID, member.UserID)
	if err != nil || !found || got.Membership != "join" {
		t.Fatalf("canonical membership = %#v found=%v err=%v, want join", got, found, err)
	}
}

func TestMemoryStoreMembersPreserveLegacyVisibilityCountsAndOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	members := []memberRecord{
		{RoomID: "!room:example.com", UserID: "@late:example.com", Membership: "join", Role: "member", JoinedAt: 30},
		{RoomID: "!room:example.com", UserID: "@owner:example.com", Membership: "JOIN", Role: "OWNER", JoinedAt: 20},
		{RoomID: "!room:example.com", UserID: "@pending:example.com", Membership: "pending", Role: "member", JoinedAt: 10},
		{RoomID: "!room:example.com", UserID: "@left:example.com", Membership: "LEFT", Role: "member", JoinedAt: 5},
	}
	for _, member := range members {
		if err := store.UpsertMember(ctx, member); err != nil {
			t.Fatalf("UpsertMember(%s): %v", member.UserID, err)
		}
	}

	listed, err := store.ListMembers(ctx, "!room:example.com", "")
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	if len(listed) != 4 {
		t.Fatalf("ListMembers length = %d, want 4 legacy raw members", len(listed))
	}
	if listed[0].UserID != "@left:example.com" {
		t.Fatalf("first member = %q, want earliest joined_at first", listed[0].UserID)
	}
	if listed[1].UserID != "@pending:example.com" {
		t.Fatalf("ListMembers must retain hidden records for legacy internal consumers: %#v", listed)
	}

	forUser, err := store.ListMembersForUser(ctx, "@left:example.com")
	if err != nil {
		t.Fatalf("ListMembersForUser: %v", err)
	}
	if len(forUser) != 0 {
		t.Fatalf("ListMembersForUser returned hidden membership: %#v", forUser)
	}

	joined, pending, err := store.CountProductMembers(ctx, "!room:example.com", "")
	if err != nil {
		t.Fatalf("CountProductMembers: %v", err)
	}
	if joined != 2 || pending != 1 {
		t.Fatalf("CountProductMembers = (%d, %d), want (2, 1)", joined, pending)
	}
}

func TestMemoryStoreChannelQueriesPreserveLegacyMatching(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	channels := []channel{
		{ChannelID: "ch_owned", RoomID: "!owned:example.com", Name: "Zulu", Visibility: "PUBLIC"},
		{ChannelID: "ch_other", RoomID: "!other:example.com", Name: "Alpha", Description: "Needle", Visibility: "public"},
		{ChannelID: "ch_private", RoomID: "!private:example.com", Name: "Private", Visibility: "private"},
	}
	for _, ch := range channels {
		if err := store.UpsertChannel(ctx, ch); err != nil {
			t.Fatalf("UpsertChannel(%s): %v", ch.ChannelID, err)
		}
	}
	if err := store.UpsertMember(ctx, memberRecord{
		RoomID: "!owned:example.com", ChannelID: "ch_owned", UserID: "@owner:example.com", Membership: "JOIN", Role: "OWNER",
	}); err != nil {
		t.Fatalf("UpsertMember owner: %v", err)
	}

	joined, err := store.ListJoinedChannelsForUser(ctx, "@owner:example.com")
	if err != nil {
		t.Fatalf("ListJoinedChannelsForUser: %v", err)
	}
	if len(joined) != 1 || joined[0].ChannelID != "ch_owned" || !joined[0].IsOwned || joined[0].Role != "owner" || joined[0].MemberStatus != "join" {
		t.Fatalf("joined channels = %#v", joined)
	}

	public, err := store.SearchPublicChannels(ctx, "needle", 20)
	if err != nil {
		t.Fatalf("SearchPublicChannels: %v", err)
	}
	if len(public) != 1 || public[0].ChannelID != "ch_other" {
		t.Fatalf("public search = %#v, want ch_other", public)
	}
	public, err = store.SearchPublicChannels(ctx, "zulu", 20)
	if err != nil {
		t.Fatalf("SearchPublicChannels case-insensitive visibility: %v", err)
	}
	if len(public) != 1 || public[0].ChannelID != "ch_owned" {
		t.Fatalf("case-insensitive public search = %#v, want ch_owned", public)
	}

	owned, err := store.ListPublicChannelsForOwner(ctx, "@owner:example.com")
	if err != nil {
		t.Fatalf("ListPublicChannelsForOwner: %v", err)
	}
	if len(owned) != 1 || owned[0].ChannelID != "ch_owned" {
		t.Fatalf("owned public channels = %#v", owned)
	}
}

func TestMemoryStoreChannelContentOrderUpsertPaginationAndDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	for _, post := range []channelPostRecord{
		{PostID: "post_old", ChannelID: "ch", EventID: "$old", Body: "old", OriginServerTS: 10},
		{PostID: "post_new_a", ChannelID: "ch", EventID: "$new-a", Body: "new a", OriginServerTS: 20},
		{PostID: "post_new_b", ChannelID: "ch", EventID: "$new-b", Body: "new b", OriginServerTS: 20},
	} {
		if err := store.InsertChannelPost(ctx, post); err != nil {
			t.Fatalf("InsertChannelPost(%s): %v", post.PostID, err)
		}
	}
	if err := store.InsertChannelPost(ctx, channelPostRecord{PostID: "post_old", ChannelID: "ch", EventID: "$old", Body: "updated", OriginServerTS: 10}); err != nil {
		t.Fatalf("InsertChannelPost update: %v", err)
	}

	listed, err := store.ListChannelPosts(ctx, "ch")
	if err != nil {
		t.Fatalf("ListChannelPosts: %v", err)
	}
	if len(listed) != 3 || listed[0].PostID != "post_old" || listed[0].Body != "updated" || listed[2].PostID != "post_new_b" {
		t.Fatalf("posts preserve insertion position = %#v", listed)
	}

	page, more, err := store.ListChannelPostsPage(ctx, "ch", 0, 20, 0, "", 2)
	if err != nil {
		t.Fatalf("ListChannelPostsPage: %v", err)
	}
	if !more || len(page) != 2 || page[0].PostID != "post_new_b" || page[1].PostID != "post_new_a" {
		t.Fatalf("post page = %#v more=%v", page, more)
	}
	page, more, err = store.ListChannelPostsPage(ctx, "ch", 0, 20, 20, "post_new_a", 2)
	if err != nil {
		t.Fatalf("ListChannelPostsPage cursor: %v", err)
	}
	if more || len(page) != 1 || page[0].PostID != "post_old" {
		t.Fatalf("post cursor page = %#v more=%v", page, more)
	}
	offsetPage, more, err := store.ListChannelPostsOffsetPage(ctx, "ch", 1, 1)
	if err != nil || !more || len(offsetPage) != 1 || offsetPage[0].PostID != "post_new_a" {
		t.Fatalf("post offset page = %#v more=%v err=%v", offsetPage, more, err)
	}
	for _, post := range []channelPostRecord{
		{PostID: "public_old", ChannelID: "ch", Visibility: "public", OriginServerTS: 30},
		{PostID: "public_new", ChannelID: "ch", Visibility: "public", OriginServerTS: 40},
		{PostID: "other_public", ChannelID: "other", Visibility: "public", OriginServerTS: 50},
	} {
		if err := store.InsertChannelPost(ctx, post); err != nil {
			t.Fatalf("InsertChannelPost(%s): %v", post.PostID, err)
		}
	}
	publicPage, more, err := store.ListChannelPostsByVisibilityPage(ctx, "ch", "public", 0, 1)
	if err != nil || !more || len(publicPage) != 1 || publicPage[0].PostID != "public_new" {
		t.Fatalf("first public page = %#v more=%v err=%v", publicPage, more, err)
	}
	publicPage, more, err = store.ListChannelPostsByVisibilityPage(ctx, "ch", "public", 1, 1)
	if err != nil || more || len(publicPage) != 1 || publicPage[0].PostID != "public_old" {
		t.Fatalf("second public page = %#v more=%v err=%v", publicPage, more, err)
	}
	storedDefault, ok, err := store.GetChannelPostByID(ctx, "post_old", "ch")
	if err != nil || !ok || storedDefault.Visibility != "private" {
		t.Fatalf("legacy/default visibility = %#v ok=%v err=%v", storedDefault, ok, err)
	}

	if removed, err := store.DeleteChannelPost(ctx, "$old"); err != nil || !removed {
		t.Fatalf("DeleteChannelPost by event = (%v, %v), want (true, nil)", removed, err)
	}
	if removed, err := store.DeleteChannelPost(ctx, "$old"); err != nil || removed {
		t.Fatalf("DeleteChannelPost idempotent miss = (%v, %v), want (false, nil)", removed, err)
	}

	for _, comment := range []channelCommentRecord{
		{CommentID: "comment_old", PostID: "post_new_a", OriginServerTS: 10},
		{CommentID: "comment_new_a", PostID: "post_new_a", OriginServerTS: 20},
		{CommentID: "comment_new_b", PostID: "post_new_a", OriginServerTS: 20},
	} {
		if err := store.InsertChannelComment(ctx, comment); err != nil {
			t.Fatalf("InsertChannelComment(%s): %v", comment.CommentID, err)
		}
	}
	commentPage, more, err := store.ListChannelCommentsPage(ctx, "post_new_a", 0, 20, 0, "", 2)
	if err != nil {
		t.Fatalf("ListChannelCommentsPage: %v", err)
	}
	if !more || len(commentPage) != 2 || commentPage[0].CommentID != "comment_new_b" || commentPage[1].CommentID != "comment_new_a" {
		t.Fatalf("comment page = %#v more=%v", commentPage, more)
	}
}

func TestMemoryStoreChannelPostSettingsOverrideOnlyAfterExplicitUpdate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryStore()
	placeholder := channelPostRecord{PostID: "post", EventID: "$post", Visibility: "private"}
	if err := store.InsertChannelPost(ctx, placeholder); err != nil {
		t.Fatal(err)
	}
	projected := channelPostRecord{
		PostID: "post", EventID: "$post", Visibility: "public",
		CommentsEnabled: false, CommentsEnabledSet: true,
	}
	if err := store.InsertChannelPost(ctx, projected); err != nil {
		t.Fatal(err)
	}
	post, found, err := store.GetChannelPostByID(ctx, "post", "")
	if err != nil || !found || post.Visibility != "public" || post.CommentsEnabled {
		t.Fatalf("authoritative projection did not replace placeholder: post=%#v found=%v err=%v", post, found, err)
	}
	if err := store.ApplyChannelPostSettings(ctx, channelPostSettingsRecord{
		PostID: "post", PostEventID: "$post", Visibility: "private",
		CommentsEnabled: true, UpdatedAt: 2,
	}); err != nil {
		t.Fatalf("apply settings: %v", err)
	}
	if err := store.InsertChannelPost(ctx, projected); err != nil {
		t.Fatal(err)
	}
	post, found, err = store.GetChannelPostByID(ctx, "post", "")
	if err != nil || !found || post.Visibility != "private" || !post.CommentsEnabled || !post.SettingsUpdated {
		t.Fatalf("projection overwrote explicit settings: post=%#v found=%v err=%v", post, found, err)
	}
}
