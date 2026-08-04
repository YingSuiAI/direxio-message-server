package productcapability

import (
	"context"
	"fmt"
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
	if provider == nil || provider.Descriptor == nil || provider.Handler == nil {
		return fmt.Errorf("provider, descriptor and handler are required")
	}
	capabilityID := provider.Descriptor.CapabilityId
	if capabilityID == "" {
		return fmt.Errorf("capability_id is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.capabilities[capabilityID]; exists {
		return fmt.Errorf("capability %q is already registered", capabilityID)
	}
	r.capabilities[capabilityID] = provider
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

func (r *Registry) Operation(capabilityID, operationID string) (*capv1.OperationDescriptor, bool) {
	provider, ok := r.Get(capabilityID)
	if !ok || provider == nil || provider.Descriptor == nil {
		return nil, false
	}
	for _, operation := range provider.Descriptor.Operations {
		if operation != nil && operation.OperationId == operationID {
			return operation, true
		}
	}
	return nil, false
}
