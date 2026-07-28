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
	"unicode/utf8"
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
	ID, Title, Content   string
	SourceID             string
	ChunkOrdinal         int64
	Tags                 []string
	Revision             int64
	CreatedAt, UpdatedAt time.Time
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

// ManagedKnowledgeStore owns the user-editable Eino remember/recall records.
// It is intentionally an optional extension so existing lexical test doubles
// remain source compatible.
type ManagedKnowledgeStore interface {
	ListKnowledgeMemories(context.Context, string, int, string) ([]KnowledgeMemory, string, error)
	UpdateKnowledgeMemory(context.Context, string, string, string, string, []string, int64, string, [32]byte, KnowledgeEmbeddingSessionFunc) (KnowledgeMemory, bool, error)
	DeleteKnowledgeMemory(context.Context, string, string, int64, string, [32]byte) (KnowledgeMemory, bool, error)
}

type KnowledgeSource struct {
	ID, Kind, Status, Title, MimeType, Error   string
	Size, TotalChunks, IndexedChunks, Revision int64
	CreatedAt, UpdatedAt                       time.Time
}
type KnowledgeUpload struct {
	ID, SourceID, Owner, Filename, MimeType, Status string
	Size, Received                                  int64
	Data                                            []byte
	Idempotency                                     map[string][32]byte
	CreatedAt                                       time.Time
}
type KnowledgeSourceStore interface {
	StartKnowledgeUpload(context.Context, string, string, string, int64, string) (KnowledgeUpload, bool, error)
	AppendKnowledgeUpload(context.Context, string, string, int64, []byte, string, [32]byte) (KnowledgeUpload, error)
	FinishKnowledgeUpload(context.Context, string, string, string, [32]byte, string, KnowledgeEmbeddingSessionFunc) (KnowledgeSource, error)
	ListKnowledgeSources(context.Context, string, int, string) ([]KnowledgeSource, string, error)
	DeleteKnowledgeSource(context.Context, string, string, int64, string, [32]byte) (KnowledgeSource, bool, error)
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
	mu                sync.Mutex
	data              map[string]*record
	knowledge         map[string][]knowledgeRecord
	conversations     map[string]Conversation
	receipts          map[string][32]byte
	knowledgeReceipts map[string]knowledgeReceipt
	sourceReceipts    map[string]sourceReceipt
	sources           map[string][]KnowledgeSource
	uploads           map[string]KnowledgeUpload
}
type knowledgeReceipt struct {
	digest [32]byte
	item   KnowledgeMemory
}
type sourceReceipt struct {
	digest [32]byte
	upload KnowledgeUpload
	source KnowledgeSource
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
	return &InMemoryStore{data: map[string]*record{}, knowledge: map[string][]knowledgeRecord{}, conversations: map[string]Conversation{}, receipts: map[string][32]byte{}, knowledgeReceipts: map[string]knowledgeReceipt{}, sourceReceipts: map[string]sourceReceipt{}, sources: map[string][]KnowledgeSource{}, uploads: map[string]KnowledgeUpload{}}
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
	now := time.Now().UTC()
	item := KnowledgeMemory{ID: id, Title: strings.TrimSpace(title), Content: content, Tags: append([]string(nil), tags...), Revision: 1, CreatedAt: now, UpdatedAt: now}
	s.knowledge[owner] = append(items, knowledgeRecord{item: item, digest: digest})
	return item, false, nil
}

func (s *InMemoryStore) ListKnowledgeMemories(ctx context.Context, owner string, limit int, token string) ([]KnowledgeMemory, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	items := s.knowledge[strings.TrimSpace(owner)]
	start := 0
	if token != "" {
		if _, err := fmt.Sscanf(token, "%d", &start); err != nil || start < 0 {
			return nil, "", ErrInvalidCursor
		}
	}
	out := make([]KnowledgeMemory, 0, limit)
	nextIndex := start
	for i := start; i < len(items) && len(out) < limit; i++ {
		nextIndex = i + 1
		if items[i].item.SourceID != "" {
			continue
		}
		out = append(out, items[i].item)
	}
	next := ""
	if nextIndex < len(items) {
		next = fmt.Sprintf("%d", nextIndex)
	}
	return out, next, nil
}

func (s *InMemoryStore) UpdateKnowledgeMemory(ctx context.Context, owner, id, title, content string, tags []string, expected int64, idem string, digest [32]byte, openSession KnowledgeEmbeddingSessionFunc) (KnowledgeMemory, bool, error) {
	if err := ctx.Err(); err != nil {
		return KnowledgeMemory{}, false, err
	}
	owner = strings.TrimSpace(owner)
	id = strings.TrimSpace(id)
	s.mu.Lock()
	if old, ok := s.knowledgeReceipts[key(owner, "knowledge.update\x00"+idem)]; ok {
		if old.digest != digest {
			s.mu.Unlock()
			return KnowledgeMemory{}, false, ErrIdempotencyConflict
		}
		s.mu.Unlock()
		return old.item, true, nil
	}
	items := s.knowledge[owner]
	idx := -1
	for i := range items {
		if items[i].item.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.mu.Unlock()
		return KnowledgeMemory{}, false, ErrNotFound
	}
	r := items[idx]
	if r.item.SourceID != "" {
		s.mu.Unlock()
		return KnowledgeMemory{}, false, ErrNotFound
	}
	if expected <= 0 || r.item.Revision != expected {
		s.mu.Unlock()
		return r.item, false, ErrRevisionConflict
	}
	if idem != "" && r.digest == digest {
		s.mu.Unlock()
		return r.item, true, nil
	}
	s.mu.Unlock()
	var emb *KnowledgeEmbedding
	if openSession != nil {
		session, err := openSession(ctx)
		if err != nil {
			return KnowledgeMemory{}, false, err
		}
		if session.Embed != nil {
			v, err := session.Embed(ctx, strings.TrimSpace(title)+"\n"+content)
			if err != nil {
				return KnowledgeMemory{}, false, err
			}
			current := KnowledgeEmbedding{ProfileID: session.ProfileID, Revision: session.Revision, Model: session.Model, Vector: v}
			if !validEmbedding(current) {
				return KnowledgeMemory{}, false, errors.New("invalid embedding vector")
			}
			if session.ValidateCurrent != nil {
				if err := session.ValidateCurrent(ctx); err != nil {
					return KnowledgeMemory{}, false, err
				}
			}
			emb = &current
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	items = s.knowledge[owner]
	if idx >= len(items) || items[idx].item.ID != id {
		return KnowledgeMemory{}, false, ErrNotFound
	}
	if items[idx].item.Revision != expected {
		return items[idx].item, false, ErrRevisionConflict
	}
	now := time.Now().UTC()
	item := items[idx].item
	item.Title, item.Content, item.Tags = strings.TrimSpace(title), content, append([]string(nil), tags...)
	item.Revision++
	item.UpdatedAt = now
	items[idx].item, items[idx].digest, items[idx].embedding = item, digest, emb
	s.knowledge[owner] = items
	s.knowledgeReceipts[key(owner, "knowledge.update\x00"+idem)] = knowledgeReceipt{digest: digest, item: item}
	return item, false, nil
}

func (s *InMemoryStore) DeleteKnowledgeMemory(ctx context.Context, owner, id string, expected int64, idem string, digest [32]byte) (KnowledgeMemory, bool, error) {
	if err := ctx.Err(); err != nil {
		return KnowledgeMemory{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	owner, id = strings.TrimSpace(owner), strings.TrimSpace(id)
	items := s.knowledge[owner]
	if old, ok := s.knowledgeReceipts[key(owner, "knowledge.delete\x00"+idem)]; ok {
		if old.digest != digest {
			return KnowledgeMemory{}, false, ErrIdempotencyConflict
		}
		return old.item, true, nil
	}
	for i := range items {
		if items[i].item.ID != id {
			continue
		}
		if items[i].item.SourceID != "" {
			return KnowledgeMemory{}, false, ErrNotFound
		}
		if items[i].item.Revision != expected {
			return items[i].item, false, ErrRevisionConflict
		}
		item := items[i].item
		copy(items[i:], items[i+1:])
		s.knowledge[owner] = items[:len(items)-1]
		s.knowledgeReceipts[key(owner, "knowledge.delete\x00"+idem)] = knowledgeReceipt{digest: digest, item: item}
		return item, false, nil
	}
	return KnowledgeMemory{}, false, ErrNotFound
}

func (s *InMemoryStore) StartKnowledgeUpload(ctx context.Context, owner, filename, mime string, size int64, idem string) (KnowledgeUpload, bool, error) {
	if err := ctx.Err(); err != nil {
		return KnowledgeUpload{}, false, err
	}
	if strings.TrimSpace(owner) == "" || strings.TrimSpace(filename) == "" || size < 0 || size > 10*1024*1024 || idem == "" {
		return KnowledgeUpload{}, false, fmt.Errorf("owner, filename, size and idempotency_key are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	digest := sourceRequestDigest("upload.start", owner, filename, mime, fmt.Sprintf("%d", size))
	keyID := key(owner, "upload.start\x00"+idem)
	if old, ok := s.sourceReceipts[keyID]; ok {
		if old.digest != digest {
			return KnowledgeUpload{}, false, ErrIdempotencyConflict
		}
		return old.upload, true, nil
	}
	id := fmt.Sprintf("upload-%d", len(s.uploads)+1)
	src := fmt.Sprintf("source-%d", len(s.sources[owner])+1)
	now := time.Now().UTC()
	u := KnowledgeUpload{ID: id, SourceID: src, Owner: owner, Filename: filename, MimeType: mime, Size: size, Status: "receiving", Idempotency: map[string][32]byte{}, CreatedAt: now}
	s.uploads[id] = u
	s.sourceReceipts[keyID] = sourceReceipt{digest: digest, upload: u}
	return u, false, nil
}
func (s *InMemoryStore) AppendKnowledgeUpload(ctx context.Context, owner, uploadID string, offset int64, data []byte, idem string, digest [32]byte) (KnowledgeUpload, error) {
	if err := ctx.Err(); err != nil {
		return KnowledgeUpload{}, err
	}
	if idem == "" || len(data) > 256*1024 {
		return KnowledgeUpload{}, fmt.Errorf("chunk exceeds 256 KiB")
	}
	if sha256.Sum256(data) != digest {
		return KnowledgeUpload{}, fmt.Errorf("chunk_sha256 mismatch")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	request := sourceRequestDigest("upload.append", owner, uploadID, fmt.Sprintf("%d", offset), fmt.Sprintf("%x", digest))
	if old, ok := s.sourceReceipts[key(owner, "upload.append\x00"+idem)]; ok {
		if old.digest != request {
			return KnowledgeUpload{}, ErrIdempotencyConflict
		}
		return old.upload, nil
	}
	u, ok := s.uploads[uploadID]
	if !ok || u.ID != uploadID || u.Owner != strings.TrimSpace(owner) {
		return KnowledgeUpload{}, ErrNotFound
	}
	if old, ok := u.Idempotency[idem]; ok {
		if old != digest {
			return KnowledgeUpload{}, ErrIdempotencyConflict
		}
		return u, nil
	}
	if offset != u.Received {
		return KnowledgeUpload{}, fmt.Errorf("chunk offset mismatch")
	}
	if u.Received+int64(len(data)) > u.Size {
		return KnowledgeUpload{}, fmt.Errorf("upload exceeds declared size")
	}
	u.Data = append(u.Data, data...)
	u.Received += int64(len(data))
	u.Idempotency[idem] = digest
	s.uploads[uploadID] = u
	s.sourceReceipts[key(owner, "upload.append\x00"+idem)] = sourceReceipt{digest: request, upload: u}
	return u, nil
}
func (s *InMemoryStore) FinishKnowledgeUpload(ctx context.Context, owner, uploadID, title string, digest [32]byte, idem string, openSession KnowledgeEmbeddingSessionFunc) (KnowledgeSource, error) {
	if err := ctx.Err(); err != nil {
		return KnowledgeSource{}, err
	}
	if idem == "" {
		return KnowledgeSource{}, fmt.Errorf("idempotency_key is required")
	}
	s.mu.Lock()
	request := sourceRequestDigest("upload.finish", owner, uploadID, title, fmt.Sprintf("%x", digest))
	if old, ok := s.sourceReceipts[key(owner, "upload.finish\x00"+idem)]; ok {
		if old.digest != request {
			s.mu.Unlock()
			return KnowledgeSource{}, ErrIdempotencyConflict
		}
		s.mu.Unlock()
		return old.source, nil
	}
	u, ok := s.uploads[uploadID]
	if ok && u.Owner != strings.TrimSpace(owner) {
		ok = false
	}
	if !ok || u.ID != uploadID {
		s.mu.Unlock()
		return KnowledgeSource{}, ErrNotFound
	}
	if u.Received != u.Size {
		s.mu.Unlock()
		return KnowledgeSource{}, fmt.Errorf("upload is incomplete")
	}
	if sha256.Sum256(u.Data) != digest {
		s.mu.Unlock()
		return KnowledgeSource{}, fmt.Errorf("content_sha256 mismatch")
	}
	if !utf8.Valid(u.Data) {
		s.mu.Unlock()
		return KnowledgeSource{}, fmt.Errorf("upload must be valid UTF-8")
	}
	u.Data = append([]byte(nil), u.Data...)
	s.mu.Unlock()
	if openSession == nil {
		return KnowledgeSource{}, fmt.Errorf("default embedding profile is required")
	}
	session, err := openSession(ctx)
	if err != nil || session.Embed == nil {
		if err != nil {
			return KnowledgeSource{}, err
		}
		return KnowledgeSource{}, fmt.Errorf("embedding session is not ready")
	}
	if session.ValidateCurrent != nil {
		if err := session.ValidateCurrent(ctx); err != nil {
			return KnowledgeSource{}, err
		}
	}
	parts := boundedKnowledgeChunks(string(u.Data), 8192)
	type indexedChunk struct {
		ordinal int64
		text    string
		vector  []float32
	}
	prepared := make([]indexedChunk, 0, len(parts))
	for ordinal, part := range parts {
		if strings.TrimSpace(part) == "" {
			continue
		}
		vector, e := session.Embed(ctx, part)
		if e != nil {
			return KnowledgeSource{}, e
		}
		if !validEmbedding(KnowledgeEmbedding{ProfileID: session.ProfileID, Revision: session.Revision, Model: session.Model, Vector: vector}) {
			return KnowledgeSource{}, fmt.Errorf("invalid embedding vector")
		}
		prepared = append(prepared, indexedChunk{ordinal: int64(ordinal), text: part, vector: append([]float32(nil), vector...)})
	}
	if len(prepared) == 0 {
		return KnowledgeSource{}, fmt.Errorf("upload contains no text")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.uploads[uploadID]
	if !ok || current.Owner != u.Owner || current.SourceID != u.SourceID || current.Received != u.Received || current.Size != u.Size || sha256.Sum256(current.Data) != digest {
		return KnowledgeSource{}, fmt.Errorf("upload changed during indexing")
	}
	indexed := int64(len(prepared))
	now := time.Now().UTC()
	for _, chunk := range prepared {
		item := KnowledgeMemory{ID: fmt.Sprintf("%s-%d", u.SourceID, chunk.ordinal), Title: strings.TrimSpace(title), Content: chunk.text, SourceID: u.SourceID, ChunkOrdinal: chunk.ordinal, Revision: 1, CreatedAt: now, UpdatedAt: now}
		s.knowledge[u.Owner] = append(s.knowledge[u.Owner], knowledgeRecord{item: item, digest: KnowledgeDigest(item.Title, item.Content, nil), embedding: &KnowledgeEmbedding{ProfileID: session.ProfileID, Revision: session.Revision, Model: session.Model, Vector: chunk.vector}})
	}
	chunks := indexed
	src := KnowledgeSource{ID: u.SourceID, Kind: "upload", Status: "ready", Title: strings.TrimSpace(title), MimeType: u.MimeType, Size: u.Size, TotalChunks: chunks, IndexedChunks: indexed, Revision: 1, CreatedAt: now, UpdatedAt: now}
	s.sources[u.Owner] = append(s.sources[u.Owner], src)
	delete(s.uploads, uploadID)
	s.sourceReceipts[key(owner, "upload.finish\x00"+idem)] = sourceReceipt{digest: request, source: src}
	return src, nil
}
func boundedKnowledgeChunks(text string, maxRunes int) []string {
	if maxRunes <= 0 {
		maxRunes = 8192
	}
	out := []string{}
	for _, para := range strings.Split(text, "\n\n") {
		r := []rune(para)
		for len(r) > maxRunes {
			out = append(out, string(r[:maxRunes]))
			r = r[maxRunes:]
		}
		if len(strings.TrimSpace(string(r))) > 0 {
			out = append(out, string(r))
		}
	}
	return out
}
func (s *InMemoryStore) ListKnowledgeSources(ctx context.Context, owner string, limit int, token string) ([]KnowledgeSource, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	items := s.sources[owner]
	start := 0
	if token != "" {
		if _, err := fmt.Sscanf(token, "%d", &start); err != nil || start < 0 || start > len(items) {
			return nil, "", ErrInvalidCursor
		}
	}
	end := start + limit
	if end > len(items) {
		end = len(items)
	}
	next := ""
	if end < len(items) {
		next = fmt.Sprintf("%d", end)
	}
	return append([]KnowledgeSource(nil), items[start:end]...), next, nil
}
func (s *InMemoryStore) DeleteKnowledgeSource(ctx context.Context, owner, id string, expected int64, idem string, digest [32]byte) (KnowledgeSource, bool, error) {
	if err := ctx.Err(); err != nil {
		return KnowledgeSource{}, false, err
	}
	if idem == "" || expected <= 0 {
		return KnowledgeSource{}, false, fmt.Errorf("expected_revision and idempotency_key are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.sourceReceipts[key(owner, "source.delete\x00"+idem)]; ok {
		if old.digest != digest {
			return KnowledgeSource{}, false, ErrIdempotencyConflict
		}
		return old.source, true, nil
	}
	items := s.sources[owner]
	for i, src := range items {
		if src.ID != id {
			continue
		}
		if src.Revision != expected {
			return src, false, ErrRevisionConflict
		}
		copy(items[i:], items[i+1:])
		s.sources[owner] = items[:len(items)-1]
		mems := s.knowledge[owner]
		kept := mems[:0]
		for _, mem := range mems {
			if mem.item.SourceID != id {
				kept = append(kept, mem)
			}
		}
		s.knowledge[owner] = kept
		s.sourceReceipts[key(owner, "source.delete\x00"+idem)] = sourceReceipt{digest: digest, source: src}
		return src, false, nil
	}
	return KnowledgeSource{}, false, ErrNotFound
}
func sourceRequestDigest(parts ...string) [32]byte {
	return sha256.Sum256([]byte(strings.Join(parts, "\x00")))
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
		now := time.Now().UTC()
		item := KnowledgeMemory{ID: id, Title: strings.TrimSpace(title), Content: content, Tags: append([]string(nil), tags...), Revision: 1, CreatedAt: now, UpdatedAt: now}
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
