package agentgateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
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
	// InputSchemaDigest, ResultSchemaDigest, and EventSchemaDigest pin the exact Agent descriptor
	// schema identities expected for this action. Empty values retain the
	// generic self-consistency proof for optional/extension actions; required
	// baseline actions with a known Agent contract are populated by
	// NewCatalogRequirement.
	InputSchemaDigest  []byte
	ResultSchemaDigest []byte
	EventSchemaDigest  []byte
	// RequireSchemaPin makes an empty expected digest a configuration error.
	// Readiness baseline actions set this flag so a missing generated pin fails
	// closed instead of silently degrading to self-consistency only. Optional
	// extension actions may leave it false.
	RequireSchemaPin bool
	// RequireEventSchemaPin applies the same fail-closed rule to a durable
	// stream's progress-event contract.
	RequireEventSchemaPin bool
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
		var operation *capv1.OperationDescriptor
		for _, candidate := range descriptor.GetOperations() {
			if candidate != nil && candidate.GetOperationId() == binding.operation {
				operation = candidate
				break
			}
		}
		if operation == nil {
			return fmt.Errorf("%w: operation %q/%q for action %q is not advertised", ErrCatalogInvalid, binding.capabilityID, binding.operation, action)
		}
		if requirement.RequireSchemaPin && (len(requirement.InputSchemaDigest) != sha256.Size || len(requirement.ResultSchemaDigest) != sha256.Size) {
			return fmt.Errorf("%w: action %q has no pinned schema identity", ErrCatalogInvalid, action)
		}
		if requirement.RequireEventSchemaPin && len(requirement.EventSchemaDigest) != sha256.Size {
			return fmt.Errorf("%w: action %q has no pinned event schema identity", ErrCatalogInvalid, action)
		}
		if err := validateOperationSchemas(operation, requirement); err != nil {
			return fmt.Errorf("%w: action %q: %v", ErrCatalogInvalid, action, err)
		}
	}
	return nil
}

func validateOperationSchemas(operation *capv1.OperationDescriptor, requirement CatalogRequirement) error {
	if operation == nil {
		return errors.New("operation descriptor is missing")
	}
	inputSchema := operation.GetInputSchemaJson()
	if strings.TrimSpace(inputSchema) == "" || !json.Valid([]byte(inputSchema)) {
		return errors.New("input schema is missing or invalid")
	}
	inputDigest := sha256.Sum256([]byte(inputSchema))
	if len(operation.GetInputSchemaDigest()) != sha256.Size || !bytes.Equal(operation.GetInputSchemaDigest(), inputDigest[:]) {
		return errors.New("input schema digest is missing or mismatched")
	}
	if len(requirement.InputSchemaDigest) > 0 {
		if len(requirement.InputSchemaDigest) != sha256.Size || !bytes.Equal(requirement.InputSchemaDigest, inputDigest[:]) {
			return errors.New("input schema does not match the expected contract")
		}
	}

	resultSchema := operation.GetResultSchemaJson()
	if strings.TrimSpace(resultSchema) == "" || !json.Valid([]byte(resultSchema)) {
		return errors.New("result schema is missing or invalid")
	}
	resultDigest := sha256.Sum256([]byte(resultSchema))
	if len(operation.GetResultSchemaDigest()) != sha256.Size || !bytes.Equal(operation.GetResultSchemaDigest(), resultDigest[:]) {
		return errors.New("result schema digest is missing or mismatched")
	}
	if len(requirement.ResultSchemaDigest) > 0 {
		if len(requirement.ResultSchemaDigest) != sha256.Size || !bytes.Equal(requirement.ResultSchemaDigest, resultDigest[:]) {
			return errors.New("result schema does not match the expected contract")
		}
	}

	eventSchema := operation.GetEventSchemaJson()
	eventDigestBytes := operation.GetEventSchemaDigest()
	if strings.TrimSpace(eventSchema) != "" || len(eventDigestBytes) > 0 || len(requirement.EventSchemaDigest) > 0 {
		if strings.TrimSpace(eventSchema) == "" || !json.Valid([]byte(eventSchema)) {
			return errors.New("event schema is missing or invalid")
		}
		eventDigest := sha256.Sum256([]byte(eventSchema))
		if len(eventDigestBytes) != sha256.Size || !bytes.Equal(eventDigestBytes, eventDigest[:]) {
			return errors.New("event schema digest is missing or mismatched")
		}
		if len(requirement.EventSchemaDigest) > 0 &&
			(len(requirement.EventSchemaDigest) != sha256.Size || !bytes.Equal(requirement.EventSchemaDigest, eventDigest[:])) {
			return errors.New("event schema does not match the expected contract")
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
