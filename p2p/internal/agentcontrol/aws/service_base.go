package aws

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	"github.com/google/uuid"
)

type Task struct {
	ID             string
	Status         string
	Revision       int64
	PlanID         string
	ConfirmationID string
	Attempt        uint32
	LeaseEpoch     uint64
	FailureCode    string
	FailureSummary string
}
type TaskCreateRequest struct{ Goal, PlanID, IdempotencyKey string }
type TaskPort interface {
	CreateWaitingUser(context.Context, TaskCreateRequest) (Task, error)
	Queue(context.Context, string) error
	Fail(context.Context, string, string, string) error
}
type ConfirmationPort interface {
	Request(context.Context, coreconfirmation.RequestCommand) (coreconfirmation.Confirmation, error)
	Get(context.Context, string) (coreconfirmation.Confirmation, error)
}

// ChangeCoordinator is the atomic durable boundary used by production
// adapters. Implementations commit confirmation consumption, Task fencing,
// change cursor/terminal state, target updates and reservation release in one
// transaction.
type ChangeCoordinator interface {
	RequestChange(context.Context, RequestChangeInput) (ChangeRequestResult, error)
	ConsumeChange(context.Context, ConsumeChangeCommand) (Reservation, error)
	CompleteChange(context.Context, CompleteChangeCommand) (Change, error)
	ExecutionFence(context.Context, string) (ExecutionFence, error)
	ReconcileChange(context.Context, ReconcileChangeCommand) (Change, error)
	ClaimProviderMutation(context.Context, ProviderMutationCommand) (ExecutionFence, error)
	CommitProviderMutation(context.Context, ProviderMutationResult) (Change, error)
	PersistChangeSetEvidence(context.Context, ChangeSetEvidenceCommand) (Change, error)
}
type ConsumeChangeCommand struct {
	ChangeID, ConfirmationID, TaskID, IdempotencyKey                           string
	Attempt                                                                    uint32
	LeaseEpoch                                                                 uint64
	ExpectedChangeRevision, ExpectedConfirmationRevision, ExpectedTaskRevision int64
	Binding                                                                    coreconfirmation.Binding
}
type Reservation struct {
	ConfirmationID, TaskID string
	Attempt                uint32
	LeaseEpoch             uint64
	TaskRevision           int64
	Active                 bool
}
type ExecutionFence struct {
	Change       Change
	Task         Task
	Confirmation coreconfirmation.Confirmation
	Reservation  Reservation
}
type ReconcileChangeCommand struct {
	ChangeID, ConfirmationID, TaskID                                           string
	Attempt                                                                    uint32
	LeaseEpoch                                                                 uint64
	ExpectedChangeRevision, ExpectedTaskRevision, ExpectedConfirmationRevision int64
	ProviderChangeSetID, ProviderToken, ProviderRequestDigest                  string
	Success                                                                    bool
	ErrorCode, ErrorSummary                                                    string
}
type ProviderMutationKind string

const (
	ProviderMutationCreate  ProviderMutationKind = "create_change_set"
	ProviderMutationExecute ProviderMutationKind = "execute_change_set"
	ProviderMutationDelete  ProviderMutationKind = "delete_stack"
)

type ProviderMutationCommand struct {
	ChangeID, ConfirmationID, TaskID                                           string
	Attempt                                                                    uint32
	LeaseEpoch                                                                 uint64
	ExpectedChangeRevision, ExpectedTaskRevision, ExpectedConfirmationRevision int64
	Kind                                                                       ProviderMutationKind
	ProviderChangeSetID                                                        string
	OperationKey                                                               string
}
type ProviderMutationResult struct {
	Command                 ProviderMutationCommand
	Success                 bool
	ResponseUncertain       bool
	ProviderChangeSetID     string
	ErrorCode, ErrorSummary string
}
type ChangeSetEvidenceCommand struct {
	ChangeID, ConfirmationID, TaskID, ProviderChangeSetID                      string
	Attempt                                                                    uint32
	LeaseEpoch                                                                 uint64
	ExpectedChangeRevision, ExpectedTaskRevision, ExpectedConfirmationRevision int64
}
type CompleteChangeCommand struct {
	ChangeID, ConfirmationID, TaskID             string
	Attempt                                      uint32
	LeaseEpoch                                   uint64
	ExpectedTaskRevision, ExpectedChangeRevision int64
	ExpectedConfirmationRevision                 int64
	Status                                       ChangeStatus
	ErrorCode, ErrorSummary                      string
	OperationKey                                 string
	Readback                                     *ProvisionReadback
}

// operationKey identifies one provider-side action.  It deliberately excludes
// worker attempt and lease epoch: a reclaimed lease must reissue the exact
// same idempotent AWS request after a crash between claim and call.
func operationKey(changeID, token, kind string, _ uint32, _ uint64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s", changeID, token, kind)))
	return uuid.NewSHA1(uuid.NameSpaceOID, sum[:]).String()
}

type Service struct {
	repo          Repository
	confirmations ConfirmationPort
	tasks         TaskPort
	sts           STSProvider
	provider      CloudProvider
	coordinator   ChangeCoordinator
	now           func() time.Time
}

// ReadyForEmbedded reports whether the service has a durable repository and
// atomic change coordinator. The embedded action facade uses this as a
// fail-closed readiness check before publishing aws.control.
func (s *Service) ReadyForEmbedded() bool {
	return s != nil && s.repo != nil && s.coordinator != nil && s.sts != nil && s.provider != nil
}

func NewService(repo Repository, confirmations ConfirmationPort, tasks TaskPort, sts STSProvider, provider CloudProvider, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	s := &Service{repo: repo, confirmations: confirmations, tasks: tasks, sts: sts, provider: provider, now: now}
	if mr, ok := repo.(*MemoryRepository); ok {
		s.coordinator = NewMemoryChangeCoordinator(mr, confirmations, tasks, now)
	}
	return s
}

// NewServiceWithCoordinator is the explicit constructor used by production
// wiring. Lifecycle state is never assembled from independent service calls.
func NewServiceWithCoordinator(repo Repository, coordinator ChangeCoordinator, confirmations ConfirmationPort, tasks TaskPort, sts STSProvider, provider CloudProvider, now func() time.Time) *Service {
	s := NewService(repo, confirmations, tasks, sts, provider, now)
	s.coordinator = coordinator
	return s
}

func bindingForPlan(p Plan, c Credentials) coreconfirmation.Binding {
	param := canonicalDigest(struct {
		Parameters, Tags   map[string]string
		Capabilities       []string
		CredentialRevision int64
		AccountID, UserARN string
	}{p.Parameters, p.Tags, p.Capabilities, c.Revision, c.AccountID, c.UserARN})
	secret := canonicalDigest([]string{c.ID})
	return coreconfirmation.Binding{OperationDomain: "aws", TargetID: canonicalTargetKey(c.AccountID, p.Region, p.StackName), TargetRevision: p.Revision, SourceVersion: "core-v1", ContentDigest: coreconfirmation.Digest(p.TemplateSHA256), ParameterDigest: coreconfirmation.Digest(param), NetworkDigest: coreconfirmation.Digest(canonicalDigest([]string{})), SecretGrantDigest: coreconfirmation.Digest(secret), SecretGrants: []coreconfirmation.SecretGrant{{ReferenceID: c.ID, Purpose: coreconfirmation.SecretPurposeAWSCredential, Revision: c.Revision, BindingDigest: coreconfirmation.Digest(secret)}}}
}

func bindingForPlanOwner(p Plan, c Credentials, ownerID string) coreconfirmation.Binding {
	binding := bindingForPlan(p, c)
	binding.OwnerID = strings.TrimSpace(ownerID)
	return binding
}

// BindingForPlan is the canonical immutable AWS confirmation binding builder.
func BindingForPlan(p Plan, c Credentials) coreconfirmation.Binding { return bindingForPlan(p, c) }

func canonicalTargetKey(account, region, stack string) string {
	return "aws-target:" + canonicalDigest(struct{ Account, Region, Stack string }{strings.TrimSpace(account), strings.TrimSpace(region), strings.TrimSpace(stack)})
}

func providerRequestDigest(p Plan, token string) string {
	return canonicalDigest(struct {
		Region, Stack, Name string
		Operation           Operation
		Template            []byte
		Parameters, Tags    map[string]string
		Capabilities        []string
	}{p.Region, p.StackName, token, p.Operation, p.Template, p.Parameters, p.Tags, p.Capabilities})
}

// ProviderRequestDigest returns the immutable digest used for typed provider
// idempotency and response binding.
func ProviderRequestDigest(p Plan, token string) string { return providerRequestDigest(p, token) }

func newUUID() string { return uuid.New().String() }
