package coreworkload

import (
	"context"
	"sync"
)

// ProviderRegistry is the production target multiplexer. Routes are exact
// TargetKind matches; an unknown kind never falls back to another provider.
type ProviderRegistry struct {
	mu        sync.RWMutex
	providers map[TargetKind]Provider
}

func NewProviderRegistry(routes map[TargetKind]Provider) (*ProviderRegistry, error) {
	r := &ProviderRegistry{providers: make(map[TargetKind]Provider)}
	for kind, provider := range routes {
		if err := r.Register(kind, provider); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// NewMultiplexer is retained as a descriptive alias for callers that do not
// own the registry lifecycle.
func NewMultiplexer(routes map[TargetKind]Provider) (*ProviderRegistry, error) {
	return NewProviderRegistry(routes)
}

func (r *ProviderRegistry) Register(kind TargetKind, provider Provider) error {
	if r == nil || !validTarget(kind) || provider == nil {
		return ErrInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.providers[kind]; exists {
		return ErrConflict
	}
	r.providers[kind] = provider
	return nil
}

func (r *ProviderRegistry) Lookup(kind TargetKind) (Provider, error) {
	if r == nil || !validTarget(kind) {
		return nil, ErrProvider
	}
	r.mu.RLock()
	p := r.providers[kind]
	r.mu.RUnlock()
	if p == nil {
		return nil, ErrProvider
	}
	return p, nil
}

func (r *ProviderRegistry) Has(kind TargetKind) bool {
	_, err := r.Lookup(kind)
	return err == nil
}

func (r *ProviderRegistry) Apply(ctx context.Context, plan Plan, op Operation) (Readback, error) {
	p, err := r.Lookup(plan.TargetKind)
	if err != nil {
		return Readback{}, err
	}
	return p.Apply(ctx, plan, op)
}

func (r *ProviderRegistry) Destroy(ctx context.Context, plan Plan, op Operation) (Readback, error) {
	p, err := r.Lookup(plan.TargetKind)
	if err != nil {
		return Readback{}, err
	}
	return p.Destroy(ctx, plan, op)
}

func (r *ProviderRegistry) Read(ctx context.Context, plan Plan, op Operation) (Readback, error) {
	p, err := r.Lookup(plan.TargetKind)
	if err != nil {
		return Readback{}, err
	}
	return p.Read(ctx, plan, op)
}

var _ Provider = (*ProviderRegistry)(nil)
