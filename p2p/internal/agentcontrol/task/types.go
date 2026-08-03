// Package coretask contains the Core v1 task and schedule domain contracts.
package task

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

type Status string

const (
	StatusQueued      Status = "queued"
	StatusRunning     Status = "running"
	StatusSucceeded   Status = "succeeded"
	StatusFailed      Status = "failed"
	StatusWaitingUser Status = "waiting_user"
	StatusCanceled    Status = "canceled"
)

// TaskKind is a closed discriminator for durable Core v1 task payloads.
type TaskKind string

const (
	TaskKindAgent            TaskKind = "agent"
	TaskKindExtension        TaskKind = "extension"
	TaskKindKnowledgeIndex   TaskKind = "knowledge_index"
	TaskKindExecutionStage   TaskKind = "execution_stage"
	TaskKindConversationTool TaskKind = "conversation_tool"
)

// ExecutionStageTaskPayload is the immutable worker fence for one materialized
// execution.v2 stage. It contains identities, revisions, and server-computed
// digests only; executable content and credentials are resolved from the
// authoritative plan and target snapshots after the task is claimed.
type ExecutionStageTaskPayload struct {
	PlanID         string `json:"plan_id"`
	PlanRevision   uint64 `json:"plan_revision"`
	PlanDigest     string `json:"plan_digest"`
	DeploymentID   string `json:"deployment_id,omitempty"`
	RunID          string `json:"run_id"`
	RunRevision    uint64 `json:"run_revision"`
	StageID        string `json:"stage_id"`
	StageRevision  uint64 `json:"stage_revision"`
	StageDigest    string `json:"stage_digest"`
	TargetID       string `json:"target_id"`
	TargetRevision uint64 `json:"target_revision"`
	TargetDigest   string `json:"target_digest"`
	ConfirmationID string `json:"confirmation_id,omitempty"`
}

type ExtensionOperation string

const (
	ExtensionOperationInstall      ExtensionOperation = "install"
	ExtensionOperationUpdate       ExtensionOperation = "update"
	ExtensionOperationUninstall    ExtensionOperation = "uninstall"
	ExtensionOperationExecuteTool  ExtensionOperation = "execute_tool"
	ExtensionOperationExecuteSkill ExtensionOperation = "execute_skill"
)

const (
	MaxCanonicalInputBytes = 64 << 10
	MaxSourceIDCount       = 128
)

type ExtensionTaskPayload struct {
	Operation          ExtensionOperation `json:"operation"`
	InstallationID     string             `json:"installation_id"`
	ExpectedRevision   uint64             `json:"expected_revision,omitempty"`
	Version            string             `json:"version"`
	Digest             string             `json:"digest"`
	ArtifactDigest     string             `json:"artifact_digest,omitempty"`
	ConfirmationID     string             `json:"confirmation_id,omitempty"`
	ToolName           string             `json:"tool_name,omitempty"`
	CanonicalInputJSON json.RawMessage    `json:"input_json,omitempty"`
}

// ConversationToolTaskPayload is the exact durable handoff from a turn to
// the common task/runner path. It contains only IDs and digests; arguments
// are represented by their digest and a bounded safe summary.
type ConversationToolTaskPayload struct {
	TurnID                  string `json:"turn_id"`
	AttemptID               string `json:"attempt_id"`
	Round                   uint32 `json:"round"`
	CallID                  string `json:"call_id"`
	ExtensionSnapshotDigest string `json:"extension_snapshot_digest"`
	InstallationID          string `json:"installation_id"`
	VersionID               string `json:"version_id"`
	InstallationRevision    uint64 `json:"installation_revision"`
	ToolName                string `json:"tool_name"`
	ToolSchemaDigest        string `json:"tool_schema_digest"`
	ArgumentsDigest         string `json:"arguments_digest"`
	ConfirmationID          string `json:"confirmation_id,omitempty"`
	SafeSummary             string `json:"safe_summary,omitempty"`
}

type KnowledgeIndexTaskPayload struct {
	SourceIDs              []string `json:"source_ids"`
	ExpectedSourceRevision []uint64 `json:"expected_source_revisions"`
	CollectionConfigDigest string   `json:"collection_config_digest"`
}

// TaskPayload is a closed union; exactly one branch must match Kind.
type TaskPayload struct {
	ConversationTool *ConversationToolTaskPayload `json:"conversation_tool,omitempty"`
	Extension        *ExtensionTaskPayload        `json:"extension,omitempty"`
	KnowledgeIndex   *KnowledgeIndexTaskPayload   `json:"knowledge_index,omitempty"`
	ExecutionStage   *ExecutionStageTaskPayload   `json:"execution_stage,omitempty"`
}

var (
	ErrInvalid          = errors.New("coretask: invalid")
	ErrConflict         = errors.New("coretask: conflict")
	ErrRevisionConflict = errors.New("coretask: revision conflict")
	ErrLeaseConflict    = errors.New("coretask: lease conflict")
	ErrNotFound         = errors.New("coretask: not found")
	ErrTerminal         = errors.New("coretask: terminal task")
	ErrTimedOut         = errors.New("task_timed_out")
)

const (
	MaxGoalBytes        = 64 << 10
	MaxRefCount         = 128
	MaxRefBytes         = 512
	MaxProgressMessage  = 4 << 10
	MaxTimeoutSeconds   = int64((30 * 24 * time.Hour) / time.Second)
	MaxResultBytes      = 1 << 20
	MaxResultTextBytes  = 64 << 10
	MaxFileCount        = 128
	MaxFilePathBytes    = 1024
	MaxSummaryBytes     = 4 << 10
	MaxLeaseHolderBytes = 256
	// PostgreSQL and confirmation revisions are signed BIGINT/int64 values.
	// Reject a task fence that cannot be represented identically downstream.
	MaxPersistentRevision = uint64(1<<63 - 1)
)

type TaskSpec struct {
	Kind           TaskKind             `json:"kind,omitempty"`
	Payload        TaskPayload          `json:"payload,omitempty"`
	Goal           string               `json:"goal"`
	ConversationID string               `json:"conversation_id,omitempty"`
	AttachmentRefs []string             `json:"attachment_refs,omitempty"`
	ModelProfileID string               `json:"model_profile_id"`
	Extensions     []ExtensionSelection `json:"extensions,omitempty"`
	KnowledgeRefs  []string             `json:"knowledge_refs,omitempty"`
	TimeoutSeconds int64                `json:"timeout_seconds,omitempty"`
	IdempotencyKey string               `json:"idempotency_key"`
	AvailableAt    time.Time            `json:"available_at,omitempty"`
}

type ExtensionKind string

const (
	ExtensionMCP   ExtensionKind = "mcp"
	ExtensionSkill ExtensionKind = "skill"
)

type ExtensionSelection struct {
	Kind         ExtensionKind `json:"kind"`
	ID           string        `json:"id"`
	Version      string        `json:"version"`
	Digest       string        `json:"digest"`
	AllowedTools []string      `json:"allowed_tools,omitempty"`
}

func normalizeExtensions(in []ExtensionSelection) ([]ExtensionSelection, error) {
	if len(in) > MaxRefCount {
		return nil, ErrInvalid
	}
	out := make([]ExtensionSelection, 0, len(in))
	for _, e := range in {
		e.ID = strings.TrimSpace(e.ID)
		e.Version = strings.TrimSpace(e.Version)
		e.Digest = strings.TrimSpace(e.Digest)
		if (e.Kind != ExtensionMCP && e.Kind != ExtensionSkill) || !ValidUUID(e.ID) || e.Version == "" || len([]byte(e.Version)) > 128 || !utf8.ValidString(e.Version) || len(e.Digest) != 64 || strings.ToLower(e.Digest) != e.Digest {
			return nil, ErrInvalid
		}
		if _, err := hex.DecodeString(e.Digest); err != nil {
			return nil, ErrInvalid
		}
		tools, err := normalizeRefs(e.AllowedTools)
		if err != nil {
			return nil, err
		}
		e.AllowedTools = tools
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		if out[i].Version != out[j].Version {
			return out[i].Version < out[j].Version
		}
		return out[i].Digest < out[j].Digest
	})
	for i := 1; i < len(out); i++ {
		if out[i].Kind == out[i-1].Kind && out[i].ID == out[i-1].ID && out[i].Version == out[i-1].Version && out[i].Digest == out[i-1].Digest {
			return nil, ErrConflict
		}
	}
	return out, nil
}

type TaskTemplate struct {
	Kind           TaskKind             `json:"kind,omitempty"`
	Payload        TaskPayload          `json:"payload,omitempty"`
	Goal           string               `json:"goal"`
	ConversationID string               `json:"conversation_id,omitempty"`
	AttachmentRefs []string             `json:"attachment_refs,omitempty"`
	ModelProfileID string               `json:"model_profile_id"`
	Extensions     []ExtensionSelection `json:"extensions,omitempty"`
	KnowledgeRefs  []string             `json:"knowledge_refs,omitempty"`
	TimeoutSeconds int64                `json:"timeout_seconds,omitempty"`
}

func (t TaskTemplate) Normalize() (TaskTemplate, error) {
	s, err := (TaskSpec{Kind: t.Kind, Payload: t.Payload, Goal: t.Goal, ConversationID: t.ConversationID, AttachmentRefs: t.AttachmentRefs, ModelProfileID: t.ModelProfileID, Extensions: t.Extensions, KnowledgeRefs: t.KnowledgeRefs, TimeoutSeconds: t.TimeoutSeconds, IdempotencyKey: "00000000-0000-4000-8000-000000000001"}).Normalize()
	if err != nil {
		return TaskTemplate{}, err
	}
	return TaskTemplate{Kind: s.Kind, Payload: s.Payload, Goal: s.Goal, ConversationID: s.ConversationID, AttachmentRefs: s.AttachmentRefs, ModelProfileID: s.ModelProfileID, Extensions: s.Extensions, KnowledgeRefs: s.KnowledgeRefs, TimeoutSeconds: s.TimeoutSeconds}, nil
}
func (t TaskTemplate) Validate() error { _, err := t.Normalize(); return err }
func (t TaskTemplate) Materialize(idempotencyKey string, availableAt time.Time) (TaskSpec, error) {
	n, err := t.Normalize()
	if err != nil {
		return TaskSpec{}, err
	}
	if !ValidUUID(idempotencyKey) || availableAt.IsZero() {
		return TaskSpec{}, ErrInvalid
	}
	return TaskSpec{Kind: n.Kind, Payload: n.Payload, Goal: n.Goal, ConversationID: n.ConversationID, AttachmentRefs: n.AttachmentRefs, ModelProfileID: n.ModelProfileID, Extensions: n.Extensions, KnowledgeRefs: n.KnowledgeRefs, TimeoutSeconds: n.TimeoutSeconds, IdempotencyKey: idempotencyKey, AvailableAt: availableAt.UTC()}.Normalize()
}

func (s TaskSpec) Normalize() (TaskSpec, error) {
	if s.Kind == "" {
		s.Kind = TaskKindAgent
	}
	goal := strings.TrimSpace(s.Goal)
	s.ModelProfileID = strings.TrimSpace(s.ModelProfileID)
	s.ConversationID = strings.TrimSpace(s.ConversationID)
	s.IdempotencyKey = strings.TrimSpace(s.IdempotencyKey)
	if goal == "" || !utf8.ValidString(s.Goal) || len([]byte(s.Goal)) > MaxGoalBytes || !ValidUUID(s.IdempotencyKey) {
		return TaskSpec{}, ErrInvalid
	}
	if err := normalizePayload(&s); err != nil {
		return TaskSpec{}, err
	}
	if (s.Kind == TaskKindAgent || s.Kind == TaskKindKnowledgeIndex) && !ValidUUID(s.ModelProfileID) {
		return TaskSpec{}, ErrInvalid
	}
	if s.Kind != TaskKindAgent && s.Kind != TaskKindKnowledgeIndex && s.ModelProfileID != "" && !ValidUUID(s.ModelProfileID) {
		return TaskSpec{}, ErrInvalid
	}
	if s.Kind != TaskKindAgent && (s.ConversationID != "" || len(s.AttachmentRefs) != 0 || len(s.Extensions) != 0 || len(s.KnowledgeRefs) != 0) {
		return TaskSpec{}, ErrInvalid
	}
	if s.ConversationID != "" && !ValidUUID(s.ConversationID) {
		return TaskSpec{}, ErrInvalid
	}
	if s.TimeoutSeconds < 0 || s.TimeoutSeconds > MaxTimeoutSeconds {
		return TaskSpec{}, ErrInvalid
	}
	var err error
	if s.AttachmentRefs, err = normalizeRefs(s.AttachmentRefs); err != nil {
		return TaskSpec{}, err
	}
	if s.Extensions, err = normalizeExtensions(s.Extensions); err != nil {
		return TaskSpec{}, err
	}
	if s.KnowledgeRefs, err = normalizeRefs(s.KnowledgeRefs); err != nil {
		return TaskSpec{}, err
	}
	if !s.AvailableAt.IsZero() {
		s.AvailableAt = s.AvailableAt.UTC()
	}
	return s, nil
}

func normalizePayload(s *TaskSpec) error {
	count := 0
	if s.Payload.Extension != nil {
		count++
	}
	if s.Payload.KnowledgeIndex != nil {
		count++
	}
	if s.Payload.ExecutionStage != nil {
		count++
	}
	if s.Payload.ConversationTool != nil {
		count++
	}
	switch s.Kind {
	case TaskKindAgent:
		if count != 0 {
			return ErrInvalid
		}
	case TaskKindExtension:
		if count != 1 || s.Payload.Extension == nil {
			return ErrInvalid
		}
		p := s.Payload.Extension
		if !ValidUUID(strings.TrimSpace(p.InstallationID)) || !validExtensionOperation(p.Operation) || strings.TrimSpace(p.Version) == "" || len([]byte(p.Version)) > 128 || len(p.Digest) != 64 || strings.ToLower(p.Digest) != p.Digest {
			return ErrInvalid
		}
		if _, err := hex.DecodeString(p.Digest); err != nil {
			return ErrInvalid
		}
		p.InstallationID = strings.TrimSpace(p.InstallationID)
		p.Version = strings.TrimSpace(p.Version)
		p.Digest = strings.TrimSpace(p.Digest)
		lifecycle := p.Operation == ExtensionOperationInstall || p.Operation == ExtensionOperationUpdate || p.Operation == ExtensionOperationUninstall
		if lifecycle {
			if !ValidUUID(strings.TrimSpace(p.ConfirmationID)) || p.ToolName != "" || len(p.CanonicalInputJSON) != 0 {
				return ErrInvalid
			}
			p.ConfirmationID = strings.TrimSpace(p.ConfirmationID)
		} else {
			if p.ExpectedRevision == 0 || (p.ConfirmationID != "" && !ValidUUID(strings.TrimSpace(p.ConfirmationID))) || len(p.CanonicalInputJSON) == 0 || len(p.CanonicalInputJSON) > MaxCanonicalInputBytes || !json.Valid(p.CanonicalInputJSON) {
				return ErrInvalid
			}
			var v any
			if err := json.Unmarshal(p.CanonicalInputJSON, &v); err != nil {
				return ErrInvalid
			}
			if containsForbiddenInput(v) {
				return ErrInvalid
			}
			canonical, _ := json.Marshal(v)
			p.CanonicalInputJSON = canonical
			p.ToolName = strings.TrimSpace(p.ToolName)
		}
	case TaskKindConversationTool:
		if count != 1 || s.Payload.ConversationTool == nil {
			return ErrInvalid
		}
		p := s.Payload.ConversationTool
		if !ValidUUID(p.TurnID) || !ValidUUID(p.AttemptID) || !ValidUUID(p.CallID) || p.Round > 100 || !ValidUUID(p.InstallationID) || !ValidUUID(p.VersionID) || p.InstallationRevision == 0 || strings.TrimSpace(p.ToolName) == "" || !ValidDigest(p.ExtensionSnapshotDigest) || !ValidDigest(p.ToolSchemaDigest) || !ValidDigest(p.ArgumentsDigest) || len([]byte(p.SafeSummary)) > MaxSummaryBytes {
			return ErrInvalid
		}
	case TaskKindKnowledgeIndex:
		if count != 1 || s.Payload.KnowledgeIndex == nil {
			return ErrInvalid
		}
		p := s.Payload.KnowledgeIndex
		if len(p.SourceIDs) == 0 || len(p.SourceIDs) > MaxSourceIDCount || len(p.SourceIDs) != len(p.ExpectedSourceRevision) || strings.TrimSpace(p.CollectionConfigDigest) == "" || len(p.CollectionConfigDigest) != 64 || strings.ToLower(p.CollectionConfigDigest) != p.CollectionConfigDigest {
			return ErrInvalid
		}
		if _, err := hex.DecodeString(p.CollectionConfigDigest); err != nil {
			return ErrInvalid
		}
		for i := range p.SourceIDs {
			if strings.TrimSpace(p.SourceIDs[i]) == "" || p.ExpectedSourceRevision[i] == 0 {
				return ErrInvalid
			}
			p.SourceIDs[i] = strings.TrimSpace(p.SourceIDs[i])
		}
		for i := 1; i < len(p.SourceIDs); i++ {
			if p.SourceIDs[i] <= p.SourceIDs[i-1] {
				return ErrInvalid
			}
		}
	case TaskKindExecutionStage:
		if count != 1 || s.Payload.ExecutionStage == nil {
			return ErrInvalid
		}
		p := s.Payload.ExecutionStage
		for _, id := range []string{p.PlanID, p.RunID, p.StageID, p.TargetID} {
			if !ValidUUID(strings.TrimSpace(id)) {
				return ErrInvalid
			}
		}
		if p.DeploymentID != "" && !ValidUUID(strings.TrimSpace(p.DeploymentID)) {
			return ErrInvalid
		}
		if p.ConfirmationID != "" && !ValidUUID(strings.TrimSpace(p.ConfirmationID)) {
			return ErrInvalid
		}
		if p.PlanRevision == 0 || p.PlanRevision > MaxPersistentRevision ||
			p.RunRevision == 0 || p.RunRevision > MaxPersistentRevision ||
			p.StageRevision == 0 || p.StageRevision > MaxPersistentRevision ||
			p.TargetRevision == 0 || p.TargetRevision > MaxPersistentRevision ||
			!ValidDigest(strings.TrimSpace(p.PlanDigest)) ||
			!ValidDigest(strings.TrimSpace(p.StageDigest)) ||
			!ValidDigest(strings.TrimSpace(p.TargetDigest)) {
			return ErrInvalid
		}
		p.PlanID = strings.TrimSpace(p.PlanID)
		p.PlanDigest = strings.TrimSpace(p.PlanDigest)
		p.DeploymentID = strings.TrimSpace(p.DeploymentID)
		p.RunID = strings.TrimSpace(p.RunID)
		p.StageID = strings.TrimSpace(p.StageID)
		p.StageDigest = strings.TrimSpace(p.StageDigest)
		p.TargetID = strings.TrimSpace(p.TargetID)
		p.TargetDigest = strings.TrimSpace(p.TargetDigest)
		p.ConfirmationID = strings.TrimSpace(p.ConfirmationID)
	default:
		return ErrInvalid
	}
	return nil
}

func ValidDigest(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func containsForbiddenInput(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, value := range x {
			lk := strings.ToLower(strings.ReplaceAll(k, "-", "_"))
			for _, forbidden := range []string{"secret", "token", "password", "credential", "api_key", "apikey", "argv", "command", "endpoint"} {
				if strings.Contains(lk, forbidden) {
					return true
				}
			}
			if containsForbiddenInput(value) {
				return true
			}
		}
	case []any:
		for _, value := range x {
			if containsForbiddenInput(value) {
				return true
			}
		}
	}
	return false
}

func validExtensionOperation(op ExtensionOperation) bool {
	switch op {
	case ExtensionOperationInstall, ExtensionOperationUpdate, ExtensionOperationUninstall, ExtensionOperationExecuteTool, ExtensionOperationExecuteSkill:
		return true
	}
	return false
}

func normalizeRefs(refs []string) ([]string, error) {
	if len(refs) > MaxRefCount {
		return nil, ErrInvalid
	}
	seen := make(map[string]struct{}, len(refs))
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" || !utf8.ValidString(ref) || len([]byte(ref)) > MaxRefBytes {
			return nil, ErrInvalid
		}
		if _, ok := seen[ref]; ok {
			continue
		}
		seen[ref] = struct{}{}
		out = append(out, ref)
	}
	sort.Strings(out)
	return out, nil
}

func (s TaskSpec) Validate() error { _, err := s.Normalize(); return err }

func ValidUUID(value string) bool {
	v := strings.TrimSpace(value)
	u, err := uuid.Parse(v)
	return err == nil && u != uuid.Nil && strings.ToLower(v) == u.String()
}

type FileRef struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

func (f FileRef) Validate() error {
	p := strings.TrimSpace(f.Path)
	if p == "" || len([]byte(p)) > MaxFilePathBytes || !utf8.ValidString(p) || path.IsAbs(p) || path.Clean(p) != p || p == "." || strings.HasPrefix(p, "../") || strings.Contains(p, `\`) || f.Size < 0 || len(f.Digest) != 64 {
		return ErrInvalid
	}
	for _, r := range f.Digest {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return ErrInvalid
		}
	}
	return nil
}

type Result struct {
	Text    string          `json:"text,omitempty"`
	JSON    json.RawMessage `json:"json,omitempty"`
	Files   []FileRef       `json:"files,omitempty"`
	Summary string          `json:"summary,omitempty"`
}

func (r Result) Validate() error {
	if len([]byte(r.Text)) > MaxResultTextBytes || !utf8.ValidString(r.Text) || len(r.JSON) > MaxResultBytes || len([]byte(r.Summary)) > MaxSummaryBytes || !utf8.ValidString(r.Summary) || len(r.Files) > MaxFileCount {
		return ErrInvalid
	}
	if len(r.JSON) > 0 && !json.Valid(r.JSON) {
		return ErrInvalid
	}
	for _, f := range r.Files {
		if err := f.Validate(); err != nil {
			return err
		}
	}
	return nil
}

type Progress struct {
	EventID       string          `json:"event_id,omitempty"`
	TaskID        string          `json:"task_id"`
	Attempt       uint32          `json:"attempt"`
	Sequence      uint64          `json:"sequence"`
	At            time.Time       `json:"at"`
	Status        Status          `json:"status"`
	Phase         string          `json:"phase,omitempty"`
	Message       string          `json:"message,omitempty"`
	Percent       *float64        `json:"percent,omitempty"`
	ResultSummary string          `json:"result_summary,omitempty"`
	ResultJSON    json.RawMessage `json:"result_json,omitempty"`
	ErrorCode     string          `json:"error_code,omitempty"`
	ErrorSummary  string          `json:"error_summary,omitempty"`
}

type Lease struct {
	TaskID    string    `json:"task_id"`
	Attempt   uint32    `json:"attempt"`
	Epoch     uint64    `json:"epoch"`
	Holder    string    `json:"holder"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Task struct {
	OwnerID             string             `json:"owner_id,omitempty"`
	ID                  string             `json:"id"`
	Spec                TaskSpec           `json:"spec"`
	Status              Status             `json:"status"`
	Attempt             uint32             `json:"attempt"`
	LeaseEpoch          uint64             `json:"lease_epoch"`
	Revision            uint64             `json:"revision"`
	ProgressSequence    uint64             `json:"progress_sequence"`
	CreatedAt           time.Time          `json:"created_at"`
	UpdatedAt           time.Time          `json:"updated_at"`
	AvailableAt         time.Time          `json:"available_at"`
	ExecutionStartedAt  *time.Time         `json:"execution_started_at,omitempty"`
	ExecutionDeadlineAt *time.Time         `json:"execution_deadline_at,omitempty"`
	DeletedAt           *time.Time         `json:"deleted_at,omitempty"`
	Lease               *Lease             `json:"lease,omitempty"`
	Result              *Result            `json:"result,omitempty"`
	FailureCode         string             `json:"failure_code,omitempty"`
	FailureSummary      string             `json:"failure_summary,omitempty"`
	RetryOfTaskID       string             `json:"retry_of_task_id,omitempty"`
	Snapshot            *ExecutionSnapshot `json:"snapshot,omitempty"`
}
type Event struct {
	TaskID   string          `json:"task_id"`
	Sequence uint64          `json:"sequence"`
	Type     string          `json:"type"`
	Status   Status          `json:"status"`
	Payload  json.RawMessage `json:"payload,omitempty"`
	At       time.Time       `json:"at"`
}
type CreateCommand struct {
	OwnerID        string
	Spec           TaskSpec
	IdempotencyKey string
	RequestDigest  string
	At             time.Time
}
type DeleteCommand struct {
	TaskID, OwnerID  string
	ExpectedRevision uint64
	At               time.Time
}

func Digest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (t Task) Validate() error {
	if !ValidUUID(t.ID) || !validStatus(t.Status) || t.Revision == 0 || t.CreatedAt.IsZero() || t.UpdatedAt.IsZero() || t.AvailableAt.IsZero() || t.CreatedAt.Location() != time.UTC || t.UpdatedAt.Location() != time.UTC || t.AvailableAt.Location() != time.UTC || t.Attempt > 1 {
		return ErrInvalid
	}
	if err := t.Spec.Validate(); err != nil {
		return err
	}
	if t.Snapshot != nil {
		if err := t.Snapshot.Validate(); err != nil {
			return err
		}
	}
	if t.RetryOfTaskID != "" && (!ValidUUID(t.RetryOfTaskID) || t.RetryOfTaskID == t.ID) {
		return ErrInvalid
	}
	if t.DeletedAt != nil && (t.DeletedAt.IsZero() || t.DeletedAt.Location() != time.UTC) {
		return ErrInvalid
	}
	if t.Status == StatusWaitingUser && t.Attempt != 1 {
		return ErrInvalid
	}
	if t.Status == StatusSucceeded {
		if t.Attempt != 1 || t.Result == nil || t.FailureCode != "" || t.FailureSummary != "" {
			return ErrInvalid
		}
		if err := t.Result.Validate(); err != nil {
			return err
		}
	}
	if t.Status == StatusRunning || t.Status == StatusQueued || t.Status == StatusWaitingUser {
		if t.Result != nil || t.FailureCode != "" || t.FailureSummary != "" {
			return ErrInvalid
		}
	}
	if t.Status == StatusFailed {
		if t.Attempt != 1 || strings.TrimSpace(t.FailureCode) == "" || len([]byte(t.FailureCode)) > 128 || !utf8.ValidString(t.FailureCode) || strings.TrimSpace(t.FailureSummary) == "" || len([]byte(t.FailureSummary)) > MaxSummaryBytes || !utf8.ValidString(t.FailureSummary) {
			return ErrInvalid
		}
		if t.Result != nil {
			if err := t.Result.Validate(); err != nil {
				return err
			}
		}
	}
	if t.Status == StatusCanceled && t.Result != nil {
		if err := t.Result.Validate(); err != nil {
			return err
		}
	}
	if t.Status == StatusRunning {
		if t.Attempt != 1 || t.Lease == nil || t.Lease.TaskID != t.ID || t.Lease.Attempt != t.Attempt || t.Lease.Epoch != t.LeaseEpoch || t.Lease.Holder == "" || len([]byte(t.Lease.Holder)) > MaxLeaseHolderBytes || !utf8.ValidString(t.Lease.Holder) || t.Lease.ExpiresAt.IsZero() || t.Lease.ExpiresAt.Location() != time.UTC {
			return ErrInvalid
		}
	} else if t.Lease != nil {
		return ErrInvalid
	}
	return nil
}

func validStatus(s Status) bool {
	switch s {
	case StatusQueued, StatusRunning, StatusSucceeded, StatusFailed, StatusWaitingUser, StatusCanceled:
		return true
	}
	return false
}

func (s TaskSpec) MutationDigest() (string, error) {
	n, err := s.Normalize()
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(n)
	if err != nil {
		return "", err
	}
	d := sha256.Sum256(b)
	return hex.EncodeToString(d[:]), nil
}

func CanonicalMutationDigest(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	var value any
	if err := json.Unmarshal(b, &value); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	value = stripSecrets(value)
	b, err = json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	d := sha256.Sum256(b)
	return hex.EncodeToString(d[:]), nil
}

func stripSecrets(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, value := range x {
			lk := strings.ToLower(k)
			if strings.Contains(lk, "secret") || strings.Contains(lk, "token") || strings.Contains(lk, "password") || strings.Contains(lk, "api_key") || strings.Contains(lk, "apikey") {
				encoded, _ := json.Marshal(value)
				digest := sha256.Sum256(encoded)
				out[k] = "sha256:" + hex.EncodeToString(digest[:])
				continue
			}
			out[k] = stripSecrets(value)
		}
		return out
	case []any:
		for i := range x {
			x[i] = stripSecrets(x[i])
		}
	}
	return v
}
