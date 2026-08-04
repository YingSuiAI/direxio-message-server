package productcapability

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
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

type Member struct {
	UserID     string    `json:"user_id"`
	RoomID     string    `json:"room_id"`
	Membership string    `json:"membership"`
	EventID    string    `json:"event_id"`
	JoinedAt   time.Time `json:"joined_at,omitempty"`
}

func (c *MembersCapability) listMembers(ctx context.Context, input []byte) ([]byte, error) {
	var req struct {
		RoomID     string `json:"room_id"`
		Membership string `json:"membership"` // Optional filter: join, invite, leave, ban
		Limit      int    `json:"limit"`
		Offset     int    `json:"offset"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return nil, err
	}

	if req.Limit <= 0 {
		req.Limit = 100
	}

	var query string
	var args []interface{}

	if req.Membership != "" {
		query = `
			SELECT user_id, room_id, membership, event_id, stream_pos
			FROM syncapi_memberships
			WHERE room_id = $1 AND membership = $2
			ORDER BY stream_pos DESC
			LIMIT $3 OFFSET $4
		`
		args = []interface{}{req.RoomID, req.Membership, req.Limit, req.Offset}
	} else {
		query = `
			SELECT user_id, room_id, membership, event_id, stream_pos
			FROM syncapi_memberships
			WHERE room_id = $1
			ORDER BY stream_pos DESC
			LIMIT $2 OFFSET $3
		`
		args = []interface{}{req.RoomID, req.Limit, req.Offset}
	}

	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var members []*Member
	for rows.Next() {
		member := &Member{}
		var streamPos int64
		err := rows.Scan(&member.UserID, &member.RoomID, &member.Membership, &member.EventID, &streamPos)
		if err != nil {
			return nil, err
		}
		members = append(members, member)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return json.Marshal(map[string]interface{}{
		"members": members,
		"total":   len(members),
	})
}

func (c *MembersCapability) getMember(ctx context.Context, input []byte) ([]byte, error) {
	var req struct {
		RoomID string `json:"room_id"`
		UserID string `json:"user_id"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return nil, err
	}

	query := `
		SELECT user_id, room_id, membership, event_id
		FROM syncapi_memberships
		WHERE room_id = $1 AND user_id = $2
		ORDER BY stream_pos DESC
		LIMIT 1
	`

	member := &Member{}
	err := c.db.QueryRowContext(ctx, query, req.RoomID, req.UserID).Scan(
		&member.UserID, &member.RoomID, &member.Membership, &member.EventID,
	)
	if err == sql.ErrNoRows {
		return json.Marshal(map[string]interface{}{"member": nil})
	}
	if err != nil {
		return nil, err
	}

	return json.Marshal(map[string]interface{}{"member": member})
}

func (c *MembersCapability) addMember(ctx context.Context, input []byte) ([]byte, error) {
	var req struct {
		RoomID string `json:"room_id"`
		UserID string `json:"user_id"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return nil, err
	}

	// TODO: This would need to integrate with the Matrix roomserver to actually send invite/join events
	// For now, return a placeholder response
	// In production, this should call the roomserver API to create membership events

	return json.Marshal(map[string]interface{}{
		"success": false,
		"note":    "Member addition requires roomserver integration to send m.room.member events",
		"room_id": req.RoomID,
		"user_id": req.UserID,
	})
}

func (c *MembersCapability) removeMember(ctx context.Context, input []byte) ([]byte, error) {
	var req struct {
		RoomID string `json:"room_id"`
		UserID string `json:"user_id"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return nil, err
	}

	// TODO: This would need to integrate with the Matrix roomserver to send leave/kick/ban events
	// For now, return a placeholder response

	return json.Marshal(map[string]interface{}{
		"success": false,
		"note":    "Member removal requires roomserver integration to send m.room.member events",
		"room_id": req.RoomID,
		"user_id": req.UserID,
	})
}
