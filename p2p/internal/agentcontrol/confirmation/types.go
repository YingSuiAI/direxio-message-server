// Package coreconfirmation owns the shared, single-user confirmation contract
// used by MCP, Skill and AWS operations. It deliberately stores only binding
// facts and digests; secret bytes and credentials never cross this boundary.
package confirmation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	coretask "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
	"github.com/google/uuid"
)

type State string

// TaskTerminalCommand is the narrow hook used by the task ledger after it has
// committed cancellation or timeout.  Pending/confirmed confirmations can be
// compensated immediately; consumed work must remain fenced for reconciliation
// and must never be represented as a stopped provider operation.
type TaskTerminalCommand struct {
	TaskID string
	Reason string
	At     time.Time
}

const (
	StatePending   State = "pending"
	StateConfirmed State = "confirmed"
	StateConsumed  State = "consumed"
	StateRejected  State = "rejected"
	StateExpired   State = "expired"
)

type ExtensionUncertainResolution string

const ExtensionUncertainAcknowledgedUnknownNoRetry ExtensionUncertainResolution = "acknowledged_unknown_no_retry"

// AcknowledgeExtensionExecutionUncertainCommand is the owner-only, explicit
// acknowledgement for a consumed extension execution whose provider outcome
// cannot be proven. It never retries or dispatches work.
type AcknowledgeExtensionExecutionUncertainCommand struct {
	ConfirmationID               string
	TaskID                       string
	InstallationID               string
	ExpectedTaskRevision         int64
	ExpectedConfirmationRevision int64
	Resolution                   ExtensionUncertainResolution
	IdempotencyKey               string
}

type AcknowledgeExtensionExecutionUncertainResult struct {
	Confirmation        Confirmation
	Task                coretask.Task
	Resolution          ExtensionUncertainResolution
	ReservationReleased bool
}

// ExtensionUncertainAcknowledger is optional so deployments without the
// extension execution store do not accidentally advertise reconciliation.
type ExtensionUncertainAcknowledger interface {
	AcknowledgeExtensionExecutionUncertain(context.Context, AcknowledgeExtensionExecutionUncertainCommand) (AcknowledgeExtensionExecutionUncertainResult, error)
}

const (
	ReasonUserRejected = "user_rejected"
	ReasonExpired      = "confirmation_expired"
	ReasonStale        = "confirmation_stale"
)

var (
	ErrInvalid             = errors.New("invalid confirmation")
	ErrNotFound            = errors.New("confirmation not found")
	ErrConflict            = errors.New("confirmation conflict")
	ErrRevisionConflict    = errors.New("confirmation revision conflict")
	ErrIdempotencyConflict = errors.New("confirmation idempotency conflict")
	ErrStale               = errors.New("confirmation binding is stale")
	ErrExpired             = errors.New("confirmation expired")
	ErrInvalidTransition   = errors.New("invalid confirmation transition")
	ErrTaskFenceConflict   = errors.New("confirmation task fence conflict")
	ErrBindingUnavailable  = errors.New("authoritative confirmation binding unavailable")
)

// Digest is a lowercase SHA-256 hex digest. Raw content is never retained.
type Digest string

func (d Digest) Valid() bool {
	if len(d) != sha256.Size*2 || strings.ToLower(string(d)) != string(d) {
		return false
	}
	_, err := hex.DecodeString(string(d))
	return err == nil
}

// Binding is the immutable operation snapshot revalidated before confirmation
// and consumption.
type Binding struct {
	Digest string `json:"digest,omitempty"`
	// OwnerID identifies the single Agent owner/instance that authorized this
	// operation. It is a stable identity descriptor, never a secret.
	OwnerID           string
	OperationDomain   string
	TargetID          string
	TargetRevision    int64
	TargetKind        string
	SourceVersion     string
	SourceCommit      string
	ContentDigest     Digest
	ManifestDigest    Digest
	ExecutionDigest   Digest
	PermissionDigest  Digest
	ParameterDigest   Digest
	NetworkDigest     Digest
	SecretGrantDigest Digest
	// Execution V2 binds a user decision to one immutable plan/run/stage and
	// target snapshot. These fields remain empty for legacy confirmation
	// domains and are mandatory for operation domains prefixed execution:v2.
	PlanID              string
	PlanRevision        int64
	PlanDigest          Digest
	DeploymentID        string
	RunID               string
	RunRevision         int64
	StageID             string
	StageRevision       int64
	StageDigest         Digest
	TargetDigest        Digest
	ArtifactSetDigest   Digest
	PolicyDigest        Digest
	CostQuoteDigest     Digest
	RollbackDigest      Digest
	PreviewDigest       Digest
	RiskLevel           string
	GateType            string
	StageIdempotencyKey string
	BindingExpiresAt    time.Time
	SelectedTool        string
	SelectedCommand     []string
	NetworkGrants       []string
	SecretGrants        []SecretGrant
}

// IsZero reports whether no binding expectation was supplied. Keep this
// centralized as the binding evolves so omitted fields cannot bypass an exact
// comparison at a persistence or confirmation boundary.
func (b Binding) IsZero() bool {
	return b.Digest == "" && b.OwnerID == "" && b.OperationDomain == "" && b.TargetID == "" &&
		b.TargetRevision == 0 && b.TargetKind == "" && b.SourceVersion == "" && b.SourceCommit == "" &&
		b.ContentDigest == "" && b.ManifestDigest == "" && b.ExecutionDigest == "" && b.PermissionDigest == "" &&
		b.ParameterDigest == "" && b.NetworkDigest == "" && b.SecretGrantDigest == "" &&
		b.PlanID == "" && b.PlanRevision == 0 && b.PlanDigest == "" && b.DeploymentID == "" &&
		b.RunID == "" && b.RunRevision == 0 && b.StageID == "" && b.StageRevision == 0 &&
		b.StageDigest == "" && b.TargetDigest == "" && b.ArtifactSetDigest == "" &&
		b.PolicyDigest == "" && b.CostQuoteDigest == "" && b.RollbackDigest == "" &&
		b.PreviewDigest == "" && b.RiskLevel == "" && b.GateType == "" &&
		b.StageIdempotencyKey == "" && b.BindingExpiresAt.IsZero() &&
		b.SelectedTool == "" &&
		len(b.SelectedCommand) == 0 && len(b.NetworkGrants) == 0 && len(b.SecretGrants) == 0
}

// SecretGrant is a safe descriptor. It can identify an authorized secret but
// cannot represent the secret value itself.
type SecretGrant struct {
	ReferenceID string
	Purpose     SecretPurpose
	// Revision is required for AWS credential grants. It binds confirmation to
	// the immutable credential revision rather than the mutable current row.
	Revision      int64
	BindingDigest Digest
}

type SecretPurpose string

const (
	SecretPurposeModelAPIKey          SecretPurpose = "model_api_key"
	SecretPurposeMCPCredential        SecretPurpose = "mcp_credential"
	SecretPurposeSkillSecret          SecretPurpose = "skill_secret"
	SecretPurposeAWSCredential        SecretPurpose = "aws_credential"
	SecretPurposeOtherExtensionSecret SecretPurpose = "other_extension_secret"
)

func (b Binding) normalized() (Binding, error) {
	b.OwnerID = strings.TrimSpace(b.OwnerID)
	b.OperationDomain = strings.TrimSpace(b.OperationDomain)
	b.TargetID = strings.TrimSpace(b.TargetID)
	b.SourceVersion = strings.TrimSpace(b.SourceVersion)
	b.SourceCommit = strings.TrimSpace(b.SourceCommit)
	b.TargetKind = strings.TrimSpace(b.TargetKind)
	b.PlanID = strings.TrimSpace(b.PlanID)
	b.DeploymentID = strings.TrimSpace(b.DeploymentID)
	b.RunID = strings.TrimSpace(b.RunID)
	b.StageID = strings.TrimSpace(b.StageID)
	b.RiskLevel = strings.TrimSpace(b.RiskLevel)
	b.GateType = strings.TrimSpace(b.GateType)
	b.StageIdempotencyKey = strings.TrimSpace(b.StageIdempotencyKey)
	b.SelectedTool = strings.TrimSpace(b.SelectedTool)
	executionV2 := isExecutionV2Domain(b.OperationDomain)
	if strings.HasPrefix(b.OperationDomain, "execution:v2") && !executionV2 {
		return Binding{}, ErrInvalid
	}
	if b.OperationDomain == "" || b.TargetID == "" || b.TargetRevision < 1 ||
		(!executionV2 && b.SourceVersion == "" && b.SourceCommit == "") {
		return Binding{}, ErrInvalid
	}
	for _, d := range []Digest{b.ContentDigest, b.ParameterDigest, b.NetworkDigest, b.SecretGrantDigest} {
		if !d.Valid() {
			return Binding{}, ErrInvalid
		}
	}
	for _, d := range []Digest{b.ManifestDigest, b.ExecutionDigest, b.PermissionDigest} {
		if d != "" && !d.Valid() {
			return Binding{}, ErrInvalid
		}
	}
	for _, d := range []Digest{
		b.PlanDigest, b.StageDigest, b.TargetDigest, b.ArtifactSetDigest,
		b.PolicyDigest, b.CostQuoteDigest, b.RollbackDigest, b.PreviewDigest,
	} {
		if d != "" && !d.Valid() {
			return Binding{}, ErrInvalid
		}
	}
	if executionV2 {
		if !validBoundedIdentity(b.OwnerID) || !validBindingToken(b.TargetKind) ||
			!validateUUID(b.TargetID) ||
			!validateUUID(b.PlanID) || b.PlanRevision < 1 || !b.PlanDigest.Valid() ||
			!validateUUID(b.RunID) || b.RunRevision < 1 ||
			!validateUUID(b.StageID) || b.StageRevision < 1 || !b.StageDigest.Valid() ||
			!b.TargetDigest.Valid() || !b.ExecutionDigest.Valid() ||
			!b.ArtifactSetDigest.Valid() || !b.PolicyDigest.Valid() ||
			!b.CostQuoteDigest.Valid() || !b.RollbackDigest.Valid() ||
			!b.PreviewDigest.Valid() || b.BindingExpiresAt.IsZero() ||
			b.BindingExpiresAt.Location() != time.UTC ||
			!validateUUID(b.StageIdempotencyKey) ||
			!validExecutionV2RiskGate(b.RiskLevel, b.GateType) {
			return Binding{}, ErrInvalid
		}
		if strings.TrimPrefix(b.OperationDomain, "execution:v2:") != b.GateType ||
			b.SourceVersion != "" || b.SourceCommit != "" ||
			b.ManifestDigest != "" || b.PermissionDigest != "" {
			return Binding{}, ErrInvalid
		}
		if b.SelectedTool != "" || len(b.SelectedCommand) != 0 ||
			len(b.NetworkGrants) != 0 || len(b.SecretGrants) != 0 {
			return Binding{}, ErrInvalid
		}
		if b.DeploymentID != "" && !validateUUID(b.DeploymentID) {
			return Binding{}, ErrInvalid
		}
		b.BindingExpiresAt = b.BindingExpiresAt.UTC()
	}
	for _, command := range b.SelectedCommand {
		if command == "" || strings.ContainsAny(command, "\r\n\x00") {
			return Binding{}, ErrInvalid
		}
	}
	var err error
	b.NetworkGrants, err = normalizeGrantList(b.NetworkGrants)
	if err != nil {
		return Binding{}, err
	}
	b.SecretGrants, err = normalizeSecretGrantList(b.SecretGrants)
	if err != nil {
		return Binding{}, err
	}
	if executionV2 {
		// Canonicalize empty legacy slices before sealing so nil and [] cannot
		// produce different confirmation identities for the same V2 snapshot.
		b.SelectedCommand = nil
		b.NetworkGrants = nil
		b.SecretGrants = nil
		expected := executionV2BindingDigest(b)
		if b.Digest == "" {
			b.Digest = expected
		} else if !Digest(b.Digest).Valid() || b.Digest != expected {
			return Binding{}, ErrInvalid
		}
	}
	return b, nil
}

func executionV2BindingDigest(b Binding) string {
	b.Digest = ""
	raw, _ := json.Marshal(b)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func validExecutionV2RiskGate(risk, gate string) bool {
	switch risk {
	case "R2":
		switch gate {
		case "resource_purchase", "secret_access", "remote_execution", "remote_privileged_execution", "repository_write":
			return true
		}
	case "R3":
		switch gate {
		case "public_network_exposure", "dns_change", "tls_certificate_issue", "data_migration", "production_cutover":
			return true
		}
	case "R4":
		switch gate {
		case "service_destroy", "rollback":
			return true
		}
	}
	return false
}

func isExecutionV2Domain(domain string) bool {
	const prefix = "execution:v2:"
	domain = strings.TrimSpace(domain)
	return strings.HasPrefix(domain, prefix) && validBindingToken(strings.TrimPrefix(domain, prefix))
}

func validBoundedIdentity(value string) bool {
	return value != "" && len(value) <= 255 && utf8.ValidString(value) &&
		!strings.ContainsAny(value, "\x00\r\n")
}

func validBindingToken(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') ||
			r == '_' || r == '-' || r == '.' || r == ':') {
			return false
		}
	}
	return true
}

// Normalize validates and canonicalizes a binding for persistence boundaries.
func (b Binding) Normalize() (Binding, error) { return b.normalized() }

// MatchesConfirmationExpiry prevents an execution.v2 card from displaying or
// persisting an expiry that differs from the immutable value covered by its
// binding. Legacy domains did not bind expiry and therefore remain compatible.
func (b Binding) MatchesConfirmationExpiry(expiresAt time.Time) bool {
	if !isExecutionV2Domain(b.OperationDomain) {
		return true
	}
	return !expiresAt.IsZero() && expiresAt.Location() == time.UTC && b.BindingExpiresAt.Equal(expiresAt)
}

// MatchesOwner keeps the owner carried by an execution.v2 snapshot identical
// to the owner-scoped confirmation row. Legacy bindings predate this explicit
// field and remain governed by their existing row-level owner fence.
func (b Binding) MatchesOwner(ownerID string) bool {
	if !isExecutionV2Domain(b.OperationDomain) {
		return true
	}
	return strings.TrimSpace(ownerID) != "" && b.OwnerID == strings.TrimSpace(ownerID)
}

func normalizeGrantList(values []string) ([]string, error) {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > 256 || strings.ContainsAny(value, "\r\n") {
			return nil, ErrInvalid
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

func normalizeSecretGrantList(values []SecretGrant) ([]SecretGrant, error) {
	seen := make(map[string]struct{}, len(values))
	result := make([]SecretGrant, 0, len(values))
	for _, grant := range values {
		grant.ReferenceID = strings.TrimSpace(grant.ReferenceID)
		if !validateUUID(grant.ReferenceID) || !validSecretPurpose(grant.Purpose) || !grant.BindingDigest.Valid() {
			return nil, ErrInvalid
		}
		if grant.Purpose == SecretPurposeAWSCredential && grant.Revision < 1 || grant.Purpose != SecretPurposeAWSCredential && grant.Revision != 0 {
			return nil, ErrInvalid
		}
		key := grant.ReferenceID + "\x00" + string(grant.Purpose) + "\x00" + strconv.FormatInt(grant.Revision, 10)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, grant)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].ReferenceID == result[j].ReferenceID {
			return result[i].Purpose < result[j].Purpose
		}
		return result[i].ReferenceID < result[j].ReferenceID
	})
	return result, nil
}

func validSecretPurpose(p SecretPurpose) bool {
	switch p {
	case SecretPurposeModelAPIKey, SecretPurposeMCPCredential, SecretPurposeSkillSecret, SecretPurposeAWSCredential, SecretPurposeOtherExtensionSecret:
		return true
	}
	return false
}

func (b Binding) Equal(other Binding) bool {
	a, errA := b.normalized()
	c, errB := other.normalized()
	if errA != nil || errB != nil {
		return false
	}
	return a.Digest == c.Digest &&
		a.OwnerID == c.OwnerID && a.OperationDomain == c.OperationDomain && a.TargetID == c.TargetID && a.TargetRevision == c.TargetRevision && a.TargetKind == c.TargetKind &&
		a.SourceVersion == c.SourceVersion && a.SourceCommit == c.SourceCommit && a.ContentDigest == c.ContentDigest &&
		a.ManifestDigest == c.ManifestDigest && a.ExecutionDigest == c.ExecutionDigest && a.PermissionDigest == c.PermissionDigest &&
		a.ParameterDigest == c.ParameterDigest && a.NetworkDigest == c.NetworkDigest && a.SecretGrantDigest == c.SecretGrantDigest &&
		a.PlanID == c.PlanID && a.PlanRevision == c.PlanRevision && a.PlanDigest == c.PlanDigest &&
		a.DeploymentID == c.DeploymentID && a.RunID == c.RunID && a.RunRevision == c.RunRevision &&
		a.StageID == c.StageID && a.StageRevision == c.StageRevision && a.StageDigest == c.StageDigest &&
		a.TargetDigest == c.TargetDigest && a.ArtifactSetDigest == c.ArtifactSetDigest &&
		a.PolicyDigest == c.PolicyDigest && a.CostQuoteDigest == c.CostQuoteDigest &&
		a.RollbackDigest == c.RollbackDigest && a.PreviewDigest == c.PreviewDigest &&
		a.RiskLevel == c.RiskLevel && a.GateType == c.GateType &&
		a.StageIdempotencyKey == c.StageIdempotencyKey &&
		a.BindingExpiresAt.Equal(c.BindingExpiresAt) &&
		a.SelectedTool == c.SelectedTool && equalStrings(a.SelectedCommand, c.SelectedCommand) &&
		equalStrings(a.NetworkGrants, c.NetworkGrants) && equalSecretGrants(a.SecretGrants, c.SecretGrants)
}

func equalSecretGrants(a, b []SecretGrant) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type Confirmation struct {
	ID             string `json:"id,omitempty"`
	OwnerID        string `json:"owner_id,omitempty"`
	ConfirmationID string
	Binding        Binding
	TaskID         string
	State          State
	Revision       int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ExpiresAt      time.Time
	TerminalCode   string
	TerminalNote   string
	TerminalReason string
}

type Reservation struct {
	ConfirmationID     string
	TaskID             string
	AcquiredAttempt    uint32
	AcquiredLeaseEpoch uint64
	TaskRevision       int64
	Active             bool
}

type RequestCommand struct {
	OwnerID        string
	IdempotencyKey string
	RequestDigest  Digest
	Binding        Binding
	TaskID         string
	ExpiresAt      time.Time
	At             time.Time
}

type ConfirmCommand struct {
	OwnerID          string
	ID               string
	ConfirmationID   string
	IdempotencyKey   string
	RequestDigest    Digest
	ExpectedRevision int64
	Binding          Binding // authoritative snapshot populated by Service
	At               time.Time
	ResolveBinding   func(context.Context) (Binding, error)
}

type RejectCommand struct {
	OwnerID          string
	ID               string
	ConfirmationID   string
	IdempotencyKey   string
	RequestDigest    Digest
	ExpectedRevision int64
	Reason           string
	Note             string
	At               time.Time
}

type ConsumeCommand struct {
	OwnerID              string
	ID                   string
	ConfirmationID       string
	IdempotencyKey       string
	RequestDigest        Digest
	TaskID               string
	Attempt              uint32
	LeaseEpoch           uint64
	ExpectedRevision     int64
	ExpectedTaskRevision int64
	Binding              Binding
	At                   time.Time
	ResolveBinding       func(context.Context) (Binding, error)
	ResolveTaskFence     func(context.Context, string) (TaskFence, error)
}
type ReleaseCommand struct {
	OwnerID, ID, TaskID string
	Attempt             uint32
	LeaseEpoch          uint64
	At                  time.Time
}

type TaskFence struct {
	TaskID         string
	State          string
	FailureCode    string
	InstallationID string
	ConfirmationID string
	Attempt        uint32
	LeaseEpoch     uint64
	Revision       int64
}

type ReleaseReservationCommand struct {
	ConfirmationID       string
	TaskID               string
	AcquiredAttempt      uint32
	AcquiredLeaseEpoch   uint64
	TerminalAttempt      uint32
	TerminalLeaseEpoch   uint64
	ExpectedTaskRevision int64
	IdempotencyKey       string
	RequestDigest        Digest
	ResolveTaskFence     func(context.Context, string) (TaskFence, error)
}

type ExpireCommand struct {
	ConfirmationID   string
	IdempotencyKey   string
	RequestDigest    Digest
	ExpectedRevision int64
	Reason           string
	At               time.Time
}

type ListQuery struct {
	OwnerID   string
	State     *State
	Limit     int
	PageSize  int
	PageToken string
	Domain    string
	TargetID  string
	States    []State
}

type Page struct {
	Confirmations []Confirmation
	NextPageToken string
}

func cloneConfirmation(in Confirmation) Confirmation {
	in.Binding.NetworkGrants = append([]string(nil), in.Binding.NetworkGrants...)
	in.Binding.SecretGrants = append([]SecretGrant(nil), in.Binding.SecretGrants...)
	in.Binding.SelectedCommand = append([]string(nil), in.Binding.SelectedCommand...)
	return in
}

func validateUUID(value string) bool {
	id, err := uuid.Parse(strings.TrimSpace(value))
	return err == nil && id != uuid.Nil && id.String() == strings.TrimSpace(value)
}

func validateMutation(key string, digest Digest) error {
	if !validateUUID(key) || !digest.Valid() {
		return ErrInvalid
	}
	return nil
}

func canonicalDigest(value any) Digest {
	encoded, _ := json.Marshal(value)
	sum := sha256.Sum256(encoded)
	return Digest(hex.EncodeToString(sum[:]))
}

func requestDigest(command RequestCommand) Digest {
	return canonicalDigest(struct {
		Binding   Binding
		TaskID    string
		ExpiresAt time.Time
	}{command.Binding, command.TaskID, command.ExpiresAt.UTC()})
}

// RequestDigestFor* helpers keep durable repository adapters and the in-memory
// reference implementation on one canonical idempotency contract. Repositories
// must recompute these digests rather than trusting caller-supplied values.
func RequestDigestForRequest(command RequestCommand) Digest {
	return requestDigest(command)
}

func RequestDigestForConfirm(command ConfirmCommand) Digest {
	if command.ConfirmationID == "" {
		command.ConfirmationID = command.ID
	}
	return confirmDigest(command)
}

func RequestDigestForReject(command RejectCommand) Digest {
	if command.ConfirmationID == "" {
		command.ConfirmationID = command.ID
	}
	return rejectDigest(command)
}

func RequestDigestForConsume(command ConsumeCommand) Digest {
	if command.ConfirmationID == "" {
		command.ConfirmationID = command.ID
	}
	return consumeDigest(command)
}

func RequestDigestForExpire(command ExpireCommand) Digest {
	return expireDigest(command)
}

func RequestDigestForRelease(command ReleaseReservationCommand) Digest {
	return releaseDigest(command)
}

// ListFilterDigest binds a page token to the exact filter set.
func ListFilterDigest(domain, target string, states []State) Digest {
	normalized := append([]State(nil), states...)
	sort.Slice(normalized, func(i, j int) bool { return normalized[i] < normalized[j] })
	return canonicalDigest(struct {
		Domain, Target string
		States         []State
	}{strings.TrimSpace(domain), strings.TrimSpace(target), normalized})
}

// ValidState exposes the shared state validator to durable repository
// adapters without duplicating the lifecycle enum.
func ValidState(state State) bool {
	return validState(state)
}

func confirmDigest(command ConfirmCommand) Digest {
	return canonicalDigest(struct {
		ID               string
		ExpectedRevision int64
	}{command.ConfirmationID, command.ExpectedRevision})
}
func rejectDigest(command RejectCommand) Digest {
	return canonicalDigest(struct {
		ID               string
		ExpectedRevision int64
		Reason, Note     string
	}{command.ConfirmationID, command.ExpectedRevision, command.Reason, command.Note})
}
func consumeDigest(command ConsumeCommand) Digest {
	return canonicalDigest(struct {
		ID, TaskID                             string
		Attempt, LeaseEpoch                    uint64
		ExpectedRevision, ExpectedTaskRevision int64
	}{command.ConfirmationID, command.TaskID, uint64(command.Attempt), command.LeaseEpoch, command.ExpectedRevision, command.ExpectedTaskRevision})
}
func expireDigest(command ExpireCommand) Digest {
	return canonicalDigest(struct {
		ID               string
		ExpectedRevision int64
		Reason           string
	}{command.ConfirmationID, command.ExpectedRevision, command.Reason})
}
func releaseDigest(command ReleaseReservationCommand) Digest {
	return canonicalDigest(struct {
		ID, TaskID                                                               string
		AcquiredAttempt, AcquiredLeaseEpoch, TerminalAttempt, TerminalLeaseEpoch uint64
		ExpectedTaskRevision                                                     int64
	}{command.ConfirmationID, command.TaskID, uint64(command.AcquiredAttempt), uint64(command.AcquiredLeaseEpoch), uint64(command.TerminalAttempt), uint64(command.TerminalLeaseEpoch), command.ExpectedTaskRevision})
}

func AcknowledgeExtensionExecutionUncertainDigest(command AcknowledgeExtensionExecutionUncertainCommand) Digest {
	return canonicalDigest(struct {
		ConfirmationID, TaskID, InstallationID             string
		ExpectedTaskRevision, ExpectedConfirmationRevision int64
		Resolution                                         ExtensionUncertainResolution
	}{command.ConfirmationID, command.TaskID, command.InstallationID, command.ExpectedTaskRevision, command.ExpectedConfirmationRevision, command.Resolution})
}
