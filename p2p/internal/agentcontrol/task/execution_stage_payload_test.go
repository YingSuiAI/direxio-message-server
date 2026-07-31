package task

import (
	"strings"
	"testing"
	"time"
)

func validExecutionStageTaskSpec() TaskSpec {
	return TaskSpec{
		Kind: TaskKindExecutionStage,
		Payload: TaskPayload{ExecutionStage: &ExecutionStageTaskPayload{
			PlanID:         "00000000-0000-4000-8000-000000000001",
			PlanRevision:   2,
			PlanDigest:     strings.Repeat("1", 64),
			DeploymentID:   "00000000-0000-4000-8000-000000000002",
			RunID:          "00000000-0000-4000-8000-000000000003",
			RunRevision:    3,
			StageID:        "00000000-0000-4000-8000-000000000004",
			StageRevision:  1,
			StageDigest:    strings.Repeat("2", 64),
			TargetID:       "00000000-0000-4000-8000-000000000005",
			TargetRevision: 4,
			TargetDigest:   strings.Repeat("3", 64),
			ConfirmationID: "00000000-0000-4000-8000-000000000006",
		}},
		Goal:           "execute the frozen stage",
		IdempotencyKey: "00000000-0000-4000-8000-000000000007",
	}
}

func TestExecutionStageTaskPayloadNormalizesImmutableFence(t *testing.T) {
	spec := validExecutionStageTaskSpec()
	spec.Payload.ExecutionStage.PlanID = " " + spec.Payload.ExecutionStage.PlanID + " "
	spec.Payload.ExecutionStage.ConfirmationID = ""

	normalized, err := spec.Normalize()
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	payload := normalized.Payload.ExecutionStage
	if payload.PlanID != "00000000-0000-4000-8000-000000000001" {
		t.Fatalf("PlanID = %q", payload.PlanID)
	}
	if payload.ConfirmationID != "" {
		t.Fatalf("automatic stage gained confirmation %q", payload.ConfirmationID)
	}
}

func TestExecutionStageTaskPayloadRejectsInvalidFence(t *testing.T) {
	tests := map[string]func(*ExecutionStageTaskPayload){
		"plan id": func(p *ExecutionStageTaskPayload) { p.PlanID = "plan" },
		"plan revision": func(p *ExecutionStageTaskPayload) {
			p.PlanRevision = 0
		},
		"stage digest": func(p *ExecutionStageTaskPayload) {
			p.StageDigest = strings.Repeat("z", 64)
		},
		"run revision": func(p *ExecutionStageTaskPayload) { p.RunRevision = 0 },
		"target revision": func(p *ExecutionStageTaskPayload) {
			p.TargetRevision = 0
		},
		"unrepresentable revision": func(p *ExecutionStageTaskPayload) {
			p.PlanRevision = MaxPersistentRevision + 1
		},
		"confirmation id": func(p *ExecutionStageTaskPayload) {
			p.ConfirmationID = "confirmation"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			spec := validExecutionStageTaskSpec()
			mutate(spec.Payload.ExecutionStage)
			if _, err := spec.Normalize(); err == nil {
				t.Fatal("Normalize() accepted an invalid execution stage fence")
			}
		})
	}
}

func TestExecutionStageTaskRejectsGenericCancellation(t *testing.T) {
	taskID := "00000000-0000-4000-8000-000000000009"
	value := Task{
		ID:       taskID,
		Spec:     validExecutionStageTaskSpec(),
		Status:   StatusQueued,
		Revision: 1,
	}
	command := CancelCommand{
		TaskID:           taskID,
		ExpectedRevision: 1,
		Mutation: MutationCommand{
			IdempotencyKey:   "00000000-0000-4000-8000-000000000010",
			RequestDigest:    strings.Repeat("4", 64),
			ExpectedRevision: 1,
		},
		At: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	if err := Cancel(&value, command); err != ErrConflict {
		t.Fatalf("Cancel() error = %v, want ErrConflict", err)
	}
	if value.Status != StatusQueued || value.Revision != 1 {
		t.Fatalf("generic cancel mutated execution stage task: %#v", value)
	}
}

func TestExecutionStageTaskRejectsEveryGenericTerminalizer(t *testing.T) {
	value := Task{ID: "00000000-0000-4000-8000-000000000009", Spec: validExecutionStageTaskSpec(), Status: StatusRunning, Revision: 2, Attempt: 1, LeaseEpoch: 1, Lease: &Lease{TaskID: "00000000-0000-4000-8000-000000000009", Attempt: 1, Epoch: 1, Holder: "worker", ExpiresAt: time.Date(2030, 1, 2, 3, 5, 5, 0, time.UTC)}}
	fence := Fence{TaskID: value.ID, Attempt: 1, LeaseEpoch: 1, ExpectedRevision: 2}
	at := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	for name, terminalize := range map[string]func(*Task) error{
		"complete": func(v *Task) error {
			return Complete(v, CompleteCommand{Fence: fence, Result: Result{Summary: "done"}, At: at})
		},
		"fail": func(v *Task) error {
			return Fail(v, FailCommand{Fence: fence, ErrorCode: "failed", ErrorSummary: "failed", At: at})
		},
		"timeout": func(v *Task) error { return Timeout(v, TimeoutCommand{Fence: fence, At: at}) },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := value
			if err := terminalize(&candidate); err != ErrConflict {
				t.Fatalf("%s error = %v, want ErrConflict", name, err)
			}
			if candidate.Status != StatusRunning || candidate.Revision != value.Revision {
				t.Fatalf("%s mutated execution stage: %#v", name, candidate)
			}
		})
	}
}

func TestExecutionStageTaskPayloadRemainsClosedUnion(t *testing.T) {
	spec := validExecutionStageTaskSpec()
	spec.Payload.Extension = &ExtensionTaskPayload{}
	if _, err := spec.Normalize(); err == nil {
		t.Fatal("Normalize() accepted two payload branches")
	}
}

func TestExecutionStageTaskMutationDigestBindsTargetRevision(t *testing.T) {
	first := validExecutionStageTaskSpec()
	firstDigest, err := first.MutationDigest()
	if err != nil {
		t.Fatalf("first MutationDigest() error = %v", err)
	}
	second := validExecutionStageTaskSpec()
	second.Payload.ExecutionStage.TargetRevision++
	secondDigest, err := second.MutationDigest()
	if err != nil {
		t.Fatalf("second MutationDigest() error = %v", err)
	}
	if firstDigest == secondDigest {
		t.Fatal("target revision did not change the task mutation digest")
	}
}
