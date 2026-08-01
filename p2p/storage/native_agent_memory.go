package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/agentmemory"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

func (s *DatabaseStore) LoadConversationMemory(ctx context.Context, owner, conversation string) (agentmemory.ConversationMemory, error) {
	owner, conversation = strings.TrimSpace(owner), strings.TrimSpace(conversation)
	if owner == "" || conversation == "" {
		return agentmemory.ConversationMemory{}, fmt.Errorf("owner and conversation_id are required")
	}
	var out agentmemory.ConversationMemory
	var deleted bool
	err := s.db.QueryRowContext(ctx, `SELECT conversation_id,title,summary,summary_through_seq,last_message_seq,deleted FROM p2p_native_agent_conversations WHERE owner_id=$1 AND conversation_id=$2`, owner, conversation).Scan(&out.ConversationID, &out.Title, &out.Summary, &out.SummaryThroughSeq, &out.LastMessageSeq, &deleted)
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

func (s *DatabaseStore) SetAutomaticConversationTitle(ctx context.Context, owner, conversation, title string) (bool, error) {
	owner, conversation, title = strings.TrimSpace(owner), strings.TrimSpace(conversation), strings.TrimSpace(title)
	if owner == "" || conversation == "" || title == "" {
		return false, nil
	}
	result, err := s.db.ExecContext(ctx, `UPDATE p2p_native_agent_conversations SET title=$3,revision=revision+1,updated_at=NOW() WHERE owner_id=$1 AND conversation_id=$2 AND deleted=FALSE AND title=''`, owner, conversation, title)
	if err != nil {
		return false, err
	}
	updated, err := result.RowsAffected()
	return updated == 1, err
}

var _ agentmemory.Store = (*DatabaseStore)(nil)
var _ agentmemory.ConversationStore = (*DatabaseStore)(nil)
var _ agentmemory.AutomaticConversationTitleStore = (*DatabaseStore)(nil)
var _ agentmemory.ManagedKnowledgeStore = (*DatabaseStore)(nil)
var _ agentmemory.KnowledgeSourceStore = (*DatabaseStore)(nil)

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

func (s *DatabaseStore) ListKnowledgeMemories(ctx context.Context, owner string, limit int, token string) ([]agentmemory.KnowledgeMemory, string, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	cursor, err := decodeKnowledgeCursor(token)
	if err != nil {
		return nil, "", err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT memory_id,title,content,tags_json,revision,created_at,updated_at FROM p2p_native_agent_memory_records WHERE owner_id=$1 AND source_id IS NULL AND ($2='' OR created_at>$3 OR (created_at=$3 AND memory_id>$2)) ORDER BY created_at ASC,memory_id ASC LIMIT $4`, owner, cursor.ID, cursor.CreatedAt, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []agentmemory.KnowledgeMemory{}
	for rows.Next() {
		var i agentmemory.KnowledgeMemory
		var tags []byte
		if err := rows.Scan(&i.ID, &i.Title, &i.Content, &tags, &i.Revision, &i.CreatedAt, &i.UpdatedAt); err != nil {
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

func (s *DatabaseStore) UpdateKnowledgeMemory(ctx context.Context, owner, id, title, content string, tags []string, expected int64, idem string, digest [32]byte, openSession agentmemory.KnowledgeEmbeddingSessionFunc) (agentmemory.KnowledgeMemory, bool, error) {
	owner, id = strings.TrimSpace(owner), strings.TrimSpace(id)
	if owner == "" || id == "" || expected <= 0 || idem == "" {
		return agentmemory.KnowledgeMemory{}, false, fmt.Errorf("owner, memory_id, expected_revision and idempotency_key are required")
	}
	var replayedItem agentmemory.KnowledgeMemory
	if ok, err := readKnowledgeReceipt(ctx, s.db, owner, "knowledge.update", idem, digest, &replayedItem); err != nil {
		return agentmemory.KnowledgeMemory{}, false, err
	} else if ok {
		return replayedItem, true, nil
	}
	var emb *agentmemory.KnowledgeEmbedding
	if openSession != nil {
		session, err := openSession(ctx)
		if err != nil {
			return agentmemory.KnowledgeMemory{}, false, err
		}
		if session.Embed != nil {
			v, err := session.Embed(ctx, strings.TrimSpace(title)+"\n"+content)
			if err != nil {
				return agentmemory.KnowledgeMemory{}, false, err
			}
			e := &agentmemory.KnowledgeEmbedding{ProfileID: session.ProfileID, Revision: session.Revision, Model: session.Model, Vector: v}
			if !validVector(v) {
				return agentmemory.KnowledgeMemory{}, false, errors.New("invalid embedding vector")
			}
			if session.ValidateCurrent != nil {
				if err := session.ValidateCurrent(ctx); err != nil {
					return agentmemory.KnowledgeMemory{}, false, err
				}
			}
			emb = e
		}
	}
	tagsJSON, _ := json.Marshal(tags)
	var item agentmemory.KnowledgeMemory
	var replay bool
	err := s.writer.Do(s.db, nil, func(tx *sql.Tx) error {
		var oldDigest, response []byte
		err := tx.QueryRowContext(ctx, `SELECT request_digest,response_json FROM p2p_native_agent_conversation_mutations WHERE owner_id=$1 AND action=$2 AND idempotency_key=$3`, owner, "knowledge.update", idem).Scan(&oldDigest, &response)
		if err == nil {
			if !bytes.Equal(oldDigest, digest[:]) {
				return agentmemory.ErrIdempotencyConflict
			}
			replay = true
			return json.Unmarshal(response, &item)
		}
		if err != sql.ErrNoRows {
			return err
		}
		var created, updated time.Time
		err = tx.QueryRowContext(ctx, `UPDATE p2p_native_agent_memory_records SET title=$3,content=$4,tags_json=$5,revision=revision+1,updated_at=NOW() WHERE owner_id=$1 AND memory_id=$2 AND source_id IS NULL AND revision=$6 RETURNING memory_id,title,content,tags_json,revision,created_at,updated_at`, owner, id, title, content, tagsJSON, expected).Scan(&item.ID, &item.Title, &item.Content, &tagsJSON, &item.Revision, &created, &updated)
		if err == sql.ErrNoRows {
			var rev int64
			if e := tx.QueryRowContext(ctx, `SELECT revision FROM p2p_native_agent_memory_records WHERE owner_id=$1 AND memory_id=$2 AND source_id IS NULL`, owner, id).Scan(&rev); e == sql.ErrNoRows {
				return agentmemory.ErrNotFound
			}
			return agentmemory.ErrRevisionConflict
		}
		if err != nil {
			return err
		}
		item.CreatedAt, item.UpdatedAt = created, updated
		_ = json.Unmarshal(tagsJSON, &item.Tags)
		if emb != nil {
			vals := make([]float64, len(emb.Vector))
			for i, v := range emb.Vector {
				vals[i] = float64(v)
			}
			res, err := tx.ExecContext(ctx, `INSERT INTO p2p_native_agent_memory_embeddings(owner_id,memory_id,profile_id,profile_revision,model,dimension,content_digest,vector,indexed_at) SELECT $1,$2,$3,$4,$5,$6,$7,$8,NOW() WHERE EXISTS (SELECT 1 FROM p2p_agent_model_profile_defaults d JOIN p2p_agent_model_profiles p ON p.owner_id=d.owner_id AND p.profile_id=d.embedding_profile_id WHERE d.owner_id=$1 AND d.embedding_profile_id=$3 AND p.revision=$4 AND p.model=$5 AND p.deleted_at IS NULL) ON CONFLICT(owner_id,memory_id) DO UPDATE SET profile_id=EXCLUDED.profile_id,profile_revision=EXCLUDED.profile_revision,model=EXCLUDED.model,dimension=EXCLUDED.dimension,content_digest=EXCLUDED.content_digest,vector=EXCLUDED.vector,indexed_at=EXCLUDED.indexed_at WHERE EXISTS (SELECT 1 FROM p2p_agent_model_profile_defaults d JOIN p2p_agent_model_profiles p ON p.owner_id=d.owner_id AND p.profile_id=d.embedding_profile_id WHERE d.owner_id=EXCLUDED.owner_id AND d.embedding_profile_id=EXCLUDED.profile_id AND p.revision=EXCLUDED.profile_revision AND p.model=EXCLUDED.model AND p.deleted_at IS NULL)`, owner, id, emb.ProfileID, emb.Revision, emb.Model, len(vals), digest[:], pq.Array(vals))
			if err != nil {
				return err
			}
			if affected, err := res.RowsAffected(); err == nil && affected == 0 {
				return agentmemory.ErrEmbeddingSessionStale
			}
		}
		encoded, _ := json.Marshal(item)
		_, err = tx.ExecContext(ctx, `INSERT INTO p2p_native_agent_conversation_mutations(owner_id,action,idempotency_key,request_digest,response_json,created_at) VALUES($1,$2,$3,$4,$5::jsonb,NOW())`, owner, "knowledge.update", idem, digest[:], string(encoded))
		return err
	})
	return item, replay, err
}

func (s *DatabaseStore) DeleteKnowledgeMemory(ctx context.Context, owner, id string, expected int64, idem string, digest [32]byte) (agentmemory.KnowledgeMemory, bool, error) {
	owner, id = strings.TrimSpace(owner), strings.TrimSpace(id)
	if owner == "" || id == "" || expected <= 0 || idem == "" {
		return agentmemory.KnowledgeMemory{}, false, fmt.Errorf("owner, memory_id, expected_revision and idempotency_key are required")
	}
	var item agentmemory.KnowledgeMemory
	var replay bool
	err := s.writer.Do(s.db, nil, func(tx *sql.Tx) error {
		var old, response []byte
		err := tx.QueryRowContext(ctx, `SELECT request_digest,response_json FROM p2p_native_agent_conversation_mutations WHERE owner_id=$1 AND action=$2 AND idempotency_key=$3`, owner, "knowledge.delete", idem).Scan(&old, &response)
		if err == nil {
			if !bytes.Equal(old, digest[:]) {
				return agentmemory.ErrIdempotencyConflict
			}
			replay = true
			return json.Unmarshal(response, &item)
		}
		if err != sql.ErrNoRows {
			return err
		}
		var tags []byte
		err = tx.QueryRowContext(ctx, `DELETE FROM p2p_native_agent_memory_records WHERE owner_id=$1 AND memory_id=$2 AND source_id IS NULL AND revision=$3 RETURNING memory_id,title,content,tags_json,revision,created_at,updated_at`, owner, id, expected).Scan(&item.ID, &item.Title, &item.Content, &tags, &item.Revision, &item.CreatedAt, &item.UpdatedAt)
		if err == sql.ErrNoRows {
			var rev int64
			if e := tx.QueryRowContext(ctx, `SELECT revision FROM p2p_native_agent_memory_records WHERE owner_id=$1 AND memory_id=$2 AND source_id IS NULL`, owner, id).Scan(&rev); e == sql.ErrNoRows {
				return agentmemory.ErrNotFound
			}
			return agentmemory.ErrRevisionConflict
		}
		if err != nil {
			return err
		}
		_ = json.Unmarshal(tags, &item.Tags)
		encoded, _ := json.Marshal(item)
		_, err = tx.ExecContext(ctx, `INSERT INTO p2p_native_agent_conversation_mutations(owner_id,action,idempotency_key,request_digest,response_json,created_at) VALUES($1,$2,$3,$4,$5::jsonb,NOW())`, owner, "knowledge.delete", idem, digest[:], string(encoded))
		return err
	})
	return item, replay, err
}

func (s *DatabaseStore) CreateKnowledgeMemorySemantic(ctx context.Context, owner, title, content string, tags []string, idem string, digest [32]byte, openSession agentmemory.KnowledgeEmbeddingSessionFunc) (agentmemory.KnowledgeMemory, bool, bool, agentmemory.KnowledgeEmbedding, error) {
	if openSession == nil {
		return agentmemory.KnowledgeMemory{}, false, false, agentmemory.KnowledgeEmbedding{}, agentmemory.ErrNoEmbeddingProfile
	}
	item, replayed, err := s.CreateKnowledgeMemory(ctx, owner, title, content, tags, idem, digest)
	if err != nil {
		return item, replayed, false, agentmemory.KnowledgeEmbedding{}, err
	}
	session, err := openSession(ctx)
	if err != nil {
		return item, replayed, false, agentmemory.KnowledgeEmbedding{}, err
	}
	current := agentmemory.KnowledgeEmbedding{ProfileID: session.ProfileID, Revision: session.Revision, Model: session.Model}
	var vector []float64
	var oldProfile, oldModel string
	var oldRevision int64
	var oldDimension int
	var oldDigest []byte
	rowErr := s.db.QueryRowContext(ctx, `SELECT profile_id,profile_revision,model,dimension,content_digest,vector FROM p2p_native_agent_memory_embeddings WHERE owner_id=$1 AND memory_id=$2`, strings.TrimSpace(owner), item.ID).Scan(&oldProfile, &oldRevision, &oldModel, &oldDimension, &oldDigest, pq.Array(&vector))
	if rowErr == nil && bytes.Equal(oldDigest, digest[:]) && oldProfile == current.ProfileID && oldRevision == current.Revision && oldModel == current.Model && oldDimension == len(vector) {
		current.Vector = make([]float32, len(vector))
		for i, v := range vector {
			current.Vector[i] = float32(v)
		}
		return item, replayed, validVector(current.Vector), current, nil
	}
	if session.Embed == nil {
		return item, replayed, false, current, errors.New("embedding session is not ready")
	}
	values, err := session.Embed(ctx, item.Title+"\n"+item.Content)
	if err != nil {
		return item, replayed, false, current, err
	}
	current.Vector = append([]float32(nil), values...)
	if !validVector(current.Vector) {
		return item, replayed, false, current, errors.New("invalid embedding vector")
	}
	if session.ValidateCurrent != nil {
		if err := session.ValidateCurrent(ctx); err != nil {
			return item, replayed, false, current, err
		}
	}
	dimension := len(current.Vector)
	encoded := make([]float64, dimension)
	for i, v := range current.Vector {
		encoded[i] = float64(v)
	}
	var result sql.Result
	result, err = s.db.ExecContext(ctx, `INSERT INTO p2p_native_agent_memory_embeddings(owner_id,memory_id,profile_id,profile_revision,model,dimension,content_digest,vector,indexed_at) SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9 WHERE EXISTS (SELECT 1 FROM p2p_agent_model_profile_defaults d JOIN p2p_agent_model_profiles p ON p.owner_id=d.owner_id AND p.profile_id=d.embedding_profile_id WHERE d.owner_id=$1 AND d.embedding_profile_id=$3 AND p.profile_id=$3 AND p.revision=$4 AND p.model=$5 AND p.deleted_at IS NULL) ON CONFLICT(owner_id,memory_id) DO UPDATE SET profile_id=EXCLUDED.profile_id,profile_revision=EXCLUDED.profile_revision,model=EXCLUDED.model,dimension=EXCLUDED.dimension,content_digest=EXCLUDED.content_digest,vector=EXCLUDED.vector,indexed_at=EXCLUDED.indexed_at WHERE EXISTS (SELECT 1 FROM p2p_agent_model_profile_defaults d JOIN p2p_agent_model_profiles p ON p.owner_id=d.owner_id AND p.profile_id=d.embedding_profile_id WHERE d.owner_id=EXCLUDED.owner_id AND d.embedding_profile_id=EXCLUDED.profile_id AND p.revision=EXCLUDED.profile_revision AND p.model=EXCLUDED.model AND p.deleted_at IS NULL)`, owner, item.ID, current.ProfileID, current.Revision, current.Model, dimension, digest[:], pq.Array(encoded), time.Now().UTC())
	if err != nil {
		return item, replayed, false, current, err
	}
	if affected, rowsErr := result.RowsAffected(); rowsErr == nil && affected == 0 {
		return item, replayed, false, current, agentmemory.ErrEmbeddingSessionStale
	}
	return item, replayed, true, current, nil
}

func (s *DatabaseStore) SearchKnowledgeMemorySemantic(ctx context.Context, owner, query string, limit int, token string, openSession agentmemory.KnowledgeEmbeddingSessionFunc) ([]agentmemory.KnowledgeMemory, string, agentmemory.KnowledgeEmbedding, error) {
	if strings.TrimSpace(token) != "" {
		return nil, "", agentmemory.KnowledgeEmbedding{}, agentmemory.ErrInvalidCursor
	}
	if openSession == nil {
		return nil, "", agentmemory.KnowledgeEmbedding{}, agentmemory.ErrNoEmbeddingProfile
	}
	session, err := openSession(ctx)
	if err != nil {
		return nil, "", agentmemory.KnowledgeEmbedding{}, err
	}
	if session.Embed == nil {
		return nil, "", agentmemory.KnowledgeEmbedding{}, errors.New("embedding session is not ready")
	}
	values, err := session.Embed(ctx, query)
	if err != nil {
		return nil, "", agentmemory.KnowledgeEmbedding{}, err
	}
	queryEmbedding := agentmemory.KnowledgeEmbedding{ProfileID: session.ProfileID, Revision: session.Revision, Model: session.Model, Vector: values}
	if !validVector(queryEmbedding.Vector) {
		return nil, "", agentmemory.KnowledgeEmbedding{}, errors.New("invalid embedding vector")
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `SELECT m.memory_id,m.title,m.content,m.tags_json,m.created_at,e.vector FROM p2p_native_agent_memory_embeddings e JOIN p2p_native_agent_memory_records m ON m.owner_id=e.owner_id AND m.memory_id=e.memory_id WHERE e.owner_id=$1 AND e.profile_id=$2 AND e.profile_revision=$3 AND e.model=$4 AND e.dimension=$5 ORDER BY m.created_at ASC,m.memory_id ASC LIMIT $6`, owner, queryEmbedding.ProfileID, queryEmbedding.Revision, queryEmbedding.Model, len(queryEmbedding.Vector), 10001)
	if err != nil {
		return nil, "", queryEmbedding, err
	}
	defer rows.Close()
	type scored struct {
		item  agentmemory.KnowledgeMemory
		score float64
	}
	matches := make([]scored, 0)
	scanned := 0
	for rows.Next() {
		scanned++
		if scanned > 10000 {
			return nil, "", queryEmbedding, errors.New("semantic search candidate capacity exceeded")
		}
		var item agentmemory.KnowledgeMemory
		var tags []byte
		var vector []float64
		if err := rows.Scan(&item.ID, &item.Title, &item.Content, &tags, &item.CreatedAt, pq.Array(&vector)); err != nil {
			return nil, "", queryEmbedding, err
		}
		_ = json.Unmarshal(tags, &item.Tags)
		fv := make([]float32, len(vector))
		if len(vector) != len(queryEmbedding.Vector) {
			return nil, "", queryEmbedding, errors.New("stored embedding dimension mismatch")
		}
		for i, v := range vector {
			fv[i] = float32(v)
		}
		if !validVector(fv) {
			continue
		}
		matches = append(matches, scored{item: item, score: cosine(queryEmbedding.Vector, fv)})
	}
	if err := rows.Err(); err != nil {
		return nil, "", queryEmbedding, err
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		return matches[i].item.ID < matches[j].item.ID
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}
	out := make([]agentmemory.KnowledgeMemory, len(matches))
	for i := range matches {
		out[i] = matches[i].item
	}
	return out, "", queryEmbedding, nil
}

func (s *DatabaseStore) KnowledgeStatusSemantic(ctx context.Context, owner string, openSession agentmemory.KnowledgeEmbeddingSessionFunc) (int, int, int, agentmemory.KnowledgeEmbedding, error) {
	if openSession == nil {
		return 0, 0, 0, agentmemory.KnowledgeEmbedding{}, agentmemory.ErrNoEmbeddingProfile
	}
	session, err := openSession(ctx)
	if err != nil {
		return 0, 0, 0, agentmemory.KnowledgeEmbedding{}, err
	}
	profile := agentmemory.KnowledgeEmbedding{ProfileID: session.ProfileID, Revision: session.Revision, Model: session.Model}
	var total, indexed int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM p2p_native_agent_memory_records WHERE owner_id=$1`, owner).Scan(&total); err != nil {
		return 0, 0, 0, agentmemory.KnowledgeEmbedding{}, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM p2p_native_agent_memory_embeddings WHERE owner_id=$1 AND profile_id=$2 AND profile_revision=$3 AND model=$4`, owner, profile.ProfileID, profile.Revision, profile.Model).Scan(&indexed); err != nil {
		return total, 0, total, profile, err
	}
	return total, indexed, total - indexed, profile, nil
}

func validVector(v []float32) bool {
	if len(v) == 0 {
		return false
	}
	for _, x := range v {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return false
		}
	}
	return true
}
func cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		if i >= len(b) {
			break
		}
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / math.Sqrt(na*nb)
}
