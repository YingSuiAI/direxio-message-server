package productcapability

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

type ChannelsCapability struct {
	db *sql.DB
}

func NewChannelsCapability(db *sql.DB) *ChannelsCapability {
	return &ChannelsCapability{db: db}
}

func (c *ChannelsCapability) DescriptorID() string {
	return "product.channels.v1"
}

func (c *ChannelsCapability) HandleRead(ctx context.Context, operation string, input []byte) ([]byte, error) {
	switch operation {
	case "list":
		return c.listChannels(ctx, input)
	case "get_posts":
		return c.getPosts(ctx, input)
	default:
		return nil, ErrOperationNotFound
	}
}

type Channel struct {
	ChannelID   string    `json:"channel_id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Type        string    `json:"type"`
	CreatedAt   time.Time `json:"created_at"`
}

type Post struct {
	PostID    string                 `json:"post_id"`
	ChannelID string                 `json:"channel_id"`
	UserID    string                 `json:"user_id"`
	Content   map[string]interface{} `json:"content"`
	CreatedAt time.Time              `json:"created_at"`
	UpdatedAt time.Time              `json:"updated_at"`
}

func (c *ChannelsCapability) listChannels(ctx context.Context, input []byte) ([]byte, error) {
	var req struct {
		UserID string `json:"user_id"`
		Limit  int    `json:"limit"`
		Offset int    `json:"offset"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return nil, err
	}

	if req.Limit <= 0 {
		req.Limit = 50
	}

	// Query channels from a hypothetical channels table
	// Note: This assumes a channels table exists. If not, this will need adjustment.
	query := `
		SELECT channel_id, name, description, type, created_at
		FROM channels
		WHERE user_id = $1 OR channel_id IN (
			SELECT channel_id FROM channel_members WHERE user_id = $1
		)
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`

	rows, err := c.db.QueryContext(ctx, query, req.UserID, req.Limit, req.Offset)
	if err != nil {
		// If table doesn't exist, return empty result
		if err.Error() == `pq: relation "channels" does not exist` {
			return json.Marshal(map[string]interface{}{
				"channels": []interface{}{},
				"total":    0,
				"note":     "Channels table not yet implemented",
			})
		}
		return nil, err
	}
	defer rows.Close()

	var channels []*Channel
	for rows.Next() {
		channel := &Channel{}
		err := rows.Scan(&channel.ChannelID, &channel.Name, &channel.Description, &channel.Type, &channel.CreatedAt)
		if err != nil {
			return nil, err
		}
		channels = append(channels, channel)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return json.Marshal(map[string]interface{}{
		"channels": channels,
		"total":    len(channels),
	})
}

func (c *ChannelsCapability) getPosts(ctx context.Context, input []byte) ([]byte, error) {
	var req struct {
		ChannelID string `json:"channel_id"`
		Limit     int    `json:"limit"`
		Before    string `json:"before"` // post_id for pagination
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return nil, err
	}

	if req.Limit <= 0 {
		req.Limit = 50
	}

	var query string
	var args []interface{}

	if req.Before != "" {
		query = `
			SELECT post_id, channel_id, user_id, content, created_at, updated_at
			FROM posts
			WHERE channel_id = $1 AND created_at < (SELECT created_at FROM posts WHERE post_id = $2)
			ORDER BY created_at DESC
			LIMIT $3
		`
		args = []interface{}{req.ChannelID, req.Before, req.Limit}
	} else {
		query = `
			SELECT post_id, channel_id, user_id, content, created_at, updated_at
			FROM posts
			WHERE channel_id = $1
			ORDER BY created_at DESC
			LIMIT $2
		`
		args = []interface{}{req.ChannelID, req.Limit}
	}

	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		// If table doesn't exist, return empty result
		if err.Error() == `pq: relation "posts" does not exist` {
			return json.Marshal(map[string]interface{}{
				"posts": []interface{}{},
				"total": 0,
				"note":  "Posts table not yet implemented",
			})
		}
		return nil, err
	}
	defer rows.Close()

	var posts []*Post
	for rows.Next() {
		post := &Post{}
		var contentJSON []byte
		err := rows.Scan(&post.PostID, &post.ChannelID, &post.UserID, &contentJSON, &post.CreatedAt, &post.UpdatedAt)
		if err != nil {
			return nil, err
		}
		if len(contentJSON) > 0 {
			json.Unmarshal(contentJSON, &post.Content)
		}
		posts = append(posts, post)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return json.Marshal(map[string]interface{}{
		"posts": posts,
		"total": len(posts),
	})
}
