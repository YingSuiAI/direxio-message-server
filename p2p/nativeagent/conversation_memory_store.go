package nativeagent

import (
	"context"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/agentmemory"
	"strings"
)

type ConversationMemory = agentmemory.ConversationMemory
type StoredMessage = agentmemory.StoredMessage
type ConversationMemoryStore = agentmemory.Store
type KnowledgeStore = agentmemory.KnowledgeStore
type ConversationStore = agentmemory.ConversationStore
type InMemoryConversationMemoryStore = agentmemory.InMemoryStore

func NewInMemoryConversationMemoryStore() *InMemoryConversationMemoryStore {
	return agentmemory.NewInMemoryStore()
}
func MemoryStoreOwner(ctx context.Context) string {
	owner, _, _ := RequestContext(ctx)
	return strings.TrimSpace(owner)
}
