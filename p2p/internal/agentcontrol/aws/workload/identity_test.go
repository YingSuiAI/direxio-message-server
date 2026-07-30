package coreworkload

import "testing"

func TestDestroyedEC2IdentityComparisonIgnoresEndpointOnly(t *testing.T) {
	desired := TargetIdentity{Kind: TargetAWSEC2SSM, AccountID: "123456789012", Region: "ap-east-1", InstanceID: "i-0123456789abcdef0", Endpoint: "http://192.0.2.10"}
	actual := desired
	actual.Endpoint = ""
	if !targetIdentityEqualForState(actual, desired, TargetAWSEC2SSM, "destroyed") {
		t.Fatal("destroyed EC2 identity should preserve account/region/instance binding while clearing endpoint")
	}
	actual.Endpoint = "http://198.51.100.20"
	if targetIdentityEqualForState(actual, desired, TargetAWSEC2SSM, "destroyed") {
		t.Fatal("destroyed EC2 identity must reject stale actual endpoint")
	}
	actual.Endpoint = ""
	actual.InstanceID = "i-0badcafe"
	if targetIdentityEqualForState(actual, desired, TargetAWSEC2SSM, "destroyed") {
		t.Fatal("destroyed EC2 identity must reject instance drift")
	}
}
