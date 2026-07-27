package agentmemory

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/cloudwego/eino/schema"
	"strings"
	"sync"
	"time"
)

var ErrIdempotencyConflict = fmt.Errorf("idempotency conflict")
var ErrRevisionConflict = fmt.Errorf("revision conflict")
var ErrNotFound = fmt.Errorf("conversation not found")
var ErrDeleted = fmt.Errorf("conversation is deleted")
var ErrInvalidCursor = errors.New("invalid cursor")

type ConversationMemory struct {
	ConversationID                    string
	Summary                           string
	SummaryThroughSeq, LastMessageSeq int64
	Messages                          []*schema.Message
}
type StoredMessage struct {
	MessageID, TurnID, Role, Content string
	References                       []map[string]any
	Message                          *schema.Message
	CreatedAt                        time.Time
	Seq                              int64
}
type Store interface {
	LoadConversationMemory(context.Context, string, string) (ConversationMemory, error)
	AppendConversationMessages(context.Context, string, string, string, []StoredMessage) error
	SaveConversationSummary(context.Context, string, string, string, int64) error
}

type KnowledgeMemory struct {
	ID, Title, Content string
	Tags               []string
	CreatedAt          time.Time
}
type KnowledgeStore interface {
	CreateKnowledgeMemory(context.Context, string, string, string, []string, string, [32]byte) (KnowledgeMemory, bool, error)
	SearchKnowledgeMemory(context.Context, string, string, int, string) ([]KnowledgeMemory, string, error)
	KnowledgeStatus(context.Context, string) (int, error)
}
type Conversation struct {
	ID, Title              string
	Active, Deleted        bool
	Revision               int64
	LastMessageSeq         int64
	Summary, SummaryCursor string
	UpdatedAt, CreatedAt   time.Time
}
type ConversationStore interface {
	CreateConversation(context.Context, string, string, string, string, [32]byte) (Conversation, bool, error)
	ListAgentConversations(context.Context, string, int, string) ([]Conversation, string, error)
	GetConversation(context.Context, string, string, int, string) (Conversation, []StoredMessage, string, error)
	RenameConversation(context.Context, string, string, string, int64, string, [32]byte) (Conversation, bool, error)
	DeleteConversation(context.Context, string, string, int64, string, [32]byte) (Conversation, bool, error)
}
type InMemoryStore struct {
	mu            sync.Mutex
	data          map[string]*record
	knowledge     map[string][]knowledgeRecord
	conversations map[string]Conversation
	receipts      map[string][32]byte
}
type knowledgeRecord struct {
	item   KnowledgeMemory
	digest [32]byte
}
type record struct {
	memory   ConversationMemory
	messages []StoredMessage
	turns    map[string]struct{}
}

func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{data: map[string]*record{}, knowledge: map[string][]knowledgeRecord{}, conversations: map[string]Conversation{}, receipts: map[string][32]byte{}}
}
func key(o, c string) string { return strings.TrimSpace(o) + "\x00" + strings.TrimSpace(c) }
func clone(m *schema.Message) *schema.Message {
	if m == nil {
		return nil
	}
	o := *m
	o.ToolCalls = append([]schema.ToolCall(nil), m.ToolCalls...)
	return &o
}
func (s *InMemoryStore) LoadConversationMemory(ctx context.Context, o, c string) (ConversationMemory, error) {
	if err := ctx.Err(); err != nil {
		return ConversationMemory{}, err
	}
	if strings.TrimSpace(o) == "" || strings.TrimSpace(c) == "" {
		return ConversationMemory{}, fmt.Errorf("owner and conversation_id are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.data[key(o, c)]
	if r == nil {
		r = &record{memory: ConversationMemory{ConversationID: c}, turns: map[string]struct{}{}}
		s.data[key(o, c)] = r
	}
	out := r.memory
	out.Messages = make([]*schema.Message, 0, len(r.memory.Messages))
	for _, m := range r.memory.Messages {
		out.Messages = append(out.Messages, clone(m))
	}
	return out, nil
}
func (s *InMemoryStore) AppendConversationMessages(ctx context.Context, o, c, t string, msgs []StoredMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.data[key(o, c)]
	if r == nil {
		r = &record{memory: ConversationMemory{ConversationID: c}, turns: map[string]struct{}{}}
		s.data[key(o, c)] = r
	}
	if t != "" {
		if _, ok := r.turns[t]; ok {
			return nil
		}
		r.turns[t] = struct{}{}
	}
	for _, m := range msgs {
		m.Seq = r.memory.LastMessageSeq + 1
		if m.CreatedAt.IsZero() {
			m.CreatedAt = time.Now().UTC()
		}
		r.messages = append(r.messages, m)
		r.memory.LastMessageSeq = m.Seq
		if m.Message != nil {
			r.memory.Messages = append(r.memory.Messages, clone(m.Message))
		}
	}
	return nil
}
func (s *InMemoryStore) SaveConversationSummary(ctx context.Context, o, c, sum string, through int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.data[key(o, c)]
	if r == nil {
		r = &record{memory: ConversationMemory{ConversationID: c}, turns: map[string]struct{}{}}
		s.data[key(o, c)] = r
	}
	if through < r.memory.SummaryThroughSeq {
		return nil
	}
	r.memory.Summary = strings.TrimSpace(sum)
	r.memory.SummaryThroughSeq = through
	return nil
}

func (s *InMemoryStore) CreateKnowledgeMemory(ctx context.Context, owner, title, content string, tags []string, idem string, digest [32]byte) (KnowledgeMemory, bool, error) {
	if err := ctx.Err(); err != nil {
		return KnowledgeMemory{}, false, err
	}
	if strings.TrimSpace(owner) == "" || strings.TrimSpace(content) == "" {
		return KnowledgeMemory{}, false, fmt.Errorf("owner and content are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	items := s.knowledge[strings.TrimSpace(owner)]
	for _, r := range items {
		if idem != "" && r.item.ID == idem {
			if r.digest != digest {
				return KnowledgeMemory{}, false, ErrIdempotencyConflict
			}
			return r.item, true, nil
		}
	}
	id := idem
	if id == "" {
		id = fmt.Sprintf("memory-%d", len(items)+1)
	}
	item := KnowledgeMemory{ID: id, Title: strings.TrimSpace(title), Content: content, Tags: append([]string(nil), tags...), CreatedAt: time.Now().UTC()}
	s.knowledge[owner] = append(items, knowledgeRecord{item: item, digest: digest})
	return item, false, nil
}
func (s *InMemoryStore) SearchKnowledgeMemory(ctx context.Context, owner, query string, limit int, token string) ([]KnowledgeMemory, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	items := s.knowledge[strings.TrimSpace(owner)]
	start := 0
	if token != "" {
		_, _ = fmt.Sscanf(token, "%d", &start)
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	query = strings.ToLower(strings.TrimSpace(query))
	out := []KnowledgeMemory{}
	for i := start; i < len(items) && len(out) < limit; i++ {
		if query == "" || strings.Contains(strings.ToLower(items[i].item.Title), query) || strings.Contains(strings.ToLower(items[i].item.Content), query) {
			out = append(out, items[i].item)
		}
	}
	next := ""
	if start+len(out) < len(items) {
		next = fmt.Sprintf("%d", start+len(out))
	}
	return out, next, nil
}
func (s *InMemoryStore) KnowledgeStatus(ctx context.Context, owner string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.knowledge[strings.TrimSpace(owner)]), nil
}

func KnowledgeDigest(title, content string, tags []string) [32]byte {
	h := sha256.New()
	h.Write([]byte(title))
	h.Write([]byte("\x00"))
	h.Write([]byte(content))
	for _, t := range tags {
		h.Write([]byte("\x00"))
		h.Write([]byte(t))
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func (s *InMemoryStore) CreateConversation(ctx context.Context, owner, id, title, idem string, digest [32]byte) (Conversation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if idem != "" {
		rk := key(owner, "create\x00"+idem)
		if old, ok := s.receipts[rk]; ok {
			if old != digest {
				return Conversation{}, false, ErrIdempotencyConflict
			}
			if c, exists := s.conversations[key(owner, id)]; exists {
				return c, true, nil
			}
		}
		s.receipts[rk] = digest
	}
	k := key(owner, id)
	if c, ok := s.conversations[k]; ok {
		return c, true, nil
	}
	now := time.Now().UTC()
	c := Conversation{ID: id, Title: title, Active: true, Revision: 1, CreatedAt: now, UpdatedAt: now}
	s.conversations[k] = c
	return c, false, nil
}
func (s *InMemoryStore) ListAgentConversations(ctx context.Context, owner string, limit int, token string) ([]Conversation, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Conversation{}
	for k, c := range s.conversations {
		if strings.HasPrefix(k, strings.TrimSpace(owner)+"\x00") && !c.Deleted {
			out = append(out, c)
		}
	}
	if limit <= 0 {
		limit = 20
	}
	if len(out) > limit {
		return out[:limit], fmt.Sprintf("%d", limit), nil
	}
	return out, "", nil
}
func (s *InMemoryStore) GetConversation(ctx context.Context, owner, id string, limit int, token string) (Conversation, []StoredMessage, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.conversations[key(owner, id)]
	if !ok {
		return Conversation{}, nil, "", ErrNotFound
	}
	r := s.data[key(owner, id)]
	if r == nil {
		return c, nil, "", nil
	}
	return c, append([]StoredMessage(nil), r.messages...), "", nil
}
func (s *InMemoryStore) RenameConversation(ctx context.Context, owner, id, title string, expected int64, idem string, digest [32]byte) (Conversation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(owner, id)
	if idem != "" {
		rk := key(owner, "rename\x00"+idem)
		if old, ok := s.receipts[rk]; ok && old != digest {
			return Conversation{}, false, ErrIdempotencyConflict
		}
		s.receipts[rk] = digest
	}
	c, ok := s.conversations[k]
	if !ok {
		return Conversation{}, false, ErrNotFound
	}
	if c.Deleted {
		return c, false, ErrDeleted
	}
	if expected != c.Revision {
		return c, false, ErrRevisionConflict
	}
	c.Title = title
	c.Revision++
	c.UpdatedAt = time.Now().UTC()
	s.conversations[k] = c
	return c, false, nil
}
func (s *InMemoryStore) DeleteConversation(ctx context.Context, owner, id string, expected int64, idem string, digest [32]byte) (Conversation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(owner, id)
	if idem != "" {
		rk := key(owner, "delete\x00"+idem)
		if old, ok := s.receipts[rk]; ok && old != digest {
			return Conversation{}, false, ErrIdempotencyConflict
		}
		s.receipts[rk] = digest
	}
	c, ok := s.conversations[k]
	if !ok {
		return Conversation{}, false, ErrNotFound
	}
	if expected != c.Revision {
		return c, false, ErrRevisionConflict
	}
	c.Deleted = true
	c.Active = false
	c.Revision++
	c.UpdatedAt = time.Now().UTC()
	s.conversations[k] = c
	return c, false, nil
}
