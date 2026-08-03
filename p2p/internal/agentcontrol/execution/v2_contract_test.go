package execution

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestV2SnapshotsStrictCanonicalAndStatusFree(t *testing.T) {
	pl := plan()
	pl.Targets[0].InfrastructureProfileID = "general-linux-ssm-v1"
	pl.Targets[0].Digest = ""
	p, err := PlanSnapshotFromPlan(pl)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := EncodePlanSnapshot(p)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"status"`)) {
		t.Fatalf("mutable status leaked: %s", raw)
	}
	decoded, err := DecodePlanSnapshot(raw)
	if err != nil || string(mustJSON(raw)) != string(mustJSON(mustEncodePlan(decoded))) {
		t.Fatalf("roundtrip: %v", err)
	}
	if decoded.Stages[0].Steps[0].ObservationRef == nil {
		t.Fatal("script observation reference was not retained in snapshot")
	}
	missingObservation := decoded
	missingObservation.Stages[0].Steps[0].ObservationRef = nil
	if _, err := EncodePlanSnapshot(missingObservation); err == nil {
		t.Fatal("snapshot accepted script.run without observation reference")
	}
	for _, bad := range [][]byte{append(append([]byte{}, raw...), []byte(" {}")...), append(raw, []byte(`{"unknown":1}`)...)} {
		if _, err := DecodePlanSnapshot(bad); err == nil {
			t.Fatal("accepted trailing/unknown JSON")
		}
	}
	if _, err := DecodePlanSnapshot(append([]byte(" "), raw...)); err == nil {
		t.Fatal("accepted a noncanonical whitespace representation")
	}
	duplicate := append([]byte(`{"schema_version":"execution-plan-snapshot/v2",`), raw[1:]...)
	if _, err := DecodePlanSnapshot(duplicate); err == nil {
		t.Fatal("accepted a duplicate JSON member")
	}
	wrongDigest := p
	wrongDigest.Digest = Digest(sha)
	wrongDigestRaw, err := json.Marshal(wrongDigest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodePlanSnapshot(wrongDigestRaw); err == nil {
		t.Fatal("accepted an alternative envelope digest")
	}
	if _, err := DecodePlanSnapshot(append(bytes.Repeat([]byte("x"), MaxSnapshotBytes), raw...)); err == nil {
		t.Fatal("accepted oversized snapshot")
	}
}

func mustEncodePlan(s PlanSnapshot) []byte { b, _ := EncodePlanSnapshot(s); return b }
func mustJSON(b []byte) []byte {
	var v any
	_ = json.Unmarshal(b, &v)
	out, _ := json.Marshal(v)
	return out
}

func TestV2IdentityLengthDelimitedAndSelection(t *testing.T) {
	if DeterministicRunID(ownerID, "ab") == DeterministicRunID(ownerID+"a", "b") {
		t.Fatal("identity inputs were concatenated ambiguously")
	}
	if DeterministicRunID(ownerID, "idem") != DeterministicRunID(ownerID, "idem") {
		t.Fatal("run id not stable")
	}
	p := plan()
	stage := p.Stages[0]
	forward, err := SelectStageExecution(RunOperationExecute, stage)
	if err != nil || forward.StepSet != StepSetForward || len(forward.Steps) != 1 || forward.Risk != stage.Risk {
		t.Fatalf("forward selection: %#v %v", forward, err)
	}
	rollback, err := SelectStageExecution(RunOperationRollback, stage)
	if err != nil || !rollback.Skipped || rollback.Risk != RiskR4 || rollback.Gate != GateRollback {
		t.Fatalf("rollback selection: %#v %v", rollback, err)
	}
}

func TestV2ConfirmationPreviewRedactsExecutionPayload(t *testing.T) {
	p, err := plan().Normalize()
	if err != nil {
		t.Fatal(err)
	}
	r := ExecutionRun{
		RunID: DeterministicRunID(ownerID, "preview"), OwnerID: ownerID,
		Operation: RunOperationExecute, TriggerKind: TriggerManual,
		PlanID: p.ID, ProjectID: p.ProjectID, Purpose: p.Purpose,
		PlanRevision: p.Revision, PlanDigest: p.Digest, RunDigest: Digest(sha),
		Status: RunPending, Revision: 1, CreatedAt: p.CreatedAt, UpdatedAt: p.CreatedAt,
	}
	preview, err := BuildConfirmationPreview(p, r, p.Stages[0])
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(preview)
	text := string(raw)
	for _, forbidden := range []string{"argv", "env", "secret_refs", "raw_provider", "interpreter"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("preview leaked %q: %s", forbidden, text)
		}
	}
	binding, err := BuildConfirmationBinding(preview)
	if err != nil {
		t.Fatal(err)
	}
	if binding.PlanDigest != preview.PlanDigest || binding.StageDigest != preview.StageDigest || binding.TargetDigest != preview.TargetDigest {
		t.Fatal("digest pin was re-hashed")
	}
	missing := p.Stages[0]
	missing.StageKey = "missing"
	missing.Digest = ""
	if _, err := BuildConfirmationPreview(p, r, missing); err == nil {
		t.Fatal("preview accepted a stage outside the pinned plan")
	}
	driftedStage := p.Stages[0]
	driftedStage.Title = "different content"
	driftedStage.Digest = ""
	if _, err := BuildConfirmationPreview(p, r, driftedStage); err == nil {
		t.Fatal("preview accepted stage content drift")
	}
	driftedRun := r
	driftedRun.PlanRevision++
	if _, err := BuildConfirmationPreview(p, driftedRun, p.Stages[0]); err == nil {
		t.Fatal("preview accepted a run not pinned to the plan")
	}
}

func TestV2ConfirmationPreviewDisclosesAndDigestsSelectedNetworkGrants(t *testing.T) {
	p := plan()
	publicHTTPS, err := (NetworkGrant{Scheme: "https", Host: PublicHTTPSWildcardHost, Port: 443, Scope: "external"}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	p.Targets[0].Network.Mode = NetworkPolicyModeObservedHTTPSEgress
	p.Targets[0].Digest = ""
	p.Targets[0], err = p.Targets[0].Normalize()
	if err != nil {
		t.Fatal(err)
	}
	step := &p.Stages[0].Steps[0]
	step.TargetDigest = p.Targets[0].Digest
	step.NetworkGrants = []NetworkGrant{publicHTTPS}
	step.ScriptRun.NetworkGrants = []NetworkGrant{publicHTTPS}
	p.Stages[0].TargetDigest = p.Targets[0].Digest
	p.Stages[0].Digest = ""
	p.Digest = ""
	p, err = p.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	r := ExecutionRun{
		RunID: DeterministicRunID(ownerID, "network-preview"), OwnerID: ownerID,
		Operation: RunOperationExecute, TriggerKind: TriggerManual,
		PlanID: p.ID, ProjectID: p.ProjectID, Purpose: p.Purpose,
		PlanRevision: p.Revision, PlanDigest: p.Digest, RunDigest: Digest(sha),
		Status: RunPending, Revision: 1, CreatedAt: p.CreatedAt, UpdatedAt: p.CreatedAt,
	}
	preview, err := BuildConfirmationPreview(p, r, p.Stages[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.NetworkGrants) != 1 || preview.NetworkGrants[0].Host != PublicHTTPSWildcardHost || preview.NetworkGrants[0].PathPrefix != "" {
		t.Fatalf("preview hid or narrowed actual network grant: %+v", preview.NetworkGrants)
	}
	want, err := CanonicalDigest(struct {
		TargetPolicy NetworkPolicy  `json:"target_policy"`
		StepGrants   []NetworkGrant `json:"step_grants"`
	}{TargetPolicy: p.Targets[0].Network, StepGrants: []NetworkGrant{publicHTTPS}})
	if err != nil || preview.NetworkDigest != want {
		t.Fatalf("network digest=%s want=%s err=%v", preview.NetworkDigest, want, err)
	}
}
