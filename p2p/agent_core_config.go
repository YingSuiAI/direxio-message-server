package p2p

import agentcoremodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcore"

// AgentCoreConfig is the public deployment-config shape used by p2p.Config;
// the client implementation remains internal.
type AgentCoreConfig = agentcoremodule.Config

// AgentCoreConfigFromEnv exposes deployment parsing to the monolith wiring
// without leaking the internal gRPC client or protected values.
func AgentCoreConfigFromEnv() (AgentCoreConfig, error) {
	return agentcoremodule.ConfigFromEnv()
}
