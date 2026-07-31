package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
)

const executionSecretParameterIntentSchema = "execution-secret-parameter-intent/v1"

// DatabaseExecutionSecretParameterRuntime is the durable bridge between the
// execution ledger and the provider-typed secret parameter executor. It never
// persists or returns plaintext except from ResolveAuthorizedSecretValues,
// after proving an exact consumed secret_access confirmation in one DB tx.
type DatabaseExecutionSecretParameterRuntime struct {
	db      *sql.DB
	secrets *DatabaseExecutionSecretStore
}

func NewDatabaseExecutionSecretParameterRuntime(db *sql.DB, secrets *DatabaseExecutionSecretStore) *DatabaseExecutionSecretParameterRuntime {
	return &DatabaseExecutionSecretParameterRuntime{db: db, secrets: secrets}
}

func (s *DatabaseExecutionSecretParameterRuntime) Ready() bool {
	return s != nil && s.db != nil && s.secrets != nil && s.secrets.Ready() && s.secrets.db == s.db
}

func (s *DatabaseExecutionSecretParameterRuntime) ResolveAuthorizedSecretValues(ctx context.Context, req coreaws.AuthorizedSecretRequest) (coreaws.AuthorizedSecretSet, error) {
	if !s.Ready() || req.Mode != "provision" {
		return coreaws.AuthorizedSecretSet{}, coreaws.ErrSecretParameterUnauthorized
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return coreaws.AuthorizedSecretSet{}, err
	}
	defer tx.Rollback()
	authorization, err := s.proveProvisionAuthorizationTx(ctx, tx, req, false)
	if err != nil {
		return coreaws.AuthorizedSecretSet{}, coreaws.ErrSecretParameterUnauthorized
	}
	ref := req.SecretRefs[0]
	if err = lockExecutionSecretParameterTx(ctx, tx, req.OwnerID, ref.Ref); err != nil {
		return coreaws.AuthorizedSecretSet{}, err
	}
	meta, err := loadExecutionSecret(ctx, s.db, tx, req.OwnerID, ref.Ref, 0, true)
	if err != nil || meta.Status != "active" || meta.Revision != ref.Revision || meta.Purpose != ref.Purpose || meta.BindingDigest != ref.BindingDigest {
		return coreaws.AuthorizedSecretSet{}, coreaws.ErrSecretParameterUnauthorized
	}
	value, err := s.secrets.openExecutionSecretTx(ctx, tx, meta)
	if err != nil {
		return coreaws.AuthorizedSecretSet{}, coreaws.ErrSecretParameterUnauthorized
	}
	if err = tx.Commit(); err != nil {
		clear(value)
		return coreaws.AuthorizedSecretSet{}, err
	}
	return coreaws.AuthorizedSecretSet{Authorization: authorization, Values: []coreaws.AuthorizedSecretValue{{Ref: ref, Value: value}}}, nil
}

func (s *DatabaseExecutionSecretParameterRuntime) ReserveSecretParameterIntent(ctx context.Context, intent coreaws.SecretParameterIntent) (coreaws.SecretParameterIntentRecord, bool, error) {
	if !s.Ready() || validateSecretParameterIntent(intent) != nil {
		return coreaws.SecretParameterIntentRecord{}, false, ErrExecutionStoreInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return coreaws.SecretParameterIntentRecord{}, false, err
	}
	defer tx.Rollback()
	if err = lockExecutionSecretParameterTx(ctx, tx, intent.OwnerID, intent.Request.SecretRef.Ref); err != nil {
		return coreaws.SecretParameterIntentRecord{}, false, err
	}
	authReq := authorizedRequestFromProvision(intent.Request)
	authorization, err := s.proveProvisionAuthorizationTx(ctx, tx, authReq, false)
	if err != nil {
		return coreaws.SecretParameterIntentRecord{}, false, coreaws.ErrSecretParameterUnauthorized
	}
	if err = proveSecretParameterDispatchIntentTx(ctx, tx, intent.Request); err != nil {
		return coreaws.SecretParameterIntentRecord{}, false, coreaws.ErrSecretParameterUnauthorized
	}
	meta, err := loadExecutionSecret(ctx, s.db, tx, intent.OwnerID, intent.Request.SecretRef.Ref, 0, true)
	if err != nil || meta.Status != "active" || meta.Revision != intent.Request.SecretRef.Revision || meta.Purpose != intent.Request.SecretRef.Purpose || meta.BindingDigest != intent.Request.SecretRef.BindingDigest {
		return coreaws.SecretParameterIntentRecord{}, false, coreaws.ErrSecretParameterUnauthorized
	}
	raw, err := json.Marshal(intent.Request)
	if err != nil || len(raw) == 0 || bytesContainSecretMaterial(raw) {
		return coreaws.SecretParameterIntentRecord{}, false, ErrExecutionStoreInvalid
	}
	now := s.secrets.now().UTC().Truncate(time.Microsecond)
	result, err := tx.ExecContext(ctx, `INSERT INTO core_execution_secret_parameter_intents(owner_id,fence_digest,request_digest,parameter_name,run_id,provision_stage_id,provision_attempt_id,target_id,target_revision,target_digest,secret_ref,secret_revision,secret_purpose,secret_binding_digest,confirmation_id,status,provider_version,revision,schema_version,request_json,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,'reserved',0,1,$16,$17,$18,$18) ON CONFLICT(owner_id,fence_digest) DO NOTHING`, intent.OwnerID, intent.FenceDigest, intent.RequestDigest, intent.ParameterName, intent.Request.RunID, intent.Request.StageID, intent.Request.AttemptID, intent.Request.Target.ID, intent.Request.Target.Revision, intent.Request.Target.Digest, intent.Request.SecretRef.Ref, intent.Request.SecretRef.Revision, intent.Request.SecretRef.Purpose, intent.Request.SecretRef.BindingDigest, authorization.ConfirmationID, executionSecretParameterIntentSchema, raw, now)
	if err != nil {
		return coreaws.SecretParameterIntentRecord{}, false, err
	}
	created, err := result.RowsAffected()
	if err != nil {
		return coreaws.SecretParameterIntentRecord{}, false, err
	}
	record, err := loadSecretParameterIntentTx(ctx, tx, intent.OwnerID, intent.FenceDigest, true)
	if err != nil || !sameSecretParameterIntent(record.Intent, intent) {
		return coreaws.SecretParameterIntentRecord{}, false, coreexecution.ErrConflict
	}
	// Pin the deterministic Parameter Store handle to the generic provider
	// receipt before PutParameter. SSM commands reach running through
	// RecordAccepted(command_id); provider-typed operations need the equivalent
	// provider_operation_id transition so a terminal receipt has exact evidence
	// and remains recoverable without another Put.
	var receiptID string
	if err = tx.QueryRowContext(ctx, `SELECT receipt_id::text FROM core_execution_dispatch_intents WHERE owner_id=$1 AND fence_digest=$2 AND request_digest=$3 AND run_id=$4 AND stage_id=$5 AND attempt_id=$6 AND status IN ('intent','accepted','uncertain')`, intent.OwnerID, intent.FenceDigest, intent.RequestDigest, intent.Request.RunID, intent.Request.StageID, intent.Request.AttemptID).Scan(&receiptID); err != nil {
		return coreaws.SecretParameterIntentRecord{}, false, coreexecution.ErrConflict
	}
	receiptResult, err := tx.ExecContext(ctx, `UPDATE core_execution_receipts SET provider_operation_id=$1,status='running',revision=revision+1 WHERE owner_id=$2 AND receipt_id=$3 AND attempt_id=$4 AND status='accepted' AND provider_operation_id=''`, intent.ParameterName, intent.OwnerID, receiptID, intent.Request.AttemptID)
	if err != nil {
		return coreaws.SecretParameterIntentRecord{}, false, err
	}
	if changed, rowsErr := receiptResult.RowsAffected(); rowsErr != nil {
		return coreaws.SecretParameterIntentRecord{}, false, rowsErr
	} else if changed == 0 {
		var status, providerOperation string
		if err = tx.QueryRowContext(ctx, `SELECT status,provider_operation_id FROM core_execution_receipts WHERE owner_id=$1 AND receipt_id=$2 AND attempt_id=$3`, intent.OwnerID, receiptID, intent.Request.AttemptID).Scan(&status, &providerOperation); err != nil || status != "running" || providerOperation != intent.ParameterName {
			return coreaws.SecretParameterIntentRecord{}, false, coreexecution.ErrConflict
		}
	} else if changed != 1 {
		return coreaws.SecretParameterIntentRecord{}, false, coreexecution.ErrConflict
	}
	leaseResult, err := tx.ExecContext(ctx, `UPDATE core_execution_target_mutation_leases SET provider_operation_id=$1,receipt_id=$2,revision=revision+1,updated_at=$3 WHERE owner_id=$4 AND run_id=$5 AND stage_id=$6 AND target_id=$7 AND target_revision=$8 AND status='active' AND provider_operation_id=''`, intent.ParameterName, receiptID, now, intent.OwnerID, intent.Request.RunID, intent.Request.StageID, intent.Request.Target.ID, intent.Request.Target.Revision)
	if err != nil {
		return coreaws.SecretParameterIntentRecord{}, false, err
	}
	if changed, rowsErr := leaseResult.RowsAffected(); rowsErr != nil {
		return coreaws.SecretParameterIntentRecord{}, false, rowsErr
	} else if changed == 0 {
		var providerOperation, pinnedReceipt string
		if err = tx.QueryRowContext(ctx, `SELECT provider_operation_id,COALESCE(receipt_id::text,'') FROM core_execution_target_mutation_leases WHERE owner_id=$1 AND run_id=$2 AND stage_id=$3 AND target_id=$4 AND target_revision=$5 AND status='active'`, intent.OwnerID, intent.Request.RunID, intent.Request.StageID, intent.Request.Target.ID, intent.Request.Target.Revision).Scan(&providerOperation, &pinnedReceipt); err != nil || providerOperation != intent.ParameterName || pinnedReceipt != receiptID {
			return coreaws.SecretParameterIntentRecord{}, false, coreexecution.ErrConflict
		}
	} else if changed != 1 {
		return coreaws.SecretParameterIntentRecord{}, false, coreexecution.ErrConflict
	}
	if err = tx.Commit(); err != nil {
		return coreaws.SecretParameterIntentRecord{}, false, err
	}
	return record.SecretParameterIntentRecord, created == 1, nil
}

func (s *DatabaseExecutionSecretParameterRuntime) GetSecretParameterIntent(ctx context.Context, owner string, fence coreexecution.Digest) (coreaws.SecretParameterIntentRecord, error) {
	if !s.Ready() || strings.TrimSpace(owner) == "" || !fence.Valid() {
		return coreaws.SecretParameterIntentRecord{}, ErrExecutionStoreInvalid
	}
	return loadSecretParameterIntent(ctx, s.db, nil, strings.TrimSpace(owner), fence, false)
}

func (s *DatabaseExecutionSecretParameterRuntime) RecordSecretParameterVersion(ctx context.Context, owner string, fence coreexecution.Digest, version int64) (coreaws.SecretParameterIntentRecord, error) {
	if !s.Ready() || strings.TrimSpace(owner) == "" || !fence.Valid() || version <= 0 {
		return coreaws.SecretParameterIntentRecord{}, ErrExecutionStoreInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return coreaws.SecretParameterIntentRecord{}, err
	}
	defer tx.Rollback()
	record, err := loadSecretParameterIntentTx(ctx, tx, owner, fence, true)
	if err != nil {
		return coreaws.SecretParameterIntentRecord{}, err
	}
	if record.ProviderVersion > 0 {
		if record.ProviderVersion != version {
			return coreaws.SecretParameterIntentRecord{}, coreexecution.ErrConflict
		}
		return record.SecretParameterIntentRecord, tx.Commit()
	}
	if record.Status != "reserved" && record.Status != "uncertain" {
		return coreaws.SecretParameterIntentRecord{}, coreexecution.ErrConflict
	}
	status := "versioned"
	if record.Status == "uncertain" {
		status = "uncertain"
	}
	if _, err = tx.ExecContext(ctx, `UPDATE core_execution_secret_parameter_intents SET provider_version=$1,status=$2,revision=revision+1,updated_at=NOW() WHERE owner_id=$3 AND fence_digest=$4 AND revision=$5 AND provider_version=0`, version, status, owner, fence, record.Revision); err != nil {
		return coreaws.SecretParameterIntentRecord{}, err
	}
	updated, err := loadSecretParameterIntentTx(ctx, tx, owner, fence, false)
	if err != nil {
		return coreaws.SecretParameterIntentRecord{}, err
	}
	if err = tx.Commit(); err != nil {
		return coreaws.SecretParameterIntentRecord{}, err
	}
	return updated.SecretParameterIntentRecord, nil
}

func (s *DatabaseExecutionSecretParameterRuntime) MarkSecretParameterUncertain(ctx context.Context, owner string, fence coreexecution.Digest) error {
	return s.transitionSecretParameterStatus(ctx, owner, fence, "uncertain")
}

func (s *DatabaseExecutionSecretParameterRuntime) CompleteSecretParameter(ctx context.Context, lease coreaws.SecretParameterLease) error {
	if !s.Ready() || validateStoredSecretParameterLease(lease) != nil {
		return ErrExecutionStoreInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	record, err := loadSecretParameterIntentTx(ctx, tx, lease.OwnerID, lease.FenceDigest, true)
	if err != nil {
		return err
	}
	if !leaseMatchesIntent(lease, record.Intent) || record.Intent.RequestDigest != lease.RequestDigest || record.ProviderVersion > 0 && record.ProviderVersion != lease.ProviderVersion {
		return coreexecution.ErrConflict
	}
	if record.Status == "completed" {
		if record.ProviderVersion != lease.ProviderVersion || record.LeaseDigest != mustCanonicalDigest(lease) {
			return coreexecution.ErrConflict
		}
		return tx.Commit()
	}
	if record.Status == "revoked" {
		return coreexecution.ErrConflict
	}
	if _, err = s.proveProvisionAuthorizationTx(ctx, tx, authorizedRequestFromProvision(record.Intent.Request), false); err != nil {
		return coreaws.ErrSecretParameterUnauthorized
	}
	raw, err := json.Marshal(lease)
	if err != nil || bytesContainSecretMaterial(raw) {
		return ErrExecutionStoreInvalid
	}
	digest := mustCanonicalDigest(lease)
	if _, err = tx.ExecContext(ctx, `UPDATE core_execution_secret_parameter_intents SET status='completed',provider_version=$1,lease_json=$2,lease_digest=$3,revision=revision+1,updated_at=NOW() WHERE owner_id=$4 AND fence_digest=$5 AND revision=$6 AND status IN ('reserved','versioned','uncertain')`, lease.ProviderVersion, raw, digest, lease.OwnerID, lease.FenceDigest, record.Revision); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *DatabaseExecutionSecretParameterRuntime) RevokeSecretParameter(ctx context.Context, owner string, fence coreexecution.Digest) error {
	return s.transitionSecretParameterStatus(ctx, owner, fence, "revoked")
}

// ResolveActiveSecretParameterLease reloads one exact durable provider handle.
// It is intended for bounded cleanup workers and never opens secret material.
func (s *DatabaseExecutionSecretParameterRuntime) ResolveActiveSecretParameterLease(ctx context.Context, owner string, fence coreexecution.Digest) (coreaws.SecretParameterLease, error) {
	owner = strings.TrimSpace(owner)
	if !s.Ready() || owner == "" || !fence.Valid() {
		return coreaws.SecretParameterLease{}, ErrExecutionStoreInvalid
	}
	record, err := loadSecretParameterIntentStored(ctx, s.db, nil, owner, fence, false)
	if err != nil {
		return coreaws.SecretParameterLease{}, err
	}
	if record.Status != "completed" || record.Lease == nil {
		return coreaws.SecretParameterLease{}, coreexecution.ErrConflict
	}
	return *record.Lease, nil
}

// ListReapableSecretParameterLeases returns a bounded, deterministic set of
// completed handles whose run terminated before any direct consumer stage was
// dispatched. Provider cleanup remains an explicit idempotent operation; the
// caller must delete the exact handle first and only then call
// RevokeSecretParameter. Listing is deliberately safe to repeat after a crash.
func (s *DatabaseExecutionSecretParameterRuntime) ListReapableSecretParameterLeases(ctx context.Context, limit int) ([]coreaws.SecretParameterLease, error) {
	if !s.Ready() || limit < 1 || limit > 100 {
		return nil, ErrExecutionStoreInvalid
	}
	rows, err := s.db.QueryContext(ctx, `SELECT i.owner_id,i.fence_digest
		FROM core_execution_secret_parameter_intents i
		JOIN core_execution_runs r ON r.owner_id=i.owner_id AND r.run_id=i.run_id
		WHERE i.status='completed'
		  AND r.status IN ('failed','canceled','rejected','expired')
		  AND NOT EXISTS (
			SELECT 1
			FROM core_execution_run_stage_dependencies d
			JOIN core_execution_dispatch_intents dispatched
			  ON dispatched.owner_id=d.owner_id
			 AND dispatched.run_id=d.run_id
			 AND dispatched.stage_id=d.stage_id
			WHERE d.owner_id=i.owner_id
			  AND d.run_id=i.run_id
			  AND d.depends_on_stage_id=i.provision_stage_id
		  )
		ORDER BY i.updated_at,i.owner_id,i.fence_digest
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	type identity struct {
		owner string
		fence coreexecution.Digest
	}
	identities := make([]identity, 0, limit)
	for rows.Next() {
		var item identity
		if err = rows.Scan(&item.owner, &item.fence); err != nil {
			rows.Close()
			return nil, err
		}
		identities = append(identities, item)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	leases := make([]coreaws.SecretParameterLease, 0, len(identities))
	for _, item := range identities {
		lease, loadErr := s.ResolveActiveSecretParameterLease(ctx, item.owner, item.fence)
		if errors.Is(loadErr, coreexecution.ErrConflict) || errors.Is(loadErr, coreexecution.ErrNotFound) {
			continue
		}
		if loadErr != nil {
			return nil, loadErr
		}
		leases = append(leases, lease)
	}
	return leases, nil
}

func (s *DatabaseExecutionSecretParameterRuntime) transitionSecretParameterStatus(ctx context.Context, owner string, fence coreexecution.Digest, target string) error {
	owner = strings.TrimSpace(owner)
	if !s.Ready() || owner == "" || !fence.Valid() || target != "uncertain" && target != "revoked" {
		return ErrExecutionStoreInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	record, err := loadSecretParameterIntentTx(ctx, tx, owner, fence, true)
	if err != nil {
		return err
	}
	if record.Status == target {
		return tx.Commit()
	}
	if target == "uncertain" && (record.Status == "completed" || record.Status == "revoked") {
		return coreexecution.ErrConflict
	}
	if target == "revoked" && record.Status == "revoked" {
		return tx.Commit()
	}
	if _, err = tx.ExecContext(ctx, `UPDATE core_execution_secret_parameter_intents SET status=$1,revision=revision+1,updated_at=NOW() WHERE owner_id=$2 AND fence_digest=$3 AND revision=$4`, target, owner, fence, record.Revision); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *DatabaseExecutionSecretParameterRuntime) ResolveAuthorizedSecretParameterLease(ctx context.Context, req coreaws.AuthorizedSecretRequest) (coreaws.SecretParameterLease, coreaws.SecretAccessAuthorization, error) {
	if !s.Ready() || req.Mode != "consume" || len(req.SecretRefs) != 1 {
		return coreaws.SecretParameterLease{}, coreaws.SecretAccessAuthorization{}, coreaws.ErrSecretParameterUnauthorized
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return coreaws.SecretParameterLease{}, coreaws.SecretAccessAuthorization{}, err
	}
	defer tx.Rollback()
	consumerStageKey, err := proveConsumerSecretUseTx(ctx, tx, req)
	if err != nil {
		return coreaws.SecretParameterLease{}, coreaws.SecretAccessAuthorization{}, coreaws.ErrSecretParameterUnauthorized
	}
	rows, err := tx.QueryContext(ctx, `SELECT i.fence_digest FROM core_execution_secret_parameter_intents i JOIN core_execution_run_stage_dependencies d ON d.owner_id=i.owner_id AND d.run_id=i.run_id AND d.depends_on_stage_id=i.provision_stage_id JOIN core_execution_run_stages consumer ON consumer.owner_id=d.owner_id AND consumer.run_id=d.run_id AND consumer.stage_id=d.stage_id WHERE i.owner_id=$1 AND i.run_id=$2 AND consumer.stage_id=$3 AND consumer.plan_stage_key=$4 AND i.target_id=$5 AND i.target_revision=$6 AND i.target_digest=$7 AND i.secret_ref=$8 AND i.secret_revision=$9 AND i.secret_purpose=$10 AND i.secret_binding_digest=$11 AND i.status='completed' ORDER BY i.fence_digest FOR KEY SHARE OF i,d,consumer`, req.OwnerID, req.RunID, req.StageID, consumerStageKey, req.TargetID, req.TargetRevision, req.TargetDigest, req.SecretRefs[0].Ref, req.SecretRefs[0].Revision, req.SecretRefs[0].Purpose, req.SecretRefs[0].BindingDigest)
	if err != nil {
		return coreaws.SecretParameterLease{}, coreaws.SecretAccessAuthorization{}, err
	}
	var fences []coreexecution.Digest
	for rows.Next() {
		var fence coreexecution.Digest
		if err = rows.Scan(&fence); err != nil {
			rows.Close()
			return coreaws.SecretParameterLease{}, coreaws.SecretAccessAuthorization{}, err
		}
		fences = append(fences, fence)
	}
	if err = rows.Close(); err != nil || len(fences) != 1 {
		return coreaws.SecretParameterLease{}, coreaws.SecretAccessAuthorization{}, coreaws.ErrSecretParameterUnauthorized
	}
	record, err := loadSecretParameterIntentTx(ctx, tx, req.OwnerID, fences[0], false)
	if err != nil || record.Status != "completed" || record.Lease == nil {
		return coreaws.SecretParameterLease{}, coreaws.SecretAccessAuthorization{}, coreaws.ErrSecretParameterUnauthorized
	}
	authorization, err := s.proveSucceededProvisionAuthorizationTx(ctx, tx, record.Intent.Request)
	if err != nil || record.Lease.ProvisionStageID != authorization.StageID || !leaseMatchesIntent(*record.Lease, record.Intent) {
		return coreaws.SecretParameterLease{}, coreaws.SecretAccessAuthorization{}, coreaws.ErrSecretParameterUnauthorized
	}
	if err = tx.Commit(); err != nil {
		return coreaws.SecretParameterLease{}, coreaws.SecretAccessAuthorization{}, err
	}
	return *record.Lease, authorization, nil
}

type storedSecretParameterRecord struct {
	coreaws.SecretParameterIntentRecord
	Lease       *coreaws.SecretParameterLease
	LeaseDigest coreexecution.Digest
}

func loadSecretParameterIntent(ctx context.Context, db *sql.DB, tx *sql.Tx, owner string, fence coreexecution.Digest, lock bool) (coreaws.SecretParameterIntentRecord, error) {
	stored, err := loadSecretParameterIntentStored(ctx, db, tx, owner, fence, lock)
	return stored.SecretParameterIntentRecord, err
}

func loadSecretParameterIntentTx(ctx context.Context, tx *sql.Tx, owner string, fence coreexecution.Digest, lock bool) (storedSecretParameterRecord, error) {
	return loadSecretParameterIntentStored(ctx, nil, tx, owner, fence, lock)
}

func loadSecretParameterIntentStored(ctx context.Context, db *sql.DB, tx *sql.Tx, owner string, fence coreexecution.Digest, lock bool) (storedSecretParameterRecord, error) {
	query := `SELECT owner_id,fence_digest,request_digest,parameter_name,status,provider_version,revision,request_json,lease_json,COALESCE(lease_digest,'') FROM core_execution_secret_parameter_intents WHERE owner_id=$1 AND fence_digest=$2`
	if lock {
		query += ` FOR UPDATE`
	}
	var stored storedSecretParameterRecord
	var rawRequest []byte
	var rawLease []byte
	var err error
	row := func() *sql.Row {
		if tx != nil {
			return tx.QueryRowContext(ctx, query, owner, fence)
		}
		return db.QueryRowContext(ctx, query, owner, fence)
	}()
	err = row.Scan(&stored.Intent.OwnerID, &stored.Intent.FenceDigest, &stored.Intent.RequestDigest, &stored.Intent.ParameterName, &stored.Status, &stored.ProviderVersion, &stored.Revision, &rawRequest, &rawLease, &stored.LeaseDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return storedSecretParameterRecord{}, coreexecution.ErrNotFound
	}
	if err != nil {
		return storedSecretParameterRecord{}, err
	}
	if json.Unmarshal(rawRequest, &stored.Intent.Request) != nil || !canonicalJSONMatches(rawRequest, stored.Intent.Request) || validateSecretParameterIntent(stored.Intent) != nil {
		return storedSecretParameterRecord{}, ErrExecutionStoreDrift
	}
	if len(rawLease) != 0 {
		var lease coreaws.SecretParameterLease
		if strictJSON(rawLease, &lease) != nil || validateStoredSecretParameterLease(lease) != nil || stored.LeaseDigest != mustCanonicalDigest(lease) || !leaseMatchesIntent(lease, stored.Intent) {
			return storedSecretParameterRecord{}, ErrExecutionStoreDrift
		}
		stored.Lease = &lease
	}
	return stored, nil
}

func (s *DatabaseExecutionSecretParameterRuntime) proveProvisionAuthorizationTx(ctx context.Context, tx *sql.Tx, req coreaws.AuthorizedSecretRequest, succeeded bool) (coreaws.SecretAccessAuthorization, error) {
	if req.Mode != "provision" || len(req.SecretRefs) != 1 {
		return coreaws.SecretAccessAuthorization{}, coreaws.ErrSecretParameterUnauthorized
	}
	var planID, planDigest, runDigest, stageKey, stageDigest, targetID, targetDigest, stageStatus, confirmationID, confirmationState string
	var planRevision, currentRunRevision, confirmationRunRevision, stageRevision, targetRevision uint64
	var stageRaw, bindingRaw, stepRaw []byte
	var attemptStatus string
	err := tx.QueryRowContext(ctx, `SELECT r.plan_id::text,r.plan_revision,r.plan_digest,r.run_digest,r.revision,s.run_revision,s.plan_stage_key,s.stage_revision,s.plan_stage_digest,s.target_id::text,s.target_revision,s.target_digest,s.status,s.confirmation_id::text,c.state,c.binding_json,ps.snapshot_json,a.status,pstep.snapshot_json FROM core_execution_runs r JOIN core_execution_run_stages s ON s.owner_id=r.owner_id AND s.run_id=r.run_id JOIN agent_confirmations c ON c.owner_id=s.owner_id AND c.confirmation_id=s.confirmation_id JOIN core_execution_plan_stages ps ON ps.owner_id=s.owner_id AND ps.plan_id=s.plan_id AND ps.plan_revision=s.plan_revision AND ps.stage_key=s.plan_stage_key JOIN core_execution_step_attempts a ON a.owner_id=s.owner_id AND a.run_id=s.run_id AND a.stage_id=s.stage_id JOIN core_execution_plan_steps pstep ON pstep.owner_id=a.owner_id AND pstep.plan_id=a.plan_id AND pstep.plan_revision=a.plan_revision AND pstep.stage_key=a.plan_stage_key AND pstep.step_set=a.step_set AND pstep.step_key=a.step_key WHERE s.owner_id=$1 AND s.run_id=$2 AND s.stage_id=$3 AND a.attempt_id=$4 FOR KEY SHARE OF r,s,c,ps,a,pstep`, req.OwnerID, req.RunID, req.StageID, req.AttemptID).Scan(&planID, &planRevision, &planDigest, &runDigest, &currentRunRevision, &confirmationRunRevision, &stageKey, &stageRevision, &stageDigest, &targetID, &targetRevision, &targetDigest, &stageStatus, &confirmationID, &confirmationState, &bindingRaw, &stageRaw, &attemptStatus, &stepRaw)
	if err != nil {
		return coreaws.SecretAccessAuthorization{}, err
	}
	wantStage := string(coreexecution.StageRunning)
	wantAttempt := string(coreexecution.AttemptRunning)
	if succeeded {
		wantStage, wantAttempt = string(coreexecution.StageSucceeded), string(coreexecution.AttemptSucceeded)
	}
	stageSnapshot, stageErr := decodeStoredStage(stageRaw)
	stepSnapshot, stepErr := decodeStoredStep(stepRaw)
	binding, bindingErr := parseExecutionBindingJSON(bindingRaw)
	secretDigest, digestErr := coreexecution.CanonicalDigest(req.SecretRefs)
	if stageErr != nil || stepErr != nil || bindingErr != nil || digestErr != nil || confirmationState != string(coreconfirmation.StateConsumed) ||
		planID != req.PlanID || planRevision != req.PlanRevision || coreexecution.Digest(planDigest) != req.PlanDigest || coreexecution.Digest(runDigest) != req.RunDigest || confirmationRunRevision != req.RunRevision || currentRunRevision < req.RunRevision || stageRevision != req.StageRevision || coreexecution.Digest(stageDigest) != req.StageDigest || targetID != req.TargetID || targetRevision != req.TargetRevision || coreexecution.Digest(targetDigest) != req.TargetDigest ||
		stageStatus != wantStage && !(succeeded == false && stageStatus == string(coreexecution.StageUncertain)) || attemptStatus != wantAttempt && !(succeeded == false && attemptStatus == string(coreexecution.AttemptUncertain)) ||
		stageSnapshot.Stage.StageKey != stageKey || stageSnapshot.Stage.Gate != coreexecution.GateSecretAccess || stageSnapshot.Stage.Risk != coreexecution.RiskR2 || len(stageSnapshot.Stage.Steps) != 1 || stepSnapshot.StepSet != coreexecution.StepSetForward || stepSnapshot.Step.Kind != coreexecution.StepSecretProvision || len(stepSnapshot.Step.SecretRefs) != 1 || stepSnapshot.Step.SecretRefs[0] != req.SecretRefs[0] ||
		!bindingMatchesAuthorizedRequest(binding, req, confirmationRunRevision, coreexecution.GateSecretAccess, secretDigest) {
		return coreaws.SecretAccessAuthorization{}, coreaws.ErrSecretParameterUnauthorized
	}
	return coreaws.SecretAccessAuthorization{RunID: req.RunID, StageID: req.StageID, ConfirmationID: confirmationID, Gate: coreexecution.GateSecretAccess, StageStatus: coreexecution.StageStatus(stageStatus), SecretGrantDigest: secretDigest}, nil
}

func (s *DatabaseExecutionSecretParameterRuntime) proveSucceededProvisionAuthorizationTx(ctx context.Context, tx *sql.Tx, req coreaws.SecretParameterProvisionRequest) (coreaws.SecretAccessAuthorization, error) {
	authReq := authorizedRequestFromProvision(req)
	return s.proveProvisionAuthorizationTx(ctx, tx, authReq, true)
}

func proveConsumerSecretUseTx(ctx context.Context, tx *sql.Tx, req coreaws.AuthorizedSecretRequest) (string, error) {
	var planID, planDigest, runDigest, stageKey, stageDigest, targetID, targetDigest, stageStatus, confirmationState string
	var planRevision, currentRunRevision, confirmationRunRevision, stageRevision, targetRevision uint64
	var stageRaw, stepRaw, bindingRaw []byte
	var attemptStatus string
	err := tx.QueryRowContext(ctx, `SELECT r.plan_id::text,r.plan_revision,r.plan_digest,r.run_digest,r.revision,s.run_revision,s.plan_stage_key,s.stage_revision,s.plan_stage_digest,s.target_id::text,s.target_revision,s.target_digest,s.status,c.state,c.binding_json,ps.snapshot_json,a.status,pstep.snapshot_json FROM core_execution_runs r JOIN core_execution_run_stages s ON s.owner_id=r.owner_id AND s.run_id=r.run_id JOIN agent_confirmations c ON c.owner_id=s.owner_id AND c.confirmation_id=s.confirmation_id JOIN core_execution_plan_stages ps ON ps.owner_id=s.owner_id AND ps.plan_id=s.plan_id AND ps.plan_revision=s.plan_revision AND ps.stage_key=s.plan_stage_key JOIN core_execution_step_attempts a ON a.owner_id=s.owner_id AND a.run_id=s.run_id AND a.stage_id=s.stage_id JOIN core_execution_plan_steps pstep ON pstep.owner_id=a.owner_id AND pstep.plan_id=a.plan_id AND pstep.plan_revision=a.plan_revision AND pstep.stage_key=a.plan_stage_key AND pstep.step_set=a.step_set AND pstep.step_key=a.step_key WHERE s.owner_id=$1 AND s.run_id=$2 AND s.stage_id=$3 AND a.attempt_id=$4 FOR KEY SHARE OF r,s,c,ps,a,pstep`, req.OwnerID, req.RunID, req.StageID, req.AttemptID).Scan(&planID, &planRevision, &planDigest, &runDigest, &currentRunRevision, &confirmationRunRevision, &stageKey, &stageRevision, &stageDigest, &targetID, &targetRevision, &targetDigest, &stageStatus, &confirmationState, &bindingRaw, &stageRaw, &attemptStatus, &stepRaw)
	if err != nil {
		return "", err
	}
	stageSnapshot, stageErr := decodeStoredStage(stageRaw)
	stepSnapshot, stepErr := decodeStoredStep(stepRaw)
	binding, bindingErr := parseExecutionBindingJSON(bindingRaw)
	secretDigest, digestErr := coreexecution.CanonicalDigest(req.SecretRefs)
	gate := stageSnapshot.Stage.Gate
	if stageErr != nil || stepErr != nil || bindingErr != nil || digestErr != nil || confirmationState != string(coreconfirmation.StateConsumed) || len(req.SecretRefs) != 1 ||
		planID != req.PlanID || planRevision != req.PlanRevision || coreexecution.Digest(planDigest) != req.PlanDigest || coreexecution.Digest(runDigest) != req.RunDigest || confirmationRunRevision != req.RunRevision || currentRunRevision < req.RunRevision || stageRevision != req.StageRevision || coreexecution.Digest(stageDigest) != req.StageDigest || targetID != req.TargetID || targetRevision != req.TargetRevision || coreexecution.Digest(targetDigest) != req.TargetDigest ||
		stageStatus != string(coreexecution.StageRunning) && stageStatus != string(coreexecution.StageUncertain) || attemptStatus != string(coreexecution.AttemptRunning) && attemptStatus != string(coreexecution.AttemptUncertain) || stageSnapshot.Stage.StageKey != stageKey || gate != coreexecution.GateRemoteExecution && gate != coreexecution.GateRemotePrivilegedExecution || stepSnapshot.StepSet != coreexecution.StepSetForward || len(stepSnapshot.Step.SecretRefs) != 1 || stepSnapshot.Step.SecretRefs[0] != req.SecretRefs[0] || !bindingMatchesAuthorizedRequest(binding, req, confirmationRunRevision, gate, secretDigest) {
		return "", coreaws.ErrSecretParameterUnauthorized
	}
	return stageKey, nil
}

func bindingMatchesAuthorizedRequest(binding coreconfirmation.Binding, req coreaws.AuthorizedSecretRequest, confirmationRunRevision uint64, gate coreexecution.Gate, secretDigest coreexecution.Digest) bool {
	normalized, err := binding.Normalize()
	return err == nil && confirmationRunRevision == req.RunRevision && normalized.OwnerID == req.OwnerID && normalized.OperationDomain == "execution:v2:"+string(gate) && normalized.PlanID == req.PlanID && normalized.PlanRevision == int64(req.PlanRevision) && normalized.PlanDigest == coreconfirmation.Digest(req.PlanDigest) && normalized.RunID == req.RunID && normalized.RunRevision == int64(req.RunRevision) && normalized.StageID == req.StageID && normalized.StageRevision == int64(req.StageRevision) && normalized.StageDigest == coreconfirmation.Digest(req.StageDigest) && normalized.TargetID == req.TargetID && normalized.TargetRevision == int64(req.TargetRevision) && normalized.TargetDigest == coreconfirmation.Digest(req.TargetDigest) && normalized.GateType == string(gate) && normalized.SecretGrantDigest == coreconfirmation.Digest(secretDigest)
}

func authorizedRequestFromProvision(req coreaws.SecretParameterProvisionRequest) coreaws.AuthorizedSecretRequest {
	return coreaws.AuthorizedSecretRequest{Mode: "provision", OwnerID: req.OwnerID, PlanID: req.PlanID, PlanRevision: req.PlanRevision, PlanDigest: req.PlanDigest, RunID: req.RunID, RunRevision: req.RunRevision, RunDigest: req.RunDigest, StageID: req.StageID, StageRevision: req.StageRevision, StageDigest: req.StageDigest, AttemptID: req.AttemptID, TargetID: req.Target.ID, TargetRevision: req.Target.Revision, TargetDigest: req.Target.Digest, SecretRefs: []coreexecution.CredentialRef{req.SecretRef}}
}

func proveSecretParameterDispatchIntentTx(ctx context.Context, tx *sql.Tx, req coreaws.SecretParameterProvisionRequest) error {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM core_execution_dispatch_intents WHERE owner_id=$1 AND fence_digest=$2 AND request_digest=$3 AND run_id=$4 AND stage_id=$5 AND attempt_id=$6 AND target_id=$7 AND target_revision=$8 AND target_digest=$9 AND plan_id=$10 AND plan_revision=$11 AND plan_digest=$12 AND stage_revision=$13 AND stage_digest=$14 AND step_revision=$15 AND step_digest=$16 AND step_set='forward' AND status IN ('intent','accepted','uncertain')`, req.OwnerID, req.FenceDigest, req.RequestDigest, req.RunID, req.StageID, req.AttemptID, req.Target.ID, req.Target.Revision, req.Target.Digest, req.PlanID, req.PlanRevision, req.PlanDigest, req.StageRevision, req.StageDigest, req.StepRevision, req.StepDigest).Scan(&count)
	if err != nil || count != 1 {
		return coreaws.ErrSecretParameterUnauthorized
	}
	return nil
}

func validateSecretParameterIntent(intent coreaws.SecretParameterIntent) error {
	req := intent.Request
	if strings.TrimSpace(intent.OwnerID) == "" || intent.OwnerID != req.OwnerID || !intent.FenceDigest.Valid() || intent.FenceDigest != req.FenceDigest || !intent.RequestDigest.Valid() || intent.RequestDigest != req.RequestDigest || len(req.SecretRef.Ref) == 0 || req.SecretRef.Purpose != coreaws.ExecutionSecretPurposeAIProviderAPIKey || req.SecretRef.Revision == 0 || !req.SecretRef.BindingDigest.Valid() || req.Delivery != coreaws.SecretParameterDeliveryTargetSecure {
		return ErrExecutionStoreInvalid
	}
	name, err := coreaws.SecretParameterName(req.Target.ID, req.AttemptID, req.SecretRef)
	if err != nil || name != intent.ParameterName {
		return ErrExecutionStoreInvalid
	}
	digest, err := coreaws.CanonicalSecretParameterRequestDigest(req)
	if err != nil || digest != intent.RequestDigest {
		return ErrExecutionStoreInvalid
	}
	access, secret, token := req.Credential.StoredSecretBytes()
	hasPlaintext := len(access) != 0 || len(secret) != 0 || len(token) != 0
	clear(access)
	clear(secret)
	clear(token)
	if hasPlaintext || req.Credential.ID != req.CredentialID || uint64(req.Credential.Revision) != req.CredentialRevision {
		return ErrExecutionStoreInvalid
	}
	return nil
}

func validateStoredSecretParameterLease(lease coreaws.SecretParameterLease) error {
	name, err := coreaws.SecretParameterName(lease.TargetID, lease.ProvisionAttemptID, lease.SecretRef)
	if err != nil || lease.SchemaVersion != "execution-secret-parameter/v1" || strings.TrimSpace(lease.OwnerID) == "" || !coreexecution.ValidateUUID(lease.RunID) || !coreexecution.ValidateUUID(lease.ProvisionStageID) || !coreexecution.ValidateUUID(lease.ProvisionAttemptID) || !coreexecution.ValidateUUID(lease.TargetID) || lease.TargetRevision == 0 || !lease.TargetDigest.Valid() || name != lease.ParameterName || lease.ContainerMountPath != "/run/secrets/dirextalk/"+lease.SecretRef.Purpose || !lease.FenceDigest.Valid() || !lease.RequestDigest.Valid() || lease.ProviderVersion <= 0 {
		return ErrExecutionStoreInvalid
	}
	return nil
}

func leaseMatchesIntent(lease coreaws.SecretParameterLease, intent coreaws.SecretParameterIntent) bool {
	return lease.OwnerID == intent.OwnerID && lease.RunID == intent.Request.RunID && lease.ProvisionStageID == intent.Request.StageID && lease.ProvisionAttemptID == intent.Request.AttemptID && lease.TargetID == intent.Request.Target.ID && lease.TargetRevision == intent.Request.Target.Revision && lease.TargetDigest == intent.Request.Target.Digest && lease.SecretRef == intent.Request.SecretRef && lease.ParameterName == intent.ParameterName && lease.FenceDigest == intent.FenceDigest && lease.RequestDigest == intent.RequestDigest
}

func sameSecretParameterIntent(a, b coreaws.SecretParameterIntent) bool {
	ad, ae := coreexecution.CanonicalDigest(a)
	bd, be := coreexecution.CanonicalDigest(b)
	return ae == nil && be == nil && ad == bd
}

func mustCanonicalDigest(v any) coreexecution.Digest {
	d, _ := coreexecution.CanonicalDigest(v)
	return d
}

func lockExecutionSecretParameterTx(ctx context.Context, tx *sql.Tx, owner, secretRef string) error {
	if tx == nil || strings.TrimSpace(owner) == "" || !coreexecution.ValidateUUID(secretRef) {
		return ErrExecutionStoreInvalid
	}
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, canonicalAdvisoryLockIdentity("execution-secret-parameter", owner, secretRef))
	return err
}

func canonicalJSONMatches(raw []byte, value any) bool {
	var stored, expected any
	expectedRaw, err := json.Marshal(value)
	if err != nil || json.Unmarshal(raw, &stored) != nil || json.Unmarshal(expectedRaw, &expected) != nil {
		return false
	}
	a, errA := coreexecution.CanonicalDigest(stored)
	b, errB := coreexecution.CanonicalDigest(expected)
	return errA == nil && errB == nil && a == b
}

func bytesContainSecretMaterial(raw []byte) bool {
	lower := strings.ToLower(string(raw))
	for _, forbidden := range []string{"access_key_id\"", "secret_access_key\"", "session_token\"", "api_key_value", "plaintext", "ciphertext"} {
		if strings.Contains(lower, forbidden) {
			return true
		}
	}
	return false
}

var (
	_ coreaws.SecretParameterIntentStore   = (*DatabaseExecutionSecretParameterRuntime)(nil)
	_ coreaws.AuthorizedSecretResolver     = (*DatabaseExecutionSecretParameterRuntime)(nil)
	_ coreaws.SecretParameterLeaseResolver = (*DatabaseExecutionSecretParameterRuntime)(nil)
)
