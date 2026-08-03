package nativeagent

import (
	"context"
	"fmt"
	"strings"
)

func (r *Runtime) effectiveOwner(ctx context.Context) string {
	if o := MemoryStoreOwner(ctx); o != "" {
		return o
	}
	if _, ok := r.memory.(*InMemoryConversationMemoryStore); ok && r.ownerID == nil {
		return "test-owner"
	}
	return ""
}

func (r *Runtime) loadMemory(ctx context.Context, conversationID string) (nativeAgentMemory, error) {
	owner := r.effectiveOwner(ctx)
	if owner == "" {
		return nativeAgentMemory{}, fmt.Errorf("owner context is required")
	}
	if r.memory == nil {
		return nativeAgentMemory{ConversationID: conversationID}, nil
	}
	m, err := r.memory.LoadConversationMemory(ctx, owner, conversationID)
	if err != nil {
		return nativeAgentMemory{}, err
	}
	return nativeAgentMemory{ConversationID: m.ConversationID, Title: m.Title, Summary: m.Summary, Messages: compactEinoMessagesForMemory(m.Messages), LastMessageSeq: m.LastMessageSeq, UpdatedAt: 0}, nil
}

func (r *Runtime) saveMemory(ctx context.Context, memory nativeAgentMemory) error {
	owner := r.effectiveOwner(ctx)
	if owner == "" {
		return fmt.Errorf("owner context is required")
	}
	if r.memory == nil {
		return fmt.Errorf("conversation memory store is not configured")
	}
	current, err := r.memory.LoadConversationMemory(ctx, owner, memory.ConversationID)
	if err != nil {
		return err
	}
	if current.LastMessageSeq == 0 && len(memory.Messages) > 0 {
		stored := make([]StoredMessage, 0, len(memory.Messages))
		for _, msg := range memory.Messages {
			if msg == nil {
				continue
			}
			stored = append(stored, StoredMessage{Role: string(msg.Role), Content: msg.Content, Message: msg})
		}
		if err := r.memory.AppendConversationMessages(ctx, owner, memory.ConversationID, "", stored); err != nil {
			return err
		}
		current, err = r.memory.LoadConversationMemory(ctx, owner, memory.ConversationID)
		if err != nil {
			return err
		}
	}
	through := current.LastMessageSeq - int64(len(memory.Messages))
	if through < 0 {
		through = 0
	}
	return r.memory.SaveConversationSummary(ctx, owner, memory.ConversationID, strings.TrimSpace(memory.Summary), through)
}
