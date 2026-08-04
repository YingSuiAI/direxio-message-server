package agentgateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strings"

	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
	"google.golang.org/protobuf/proto"
)

// ErrCatalogInvalid marks a peer that is reachable but cannot safely serve the
// configured ProductCore Native Agent surface. Callers should keep readiness
// failed until a fresh catalog probe succeeds.
var ErrCatalogInvalid = errors.New("native agent capability catalog is invalid")

// CatalogRequirement is a public action that must be represented by one
// advertised, ready Agent capability operation before message-server can
// report external Native Agent readiness.
type CatalogRequirement struct {
	Action string
}

// ProbeCatalog performs one bounded, authenticated catalog probe. The
// operation map remains owned by Runner, so callers cannot accidentally send
// an action name over the wire or validate a different capability/operation
// pair than Invoke would use.
func (r *Runner) ProbeCatalog(ctx context.Context, requirements []CatalogRequirement) error {
	if r == nil || r.client == nil {
		return fmt.Errorf("%w: agent gateway is not configured", ErrCatalogInvalid)
	}
	callCtx := r.client.createCallContext(r.operationIDFor(nil))
	catalog, err := r.client.DescribeCapabilities(ctx, callCtx)
	if err != nil {
		return err
	}
	return ValidateCatalog(catalog, requirements)
}

// ValidateCatalog checks the immutable protocol/digest envelope and every
// required action binding. Optional capabilities are deliberately omitted from
// requirements so a disabled deployment does not poison overall readiness.
func ValidateCatalog(catalog *capv1.DescribeCapabilitiesResponse, requirements []CatalogRequirement) error {
	if catalog == nil {
		return fmt.Errorf("%w: empty response", ErrCatalogInvalid)
	}
	if catalog.GetCatalogVersion() != 1 {
		return fmt.Errorf("%w: unsupported catalog version %d", ErrCatalogInvalid, catalog.GetCatalogVersion())
	}
	if len(catalog.GetCatalogDigest()) != sha256.Size {
		return fmt.Errorf("%w: catalog digest is missing", ErrCatalogInvalid)
	}
	if digest := computeCatalogDigest(catalog.GetCapabilities()); !bytes.Equal(digest, catalog.GetCatalogDigest()) {
		return fmt.Errorf("%w: catalog digest mismatch", ErrCatalogInvalid)
	}

	byCapability := make(map[string]*capv1.CapabilityDescriptor, len(catalog.GetCapabilities()))
	for _, descriptor := range catalog.GetCapabilities() {
		if descriptor == nil || strings.TrimSpace(descriptor.GetCapabilityId()) == "" || descriptor.GetProtocolVersion() != 1 {
			return fmt.Errorf("%w: malformed capability descriptor", ErrCatalogInvalid)
		}
		if _, exists := byCapability[descriptor.GetCapabilityId()]; exists {
			return fmt.Errorf("%w: duplicate capability %q", ErrCatalogInvalid, descriptor.GetCapabilityId())
		}
		byCapability[descriptor.GetCapabilityId()] = descriptor
	}
	for _, requirement := range requirements {
		action := strings.TrimSpace(requirement.Action)
		if action == "" {
			return fmt.Errorf("%w: empty required action", ErrCatalogInvalid)
		}
		binding, ok := actionBindingFor(action)
		if !ok {
			return fmt.Errorf("%w: action %q has no binding", ErrCatalogInvalid, action)
		}
		descriptor := byCapability[binding.capabilityID]
		if descriptor == nil {
			return fmt.Errorf("%w: capability %q for action %q is not advertised", ErrCatalogInvalid, binding.capabilityID, action)
		}
		if !descriptor.GetReadiness() {
			return fmt.Errorf("%w: capability %q is not ready", ErrCatalogInvalid, binding.capabilityID)
		}
		found := false
		for _, operation := range descriptor.GetOperations() {
			if operation != nil && operation.GetOperationId() == binding.operation {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%w: operation %q/%q for action %q is not advertised", ErrCatalogInvalid, binding.capabilityID, binding.operation, action)
		}
	}
	return nil
}

func computeCatalogDigest(descriptors []*capv1.CapabilityDescriptor) []byte {
	sorted := append([]*capv1.CapabilityDescriptor(nil), descriptors...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i] == nil {
			return false
		}
		if sorted[j] == nil {
			return true
		}
		return sorted[i].GetCapabilityId() < sorted[j].GetCapabilityId()
	})
	h := sha256.New()
	for _, descriptor := range sorted {
		if descriptor == nil {
			continue
		}
		encoded, _ := proto.MarshalOptions{Deterministic: true}.Marshal(descriptor)
		_, _ = h.Write(encoded)
	}
	return h.Sum(nil)
}
