package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/agentmemory"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
)

func (s *DatabaseStore) LoadConversationMemory(ctx context.Context, owner, conversation string) (agentmemory.ConversationMemory, error) {
	owner, conversation = strings.TrimSpace(owner), strings.TrimSpace(conversation)
	if owner == "" || conversation == "" {
		return agentmemory.ConversationMemory{}, fmt.Errorf("owner and conversation_id are required")
	}
	var out agentmemory.ConversationMemory
	var deleted bool
	err := s.db.QueryRowContext(ctx, `SELECT conversation_id,summary,summary_through_seq,last_message_seq,deleted FROM p2p_native_agent_conversations WHERE owner_id=$1 AND conversation_id=$2`, owner, conversation).Scan(&out.ConversationID, &out.Summary, &out.SummaryThroughSeq, &out.LastMessageSeq, &deleted)
	if err == sql.ErrNoRows {
		_, err = s.db.ExecContext(ctx, `INSERT INTO p2p_native_agent_conversations(owner_id,conversation_id,title,active,deleted,revision,last_message_seq,summary,summary_through_seq,created_at,updated_at) VALUES($1,$2,'',TRUE,FALSE,1,0,'',0,NOW(),NOW()) ON CONFLICT DO NOTHING`, owner, conversation)
		if err != nil {
			return agentmemory.ConversationMemory{}, err
		}
		out.ConversationID = conversation
	} else if err != nil {
		return agentmemory.ConversationMemory{}, err
	} else if deleted {
		return agentmemory.ConversationMemory{}, fmt.Errorf("conversation is deleted")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT role,content,references_json FROM p2p_native_agent_messages WHERE owner_id=$1 AND conversation_id=$2 AND seq>$3 ORDER BY seq ASC`, owner, conversation, out.SummaryThroughSeq)
	if err != nil {
		return agentmemory.ConversationMemory{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var role, content string
		var refs []byte
		if err := rows.Scan(&role, &content, &refs); err != nil {
			return agentmemory.ConversationMemory{}, err
		}
		msg := &schema.Message{Role: schema.RoleType(role), Content: content}
		out.Messages = append(out.Messages, msg)
	}
	return out, rows.Err()
}

func (s *DatabaseStore) AppendConversationMessages(ctx context.Context, owner, conversation, turnID string, messages []agentmemory.StoredMessage) error {
	owner, conversation = strings.TrimSpace(owner), strings.TrimSpace(conversation)
	if owner == "" || conversation == "" {
		return fmt.Errorf("owner and conversation_id are required")
	}
	return s.writer.Do(s.db, nil, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO p2p_native_agent_conversations(owner_id,conversation_id,title,active,deleted,revision,last_message_seq,summary,summary_through_seq,created_at,updated_at) VALUES($1,$2,'',TRUE,FALSE,1,0,'',0,NOW(),NOW()) ON CONFLICT DO NOTHING`, owner, conversation); err != nil {
			return err
		}
		var deleted bool
		if err := tx.QueryRowContext(ctx, `SELECT deleted FROM p2p_native_agent_conversations WHERE owner_id=$1 AND conversation_id=$2 FOR UPDATE`, owner, conversation).Scan(&deleted); err != nil {
			return err
		}
		if deleted {
			return fmt.Errorf("conversation is deleted")
		}
		if turnID != "" {
			res, err := tx.ExecContext(ctx, `INSERT INTO p2p_native_agent_memory_turns(owner_id,conversation_id,turn_id,created_at) VALUES($1,$2,$3,NOW()) ON CONFLICT DO NOTHING`, owner, conversation, turnID)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			if n == 0 {
				return nil
			}
		}
		for _, msg := range messages {
			refs, _ := json.Marshal(msg.References)
			content := msg.Content
			if msg.Message != nil && content == "" {
				content = msg.Message.Content
			}
			role := msg.Role
			if role == "" && msg.Message != nil {
				role = string(msg.Message.Role)
			}
			if role != "user" && role != "assistant" {
				continue
			}
			messageID := msg.MessageID
			if messageID == "" {
				messageID = uuid.NewString()
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO p2p_native_agent_messages(owner_id,conversation_id,seq,turn_id,message_id,role,content,references_json,created_at) VALUES($1,$2,(SELECT last_message_seq+1 FROM p2p_native_agent_conversations WHERE owner_id=$1 AND conversation_id=$2),$3,$4,$5,$6,$7,NOW())`, owner, conversation, turnID, messageID, role, content, refs)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE p2p_native_agent_conversations SET last_message_seq=last_message_seq+1,updated_at=NOW() WHERE owner_id=$1 AND conversation_id=$2`, owner, conversation); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *DatabaseStore) SaveConversationSummary(ctx context.Context, owner, conversation, summary string, throughSeq int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE p2p_native_agent_conversations SET summary=$3,summary_through_seq=$4,updated_at=NOW() WHERE owner_id=$1 AND conversation_id=$2 AND deleted=FALSE AND $4>=summary_through_seq AND $4<=last_message_seq`, strings.TrimSpace(owner), strings.TrimSpace(conversation), strings.TrimSpace(summary), throughSeq)
	return err
}

var _ agentmemory.Store = (*DatabaseStore)(nil)
var _ agentmemory.ConversationStore = (*DatabaseStore)(nil)

func conversationMutationResponse(raw []byte) (agentmemory.Conversation, error) {
	var c agentmemory.Conversation
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("decode conversation mutation response: %w", err)
	}
	if c.ID == "" || c.Revision <= 0 || c.CreatedAt.IsZero() || c.UpdatedAt.IsZero() {
		return c, fmt.Errorf("decode conversation mutation response: invalid conversation")
	}
	return c, nil
}

func conversationMutationMiss(ctx context.Context, tx *sql.Tx, owner, id string) error {
	var deleted bool
	err := tx.QueryRowContext(ctx, `SELECT deleted FROM p2p_native_agent_conversations WHERE owner_id=$1 AND conversation_id=$2`, owner, id).Scan(&deleted)
	if err == sql.ErrNoRows {
		return agentmemory.ErrNotFound
	}
	if err != nil {
		return err
	}
	if deleted {
		return agentmemory.ErrDeleted
	}
	return agentmemory.ErrRevisionConflict
}

type conversationCursor struct {
	UpdatedAt time.Time `json:"updated_at"`
	ID        string    `json:"conversation_id"`
}

func encodeConversationCursor(c agentmemory.Conversation) (string, error) {
	raw, err := json.Marshal(conversationCursor{UpdatedAt: c.UpdatedAt.UTC(), ID: c.ID})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeConversationCursor(token string) (conversationCursor, error) {
	if token == "" {
		return conversationCursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return conversationCursor{}, fmt.Errorf("%w: conversation cursor", agentmemory.ErrInvalidCursor)
	}
	var cursor conversationCursor
	if err := decodeStrictCursor(raw, &cursor); err != nil || cursor.ID == "" || cursor.UpdatedAt.IsZero() {
		return conversationCursor{}, fmt.Errorf("%w: conversation cursor", agentmemory.ErrInvalidCursor)
	}
	return cursor, nil
}

func decodeMessageCursor(token string) (int64, error) {
	if token == "" {
		return 0, nil
	}
	seq, err := strconv.ParseInt(token, 10, 64)
	if err != nil || seq <= 0 {
		return 0, fmt.Errorf("%w: message cursor", agentmemory.ErrInvalidCursor)
	}
	return seq, nil
}

func decodeStrictCursor(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("trailing cursor data")
	}
	return nil
}

func mutationReceipt(ctx context.Context, tx *sql.Tx, owner, action, idem string, digest [32]byte) (agentmemory.Conversation, bool, error) {
	if idem == "" {
		return agentmemory.Conversation{}, false, nil
	}
	var old, response []byte
	err := tx.QueryRowContext(ctx, `SELECT request_digest,response_json FROM p2p_native_agent_conversation_mutations WHERE owner_id=$1 AND action=$2 AND idempotency_key=$3`, owner, action, idem).Scan(&old, &response)
	if err == sql.ErrNoRows {
		return agentmemory.Conversation{}, false, nil
	}
	if err != nil {
		return agentmemory.Conversation{}, false, err
	}
	if !bytes.Equal(old, digest[:]) {
		return agentmemory.Conversation{}, false, agentmemory.ErrIdempotencyConflict
	}
	c, err := conversationMutationResponse(response)
	return c, true, err
}

func recordMutation(ctx context.Context, tx *sql.Tx, owner, action, idem string, digest [32]byte, c agentmemory.Conversation) error {
	if idem == "" {
		return nil
	}
	response, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO p2p_native_agent_conversation_mutations(owner_id,action,idempotency_key,request_digest,response_json,created_at) VALUES($1,$2,$3,$4,$5::jsonb,NOW())`, owner, action, idem, digest[:], string(response))
	return err
}

func (s *DatabaseStore) CreateConversation(ctx context.Context, o, id, title, idem string, digest [32]byte) (agentmemory.Conversation, bool, error) {
	var c agentmemory.Conversation
	var replay bool
	err := s.writer.Do(s.db, nil, func(tx *sql.Tx) error {
		var err error
		if c, replay, err = mutationReceipt(ctx, tx, o, "create", idem, digest); err != nil || replay {
			return err
		}
		if err = tx.QueryRowContext(ctx, `INSERT INTO p2p_native_agent_conversations(owner_id,conversation_id,title,active,deleted,revision,last_message_seq,summary,summary_through_seq,created_at,updated_at) VALUES($1,$2,$3,TRUE,FALSE,1,0,'',0,NOW(),NOW()) ON CONFLICT(owner_id,conversation_id) DO UPDATE SET title=p2p_native_agent_conversations.title RETURNING conversation_id,title,active,deleted,revision,last_message_seq,created_at,updated_at`, o, id, title).Scan(&c.ID, &c.Title, &c.Active, &c.Deleted, &c.Revision, &c.LastMessageSeq, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return err
		}
		return recordMutation(ctx, tx, o, "create", idem, digest, c)
	})
	return c, replay, err
}
func (s *DatabaseStore) ListAgentConversations(ctx context.Context, o string, limit int, token string) ([]agentmemory.Conversation, string, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	cursor, err := decodeConversationCursor(token)
	if err != nil {
		return nil, "", err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT conversation_id,title,active,deleted,revision,last_message_seq,created_at,updated_at FROM p2p_native_agent_conversations WHERE owner_id=$1 AND deleted=FALSE AND ($2='' OR updated_at<$3 OR (updated_at=$3 AND conversation_id<$2)) ORDER BY updated_at DESC,conversation_id DESC LIMIT $4`, o, cursor.ID, cursor.UpdatedAt, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []agentmemory.Conversation{}
	for rows.Next() {
		var c agentmemory.Conversation
		if err := rows.Scan(&c.ID, &c.Title, &c.Active, &c.Deleted, &c.Revision, &c.LastMessageSeq, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, "", err
		}
		out = append(out, c)
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next, err = encodeConversationCursor(out[len(out)-1])
		if err != nil {
			return nil, "", err
		}
	}
	return out, next, rows.Err()
}
func (s *DatabaseStore) GetConversation(ctx context.Context, o, id string, limit int, token string) (agentmemory.Conversation, []agentmemory.StoredMessage, string, error) {
	var c agentmemory.Conversation
	err := s.db.QueryRowContext(ctx, `SELECT conversation_id,title,active,deleted,revision,last_message_seq,created_at,updated_at FROM p2p_native_agent_conversations WHERE owner_id=$1 AND conversation_id=$2`, o, id).Scan(&c.ID, &c.Title, &c.Active, &c.Deleted, &c.Revision, &c.LastMessageSeq, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return c, nil, "", err
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	cursor, err := decodeMessageCursor(token)
	if err != nil {
		return c, nil, "", err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT seq,message_id,turn_id,role,content,references_json,created_at FROM p2p_native_agent_messages WHERE owner_id=$1 AND conversation_id=$2 AND ($3=0 OR seq<$3) ORDER BY seq DESC LIMIT $4`, o, id, cursor, limit+1)
	if err != nil {
		return c, nil, "", err
	}
	defer rows.Close()
	out := []agentmemory.StoredMessage{}
	for rows.Next() {
		var m agentmemory.StoredMessage
		var refs []byte
		if err := rows.Scan(&m.Seq, &m.MessageID, &m.TurnID, &m.Role, &m.Content, &refs, &m.CreatedAt); err != nil {
			return c, nil, "", err
		}
		_ = json.Unmarshal(refs, &m.References)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return c, nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = strconv.FormatInt(out[len(out)-1].Seq, 10)
	}
	for left, right := 0, len(out)-1; left < right; left, right = left+1, right-1 {
		out[left], out[right] = out[right], out[left]
	}
	return c, out, next, nil
}
func (s *DatabaseStore) RenameConversation(ctx context.Context, o, id, title string, expected int64, idem string, digest [32]byte) (agentmemory.Conversation, bool, error) {
	var c agentmemory.Conversation
	var replay bool
	err := s.writer.Do(s.db, nil, func(tx *sql.Tx) error {
		var err error
		if c, replay, err = mutationReceipt(ctx, tx, o, "rename", idem, digest); err != nil || replay {
			return err
		}
		if err = tx.QueryRowContext(ctx, `UPDATE p2p_native_agent_conversations SET title=$3,revision=revision+1,updated_at=NOW() WHERE owner_id=$1 AND conversation_id=$2 AND deleted=FALSE AND revision=$4 RETURNING conversation_id,title,active,deleted,revision,last_message_seq,created_at,updated_at`, o, id, title, expected).Scan(&c.ID, &c.Title, &c.Active, &c.Deleted, &c.Revision, &c.LastMessageSeq, &c.CreatedAt, &c.UpdatedAt); err == sql.ErrNoRows {
			return conversationMutationMiss(ctx, tx, o, id)
		} else if err != nil {
			return err
		}
		return recordMutation(ctx, tx, o, "rename", idem, digest, c)
	})
	return c, replay, err
}
func (s *DatabaseStore) DeleteConversation(ctx context.Context, o, id string, expected int64, idem string, digest [32]byte) (agentmemory.Conversation, bool, error) {
	var c agentmemory.Conversation
	var replay bool
	err := s.writer.Do(s.db, nil, func(tx *sql.Tx) error {
		var err error
		if c, replay, err = mutationReceipt(ctx, tx, o, "delete", idem, digest); err != nil || replay {
			return err
		}
		if err = tx.QueryRowContext(ctx, `UPDATE p2p_native_agent_conversations SET deleted=TRUE,active=FALSE,revision=revision+1,deleted_at=NOW(),updated_at=NOW() WHERE owner_id=$1 AND conversation_id=$2 AND deleted=FALSE AND revision=$3 RETURNING conversation_id,title,active,deleted,revision,last_message_seq,created_at,updated_at`, o, id, expected).Scan(&c.ID, &c.Title, &c.Active, &c.Deleted, &c.Revision, &c.LastMessageSeq, &c.CreatedAt, &c.UpdatedAt); err == sql.ErrNoRows {
			return conversationMutationMiss(ctx, tx, o, id)
		} else if err != nil {
			return err
		}
		return recordMutation(ctx, tx, o, "delete", idem, digest, c)
	})
	return c, replay, err
}

func (s *DatabaseStore) CreateKnowledgeMemory(ctx context.Context, owner, title, content string, tags []string, idem string, digest [32]byte) (agentmemory.KnowledgeMemory, bool, error) {
	if strings.TrimSpace(owner) == "" || strings.TrimSpace(content) == "" || idem == "" {
		return agentmemory.KnowledgeMemory{}, false, fmt.Errorf("owner, content and idempotency_key are required")
	}
	tagsJSON, _ := json.Marshal(tags)
	var item agentmemory.KnowledgeMemory
	var created time.Time
	var replay bool
	err := s.writer.Do(s.db, nil, func(tx *sql.Tx) error {
		var existingDigest []byte
		err := tx.QueryRowContext(ctx, `INSERT INTO p2p_native_agent_memory_records(owner_id,memory_id,title,content,tags_json,request_digest,created_at) VALUES($1,$2,$3,$4,$5,$6,NOW()) ON CONFLICT(owner_id,memory_id) DO NOTHING RETURNING memory_id,title,content,tags_json,created_at`, owner, idem, title, content, tagsJSON, digest[:]).Scan(&item.ID, &item.Title, &item.Content, &tagsJSON, &created)
		if err == nil {
			return nil
		}
		if err != sql.ErrNoRows {
			return err
		}
		replay = true
		if err = tx.QueryRowContext(ctx, `SELECT memory_id,title,content,tags_json,created_at,request_digest FROM p2p_native_agent_memory_records WHERE owner_id=$1 AND memory_id=$2`, owner, idem).Scan(&item.ID, &item.Title, &item.Content, &tagsJSON, &created, &existingDigest); err != nil {
			return err
		}
		if !bytes.Equal(existingDigest, digest[:]) {
			return agentmemory.ErrIdempotencyConflict
		}
		return nil
	})
	if err != nil {
		return item, false, err
	}
	item.CreatedAt = created
	_ = json.Unmarshal(tagsJSON, &item.Tags)
	return item, replay, nil
}
func (s *DatabaseStore) SearchKnowledgeMemory(ctx context.Context, owner, query string, limit int, token string) ([]agentmemory.KnowledgeMemory, string, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	cursor, err := decodeKnowledgeCursor(token)
	if err != nil {
		return nil, "", err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT memory_id,title,content,tags_json,created_at FROM p2p_native_agent_memory_records WHERE owner_id=$1 AND ($2='' OR title ILIKE '%'||$2||'%' OR content ILIKE '%'||$2||'%') AND ($3='' OR created_at>$4 OR (created_at=$4 AND memory_id>$3)) ORDER BY created_at ASC,memory_id ASC LIMIT $5`, owner, strings.TrimSpace(query), cursor.ID, cursor.CreatedAt, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []agentmemory.KnowledgeMemory{}
	for rows.Next() {
		var i agentmemory.KnowledgeMemory
		var tags []byte
		if err := rows.Scan(&i.ID, &i.Title, &i.Content, &tags, &i.CreatedAt); err != nil {
			return nil, "", err
		}
		_ = json.Unmarshal(tags, &i.Tags)
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next, err = encodeKnowledgeCursor(out[len(out)-1])
		if err != nil {
			return nil, "", err
		}
	}
	return out, next, nil
}

type knowledgeCursor struct {
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"memory_id"`
}

func encodeKnowledgeCursor(item agentmemory.KnowledgeMemory) (string, error) {
	raw, err := json.Marshal(knowledgeCursor{CreatedAt: item.CreatedAt.UTC(), ID: item.ID})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeKnowledgeCursor(token string) (knowledgeCursor, error) {
	if token == "" {
		return knowledgeCursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return knowledgeCursor{}, fmt.Errorf("%w: knowledge cursor", agentmemory.ErrInvalidCursor)
	}
	var cursor knowledgeCursor
	if err := decodeStrictCursor(raw, &cursor); err != nil || cursor.ID == "" || cursor.CreatedAt.IsZero() {
		return knowledgeCursor{}, fmt.Errorf("%w: knowledge cursor", agentmemory.ErrInvalidCursor)
	}
	return cursor, nil
}
func (s *DatabaseStore) KnowledgeStatus(ctx context.Context, owner string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM p2p_native_agent_memory_records WHERE owner_id=$1`, owner).Scan(&n)
	return n, err
}
