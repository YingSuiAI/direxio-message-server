package channels

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalktransport"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
)

func (m *ContentModule) reaction(action string) actionbase.Handler {
	return func(ctx context.Context, raw map[string]any) (any, *actionbase.Error) {
		return m.ToggleReaction(ctx, action, raw)
	}
}

func (m *ContentModule) ToggleReaction(ctx context.Context, action string, raw map[string]any) (any, *actionbase.Error) {
	params := actionbase.Params(raw)
	reactionName := fallback(params.String("reaction"), "like")
	targetType := "post"
	targetID := params.String("post_id")
	roomID := params.String("room_id")
	eventID := ""
	reactionEventID := ""
	channelID := params.String("channel_id")
	postID := params.String("post_id")
	commentID := params.String("comment_id")
	if action == actionCommentReaction {
		targetType, targetID = "comment", commentID
	}
	if targetID == "" {
		return nil, actionbase.BadRequest(targetType + "_id is required")
	}
	if targetType == "post" {
		post, ok, err := m.PostByID(ctx, targetID, channelID)
		if err != nil {
			return nil, actionbase.InternalError(err)
		}
		if !ok {
			return nil, actionbase.StatusError(http.StatusNotFound, "post not found")
		}
		eventID = post.EventID
		roomID = fallback(roomID, post.RoomID)
		channelID = fallback(channelID, post.ChannelID)
		postID = post.PostID
	} else {
		comment, ok, err := m.CommentByID(ctx, targetID, postID)
		if err != nil {
			return nil, actionbase.InternalError(err)
		}
		if !ok {
			return nil, actionbase.StatusError(http.StatusNotFound, "comment not found")
		}
		eventID = comment.EventID
		channelID = fallback(channelID, comment.ChannelID)
		postID = fallback(postID, comment.PostID)
		commentID = comment.CommentID
		roomID, err = m.RoomIDForComment(ctx, comment, roomID)
		if err != nil {
			return nil, actionbase.InternalError(err)
		}
	}
	if actionErr := m.requireJoined(ctx, roomID); actionErr != nil {
		return nil, actionErr
	}
	if m.store == nil {
		return nil, actionbase.InternalError(errors.New("channel content store is not configured"))
	}
	userID := m.owner().MXID
	existing, ok, err := m.store.GetReaction(ctx, targetType, targetID, reactionName, userID)
	if err != nil {
		return nil, actionbase.InternalError(err)
	}
	record := dirextalkdomain.ReactionRecord{
		TargetType: targetType, TargetID: targetID, ChannelID: channelID,
		PostID: postID, CommentID: commentID, Reaction: reactionName,
		UserID: userID, Active: true, CreatedAt: m.now().Format(time.RFC3339Nano),
	}
	if ok {
		record = existing
		record.Active = !existing.Active
	}
	if matrix := m.matrixPort(); matrix != nil && roomID != "" && eventID != "" {
		result, err := matrix.SendMessage(ctx, dirextalktransport.SendMessageRequest{
			SenderMXID: userID, RoomID: roomID, EventType: "m.reaction",
			MessageType: "m.reaction", Timestamp: m.now(),
			Content: map[string]any{
				"m.relates_to": map[string]any{
					"rel_type": "m.annotation", "event_id": eventID, "key": reactionName,
				},
				"channel_id": channelID, "post_id": postID, "comment_id": commentID,
				"reaction": reactionName, "active": record.Active,
			},
		})
		if err != nil {
			return nil, m.transportError(err)
		}
		reactionEventID = result.EventID
	}
	record.EventID = reactionEventID
	if err := m.store.UpsertReaction(ctx, record); err != nil {
		return nil, actionbase.InternalError(err)
	}
	count, err := m.store.CountActiveReactions(ctx, targetType, targetID, reactionName)
	if err != nil {
		return nil, actionbase.InternalError(err)
	}
	result := map[string]any{
		"post_id": record.PostID, "comment_id": record.CommentID,
		"channel_id": record.ChannelID, "reaction": record.Reaction,
		"active": record.Active, "reaction_count": count,
	}
	if m.conversation == nil {
		return nil, actionbase.InternalError(errors.New("channel content conversation port is not configured"))
	}
	if err := m.conversation.AttachOperation(ctx, result, action, "ok", roomID); err != nil {
		return nil, actionbase.InternalError(err)
	}
	return result, nil
}

func (m *ContentModule) EnrichPosts(ctx context.Context, posts []Post, ownerMXID string) {
	for i := range posts {
		if comments, err := m.listCommentsForPost(ctx, posts[i].PostID); err == nil {
			posts[i].CommentCount = int64(len(comments))
		}
		if m.store == nil {
			continue
		}
		if count, err := m.store.CountActiveReactions(ctx, "post", posts[i].PostID, "like"); err == nil {
			posts[i].ReactionCount = count
			posts[i].LikeCount = count
		}
		if ownerMXID != "" {
			if reaction, ok, err := m.store.GetReaction(ctx, "post", posts[i].PostID, "like", ownerMXID); err == nil && ok {
				posts[i].ReactedByMe = reaction.Active
			}
		}
		if count, err := m.store.CountActiveReactions(ctx, "post", posts[i].PostID, "favorite"); err == nil {
			posts[i].FavoriteCount = count
		}
		if ownerMXID != "" {
			if reaction, ok, err := m.store.GetReaction(ctx, "post", posts[i].PostID, "favorite", ownerMXID); err == nil && ok {
				posts[i].FavoritedByMe = reaction.Active
			}
		}
	}
}

func (m *ContentModule) EnrichComments(ctx context.Context, comments []Comment, ownerMXID string) {
	if m.store == nil {
		return
	}
	for i := range comments {
		if count, err := m.store.CountActiveReactions(ctx, "comment", comments[i].CommentID, "like"); err == nil {
			comments[i].ReactionCount = count
		}
		if ownerMXID != "" {
			if reaction, ok, err := m.store.GetReaction(ctx, "comment", comments[i].CommentID, "like", ownerMXID); err == nil && ok {
				comments[i].ReactedByMe = reaction.Active
			}
		}
	}
}

func (m *ContentModule) listCommentsForPost(ctx context.Context, postID string) ([]Comment, error) {
	if m.store == nil {
		return nil, errors.New("channel content store is not configured")
	}
	records, err := m.store.ListChannelComments(ctx, postID)
	if err != nil {
		return nil, err
	}
	return CommentsFromRecords(records), nil
}

func (m *ContentModule) MyReactions(ctx context.Context, _ map[string]any) (any, *actionbase.Error) {
	userID := m.owner().MXID
	if m.store == nil {
		return map[string]any{"reactions": []ReactionHistory{}}, nil
	}
	reactions, err := m.store.ListReactions(ctx, userID)
	if err != nil {
		return map[string]any{"reactions": []ReactionHistory{}}, nil
	}
	return map[string]any{"reactions": m.ReactionHistory(ctx, reactions, userID)}, nil
}

func (m *ContentModule) ReactionHistory(ctx context.Context, reactions []dirextalkdomain.ReactionRecord, ownerMXID string) []ReactionHistory {
	history := make([]ReactionHistory, 0, len(reactions))
	for _, reaction := range reactions {
		item := ReactionHistory{Reaction: reaction}
		channelID := strings.TrimSpace(reaction.ChannelID)
		postID := strings.TrimSpace(reaction.PostID)
		if strings.EqualFold(reaction.TargetType, "post") && postID == "" {
			postID = strings.TrimSpace(reaction.TargetID)
		}
		commentID := strings.TrimSpace(reaction.CommentID)
		if strings.EqualFold(reaction.TargetType, "comment") && commentID == "" {
			commentID = strings.TrimSpace(reaction.TargetID)
		}
		item.Channel = m.historyChannel(ctx, channelID)
		if postID != "" {
			if post, ok, err := m.PostByID(ctx, postID, channelID); err == nil && ok {
				posts := []Post{post}
				m.EnrichPosts(ctx, posts, ownerMXID)
				item.Post = &posts[0]
				channelID = fallback(channelID, posts[0].ChannelID)
			}
		}
		if commentID != "" {
			if comment, ok, err := m.CommentByID(ctx, commentID, postID); err == nil && ok {
				comments := []Comment{comment}
				m.EnrichComments(ctx, comments, ownerMXID)
				item.Comment = &comments[0]
				postID = fallback(postID, comments[0].PostID)
				channelID = fallback(channelID, comments[0].ChannelID)
			}
		}
		if item.Post == nil && postID != "" {
			if post, ok, err := m.PostByID(ctx, postID, channelID); err == nil && ok {
				posts := []Post{post}
				m.EnrichPosts(ctx, posts, ownerMXID)
				item.Post = &posts[0]
				channelID = fallback(channelID, posts[0].ChannelID)
			}
		}
		if item.Channel == nil {
			item.Channel = m.historyChannel(ctx, channelID)
		}
		history = append(history, item)
	}
	return history
}

func (m *ContentModule) historyChannel(ctx context.Context, channelID string) *Channel {
	if channelID == "" || m.channels == nil {
		return nil
	}
	channel, ok, err := m.channels.ByIDOrRoom(ctx, channelID, "")
	if err != nil || !ok {
		return nil
	}
	if enriched, err := m.channels.WithCurrentCounts(ctx, channel); err == nil {
		channel = enriched
	}
	return &channel
}

// ProjectionEvent is the protocol-neutral subset of a Matrix message event
// needed to project channel content. Root adapters remain responsible for
// decoding HeaderedEvent and identifying the content kind.
type ProjectionEvent struct {
	RoomID         string
	EventID        string
	SenderMXID     string
	OriginServerTS int64
	Content        map[string]any
	Body           string
	MessageType    string
}

func (m *ContentModule) ProjectPost(ctx context.Context, event ProjectionEvent) error {
	if m.store == nil {
		return errors.New("channel content store is not configured")
	}
	params := actionbase.Params(event.Content)
	postID := params.String("post_id")
	if postID == "" {
		postID = "post_" + strings.TrimPrefix(event.EventID, "$")
	}
	commentsEnabled := true
	if _, exists := event.Content["comments_enabled"]; exists {
		commentsEnabled = params.Bool("comments_enabled")
	}
	return m.store.InsertChannelPost(ctx, dirextalkdomain.ChannelPostRecord{
		PostID: postID, ChannelID: params.String("channel_id"), RoomID: event.RoomID,
		EventID: event.EventID, AuthorMXID: event.SenderMXID,
		AuthorName: params.String("sender_name"), Body: event.Body,
		MessageType: event.MessageType, MediaJSON: params.String("media_json"),
		Visibility:      dirextalkdomain.NormalizeChannelPostVisibility(params.String("visibility")),
		CommentsEnabled: commentsEnabled, CommentsEnabledSet: true,
		OriginServerTS: event.OriginServerTS,
	})
}

func (m *ContentModule) ProjectComment(ctx context.Context, event ProjectionEvent) error {
	if m.store == nil {
		return errors.New("channel content store is not configured")
	}
	params := actionbase.Params(event.Content)
	commentID := params.String("comment_id")
	if commentID == "" {
		commentID = "comment_" + strings.TrimPrefix(event.EventID, "$")
	}
	mentionsJSON := "[]"
	if raw, ok := event.Content["mentions_json"]; ok {
		if normalized, err := jsonArray(raw); err == nil {
			mentionsJSON = normalized
		}
	} else if raw, ok := event.Content["mentions"]; ok {
		if normalized, err := jsonArray(raw); err == nil {
			mentionsJSON = normalized
		}
	}
	return m.store.InsertChannelComment(ctx, dirextalkdomain.ChannelCommentRecord{
		CommentID: commentID, PostID: params.String("post_id"), ChannelID: params.String("channel_id"),
		EventID: event.EventID, AuthorMXID: event.SenderMXID, AuthorName: params.String("sender_name"),
		Body: event.Body, MessageType: event.MessageType, MediaJSON: params.String("media_json"),
		ReplyToCommentID:  params.String("reply_to_comment_id"),
		ReplyToAuthorMXID: params.String("reply_to_author_mxid"), MentionsJSON: mentionsJSON,
		OriginServerTS: event.OriginServerTS,
	})
}

func (m *ContentModule) ProjectReaction(ctx context.Context, event ProjectionEvent) error {
	if m.store == nil {
		return errors.New("channel content store is not configured")
	}
	params := actionbase.Params(event.Content)
	relatesTo, _ := event.Content["m.relates_to"].(map[string]any)
	relation := actionbase.Params(relatesTo)
	reactionName := fallback(relation.String("key"), fallback(params.String("reaction"), "like"))
	channelID := params.String("channel_id")
	postID := params.String("post_id")
	commentID := params.String("comment_id")
	targetType, targetID := "post", postID
	if commentID != "" {
		targetType, targetID = "comment", commentID
	}
	if targetID == "" {
		relatedEventID := relation.String("event_id")
		resolvedType, resolvedID, resolvedPostID, resolvedCommentID, resolvedChannelID, err := m.ReactionTargetByEventID(ctx, relatedEventID, channelID)
		if err != nil {
			return err
		}
		if resolvedID != "" {
			targetType, targetID, postID, commentID = resolvedType, resolvedID, resolvedPostID, resolvedCommentID
			channelID = fallback(channelID, resolvedChannelID)
		} else {
			targetID = relatedEventID
		}
	}
	if targetID == "" {
		return nil
	}
	active := true
	if _, ok := event.Content["active"]; ok {
		active = params.Bool("active")
	}
	createdAt := m.now()
	if event.OriginServerTS > 0 {
		createdAt = time.UnixMilli(event.OriginServerTS).UTC()
	}
	return m.store.UpsertReaction(ctx, dirextalkdomain.ReactionRecord{
		EventID:    event.EventID,
		TargetType: targetType, TargetID: targetID, ChannelID: channelID,
		PostID: postID, CommentID: commentID, Reaction: reactionName,
		UserID: event.SenderMXID, Active: active, CreatedAt: createdAt.Format(time.RFC3339Nano),
	})
}

func (m *ContentModule) RemoveProjectedEvent(ctx context.Context, eventID string) (bool, error) {
	if strings.TrimSpace(eventID) == "" {
		return false, nil
	}
	if m.store == nil {
		return false, errors.New("channel content store is not configured")
	}
	postRemoved, err := m.store.DeleteChannelPost(ctx, eventID)
	if err != nil {
		return false, err
	}
	commentRemoved, err := m.store.DeleteChannelComment(ctx, eventID)
	if err != nil {
		return false, err
	}
	reactionRemoved, err := m.store.DeactivateReactionByEventID(ctx, eventID)
	if err != nil {
		return false, err
	}
	return postRemoved || commentRemoved || reactionRemoved, nil
}
