package aws

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	"github.com/google/uuid"
)

func TestCredentialViewRedactsSecrets(t *testing.T) {
	c := Credentials{ID: "11111111-1111-4111-8111-111111111111", Name: "prod", Region: "us-east-1", private: &credentialPayload{accessKeyID: "AKIA", secretAccessKey: "secret", sessionToken: "token"}, Revision: 1}
	v := c.View()
	if !v.HasAccessKey || !v.HasSecretKey || !v.HasSessionToken {
		t.Fatal("configured flags missing")
	}
	b, _ := json.Marshal(c)
	out := string(b) + fmt.Sprintf("%v %#+v %v", c, c, c)
	if bytes.Contains([]byte(out), []byte("secret")) || bytes.Contains([]byte(out), []byte("token")) {
		t.Fatalf("secret leaked: %s", out)
	}
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("credential", "value", c)
	if bytes.Contains(buf.Bytes(), []byte("secret")) {
		t.Fatal("slog leaked secret")
	}
	if d := canonicalDigest(c); strings.Contains(d, "secret") {
		t.Fatal("digest leaked secret")
	}
}

func TestPlanPinsCredentialRevisionAndDeleteTombstonesCurrentProjection(t *testing.T) {
	ctx := context.Background()
	r := NewMemoryRepository()
	s := NewService(r, &testConfirm{}, testTasks{}, nil, NewFakeProvider(), time.Now)
	created, err := s.SaveCredential(ctx, CredentialInput{Name: "rev-one", Region: "us-east-1", AccessKeyID: "old-access", SecretAccessKey: "old-secret", IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.CreatePlan(ctx, PlanInput{CredentialID: created.ID, StackName: "pinned-plan", Operation: OperationCreate, Template: []byte(`{"Resources":{}}`), IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := r.GetPlan(ctx, plan.ID); err != nil || got.CredentialRevision != 1 {
		t.Fatalf("plan pin: %#v %v", got, err)
	}
	updated, err := s.ReplaceCredential(ctx, CredentialInput{ID: created.ID, Name: "rev-two", Region: "us-west-2", AccessKeyID: "new-access", SecretAccessKey: "new-secret"}, 1, uuid.NewString())
	if err != nil || updated.Revision != 2 {
		t.Fatalf("update: %#v %v", updated, err)
	}
	old, err := r.GetCredentialRevision(ctx, created.ID, 1)
	if err != nil || old.Region != "us-east-1" {
		t.Fatalf("historical revision lost: %#v %v", old, err)
	}
	if got := BindingForPlan(mustPlan(t, r, plan.ID), old); got.TargetID == "" {
		t.Fatal("old plan no longer binds its exact historical credential")
	}
	// The new revision deliberately changes region. Successful execution proves
	// both mutation and reconciliation used the plan's rev1, not current rev2.
	requested, err := s.RequestChange(ctx, RequestChangeInput{PlanID: plan.ID, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	consumeWorkflowChange(t, s, r, requested)
	if _, err = s.ExecuteChange(ctx, requested.Confirmation.ConfirmationID); err != nil {
		t.Fatalf("old pinned plan executed with current credential instead of rev1: %v", err)
	}
	page, err := s.ListCredentials(ctx, 10, "")
	if err != nil || len(page.Items) != 1 || page.Items[0].Revision != 2 {
		t.Fatalf("current projection: %#v %v", page, err)
	}
	if err = s.DeleteCredential(ctx, created.ID, 2, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.GetCredential(ctx, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted credential resurrected: %v", err)
	}
	page, err = s.ListCredentials(ctx, 10, "")
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("deleted credential remains listed: %#v %v", page, err)
	}
	if old, err = r.GetCredentialRevision(ctx, created.ID, 1); err != nil || old.Region != "us-east-1" {
		t.Fatalf("pinned history unavailable after delete: %#v %v", old, err)
	}
	if _, err = s.SaveCredential(ctx, CredentialInput{ID: created.ID, Name: "resurrect", Region: "us-east-1", AccessKeyID: "x", SecretAccessKey: "y", IdempotencyKey: uuid.NewString()}); !errors.Is(err, ErrConflict) {
		t.Fatalf("tombstone allowed resurrection: %v", err)
	}
}

func TestMemoryCredentialListDefaultPageAndCursor(t *testing.T) {
	ctx := context.Background()
	r := NewMemoryRepository()
	for i := 0; i < 26; i++ {
		id := uuid.NewString()
		c := RehydrateCredentials(id, fmt.Sprintf("credential-%02d", i), "us-east-1", "", "", []byte("access"), []byte("secret"), nil, 0, 1, time.Unix(int64(i), 0).UTC(), time.Unix(int64(i), 0).UTC())
		if _, err := r.CreateCredential(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	first, err := r.ListCredentials(ctx, 0, "")
	if err != nil || len(first.Items) != 25 || first.NextPageToken == "" {
		t.Fatalf("default credential page = %#v, err=%v", first, err)
	}
	second, err := r.ListCredentials(ctx, 0, first.NextPageToken)
	if err != nil || len(second.Items) != 1 || second.NextPageToken != "" {
		t.Fatalf("cursor credential page = %#v, err=%v", second, err)
	}
}

func mustPlan(t *testing.T, r *MemoryRepository, id string) Plan {
	t.Helper()
	p, err := r.GetPlan(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFakeProviderTypedIdempotencyAndProgress(t *testing.T) {
	f := NewFakeProvider()
	req := ChangeSetRequest{Region: "us-east-1", StackName: "demo", ChangeSetName: "c1", ClientToken: "11111111-1111-4111-8111-111111111111", Operation: OperationCreate, Template: []byte("{}")}
	cs, e := f.CreateChangeSet(context.Background(), CredentialHandle{Region: req.Region}, req)
	if e != nil {
		t.Fatal(e)
	}
	replay, e := f.CreateChangeSet(context.Background(), CredentialHandle{Region: req.Region}, req)
	if e != nil || replay.ID != cs.ID {
		t.Fatalf("replay: %v", e)
	}
	if e = f.ExecuteChangeSet(context.Background(), CredentialHandle{Region: req.Region}, req.Region, req.StackName, cs.ID, req.ClientToken); e != nil {
		t.Fatal(e)
	}
	s, e := f.DescribeStack(context.Background(), CredentialHandle{Region: req.Region}, req.Region, req.StackName)
	if e != nil || s.Status != "CREATE_COMPLETE" {
		t.Fatalf("stack: %#v %v", s, e)
	}
	changed := req
	changed.Template = []byte("{\"changed\":true}")
	if _, e = f.CreateChangeSet(context.Background(), CredentialHandle{Region: changed.Region}, changed); !errors.Is(e, ErrIdempotencyConflict) {
		t.Fatalf("changed token scope accepted: %v", e)
	}
}

type testConfirm struct{ c coreconfirmation.Confirmation }

func (x *testConfirm) Request(_ context.Context, cmd coreconfirmation.RequestCommand) (coreconfirmation.Confirmation, error) {
	x.c = coreconfirmation.Confirmation{ConfirmationID: "22222222-2222-4222-8222-222222222222", Binding: cmd.Binding, TaskID: cmd.TaskID, State: coreconfirmation.StatePending, Revision: 1}
	return x.c, nil
}
func (x *testConfirm) Get(_ context.Context, id string) (coreconfirmation.Confirmation, error) {
	if id != x.c.ConfirmationID {
		return coreconfirmation.Confirmation{}, ErrNotFound
	}
	return x.c, nil
}

type testTasks struct{}

func (testTasks) CreateWaitingUser(_ context.Context, r TaskCreateRequest) (Task, error) {
	return Task{ID: "33333333-3333-4333-8333-333333333333", Status: "waiting_user", Revision: 1, PlanID: r.PlanID}, nil
}
func (testTasks) Queue(context.Context, string) error                { return nil }
func (testTasks) Fail(context.Context, string, string, string) error { return nil }

func TestServiceRejectsUnconfirmedMutation(t *testing.T) {
	r := NewMemoryRepository()
	now := func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	s := NewService(r, &testConfirm{}, testTasks{}, nil, NewFakeProvider(), now)
	cred, _ := s.SaveCredential(context.Background(), CredentialInput{Name: "x", Region: "us-east-1", AccessKeyID: "a", SecretAccessKey: "b"})
	p, _ := s.CreatePlan(context.Background(), PlanInput{CredentialID: cred.ID, StackName: "demo", Operation: OperationCreate, Template: []byte(`{"Resources":{"X":{"Type":"AWS::S3::Bucket"}}}`)})
	out, e := s.RequestChange(context.Background(), RequestChangeInput{PlanID: p.ID, IdempotencyKey: "44444444-4444-4444-8444-444444444444"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.ExecuteChange(context.Background(), out.Confirmation.ConfirmationID); !errors.Is(e, ErrUnconfirmed) {
		t.Fatalf("got %v", e)
	}
	if n := s.provider.(*FakeProvider).UnconfirmedMutationCalls(); n != 0 {
		t.Fatalf("unconfirmed calls=%d", n)
	}
}

func TestRequestChangeConcurrentReplay(t *testing.T) {
	r := NewMemoryRepository()
	now := func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	conf := &testConfirm{}
	s := NewService(r, conf, testTasks{}, nil, NewFakeProvider(), now)
	cred, _ := s.SaveCredential(context.Background(), CredentialInput{Name: "x", Region: "us-east-1", AccessKeyID: "a", SecretAccessKey: "b"})
	p, _ := s.CreatePlan(context.Background(), PlanInput{CredentialID: cred.ID, StackName: "demo", Operation: OperationCreate, Template: []byte(`{"Resources":{"X":{"Type":"AWS::S3::Bucket"}}}`)})
	input := RequestChangeInput{PlanID: p.ID, IdempotencyKey: "55555555-5555-4555-8555-555555555555"}
	var wg sync.WaitGroup
	results := make([]ChangeRequestResult, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results[i], errs[i] = s.RequestChange(context.Background(), input) }(i)
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || results[0].Confirmation.ConfirmationID != results[1].Confirmation.ConfirmationID || results[0].Change.ID != results[1].Change.ID {
		t.Fatalf("replay mismatch: %#v %#v", errs, results)
	}
}
