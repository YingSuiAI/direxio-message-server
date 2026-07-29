package fakes

import (
	"context"
	workaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload/aws"
	"testing"
)

func TestResolversKeepCredentialAndSecretLookupsSeparate(t *testing.T) {
	r := &Resolvers{Credentials: map[string]workaws.CredentialHandle{"cred": {ReferenceID: "cred", Revision: 1, Region: "us-east-1", AccountID: "123456789012", AccessKeyID: "key", SecretAccessKey: "secret"}}, Secrets: map[string]string{"app": "arn:aws:ssm:us-east-1:123456789012:parameter/app"}}
	if _, e := r.ResolveCredential(context.Background(), "app", 1); e == nil {
		t.Fatal("secret reference resolved as credential")
	}
	if got, e := r.ResolveSecretReference(context.Background(), "app"); e != nil || got == "" {
		t.Fatalf("secret lookup failed: %v", e)
	}
}
