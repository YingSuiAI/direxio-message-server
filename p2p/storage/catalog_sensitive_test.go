package storage

import (
	"encoding/json"
	"strings"
	"testing"

	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
)

func TestValidateCatalogSensitiveDataFailsClosedRecursively(t *testing.T) {
	for name, value := range map[string]any{
		"sensitive key":  map[string]any{"meta": []any{map[string]any{"access_token": "opaque"}}},
		"bearer":         map[string]any{"message": "Bearer opaque-token"},
		"basic":          map[string]any{"message": "Basic dXNlcjpwYXNz"},
		"aws access key": map[string]any{"message": "AKIAIOSFODNN7EXAMPLE"},
		"high entropy":   map[string]any{"value": "mF8wK2xQ9vR4sT7yU1nB6cD3eG5hJ0pL"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateCatalogSensitiveData(value); err == nil {
				t.Fatal("sensitive catalog value accepted")
			}
		})
	}
}

func TestValidateCatalogSensitiveDataAllowsUUIDAndSHADigests(t *testing.T) {
	value := map[string]any{
		"target_id": "35353535-3535-4535-8535-353535353535",
		"digest":    strings.Repeat("a", 64),
		"nested":    []any{strings.Repeat("b", 40), strings.Repeat("c", 96)},
	}
	if err := validateCatalogSensitiveData(value); err != nil {
		t.Fatalf("ordinary identifiers rejected: %v", err)
	}
}

func TestSafeServiceBindingInvokeOutputFailsClosedAndPreservesSafeReferences(t *testing.T) {
	for name, value := range map[string]any{
		"mixed case token key": map[string]any{"nested": []any{map[string]any{"Access-Token": "never-return"}}},
		"cookie key":           map[string]any{"headers": map[string]any{"Set.Cookie": "session=never-return"}},
		"headers":              map[string]any{"response_headers": map[string]any{"x-safe": "still-not-returned"}},
		"bearer marker":        map[string]any{"nested": []any{"Bearer never-return"}},
		"basic marker":         map[string]any{"nested": []any{"Basic dXNlcjpuZXZlci1yZXR1cm4="}},
		"private key":          map[string]any{"private.key": "never-return"},
		"private key marker":   map[string]any{"detail": "-----BEGIN PRIVATE KEY-----\\nnever-return"},
		"aws credential":       map[string]any{"detail": "AWS_SECRET_ACCESS_KEY=never-return"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := SafeServiceBindingInvokeOutput(value); err == nil {
				t.Fatal("unsafe invoke output accepted")
			}
		})
	}

	value := map[string]any{
		"accepted": true,
		"secret_ref": map[string]any{
			"ref": "credential-service-auth", "purpose": "service_auth", "revision": 3,
			"binding_digest": strings.Repeat("a", 64),
		},
		"secret_refs": []any{"credential-logs"},
		"purposes":    []any{"service_auth", "audit"},
	}
	safe, err := SafeServiceBindingInvokeOutput(value)
	if err != nil {
		t.Fatalf("safe invocation output rejected: %v", err)
	}
	if safe["secret_ref"] == nil || safe["secret_refs"] == nil || safe["purposes"] == nil {
		t.Fatalf("safe reference metadata removed: %#v", safe)
	}
}

func TestValidateCatalogSensitiveDataAllowsServerOwnedHTTPSEgressFactOnly(t *testing.T) {
	if err := validateCatalogSensitiveData(map[string]any{
		"https_egress": "security_group_public_tcp_443",
	}); err != nil {
		t.Fatalf("server-owned observation enum rejected: %v", err)
	}
	for name, value := range map[string]string{
		"adjacent":   "security_group_public_tcp_443_arbitrary_extension",
		"case drift": "SECURITY_GROUP_PUBLIC_TCP_443",
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateCatalogSensitiveData(map[string]any{"https_egress": value}); err == nil {
				t.Fatal("non-exact high-entropy observation value was accepted")
			}
		})
	}
}

func TestValidateCatalogSensitiveDataAllowsOnlyTypedCredentialMetadata(t *testing.T) {
	safe := map[string]any{
		"credential_id":         "35353535-3535-4535-8535-353535353535",
		"credential_revision":   3,
		"credential_account_id": "123456789012",
		"credential_region":     "us-east-1",
		"credential_user_arn":   "arn:aws:iam::123456789012:role/execution-v2",
		"credential_refs": []any{map[string]any{
			"ref": "35353535-3535-4535-8535-353535353535", "purpose": "aws",
			"revision": 3, "binding_digest": strings.Repeat("d", 64),
		}},
		"secret_refs": []any{map[string]any{
			"ref": "45454545-4545-4545-8545-454545454545", "purpose": "registry",
			"revision": 1, "binding_digest": strings.Repeat("e", 64),
		}},
	}
	if err := validateCatalogSensitiveData(safe); err != nil {
		t.Fatalf("typed credential reference metadata rejected: %v", err)
	}

	for name, value := range map[string]any{
		"lookalike key":      map[string]any{"credential_blob": "short-value"},
		"invalid id":         map[string]any{"credential_id": "not-a-uuid"},
		"invalid revision":   map[string]any{"credential_revision": 0},
		"invalid account":    map[string]any{"credential_account_id": "1234"},
		"invalid region":     map[string]any{"credential_region": "local"},
		"invalid principal":  map[string]any{"credential_user_arn": "not-an-arn"},
		"raw nested secret":  map[string]any{"secret_refs": []any{map[string]any{"authorization": "Bearer never-persist"}}},
		"raw credential key": map[string]any{"credential": "never-persist"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateCatalogSensitiveData(value); err == nil {
				t.Fatal("unsafe credential metadata accepted")
			}
		})
	}
}

func TestValidateCatalogSensitiveDataAllowsOnlyClosedExecutionIdentityMetadata(t *testing.T) {
	safe := map[string]any{
		"owner_id": "@execution-service-loop:example.org",
		"fence":    "35353535-3535-4535-8535-353535353535:7:ensure-package",
		"capabilities": []any{
			"target.aws_ec2_instance",
			"target.instance.i-0123456789abcdef0",
			"transport.aws_ssm",
		},
	}
	if err := validateCatalogSensitiveData(safe); err != nil {
		t.Fatalf("closed execution identity metadata rejected: %v", err)
	}

	for name, value := range map[string]any{
		"owner lookalike": map[string]any{"owner_id": "opaque-high-entropy-identity"},
		"fence lookalike": map[string]any{"fence": "35353535-3535-4535-8535-353535353535:0:ensure-package"},
		"capability":      map[string]any{"capabilities": []any{"target.external.high-entropy-value"}},
		"instance case":   map[string]any{"capabilities": []any{"target.instance.i-0123456789ABCDEF0"}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateCatalogSensitiveData(value); err == nil {
				t.Fatal("non-canonical execution identity metadata accepted")
			}
		})
	}
}

func TestValidateCatalogSensitiveDataAllowsOnlyPinnedAMIParameter(t *testing.T) {
	if err := validateCatalogSensitiveData(map[string]any{
		"ami_parameter": coreexecution.AWSAL2023X8664AMIParameter,
	}); err != nil {
		t.Fatalf("pinned server-owned AMI parameter rejected: %v", err)
	}
	if err := validateCatalogSensitiveData(map[string]any{
		"ami_parameter": coreexecution.AWSAL2023X8664AMIParameter + "-caller-controlled",
	}); err == nil {
		t.Fatal("caller-controlled AMI parameter accepted")
	}
}

func TestValidateCatalogSensitiveDataAllowsOnlyManagedCloudFormationStackARN(t *testing.T) {
	if err := validateCatalogSensitiveData(map[string]any{
		"stack_name": "dirextalk-v2-0123456789abcdef01234567",
		"stack_id":   "arn:aws:cloudformation:us-east-1:123456789012:stack/dirextalk-v2-0123456789abcdef01234567/35353535-3535-4535-8535-353535353535",
	}); err != nil {
		t.Fatalf("managed CloudFormation stack ARN rejected: %v", err)
	}
	for name, value := range map[string]string{
		"wrong service": "arn:aws:ssm:us-east-1:123456789012:stack/dirextalk-v2-0123456789abcdef01234567/35353535-3535-4535-8535-353535353535",
		"wrong stack":   "arn:aws:cloudformation:us-east-1:123456789012:stack/caller-stack/35353535-3535-4535-8535-353535353535",
		"wrong suffix":  "arn:aws:cloudformation:us-east-1:123456789012:stack/dirextalk-v2-0123456789abcdef01234567/not-a-uuid",
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateCatalogSensitiveData(map[string]any{"stack_id": value}); err == nil {
				t.Fatal("unmanaged CloudFormation stack ARN accepted")
			}
		})
	}
}

func TestValidateCatalogSensitiveDataAllowsTypedTimestampsButNotOpaqueLookalikes(t *testing.T) {
	if err := validateCatalogSensitiveData(map[string]any{
		"expires_at":  "2030-01-02T03:04:05.123456Z",
		"observed_at": "2030-01-02T11:04:05+08:00",
	}); err != nil {
		t.Fatalf("typed timestamps rejected: %v", err)
	}
	if err := validateCatalogSensitiveData(map[string]any{
		"expires_at": "not-a-timestamp-0123456789abcdef0123456789abcdef",
	}); err == nil {
		t.Fatal("opaque high-entropy timestamp lookalike accepted")
	}
}

func TestValidateCatalogSensitiveDataAllowsPinnedEC2ProvisionReadback(t *testing.T) {
	readback := coreaws.CloudFormationProvisionReadback{
		StackName:        "dirextalk-v2-0123456789abcdef01234567",
		StackID:          "arn:aws:cloudformation:us-east-1:123456789012:stack/dirextalk-v2-0123456789abcdef01234567/35353535-3535-4535-8535-353535353535",
		Status:           "CREATE_IN_PROGRESS",
		AvailabilityZone: "us-east-1a",
	}
	if err := validateCatalogSensitiveData(readback); err != nil {
		t.Fatalf("typed EC2 readback rejected: %v", err)
	}
}

func TestValidateCatalogSensitiveDataAllowsOnlyDigestPinnedOCIImage(t *testing.T) {
	valid := "public.ecr.aws/u0c3l3d8/dirextalk-v2-live-web-20260731@sha256:" + strings.Repeat("a", 64)
	if err := validateCatalogSensitiveData(map[string]any{"image": valid}); err != nil {
		t.Fatalf("digest-pinned OCI image rejected: %v", err)
	}
	snapshot := coreaws.FrozenRequestSnapshot{
		OwnerID:      "@execution-service-loop:example.org",
		Fence:        "35353535-3535-4535-8535-353535353535:1:apply-container",
		CredentialID: "45454545-4545-4545-8545-454545454545", CredentialRevision: 1,
		CredentialAccountID: "123456789012", CredentialRegion: "us-east-1",
		CredentialUserARN: "arn:aws:iam::123456789012:role/execution-v2",
		Script: coreaws.FrozenScript{Step: coreexecution.ExecutionStep{
			Kind: coreexecution.StepContainerApply,
			ContainerApply: &coreexecution.ContainerApplyStep{
				Image: valid, Name: "dirextalk-service", HostAddress: "127.0.0.1",
				HostPort: 8080, ContainerPort: 8080, RestartPolicy: "unless-stopped",
			},
		}},
	}
	if err := validateCatalogSensitiveData(snapshot); err != nil {
		t.Fatalf("redacted container SSM snapshot rejected: %v", err)
	}
	for name, image := range map[string]string{
		"tag only":     "public.ecr.aws/team/service:latest",
		"userinfo":     "user:password@registry.example/team/service@sha256:" + strings.Repeat("a", 64),
		"scheme":       "https://registry.example/team/service@sha256:" + strings.Repeat("a", 64),
		"query":        "registry.example/team/service@sha256:" + strings.Repeat("a", 64) + "?token=value",
		"upper digest": "registry.example/team/service@sha256:" + strings.Repeat("A", 64),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateCatalogSensitiveData(map[string]any{"image": image}); err == nil {
				t.Fatal("non-canonical OCI image accepted")
			}
		})
	}
}

func TestValidateCatalogSensitiveDataAllowsRedactedAISecretReference(t *testing.T) {
	valid := "public.ecr.aws/u0c3l3d8/dirextalk-v2-live-openclaw-20260731@sha256:" + strings.Repeat("a", 64)
	snapshot := coreaws.FrozenRequestSnapshot{
		OwnerID:      "@execution-service-loop:example.org",
		Fence:        "35353535-3535-4535-8535-353535353535:1:apply-container",
		CredentialID: "45454545-4545-4545-8545-454545454545", CredentialRevision: 1,
		CredentialAccountID: "123456789012", CredentialRegion: "us-east-1",
		CredentialUserARN: "arn:aws:iam::123456789012:role/execution-v2",
		Script: coreaws.FrozenScript{
			SecretRefs: []coreexecution.CredentialRef{{
				Ref:           "55555555-5555-4555-8555-555555555555",
				Purpose:       coreexecution.AISecretPurposeProviderAPIKey,
				Revision:      1,
				BindingDigest: coreexecution.Digest(strings.Repeat("b", 64)),
			}},
			Step: coreexecution.ExecutionStep{
				Kind: coreexecution.StepContainerApply,
				SecretRefs: []coreexecution.CredentialRef{{
					Ref:           "55555555-5555-4555-8555-555555555555",
					Purpose:       coreexecution.AISecretPurposeProviderAPIKey,
					Revision:      1,
					BindingDigest: coreexecution.Digest(strings.Repeat("b", 64)),
				}},
				ContainerApply: &coreexecution.ContainerApplyStep{
					Image: valid, Name: "dirextalk-service", HostAddress: "127.0.0.1",
					HostPort: 8080, ContainerPort: 8080, RestartPolicy: "unless-stopped",
				},
			},
		},
	}
	if err := validateCatalogSensitiveData(snapshot); err != nil {
		t.Fatalf("redacted AI secret reference rejected at %s: %v", catalogSensitiveJSONFailure(mustCatalogJSON(t, snapshot), "", "root"), err)
	}
}

func TestValidateCatalogSensitiveDataAllowsOnlyTargetLocalProbeURL(t *testing.T) {
	valid := "http://127.0.0.1:18789/healthz/openclaw"
	if err := validateCatalogSensitiveData(map[string]any{"http_probe": map[string]any{"url": valid}}); err != nil {
		t.Fatalf("typed target-local probe URL rejected: %v", err)
	}
	for name, value := range map[string]string{
		"remote":        "https://example.test/healthz",
		"userinfo":      "http://user:pass@127.0.0.1:18789/healthz",
		"query":         "http://127.0.0.1:18789/healthz?token=value",
		"fragment":      "http://127.0.0.1:18789/healthz#opaque",
		"encoded":       "http://127.0.0.1:18789/%68ealthz",
		"invalid port":  "http://127.0.0.1:99999/healthz",
		"token carrier": "http://127.0.0.1:18789/" + strings.Repeat("a", 256),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateCatalogSensitiveData(map[string]any{"http_probe": map[string]any{"url": value}}); err == nil {
				t.Fatal("unsafe probe URL accepted")
			}
		})
	}
}

func mustCatalogJSON(t *testing.T, value any) any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}
