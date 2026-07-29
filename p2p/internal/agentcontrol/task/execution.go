package task

// Durable execution bindings and ledgers for the bounded Core agent loop.
// These types deliberately contain no secret material.  SecretRef identifies
// a separately protected profile secret; it is never resolved or serialized by
// task events/results.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const MaxAgentRounds = 8

type ModelProfileSnapshot struct {
	ProfileID       string   `json:"profile_id"`
	Revision        int64    `json:"revision"`
	Digest          string   `json:"digest"`
	SecretRef       string   `json:"secret_ref"`
	Provider        string   `json:"provider"`
	BaseURL         string   `json:"base_url"`
	Model           string   `json:"model"`
	SystemPrompt    string   `json:"system_prompt"`
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"top_p,omitempty"`
	MaxOutputTokens int      `json:"max_output_tokens"`
	ContextWindow   int      `json:"context_window"`
	ReasoningEffort string   `json:"reasoning_effort"`
}

type ExtensionExecutionSnapshot struct {
	Kind           ExtensionKind    `json:"kind"`
	InstallationID string           `json:"installation_id"`
	Revision       int64            `json:"revision"`
	VersionID      string           `json:"version_id"`
	Version        string           `json:"version"`
	ContentDigest  string           `json:"content_digest"`
	ArtifactDigest string           `json:"artifact_digest"`
	AllowedTools   []string         `json:"allowed_tools,omitempty"`
	Tools          []ToolDescriptor `json:"tools,omitempty"`
}

// ToolDescriptor is the exact provider-facing contract pinned with an
// extension version. InputSchema is canonical JSON and SchemaDigest is its
// SHA-256 digest; neither is re-discovered while a task is running.
type ToolDescriptor struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"input_schema"`
	SchemaDigest string          `json:"schema_digest"`
}

type KnowledgeExecutionSnapshot struct {
	SourceID      string `json:"source_id"`
	Revision      int64  `json:"revision"`
	ContentDigest string `json:"content_digest"`
	IndexDigest   string `json:"index_digest"`
	Ready         bool   `json:"ready"`
}

// AttachmentDescriptor is the authoritative immutable file reference used by
// a task. RelativePath is always beneath the configured Agent data root.
type AttachmentDescriptor struct {
	ID           string `json:"id"`
	RelativePath string `json:"relative_path"`
	Digest       string `json:"digest"`
	Size         int64  `json:"size"`
	MediaType    string `json:"media_type"`
}

type ExecutionSnapshot struct {
	Model       ModelProfileSnapshot         `json:"model"`
	Extensions  []ExtensionExecutionSnapshot `json:"extensions,omitempty"`
	Knowledge   []KnowledgeExecutionSnapshot `json:"knowledge,omitempty"`
	Attachments []AttachmentDescriptor       `json:"attachments,omitempty"`
	Digest      string                       `json:"digest"`
}

func (s ExecutionSnapshot) canonicalWithoutDigest() ([]byte, error) {
	s.Digest = ""
	return json.Marshal(s)
}

func (s ExecutionSnapshot) ComputeDigest() (string, error) {
	b, err := s.canonicalWithoutDigest()
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

func (s *ExecutionSnapshot) Seal() error {
	if s == nil {
		return ErrInvalid
	}
	normalizeSnapshot(s)
	d, err := s.ComputeDigest()
	if err != nil {
		return err
	}
	s.Digest = d
	return s.Validate()
}

func normalizeSnapshot(s *ExecutionSnapshot) {
	if s == nil {
		return
	}
	for i := range s.Extensions {
		s.Extensions[i].InstallationID = strings.TrimSpace(s.Extensions[i].InstallationID)
		s.Extensions[i].Version = strings.TrimSpace(s.Extensions[i].Version)
		for j := range s.Extensions[i].AllowedTools {
			s.Extensions[i].AllowedTools[j] = strings.TrimSpace(s.Extensions[i].AllowedTools[j])
		}
		for j := range s.Extensions[i].Tools {
			d := &s.Extensions[i].Tools[j]
			d.Name = strings.TrimSpace(d.Name)
			d.Description = strings.TrimSpace(d.Description)
			d.SchemaDigest = strings.ToLower(strings.TrimSpace(d.SchemaDigest))
			var value any
			if json.Unmarshal(d.InputSchema, &value) == nil {
				if canonical, err := json.Marshal(value); err == nil {
					d.InputSchema = canonical
				}
			}
		}
		sort.Slice(s.Extensions[i].Tools, func(a, b int) bool { return s.Extensions[i].Tools[a].Name < s.Extensions[i].Tools[b].Name })
	}
	for i := range s.Knowledge {
		s.Knowledge[i].SourceID = strings.TrimSpace(s.Knowledge[i].SourceID)
	}
	for i := range s.Attachments {
		s.Attachments[i].ID = strings.TrimSpace(s.Attachments[i].ID)
		s.Attachments[i].RelativePath = strings.TrimSpace(s.Attachments[i].RelativePath)
		s.Attachments[i].Digest = strings.ToLower(strings.TrimSpace(s.Attachments[i].Digest))
		s.Attachments[i].MediaType = strings.TrimSpace(s.Attachments[i].MediaType)
	}
}

func (s ExecutionSnapshot) Validate() error {
	if s.Digest == "" {
		return ErrInvalid
	}
	if s.Model.ProfileID != "" {
		if !ValidUUID(s.Model.ProfileID) || s.Model.Revision <= 0 || len(s.Model.Digest) != 64 || len(s.Model.SecretRef) == 0 || s.Model.Provider == "" || s.Model.BaseURL == "" || s.Model.Model == "" {
			return ErrInvalid
		}
		if _, err := hex.DecodeString(s.Model.Digest); err != nil {
			return ErrInvalid
		}
	}
	for _, e := range s.Extensions {
		if (e.Kind != ExtensionMCP && e.Kind != ExtensionSkill) || !ValidUUID(e.InstallationID) || e.Revision <= 0 || !ValidUUID(e.VersionID) || e.Version == "" || len(e.ContentDigest) != 64 || len(e.ArtifactDigest) != 64 {
			return ErrInvalid
		}
		if _, err := hex.DecodeString(e.ContentDigest); err != nil {
			return ErrInvalid
		}
		if _, err := hex.DecodeString(e.ArtifactDigest); err != nil {
			return ErrInvalid
		}
		descriptors := make(map[string]ToolDescriptor, len(e.Tools))
		for _, tool := range e.Tools {
			if tool.Name == "" || len(tool.InputSchema) == 0 || !json.Valid(tool.InputSchema) || len(tool.SchemaDigest) != 64 {
				return ErrInvalid
			}
			if _, exists := descriptors[tool.Name]; exists {
				return ErrConflict
			}
			var value map[string]any
			if json.Unmarshal(tool.InputSchema, &value) != nil || value == nil {
				return ErrInvalid
			}
			canonical, _ := json.Marshal(value)
			h := sha256.Sum256(canonical)
			if hex.EncodeToString(h[:]) != strings.ToLower(tool.SchemaDigest) {
				return ErrRevisionConflict
			}
			descriptors[tool.Name] = tool
		}
		for _, name := range e.AllowedTools {
			if name == "" {
				return ErrInvalid
			}
			if len(e.Tools) == 0 {
				return ErrRevisionConflict
			}
			if _, ok := descriptors[name]; !ok {
				return ErrRevisionConflict
			}
		}
	}
	seenTools := map[string]struct{}{}
	for _, e := range s.Extensions {
		for _, name := range e.AllowedTools {
			if _, exists := seenTools[name]; exists {
				return ErrConflict
			}
			seenTools[name] = struct{}{}
		}
	}
	for _, k := range s.Knowledge {
		if !ValidUUID(k.SourceID) || k.Revision <= 0 || !k.Ready || len(k.ContentDigest) != 64 || len(k.IndexDigest) != 64 {
			return ErrInvalid
		}
	}
	for _, a := range s.Attachments {
		if !ValidUUID(a.ID) || a.RelativePath == "" || a.Digest == "" || a.Size < 0 || a.MediaType == "" {
			return ErrInvalid
		}
		if err := (FileRef{Path: a.RelativePath, Digest: a.Digest, Size: a.Size}).Validate(); err != nil {
			return err
		}
	}
	d, err := s.ComputeDigest()
	if err != nil || d != s.Digest {
		return ErrInvalid
	}
	return nil
}

type ModelRoundState string

const (
	ModelRoundPrepared   ModelRoundState = "prepared"
	ModelRoundDispatched ModelRoundState = "dispatched"
	ModelRoundCompleted  ModelRoundState = "completed"
	ModelRoundUncertain  ModelRoundState = "uncertain"
)

type ModelRoundLedger struct {
	TaskID       string          `json:"task_id"`
	Attempt      uint32          `json:"attempt"`
	Round        uint32          `json:"round"`
	LeaseEpoch   uint64          `json:"lease_epoch"`
	TaskRevision uint64          `json:"task_revision"`
	InputDigest  string          `json:"input_digest"`
	State        ModelRoundState `json:"state"`
	Response     json.RawMessage `json:"response,omitempty"`
	ErrorCode    string          `json:"error_code,omitempty"`
	ErrorSummary string          `json:"error_summary,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

type ToolCallState string

const (
	ToolCallPrepared   ToolCallState = "prepared"
	ToolCallDispatched ToolCallState = "dispatched"
	ToolCallCompleted  ToolCallState = "completed"
	ToolCallUncertain  ToolCallState = "uncertain"
)

type ToolCallLedger struct {
	TaskID          string          `json:"task_id"`
	Attempt         uint32          `json:"attempt"`
	Round           uint32          `json:"round"`
	CallID          string          `json:"call_id"`
	LeaseEpoch      uint64          `json:"lease_epoch"`
	TaskRevision    uint64          `json:"task_revision"`
	ToolDigest      string          `json:"tool_digest"`
	ArgumentsDigest string          `json:"arguments_digest"`
	State           ToolCallState   `json:"state"`
	Result          json.RawMessage `json:"result,omitempty"`
	ErrorCode       string          `json:"error_code,omitempty"`
	ErrorSummary    string          `json:"error_summary,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

func (m ModelRoundLedger) Validate() error {
	if !ValidUUID(m.TaskID) || m.Attempt == 0 || m.Round >= MaxAgentRounds || m.LeaseEpoch == 0 || m.TaskRevision == 0 || len(m.InputDigest) != 64 || !validModelState(m.State) {
		return ErrInvalid
	}
	if m.State == ModelRoundCompleted && len(m.Response) == 0 {
		return ErrInvalid
	}
	if m.State == ModelRoundUncertain && (strings.TrimSpace(m.ErrorCode) == "" || strings.TrimSpace(m.ErrorSummary) == "" || len(m.Response) != 0) {
		return ErrInvalid
	}
	if len(m.Response) > MaxResultBytes || (len(m.Response) > 0 && !json.Valid(m.Response)) {
		return ErrInvalid
	}
	return nil
}
func (t ToolCallLedger) Validate() error {
	if !ValidUUID(t.TaskID) || t.Attempt == 0 || t.Round >= MaxAgentRounds || t.LeaseEpoch == 0 || t.TaskRevision == 0 || strings.TrimSpace(t.CallID) == "" || len(t.ToolDigest) != 64 || len(t.ArgumentsDigest) != 64 || !validToolState(t.State) {
		return ErrInvalid
	}
	if t.State == ToolCallCompleted && len(t.Result) == 0 {
		return ErrInvalid
	}
	if t.State == ToolCallUncertain && (strings.TrimSpace(t.ErrorCode) == "" || strings.TrimSpace(t.ErrorSummary) == "") {
		return ErrInvalid
	}
	if len(t.Result) > MaxResultBytes || (len(t.Result) > 0 && !json.Valid(t.Result)) {
		return ErrInvalid
	}
	return nil
}
func validModelState(s ModelRoundState) bool {
	return s == ModelRoundPrepared || s == ModelRoundDispatched || s == ModelRoundCompleted || s == ModelRoundUncertain
}
func validToolState(s ToolCallState) bool {
	return s == ToolCallPrepared || s == ToolCallDispatched || s == ToolCallCompleted || s == ToolCallUncertain
}

var ErrLedgerConflict = errors.New("coretask: ledger conflict")

func redactLedgerError(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%T", err)
}

type ModelRoundCommand struct {
	Fence
	Round       uint32
	InputDigest string
	At          time.Time
}
type ModelResponseCommand struct {
	Fence
	Round    uint32
	Response json.RawMessage
	At       time.Time
}
type ModelUncertainCommand struct {
	Fence
	Round                   uint32
	ErrorCode, ErrorSummary string
	At                      time.Time
}
type ToolCallCommand struct {
	Fence
	Round                               uint32
	CallID, ToolDigest, ArgumentsDigest string
	At                                  time.Time
}
type ToolResultCommand struct {
	Fence
	Round  uint32
	CallID string
	Result json.RawMessage
	At     time.Time
}
type ToolUncertainCommand struct {
	Fence
	Round                           uint32
	CallID, ErrorCode, ErrorSummary string
	At                              time.Time
}

type AgentLedgerRepository interface {
	PrepareModelRound(context.Context, ModelRoundCommand) (ModelRoundLedger, error)
	MarkModelDispatched(context.Context, ModelRoundCommand) (ModelRoundLedger, error)
	CompleteModelRound(context.Context, ModelResponseCommand) (ModelRoundLedger, error)
	MarkModelUncertain(context.Context, ModelUncertainCommand) (ModelRoundLedger, error)
	PrepareToolCall(context.Context, ToolCallCommand) (ToolCallLedger, error)
	MarkToolDispatched(context.Context, ToolCallCommand) (ToolCallLedger, error)
	CompleteToolCall(context.Context, ToolResultCommand) (ToolCallLedger, error)
	MarkToolUncertain(context.Context, ToolUncertainCommand) (ToolCallLedger, error)
	GetModelRound(context.Context, string, uint32, uint32) (ModelRoundLedger, error)
	GetToolCall(context.Context, string, uint32, uint32, string) (ToolCallLedger, error)
}
