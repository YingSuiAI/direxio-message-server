package nativeagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// knowledgeEinoTools is the only Eino-facing durable memory surface. The
// owner and idempotency key are deliberately kept out of the model schema.
func (r *Runtime) knowledgeEinoTools() []Tool {
	return []Tool{
		{
			Name:        "native_agent_memory_remember",
			Description: "Persist an explicit user-requested fact in durable owner-scoped memory.",
			Parameters: objectSchema(map[string]any{
				"title":   stringSchema(),
				"content": stringSchema(),
				"tags":    map[string]any{"type": "array", "items": stringSchema()},
			}),
			Write: true,
			Handler: func(ctx context.Context, params map[string]any) (any, error) {
				_, _, userText := RequestContext(ctx)
				if !explicitMemoryRememberIntent(userText) {
					return nil, validationErrorf("durable memory writes require an explicit remember request")
				}
				title := trimString(params["title"])
				content := trimString(params["content"])
				tags := stringSlice(params["tags"])
				params = map[string]any{
					"title":           title,
					"content":         content,
					"tags":            toAnyStrings(tags),
					"idempotency_key": knowledgeEinoIdempotencyKey(ctx, title, content, tags),
				}
				return r.createKnowledgeMemory(ctx, params)
			},
		},
		{
			Name:        "native_agent_memory_search",
			Description: "Search durable owner-scoped memory before answering a user recall request.",
			Parameters: objectSchema(map[string]any{
				"query":      stringSchema(),
				"page_size":  map[string]any{"type": "integer"},
				"page_token": stringSchema(),
			}),
			Handler: func(ctx context.Context, params map[string]any) (any, error) {
				return r.searchKnowledgeMemory(ctx, params)
			},
		},
	}
}

func toAnyStrings(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}

func knowledgeEinoIdempotencyKey(ctx context.Context, title, content string, tags []string) string {
	owner, conversation, userText := RequestContext(ctx)
	canonical := strings.Join([]string{
		strings.TrimSpace(owner), strings.TrimSpace(conversation), strings.TrimSpace(userText),
		strings.TrimSpace(title), strings.TrimSpace(content), strings.Join(tags, "\x00"),
	}, "\x00")
	digest := sha256.Sum256([]byte(canonical))
	return "native-agent-memory-" + hex.EncodeToString(digest[:])
}

func explicitMemoryRememberIntent(text string) bool {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return false
	}
	for _, phrase := range []string{
		"不要记住", "别记住", "不必记住", "不用记住", "无需记住", "不要保存", "别保存", "不要存入", "别存入",
		"do not remember", "don't remember", "dont remember", "never remember", "i remember", "we remember",
	} {
		if strings.Contains(text, phrase) {
			return false
		}
	}
	for _, phrase := range []string{
		"还记得", "你记得", "回忆", "recall", "do you remember", "what do you remember", "remember what", "remember when",
		"remember where", "remember why", "remember who", "remember if", "can you remember",
	} {
		if strings.Contains(text, phrase) {
			return false
		}
	}
	for _, phrase := range []string{
		"记住", "请记住", "帮我记", "记下来", "保存到记忆", "请保存到记忆", "存到记忆", "请存到记忆", "存入记忆", "请存入记忆", "别忘了", "please remember", "remember ",
		"save this", "store this", "don't forget", "dont forget",
	} {
		if strings.HasPrefix(text, phrase) {
			return true
		}
	}
	return false
}
