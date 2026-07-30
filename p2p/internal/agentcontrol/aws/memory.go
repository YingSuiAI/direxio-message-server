package aws

import (
	"context"
	"encoding/base64"
	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	"github.com/google/uuid"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

func encodeChangeCursor(planID, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(planID + "\x00" + id))
}
func decodeChangeCursor(planID, token string) (string, error) {
	if token == "" {
		return "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", err
	}
	parts := strings.Split(string(raw), "\x00")
	if len(parts) != 2 || parts[0] != planID || uuid.Validate(parts[1]) != nil {
		return "", ErrInvalid
	}
	return parts[1], nil
}

type MemoryRepository struct {
	mu                sync.RWMutex
	credentials       map[string]Credentials
	credentialHistory map[string]map[int64]Credentials
	credentialDeleted map[string]bool
	plans             map[string]Plan
	provisions        map[string]Provision
	changes           map[string]Change
	tasks             map[string]Task
	confirmations     map[string]coreconfirmation.Confirmation
	reservations      map[string]Reservation
	events            []ChangeEvent
	replays           map[string]memoryReplay
}

type ChangeEvent struct {
	ChangeID string
	TaskID   string
	Kind     string
	Revision int64
	At       time.Time
}

type memoryReplay struct {
	digest     string
	err        error
	credential *CredentialView
	plan       *PlanView
	change     *ChangeRequestResult
	provision  *Provision
	deleted    bool
}

func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{credentials: map[string]Credentials{}, credentialHistory: map[string]map[int64]Credentials{}, credentialDeleted: map[string]bool{}, plans: map[string]Plan{}, provisions: map[string]Provision{}, changes: map[string]Change{}, tasks: map[string]Task{}, confirmations: map[string]coreconfirmation.Confirmation{}, reservations: map[string]Reservation{}, replays: map[string]memoryReplay{}}
}

func (r *MemoryRepository) CreateProvision(_ context.Context, p Provision) (Provision, error) {
	if p.Validate() != nil {
		return Provision{}, ErrInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	plan, ok := r.plans[p.PlanID]
	if !ok {
		return Provision{}, ErrNotFound
	}
	if !ProvisionPlanMatches(p, plan) {
		return Provision{}, ErrRevisionConflict
	}
	if _, ok := r.provisions[p.ID]; ok {
		return Provision{}, ErrConflict
	}
	r.provisions[p.ID] = p
	return p, nil
}

func (r *MemoryRepository) RetryProvision(_ context.Context, provisionID string, expectedRevision int64, idempotencyKey string) (Provision, error) {
	if !validUUID(provisionID) || !validUUID(idempotencyKey) || expectedRevision < 1 {
		return Provision{}, ErrInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.replays == nil {
		r.replays = map[string]memoryReplay{}
	}
	digest := canonicalDigest(struct {
		ProvisionID string
		Revision    int64
	}{provisionID, expectedRevision})
	if v, ok, err := r.replayLocked("provision-retry", idempotencyKey, digest); ok {
		if err != nil {
			return Provision{}, err
		}
		if v.provision == nil {
			return Provision{}, ErrConflict
		}
		return *v.provision, nil
	}
	p, ok := r.provisions[provisionID]
	if !ok {
		return Provision{}, ErrNotFound
	}
	plan, ok := r.plans[p.PlanID]
	if !ok {
		return Provision{}, ErrNotFound
	}
	if p.Revision != expectedRevision || (p.State != "failed" && p.State != "destroyed") || !provisionSnapshotMatches(p, plan) {
		return Provision{}, ErrRevisionConflict
	}
	now := time.Now().UTC()
	// A failed delete means the provider may still have the active service.
	// Restore the verified readback and require a new explicit delete request;
	// failed creates and destroyed resources are safe to re-arm as planned.
	retryState := "planned"
	clearReadback := true
	selectedChangeID := ""
	if p.State == "failed" {
		var failed *Change
		for _, changeID := range []string{p.CreateChangeID, p.DestroyChangeID} {
			if changeID == "" {
				continue
			}
			change, found := r.changes[changeID]
			if !found || change.Status != ChangeFailed || change.ProvisionID != p.ID {
				continue
			}
			if failed != nil || (change.Operation != OperationCreate && change.Operation != OperationDelete) {
				return Provision{}, ErrRevisionConflict
			}
			candidate := change
			failed = &candidate
			selectedChangeID = changeID
		}
		if failed == nil {
			return Provision{}, ErrRevisionConflict
		}
		if failed.Operation == OperationDelete {
			if p.Readback.Validate() != nil {
				return Provision{}, ErrRevisionConflict
			}
			retryState, clearReadback = "active", false
		}
	}
	if selectedChangeID == "" {
		selectedChangeID = p.DestroyChangeID
		if selectedChangeID == "" {
			selectedChangeID = p.CreateChangeID
		}
	}
	p.State, p.ActiveChangeID = retryState, ""
	if clearReadback {
		p.Readback = ProvisionReadback{}
	}
	p.ReconciliationRequired, p.ErrorCode, p.ErrorSummary = false, "", ""
	p.Revision, p.UpdatedAt = p.Revision+1, now
	r.provisions[p.ID] = p
	changeID := p.DestroyChangeID
	if changeID == "" {
		changeID = p.CreateChangeID
	}
	if selectedChangeID != "" {
		changeID = selectedChangeID
	}
	r.events = append(r.events, ChangeEvent{ChangeID: changeID, TaskID: "", Kind: "provision_retry_requested", Revision: p.Revision, At: now})
	copy := p
	r.replays[replayKey("provision-retry", idempotencyKey)] = memoryReplay{digest: digest, provision: &copy}
	return p, nil
}
func (r *MemoryRepository) GetProvision(_ context.Context, id string) (Provision, error) {
	if !validUUID(id) {
		return Provision{}, ErrInvalid
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.provisions[id]
	if !ok {
		return Provision{}, ErrNotFound
	}
	return p, nil
}
func (r *MemoryRepository) GetProvisionByChange(_ context.Context, changeID string) (Provision, error) {
	if !validUUID(changeID) {
		return Provision{}, ErrInvalid
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.provisions {
		if p.ActiveChangeID == changeID || p.CreateChangeID == changeID || p.DestroyChangeID == changeID {
			return p, nil
		}
	}
	return Provision{}, ErrNotFound
}

func (r *MemoryRepository) rememberCredentialLocked(c Credentials) {
	if r.credentialHistory == nil {
		r.credentialHistory = map[string]map[int64]Credentials{}
	}
	if r.credentialHistory[c.ID] == nil {
		r.credentialHistory[c.ID] = map[int64]Credentials{}
	}
	r.credentialHistory[c.ID][c.Revision] = cloneCredential(c)
}

func replayKey(op, key string) string { return op + ":" + key }

// ReplayCredential and ReplayPlan expose the durable idempotency snapshot for
// restart/recovery tests without exposing credential secret bytes.
func (r *MemoryRepository) ReplayCredential(_ context.Context, operation, key, digest string) (CredentialView, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok, err := r.replayLocked(operation, key, digest)
	if !ok || err != nil {
		return CredentialView{}, ok, err
	}
	if v.credential == nil {
		return CredentialView{}, true, ErrConflict
	}
	return *v.credential, true, nil
}
func (r *MemoryRepository) ReplayPlan(_ context.Context, operation, key, digest string) (PlanView, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok, err := r.replayLocked(operation, key, digest)
	if !ok || err != nil {
		return PlanView{}, ok, err
	}
	if v.plan == nil {
		return PlanView{}, true, ErrConflict
	}
	return *v.plan, true, nil
}

func (r *MemoryRepository) GetTask(_ context.Context, id string) (Task, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tasks[id]
	if !ok {
		return Task{}, ErrNotFound
	}
	return t, nil
}

func (r *MemoryRepository) GetConfirmation(_ context.Context, id string) (coreconfirmation.Confirmation, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.confirmations[id]
	if !ok {
		return coreconfirmation.Confirmation{}, ErrNotFound
	}
	return c, nil
}

func (r *MemoryRepository) GetReservation(_ context.Context, id string) (Reservation, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	value, ok := r.reservations[id]
	if !ok {
		return Reservation{}, ErrNotFound
	}
	return value, nil
}

func (r *MemoryRepository) Events() []ChangeEvent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]ChangeEvent(nil), r.events...)
}

func (r *MemoryRepository) changesByConfirmationLocked(id string) (Change, bool) {
	for _, c := range r.changes {
		if c.ConfirmationID == id {
			return c, true
		}
	}
	return Change{}, false
}
func (r *MemoryRepository) replayLocked(op, key, digest string) (memoryReplay, bool, error) {
	v, ok := r.replays[replayKey(op, key)]
	if !ok {
		return memoryReplay{}, false, nil
	}
	if v.digest != digest {
		return memoryReplay{}, true, ErrIdempotencyConflict
	}
	if v.err != nil {
		return v, true, v.err
	}
	return v, true, nil
}
func (r *MemoryRepository) saveCredentialIdempotent(_ context.Context, c Credentials, key, digest string) (CredentialView, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.replays == nil {
		r.replays = map[string]memoryReplay{}
	}
	if v, ok, e := r.replayLocked("credential-save", key, digest); ok {
		if e != nil {
			return CredentialView{}, e
		}
		return *v.credential, nil
	}
	if _, ok := r.credentials[c.ID]; ok || r.credentialDeleted[c.ID] {
		return CredentialView{}, ErrConflict
	}
	r.credentials[c.ID] = cloneCredential(c)
	r.rememberCredentialLocked(c)
	v := c.View()
	r.replays[replayKey("credential-save", key)] = memoryReplay{digest: digest, credential: &v}
	return v, nil
}
func (r *MemoryRepository) replaceCredentialIdempotent(_ context.Context, c Credentials, expected int64, key, digest string) (CredentialView, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.replays == nil {
		r.replays = map[string]memoryReplay{}
	}
	if v, ok, e := r.replayLocked("credential-replace", key, digest); ok {
		if e != nil {
			return CredentialView{}, e
		}
		return *v.credential, nil
	}
	old, ok := r.credentials[c.ID]
	if !ok {
		return CredentialView{}, ErrNotFound
	}
	if old.Revision != expected || c.Revision != expected+1 {
		return CredentialView{}, ErrRevisionConflict
	}
	r.credentials[c.ID] = cloneCredential(c)
	r.rememberCredentialLocked(c)
	v := c.View()
	r.replays[replayKey("credential-replace", key)] = memoryReplay{digest: digest, credential: &v}
	return v, nil
}
func (r *MemoryRepository) deleteCredentialIdempotent(_ context.Context, id string, expected int64, key, digest string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.replays == nil {
		r.replays = map[string]memoryReplay{}
	}
	if v, ok, e := r.replayLocked("credential-delete", key, digest); ok {
		if e != nil {
			return e
		}
		if v.deleted {
			return nil
		}
	}
	old, ok := r.credentials[id]
	if !ok {
		return ErrNotFound
	}
	if old.Revision != expected {
		return ErrRevisionConflict
	}
	delete(r.credentials, id)
	r.credentialDeleted[id] = true
	v := old.View()
	r.replays[replayKey("credential-delete", key)] = memoryReplay{digest: digest, deleted: true, credential: &v}
	return nil
}
func (r *MemoryRepository) createPlanIdempotent(_ context.Context, p Plan, key, digest string) (PlanView, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.replays == nil {
		r.replays = map[string]memoryReplay{}
	}
	if v, ok, e := r.replayLocked("plan-create", key, digest); ok {
		if e != nil {
			return PlanView{}, e
		}
		return *v.plan, nil
	}
	if _, ok := r.plans[p.ID]; ok {
		return PlanView{}, ErrConflict
	}
	if current, ok := r.credentials[p.CredentialID]; !ok || current.Revision != p.CredentialRevision {
		return PlanView{}, ErrRevisionConflict
	}
	r.plans[p.ID] = clonePlan(p)
	v := p.View()
	r.replays[replayKey("plan-create", key)] = memoryReplay{digest: digest, plan: &v}
	return v, nil
}
func (r *MemoryRepository) CreateCredential(_ context.Context, c Credentials) (Credentials, error) {
	if r == nil || c.Validate() != nil {
		return Credentials{}, ErrInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.credentials[c.ID]; ok || r.credentialDeleted[c.ID] {
		return Credentials{}, ErrConflict
	}
	r.credentials[c.ID] = cloneCredential(c)
	r.rememberCredentialLocked(c)
	r.credentialDeleted[c.ID] = false
	return cloneCredential(c), nil
}
func (r *MemoryRepository) GetCredential(_ context.Context, id string) (Credentials, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.credentials[id]
	if !ok {
		return Credentials{}, ErrNotFound
	}
	return cloneCredential(c), nil
}
func (r *MemoryRepository) GetCredentialRevision(_ context.Context, id string, revision int64) (Credentials, error) {
	if revision < 1 {
		return Credentials{}, ErrInvalid
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.credentialHistory[id][revision]
	if !ok {
		return Credentials{}, ErrNotFound
	}
	return cloneCredential(c), nil
}
func (r *MemoryRepository) ListCredentials(_ context.Context, size int, token string) (CredentialPage, error) {
	if size < 0 || size > 100 {
		return CredentialPage{}, ErrInvalid
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.credentials))
	for id := range r.credentials {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	start, err := startAfter(ids, token)
	if err != nil {
		return CredentialPage{}, ErrInvalid
	}
	if start > len(ids) {
		return CredentialPage{}, ErrInvalid
	}
	end := len(ids)
	if size > 0 && end > start+size {
		end = start + size
	}
	out := make([]CredentialView, 0, end-start)
	for _, id := range ids[start:end] {
		out = append(out, r.credentials[id].View())
	}
	next := ""
	if end < len(ids) {
		next = ids[end-1]
	}
	return CredentialPage{Items: out, NextPageToken: next}, nil
}
func (r *MemoryRepository) UpdateCredential(_ context.Context, c Credentials, expected int64) (Credentials, error) {
	if c.Validate() != nil {
		return Credentials{}, ErrInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	old, ok := r.credentials[c.ID]
	if !ok {
		return Credentials{}, ErrNotFound
	}
	if old.Revision != expected {
		return Credentials{}, ErrRevisionConflict
	}
	if c.Revision != expected+1 {
		return Credentials{}, ErrRevisionConflict
	}
	r.credentials[c.ID] = cloneCredential(c)
	r.rememberCredentialLocked(c)
	return cloneCredential(c), nil
}
func (r *MemoryRepository) DeleteCredential(_ context.Context, id string, expected int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	old, ok := r.credentials[id]
	if !ok {
		return ErrNotFound
	}
	if old.Revision != expected {
		return ErrRevisionConflict
	}
	delete(r.credentials, id)
	r.credentialDeleted[id] = true
	return nil
}

func (r *MemoryRepository) RecordCredentialIdentity(_ context.Context, id string, expected int64, identity Identity) (Credentials, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.credentials[id]
	if !ok {
		return Credentials{}, ErrNotFound
	}
	if c.Revision != expected {
		return Credentials{}, ErrRevisionConflict
	}
	c.AccountID, c.UserARN, c.VerifiedRevision, c.UpdatedAt = identity.AccountID, identity.UserARN, c.Revision, time.Now().UTC()
	r.credentials[id] = cloneCredential(c)
	r.rememberCredentialLocked(c)
	return cloneCredential(c), nil
}
func (r *MemoryRepository) CreatePlan(_ context.Context, p Plan) (Plan, error) {
	if p.Validate() != nil {
		return Plan{}, ErrInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.plans[p.ID]; ok {
		return Plan{}, ErrConflict
	}
	if current, ok := r.credentials[p.CredentialID]; !ok || current.Revision != p.CredentialRevision {
		return Plan{}, ErrRevisionConflict
	}
	r.plans[p.ID] = clonePlan(p)
	return clonePlan(p), nil
}
func (r *MemoryRepository) GetPlan(_ context.Context, id string) (Plan, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.plans[id]
	if !ok {
		return Plan{}, ErrNotFound
	}
	return clonePlan(p), nil
}
func (r *MemoryRepository) ListPlans(_ context.Context, size int, token string) (PlanPage, error) {
	if size < 0 || size > 100 {
		return PlanPage{}, ErrInvalid
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.plans))
	for id := range r.plans {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	start, e := startAfter(ids, token)
	if e != nil || start > len(ids) {
		return PlanPage{}, ErrInvalid
	}
	end := len(ids)
	if size > 0 && end > start+size {
		end = start + size
	}
	out := make([]PlanView, 0, end-start)
	for _, id := range ids[start:end] {
		out = append(out, r.plans[id].View())
	}
	next := ""
	if end < len(ids) {
		next = ids[end-1]
	}
	return PlanPage{Items: out, NextPageToken: next}, nil
}
func (r *MemoryRepository) CreateChange(_ context.Context, c Change) (Change, error) {
	if !validUUID(c.ID) || !validUUID(c.PlanID) || !validUUID(c.CredentialID) || !validUUID(c.TaskID) || !validUUID(c.ConfirmationID) || !validOperation(c.Operation) || !validStage(c.Stage) || c.Revision < 1 {
		return Change{}, ErrInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.changes[c.ID]; ok {
		return Change{}, ErrConflict
	}
	r.changes[c.ID] = c
	return c, nil
}
func (r *MemoryRepository) GetChange(_ context.Context, id string) (Change, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.changes[id]
	if !ok {
		return Change{}, ErrNotFound
	}
	return c, nil
}
func (r *MemoryRepository) GetChangeByConfirmation(_ context.Context, id string) (Change, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, c := range r.changes {
		if c.ConfirmationID == id {
			return c, nil
		}
	}
	return Change{}, ErrNotFound
}
func (r *MemoryRepository) ListChanges(_ context.Context, size int, planID, token string) (ChangePage, error) {
	if size < 0 || size > 100 {
		return ChangePage{}, ErrInvalid
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.changes))
	for id, change := range r.changes {
		if planID == "" || change.PlanID == planID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	lastID, err := decodeChangeCursor(planID, token)
	if err != nil {
		return ChangePage{}, ErrInvalid
	}
	start, err := startAfter(ids, lastID)
	if err != nil || start > len(ids) {
		return ChangePage{}, ErrInvalid
	}
	end := len(ids)
	if size > 0 && end > start+size {
		end = start + size
	}
	out := make([]Change, 0, end-start)
	for _, id := range ids[start:end] {
		out = append(out, r.changes[id])
	}
	next := ""
	if end < len(ids) {
		next = encodeChangeCursor(planID, ids[end-1])
	}
	return ChangePage{Items: out, NextPageToken: next}, nil
}
func (r *MemoryRepository) UpdateChange(_ context.Context, c Change, expected int64) (Change, error) {
	if !validStage(c.Stage) {
		return Change{}, ErrInvalid
	}
	if c.Revision != expected+1 {
		return Change{}, ErrRevisionConflict
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	old, ok := r.changes[c.ID]
	if !ok {
		return Change{}, ErrNotFound
	}
	if old.Revision != expected {
		return Change{}, ErrRevisionConflict
	}
	if (old.Stage == StageSucceeded || old.Stage == StageFailed || old.Stage == StageCanceled || old.Status == ChangeSucceeded || old.Status == ChangeFailed || old.Status == ChangeCanceled) && (c.Stage != old.Stage || c.Status != old.Status) {
		return Change{}, ErrConflict
	}
	r.changes[c.ID] = c
	return c, nil
}
func pageStart(token string) (int, error) {
	if strings.TrimSpace(token) == "" {
		return 0, nil
	}
	return strconv.Atoi(token)
}
func startAfter(ids []string, token string) (int, error) {
	if strings.TrimSpace(token) == "" {
		return 0, nil
	}
	for i, id := range ids {
		if id > token {
			return i, nil
		}
	}
	return len(ids), nil
}
func cloneCredential(c Credentials) Credentials {
	if c.private != nil {
		c.private = &credentialPayload{accessKeyID: c.private.accessKeyID, secretAccessKey: c.private.secretAccessKey, sessionToken: c.private.sessionToken}
	}
	return c
}
func clonePlan(p Plan) Plan {
	p.Template = append([]byte(nil), p.Template...)
	p.Parameters = cloneMap(p.Parameters)
	p.Tags = cloneMap(p.Tags)
	p.Capabilities = append([]string(nil), p.Capabilities...)
	return p
}
