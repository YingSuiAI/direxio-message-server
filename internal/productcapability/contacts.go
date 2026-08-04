package productcapability

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// ContactsCapability implements product.contacts.v1
type ContactsCapability struct {
	db *sql.DB
}

func NewContactsCapability(db *sql.DB) *ContactsCapability {
	return &ContactsCapability{db: db}
}

func (c *ContactsCapability) DescriptorID() string {
	return "product.contacts.v1"
}

func (c *ContactsCapability) HandleRead(ctx context.Context, operation string, input []byte) ([]byte, error) {
	switch operation {
	case "list":
		return c.listContacts(ctx, input)
	case "get":
		return c.getContact(ctx, input)
	case "search":
		return c.searchContacts(ctx, input)
	default:
		return nil, ErrOperationNotFound
	}
}

func (c *ContactsCapability) HandleMutation(ctx context.Context, operation string, input []byte) ([]byte, error) {
	switch operation {
	case "create":
		return c.createContact(ctx, input)
	case "update":
		return c.updateContact(ctx, input)
	case "delete":
		return c.deleteContact(ctx, input)
	default:
		return nil, ErrOperationNotFound
	}
}

type Contact struct {
	ID        string
	OwnerID   string
	Name      string
	Email     string
	Phone     string
	Avatar    string
	Metadata  map[string]interface{}
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (c *ContactsCapability) listContacts(ctx context.Context, input []byte) ([]byte, error) {
	var req struct {
		Limit  int    `json:"limit"`
		Offset int    `json:"offset"`
		OwnerID string `json:"owner_id"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return nil, err
	}

	// Query contacts from database
	query := `
		SELECT id, owner_id, name, email, phone, avatar, metadata, created_at, updated_at
		FROM contacts
		WHERE owner_id = $1
		ORDER BY name
		LIMIT $2 OFFSET $3
	`

	rows, err := c.db.QueryContext(ctx, query, req.OwnerID, req.Limit, req.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var contacts []*Contact
	for rows.Next() {
		contact := &Contact{}
		var metadataJSON []byte
		err := rows.Scan(
			&contact.ID, &contact.OwnerID, &contact.Name, &contact.Email,
			&contact.Phone, &contact.Avatar, &metadataJSON,
			&contact.CreatedAt, &contact.UpdatedAt,
		)
		if err != nil {
			return nil, err
		}
		if len(metadataJSON) > 0 {
			json.Unmarshal(metadataJSON, &contact.Metadata)
		}
		contacts = append(contacts, contact)
	}

	return json.Marshal(map[string]interface{}{
		"contacts": contacts,
		"total":    len(contacts),
	})
}

func (c *ContactsCapability) getContact(ctx context.Context, input []byte) ([]byte, error) {
	// TODO: Implement
	return json.Marshal(map[string]interface{}{"contact": nil})
}

func (c *ContactsCapability) searchContacts(ctx context.Context, input []byte) ([]byte, error) {
	// TODO: Implement
	return json.Marshal(map[string]interface{}{"contacts": []interface{}{}})
}

func (c *ContactsCapability) createContact(ctx context.Context, input []byte) ([]byte, error) {
	// TODO: Implement
	return nil, nil
}

func (c *ContactsCapability) updateContact(ctx context.Context, input []byte) ([]byte, error) {
	// TODO: Implement
	return nil, nil
}

func (c *ContactsCapability) deleteContact(ctx context.Context, input []byte) ([]byte, error) {
	// TODO: Implement
	return nil, nil
}

var ErrOperationNotFound = sql.ErrNoRows
