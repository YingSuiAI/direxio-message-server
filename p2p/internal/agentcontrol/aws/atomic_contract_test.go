package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"log/slog"
	"strings"
	"testing"
)

func TestCredentialInputRedactionAndDigest(t *testing.T) {
	in := CredentialInput{ID: uuid.NewString(), Name: "prod", Region: "us-east-1", AccessKeyID: "AKIA", SecretAccessKey: "super-secret", SessionToken: "token", IdempotencyKey: uuid.NewString()}
	b, _ := json.Marshal(in)
	out := string(b) + fmt.Sprintf("%v %#+v", in, in)
	var log strings.Builder
	slog.New(slog.NewTextHandler(&log, nil)).Info("credential", "input", in)
	out += log.String()
	if strings.Contains(out, "super-secret") || strings.Contains(out, `"token"`) || strings.Contains(credentialInputDigest(in), "super-secret") {
		t.Fatalf("credential leaked: %s", out)
	}
}

func TestCredentialIdentityBindsRevisionAndReplacementInvalidates(t *testing.T) {
	r := NewMemoryRepository()
	sts := &FakeSTSProvider{Identity: Identity{AccountID: "123456789012", UserARN: "arn:aws:iam::123456789012:user/test"}}
	s := NewService(r, nil, nil, sts, nil, nil)
	view, err := s.SaveCredential(context.Background(), CredentialInput{Name: "prod", Region: "us-east-1", AccessKeyID: "a", SecretAccessKey: "b", IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	checked, err := s.TestCredential(context.Background(), view.ID)
	if err != nil || checked.CredentialRevision != 1 {
		t.Fatalf("test=%#v err=%v", checked, err)
	}
	updated, err := s.ReplaceCredential(context.Background(), CredentialInput{ID: view.ID, Name: "prod2", Region: "us-east-1", AccessKeyID: "a", SecretAccessKey: "b"}, 1, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if updated.AccountID != "" || updated.UserARN != "" {
		t.Fatal("identity survived replacement")
	}
}

func TestAtomicFullWorkflowUsesSingleProviderToken(t *testing.T) {
	s, repo, provider, plan := workflowFixture(t)
	requested, err := s.RequestChange(context.Background(), RequestChangeInput{PlanID: plan.ID, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	if external := s.confirmations.(*testConfirm); external.c.ConfirmationID != "" {
		t.Fatal("request delegated confirmation side effect")
	}
	confirmed := confirmAWSMemory(t, repo, requested)
	if err := s.coordinator.(*MemoryChangeCoordinator).SetTaskRunning(requested.Task.ID, 1, 1, confirmed.Task.Revision); err != nil {
		t.Fatal(err)
	}
	task, _ := repo.GetTask(context.Background(), requested.Task.ID)
	reservation, err := s.ConsumeChange(context.Background(), ConsumeChangeCommand{ChangeID: requested.Change.ID, ConfirmationID: requested.Confirmation.ConfirmationID, TaskID: requested.Task.ID, IdempotencyKey: uuid.NewString(), Attempt: 1, LeaseEpoch: 1, ExpectedChangeRevision: confirmed.Change.Revision, ExpectedConfirmationRevision: confirmed.Confirmation.Revision, ExpectedTaskRevision: task.Revision, Binding: requested.Confirmation.Binding})
	if err != nil || !reservation.Active {
		t.Fatalf("consume: %#v %v", reservation, err)
	}
	completed, err := s.ExecuteChange(context.Background(), requested.Confirmation.ConfirmationID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != ChangeSucceeded || len(provider.Calls) < 2 {
		t.Fatalf("completion=%#v calls=%v", completed, provider.Calls)
	}
	finalReservation, _ := repo.GetReservation(context.Background(), requested.Confirmation.ConfirmationID)
	if finalReservation.Active {
		t.Fatal("reservation not released")
	}
}

func TestExecuteChangeStrictAcceptsAuthoritativeOwnerBinding(t *testing.T) {
	s, repo, provider, plan := workflowFixture(t)
	requested, err := s.RequestChange(context.Background(), RequestChangeInput{PlanID: plan.ID, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	const ownerID = "@owner:example.test"
	repo.mu.Lock()
	confirmation := repo.confirmations[requested.Confirmation.ConfirmationID]
	confirmation.OwnerID = ownerID
	confirmation.Binding.OwnerID = ownerID
	repo.confirmations[confirmation.ConfirmationID] = confirmation
	repo.mu.Unlock()
	requested.Confirmation = confirmation
	consumed := consumeWorkflowChange(t, s, repo, requested)

	completed, err := s.ExecuteChange(context.Background(), consumed.Confirmation.ConfirmationID)
	if err != nil {
		t.Fatalf("owner-bound ExecuteChange = %v", err)
	}
	if completed.Status != ChangeSucceeded || len(provider.Calls) < 2 {
		t.Fatalf("owner-bound completion=%#v calls=%v", completed, provider.Calls)
	}
}
