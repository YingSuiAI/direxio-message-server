package p2p

import (
	schedulesmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agent/schedules"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/nativeagent"
)

// RestrictedScheduledRunnerFactory is the production wiring hook. It creates
// a runner only after the service-owned compiled MCP adapters exist.
func RestrictedScheduledRunnerFactory(tools []nativeagent.Tool) (schedulesmodule.ScheduledRunner, error) {
	return nativeagent.NewScheduledRunner(tools)
}
