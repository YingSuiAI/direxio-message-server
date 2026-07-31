package agentembedded

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
)

func requiredString(p map[string]any, key string) (string, *actionbase.Error) {
	v, e := optionalString(p, key)
	if e != nil {
		return "", e
	}
	if strings.TrimSpace(v) == "" {
		return "", actionbase.BadRequest(key + " is required")
	}
	return strings.TrimSpace(v), nil
}
func optionalString(p map[string]any, key string) (string, *actionbase.Error) {
	v, ok := p[key]
	if !ok || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", actionbase.BadRequest(key + " must be a string")
	}
	return s, nil
}
func optionalInt64(p map[string]any, key string) (int64, *actionbase.Error) {
	v, ok := p[key]
	if !ok || v == nil {
		return 0, nil
	}
	switch x := v.(type) {
	case int:
		return int64(x), nil
	case int64:
		return x, nil
	case float64:
		if x != float64(int64(x)) {
			return 0, actionbase.BadRequest(key + " must be an integer")
		}
		return int64(x), nil
	case json.Number:
		n, e := x.Int64()
		if e == nil {
			return n, nil
		}
	}
	return 0, actionbase.BadRequest(key + " must be an integer")
}
func requiredPositiveInt64(p map[string]any, key string) (int64, *actionbase.Error) {
	v, err := optionalInt64(p, key)
	if err != nil || v < 1 {
		if err != nil {
			return 0, err
		}
		return 0, actionbase.BadRequest(key + " must be positive")
	}
	return v, nil
}
func page(p map[string]any) (int, string, *actionbase.Error) {
	size, e := optionalInt64(p, "page_size")
	if e != nil {
		return 0, "", e
	}
	if size <= 0 {
		size = 25
	}
	if size > 100 {
		return 0, "", actionbase.BadRequest("page_size is too large")
	}
	token, e := optionalString(p, "page_token")
	if e != nil {
		return 0, "", e
	}
	return int(size), token, nil
}
func ptrInt64(v int64, p map[string]any, key string) *int64 {
	if _, ok := p[key]; !ok {
		return nil
	}
	return &v
}
func stringValue(v any) string        { s, _ := v.(string); return strings.TrimSpace(s) }
func errorsIs(err, target error) bool { return errors.Is(err, target) }
func statusUnavailable() *actionbase.Error {
	return actionbase.CodedError(http.StatusPreconditionFailed, "agent_embedded_unavailable", ErrUnavailable.Error())
}
