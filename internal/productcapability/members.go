package productcapability

import (
	"context"
	"database/sql"
	"encoding/json"
)

type MembersCapability struct {
	db *sql.DB
}

func NewMembersCapability(db *sql.DB) *MembersCapability {
	return &MembersCapability{db: db}
}

func (c *MembersCapability) DescriptorID() string {
	return "product.members.v1"
}

func (c *MembersCapability) HandleRead(ctx context.Context, operation string, input []byte) ([]byte, error) {
	switch operation {
	case "list":
		return c.listMembers(ctx, input)
	case "get":
		return c.getMember(ctx, input)
	default:
		return nil, ErrOperationNotFound
	}
}

func (c *MembersCapability) HandleMutation(ctx context.Context, operation string, input []byte) ([]byte, error) {
	switch operation {
	case "add":
		return c.addMember(ctx, input)
	case "remove":
		return c.removeMember(ctx, input)
	default:
		return nil, ErrOperationNotFound
	}
}

func (c *MembersCapability) listMembers(ctx context.Context, input []byte) ([]byte, error) {
	return json.Marshal(map[string]interface{}{"members": []interface{}{}})
}

func (c *MembersCapability) getMember(ctx context.Context, input []byte) ([]byte, error) {
	return json.Marshal(map[string]interface{}{"member": nil})
}

func (c *MembersCapability) addMember(ctx context.Context, input []byte) ([]byte, error) {
	return json.Marshal(map[string]interface{}{"success": true})
}

func (c *MembersCapability) removeMember(ctx context.Context, input []byte) ([]byte, error) {
	return json.Marshal(map[string]interface{}{"success": true})
}
