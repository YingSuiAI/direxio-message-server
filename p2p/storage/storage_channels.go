package storage

import (
	"context"
	"database/sql"
	"strings"
)

func (s *DatabaseStore) UpsertChannel(ctx context.Context, ch channel) error {
	return s.writer.Do(nil, nil, func(txn *sql.Tx) error {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO p2p_channels (
				channel_id, room_id, name, description, avatar_url, visibility,
				join_policy, channel_type, comments_enabled, muted, member_count, pending_join_count
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			ON CONFLICT(channel_id) DO UPDATE SET
				room_id = EXCLUDED.room_id,
				name = EXCLUDED.name,
				description = EXCLUDED.description,
				avatar_url = EXCLUDED.avatar_url,
				visibility = EXCLUDED.visibility,
				join_policy = EXCLUDED.join_policy,
				channel_type = EXCLUDED.channel_type,
				comments_enabled = EXCLUDED.comments_enabled,
				muted = EXCLUDED.muted,
				member_count = EXCLUDED.member_count,
				pending_join_count = EXCLUDED.pending_join_count
		`, ch.ChannelID, ch.RoomID, ch.Name, ch.Description, ch.AvatarURL, ch.Visibility,
			ch.JoinPolicy, ch.ChannelType, boolInt(ch.CommentsEnabled), boolInt(ch.Muted), ch.MemberCount, ch.PendingJoinCount)
		return err
	})
}

func (s *DatabaseStore) DeleteChannel(ctx context.Context, channelID string) error {
	return s.writer.Do(nil, nil, func(txn *sql.Tx) error {
		_, err := s.db.ExecContext(ctx, `DELETE FROM p2p_channels WHERE channel_id = $1`, channelID)
		return err
	})
}

func (s *DatabaseStore) ListChannels(ctx context.Context) ([]channel, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT channel_id, room_id, name, description, avatar_url, visibility,
			join_policy, channel_type, comments_enabled, muted, member_count, pending_join_count
		FROM p2p_channels ORDER BY channel_id ASC
	`)
	if err != nil {
		return nil, err
	}
	defer closeResource(rows)
	var channels []channel
	for rows.Next() {
		var ch channel
		var commentsEnabled, muted int64
		if err := rows.Scan(&ch.ChannelID, &ch.RoomID, &ch.Name, &ch.Description, &ch.AvatarURL, &ch.Visibility,
			&ch.JoinPolicy, &ch.ChannelType, &commentsEnabled, &muted, &ch.MemberCount, &ch.PendingJoinCount); err != nil {
			return nil, err
		}
		ch.CommentsEnabled = commentsEnabled == 1
		ch.Muted = muted == 1
		channels = append(channels, ch)
	}
	return channels, rows.Err()
}

func (s *DatabaseStore) GetChannelByIDOrRoom(ctx context.Context, channelID, roomID string) (channel, bool, error) {
	if channelID == "" && roomID == "" {
		return channel{}, false, nil
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT channel_id, room_id, name, description, avatar_url, visibility,
			join_policy, channel_type, comments_enabled, muted, member_count, pending_join_count
		FROM p2p_channels
		WHERE ($1 <> '' AND channel_id = $1) OR ($2 <> '' AND room_id = $2)
		ORDER BY CASE WHEN channel_id = $1 THEN 0 ELSE 1 END
		LIMIT 1
	`, channelID, roomID)
	ch, err := scanChannel(row)
	if err == sql.ErrNoRows {
		return channel{}, false, nil
	}
	if err != nil {
		return channel{}, false, err
	}
	return ch, true, nil
}

func (s *DatabaseStore) ListJoinedChannelsForUser(ctx context.Context, userID string) ([]channel, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT * FROM (
			SELECT c.channel_id, c.room_id, c.name, c.description, c.avatar_url, c.visibility,
				c.join_policy, c.channel_type, c.comments_enabled, c.muted, c.member_count, c.pending_join_count,
				m.role, m.membership
			FROM p2p_channels c
			INNER JOIN p2p_members m ON m.channel_id = c.channel_id
			WHERE m.user_id = $1 AND m.membership = 'join' AND m.channel_id <> ''
			UNION ALL
			SELECT c.channel_id, c.room_id, c.name, c.description, c.avatar_url, c.visibility,
				c.join_policy, c.channel_type, c.comments_enabled, c.muted, c.member_count, c.pending_join_count,
				m.role, m.membership
			FROM p2p_channels c
			INNER JOIN p2p_members m ON m.room_id = c.room_id
			WHERE m.user_id = $1 AND m.membership = 'join' AND m.channel_id = ''
		) joined_channels
		ORDER BY channel_id ASC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer closeResource(rows)
	var channels []channel
	for rows.Next() {
		var ch channel
		var commentsEnabled, muted int64
		if err := rows.Scan(&ch.ChannelID, &ch.RoomID, &ch.Name, &ch.Description, &ch.AvatarURL, &ch.Visibility,
			&ch.JoinPolicy, &ch.ChannelType, &commentsEnabled, &muted, &ch.MemberCount, &ch.PendingJoinCount,
			&ch.Role, &ch.MemberStatus); err != nil {
			return nil, err
		}
		ch.CommentsEnabled = commentsEnabled == 1
		ch.Muted = muted == 1
		ch.Role = normalizeStoredProductMemberRole(ch.Role)
		ch.MemberStatus = "join"
		ch.IsOwned = strings.EqualFold(ch.Role, "owner")
		channels = append(channels, ch)
	}
	return channels, rows.Err()
}

func (s *DatabaseStore) SearchPublicChannels(ctx context.Context, query string, limit int) ([]channel, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	query = strings.ToLower(strings.TrimSpace(query))
	var rows *sql.Rows
	var err error
	if query == "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT channel_id, room_id, name, description, avatar_url, visibility,
				join_policy, channel_type, comments_enabled, muted, member_count, pending_join_count
			FROM p2p_channels
			WHERE visibility = 'public'
			ORDER BY channel_id ASC
			LIMIT $1
		`, limit)
	} else {
		pattern := "%" + query + "%"
		rows, err = s.db.QueryContext(ctx, `
			SELECT channel_id, room_id, name, description, avatar_url, visibility,
				join_policy, channel_type, comments_enabled, muted, member_count, pending_join_count
			FROM p2p_channels
			WHERE visibility = 'public'
				AND LOWER(channel_id || ' ' || room_id || ' ' || name || ' ' || description) LIKE $1
			ORDER BY channel_id ASC
			LIMIT $2
		`, pattern, limit)
	}
	if err != nil {
		return nil, err
	}
	defer closeResource(rows)
	return scanChannels(rows)
}

func (s *DatabaseStore) ListPublicChannelsForOwner(ctx context.Context, userID string) ([]channel, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT * FROM (
			SELECT c.channel_id, c.room_id, c.name, c.description, c.avatar_url, c.visibility,
				c.join_policy, c.channel_type, c.comments_enabled, c.muted, c.member_count, c.pending_join_count
			FROM p2p_channels c
			INNER JOIN p2p_members m ON m.channel_id = c.channel_id
			WHERE m.user_id = $1 AND m.role = 'owner' AND m.membership = 'join' AND m.channel_id <> '' AND c.visibility = 'public'
			UNION ALL
			SELECT c.channel_id, c.room_id, c.name, c.description, c.avatar_url, c.visibility,
				c.join_policy, c.channel_type, c.comments_enabled, c.muted, c.member_count, c.pending_join_count
			FROM p2p_channels c
			INNER JOIN p2p_members m ON m.room_id = c.room_id
			WHERE m.user_id = $1 AND m.role = 'owner' AND m.membership = 'join' AND m.channel_id = '' AND c.visibility = 'public'
		) public_owner_channels
		ORDER BY name ASC, channel_id ASC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer closeResource(rows)
	return scanChannels(rows)
}

type channelScanner interface {
	Scan(dest ...any) error
}

func scanChannels(rows *sql.Rows) ([]channel, error) {
	var channels []channel
	for rows.Next() {
		ch, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		channels = append(channels, ch)
	}
	return channels, rows.Err()
}

func scanChannel(row channelScanner) (channel, error) {
	var ch channel
	var commentsEnabled, muted int64
	if err := row.Scan(&ch.ChannelID, &ch.RoomID, &ch.Name, &ch.Description, &ch.AvatarURL, &ch.Visibility,
		&ch.JoinPolicy, &ch.ChannelType, &commentsEnabled, &muted, &ch.MemberCount, &ch.PendingJoinCount); err != nil {
		return channel{}, err
	}
	ch.CommentsEnabled = commentsEnabled == 1
	ch.Muted = muted == 1
	return ch, nil
}

func (s *DatabaseStore) UpsertChannelInviteGrant(ctx context.Context, grant channelInviteGrant) error {
	return s.writer.Do(nil, nil, func(txn *sql.Tx) error {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO p2p_channel_invite_grants (grant_id, channel_id, room_id, share_room_id, created_by, created_at)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT(grant_id) DO UPDATE SET
				channel_id = EXCLUDED.channel_id,
				room_id = EXCLUDED.room_id,
				share_room_id = EXCLUDED.share_room_id,
				created_by = EXCLUDED.created_by,
				created_at = EXCLUDED.created_at
		`, grant.GrantID, grant.ChannelID, grant.RoomID, grant.ShareRoomID, grant.CreatedBy, grant.CreatedAt)
		return err
	})
}

func (s *DatabaseStore) ListChannelInviteGrants(ctx context.Context) ([]channelInviteGrant, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT grant_id, channel_id, room_id, share_room_id, created_by, created_at
		FROM p2p_channel_invite_grants ORDER BY created_at DESC, grant_id ASC
	`)
	if err != nil {
		return nil, err
	}
	defer closeResource(rows)
	var grants []channelInviteGrant
	for rows.Next() {
		var grant channelInviteGrant
		if err := rows.Scan(&grant.GrantID, &grant.ChannelID, &grant.RoomID, &grant.ShareRoomID, &grant.CreatedBy, &grant.CreatedAt); err != nil {
			return nil, err
		}
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

func (s *DatabaseStore) InsertChannelPost(ctx context.Context, post channelPostRecord) error {
	post = normalizeChannelPostRecord(post)
	return s.writer.Do(nil, nil, func(txn *sql.Tx) error {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO p2p_channel_posts (
				post_id, channel_id, room_id, event_id, author_mxid, author_name,
				body, message_type, media_json, visibility, origin_server_ts, comment_count
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			ON CONFLICT(post_id) DO UPDATE SET
				channel_id = EXCLUDED.channel_id,
				room_id = EXCLUDED.room_id,
				event_id = EXCLUDED.event_id,
				author_mxid = EXCLUDED.author_mxid,
				author_name = EXCLUDED.author_name,
				body = EXCLUDED.body,
				message_type = EXCLUDED.message_type,
				media_json = EXCLUDED.media_json,
				visibility = EXCLUDED.visibility,
				origin_server_ts = EXCLUDED.origin_server_ts,
				comment_count = EXCLUDED.comment_count
		`, post.PostID, post.ChannelID, post.RoomID, post.EventID, post.AuthorMXID, post.AuthorName,
			post.Body, post.MessageType, post.MediaJSON, post.Visibility, post.OriginServerTS, post.CommentCount)
		return err
	})
}

func (s *DatabaseStore) GetChannelPostByID(ctx context.Context, postID, channelID string) (channelPostRecord, bool, error) {
	if postID == "" {
		return channelPostRecord{}, false, nil
	}
	row := s.db.QueryRowContext(ctx, listPostsSelect+`
		WHERE post_id = $1 AND ($2 = '' OR channel_id = $2)
		LIMIT 1
	`, postID, channelID)
	post, err := scanChannelPost(row)
	if err == sql.ErrNoRows {
		return channelPostRecord{}, false, nil
	}
	if err != nil {
		return channelPostRecord{}, false, err
	}
	return post, true, nil
}

func (s *DatabaseStore) GetChannelPostByEventID(ctx context.Context, eventID, channelID string) (channelPostRecord, bool, error) {
	if eventID == "" {
		return channelPostRecord{}, false, nil
	}
	row := s.db.QueryRowContext(ctx, listPostsSelect+`
		WHERE event_id = $1 AND ($2 = '' OR channel_id = $2)
		LIMIT 1
	`, eventID, channelID)
	post, err := scanChannelPost(row)
	if err == sql.ErrNoRows {
		return channelPostRecord{}, false, nil
	}
	if err != nil {
		return channelPostRecord{}, false, err
	}
	return post, true, nil
}

func (s *DatabaseStore) ListChannelPosts(ctx context.Context, channelID string) ([]channelPostRecord, error) {
	var rows *sql.Rows
	var err error
	if channelID == "" {
		rows, err = s.db.QueryContext(ctx, listPostsSelect+` ORDER BY origin_server_ts DESC`)
	} else {
		rows, err = s.db.QueryContext(ctx, listPostsSelect+` WHERE channel_id = $1 ORDER BY origin_server_ts DESC`, channelID)
	}
	if err != nil {
		return nil, err
	}
	defer closeResource(rows)
	var posts []channelPostRecord
	for rows.Next() {
		post, err := scanChannelPost(rows)
		if err != nil {
			return nil, err
		}
		posts = append(posts, post)
	}
	return posts, rows.Err()
}

func (s *DatabaseStore) ListChannelPostsPage(ctx context.Context, channelID string, fromTS, snapshotTS, cursorTS int64, cursorID string, limit int) ([]channelPostRecord, bool, error) {
	return listChannelPage(
		ctx,
		s.db,
		listPostsSelect,
		"channel_id",
		channelID,
		"post_id",
		fromTS,
		snapshotTS,
		cursorTS,
		cursorID,
		limit,
		scanChannelPost,
	)
}

func (s *DatabaseStore) ListChannelPostsByVisibilityPage(ctx context.Context, channelID, visibility string, offset int64, limit int) ([]channelPostRecord, bool, error) {
	visibility = normalizeChannelPostRecord(channelPostRecord{Visibility: visibility}).Visibility
	rows, err := s.db.QueryContext(ctx, listPostsSelect+`
		WHERE channel_id = $1 AND visibility = $2
		ORDER BY origin_server_ts DESC, post_id DESC
		LIMIT $3 OFFSET $4
	`, channelID, visibility, limit+1, offset)
	if err != nil {
		return nil, false, err
	}
	defer closeResource(rows)
	posts := make([]channelPostRecord, 0, limit+1)
	for rows.Next() {
		post, err := scanChannelPost(rows)
		if err != nil {
			return nil, false, err
		}
		posts = append(posts, post)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := len(posts) > limit
	if hasMore {
		posts = posts[:limit]
	}
	return posts, hasMore, nil
}

const listPostsSelect = `SELECT post_id, channel_id, room_id, event_id, author_mxid, author_name, body, message_type, media_json, visibility, origin_server_ts, comment_count FROM p2p_channel_posts`

func scanChannelPost(row channelScanner) (channelPostRecord, error) {
	var post channelPostRecord
	if err := row.Scan(&post.PostID, &post.ChannelID, &post.RoomID, &post.EventID, &post.AuthorMXID, &post.AuthorName,
		&post.Body, &post.MessageType, &post.MediaJSON, &post.Visibility, &post.OriginServerTS, &post.CommentCount); err != nil {
		return channelPostRecord{}, err
	}
	return post, nil
}

func (s *DatabaseStore) InsertChannelComment(ctx context.Context, comment channelCommentRecord) error {
	return s.writer.Do(nil, nil, func(txn *sql.Tx) error {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO p2p_channel_comments (
				comment_id, post_id, channel_id, event_id, author_mxid, author_name,
				body, message_type, media_json, reply_to_comment_id, reply_to_author_mxid, mentions_json,
				origin_server_ts, reaction_count, reacted_by_me
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
			ON CONFLICT(comment_id) DO UPDATE SET
				post_id = EXCLUDED.post_id,
				channel_id = EXCLUDED.channel_id,
				event_id = EXCLUDED.event_id,
				author_mxid = EXCLUDED.author_mxid,
				author_name = EXCLUDED.author_name,
				body = EXCLUDED.body,
				message_type = EXCLUDED.message_type,
				media_json = EXCLUDED.media_json,
				reply_to_comment_id = EXCLUDED.reply_to_comment_id,
				reply_to_author_mxid = EXCLUDED.reply_to_author_mxid,
				mentions_json = EXCLUDED.mentions_json,
				origin_server_ts = EXCLUDED.origin_server_ts,
				reaction_count = EXCLUDED.reaction_count,
				reacted_by_me = EXCLUDED.reacted_by_me
		`, comment.CommentID, comment.PostID, comment.ChannelID, comment.EventID, comment.AuthorMXID, comment.AuthorName,
			comment.Body, comment.MessageType, comment.MediaJSON, comment.ReplyToCommentID, comment.ReplyToAuthorMXID, fallbackString(comment.MentionsJSON, "[]"),
			comment.OriginServerTS, int64(0), int64(0))
		return err
	})
}

func (s *DatabaseStore) GetChannelCommentByID(ctx context.Context, commentID, postID string) (channelCommentRecord, bool, error) {
	if commentID == "" {
		return channelCommentRecord{}, false, nil
	}
	row := s.db.QueryRowContext(ctx, listCommentsSelect+`
		WHERE comment_id = $1 AND ($2 = '' OR post_id = $2)
		LIMIT 1
	`, commentID, postID)
	comment, err := scanChannelComment(row)
	if err == sql.ErrNoRows {
		return channelCommentRecord{}, false, nil
	}
	if err != nil {
		return channelCommentRecord{}, false, err
	}
	return comment, true, nil
}

func (s *DatabaseStore) GetChannelCommentByEventID(ctx context.Context, eventID, channelID string) (channelCommentRecord, bool, error) {
	if eventID == "" {
		return channelCommentRecord{}, false, nil
	}
	row := s.db.QueryRowContext(ctx, listCommentsSelect+`
		WHERE event_id = $1 AND ($2 = '' OR channel_id = $2)
		LIMIT 1
	`, eventID, channelID)
	comment, err := scanChannelComment(row)
	if err == sql.ErrNoRows {
		return channelCommentRecord{}, false, nil
	}
	if err != nil {
		return channelCommentRecord{}, false, err
	}
	return comment, true, nil
}

func (s *DatabaseStore) ListChannelComments(ctx context.Context, postID string) ([]channelCommentRecord, error) {
	var rows *sql.Rows
	var err error
	if postID == "" {
		rows, err = s.db.QueryContext(ctx, listCommentsSelect+` ORDER BY origin_server_ts ASC`)
	} else {
		rows, err = s.db.QueryContext(ctx, listCommentsSelect+` WHERE post_id = $1 ORDER BY origin_server_ts ASC`, postID)
	}
	if err != nil {
		return nil, err
	}
	defer closeResource(rows)
	var comments []channelCommentRecord
	for rows.Next() {
		comment, err := scanChannelComment(rows)
		if err != nil {
			return nil, err
		}
		comments = append(comments, comment)
	}
	return comments, rows.Err()
}

func (s *DatabaseStore) ListChannelCommentsPage(ctx context.Context, postID string, fromTS, snapshotTS, cursorTS int64, cursorID string, limit int) ([]channelCommentRecord, bool, error) {
	return listChannelPage(
		ctx,
		s.db,
		listCommentsSelect,
		"post_id",
		postID,
		"comment_id",
		fromTS,
		snapshotTS,
		cursorTS,
		cursorID,
		limit,
		scanChannelComment,
	)
}

const listCommentsSelect = `SELECT comment_id, post_id, channel_id, event_id, author_mxid, author_name, body, message_type, media_json, reply_to_comment_id, reply_to_author_mxid, mentions_json, origin_server_ts, reaction_count, reacted_by_me FROM p2p_channel_comments`

func scanChannelComment(row channelScanner) (channelCommentRecord, error) {
	var comment channelCommentRecord
	var reactionCount, reacted int64
	if err := row.Scan(&comment.CommentID, &comment.PostID, &comment.ChannelID, &comment.EventID, &comment.AuthorMXID, &comment.AuthorName,
		&comment.Body, &comment.MessageType, &comment.MediaJSON, &comment.ReplyToCommentID, &comment.ReplyToAuthorMXID, &comment.MentionsJSON,
		&comment.OriginServerTS, &reactionCount, &reacted); err != nil {
		return channelCommentRecord{}, err
	}
	return comment, nil
}
