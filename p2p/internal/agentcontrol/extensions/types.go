// Package extensions contains the in-process, owner-scoped extension boundary.
// It intentionally does not know about ProductCore actions or the process
// extension runner.  Only immutable remote MCP metadata crosses this boundary.
package extensions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

const TransportRemote = "remote"
const KindMCP = "mcp"
const KindSkill = "skill"

var (
	ErrInvalid                = errors.New("extension: invalid")
	ErrNotFound               = errors.New("extension: not found")
	ErrConflict               = errors.New("extension: conflict")
	ErrIdempotencyConflict    = errors.New("extension: idempotency conflict")
	ErrRevisionConflict       = errors.New("extension: revision conflict")
	ErrLocalExecutionDisabled = errors.New("local_execution_disabled")
	ErrUncertain              = errors.New("extension execution uncertain")
	ErrUnavailable            = errors.New("extension capability unavailable")
	ErrAlreadyFinalized       = errors.New("extension execution already finalized")
)

var digestRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

func DigestBytes(v []byte) string { h := sha256.Sum256(v); return hex.EncodeToString(h[:]) }
func validDigest(v string) bool   { return digestRE.MatchString(v) }
func validUUID(v string) bool     { _, err := uuid.Parse(strings.TrimSpace(v)); return err == nil }

type Candidate struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	Source      string    `json:"source"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Transport   string    `json:"transport"`
	Pin         SourcePin `json:"pin"`
}
type SourcePin struct {
	RegistryVersion string `json:"registry_version,omitempty"`
	RegistrySHA256  string `json:"registry_sha256,omitempty"`
	GitCommit       string `json:"git_commit,omitempty"`
	GitSHA256       string `json:"git_sha256,omitempty"`
}
type Endpoint struct {
	URL                   string `json:"url"`
	CredentialReferenceID string `json:"credential_reference_id"`
}
type NetworkGrant struct {
	Scheme     string `json:"scheme"`
	Host       string `json:"host"`
	PathPrefix string `json:"path_prefix"`
	Digest     string `json:"digest"`
	Port       uint32 `json:"port"`
}
type SecretGrant struct {
	ReferenceID   string `json:"reference_id"`
	Purpose       string `json:"purpose"`
	BindingDigest string `json:"binding_digest"`
	Configured    bool   `json:"configured"`
}
type Execution struct {
	Remote *Endpoint       `json:"remote,omitempty"`
	Stdio  json.RawMessage `json:"stdio,omitempty"`
}
type Tool struct {
	Name              string          `json:"name"`
	Description       string          `json:"description"`
	InputSchemaDigest string          `json:"input_schema_digest"`
	InputSchema       json.RawMessage `json:"input_schema,omitempty"`
}
type Inspection struct {
	Candidate       Candidate      `json:"candidate"`
	ContentDigest   string         `json:"content_digest"`
	ManifestDigest  string         `json:"manifest_digest"`
	ExecutionDigest string         `json:"execution_digest"`
	NetworkDigest   string         `json:"network_digest"`
	SecretDigest    string         `json:"secret_digest"`
	Execution       Execution      `json:"execution"`
	NetworkGrants   []NetworkGrant `json:"network_grants"`
	SecretGrants    []SecretGrant  `json:"secret_grants"`
	Tools           []Tool         `json:"tools,omitempty"`
}
type Version struct {
	VersionID                                                                   string
	Pin                                                                         SourcePin
	ContentDigest, ManifestDigest, ExecutionDigest, NetworkDigest, SecretDigest string
	Execution                                                                   Execution
	NetworkGrants                                                               []NetworkGrant
	SecretGrants                                                                []SecretGrant
	Tools                                                                       []Tool
	CreatedAt                                                                   time.Time
}
type Installation struct {
	ID, OwnerID            string
	Candidate              Candidate
	Revision               int64
	State, ActiveVersionID string
	Versions               []Version
	CreatedAt, UpdatedAt   time.Time
}

func (p SourcePin) Validate() error {
	reg := p.RegistryVersion != "" || p.RegistrySHA256 != ""
	git := p.GitCommit != "" || p.GitSHA256 != ""
	if reg == git || (reg && (strings.EqualFold(p.RegistryVersion, "latest") || !validDigest(p.RegistrySHA256))) || (git && (!regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(p.GitCommit) || !validDigest(p.GitSHA256))) {
		return ErrInvalid
	}
	return nil
}
func endpoint(u string) (*url.URL, error) {
	p, e := url.Parse(strings.TrimSpace(u))
	if e != nil || p.Scheme != "https" || p.Host == "" || p.User != nil || p.RawQuery != "" || p.Fragment != "" || p.Opaque != "" || p.Host != strings.ToLower(p.Host) || strings.Contains(p.Path, "..") {
		return nil, ErrInvalid
	}
	return p, nil
}
func (c Candidate) Validate() error {
	sourceOK := c.Source == "official_registry" || c.Source == "smithery" || c.Source == "glama" || c.Source == "github"
	if strings.TrimSpace(c.ID) == "" || strings.TrimSpace(c.Name) == "" || !sourceOK || strings.TrimSpace(c.Kind) != KindMCP || strings.TrimSpace(c.Transport) != TransportRemote || c.Pin.Validate() != nil {
		return ErrInvalid
	}
	return nil
}
func (g NetworkGrant) Validate() error {
	if g.Scheme != "https" || g.Host == "" || g.Port == 0 || g.Port > 65535 || !strings.HasPrefix(g.PathPrefix, "/") || !validDigest(g.Digest) || strings.ContainsAny(g.Host, " /\\\r\n") {
		return ErrInvalid
	}
	return nil
}
func (s SecretGrant) Validate() error {
	if !validUUID(s.ReferenceID) || strings.TrimSpace(s.Purpose) == "" || !validDigest(s.BindingDigest) {
		return ErrInvalid
	}
	return nil
}
func (i Inspection) Validate() error {
	if i.Candidate.Validate() != nil || !validDigest(i.ContentDigest) || !validDigest(i.ManifestDigest) || !validDigest(i.ExecutionDigest) || !validDigest(i.NetworkDigest) || !validDigest(i.SecretDigest) || i.Execution.Remote == nil || i.Execution.Stdio != nil {
		return ErrInvalid
	}
	u, e := endpoint(i.Execution.Remote.URL)
	if e != nil || !validUUID(i.Execution.Remote.CredentialReferenceID) {
		return ErrInvalid
	}
	port := uint32(443)
	if p := u.Port(); p != "" {
		var n uint64
		for _, r := range p {
			n = n*10 + uint64(r-'0')
		}
		port = uint32(n)
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	matched := false
	for _, g := range i.NetworkGrants {
		if g.Validate() != nil {
			return ErrInvalid
		}
		if g.Scheme == u.Scheme && g.Host == u.Hostname() && g.Port == port && g.PathPrefix == path {
			matched = true
		}
	}
	secret := false
	for _, g := range i.SecretGrants {
		if g.Validate() != nil {
			return ErrInvalid
		}
		if g.ReferenceID == i.Execution.Remote.CredentialReferenceID {
			secret = true
		}
	}
	if !matched || !secret {
		return ErrInvalid
	}
	return nil
}
func (i Installation) Validate() error {
	if !validUUID(i.ID) || strings.TrimSpace(i.OwnerID) == "" || i.Candidate.Validate() != nil || i.Revision < 1 || i.State == "" {
		return ErrInvalid
	}
	for _, v := range i.Versions {
		if !validUUID(v.VersionID) || !validDigest(v.ContentDigest) || !validDigest(v.ManifestDigest) || !validDigest(v.ExecutionDigest) || !validDigest(v.NetworkDigest) || !validDigest(v.SecretDigest) {
			return ErrInvalid
		}
		for _, grant := range v.NetworkGrants {
			if grant.Validate() != nil {
				return ErrInvalid
			}
		}
		for _, grant := range v.SecretGrants {
			if grant.Validate() != nil || !grant.Configured {
				return ErrInvalid
			}
		}
	}
	return nil
}
func (i Installation) Redacted() Installation {
	x := i
	x.Versions = append([]Version(nil), i.Versions...)
	for n := range x.Versions {
		x.Versions[n].SecretGrants = append([]SecretGrant(nil), i.Versions[n].SecretGrants...)
	}
	return x
}
func (i Installation) String() string { b, _ := json.Marshal(i.Redacted()); return string(b) }

type Mutation struct {
	OwnerID, IdempotencyKey, InstallationID string
	ExpectedRevision                        int64
	Candidate                               Candidate
	Inspection                              Inspection
	SecretInputs                            []SecretInput
	Content                                 []byte
}
type SecretInput struct{ ReferenceID, Purpose, Value string }
type LifecycleResult struct {
	Installation           Installation
	TaskID, ConfirmationID string
}
type ConfirmationBinding struct {
	OwnerID, Operation, TargetID                                                                 string
	VersionID, ToolName, ToolSchemaDigest, SourceVersion, SourceCommit                           string
	TargetRevision                                                                               int64
	ParameterDigest, ContentDigest, ManifestDigest, ExecutionDigest, NetworkDigest, SecretDigest string
	NetworkGrants                                                                                []string
	SecretGrants                                                                                 []SecretGrant
}

// Equal reports whether two extension confirmation bindings cover the same
// immutable operation snapshot. Keep this comparison at the extension
// boundary so adapters and workers cannot accidentally omit a newly-added
// digest (in particular the input and tool-schema digests).
func (b ConfirmationBinding) Equal(other ConfirmationBinding) bool {
	if b.OwnerID != other.OwnerID || b.Operation != other.Operation || b.TargetID != other.TargetID ||
		b.VersionID != other.VersionID || b.ToolName != other.ToolName || b.ToolSchemaDigest != other.ToolSchemaDigest ||
		b.SourceVersion != other.SourceVersion || b.SourceCommit != other.SourceCommit || b.TargetRevision != other.TargetRevision ||
		b.ParameterDigest != other.ParameterDigest || b.ContentDigest != other.ContentDigest || b.ManifestDigest != other.ManifestDigest ||
		b.ExecutionDigest != other.ExecutionDigest || b.NetworkDigest != other.NetworkDigest || b.SecretDigest != other.SecretDigest {
		return false
	}
	if len(b.NetworkGrants) != len(other.NetworkGrants) || len(b.SecretGrants) != len(other.SecretGrants) {
		return false
	}
	for n := range b.NetworkGrants {
		if b.NetworkGrants[n] != other.NetworkGrants[n] {
			return false
		}
	}
	for n := range b.SecretGrants {
		if b.SecretGrants[n] != other.SecretGrants[n] {
			return false
		}
	}
	return true
}

type ConfirmationRequest struct {
	OwnerID, IdempotencyKey, TaskID string
	Binding                         ConfirmationBinding
	ExpiresAt                       time.Time
}
type Confirmation struct {
	ID, OwnerID, TaskID, State string
	Revision                   int64
	Binding                    ConfirmationBinding
	ExpiresAt                  time.Time
}
type ConsumeRequest struct {
	OwnerID, ID, TaskID, IdempotencyKey string
	ExpectedRevision                    int64
	Attempt                             uint32
	LeaseEpoch                          uint64
	ExpectedTaskRevision                int64
	Binding                             ConfirmationBinding
}
type TaskRequest struct {
	OwnerID, TaskID, IdempotencyKey, Goal string
	Payload                               json.RawMessage
}
type Task struct {
	ID, OwnerID, Status string
	Revision            uint64
}
type ExecuteRequest struct {
	OwnerID, InstallationID, IdempotencyKey, ToolName string
	ExpectedRevision                                  int64
	Input                                             json.RawMessage
}
type ExecuteResult struct {
	TaskID, ConfirmationID    string
	InstallationID, VersionID string
}

// ExecutionFinalizeRequest is the durable fence used after the single
// external MCP call.  The storage implementation must atomically terminalize
// the generic task, release the confirmation reservation and persist the
// execution receipt; callers must never retry the provider call on an error.
type ExecutionFinalizeRequest struct {
	OwnerID, TaskID, ConfirmationID, InstallationID, VersionID string
	RequestDigest, ResultDigest, ErrorCode, ErrorSummary       string
	LeaseHolder                                                string
	Operation                                                  string
	Attempt                                                    uint32
	LeaseEpoch, TaskRevision, InstallationRevision             uint64
	Success, Uncertain                                         bool
	// ReconcileConsumed is set only by a successor lease after finding a
	// confirmation already consumed by an expired lease. Execute-tool
	// recovery terminalizes as uncertain without calling the provider again;
	// lifecycle recovery may safely finish its database-only projection.
	ReconcileConsumed bool
}

type ExecutionFinalizer interface {
	FinalizeExecution(context.Context, ExecutionFinalizeRequest) error
}

type LifecycleFinalizer interface {
	FinalizeLifecycle(context.Context, ExecutionFinalizeRequest) error
}

type ConfirmationPort interface {
	Request(context.Context, ConfirmationRequest) (Confirmation, error)
	Consume(context.Context, ConsumeRequest) (Confirmation, error)
}
type TaskPort interface {
	CreateWaitingUser(context.Context, TaskRequest) (Task, error)
	Run(context.Context, Task, Confirmation) error
	MarkUncertain(context.Context, Task, string) error
}
type SecretResolver interface {
	Resolve(context.Context, string, string, string, string, string) ([]byte, error)
}
type SecretStager interface {
	Stage(context.Context, string, string, int64, string, []SecretInput) error
}
type AtomicLifecycleStore interface {
	SupportsAtomicLifecycle() bool
	CommitLifecycle(context.Context, AtomicLifecycleRequest) (LifecycleResult, error)
	CommitUninstall(context.Context, AtomicLifecycleRequest) (LifecycleResult, error)
	CommitExecute(context.Context, ExecuteAtomicRequest) (ExecuteResult, error)
}
type AtomicLifecycleRequest struct {
	OwnerID, IdempotencyKey, Operation, MutationDigest string
	ExpectedRevision                                   int64
	Installation                                       Installation
	SecretInputs                                       []SecretInput
	Task                                               TaskRequest
	Confirmation                                       ConfirmationRequest
}
type ExecuteAtomicRequest struct {
	OwnerID, IdempotencyKey string
	Installation            Installation
	Version                 Version
	Tool                    Tool
	Input                   json.RawMessage
	Task                    TaskRequest
	Confirmation            ConfirmationRequest
}
