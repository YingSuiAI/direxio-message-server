package productcapability

import (
	"context"
	"database/sql"
	"encoding/json"
)

// RoomsCapability implements product.rooms.v1
type RoomsCapability struct {
	db *sql.DB
}

func NewRoomsCapability(db *sql.DB) *RoomsCapability {
	return &RoomsCapability{db: db}
}

func (c *RoomsCapability) DescriptorID() string {
	return "product.rooms.v1"
}

func (c *RoomsCapability) HandleRead(ctx context.Context, operation string, input []byte) ([]byte, error) {
	switch operation {
	case "list":
		return c.listRooms(ctx, input)
	case "search":
		return c.searchRooms(ctx, input)
	default:
		return nil, ErrOperationNotFound
	}
}

func (c *RoomsCapability) listRooms(ctx context.Context, input []byte) ([]byte, error) {
	// TODO: Query from database
	return json.Marshal(map[string]interface{}{"rooms": []interface{}{}})
}

func (c *RoomsCapability) searchRooms(ctx context.Context, input []byte) ([]byte, error) {
	// TODO: Implement search
	return json.Marshal(map[string]interface{}{"rooms": []interface{}{}})
}
