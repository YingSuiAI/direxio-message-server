package executionplanning

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	"gopkg.in/yaml.v2"
)

var (
	environmentNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	packageNameRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@._/+:-]{0,199}$`)
	sensitiveNameRE   = regexp.MustCompile(`(?i)(secret|token|password|passwd|credential|authorization|api[_-]?key|client[_-]?secret|private[_-]?key|access[_-]?key|(encryption|signing|ssh)[_-]?key|database[_-]?url|db[_-]?url|connection[_-]?string|(^|[_-])auth($|[_-]))`)
	yamlAliasRE       = regexp.MustCompile(`(^|[\s:\-\[,])(?:&|\*)[A-Za-z0-9_-]+`)
)

type staticAnalysisCollector struct {
	source        coreexecution.SourceRef
	stacks        map[string]bool
	dependencies  map[string]bool
	environments  map[string]bool
	secretPurpose map[string]bool
	volumes       map[string]bool
	migrations    map[string]bool
	probes        map[string]bool
	assumptions   map[string]bool
	blockers      map[string]bool
	ports         map[int]bool
	exposure      string
	manifestKinds map[string]int
	nodeStart     bool
	pythonStart   bool
	containerRun  bool
}

func newStaticAnalysisCollector(source coreexecution.SourceRef) *staticAnalysisCollector {
	return &staticAnalysisCollector{
		source: source, stacks: map[string]bool{}, dependencies: map[string]bool{},
		environments: map[string]bool{}, secretPurpose: map[string]bool{}, volumes: map[string]bool{},
		migrations: map[string]bool{}, probes: map[string]bool{}, assumptions: map[string]bool{},
		blockers: map[string]bool{}, ports: map[int]bool{}, manifestKinds: map[string]int{},
	}
}

func (c *staticAnalysisCollector) inspect(manifest staticManifest) {
	c.manifestKinds[manifest.kind]++
	var err error
	switch manifest.kind {
	case "dockerfile":
		err = c.inspectDockerfile(manifest.data)
	case "compose":
		err = c.inspectCompose(manifest.data)
	case "go.mod":
		err = c.inspectGoMod(manifest.data)
	case "package.json":
		err = c.inspectPackageJSON(manifest.data)
	case "pyproject.toml":
		err = c.inspectPyproject(manifest.data)
	case "requirements":
		err = c.inspectRequirements(manifest.data)
	case "Cargo.toml":
		err = c.inspectCargo(manifest.data)
	case "pubspec":
		err = c.inspectPubspec(manifest.data)
	}
	if err != nil {
		c.block("a supported " + manifest.kind + " manifest is malformed or exceeds parser limits")
	}
}

func (c *staticAnalysisCollector) facts() SourceFacts {
	if c.manifestKinds["package.json"] > 0 && !c.nodeStart && c.manifestKinds["dockerfile"] == 0 && c.manifestKinds["compose"] == 0 {
		c.block("Node service start behavior is not declared by an allowlisted manifest")
	}
	if (c.manifestKinds["pyproject.toml"] > 0 || c.manifestKinds["requirements"] > 0) && !c.pythonStart && c.manifestKinds["dockerfile"] == 0 && c.manifestKinds["compose"] == 0 {
		c.block("Python service start behavior is not declared by an allowlisted manifest")
	}
	if c.manifestKinds["go.mod"] > 0 && c.manifestKinds["dockerfile"] == 0 && c.manifestKinds["compose"] == 0 {
		c.block("Go module entrypoint is not established by allowlisted manifests")
	}
	if len(c.stacks) > 1 {
		c.assume("multiple project stacks were detected and require an explicit deployment selection")
	}
	c.assume("analysis is static and only allowlisted manifests were read")
	c.assume("project code and manifest commands were not executed on the Message Server")
	c.assume("resource sizing requires a target-specific placement decision")
	ports := make([]int, 0, len(c.ports))
	for port := range c.ports {
		ports = append(ports, port)
	}
	sort.Ints(ports)
	analysis := coreexecution.ProjectAnalysis{
		Source: c.source, DetectedStacks: mapKeys(c.stacks, 128), Dependencies: mapKeys(c.dependencies, 128),
		Ports: ports, EnvironmentNames: mapKeys(c.environments, 128), SecretPurposes: mapKeys(c.secretPurpose, 128),
		Volumes: mapKeys(c.volumes, 128), Migrations: mapKeys(c.migrations, 128), Probes: mapKeys(c.probes, 128),
		Exposure: c.exposure, Assumptions: mapKeys(c.assumptions, 128), BlockingUncertainties: mapKeys(c.blockers, 128),
	}
	return SourceFacts{Analysis: analysis, BlockingUncertainties: append([]string(nil), analysis.BlockingUncertainties...)}
}

func (c *staticAnalysisCollector) block(value string)  { addBounded(c.blockers, value) }
func (c *staticAnalysisCollector) assume(value string) { addBounded(c.assumptions, value) }
func (c *staticAnalysisCollector) stack(value string)  { addBounded(c.stacks, value) }
func (c *staticAnalysisCollector) dependency(kind, name string) {
	if safePackageName(name) {
		addBounded(c.dependencies, kind+" dependency "+name)
	} else {
		c.block("a manifest contains a dependency name that cannot be safely cataloged")
	}
}
func (c *staticAnalysisCollector) environment(name string) {
	if !safeEnvironmentName(name) {
		c.block("a manifest contains an environment name that cannot be safely cataloged")
		return
	}
	addBounded(c.environments, name)
	if sensitiveNameRE.MatchString(name) {
		addBounded(c.secretPurpose, "environment secret for "+name)
	}
}
func (c *staticAnalysisCollector) port(value int) {
	if value >= 1 && value <= 65535 && len(c.ports) < 128 {
		c.ports[value] = true
		c.exposure = "declared_ports"
	} else {
		c.block("a manifest contains an invalid or excessive port declaration")
	}
}

func (c *staticAnalysisCollector) inspectDockerfile(data []byte) error {
	lines, err := boundedLogicalLines(data)
	if err != nil {
		return err
	}
	c.stack("container")
	fromCount := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		parts := strings.Fields(trimmed)
		if len(parts) == 0 {
			continue
		}
		switch strings.ToUpper(parts[0]) {
		case "FROM":
			fromCount++
			image := ""
			for _, part := range parts[1:] {
				if !strings.HasPrefix(part, "--") {
					image = part
					break
				}
			}
			if image == "" || strings.ContainsAny(image, "${}") {
				c.block("Dockerfile base image cannot be resolved statically")
				continue
			}
			c.dependency("OCI", stripOCIIdentity(image))
			if !pinnedOCI(image) {
				c.block("Dockerfile base images are not all pinned by SHA-256 digest")
			}
		case "EXPOSE":
			for _, value := range parts[1:] {
				value = strings.SplitN(value, "/", 2)[0]
				port, parseErr := strconv.Atoi(value)
				if parseErr != nil {
					c.block("Dockerfile exposes a dynamic or non-numeric port")
					continue
				}
				c.port(port)
			}
		case "ENV", "ARG":
			for _, value := range parts[1:] {
				name := strings.SplitN(value, "=", 2)[0]
				c.environment(name)
				if !strings.Contains(value, "=") {
					break
				}
			}
		case "HEALTHCHECK":
			addBounded(c.probes, "Docker health check")
		case "CMD", "ENTRYPOINT":
			c.containerRun = true
		case "RUN":
			c.block("Dockerfile RUN dependency effects require a separately frozen build plan")
		}
	}
	if fromCount == 0 {
		return fmt.Errorf("missing FROM")
	}
	if !c.containerRun {
		c.block("Dockerfile has no explicit CMD or ENTRYPOINT")
	}
	return nil
}

func (c *staticAnalysisCollector) inspectCompose(data []byte) error {
	root, err := decodeBoundedYAML(data)
	if err != nil {
		return err
	}
	services, ok := yamlMap(root["services"])
	if !ok || len(services) == 0 || len(services) > 64 {
		return fmt.Errorf("invalid services")
	}
	c.stack("docker-compose")
	for _, serviceName := range sortedAnyMapKeys(services) {
		service, ok := yamlMap(services[serviceName])
		if !ok {
			return fmt.Errorf("invalid service")
		}
		if image, ok := yamlString(service["image"]); ok && image != "" {
			c.dependency("OCI", stripOCIIdentity(image))
			if !pinnedOCI(image) {
				c.block("Compose service images are not all pinned by SHA-256 digest")
			}
		}
		if _, ok := service["build"]; ok {
			c.stack("container-build")
		}
		for _, rawPort := range yamlList(service["ports"]) {
			if port, ok := composeTargetPort(rawPort); ok {
				c.port(port)
			} else {
				c.block("Compose contains a dynamic or unsupported port mapping")
			}
		}
		collectComposeEnvironment(c, service["environment"])
		for _, volume := range yamlList(service["volumes"]) {
			if value, ok := yamlString(volume); ok {
				parts := strings.Split(value, ":")
				if len(parts) >= 2 && safeManifestPath(parts[len(parts)-1]) {
					addBounded(c.volumes, "container volume "+parts[len(parts)-1])
				} else {
					c.block("Compose volume mapping cannot be resolved statically")
				}
			} else {
				c.block("Compose long-form volume mapping requires explicit review")
			}
		}
		if _, ok := service["secrets"]; ok {
			addBounded(c.secretPurpose, "Compose declared secret")
		}
		if _, ok := service["healthcheck"]; ok {
			addBounded(c.probes, "Compose health check")
		}
	}
	return nil
}

func (c *staticAnalysisCollector) inspectGoMod(data []byte) error {
	lines, err := boundedPhysicalLines(data)
	if err != nil {
		return err
	}
	c.stack("go")
	moduleSeen := false
	inRequire := false
	for _, raw := range lines {
		line := strings.TrimSpace(strings.SplitN(raw, "//", 2)[0])
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "module ") {
			moduleSeen = safePackageName(strings.TrimSpace(strings.TrimPrefix(line, "module ")))
			continue
		}
		if line == "require (" {
			inRequire = true
			continue
		}
		if inRequire && line == ")" {
			inRequire = false
			continue
		}
		if strings.HasPrefix(line, "replace ") && (strings.Contains(line, " => ./") || strings.Contains(line, " => ../") || strings.Contains(line, " => /")) {
			c.block("go.mod contains a local replacement whose target was not inspected")
		}
		if inRequire || strings.HasPrefix(line, "require ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "require "))
			parts := strings.Fields(line)
			if len(parts) < 2 || !safePackageName(parts[0]) {
				return fmt.Errorf("invalid requirement")
			}
			c.dependency("Go module", parts[0])
			if !strings.HasPrefix(parts[1], "v") {
				c.block("go.mod contains a dependency without a canonical module version")
			}
		}
	}
	if !moduleSeen {
		return fmt.Errorf("missing module")
	}
	return nil
}

func (c *staticAnalysisCollector) inspectPackageJSON(data []byte) error {
	var manifest struct {
		Scripts          map[string]string `json:"scripts"`
		Dependencies     map[string]string `json:"dependencies"`
		DevDependencies  map[string]string `json:"devDependencies"`
		PeerDependencies map[string]string `json:"peerDependencies"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	if err := decoder.Decode(&manifest); err != nil {
		return err
	}
	var extra any
	if decoder.Decode(&extra) == nil {
		return fmt.Errorf("multiple values")
	}
	c.stack("node")
	for _, dependencies := range []map[string]string{manifest.Dependencies, manifest.DevDependencies, manifest.PeerDependencies} {
		for name, version := range dependencies {
			c.dependency("npm", name)
			if !exactPackageVersion(version) {
				c.block("package.json dependencies are not all exact immutable versions")
			}
		}
	}
	for name := range manifest.Scripts {
		switch strings.ToLower(name) {
		case "start", "serve":
			c.nodeStart = true
		case "migrate", "migration", "db:migrate":
			addBounded(c.migrations, "package.json migration script")
		}
	}
	return nil
}

func (c *staticAnalysisCollector) inspectRequirements(data []byte) error {
	lines, err := boundedPhysicalLines(data)
	if err != nil {
		return err
	}
	c.stack("python")
	for _, raw := range lines {
		line := strings.TrimSpace(strings.SplitN(raw, "#", 2)[0])
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "-") {
			c.block("requirements include or installer options require explicit dependency resolution")
			continue
		}
		name := pythonRequirementName(line)
		if name == "" {
			return fmt.Errorf("invalid requirement")
		}
		c.dependency("Python", name)
		if !strings.Contains(line, "==") || strings.ContainsAny(line, "*<>=!~") && strings.Count(line, "==") != 1 {
			c.block("Python requirements are not all exact versions")
		}
	}
	return nil
}

func (c *staticAnalysisCollector) inspectPyproject(data []byte) error {
	lines, err := boundedPhysicalLines(data)
	if err != nil {
		return err
	}
	c.stack("python")
	section := ""
	for _, raw := range lines {
		line := strings.TrimSpace(stripTOMLComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.Trim(line, "[] ")
			continue
		}
		key, value, ok := splitAssignment(line)
		if !ok {
			continue
		}
		if section == "project.scripts" || section == "tool.poetry.scripts" {
			if safePackageName(key) {
				c.pythonStart = true
			}
		}
		if section == "tool.poetry.dependencies" && key != "python" {
			c.dependency("Python", unquoteSimple(key))
			if !strings.HasPrefix(strings.TrimSpace(value), `"==`) && !strings.HasPrefix(strings.TrimSpace(value), `'==`) {
				c.block("pyproject dependencies are not all exact versions")
			}
		}
		if section == "project" && key == "dependencies" {
			for _, dependency := range quotedValues(value) {
				name := pythonRequirementName(dependency)
				if name != "" {
					c.dependency("Python", name)
				}
				if !strings.Contains(dependency, "==") {
					c.block("pyproject dependencies are not all exact versions")
				}
			}
			if strings.Contains(value, "[") && !strings.Contains(value, "]") {
				c.block("multiline pyproject dependencies require a fuller trusted parser")
			}
		}
	}
	return nil
}

func (c *staticAnalysisCollector) inspectCargo(data []byte) error {
	lines, err := boundedPhysicalLines(data)
	if err != nil {
		return err
	}
	c.stack("rust")
	section := ""
	for _, raw := range lines {
		line := strings.TrimSpace(stripTOMLComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.Trim(line, "[] ")
			continue
		}
		if !strings.HasSuffix(section, "dependencies") && !strings.Contains(section, "dependencies.") {
			continue
		}
		key, value, ok := splitAssignment(line)
		if !ok {
			continue
		}
		name := unquoteSimple(key)
		c.dependency("Cargo", name)
		if strings.Contains(value, "path") {
			c.block("Cargo.toml contains local path dependencies that were not inspected")
		}
		if strings.Contains(value, "git") && !strings.Contains(value, "rev") {
			c.block("Cargo git dependencies are not all pinned to revisions")
		}
		trimmed := strings.TrimSpace(value)
		if (strings.HasPrefix(trimmed, `"`) || strings.HasPrefix(trimmed, `'`)) && !strings.Contains(trimmed, `"=`) && !strings.Contains(trimmed, `'=`) {
			c.block("Cargo dependencies rely on ranges or lockfile resolution")
		}
	}
	return nil
}

func (c *staticAnalysisCollector) inspectPubspec(data []byte) error {
	root, err := decodeBoundedYAML(data)
	if err != nil {
		return err
	}
	c.stack("dart")
	if environment, ok := yamlMap(root["environment"]); ok {
		if _, exists := environment["flutter"]; exists {
			c.stack("flutter")
		}
	}
	for _, section := range []string{"dependencies", "dev_dependencies"} {
		dependencies, ok := yamlMap(root[section])
		if !ok {
			continue
		}
		for _, name := range sortedAnyMapKeys(dependencies) {
			if name == "flutter" {
				c.stack("flutter")
			}
			c.dependency("Dart", name)
			value := dependencies[name]
			if nested, ok := yamlMap(value); ok {
				if _, pathDependency := nested["path"]; pathDependency {
					c.block("pubspec contains local path dependencies that were not inspected")
				}
				if _, gitDependency := nested["git"]; gitDependency {
					c.block("pubspec git dependencies require an exact revision review")
				}
				continue
			}
			constraint, ok := yamlString(value)
			if ok && constraint != "any" && !strings.HasPrefix(constraint, "=") {
				c.block("pubspec dependencies rely on ranges or lockfile resolution")
			}
		}
	}
	return nil
}

func boundedLogicalLines(data []byte) ([]string, error) {
	physical, err := boundedPhysicalLines(data)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(physical))
	current := ""
	for _, line := range physical {
		if len(current)+len(line) > 16<<10 {
			return nil, fmt.Errorf("logical line too large")
		}
		current += line
		if strings.HasSuffix(strings.TrimSpace(current), "\\") {
			current = strings.TrimSuffix(strings.TrimSpace(current), "\\") + " "
			continue
		}
		out = append(out, current)
		current = ""
	}
	if current != "" {
		out = append(out, current)
	}
	return out, nil
}

func boundedPhysicalLines(data []byte) ([]string, error) {
	if int64(len(data)) > maxStaticManifestBytes || !utf8Safe(data) {
		return nil, fmt.Errorf("manifest size or encoding")
	}
	scanner := bufio.NewScanner(bytesReader(data))
	scanner.Buffer(make([]byte, 4096), 16<<10)
	lines := make([]string, 0)
	for scanner.Scan() {
		if len(lines) >= 8192 {
			return nil, fmt.Errorf("too many lines")
		}
		lines = append(lines, scanner.Text())
	}
	return lines, scanner.Err()
}

func decodeBoundedYAML(data []byte) (map[interface{}]interface{}, error) {
	if int64(len(data)) > maxStaticManifestBytes || !utf8Safe(data) {
		return nil, fmt.Errorf("YAML size or encoding")
	}
	text := string(data)
	if strings.Contains(text, "<<:") || strings.Contains(text, "!!") || strings.Contains(text, "!<") || yamlAliasRE.MatchString(text) {
		return nil, fmt.Errorf("YAML aliases or tags are not allowed")
	}
	var value interface{}
	if err := yaml.UnmarshalStrict(data, &value); err != nil {
		return nil, err
	}
	nodes := 0
	if !boundedYAMLValue(value, 0, &nodes) {
		return nil, fmt.Errorf("YAML structure limit")
	}
	root, ok := yamlMap(value)
	if !ok {
		return nil, fmt.Errorf("YAML root")
	}
	return root, nil
}

func boundedYAMLValue(value interface{}, depth int, nodes *int) bool {
	(*nodes)++
	if depth > 24 || *nodes > 8192 {
		return false
	}
	switch typed := value.(type) {
	case map[interface{}]interface{}:
		for key, child := range typed {
			if _, ok := key.(string); !ok || !boundedYAMLValue(child, depth+1, nodes) {
				return false
			}
		}
	case []interface{}:
		for _, child := range typed {
			if !boundedYAMLValue(child, depth+1, nodes) {
				return false
			}
		}
	case string:
		return len(typed) <= 4096 && !strings.ContainsAny(typed, "\r\n\x00")
	case nil, bool, int, uint64, float64:
		return true
	default:
		return false
	}
	return true
}

func stableStrings(values []string, limit int) []string {
	set := map[string]bool{}
	for _, value := range values {
		addBounded(set, value)
	}
	return mapKeys(set, limit)
}

func mapKeys(values map[string]bool, limit int) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func addBounded(target map[string]bool, value string) {
	value = strings.TrimSpace(value)
	if value != "" && len(value) <= 512 && !strings.ContainsAny(value, "\r\n\x00") && len(target) < 128 {
		target[value] = true
	}
}

func safeEnvironmentName(value string) bool {
	// Environment names are intentionally cataloged without their values. Long
	// conventional names such as PLAYWRIGHT_BROWSERS_PATH can look entropic,
	// but the closed identifier grammar is the authority at this boundary; the
	// corresponding value still never enters ProjectAnalysis.
	return environmentNameRE.MatchString(value)
}

func safePackageName(value string) bool {
	return packageNameRE.MatchString(value) && !highEntropyManifestString(value)
}

func highEntropyManifestString(value string) bool {
	if len(value) < 24 {
		return false
	}
	counts := map[rune]int{}
	length := 0
	for _, r := range value {
		if unicode.IsSpace(r) {
			return false
		}
		counts[r]++
		length++
	}
	entropy := 0.0
	for _, count := range counts {
		p := float64(count) / float64(length)
		entropy -= p * math.Log2(p)
	}
	return entropy >= 3.75
}

func pinnedOCI(value string) bool {
	const marker = "@sha256:"
	index := strings.LastIndex(value, marker)
	return index > 0 && len(value[index+len(marker):]) == 64 && isHexPin(value[index+len(marker):], 64)
}

func stripOCIIdentity(value string) string {
	if index := strings.LastIndex(value, "@sha256:"); index > 0 {
		return value[:index]
	}
	lastSlash := strings.LastIndexByte(value, '/')
	if lastColon := strings.LastIndexByte(value, ':'); lastColon > lastSlash {
		return value[:lastColon]
	}
	return value
}

func exactPackageVersion(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "^~*<>=| ") || strings.HasPrefix(value, "git+") || strings.HasPrefix(value, "file:") || strings.HasPrefix(value, "workspace:") {
		return false
	}
	parts := strings.Split(value, ".")
	return len(parts) >= 3 && parts[0] != "" && parts[1] != "" && parts[2] != ""
}

func pythonRequirementName(value string) string {
	value = strings.TrimSpace(value)
	if index := strings.Index(value, ";"); index >= 0 {
		value = value[:index]
	}
	for i, r := range value {
		if strings.ContainsRune("<>=!~[ ", r) {
			value = value[:i]
			break
		}
	}
	value = strings.TrimSpace(value)
	if safePackageName(value) {
		return value
	}
	return ""
}

func composeTargetPort(value interface{}) (int, bool) {
	if number, ok := value.(int); ok {
		return number, number >= 1 && number <= 65535
	}
	if mapping, ok := yamlMap(value); ok {
		if number, ok := mapping["target"].(int); ok {
			return number, number >= 1 && number <= 65535
		}
		if text, ok := yamlString(mapping["target"]); ok {
			number, err := strconv.Atoi(text)
			return number, err == nil && number >= 1 && number <= 65535
		}
	}
	text, ok := yamlString(value)
	if !ok || strings.Contains(text, "-") {
		return 0, false
	}
	text = strings.SplitN(text, "/", 2)[0]
	parts := strings.Split(text, ":")
	number, err := strconv.Atoi(parts[len(parts)-1])
	return number, err == nil && number >= 1 && number <= 65535
}

func collectComposeEnvironment(c *staticAnalysisCollector, value interface{}) {
	if mapping, ok := yamlMap(value); ok {
		for _, name := range sortedAnyMapKeys(mapping) {
			c.environment(name)
		}
		return
	}
	for _, item := range yamlList(value) {
		if text, ok := yamlString(item); ok {
			c.environment(strings.SplitN(text, "=", 2)[0])
		}
	}
}

func yamlMap(value interface{}) (map[interface{}]interface{}, bool) {
	mapping, ok := value.(map[interface{}]interface{})
	return mapping, ok
}
func yamlList(value interface{}) []interface{} {
	items, _ := value.([]interface{})
	return items
}
func yamlString(value interface{}) (string, bool) {
	text, ok := value.(string)
	return strings.TrimSpace(text), ok
}
func sortedAnyMapKeys(value map[interface{}]interface{}) []string {
	out := make([]string, 0, len(value))
	for key := range value {
		if text, ok := key.(string); ok && len(text) <= 256 {
			out = append(out, text)
		}
	}
	sort.Strings(out)
	return out
}

func safeManifestPath(value string) bool {
	return strings.HasPrefix(value, "/") && len(value) <= 256 && !strings.Contains(value, "..") && !strings.ContainsAny(value, "\r\n\x00")
}

func splitAssignment(value string) (string, string, bool) {
	index := strings.Index(value, "=")
	if index <= 0 {
		return "", "", false
	}
	return strings.TrimSpace(value[:index]), strings.TrimSpace(value[index+1:]), true
}

func stripTOMLComment(value string) string {
	quoted := rune(0)
	for index, r := range value {
		if r == '\'' || r == '"' {
			if quoted == 0 {
				quoted = r
			} else if quoted == r {
				quoted = 0
			}
		}
		if r == '#' && quoted == 0 {
			return value[:index]
		}
	}
	return value
}

func quotedValues(value string) []string {
	out := make([]string, 0)
	quote := rune(0)
	start := -1
	for index, r := range value {
		if quote == 0 && (r == '\'' || r == '"') {
			quote, start = r, index+1
			continue
		}
		if quote != 0 && r == quote {
			out = append(out, value[start:index])
			quote, start = 0, -1
		}
	}
	return out
}

func unquoteSimple(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
		return value[1 : len(value)-1]
	}
	return value
}

func utf8Safe(data []byte) bool {
	return utf8.Valid(data) && !strings.ContainsRune(string(data), '\x00')
}
func bytesReader(data []byte) *strings.Reader { return strings.NewReader(string(data)) }
