package channels

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strings"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkstate"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalktransport"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
)

func (m *ContentModule) CreatePost(ctx context.Context, raw map[string]any) (any, *actionbase.Error) {
	params := actionbase.Params(raw)
	now := m.now()
	channelID := fallback(params.String("channel_id"), "channel")
	postID := "post_" + m.token("post")
	owner := m.owner()
	roomID, actionErr := m.roomIDForChannel(ctx, channelID, params.String("room_id"))
	if actionErr != nil {
		return nil, actionErr
	}
	body := fallback(params.String("body"), params.String("content"))
	messageType := fallback(params.String("message_type"), "text")
	if value, exists := raw["visibility"]; exists && value != nil {
		if _, ok := value.(string); !ok {
			return nil, actionbase.BadRequest("visibility must be public or private")
		}
	}
	visibility, ok := validatedPostVisibility(params.String("visibility"))
	if !ok {
		return nil, actionbase.BadRequest("visibility must be public or private")
	}
	mediaJSON, media, err := mediaPayload(params.Raw("media_json"))
	if err != nil {
		return nil, actionbase.BadRequest("media_json is invalid")
	}
	eventID := m.eventID(postID)
	originServerTS := now.UnixMilli()
	if matrix := m.matrixPort(); matrix != nil && roomID != "" {
		content := channelMessageContent("channel_post", body, messageType, mediaJSON, media)
		content["channel_id"] = channelID
		content["post_id"] = postID
		content["visibility"] = visibility
		content["comments_enabled"] = true
		result, err := matrix.SendMessage(ctx, dirextalktransport.SendMessageRequest{
			SenderMXID: owner.MXID, RoomID: roomID, MessageType: messageType,
			Timestamp: now, Content: content,
		})
		if err != nil {
			return nil, m.transportError(err)
		}
		eventID = result.EventID
		originServerTS = result.OriginServerTS
	}
	post := Post{
		PostID: postID, ChannelID: channelID, RoomID: roomID, EventID: eventID,
		AuthorMXID: owner.MXID, AuthorName: owner.DisplayName, Body: body,
		MessageType: messageType, MediaJSON: mediaJSON, Visibility: visibility, CommentsEnabled: true,
		OriginServerTS: originServerTS,
	}
	if m.store == nil {
		return nil, actionbase.InternalError(errors.New("channel content store is not configured"))
	}
	if err := m.store.InsertChannelPost(ctx, postRecord(post)); err != nil {
		return nil, actionbase.InternalError(err)
	}
	if err := m.attachPostOperation(ctx, &post, actionPostsCreate, "ok", roomID); err != nil {
		return nil, actionbase.InternalError(err)
	}
	return post, nil
}

// UpdatePost changes the mutable ProductCore settings for an
// existing Matrix-backed post. The original timeline event remains immutable;
// public listing and comment creation read this durable settings projection.
func (m *ContentModule) UpdatePost(ctx context.Context, raw map[string]any) (any, *actionbase.Error) {
	params := actionbase.Params(raw)
	postID := params.String("post_id")
	if postID == "" {
		return nil, actionbase.BadRequest("post_id is required")
	}
	var visibility *string
	if value, exists := raw["visibility"]; exists {
		if _, ok := value.(string); !ok {
			return nil, actionbase.BadRequest("visibility must be public or private")
		}
		normalized, ok := validatedPostVisibility(params.String("visibility"))
		if !ok || strings.TrimSpace(params.String("visibility")) == "" {
			return nil, actionbase.BadRequest("visibility must be public or private")
		}
		visibility = &normalized
	}
	var commentsEnabled *bool
	if value, exists := raw["comments_enabled"]; exists {
		enabled, ok := value.(bool)
		if !ok {
			return nil, actionbase.BadRequest("comments_enabled must be boolean")
		}
		commentsEnabled = &enabled
	}
	if visibility == nil && commentsEnabled == nil {
		return nil, actionbase.BadRequest("visibility or comments_enabled is required")
	}
	if m.store == nil {
		return nil, actionbase.InternalError(errors.New("channel content store is not configured"))
	}
	record, found, err := m.store.GetChannelPostByID(ctx, postID, params.String("channel_id"))
	if err != nil {
		return nil, actionbase.InternalError(err)
	}
	if !found {
		return nil, actionbase.StatusError(http.StatusNotFound, "post not found")
	}
	if m.config.AuthorizeRecall != nil {
		if actionErr := m.config.AuthorizeRecall(ctx, record.RoomID, record.AuthorMXID); actionErr != nil {
			return nil, actionErr
		}
	}
	now := m.now()
	effectiveVisibility := dirextalkdomain.NormalizeChannelPostVisibility(record.Visibility)
	effectiveCommentsEnabled := record.CommentsEnabled
	if visibility != nil {
		effectiveVisibility = *visibility
	}
	if commentsEnabled != nil {
		effectiveCommentsEnabled = *commentsEnabled
	}
	settingsState := dirextalkstate.ChannelPostSettings(dirextalkstate.ChannelPostSettingsInput{
		PostID: record.PostID, ChannelID: record.ChannelID, RoomID: record.RoomID,
		PostEventID: record.EventID, Visibility: effectiveVisibility,
		CommentsEnabled: effectiveCommentsEnabled, UpdatedAt: now,
	})
	if matrix := m.matrixPort(); matrix != nil && record.RoomID != "" {
		if err := matrix.SendStateEvent(ctx, dirextalktransport.SendStateEventRequest{
			RoomID: record.RoomID, SenderMXID: m.owner().MXID,
			Event: dirextalktransport.RoomStateEvent{
				Type: settingsState.Type, StateKey: settingsState.StateKey, Content: settingsState.Content,
			},
			Timestamp: now,
		}); err != nil {
			return nil, m.transportError(err)
		}
	}
	if err := m.store.ApplyChannelPostSettings(ctx, dirextalkdomain.ChannelPostSettingsRecord{
		PostID: record.PostID, ChannelID: record.ChannelID, RoomID: record.RoomID,
		PostEventID: record.EventID, Visibility: effectiveVisibility,
		CommentsEnabled: effectiveCommentsEnabled, UpdatedAt: now.UnixMilli(),
	}); err != nil {
		return nil, actionbase.InternalError(err)
	}
	record.Visibility = effectiveVisibility
	record.CommentsEnabled = effectiveCommentsEnabled
	record.CommentsEnabledSet = true
	post := postFromRecord(record)
	posts := []Post{post}
	m.EnrichPosts(ctx, posts, m.owner().MXID)
	post = posts[0]
	if err := m.attachPostOperation(ctx, &post, actionPostUpdate, "ok", record.RoomID); err != nil {
		return nil, actionbase.InternalError(err)
	}
	return post, nil
}

func (m *ContentModule) Posts(ctx context.Context, raw map[string]any) (any, *actionbase.Error) {
	channelID := actionbase.Params(raw).String("channel_id")
	if m.store == nil {
		return map[string]any{"posts": []Post{}}, nil
	}
	if _, exists := raw["visibility"]; exists {
		return nil, actionbase.BadRequest("visibility is only supported by channels.public.posts.list")
	}
	if _, hasPage := raw["page"]; hasPage {
		return m.postsOffsetPage(ctx, channelID, raw, m.owner().MXID)
	}
	if _, hasPageSize := raw["page_size"]; hasPageSize {
		return m.postsOffsetPage(ctx, channelID, raw, m.owner().MXID)
	}
	records, err := m.store.ListChannelPosts(ctx, channelID)
	if err != nil {
		return map[string]any{"posts": []Post{}}, nil
	}
	posts := PostsFromRecords(records)
	m.EnrichPosts(ctx, posts, m.owner().MXID)
	return map[string]any{"posts": posts}, nil
}

func (m *ContentModule) postsOffsetPage(
	ctx context.Context,
	channelID string,
	raw map[string]any,
	ownerMXID string,
) (map[string]any, *actionbase.Error) {
	page, pageSize, offset, actionErr := postPageParams(raw)
	if actionErr != nil {
		return nil, actionErr
	}
	records, hasMore, err := m.store.ListChannelPostsOffsetPage(ctx, channelID, offset, int(pageSize))
	if err != nil {
		return nil, actionbase.InternalError(err)
	}
	posts := PostsFromRecords(records)
	m.EnrichPosts(ctx, posts, ownerMXID)
	result := map[string]any{
		"posts": posts, "page": page, "page_size": pageSize, "has_more": hasMore,
	}
	if hasMore {
		result["next_page"] = page + 1
	}
	return result, nil
}

// PublicPosts returns a non-personalized page containing public posts only.
// The root service verifies that the target channel exists before calling it.
func (m *ContentModule) PublicPosts(ctx context.Context, channelID string, raw map[string]any) (map[string]any, *actionbase.Error) {
	if m.store == nil {
		return map[string]any{
			"posts": []Post{}, "visibility": dirextalkdomain.ChannelPostVisibilityPublic,
			"page": int64(1), "page_size": int64(5), "has_more": false,
		}, nil
	}
	return m.postsByVisibilityPage(
		ctx,
		strings.TrimSpace(channelID),
		dirextalkdomain.ChannelPostVisibilityPublic,
		raw,
		"",
	)
}

func (m *ContentModule) postsByVisibilityPage(
	ctx context.Context,
	channelID, visibility string,
	raw map[string]any,
	ownerMXID string,
) (map[string]any, *actionbase.Error) {
	page, pageSize, offset, actionErr := postPageParams(raw)
	if actionErr != nil {
		return nil, actionErr
	}
	records, hasMore, err := m.store.ListChannelPostsByVisibilityPage(ctx, channelID, visibility, offset, int(pageSize))
	if err != nil {
		return nil, actionbase.InternalError(err)
	}
	posts := PostsFromRecords(records)
	m.EnrichPosts(ctx, posts, ownerMXID)
	result := map[string]any{
		"posts": posts, "visibility": visibility, "page": page,
		"page_size": pageSize, "has_more": hasMore,
	}
	if hasMore {
		result["next_page"] = page + 1
	}
	return result, nil
}

func postPageParams(raw map[string]any) (page, pageSize, offset int64, actionErr *actionbase.Error) {
	params := actionbase.Params(raw)
	page = params.Int64("page")
	if page <= 0 {
		page = 1
	}
	pageSize = params.Int64("page_size")
	if pageSize <= 0 {
		pageSize = params.Int64("limit")
	}
	if pageSize <= 0 {
		pageSize = 5
	}
	if pageSize > 100 {
		pageSize = 100
	}
	if page > math.MaxInt64/pageSize {
		return 0, 0, 0, actionbase.BadRequest("page is too large")
	}
	return page, pageSize, (page - 1) * pageSize, nil
}

func validatedPostVisibility(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return dirextalkdomain.ChannelPostVisibilityPrivate, true
	case dirextalkdomain.ChannelPostVisibilityPrivate:
		return dirextalkdomain.ChannelPostVisibilityPrivate, true
	case dirextalkdomain.ChannelPostVisibilityPublic:
		return dirextalkdomain.ChannelPostVisibilityPublic, true
	default:
		return "", false
	}
}

func (m *ContentModule) PostPage(ctx context.Context, channelID string, fromTS, snapshotTS, cursorTS int64, cursorID string, limit int) ([]Post, bool, error) {
	if m.store == nil {
		return nil, false, errors.New("channel content store is not configured")
	}
	records, hasMore, err := m.store.ListChannelPostsPage(ctx, channelID, fromTS, snapshotTS, cursorTS, cursorID, limit)
	if err != nil {
		return nil, false, err
	}
	posts := PostsFromRecords(records)
	m.EnrichPosts(ctx, posts, m.owner().MXID)
	return posts, hasMore, nil
}

func (m *ContentModule) CreateComment(ctx context.Context, raw map[string]any) (any, *actionbase.Error) {
	params := actionbase.Params(raw)
	now := m.now()
	commentID := "comment_" + m.token("comment")
	owner := m.owner()
	channelID := params.String("channel_id")
	postID := params.String("post_id")
	if postID == "" {
		return nil, actionbase.BadRequest("post_id is required")
	}
	post, ok, err := m.PostByID(ctx, postID, channelID)
	if err != nil {
		return nil, actionbase.InternalError(err)
	}
	if !ok {
		return nil, actionbase.StatusError(http.StatusNotFound, "post not found")
	}
	if !post.CommentsEnabled {
		return nil, actionbase.StatusError(http.StatusForbidden, "comments are disabled for this post")
	}
	channelID = fallback(channelID, post.ChannelID)
	body := fallback(params.String("body"), params.String("content"))
	messageType := fallback(params.String("message_type"), "text")
	mediaJSON, media, err := mediaPayload(params.Raw("media_json"))
	if err != nil {
		return nil, actionbase.BadRequest("media_json is invalid")
	}
	replyToCommentID := params.String("reply_to_comment_id")
	replyToAuthorMXID := params.String("reply_to_author_mxid")
	mentionsJSON, err := jsonArray(params.Raw("mentions"))
	if err != nil {
		return nil, actionbase.BadRequest("mentions is invalid")
	}
	if _, ok := raw["mentions"]; !ok {
		mentionsJSON, err = jsonArray(params.Raw("mentions_json"))
		if err != nil {
			return nil, actionbase.BadRequest("mentions_json is invalid")
		}
	}
	eventID := m.eventID(commentID)
	authorMXID := owner.MXID
	authorName := owner.DisplayName
	senderMXID := owner.MXID
	operation, hasOperation := dirextalktransport.CapabilityOperationContextFrom(ctx)
	if hasOperation {
		// Product capability mutations are trusted Agent-originated work.  The
		// Matrix event and the durable channel author must carry the service
		// identity, never the authenticated owner, so a delegated Agent action
		// cannot be misrepresented as a human-authored comment.
		senderMXID = strings.TrimSpace(owner.AgentMXID)
		if senderMXID == "" {
			return nil, actionbase.InternalError(errors.New("agent Matrix identity is unavailable"))
		}
		authorMXID = senderMXID
		authorName = fallback(owner.AgentDisplayName, senderMXID)
		// Product Capability retries must rebuild the same logical comment row;
		// bind its ID to the immutable operation UUID instead of generating a
		// second random comment after a Matrix crash window.
		commentID = "comment_" + strings.ReplaceAll(operation.OperationID, "-", "")
		eventID = m.eventID(commentID)
	}
	originServerTS := now.UnixMilli()
	roomID, actionErr := m.roomIDForChannel(ctx, channelID, fallback(params.String("room_id"), post.RoomID))
	if actionErr != nil {
		return nil, actionErr
	}
	if actionErr := m.requireJoined(ctx, roomID); actionErr != nil {
		return nil, actionErr
	}
	if matrix := m.matrixPort(); matrix != nil && roomID != "" {
		content := channelMessageContent("channel_comment", body, messageType, mediaJSON, media)
		content["channel_id"] = channelID
		content["post_id"] = postID
		content["comment_id"] = commentID
		content["reply_to_comment_id"] = replyToCommentID
		content["reply_to_author_mxid"] = replyToAuthorMXID
		content["mentions_json"] = mentionsJSON
		if hasOperation {
			content["io.dirextalk.agent_gateway"] = true
			content["io.dirextalk.gateway_source"] = "native_agent"
		}
		request := dirextalktransport.SendMessageRequest{
			SenderMXID: senderMXID, RoomID: roomID, MessageType: messageType,
			Timestamp: now, Content: content, LogicalID: commentID, PostID: postID,
		}
		var result dirextalktransport.SendMessageResult
		var err error
		if hasOperation {
			prepared, preparedOK := matrix.(dirextalktransport.PreparedMessagePort)
			if !preparedOK || m.config.PreparedMatrixStore == nil {
				return nil, m.transportError(errors.New("durable Matrix mutation is unavailable"))
			}
			result, err = dirextalktransport.ExecutePreparedMatrixMutation(ctx, prepared, m.config.PreparedMatrixStore, operation, "product.channel_comments.v1", "create", request)
		} else {
			result, err = matrix.SendMessage(ctx, request)
		}
		if err != nil {
			return nil, m.transportError(err)
		}
		eventID = result.EventID
		originServerTS = result.OriginServerTS
	}
	comment := Comment{
		CommentID: commentID, PostID: postID, ChannelID: channelID, EventID: eventID,
		AuthorMXID: authorMXID, AuthorName: authorName, Body: body,
		MessageType: messageType, MediaJSON: mediaJSON, ReplyToCommentID: replyToCommentID,
		ReplyToAuthorMXID: replyToAuthorMXID, MentionsJSON: mentionsJSON,
		OriginServerTS: originServerTS,
	}
	if m.store == nil {
		return nil, actionbase.InternalError(errors.New("channel content store is not configured"))
	}
	if err := m.store.InsertChannelComment(ctx, commentRecord(comment)); err != nil {
		// The Matrix event may already have been committed when the process
		// crashed before this logical row. Replaying the same operation is
		// idempotent when the existing row carries the exact event/identity.
		existing, found, lookupErr := m.store.GetChannelCommentByID(ctx, comment.CommentID, comment.ChannelID)
		if lookupErr != nil || !found || existing.EventID != comment.EventID {
			return nil, actionbase.InternalError(err)
		}
		comment = commentFromRecord(existing)
	}
	if err := m.attachCommentOperation(ctx, &comment, actionCommentsCreate, "ok", roomID); err != nil {
		return nil, actionbase.InternalError(err)
	}
	return comment, nil
}

func (m *ContentModule) Comments(ctx context.Context, raw map[string]any) (any, *actionbase.Error) {
	postID := actionbase.Params(raw).String("post_id")
	if m.store == nil {
		return map[string]any{"comments": []Comment{}}, nil
	}
	records, err := m.store.ListChannelComments(ctx, postID)
	if err != nil {
		return map[string]any{"comments": []Comment{}}, nil
	}
	comments := CommentsFromRecords(records)
	m.EnrichComments(ctx, comments, m.owner().MXID)
	return map[string]any{"comments": comments}, nil
}

func (m *ContentModule) CommentPage(ctx context.Context, postID string, fromTS, snapshotTS, cursorTS int64, cursorID string, limit int) ([]Comment, bool, error) {
	if m.store == nil {
		return nil, false, errors.New("channel content store is not configured")
	}
	records, hasMore, err := m.store.ListChannelCommentsPage(ctx, postID, fromTS, snapshotTS, cursorTS, cursorID, limit)
	if err != nil {
		return nil, false, err
	}
	comments := CommentsFromRecords(records)
	m.EnrichComments(ctx, comments, m.owner().MXID)
	return comments, hasMore, nil
}

func (m *ContentModule) MyComments(ctx context.Context, raw map[string]any) (any, *actionbase.Error) {
	params := actionbase.Params(raw)
	channelID := params.String("channel_id")
	postID := params.String("post_id")
	ownerMXID := m.owner().MXID
	if m.store == nil {
		return map[string]any{"comments": []Comment{}}, nil
	}
	records, err := m.store.ListChannelComments(ctx, postID)
	if err != nil {
		return map[string]any{"comments": []Comment{}}, nil
	}
	comments := CommentsFromRecords(records)
	filtered := make([]Comment, 0, len(comments))
	for _, comment := range comments {
		if comment.AuthorMXID != ownerMXID || channelID != "" && comment.ChannelID != channelID {
			continue
		}
		filtered = append(filtered, comment)
	}
	return map[string]any{"comments": filtered}, nil
}

func (m *ContentModule) recall(action string) actionbase.Handler {
	return func(ctx context.Context, raw map[string]any) (any, *actionbase.Error) {
		return m.Recall(ctx, action, raw)
	}
}

func (m *ContentModule) Recall(ctx context.Context, action string, raw map[string]any) (any, *actionbase.Error) {
	params := actionbase.Params(raw)
	if action == actionPostsRecall {
		postID := params.String("post_id")
		if postID == "" {
			return nil, actionbase.BadRequest("post_id is required")
		}
		post, ok, err := m.PostByID(ctx, postID, params.String("channel_id"))
		if err != nil {
			return nil, m.transportError(err)
		}
		if !ok {
			return nil, actionbase.StatusError(http.StatusNotFound, "post not found")
		}
		if actionErr := m.authorizeRecall(ctx, post.RoomID, post.AuthorMXID); actionErr != nil {
			return nil, actionErr
		}
		if err := m.redact(ctx, post.RoomID, post.EventID, params.String("reason")); err != nil {
			return nil, m.transportError(err)
		}
		if m.store == nil {
			return nil, actionbase.InternalError(errors.New("channel content store is not configured"))
		}
		if _, err := m.store.DeleteChannelPost(ctx, postID); err != nil {
			return nil, actionbase.InternalError(err)
		}
		return m.mutationResult(ctx, action, post.RoomID)
	}

	commentID := params.String("comment_id")
	if commentID == "" {
		return nil, actionbase.BadRequest("comment_id is required")
	}
	comment, ok, err := m.CommentByID(ctx, commentID, params.String("post_id"))
	if err != nil {
		return nil, actionbase.InternalError(err)
	}
	if !ok {
		return nil, actionbase.StatusError(http.StatusNotFound, "comment not found")
	}
	roomID, err := m.RoomIDForComment(ctx, comment, params.String("room_id"))
	if err != nil {
		return nil, actionbase.InternalError(err)
	}
	if actionErr := m.authorizeRecall(ctx, roomID, comment.AuthorMXID); actionErr != nil {
		return nil, actionErr
	}
	if err := m.redact(ctx, roomID, comment.EventID, params.String("reason")); err != nil {
		return nil, m.transportError(err)
	}
	if m.store == nil {
		return nil, actionbase.InternalError(errors.New("channel content store is not configured"))
	}
	if _, err := m.store.DeleteChannelComment(ctx, commentID); err != nil {
		return nil, actionbase.InternalError(err)
	}
	return m.mutationResult(ctx, action, roomID)
}

func (m *ContentModule) authorizeRecall(ctx context.Context, roomID, authorMXID string) *actionbase.Error {
	if m.matrixPort() != nil || m.config.AuthorizeRecall == nil {
		return nil
	}
	return m.config.AuthorizeRecall(ctx, roomID, authorMXID)
}

func (m *ContentModule) redact(ctx context.Context, roomID, eventID, reason string) error {
	matrix := m.matrixPort()
	if matrix == nil || roomID == "" || eventID == "" {
		return nil
	}
	_, err := matrix.RedactEvent(ctx, dirextalktransport.RedactEventRequest{
		RoomID: roomID, EventID: eventID, SenderMXID: m.owner().MXID,
		Reason: reason, Timestamp: m.now(),
	})
	return err
}

func (m *ContentModule) mutationResult(ctx context.Context, action, roomID string) (any, *actionbase.Error) {
	result := map[string]any{"status": "ok"}
	if m.conversation == nil {
		return nil, actionbase.InternalError(errors.New("channel content conversation port is not configured"))
	}
	if err := m.conversation.AttachOperation(ctx, result, action, "ok", roomID); err != nil {
		return nil, actionbase.InternalError(err)
	}
	return result, nil
}

func (m *ContentModule) attachPostOperation(ctx context.Context, post *Post, action, status, roomID string) error {
	if m.conversation == nil {
		return errors.New("channel content conversation port is not configured")
	}
	operation, conversation, err := m.conversation.Operation(ctx, action, status, roomID)
	if err != nil {
		return err
	}
	post.Operation, post.Conversation = operation, conversation
	return nil
}

func (m *ContentModule) attachCommentOperation(ctx context.Context, comment *Comment, action, status, roomID string) error {
	if m.conversation == nil {
		return errors.New("channel content conversation port is not configured")
	}
	operation, conversation, err := m.conversation.Operation(ctx, action, status, roomID)
	if err != nil {
		return err
	}
	comment.Operation, comment.Conversation = operation, conversation
	return nil
}

func (m *ContentModule) PostByID(ctx context.Context, postID, channelID string) (Post, bool, error) {
	if m.store == nil {
		return Post{}, false, errors.New("channel content store is not configured")
	}
	record, ok, err := m.store.GetChannelPostByID(ctx, postID, channelID)
	return postFromRecord(record), ok, err
}

func (m *ContentModule) PostByEventID(ctx context.Context, eventID, channelID string) (Post, bool, error) {
	if strings.TrimSpace(eventID) == "" {
		return Post{}, false, nil
	}
	if m.store == nil {
		return Post{}, false, errors.New("channel content store is not configured")
	}
	record, ok, err := m.store.GetChannelPostByEventID(ctx, strings.TrimSpace(eventID), channelID)
	return postFromRecord(record), ok, err
}

func (m *ContentModule) CommentByID(ctx context.Context, commentID, postID string) (Comment, bool, error) {
	if m.store == nil {
		return Comment{}, false, errors.New("channel content store is not configured")
	}
	record, ok, err := m.store.GetChannelCommentByID(ctx, commentID, postID)
	return commentFromRecord(record), ok, err
}

func (m *ContentModule) CommentByEventID(ctx context.Context, eventID, channelID string) (Comment, bool, error) {
	if strings.TrimSpace(eventID) == "" {
		return Comment{}, false, nil
	}
	if m.store == nil {
		return Comment{}, false, errors.New("channel content store is not configured")
	}
	record, ok, err := m.store.GetChannelCommentByEventID(ctx, strings.TrimSpace(eventID), channelID)
	return commentFromRecord(record), ok, err
}

func (m *ContentModule) ReactionTargetByEventID(ctx context.Context, eventID, channelID string) (targetType, targetID, postID, commentID, resolvedChannelID string, err error) {
	if comment, ok, lookupErr := m.CommentByEventID(ctx, eventID, channelID); lookupErr != nil {
		return "", "", "", "", "", lookupErr
	} else if ok {
		return "comment", comment.CommentID, comment.PostID, comment.CommentID, comment.ChannelID, nil
	}
	if post, ok, lookupErr := m.PostByEventID(ctx, eventID, channelID); lookupErr != nil {
		return "", "", "", "", "", lookupErr
	} else if ok {
		return "post", post.PostID, post.PostID, "", post.ChannelID, nil
	}
	return "", "", "", "", "", nil
}

func (m *ContentModule) RoomIDForComment(ctx context.Context, comment Comment, fallbackRoomID string) (string, error) {
	if fallbackRoomID = strings.TrimSpace(fallbackRoomID); fallbackRoomID != "" {
		return fallbackRoomID, nil
	}
	if comment.PostID != "" {
		post, ok, err := m.PostByID(ctx, comment.PostID, comment.ChannelID)
		if err != nil {
			return "", err
		}
		if ok && post.RoomID != "" {
			return post.RoomID, nil
		}
	}
	if comment.ChannelID != "" && m.channels != nil {
		channel, ok, err := m.channels.ByIDOrRoom(ctx, comment.ChannelID, "")
		if err != nil {
			return "", err
		}
		if ok {
			return channel.RoomID, nil
		}
	}
	return "", nil
}

func PostsFromRecords(records []dirextalkdomain.ChannelPostRecord) []Post {
	if len(records) == 0 {
		return []Post{}
	}
	posts := make([]Post, 0, len(records))
	for _, record := range records {
		posts = append(posts, postFromRecord(record))
	}
	return posts
}

func CommentsFromRecords(records []dirextalkdomain.ChannelCommentRecord) []Comment {
	if len(records) == 0 {
		return []Comment{}
	}
	comments := make([]Comment, 0, len(records))
	for _, record := range records {
		comments = append(comments, commentFromRecord(record))
	}
	return comments
}

func channelMessageContent(kind, body, messageType, mediaJSON string, media map[string]any) map[string]any {
	content := map[string]any{
		"msgtype": matrixMessageType(messageType, mediaMessageType(messageType) || len(media) > 0),
		"body":    body, "p2p_kind": kind, "client_type": messageType,
	}
	if mediaJSON != "" {
		content["media_json"] = mediaJSON
	}
	for key, value := range media {
		if key == "body" || key == "msgtype" || key == "p2p_kind" || key == "client_type" || key == "media_json" {
			continue
		}
		if key == "mxc" && content["url"] == nil {
			content["url"] = value
			continue
		}
		content[key] = value
	}
	return content
}

func mediaMessageType(messageType string) bool {
	switch strings.TrimSpace(messageType) {
	case "image", "m.image", "video", "m.video", "audio", "m.audio", "file", "m.file":
		return true
	default:
		return false
	}
}

func matrixMessageType(messageType string, media bool) string {
	if !media {
		return "m.text"
	}
	switch strings.TrimSpace(messageType) {
	case "image", "m.image":
		return "m.image"
	case "video", "m.video":
		return "m.video"
	case "audio", "m.audio":
		return "m.audio"
	default:
		return "m.file"
	}
}
