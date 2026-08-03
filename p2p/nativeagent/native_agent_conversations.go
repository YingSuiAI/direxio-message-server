package nativeagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/agentmemory"
	"math"
	"strconv"
	"strings"
)

// validationError marks client-controlled parameter validation failures. It is
// deliberately distinct from store and runtime failures so ProductCore can
// preserve their 5xx status at the handler boundary.
type validationError struct{ message string }

func (e *validationError) Error() string { return e.message }

func validationErrorf(format string, args ...any) error {
	return &validationError{message: fmt.Sprintf(format, args...)}
}

// IsValidationError reports whether err is a strict Native Agent parameter
// validation error suitable for a ProductCore 400 response.
func IsValidationError(err error) bool {
	var validation *validationError
	return errors.As(err, &validation)
}

func strictParams(p map[string]any, allowed map[string]string) error {
	for k, v := range p {
		kind, ok := allowed[k]
		if !ok {
			return validationErrorf("unknown parameter: %s", k)
		}
		switch kind {
		case "string":
			if _, ok := v.(string); !ok {
				return validationErrorf("%s must be string", k)
			}
		case "array":
			if _, ok := v.([]any); !ok {
				return validationErrorf("%s must be array", k)
			}
		case "integer":
			if !strictInteger(v) {
				return validationErrorf("%s must be integer", k)
			}
		}
	}
	return nil
}

func strictInteger(value any) bool {
	switch n := value.(type) {
	case json.Number:
		_, err := n.Int64()
		return err == nil
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32:
		return true
	case uint64:
		return n <= math.MaxInt64
	case float32:
		return !math.IsNaN(float64(n)) && !math.IsInf(float64(n), 0) && n == float32(int64(n))
	case float64:
		return !math.IsNaN(n) && !math.IsInf(n, 0) && n >= math.MinInt64 && n <= math.MaxInt64 && n == float64(int64(n))
	default:
		return false
	}
}

func requiredString(p map[string]any, key string) error {
	if strings.TrimSpace(trimString(p[key])) == "" {
		return validationErrorf("%s is required", key)
	}
	return nil
}

func (r *Runtime) conversationStore(ctx context.Context) (ConversationStore, error) {
	if r.conversations == nil {
		return nil, fmt.Errorf("conversation store is not configured")
	}
	o := r.effectiveOwner(ctx)
	if o == "" {
		return nil, fmt.Errorf("owner context is required")
	}
	return r.conversations, nil
}
func convMap(c agentmemory.Conversation) map[string]any {
	status := "active"
	if c.Deleted {
		status = "deleted"
	}
	return map[string]any{"conversation_id": c.ID, "title": c.Title, "status": status, "revision": c.Revision, "created_at": c.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"), "updated_at": c.UpdatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")}
}
func (r *Runtime) createConversation(ctx context.Context, p map[string]any) (map[string]any, error) {
	if err := strictParams(p, map[string]string{"conversation_id": "string", "title": "string", "idempotency_key": "string"}); err != nil {
		return nil, err
	}
	for _, k := range []string{"conversation_id", "idempotency_key"} {
		if err := requiredString(p, k); err != nil {
			return nil, err
		}
	}
	s, e := r.conversationStore(ctx)
	if e != nil {
		return nil, e
	}
	o := r.effectiveOwner(ctx)
	id := sanitizeNativeID(trimString(p["conversation_id"]))
	if id == "" {
		return nil, validationErrorf("conversation_id is required")
	}
	title := trimString(p["title"])
	idem := trimString(p["idempotency_key"])
	c, re, e := s.CreateConversation(ctx, o, id, title, idem, agentmemory.KnowledgeDigest(id, title, nil))
	if e != nil {
		return nil, e
	}
	return map[string]any{"conversation": convMap(c), "replayed": re}, nil
}
func (r *Runtime) listConversations(ctx context.Context, p map[string]any) (map[string]any, error) {
	if err := strictParams(p, map[string]string{"page_size": "integer", "page_token": "string"}); err != nil {
		return nil, err
	}
	s, e := r.conversationStore(ctx)
	if e != nil {
		return nil, e
	}
	n := int(int64Param(p["page_size"]))
	if n < 0 || n > 100 {
		return nil, validationErrorf("page_size is out of range")
	}
	token := trimString(p["page_token"])
	if len(token) > 512 {
		return nil, validationErrorf("page_token is invalid")
	}
	items, next, e := s.ListAgentConversations(ctx, r.effectiveOwner(ctx), n, token)
	if e != nil {
		return nil, e
	}
	out := make([]map[string]any, 0, len(items))
	for _, c := range items {
		out = append(out, convMap(c))
	}
	return map[string]any{"conversations": out, "next_cursor": next}, nil
}
func (r *Runtime) getConversation(ctx context.Context, p map[string]any) (map[string]any, error) {
	if err := strictParams(p, map[string]string{"conversation_id": "string", "message_limit": "integer", "message_cursor": "string"}); err != nil {
		return nil, err
	}
	if err := requiredString(p, "conversation_id"); err != nil {
		return nil, err
	}
	s, e := r.conversationStore(ctx)
	if e != nil {
		return nil, e
	}
	id := trimString(p["conversation_id"])
	token := trimString(p["message_cursor"])
	if token != "" {
		if _, err := strconv.Atoi(token); err != nil {
			return nil, validationErrorf("message_cursor is invalid")
		}
	}
	limit := int(int64Param(p["message_limit"]))
	if limit < 0 || limit > 100 {
		return nil, validationErrorf("message_limit is out of range")
	}
	c, msgs, next, e := s.GetConversation(ctx, r.effectiveOwner(ctx), id, limit, token)
	if e != nil {
		return nil, e
	}
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, map[string]any{"message_id": m.MessageID, "message_seq": m.Seq, "role": m.Role, "content": m.Content, "references": m.References, "created_at": m.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")})
	}
	return map[string]any{"conversation": convMap(c), "messages": out, "next_cursor": next}, nil
}
func (r *Runtime) renameConversation(ctx context.Context, p map[string]any) (map[string]any, error) {
	if err := strictParams(p, map[string]string{"conversation_id": "string", "title": "string", "expected_revision": "integer", "idempotency_key": "string"}); err != nil {
		return nil, err
	}
	for _, k := range []string{"conversation_id", "idempotency_key"} {
		if err := requiredString(p, k); err != nil {
			return nil, err
		}
	}
	if int64Param(p["expected_revision"]) <= 0 {
		return nil, validationErrorf("expected_revision must be positive")
	}
	s, e := r.conversationStore(ctx)
	if e != nil {
		return nil, e
	}
	id := trimString(p["conversation_id"])
	expected := int64Param(p["expected_revision"])
	c, re, e := s.RenameConversation(ctx, r.effectiveOwner(ctx), id, trimString(p["title"]), expected, trimString(p["idempotency_key"]), agentmemory.KnowledgeDigest(id, trimString(p["title"]), []string{fmt.Sprintf("expected_revision:%d", expected)}))
	if e != nil {
		return nil, e
	}
	return map[string]any{"conversation": convMap(c), "replayed": re}, nil
}
func (r *Runtime) deleteConversation(ctx context.Context, p map[string]any) (map[string]any, error) {
	if err := strictParams(p, map[string]string{"conversation_id": "string", "expected_revision": "integer", "idempotency_key": "string"}); err != nil {
		return nil, err
	}
	for _, k := range []string{"conversation_id", "idempotency_key"} {
		if err := requiredString(p, k); err != nil {
			return nil, err
		}
	}
	if int64Param(p["expected_revision"]) <= 0 {
		return nil, validationErrorf("expected_revision must be positive")
	}
	s, e := r.conversationStore(ctx)
	if e != nil {
		return nil, e
	}
	id := trimString(p["conversation_id"])
	expected := int64Param(p["expected_revision"])
	c, re, e := s.DeleteConversation(ctx, r.effectiveOwner(ctx), id, expected, trimString(p["idempotency_key"]), agentmemory.KnowledgeDigest(id, "delete", []string{fmt.Sprintf("expected_revision:%d", expected)}))
	if e != nil {
		return nil, e
	}
	return map[string]any{"conversation": convMap(c), "replayed": re}, nil
}
