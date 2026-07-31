package storage

import (
	"fmt"

	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
)

// ErrExecutionExecutorUnavailable is returned before a run graph is
// materialized when its immutable plan requires an executor that is not part
// of the live runtime. It remains a conflict for the public action contract:
// changing runtime wiring must never create a partial run as a side effect.
var ErrExecutionExecutorUnavailable = fmt.Errorf("execution executor unavailable: %w", coreexecution.ErrConflict)

// ExecutionExecutorAvailability is the first production executor catalog.
// It is deliberately closed: provider-native EC2 provisioning, the durable
// Parameter Store secret gate, and sealed AWS SSM steps are the only
// executable forms in this release.
type ExecutionExecutorAvailability struct {
	AWSSSM           bool
	ComputeProvision bool
	SecretProvision  bool
}

func (a ExecutionExecutorAvailability) Supports(step coreexecution.ExecutionStep) bool {
	if step.Kind == coreexecution.StepComputeProvision {
		return a.ComputeProvision && step.ComputeProvision != nil
	}
	if step.Kind == coreexecution.StepSecretProvision {
		return a.SecretProvision && step.SecretProvision != nil
	}
	return a.AWSSSM && coreaws.IsExecutableSSMStep(step)
}

func (a ExecutionExecutorAvailability) validatePlan(plan coreexecution.ExecutionPlan, operation coreexecution.RunOperation) error {
	for _, stage := range plan.Stages {
		steps := stage.Steps
		if operation == coreexecution.RunOperationRollback {
			steps = stage.RollbackSteps
		}
		for _, step := range steps {
			if !a.Supports(step) {
				return ErrExecutionExecutorUnavailable
			}
		}
	}
	return nil
}
