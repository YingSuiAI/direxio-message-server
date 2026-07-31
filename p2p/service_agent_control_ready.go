package p2p

import "strings"

type nativeAgentControlRequirement struct {
	all []string
	any []string
}

func nativeAgentControlRequirements(action string) (nativeAgentControlRequirement, bool) {
	switch strings.TrimSpace(action) {
	case "agent.core.aws.credentials.list",
		"agent.core.aws.credentials.test":
		return nativeAgentControlRequirement{all: []string{"aws.control"}}, true
	case "agent.execution.v2.projects.analyze",
		"agent.execution.v2.plans.create",
		"agent.execution.v2.plans.get":
		return nativeAgentControlRequirement{all: []string{"execution.v2.plan"}}, true
	case "agent.execution.v2.targets.list",
		"agent.execution.v2.targets.get":
		return nativeAgentControlRequirement{all: []string{"execution.v2"}}, true
	case "agent.execution.v2.runs.create",
		"agent.execution.v2.runs.get",
		"agent.execution.v2.runs.events":
		return nativeAgentControlRequirement{all: []string{"execution.v2.run"}}, true
	case "agent.execution.v2.service_bindings.list",
		"agent.execution.v2.service_bindings.get":
		return nativeAgentControlRequirement{all: []string{"execution.v2.bindings"}}, true
	case "agent.execution.v2.service_bindings.invoke":
		return nativeAgentControlRequirement{all: []string{"execution.v2.bindings", "execution.v2.transport.http_api"}}, true
	default:
		return nativeAgentControlRequirement{}, false
	}
}

func (s *Service) nativeAgentControlActionReady(action string) bool {
	requirement, known := nativeAgentControlRequirements(action)
	if s == nil || !known || s.agentEmbedded == nil {
		return false
	}
	if s.agentEmbedded.Handlers()[strings.TrimSpace(action)] == nil {
		return false
	}
	for _, capability := range requirement.all {
		if !s.embeddedAgentCapabilityReady(capability) {
			return false
		}
	}
	if len(requirement.any) > 0 {
		for _, capability := range requirement.any {
			if s.embeddedAgentCapabilityReady(capability) {
				return true
			}
		}
		return false
	}
	return true
}
