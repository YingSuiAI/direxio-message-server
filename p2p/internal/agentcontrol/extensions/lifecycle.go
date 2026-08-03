package extensions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	coretask "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
	"github.com/google/uuid"
)

type Store interface {
	Get(context.Context, string, string) (Installation, error)
	List(context.Context, string, int, string) ([]Installation, string, error)
	Put(context.Context, Installation) error
	PutCAS(context.Context, Installation, int64) error
	Replay(context.Context, string, string, string) (LifecycleResult, bool, error)
	SaveReplay(context.Context, string, string, string, LifecycleResult) error
}

type MemoryStore struct {
	mu     sync.Mutex
	items  map[string]Installation
	replay map[string]replayRecord
}
type replayRecord struct {
	owner, digest string
	result        LifecycleResult
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{items: map[string]Installation{}, replay: map[string]replayRecord{}}
}
func cloneInstallation(i Installation) Installation {
	i.Versions = append([]Version(nil), i.Versions...)
	for n := range i.Versions {
		i.Versions[n].NetworkGrants = append([]NetworkGrant(nil), i.Versions[n].NetworkGrants...)
		i.Versions[n].SecretGrants = append([]SecretGrant(nil), i.Versions[n].SecretGrants...)
		i.Versions[n].Tools = append([]Tool(nil), i.Versions[n].Tools...)
	}
	return i
}
func (s *MemoryStore) Get(_ context.Context, owner, id string) (Installation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.items[owner+"\x00"+id]
	if !ok {
		return Installation{}, ErrNotFound
	}
	return cloneInstallation(i), nil
}
func (s *MemoryStore) List(_ context.Context, owner string, limit int, token string) ([]Installation, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	owner, token = strings.TrimSpace(owner), strings.TrimSpace(token)
	if limit <= 0 || limit > 100 || owner == "" {
		return nil, "", ErrInvalid
	}
	out := make([]Installation, 0)
	prefix := owner + "\x00"
	for k, v := range s.items {
		if strings.HasPrefix(k, prefix) && v.ID > token {
			out = append(out, cloneInstallation(v))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		return out[:limit], out[limit-1].ID, nil
	}
	return out, "", nil
}
func (s *MemoryStore) Put(_ context.Context, i Installation) error {
	if i.Validate() != nil {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[i.OwnerID+"\x00"+i.ID] = cloneInstallation(i)
	return nil
}
func (s *MemoryStore) PutCAS(_ context.Context, i Installation, expected int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := i.OwnerID + "\x00" + i.ID
	old, ok := s.items[key]
	if expected > 0 && (!ok || old.Revision != expected) {
		return ErrRevisionConflict
	}
	if i.Validate() != nil {
		return ErrInvalid
	}
	s.items[key] = cloneInstallation(i)
	return nil
}
func (s *MemoryStore) Replay(_ context.Context, owner, key, digest string) (LifecycleResult, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.replay[owner+"\x00"+key]
	if !ok {
		return LifecycleResult{}, false, nil
	}
	if r.owner != owner || r.digest != digest {
		return LifecycleResult{}, true, ErrIdempotencyConflict
	}
	return r.result, true, nil
}
func (s *MemoryStore) SaveReplay(_ context.Context, owner, key, digest string, r LifecycleResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := owner + "\x00" + key
	if old, ok := s.replay[k]; ok && old.digest != digest {
		return ErrIdempotencyConflict
	}
	s.replay[k] = replayRecord{owner: owner, digest: digest, result: r}
	return nil
}

type Service struct {
	Store         Store
	Confirmations ConfirmationPort
	Tasks         TaskPort
	Secrets       SecretResolver
	SecretWriter  SecretStager
	Now           func() time.Time
}

func (s *Service) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
func (s *Service) validate() error {
	if s == nil || s.Store == nil || s.Confirmations == nil || s.Tasks == nil {
		return ErrInvalid
	}
	return nil
}

func (s *Service) Install(ctx context.Context, m Mutation) (LifecycleResult, error) {
	return s.mutate(ctx, m, "install")
}
func (s *Service) Update(ctx context.Context, m Mutation) (LifecycleResult, error) {
	return s.mutate(ctx, m, "update")
}
func (s *Service) mutate(ctx context.Context, m Mutation, op string) (LifecycleResult, error) {
	if m.Candidate.Kind == KindSkill {
		return LifecycleResult{}, ErrLocalExecutionDisabled
	}
	if err := bindSecretInputs(&m); err != nil {
		return LifecycleResult{}, err
	}
	if s.validate() != nil || strings.TrimSpace(m.OwnerID) == "" || !validUUID(m.IdempotencyKey) || m.Candidate.Validate() != nil || m.Inspection.Validate() != nil {
		return LifecycleResult{}, ErrInvalid
	}
	if m.Candidate.ID != m.Inspection.Candidate.ID || m.Candidate.Transport != TransportRemote {
		return LifecycleResult{}, ErrInvalid
	}
	if m.Inspection.ContentDigest != DigestBytes(m.Content) && len(m.Content) > 0 {
		return LifecycleResult{}, ErrInvalid
	}
	digest := mutationDigest(m, op)
	if prior, found, e := s.Store.Replay(ctx, m.OwnerID, m.IdempotencyKey, digest); found || e != nil {
		return prior, e
	}
	old := Installation{}
	if op == "update" {
		var e error
		old, e = s.Store.Get(ctx, m.OwnerID, m.InstallationID)
		if e != nil {
			return LifecycleResult{}, e
		}
		if old.Revision != m.ExpectedRevision {
			return LifecycleResult{}, ErrRevisionConflict
		}
		if old.Candidate.ID != m.Candidate.ID {
			return LifecycleResult{}, ErrConflict
		}
	}
	id := m.InstallationID
	if id == "" {
		id = uuid.NewString()
	}
	rev := int64(1)
	if op == "update" {
		rev = old.Revision + 1
	}
	version := Version{VersionID: uuid.NewString(), Pin: m.Candidate.Pin, ContentDigest: m.Inspection.ContentDigest, ManifestDigest: m.Inspection.ManifestDigest, ExecutionDigest: m.Inspection.ExecutionDigest, NetworkDigest: m.Inspection.NetworkDigest, SecretDigest: m.Inspection.SecretDigest, Execution: m.Inspection.Execution, NetworkGrants: append([]NetworkGrant(nil), m.Inspection.NetworkGrants...), SecretGrants: append([]SecretGrant(nil), m.Inspection.SecretGrants...), Tools: append([]Tool(nil), m.Inspection.Tools...), CreatedAt: s.now()}
	inst := Installation{ID: id, OwnerID: m.OwnerID, Candidate: m.Candidate, Revision: rev, State: "installing", Versions: []Version{version}, CreatedAt: s.now(), UpdatedAt: s.now()}
	if op == "update" {
		inst.Versions = append(append([]Version(nil), old.Versions...), version)
		inst.ActiveVersionID = old.ActiveVersionID
	}
	if inst.Validate() != nil {
		return LifecycleResult{}, ErrInvalid
	}
	if len(m.SecretInputs) > 0 {
		if s.SecretWriter == nil && !supportsAtomic(s.Store) {
			return LifecycleResult{}, ErrUnavailable
		}
		if err := validateSecretInputs(m); err != nil {
			return LifecycleResult{}, err
		}
		if s.SecretWriter != nil && !supportsAtomic(s.Store) {
			if err := s.SecretWriter.Stage(ctx, m.OwnerID, inst.ID, inst.Revision, version.VersionID, m.SecretInputs); err != nil {
				return LifecycleResult{}, err
			}
		}
	}
	taskID := uuid.NewString()
	binding := makeBinding(m.OwnerID, op, inst, version)
	exp := s.now().Add(time.Hour)
	if atomic, ok := s.Store.(AtomicLifecycleStore); ok && atomic.SupportsAtomicLifecycle() {
		expected := int64(0)
		if op == "update" {
			expected = old.Revision
		}
		return atomic.CommitLifecycle(ctx, AtomicLifecycleRequest{OwnerID: m.OwnerID, IdempotencyKey: m.IdempotencyKey, Operation: op, MutationDigest: digest, ExpectedRevision: expected, Installation: inst, SecretInputs: m.SecretInputs, Task: TaskRequest{OwnerID: m.OwnerID, TaskID: taskID, IdempotencyKey: m.IdempotencyKey, Goal: "extension " + op, Payload: mustJSON(struct{ InstallationID, VersionID string }{inst.ID, version.VersionID})}, Confirmation: ConfirmationRequest{OwnerID: m.OwnerID, IdempotencyKey: m.IdempotencyKey, TaskID: taskID, Binding: binding, ExpiresAt: exp}})
	}
	tr, err := s.Tasks.CreateWaitingUser(ctx, TaskRequest{OwnerID: m.OwnerID, TaskID: taskID, IdempotencyKey: m.IdempotencyKey, Goal: "extension " + op, Payload: mustJSON(struct{ InstallationID, VersionID string }{inst.ID, version.VersionID})})
	if err != nil {
		return LifecycleResult{}, err
	}
	if tr.ID != "" {
		taskID = tr.ID
	}
	c, err := s.Confirmations.Request(ctx, ConfirmationRequest{OwnerID: m.OwnerID, IdempotencyKey: m.IdempotencyKey, TaskID: taskID, Binding: binding, ExpiresAt: exp})
	if err != nil {
		return LifecycleResult{}, err
	}
	res := LifecycleResult{Installation: inst, TaskID: taskID, ConfirmationID: c.ID}
	expected := int64(0)
	if op == "update" {
		expected = old.Revision
	}
	if err = s.Store.PutCAS(ctx, inst, expected); err != nil {
		return LifecycleResult{}, err
	}
	if err = s.Store.SaveReplay(ctx, m.OwnerID, m.IdempotencyKey, digest, res); err != nil {
		return LifecycleResult{}, err
	}
	return res, nil
}
func supportsAtomic(s Store) bool {
	a, ok := s.(AtomicLifecycleStore)
	return ok && a.SupportsAtomicLifecycle()
}

func (s *Service) Uninstall(ctx context.Context, owner, id, key string, rev int64) (LifecycleResult, error) {
	if s.validate() != nil || strings.TrimSpace(owner) == "" || !validUUID(key) {
		return LifecycleResult{}, ErrInvalid
	}
	i, e := s.Store.Get(ctx, owner, id)
	if e != nil {
		return LifecycleResult{}, e
	}
	if i.Revision != rev {
		return LifecycleResult{}, ErrRevisionConflict
	}
	if i.ActiveVersionID == "" && len(i.Versions) > 0 {
		i.ActiveVersionID = i.Versions[len(i.Versions)-1].VersionID
	}
	i.Revision++
	i.State = "uninstalling"
	i.UpdatedAt = s.now()
	var v Version
	for _, x := range i.Versions {
		if x.VersionID == i.ActiveVersionID {
			v = x
		}
	}
	digest := mutationDigest(Mutation{OwnerID: owner, IdempotencyKey: key, InstallationID: id, ExpectedRevision: rev, Candidate: i.Candidate, Inspection: Inspection{Candidate: i.Candidate, ContentDigest: v.ContentDigest, ManifestDigest: v.ManifestDigest, ExecutionDigest: v.ExecutionDigest, NetworkDigest: v.NetworkDigest, SecretDigest: v.SecretDigest, Execution: v.Execution, NetworkGrants: v.NetworkGrants, SecretGrants: v.SecretGrants}}, "uninstall")
	if prior, found, er := s.Store.Replay(ctx, owner, key, digest); found || er != nil {
		return prior, er
	}
	taskID := uuid.NewString()
	if atomic, ok := s.Store.(AtomicLifecycleStore); ok && atomic.SupportsAtomicLifecycle() {
		return atomic.CommitUninstall(ctx, AtomicLifecycleRequest{OwnerID: owner, IdempotencyKey: key, Operation: "uninstall", MutationDigest: digest, ExpectedRevision: rev, Installation: i, Task: TaskRequest{OwnerID: owner, TaskID: taskID, IdempotencyKey: key, Goal: "extension uninstall"}, Confirmation: ConfirmationRequest{OwnerID: owner, IdempotencyKey: key, TaskID: taskID, Binding: makeBinding(owner, "uninstall", i, v), ExpiresAt: s.now().Add(time.Hour)}})
	}
	tr, er := s.Tasks.CreateWaitingUser(ctx, TaskRequest{OwnerID: owner, TaskID: taskID, IdempotencyKey: key, Goal: "extension uninstall"})
	if er != nil {
		return LifecycleResult{}, er
	}
	if tr.ID != "" {
		taskID = tr.ID
	}
	c, er := s.Confirmations.Request(ctx, ConfirmationRequest{OwnerID: owner, IdempotencyKey: key, TaskID: taskID, Binding: makeBinding(owner, "uninstall", i, v), ExpiresAt: s.now().Add(time.Hour)})
	if er != nil {
		return LifecycleResult{}, er
	}
	res := LifecycleResult{Installation: i, TaskID: taskID, ConfirmationID: c.ID}
	if er = s.Store.PutCAS(ctx, i, rev); er != nil {
		return LifecycleResult{}, er
	}
	er = s.Store.SaveReplay(ctx, owner, key, digest, res)
	return res, er
}

func (s *Service) ConsumeAndRun(ctx context.Context, owner string, c ConsumeRequest) (Confirmation, error) {
	if s.validate() != nil {
		return Confirmation{}, ErrInvalid
	}
	got, e := s.Confirmations.Consume(ctx, c)
	if e != nil {
		return Confirmation{}, e
	}
	if got.OwnerID != "" && got.OwnerID != owner || !got.Binding.Equal(c.Binding) {
		return Confirmation{}, ErrConflict
	}
	t, e := s.Store.Get(ctx, owner, c.Binding.TargetID)
	if e != nil {
		return Confirmation{}, e
	}
	for _, v := range t.Versions {
		if v.VersionID == c.Binding.VersionID && v.ContentDigest == c.Binding.ContentDigest && v.ManifestDigest == c.Binding.ManifestDigest && v.ExecutionDigest == c.Binding.ExecutionDigest && v.NetworkDigest == c.Binding.NetworkDigest && v.SecretDigest == c.Binding.SecretDigest {
			if e = s.Tasks.Run(ctx, Task{ID: got.TaskID, OwnerID: owner, Status: "running", Revision: uint64(c.ExpectedRevision)}, got); e != nil {
				// A consumed confirmation is a non-idempotent fence. Once the
				// provider result is lost or malformed, preserve uncertainty and
				// never submit the same call again.
				if errors.Is(e, ErrMCPUnavailable) || errors.Is(e, ErrMCPProtocol) || errors.Is(e, ErrUncertain) {
					_ = s.Tasks.MarkUncertain(ctx, Task{ID: got.TaskID, OwnerID: owner, Status: "running", Revision: uint64(c.ExpectedRevision)}, e.Error())
					return got, ErrUncertain
				}
				return got, e
			}
			return got, nil
		}
	}
	return got, ErrConflict
}

func (s *Service) RequestExecute(ctx context.Context, r ExecuteRequest) (ExecuteResult, error) {
	if s.validate() != nil || strings.TrimSpace(r.OwnerID) == "" || !validUUID(r.IdempotencyKey) || !validUUID(r.InstallationID) || strings.TrimSpace(r.ToolName) == "" {
		return ExecuteResult{}, ErrInvalid
	}
	canonicalInput, err := CanonicalizeInput(r.Input)
	if err != nil {
		return ExecuteResult{}, err
	}
	i, err := s.Store.Get(ctx, r.OwnerID, r.InstallationID)
	if err != nil {
		return ExecuteResult{}, err
	}
	if i.State != "installed" || i.Revision != r.ExpectedRevision || i.ActiveVersionID == "" {
		return ExecuteResult{}, ErrRevisionConflict
	}
	var v Version
	found := false
	for _, x := range i.Versions {
		if x.VersionID == i.ActiveVersionID {
			v = x
			found = true
		}
	}
	if !found {
		return ExecuteResult{}, ErrConflict
	}
	var tool Tool
	for _, x := range v.Tools {
		if x.Name == r.ToolName {
			tool = x
		}
	}
	if tool.Name == "" {
		return ExecuteResult{}, ErrNotFound
	}
	binding, err := ExecutionBinding(r.OwnerID, i, v, r.ToolName, canonicalInput)
	if err != nil {
		return ExecuteResult{}, err
	}
	taskID := uuid.NewString()
	if atomic, ok := s.Store.(AtomicLifecycleStore); ok && atomic.SupportsAtomicLifecycle() {
		return atomic.CommitExecute(ctx, ExecuteAtomicRequest{OwnerID: r.OwnerID, IdempotencyKey: r.IdempotencyKey, Installation: i, Version: v, Tool: tool, Input: append([]byte(nil), canonicalInput...), Task: TaskRequest{OwnerID: r.OwnerID, TaskID: taskID, IdempotencyKey: r.IdempotencyKey, Goal: "extension tool " + r.ToolName, Payload: canonicalInput}, Confirmation: ConfirmationRequest{OwnerID: r.OwnerID, IdempotencyKey: r.IdempotencyKey, TaskID: taskID, Binding: binding, ExpiresAt: s.now().Add(time.Hour)}})
	}
	tr, err := s.Tasks.CreateWaitingUser(ctx, TaskRequest{OwnerID: r.OwnerID, TaskID: taskID, IdempotencyKey: r.IdempotencyKey, Goal: "extension tool " + r.ToolName, Payload: mustJSON(struct{ InstallationID, VersionID, ToolName, InputDigest string }{i.ID, v.VersionID, r.ToolName, DigestBytes(canonicalInput)})})
	if err != nil {
		return ExecuteResult{}, err
	}
	if tr.ID != "" {
		taskID = tr.ID
	}
	c, err := s.Confirmations.Request(ctx, ConfirmationRequest{OwnerID: r.OwnerID, IdempotencyKey: r.IdempotencyKey, TaskID: taskID, Binding: binding, ExpiresAt: s.now().Add(time.Hour)})
	if err != nil {
		return ExecuteResult{}, err
	}
	return ExecuteResult{TaskID: taskID, ConfirmationID: c.ID, InstallationID: i.ID, VersionID: v.VersionID}, nil
}

func makeBinding(owner, op string, i Installation, v Version) ConfirmationBinding {
	n := make([]string, len(v.NetworkGrants))
	for x, g := range v.NetworkGrants {
		n[x] = fmt.Sprintf("%s://%s:%d%s:%s", g.Scheme, g.Host, g.Port, g.PathPrefix, g.Digest)
	}
	sort.Strings(n)
	secrets := append([]SecretGrant(nil), v.SecretGrants...)
	sort.Slice(secrets, func(left, right int) bool {
		if secrets[left].ReferenceID != secrets[right].ReferenceID {
			return secrets[left].ReferenceID < secrets[right].ReferenceID
		}
		if secrets[left].Purpose != secrets[right].Purpose {
			return secrets[left].Purpose < secrets[right].Purpose
		}
		return secrets[left].BindingDigest < secrets[right].BindingDigest
	})
	return ConfirmationBinding{OwnerID: owner, Operation: op, TargetID: i.ID, VersionID: v.VersionID, SourceVersion: v.Pin.RegistryVersion, SourceCommit: v.Pin.GitCommit, TargetRevision: i.Revision, ParameterDigest: v.ContentDigest, ContentDigest: v.ContentDigest, ManifestDigest: v.ManifestDigest, ExecutionDigest: v.ExecutionDigest, NetworkDigest: v.NetworkDigest, SecretDigest: v.SecretDigest, NetworkGrants: n, SecretGrants: secrets}
}

// CanonicalizeInput applies the same JSON normalization used by durable task
// payloads. Confirmation must cover this exact byte representation rather
// than the caller's original whitespace/key ordering.
func CanonicalizeInput(raw []byte) ([]byte, error) {
	if len(raw) == 0 || len(raw) > coretask.MaxCanonicalInputBytes || !json.Valid(raw) {
		return nil, ErrInvalid
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, ErrInvalid
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, ErrInvalid
	}
	return canonical, nil
}

// ExecutionBinding derives the immutable confirmation facts for one tool
// call. ParameterDigest covers canonical input and ToolSchemaDigest covers
// the exact pinned schema selected from the immutable version.
func ExecutionBinding(owner string, i Installation, v Version, toolName string, input []byte) (ConfirmationBinding, error) {
	toolName = strings.TrimSpace(toolName)
	if toolName == "" {
		return ConfirmationBinding{}, ErrInvalid
	}
	canonical, err := CanonicalizeInput(input)
	if err != nil {
		return ConfirmationBinding{}, err
	}
	if ValidatePinnedTools(v.Tools) != nil {
		return ConfirmationBinding{}, ErrInvalid
	}
	var tool Tool
	for _, candidate := range v.Tools {
		if candidate.Name == toolName {
			tool = candidate
			break
		}
	}
	if tool.Name == "" || !validDigest(tool.InputSchemaDigest) {
		return ConfirmationBinding{}, ErrInvalid
	}
	binding := makeBinding(owner, "execute", i, v)
	binding.ToolName = tool.Name
	binding.ToolSchemaDigest = tool.InputSchemaDigest
	binding.ParameterDigest = DigestBytes(canonical)
	return binding, nil
}

func mutationDigest(m Mutation, op string) string {
	return DigestBytes(mustJSON(struct {
		Owner, Key, ID, Op string
		Revision           int64
		Candidate          Candidate
		Inspection         Inspection
	}{m.OwnerID, m.IdempotencyKey, m.InstallationID, op, m.ExpectedRevision, m.Candidate, m.Inspection}))
}
func validateSecretInputs(m Mutation) error {
	seen := map[string]bool{}
	for _, in := range m.SecretInputs {
		if !validUUID(in.ReferenceID) || in.Purpose != "mcp_credential" || in.Value == "" {
			return ErrInvalid
		}
		key := in.ReferenceID + "\x00" + in.Purpose
		if seen[key] {
			return ErrInvalid
		}
		seen[key] = true
		found := false
		for _, g := range m.Inspection.SecretGrants {
			if g.ReferenceID == in.ReferenceID && g.Purpose == in.Purpose && g.BindingDigest == DigestBytes([]byte(in.Value)) && g.Configured {
				found = true
				break
			}
		}
		if !found {
			return ErrInvalid
		}
	}
	if len(seen) != len(m.Inspection.SecretGrants) {
		return ErrInvalid
	}
	return nil
}

// bindSecretInputs converts an inspected, unconfigured secret descriptor into
// the immutable configured grant persisted with a version. The write-only
// value never enters a DTO or event; only its SHA-256 fingerprint is bound to
// the confirmation and secret envelope.
func bindSecretInputs(m *Mutation) error {
	if m == nil {
		return ErrInvalid
	}
	inputs := make(map[string]SecretInput, len(m.SecretInputs))
	for _, input := range m.SecretInputs {
		if !validUUID(input.ReferenceID) || input.Purpose != "mcp_credential" || input.Value == "" {
			return ErrInvalid
		}
		key := input.ReferenceID + "\x00" + input.Purpose
		if _, exists := inputs[key]; exists {
			return ErrInvalid
		}
		inputs[key] = input
	}
	if len(inputs) != len(m.Inspection.SecretGrants) {
		return ErrInvalid
	}
	grants := append([]SecretGrant(nil), m.Inspection.SecretGrants...)
	for index, grant := range grants {
		if grant.Validate() != nil || grant.Purpose != "mcp_credential" {
			return ErrInvalid
		}
		key := grant.ReferenceID + "\x00" + grant.Purpose
		input, exists := inputs[key]
		if !exists {
			return ErrInvalid
		}
		grants[index].BindingDigest = DigestBytes([]byte(input.Value))
		grants[index].Configured = true
		delete(inputs, key)
	}
	if len(inputs) != 0 {
		return ErrInvalid
	}
	m.Inspection.SecretGrants = grants
	return nil
}
func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
