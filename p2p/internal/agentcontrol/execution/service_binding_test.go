package execution

import "testing"

func TestServiceBindingSSMIsManagementOnly(t *testing.T) {
	binding := ServiceBinding{
		BindingID:      "11111111-1111-4111-8111-111111111111",
		OwnerID:        "@owner:example.test",
		DeploymentID:   "22222222-2222-4222-8222-222222222222",
		ProjectID:      "33333333-3333-4333-8333-333333333333",
		RunID:          "44444444-4444-4444-8444-444444444444",
		TargetID:       "55555555-5555-4555-8555-555555555555",
		TargetRevision: 1,
		TargetDigest:   Digest("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"),
		Protocol:       "ssm",
		Endpoint:       "ssm://i-0123456789abcdef0",
		Revision:       1,
	}
	if _, err := binding.Normalize(); err != nil {
		t.Fatalf("normalize management binding: %v", err)
	}

	for name, mutate := range map[string]func(*ServiceBinding){
		"path":  func(v *ServiceBinding) { v.Endpoint += "/shell" },
		"query": func(v *ServiceBinding) { v.Endpoint += "?command=run" },
		"operation": func(v *ServiceBinding) {
			v.OperationSchemas = []OperationSchema{{Name: "invoke", Version: "1", Digest: Digest("abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd")}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := binding
			mutate(&candidate)
			if _, err := candidate.Normalize(); err == nil {
				t.Fatal("SSM binding became directly invokable")
			}
		})
	}
}
