package p2p

import (
	"context"
	"net/http"
	"testing"
	"time"

	matrixhistory "github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkmatrix"
	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/test"
)

func TestChannelContentBackfillProjectsReactionsAfterTargetsDespiteTimestamps(t *testing.T) {
	transport := &recordingTransport{roomID: "!channel:example.com"}
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)
	bootstrapService(t, service)

	ch := mustHandle[channel](t, service, "channels.create", map[string]any{
		"channel_id":       "post_channel",
		"name":             "Post Channel",
		"channel_type":     "post",
		"comments_enabled": true,
	})
	service.SetMatrixMessageReader(&fakeChannelBackfillReader{events: []matrixhistory.Event{
		{
			Type:           "m.reaction",
			EventID:        "$reaction-before-post:example.com",
			Sender:         "@alice:example.com",
			OriginServerTS: 500,
			Content: map[string]any{
				"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": "$post-one:example.com", "key": "like"},
			},
		},
		{
			Type:           "m.room.message",
			EventID:        "$post-one:example.com",
			Sender:         "@owner:example.com",
			OriginServerTS: 1000,
			Content: map[string]any{
				"p2p_kind": "channel_post",
				"post_id":  "post_one",
				"body":     "historical post",
				"msgtype":  "m.text",
			},
		},
	}})

	if err := service.backfillJoinedChannelContent(context.Background(), ch.RoomID, ch.ChannelID); err != nil {
		t.Fatalf("backfillJoinedChannelContent returned error: %v", err)
	}
	posts := mustHandle[map[string]any](t, service, "channels.posts.list", map[string]any{
		"channel_id": ch.ChannelID,
	})["posts"].([]channelPostRecord)
	if len(posts) != 1 || posts[0].PostID != "post_one" || posts[0].ReactionCount != 1 {
		t.Fatalf("expected backfilled reaction to resolve after post projection, got %#v", posts)
	}
}

func TestCompositeMatrixHistoryReaderKeepsChannelContentReader(t *testing.T) {
	transport := &recordingTransport{roomID: "!channel:example.com"}
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)
	bootstrapService(t, service)

	ch := mustHandle[channel](t, service, "channels.create", map[string]any{
		"channel_id":       "composite_channel",
		"room_id":          "!channel:example.com",
		"name":             "Composite Channel",
		"channel_type":     "post",
		"comments_enabled": true,
	})
	ordinary := &fakeOrdinaryMatrixHistoryReader{}
	channelReader := &fakeChannelBackfillReader{events: []matrixhistory.Event{
		{
			Type:           "m.room.message",
			EventID:        "$post-one:example.com",
			Sender:         "@owner:example.com",
			OriginServerTS: 1000,
			Content: map[string]any{
				"p2p_kind": "channel_post",
				"post_id":  "post_one",
				"body":     "composite historical post",
				"msgtype":  "m.text",
			},
		},
	}}
	service.SetMatrixMessageReader(NewCompositeMatrixHistoryReader(ordinary, channelReader))

	if err := service.backfillJoinedPostChannelContent(context.Background(), ch.RoomID, ch.ChannelID); err != nil {
		t.Fatalf("backfillJoinedPostChannelContent returned error: %v", err)
	}
	if ordinary.calls != 0 {
		t.Fatalf("channel backfill should not use ordinary message reader, calls=%d", ordinary.calls)
	}
	if channelReader.calls != 1 {
		t.Fatalf("channel backfill should use channel content reader once, calls=%d", channelReader.calls)
	}
	posts := mustHandle[map[string]any](t, service, "channels.posts.list", map[string]any{
		"channel_id": ch.ChannelID,
	})["posts"].([]channelPostRecord)
	if len(posts) != 1 || posts[0].PostID != "post_one" || posts[0].Body != "composite historical post" {
		t.Fatalf("expected composite reader to project historical post, got %#v", posts)
	}
}

func TestChannelJoinBackfillHandlesMatrixRateLimit(t *testing.T) {
	oldDelays := channelContentBackfillRateLimitRetryDelays
	channelContentBackfillRateLimitRetryDelays = []time.Duration{0}
	defer func() {
		channelContentBackfillRateLimitRetryDelays = oldDelays
	}()

	rateLimitErr := matrixhistory.StatusError{StatusCode: http.StatusTooManyRequests, Message: "matrix messages failed with status 429"}
	history := []matrixhistory.Event{
		{
			Type:           "m.room.message",
			EventID:        "$post-one:example.com",
			Sender:         "@owner:example.com",
			OriginServerTS: 1000,
			Content:        map[string]any{"p2p_kind": "channel_post", "post_id": "post_one", "body": "historical post", "msgtype": "m.text"},
		},
		{
			Type:           "m.room.message",
			EventID:        "$comment-one:example.com",
			Sender:         "@owner:example.com",
			OriginServerTS: 1100,
			Content:        map[string]any{"p2p_kind": "channel_comment", "post_id": "post_one", "comment_id": "comment_one", "body": "historical comment", "msgtype": "m.text"},
		},
		{
			Type:           "m.reaction",
			EventID:        "$reaction-post-one:example.com",
			Sender:         "@owner:example.com",
			OriginServerTS: 1200,
			Content:        map[string]any{"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": "$post-one:example.com", "key": "like"}},
		},
	}

	tests := []struct {
		name           string
		reader         *fakeChannelBackfillReader
		wantProjection bool
	}{
		{
			name:           "transient",
			reader:         &fakeChannelBackfillReader{responses: [][]matrixhistory.Event{nil, history}, errs: []error{rateLimitErr, nil}},
			wantProjection: true,
		},
		{
			name:   "persistent",
			reader: &fakeChannelBackfillReader{err: rateLimitErr},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			service := NewServiceWithTransport(Config{ServerName: "example.com"}, &recordingTransport{roomID: "!channel:example.com"})
			bootstrapService(t, service)
			ch := mustHandle[channel](t, service, "channels.create", map[string]any{
				"channel_id":       tc.name + "_rate_limited_channel",
				"room_id":          "!channel:example.com",
				"name":             "Rate Limited Channel",
				"channel_type":     "post",
				"comments_enabled": true,
			})
			service.SetMatrixMessageReader(tc.reader)

			joined := mustHandle[map[string]any](t, service, "channels.join", map[string]any{
				"room_id": ch.RoomID, "channel_id": ch.ChannelID, "user_id": "@alice:example.com",
			})
			if joined["status"] != "ok" {
				t.Fatalf("channels.join should remain successful when backfill is rate-limited, got %#v", joined)
			}
			if tc.reader.calls != 2 {
				t.Fatalf("backfill should retry Matrix rate limit once, calls=%d", tc.reader.calls)
			}

			posts := mustHandle[map[string]any](t, service, "channels.posts.list", map[string]any{
				"channel_id": ch.ChannelID,
			})["posts"].([]channelPostRecord)
			if tc.wantProjection {
				if len(posts) != 1 || posts[0].PostID != "post_one" || posts[0].CommentCount != 1 || posts[0].ReactionCount != 1 {
					t.Fatalf("transient rate limit should project post/comment/reaction history, got %#v", posts)
				}
			} else if len(posts) != 0 {
				t.Fatalf("persistent rate limit should skip historical projection, got %#v", posts)
			}
		})
	}
}

type fakeOrdinaryMatrixHistoryReader struct {
	calls int
}

func (r *fakeOrdinaryMatrixHistoryReader) ListOrdinaryMessages(ctx context.Context, roomID string, page matrixhistory.Page) (matrixhistory.MessagePageResult, error) {
	r.calls++
	return matrixhistory.MessagePageResult{}, nil
}

func TestChannelContentBackfillPersistsProjectionAfterReload(t *testing.T) {
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
		"channel_id":       "persisted_channel",
		"room_id":          "!persisted-channel:example.com",
		"name":             "Persisted Channel",
		"channel_type":     "post",
		"comments_enabled": true,
	})
	service.SetMatrixMessageReader(&fakeChannelBackfillReader{events: []matrixhistory.Event{
		{
			Type:           "m.room.message",
			EventID:        "$post-one:example.com",
			Sender:         "@owner:example.com",
			OriginServerTS: 1000,
			Content: map[string]any{
				"p2p_kind": "channel_post",
				"post_id":  "post_one",
				"body":     "persisted historical post",
				"msgtype":  "m.text",
			},
		},
		{
			Type:           "m.room.message",
			EventID:        "$comment-one:example.com",
			Sender:         "@owner:example.com",
			OriginServerTS: 1100,
			Content: map[string]any{
				"p2p_kind":   "channel_comment",
				"post_id":    "post_one",
				"comment_id": "comment_one",
				"body":       "persisted historical comment",
				"msgtype":    "m.text",
			},
		},
		{
			Type:           "m.reaction",
			EventID:        "$reaction-post-one:example.com",
			Sender:         "@owner:example.com",
			OriginServerTS: 1200,
			Content: map[string]any{
				"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": "$post-one:example.com", "key": "like"},
			},
		},
		{
			Type:           "m.reaction",
			EventID:        "$reaction-comment-one:example.com",
			Sender:         "@owner:example.com",
			OriginServerTS: 1300,
			Content: map[string]any{
				"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": "$comment-one:example.com", "key": "like"},
			},
		},
	}})
	if err := service.backfillJoinedChannelContent(ctx, ch.RoomID, ch.ChannelID); err != nil {
		t.Fatalf("backfillJoinedChannelContent returned error: %v", err)
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
	posts := mustHandle[map[string]any](t, reloaded, "channels.posts.list", map[string]any{
		"channel_id": ch.ChannelID,
	})["posts"].([]channelPostRecord)
	if len(posts) != 1 || posts[0].PostID != "post_one" || posts[0].CommentCount != 1 || posts[0].ReactionCount != 1 || !posts[0].ReactedByMe {
		t.Fatalf("expected persisted backfilled post counts after reload, got %#v", posts)
	}
	comments := mustHandle[map[string]any](t, reloaded, "channels.comments.list", map[string]any{
		"post_id": "post_one",
	})["comments"].([]channelCommentRecord)
	if len(comments) != 1 || comments[0].CommentID != "comment_one" || comments[0].ReactionCount != 1 || !comments[0].ReactedByMe {
		t.Fatalf("expected persisted backfilled comment counts after reload, got %#v", comments)
	}
}
