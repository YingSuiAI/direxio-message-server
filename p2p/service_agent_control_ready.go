package p2p

import "strings"

type nativeAgentControlRequirement struct {
	all []string
	any []string
}

func nativeAgentControlRequirements(action string) (nativeAgentControlRequirement, bool) {
	switch strings.TrimSpace(action) {
	case "agent.core.aws.credentials.list",
		"agent.core.aws.credentials.test",
		"agent.core.aws.ec2_provisions.plan",
		"agent.core.aws.ec2_provisions.get",
		"agent.core.aws.ec2_provisions.list",
		"agent.core.aws.ec2_provisions.events",
		"agent.core.aws.ec2_provisions.create.request",
		"agent.core.aws.ec2_provisions.destroy.request":
		return nativeAgentControlRequirement{all: []string{"aws.control"}}, true
	case "agent.core.aws.ec2_provisions.geolibre_install.plan",
		"agent.core.aws.ec2_provisions.geolibre_install.request":
		return nativeAgentControlRequirement{all: []string{"aws.control", "workload.aws_ssm"}}, true
	case "agent.core.workloads.operations.get",
		"agent.core.workloads.operations.events",
		"agent.core.workloads.operations.reconcile",
		"agent.core.workloads.actual.get":
		return nativeAgentControlRequirement{any: []string{"workload.aws_ssm", "workload.aws_ecs"}}, true
	case "agent.core.deployments.list",
		"agent.core.deployments.get",
		"agent.core.deployments.events":
		return nativeAgentControlRequirement{all: []string{"deployments.server"}}, true
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
