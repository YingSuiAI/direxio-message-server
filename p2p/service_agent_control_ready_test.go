package p2p

import (
	"reflect"
	"testing"
)

func TestNativeAgentControlRequirementsKeepDangerousToolsCapabilityFenced(t *testing.T) {
	tests := []struct {
		action string
		all    []string
		any    []string
	}{
		{"agent.core.aws.ec2_provisions.create.request", []string{"aws.control"}, nil},
		{"agent.core.aws.ec2_provisions.destroy.request", []string{"aws.control"}, nil},
		{"agent.core.aws.ec2_provisions.geolibre_install.plan", []string{"aws.control", "workload.aws_ssm"}, nil},
		{"agent.core.aws.ec2_provisions.geolibre_install.request", []string{"aws.control", "workload.aws_ssm"}, nil},
		{"agent.core.workloads.actual.get", nil, []string{"workload.aws_ssm", "workload.aws_ecs"}},
		{"agent.core.deployments.events", []string{"deployments.server"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.action, func(t *testing.T) {
			got, ok := nativeAgentControlRequirements(tt.action)
			if !ok || !reflect.DeepEqual(got.all, tt.all) || !reflect.DeepEqual(got.any, tt.any) {
				t.Fatalf("requirements = %#v, %t; want all=%v any=%v", got, ok, tt.all, tt.any)
			}
		})
	}
	for _, action := range []string{
		"agent.core.aws.ec2_provisions.retry",
		"agent.core.confirmations.confirm",
		"agent.core.workloads.apply",
		"agent.core.skills.execute",
	} {
		if _, ok := nativeAgentControlRequirements(action); ok {
			t.Fatalf("unsafe or unsupported Native control action %q became available", action)
		}
	}
}
