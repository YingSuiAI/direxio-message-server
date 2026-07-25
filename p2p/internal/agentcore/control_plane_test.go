package agentcore

import (
	"context"
	"strings"
	"testing"

	agentv1 "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcorev1"
)

func TestControlPlaneCapabilityGateHidesAbsentOptionalToken(t *testing.T) {
	client, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	result, actionErr := client.taskGet(context.Background(), map[string]any{"task_id": "task-1"})
	if result != nil || actionErr == nil || actionErr.Code != "agent_core_capability_unavailable" {
		t.Fatalf("task gate = %#v, %#v; want capability precondition", result, actionErr)
	}
}

func TestControlPlaneProjectionsNormalizeEnumsAndOmitSensitiveFields(t *testing.T) {
	task := taskMap(&agentv1.CoreTask{TaskId: "task-1", Status: agentv1.CoreTaskStatus_CORE_TASK_STATUS_WAITING_USER, Kind: agentv1.CoreTaskKind_CORE_TASK_KIND_EXTENSION, FailureSummary: "  safe\nsummary ", Revision: 3})
	if task["status"] != "waiting-user" || task["kind"] != "extension" || task["failure_summary"] != "safe summary" {
		t.Fatalf("task projection = %#v", task)
	}
	installation := installationMap(&agentv1.CoreInstallation{InstallationId: "install-1", Versions: []*agentv1.CoreExtensionVersion{{VersionId: "v1", Execution: &agentv1.CoreExecution{Descriptor_: &agentv1.CoreExecution_Stdio{Stdio: &agentv1.CoreStaticEntry{RelativePath: "/private/runtime", Argv: []string{"secret-command"}}}}}}})
	serialized := marshalSafe(installation)
	if strings.Contains(serialized, "private/runtime") || strings.Contains(serialized, "secret-command") {
		t.Fatalf("installation projection leaked execution material: %s", serialized)
	}
}
