package coreworkload

import (
	"context"
	"encoding/json"
	"time"

	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	coretask "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
)

type Service struct {
	store Store
	now   func() time.Time
}

// ReadyForEmbedded verifies the durable/fenced execution boundary required by
// the single-process action facade. Provider routes are exact and are never
// inferred from a fallback provider.
func (s *Service) ReadyForEmbedded(handler *Handler) bool {
	if s == nil || s.store == nil || handler == nil || handler.store == nil || handler.provider == nil {
		return false
	}
	if _, ok := handler.store.(FencedStore); !ok {
		return false
	}
	registry, ok := handler.provider.(*ProviderRegistry)
	return ok && registry.Has(TargetAWSEC2SSM) && registry.Has(TargetAWSECS)
}

func NewService(store Store, now func() time.Time) (*Service, error) {
	if store == nil {
		return nil, ErrInvalid
	}
	if now == nil {
		now = time.Now
	}
	return &Service{store: store, now: now}, nil
}
func (s *Service) CreatePlan(ctx context.Context, in PlanInput) (Plan, error) {
	return s.store.CreatePlan(ctx, in)
}

func (s *Service) PinsAWSCredentialGrants() bool {
	pinner, ok := s.store.(AWSCredentialGrantPinningStore)
	return ok && pinner.PinsAWSCredentialGrants()
}
func (s *Service) GetPlan(ctx context.Context, id string) (Plan, error) {
	if !ValidUUID(id) {
		return Plan{}, ErrInvalid
	}
	return s.store.GetPlan(ctx, id)
}
func (s *Service) GetWorkload(ctx context.Context, id string) (Workload, error) {
	if !ValidUUID(id) {
		return Workload{}, ErrInvalid
	}
	return s.store.GetWorkload(ctx, id)
}
func (s *Service) ListWorkloads(ctx context.Context, limit int, cursor string) ([]Workload, string, error) {
	return s.store.ListWorkloads(ctx, limit, cursor)
}
func (s *Service) ListPlans(ctx context.Context, limit int, cursor string) ([]Plan, string, error) {
	return s.store.ListPlans(ctx, limit, cursor)
}

type Quote struct {
	ID, PlanID, PlanDigest, Summary string
	ExpiresAt, CreatedAt            time.Time
}

type QuoteStore interface {
	QuotePlan(context.Context, string, time.Time) (Quote, error)
	GetQuote(context.Context, string) (Quote, error)
}

func (s *Service) Quote(ctx context.Context, planID string) (Quote, error) {
	p, e := s.GetPlan(ctx, planID)
	if e != nil {
		return Quote{}, e
	}
	if durable, ok := s.store.(QuoteStore); ok {
		return durable.QuotePlan(ctx, p.ID, s.now().Add(15*time.Minute))
	}
	return Quote{PlanID: p.ID, PlanDigest: p.Digest, Summary: p.Summary}, nil
}

type RequestApplyInput struct {
	PlanID, WorkloadID, IdempotencyKey string
	ExpectedWorkloadRevision           uint64
	ExpiresAt                          time.Time
}
type RequestDestroyInput struct {
	PlanID, WorkloadID, IdempotencyKey string
	ExpectedWorkloadRevision           uint64
	ExpiresAt                          time.Time
}

// RequestApply accepts the structured input used by Go callers. The legacy
// three-string form remains accepted for small adapters while the API settles.
func (s *Service) RequestApply(ctx context.Context, input interface{}, rest ...string) (RequestResult, error) {
	c := RequestCommand{Kind: OperationApply}
	switch v := input.(type) {
	case RequestApplyInput:
		c.PlanID, c.WorkloadID, c.IdempotencyKey, c.ExpectedWorkloadRevision = v.PlanID, v.WorkloadID, v.IdempotencyKey, v.ExpectedWorkloadRevision
		c.ExpiresAt = v.ExpiresAt
	case string:
		if len(rest) != 2 {
			return RequestResult{}, ErrInvalid
		}
		c.PlanID, c.WorkloadID, c.IdempotencyKey = v, rest[0], rest[1]
	default:
		return RequestResult{}, ErrInvalid
	}
	return s.request(ctx, c)
}
func (s *Service) RequestDestroy(ctx context.Context, input interface{}, rest ...string) (RequestResult, error) {
	c := RequestCommand{Kind: OperationDestroy}
	switch v := input.(type) {
	case RequestDestroyInput:
		c.PlanID, c.WorkloadID, c.IdempotencyKey, c.ExpectedWorkloadRevision = v.PlanID, v.WorkloadID, v.IdempotencyKey, v.ExpectedWorkloadRevision
		c.ExpiresAt = v.ExpiresAt
	case string:
		if len(rest) != 2 {
			return RequestResult{}, ErrInvalid
		}
		c.PlanID, c.WorkloadID, c.IdempotencyKey = v, rest[0], rest[1]
	default:
		return RequestResult{}, ErrInvalid
	}
	return s.request(ctx, c)
}
func (s *Service) Cancel(ctx context.Context, operationID, idempotencyKey string, expected uint64) (Operation, error) {
	if !ValidUUID(operationID) || !ValidUUID(idempotencyKey) {
		return Operation{}, ErrInvalid
	}
	return s.store.CancelOperation(ctx, operationID, idempotencyKey, expected)
}
func (s *Service) request(ctx context.Context, c RequestCommand) (RequestResult, error) {
	if !ValidUUID(c.PlanID) || !ValidUUID(c.IdempotencyKey) || (c.WorkloadID == "") != (c.ExpectedWorkloadRevision == 0) {
		return RequestResult{}, ErrInvalid
	}
	if !c.ExpiresAt.IsZero() && c.ExpiresAt.Location() != time.UTC {
		return RequestResult{}, ErrInvalid
	}
	return s.store.RequestOperation(ctx, c)
}
func (s *Service) Confirm(ctx context.Context, id string, revision int64) (ConfirmationResult, error) {
	c, e := s.store.Confirm(ctx, id, revision)
	return ConfirmationResult{Confirmation: c}, e
}

type ConfirmationResult struct{ Confirmation coreconfirmation.Confirmation }

func (s *Service) GetOperation(ctx context.Context, id string) (Operation, error) {
	if !ValidUUID(id) {
		return Operation{}, ErrInvalid
	}
	return s.store.GetOperation(ctx, id)
}
func (s *Service) ListEvents(ctx context.Context, id string, after uint64) ([]Event, error) {
	return s.store.ListEvents(ctx, id, after)
}

// Provider is the only execution boundary. Implementations must use typed,
// preconfigured clients; this package never accepts a command string or SDK
// object from a caller.
type Provider interface {
	Apply(context.Context, Plan, Operation) (Readback, error)
	Destroy(context.Context, Plan, Operation) (Readback, error)
	Read(context.Context, Plan, Operation) (Readback, error)
}

type Handler struct {
	store    Store
	provider Provider
}

func NewHandler(store Store, provider Provider) (*Handler, error) {
	if store == nil || provider == nil {
		return nil, ErrInvalid
	}
	return &Handler{store: store, provider: provider}, nil
}
func (h *Handler) GetOperation(ctx context.Context, id string) (Operation, error) {
	if h == nil || h.store == nil || !ValidUUID(id) {
		return Operation{}, ErrInvalid
	}
	return h.store.GetOperation(ctx, id)
}
func (h *Handler) Handle(ctx context.Context, operationID, planDigest string, expectedRevision uint64, fences ...TaskFence) (Operation, error) {
	if !ValidUUID(operationID) || !ValidDigest(planDigest) || expectedRevision == 0 {
		return Operation{}, ErrInvalid
	}
	op, e := h.store.GetOperation(ctx, operationID)
	if e != nil {
		return Operation{}, e
	}
	if op.Status == OperationSucceeded || op.Status == OperationFailed || op.Status == OperationRejected || op.Status == OperationExpired || op.Status == OperationCanceled || op.Status == OperationUncertain {
		return op, nil
	}
	plan, e := h.store.GetPlan(ctx, op.PlanID)
	if e != nil {
		return Operation{}, e
	}
	if plan.Digest != planDigest || plan.Revision != op.PlanRevision {
		return Operation{}, ErrStale
	}
	var opTask coretask.Task
	var fence *TaskFence
	if len(fences) > 1 {
		return Operation{}, ErrInvalid
	}
	if len(fences) == 1 {
		fence = &fences[0]
		if !fence.Valid(time.Time{}) {
			return Operation{}, ErrRevisionConflict
		}
	}
	op, opTask, e = h.consume(ctx, op.ID, op.ConfirmationID, planDigest, expectedRevision, fence)
	if e != nil {
		return Operation{}, e
	}
	if opTask.Status != "running" || opTask.Attempt == 0 || opTask.LeaseEpoch == 0 || opTask.Lease == nil || opTask.Revision == 0 {
		return Operation{}, ErrRevisionConflict
	}
	if fence != nil && !sameTaskFence(opTask, *fence) {
		return Operation{}, ErrRevisionConflict
	}
	if _, e = h.store.AppendEvent(ctx, op.ID, Event{Kind: "dispatched", Status: OperationRunning, Message: "provider dispatch claimed"}); e != nil {
		return Operation{}, e
	}
	providerCtx, stopProvider := context.WithCancel(ctx)
	renewErr := make(chan error, 1)
	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-providerCtx.Done():
				return
			case <-ticker.C:
				if _, err := h.renewDispatchLease(providerCtx, op.ID, op.DispatchClaim, op.DispatchEpoch, fence); err != nil {
					select {
					case renewErr <- err:
					default:
					}
					stopProvider()
					return
				}
			}
		}
	}()
	if op.Kind == OperationApply {
		_, e = h.provider.Apply(providerCtx, plan, op)
	} else {
		_, e = h.provider.Destroy(providerCtx, plan, op)
	}
	providerErr := e
	stopProvider()
	<-renewDone
	select {
	case renewFailure := <-renewErr:
		return Operation{}, renewFailure
	default:
	}
	if op, e = h.renewDispatchLease(ctx, op.ID, op.DispatchClaim, op.DispatchEpoch, fence); e != nil {
		return Operation{}, e
	}
	e = providerErr
	if e != nil {
		_, errText := SafeFailure("provider_error", e.Error())
		if _, ae := h.store.AppendEvent(ctx, op.ID, Event{Kind: "provider_error", Status: OperationRunning, Message: errText}); ae != nil {
			return Operation{}, ae
		}
		recovered, readErr := h.provider.Read(ctx, plan, op)
		recovered = SanitizeReadback(recovered)
		if recovered.Digest == "" {
			recovered.Digest = ReadbackDigest(recovered)
		}
		if readErr != nil {
			_, readText := SafeFailure("provider_uncertain", readErr.Error())
			eventCtx := WithProviderResultEvent(ctx, ProviderResultEvent{Kind: "uncertain", Status: OperationUncertain, Message: readText, Readback: mustJSON(recovered)})
			out, _, ce := h.completeDispatch(eventCtx, op.ID, op.TaskID, op.DispatchClaim, op.DispatchEpoch, "provider_uncertain", recovered, readText, fence)
			if ce != nil {
				return Operation{}, ce
			}
			return out, RedactError(readErr)
		}
		eventCtx := WithProviderResultEvent(ctx, ProviderResultEvent{Kind: "recovered_readback", Status: OperationRunning, Readback: mustJSON(recovered)})
		code := ""
		if recovered.WorkloadID == "" || recovered.WorkloadID != op.WorkloadID || recovered.TargetKind != op.TargetKind || !targetIdentityEqualForState(recovered.Identity, plan.Target.Identity, plan.TargetKind, recovered.State) || (op.Kind == OperationApply && recovered.State != "ready") || (op.Kind == OperationDestroy && recovered.State != "destroyed") {
			code = "provider_uncertain"
		}
		out, _, ce := h.completeDispatch(eventCtx, op.ID, op.TaskID, op.DispatchClaim, op.DispatchEpoch, code, recovered, code, fence)
		if ce != nil {
			return Operation{}, ce
		}
		return out, nil
	}
	readback, readErr := h.provider.Read(ctx, plan, op)
	if readErr != nil {
		_, errText := SafeFailure("provider_uncertain", readErr.Error())
		eventCtx := WithProviderResultEvent(ctx, ProviderResultEvent{Kind: "uncertain", Status: OperationUncertain, Message: errText, Readback: mustJSON(SanitizeReadback(readback))})
		out, _, ce := h.completeDispatch(eventCtx, op.ID, op.TaskID, op.DispatchClaim, op.DispatchEpoch, "provider_uncertain", SanitizeReadback(readback), errText, fence)
		if ce != nil {
			return Operation{}, ce
		}
		return out, RedactError(readErr)
	}
	readback = SanitizeReadback(readback)
	if readback.Digest == "" {
		readback.Digest = ReadbackDigest(readback)
	}
	eventCtx := WithProviderResultEvent(ctx, ProviderResultEvent{Kind: "readback", Status: OperationRunning, Readback: mustJSON(readback)})
	code := ""
	if readback.WorkloadID == "" || readback.WorkloadID != op.WorkloadID || readback.TargetKind != op.TargetKind || !targetIdentityEqualForState(readback.Identity, plan.Target.Identity, plan.TargetKind, readback.State) || (op.Kind == OperationApply && readback.State != "ready") || (op.Kind == OperationDestroy && readback.State != "destroyed") {
		code = "provider_uncertain"
	}
	out, _, e := h.completeDispatch(eventCtx, op.ID, op.TaskID, op.DispatchClaim, op.DispatchEpoch, code, readback, code, fence)
	return out, e
}

func sameTaskFence(task coretask.Task, f TaskFence) bool {
	return task.ID == f.TaskID &&
		task.Attempt == f.Attempt &&
		task.LeaseEpoch == f.LeaseEpoch &&
		task.Revision >= f.Revision &&
		task.Lease != nil &&
		task.Lease.Holder == f.Holder &&
		task.Lease.ExpiresAt.After(time.Now().UTC())
}

func (h *Handler) consume(ctx context.Context, id, confirmationID, digest string, expected uint64, fence *TaskFence) (Operation, coretask.Task, error) {
	if fence != nil {
		if s, ok := h.store.(FencedStore); ok {
			return s.ConsumeFenced(ctx, id, confirmationID, digest, expected, *fence)
		}
	}
	return h.store.Consume(ctx, id, confirmationID, digest, expected)
}

func (h *Handler) renewDispatchLease(ctx context.Context, id, claim string, epoch uint64, fence *TaskFence) (Operation, error) {
	if fence != nil {
		if s, ok := h.store.(FencedStore); ok {
			return s.RenewDispatchLeaseFenced(ctx, id, claim, epoch, *fence)
		}
	}
	return h.store.RenewDispatchLease(ctx, id, claim, epoch)
}

func (h *Handler) completeDispatch(ctx context.Context, id, taskID, claim string, epoch uint64, code string, readback Readback, summary string, fence *TaskFence) (Operation, coretask.Task, error) {
	if fence != nil {
		if s, ok := h.store.(FencedStore); ok {
			return s.CompleteDispatchFenced(ctx, id, taskID, claim, epoch, code, readback, summary, *fence)
		}
	}
	return h.store.CompleteDispatch(ctx, id, taskID, claim, epoch, code, readback, summary)
}

// Recover reconciles a previously dispatched/uncertain operation by reading
// the provider. It never redispatches an unknown mutation.
func (h *Handler) Recover(ctx context.Context, operationID string, fences ...TaskFence) (Operation, error) {
	if !ValidUUID(operationID) {
		return Operation{}, ErrInvalid
	}
	if len(fences) > 1 {
		return Operation{}, ErrInvalid
	}
	var fence *TaskFence
	if len(fences) == 1 {
		fence = &fences[0]
		if !fence.Valid(time.Time{}) {
			return Operation{}, ErrRevisionConflict
		}
	}
	// A terminal task cannot reacquire the generic worker fence after an
	// uncertain provider result. Reconcile that state through the dedicated
	// read-only path instead of attempting RecoverClaim/CompleteDispatch.
	current, getErr := h.store.GetOperation(ctx, operationID)
	if getErr != nil {
		return Operation{}, getErr
	}
	if current.Status == OperationUncertain || (current.Status != OperationRunning && current.DispatchState == "uncertain") {
		if h.provider == nil {
			return current, nil
		}
		plan, planErr := h.store.GetPlan(ctx, current.PlanID)
		if planErr != nil {
			return Operation{}, planErr
		}
		rb, readErr := h.provider.Read(ctx, plan, current)
		rb = SanitizeReadback(rb)
		if rb.Digest == "" {
			rb.Digest = ReadbackDigest(rb)
		}
		code := ""
		if readErr != nil || rb.WorkloadID == "" || rb.WorkloadID != current.WorkloadID || rb.TargetKind != current.TargetKind || !targetIdentityEqualForState(rb.Identity, plan.Target.Identity, plan.TargetKind, rb.State) || (current.Kind == OperationApply && rb.State != "ready") || (current.Kind == OperationDestroy && rb.State != "destroyed") {
			code = "provider_uncertain"
		}
		if reconciler, ok := h.store.(UncertainReconciler); ok {
			message := code
			if readErr != nil {
				_, message = SafeFailure("provider_uncertain", readErr.Error())
			}
			eventStatus := OperationRunning
			if code != "" {
				eventStatus = OperationUncertain
			}
			return reconciler.ReconcileUncertain(WithProviderResultEvent(ctx, ProviderResultEvent{Kind: "reconciled_readback", Status: eventStatus, Message: message, Readback: mustJSON(rb)}), operationID, code, rb, message)
		}
		return current, nil
	}
	var op Operation
	var err error
	if fence != nil {
		if s, ok := h.store.(FencedStore); ok {
			op, err = s.RecoverClaimFenced(ctx, operationID, "", *fence)
		} else {
			op, err = h.store.RecoverClaim(ctx, operationID, "")
		}
	} else {
		op, err = h.store.RecoverClaim(ctx, operationID, "")
	}
	if err != nil {
		if err == ErrRevisionConflict {
			return h.store.GetOperation(ctx, operationID)
		}
		return Operation{}, err
	}
	if op.Status != OperationRunning || op.DispatchState != "uncertain" {
		return op, nil
	}
	plan, err := h.store.GetPlan(ctx, op.PlanID)
	if err != nil {
		return Operation{}, err
	}
	rb, readErr := h.provider.Read(ctx, plan, op)
	rb = SanitizeReadback(rb)
	if rb.Digest == "" {
		rb.Digest = ReadbackDigest(rb)
	}
	if readErr != nil {
		_, errText := SafeFailure("provider_uncertain", readErr.Error())
		eventCtx := WithProviderResultEvent(ctx, ProviderResultEvent{Kind: "recovered_uncertain", Status: OperationUncertain, Message: errText, Readback: mustJSON(rb)})
		out, _, completeErr := h.completeDispatch(eventCtx, op.ID, op.TaskID, op.DispatchClaim, op.DispatchEpoch, "provider_uncertain", rb, errText, fence)
		if completeErr != nil {
			return Operation{}, completeErr
		}
		return out, RedactError(readErr)
	}
	eventCtx := WithProviderResultEvent(ctx, ProviderResultEvent{Kind: "recovered_readback", Status: OperationRunning, Readback: mustJSON(rb)})
	code := ""
	if rb.WorkloadID == "" || rb.WorkloadID != op.WorkloadID || rb.TargetKind != op.TargetKind || !targetIdentityEqualForState(rb.Identity, plan.Target.Identity, plan.TargetKind, rb.State) || (op.Kind == OperationApply && rb.State != "ready") || (op.Kind == OperationDestroy && rb.State != "destroyed") {
		code = "provider_uncertain"
	}
	out, _, err := h.completeDispatch(eventCtx, op.ID, op.TaskID, op.DispatchClaim, op.DispatchEpoch, code, rb, code, fence)
	return out, err
}

// Reconcile is the explicit operator/user-triggered successor for an
// uncertain operation. It performs only provider Read and never redispatches
// the unknown mutation.
func (h *Handler) Reconcile(ctx context.Context, operationID string) (Operation, error) {
	return h.Recover(ctx, operationID)
}

func targetIdentityEqual(actual, desired TargetIdentity, kind TargetKind) bool {
	if actual.Kind == "" {
		actual.Kind = kind
	}
	if desired.Kind == "" {
		desired.Kind = kind
	}
	return actual == desired
}

func targetIdentityEqualForState(actual, desired TargetIdentity, kind TargetKind, state string) bool {
	if state == "destroyed" && kind == TargetAWSEC2SSM {
		desired.Endpoint = ""
	}
	return targetIdentityEqual(actual, desired, kind)
}

type redactedError string

func (e redactedError) Error() string { return string(e) }
func RedactError(err error) error {
	if err == nil {
		return nil
	}
	return redactedError("provider operation failed")
}

func ReadbackDigest(v Readback) string {
	v.Digest = ""
	raw, _ := json.Marshal(v)
	return canonicalDigest(string(raw))
}

// CompletionFingerprint binds terminal replay to the canonical outcome. A
// provider-supplied digest is never trusted.
func CompletionFingerprint(code string, readback Readback) string {
	readback = SanitizeReadback(readback)
	safeCode, _ := SafeFailure(code, "")
	return canonicalDigest(struct {
		Code     string
		Readback Readback
	}{safeCode, readback})
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
