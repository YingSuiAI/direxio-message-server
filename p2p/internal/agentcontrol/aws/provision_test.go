package aws

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

func provisionFromPlan(plan Plan, id string) Provision {
	now := time.Now().UTC()
	return Provision{ID: id, PlanID: plan.ID, CredentialID: plan.CredentialID, CredentialRevision: plan.CredentialRevision, Region: plan.Region, StackName: plan.StackName, Profile: plan.Tags["service"], OwnerDigest: plan.Tags["owner"], PlanRevision: plan.Revision, TemplateSHA256: plan.TemplateSHA256, PlanDigest: PlanDigest(plan), State: "planned", Revision: 1, CreatedAt: now, UpdatedAt: now}
}

func TestProvisionOwnerDigestRequiresCanonicalPrefix(t *testing.T) {
	plan := Plan{ID: uuid.NewString(), CredentialID: uuid.NewString(), CredentialRevision: 1, Region: "us-east-1", StackName: "owner-digest", Operation: OperationCreate, Template: []byte(`{"Resources":{}}`), TemplateSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Tags: map[string]string{"service": EC2ServiceProfile, "owner": OwnerBindingDigest("owner")}, Revision: 1}
	p := provisionFromPlan(plan, uuid.NewString())
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	p.OwnerDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := p.Validate(); err != ErrInvalid {
		t.Fatalf("raw owner digest validation = %v, want ErrInvalid", err)
	}
}

func TestProvisionIsPinnedAndChangeLinkIsExclusive(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	s := NewService(repo, &testConfirm{}, testTasks{}, nil, NewFakeProvider(), time.Now)
	credential, err := saveVerifiedCredential(t, s, repo, CredentialInput{Name: "provision", Region: "us-east-1", AccessKeyID: "a", SecretAccessKey: "b", IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.CreatePlan(ctx, PlanInput{CredentialID: credential.ID, StackName: "provision-stack", Operation: OperationCreate, Template: []byte(`{"Resources":{}}`), Tags: map[string]string{"service": EC2ServiceProfile, "owner": OwnerBindingDigest("owner")}, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	fullPlan, err := repo.GetPlan(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	p := Provision{ID: uuid.NewString(), PlanID: fullPlan.ID, CredentialID: credential.ID, CredentialRevision: credential.Revision, Region: fullPlan.Region, StackName: fullPlan.StackName, Profile: EC2ServiceProfile, OwnerDigest: fullPlan.Tags["owner"], PlanRevision: fullPlan.Revision, TemplateSHA256: fullPlan.TemplateSHA256, PlanDigest: PlanDigest(fullPlan), State: "planned", Revision: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if _, err = repo.CreateProvision(ctx, p); err != nil {
		t.Fatal(err)
	}
	requested, err := s.RequestChange(ctx, RequestChangeInput{PlanID: plan.ID, ProvisionID: p.ID, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	if requested.Change.ProvisionID != p.ID {
		t.Fatalf("missing immutable link: %#v", requested.Change)
	}
	got, err := repo.GetProvision(ctx, p.ID)
	if err != nil || got.ActiveChangeID != requested.Change.ID || got.State != "creating" {
		t.Fatalf("provision link: %#v %v", got, err)
	}
	if _, err := s.RequestChange(ctx, RequestChangeInput{PlanID: plan.ID, ProvisionID: p.ID, IdempotencyKey: uuid.NewString()}); err != ErrRevisionConflict {
		t.Fatalf("second active change = %v", err)
	}
}

func TestProvisionMutationLeaseIsBusyNonBlockingAndFenced(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	s := NewService(repo, &testConfirm{}, testTasks{}, nil, NewFakeProvider(), time.Now)
	credential, err := saveVerifiedCredential(t, s, repo, CredentialInput{Name: "lease", Region: "us-east-1", AccessKeyID: "a", SecretAccessKey: "b", IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	fullCredential, err := repo.GetCredential(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fullCredential.VerifiedRevision != fullCredential.Revision {
		fullCredential, err = repo.RecordCredentialIdentity(ctx, credential.ID, credential.Revision, Identity{AccountID: "123456789012", UserARN: "arn:aws:iam::123456789012:user/test"})
		if err != nil {
			t.Fatal(err)
		}
	}
	credential = fullCredential.View()
	planView, err := s.CreatePlan(ctx, PlanInput{CredentialID: credential.ID, StackName: "lease-stack", Operation: OperationCreate, Template: []byte(`{"Resources":{}}`), Tags: map[string]string{"service": EC2ServiceProfile, "owner": OwnerBindingDigest("owner")}, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := repo.GetPlan(ctx, planView.ID)
	if err != nil {
		t.Fatal(err)
	}
	p := provisionFromPlan(plan, uuid.NewString())
	if _, err = repo.CreateProvision(ctx, p); err != nil {
		t.Fatal(err)
	}
	first, err := s.AcquireProvisionMutation(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AcquireProvisionMutation(ctx, p.ID); err != ErrConflict {
		t.Fatalf("active lease should fail busy without blocking: %v", err)
	}
	if err := first.Renew(ctx); err != nil {
		t.Fatalf("active lease renew = %v", err)
	}
	if err := first.Assert(ctx); err != nil {
		t.Fatalf("active lease assert = %v", err)
	}
	assertCtx, cancelAssert := context.WithCancel(ctx)
	cancelAssert()
	if err := first.Assert(assertCtx); err != context.Canceled {
		t.Fatalf("canceled assert = %v, want context canceled", err)
	}
	if _, err = s.RequestChange(ctx, RequestChangeInput{PlanID: plan.ID, ProvisionID: p.ID, ExpectedProvisionRevision: p.Revision, IdempotencyKey: uuid.NewString()}); err != ErrConflict {
		t.Fatalf("request-change must honor active GeoLibre lease: %v", err)
	}
	// An expired holder can be taken over. Its stale release must not clear the
	// newer epoch, so request-change remains fenced until the new holder exits.
	repo.mu.Lock()
	lease := repo.provisionLeases[p.ID]
	lease.expiresAt = time.Now().Add(-time.Minute)
	repo.provisionLeases[p.ID] = lease
	repo.mu.Unlock()
	second, err := s.AcquireProvisionMutation(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Release(ctx); err != ErrConflict {
		t.Fatalf("stale holder release = %v, want conflict", err)
	}
	if err := first.Assert(ctx); err != ErrConflict {
		t.Fatalf("stale holder assert = %v, want conflict", err)
	}
	if _, err = s.RequestChange(ctx, RequestChangeInput{PlanID: plan.ID, ProvisionID: p.ID, ExpectedProvisionRevision: p.Revision, IdempotencyKey: uuid.NewString()}); err != ErrConflict {
		t.Fatalf("stale release cleared replacement lease: %v", err)
	}
	if err := second.Release(ctx); err != nil {
		t.Fatal(err)
	}
	third, err := s.AcquireProvisionMutation(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	operationID := uuid.NewString()
	if binder, ok := third.(ProvisionMutationOperationBinder); !ok {
		t.Fatal("memory lease does not support operation binding")
	} else if err := binder.BindOperation(ctx, operationID); err != nil {
		t.Fatal(err)
	}
	repo.mu.Lock()
	activeLease := repo.provisionLeases[p.ID]
	activeLease.expiresAt = time.Now().Add(-time.Hour)
	repo.provisionLeases[p.ID] = activeLease
	repo.mu.Unlock()
	claimedActive, err := s.ClaimProvisionMutation(ctx, p.ID, operationID)
	if err != nil {
		t.Fatalf("expired active lease claim = %v", err)
	}
	if err := claimedActive.MarkUncertain(ctx, operationID); err != nil {
		t.Fatal(err)
	}
	repo.mu.Lock()
	uncertainLease := repo.provisionLeases[p.ID]
	uncertainLease.expiresAt = time.Now().Add(-time.Hour)
	repo.provisionLeases[p.ID] = uncertainLease
	repo.mu.Unlock()
	if _, err = s.AcquireProvisionMutation(ctx, p.ID); err != ErrConflict {
		t.Fatalf("unresolved expired lease should block takeover: %v", err)
	}
	type claimResult struct {
		lease ProvisionMutationLease
		err   error
	}
	claimResults := make(chan claimResult, 2)
	for i := 0; i < 2; i++ {
		go func() {
			claimLease, claimErr := s.ClaimProvisionMutation(ctx, p.ID, operationID)
			claimResults <- claimResult{lease: claimLease, err: claimErr}
		}()
	}
	claimSuccesses := 0
	var claimed ProvisionMutationLease
	for i := 0; i < 2; i++ {
		result := <-claimResults
		if result.err == nil {
			claimSuccesses++
			claimed = result.lease
		}
	}
	if claimSuccesses != 1 {
		t.Fatalf("concurrent uncertain claims succeeded %d times, want one", claimSuccesses)
	}
	if err := claimed.MarkUncertain(ctx, operationID); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveProvisionMutation(ctx, p.ID, uncertainLease.operationID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RequestChange(ctx, RequestChangeInput{PlanID: plan.ID, ProvisionID: p.ID, ExpectedProvisionRevision: p.Revision, IdempotencyKey: uuid.NewString()}); err != nil {
		t.Fatalf("released lease should permit request-change: %v", err)
	}
}

func TestProvisionReadbackRejectsUnknownOrIncompleteOutputs(t *testing.T) {
	if _, err := ProvisionReadbackFromStack(StackOutputs{"Other": "secret"}, time.Now()); err != ErrInvalid {
		t.Fatalf("unknown output must not persist: %v", err)
	}
	readback, err := ProvisionReadbackFromStack(StackOutputs{"StackId": "stack", "InstanceId": "i-1", "SecurityGroupId": "sg-1", "Other": "secret"}, time.Now())
	if err != nil || readback.PublicIP != "" || readback.OutputDigest == "" {
		t.Fatalf("allowlist conversion: %#v %v", readback, err)
	}
}

func TestTypedProvisionCompletionFailsClosedThenPersistsReadbackAtomically(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	s := NewService(repo, &testConfirm{}, testTasks{}, nil, NewFakeProvider(), time.Now)
	credential, _ := saveVerifiedCredential(t, s, repo, CredentialInput{Name: "complete", Region: "us-east-1", AccessKeyID: "a", SecretAccessKey: "b", IdempotencyKey: uuid.NewString()})
	planView, _ := s.CreatePlan(ctx, PlanInput{CredentialID: credential.ID, StackName: "complete-stack", Operation: OperationCreate, Template: []byte(`{"Resources":{}}`), Tags: map[string]string{"service": EC2ServiceProfile, "owner": OwnerBindingDigest("owner")}, IdempotencyKey: uuid.NewString()})
	plan, _ := repo.GetPlan(ctx, planView.ID)
	p := Provision{ID: uuid.NewString(), PlanID: plan.ID, CredentialID: credential.ID, CredentialRevision: credential.Revision, Region: plan.Region, StackName: plan.StackName, Profile: EC2ServiceProfile, OwnerDigest: plan.Tags["owner"], PlanRevision: plan.Revision, TemplateSHA256: plan.TemplateSHA256, PlanDigest: PlanDigest(plan), State: "planned", Revision: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if _, err := repo.CreateProvision(ctx, p); err != nil {
		t.Fatal(err)
	}
	requested, err := s.RequestChange(ctx, RequestChangeInput{PlanID: plan.ID, ProvisionID: p.ID, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	consumeWorkflowChange(t, s, repo, requested)
	fence, err := s.ExecutionFence(ctx, requested.Confirmation.ConfirmationID)
	if err != nil {
		t.Fatal(err)
	}
	base := CompleteChangeCommand{ChangeID: fence.Change.ID, ConfirmationID: fence.Confirmation.ConfirmationID, TaskID: fence.Task.ID, Attempt: fence.Task.Attempt, LeaseEpoch: fence.Task.LeaseEpoch, ExpectedTaskRevision: fence.Task.Revision, ExpectedChangeRevision: fence.Change.Revision, ExpectedConfirmationRevision: fence.Confirmation.Revision, Status: ChangeSucceeded}
	if _, err := s.CompleteChange(ctx, base); err != ErrRevisionConflict {
		t.Fatalf("typed completion without readback = %v", err)
	}
	if got, _ := repo.GetProvision(ctx, p.ID); got.ActiveChangeID != requested.Change.ID || got.State != "creating" {
		t.Fatalf("failed terminalization mutated provision: %#v", got)
	}
	readback, _ := ProvisionReadbackFromStack(StackOutputs{"StackId": "stack", "InstanceId": "i-1", "SecurityGroupId": "sg-1"}, time.Now())
	base.Readback = &readback
	if _, err := s.CompleteChange(ctx, base); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetProvision(ctx, p.ID)
	if err != nil || got.State != "active" || got.ActiveChangeID != "" || got.Readback.OutputDigest != readback.OutputDigest {
		t.Fatalf("atomic typed completion: %#v %v", got, err)
	}
	if _, err := s.CompleteChange(ctx, base); err != nil {
		t.Fatalf("lost response replay: %v", err)
	}
}

func TestProvisionDeleteRequiresMatchingDistinctDeletePlan(t *testing.T) {
	base := Plan{ID: uuid.NewString(), CredentialID: uuid.NewString(), CredentialRevision: 1, Region: "us-east-1", StackName: "delete-stack", Operation: OperationCreate, Template: []byte(`{"Resources":{}}`), TemplateSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Tags: map[string]string{"service": EC2ServiceProfile, "owner": OwnerBindingDigest("owner")}, Revision: 1}
	p := Provision{ID: uuid.NewString(), PlanID: base.ID, CredentialID: base.CredentialID, CredentialRevision: 1, Region: base.Region, StackName: base.StackName, Profile: EC2ServiceProfile, OwnerDigest: base.Tags["owner"], PlanRevision: 1, TemplateSHA256: base.TemplateSHA256, PlanDigest: PlanDigest(base), State: "active", Revision: 2}
	deletePlan := base
	deletePlan.ID = uuid.NewString()
	deletePlan.Operation = OperationDelete
	if !ProvisionPlanMatches(p, deletePlan) {
		t.Fatal("matching distinct delete plan rejected")
	}
	deletePlan.Tags = map[string]string{"service": "other", "owner": p.OwnerDigest}
	if ProvisionPlanMatches(p, deletePlan) {
		t.Fatal("profile mismatch accepted")
	}
}

func TestCreateProvisionBindsRealEC2BuilderSnapshotAndRejectsForgedFields(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	s := NewService(repo, &testConfirm{}, testTasks{}, nil, NewFakeProvider(), time.Now)
	credential, err := saveVerifiedCredential(t, s, repo, CredentialInput{Name: "builder", Region: "us-east-1", AccessKeyID: "a", SecretAccessKey: "b", IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	request := validEC2ProvisionRequest()
	request.CredentialID, request.CredentialRevision = credential.ID, credential.Revision
	built, err := BuildEC2ProvisionPlan(request)
	if err != nil {
		t.Fatal(err)
	}
	view, err := s.CreatePlan(ctx, built.PlanInput(uuid.NewString()))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := repo.GetPlan(ctx, view.ID)
	if err != nil {
		t.Fatal(err)
	}
	valid := provisionFromPlan(plan, uuid.NewString())
	if _, err := repo.CreateProvision(ctx, valid); err != nil {
		t.Fatalf("real builder snapshot rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Provision){
		"owner":       func(v *Provision) { v.OwnerDigest = OwnerBindingDigest("other") },
		"plan_digest": func(v *Provision) { v.PlanDigest = PlanDigest(Plan{ID: uuid.NewString()}) },
		"template": func(v *Provision) {
			v.TemplateSHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		},
		"profile":    func(v *Provision) { v.Profile = "other" },
		"credential": func(v *Provision) { v.CredentialRevision++ },
	} {
		t.Run(name, func(t *testing.T) {
			forged := valid
			forged.ID = uuid.NewString()
			mutate(&forged)
			if _, err := repo.CreateProvision(ctx, forged); !errors.Is(err, ErrRevisionConflict) {
				t.Fatalf("forged snapshot error = %v, want ErrRevisionConflict", err)
			}
		})
	}
}

func TestRetryProvisionIsExplicitRevisionFencedAndReplayStable(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	s := NewService(repo, &testConfirm{}, testTasks{}, nil, NewFakeProvider(), time.Now)
	credential, err := saveVerifiedCredential(t, s, repo, CredentialInput{Name: "retry", Region: "us-east-1", AccessKeyID: "a", SecretAccessKey: "b", IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	request := validEC2ProvisionRequest()
	request.CredentialID, request.CredentialRevision = credential.ID, credential.Revision
	built, err := BuildEC2ProvisionPlan(request)
	if err != nil {
		t.Fatal(err)
	}
	view, err := s.CreatePlan(ctx, built.PlanInput(uuid.NewString()))
	if err != nil {
		t.Fatal(err)
	}
	plan, _ := repo.GetPlan(ctx, view.ID)
	p := provisionFromPlan(plan, uuid.NewString())
	if _, err = repo.CreateProvision(ctx, p); err != nil {
		t.Fatal(err)
	}
	setTerminal := func(state string, operation Operation, readback ProvisionReadback) Provision {
		repo.mu.Lock()
		defer repo.mu.Unlock()
		current := repo.provisions[p.ID]
		current.State, current.Revision, current.Readback = state, current.Revision+1, readback
		changeID := uuid.NewString()
		if operation == OperationDelete {
			// Model the successful create that must precede a delete intent; a
			// stale successful create reference must not count as failed.
			if current.CreateChangeID != "" {
				prior := repo.changes[current.CreateChangeID]
				prior.Status, prior.Operation = ChangeSucceeded, OperationCreate
				repo.changes[current.CreateChangeID] = prior
			}
			current.DestroyChangeID = changeID
		} else {
			current.CreateChangeID = changeID
		}
		repo.changes[changeID] = Change{ID: changeID, ProvisionID: p.ID, Operation: operation, Status: ChangeFailed}
		repo.provisions[p.ID] = current
		return current
	}
	failedCreate := setTerminal("failed", OperationCreate, ProvisionReadback{})
	retried, err := s.RetryProvision(ctx, p.ID, failedCreate.Revision, uuid.NewString())
	if err != nil || retried.State != "planned" || retried.Revision != failedCreate.Revision+1 || retried.Readback != (ProvisionReadback{}) {
		t.Fatalf("failed create retry: %#v %v", retried, err)
	}
	validReadback, _ := ProvisionReadbackFromStack(StackOutputs{"StackId": "stack", "InstanceId": "i-1", "SecurityGroupId": "sg-1"}, time.Now().UTC())
	failedDelete := setTerminal("failed", OperationDelete, validReadback)
	key := uuid.NewString()
	first, err := s.RetryProvision(ctx, p.ID, failedDelete.Revision, key)
	if err != nil || first.State != "active" || first.Readback != validReadback {
		t.Fatalf("failed delete retry: %#v %v", first, err)
	}
	second, err := s.RetryProvision(ctx, p.ID, failedDelete.Revision, key)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("retry replay mismatch: first=%#v second=%#v err=%v", first, second, err)
	}
	destroyed := setTerminal("destroyed", OperationDelete, validReadback)
	recreated, err := s.RetryProvision(ctx, p.ID, destroyed.Revision, uuid.NewString())
	if err != nil || recreated.State != "planned" || recreated.Readback != (ProvisionReadback{}) {
		t.Fatalf("destroyed recreate retry: %#v %v", recreated, err)
	}
	if _, err := s.RetryProvision(ctx, p.ID, destroyed.Revision-1, uuid.NewString()); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale retry revision = %v", err)
	}
	if events := repo.Events(); len(events) < 2 {
		t.Fatalf("retry audit events missing: %#v", events)
	}
}

func TestRetryProvisionRecreateIgnoresStaleSuccessfulDestroyIntent(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	s := NewService(repo, &testConfirm{}, testTasks{}, nil, NewFakeProvider(), time.Now)
	credential, err := saveVerifiedCredential(t, s, repo, CredentialInput{Name: "lifecycle", Region: "us-east-1", AccessKeyID: "a", SecretAccessKey: "b", IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	request := validEC2ProvisionRequest()
	request.CredentialID, request.CredentialRevision = credential.ID, credential.Revision
	built, err := BuildEC2ProvisionPlan(request)
	if err != nil {
		t.Fatal(err)
	}
	view, err := s.CreatePlan(ctx, built.PlanInput(uuid.NewString()))
	if err != nil {
		t.Fatal(err)
	}
	plan, _ := repo.GetPlan(ctx, view.ID)
	p := provisionFromPlan(plan, uuid.NewString())
	if _, err := repo.CreateProvision(ctx, p); err != nil {
		t.Fatal(err)
	}
	repo.mu.Lock()
	createID, destroyID := uuid.NewString(), uuid.NewString()
	p = repo.provisions[p.ID]
	p.State, p.Revision, p.CreateChangeID, p.DestroyChangeID = "destroyed", 4, createID, destroyID
	repo.provisions[p.ID] = p
	repo.changes[createID] = Change{ID: createID, ProvisionID: p.ID, Operation: OperationCreate, Status: ChangeSucceeded}
	repo.changes[destroyID] = Change{ID: destroyID, ProvisionID: p.ID, Operation: OperationDelete, Status: ChangeSucceeded}
	repo.mu.Unlock()
	retried, err := s.RetryProvision(ctx, p.ID, p.Revision, uuid.NewString())
	if err != nil || retried.State != "planned" {
		t.Fatalf("destroy retry: %#v %v", retried, err)
	}
	repo.mu.Lock()
	p = repo.provisions[p.ID]
	failedCreateID := uuid.NewString()
	p.State, p.Revision, p.CreateChangeID = "failed", p.Revision+1, failedCreateID
	repo.provisions[p.ID] = p
	repo.changes[failedCreateID] = Change{ID: failedCreateID, ProvisionID: p.ID, Operation: OperationCreate, Status: ChangeFailed}
	repo.mu.Unlock()
	// A failed change ID owned by another provision is not a valid retry
	// intent, even when the operation/status look otherwise plausible.
	repo.mu.Lock()
	wrongID := uuid.NewString()
	p = repo.provisions[p.ID]
	p.CreateChangeID = wrongID
	repo.provisions[p.ID] = p
	repo.changes[wrongID] = Change{ID: wrongID, ProvisionID: uuid.NewString(), Operation: OperationCreate, Status: ChangeFailed}
	repo.mu.Unlock()
	if _, err := s.RetryProvision(ctx, p.ID, p.Revision, uuid.NewString()); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("cross-provision failed intent accepted: %v", err)
	}
	repo.mu.Lock()
	p = repo.provisions[p.ID]
	p.CreateChangeID = failedCreateID
	repo.provisions[p.ID] = p
	repo.mu.Unlock()
	key := uuid.NewString()
	first, err := s.RetryProvision(ctx, p.ID, p.Revision, key)
	if err != nil || first.State != "planned" {
		t.Fatalf("recreate after failed create: %#v %v", first, err)
	}
	second, err := s.RetryProvision(ctx, p.ID, p.Revision, key)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("recreate retry replay mismatch: first=%#v second=%#v err=%v", first, second, err)
	}
}
