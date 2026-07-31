package agentskills

import (
	"embed"
	"fmt"
	"sort"
)

//go:embed manifests/*.json
var builtinFS embed.FS

// Builtin returns the trusted manifests shipped with the server.
func Builtin() (*Registry, error) {
	// Keep filenames explicit so the embedded registry order is stable.
	contents := make([][]byte, 0, len(builtinManifestNames))
	for _, name := range builtinManifestNames {
		content, err := builtinFS.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("read built-in manifest %s: %w", name, err)
		}
		contents = append(contents, content)
	}
	return NewRegistry(contents...)
}

// NewBuiltinRegistry is an explicit alias for callers that prefer
// constructor naming.
func NewBuiltinRegistry() (*Registry, error) { return Builtin() }

var builtinManifestNames = []string{
	"manifests/aws-target-advisor.json", "manifests/container-service-deploy.json",
	"manifests/health-verifier.json", "manifests/placement-advisor.json",
	"manifests/project-intake-analyzer.json", "manifests/repair-and-rollback.json",
	"manifests/resource-sizing.json", "manifests/source-build-systemd.json",
	"manifests/usage-runbook-generator.json",
}

func init() { sort.Strings(builtinManifestNames) }
