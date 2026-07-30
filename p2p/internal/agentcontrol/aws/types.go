// Package coreaws contains the Core v1 AWS domain boundary.  It deliberately
// exposes typed provider ports only; credentials and arbitrary AWS calls never
// cross this package's public API.
package aws

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrInvalid             = errors.New("coreaws: invalid")
	ErrNotFound            = errors.New("coreaws: not found")
	ErrConflict            = errors.New("coreaws: conflict")
	ErrRevisionConflict    = errors.New("coreaws: revision conflict")
	ErrIdempotencyConflict = errors.New("coreaws: idempotency conflict")
	ErrProvider            = errors.New("coreaws: provider operation failed")
	ErrUnconfirmed         = errors.New("coreaws: change is not confirmed")
	ErrResponseUncertain   = errors.New("coreaws: provider response uncertain")
)

type Operation string

const (
	OperationCreate Operation = "create"
	OperationUpdate Operation = "update"
	OperationDelete Operation = "delete"
)

type ChangeStage string

const (
	StageRequested         ChangeStage = "requested"
	StageChangeSetCreating ChangeStage = "change_set_creating"
	StageChangeSetReady    ChangeStage = "change_set_ready"
	StageExecuting         ChangeStage = "executing"
	StageReconciling       ChangeStage = "reconciling"
	// StageReconciliationRequired records that a task terminalized after a
	// provider request was issued. Only evidence-bound reconciliation may
	// settle the provider outcome and release the consumed confirmation.
	StageReconciliationRequired ChangeStage = "reconciliation_required"
	StageSucceeded              ChangeStage = "succeeded"
	StageFailed                 ChangeStage = "failed"
	StageCanceled               ChangeStage = "canceled"
)

type ChangeStatus string

const (
	ChangeWaitingUser ChangeStatus = "waiting_user"
	ChangeRunning     ChangeStatus = "running"
	ChangeSucceeded   ChangeStatus = "succeeded"
	ChangeFailed      ChangeStatus = "failed"
	ChangeCanceled    ChangeStatus = "canceled"
)

func validStage(s ChangeStage) bool {
	switch s {
	case StageRequested, StageChangeSetCreating, StageChangeSetReady, StageExecuting, StageReconciling, StageReconciliationRequired, StageSucceeded, StageFailed, StageCanceled:
		return true
	}
	return false
}

type Credentials struct {
	ID               string
	Name             string
	Region           string
	private          *credentialPayload
	AccountID        string
	UserARN          string
	VerifiedRevision int64
	Revision         int64
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// RehydrateCredentials reconstructs durable credentials inside the trusted
// Agent boundary. Secret bytes must never be returned from ordinary APIs.
func RehydrateCredentials(id, name, region, accountID, userARN string, accessKeyID, secretAccessKey, sessionToken []byte, verifiedRevision, revision int64, createdAt, updatedAt time.Time) Credentials {
	return Credentials{ID: id, Name: name, Region: region, AccountID: accountID, UserARN: userARN, VerifiedRevision: verifiedRevision, Revision: revision, CreatedAt: createdAt, UpdatedAt: updatedAt, private: &credentialPayload{string(accessKeyID), string(secretAccessKey), string(sessionToken)}}
}

type credentialPayload struct{ accessKeyID, secretAccessKey, sessionToken string }
type CredentialHandle struct {
	credential *credentialPayload
	Region     string
	AccountID  string
	UserARN    string
}

func (c Credentials) handle() CredentialHandle {
	if c.private == nil {
		return CredentialHandle{Region: c.Region, AccountID: c.AccountID, UserARN: c.UserARN}
	}
	return CredentialHandle{credential: &credentialPayload{c.private.accessKeyID, c.private.secretAccessKey, c.private.sessionToken}, Region: c.Region, AccountID: c.AccountID, UserARN: c.UserARN}
}
func (c Credentials) String() string   { return "[redacted-coreaws-credentials]" }
func (c Credentials) GoString() string { return "coreaws.Credentials{[redacted]}" }
func (c Credentials) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ID, Name, Region, AccountID, UserARN        string
		HasAccessKey, HasSecretKey, HasSessionToken bool
		Revision                                    int64
	}{c.ID, c.Name, c.Region, c.AccountID, c.UserARN, c.private != nil && c.private.accessKeyID != "", c.private != nil && c.private.secretAccessKey != "", c.private != nil && c.private.sessionToken != "", c.Revision})
}

// CredentialView is safe for ordinary read/list responses.
type CredentialView struct {
	ID, Name, Region, AccountID, UserARN        string
	HasAccessKey, HasSecretKey, HasSessionToken bool
	Revision                                    int64
	CreatedAt, UpdatedAt                        time.Time
}

func (c Credentials) View() CredentialView {
	return CredentialView{ID: c.ID, Name: c.Name, Region: c.Region, AccountID: c.AccountID, UserARN: c.UserARN, HasAccessKey: c.private != nil && c.private.accessKeyID != "", HasSecretKey: c.private != nil && c.private.secretAccessKey != "", HasSessionToken: c.private != nil && c.private.sessionToken != "", Revision: c.Revision, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
}

// StoredSecretBytes is restricted to persistence adapters inside Agent.
func (c Credentials) StoredSecretBytes() (accessKeyID, secretAccessKey, sessionToken []byte) {
	if c.private == nil {
		return nil, nil, nil
	}
	return []byte(c.private.accessKeyID), []byte(c.private.secretAccessKey), []byte(c.private.sessionToken)
}
func (c Credentials) Validate() error {
	if !validUUID(c.ID) || strings.TrimSpace(c.Name) == "" || !validRegion(c.Region) || c.private == nil || c.private.accessKeyID == "" || c.private.secretAccessKey == "" || c.Revision < 1 {
		return ErrInvalid
	}
	return nil
}

type Identity struct {
	AccountID   string
	UserARN     string
	PrincipalID string
}
type CredentialTest struct {
	CredentialID       string
	Identity           Identity
	CredentialRevision int64
	TestedAt           time.Time
}

type Plan struct {
	ID           string
	CredentialID string
	// CredentialRevision is captured when the plan is created.  Provider
	// execution must resolve this exact immutable credential revision, never
	// whatever revision happens to be current later.
	CredentialRevision int64
	Region             string
	StackName          string
	Operation          Operation
	Template           []byte
	TemplateSHA256     string
	Parameters         map[string]string
	Tags               map[string]string
	Capabilities       []string
	Revision           int64
	CreatedAt          time.Time
}

func (p Plan) Validate() error {
	if !validUUID(p.ID) || !validUUID(p.CredentialID) || p.CredentialRevision < 1 || !validRegion(p.Region) || !validStackName(p.StackName) || !validOperation(p.Operation) || p.Revision < 1 || len(p.Template) == 0 || len(p.Template) > 51200 {
		return ErrInvalid
	}
	norm, digest, err := normalizeTemplate(p.Template)
	if err != nil || digest != p.TemplateSHA256 || len(norm) == 0 {
		return ErrInvalid
	}
	if err := validateMap(p.Parameters, 64, 128, 2048); err != nil {
		return err
	}
	if err := validateMap(p.Tags, 50, 128, 256); err != nil {
		return err
	}
	if len(p.Capabilities) > 3 {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, c := range p.Capabilities {
		if c != "CAPABILITY_IAM" && c != "CAPABILITY_NAMED_IAM" && c != "CAPABILITY_AUTO_EXPAND" || seen[c] {
			return ErrInvalid
		}
		seen[c] = true
	}
	return nil
}

type PlanView struct {
	ID, CredentialID, Region, StackName, TemplateSHA256 string
	Operation                                           Operation
	Parameters, Tags                                    map[string]string
	Capabilities                                        []string
	CredentialRevision, Revision                        int64
	CreatedAt                                           time.Time
}

func (p Plan) View() PlanView {
	return PlanView{ID: p.ID, CredentialID: p.CredentialID, CredentialRevision: p.CredentialRevision, Region: p.Region, StackName: p.StackName, TemplateSHA256: p.TemplateSHA256, Operation: p.Operation, Parameters: cloneMap(p.Parameters), Tags: cloneMap(p.Tags), Capabilities: append([]string(nil), p.Capabilities...), Revision: p.Revision, CreatedAt: p.CreatedAt}
}

type Quote struct {
	PlanID                   string
	Operation                Operation
	Region, StackName        string
	ResourceCount            int
	ParameterCount, TagCount int
	EstimatedMonthlyUSD      float64
	PriceStatus              string
	Summary                  string
	PlanDigest               string
}

type Change struct {
	ID, PlanID, CredentialID, ProvisionID, TaskID, ConfirmationID string
	Operation                                                     Operation
	Status                                                        ChangeStatus
	Stage                                                         ChangeStage
	ChangeSetID                                                   string
	ProviderRequestDigest                                         string
	Revision                                                      int64
	ErrorCode, ErrorSummary                                       string
	ProviderToken                                                 string
	CreatedAt, UpdatedAt                                          time.Time
}

// Provision is the durable, owner-scoped identity for a typed EC2 deployment.
// It deliberately stores only verified stack readback, never provider request
// payloads or credentials.
type Provision struct {
	ID, PlanID, CredentialID, Region, StackName, Profile, OwnerDigest string
	CredentialRevision, PlanRevision                                  int64
	TemplateSHA256, PlanDigest                                        string
	State                                                             string
	Revision                                                          int64
	CreateChangeID, DestroyChangeID, ActiveChangeID                   string
	Readback                                                          ProvisionReadback
	ReconciliationRequired                                            bool
	ErrorCode, ErrorSummary                                           string
	CreatedAt, UpdatedAt                                              time.Time
}

// ProvisionReadback is accepted only after a typed CloudFormation readback
// has validated the exact allowlisted outputs.
type ProvisionReadback struct {
	StackID, InstanceID, PublicIP, SecurityGroupID, OutputDigest string
	ObservedAt                                                   time.Time
}

// ProvisionEvent is the owner-scoped immutable audit cursor for a typed EC2
// provision. Sequence is allocated by the database counter, never MAX+1.
type ProvisionEvent struct {
	ProvisionID string
	EventID     string
	ChangeID    string
	TaskID      string
	Kind        string
	Sequence    uint64
	Revision    int64
	At          time.Time
}

func (p Provision) Validate() error {
	if !validUUID(p.ID) || !validUUID(p.PlanID) || !validUUID(p.CredentialID) || !validRegion(p.Region) || !validStackName(p.StackName) || p.CredentialRevision < 1 || p.PlanRevision < 1 || p.Revision < 1 || !validSHA256(p.TemplateSHA256) || !validSHA256(p.PlanDigest) || !validOwnerDigest(p.OwnerDigest) || strings.TrimSpace(p.Profile) == "" {
		return ErrInvalid
	}
	if p.State != "planned" && p.State != "creating" && p.State != "active" && p.State != "destroying" && p.State != "destroyed" && p.State != "uncertain" && p.State != "failed" {
		return ErrInvalid
	}
	return nil
}

func (r ProvisionReadback) Validate() error {
	if strings.TrimSpace(r.StackID) == "" || strings.TrimSpace(r.InstanceID) == "" || strings.TrimSpace(r.SecurityGroupID) == "" || !validSHA256(r.OutputDigest) || r.ObservedAt.IsZero() {
		return ErrInvalid
	}
	outputs := map[string]string{string(StackOutputStackID): r.StackID, string(StackOutputInstanceID): r.InstanceID, string(StackOutputSecurityGroup): r.SecurityGroupID}
	if strings.TrimSpace(r.PublicIP) != "" {
		outputs[string(StackOutputPublicIP)] = r.PublicIP
	}
	if r.OutputDigest != canonicalDigest(outputs) {
		return ErrInvalid
	}
	return nil
}

type Page[T any] struct {
	Items         []T
	NextPageToken string
}
type CredentialPage = Page[CredentialView]
type PlanPage = Page[PlanView]
type ChangePage = Page[Change]

type CredentialInput struct{ ID, Name, Region, AccessKeyID, SecretAccessKey, SessionToken, IdempotencyKey string }

func (in CredentialInput) String() string   { return "[redacted-coreaws-credential-input]" }
func (in CredentialInput) GoString() string { return "coreaws.CredentialInput{[redacted]}" }
func (in CredentialInput) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ID, Name, Region, IdempotencyKey            string
		HasAccessKey, HasSecretKey, HasSessionToken bool
	}{in.ID, in.Name, in.Region, in.IdempotencyKey, in.AccessKeyID != "", in.SecretAccessKey != "", in.SessionToken != ""})
}
func (in CredentialInput) LogValue() slog.Value {
	return slog.GroupValue(slog.String("id", in.ID), slog.String("name", in.Name), slog.String("region", in.Region), slog.String("idempotency_key", in.IdempotencyKey), slog.Bool("has_access_key", in.AccessKeyID != ""), slog.Bool("has_secret_key", in.SecretAccessKey != ""), slog.Bool("has_session_token", in.SessionToken != ""))
}

func credentialInputDigest(in CredentialInput) string {
	return canonicalDigest(struct {
		ID, Name, Region, IdempotencyKey                               string
		AccessKeyFingerprint, SecretKeyFingerprint, SessionFingerprint string
		HasAccessKey, HasSecretKey, HasSessionToken                    bool
	}{in.ID, in.Name, in.Region, in.IdempotencyKey, secretFingerprint(in.AccessKeyID), secretFingerprint(in.SecretAccessKey), secretFingerprint(in.SessionToken), in.AccessKeyID != "", in.SecretAccessKey != "", in.SessionToken != ""})
}
func secretFingerprint(value string) string {
	if value == "" {
		return ""
	}
	buf := []byte(value)
	s := sha256.Sum256(buf)
	for i := range buf {
		buf[i] = 0
	}
	return hex.EncodeToString(s[:])
}

type PlanInput struct {
	ID, CredentialID, Region, StackName string
	ExpectedCredentialRevision          int64
	Operation                           Operation
	Template                            []byte
	Parameters, Tags                    map[string]string
	Capabilities                        []string
	IdempotencyKey                      string
}

func normalizeTemplate(in []byte) ([]byte, string, error) {
	b := []byte(strings.TrimSpace(string(in)))
	if len(b) == 0 {
		return nil, "", ErrInvalid
	}
	if json.Valid(b) {
		var v any
		if err := json.Unmarshal(b, &v); err != nil {
			return nil, "", ErrInvalid
		}
		b, _ = json.Marshal(v)
	}
	s := sha256.Sum256(b)
	return b, hex.EncodeToString(s[:]), nil
}
func NormalizeTemplate(in []byte) ([]byte, string, error) { return normalizeTemplate(in) }
func validateMap(m map[string]string, max, keyLen, valLen int) error {
	if len(m) > max {
		return ErrInvalid
	}
	for k, v := range m {
		if strings.TrimSpace(k) == "" || len(k) > keyLen || len(v) > valLen || strings.ContainsAny(k+v, "\r\n") {
			return ErrInvalid
		}
	}
	return nil
}
func cloneMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

var stackNameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,127}$`)

func validStackName(s string) bool { return stackNameRE.MatchString(s) }
func validRegion(s string) bool {
	return regexp.MustCompile(`^[a-z]{2}(?:-gov)?-[a-z]+-\d$`).MatchString(strings.TrimSpace(s))
}
func validUUID(s string) bool {
	u, e := uuid.Parse(strings.TrimSpace(s))
	return e == nil && u != uuid.Nil && u.String() == strings.TrimSpace(s)
}
func validSHA256(s string) bool {
	return regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(strings.TrimSpace(s))
}
func validOwnerDigest(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "sha256:") && validSHA256(strings.TrimPrefix(s, "sha256:"))
}
func validOperation(o Operation) bool {
	return o == OperationCreate || o == OperationUpdate || o == OperationDelete
}
func canonicalDigest(v any) string {
	b, _ := json.Marshal(v)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
func planDigest(p Plan) string {
	return canonicalDigest(struct {
		ID, CredentialID, Region, StackName string
		Operation                           Operation
		TemplateSHA256                      string
		Parameters, Tags                    map[string]string
		Capabilities                        []string
	}{p.ID, p.CredentialID, p.Region, p.StackName, p.Operation, p.TemplateSHA256, p.Parameters, p.Tags, p.Capabilities})
}

// PlanDigest returns the canonical immutable digest used to bind a provision
// snapshot to its plan row. It intentionally excludes mutable execution state.
func PlanDigest(p Plan) string { return planDigest(p) }

// OwnerBindingDigest is the canonical, explicitly typed owner tag format.
func OwnerBindingDigest(ownerID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(ownerID)))
	return "sha256:" + hex.EncodeToString(sum[:])
}
func sortedKeys(m map[string]string) []string {
	k := make([]string, 0, len(m))
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}
func quoteFor(p Plan) Quote {
	resources, resourcesKnown := cloudFormationResourceCount(p.Template)
	if !resourcesKnown {
		summary := fmt.Sprintf("%s %s in %s (resource count and AWS pricing unavailable; review the CloudFormation plan before confirmation)", p.Operation, p.StackName, p.Region)
		return Quote{PlanID: p.ID, Operation: p.Operation, Region: p.Region, StackName: p.StackName, ParameterCount: len(p.Parameters), TagCount: len(p.Tags), PriceStatus: "unavailable", Summary: summary, PlanDigest: planDigest(p)}
	}
	if p.Tags["dirextalk:price-status"] == "unavailable" {
		summary := fmt.Sprintf("%s %s in %s (%d resources; AWS pricing unavailable, review EC2, EBS, public IPv4 and data-transfer charges before confirmation)", p.Operation, p.StackName, p.Region, resources)
		return Quote{PlanID: p.ID, Operation: p.Operation, Region: p.Region, StackName: p.StackName, ResourceCount: resources, ParameterCount: len(p.Parameters), TagCount: len(p.Tags), PriceStatus: "unavailable", Summary: summary, PlanDigest: planDigest(p)}
	}
	est := float64(resources) * 0.01
	summary := fmt.Sprintf("%s %s in %s (%d resources; deterministic estimate $%.2f/month)", p.Operation, p.StackName, p.Region, resources, est)
	return Quote{PlanID: p.ID, Operation: p.Operation, Region: p.Region, StackName: p.StackName, ResourceCount: resources, ParameterCount: len(p.Parameters), TagCount: len(p.Tags), EstimatedMonthlyUSD: est, PriceStatus: "deterministic_estimate", Summary: summary, PlanDigest: planDigest(p)}
}

func cloudFormationResourceCount(template []byte) (int, bool) {
	if !json.Valid(template) {
		return 0, false
	}
	var document struct {
		Resources map[string]json.RawMessage `json:"Resources"`
	}
	if err := json.Unmarshal(template, &document); err != nil || document.Resources == nil {
		return 0, false
	}
	return len(document.Resources), true
}
