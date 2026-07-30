package storage

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrScheduleNotFound    = errors.New("schedule not found")
	ErrScheduleConflict    = errors.New("schedule conflict")
	ErrScheduleClaimed     = errors.New("schedule lease lost")
	ErrScheduleIdempotency = errors.New("schedule idempotency conflict")
)

type memoryScheduleMutation struct {
	Digest   [32]byte
	Response []byte
}

type Schedule struct {
	ScheduleID           string     `json:"schedule_id"`
	OwnerID              string     `json:"owner_id"`
	Name                 string     `json:"name"`
	Prompt               string     `json:"prompt"`
	TriggerKind          string     `json:"trigger_kind"`
	TriggerValue         string     `json:"trigger_value"`
	Timezone             string     `json:"timezone"`
	SkipIfRunning        bool       `json:"skip_if_running"`
	Status               string     `json:"status"`
	Revision             int64      `json:"revision"`
	ModelProfileID       string     `json:"model_profile_id"`
	ModelProfileRevision int64      `json:"model_profile_revision"`
	CredentialVersion    int64      `json:"credential_version"`
	NextRunAt            *time.Time `json:"next_run_at,omitempty"`
	LatestRunAt          *time.Time `json:"latest_run_at,omitempty"`
	LeaseOwner           string     `json:"lease_owner,omitempty"`
	LeaseUntil           *time.Time `json:"lease_until,omitempty"`
	LeaseEpoch           int64      `json:"lease_epoch"`
	// TaskTemplate is the generic Agent task projection. Legacy prompt/trigger
	// fields remain populated for existing agent.schedules clients.
	TaskTemplate json.RawMessage `json:"task_template,omitempty"`
	// CoreState and TriggerJSON are lossless projections for agent.core.schedules.
	// Legacy agent.schedules continues to use Status/TriggerKind/TriggerValue.
	CoreState   string          `json:"core_state,omitempty"`
	TriggerJSON json.RawMessage `json:"trigger,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}
type ScheduleRun struct {
	RunID        string     `json:"run_id"`
	ScheduleID   string     `json:"schedule_id"`
	OwnerID      string     `json:"owner_id"`
	Status       string     `json:"status"`
	ScheduledFor time.Time  `json:"scheduled_for"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	Result       string     `json:"result,omitempty"`
	Error        string     `json:"error,omitempty"`
	LeaseEpoch   int64      `json:"lease_epoch"`
}
type ScheduleTombstone struct {
	ScheduleID string
	Deleted    bool
	Revision   int64
}
type SchedulePage struct {
	Schedules  []Schedule
	NextCursor string
}
type ScheduleRunPage struct {
	Runs       []ScheduleRun
	NextCursor string
}

// ScheduleConfirmation is a durable owner/conversation-scoped approval for a
// mutating embedded schedule action. ParamsJSON and ResultJSON are always
// secret-free, bounded JSON projections.
type ScheduleConfirmation struct {
	ConfirmationID string
	OwnerID        string
	ConversationID string
	Action         string
	ParamsJSON     []byte
	RequestDigest  [32]byte
	IdempotencyKey string
	Summary        string
	ApprovalCode   string
	Status         string
	Revision       int64
	ExpiresAt      time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ResultJSON     []byte
	Error          string
}

var ErrScheduleConfirmationNotFound = errors.New("schedule confirmation not found")
var ErrScheduleConfirmationConflict = errors.New("schedule confirmation conflict")

type ScheduleConfirmationStore interface {
	ReserveScheduleConfirmation(context.Context, ScheduleConfirmation) (ScheduleConfirmation, bool, error)
	GetScheduleConfirmation(context.Context, string, string, string) (ScheduleConfirmation, bool, error)
	ClaimScheduleConfirmation(context.Context, string, string, string, int64, time.Time) (ScheduleConfirmation, error)
	CompleteScheduleConfirmation(context.Context, string, string, string, int64, string, []byte, string) error
}
type ScheduleStore interface {
	CreateSchedule(context.Context, Schedule, string) (Schedule, error)
	GetSchedule(context.Context, string, string) (Schedule, bool, error)
	GetScheduleIncludingDeleted(context.Context, string, string) (ScheduleTombstone, bool, error)
	ListSchedules(context.Context, string, int, string) (SchedulePage, error)
	UpdateSchedule(context.Context, Schedule, string) (Schedule, error)
	DeleteSchedule(context.Context, string, string, string) error
	SetScheduleStatus(context.Context, string, string, string, bool) (Schedule, error)
	ListScheduleRuns(context.Context, string, string, int, string) (ScheduleRunPage, error)
	GetScheduleRun(context.Context, string, string, string) (ScheduleRun, bool, error)
	ClaimDueSchedules(context.Context, time.Time, string, time.Duration, int) ([]Schedule, error)
	ClaimScheduleNow(context.Context, string, string, string, time.Duration) (Schedule, error)
	ListOverlappingSchedules(context.Context, time.Time, int) ([]Schedule, error)
	CoalesceSchedule(context.Context, string, string, string, int64, *time.Time) error
	RenewScheduleLease(context.Context, string, string, string, int64, time.Duration) error
	CreateScheduleRun(context.Context, ScheduleRun, string, int64, int64) (ScheduleRun, bool, error)
	AcquireScheduleRunRecoveryLease(context.Context, string, string, string, string, time.Duration) (Schedule, error)
	RecoverScheduleRun(context.Context, string, string, string, int64, time.Time) (ScheduleRun, error)
	FinishScheduleRun(context.Context, string, string, string, int64, string, string, time.Time) error
	AdvanceSchedule(context.Context, string, string, string, int64, int64, *time.Time, string) error
	UpdateScheduleCAS(context.Context, Schedule, int64) (Schedule, error)
	DeleteScheduleCAS(context.Context, string, string, int64) error
	SetScheduleStatusCAS(context.Context, string, string, int64, bool) (Schedule, error)
	GetScheduleMutation(context.Context, string, string, string) ([32]byte, []byte, bool, error)
	ReserveScheduleMutation(context.Context, string, string, string, [32]byte) ([32]byte, []byte, bool, error)
	PutScheduleMutation(context.Context, string, string, string, [32]byte, []byte) error
}

func encodeScheduleCursor(v string) string {
	if v == "" {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(v))
}
func decodeScheduleCursor(v string) (string, error) {
	if strings.TrimSpace(v) == "" {
		return "", nil
	}
	b, e := base64.RawURLEncoding.DecodeString(v)
	return string(b), e
}

type scheduleRunCursor struct {
	ScheduledFor time.Time `json:"scheduled_for"`
	RunID        string    `json:"run_id"`
}

func encodeScheduleRunCursor(scheduledFor time.Time, runID string) string {
	if runID == "" {
		return ""
	}
	b, _ := json.Marshal(scheduleRunCursor{ScheduledFor: scheduledFor.UTC(), RunID: runID})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeScheduleRunCursor(v string) (scheduleRunCursor, error) {
	if strings.TrimSpace(v) == "" {
		return scheduleRunCursor{}, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		return scheduleRunCursor{}, err
	}
	var cursor scheduleRunCursor
	if err := json.Unmarshal(b, &cursor); err != nil {
		return scheduleRunCursor{}, err
	}
	if cursor.RunID == "" || cursor.ScheduledFor.IsZero() {
		return scheduleRunCursor{}, errors.New("invalid schedule run cursor")
	}
	cursor.ScheduledFor = cursor.ScheduledFor.UTC()
	return cursor, nil
}

type memoryScheduleKey struct{ owner, id string }

func (s *MemoryStore) ensureSchedules() {
	if s.schedules == nil {
		s.schedules = make(map[memoryScheduleKey]Schedule)
	}
	if s.scheduleRuns == nil {
		s.scheduleRuns = make(map[string]ScheduleRun)
	}
	if s.scheduleMutations == nil {
		s.scheduleMutations = make(map[string]memoryScheduleMutation)
	}
	if s.scheduleConfirmations == nil {
		s.scheduleConfirmations = make(map[string]ScheduleConfirmation)
	}
}
func cloneSchedule(v Schedule) Schedule {
	v.TaskTemplate = append(json.RawMessage(nil), v.TaskTemplate...)
	v.TriggerJSON = append(json.RawMessage(nil), v.TriggerJSON...)
	if v.NextRunAt != nil {
		t := *v.NextRunAt
		v.NextRunAt = &t
	}
	if v.LatestRunAt != nil {
		t := *v.LatestRunAt
		v.LatestRunAt = &t
	}
	if v.LeaseUntil != nil {
		t := *v.LeaseUntil
		v.LeaseUntil = &t
	}
	return v
}
func cloneRun(v ScheduleRun) ScheduleRun {
	if v.StartedAt != nil {
		t := *v.StartedAt
		v.StartedAt = &t
	}
	if v.FinishedAt != nil {
		t := *v.FinishedAt
		v.FinishedAt = &t
	}
	return v
}
func compactScheduleJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return append(json.RawMessage(nil), raw...)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return append(json.RawMessage(nil), raw...)
	}
	return json.RawMessage(out)
}
func normalizeScheduleProjection(v *Schedule) {
	if v.CoreState == "" {
		if v.Status == "disabled" {
			v.CoreState = "paused"
		} else {
			v.CoreState = "active"
		}
	}
	if len(v.TaskTemplate) == 0 && (v.Prompt != "" || v.ModelProfileID != "") {
		v.TaskTemplate, _ = json.Marshal(map[string]any{"goal": v.Prompt, "model_profile_id": v.ModelProfileID})
	}
	if len(v.TriggerJSON) == 0 {
		kind := v.TriggerKind
		if kind == "one_time" {
			kind = "run_at"
		}
		if kind == "run_at" && v.TriggerValue != "" {
			v.TriggerJSON, _ = json.Marshal(map[string]any{"kind": kind, "run_at": v.TriggerValue})
		} else if kind == "cron" {
			v.TriggerJSON, _ = json.Marshal(map[string]any{"kind": kind, "expression": v.TriggerValue, "timezone": v.Timezone})
		}
	}
}
func (s *MemoryStore) CreateSchedule(_ context.Context, v Schedule, _ string) (Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureSchedules()
	if v.ScheduleID == "" {
		v.ScheduleID = uuid.NewString()
	}
	if v.Status == "" {
		v.Status = "enabled"
	}
	normalizeScheduleProjection(&v)
	if v.Revision == 0 {
		v.Revision = 1
	}
	now := time.Now().UTC()
	if v.CreatedAt.IsZero() {
		v.CreatedAt = now
	}
	v.UpdatedAt = now
	k := memoryScheduleKey{v.OwnerID, v.ScheduleID}
	if _, ok := s.schedules[k]; ok {
		return Schedule{}, ErrScheduleConflict
	}
	s.schedules[k] = cloneSchedule(v)
	return cloneSchedule(v), nil
}
func (s *MemoryStore) GetSchedule(_ context.Context, o, id string) (Schedule, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.schedules[memoryScheduleKey{o, id}]
	if !ok || v.Status == "deleted" {
		return Schedule{}, false, nil
	}
	return cloneSchedule(v), true, nil
}
func (s *MemoryStore) GetScheduleIncludingDeleted(_ context.Context, o, id string) (ScheduleTombstone, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.schedules[memoryScheduleKey{o, id}]
	if !ok {
		return ScheduleTombstone{}, false, nil
	}
	return ScheduleTombstone{ScheduleID: v.ScheduleID, Deleted: v.Status == "deleted", Revision: v.Revision}, true, nil
}
func (s *MemoryStore) ListSchedules(_ context.Context, o string, limit int, cursor string) (SchedulePage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	start, err := decodeScheduleCursor(cursor)
	if err != nil {
		return SchedulePage{}, err
	}
	a := []Schedule{}
	for k, v := range s.schedules {
		if k.owner == o && v.Status != "deleted" {
			a = append(a, cloneSchedule(v))
		}
	}
	sort.Slice(a, func(i, j int) bool { return a[i].ScheduleID < a[j].ScheduleID })
	if start != "" {
		i := 0
		for i < len(a) && a[i].ScheduleID <= start {
			i++
		}
		a = a[i:]
	}
	p := SchedulePage{Schedules: a}
	if len(a) > limit {
		p.Schedules = a[:limit]
		p.NextCursor = encodeScheduleCursor(a[limit-1].ScheduleID)
	}
	return p, nil
}
func (s *MemoryStore) UpdateSchedule(_ context.Context, v Schedule, _ string) (Schedule, error) {
	return s.UpdateScheduleCAS(context.Background(), v, v.Revision)
}
func (s *MemoryStore) UpdateScheduleCAS(_ context.Context, v Schedule, expected int64) (Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureSchedules()
	k := memoryScheduleKey{v.OwnerID, v.ScheduleID}
	old, ok := s.schedules[k]
	if !ok || old.Status == "deleted" {
		return Schedule{}, ErrScheduleNotFound
	}
	if expected != 0 && expected != old.Revision {
		return Schedule{}, ErrScheduleConflict
	}
	if old.Status == "running" {
		return Schedule{}, ErrScheduleConflict
	}
	v.Revision = old.Revision + 1
	if v.CoreState == "" {
		v.CoreState = old.CoreState
		if v.CoreState == "" {
			if v.Status == "disabled" {
				v.CoreState = "paused"
			} else {
				v.CoreState = "active"
			}
		}
	}
	normalizeScheduleProjection(&v)
	if len(v.TaskTemplate) == 0 {
		v.TaskTemplate = append(json.RawMessage(nil), old.TaskTemplate...)
	}
	if len(v.TriggerJSON) == 0 {
		v.TriggerJSON = append(json.RawMessage(nil), old.TriggerJSON...)
	}
	v.CreatedAt = old.CreatedAt
	v.UpdatedAt = time.Now().UTC()
	s.schedules[k] = cloneSchedule(v)
	return cloneSchedule(v), nil
}
func (s *MemoryStore) DeleteSchedule(_ context.Context, o, id, _ string) error {
	return s.DeleteScheduleCAS(context.Background(), o, id, 0)
}
func (s *MemoryStore) DeleteScheduleCAS(_ context.Context, o, id string, expected int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.schedules[memoryScheduleKey{o, id}]
	if !ok || v.Status == "deleted" {
		return ErrScheduleNotFound
	}
	if expected != 0 && v.Revision != expected {
		return ErrScheduleConflict
	}
	if v.Status == "running" {
		return ErrScheduleConflict
	}
	v.Status = "deleted"
	v.Revision++
	v.UpdatedAt = time.Now().UTC()
	s.schedules[memoryScheduleKey{o, id}] = v
	return nil
}
func (s *MemoryStore) SetScheduleStatus(_ context.Context, o, id, _ string, en bool) (Schedule, error) {
	return s.SetScheduleStatusCAS(context.Background(), o, id, 0, en)
}
func (s *MemoryStore) SetScheduleStatusCAS(_ context.Context, o, id string, expected int64, en bool) (Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.schedules[memoryScheduleKey{o, id}]
	if !ok || v.Status == "deleted" {
		return Schedule{}, ErrScheduleNotFound
	}
	if expected != 0 && v.Revision != expected {
		return Schedule{}, ErrScheduleConflict
	}
	if v.Status == "running" {
		return Schedule{}, ErrScheduleConflict
	}
	if en {
		v.Status = "enabled"
		v.CoreState = "active"
	} else {
		v.Status = "disabled"
		v.CoreState = "paused"
	}
	v.Revision++
	v.UpdatedAt = time.Now().UTC()
	s.schedules[memoryScheduleKey{o, id}] = v
	return cloneSchedule(v), nil
}

func scheduleMutationKey(owner, action, key string) string {
	return owner + "\x00" + action + "\x00" + key
}
func confirmationKey(owner, conversation, id string) string {
	return owner + "\x00" + conversation + "\x00" + id
}
func cloneScheduleConfirmation(v ScheduleConfirmation) ScheduleConfirmation {
	v.ParamsJSON = append([]byte(nil), v.ParamsJSON...)
	v.ResultJSON = append([]byte(nil), v.ResultJSON...)
	return v
}
func (s *MemoryStore) ReserveScheduleConfirmation(_ context.Context, v ScheduleConfirmation) (ScheduleConfirmation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureSchedules()
	for k, old := range s.scheduleConfirmations {
		if old.OwnerID == v.OwnerID && old.ConversationID == v.ConversationID && old.Status == "executing" {
			return ScheduleConfirmation{}, false, ErrScheduleConfirmationConflict
		}
		if old.OwnerID == v.OwnerID && old.ConversationID == v.ConversationID && old.Status == "pending" {
			if old.Action == v.Action && old.RequestDigest == v.RequestDigest && old.ExpiresAt.After(time.Now().UTC()) {
				return cloneScheduleConfirmation(old), false, nil
			}
			old.Status = "replaced"
			old.Revision++
			old.UpdatedAt = time.Now().UTC()
			s.scheduleConfirmations[k] = old
		}
	}
	if v.Revision <= 0 {
		v.Revision = 1
	}
	now := time.Now().UTC()
	if v.CreatedAt.IsZero() {
		v.CreatedAt = now
	}
	v.UpdatedAt = now
	s.scheduleConfirmations[confirmationKey(v.OwnerID, v.ConversationID, v.ConfirmationID)] = cloneScheduleConfirmation(v)
	return cloneScheduleConfirmation(v), true, nil
}
func (s *MemoryStore) GetScheduleConfirmation(_ context.Context, owner, conversation, id string) (ScheduleConfirmation, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.scheduleConfirmations[confirmationKey(owner, conversation, id)]
	if !ok {
		return ScheduleConfirmation{}, false, nil
	}
	return cloneScheduleConfirmation(v), true, nil
}
func (s *MemoryStore) ClaimScheduleConfirmation(_ context.Context, owner, conversation, id string, revision int64, now time.Time) (ScheduleConfirmation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.scheduleConfirmations[confirmationKey(owner, conversation, id)]
	if !ok {
		return ScheduleConfirmation{}, ErrScheduleConfirmationNotFound
	}
	if v.Status != "pending" || v.Revision != revision {
		if v.Status == "completed" || v.Status == "failed" {
			return cloneScheduleConfirmation(v), nil
		}
		return ScheduleConfirmation{}, ErrScheduleConfirmationConflict
	}
	if !v.ExpiresAt.After(now) {
		v.Status = "expired"
		v.Revision++
		v.UpdatedAt = now
		s.scheduleConfirmations[confirmationKey(owner, conversation, id)] = v
		return ScheduleConfirmation{}, ErrScheduleConfirmationConflict
	}
	v.Status = "executing"
	v.Revision++
	v.UpdatedAt = now
	s.scheduleConfirmations[confirmationKey(owner, conversation, id)] = v
	return cloneScheduleConfirmation(v), nil
}
func (s *MemoryStore) CompleteScheduleConfirmation(_ context.Context, owner, conversation, id string, revision int64, status string, result []byte, errText string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := confirmationKey(owner, conversation, id)
	v, ok := s.scheduleConfirmations[k]
	if !ok {
		return ErrScheduleConfirmationNotFound
	}
	if v.Status != "executing" || v.Revision != revision {
		return ErrScheduleConfirmationConflict
	}
	if status != "completed" {
		status = "failed"
	}
	v.Status = status
	v.Revision++
	v.ResultJSON = append([]byte(nil), result...)
	v.Error = errText
	v.UpdatedAt = time.Now().UTC()
	s.scheduleConfirmations[k] = v
	return nil
}
func (s *MemoryStore) GetScheduleMutation(_ context.Context, owner, action, key string) ([32]byte, []byte, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.scheduleMutations[scheduleMutationKey(owner, action, key)]
	return v.Digest, append([]byte(nil), v.Response...), ok, nil
}
func (s *MemoryStore) ReserveScheduleMutation(_ context.Context, owner, action, key string, digest [32]byte) ([32]byte, []byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureSchedules()
	k := scheduleMutationKey(owner, action, key)
	if old, ok := s.scheduleMutations[k]; ok {
		return old.Digest, append([]byte(nil), old.Response...), false, nil
	}
	s.scheduleMutations[k] = memoryScheduleMutation{Digest: digest}
	return digest, nil, true, nil
}
func (s *MemoryStore) PutScheduleMutation(_ context.Context, owner, action, key string, digest [32]byte, response []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureSchedules()
	k := scheduleMutationKey(owner, action, key)
	if old, ok := s.scheduleMutations[k]; ok {
		if old.Digest != digest {
			return ErrScheduleIdempotency
		}
		old.Response = append([]byte(nil), response...)
		s.scheduleMutations[k] = old
		return nil
	}
	s.scheduleMutations[k] = memoryScheduleMutation{Digest: digest, Response: append([]byte(nil), response...)}
	return nil
}
func (s *MemoryStore) ListScheduleRuns(_ context.Context, o, id string, limit int, cursor string) (ScheduleRunPage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	a := []ScheduleRun{}
	start, err := decodeScheduleRunCursor(cursor)
	if err != nil {
		return ScheduleRunPage{}, err
	}
	for _, v := range s.scheduleRuns {
		if v.OwnerID == o && v.ScheduleID == id {
			a = append(a, cloneRun(v))
		}
	}
	sort.Slice(a, func(i, j int) bool {
		if !a[i].ScheduledFor.Equal(a[j].ScheduledFor) {
			return a[i].ScheduledFor.After(a[j].ScheduledFor)
		}
		return a[i].RunID > a[j].RunID
	})
	if start.RunID != "" {
		i := 0
		for i < len(a) && (a[i].ScheduledFor.After(start.ScheduledFor) || (a[i].ScheduledFor.Equal(start.ScheduledFor) && a[i].RunID >= start.RunID)) {
			i++
		}
		a = a[i:]
	}
	p := ScheduleRunPage{Runs: a}
	if len(a) > limit {
		p.Runs = a[:limit]
		last := p.Runs[len(p.Runs)-1]
		p.NextCursor = encodeScheduleRunCursor(last.ScheduledFor, last.RunID)
	}
	return p, nil
}
func (s *MemoryStore) GetScheduleRun(_ context.Context, o, id, rid string) (ScheduleRun, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.scheduleRuns[rid]
	return cloneRun(v), ok && v.OwnerID == o && v.ScheduleID == id, nil
}
func (s *MemoryStore) ClaimDueSchedules(_ context.Context, now time.Time, worker string, lease time.Duration, limit int) ([]Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureSchedules()
	a := []Schedule{}
	for k, v := range s.schedules {
		if len(a) >= limit || v.CoreState == "paused" || (v.Status != "enabled" && !(v.Status == "running" && v.LeaseUntil != nil && !v.LeaseUntil.After(now))) || v.NextRunAt == nil || v.NextRunAt.After(now) {
			continue
		}
		v.Status = "running"
		v.Revision++
		v.LeaseEpoch++
		v.LeaseOwner = worker
		u := now.Add(lease)
		v.LeaseUntil = &u
		s.schedules[k] = v
		a = append(a, cloneSchedule(v))
	}
	return a, nil
}
func (s *MemoryStore) ClaimScheduleNow(_ context.Context, owner, id, worker string, lease time.Duration) (Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureSchedules()
	k := memoryScheduleKey{owner, id}
	v, ok := s.schedules[k]
	if !ok || v.Status == "deleted" {
		return Schedule{}, ErrScheduleNotFound
	}
	now := time.Now().UTC()
	if v.Status == "running" && v.LeaseUntil != nil && v.LeaseUntil.After(now) {
		return Schedule{}, ErrScheduleConflict
	}
	v.Status = "running"
	v.Revision++
	v.LeaseEpoch++
	v.LeaseOwner = worker
	u := now.Add(lease)
	v.LeaseUntil = &u
	v.UpdatedAt = now
	s.schedules[k] = v
	return cloneSchedule(v), nil
}
func (s *MemoryStore) ListOverlappingSchedules(_ context.Context, now time.Time, limit int) ([]Schedule, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []Schedule{}
	for _, v := range s.schedules {
		if len(out) >= limit {
			break
		}
		if v.Status == "running" && v.SkipIfRunning && v.NextRunAt != nil && !v.NextRunAt.After(now) && v.LeaseUntil != nil && v.LeaseUntil.After(now) {
			out = append(out, cloneSchedule(v))
		}
	}
	return out, nil
}
func (s *MemoryStore) CoalesceSchedule(_ context.Context, owner, id, leaseOwner string, epoch int64, next *time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := memoryScheduleKey{owner, id}
	v, ok := s.schedules[k]
	if !ok || v.Status != "running" || v.LeaseOwner != leaseOwner || v.LeaseEpoch != epoch || v.LeaseUntil == nil || !v.LeaseUntil.After(time.Now().UTC()) {
		return ErrScheduleClaimed
	}
	v.NextRunAt = next
	n := time.Now().UTC()
	v.LatestRunAt = &n
	v.Revision++
	v.UpdatedAt = n
	s.schedules[k] = v
	return nil
}
func (s *MemoryStore) RenewScheduleLease(_ context.Context, owner, id, leaseOwner string, epoch int64, lease time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := memoryScheduleKey{owner, id}
	v, ok := s.schedules[k]
	if !ok || v.Status != "running" || v.LeaseOwner != leaseOwner || v.LeaseEpoch != epoch || v.LeaseUntil == nil || !v.LeaseUntil.After(time.Now().UTC()) {
		return ErrScheduleClaimed
	}
	u := time.Now().UTC().Add(lease)
	v.LeaseUntil = &u
	v.UpdatedAt = time.Now().UTC()
	s.schedules[k] = v
	return nil
}
func (s *MemoryStore) CreateScheduleRun(_ context.Context, v ScheduleRun, leaseOwner string, revision, epoch int64) (ScheduleRun, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureSchedules()
	sch, ok := s.schedules[memoryScheduleKey{v.OwnerID, v.ScheduleID}]
	if !ok || sch.Status != "running" || sch.LeaseOwner != leaseOwner || sch.LeaseEpoch != epoch || sch.Revision != revision || sch.LeaseUntil == nil || !sch.LeaseUntil.After(time.Now().UTC()) {
		return ScheduleRun{}, false, ErrScheduleClaimed
	}
	if v.RunID == "" {
		v.RunID = uuid.NewString()
	}
	for _, existing := range s.scheduleRuns {
		if existing.OwnerID == v.OwnerID && existing.ScheduleID == v.ScheduleID && existing.ScheduledFor.Equal(v.ScheduledFor) {
			return cloneRun(existing), false, nil
		}
	}
	s.scheduleRuns[v.RunID] = cloneRun(v)
	return cloneRun(v), true, nil
}

// AcquireScheduleRunRecoveryLease fences an expired run before its deterministic
// replay can finish it. The schedule and exact run identity are checked while
// holding the same lock, so a stale worker cannot finish or advance afterward.
func (s *MemoryStore) AcquireScheduleRunRecoveryLease(_ context.Context, owner, scheduleID, runID, recoveryOwner string, lease time.Duration) (Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := memoryScheduleKey{owner, scheduleID}
	sch, ok := s.schedules[k]
	now := time.Now().UTC()
	if !ok || sch.Status != "running" || sch.LeaseUntil == nil || sch.LeaseUntil.After(now) {
		return Schedule{}, ErrScheduleClaimed
	}
	run, ok := s.scheduleRuns[runID]
	if !ok || run.OwnerID != owner || run.ScheduleID != scheduleID {
		return Schedule{}, ErrScheduleNotFound
	}
	if lease <= 0 {
		lease = 30 * time.Second
	}
	until := now.Add(lease)
	sch.Revision++
	sch.LeaseEpoch++
	sch.LeaseOwner = recoveryOwner
	sch.LeaseUntil = &until
	sch.UpdatedAt = now
	s.schedules[k] = sch
	return cloneSchedule(sch), nil
}
func (s *MemoryStore) RecoverScheduleRun(_ context.Context, owner, scheduleID, leaseOwner string, epoch int64, scheduledFor time.Time) (ScheduleRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sch, ok := s.schedules[memoryScheduleKey{owner, scheduleID}]
	if !ok || sch.Status != "running" || sch.LeaseOwner != leaseOwner || sch.LeaseEpoch != epoch {
		return ScheduleRun{}, ErrScheduleClaimed
	}
	for id, run := range s.scheduleRuns {
		if run.OwnerID == owner && run.ScheduleID == scheduleID && run.ScheduledFor.Equal(scheduledFor) {
			if run.Status == "running" {
				run.Status = "failed"
				run.Error = "schedule lease expired before completion"
				n := time.Now().UTC()
				run.FinishedAt = &n
				s.scheduleRuns[id] = run
			}
			return cloneRun(run), nil
		}
	}
	return ScheduleRun{}, ErrScheduleNotFound
}
func (s *MemoryStore) FinishScheduleRun(_ context.Context, o, rid, leaseOwner string, epoch int64, result, runErr string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.scheduleRuns[rid]
	sch, claimed := s.schedules[memoryScheduleKey{o, v.ScheduleID}]
	if !ok || v.OwnerID != o || v.LeaseEpoch != epoch || !claimed || sch.LeaseOwner != leaseOwner || sch.LeaseEpoch != epoch || sch.Status != "running" || sch.LeaseUntil == nil || !sch.LeaseUntil.After(time.Now().UTC()) {
		return ErrScheduleClaimed
	}
	v.Status = "succeeded"
	if runErr != "" {
		v.Status = "failed"
	}
	v.Result = result
	v.Error = runErr
	v.FinishedAt = &at
	s.scheduleRuns[rid] = v
	return nil
}
func (s *MemoryStore) AdvanceSchedule(_ context.Context, owner, id, leaseOwner string, revision, epoch int64, next *time.Time, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.schedules[memoryScheduleKey{owner, id}]
	if !ok || v.LeaseOwner != leaseOwner || v.LeaseEpoch != epoch || v.Status != "running" || v.LeaseUntil == nil || !v.LeaseUntil.After(time.Now().UTC()) {
		return ErrScheduleClaimed
	}
	v.NextRunAt = next
	now := time.Now().UTC()
	v.LatestRunAt = &now
	v.Status = status
	if status == "disabled" {
		v.CoreState = "paused"
	} else if status == "enabled" {
		v.CoreState = "active"
	}
	v.LeaseOwner = ""
	v.LeaseUntil = nil
	v.Revision++
	s.schedules[memoryScheduleKey{owner, id}] = v
	return nil
}

func (s *DatabaseStore) CreateSchedule(ctx context.Context, v Schedule, key string) (Schedule, error) {
	if v.ScheduleID == "" {
		v.ScheduleID = uuid.NewString()
	}
	if v.Status == "" {
		v.Status = "enabled"
	}
	normalizeScheduleProjection(&v)
	if v.Revision == 0 {
		v.Revision = 1
	}
	n := time.Now().UTC()
	if v.CreatedAt.IsZero() {
		v.CreatedAt = n
	}
	v.UpdatedAt = n
	_, e := s.db.ExecContext(ctx, `INSERT INTO p2p_agent_schedules(schedule_id,owner_id,name,prompt,trigger_kind,trigger_value,timezone,skip_if_running,status,core_state,revision,model_profile_id,model_profile_revision,credential_version,next_run_at,task_template,trigger_json,created_at,updated_at,idempotency_key) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16::jsonb,$17::jsonb,$18,$19,$20)`, v.ScheduleID, v.OwnerID, v.Name, v.Prompt, v.TriggerKind, v.TriggerValue, v.Timezone, v.SkipIfRunning, v.Status, v.CoreState, v.Revision, v.ModelProfileID, v.ModelProfileRevision, v.CredentialVersion, v.NextRunAt, stringOrEmptyJSON(v.TaskTemplate), stringOrEmptyJSON(v.TriggerJSON), v.CreatedAt, v.UpdatedAt, key)
	if e != nil {
		return Schedule{}, e
	}
	return v, nil
}

func stringOrEmptyJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	return string(raw)
}
func (s *DatabaseStore) GetSchedule(ctx context.Context, o, id string) (Schedule, bool, error) {
	var v Schedule
	e := s.db.QueryRowContext(ctx, `SELECT schedule_id,owner_id,name,prompt,trigger_kind,trigger_value,timezone,skip_if_running,status,core_state,revision,model_profile_id,model_profile_revision,credential_version,next_run_at,latest_run_at,lease_owner,lease_until,lease_epoch,task_template,trigger_json,created_at,updated_at FROM p2p_agent_schedules WHERE owner_id=$1 AND schedule_id=$2 AND deleted_at IS NULL`, o, id).Scan(&v.ScheduleID, &v.OwnerID, &v.Name, &v.Prompt, &v.TriggerKind, &v.TriggerValue, &v.Timezone, &v.SkipIfRunning, &v.Status, &v.CoreState, &v.Revision, &v.ModelProfileID, &v.ModelProfileRevision, &v.CredentialVersion, &v.NextRunAt, &v.LatestRunAt, &v.LeaseOwner, &v.LeaseUntil, &v.LeaseEpoch, &v.TaskTemplate, &v.TriggerJSON, &v.CreatedAt, &v.UpdatedAt)
	if errors.Is(e, sql.ErrNoRows) {
		return Schedule{}, false, nil
	}
	v.TaskTemplate = compactScheduleJSON(v.TaskTemplate)
	v.TriggerJSON = compactScheduleJSON(v.TriggerJSON)
	return v, e == nil, e
}
func (s *DatabaseStore) GetScheduleIncludingDeleted(ctx context.Context, o, id string) (ScheduleTombstone, bool, error) {
	var v ScheduleTombstone
	err := s.db.QueryRowContext(ctx, `SELECT schedule_id,(deleted_at IS NOT NULL),revision FROM p2p_agent_schedules WHERE owner_id=$1 AND schedule_id=$2`, o, id).Scan(&v.ScheduleID, &v.Deleted, &v.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return ScheduleTombstone{}, false, nil
	}
	return v, err == nil, err
}
func (s *DatabaseStore) ListSchedules(ctx context.Context, o string, limit int, cursor string) (SchedulePage, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	start, err := decodeScheduleCursor(cursor)
	if err != nil {
		return SchedulePage{}, err
	}
	rows, e := s.db.QueryContext(ctx, `SELECT schedule_id,owner_id,name,prompt,trigger_kind,trigger_value,timezone,skip_if_running,status,core_state,revision,model_profile_id,model_profile_revision,credential_version,next_run_at,latest_run_at,lease_owner,lease_until,lease_epoch,task_template,trigger_json,created_at,updated_at FROM p2p_agent_schedules WHERE owner_id=$1 AND deleted_at IS NULL AND schedule_id>$2 ORDER BY schedule_id LIMIT $3`, o, start, limit+1)
	if e != nil {
		return SchedulePage{}, e
	}
	defer rows.Close()
	p := SchedulePage{Schedules: []Schedule{}}
	for rows.Next() {
		var v Schedule
		if e = rows.Scan(&v.ScheduleID, &v.OwnerID, &v.Name, &v.Prompt, &v.TriggerKind, &v.TriggerValue, &v.Timezone, &v.SkipIfRunning, &v.Status, &v.CoreState, &v.Revision, &v.ModelProfileID, &v.ModelProfileRevision, &v.CredentialVersion, &v.NextRunAt, &v.LatestRunAt, &v.LeaseOwner, &v.LeaseUntil, &v.LeaseEpoch, &v.TaskTemplate, &v.TriggerJSON, &v.CreatedAt, &v.UpdatedAt); e != nil {
			return p, e
		}
		v.TaskTemplate = compactScheduleJSON(v.TaskTemplate)
		v.TriggerJSON = compactScheduleJSON(v.TriggerJSON)
		p.Schedules = append(p.Schedules, v)
	}
	if len(p.Schedules) > limit {
		p.NextCursor = encodeScheduleCursor(p.Schedules[limit-1].ScheduleID)
		p.Schedules = p.Schedules[:limit]
	}
	return p, rows.Err()
}
func (s *DatabaseStore) UpdateSchedule(ctx context.Context, v Schedule, _ string) (Schedule, error) {
	return s.UpdateScheduleCAS(ctx, v, v.Revision)
}
func (s *DatabaseStore) UpdateScheduleCAS(ctx context.Context, v Schedule, expected int64) (Schedule, error) {
	v.Revision++
	v.UpdatedAt = time.Now().UTC()
	if expected == 0 {
		expected = v.Revision - 1
	}
	if v.CoreState == "" {
		if v.Status == "disabled" {
			v.CoreState = "paused"
		} else {
			v.CoreState = "active"
		}
	}
	normalizeScheduleProjection(&v)
	r, e := s.db.ExecContext(ctx, `UPDATE p2p_agent_schedules SET name=$1,prompt=$2,trigger_kind=$3,trigger_value=$4,timezone=$5,skip_if_running=$6,status=$7,core_state=$8,revision=$9,model_profile_id=$10,model_profile_revision=$11,credential_version=$12,next_run_at=$13,task_template=$14::jsonb,trigger_json=$15::jsonb,updated_at=$16 WHERE owner_id=$17 AND schedule_id=$18 AND revision=$19 AND deleted_at IS NULL AND status<>'running'`, v.Name, v.Prompt, v.TriggerKind, v.TriggerValue, v.Timezone, v.SkipIfRunning, v.Status, v.CoreState, v.Revision, v.ModelProfileID, v.ModelProfileRevision, v.CredentialVersion, v.NextRunAt, stringOrEmptyJSON(v.TaskTemplate), stringOrEmptyJSON(v.TriggerJSON), v.UpdatedAt, v.OwnerID, v.ScheduleID, expected)
	if e != nil {
		return Schedule{}, e
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		if _, ok, _ := s.GetSchedule(ctx, v.OwnerID, v.ScheduleID); ok {
			return Schedule{}, ErrScheduleConflict
		}
		return Schedule{}, ErrScheduleNotFound
	}
	return v, nil
}
func (s *DatabaseStore) DeleteSchedule(ctx context.Context, o, id, _ string) error {
	return s.DeleteScheduleCAS(ctx, o, id, 0)
}
func (s *DatabaseStore) DeleteScheduleCAS(ctx context.Context, o, id string, expected int64) error {
	query := `UPDATE p2p_agent_schedules SET deleted_at=NOW(),status='deleted',revision=revision+1,updated_at=NOW() WHERE owner_id=$1 AND schedule_id=$2 AND deleted_at IS NULL AND status<>'running'`
	args := []any{o, id}
	if expected != 0 {
		query += ` AND revision=$3`
		args = append(args, expected)
	}
	r, e := s.db.ExecContext(ctx, query, args...)
	if e != nil {
		return e
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		if expected != 0 {
			if _, ok, _ := s.GetSchedule(ctx, o, id); ok {
				return ErrScheduleConflict
			}
		}
		return ErrScheduleNotFound
	}
	return nil
}
func (s *DatabaseStore) SetScheduleStatus(ctx context.Context, o, id, _ string, en bool) (Schedule, error) {
	return s.SetScheduleStatusCAS(ctx, o, id, 0, en)
}
func (s *DatabaseStore) SetScheduleStatusCAS(ctx context.Context, o, id string, expected int64, en bool) (Schedule, error) {
	status := "disabled"
	if en {
		status = "enabled"
	}
	coreState := "paused"
	if en {
		coreState = "active"
	}
	query := `UPDATE p2p_agent_schedules SET status=$1,core_state=$2,revision=revision+1,updated_at=NOW() WHERE owner_id=$3 AND schedule_id=$4 AND deleted_at IS NULL AND status<>'running'`
	args := []any{status, coreState, o, id}
	if expected != 0 {
		query += ` AND revision=$5`
		args = append(args, expected)
	}
	r, e := s.db.ExecContext(ctx, query, args...)
	if e != nil {
		return Schedule{}, e
	}
	if n, _ := r.RowsAffected(); n == 0 {
		if _, ok, ge := s.GetSchedule(ctx, o, id); ge != nil {
			return Schedule{}, ge
		} else if ok {
			if expected != 0 {
				return Schedule{}, ErrScheduleConflict
			}
			return Schedule{}, ErrScheduleConflict
		}
	}
	v, ok, e := s.GetSchedule(ctx, o, id)
	if e != nil {
		return v, e
	}
	if !ok {
		return v, ErrScheduleNotFound
	}
	return v, nil
}

func (s *DatabaseStore) GetScheduleMutation(ctx context.Context, owner, action, key string) ([32]byte, []byte, bool, error) {
	var raw, response []byte
	err := s.db.QueryRowContext(ctx, `SELECT request_digest,response_json FROM p2p_agent_schedule_mutations WHERE owner_id=$1 AND action=$2 AND idempotency_key=$3`, owner, action, key).Scan(&raw, &response)
	if errors.Is(err, sql.ErrNoRows) {
		return [32]byte{}, nil, false, nil
	}
	if err != nil {
		return [32]byte{}, nil, false, err
	}
	var digest [32]byte
	copy(digest[:], raw)
	return digest, response, true, nil
}
func (s *DatabaseStore) ReserveScheduleMutation(ctx context.Context, owner, action, key string, digest [32]byte) ([32]byte, []byte, bool, error) {
	var inserted []byte
	err := s.db.QueryRowContext(ctx, `INSERT INTO p2p_agent_schedule_mutations(owner_id,action,idempotency_key,request_digest,response_json,created_at) VALUES($1,$2,$3,$4,'{}'::jsonb,NOW()) ON CONFLICT(owner_id,action,idempotency_key) DO NOTHING RETURNING request_digest`, owner, action, key, digest[:]).Scan(&inserted)
	if err == nil {
		return digest, nil, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return [32]byte{}, nil, false, err
	}
	return s.GetScheduleMutation(ctx, owner, action, key)
}
func (s *DatabaseStore) PutScheduleMutation(ctx context.Context, owner, action, key string, digest [32]byte, response []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO p2p_agent_schedule_mutations(owner_id,action,idempotency_key,request_digest,response_json,created_at) VALUES($1,$2,$3,$4,$5::jsonb,NOW()) ON CONFLICT(owner_id,action,idempotency_key) DO UPDATE SET response_json=EXCLUDED.response_json WHERE p2p_agent_schedule_mutations.request_digest=EXCLUDED.request_digest`, owner, action, key, digest[:], string(response))
	if err != nil {
		return err
	}
	stored, _, ok, err := s.GetScheduleMutation(ctx, owner, action, key)
	if err == nil && (!ok || stored != digest) {
		return ErrScheduleIdempotency
	}
	return err
}
func scanScheduleConfirmation(row interface{ Scan(...any) error }) (ScheduleConfirmation, error) {
	var v ScheduleConfirmation
	var raw []byte
	err := row.Scan(&v.ConfirmationID, &v.OwnerID, &v.ConversationID, &v.Action, &v.ParamsJSON, &raw, &v.IdempotencyKey, &v.Summary, &v.ApprovalCode, &v.Status, &v.Revision, &v.ExpiresAt, &v.CreatedAt, &v.UpdatedAt, &v.ResultJSON, &v.Error)
	copy(v.RequestDigest[:], raw)
	return v, err
}
func (s *DatabaseStore) GetScheduleConfirmation(ctx context.Context, owner, conversation, id string) (ScheduleConfirmation, bool, error) {
	v, e := scanScheduleConfirmation(s.db.QueryRowContext(ctx, `SELECT confirmation_id,owner_id,conversation_id,action,params_json,request_digest,idempotency_key,summary,approval_code,status,revision,expires_at,created_at,updated_at,result_json,error_text FROM p2p_agent_schedule_confirmations WHERE owner_id=$1 AND conversation_id=$2 AND confirmation_id=$3`, owner, conversation, id))
	if errors.Is(e, sql.ErrNoRows) {
		return ScheduleConfirmation{}, false, nil
	}
	return v, e == nil, e
}
func (s *DatabaseStore) ReserveScheduleConfirmation(ctx context.Context, v ScheduleConfirmation) (ScheduleConfirmation, bool, error) {
	if v.Revision <= 0 {
		v.Revision = 1
	}
	now := time.Now().UTC()
	if v.CreatedAt.IsZero() {
		v.CreatedAt = now
	}
	if v.UpdatedAt.IsZero() {
		v.UpdatedAt = now
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return ScheduleConfirmation{}, false, e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, canonicalAdvisoryLockIdentity("schedule-confirmation", v.OwnerID, v.ConversationID)); e != nil {
		return ScheduleConfirmation{}, false, e
	}
	var active ScheduleConfirmation
	var raw []byte
	q := tx.QueryRowContext(ctx, `SELECT confirmation_id,owner_id,conversation_id,action,params_json,request_digest,idempotency_key,summary,approval_code,status,revision,expires_at,created_at,updated_at,result_json,error_text FROM p2p_agent_schedule_confirmations WHERE owner_id=$1 AND conversation_id=$2 AND status IN ('pending','executing') ORDER BY updated_at DESC LIMIT 1`, v.OwnerID, v.ConversationID)
	se := q.Scan(&active.ConfirmationID, &active.OwnerID, &active.ConversationID, &active.Action, &active.ParamsJSON, &raw, &active.IdempotencyKey, &active.Summary, &active.ApprovalCode, &active.Status, &active.Revision, &active.ExpiresAt, &active.CreatedAt, &active.UpdatedAt, &active.ResultJSON, &active.Error)
	copy(active.RequestDigest[:], raw)
	if se == nil {
		if active.Action == v.Action && active.RequestDigest == v.RequestDigest && active.Status == "pending" && active.ExpiresAt.After(now) {
			return txCommitConfirmation(tx, active)
		}
		if active.Status == "executing" {
			return ScheduleConfirmation{}, false, ErrScheduleConfirmationConflict
		}
		if _, e = tx.ExecContext(ctx, `UPDATE p2p_agent_schedule_confirmations SET status='replaced',revision=revision+1,updated_at=$1 WHERE owner_id=$2 AND conversation_id=$3 AND confirmation_id=$4`, now, v.OwnerID, v.ConversationID, active.ConfirmationID); e != nil {
			return ScheduleConfirmation{}, false, e
		}
	}
	e = tx.QueryRowContext(ctx, `INSERT INTO p2p_agent_schedule_confirmations(confirmation_id,owner_id,conversation_id,action,params_json,request_digest,idempotency_key,summary,approval_code,status,revision,expires_at,created_at,updated_at,result_json,error_text) VALUES($1,$2,$3,$4,$5::jsonb,$6,$7,$8,$9,'pending',$10,$11,$12,$13,'{}'::jsonb,'') RETURNING confirmation_id`, v.ConfirmationID, v.OwnerID, v.ConversationID, v.Action, string(v.ParamsJSON), v.RequestDigest[:], v.IdempotencyKey, v.Summary, v.ApprovalCode, v.Revision, v.ExpiresAt, v.CreatedAt, v.UpdatedAt).Scan(new(string))
	if e == nil {
		if e = tx.Commit(); e != nil {
			return ScheduleConfirmation{}, false, e
		}
		return v, true, nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return ScheduleConfirmation{}, false, e
	}
	old, ok, e := s.GetScheduleConfirmation(ctx, v.OwnerID, v.ConversationID, v.ConfirmationID)
	return old, !ok && e == nil, e
}
func txCommitConfirmation(tx *sql.Tx, v ScheduleConfirmation) (ScheduleConfirmation, bool, error) {
	if e := tx.Commit(); e != nil {
		return ScheduleConfirmation{}, false, e
	}
	return v, false, nil
}
func (s *DatabaseStore) ClaimScheduleConfirmation(ctx context.Context, owner, conversation, id string, revision int64, now time.Time) (ScheduleConfirmation, error) {
	var v ScheduleConfirmation
	var raw []byte
	e := s.db.QueryRowContext(ctx, `UPDATE p2p_agent_schedule_confirmations SET status='executing',revision=revision+1,updated_at=$5 WHERE owner_id=$1 AND conversation_id=$2 AND confirmation_id=$3 AND status='pending' AND revision=$4 AND expires_at>$5 RETURNING confirmation_id,owner_id,conversation_id,action,params_json,request_digest,idempotency_key,summary,approval_code,status,revision,expires_at,created_at,updated_at,result_json,error_text`, owner, conversation, id, revision, now).Scan(&v.ConfirmationID, &v.OwnerID, &v.ConversationID, &v.Action, &v.ParamsJSON, &raw, &v.IdempotencyKey, &v.Summary, &v.ApprovalCode, &v.Status, &v.Revision, &v.ExpiresAt, &v.CreatedAt, &v.UpdatedAt, &v.ResultJSON, &v.Error)
	copy(v.RequestDigest[:], raw)
	if e == nil {
		return v, nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return v, e
	}
	terminal, ok, ge := s.GetScheduleConfirmation(ctx, owner, conversation, id)
	if ge != nil {
		return v, ge
	}
	if !ok {
		return v, ErrScheduleConfirmationNotFound
	}
	if terminal.Status == "completed" || terminal.Status == "failed" {
		return terminal, nil
	}
	return v, ErrScheduleConfirmationConflict
}
func (s *DatabaseStore) CompleteScheduleConfirmation(ctx context.Context, owner, conversation, id string, revision int64, status string, result []byte, errText string) error {
	if status != "completed" {
		status = "failed"
	}
	r, e := s.db.ExecContext(ctx, `UPDATE p2p_agent_schedule_confirmations SET status=$1,revision=revision+1,result_json=$2::jsonb,error_text=$3,updated_at=NOW() WHERE owner_id=$4 AND conversation_id=$5 AND confirmation_id=$6 AND status='executing' AND revision=$7`, status, string(result), errText, owner, conversation, id, revision)
	if e != nil {
		return e
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return ErrScheduleConfirmationConflict
	}
	return nil
}
func (s *DatabaseStore) ListScheduleRuns(ctx context.Context, o, id string, limit int, cursor string) (ScheduleRunPage, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	start, err := decodeScheduleRunCursor(cursor)
	if err != nil {
		return ScheduleRunPage{}, err
	}
	var startTime any
	if start.RunID != "" {
		startTime = start.ScheduledFor
	}
	rows, e := s.db.QueryContext(ctx, `SELECT run_id,schedule_id,owner_id,status,scheduled_for,started_at,finished_at,result,error,lease_epoch FROM p2p_agent_schedule_runs WHERE owner_id=$1 AND schedule_id=$2 AND ($3::timestamptz IS NULL OR scheduled_for<$3 OR (scheduled_for=$3 AND run_id<$4)) ORDER BY scheduled_for DESC,run_id DESC LIMIT $5`, o, id, startTime, start.RunID, limit+1)
	if e != nil {
		return ScheduleRunPage{}, e
	}
	defer rows.Close()
	p := ScheduleRunPage{Runs: []ScheduleRun{}}
	for rows.Next() {
		var r ScheduleRun
		if e = rows.Scan(&r.RunID, &r.ScheduleID, &r.OwnerID, &r.Status, &r.ScheduledFor, &r.StartedAt, &r.FinishedAt, &r.Result, &r.Error, &r.LeaseEpoch); e != nil {
			return p, e
		}
		p.Runs = append(p.Runs, r)
	}
	if len(p.Runs) > limit {
		last := p.Runs[limit-1]
		p.NextCursor = encodeScheduleRunCursor(last.ScheduledFor, last.RunID)
		p.Runs = p.Runs[:limit]
	}
	return p, rows.Err()
}
func (s *DatabaseStore) GetScheduleRun(ctx context.Context, o, id, rid string) (ScheduleRun, bool, error) {
	var r ScheduleRun
	e := s.db.QueryRowContext(ctx, `SELECT run_id,schedule_id,owner_id,status,scheduled_for,started_at,finished_at,result,error,lease_epoch FROM p2p_agent_schedule_runs WHERE owner_id=$1 AND schedule_id=$2 AND run_id=$3`, o, id, rid).Scan(&r.RunID, &r.ScheduleID, &r.OwnerID, &r.Status, &r.ScheduledFor, &r.StartedAt, &r.FinishedAt, &r.Result, &r.Error, &r.LeaseEpoch)
	if errors.Is(e, sql.ErrNoRows) {
		return r, false, nil
	}
	return r, e == nil, e
}
func (s *DatabaseStore) ClaimDueSchedules(ctx context.Context, now time.Time, worker string, lease time.Duration, limit int) ([]Schedule, error) {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	rows, e := tx.QueryContext(ctx, `SELECT schedule_id,owner_id,name,prompt,trigger_kind,trigger_value,timezone,skip_if_running,status,core_state,revision,model_profile_id,model_profile_revision,credential_version,next_run_at,latest_run_at,lease_owner,lease_until,lease_epoch,task_template,trigger_json,created_at,updated_at FROM p2p_agent_schedules WHERE deleted_at IS NULL AND core_state='active' AND next_run_at<=$1 AND (status='enabled' OR (status='running' AND lease_until<=$1)) ORDER BY next_run_at FOR UPDATE SKIP LOCKED LIMIT $2`, now, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Schedule{}
	for rows.Next() {
		var v Schedule
		if e = rows.Scan(&v.ScheduleID, &v.OwnerID, &v.Name, &v.Prompt, &v.TriggerKind, &v.TriggerValue, &v.Timezone, &v.SkipIfRunning, &v.Status, &v.CoreState, &v.Revision, &v.ModelProfileID, &v.ModelProfileRevision, &v.CredentialVersion, &v.NextRunAt, &v.LatestRunAt, &v.LeaseOwner, &v.LeaseUntil, &v.LeaseEpoch, &v.TaskTemplate, &v.TriggerJSON, &v.CreatedAt, &v.UpdatedAt); e != nil {
			return nil, e
		}
		v.TaskTemplate = compactScheduleJSON(v.TaskTemplate)
		v.TriggerJSON = compactScheduleJSON(v.TriggerJSON)
		row := tx.QueryRowContext(ctx, `UPDATE p2p_agent_schedules SET status='running',revision=revision+1,lease_owner=$1,lease_until=$2,lease_epoch=lease_epoch+1,updated_at=$3 WHERE schedule_id=$4 AND owner_id=$5 RETURNING revision,lease_owner,lease_until,lease_epoch`, worker, now.Add(lease), now, v.ScheduleID, v.OwnerID)
		if e = row.Scan(&v.Revision, &v.LeaseOwner, &v.LeaseUntil, &v.LeaseEpoch); e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	return out, tx.Commit()
}
func (s *DatabaseStore) ClaimScheduleNow(ctx context.Context, owner, id, worker string, lease time.Duration) (Schedule, error) {
	now := time.Now().UTC()
	var v Schedule
	e := s.db.QueryRowContext(ctx, `UPDATE p2p_agent_schedules SET status='running',core_state='active',revision=revision+1,lease_owner=$1,lease_until=$2,lease_epoch=lease_epoch+1,updated_at=$3 WHERE owner_id=$4 AND schedule_id=$5 AND deleted_at IS NULL AND core_state='active' AND NOT (status='running' AND lease_until>$3) RETURNING schedule_id,owner_id,name,prompt,trigger_kind,trigger_value,timezone,skip_if_running,status,core_state,revision,model_profile_id,model_profile_revision,credential_version,next_run_at,latest_run_at,lease_owner,lease_until,lease_epoch,task_template,trigger_json,created_at,updated_at`, worker, now.Add(lease), now, owner, id).Scan(&v.ScheduleID, &v.OwnerID, &v.Name, &v.Prompt, &v.TriggerKind, &v.TriggerValue, &v.Timezone, &v.SkipIfRunning, &v.Status, &v.CoreState, &v.Revision, &v.ModelProfileID, &v.ModelProfileRevision, &v.CredentialVersion, &v.NextRunAt, &v.LatestRunAt, &v.LeaseOwner, &v.LeaseUntil, &v.LeaseEpoch, &v.TaskTemplate, &v.TriggerJSON, &v.CreatedAt, &v.UpdatedAt)
	if errors.Is(e, sql.ErrNoRows) {
		if _, ok, ge := s.GetSchedule(ctx, owner, id); ge != nil {
			return v, ge
		} else if ok {
			return v, ErrScheduleConflict
		}
		return v, ErrScheduleNotFound
	}
	v.TaskTemplate = compactScheduleJSON(v.TaskTemplate)
	v.TriggerJSON = compactScheduleJSON(v.TriggerJSON)
	return v, e
}
func (s *DatabaseStore) ListOverlappingSchedules(ctx context.Context, now time.Time, limit int) ([]Schedule, error) {
	rows, e := s.db.QueryContext(ctx, `SELECT schedule_id,owner_id,name,prompt,trigger_kind,trigger_value,timezone,skip_if_running,status,core_state,revision,model_profile_id,model_profile_revision,credential_version,next_run_at,latest_run_at,lease_owner,lease_until,lease_epoch,task_template,trigger_json,created_at,updated_at FROM p2p_agent_schedules WHERE deleted_at IS NULL AND core_state='active' AND status='running' AND skip_if_running AND next_run_at<=$1 AND lease_until>$1 ORDER BY next_run_at LIMIT $2`, now, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Schedule{}
	for rows.Next() {
		var v Schedule
		if e = rows.Scan(&v.ScheduleID, &v.OwnerID, &v.Name, &v.Prompt, &v.TriggerKind, &v.TriggerValue, &v.Timezone, &v.SkipIfRunning, &v.Status, &v.CoreState, &v.Revision, &v.ModelProfileID, &v.ModelProfileRevision, &v.CredentialVersion, &v.NextRunAt, &v.LatestRunAt, &v.LeaseOwner, &v.LeaseUntil, &v.LeaseEpoch, &v.TaskTemplate, &v.TriggerJSON, &v.CreatedAt, &v.UpdatedAt); e != nil {
			return nil, e
		}
		v.TaskTemplate = compactScheduleJSON(v.TaskTemplate)
		v.TriggerJSON = compactScheduleJSON(v.TriggerJSON)
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *DatabaseStore) CoalesceSchedule(ctx context.Context, owner, id, leaseOwner string, epoch int64, next *time.Time) error {
	r, e := s.db.ExecContext(ctx, `UPDATE p2p_agent_schedules SET next_run_at=$1,latest_run_at=NOW(),revision=revision+1,updated_at=NOW() WHERE owner_id=$2 AND schedule_id=$3 AND status='running' AND lease_owner=$4 AND lease_epoch=$5 AND lease_until>NOW() AND deleted_at IS NULL`, next, owner, id, leaseOwner, epoch)
	if e != nil {
		return e
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrScheduleClaimed
	}
	return nil
}
func (s *DatabaseStore) RenewScheduleLease(ctx context.Context, owner, id, leaseOwner string, epoch int64, lease time.Duration) error {
	now := time.Now().UTC()
	r, e := s.db.ExecContext(ctx, `UPDATE p2p_agent_schedules SET lease_until=$1,updated_at=$2 WHERE owner_id=$3 AND schedule_id=$4 AND status='running' AND lease_owner=$5 AND lease_epoch=$6 AND lease_until>$2 AND deleted_at IS NULL`, now.Add(lease), now, owner, id, leaseOwner, epoch)
	if e != nil {
		return e
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrScheduleClaimed
	}
	return nil
}
func (s *DatabaseStore) CreateScheduleRun(ctx context.Context, v ScheduleRun, leaseOwner string, revision, epoch int64) (ScheduleRun, bool, error) {
	if v.RunID == "" {
		v.RunID = uuid.NewString()
	}
	r, e := s.db.ExecContext(ctx, `INSERT INTO p2p_agent_schedule_runs(run_id,schedule_id,owner_id,status,scheduled_for,started_at,result,error,lease_epoch) SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9 WHERE EXISTS (SELECT 1 FROM p2p_agent_schedules WHERE owner_id=$3 AND schedule_id=$2 AND status='running' AND revision=$10 AND lease_owner=$11 AND lease_epoch=$9 AND lease_until>NOW() AND deleted_at IS NULL) ON CONFLICT (owner_id,schedule_id,scheduled_for) DO NOTHING`, v.RunID, v.ScheduleID, v.OwnerID, v.Status, v.ScheduledFor, v.StartedAt, v.Result, v.Error, epoch, revision, leaseOwner)
	if e != nil {
		return ScheduleRun{}, false, e
	}
	if n, _ := r.RowsAffected(); n == 1 {
		return v, true, nil
	}
	var existing ScheduleRun
	e = s.db.QueryRowContext(ctx, `SELECT run_id,schedule_id,owner_id,status,scheduled_for,started_at,finished_at,result,error,lease_epoch FROM p2p_agent_schedule_runs WHERE owner_id=$1 AND schedule_id=$2 AND scheduled_for=$3`, v.OwnerID, v.ScheduleID, v.ScheduledFor).Scan(&existing.RunID, &existing.ScheduleID, &existing.OwnerID, &existing.Status, &existing.ScheduledFor, &existing.StartedAt, &existing.FinishedAt, &existing.Result, &existing.Error, &existing.LeaseEpoch)
	if e != nil {
		return ScheduleRun{}, false, e
	}
	return existing, false, nil
}

// AcquireScheduleRunRecoveryLease atomically fences a replayable run whose
// schedule lease has expired. A terminal run remains eligible only while its
// schedule is still running, i.e. before the prior finisher advanced it.
func (s *DatabaseStore) AcquireScheduleRunRecoveryLease(ctx context.Context, owner, scheduleID, runID, recoveryOwner string, lease time.Duration) (Schedule, error) {
	if lease <= 0 {
		lease = 30 * time.Second
	}
	var v Schedule
	e := s.db.QueryRowContext(ctx, `UPDATE p2p_agent_schedules s SET revision=s.revision+1,lease_owner=$1,lease_until=NOW()+$2::interval,lease_epoch=s.lease_epoch+1,updated_at=NOW() WHERE s.owner_id=$3 AND s.schedule_id=$4 AND s.deleted_at IS NULL AND s.status='running' AND s.core_state='active' AND s.lease_until<=NOW() AND EXISTS (SELECT 1 FROM p2p_agent_schedule_runs r WHERE r.run_id=$5 AND r.owner_id=s.owner_id AND r.schedule_id=s.schedule_id) RETURNING s.schedule_id,s.owner_id,s.name,s.prompt,s.trigger_kind,s.trigger_value,s.timezone,s.skip_if_running,s.status,s.core_state,s.revision,s.model_profile_id,s.model_profile_revision,s.credential_version,s.next_run_at,s.latest_run_at,s.lease_owner,s.lease_until,s.lease_epoch,s.task_template,s.trigger_json,s.created_at,s.updated_at`, recoveryOwner, lease.String(), owner, scheduleID, runID).Scan(&v.ScheduleID, &v.OwnerID, &v.Name, &v.Prompt, &v.TriggerKind, &v.TriggerValue, &v.Timezone, &v.SkipIfRunning, &v.Status, &v.CoreState, &v.Revision, &v.ModelProfileID, &v.ModelProfileRevision, &v.CredentialVersion, &v.NextRunAt, &v.LatestRunAt, &v.LeaseOwner, &v.LeaseUntil, &v.LeaseEpoch, &v.TaskTemplate, &v.TriggerJSON, &v.CreatedAt, &v.UpdatedAt)
	if errors.Is(e, sql.ErrNoRows) {
		return Schedule{}, ErrScheduleClaimed
	}
	return v, e
}
func (s *DatabaseStore) RecoverScheduleRun(ctx context.Context, owner, scheduleID, leaseOwner string, epoch int64, scheduledFor time.Time) (ScheduleRun, error) {
	_, e := s.db.ExecContext(ctx, `UPDATE p2p_agent_schedule_runs r SET status='failed',error=CASE WHEN error='' THEN 'schedule lease expired before completion' ELSE error END,finished_at=COALESCE(finished_at,NOW()) WHERE r.owner_id=$1 AND r.schedule_id=$2 AND r.scheduled_for=$3 AND r.status='running' AND EXISTS (SELECT 1 FROM p2p_agent_schedules s WHERE s.owner_id=r.owner_id AND s.schedule_id=r.schedule_id AND s.status='running' AND s.lease_owner=$4 AND s.lease_epoch=$5 AND s.lease_until>NOW())`, owner, scheduleID, scheduledFor, leaseOwner, epoch)
	if e != nil {
		return ScheduleRun{}, e
	}
	var r ScheduleRun
	e = s.db.QueryRowContext(ctx, `SELECT run_id,schedule_id,owner_id,status,scheduled_for,started_at,finished_at,result,error,lease_epoch FROM p2p_agent_schedule_runs WHERE owner_id=$1 AND schedule_id=$2 AND scheduled_for=$3`, owner, scheduleID, scheduledFor).Scan(&r.RunID, &r.ScheduleID, &r.OwnerID, &r.Status, &r.ScheduledFor, &r.StartedAt, &r.FinishedAt, &r.Result, &r.Error, &r.LeaseEpoch)
	if errors.Is(e, sql.ErrNoRows) {
		return r, ErrScheduleNotFound
	}
	return r, e
}
func (s *DatabaseStore) FinishScheduleRun(ctx context.Context, o, rid, leaseOwner string, epoch int64, result, runErr string, at time.Time) error {
	status := "succeeded"
	if runErr != "" {
		status = "failed"
	}
	r, e := s.db.ExecContext(ctx, `UPDATE p2p_agent_schedule_runs r SET status=$1,result=$2,error=$3,finished_at=$4 WHERE run_id=$5 AND owner_id=$6 AND lease_epoch=$7 AND EXISTS (SELECT 1 FROM p2p_agent_schedules s WHERE s.owner_id=r.owner_id AND s.schedule_id=r.schedule_id AND s.status='running' AND s.lease_owner=$8 AND s.lease_epoch=$7 AND s.lease_until>NOW() AND s.deleted_at IS NULL)`, status, result, runErr, at, rid, o, epoch, leaseOwner)
	if e != nil {
		return e
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return ErrScheduleClaimed
	}
	return nil
}
func (s *DatabaseStore) AdvanceSchedule(ctx context.Context, owner, id, leaseOwner string, revision, epoch int64, next *time.Time, status string) error {
	coreState := "active"
	if status == "disabled" {
		coreState = "paused"
	}
	r, e := s.db.ExecContext(ctx, `UPDATE p2p_agent_schedules SET next_run_at=$1,latest_run_at=NOW(),status=$2,core_state=$3,lease_owner='',lease_until=NULL,revision=revision+1,updated_at=NOW() WHERE owner_id=$4 AND schedule_id=$5 AND lease_owner=$6 AND lease_epoch=$7 AND status='running' AND lease_until>NOW() AND deleted_at IS NULL`, next, status, coreState, owner, id, leaseOwner, epoch)
	if e != nil {
		return e
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return ErrScheduleClaimed
	}
	return nil
}
