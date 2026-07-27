package agentmemory

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/cloudwego/eino/schema"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

var ErrIdempotencyConflict = fmt.Errorf("idempotency conflict")
var ErrRevisionConflict = fmt.Errorf("revision conflict")
var ErrNotFound = fmt.Errorf("conversation not found")
var ErrDeleted = fmt.Errorf("conversation is deleted")
var ErrInvalidCursor = errors.New("invalid cursor")
var ErrNoEmbeddingProfile = errors.New("no default embedding profile")
var ErrEmbeddingSessionStale = errors.New("embedding profile changed during indexing")

const MaxEmbeddingDimension = 32768

// KnowledgeEmbedding is the non-secret binding persisted alongside a memory
// vector. Provider credentials are intentionally absent; they are resolved
// only for the duration of one embedding request.
type KnowledgeEmbedding struct {
	ProfileID string
	Revision  int64
	Model     string
	Vector    []float32
}

type KnowledgeEmbeddingSession struct {
	ProfileID       string
	Revision        int64
	Model           string
	Embed           func(context.Context, string) ([]float32, error)
	ValidateCurrent func(context.Context) error
}

type KnowledgeEmbeddingSessionFunc func(context.Context) (KnowledgeEmbeddingSession, error)

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

// SemanticKnowledgeStore is an optional extension. Existing KnowledgeStore
// implementations remain valid and continue to provide lexical search when
// no owner embedding profile is configured.
type SemanticKnowledgeStore interface {
	CreateKnowledgeMemorySemantic(context.Context, string, string, string, []string, string, [32]byte, KnowledgeEmbeddingSessionFunc) (KnowledgeMemory, bool, bool, KnowledgeEmbedding, error)
	SearchKnowledgeMemorySemantic(context.Context, string, string, int, string, KnowledgeEmbeddingSessionFunc) ([]KnowledgeMemory, string, KnowledgeEmbedding, error)
	KnowledgeStatusSemantic(context.Context, string, KnowledgeEmbeddingSessionFunc) (total, indexed, stale int, profile KnowledgeEmbedding, err error)
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
	item       KnowledgeMemory
	digest     [32]byte
	embedding  *KnowledgeEmbedding
	generation uint64
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

func cloneEmbedding(e *KnowledgeEmbedding) *KnowledgeEmbedding {
	if e == nil {
		return nil
	}
	out := *e
	out.Vector = append([]float32(nil), e.Vector...)
	return &out
}

func validEmbedding(e KnowledgeEmbedding) bool {
	if strings.TrimSpace(e.ProfileID) == "" || e.Revision <= 0 || strings.TrimSpace(e.Model) == "" || len(e.Vector) == 0 || len(e.Vector) > MaxEmbeddingDimension {
		return false
	}
	for _, v := range e.Vector {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return false
		}
	}
	return true
}

func (s *InMemoryStore) CreateKnowledgeMemorySemantic(ctx context.Context, owner, title, content string, tags []string, idem string, digest [32]byte, openSession KnowledgeEmbeddingSessionFunc) (KnowledgeMemory, bool, bool, KnowledgeEmbedding, error) {
	if err := ctx.Err(); err != nil {
		return KnowledgeMemory{}, false, false, KnowledgeEmbedding{}, err
	}
	if openSession == nil {
		return KnowledgeMemory{}, false, false, KnowledgeEmbedding{}, ErrNoEmbeddingProfile
	}
	owner = strings.TrimSpace(owner)
	s.mu.Lock()
	items := s.knowledge[owner]
	idx := -1
	for i, r := range items {
		if idem != "" && r.item.ID == idem {
			if r.digest != digest {
				s.mu.Unlock()
				return KnowledgeMemory{}, false, false, KnowledgeEmbedding{}, ErrIdempotencyConflict
			}
			idx = i
			break
		}
	}
	if idx < 0 {
		if owner == "" || strings.TrimSpace(content) == "" {
			s.mu.Unlock()
			return KnowledgeMemory{}, false, false, KnowledgeEmbedding{}, fmt.Errorf("owner and content are required")
		}
		id := idem
		if id == "" {
			id = fmt.Sprintf("memory-%d", len(items)+1)
		}
		item := KnowledgeMemory{ID: id, Title: strings.TrimSpace(title), Content: content, Tags: append([]string(nil), tags...), CreatedAt: time.Now().UTC()}
		s.knowledge[owner] = append(items, knowledgeRecord{item: item, digest: digest})
		idx = len(s.knowledge[owner]) - 1
	}
	item := s.knowledge[owner][idx].item
	old := cloneEmbedding(s.knowledge[owner][idx].embedding)
	claimGeneration := s.knowledge[owner][idx].generation
	replayed := idx < len(items)
	s.mu.Unlock()
	session, err := openSession(ctx)
	if err != nil {
		return item, replayed, false, KnowledgeEmbedding{}, err
	}
	current := KnowledgeEmbedding{ProfileID: session.ProfileID, Revision: session.Revision, Model: session.Model}
	if old != nil && old.ProfileID == current.ProfileID && old.Revision == current.Revision && old.Model == current.Model {
		current.Vector = append([]float32(nil), old.Vector...)
		return item, replayed, true, current, nil
	}
	s.mu.Lock()
	if idx < len(s.knowledge[owner]) && s.knowledge[owner][idx].generation == claimGeneration {
		s.knowledge[owner][idx].generation++
		claimGeneration = s.knowledge[owner][idx].generation
	}
	s.mu.Unlock()
	if session.Embed == nil {
		return item, replayed, false, KnowledgeEmbedding{}, errors.New("embedding session is not ready")
	}
	vector, err := session.Embed(ctx, item.Title+"\n"+item.Content)
	if err != nil {
		return item, replayed, false, KnowledgeEmbedding{}, err
	}
	current.Vector = append([]float32(nil), vector...)
	if !validEmbedding(current) {
		return item, replayed, false, KnowledgeEmbedding{}, errors.New("invalid embedding vector")
	}
	if session.ValidateCurrent != nil {
		if err := session.ValidateCurrent(ctx); err != nil {
			return item, replayed, false, KnowledgeEmbedding{}, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if idx < len(s.knowledge[owner]) && s.knowledge[owner][idx].generation == claimGeneration {
		s.knowledge[owner][idx].embedding = cloneEmbedding(&current)
	} else {
		return item, replayed, false, current, ErrEmbeddingSessionStale
	}
	return item, replayed, true, current, nil
}

func (s *InMemoryStore) SearchKnowledgeMemorySemantic(ctx context.Context, owner, query string, limit int, token string, openSession KnowledgeEmbeddingSessionFunc) ([]KnowledgeMemory, string, KnowledgeEmbedding, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", KnowledgeEmbedding{}, err
	}
	if strings.TrimSpace(token) != "" {
		return nil, "", KnowledgeEmbedding{}, ErrInvalidCursor
	}
	if openSession == nil {
		return nil, "", KnowledgeEmbedding{}, ErrNoEmbeddingProfile
	}
	session, err := openSession(ctx)
	if err != nil {
		return nil, "", KnowledgeEmbedding{}, err
	}
	if session.Embed == nil {
		return nil, "", KnowledgeEmbedding{}, errors.New("embedding session is not ready")
	}
	vector, err := session.Embed(ctx, query)
	if err != nil {
		return nil, "", KnowledgeEmbedding{}, err
	}
	q := KnowledgeEmbedding{ProfileID: session.ProfileID, Revision: session.Revision, Model: session.Model, Vector: vector}
	if !validEmbedding(q) {
		return nil, "", KnowledgeEmbedding{}, errors.New("invalid embedding vector")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	items := s.knowledge[strings.TrimSpace(owner)]
	type scored struct {
		item  KnowledgeMemory
		score float64
	}
	var scoredItems []scored
	for _, r := range items {
		if r.embedding == nil || r.embedding.ProfileID != q.ProfileID || r.embedding.Revision != q.Revision || r.embedding.Model != q.Model || len(r.embedding.Vector) != len(q.Vector) {
			continue
		}
		score := cosine(q.Vector, r.embedding.Vector)
		scoredItems = append(scoredItems, scored{r.item, score})
	}
	sort.Slice(scoredItems, func(i, j int) bool {
		if scoredItems[i].score != scoredItems[j].score {
			return scoredItems[i].score > scoredItems[j].score
		}
		return scoredItems[i].item.ID < scoredItems[j].item.ID
	})
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if len(scoredItems) > limit {
		scoredItems = scoredItems[:limit]
	}
	out := make([]KnowledgeMemory, len(scoredItems))
	for i := range scoredItems {
		out[i] = scoredItems[i].item
	}
	return out, "", q, nil
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

func (s *InMemoryStore) KnowledgeStatusSemantic(ctx context.Context, owner string, openSession KnowledgeEmbeddingSessionFunc) (int, int, int, KnowledgeEmbedding, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, 0, KnowledgeEmbedding{}, err
	}
	if openSession == nil {
		return 0, 0, 0, KnowledgeEmbedding{}, ErrNoEmbeddingProfile
	}
	session, err := openSession(ctx)
	if err != nil {
		return 0, 0, 0, KnowledgeEmbedding{}, err
	}
	current := KnowledgeEmbedding{ProfileID: session.ProfileID, Revision: session.Revision, Model: session.Model}
	s.mu.Lock()
	defer s.mu.Unlock()
	items := s.knowledge[strings.TrimSpace(owner)]
	indexed, stale := 0, 0
	var profile KnowledgeEmbedding
	for _, r := range items {
		if r.embedding != nil && r.embedding.ProfileID == current.ProfileID && r.embedding.Revision == current.Revision && r.embedding.Model == current.Model {
			indexed++
		} else {
			stale++
		}
	}
	profile = current
	return len(items), indexed, stale, profile, nil
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
