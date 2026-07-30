package aws

import "testing"

func TestRequiredStackOutputsAcceptsCanonicalAndLegacyMarkers(t *testing.T) {
	for _, marker := range []string{
		"InstanceId+PublicIp+SecurityGroupId+StackId",
		"InstanceId,PublicIp,SecurityGroupId,StackId",
	} {
		plan := Plan{Tags: map[string]string{RequiredOutputsTag: marker}}
		got, ok := requiredStackOutputs(plan)
		if !ok || len(got) != 4 || got[0] != string(StackOutputInstanceID) || got[3] != string(StackOutputStackID) {
			t.Fatalf("marker %q parsed as %#v, valid=%v", marker, got, ok)
		}
	}
}

func TestRequiredStackOutputsRejectsMalformedMarkers(t *testing.T) {
	for _, marker := range []string{
		"",
		"InstanceId++PublicIp",
		"InstanceId,PublicIp+SecurityGroupId",
		"InstanceId+InstanceId",
		"Unknown+PublicIp",
		"InstanceId,",
	} {
		plan := Plan{Tags: map[string]string{RequiredOutputsTag: marker}}
		if _, ok := requiredStackOutputs(plan); ok {
			t.Fatalf("malformed marker %q accepted", marker)
		}
	}
}

func TestCanonicalProviderTagsOnlyChangesLegacyMarker(t *testing.T) {
	legacy := map[string]string{RequiredOutputsTag: "InstanceId,PublicIp", "owner": "sha256:abc"}
	canonical := canonicalProviderTags(legacy)
	if canonical[RequiredOutputsTag] != "InstanceId+PublicIp" || legacy[RequiredOutputsTag] != "InstanceId,PublicIp" {
		t.Fatalf("provider tag conversion mutated raw tags: raw=%v provider=%v", legacy, canonical)
	}
}

func TestCloudFormationTagTextAllowsAWSUnicodeSet(t *testing.T) {
	if _, err := tagsToSDK(map[string]string{"中文 key_1@": "值 + / = : @"}); err != nil {
		t.Fatalf("AWS-compatible Unicode tag rejected: %v", err)
	}
}

func TestCloudFormationTagTextRejectsUnsupportedCharacters(t *testing.T) {
	for name, tags := range map[string]map[string]string{
		"comma key":   {"bad,key": "value"},
		"comma value": {"key": "bad,value"},
		"control":     {"key": "line\nfeed"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := tagsToSDK(tags); err != ErrInvalid {
				t.Fatalf("unsupported tag err=%v", err)
			}
		})
	}
}

func TestFakeProviderRejectsTypedPlanWithoutRequiredOutputsBeforeCall(t *testing.T) {
	f := NewFakeProvider()
	_, err := f.CreateChangeSet(nil, CredentialHandle{Region: "us-east-1"}, ChangeSetRequest{
		Region: "us-east-1", StackName: "typed-stack", ChangeSetName: "safe-change", ClientToken: "11111111-1111-4111-8111-111111111116",
		Operation: OperationCreate, Template: []byte(`{"Resources":{}}`), Tags: map[string]string{
			"service": EC2ServiceProfile, "dirextalk:template-profile": EC2ServiceProfile, "dirextalk:template-version": ec2TemplateVersion,
		},
	})
	if err != ErrInvalid || len(f.Calls) != 0 {
		t.Fatalf("typed marker omission err=%v calls=%v", err, f.Calls)
	}
}

func TestFakeProviderRejectsInvalidOrdinaryTagBeforeCall(t *testing.T) {
	f := NewFakeProvider()
	_, err := f.CreateChangeSet(nil, CredentialHandle{Region: "us-east-1"}, ChangeSetRequest{
		Region: "us-east-1", StackName: "ordinary-stack", ChangeSetName: "safe-change", ClientToken: "11111111-1111-4111-8111-111111111117",
		Operation: OperationCreate, Template: []byte(`{"Resources":{}}`), Tags: map[string]string{"bad?key": "value"},
	})
	if err != ErrInvalid || len(f.Calls) != 0 || len(f.Changes) != 0 {
		t.Fatalf("invalid ordinary tag err=%v calls=%v changes=%v", err, f.Calls, f.Changes)
	}
}
