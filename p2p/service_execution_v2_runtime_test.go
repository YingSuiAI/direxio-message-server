package p2p

import (
	"path/filepath"
	"testing"
)

func TestAgentRuntimePathsDefaultToMessageDataVolume(t *testing.T) {
	t.Setenv("P2P_NATIVE_AGENT_DATA_DIR", "")
	t.Setenv("P2P_AGENT_ARTIFACT_DIR", "")

	dataDir := nativeAgentDataDir("")
	if dataDir != defaultNativeAgentDataDir {
		t.Fatalf("data dir = %q, want %q", dataDir, defaultNativeAgentDataDir)
	}
	if got, want := agentArtifactDir("", dataDir), filepath.Join(defaultNativeAgentDataDir, "artifacts"); got != want {
		t.Fatalf("artifact dir = %q, want %q", got, want)
	}
}

func TestAgentRuntimePathsHonorExplicitConfiguration(t *testing.T) {
	t.Setenv("P2P_NATIVE_AGENT_DATA_DIR", "/env/agent")
	t.Setenv("P2P_AGENT_ARTIFACT_DIR", "/env/artifacts")

	if got := nativeAgentDataDir("/config/agent"); got != "/config/agent" {
		t.Fatalf("configured data dir = %q", got)
	}
	if got := agentArtifactDir("/config/artifacts", "/config/agent"); got != "/config/artifacts" {
		t.Fatalf("configured artifact dir = %q", got)
	}
	if got := nativeAgentDataDir(""); got != "/env/agent" {
		t.Fatalf("environment data dir = %q", got)
	}
	if got := agentArtifactDir("", "/env/agent"); got != "/env/artifacts" {
		t.Fatalf("environment artifact dir = %q", got)
	}
}

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
