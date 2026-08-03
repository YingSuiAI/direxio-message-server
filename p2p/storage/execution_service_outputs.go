package storage

// Generic terminal output materialization for successful execution.v2
// service runs. The filesystem publish intentionally precedes the database
// transaction: content addresses make that side effect idempotent, while a
// restart can recover any files published before the metadata transaction.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/artifactstore"
	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	"github.com/google/uuid"
)

const (
	serviceOutputSchema    = "execution-service-output/v2"
	serviceOutputMediaType = "text/markdown"
	serviceOutputMaxSize   = int64(256 * 1024)
)

type ExecutionServiceOutputMaterializer struct {
	store     *DatabaseExecutionStore
	artifacts *artifactstore.Store
	owner     string
}

func NewExecutionServiceOutputMaterializer(store *DatabaseExecutionStore, artifacts *artifactstore.Store, owner string) *ExecutionServiceOutputMaterializer {
	owner = strings.TrimSpace(owner)
	if store == nil || store.db == nil || artifacts == nil || owner == "" {
		return nil
	}
	return &ExecutionServiceOutputMaterializer{store: store, artifacts: artifacts, owner: owner}
}

func (m *ExecutionServiceOutputMaterializer) Ready() bool {
	return m != nil && m.store != nil && m.store.db != nil && m.artifacts != nil && strings.TrimSpace(m.owner) != ""
}

type serviceProbeFact struct {
	Kind           string
	Mode           string
	Endpoint       string
	ExpectedStatus []int
}

type serviceOutputDocument struct {
	ArtifactID string
	Kind       string
	Body       []byte
	Digest     coreexecution.Digest
}

type serviceOutputSpec struct {
	Run        coreexecution.ExecutionRun
	Plan       coreexecution.PlanSnapshot
	Target     coreexecution.ExecutionTarget
	BindingID  string
	ReleaseID  string
	Protocol   string
	Endpoint   string
	Usage      serviceOutputDocument
	Runbook    serviceOutputDocument
	ProbeFacts []serviceProbeFact
}

type publishedServiceOutput struct {
	Document serviceOutputDocument
	Metadata artifactstore.Metadata
}

// EnsureRun creates the two immutable documents and commits their metadata,
// the binding, the deployment projection, and both audit events atomically.
func (m *ExecutionServiceOutputMaterializer) EnsureRun(ctx context.Context, runID string) (coreexecution.ServiceBinding, error) {
	if m == nil || m.store == nil || m.artifacts == nil || !validUUID(runID) {
		return coreexecution.ServiceBinding{}, ErrExecutionStoreInvalid
	}
	spec, err := m.loadSpec(ctx, runID, false)
	if err != nil {
		return coreexecution.ServiceBinding{}, fmt.Errorf("prepare service outputs: %w", err)
	}
	published, err := m.publishDocuments(ctx, spec)
	if err != nil {
		return coreexecution.ServiceBinding{}, fmt.Errorf("publish service outputs: %w", err)
	}
	binding, err := m.commit(ctx, runID, published)
	if err != nil {
		return coreexecution.ServiceBinding{}, fmt.Errorf("commit service outputs: %w", err)
	}
	return binding, nil
}

// EnsureReceipt is the terminal hook used after a receipt commit. It is a
// no-op until that receipt made an eligible service run fully successful.
func (m *ExecutionServiceOutputMaterializer) EnsureReceipt(ctx context.Context, owner, receiptID, attemptID string) error {
	if m == nil || owner != m.owner || !validUUID(receiptID) || !validUUID(attemptID) {
		return ErrExecutionStoreInvalid
	}
	var runID string
	err := m.store.db.QueryRowContext(ctx, `SELECT r.run_id::text
		FROM core_execution_receipts receipt
		JOIN core_execution_runs r ON r.owner_id=receipt.owner_id AND r.run_id=receipt.run_id
		JOIN core_execution_plan_revisions p ON p.owner_id=r.owner_id AND p.plan_id=r.plan_id AND p.revision=r.plan_revision AND p.plan_digest=r.plan_digest
		WHERE receipt.owner_id=$1 AND receipt.receipt_id=$2 AND receipt.attempt_id=$3
		  AND receipt.status='succeeded' AND r.status='succeeded' AND r.purpose='service'
		  AND r.operation IN ('execute','deploy','upgrade','repair')
		  AND p.snapshot_json @> '{"targets":[{"provider":"aws","kind":"aws_ec2_instance"}]}'::jsonb`, owner, receiptID, attemptID).Scan(&runID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = m.EnsureRun(ctx, runID)
	return err
}

// RecoverPending is the restart path for the only non-transactional gap: CAS
// files may have been published while their database transaction did not
// commit. Re-running EnsureRun reuses those exact files.
func (m *ExecutionServiceOutputMaterializer) RecoverPending(ctx context.Context) error {
	if m == nil || m.store == nil || m.artifacts == nil {
		return ErrExecutionStoreInvalid
	}
	rows, err := m.store.db.QueryContext(ctx, `SELECT r.run_id::text
		FROM core_execution_runs r
		JOIN core_execution_plan_revisions p ON p.owner_id=r.owner_id AND p.plan_id=r.plan_id AND p.revision=r.plan_revision AND p.plan_digest=r.plan_digest
		WHERE r.owner_id=$1 AND r.status='succeeded' AND r.purpose='service'
		  AND r.operation IN ('execute','deploy','upgrade','repair')
		  AND p.snapshot_json @> '{"targets":[{"provider":"aws","kind":"aws_ec2_instance"}]}'::jsonb
		  AND NOT EXISTS (SELECT 1 FROM core_execution_service_bindings b WHERE b.owner_id=r.owner_id AND b.run_id=r.run_id)
		ORDER BY r.completed_at,r.run_id LIMIT 100`, m.owner)
	if err != nil {
		return err
	}
	var runIDs []string
	for rows.Next() {
		var runID string
		if err := rows.Scan(&runID); err != nil {
			rows.Close()
			return err
		}
		runIDs = append(runIDs, runID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, runID := range runIDs {
		if _, err := m.EnsureRun(ctx, runID); err != nil {
			return err
		}
	}
	return nil
}

func (m *ExecutionServiceOutputMaterializer) loadSpec(ctx context.Context, runID string, lock bool) (serviceOutputSpec, error) {
	opts := &sql.TxOptions{ReadOnly: !lock}
	tx, err := m.store.db.BeginTx(ctx, opts)
	if err != nil {
		return serviceOutputSpec{}, err
	}
	defer tx.Rollback()
	spec, err := loadServiceOutputSpecTx(ctx, tx, m.owner, runID, lock)
	if err != nil {
		return serviceOutputSpec{}, err
	}
	if err := tx.Commit(); err != nil {
		return serviceOutputSpec{}, err
	}
	return spec, nil
}

func (m *ExecutionServiceOutputMaterializer) publishDocuments(ctx context.Context, spec serviceOutputSpec) ([]publishedServiceOutput, error) {
	documents := []serviceOutputDocument{spec.Usage, spec.Runbook}
	out := make([]publishedServiceOutput, 0, len(documents))
	for _, document := range documents {
		size := int64(len(document.Body))
		metadata, err := m.artifacts.Put(ctx, bytes.NewReader(document.Body), artifactstore.PutOptions{
			ExpectedDigest: string(document.Digest), ExpectedSize: &size, MaxSize: serviceOutputMaxSize,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, publishedServiceOutput{Document: document, Metadata: metadata})
	}
	return out, nil
}

func (m *ExecutionServiceOutputMaterializer) commit(ctx context.Context, runID string, published []publishedServiceOutput) (coreexecution.ServiceBinding, error) {
	if len(published) != 2 {
		return coreexecution.ServiceBinding{}, ErrExecutionStoreInvalid
	}
	tx, err := m.store.db.BeginTx(ctx, nil)
	if err != nil {
		return coreexecution.ServiceBinding{}, err
	}
	defer tx.Rollback()
	spec, err := loadServiceOutputSpecTx(ctx, tx, m.owner, runID, true)
	if err != nil {
		return coreexecution.ServiceBinding{}, err
	}
	expected := map[string]serviceOutputDocument{spec.Usage.Kind: spec.Usage, spec.Runbook.Kind: spec.Runbook}
	metas := make(map[string]artifactstore.Metadata, len(published))
	for _, item := range published {
		document, ok := expected[item.Document.Kind]
		if !ok || document.ArtifactID != item.Document.ArtifactID || document.Digest != item.Document.Digest || !bytes.Equal(document.Body, item.Document.Body) || item.Metadata.Digest != string(document.Digest) || item.Metadata.Size != int64(len(document.Body)) || item.Metadata.StorageRef != serviceStorageRef(document.Digest) {
			return coreexecution.ServiceBinding{}, coreexecution.ErrConflict
		}
		metas[document.Kind] = item.Metadata
	}
	if len(metas) != 2 {
		return coreexecution.ServiceBinding{}, coreexecution.ErrConflict
	}
	for _, document := range []serviceOutputDocument{spec.Usage, spec.Runbook} {
		if err := ensureServiceArtifactTx(ctx, tx, m.store.now, spec, document, metas[document.Kind]); err != nil {
			return coreexecution.ServiceBinding{}, fmt.Errorf("persist %s artifact: %w", document.Kind, err)
		}
	}
	binding, err := serviceBindingFromSpec(spec)
	if err != nil {
		return coreexecution.ServiceBinding{}, fmt.Errorf("build service binding: %w", err)
	}
	if err = ensureServiceBindingTx(ctx, tx, m.store.now, binding); err != nil {
		return coreexecution.ServiceBinding{}, fmt.Errorf("persist service binding: %w", err)
	}
	if err = ensureServiceDeploymentProjectionTx(ctx, tx, m.store.now, spec, binding); err != nil {
		return coreexecution.ServiceBinding{}, fmt.Errorf("persist service deployment: %w", err)
	}
	if err = ensureServiceOutputEventsTx(ctx, tx, m.store.now, spec, binding); err != nil {
		return coreexecution.ServiceBinding{}, fmt.Errorf("persist service output events: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return coreexecution.ServiceBinding{}, err
	}
	return binding, nil
}

func loadServiceOutputSpecTx(ctx context.Context, tx *sql.Tx, owner, runID string, lock bool) (serviceOutputSpec, error) {
	if tx == nil || strings.TrimSpace(owner) == "" || !validUUID(runID) {
		return serviceOutputSpec{}, ErrExecutionStoreInvalid
	}
	if lock {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM core_execution_runs WHERE owner_id=$1 AND run_id=$2 FOR UPDATE`, owner, runID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return serviceOutputSpec{}, coreexecution.ErrNotFound
		} else if err != nil {
			return serviceOutputSpec{}, err
		}
	}
	view, err := readExecutionRunView(ctx, tx, owner, runID)
	if err != nil {
		return serviceOutputSpec{}, fmt.Errorf("load service output run: %w", err)
	}
	run := view.Run
	if run.Validate() != nil || run.Status != coreexecution.RunSucceeded || run.Purpose != coreexecution.PurposeService || !serviceOutputOperation(run.Operation) || !validUUID(run.DeploymentID) {
		return serviceOutputSpec{}, fmt.Errorf("load service output run eligibility: %w", coreexecution.ErrConflict)
	}
	for _, stage := range view.Stages {
		// database/sql may scan timestamptz values in the session location while
		// the immutable JSON snapshot remains UTC. Normalize the read projection
		// before applying the domain validator.
		stage.CreatedAt = stage.CreatedAt.UTC()
		stage.UpdatedAt = stage.UpdatedAt.UTC()
		stage.StartedAt = stage.StartedAt.UTC()
		stage.FinishedAt = stage.FinishedAt.UTC()
		if stageErr := stage.Validate(); stageErr != nil || stage.Status != coreexecution.StageSucceeded {
			return serviceOutputSpec{}, fmt.Errorf("load service output terminal stage %s (%v): %w", stage.StageID, stageErr, ErrExecutionStoreDrift)
		}
	}
	plan, err := loadFrozenPlanSnapshotTx(ctx, tx, owner, run.PlanID, run.PlanRevision)
	if err != nil {
		return serviceOutputSpec{}, fmt.Errorf("load service output plan: %w", err)
	}
	if plan.ProjectID != run.ProjectID || plan.Purpose != coreexecution.PurposeService || plan.DeploymentID != run.DeploymentID || plan.Digest != run.PlanDigest {
		return serviceOutputSpec{}, fmt.Errorf("load service output plan scope: %w", ErrExecutionStoreDrift)
	}
	stages := make(map[string]coreexecution.RunStage, len(view.Stages))
	for _, stage := range view.Stages {
		stages[stage.StageKey] = stage
	}
	for _, stage := range plan.Stages {
		runStage, ok := stages[stage.StageKey]
		if !ok || runStage.PlanID != plan.ID || runStage.PlanRevision != plan.Revision || runStage.StageRevision != stage.Revision || runStage.StageDigest != stage.Digest || runStage.TargetID != stage.TargetID || runStage.TargetRevision != stage.TargetRevision || runStage.TargetDigest != stage.TargetDigest {
			return serviceOutputSpec{}, fmt.Errorf("load service output plan stage %s: %w", stage.StageKey, ErrExecutionStoreDrift)
		}
	}
	if lock {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM core_execution_deployments WHERE owner_id=$1 AND deployment_id=$2 AND project_id=$3 FOR UPDATE`, owner, run.DeploymentID, run.ProjectID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return serviceOutputSpec{}, ErrExecutionStoreDrift
		} else if err != nil {
			return serviceOutputSpec{}, err
		}
	}
	deployment, err := readDeployment(ctx, tx, owner, run.DeploymentID)
	if err != nil || deployment.ProjectID != run.ProjectID || deployment.RunID != run.RunID {
		return serviceOutputSpec{}, fmt.Errorf("load service output deployment: %w", ErrExecutionStoreDrift)
	}
	spec, err := deriveServiceOutputSpec(run, plan)
	if err != nil {
		return serviceOutputSpec{}, fmt.Errorf("derive service outputs: %w", err)
	}
	return spec, nil
}

func loadFrozenPlanSnapshotTx(ctx context.Context, tx *sql.Tx, owner, planID string, revision uint64) (coreexecution.PlanSnapshot, error) {
	var rowOwner, rowPlan, projectID, analysisID, digest, schema string
	var rowRevision uint64
	var raw []byte
	var expires time.Time
	if err := tx.QueryRowContext(ctx, `SELECT owner_id,plan_id::text,revision,project_id::text,analysis_id::text,plan_digest,schema_version,snapshot_json,expires_at FROM core_execution_plan_revisions WHERE owner_id=$1 AND plan_id=$2 AND revision=$3`, owner, planID, revision).Scan(&rowOwner, &rowPlan, &rowRevision, &projectID, &analysisID, &digest, &schema, &raw, &expires); errors.Is(err, sql.ErrNoRows) {
		return coreexecution.PlanSnapshot{}, coreexecution.ErrNotFound
	} else if err != nil {
		return coreexecution.PlanSnapshot{}, err
	}
	// PostgreSQL JSONB does not preserve the canonical key order emitted by the
	// snapshot encoder. Re-encode the strict decoded value before applying the
	// byte-canonical domain decoder.
	snapshot, err := decodeStoredPlan(raw)
	if err != nil || rowOwner != owner || rowPlan != planID || rowRevision != revision || schema != coreexecution.SchemaVersion || snapshot.ID != planID || snapshot.Revision != revision || snapshot.OwnerID != owner || snapshot.ProjectID != projectID || snapshot.AnalysisID != analysisID || snapshot.Digest != coreexecution.Digest(digest) || !snapshot.ExpiresAt.Equal(expires.UTC()) {
		return coreexecution.PlanSnapshot{}, ErrExecutionStoreDrift
	}
	return snapshot, nil
}

func deriveServiceOutputSpec(run coreexecution.ExecutionRun, plan coreexecution.PlanSnapshot) (serviceOutputSpec, error) {
	if run.Status != coreexecution.RunSucceeded || run.Purpose != coreexecution.PurposeService || !serviceOutputOperation(run.Operation) || plan.Purpose != coreexecution.PurposeService || run.PlanID != plan.ID || run.PlanRevision != plan.Revision || run.PlanDigest != plan.Digest || run.DeploymentID != plan.DeploymentID || len(plan.Stages) == 0 {
		return serviceOutputSpec{}, coreexecution.ErrConflict
	}
	targetByID := make(map[string]coreexecution.ExecutionTarget, len(plan.Targets))
	for _, target := range plan.Targets {
		targetByID[target.ID] = target
	}
	selectedTargetID := plan.Stages[len(plan.Stages)-1].TargetID
	selectedTargetRevision := plan.Stages[len(plan.Stages)-1].TargetRevision
	selectedTargetDigest := plan.Stages[len(plan.Stages)-1].TargetDigest
	var externalOrigin string
	var probes []serviceProbeFact
	for _, stage := range plan.Stages {
		for _, step := range stage.Steps {
			switch step.Kind {
			case coreexecution.StepHTTPProbe:
				if step.HTTPProbe == nil {
					return serviceOutputSpec{}, ErrExecutionStoreDrift
				}
				origin, err := serviceHTTPOrigin(step.HTTPProbe.URL)
				if err != nil {
					return serviceOutputSpec{}, ErrExecutionStoreDrift
				}
				fact := serviceProbeFact{Kind: "http", Mode: step.HTTPProbe.Mode, ExpectedStatus: append([]int(nil), step.HTTPProbe.ExpectedStatus...)}
				if step.HTTPProbe.Mode == "external" {
					fact.Endpoint = origin
					if externalOrigin != "" && externalOrigin != origin {
						return serviceOutputSpec{}, coreexecution.ErrConflict
					}
					if externalOrigin != "" && (selectedTargetID != stage.TargetID || selectedTargetRevision != stage.TargetRevision || selectedTargetDigest != stage.TargetDigest) {
						return serviceOutputSpec{}, coreexecution.ErrConflict
					}
					externalOrigin = origin
					selectedTargetID, selectedTargetRevision, selectedTargetDigest = stage.TargetID, stage.TargetRevision, stage.TargetDigest
				}
				probes = append(probes, fact)
			case coreexecution.StepTCPProbe:
				if step.TCPProbe == nil {
					return serviceOutputSpec{}, ErrExecutionStoreDrift
				}
				fact := serviceProbeFact{Kind: "tcp", Mode: step.TCPProbe.Mode}
				if step.TCPProbe.Mode == "external" {
					fact.Endpoint = "tcp://" + strings.ToLower(step.TCPProbe.Address) + ":" + strconv.Itoa(step.TCPProbe.Port)
				}
				probes = append(probes, fact)
			}
		}
	}
	target, ok := targetByID[selectedTargetID]
	if !ok || target.Revision != selectedTargetRevision || target.Digest != selectedTargetDigest || target.Provider != "aws" || target.Kind != "aws_ec2_instance" || !hasString(target.Capabilities, "transport.aws_ssm") {
		return serviceOutputSpec{}, coreexecution.ErrConflict
	}
	instanceID, err := serviceTargetInstanceID(target.Capabilities)
	if err != nil {
		return serviceOutputSpec{}, err
	}
	protocol, endpoint := "ssm", "ssm://"+instanceID
	if externalOrigin != "" {
		u, _ := url.Parse(externalOrigin)
		protocol, endpoint = strings.ToLower(u.Scheme), externalOrigin
	}
	spec := serviceOutputSpec{
		Run: run, Plan: plan, Target: target,
		BindingID: deterministicServiceOutputID(run.OwnerID, run.RunID, "binding"),
		ReleaseID: fmt.Sprintf("plan-%s-r%d", plan.ID, plan.Revision),
		Protocol:  protocol, Endpoint: endpoint, ProbeFacts: probes,
	}
	spec.Usage = serviceOutputDocument{ArtifactID: deterministicServiceOutputID(run.OwnerID, run.RunID, "usage"), Kind: "usage"}
	spec.Runbook = serviceOutputDocument{ArtifactID: deterministicServiceOutputID(run.OwnerID, run.RunID, "runbook"), Kind: "runbook"}
	spec.Usage.Body = buildServiceUsage(spec)
	spec.Runbook.Body = buildServiceRunbook(spec)
	if len(spec.Usage.Body) == 0 || len(spec.Runbook.Body) == 0 || int64(len(spec.Usage.Body)) > serviceOutputMaxSize || int64(len(spec.Runbook.Body)) > serviceOutputMaxSize {
		return serviceOutputSpec{}, ErrExecutionStoreInvalid
	}
	spec.Usage.Digest = coreexecution.Digest(digestBytes(spec.Usage.Body))
	spec.Runbook.Digest = coreexecution.Digest(digestBytes(spec.Runbook.Body))
	return spec, nil
}

func buildServiceUsage(spec serviceOutputSpec) []byte {
	var b strings.Builder
	b.WriteString("# Service Usage\n\n")
	b.WriteString("This document is generated from immutable execution.v2 facts. It contains no credential values or executable commands.\n\n")
	fmt.Fprintf(&b, "- Deployment ID: `%s`\n- Run ID: `%s`\n- Release: `%s`\n- Plan: `%s` revision %d (`%s`)\n- Target: `%s` revision %d (`%s`)\n\n", spec.Run.DeploymentID, spec.Run.RunID, spec.ReleaseID, spec.Plan.ID, spec.Plan.Revision, spec.Plan.Digest, spec.Target.ID, spec.Target.Revision, spec.Target.Digest)
	b.WriteString("## Endpoint\n\n")
	fmt.Fprintf(&b, "- Protocol: `%s`\n- Endpoint: `%s`\n", spec.Protocol, spec.Endpoint)
	if spec.Protocol == "ssm" {
		b.WriteString("- Direct invocation: unavailable. Management changes require a new reviewed execution.v2 plan and run.\n")
	} else {
		b.WriteString("- Direct invocation: unavailable until a schema-pinned operation is attached to the ServiceBinding.\n")
	}
	b.WriteString("\n## Health\n\n")
	writeProbeFacts(&b, spec.ProbeFacts)
	return []byte(b.String())
}

func buildServiceRunbook(spec serviceOutputSpec) []byte {
	var b strings.Builder
	b.WriteString("# Service Runbook\n\n")
	b.WriteString("This runbook is a generic control-plane record generated from the successful execution.v2 run.\n\n")
	fmt.Fprintf(&b, "- Deployment ID: `%s`\n- Release: `%s`\n- Target: `%s` revision %d\n- Management endpoint: `%s`\n\n", spec.Run.DeploymentID, spec.ReleaseID, spec.Target.ID, spec.Target.Revision, spec.Endpoint)
	b.WriteString("## Verify\n\n")
	writeProbeFacts(&b, spec.ProbeFacts)
	b.WriteString("\n## Operate\n\n")
	b.WriteString("Create a new immutable plan revision for configuration, upgrade, repair, or management work. Confirm every gated stage before dispatch. Do not use this document as a shell script.\n\n")
	b.WriteString("## Rollback\n\n")
	b.WriteString("Rollback is a separate execution.v2 run with its own immutable stage set and confirmation. It is never implied by this binding.\n")
	return []byte(b.String())
}

func writeProbeFacts(b *strings.Builder, probes []serviceProbeFact) {
	if len(probes) == 0 {
		b.WriteString("No typed health probe was declared in this plan.\n")
		return
	}
	for _, probe := range probes {
		fmt.Fprintf(b, "- `%s` probe, mode `%s`", probe.Kind, probe.Mode)
		if probe.Endpoint != "" {
			fmt.Fprintf(b, ", endpoint `%s`", probe.Endpoint)
		}
		if len(probe.ExpectedStatus) > 0 {
			statuses := append([]int(nil), probe.ExpectedStatus...)
			sort.Ints(statuses)
			fmt.Fprintf(b, ", expected status `%v`", statuses)
		}
		b.WriteString(".\n")
	}
}

func serviceBindingFromSpec(spec serviceOutputSpec) (coreexecution.ServiceBinding, error) {
	usage := coreexecution.ArtifactRef{ID: spec.Usage.ArtifactID, Digest: spec.Usage.Digest, MediaType: serviceOutputMediaType, Size: int64(len(spec.Usage.Body)), Immutable: true}
	runbook := coreexecution.ArtifactRef{ID: spec.Runbook.ArtifactID, Digest: spec.Runbook.Digest, MediaType: serviceOutputMediaType, Size: int64(len(spec.Runbook.Body)), Immutable: true}
	return prepareBinding(coreexecution.ServiceBinding{
		BindingID: spec.BindingID, OwnerID: spec.Run.OwnerID, DeploymentID: spec.Run.DeploymentID,
		ProjectID: spec.Run.ProjectID, RunID: spec.Run.RunID, TargetID: spec.Target.ID,
		TargetRevision: spec.Target.Revision, TargetDigest: spec.Target.Digest, ReleaseID: spec.ReleaseID,
		Protocol: spec.Protocol, Endpoint: spec.Endpoint, ArtifactIDs: []string{usage.ID, runbook.ID},
		UsageArtifact: usage,
	}, 1)
}

func ensureServiceArtifactTx(ctx context.Context, tx *sql.Tx, now func() time.Time, spec serviceOutputSpec, document serviceOutputDocument, metadata artifactstore.Metadata) error {
	if metadata.Digest != string(document.Digest) || metadata.Size != int64(len(document.Body)) || metadata.StorageRef != serviceStorageRef(document.Digest) {
		return coreexecution.ErrConflict
	}
	metaJSON, err := canonicalRedactedJSON(map[string]any{
		"schema_version": serviceOutputSchema, "document_kind": document.Kind,
		"run_id": spec.Run.RunID, "binding_id": spec.BindingID,
	})
	if err != nil {
		return fmt.Errorf("service artifact metadata: %w", err)
	}
	at := now().UTC().Truncate(time.Microsecond)
	if _, err = tx.ExecContext(ctx, `INSERT INTO core_execution_artifacts(owner_id,artifact_id,project_id,plan_id,plan_revision,run_id,content_digest,storage_backend,storage_ref,size_bytes,media_type,revision,status,schema_version,metadata_json,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,'filesystem',$8,$9,$10,1,'available','execution-artifact/v2',$11,$12) ON CONFLICT (owner_id,artifact_id) DO NOTHING`, spec.Run.OwnerID, document.ArtifactID, spec.Run.ProjectID, spec.Plan.ID, spec.Plan.Revision, spec.Run.RunID, document.Digest, metadata.StorageRef, metadata.Size, serviceOutputMediaType, metaJSON, at); err != nil {
		return mapExecutionConflict(err)
	}
	got, err := getArtifactTx(ctx, tx, nil, spec.Run.OwnerID, document.ArtifactID)
	if err != nil {
		return err
	}
	if got.ProjectID != spec.Run.ProjectID || got.PlanID != spec.Plan.ID || got.PlanRevision != spec.Plan.Revision || got.RunID != spec.Run.RunID || got.AttemptID != "" || got.ContentDigest != document.Digest || got.StorageRef != metadata.StorageRef || got.SizeBytes != metadata.Size || got.MediaType != serviceOutputMediaType || !jsonEqual(got.Metadata, metaJSON) {
		return coreexecution.ErrConflict
	}
	return nil
}

func ensureServiceBindingTx(ctx context.Context, tx *sql.Tx, now func() time.Time, binding coreexecution.ServiceBinding) error {
	raw, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	var existingRaw []byte
	err = tx.QueryRowContext(ctx, `SELECT snapshot_json FROM core_execution_service_bindings WHERE owner_id=$1 AND binding_id=$2 FOR UPDATE`, binding.OwnerID, binding.BindingID).Scan(&existingRaw)
	if err == nil {
		var existing coreexecution.ServiceBinding
		if strictJSON(existingRaw, &existing) != nil || validateStoredBinding(existing) != nil || !jsonEqual(existingRaw, raw) {
			return ErrExecutionStoreDrift
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err = validateServiceBindingScope(ctx, tx, binding); err != nil {
		return err
	}
	if err = validateBindingArtifacts(ctx, tx, binding); err != nil {
		return err
	}
	at := now().UTC().Truncate(time.Microsecond)
	_, err = tx.ExecContext(ctx, `INSERT INTO core_execution_service_bindings(owner_id,binding_id,deployment_id,release_id,project_id,run_id,target_id,target_revision,protocol,endpoint,binding_digest,operation_schema_digest,usage_artifact_id,revision,status,schema_version,snapshot_json,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,1,'active','execution-service-binding/v2',$14,$15,$15)`, binding.OwnerID, binding.BindingID, binding.DeploymentID, binding.ReleaseID, binding.ProjectID, binding.RunID, binding.TargetID, binding.TargetRevision, binding.Protocol, binding.Endpoint, binding.Digest, operationSchemaDigest(binding.OperationSchemas), binding.UsageArtifact.ID, raw, at)
	return mapExecutionConflict(err)
}

func ensureServiceDeploymentProjectionTx(ctx context.Context, tx *sql.Tx, now func() time.Time, spec serviceOutputSpec, binding coreexecution.ServiceBinding) error {
	// These projections are closed server-owned structs. In particular, they
	// cannot accept arbitrary metadata or credential fields, while legitimate
	// SSM endpoints may look high-entropy to the generic catalog scanner.
	objectJSON, err := json.Marshal(struct {
		SchemaVersion  string               `json:"schema_version"`
		DeploymentID   string               `json:"deployment_id"`
		ProjectID      string               `json:"project_id"`
		PlanID         string               `json:"plan_id"`
		PlanRevision   uint64               `json:"plan_revision"`
		PlanDigest     coreexecution.Digest `json:"plan_digest"`
		RunID          string               `json:"run_id"`
		ReleaseID      string               `json:"release_id"`
		TargetID       string               `json:"target_id"`
		TargetRevision uint64               `json:"target_revision"`
	}{"execution-deployment/v2", spec.Run.DeploymentID, spec.Run.ProjectID, spec.Plan.ID, spec.Plan.Revision, spec.Plan.Digest, spec.Run.RunID, spec.ReleaseID, spec.Target.ID, spec.Target.Revision})
	if err != nil {
		return fmt.Errorf("deployment object projection: %w", err)
	}
	actualJSON, err := json.Marshal(struct {
		SchemaVersion string               `json:"schema_version"`
		BindingID     string               `json:"binding_id"`
		BindingDigest coreexecution.Digest `json:"binding_digest"`
		Protocol      string               `json:"protocol"`
		Endpoint      string               `json:"endpoint"`
		ArtifactIDs   []string             `json:"artifact_ids"`
	}{"execution-deployment-actual/v2", binding.BindingID, binding.Digest, binding.Protocol, binding.Endpoint, binding.ArtifactIDs})
	if err != nil {
		return fmt.Errorf("deployment actual projection: %w", err)
	}
	var state, releaseID string
	var revision uint64
	var storedObject, storedActual []byte
	if err = tx.QueryRowContext(ctx, `SELECT state,release_id,revision,object_json,actual_json FROM core_execution_deployments WHERE owner_id=$1 AND deployment_id=$2 AND project_id=$3 FOR UPDATE`, spec.Run.OwnerID, spec.Run.DeploymentID, spec.Run.ProjectID).Scan(&state, &releaseID, &revision, &storedObject, &storedActual); err != nil {
		return err
	}
	if state == string(coreexecution.RunSucceeded) {
		if releaseID != spec.ReleaseID || !jsonEqual(storedObject, objectJSON) || !jsonEqual(storedActual, actualJSON) {
			return ErrExecutionStoreDrift
		}
		return nil
	}
	if state == string(coreexecution.RunFailed) || state == string(coreexecution.RunUncertain) || state == string(coreexecution.RunCanceled) || state == string(coreexecution.RunRejected) || state == string(coreexecution.RunExpired) {
		return coreexecution.ErrConflict
	}
	at := now().UTC().Truncate(time.Microsecond)
	result, err := tx.ExecContext(ctx, `UPDATE core_execution_deployments SET release_id=$1,state='succeeded',revision=$2,object_json=$3,actual_json=$4,updated_at=$5 WHERE owner_id=$6 AND deployment_id=$7 AND revision=$8 AND state=$9`, spec.ReleaseID, revision+1, objectJSON, actualJSON, at, spec.Run.OwnerID, spec.Run.DeploymentID, revision, state)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		if err != nil {
			return err
		}
		return coreexecution.ErrConflict
	}
	return nil
}

func ensureServiceOutputEventsTx(ctx context.Context, tx *sql.Tx, now func() time.Time, spec serviceOutputSpec, binding coreexecution.ServiceBinding) error {
	payload, err := json.Marshal(struct {
		SchemaVersion     string               `json:"schema_version"`
		BindingID         string               `json:"binding_id"`
		BindingDigest     coreexecution.Digest `json:"binding_digest"`
		UsageArtifactID   string               `json:"usage_artifact_id"`
		RunbookArtifactID string               `json:"runbook_artifact_id"`
		TargetID          string               `json:"target_id"`
		TargetRevision    uint64               `json:"target_revision"`
	}{serviceOutputSchema, binding.BindingID, binding.Digest, spec.Usage.ArtifactID, spec.Runbook.ArtifactID, spec.Target.ID, spec.Target.Revision})
	if err != nil {
		return err
	}
	payload, err = canonicalJSONBytes(payload)
	if err != nil {
		return err
	}
	if err = ensureRunOutputEventTx(ctx, tx, now, spec.Run.OwnerID, spec.Run.RunID, payload); err != nil {
		return fmt.Errorf("run output event: %w", err)
	}
	if err = ensureDeploymentOutputEventTx(ctx, tx, now, spec.Run.OwnerID, spec.Run.DeploymentID, payload); err != nil {
		return fmt.Errorf("deployment output event: %w", err)
	}
	return nil
}

func ensureRunOutputEventTx(ctx context.Context, tx *sql.Tx, now func() time.Time, owner, runID string, payload []byte) error {
	const kind, key, status = "service.outputs.materialized", "service.outputs.materialized", "succeeded"
	eventID := deterministicServiceOutputID(owner, runID, "run-event")
	digest, err := executionEventDigest(owner, runID, "", "", "", kind, key, status, payload)
	if err != nil {
		return err
	}
	var existingID string
	err = tx.QueryRowContext(ctx, `SELECT event_id::text FROM core_execution_events WHERE owner_id=$1 AND run_id=$2 AND event_key=$3`, owner, runID, key).Scan(&existingID)
	if err == nil {
		got, getErr := getExecutionEventTx(ctx, tx, owner, runID, existingID)
		if getErr != nil || existingID != eventID || got.Kind != kind || got.Status != status || got.EventDigest != digest || !jsonEqual(got.Payload, payload) {
			return ErrExecutionStoreDrift
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO core_execution_event_counters(owner_id,run_id,next_sequence) VALUES($1,$2,1) ON CONFLICT DO NOTHING`, owner, runID); err != nil {
		return err
	}
	var sequence uint64
	if err = tx.QueryRowContext(ctx, `UPDATE core_execution_event_counters SET next_sequence=next_sequence+1 WHERE owner_id=$1 AND run_id=$2 RETURNING next_sequence-1`, owner, runID).Scan(&sequence); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO core_execution_events(owner_id,run_id,event_id,sequence,kind,event_key,event_digest,status,event_json,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, owner, runID, eventID, sequence, kind, key, digest, status, payload, now().UTC().Truncate(time.Microsecond))
	return mapExecutionConflict(err)
}

func ensureDeploymentOutputEventTx(ctx context.Context, tx *sql.Tx, now func() time.Time, owner, deploymentID string, payload []byte) error {
	const kind, key, status = "service.outputs.materialized", "service.outputs.materialized", "succeeded"
	eventID := deterministicServiceOutputID(owner, deploymentID, "deployment-event")
	digest, err := coreexecution.CanonicalDigest(struct {
		OwnerID, DeploymentID, EventKey, Kind, Status string
		Payload                                       json.RawMessage
	}{owner, deploymentID, key, kind, status, payload})
	if err != nil {
		return err
	}
	var existingID string
	err = tx.QueryRowContext(ctx, `SELECT event_id::text FROM core_execution_deployment_events WHERE owner_id=$1 AND deployment_id=$2 AND event_json->>'event_key'=$3`, owner, deploymentID, key).Scan(&existingID)
	if err == nil {
		got, getErr := getDeploymentEventTx(ctx, tx, owner, deploymentID, existingID)
		if getErr != nil || existingID != eventID || got.Kind != kind || got.Status != status || got.EventDigest != digest || !jsonEqual(got.Payload, payload) {
			return ErrExecutionStoreDrift
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO core_execution_deployment_counters(owner_id,deployment_id,next_sequence) VALUES($1,$2,1) ON CONFLICT DO NOTHING`, owner, deploymentID); err != nil {
		return err
	}
	var sequence uint64
	if err = tx.QueryRowContext(ctx, `UPDATE core_execution_deployment_counters SET next_sequence=next_sequence+1 WHERE owner_id=$1 AND deployment_id=$2 RETURNING next_sequence-1`, owner, deploymentID).Scan(&sequence); err != nil {
		return err
	}
	eventJSON, _ := json.Marshal(map[string]any{"event_key": key, "kind": kind, "status": status, "payload": json.RawMessage(payload)})
	_, err = tx.ExecContext(ctx, `INSERT INTO core_execution_deployment_events(owner_id,deployment_id,event_id,sequence,event_digest,event_json,created_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, owner, deploymentID, eventID, sequence, digest, eventJSON, now().UTC().Truncate(time.Microsecond))
	return mapExecutionConflict(err)
}

func serviceOutputOperation(operation coreexecution.RunOperation) bool {
	return operation == coreexecution.RunOperationExecute || operation == coreexecution.RunOperationDeploy || operation == coreexecution.RunOperationUpgrade || operation == coreexecution.RunOperationRepair
}

func deterministicServiceOutputID(owner, identity, kind string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(owner+"\x00"+identity+"\x00"+kind)).String()
}

func serviceStorageRef(digest coreexecution.Digest) string {
	return "sha256/" + string(digest)[:2] + "/" + string(digest)
}

func serviceHTTPOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return "", coreexecution.ErrInvalid
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host), nil
}

func serviceTargetInstanceID(capabilities []string) (string, error) {
	instanceID := ""
	for _, capability := range capabilities {
		if !strings.HasPrefix(capability, coreaws.TargetInstanceCapabilityPrefix) {
			continue
		}
		candidate := strings.TrimPrefix(capability, coreaws.TargetInstanceCapabilityPrefix)
		if !coreaws.ValidEC2InstanceID(candidate) || instanceID != "" {
			return "", coreexecution.ErrConflict
		}
		instanceID = candidate
	}
	if instanceID == "" {
		return "", coreexecution.ErrConflict
	}
	return instanceID, nil
}

func hasString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
