package productcapability

import (
	"context"
	"database/sql"
	"encoding/json"
)

// MessagesCapability implements product.messages.v1
type MessagesCapability struct {
	db *sql.DB
}

func NewMessagesCapability(db *sql.DB) *MessagesCapability {
	return &MessagesCapability{db: db}
}

func (c *MessagesCapability) DescriptorID() string {
	return "product.messages.v1"
}

func (c *MessagesCapability) HandleRead(ctx context.Context, operation string, input []byte) ([]byte, error) {
	switch operation {
	case "list":
		return c.listMessages(ctx, input)
	default:
		return nil, ErrOperationNotFound
	}
}

func (c *MessagesCapability) HandleMutation(ctx context.Context, operation string, input []byte) ([]byte, error) {
	switch operation {
	case "send":
		return c.sendMessage(ctx, input)
	default:
		return nil, ErrOperationNotFound
	}
}

func (c *MessagesCapability) listMessages(ctx context.Context, input []byte) ([]byte, error) {
	// TODO: Query from database
	return json.Marshal(map[string]interface{}{"messages": []interface{}{}})
}

func (c *MessagesCapability) sendMessage(ctx context.Context, input []byte) ([]byte, error) {
	// TODO: Send message via Dendrite
	return json.Marshal(map[string]interface{}{"message_id": "msg_123"})
}
