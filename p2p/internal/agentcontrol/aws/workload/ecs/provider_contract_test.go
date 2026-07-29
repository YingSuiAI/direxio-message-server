package ecs

import (
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload"
	"github.com/aws/aws-sdk-go-v2/aws"
	awstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"testing"
)

func TestOwnershipRequiresInstanceWorkloadAndPlan(t *testing.T) {
	plan := coreworkload.Plan{Digest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	tags := []awstypes.Tag{{Key: aws.String("dirextalk-agent-instance"), Value: aws.String("agent-a")}, {Key: aws.String("dirextalk-agent-workload"), Value: aws.String("w-1")}, {Key: aws.String("dirextalk-agent-plan"), Value: aws.String(plan.Digest)}}
	if !owned(tags, plan, "w-1", "agent-a") {
		t.Fatal("owned service rejected")
	}
	if owned(tags, plan, "w-2", "agent-a") {
		t.Fatal("cross-workload service accepted")
	}
	if owned(tags, plan, "w-1", "agent-b") {
		t.Fatal("cross-instance service accepted")
	}
}
