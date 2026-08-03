package agentskills

import (
	"embed"
	"fmt"
	"sort"
)

//go:embed manifests/*.json manifests/archive/*.json
var builtinFS embed.FS

// Builtin returns the trusted manifests shipped with the server.
func Builtin() (*Registry, error) {
	// Keep filenames explicit so the embedded registry order is stable. Archived
	// manifests are intentionally loaded into a separate resolution tier and do
	// not appear in Manifests or Select.
	contents := make([][]byte, 0, len(builtinManifestNames))
	for _, name := range builtinManifestNames {
		content, err := builtinFS.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("read built-in manifest %s: %w", name, err)
		}
		contents = append(contents, content)
	}
	archived := make([][]byte, 0, len(archivedManifestNames))
	for _, name := range archivedManifestNames {
		content, err := builtinFS.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("read archived built-in manifest %s: %w", name, err)
		}
		archived = append(archived, content)
	}
	return NewRegistryWithArchived(contents, archived)
}

// NewBuiltinRegistry is an explicit alias for callers that prefer
// constructor naming.
func NewBuiltinRegistry() (*Registry, error) { return Builtin() }

var builtinManifestNames = []string{
	"manifests/aws-target-advisor.json", "manifests/container-service-deploy.json",
	"manifests/health-verifier.json", "manifests/project-intake-analyzer.json",
	"manifests/repair-and-rollback.json", "manifests/source-build-systemd.json",
	"manifests/usage-runbook-generator.json",
}

var archivedManifestNames = []string{
	"manifests/archive/aws-target-advisor.json",
	"manifests/archive/placement-advisor.json",
	"manifests/archive/resource-sizing.json",
}

func init() {
	sort.Strings(builtinManifestNames)
	sort.Strings(archivedManifestNames)
}
