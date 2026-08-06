package agentgateway

import (
	"testing"
	"time"

	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
	"github.com/google/uuid"
)

func TestCreateCallContextUsesBoundedTwoMinuteBudget(t *testing.T) {
	fixedNow := time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)
	rootOperationID := uuid.NewString()
	client := &Client{now: func() time.Time { return fixedNow }}

	callCtx := client.createCallContext(rootOperationID)

	if got, want := callCtx.GetDeadlineUnixMs(), fixedNow.Add(2*time.Minute).UnixMilli(); got != want {
		t.Fatalf("call deadline = %d, want %d", got, want)
	}
	if got := callCtx.GetRootOperationId(); got != rootOperationID {
		t.Fatalf("root operation id = %q, want %q", got, rootOperationID)
	}
	if got := callCtx.GetRoute(); got != capv1.NodeMessage {
		t.Fatalf("call route = %q, want %q", got, capv1.NodeMessage)
	}
	if got := callCtx.GetHop(); got != 1 {
		t.Fatalf("call hop = %d, want 1", got)
	}
	if err := capv1.ValidateStrictCallContext(callCtx); err != nil {
		t.Fatalf("created call context is invalid: %v", err)
	}
}
