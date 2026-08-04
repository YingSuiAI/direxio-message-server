package dirextalktransport

import "context"

type capabilityOperationContextKey struct{}

// CapabilityOperationContext is trusted, server-derived operation metadata.
// It is injected by Product Capability immediately before invoking a provider
// handler and is never decoded from Agent-controlled JSON.
type CapabilityOperationContext struct {
	OperationID string
	OwnerID     string
	Generation  int64
	RootDigest  []byte
}

func WithCapabilityOperationContext(ctx context.Context, value CapabilityOperationContext) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	value.RootDigest = append([]byte(nil), value.RootDigest...)
	return context.WithValue(ctx, capabilityOperationContextKey{}, value)
}

func CapabilityOperationContextFrom(ctx context.Context) (CapabilityOperationContext, bool) {
	if ctx == nil {
		return CapabilityOperationContext{}, false
	}
	value, ok := ctx.Value(capabilityOperationContextKey{}).(CapabilityOperationContext)
	if !ok || value.OperationID == "" || value.OwnerID == "" || value.Generation <= 0 || len(value.RootDigest) != 32 {
		return CapabilityOperationContext{}, false
	}
	value.RootDigest = append([]byte(nil), value.RootDigest...)
	return value, true
}
