package aws

import (
	"reflect"
	"strings"
	"testing"
	"time"

	coreworkload "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload"
	"github.com/google/uuid"
)

func validGeoLibreTarget() GeoLibreInstallTarget {
	return GeoLibreInstallTarget{
		ProvisionID:        "11111111-1111-4111-8111-111111111111",
		ProvisionPlanID:    "22222222-2222-4222-8222-222222222222",
		ProvisionRevision:  1,
		CredentialID:       "33333333-3333-4333-8333-333333333333",
		CredentialRevision: 4,
		AccountID:          "123456789012",
		Region:             "ap-east-1",
		InstanceID:         "i-0123456789abcdef0",
		PublicIP:           "192.0.2.10",
		SecurityGroupID:    "sg-0123456789abcdef0",
		OwnerBindingDigest: "sha256:" + strings.Repeat("a", 64),
	}
}

func TestGeoLibreReleaseManifestIsPinnedAndIsolated(t *testing.T) {
	one := CurrentGeoLibreReleaseManifest()
	two := CurrentGeoLibreReleaseManifest()
	if one.Digest() != coreworkload.GeoLibreStaticV1ManifestDigest ||
		coreworkload.CommandStepsDigest(one.CommandSteps) != coreworkload.GeoLibreStaticV1CommandDigest {
		t.Fatalf("unexpected fixed release digests: manifest=%s commands=%s", one.Digest(), coreworkload.CommandStepsDigest(one.CommandSteps))
	}
	if one.Digest() == "" || one.Digest() != two.Digest() || len(one.CommandSteps) == 0 {
		t.Fatal("manifest is not deterministic")
	}
	one.CommandSteps[0] = "forged"
	if two.CommandSteps[0] == "forged" {
		t.Fatal("manifest commands share mutable storage")
	}
	joined := strings.Join(two.CommandSteps, "\n")
	for _, required := range []string{
		GeoLibreImageURI,
		"docker pull --platform linux/amd64",
		"GEOLIBRE_DISABLE_SIDECAR=1",
		"systemctl enable --now " + GeoLibreSystemdService,
		"http://127.0.0.1/healthz",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("manifest commands missing %q", required)
		}
	}
	nginxTemplate := coreworkload.GeoLibreStaticV1NginxTemplate()
	if !strings.Contains(nginxTemplate, "location /sidecar/ { return 404; }") ||
		strings.Contains(nginxTemplate, "localhost") ||
		strings.Contains(nginxTemplate, "127.0.0.1:*") {
		t.Fatal("hardened nginx template exposes sidecar or browser loopback")
	}
	for _, forbidden := range []string{"latest", "workload.core_runner", "docker.sock", "docker build", "git clone", "git fetch"} {
		if strings.Contains(strings.ToLower(joined), forbidden) {
			t.Fatalf("manifest commands contain forbidden mutable/runtime value %q", forbidden)
		}
	}
}

func TestBuildGeoLibreSSMPlanUsesOnlyProvisionIdentity(t *testing.T) {
	expires := time.Now().UTC().Add(time.Hour)
	target := validGeoLibreTarget()
	input, err := BuildGeoLibreSSMPlan(target, uuid.NewString(), expires)
	if err != nil {
		t.Fatal(err)
	}
	if input.TargetKind != coreworkload.TargetAWSEC2SSM ||
		input.Target.Identity.InstanceID != target.InstanceID ||
		input.Target.Identity.Endpoint != "http://"+target.PublicIP ||
		input.Target.RequiredInstanceTags["owner"] != target.OwnerBindingDigest ||
		input.Target.RequiredInstanceTags["dirextalk:plan-id"] != target.ProvisionPlanID ||
		input.Target.EC2DocumentVersion != "1" ||
		input.Target.EC2SystemdService != GeoLibreSystemdService ||
		input.Target.EC2CleanupProfile != coreworkload.EC2CleanupProfileGeoLibreStaticV1 ||
		input.ImageURI != GeoLibreImageURI ||
		input.ImageDigest != GeoLibreImageDigest ||
		!strings.Contains(input.Summary, "public unauthenticated HTTP") ||
		!strings.Contains(input.Summary, "no TLS") {
		t.Fatalf("unsafe or incomplete target: %#v", input.Target)
	}
	if len(input.SecretGrantRefs) != 1 ||
		input.SecretGrantRefs[0].ReferenceID != target.CredentialID ||
		input.SecretGrantRefs[0].Revision != target.CredentialRevision {
		t.Fatalf("credential revision not pinned: %#v", input.SecretGrantRefs)
	}
	before := append([]string(nil), input.CommandSteps...)
	manifest := CurrentGeoLibreReleaseManifest()
	manifest.CommandSteps[0] = "forged"
	if !reflect.DeepEqual(before, input.CommandSteps) {
		t.Fatal("plan commands were mutable through manifest copy")
	}
}

func TestBuildGeoLibreSSMPlanRejectsUntrustedIdentity(t *testing.T) {
	base := validGeoLibreTarget()
	cases := []struct {
		name string
		edit func(*GeoLibreInstallTarget)
	}{
		{"provision", func(v *GeoLibreInstallTarget) { v.ProvisionID = "bad" }},
		{"plan", func(v *GeoLibreInstallTarget) { v.ProvisionPlanID = "bad" }},
		{"credential", func(v *GeoLibreInstallTarget) { v.CredentialID = "bad" }},
		{"credential revision", func(v *GeoLibreInstallTarget) { v.CredentialRevision = 0 }},
		{"account", func(v *GeoLibreInstallTarget) { v.AccountID = "123" }},
		{"region", func(v *GeoLibreInstallTarget) { v.Region = "ap-east-1a" }},
		{"instance", func(v *GeoLibreInstallTarget) { v.InstanceID = "i-$(touch /tmp/pwn)" }},
		{"public ip", func(v *GeoLibreInstallTarget) { v.PublicIP = "127.0.0.1;id" }},
		{"security group", func(v *GeoLibreInstallTarget) { v.SecurityGroupID = "sg-x" }},
		{"owner", func(v *GeoLibreInstallTarget) { v.OwnerBindingDigest = "sha256:ABC" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := base
			tc.edit(&target)
			if _, err := BuildGeoLibreSSMPlan(target, uuid.NewString(), time.Now().UTC().Add(time.Hour)); err == nil {
				t.Fatalf("unsafe target accepted: %#v", target)
			}
		})
	}
	if _, err := BuildGeoLibreSSMPlan(base, "model-key", time.Now().UTC().Add(time.Hour)); err == nil {
		t.Fatal("non-UUID idempotency key accepted")
	}
}

func TestBuildGeoLibreSSMPlanRejectsNonPublicOrNonCanonicalIPv4(t *testing.T) {
	for _, value := range []string{
		"127.0.0.1",
		"10.0.0.1",
		"169.254.1.1",
		"0.0.0.0",
		"224.0.0.1",
		"2001:db8::1",
		"192.0.2.010",
		" 192.0.2.10 ",
	} {
		t.Run(value, func(t *testing.T) {
			target := validGeoLibreTarget()
			target.PublicIP = value
			if _, err := BuildGeoLibreSSMPlan(target, uuid.NewString(), time.Now().UTC().Add(time.Hour)); err == nil {
				t.Fatalf("unsafe address %q accepted", value)
			}
		})
	}
}
