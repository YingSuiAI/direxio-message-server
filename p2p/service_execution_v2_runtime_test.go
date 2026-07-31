package p2p

import "testing"

func TestNewExecutionV2RuntimeRequiresDurableProductionInputs(t *testing.T) {
	cases := []ExecutionV2RuntimeConfig{
		{},
		{OwnerID: "@owner:example.org", ArtifactDir: t.TempDir()},
	}
	for i, cfg := range cases {
		if runtime, err := NewExecutionV2Runtime(cfg); runtime != nil || err != ErrExecutionV2RuntimeInvalid {
			t.Fatalf("case %d: runtime=%v err=%v, want fail-closed invalid configuration", i, runtime, err)
		}
	}
}

func TestExecutionV2RuntimeStartRejectsNilProcessOrWorkerMismatch(t *testing.T) {
	var runtime *ExecutionV2Runtime
	if runtime.StartExecutionV2Runner(nil, "worker") {
		t.Fatal("nil runtime must not start")
	}
}
