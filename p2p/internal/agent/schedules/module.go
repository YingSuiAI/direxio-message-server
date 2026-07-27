package schedules

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/nativeagent"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
	"github.com/google/uuid"
)

type ScheduledRunner interface {
	ExecuteScheduled(context.Context, string, storage.ModelProfile, []string) (string, error)
}
type Config struct {
	Store          storage.ScheduleStore
	Profiles       storage.ModelProfileStore
	Runner         ScheduledRunner
	OwnerID        func() string
	SchedulerReady func() bool
}
type Module struct {
	store          storage.ScheduleStore
	profiles       storage.ModelProfileStore
	runner         ScheduledRunner
	ownerID        func() string
	schedulerReady func() bool
	mutationMu     sync.Mutex
}

func New(c Config) *Module {
	return &Module{store: c.Store, profiles: c.Profiles, runner: c.Runner, ownerID: c.OwnerID, schedulerReady: c.SchedulerReady}
}
func (m *Module) schedulerRunning() bool {
	return m != nil && m.runner != nil && (m.schedulerReady == nil || m.schedulerReady())
}
func (m *Module) Ready() bool {
	return m != nil && m.store != nil && m.profiles != nil && m.profiles.ModelProfileStoreReady() && m.runner != nil
}

func (m *Module) Worker(ownerID, workerID string) Worker {
	return Worker{Store: m.store, Profiles: m.profiles, Runner: m.runner, OwnerID: ownerID, WorkerID: workerID}
}
func (m *Module) owner() string {
	if m != nil && m.ownerID != nil {
		return strings.TrimSpace(m.ownerID())
	}
	return "owner"
}
func (m *Module) Handlers() map[string]actionbase.Handler {
	return map[string]actionbase.Handler{
		"agent.schedules.create": m.create, "agent.schedules.update": m.update, "agent.schedules.get": m.get, "agent.schedules.list": m.list, "agent.schedules.delete": m.delete, "agent.schedules.enable": m.enable, "agent.schedules.disable": m.disable, "agent.schedules.run_now": m.runNow, "agent.schedule_runs.list": m.runsList, "agent.schedule_runs.get": m.runGet,
	}
}

func mutationDigest(action string, p map[string]any) [32]byte {
	copyParams := make(map[string]any, len(p)+1)
	for k, v := range p {
		if k != "idempotency_key" {
			copyParams[k] = v
		}
	}
	copyParams["action"] = action
	b, _ := json.Marshal(copyParams)
	return sha256.Sum256(b)
}
func (m *Module) mutate(action string, fn func(context.Context, map[string]any) (any, *actionbase.Error), ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	if m.store == nil {
		return nil, actionbase.StatusError(http.StatusBadGateway, "embedded schedules unavailable")
	}
	key, e := str(p, "idempotency_key")
	if e != nil {
		return nil, invalid(e)
	}
	allowed := map[string]map[string]bool{
		"agent.schedules.create":  {"name": true, "prompt": true, "model_profile_id": true, "trigger": true, "skip_if_running": true, "idempotency_key": true},
		"agent.schedules.update":  {"schedule_id": true, "name": true, "prompt": true, "model_profile_id": true, "trigger": true, "skip_if_running": true, "expected_revision": true, "idempotency_key": true},
		"agent.schedules.delete":  {"schedule_id": true, "expected_revision": true, "idempotency_key": true},
		"agent.schedules.enable":  {"schedule_id": true, "expected_revision": true, "idempotency_key": true},
		"agent.schedules.disable": {"schedule_id": true, "expected_revision": true, "idempotency_key": true},
		"agent.schedules.run_now": {"schedule_id": true, "idempotency_key": true},
	}[action]
	for k := range p {
		if !allowed[k] {
			return nil, invalid(fmt.Errorf("unknown field: %s", k))
		}
	}
	parsed, pe := uuid.Parse(key)
	if pe != nil || parsed.String() != key {
		return nil, invalid(fmt.Errorf("idempotency_key must be canonical UUID"))
	}
	digest := mutationDigest(action, p)
	m.mutationMu.Lock()
	defer m.mutationMu.Unlock()
	oldDigest, response, created, e := m.store.ReserveScheduleMutation(ctx, m.owner(), action, key, digest)
	if e != nil {
		return nil, actionbase.InternalError(e)
	}
	if !created {
		if oldDigest != digest {
			return nil, actionbase.StatusError(http.StatusConflict, "idempotency key reused with different request")
		}
		for i := 0; len(response) == 0 || string(response) == "{}"; i++ {
			if i >= 250 {
				break
			}
			time.Sleep(20 * time.Millisecond)
			_, response, _, e = m.store.GetScheduleMutation(ctx, m.owner(), action, key)
			if e != nil {
				return nil, actionbase.InternalError(e)
			}
		}
		if len(response) == 0 || string(response) == "{}" {
			if action == "agent.schedules.run_now" {
				if sid, ok := p["schedule_id"].(string); ok {
					rid := uuid.NewSHA1(uuid.Nil, []byte(m.owner()+"\x00agent.schedules.run_now\x00"+key)).String()
					if out, terminal, handled := m.reconcileRunNowReplay(ctx, sid, rid); handled {
						if terminal {
							if b, me := json.Marshal(out); me == nil {
								_ = m.store.PutScheduleMutation(ctx, m.owner(), action, key, digest, b)
							}
						}
						return out, nil
					}
				}
			}
			if action == "agent.schedules.create" || action == "agent.schedules.run_now" || action == "agent.schedules.delete" || action == "agent.schedules.update" || action == "agent.schedules.enable" || action == "agent.schedules.disable" {
				// Deterministic schedule_id lets create reconcile a receipt left
				// pending by a lost response without duplicating the mutation.
				out, ae := fn(ctx, p)
				if ae == nil {
					if b, me := json.Marshal(out); me == nil {
						_ = m.store.PutScheduleMutation(ctx, m.owner(), action, key, digest, b)
					}
				}
				return out, ae
			}
			return nil, actionbase.StatusError(http.StatusConflict, "idempotent mutation is still in progress")
		}
		var out any
		if json.Unmarshal(response, &out) == nil {
			return out, nil
		}
	}
	out, ae := fn(ctx, p)
	if ae != nil {
		return out, ae
	}
	b, e := json.Marshal(out)
	if e != nil {
		return nil, actionbase.InternalError(e)
	}
	if e = m.store.PutScheduleMutation(ctx, m.owner(), action, key, digest, b); e != nil {
		return nil, actionbase.InternalError(e)
	}
	return out, nil
}

func (m *Module) create(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	return m.mutate("agent.schedules.create", m.createOnce, ctx, p)
}
func (m *Module) update(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	return m.mutate("agent.schedules.update", m.updateOnce, ctx, p)
}
func (m *Module) delete(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	return m.mutate("agent.schedules.delete", m.deleteOnce, ctx, p)
}
func (m *Module) enable(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	return m.mutate("agent.schedules.enable", func(c context.Context, x map[string]any) (any, *actionbase.Error) { return m.setStatusOnce(c, x, true) }, ctx, p)
}
func (m *Module) disable(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	return m.mutate("agent.schedules.disable", func(c context.Context, x map[string]any) (any, *actionbase.Error) {
		return m.setStatusOnce(c, x, false)
	}, ctx, p)
}
func (m *Module) runNow(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	return m.mutate("agent.schedules.run_now", m.runNowOnce, ctx, p)
}
func invalid(e error) *actionbase.Error { return actionbase.BadRequest(e.Error()) }
func rejectUnknown(p map[string]any, allowed ...string) error {
	set := map[string]bool{}
	for _, k := range allowed {
		set[k] = true
	}
	for k := range p {
		if !set[k] {
			return fmt.Errorf("unknown field: %s", k)
		}
	}
	return nil
}
func str(params map[string]any, k string) (string, error) {
	v, ok := params[k]
	if !ok {
		return "", fmt.Errorf("%s is required", k)
	}
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("%s must be a non-empty string", k)
	}
	return strings.TrimSpace(s), nil
}
func optionalString(p map[string]any, k string) (string, error) {
	if _, ok := p[k]; !ok {
		return "", nil
	}
	v, ok := p[k].(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", k)
	}
	return strings.TrimSpace(v), nil
}
func boolVal(p map[string]any, k string) (bool, error) {
	if _, ok := p[k]; !ok {
		return false, nil
	}
	v, ok := p[k].(bool)
	if !ok {
		return false, fmt.Errorf("%s must be boolean", k)
	}
	return v, nil
}
func intVal(p map[string]any, k string) (int, error) {
	if _, ok := p[k]; !ok {
		return 50, nil
	}
	v, ok := p[k].(int)
	if ok {
		return v, nil
	}
	f, ok := p[k].(float64)
	if ok && f == float64(int(f)) {
		return int(f), nil
	}
	return 0, fmt.Errorf("%s must be integer", k)
}
func expectedRevision(p map[string]any) (int64, error) {
	v, ok := p["expected_revision"]
	if !ok {
		return 0, fmt.Errorf("expected_revision is required")
	}
	switch n := v.(type) {
	case int:
		if n > 0 {
			return int64(n), nil
		}
	case int64:
		if n > 0 {
			return n, nil
		}
	case float64:
		if n > 0 && n == float64(int64(n)) {
			return int64(n), nil
		}
	}
	return 0, fmt.Errorf("expected_revision must be positive integer")
}
func trigger(p map[string]any) (kind, value, tz string, next *time.Time, err error) {
	raw, ok := p["trigger"].(map[string]any)
	if !ok {
		return "", "", "", nil, fmt.Errorf("trigger must be object")
	}
	kind, err = str(raw, "kind")
	if err != nil {
		return
	}
	value, err = str(raw, "value")
	if err != nil {
		return
	}
	tz, err = optionalString(raw, "timezone")
	if err != nil {
		return
	}
	if kind != "one_time" && kind != "cron" {
		err = fmt.Errorf("trigger.kind must be one_time or cron")
		return
	}
	if kind == "one_time" {
		t, e := time.Parse(time.RFC3339, value)
		if e != nil {
			err = fmt.Errorf("trigger.value must be RFC3339")
			return
		}
		if tz == "" {
			err = fmt.Errorf("one_time timezone is required")
			return
		}
		if _, e = time.LoadLocation(tz); e != nil {
			err = fmt.Errorf("invalid timezone")
		}
		next = &t
	} else {
		if tz == "" {
			err = fmt.Errorf("cron timezone is required")
			return
		}
		if _, err = time.LoadLocation(tz); err != nil {
			err = fmt.Errorf("invalid timezone")
		}
	}
	return
}
func (m *Module) createOnce(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	if m.store == nil {
		return nil, actionbase.StatusError(http.StatusBadGateway, "embedded schedules unavailable")
	}
	name, e := str(p, "name")
	if e != nil {
		return nil, invalid(e)
	}
	prompt, e := str(p, "prompt")
	if e != nil {
		return nil, invalid(e)
	}
	profile, e := str(p, "model_profile_id")
	if e != nil {
		return nil, invalid(e)
	}
	if !m.schedulerRunning() {
		return nil, actionbase.StatusError(http.StatusServiceUnavailable, "scheduler is not running")
	}
	kind, val, tz, next, e := trigger(p)
	if e != nil {
		return nil, invalid(e)
	}
	if kind == "cron" {
		next, e = nextCron(val, tz, time.Now().UTC())
		if e != nil {
			return nil, invalid(e)
		}
	}
	skip, e := boolVal(p, "skip_if_running")
	if e != nil {
		return nil, invalid(e)
	}
	if m.profiles == nil {
		return nil, actionbase.StatusError(http.StatusBadGateway, "model profiles unavailable")
	}
	pr, e := m.profiles.ResolveModelProfilePin(ctx, m.owner(), profile)
	if e != nil {
		return nil, actionbase.StatusError(http.StatusBadRequest, e.Error())
	}
	key, e := str(p, "idempotency_key")
	if e != nil {
		return nil, invalid(e)
	}
	scheduleID := uuid.NewSHA1(uuid.Nil, []byte(m.owner()+"\x00agent.schedules.create\x00"+key)).String()
	if prior, ok, ge := m.store.GetSchedule(ctx, m.owner(), scheduleID); ge == nil && ok {
		return map[string]any{"schedule": prior}, nil
	}
	v, e := m.store.CreateSchedule(ctx, storage.Schedule{ScheduleID: scheduleID, OwnerID: m.owner(), Name: name, Prompt: prompt, TriggerKind: kind, TriggerValue: val, Timezone: tz, SkipIfRunning: skip, NextRunAt: next, ModelProfileID: profile, ModelProfileRevision: pr.Revision, CredentialVersion: pr.CredentialVersion}, key)
	if e != nil {
		return nil, actionbase.InternalError(e)
	}
	return map[string]any{"schedule": v}, nil
}
func (m *Module) get(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	if e := rejectUnknown(p, "schedule_id"); e != nil {
		return nil, invalid(e)
	}
	id, e := str(p, "schedule_id")
	if e != nil {
		return nil, invalid(e)
	}
	v, ok, e := m.store.GetSchedule(ctx, m.owner(), id)
	if e != nil {
		return nil, actionbase.InternalError(e)
	}
	if !ok {
		return nil, actionbase.StatusError(http.StatusNotFound, "schedule not found")
	}
	return map[string]any{"schedule": v}, nil
}
func (m *Module) list(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	if e := rejectUnknown(p, "limit", "cursor"); e != nil {
		return nil, invalid(e)
	}
	n, e := intVal(p, "limit")
	if e != nil {
		return nil, invalid(e)
	}
	c, e := optionalString(p, "cursor")
	if e != nil {
		return nil, invalid(e)
	}
	v, e := m.store.ListSchedules(ctx, m.owner(), n, c)
	if e != nil {
		return nil, invalid(e)
	}
	return map[string]any{"schedules": v.Schedules, "next_cursor": v.NextCursor}, nil
}
func (m *Module) updateOnce(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	if _, e := str(p, "idempotency_key"); e != nil {
		return nil, invalid(e)
	}
	id, e := str(p, "schedule_id")
	if e != nil {
		return nil, invalid(e)
	}
	expected, e := expectedRevision(p)
	if e != nil {
		return nil, invalid(e)
	}
	old, ok, e := m.store.GetSchedule(ctx, m.owner(), id)
	if e != nil || !ok {
		return nil, actionbase.StatusError(http.StatusNotFound, "schedule not found")
	}
	if old.Status == "enabled" && !m.schedulerRunning() {
		return nil, actionbase.StatusError(http.StatusServiceUnavailable, "scheduler is not running")
	}
	name, eName := optionalString(p, "name")
	if eName != nil {
		return nil, invalid(eName)
	}
	if name == "" {
		name = old.Name
	}
	prompt, ePrompt := optionalString(p, "prompt")
	if ePrompt != nil {
		return nil, invalid(ePrompt)
	}
	if prompt == "" {
		prompt = old.Prompt
	}
	kind, val, tz, next, te := trigger(p)
	if _, hasTrigger := p["trigger"]; hasTrigger && te != nil {
		return nil, invalid(te)
	}
	if te != nil {
		kind, val, tz, next = old.TriggerKind, old.TriggerValue, old.Timezone, old.NextRunAt
	} else if kind == "cron" {
		next, te = nextCron(val, tz, time.Now().UTC())
		if te != nil {
			return nil, invalid(te)
		}
	}
	skip, eSkip := boolVal(p, "skip_if_running")
	if eSkip != nil {
		return nil, invalid(eSkip)
	}
	if _, present := p["skip_if_running"]; !present {
		skip = old.SkipIfRunning
	}
	profile := old.ModelProfileID
	if x, ee := optionalString(p, "model_profile_id"); ee != nil {
		return nil, invalid(ee)
	} else if x != "" {
		profile = x
	}
	pr := storage.ModelProfile{Revision: old.ModelProfileRevision, CredentialVersion: old.CredentialVersion}
	if profile != old.ModelProfileID {
		if m.profiles == nil {
			return nil, actionbase.StatusError(http.StatusBadGateway, "model profiles unavailable")
		}
		pr, te = m.profiles.ResolveModelProfilePin(ctx, m.owner(), profile)
		if te != nil {
			return nil, invalid(te)
		}
	}
	if old.Revision == expected+1 && old.Name == name && old.Prompt == prompt && old.TriggerKind == kind && old.TriggerValue == val && old.Timezone == tz && old.SkipIfRunning == skip && old.ModelProfileID == profile {
		return map[string]any{"schedule": old}, nil
	}
	v, te := m.store.UpdateScheduleCAS(ctx, storage.Schedule{OwnerID: m.owner(), ScheduleID: id, Name: name, Prompt: prompt, TriggerKind: kind, TriggerValue: val, Timezone: tz, NextRunAt: next, SkipIfRunning: skip, Status: old.Status, ModelProfileID: profile, ModelProfileRevision: pr.Revision, CredentialVersion: pr.CredentialVersion, Revision: old.Revision}, expected)
	if te != nil {
		if te == storage.ErrScheduleConflict {
			return nil, actionbase.StatusError(http.StatusPreconditionFailed, "schedule revision conflict")
		}
		return nil, actionbase.InternalError(te)
	}
	return map[string]any{"schedule": v}, nil
}
func (m *Module) deleteOnce(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	if _, e := str(p, "idempotency_key"); e != nil {
		return nil, invalid(e)
	}
	id, e := str(p, "schedule_id")
	if e != nil {
		return nil, invalid(e)
	}
	expected, e := expectedRevision(p)
	if e != nil {
		return nil, invalid(e)
	}
	if state, exists, ge := m.store.GetScheduleIncludingDeleted(ctx, m.owner(), id); ge == nil && exists {
		if state.Deleted && state.Revision == expected+1 {
			return map[string]any{"deleted": true, "schedule_id": id}, nil
		}
		if state.Revision != expected {
			return nil, actionbase.StatusError(http.StatusPreconditionFailed, "schedule revision conflict")
		}
	}
	if e = m.store.DeleteScheduleCAS(ctx, m.owner(), id, expected); e != nil {
		if e == storage.ErrScheduleConflict {
			return nil, actionbase.StatusError(http.StatusPreconditionFailed, "schedule revision conflict")
		}
		return nil, actionbase.StatusError(http.StatusNotFound, e.Error())
	}
	return map[string]any{"deleted": true, "schedule_id": id}, nil
}
func (m *Module) setStatusOnce(ctx context.Context, p map[string]any, en bool) (any, *actionbase.Error) {
	if _, e := str(p, "idempotency_key"); e != nil {
		return nil, invalid(e)
	}
	id, e := str(p, "schedule_id")
	if e != nil {
		return nil, invalid(e)
	}
	expected, e := expectedRevision(p)
	if e != nil {
		return nil, invalid(e)
	}
	if en && !m.schedulerRunning() {
		return nil, actionbase.StatusError(http.StatusServiceUnavailable, "scheduler is not running")
	}
	if current, ok, ge := m.store.GetSchedule(ctx, m.owner(), id); ge == nil && ok && current.Revision == expected+1 {
		want := "disabled"
		if en {
			want = "enabled"
		}
		if current.Status == want {
			return map[string]any{"schedule": current}, nil
		}
	}
	v, e := m.store.SetScheduleStatusCAS(ctx, m.owner(), id, expected, en)
	if e != nil {
		if e == storage.ErrScheduleConflict {
			return nil, actionbase.StatusError(http.StatusPreconditionFailed, "schedule revision conflict")
		}
		return nil, actionbase.StatusError(http.StatusNotFound, e.Error())
	}
	return map[string]any{"schedule": v}, nil
}

// reconcileRunNowReplay never invokes the model. An active run deliberately
// leaves its mutation receipt pending; a completed recovery writes a terminal
// receipt through mutate so later retries observe the same authoritative run.
func (m *Module) reconcileRunNowReplay(ctx context.Context, id, runID string) (any, bool, bool) {
	prior, ok, ge := m.store.GetScheduleRun(ctx, m.owner(), id, runID)
	if ge != nil || !ok {
		return nil, false, false
	}
	terminal := func(run storage.ScheduleRun) (any, bool, bool) {
		return map[string]any{"run": run}, true, true
	}
	advance := func(sch storage.Schedule) (any, bool, bool) {
		status := "enabled"
		next := sch.NextRunAt
		if sch.TriggerKind == "one_time" {
			status = "disabled"
			next = nil
		}
		if e := m.store.AdvanceSchedule(ctx, m.owner(), id, sch.LeaseOwner, sch.Revision, sch.LeaseEpoch, next, status); e != nil {
			return map[string]any{"run": prior}, false, true
		}
		if run, found, e := m.store.GetScheduleRun(ctx, m.owner(), id, runID); e == nil && found {
			return terminal(run)
		}
		return terminal(prior)
	}
	recoveryOwner := "run-now-recovery:" + runID
	if prior.Status == "running" {
		if sch, sok, _ := m.store.GetSchedule(ctx, m.owner(), id); sok && sch.LeaseUntil != nil && !sch.LeaseUntil.After(time.Now().UTC()) {
			if recoveredLease, re := m.store.AcquireScheduleRunRecoveryLease(ctx, m.owner(), id, runID, recoveryOwner, 30*time.Second); re == nil {
				recovered, recoverErr := m.store.RecoverScheduleRun(ctx, m.owner(), id, recoveredLease.LeaseOwner, recoveredLease.LeaseEpoch, prior.ScheduledFor)
				if recoverErr == nil {
					prior = recovered
					return advance(recoveredLease)
				}
			}
		}
		return map[string]any{"run": prior}, false, true
	}
	if sch, sok, _ := m.store.GetSchedule(ctx, m.owner(), id); sok && sch.Status == "running" {
		if sch.LeaseUntil != nil && !sch.LeaseUntil.After(time.Now().UTC()) {
			if recoveredLease, e := m.store.AcquireScheduleRunRecoveryLease(ctx, m.owner(), id, runID, recoveryOwner, 30*time.Second); e == nil {
				return advance(recoveredLease)
			}
			return map[string]any{"run": prior}, false, true
		}
		if prior.LeaseEpoch == sch.LeaseEpoch {
			return advance(sch)
		}
		return map[string]any{"run": prior}, false, true
	}
	return terminal(prior)
}

func (m *Module) runNowOnce(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	if _, e := str(p, "idempotency_key"); e != nil {
		return nil, invalid(e)
	}
	id, e := str(p, "schedule_id")
	if e != nil {
		return nil, invalid(e)
	}
	key, _ := str(p, "idempotency_key")
	deterministicRunID := uuid.NewSHA1(uuid.Nil, []byte(m.owner()+"\x00agent.schedules.run_now\x00"+key)).String()
	if out, _, handled := m.reconcileRunNowReplay(ctx, id, deterministicRunID); handled {
		return out, nil
	}
	if !m.Ready() {
		return nil, actionbase.StatusError(http.StatusServiceUnavailable, "embedded scheduler is not ready")
	}
	v, ok, e := m.store.GetSchedule(ctx, m.owner(), id)
	if e != nil || !ok {
		return nil, actionbase.StatusError(http.StatusNotFound, "schedule not found")
	}
	claimOwner := "run-now:" + uuid.NewString()
	v, e = m.store.ClaimScheduleNow(ctx, m.owner(), id, claimOwner, 30*time.Second)
	if e == storage.ErrScheduleConflict {
		return nil, actionbase.StatusError(http.StatusConflict, "schedule is already running")
	}
	if e != nil {
		return nil, actionbase.InternalError(e)
	}
	// A mutation receipt reserves this deterministic run identity before the
	// model call, so retries can only ever address the same authoritative run.
	r := storage.ScheduleRun{RunID: deterministicRunID, ScheduleID: id, OwnerID: m.owner(), Status: "running", ScheduledFor: time.Now().UTC(), LeaseEpoch: v.LeaseEpoch}
	if _, created, ce := m.store.CreateScheduleRun(ctx, r, claimOwner, v.Revision, v.LeaseEpoch); ce != nil {
		e = ce
		return nil, actionbase.InternalError(e)
	} else if !created {
		return nil, actionbase.StatusError(http.StatusConflict, "schedule run already exists")
	}
	pr, e := m.profiles.ResolveModelProfilePinned(ctx, m.owner(), v.ModelProfileID, v.ModelProfileRevision, v.CredentialVersion)
	if e != nil {
		if fe := m.store.FinishScheduleRun(ctx, m.owner(), r.RunID, claimOwner, v.LeaseEpoch, "", e.Error(), time.Now().UTC()); fe != nil {
			return nil, actionbase.InternalError(fe)
		}
		if ae := m.store.AdvanceSchedule(ctx, m.owner(), id, claimOwner, v.Revision, v.LeaseEpoch, v.NextRunAt, "enabled"); ae != nil {
			return nil, actionbase.InternalError(ae)
		}
		return nil, actionbase.StatusError(http.StatusBadGateway, e.Error())
	}
	result, e := m.executeWithLeaseHeartbeat(ctx, v, claimOwner, pr)
	runErr := ""
	if e != nil {
		runErr = nativeagent.SanitizeScheduledText(e.Error(), pr.APIKey)
	} else {
		result = nativeagent.SanitizeScheduledText(result, pr.APIKey)
	}
	if fe := m.store.FinishScheduleRun(ctx, m.owner(), r.RunID, claimOwner, v.LeaseEpoch, result, runErr, time.Now().UTC()); fe != nil {
		return nil, actionbase.InternalError(fe)
	}
	if ae := m.store.AdvanceSchedule(ctx, m.owner(), id, claimOwner, v.Revision, v.LeaseEpoch, v.NextRunAt, "enabled"); ae != nil {
		return nil, actionbase.InternalError(ae)
	}
	if finished, ok, ge := m.store.GetScheduleRun(ctx, m.owner(), id, r.RunID); ge == nil && ok {
		return map[string]any{"run": finished}, nil
	}
	return map[string]any{"run": r}, nil
}

// executeWithLeaseHeartbeat keeps run_now fenced for the full model call,
// matching the background worker's lease renewal behavior.
func (m *Module) executeWithLeaseHeartbeat(ctx context.Context, s storage.Schedule, leaseOwner string, profile storage.ModelProfile) (string, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type outcome struct {
		text string
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		text, err := m.runner.ExecuteScheduled(runCtx, s.Prompt, profile, nativeagent.EmbeddedAllowedTools())
		done <- outcome{text, err}
	}()
	interval := 10 * time.Second
	if s.LeaseUntil != nil {
		remaining := time.Until(*s.LeaseUntil)
		if remaining > 0 && remaining/3 < interval {
			interval = remaining / 3
		}
	}
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case out := <-done:
			return out.text, out.err
		case <-ticker.C:
			if err := m.store.RenewScheduleLease(ctx, s.OwnerID, s.ScheduleID, leaseOwner, s.LeaseEpoch, 30*time.Second); err != nil {
				cancel()
				return "", fmt.Errorf("schedule lease expired: %w", err)
			}
		case <-ctx.Done():
			cancel()
			return "", ctx.Err()
		}
	}
}
func (m *Module) runsList(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	if e := rejectUnknown(p, "schedule_id", "limit", "cursor"); e != nil {
		return nil, invalid(e)
	}
	id, e := str(p, "schedule_id")
	if e != nil {
		return nil, invalid(e)
	}
	n, e := intVal(p, "limit")
	if e != nil {
		return nil, invalid(e)
	}
	c, e := optionalString(p, "cursor")
	if e != nil {
		return nil, invalid(e)
	}
	v, e := m.store.ListScheduleRuns(ctx, m.owner(), id, n, c)
	if e != nil {
		return nil, invalid(e)
	}
	return map[string]any{"runs": v.Runs, "next_cursor": v.NextCursor}, nil
}
func (m *Module) runGet(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	if e := rejectUnknown(p, "schedule_id", "run_id"); e != nil {
		return nil, invalid(e)
	}
	sid, e := str(p, "schedule_id")
	if e != nil {
		return nil, invalid(e)
	}
	rid, e := str(p, "run_id")
	if e != nil {
		return nil, invalid(e)
	}
	v, ok, e := m.store.GetScheduleRun(ctx, m.owner(), sid, rid)
	if e != nil {
		return nil, actionbase.InternalError(e)
	}
	if !ok {
		return nil, actionbase.StatusError(http.StatusNotFound, "schedule run not found")
	}
	return map[string]any{"run": v}, nil
}
