// Package agentskills contains trusted, built-in declarative planning skills.
// Skills are immutable planning data; this package never executes them.
package agentskills

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

var (
	canonicalID                = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	semver                     = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
	digest                     = regexp.MustCompile(`^[a-f0-9]{64}$`)
	identifier                 = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
	endpointPattern            = regexp.MustCompile(`^https://[a-z0-9.-]+(?:/[a-zA-Z0-9._${}-]+)*$`)
	embeddedPlaceholderPattern = regexp.MustCompile(`\$\{([a-z][a-z0-9]*(?:[._-][a-z0-9]+)*)\}`)
)

const (
	maxSkillBytes   = 1 << 20
	maxSkillDepth   = 32
	maxSkillNodes   = 4096
	maxSkillSteps   = 64
	maxSkillTags    = 32
	maxSkillString  = 256
	maxSkillTimeout = uint64(24 * 60 * 60)
)

// InitialStepKinds is the complete initial typed execution vocabulary. The
// planner may reference script.run declaratively; no code path executes it.
var initialStepKinds = map[string]struct{}{
	"target.inspect": {}, "compute.provision": {}, "compute.destroy": {},
	"source.fetch": {}, "artifact.upload": {}, "package.ensure": {},
	"file.put": {}, "container.apply": {}, "systemd.apply": {},
	"script.run": {}, "http.probe": {}, "tcp.probe": {},
	"artifact.collect": {}, "cleanup": {},
}
var planningStepKinds = map[string]struct{}{"analysis.plan": {}, "analysis.project": {}, "analysis.resources": {}, "analysis.placement": {}, "analysis.target": {}, "plan.fragment": {}}
var allowedNetworkHosts = map[string]struct{}{"registry.example": {}, "source.example": {}}

var knownTargetCapabilities = map[string]struct{}{
	"transport.aws_ssm": {}, "target.aws_ec2_instance": {}, "runtime.container": {},
	"runtime.systemd": {}, "probe.http": {}, "probe.tcp": {}, "artifact.fetch": {},
	"artifact.collect": {}, "secret.reference": {}, "compute.catalog": {},
}

type Manifest struct {
	ID                         string          `json:"id"`
	Version                    string          `json:"version"`
	SchemaVersion              string          `json:"schema_version"`
	ContentDigest              string          `json:"content_digest"`
	MinimumCoreVersion         string          `json:"minimum_core_version"`
	InputSchema                json.RawMessage `json:"input_schema,omitempty"`
	OutputSchema               json.RawMessage `json:"output_schema,omitempty"`
	InputSchemaRef             string          `json:"input_schema_ref,omitempty"`
	OutputSchemaRef            string          `json:"output_schema_ref,omitempty"`
	AllowedStepKinds           []string        `json:"allowed_step_kinds"`
	RequiredTargetCapabilities []string        `json:"required_target_capabilities"`
	NetworkAccess              NetworkAccess   `json:"network_access"`
	SecretPurposes             []string        `json:"secret_purposes"`
	IntentTags                 []string        `json:"intent_tags"`
	Steps                      []Step          `json:"steps"`
}

type NetworkAccess struct {
	Allowed   bool     `json:"allowed"`
	Purpose   string   `json:"purpose,omitempty"`
	Endpoints []string `json:"endpoints,omitempty"`
}
type Step struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"`
	IntentTags []string `json:"intent_tags,omitempty"`
	Inputs     []string `json:"inputs,omitempty"`
	Outputs    []string `json:"outputs,omitempty"`
}
type SelectionQuery struct {
	Intent             string
	TargetCapabilities []string
	Limit              int
}
type Pin struct{ ID, Version, ContentDigest string }
type Registry struct{ manifests []Manifest }

func NewRegistry(contents ...[]byte) (*Registry, error) {
	entries := make([]Manifest, 0, len(contents))
	seen := map[string]struct{}{}
	for _, content := range contents {
		m, err := Parse(content)
		if err != nil {
			return nil, err
		}
		key := m.ID + "@" + m.Version
		if _, ok := seen[key]; ok {
			return nil, fmt.Errorf("duplicate manifest %s", key)
		}
		seen[key] = struct{}{}
		entries = append(entries, m)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].ID != entries[j].ID {
			return entries[i].ID < entries[j].ID
		}
		return entries[i].Version < entries[j].Version
	})
	return &Registry{manifests: entries}, nil
}

func (r *Registry) Manifests() []Manifest {
	if r == nil {
		return nil
	}
	out := make([]Manifest, len(r.manifests))
	for i := range r.manifests {
		out[i] = cloneManifest(r.manifests[i])
	}
	return out
}

func (r *Registry) Select(q SelectionQuery) ([]Manifest, error) {
	if r == nil {
		return nil, errors.New("nil registry")
	}
	if strings.TrimSpace(q.Intent) == "" {
		return nil, errors.New("intent is required")
	}
	for _, cap := range q.TargetCapabilities {
		if _, ok := knownTargetCapabilities[cap]; !ok {
			return nil, fmt.Errorf("unknown target capability %q", cap)
		}
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 1
	}
	if limit > 3 {
		limit = 3
	}
	wanted := make(map[string]struct{}, len(q.TargetCapabilities))
	for _, c := range q.TargetCapabilities {
		wanted[c] = struct{}{}
	}
	result := make([]Manifest, 0, limit)
	for _, m := range r.manifests {
		if !contains(m.IntentTags, q.Intent) || !capabilitiesSubset(m.RequiredTargetCapabilities, wanted) {
			continue
		}
		result = append(result, cloneManifest(m))
		if len(result) == limit {
			break
		}
	}
	return result, nil
}
func (r *Registry) SelectByIntent(intent string, limit int) ([]Manifest, error) {
	return r.Select(SelectionQuery{Intent: intent, Limit: limit})
}
func (r *Registry) Resolve(pin Pin) (Manifest, error) {
	if r == nil {
		return Manifest{}, errors.New("nil registry")
	}
	for _, m := range r.manifests {
		if m.ID == pin.ID && m.Version == pin.Version {
			if m.ContentDigest != pin.ContentDigest {
				return Manifest{}, fmt.Errorf("digest mismatch for %s@%s", pin.ID, pin.Version)
			}
			return cloneManifest(m), nil
		}
	}
	return Manifest{}, fmt.Errorf("manifest pin not found: %s@%s", pin.ID, pin.Version)
}
func (r *Registry) ResolveExact(id, version, contentDigest string) (Manifest, error) {
	return r.Resolve(Pin{ID: id, Version: version, ContentDigest: contentDigest})
}

func Parse(content []byte) (Manifest, error) {
	if len(content) > maxSkillBytes {
		return Manifest{}, errors.New("skill manifest exceeds byte cap")
	}
	var raw map[string]interface{}
	dec := json.NewDecoder(bytes.NewReader(content))
	if err := dec.Decode(&raw); err != nil {
		return Manifest{}, fmt.Errorf("manifest JSON: %w", err)
	}
	var trailing interface{}
	if err := dec.Decode(&trailing); err == nil {
		return Manifest{}, errors.New("manifest contains trailing JSON")
	} else if !errors.Is(err, io.EOF) {
		return Manifest{}, fmt.Errorf("manifest trailing data: %w", err)
	}
	if err := rejectUnsafeFields(raw, ""); err != nil {
		return Manifest{}, err
	}
	if _, err := canonicalValue(content); err != nil {
		return Manifest{}, fmt.Errorf("manifest canonical JSON: %w", err)
	}
	var m Manifest
	strict := json.NewDecoder(bytes.NewReader(content))
	strict.DisallowUnknownFields()
	if err := strict.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("manifest shape: %w", err)
	}
	if err := Validate(m); err != nil {
		return Manifest{}, err
	}
	if got := ContentDigest(content); got != m.ContentDigest {
		return Manifest{}, fmt.Errorf("content_digest mismatch: declared %s computed %s", m.ContentDigest, got)
	}
	return m, nil
}

func Validate(m Manifest) error {
	if len(m.ID) > 128 || !canonicalID.MatchString(m.ID) {
		return fmt.Errorf("invalid canonical id %q", m.ID)
	}
	if !semver.MatchString(m.Version) || m.SchemaVersion != "skill/v1" || !semver.MatchString(m.MinimumCoreVersion) {
		return fmt.Errorf("invalid version fields in %s", m.ID)
	}
	if !digest.MatchString(m.ContentDigest) {
		return fmt.Errorf("invalid content digest in %s", m.ID)
	}
	if (len(m.InputSchema) == 0) == (m.InputSchemaRef == "") || (len(m.OutputSchema) == 0) == (m.OutputSchemaRef == "") {
		return fmt.Errorf("%s requires exactly one input/output schema form", m.ID)
	}
	if err := validateInlineSchema(m.InputSchema, "input_schema"); err != nil {
		return fmt.Errorf("%s: %w", m.ID, err)
	}
	if err := validateInlineSchema(m.OutputSchema, "output_schema"); err != nil {
		return fmt.Errorf("%s: %w", m.ID, err)
	}
	if !validSchemaRef(m.InputSchemaRef) || !validSchemaRef(m.OutputSchemaRef) {
		return fmt.Errorf("schema refs must be local in %s", m.ID)
	}
	if !sortedUnique(m.IntentTags) || !sortedUnique(m.AllowedStepKinds) || !sortedUnique(m.RequiredTargetCapabilities) || !sortedUnique(m.SecretPurposes) {
		return fmt.Errorf("unsorted or duplicate metadata in %s", m.ID)
	}
	if len(m.IntentTags) == 0 || len(m.IntentTags) > maxSkillTags || len(m.Steps) == 0 || len(m.Steps) > maxSkillSteps {
		return fmt.Errorf("invalid skill metadata count in %s", m.ID)
	}
	if len(m.AllowedStepKinds) > maxSkillTags || len(m.RequiredTargetCapabilities) > maxSkillTags || len(m.SecretPurposes) > maxSkillTags {
		return fmt.Errorf("too many skill metadata entries in %s", m.ID)
	}
	for _, tag := range m.IntentTags {
		if !identifier.MatchString(tag) {
			return fmt.Errorf("invalid intent tag %q", tag)
		}
	}
	if !m.NetworkAccess.Allowed && (m.NetworkAccess.Purpose != "" || len(m.NetworkAccess.Endpoints) > 0) {
		return fmt.Errorf("disabled network_access must have no metadata in %s", m.ID)
	}
	if m.NetworkAccess.Allowed {
		if !identifier.MatchString(m.NetworkAccess.Purpose) || len(m.NetworkAccess.Endpoints) == 0 || len(m.NetworkAccess.Endpoints) > 16 || !sortedUnique(m.NetworkAccess.Endpoints) {
			return fmt.Errorf("invalid skill network metadata in %s", m.ID)
		}
		for _, endpoint := range m.NetworkAccess.Endpoints {
			if !validEndpoint(endpoint) {
				return fmt.Errorf("invalid skill endpoint %q", endpoint)
			}
		}
	}
	declaredPaths := skillSchemaPaths(m.InputSchema)
	for path := range skillSchemaPaths(m.OutputSchema) {
		declaredPaths[path] = struct{}{}
	}
	for _, endpoint := range m.NetworkAccess.Endpoints {
		if err := validateSkillPlaceholders(endpoint, declaredPaths); err != nil {
			return err
		}
	}
	for _, p := range m.SecretPurposes {
		if !identifier.MatchString(p) {
			return fmt.Errorf("invalid secret purpose %q", p)
		}
	}
	for _, k := range m.AllowedStepKinds {
		if !knownStepKind(k) {
			return fmt.Errorf("unknown step kind %q", k)
		}
	}
	for _, c := range m.RequiredTargetCapabilities {
		if _, ok := knownTargetCapabilities[c]; !ok {
			return fmt.Errorf("unknown target capability %q", c)
		}
	}
	seenIDs := map[string]struct{}{}
	for _, s := range m.Steps {
		if !canonicalID.MatchString(s.ID) || len(s.ID) > maxSkillString {
			return fmt.Errorf("invalid step id %q", s.ID)
		}
		if _, ok := seenIDs[s.ID]; ok {
			return fmt.Errorf("duplicate step id %q", s.ID)
		}
		seenIDs[s.ID] = struct{}{}
		if !knownStepKind(s.Kind) || !contains(m.AllowedStepKinds, s.Kind) {
			return fmt.Errorf("invalid or disallowed step kind %q", s.Kind)
		}
		if !sortedUnique(s.IntentTags) || !sortedUnique(s.Inputs) || !sortedUnique(s.Outputs) {
			return fmt.Errorf("unsorted step metadata %s", s.ID)
		}
		if len(s.IntentTags) > maxSkillTags || len(s.Inputs) > maxSkillTags || len(s.Outputs) > maxSkillTags {
			return fmt.Errorf("too many step references %s", s.ID)
		}
		for _, tag := range s.IntentTags {
			if !identifier.MatchString(tag) {
				return fmt.Errorf("invalid step intent tag %q", tag)
			}
		}
		for _, ref := range append(append([]string{}, s.Inputs...), s.Outputs...) {
			if !identifier.MatchString(ref) {
				if err := validateSkillPlaceholders(ref, declaredPaths); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func knownStepKind(k string) bool {
	if _, ok := initialStepKinds[k]; ok {
		return true
	}
	_, ok := planningStepKinds[k]
	return ok
}
func validateInlineSchema(raw json.RawMessage, field string) error {
	if len(raw) == 0 {
		return nil
	}
	var obj map[string]interface{}
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return fmt.Errorf("%s must be a JSON Schema object", field)
	}
	return validateSchemaDocument(obj)
}
func skillSchemaPaths(raw json.RawMessage) map[string]struct{} {
	out := map[string]struct{}{}
	var root map[string]interface{}
	if json.Unmarshal(raw, &root) != nil {
		return out
	}
	var walk func(map[string]interface{}, string, map[string]bool)
	walk = func(node map[string]interface{}, prefix string, seen map[string]bool) {
		if ref, ok := node["$ref"].(string); ok {
			if seen[ref] {
				return
			}
			if target, ok := resolveSchemaRef(root, ref); ok {
				seen[ref] = true
				walk(target, prefix, seen)
				delete(seen, ref)
			}
		}
		props, _ := node["properties"].(map[string]interface{})
		for key, value := range props {
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			out[path] = struct{}{}
			if child, ok := value.(map[string]interface{}); ok {
				walk(child, path, seen)
			}
		}
	}
	walk(root, "", map[string]bool{})
	return out
}
func validateSkillPlaceholders(value string, paths map[string]struct{}) error {
	for _, match := range embeddedPlaceholderPattern.FindAllStringSubmatch(value, -1) {
		if _, ok := paths[match[1]]; !ok {
			return fmt.Errorf("undeclared skill placeholder %s", match[0])
		}
	}
	return nil
}

var supportedSchemaKeywords = map[string]struct{}{"$ref": {}, "$defs": {}, "definitions": {}, "type": {}, "properties": {}, "items": {}, "required": {}, "additionalProperties": {}, "enum": {}, "const": {}, "minLength": {}, "maxLength": {}, "pattern": {}, "minimum": {}, "maximum": {}}

func validateSchemaDocument(root map[string]interface{}) error {
	return validateSchemaNode(root, root, 0, map[string]bool{})
}

func validateSchemaNode(node map[string]interface{}, root map[string]interface{}, depth int, stack map[string]bool) error {
	if depth > 16 {
		return errors.New("schema depth cap exceeded")
	}
	for key := range node {
		if _, ok := supportedSchemaKeywords[key]; !ok {
			return fmt.Errorf("unsupported schema keyword %q", key)
		}
	}
	if ref, ok := node["$ref"].(string); ok {
		if !validSchemaRef(ref) {
			return errors.New("invalid schema ref")
		}
		target, ok := resolveSchemaRef(root, ref)
		if !ok {
			return errors.New("unresolved schema ref")
		}
		if stack[ref] {
			return errors.New("cyclic schema ref")
		}
		stack[ref] = true
		if err := validateSchemaNode(target, root, depth+1, stack); err != nil {
			return err
		}
		delete(stack, ref)
	}
	if typ, ok := node["type"].(string); ok {
		switch typ {
		case "object", "array", "string", "number", "integer", "boolean", "null":
		default:
			return errors.New("unsupported schema type")
		}
	} else if _, exists := node["type"]; exists {
		return errors.New("schema type must be string")
	}
	for _, defsKey := range []string{"$defs", "definitions"} {
		if defs, ok := node[defsKey].(map[string]interface{}); ok {
			if len(defs) > 128 {
				return errors.New("too many schema definitions")
			}
			for key, value := range defs {
				if !identifier.MatchString(key) {
					return errors.New("invalid schema definition name")
				}
				child, ok := value.(map[string]interface{})
				if !ok {
					return errors.New("schema definition must be object")
				}
				if err := validateSchemaNode(child, root, depth+1, stack); err != nil {
					return err
				}
			}
		} else if _, exists := node[defsKey]; exists {
			return errors.New("schema definitions must be object")
		}
	}
	if props, ok := node["properties"].(map[string]interface{}); ok {
		if len(props) > 128 {
			return errors.New("too many schema properties")
		}
		for key, v := range props {
			if !identifier.MatchString(key) {
				return errors.New("invalid schema property")
			}
			child, ok := v.(map[string]interface{})
			if !ok {
				return errors.New("schema property must be object")
			}
			if err := validateSchemaNode(child, root, depth+1, stack); err != nil {
				return err
			}
		}
	} else if _, exists := node["properties"]; exists {
		return errors.New("schema properties must be object")
	}
	if items, ok := node["items"].(map[string]interface{}); ok {
		if err := validateSchemaNode(items, root, depth+1, stack); err != nil {
			return err
		}
	} else if _, exists := node["items"]; exists {
		return errors.New("schema items must be object")
	}
	if req, ok := node["required"].([]interface{}); ok {
		props, _ := node["properties"].(map[string]interface{})
		seen := map[string]bool{}
		for _, v := range req {
			s, ok := v.(string)
			if !ok || !identifier.MatchString(s) || seen[s] {
				return errors.New("invalid schema required")
			}
			seen[s] = true
			if props != nil {
				if _, exists := props[s]; !exists {
					return fmt.Errorf("required schema property %q is not declared", s)
				}
			}
		}
	} else if _, exists := node["required"]; exists {
		return errors.New("schema required must be array")
	}
	if additional, ok := node["additionalProperties"]; ok {
		switch value := additional.(type) {
		case bool:
		case map[string]interface{}:
			if err := validateSchemaNode(value, root, depth+1, stack); err != nil {
				return err
			}
		default:
			return errors.New("schema additionalProperties must be boolean or object")
		}
	}
	if enum, ok := node["enum"].([]interface{}); ok {
		if len(enum) == 0 {
			return errors.New("schema enum must not be empty")
		}
	} else if _, exists := node["enum"]; exists {
		return errors.New("schema enum must be array")
	}
	for _, key := range []string{"minLength", "maxLength", "minimum", "maximum"} {
		if value, ok := node[key]; ok {
			if _, ok := value.(float64); !ok {
				return fmt.Errorf("schema %s must be number", key)
			}
		}
	}
	if value, ok := node["pattern"]; ok {
		pattern, ok := value.(string)
		if !ok || len(pattern) > 256 {
			return errors.New("schema pattern must be string")
		}
		if _, err := regexp.Compile(pattern); err != nil {
			return errors.New("invalid schema pattern")
		}
	}
	return nil
}

func resolveSchemaRef(root map[string]interface{}, ref string) (map[string]interface{}, bool) {
	if !strings.HasPrefix(ref, "#/") {
		return nil, false
	}
	current := interface{}(root)
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		obj, ok := current.(map[string]interface{})
		if !ok {
			return nil, false
		}
		current, ok = obj[part]
		if !ok {
			return nil, false
		}
	}
	result, ok := current.(map[string]interface{})
	return result, ok
}
func validSchemaRef(ref string) bool {
	if ref == "" {
		return true
	}
	if len(ref) > maxSkillString || !strings.HasPrefix(ref, "#/") || strings.Contains(ref, "\\") || strings.Contains(ref, "..") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(ref, "#/"), "/")
	if len(parts) != 2 || (parts[0] != "$defs" && parts[0] != "definitions") || !identifier.MatchString(parts[1]) {
		return false
	}
	for _, r := range ref {
		if r < 0x20 || r == 0x7f || r == '#' {
			return false
		}
	}
	return true
}
func validEndpoint(endpoint string) bool {
	if len(endpoint) > maxSkillString || strings.ContainsAny(endpoint, "*?") || !endpointPattern.MatchString(endpoint) || !strings.Contains(endpoint, "${") {
		return false
	}
	host := strings.TrimPrefix(endpoint, "https://")
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	if _, ok := allowedNetworkHosts[host]; !ok {
		return false
	}
	for i := 0; i < len(endpoint); {
		start := strings.Index(endpoint[i:], "${")
		if start < 0 {
			break
		}
		start += i
		end := strings.IndexByte(endpoint[start:], '}')
		if end < 0 || !identifier.MatchString(endpoint[start+2:start+end]) {
			return false
		}
		i = start + end + 1
	}
	return true
}
func ContentDigest(content []byte) string {
	value, err := canonicalValue(content)
	if err != nil {
		return ""
	}
	raw, ok := value.(map[string]interface{})
	if !ok {
		return ""
	}
	delete(raw, "content_digest")
	canonical, err := json.Marshal(raw)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

func canonicalValue(content []byte) (interface{}, error) {
	dec := json.NewDecoder(bytes.NewReader(content))
	dec.UseNumber()
	value, err := readCanonicalValue(dec)
	if err != nil {
		return nil, err
	}
	if err := checkJSONLimits(value, 0, new(int)); err != nil {
		return nil, err
	}
	var trailing interface{}
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("trailing JSON")
		}
		return nil, err
	}
	return value, nil
}
func checkJSONLimits(value interface{}, depth int, nodes *int) error {
	if depth > maxSkillDepth {
		return errors.New("JSON depth cap exceeded")
	}
	*nodes = *nodes + 1
	if *nodes > maxSkillNodes {
		return errors.New("JSON node cap exceeded")
	}
	switch v := value.(type) {
	case map[string]interface{}:
		for _, child := range v {
			if err := checkJSONLimits(child, depth+1, nodes); err != nil {
				return err
			}
		}
	case []interface{}:
		for _, child := range v {
			if err := checkJSONLimits(child, depth+1, nodes); err != nil {
				return err
			}
		}
	}
	return nil
}
func readCanonicalValue(dec *json.Decoder) (interface{}, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch delim := tok.(type) {
	case json.Delim:
		switch delim {
		case '{':
			m := map[string]interface{}{}
			for dec.More() {
				keyTok, e := dec.Token()
				if e != nil {
					return nil, e
				}
				key := keyTok.(string)
				if _, exists := m[key]; exists {
					return nil, fmt.Errorf("duplicate key %q", key)
				}
				v, e := readCanonicalValue(dec)
				if e != nil {
					return nil, e
				}
				m[key] = v
			}
			if _, e := dec.Token(); e != nil {
				return nil, e
			}
			return m, nil
		case '[':
			a := []interface{}{}
			for dec.More() {
				v, e := readCanonicalValue(dec)
				if e != nil {
					return nil, e
				}
				a = append(a, v)
			}
			if _, e := dec.Token(); e != nil {
				return nil, e
			}
			return a, nil
		}
	}
	return tok, nil
}
func cloneManifest(m Manifest) Manifest {
	b, _ := json.Marshal(m)
	var c Manifest
	_ = json.Unmarshal(b, &c)
	return c
}
func contains(values []string, wanted string) bool {
	for _, v := range values {
		if v == wanted {
			return true
		}
	}
	return false
}
func sortedUnique(values []string) bool {
	return sort.StringsAreSorted(values) && len(values) == uniqueCount(values)
}
func uniqueCount(values []string) int {
	seen := map[string]struct{}{}
	for _, v := range values {
		seen[v] = struct{}{}
	}
	return len(seen)
}
func capabilitiesSubset(required []string, available map[string]struct{}) bool {
	for _, c := range required {
		if _, ok := available[c]; !ok {
			return false
		}
	}
	return true
}

var prohibitedFieldNames = map[string]struct{}{"command": {}, "commands": {}, "shell": {}, "runtime": {}, "executable": {}, "exec": {}, "entrypoint": {}, "script": {}, "local_execution": {}}

func rejectUnsafeFields(value interface{}, path string) error {
	return rejectUnsafeFieldsAt(value, path, false)
}
func rejectUnsafeFieldsAt(value interface{}, path string, inNetwork bool) error {
	switch v := value.(type) {
	case map[string]interface{}:
		for key, child := range v {
			lower := strings.ToLower(key)
			if _, bad := prohibitedFieldNames[lower]; bad {
				return fmt.Errorf("prohibited execution field %q", path+key)
			}
			childNetwork := inNetwork || lower == "network_access"
			if s, ok := child.(string); ok && strings.Contains(s, "://") && !childNetwork {
				return fmt.Errorf("URL is only allowed in network_access metadata: %s", path+key)
			}
			if err := rejectUnsafeFieldsAt(child, path+key+".", childNetwork); err != nil {
				return err
			}
		}
	case []interface{}:
		for i, child := range v {
			if s, ok := child.(string); ok && strings.Contains(s, "://") && !inNetwork {
				return fmt.Errorf("URL is only allowed in network_access metadata: %s", path)
			}
			if err := rejectUnsafeFieldsAt(child, fmt.Sprintf("%s[%d].", path, i), inNetwork); err != nil {
				return err
			}
		}
	case string:
		if strings.Contains(v, "://") && !inNetwork {
			return fmt.Errorf("URL is only allowed in network_access metadata: %s", path)
		}
	}
	return nil
}
