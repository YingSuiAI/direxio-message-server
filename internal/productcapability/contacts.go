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
		Limit   int    `json:"limit"`
		Offset  int    `json:"offset"`
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
	var req struct {
		ID      string `json:"id"`
		OwnerID string `json:"owner_id"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return nil, err
	}

	query := `
		SELECT id, owner_id, name, email, phone, avatar, metadata, created_at, updated_at
		FROM contacts
		WHERE id = $1 AND owner_id = $2
	`

	contact := &Contact{}
	var metadataJSON []byte
	err := c.db.QueryRowContext(ctx, query, req.ID, req.OwnerID).Scan(
		&contact.ID, &contact.OwnerID, &contact.Name, &contact.Email,
		&contact.Phone, &contact.Avatar, &metadataJSON,
		&contact.CreatedAt, &contact.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return json.Marshal(map[string]interface{}{"contact": nil})
	}
	if err != nil {
		return nil, err
	}
	if len(metadataJSON) > 0 {
		json.Unmarshal(metadataJSON, &contact.Metadata)
	}

	return json.Marshal(map[string]interface{}{"contact": contact})
}

func (c *ContactsCapability) searchContacts(ctx context.Context, input []byte) ([]byte, error) {
	var req struct {
		Query   string `json:"query"`
		OwnerID string `json:"owner_id"`
		Limit   int    `json:"limit"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return nil, err
	}

	if req.Limit <= 0 {
		req.Limit = 50
	}

	query := `
		SELECT id, owner_id, name, email, phone, avatar, metadata, created_at, updated_at
		FROM contacts
		WHERE owner_id = $1 AND (name ILIKE $2 OR email ILIKE $2 OR phone ILIKE $2)
		ORDER BY name
		LIMIT $3
	`

	searchPattern := "%" + req.Query + "%"
	rows, err := c.db.QueryContext(ctx, query, req.OwnerID, searchPattern, req.Limit)
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

func (c *ContactsCapability) createContact(ctx context.Context, input []byte) ([]byte, error) {
	var req struct {
		OwnerID  string                 `json:"owner_id"`
		Name     string                 `json:"name"`
		Email    string                 `json:"email"`
		Phone    string                 `json:"phone"`
		Avatar   string                 `json:"avatar"`
		Metadata map[string]interface{} `json:"metadata"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return nil, err
	}

	metadataJSON, err := json.Marshal(req.Metadata)
	if err != nil {
		return nil, err
	}

	query := `
		INSERT INTO contacts (owner_id, name, email, phone, avatar, metadata, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW(), NOW())
		RETURNING id, created_at, updated_at
	`

	var id string
	var createdAt, updatedAt time.Time
	err = c.db.QueryRowContext(ctx, query, req.OwnerID, req.Name, req.Email, req.Phone, req.Avatar, metadataJSON).Scan(&id, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}

	return json.Marshal(map[string]interface{}{
		"id":         id,
		"owner_id":   req.OwnerID,
		"name":       req.Name,
		"email":      req.Email,
		"phone":      req.Phone,
		"avatar":     req.Avatar,
		"metadata":   req.Metadata,
		"created_at": createdAt,
		"updated_at": updatedAt,
	})
}

func (c *ContactsCapability) updateContact(ctx context.Context, input []byte) ([]byte, error) {
	var req struct {
		ID       string                 `json:"id"`
		OwnerID  string                 `json:"owner_id"`
		Name     string                 `json:"name"`
		Email    string                 `json:"email"`
		Phone    string                 `json:"phone"`
		Avatar   string                 `json:"avatar"`
		Metadata map[string]interface{} `json:"metadata"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return nil, err
	}

	metadataJSON, err := json.Marshal(req.Metadata)
	if err != nil {
		return nil, err
	}

	query := `
		UPDATE contacts
		SET name = $3, email = $4, phone = $5, avatar = $6, metadata = $7, updated_at = NOW()
		WHERE id = $1 AND owner_id = $2
		RETURNING updated_at
	`

	var updatedAt time.Time
	err = c.db.QueryRowContext(ctx, query, req.ID, req.OwnerID, req.Name, req.Email, req.Phone, req.Avatar, metadataJSON).Scan(&updatedAt)
	if err == sql.ErrNoRows {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, err
	}

	return json.Marshal(map[string]interface{}{
		"id":         req.ID,
		"owner_id":   req.OwnerID,
		"name":       req.Name,
		"email":      req.Email,
		"phone":      req.Phone,
		"avatar":     req.Avatar,
		"metadata":   req.Metadata,
		"updated_at": updatedAt,
	})
}

func (c *ContactsCapability) deleteContact(ctx context.Context, input []byte) ([]byte, error) {
	var req struct {
		ID      string `json:"id"`
		OwnerID string `json:"owner_id"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return nil, err
	}

	query := `DELETE FROM contacts WHERE id = $1 AND owner_id = $2`
	result, err := c.db.ExecContext(ctx, query, req.ID, req.OwnerID)
	if err != nil {
		return nil, err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}

	return json.Marshal(map[string]interface{}{
		"success": rowsAffected > 0,
		"deleted": rowsAffected > 0,
	})
}

var ErrOperationNotFound = sql.ErrNoRows
