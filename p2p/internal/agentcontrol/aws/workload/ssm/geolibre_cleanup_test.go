package ssm

import (
	"strings"
	"testing"

	coreworkload "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload"
)

func TestGeoLibreCleanupProfileRemovesAllRuntimeArtifacts(t *testing.T) {
	plan := coreworkload.Plan{Target: coreworkload.TargetSettings{
		EC2SystemdService: "dirextalk-geolibre.service",
		EC2CleanupProfile: coreworkload.EC2CleanupProfileGeoLibreStaticV1,
	}}
	commands, err := destroyCommands(plan)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(commands, "\n")
	for _, required := range []string{
		"set -euo pipefail",
		"systemctl stop dirextalk-geolibre.service",
		"systemctl disable dirextalk-geolibre.service",
		"docker info",
		"docker rm -f dirextalk-geolibre",
		"docker image rm ghcr.io/opengeos/geolibre@sha256:bd18a93768087e5619e75e2e8282ce347aed9179987ee8a7f471df862b72d64d",
		"rm -f /etc/systemd/system/dirextalk-geolibre.service /var/lib/dirextalk-geolibre/nginx.conf.template",
		"systemctl daemon-reload",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("cleanup missing %q: %s", required, joined)
		}
	}
	for _, forbidden := range []string{"|| true"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("cleanup masks failure with %q: %s", forbidden, joined)
		}
	}
}

func TestDestroyCommandsRejectsUnknownCleanupProfile(t *testing.T) {
	_, err := destroyCommands(coreworkload.Plan{Target: coreworkload.TargetSettings{
		EC2SystemdService: "dirextalk-geolibre.service",
		EC2CleanupProfile: "caller-controlled",
	}})
	if err == nil {
		t.Fatal("unknown cleanup profile accepted")
	}
}

func TestGeoLibreDestroyedProbeRequiresExactArtifactAbsence(t *testing.T) {
	plan := coreworkload.Plan{Target: coreworkload.TargetSettings{
		EC2SystemdService: coreworkload.GeoLibreStaticV1Service,
		EC2CleanupProfile: coreworkload.EC2CleanupProfileGeoLibreStaticV1,
	}}
	probe := destroyedProbe(plan)
	for _, required := range []string{
		"docker info",
		"test ! -e /etc/systemd/system/dirextalk-geolibre.service",
		"test ! -e /var/lib/dirextalk-geolibre/nginx.conf.template",
		"docker container ls",
		"docker image inspect " + coreworkload.GeoLibreStaticV1ImageURI,
	} {
		if !strings.Contains(probe, required) {
			t.Fatalf("destroy probe missing %q: %s", required, probe)
		}
	}
}
