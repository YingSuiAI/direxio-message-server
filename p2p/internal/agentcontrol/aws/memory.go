package aws

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"
)

type memoryReplay struct {
	digest string
	view   CredentialView
	delete bool
}
type memoryTestReplay struct {
	digest string
	tested CredentialTest
}

type MemoryRepository struct {
	mu          sync.Mutex
	credentials map[string]Credentials
	history     map[string]map[int64]Credentials
	deleted     map[string]bool
	replays     map[string]memoryReplay
	testReplays map[string]memoryTestReplay
}

func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{credentials: map[string]Credentials{}, history: map[string]map[int64]Credentials{}, deleted: map[string]bool{}, replays: map[string]memoryReplay{}, testReplays: map[string]memoryTestReplay{}}
}
func (r *MemoryRepository) replayLocked(operation, key, digest string) (memoryReplay, bool, error) {
	v, ok := r.replays[operation+":"+key]
	if !ok {
		return memoryReplay{}, false, nil
	}
	if v.digest != digest {
		return memoryReplay{}, true, ErrIdempotencyConflict
	}
	return v, true, nil
}
func (r *MemoryRepository) rememberLocked(operation, key, digest string, view CredentialView, deleted bool) {
	r.replays[operation+":"+key] = memoryReplay{digest: digest, view: view, delete: deleted}
}
func (r *MemoryRepository) ReplayCredential(_ context.Context, operation, key, digest string) (CredentialView, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, hit, err := r.replayLocked(operation, key, digest)
	return v.view, hit, err
}
func (r *MemoryRepository) ReplayCredentialTest(_ context.Context, id string, expected int64, key, digest string) (CredentialTest, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var v memoryTestReplay
	ok := false
	for replayKey, candidate := range r.testReplays {
		if strings.HasPrefix(replayKey, id+":") && strings.HasSuffix(replayKey, ":"+key) {
			v, ok = candidate, true
			break
		}
	}
	if !ok {
		return CredentialTest{}, false, nil
	}
	if v.digest != digest {
		return CredentialTest{}, true, ErrIdempotencyConflict
	}
	return v.tested, true, nil
}
func (r *MemoryRepository) TestCredentialIdempotent(ctx context.Context, id string, expected int64, identity Identity, key, digest string) (CredentialTest, error) {
	r.mu.Lock()
	replayKey := id + ":" + strconv.FormatInt(expected, 10) + ":" + key
	if v, ok := r.testReplays[replayKey]; ok {
		r.mu.Unlock()
		if v.digest != digest {
			return CredentialTest{}, ErrIdempotencyConflict
		}
		return v.tested, nil
	}
	r.mu.Unlock()
	updated, err := r.RecordCredentialIdentity(ctx, id, expected, identity)
	if err != nil {
		return CredentialTest{}, err
	}
	tested := CredentialTest{CredentialID: id, Identity: identity, CredentialRevision: updated.Revision, TestedAt: updated.UpdatedAt}
	r.mu.Lock()
	r.testReplays[replayKey] = memoryTestReplay{digest: digest, tested: tested}
	r.mu.Unlock()
	return tested, nil
}
func (r *MemoryRepository) SaveCredentialIdempotent(_ context.Context, c Credentials, key, digest string) (CredentialView, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v, hit, err := r.replayLocked("credential-save", key, digest); hit {
		return v.view, err
	}
	if r.deleted[c.ID] || r.credentials[c.ID].ID != "" {
		return CredentialView{}, ErrConflict
	}
	r.credentials[c.ID] = cloneCredential(c)
	if r.history[c.ID] == nil {
		r.history[c.ID] = map[int64]Credentials{}
	}
	r.history[c.ID][c.Revision] = cloneCredential(c)
	v := c.View()
	r.rememberLocked("credential-save", key, digest, v, false)
	return v, nil
}
func (r *MemoryRepository) ReplaceCredentialIdempotent(_ context.Context, c Credentials, expected int64, key, digest string) (CredentialView, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v, hit, err := r.replayLocked("credential-replace", key, digest); hit {
		return v.view, err
	}
	old, ok := r.credentials[c.ID]
	if !ok {
		return CredentialView{}, ErrNotFound
	}
	if old.Revision != expected {
		return CredentialView{}, ErrRevisionConflict
	}
	r.credentials[c.ID] = cloneCredential(c)
	r.history[c.ID][c.Revision] = cloneCredential(c)
	v := c.View()
	r.rememberLocked("credential-replace", key, digest, v, false)
	return v, nil
}
func (r *MemoryRepository) DeleteCredentialIdempotent(_ context.Context, id string, expected int64, key, digest string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v, hit, err := r.replayLocked("credential-delete", key, digest); hit {
		_ = v
		return err
	}
	c, ok := r.credentials[id]
	if !ok || c.Revision != expected {
		return ErrRevisionConflict
	}
	r.deleted[id] = true
	delete(r.credentials, id)
	r.rememberLocked("credential-delete", key, digest, CredentialView{}, true)
	return nil
}
func (r *MemoryRepository) CreateCredential(_ context.Context, c Credentials) (Credentials, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c.Validate() != nil {
		return Credentials{}, ErrInvalid
	}
	if r.deleted[c.ID] || r.credentials[c.ID].ID != "" {
		return Credentials{}, ErrConflict
	}
	r.credentials[c.ID] = cloneCredential(c)
	if r.history[c.ID] == nil {
		r.history[c.ID] = map[int64]Credentials{}
	}
	r.history[c.ID][c.Revision] = cloneCredential(c)
	return cloneCredential(c), nil
}
func (r *MemoryRepository) GetCredential(_ context.Context, id string) (Credentials, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.credentials[id]
	if !ok {
		return Credentials{}, ErrNotFound
	}
	return cloneCredential(c), nil
}
func (r *MemoryRepository) GetCredentialRevision(_ context.Context, id string, revision int64) (Credentials, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.history[id][revision]
	if !ok {
		return Credentials{}, ErrNotFound
	}
	return cloneCredential(c), nil
}
func (r *MemoryRepository) ListCredentials(_ context.Context, size int, token string) (CredentialPage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if size < 0 || size > 100 {
		return CredentialPage{}, ErrInvalid
	}
	if size == 0 {
		size = 25
	}
	out := CredentialPage{}
	for _, c := range r.credentials {
		if c.ID > token {
			out.Items = append(out.Items, c.View())
		}
	}
	if len(out.Items) > size {
		out.NextPageToken = out.Items[size-1].ID
		out.Items = out.Items[:size]
	}
	return out, nil
}
func (r *MemoryRepository) UpdateCredential(ctx context.Context, c Credentials, expected int64) (Credentials, error) {
	v, err := r.ReplaceCredentialIdempotent(ctx, c, expected, newUUID(), canonicalDigest(c))
	if err != nil {
		return Credentials{}, err
	}
	return r.GetCredentialRevision(ctx, v.ID, v.Revision)
}
func (r *MemoryRepository) DeleteCredential(_ context.Context, id string, expected int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.credentials[id]
	if !ok || c.Revision != expected {
		return ErrRevisionConflict
	}
	r.deleted[id] = true
	delete(r.credentials, id)
	return nil
}
func (r *MemoryRepository) RecordCredentialIdentity(_ context.Context, id string, revision int64, identity Identity) (Credentials, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.credentials[id]
	if !ok || c.Revision != revision {
		return Credentials{}, ErrRevisionConflict
	}
	if c.VerifiedRevision == revision {
		if c.AccountID != identity.AccountID || c.UserARN != identity.UserARN {
			return Credentials{}, ErrConflict
		}
		return cloneCredential(c), nil
	}
	c.AccountID, c.UserARN, c.VerifiedRevision, c.UpdatedAt = identity.AccountID, identity.UserARN, revision, time.Now().UTC()
	r.credentials[id] = cloneCredential(c)
	r.history[id][revision] = cloneCredential(c)
	return cloneCredential(c), nil
}

func cloneCredential(c Credentials) Credentials {
	if c.private == nil {
		return c
	}
	c.private = &credentialPayload{c.private.accessKeyID, c.private.secretAccessKey, c.private.sessionToken}
	return c
}
