package agentrecipes

import "embed"

//go:embed manifests/*.json
var manifestFS embed.FS
