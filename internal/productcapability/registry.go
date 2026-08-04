package productcapability

import (
	"context"
	"sync"

	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
)

// ProviderFunc 是 capability 提供者函数类型
type ProviderFunc func(ctx context.Context, input []byte) ([]byte, error)

// Provider 定义了一个 capability 提供者
type Provider struct {
	Descriptor *capv1.CapabilityDescriptor
	Handler    ProviderFunc
}

// Registry 管理所有注册的 Product capabilities
type Registry struct {
	mu           sync.RWMutex
	capabilities map[string]*Provider // key: capability_id
}

// NewRegistry 创建新的 registry
func NewRegistry() *Registry {
	return &Registry{
		capabilities: make(map[string]*Provider),
	}
}

// Register 注册一个 capability
func (r *Registry) Register(provider *Provider) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.capabilities[provider.Descriptor.CapabilityId] = provider
	return nil
}

// Get 获取一个 capability
func (r *Registry) Get(capabilityID string) (*Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	provider, ok := r.capabilities[capabilityID]
	return provider, ok
}

// List 列出所有 capabilities
func (r *Registry) List() []*capv1.CapabilityDescriptor {
	r.mu.RLock()
	defer r.mu.RUnlock()

	descriptors := make([]*capv1.CapabilityDescriptor, 0, len(r.capabilities))
	for _, provider := range r.capabilities {
		descriptors = append(descriptors, provider.Descriptor)
	}
	return descriptors
}
