package p2p

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/test"
)

func TestReactionTogglePersistsAndListsActiveReaction(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := NewServiceWithStore(ctx, withTestExternalAgent(Config{ServerName: "example.com"}), store)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapService(t, service)
	ch := mustHandle[channel](t, service, "channels.create", map[string]any{
		"channel_id":   "ch",
		"room_id":      "!channel:example.com",
		"name":         "Public Posts",
		"avatar_url":   "mxc://example.com/channel-avatar",
		"channel_type": "post",
	})
	post := mustHandle[channelPostRecord](t, service, "channels.posts.create", map[string]any{
		"channel_id":   ch.ChannelID,
		"body":         "image caption",
		"message_type": "m.image",
		"media_json":   `{"url":"mxc://example.com/post-image"}`,
	})

	first := mustHandle[map[string]any](t, service, "channels.post_reaction.toggle", map[string]any{
		"channel_id": ch.ChannelID,
		"post_id":    post.PostID,
		"reaction":   "like",
	})
	if first["active"] != true || int64Param(first["reaction_count"]) != 1 {
		t.Fatalf("expected first toggle to activate reaction, got %#v", first)
	}

	reloadedStore, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer reloadedStore.Close()
	reloaded, err := NewServiceWithStore(ctx, withTestExternalAgent(Config{ServerName: "example.com"}), reloadedStore)
	if err != nil {
		t.Fatal(err)
	}
	reactions := mustHandle[map[string]any](t, reloaded, "channels.my_reactions", nil)
	got := reactionHistoryPayloads(t, reactions)
	if len(got) != 1 {
		t.Fatalf("expected active reaction after reload, got %#v", reactions)
	}
	reaction := mapValue(t, got[0], "reaction")
	channelSnapshot := mapValue(t, got[0], "channel")
	postSnapshot := mapValue(t, got[0], "post")
	if reaction["post_id"] != post.PostID || reaction["active"] != true {
		t.Fatalf("expected active reaction record after reload, got %#v", got[0])
	}
	if channelSnapshot["name"] != "Public Posts" || channelSnapshot["avatar_url"] != "mxc://example.com/channel-avatar" || channelSnapshot["channel_type"] != "post" {
		t.Fatalf("expected channel snapshot in reaction history, got %#v", got[0])
	}
	if postSnapshot["post_id"] != post.PostID || postSnapshot["message_type"] != "m.image" || postSnapshot["body"] != "image caption" || postSnapshot["reaction_count"] != float64(1) || postSnapshot["reacted_by_me"] != true {
		t.Fatalf("expected post snapshot in reaction history, got %#v", got[0])
	}

	second := mustHandle[map[string]any](t, reloaded, "channels.post_reaction.toggle", map[string]any{
		"channel_id": ch.ChannelID,
		"post_id":    post.PostID,
		"reaction":   "like",
	})
	if second["active"] != false || int64Param(second["reaction_count"]) != 0 {
		t.Fatalf("expected second toggle to deactivate reaction, got %#v", second)
	}
}

func TestReactionToggleDeactivatesExistingReactionsAfterPostDeletion(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	bootstrapService(t, service)
	ch := mustHandle[channel](t, service, "channels.create", map[string]any{
		"channel_id": "ch_deleted_reactions",
		"room_id":    "!deleted-reactions:example.com",
		"name":       "Deleted reaction targets",
	})

	posts := make(map[string]channelPostRecord)
	for _, reaction := range []string{"like", "favorite"} {
		post := mustHandle[channelPostRecord](t, service, "channels.posts.create", map[string]any{
			"channel_id": ch.ChannelID,
			"body":       reaction + " target",
		})
		posts[reaction] = post
		mustHandle[map[string]any](t, service, "channels.post_reaction.toggle", map[string]any{
			"channel_id": ch.ChannelID,
			"post_id":    post.PostID,
			"reaction":   reaction,
		})
		mustHandle[map[string]any](t, service, "channels.posts.recall", map[string]any{
			"channel_id": ch.ChannelID,
			"post_id":    post.PostID,
		})
	}

	reactions := mustHandle[map[string]any](t, service, "channels.my_reactions", nil)
	if got := reactionHistoryPayloads(t, reactions); len(got) != 2 {
		t.Fatalf("expected deleted posts to leave two removable reaction records, got %#v", reactions)
	}
	if err := service.memberStore().UpsertMember(context.Background(), memberRecord{
		RoomID: ch.RoomID, ChannelID: ch.ChannelID, UserID: service.ownerMXID,
		Membership: "leave", Role: "owner",
	}); err != nil {
		t.Fatal(err)
	}
	if _, apiErr := service.Handle(context.Background(), "channels.post_reaction.toggle", map[string]any{
		"channel_id": ch.ChannelID,
		"post_id":    posts["like"].PostID,
		"reaction":   "like",
	}); apiErr == nil || apiErr.Status != http.StatusForbidden {
		t.Fatalf("stale reaction removal without joined membership error = %#v, want 403", apiErr)
	}
	if err := service.memberStore().UpsertMember(context.Background(), memberRecord{
		RoomID: ch.RoomID, ChannelID: ch.ChannelID, UserID: service.ownerMXID,
		Membership: "join", Role: "owner",
	}); err != nil {
		t.Fatal(err)
	}

	for reaction, post := range posts {
		result := mustHandle[map[string]any](t, service, "channels.post_reaction.toggle", map[string]any{
			"channel_id": ch.ChannelID,
			"post_id":    post.PostID,
			"reaction":   reaction,
		})
		if result["active"] != false || int64Param(result["reaction_count"]) != 0 {
			t.Fatalf("expected stale %s reaction to deactivate, got %#v", reaction, result)
		}
		idempotent := mustHandle[map[string]any](t, service, "channels.post_reaction.toggle", map[string]any{
			"channel_id": ch.ChannelID,
			"post_id":    post.PostID,
			"reaction":   reaction,
		})
		if idempotent["active"] != false || int64Param(idempotent["reaction_count"]) != 0 {
			t.Fatalf("expected stale %s reaction deactivation to be idempotent, got %#v", reaction, idempotent)
		}
	}

	reactions = mustHandle[map[string]any](t, service, "channels.my_reactions", nil)
	if got := reactionHistoryPayloads(t, reactions); len(got) != 0 {
		t.Fatalf("expected stale reactions removed from history, got %#v", reactions)
	}
	if _, apiErr := service.Handle(context.Background(), "channels.post_reaction.toggle", map[string]any{
		"channel_id": ch.ChannelID,
		"post_id":    "post_never_existed",
		"reaction":   "like",
	}); apiErr == nil || apiErr.Status != http.StatusNotFound {
		t.Fatalf("unknown reaction target error = %#v, want 404", apiErr)
	}
}

func TestCommentReactionTogglePersistsAndListsActiveReaction(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := NewServiceWithStore(ctx, withTestExternalAgent(Config{ServerName: "example.com"}), store)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapService(t, service)
	ch := mustHandle[channel](t, service, "channels.create", map[string]any{
		"channel_id":   "ch",
		"room_id":      "!channel:example.com",
		"name":         "Public Posts",
		"avatar_url":   "mxc://example.com/channel-avatar",
		"channel_type": "post",
	})
	post := mustHandle[channelPostRecord](t, service, "channels.posts.create", map[string]any{
		"channel_id": ch.ChannelID,
		"body":       "post body",
	})
	comment := mustHandle[channelCommentRecord](t, service, "channels.comments.create", map[string]any{
		"channel_id": ch.ChannelID,
		"post_id":    post.PostID,
		"body":       "comment",
	})

	first := mustHandle[map[string]any](t, service, "channels.comment_reaction.toggle", map[string]any{
		"channel_id": ch.ChannelID,
		"post_id":    post.PostID,
		"comment_id": comment.CommentID,
		"reaction":   "like",
	})
	if first["active"] != true || int64Param(first["reaction_count"]) != 1 || first["comment_id"] != comment.CommentID {
		t.Fatalf("expected first comment toggle to activate reaction, got %#v", first)
	}

	reloadedStore, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer reloadedStore.Close()
	reloaded, err := NewServiceWithStore(ctx, withTestExternalAgent(Config{ServerName: "example.com"}), reloadedStore)
	if err != nil {
		t.Fatal(err)
	}
	reactions := mustHandle[map[string]any](t, reloaded, "channels.my_reactions", nil)
	got := reactionHistoryPayloads(t, reactions)
	if len(got) != 1 {
		t.Fatalf("expected active comment reaction after reload, got %#v", reactions)
	}
	reaction := mapValue(t, got[0], "reaction")
	channelSnapshot := mapValue(t, got[0], "channel")
	postSnapshot := mapValue(t, got[0], "post")
	commentSnapshot := mapValue(t, got[0], "comment")
	if reaction["target_type"] != "comment" || reaction["comment_id"] != comment.CommentID || reaction["active"] != true {
		t.Fatalf("expected active comment reaction after reload, got %#v", got[0])
	}
	if channelSnapshot["name"] != "Public Posts" || channelSnapshot["avatar_url"] != "mxc://example.com/channel-avatar" {
		t.Fatalf("expected channel snapshot in comment reaction history, got %#v", got[0])
	}
	if postSnapshot["post_id"] != post.PostID || postSnapshot["body"] != "post body" {
		t.Fatalf("expected parent post snapshot in comment reaction history, got %#v", got[0])
	}
	if commentSnapshot["comment_id"] != comment.CommentID || commentSnapshot["body"] != "comment" || commentSnapshot["reaction_count"] != float64(1) || commentSnapshot["reacted_by_me"] != true {
		t.Fatalf("expected comment snapshot in reaction history, got %#v", got[0])
	}

	second := mustHandle[map[string]any](t, reloaded, "channels.comment_reaction.toggle", map[string]any{
		"channel_id": ch.ChannelID,
		"post_id":    post.PostID,
		"comment_id": comment.CommentID,
		"reaction":   "like",
	})
	if second["active"] != false || int64Param(second["reaction_count"]) != 0 {
		t.Fatalf("expected second comment toggle to deactivate reaction, got %#v", second)
	}
}

func TestChannelPostAndCommentListsExposeCountsMediaAndReactionState(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	bootstrapService(t, service)
	post := mustHandle[channelPostRecord](t, service, "channels.posts.create", map[string]any{
		"channel_id":   "ch",
		"body":         "post",
		"message_type": "m.image",
		"media_json":   `{"url":"mxc://example.com/post"}`,
	})
	comment := mustHandle[channelCommentRecord](t, service, "channels.comments.create", map[string]any{
		"channel_id":   "ch",
		"post_id":      post.PostID,
		"body":         "comment",
		"message_type": "m.image",
		"media_json":   `{"url":"mxc://example.com/comment"}`,
	})
	mustHandle[map[string]any](t, service, "channels.post_reaction.toggle", map[string]any{
		"channel_id": "ch",
		"post_id":    post.PostID,
	})
	mustHandle[map[string]any](t, service, "channels.comment_reaction.toggle", map[string]any{
		"channel_id": "ch",
		"post_id":    post.PostID,
		"comment_id": comment.CommentID,
		"reaction":   "like",
	})

	posts := mustHandle[map[string]any](t, service, "channels.posts.list", map[string]any{"channel_id": "ch"})["posts"].([]channelPostRecord)
	if len(posts) != 1 || posts[0].CommentCount != 1 || posts[0].ReactionCount != 1 || !posts[0].ReactedByMe || !strings.Contains(posts[0].MediaJSON, "mxc://example.com/post") {
		t.Fatalf("expected post list counts, reaction state, and media, got %#v", posts)
	}
	comments := mustHandle[map[string]any](t, service, "channels.comments.list", map[string]any{"post_id": post.PostID})["comments"].([]channelCommentRecord)
	if len(comments) != 1 || comments[0].ReactionCount != 1 || !comments[0].ReactedByMe || !strings.Contains(comments[0].MediaJSON, "mxc://example.com/comment") {
		t.Fatalf("expected comment list reaction state and media, got %#v", comments)
	}
}

func TestChannelPostCreateRequiresTitleBodyOrImageAndPreservesIndependentFields(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	bootstrapService(t, service)

	titleOnly := mustHandle[channelPostRecord](t, service, "channels.posts.create", map[string]any{
		"channel_id": "ch_content",
		"title":      "  Title only  ",
	})
	if titleOnly.Title != "Title only" || titleOnly.Body != "" {
		t.Fatalf("title-only create = %#v", titleOnly)
	}
	bodyOnly := mustHandle[channelPostRecord](t, service, "channels.posts.create", map[string]any{
		"channel_id": "ch_content",
		"body":       "  Body only  ",
	})
	if bodyOnly.Title != "" || bodyOnly.Body != "Body only" {
		t.Fatalf("body-only create = %#v", bodyOnly)
	}
	imageOnly := mustHandle[channelPostRecord](t, service, "channels.posts.create", map[string]any{
		"channel_id": "ch_content",
		"media_json": `{"url":"mxc://example.com/image"}`,
	})
	if imageOnly.Title != "" || imageOnly.Body != "" || !strings.Contains(imageOnly.MediaJSON, "mxc://example.com/image") {
		t.Fatalf("image-only create = %#v", imageOnly)
	}
	imageListOnly := mustHandle[channelPostRecord](t, service, "channels.posts.create", map[string]any{
		"channel_id": "ch_content",
		"media_json": `{"images":[{"mxc":"mxc://example.com/list-image"}]}`,
	})
	if !strings.Contains(imageListOnly.MediaJSON, "mxc://example.com/list-image") {
		t.Fatalf("image-list-only create = %#v", imageListOnly)
	}

	posts := mustHandle[map[string]any](t, service, "channels.posts.list", map[string]any{
		"channel_id": "ch_content",
	})["posts"].([]channelPostRecord)
	seen := make(map[string]channelPostRecord, len(posts))
	for _, post := range posts {
		seen[post.PostID] = post
	}
	if seen[titleOnly.PostID].Title != "Title only" || seen[titleOnly.PostID].Body != "" ||
		seen[bodyOnly.PostID].Title != "" || seen[bodyOnly.PostID].Body != "Body only" {
		t.Fatalf("authenticated list did not preserve independent title/body: %#v", posts)
	}

	for _, tc := range []struct {
		name   string
		params map[string]any
	}{
		{name: "missing", params: map[string]any{"channel_id": "ch_content"}},
		{name: "whitespace", params: map[string]any{"channel_id": "ch_content", "title": "  ", "body": "\n"}},
		{name: "empty media", params: map[string]any{"channel_id": "ch_content", "media_json": `{}`}},
		{name: "metadata only", params: map[string]any{"channel_id": "ch_content", "media_json": `{"info":{"mimetype":"image/png"}}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, apiErr := service.Handle(context.Background(), "channels.posts.create", tc.params); apiErr == nil || apiErr.Status != http.StatusBadRequest {
				t.Fatalf("empty create error = %#v, want 400", apiErr)
			}
		})
	}
}

func TestChannelPostVisibilityDefaultsPrivateAndAuthenticatedListOptionallyPaginates(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	bootstrapService(t, service)

	privatePost := mustHandle[channelPostRecord](t, service, "channels.posts.create", map[string]any{
		"channel_id": "ch_visibility",
		"body":       "private by default",
	})
	if privatePost.Visibility != "private" {
		t.Fatalf("default post visibility = %q, want private", privatePost.Visibility)
	}
	if _, apiErr := service.Handle(context.Background(), "channels.posts.create", map[string]any{
		"channel_id": "ch_visibility",
		"body":       "invalid visibility",
		"visibility": "friends",
	}); apiErr == nil || apiErr.Status != http.StatusBadRequest {
		t.Fatalf("invalid create visibility error = %#v, want 400", apiErr)
	}
	if _, apiErr := service.Handle(context.Background(), "channels.posts.create", map[string]any{
		"channel_id": "ch_visibility",
		"body":       "invalid visibility type",
		"visibility": true,
	}); apiErr == nil || apiErr.Status != http.StatusBadRequest {
		t.Fatalf("non-string create visibility error = %#v, want 400", apiErr)
	}

	publicPosts := make([]channelPostRecord, 0, 7)
	for i := 0; i < 7; i++ {
		publicPosts = append(publicPosts, mustHandle[channelPostRecord](t, service, "channels.posts.create", map[string]any{
			"channel_id": "ch_visibility",
			"body":       fmt.Sprintf("public %d", i),
			"visibility": "public",
		}))
	}
	target := publicPosts[len(publicPosts)-1]
	mustHandle[channelCommentRecord](t, service, "channels.comments.create", map[string]any{
		"channel_id": "ch_visibility",
		"post_id":    target.PostID,
		"body":       "comment",
	})
	mustHandle[map[string]any](t, service, "channels.post_reaction.toggle", map[string]any{
		"channel_id": "ch_visibility",
		"post_id":    target.PostID,
		"reaction":   "like",
	})
	mustHandle[map[string]any](t, service, "channels.post_reaction.toggle", map[string]any{
		"channel_id": "ch_visibility",
		"post_id":    target.PostID,
		"reaction":   "favorite",
	})

	legacy := mustHandle[map[string]any](t, service, "channels.posts.list", map[string]any{
		"channel_id": "ch_visibility",
	})
	if posts := legacy["posts"].([]channelPostRecord); len(posts) != 8 {
		t.Fatalf("legacy unfiltered list = %#v", legacy)
	}
	if _, apiErr := service.Handle(context.Background(), "channels.posts.list", map[string]any{
		"channel_id": "ch_visibility",
		"visibility": "public",
	}); apiErr == nil || apiErr.Status != http.StatusBadRequest {
		t.Fatalf("visibility on authenticated list error = %#v, want 400", apiErr)
	}

	first := mustHandle[map[string]any](t, service, "channels.posts.list", map[string]any{
		"channel_id": "ch_visibility",
		"page_size":  5,
	})
	firstPosts := first["posts"].([]channelPostRecord)
	if len(firstPosts) != 5 || first["has_more"] != true || first["page"] != int64(1) || first["page_size"] != int64(5) || first["next_page"] != int64(2) {
		t.Fatalf("first authenticated page = %#v", first)
	}
	second := mustHandle[map[string]any](t, service, "channels.posts.list", map[string]any{
		"channel_id": "ch_visibility",
		"page":       2,
	})
	secondPosts := second["posts"].([]channelPostRecord)
	if len(secondPosts) != 3 || second["has_more"] != false {
		t.Fatalf("second authenticated page = %#v", second)
	}
	seen := make(map[string]channelPostRecord, 8)
	for _, post := range append(firstPosts, secondPosts...) {
		seen[post.PostID] = post
	}
	if len(seen) != 8 || seen[privatePost.PostID].Visibility != "private" {
		t.Fatalf("authenticated pages did not preserve all posts: %#v", seen)
	}
	enriched := seen[target.PostID]
	if enriched.CommentCount != 1 || enriched.ReactionCount != 1 || enriched.LikeCount != 1 || enriched.FavoriteCount != 1 {
		t.Fatalf("authenticated post counts = %#v", enriched)
	}
}

func TestChannelPostVisibilityCanSwitchPublicAndPrivate(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	bootstrapService(t, service)
	ch := mustHandle[channel](t, service, "channels.create", map[string]any{
		"channel_id": "ch_visibility_switch",
		"room_id":    "!visibility-switch:example.com",
		"name":       "Visibility Switch",
	})
	post := mustHandle[channelPostRecord](t, service, "channels.posts.create", map[string]any{
		"channel_id": ch.ChannelID,
		"body":       "switch me",
	})
	otherPost := mustHandle[channelPostRecord](t, service, "channels.posts.create", map[string]any{
		"channel_id": ch.ChannelID,
		"body":       "comments stay enabled",
	})
	if !post.CommentsEnabled || !otherPost.CommentsEnabled {
		t.Fatalf("new posts must allow comments by default: post=%#v other=%#v", post, otherPost)
	}

	updated := mustHandle[channelPostRecord](t, service, "channels.posts.update", map[string]any{
		"channel_id":       ch.ChannelID,
		"post_id":          post.PostID,
		"visibility":       "public",
		"comments_enabled": false,
	})
	if updated.Visibility != "public" || updated.CommentsEnabled {
		t.Fatalf("updated post settings = %#v, want public with comments disabled", updated)
	}
	if _, apiErr := service.Handle(context.Background(), "channels.comments.create", map[string]any{
		"channel_id": ch.ChannelID, "post_id": post.PostID, "body": "blocked",
	}); apiErr == nil || apiErr.Status != http.StatusForbidden {
		t.Fatalf("disabled post comment error = %#v, want 403", apiErr)
	}
	mustHandle[channelCommentRecord](t, service, "channels.comments.create", map[string]any{
		"channel_id": ch.ChannelID, "post_id": otherPost.PostID, "body": "still allowed",
	})
	listed := mustHandle[map[string]any](t, service, "channels.posts.list", map[string]any{"channel_id": ch.ChannelID})["posts"].([]channelPostRecord)
	listedSettings := make(map[string]bool, len(listed))
	for _, item := range listed {
		listedSettings[item.PostID] = item.CommentsEnabled
	}
	if listedSettings[post.PostID] || !listedSettings[otherPost.PostID] {
		t.Fatalf("post list comments_enabled = %#v", listedSettings)
	}
	publicPage := mustHandle[map[string]any](t, service, "channels.public.posts.list", map[string]any{"room_id": ch.RoomID})
	publicPosts := publicPage["posts"].([]channelPostRecord)
	if len(publicPosts) != 1 || publicPosts[0].PostID != post.PostID || publicPosts[0].CommentsEnabled {
		t.Fatalf("public page after publish = %#v", publicPage)
	}

	// Replaying the immutable Matrix event must preserve the explicit
	// ProductCore visibility switch.
	mustInsertChannelPost(t, service, post)
	replayed := mustHandle[map[string]any](t, service, "channels.public.posts.list", map[string]any{"room_id": ch.RoomID})
	if posts := replayed["posts"].([]channelPostRecord); len(posts) != 1 || posts[0].Visibility != "public" {
		t.Fatalf("Matrix replay reset switched visibility: %#v", replayed)
	}

	updated = mustHandle[channelPostRecord](t, service, "channels.posts.update", map[string]any{
		"post_id":          post.PostID,
		"visibility":       "private",
		"comments_enabled": true,
	})
	if updated.Visibility != "private" || !updated.CommentsEnabled {
		t.Fatalf("updated post settings = %#v, want private with comments enabled", updated)
	}
	mustHandle[channelCommentRecord](t, service, "channels.comments.create", map[string]any{
		"channel_id": ch.ChannelID, "post_id": post.PostID, "body": "enabled again",
	})
	privatePage := mustHandle[map[string]any](t, service, "channels.public.posts.list", map[string]any{"room_id": ch.RoomID})
	if posts := privatePage["posts"].([]channelPostRecord); len(posts) != 0 {
		t.Fatalf("private post remained publicly listed: %#v", privatePage)
	}

	for _, params := range []map[string]any{
		{"post_id": post.PostID},
		{"post_id": post.PostID, "visibility": "friends"},
		{"post_id": post.PostID, "visibility": true},
		{"post_id": post.PostID, "comments_enabled": "false"},
	} {
		if _, apiErr := service.Handle(context.Background(), "channels.posts.update", params); apiErr == nil || apiErr.Status != http.StatusBadRequest {
			t.Fatalf("invalid visibility update %#v error = %#v, want 400", params, apiErr)
		}
	}
}

func TestPublicChannelPostsAreReadableButNonMembersCannotInteract(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	bootstrapService(t, service)
	ch := mustHandle[channel](t, service, "channels.create", map[string]any{
		"channel_id": "ch_public_posts",
		"room_id":    "!public-posts:example.com",
		"name":       "Public Posts",
		"visibility": "private",
	})
	publicPost := mustHandle[channelPostRecord](t, service, "channels.posts.create", map[string]any{
		"channel_id": ch.ChannelID,
		"title":      "Public title",
		"body":       "visible",
		"visibility": "public",
	})
	mustHandle[channelPostRecord](t, service, "channels.posts.create", map[string]any{
		"channel_id": ch.ChannelID,
		"body":       "hidden",
		"visibility": "private",
	})
	comment := mustHandle[channelCommentRecord](t, service, "channels.comments.create", map[string]any{
		"channel_id": ch.ChannelID,
		"post_id":    publicPost.PostID,
		"body":       "member comment",
	})

	service.mu.Lock()
	service.ownerMXID = "@visitor:example.com"
	service.mu.Unlock()

	publicPage := mustHandle[map[string]any](t, service, "channels.public.posts.list", map[string]any{
		"room_id": ch.RoomID,
	})
	posts := publicPage["posts"].([]channelPostRecord)
	if len(posts) != 1 || posts[0].PostID != publicPost.PostID || posts[0].Title != "Public title" || posts[0].Visibility != "public" ||
		posts[0].CommentCount != 1 || publicPage["page_size"] != int64(5) {
		t.Fatalf("unexpected public posts page %#v", publicPage)
	}
	if posts[0].ReactedByMe || posts[0].FavoritedByMe {
		t.Fatalf("public reads must not expose server-owner personalized state: %#v", posts[0])
	}

	for _, tc := range []struct {
		name   string
		action string
		params map[string]any
	}{
		{
			name:   "comment",
			action: "channels.comments.create",
			params: map[string]any{"channel_id": ch.ChannelID, "post_id": publicPost.PostID, "body": "blocked"},
		},
		{
			name:   "like",
			action: "channels.post_reaction.toggle",
			params: map[string]any{"channel_id": ch.ChannelID, "post_id": publicPost.PostID, "reaction": "like"},
		},
		{
			name:   "favorite",
			action: "channels.post_reaction.toggle",
			params: map[string]any{"channel_id": ch.ChannelID, "post_id": publicPost.PostID, "reaction": "favorite"},
		},
		{
			name:   "comment like",
			action: "channels.comment_reaction.toggle",
			params: map[string]any{"channel_id": ch.ChannelID, "post_id": publicPost.PostID, "comment_id": comment.CommentID, "reaction": "like"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, apiErr := service.Handle(context.Background(), tc.action, tc.params); apiErr == nil || apiErr.Status != http.StatusForbidden {
				t.Fatalf("%s error = %#v, want 403", tc.action, apiErr)
			}
		})
	}

	if _, apiErr := service.Handle(context.Background(), "channels.post_reaction.toggle", map[string]any{
		"channel_id": ch.ChannelID,
		"post_id":    "post_missing",
		"reaction":   "like",
	}); apiErr == nil || apiErr.Status != http.StatusNotFound {
		t.Fatalf("unknown reaction target error = %#v, want 404", apiErr)
	}
}

func TestPublicChannelReadsRefreshMemberCountFromMembership(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	bootstrapService(t, service)
	ch := mustHandle[channel](t, service, "channels.create", map[string]any{
		"channel_id": "ch_public",
		"room_id":    "!public:example.com",
		"name":       "Public",
		"visibility": "public",
	})
	ch.MemberCount = 0
	if err := service.store.UpsertChannel(context.Background(), ch); err != nil {
		t.Fatalf("store stale channel count: %v", err)
	}

	detail := mustHandle[channel](t, service, "channels.public.get", map[string]any{
		"channel_id": ch.ChannelID,
	})
	if detail.MemberCount != 1 {
		t.Fatalf("expected public detail member_count to refresh from joined members, got %#v", detail)
	}

	list := mustHandle[map[string]any](t, service, "users.public_channels", map[string]any{
		"user_id": "@owner:example.com",
	})
	channels := list["channels"].([]channel)
	if len(channels) != 1 || channels[0].MemberCount != 1 {
		t.Fatalf("expected users.public_channels member_count to match public detail, got %#v", list)
	}
}

func TestChannelCommentCreateRequiresExistingPost(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	bootstrapService(t, service)
	post := mustHandle[channelPostRecord](t, service, "channels.posts.create", map[string]any{
		"channel_id": "ch",
		"body":       "post",
	})

	for _, tc := range []struct {
		name       string
		params     map[string]any
		wantStatus int
	}{
		{name: "missing post ID", params: map[string]any{"channel_id": "ch", "body": "orphan"}, wantStatus: http.StatusBadRequest},
		{name: "unknown post", params: map[string]any{"channel_id": "ch", "post_id": "post_missing", "body": "orphan"}, wantStatus: http.StatusNotFound},
		{name: "wrong channel", params: map[string]any{"channel_id": "other", "post_id": post.PostID, "body": "orphan"}, wantStatus: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, apiErr := service.Handle(context.Background(), "channels.comments.create", tc.params); apiErr == nil || apiErr.Status != tc.wantStatus {
				t.Fatalf("expected status %d, got %#v", tc.wantStatus, apiErr)
			}
		})
	}

	comment := mustHandle[channelCommentRecord](t, service, "channels.comments.create", map[string]any{
		"channel_id": "ch",
		"post_id":    post.PostID,
		"body":       "valid",
	})
	if comment.PostID != post.PostID || comment.Body != "valid" {
		t.Fatalf("expected valid comment on existing post, got %#v", comment)
	}
}

func TestRecallChannelPostAndCommentHideFromLists(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	bootstrapService(t, service)
	post := mustHandle[channelPostRecord](t, service, "channels.posts.create", map[string]any{
		"channel_id": "ch",
		"body":       "recall post",
	})
	comment := mustHandle[channelCommentRecord](t, service, "channels.comments.create", map[string]any{
		"channel_id": "ch",
		"post_id":    post.PostID,
		"body":       "recall comment",
	})

	mustHandle[map[string]any](t, service, "channels.posts.recall", map[string]any{"post_id": post.PostID})
	posts := mustHandle[map[string]any](t, service, "channels.posts.list", map[string]any{"channel_id": "ch"})
	if got, ok := posts["posts"].([]channelPostRecord); !ok || len(got) != 0 {
		t.Fatalf("expected recalled post hidden, got %#v", posts)
	}

	mustHandle[map[string]any](t, service, "channels.comments.recall", map[string]any{"comment_id": comment.CommentID, "post_id": post.PostID})
	comments := mustHandle[map[string]any](t, service, "channels.comments.list", map[string]any{"post_id": post.PostID})
	if got, ok := comments["comments"].([]channelCommentRecord); !ok || len(got) != 0 {
		t.Fatalf("expected recalled comment hidden, got %#v", comments)
	}
}

func TestRecallChannelContentRequiresAuthorOrChannelOwner(t *testing.T) {
	ctx := context.Background()
	service := NewService(Config{ServerName: "example.com"})
	bootstrapService(t, service)
	ch := mustHandle[channel](t, service, "channels.create", map[string]any{
		"channel_id": "ch_auth",
		"room_id":    "!ch-auth:example.com",
		"name":       "Auth Channel",
	})
	post := mustHandle[channelPostRecord](t, service, "channels.posts.create", map[string]any{
		"channel_id": ch.ChannelID,
		"room_id":    ch.RoomID,
		"body":       "owner post",
	})
	if err := service.saveMember(ctx, memberRecord{
		RoomID:      ch.RoomID,
		ChannelID:   ch.ChannelID,
		UserID:      "@bob:example.com",
		DisplayName: "Bob",
		Membership:  "join",
		Role:        "member",
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.saveMember(ctx, memberRecord{
		RoomID:      ch.RoomID,
		ChannelID:   ch.ChannelID,
		UserID:      "@alice:example.com",
		DisplayName: "Alice",
		Membership:  "join",
		Role:        "member",
	}); err != nil {
		t.Fatal(err)
	}

	setServiceOwnerForTest(service, "@bob:example.com", "Bob")
	if _, apiErr := service.Handle(ctx, "channels.posts.recall", map[string]any{"post_id": post.PostID}); apiErr == nil || apiErr.Status != http.StatusForbidden {
		t.Fatalf("expected member recall of owner post to be forbidden, got %#v", apiErr)
	}
	posts := mustHandle[map[string]any](t, service, "channels.posts.list", map[string]any{"channel_id": ch.ChannelID})
	if got := posts["posts"].([]channelPostRecord); len(got) != 1 || got[0].PostID != post.PostID {
		t.Fatalf("expected forbidden post recall to keep post, got %#v", got)
	}

	ownComment := mustHandle[channelCommentRecord](t, service, "channels.comments.create", map[string]any{
		"channel_id": ch.ChannelID,
		"post_id":    post.PostID,
		"room_id":    ch.RoomID,
		"body":       "bob comment",
	})
	if _, apiErr := service.Handle(ctx, "channels.comments.recall", map[string]any{"comment_id": ownComment.CommentID, "post_id": post.PostID}); apiErr != nil {
		t.Fatalf("expected author to recall own comment, got %#v", apiErr)
	}

	foreignComment := channelCommentRecord{
		CommentID:      "comment_alice",
		PostID:         post.PostID,
		ChannelID:      ch.ChannelID,
		EventID:        "$comment_alice:example.com",
		AuthorMXID:     "@alice:example.com",
		AuthorName:     "Alice",
		Body:           "alice comment",
		MessageType:    "text",
		MentionsJSON:   "[]",
		OriginServerTS: time.Now().UTC().UnixMilli(),
	}
	mustInsertChannelComment(t, service, foreignComment)
	if _, apiErr := service.Handle(ctx, "channels.comments.recall", map[string]any{"comment_id": foreignComment.CommentID, "post_id": post.PostID}); apiErr == nil || apiErr.Status != http.StatusForbidden {
		t.Fatalf("expected member recall of another member comment to be forbidden, got %#v", apiErr)
	}
	comments := mustHandle[map[string]any](t, service, "channels.comments.list", map[string]any{"post_id": post.PostID})
	if got := comments["comments"].([]channelCommentRecord); len(got) != 1 || got[0].CommentID != foreignComment.CommentID {
		t.Fatalf("expected forbidden comment recall to keep comment, got %#v", got)
	}

	setServiceOwnerForTest(service, "@owner:example.com", "")
	if _, apiErr := service.Handle(ctx, "channels.comments.recall", map[string]any{"comment_id": foreignComment.CommentID, "post_id": post.PostID}); apiErr != nil {
		t.Fatalf("expected channel owner to recall member comment, got %#v", apiErr)
	}
}

func TestChannelCommentPersistsReplyAndMentions(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	bootstrapService(t, service)
	post := mustHandle[channelPostRecord](t, service, "channels.posts.create", map[string]any{
		"channel_id": "ch",
		"body":       "post",
	})
	parent := mustHandle[channelCommentRecord](t, service, "channels.comments.create", map[string]any{
		"channel_id": "ch",
		"post_id":    post.PostID,
		"body":       "parent",
	})

	reply := mustHandle[channelCommentRecord](t, service, "channels.comments.create", map[string]any{
		"channel_id":           "ch",
		"post_id":              post.PostID,
		"body":                 "reply @alice",
		"reply_to_comment_id":  parent.CommentID,
		"reply_to_author_mxid": parent.AuthorMXID,
		"mentions": []any{
			map[string]any{"user_id": "@alice:remote.example", "display_name": "Alice"},
		},
	})
	if reply.ReplyToCommentID != parent.CommentID || !strings.Contains(reply.MentionsJSON, "@alice:remote.example") {
		t.Fatalf("expected reply metadata on created comment, got %#v", reply)
	}
	comments := mustHandle[map[string]any](t, service, "channels.comments.list", map[string]any{"post_id": post.PostID})
	got := comments["comments"].([]channelCommentRecord)
	if len(got) != 2 || got[1].ReplyToCommentID != parent.CommentID || !strings.Contains(got[1].MentionsJSON, "@alice:remote.example") {
		t.Fatalf("expected reply metadata in comments list, got %#v", got)
	}
}

func TestStoredChannelCountsSurviveReload(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := NewServiceWithStore(ctx, withTestExternalAgent(Config{ServerName: "example.com"}), store)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapService(t, service)
	ch := mustHandle[channel](t, service, "channels.create", map[string]any{
		"channel_id":  "stored-counts",
		"room_id":     "!stored-counts:example.com",
		"name":        "Stored Counts",
		"visibility":  "public",
		"join_policy": "approval",
	})
	if err := service.saveMember(ctx, memberRecord{
		RoomID:      ch.RoomID,
		ChannelID:   ch.ChannelID,
		UserID:      "@bob:remote.example",
		DisplayName: "Bob",
		Domain:      "remote.example",
		Membership:  "join",
		Role:        "member",
	}); err != nil {
		t.Fatal(err)
	}
	request := mustHandle[map[string]any](t, service, "channels.public.join_request", map[string]any{
		"channel_id":   ch.ChannelID,
		"room_id":      ch.RoomID,
		"user_mxid":    "@alice:remote.example",
		"display_name": "Alice",
	})
	if request["status"] != "pending" {
		t.Fatalf("expected pending join request, got %#v", request)
	}

	reloadedStore, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer reloadedStore.Close()
	reloaded, err := NewServiceWithStore(ctx, withTestExternalAgent(Config{ServerName: "example.com"}), reloadedStore)
	if err != nil {
		t.Fatal(err)
	}
	channels := mustHandle[map[string]any](t, reloaded, "channels.list", nil)["channels"].([]channel)
	restored := findChannel(channels, ch.ChannelID)
	if restored.ChannelID == "" || restored.MemberCount != 2 || restored.PendingJoinCount != 1 {
		t.Fatalf("expected stored channel counts to survive reload, got %#v", channels)
	}
	detail := mustHandle[channel](t, reloaded, "channels.public.get", map[string]any{"channel_id": ch.ChannelID})
	if detail.MemberCount != 2 || detail.PendingJoinCount != 1 {
		t.Fatalf("expected public channel detail counts to survive reload, got %#v", detail)
	}
}

func TestMyChannelCommentsListsOwnerHistory(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	post := mustHandle[channelPostRecord](t, service, "channels.posts.create", map[string]any{
		"channel_id": "ch",
		"body":       "post",
	})
	comment := mustHandle[channelCommentRecord](t, service, "channels.comments.create", map[string]any{
		"channel_id": "ch",
		"post_id":    post.PostID,
		"body":       "my comment",
	})

	history := mustHandle[map[string]any](t, service, "channels.my_comments", nil)
	comments := jsonList(t, history["comments"])
	if len(comments) != 1 || comments[0]["comment_id"] != comment.CommentID || comments[0]["body"] != "my comment" {
		t.Fatalf("expected my channel comment history, got %#v", history)
	}
}
