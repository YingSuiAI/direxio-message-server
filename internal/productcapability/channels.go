package productcapability

import (
	"context"
	"database/sql"
	"encoding/json"
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

func (c *ChannelsCapability) listChannels(ctx context.Context, input []byte) ([]byte, error) {
	return json.Marshal(map[string]interface{}{"channels": []interface{}{}})
}

func (c *ChannelsCapability) getPosts(ctx context.Context, input []byte) ([]byte, error) {
	return json.Marshal(map[string]interface{}{"posts": []interface{}{}})
}
