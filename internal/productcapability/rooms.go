package productcapability

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
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

type Room struct {
	RoomID      string    `json:"room_id"`
	RoomVersion string    `json:"room_version"`
	Name        string    `json:"name,omitempty"`
	Topic       string    `json:"topic,omitempty"`
	JoinedAt    time.Time `json:"joined_at,omitempty"`
}

func (c *RoomsCapability) listRooms(ctx context.Context, input []byte) ([]byte, error) {
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

	// Query rooms the user is a member of from syncapi_memberships and roomserver_rooms
	query := `
		SELECT DISTINCT r.room_id, r.room_version, m.stream_pos
		FROM syncapi_memberships m
		INNER JOIN roomserver_rooms r ON m.room_id = r.room_id
		WHERE m.user_id = $1 AND m.membership = 'join'
		ORDER BY m.stream_pos DESC
		LIMIT $2 OFFSET $3
	`

	rows, err := c.db.QueryContext(ctx, query, req.UserID, req.Limit, req.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rooms []*Room
	for rows.Next() {
		room := &Room{}
		var streamPos int64
		err := rows.Scan(&room.RoomID, &room.RoomVersion, &streamPos)
		if err != nil {
			return nil, err
		}
		rooms = append(rooms, room)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return json.Marshal(map[string]interface{}{
		"rooms": rooms,
		"total": len(rooms),
	})
}

func (c *RoomsCapability) searchRooms(ctx context.Context, input []byte) ([]byte, error) {
	var req struct {
		UserID string `json:"user_id"`
		Query  string `json:"query"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return nil, err
	}

	if req.Limit <= 0 {
		req.Limit = 50
	}

	// Search rooms by room_id pattern (simplified search)
	query := `
		SELECT DISTINCT r.room_id, r.room_version
		FROM syncapi_memberships m
		INNER JOIN roomserver_rooms r ON m.room_id = r.room_id
		WHERE m.user_id = $1 AND m.membership = 'join' AND r.room_id ILIKE $2
		ORDER BY r.room_id
		LIMIT $3
	`

	searchPattern := "%" + req.Query + "%"
	rows, err := c.db.QueryContext(ctx, query, req.UserID, searchPattern, req.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rooms []*Room
	for rows.Next() {
		room := &Room{}
		err := rows.Scan(&room.RoomID, &room.RoomVersion)
		if err != nil {
			return nil, err
		}
		rooms = append(rooms, room)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return json.Marshal(map[string]interface{}{
		"rooms": rooms,
		"total": len(rooms),
	})
}
