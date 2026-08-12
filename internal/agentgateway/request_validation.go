package agentgateway

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"mime"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
	"github.com/google/uuid"
	"golang.org/x/text/language"
)

// ErrInvalidActionRequest marks ProductCore parameters that cannot be sent to
// the external Agent capability. It is intentionally separate from a
// capability INVALID_ARGUMENT response: these failures are rejected before a
// catalog lookup, grant, or operation is created.
var ErrInvalidActionRequest = errors.New("native agent action request is invalid")

// Keep the request-side key policy aligned with the service-binding output
// sanitizer. Chat input is not scanned for secret-looking string values, but
// a key that names a credential or bearer must never cross the gateway.
var catalogSensitiveKeyRE = regexp.MustCompile(`(?i)(^|[^a-z0-9])(access[_-]?token|refresh[_-]?token|client[_-]?secret|authorization|headers?|cookies?|bearer|basic|secret|token|pass(?:word|wd|phrase)|credential|api[_-]?key|private[_-]?key)([^a-z0-9]|$)`)
var extensionExactSemverRE = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
var extensionNPMNamePartRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

var maxInt64Rat = new(big.Rat).SetInt64(math.MaxInt64)

const (
	maxChatMessageBytes              = 1 << 20
	maxChatAttachmentBytes           = int64(8 << 20)
	maxChatAttachmentChunkBytes      = int64(1 << 20)
	maxChatExtensions                = 64
	maxChatExtensionVersionBytes     = 256
	maxChatExtensionToolNameBytes    = 256
	maxChatAttachmentNameBytes       = 255
	maxChatAttachments               = 4
	maxCloudWorkerArtifactBytes      = int64(8 << 20)
	maxCloudWorkerArtifactChunkBytes = int64(512 << 10)
	maxTextToolNameBytes             = 64
	maxTextToolPromptBytes           = 16 << 10
	maxSelectedTextBytes             = 64 << 10
	maxTextTools                     = 32
	maxEnabledTextTools              = 6
)

// InvalidActionRequestError carries only a field name and a stable reason. It
// never includes the value of a write-only field (for example api_key).
type InvalidActionRequestError struct {
	Action string
	Field  string
	Reason string
}

func (e *InvalidActionRequestError) Error() string {
	if e == nil {
		return ErrInvalidActionRequest.Error()
	}
	action := strings.TrimSpace(e.Action)
	field := strings.TrimSpace(e.Field)
	reason := strings.TrimSpace(e.Reason)
	if action == "" {
		action = "native agent action"
	}
	if field == "" {
		field = "request"
	}
	if reason == "" {
		reason = "is invalid"
	}
	return fmt.Sprintf("%s %s: %s", action, field, reason)
}

func (e *InvalidActionRequestError) Unwrap() error { return ErrInvalidActionRequest }

func invalidActionRequest(action, field, reason string) error {
	return &InvalidActionRequestError{Action: strings.TrimSpace(action), Field: field, Reason: reason}
}

// ValidateActionRequest enforces the request-side parts of the frozen Native
// Agent ProductCore contract. It is safe to call from both the HTTP/WS module
// and the capability gateway; the validation is side-effect free.
func ValidateActionRequest(action string, params map[string]any) error {
	action = strings.TrimSpace(action)
	switch action {
	case "agent.config.get":
		return rejectUnknownActionFields(action, params)
	case "agent.config.update":
		return validateAgentConfigUpdateRequest(action, params)
	case "agent.chat", "agent.chat.stream":
		return validateChatRequest(action, params)
	case "agent.chat.attachment.begin":
		return validateChatAttachmentBeginRequest(action, params)
	case "agent.chat.attachment.append":
		return validateChatAttachmentAppendRequest(action, params)
	case "agent.chat.attachment.commit":
		return validateChatAttachmentCommitRequest(action, params)
	case "agent.models.list":
		return validateModelCatalogRequest(action, params)
	case "agent.model_profiles.sync":
		return validateModelProfileSyncRequest(action, params)
	case "agent.chat.turn.stop":
		return validateTurnStopRequest(action, params)
	case "agent.chat.turn.steer":
		return validateTurnSteerRequest(action, params)
	case "agent.chat.turns.list":
		return validateTurnsListRequest(action, params)
	case "agent.core.mcp.discover":
		return validateCoreExtensionDiscoverRequest(action, params, "mcp")
	case "agent.core.mcp.get", "agent.core.skills.get":
		return validateCoreExtensionGetRequest(action, params)
	case "agent.core.mcp.list":
		return validateCoreExtensionListRequest(action, params, "mcp")
	case "agent.core.mcp.inspect":
		return validateCoreExtensionInspectRequest(action, params, "mcp")
	case "agent.core.mcp.install", "agent.core.mcp.update":
		return validateCoreExtensionMutationRequest(action, params, "mcp")
	case "agent.core.skills.discover":
		return validateCoreExtensionDiscoverRequest(action, params, "skill")
	case "agent.core.skills.list":
		return validateCoreExtensionListRequest(action, params, "skill")
	case "agent.core.skills.inspect":
		return validateCoreExtensionInspectRequest(action, params, "skill")
	case "agent.core.skills.install", "agent.core.skills.update":
		return validateCoreExtensionMutationRequest(action, params, "skill")
	case "agent.core.mcp.remove", "agent.core.skills.remove":
		return validateCoreExtensionRemoveRequest(action, params)
	case "agent.execution.v2.artifacts.download":
		return validateCloudWorkerArtifactDownloadRequest(action, params)
	case "agent.text_tools.config.get":
		return rejectUnknownActionFields(action, params)
	case "agent.memory.config.get", "agent.memory.status":
		return rejectUnknownActionFields(action, params)
	case "agent.memory.config.update":
		return validateMemoryConfigUpdateRequest(action, params)
	case "agent.memory.facts.update":
		return validateMemoryFactUpdateRequest(action, params)
	case "agent.memory.facts.delete":
		return validateMemoryFactDeleteRequest(action, params)
	case "agent.text_tools.config.update":
		return validateTextToolsConfigUpdateRequest(action, params)
	case "agent.text_tools.execute":
		return validateTextToolsExecuteRequest(action, params)
	case "agent.image_tools.upload.begin":
		return validateImageToolUploadBeginRequest(action, params)
	case "agent.image_tools.upload.append":
		return validateChatAttachmentAppendRequest(action, params)
	case "agent.image_tools.upload.commit":
		return validateChatAttachmentCommitRequest(action, params)
	case "agent.image_tools.extract_text":
		return validateImageToolExecuteRequest(action, params, false)
	case "agent.image_tools.translate_text":
		return validateImageToolExecuteRequest(action, params, true)
	default:
		return nil
	}
}

func validateAgentConfigUpdateRequest(action string, params map[string]any) error {
	if err := rejectUnknownActionFields(action, params,
		"operation_id", "idempotency_key", "expected_revision", "native_agent_identity",
		"online_agent_identity", "enabled", "mcp_blocked_room_ids"); err != nil {
		return err
	}
	for _, field := range []string{"operation_id", "idempotency_key"} {
		if value, present := params[field]; present && !canonicalActionUUID(value) {
			return invalidActionRequest(action, field, "must be a canonical UUID")
		}
	}
	if value, present := params["expected_revision"]; present && !actionIntegerInRange(value, 0, math.MaxInt64) {
		return invalidActionRequest(action, "expected_revision", "must be a non-negative integer")
	}
	for _, field := range []string{"native_agent_identity", "online_agent_identity"} {
		value, present := params[field]
		if !present {
			continue
		}
		identity, ok := value.(map[string]any)
		if !ok {
			return invalidActionRequest(action, field, "must be an object")
		}
		if err := rejectUnknownActionFields(action, identity, "display_name", "avatar_url"); err != nil {
			return invalidActionRequest(action, field, "contains an unsupported field")
		}
		for _, key := range []string{"display_name", "avatar_url"} {
			if raw, ok := identity[key]; ok {
				text, ok := raw.(string)
				if !ok || text != strings.TrimSpace(text) {
					return invalidActionRequest(action, field+"."+key, "must be a trimmed string")
				}
			}
		}
	}
	if value, present := params["enabled"]; present {
		if _, ok := value.(bool); !ok {
			return invalidActionRequest(action, "enabled", "must be a boolean")
		}
	}
	if value, present := params["mcp_blocked_room_ids"]; present {
		rooms, ok := value.([]any)
		if !ok {
			if typed, typedOK := value.([]string); typedOK {
				rooms = make([]any, len(typed))
				for index := range typed {
					rooms[index] = typed[index]
				}
			} else {
				return invalidActionRequest(action, "mcp_blocked_room_ids", "must be an array of strings")
			}
		}
		if len(rooms) > 512 {
			return invalidActionRequest(action, "mcp_blocked_room_ids", "contains too many room IDs")
		}
		for _, room := range rooms {
			text, ok := room.(string)
			if !ok || text != strings.TrimSpace(text) || len(text) > 256 {
				return invalidActionRequest(action, "mcp_blocked_room_ids", "must contain bounded trimmed strings")
			}
		}
	}
	return nil
}

func validateMemoryFactUpdateRequest(action string, params map[string]any) error {
	if err := validateMemoryFactDeleteRequest(action, params, "value"); err != nil {
		return err
	}
	value, ok := params["value"].(string)
	if !ok || value != strings.TrimSpace(value) || value == "" || utf8.RuneCountInString(value) > 2048 {
		return invalidActionRequest(action, "value", "must be a non-empty string no longer than 2048 bytes")
	}
	return nil
}

func validateMemoryFactDeleteRequest(action string, params map[string]any, extra ...string) error {
	fields := append([]string{"fact_id", "idempotency_key"}, extra...)
	if err := rejectUnknownActionFields(action, params, fields...); err != nil {
		return err
	}
	if !canonicalActionUUID(params["fact_id"]) || !canonicalActionUUID(params["idempotency_key"]) {
		return invalidActionRequest(action, "fact_id", "must include canonical fact_id and idempotency_key UUIDs")
	}
	return nil
}

func validateMemoryConfigUpdateRequest(action string, params map[string]any) error {
	if err := rejectUnknownActionFields(action, params, "idempotency_key", "expected_revision", "enabled"); err != nil {
		return err
	}
	if !canonicalActionUUID(params["idempotency_key"]) || !actionIntegerInRange(params["expected_revision"], 0, math.MaxInt64) {
		return invalidActionRequest(action, "idempotency_key", "must include a canonical UUID and non-negative expected_revision")
	}
	if _, ok := params["enabled"].(bool); !ok {
		return invalidActionRequest(action, "enabled", "must be a boolean")
	}
	return nil
}

func validateCoreExtensionDiscoverRequest(action string, params map[string]any, kind string) error {
	if err := rejectUnknownActionFields(action, params, "source", "query", "page_size", "page_token"); err != nil {
		return err
	}
	source := ""
	if rawSource, present := params["source"]; present {
		var ok bool
		source, ok = rawSource.(string)
		if !ok || source == "" {
			return invalidActionRequest(action, "source", "must be a non-empty string")
		}
	}
	if source == "" {
		if kind == "skill" {
			source = "builtin"
		} else {
			source = "official_registry"
		}
	}
	if !validCoreExtensionSource(kind, source) {
		return invalidActionRequest(action, "source", "must match the action family")
	}
	query := ""
	if rawQuery, present := params["query"]; present {
		var ok bool
		query, ok = rawQuery.(string)
		if !ok {
			return invalidActionRequest(action, "query", "must be a string")
		}
	}
	if source == "npm" && strings.TrimSpace(query) == "" {
		return invalidActionRequest(action, "query", "is required for npm discovery")
	}
	if pageSize, present := params["page_size"]; present {
		maximum := int64(100)
		if source == "npm" {
			maximum = 10
		}
		if !actionIntegerInRange(pageSize, 0, maximum) {
			return invalidActionRequest(action, "page_size", "is outside the source limit")
		}
	}
	if pageToken, present := params["page_token"]; present {
		if token, ok := pageToken.(string); !ok || len(token) > 4096 {
			return invalidActionRequest(action, "page_token", "must be a bounded string")
		}
	}
	return nil
}

func validateCoreExtensionGetRequest(action string, params map[string]any) error {
	if err := rejectUnknownActionFields(action, params, "installation_id"); err != nil {
		return err
	}
	if !canonicalActionUUID(params["installation_id"]) {
		return invalidActionRequest(action, "installation_id", "must be a canonical UUID")
	}
	return nil
}

func validateCoreExtensionListRequest(action string, params map[string]any, kind string) error {
	if err := rejectUnknownActionFields(action, params, "source", "state", "page_size", "page_token"); err != nil {
		return err
	}
	if source, present := params["source"]; present {
		value, ok := source.(string)
		if !ok || value == "" || !validCoreExtensionSource(kind, value) {
			return invalidActionRequest(action, "source", "must match the action family")
		}
	}
	if state, present := params["state"]; present {
		value, ok := state.(string)
		if !ok || !oneOfString(value, "draft", "installing", "installed", "updating", "uninstalling", "removed", "failed") {
			return invalidActionRequest(action, "state", "must be a current extension state")
		}
	}
	if pageSize, present := params["page_size"]; present && !actionIntegerInRange(pageSize, 0, 100) {
		return invalidActionRequest(action, "page_size", "is outside the source limit")
	}
	if pageToken, present := params["page_token"]; present {
		if token, ok := pageToken.(string); !ok || len(token) > 4096 {
			return invalidActionRequest(action, "page_token", "must be a bounded string")
		}
	}
	return nil
}

func validateCoreExtensionInspectRequest(action string, params map[string]any, kind string) error {
	if err := rejectUnknownActionFields(action, params, "candidate"); err != nil {
		return err
	}
	_, err := validateCoreExtensionCandidate(action, params["candidate"], kind)
	return err
}

func validateCoreExtensionMutationRequest(action string, params map[string]any, kind string) error {
	fields := []string{"idempotency_key", "candidate", "inspection", "secret_inputs"}
	if strings.HasSuffix(action, ".update") {
		fields = append(fields, "installation_id", "expected_revision")
	}
	if err := rejectUnknownActionFields(action, params, fields...); err != nil {
		return err
	}
	if !canonicalActionUUID(params["idempotency_key"]) {
		return invalidActionRequest(action, "idempotency_key", "must be a canonical UUID")
	}
	if strings.HasSuffix(action, ".update") && (!canonicalActionUUID(params["installation_id"]) || !actionIntegerInRange(params["expected_revision"], 1, math.MaxInt64)) {
		return invalidActionRequest(action, "installation_id", "and expected_revision must identify the current installation")
	}
	candidate, err := validateCoreExtensionCandidate(action, params["candidate"], kind)
	if err != nil {
		return err
	}
	inspection, ok := params["inspection"].(map[string]any)
	if !ok {
		return invalidActionRequest(action, "inspection", "must be an object")
	}
	if err := rejectUnknownActionFields(action, inspection, "candidate", "content_digest", "manifest_digest", "execution_digest", "network_schema_digest", "secret_schema_digest", "execution", "network_grants", "secret_grants"); err != nil {
		return err
	}
	inspectionCandidate, err := validateCoreExtensionCandidate(action, inspection["candidate"], kind)
	if err != nil || !reflect.DeepEqual(candidate, inspectionCandidate) {
		return invalidActionRequest(action, "inspection.candidate", "must exactly match candidate")
	}
	for _, field := range []string{"content_digest", "manifest_digest", "execution_digest", "network_schema_digest", "secret_schema_digest"} {
		if !canonicalActionSHA256(inspection[field]) {
			return invalidActionRequest(action, "inspection."+field, "must be a lowercase SHA-256 digest")
		}
	}
	if _, ok := inspection["network_grants"].([]any); !ok {
		return invalidActionRequest(action, "inspection.network_grants", "must be an array")
	}
	if _, ok := inspection["secret_grants"].([]any); !ok {
		return invalidActionRequest(action, "inspection.secret_grants", "must be an array")
	}
	switch candidate["transport"] {
	case "stdio_node":
		if err := validateCoreMCPNodeExecution(action, inspection["execution"]); err != nil {
			return err
		}
		if !emptyActionArray(inspection["network_grants"]) || !emptyActionArray(inspection["secret_grants"]) {
			return invalidActionRequest(action, "inspection", "managed Node MCP must not request network or secret grants")
		}
		if !emptyActionArray(params["secret_inputs"]) {
			return invalidActionRequest(action, "secret_inputs", "managed Node MCP must not accept client secret material")
		}
	case "stdio_static":
		if !coreExtensionExecutionBranch(inspection["execution"], "stdio") {
			return invalidActionRequest(action, "inspection.execution", "stdio_static requires exactly one stdio branch")
		}
	case "streamable_http":
		if !coreExtensionExecutionBranch(inspection["execution"], "remote") {
			return invalidActionRequest(action, "inspection.execution", "streamable_http requires exactly one remote branch")
		}
	case "skill_static":
		if err := validateCoreSkillExecution(action, inspection["execution"]); err != nil {
			return err
		}
	}
	if secretInputs, present := params["secret_inputs"]; present {
		if _, ok := secretInputs.([]any); !ok {
			return invalidActionRequest(action, "secret_inputs", "must be an array")
		}
	}
	return nil
}

func validateCoreExtensionRemoveRequest(action string, params map[string]any) error {
	if err := rejectUnknownActionFields(action, params, "idempotency_key", "installation_id", "expected_revision"); err != nil {
		return err
	}
	if !canonicalActionUUID(params["idempotency_key"]) || !canonicalActionUUID(params["installation_id"]) || !actionIntegerInRange(params["expected_revision"], 1, math.MaxInt64) {
		return invalidActionRequest(action, "installation_id", "must include a canonical idempotency key, installation id, and positive expected_revision")
	}
	return nil
}

func validateCoreExtensionCandidate(action string, raw any, kind string) (map[string]any, error) {
	candidate, ok := raw.(map[string]any)
	if !ok {
		return nil, invalidActionRequest(action, "candidate", "must be an object")
	}
	if err := rejectUnknownActionFields(action, candidate, "id", "kind", "source", "name", "description", "pin", "transport"); err != nil {
		return nil, err
	}
	if candidate["kind"] != kind {
		return nil, invalidActionRequest(action, "candidate.kind", "must match the action family")
	}
	source, sourceOK := candidate["source"].(string)
	transport, transportOK := candidate["transport"].(string)
	id, idOK := candidate["id"].(string)
	name, nameOK := candidate["name"].(string)
	if !sourceOK || !transportOK || !idOK || !nameOK || id == "" || name == "" || !validCoreExtensionSource(kind, source) || !validCoreExtensionTransport(kind, transport) {
		return nil, invalidActionRequest(action, "candidate", "has an invalid identity, source, or transport for the action family")
	}
	if description, present := candidate["description"]; present {
		if _, ok := description.(string); !ok {
			return nil, invalidActionRequest(action, "candidate.description", "must be a string")
		}
	}
	if source == "npm" && (transport != "stdio_node" || !validExtensionNPMPackageName(id)) {
		return nil, invalidActionRequest(action, "candidate", "npm requires a canonical package id and stdio_node")
	}
	if transport == "stdio_node" && source != "npm" && source != "github" {
		return nil, invalidActionRequest(action, "candidate.transport", "stdio_node requires npm or github")
	}
	pin, ok := candidate["pin"].(map[string]any)
	if !ok {
		return nil, invalidActionRequest(action, "candidate.pin", "must be an object")
	}
	if err := rejectUnknownActionFields(action, pin, "registry_version", "registry_sha256", "git_commit", "git_sha256"); err != nil {
		return nil, err
	}
	if source == "github" {
		commit, commitOK := pin["git_commit"].(string)
		if !commitOK || len(commit) != 40 || commit != strings.ToLower(commit) || !hexString(commit) || !canonicalActionSHA256(pin["git_sha256"]) || pin["registry_version"] != nil || pin["registry_sha256"] != nil {
			return nil, invalidActionRequest(action, "candidate.pin", "github requires only git_commit and git_sha256")
		}
	} else {
		version, versionOK := pin["registry_version"].(string)
		if !versionOK || strings.TrimSpace(version) == "" || strings.EqualFold(version, "latest") || !canonicalActionSHA256(pin["registry_sha256"]) || pin["git_commit"] != nil || pin["git_sha256"] != nil {
			return nil, invalidActionRequest(action, "candidate.pin", "registry source requires only registry_version and registry_sha256")
		}
		if source == "npm" && !validExtensionExactSemver(version) {
			return nil, invalidActionRequest(action, "candidate.pin.registry_version", "npm requires an exact semantic version")
		}
	}
	return candidate, nil
}

func validCoreExtensionSource(kind, source string) bool {
	if kind == "skill" {
		return oneOfString(source, "builtin", "skills_sh", "github")
	}
	return kind == "mcp" && oneOfString(source, "official_registry", "smithery", "glama", "github", "npm")
}

func validCoreExtensionTransport(kind, transport string) bool {
	if kind == "skill" {
		return transport == "skill_static"
	}
	return kind == "mcp" && oneOfString(transport, "stdio_static", "streamable_http", "stdio_node")
}

func coreExtensionExecutionBranch(raw any, branch string) bool {
	execution, ok := raw.(map[string]any)
	if !ok || len(execution) != 1 {
		return false
	}
	_, ok = execution[branch].(map[string]any)
	return ok
}

func validateCoreSkillExecution(action string, raw any) error {
	execution, ok := raw.(map[string]any)
	if !ok || len(execution) != 1 {
		return invalidActionRequest(action, "inspection.execution", "skill_static requires exactly one skill branch")
	}
	skill, ok := execution["skill"].(map[string]any)
	if !ok {
		return invalidActionRequest(action, "inspection.execution.skill", "is required for skill_static")
	}
	if err := rejectUnknownActionFields(action, skill, "relative_path", "digest", "executable", "argv"); err != nil {
		return err
	}
	path, pathOK := skill["relative_path"].(string)
	if !pathOK || !validCoreExtensionRelativePath(path) || !canonicalActionSHA256(skill["digest"]) {
		return invalidActionRequest(action, "inspection.execution.skill", "must be a canonical Skill entry")
	}
	executable, executablePresent := skill["executable"]
	isExecutable := false
	if executablePresent {
		var ok bool
		isExecutable, ok = executable.(bool)
		if !ok {
			return invalidActionRequest(action, "inspection.execution.skill.executable", "must be a boolean")
		}
	}
	argv := []any(nil)
	if rawArgv, present := skill["argv"]; present {
		var ok bool
		argv, ok = rawArgv.([]any)
		if !ok {
			return invalidActionRequest(action, "inspection.execution.skill.argv", "must be an array")
		}
	}
	if !isExecutable && len(argv) != 0 || isExecutable && (path != "entry" || len(argv) == 0 || len(argv) > 128) {
		return invalidActionRequest(action, "inspection.execution.skill", "has an invalid executable entry")
	}
	for _, rawArg := range argv {
		arg, ok := rawArg.(string)
		if !ok || len(arg) > 16<<10 || strings.IndexByte(arg, 0) >= 0 {
			return invalidActionRequest(action, "inspection.execution.skill.argv", "contains an invalid argument")
		}
	}
	return nil
}

func validCoreExtensionRelativePath(path string) bool {
	return path != "" && !strings.HasPrefix(path, "/") && !strings.Contains(path, "..") && !strings.ContainsAny(path, "\\\x00\r\n")
}

func validateCoreMCPNodeExecution(action string, raw any) error {
	execution, ok := raw.(map[string]any)
	if !ok || len(execution) != 1 {
		return invalidActionRequest(action, "inspection.execution", "stdio_node requires exactly one stdio branch")
	}
	stdio, ok := execution["stdio"].(map[string]any)
	if !ok {
		return invalidActionRequest(action, "inspection.execution.stdio", "is required for stdio_node")
	}
	if err := rejectUnknownActionFields(action, stdio, "relative_path", "digest", "argv", "runtime"); err != nil {
		return err
	}
	path, pathOK := stdio["relative_path"].(string)
	if !pathOK || !validCoreExtensionRelativePath(path) || !canonicalActionSHA256(stdio["digest"]) || stdio["runtime"] != "node" {
		return invalidActionRequest(action, "inspection.execution.stdio", "must be a canonical managed Node entry")
	}
	argv, ok := stdio["argv"].([]any)
	if !ok || len(argv) > 128 {
		return invalidActionRequest(action, "inspection.execution.stdio.argv", "must be an array of at most 128 strings")
	}
	for _, rawArg := range argv {
		arg, ok := rawArg.(string)
		if !ok || arg == "" || len(arg) > 16<<10 || strings.ContainsAny(arg, "\x00\r\n") {
			return invalidActionRequest(action, "inspection.execution.stdio.argv", "contains an invalid argument")
		}
	}
	return nil
}

func validExtensionNPMPackageName(value string) bool {
	if value == "" || len(value) > 214 || value != strings.ToLower(value) || strings.Contains(value, "..") {
		return false
	}
	if strings.HasPrefix(value, "@") {
		parts := strings.Split(value[1:], "/")
		return len(parts) == 2 && extensionNPMNamePartRE.MatchString(parts[0]) && extensionNPMNamePartRE.MatchString(parts[1])
	}
	return !strings.Contains(value, "/") && extensionNPMNamePartRE.MatchString(value)
}

func validExtensionExactSemver(value string) bool {
	match := extensionExactSemverRE.FindStringSubmatch(value)
	if match == nil {
		return false
	}
	if match[4] == "" {
		return true
	}
	for _, identifier := range strings.Split(match[4], ".") {
		if len(identifier) <= 1 || identifier[0] != '0' {
			continue
		}
		numeric := true
		for _, char := range identifier {
			if char < '0' || char > '9' {
				numeric = false
				break
			}
		}
		if numeric {
			return false
		}
	}
	return true
}

func oneOfString(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func emptyActionArray(value any) bool {
	if value == nil {
		return true
	}
	array, ok := value.([]any)
	return ok && len(array) == 0
}

func hexString(value string) bool {
	_, err := hex.DecodeString(value)
	return err == nil
}

func validateImageToolUploadBeginRequest(action string, params map[string]any) error {
	if err := rejectUnknownActionFields(action, params,
		"idempotency_key", "image_request_id", "name", "mime_type", "declared_size", "content_sha256"); err != nil {
		return err
	}
	for _, field := range []string{"idempotency_key", "image_request_id"} {
		if !canonicalActionUUID(params[field]) {
			return invalidActionRequest(action, field, "must be a canonical UUID")
		}
	}
	if !validChatAttachmentName(params["name"]) {
		return invalidActionRequest(action, "name", "must be a 1 to 255 byte basename")
	}
	if !validImageToolMIME(params["mime_type"]) {
		return invalidActionRequest(action, "mime_type", "must be image/jpeg, image/png, or image/webp")
	}
	if !actionIntegerInRange(params["declared_size"], 1, maxChatAttachmentBytes) {
		return invalidActionRequest(action, "declared_size", "must be an integer from 1 to 8388608")
	}
	if !canonicalActionSHA256(params["content_sha256"]) {
		return invalidActionRequest(action, "content_sha256", "must be a lowercase SHA-256 digest")
	}
	return nil
}

func validateImageToolExecuteRequest(action string, params map[string]any, translate bool) error {
	allowed := []string{"idempotency_key", "source_id", "source_revision"}
	if translate {
		allowed = append(allowed, "target_locale")
	}
	if err := rejectUnknownActionFields(action, params, allowed...); err != nil {
		return err
	}
	for _, field := range []string{"idempotency_key", "source_id"} {
		if !canonicalActionUUID(params[field]) {
			return invalidActionRequest(action, field, "must be a canonical UUID")
		}
	}
	if !actionIntegerInRange(params["source_revision"], 1, 1) {
		return invalidActionRequest(action, "source_revision", "must be exactly 1")
	}
	if translate && !canonicalBCP47Locale(params["target_locale"]) {
		return invalidActionRequest(action, "target_locale", "must be a canonical BCP-47 language tag")
	}
	return nil
}

func validImageToolMIME(value any) bool {
	mimeType, ok := value.(string)
	if !ok || mimeType != strings.TrimSpace(mimeType) {
		return false
	}
	return mimeType == "image/jpeg" || mimeType == "image/png" || mimeType == "image/webp"
}

func canonicalBCP47Locale(value any) bool {
	text, ok := value.(string)
	if !ok || text == "" || text != strings.TrimSpace(text) || len(text) > 64 || !utf8.ValidString(text) {
		return false
	}
	tag, err := language.Parse(text)
	return err == nil && tag != language.Und && tag.String() == text
}

func validateModelProfileSyncRequest(action string, params map[string]any) error {
	value, present := params["default_tool_client_profile_id"]
	if !present {
		return nil
	}
	profileID, ok := value.(string)
	if !ok {
		return invalidActionRequest(action, "default_tool_client_profile_id", "must be a string")
	}
	if profileID == "" {
		return nil
	}
	entries, ok := actionObjectSlice(params["entries"])
	if !ok {
		return invalidActionRequest(action, "entries", "must be an array")
	}
	for _, entry := range entries {
		if entry["client_profile_id"] != profileID {
			continue
		}
		kind, _ := entry["model_kind"].(string)
		if kind == "" || kind == "conversation" {
			return nil
		}
		return invalidActionRequest(action, "default_tool_client_profile_id", "must reference a conversation profile")
	}
	// Agent owns persisted profiles and validates a default that is not part of
	// this sync batch against its authoritative store.
	return nil
}

func validateTextToolsConfigUpdateRequest(action string, params map[string]any) error {
	if err := rejectUnknownActionFields(action, params, "idempotency_key", "expected_revision", "enabled", "tools"); err != nil {
		return err
	}
	if params == nil || !canonicalActionUUID(params["idempotency_key"]) {
		return invalidActionRequest(action, "idempotency_key", "must be a canonical UUID")
	}
	if !actionIntegerInRange(params["expected_revision"], 0, math.MaxInt64) {
		return invalidActionRequest(action, "expected_revision", "must be a non-negative integer")
	}
	if _, ok := params["enabled"].(bool); !ok {
		return invalidActionRequest(action, "enabled", "must be a boolean")
	}
	return validateTextTools(action, params["tools"])
}

func validateTextToolsExecuteRequest(action string, params map[string]any) error {
	if err := rejectUnknownActionFields(action, params, "tool_id", "selected_text", "output_language"); err != nil {
		return err
	}
	if params == nil || !validTextToolID(params["tool_id"]) {
		return invalidActionRequest(action, "tool_id", "must be a stable default id or canonical UUID")
	}
	if !validTextToolString(params["selected_text"], 1, maxSelectedTextBytes, false) {
		return invalidActionRequest(action, "selected_text", "must be UTF-8 of 1 to 65536 bytes")
	}
	outputLanguage, ok := params["output_language"].(string)
	if !ok || (outputLanguage != "zh" && outputLanguage != "en") {
		return invalidActionRequest(action, "output_language", "must be zh or en")
	}
	return nil
}

func validateTextTools(action string, value any) error {
	tools, ok := actionObjectSlice(value)
	if !ok || len(tools) > maxTextTools {
		return invalidActionRequest(action, "tools", "must be an array of at most 32 tools")
	}
	orders := make(map[int64]struct{}, len(tools))
	ids := make(map[string]struct{}, len(tools))
	enabledCount := 0
	for _, tool := range tools {
		if err := rejectUnknownActionFields(action, tool, "tool_id", "name", "system_prompt", "order", "enabled"); err != nil {
			return err
		}
		toolID, _ := tool["tool_id"].(string)
		if !validTextToolID(toolID) {
			return invalidActionRequest(action, "tools.tool_id", "must be a stable default id or canonical UUID")
		}
		if _, duplicate := ids[toolID]; duplicate {
			return invalidActionRequest(action, "tools.tool_id", "must be unique")
		}
		ids[toolID] = struct{}{}
		if !validTextToolString(tool["name"], 1, maxTextToolNameBytes, false) {
			return invalidActionRequest(action, "tools.name", "must be UTF-8 of 1 to 64 bytes")
		}
		if !validTextToolString(tool["system_prompt"], 1, maxTextToolPromptBytes, false) {
			return invalidActionRequest(action, "tools.system_prompt", "must be UTF-8 of 1 to 16384 bytes")
		}
		order, valid := actionInteger(tool["order"])
		if !valid || order < 0 || order >= int64(len(tools)) {
			return invalidActionRequest(action, "tools.order", "must form a contiguous zero-based sequence")
		}
		if _, duplicate := orders[order]; duplicate {
			return invalidActionRequest(action, "tools.order", "must be unique")
		}
		orders[order] = struct{}{}
		enabled, ok := tool["enabled"].(bool)
		if !ok {
			return invalidActionRequest(action, "tools.enabled", "must be a boolean")
		}
		if enabled {
			enabledCount++
			if enabledCount > maxEnabledTextTools {
				return invalidActionRequest(action, "tools.enabled", "must enable at most 6 tools")
			}
		}
	}
	return nil
}

func validTextToolID(value any) bool {
	id, ok := value.(string)
	if !ok {
		return false
	}
	switch id {
	case "translation", "summary", "explanation", "search":
		return true
	default:
		return canonicalActionUUID(id)
	}
}

func validTextToolString(value any, minimum, maximum int, rejectBlank bool) bool {
	text, ok := value.(string)
	if !ok || len(text) < minimum || len(text) > maximum || !utf8.ValidString(text) {
		return false
	}
	return !rejectBlank || strings.TrimSpace(text) != ""
}

func actionObjectSlice(value any) ([]map[string]any, bool) {
	items, ok := value.([]any)
	if !ok {
		if typed, typedOK := value.([]map[string]any); typedOK {
			return typed, true
		}
		return nil, false
	}
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			return nil, false
		}
		result = append(result, object)
	}
	return result, true
}

func validateCloudWorkerArtifactDownloadRequest(action string, params map[string]any) error {
	if err := rejectUnknownActionFields(action, params, "record_kind", "artifact_id", "offset_bytes", "max_chunk_bytes"); err != nil {
		return err
	}
	if params == nil || params["record_kind"] != "cloud_worker" {
		return invalidActionRequest(action, "record_kind", "must be exactly cloud_worker")
	}
	if !canonicalActionUUID(params["artifact_id"]) {
		return invalidActionRequest(action, "artifact_id", "must be a canonical UUID")
	}
	if !actionIntegerInRange(params["offset_bytes"], 0, maxCloudWorkerArtifactBytes-1) {
		return invalidActionRequest(action, "offset_bytes", "must be an integer from 0 to 8388607")
	}
	if !actionIntegerInRange(params["max_chunk_bytes"], 1, maxCloudWorkerArtifactChunkBytes) {
		return invalidActionRequest(action, "max_chunk_bytes", "must be an integer from 1 to 524288")
	}
	return nil
}

func validateTurnStopRequest(action string, params map[string]any) error {
	if err := rejectUnknownActionFields(action, params, "idempotency_key", "turn_id", "expected_revision"); err != nil {
		return err
	}
	if params == nil {
		return invalidActionRequest(action, "idempotency_key", "is required")
	}
	for _, field := range []string{"idempotency_key", "turn_id"} {
		if !canonicalActionUUID(params[field]) {
			return invalidActionRequest(action, field, "must be a canonical UUID")
		}
	}
	if !positiveInteger(params["expected_revision"]) {
		return invalidActionRequest(action, "expected_revision", "must be a positive integer")
	}
	return nil
}

func validateTurnSteerRequest(action string, params map[string]any) error {
	if err := rejectUnknownActionFields(action, params, "idempotency_key", "turn_id", "expected_revision", "instruction"); err != nil {
		return err
	}
	if params == nil {
		return invalidActionRequest(action, "idempotency_key", "is required")
	}
	for _, field := range []string{"idempotency_key", "turn_id"} {
		if !canonicalActionUUID(params[field]) {
			return invalidActionRequest(action, field, "must be a canonical UUID")
		}
	}
	if !positiveInteger(params["expected_revision"]) {
		return invalidActionRequest(action, "expected_revision", "must be a positive integer")
	}
	instruction, ok := params["instruction"].(string)
	if !ok || strings.TrimSpace(instruction) == "" || len(instruction) > maxChatMessageBytes || !utf8.ValidString(instruction) {
		return invalidActionRequest(action, "instruction", "must be non-empty UTF-8 of at most 1048576 bytes")
	}
	return nil
}

func validateTurnsListRequest(action string, params map[string]any) error {
	if err := rejectUnknownActionFields(action, params, "conversation_id", "page_token", "limit"); err != nil {
		return err
	}
	if params == nil || !canonicalActionUUID(params["conversation_id"]) {
		return invalidActionRequest(action, "conversation_id", "must be a canonical UUID")
	}
	if value, present := params["page_token"]; present {
		pageToken, ok := value.(string)
		if !ok || len(pageToken) > 4096 {
			return invalidActionRequest(action, "page_token", "must be a bounded string")
		}
	}
	if value, present := params["limit"]; present && !positiveIntegerAtMost(value, 1000) {
		return invalidActionRequest(action, "limit", "must be an integer from 1 to 1000")
	}
	return nil
}

func rejectUnknownActionFields(action string, params map[string]any, allowed ...string) error {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, field := range allowed {
		allowedSet[field] = struct{}{}
	}
	unknown := make([]string, 0)
	for field := range params {
		if _, ok := allowedSet[field]; !ok {
			unknown = append(unknown, field)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return invalidActionRequest(action, unknown[0], "is not supported")
}

func canonicalActionUUID(value any) bool {
	text, ok := value.(string)
	if !ok || text != strings.TrimSpace(text) {
		return false
	}
	parsed, err := uuid.Parse(text)
	return err == nil && parsed != uuid.Nil && parsed.String() == text
}

func validateChatRequest(action string, params map[string]any) error {
	if params == nil {
		return invalidActionRequest(action, "idempotency_key", "is required")
	}
	allowed := []string{
		"idempotency_key", "conversation_id", "message", "model_profile_id",
		"model_profile_revision", "credential_version",
	}
	if action == "agent.chat.stream" {
		allowed = append(allowed, "after_seq", "accepted_attachment_ids", "extensions")
	}
	if err := rejectUnknownActionFields(action, params, allowed...); err != nil {
		return err
	}
	if field := firstForbiddenChatRequestKey(params); field != "" {
		return invalidActionRequest(action, field, "is not supported")
	}
	if !canonicalActionUUID(params["idempotency_key"]) {
		return invalidActionRequest(action, "idempotency_key", "must be a canonical UUID")
	}
	message, ok := params["message"].(string)
	if !ok || strings.TrimSpace(message) == "" || len(message) > maxChatMessageBytes || !utf8.ValidString(message) {
		return invalidActionRequest(action, "message", "must be non-empty UTF-8 of at most 1048576 bytes")
	}
	if conversationID, present := params["conversation_id"]; present {
		if !canonicalActionUUID(conversationID) {
			return invalidActionRequest(action, "conversation_id", "must be a canonical UUID")
		}
	} else if action == "agent.chat.stream" {
		return invalidActionRequest(action, "conversation_id", "is required")
	}
	if value, present := params["after_seq"]; present {
		sequence, valid := actionInteger(value)
		if action != "agent.chat.stream" || !valid || sequence < 0 {
			return invalidActionRequest(action, "after_seq", "must be a non-negative integer")
		}
	}

	profileID, present := params["model_profile_id"]
	if !present {
		return invalidActionRequest(action, "model_profile_id", "is required")
	}
	profileIDText, ok := profileID.(string)
	if !ok || strings.TrimSpace(profileIDText) == "" {
		return invalidActionRequest(action, "model_profile_id", "must be a non-empty string")
	}
	for _, field := range []string{"model_profile_revision", "credential_version"} {
		value, present := params[field]
		if !present {
			return invalidActionRequest(action, field, "is required")
		}
		if !positiveInteger(value) {
			return invalidActionRequest(action, field, "must be a positive integer")
		}
	}
	if value, present := params["accepted_attachment_ids"]; present {
		if action != "agent.chat.stream" {
			return invalidActionRequest(action, "accepted_attachment_ids", "is supported only by agent.chat.stream")
		}
		ids, ok := actionStringSlice(value)
		if !ok || len(ids) > maxChatAttachments {
			return invalidActionRequest(action, "accepted_attachment_ids", "must contain at most 4 canonical UUIDs")
		}
		seen := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			if !canonicalActionUUID(id) {
				return invalidActionRequest(action, "accepted_attachment_ids", "must contain at most 4 canonical UUIDs")
			}
			if _, duplicate := seen[id]; duplicate {
				return invalidActionRequest(action, "accepted_attachment_ids", "must not contain duplicates")
			}
			seen[id] = struct{}{}
		}
	}
	if value, present := params["extensions"]; present {
		if action != "agent.chat.stream" {
			return invalidActionRequest(action, "extensions", "is supported only by agent.chat.stream")
		}
		if err := validateChatExtensions(action, value); err != nil {
			return err
		}
	}
	return nil
}

func validateChatExtensions(action string, value any) error {
	extensions, ok := value.([]any)
	if !ok || len(extensions) == 0 || len(extensions) > maxChatExtensions {
		return invalidActionRequest(action, "extensions", "must contain 1 to 64 exact local extension selections")
	}
	seenInstallations := make(map[string]struct{}, len(extensions))
	for _, raw := range extensions {
		extension, ok := raw.(map[string]any)
		if !ok {
			return invalidActionRequest(action, "extensions", "must contain exact local extension selection objects")
		}
		if err := rejectUnknownActionFields(action, extension, "kind", "id", "pinned_version", "digest", "allowed_tools"); err != nil {
			return invalidActionRequest(action, "extensions", "contains an unsupported selection field")
		}
		kind, kindOK := extension["kind"].(string)
		id, idOK := extension["id"].(string)
		version, versionOK := extension["pinned_version"].(string)
		if !kindOK || kind != "mcp" ||
			!idOK || !canonicalActionUUID(id) || !versionOK || version == "" || version != strings.TrimSpace(version) ||
			len(version) > maxChatExtensionVersionBytes || !utf8.ValidString(version) || !canonicalActionSHA256(extension["digest"]) {
			return invalidActionRequest(action, "extensions", "contains an invalid immutable extension binding")
		}
		if _, duplicate := seenInstallations[id]; duplicate {
			return invalidActionRequest(action, "extensions", "must not contain duplicate installation IDs")
		}
		seenInstallations[id] = struct{}{}
		tools, ok := actionStringSlice(extension["allowed_tools"])
		if !ok || len(tools) == 0 || len(tools) > maxChatExtensions {
			return invalidActionRequest(action, "extensions", "allowed_tools must contain 1 to 64 exact tool names")
		}
		seenTools := make(map[string]struct{}, len(tools))
		for _, tool := range tools {
			if tool == "" || tool != strings.TrimSpace(tool) || len(tool) > maxChatExtensionToolNameBytes || !utf8.ValidString(tool) || tool == "cloud_worker_propose" {
				return invalidActionRequest(action, "extensions", "contains an invalid local tool name")
			}
			if _, duplicate := seenTools[tool]; duplicate {
				return invalidActionRequest(action, "extensions", "must not contain duplicate tool names")
			}
			seenTools[tool] = struct{}{}
		}
	}
	return nil
}

func validateChatAttachmentBeginRequest(action string, params map[string]any) error {
	if err := rejectUnknownActionFields(action, params,
		"idempotency_key", "turn_request_id", "kind", "name", "mime_type", "declared_size", "content_sha256"); err != nil {
		return err
	}
	if !canonicalActionUUID(params["idempotency_key"]) {
		return invalidActionRequest(action, "idempotency_key", "must be a canonical UUID")
	}
	if !canonicalActionUUID(params["turn_request_id"]) {
		return invalidActionRequest(action, "turn_request_id", "must be a canonical UUID")
	}
	if !validChatAttachmentName(params["name"]) {
		return invalidActionRequest(action, "name", "must be a 1 to 255 byte basename")
	}
	if !validChatAttachmentMIME(params["kind"], params["mime_type"]) {
		return invalidActionRequest(action, "kind", "must bind an approved image, file, or workspace_archive media type")
	}
	if !actionIntegerInRange(params["declared_size"], 1, maxChatAttachmentBytes) {
		return invalidActionRequest(action, "declared_size", "must be an integer from 1 to 8388608")
	}
	if !canonicalActionSHA256(params["content_sha256"]) {
		return invalidActionRequest(action, "content_sha256", "must be a lowercase SHA-256 digest")
	}
	return nil
}

func validateChatAttachmentAppendRequest(action string, params map[string]any) error {
	if err := rejectUnknownActionFields(action, params,
		"idempotency_key", "upload_id", "expected_revision", "ordinal", "offset_bytes", "data_base64", "chunk_sha256"); err != nil {
		return err
	}
	for _, field := range []string{"idempotency_key", "upload_id"} {
		if !canonicalActionUUID(params[field]) {
			return invalidActionRequest(action, field, "must be a canonical UUID")
		}
	}
	if !positiveInteger(params["expected_revision"]) {
		return invalidActionRequest(action, "expected_revision", "must be a positive integer")
	}
	if !actionIntegerInRange(params["ordinal"], 0, math.MaxUint32) {
		return invalidActionRequest(action, "ordinal", "must be a non-negative integer")
	}
	if !actionIntegerInRange(params["offset_bytes"], 0, math.MaxInt64) {
		return invalidActionRequest(action, "offset_bytes", "must be a non-negative integer")
	}
	encoded, ok := params["data_base64"].(string)
	if !ok || encoded == "" {
		return invalidActionRequest(action, "data_base64", "must be canonical standard base64 for 1 to 1048576 bytes")
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || int64(len(decoded)) < 1 || int64(len(decoded)) > maxChatAttachmentChunkBytes || base64.StdEncoding.EncodeToString(decoded) != encoded {
		return invalidActionRequest(action, "data_base64", "must be canonical standard base64 for 1 to 1048576 bytes")
	}
	defer clear(decoded)
	if !canonicalActionSHA256(params["chunk_sha256"]) {
		return invalidActionRequest(action, "chunk_sha256", "must be a lowercase SHA-256 digest")
	}
	digest := sha256.Sum256(decoded)
	if params["chunk_sha256"] != hex.EncodeToString(digest[:]) {
		return invalidActionRequest(action, "chunk_sha256", "must match the decoded chunk")
	}
	return nil
}

func validateChatAttachmentCommitRequest(action string, params map[string]any) error {
	if err := rejectUnknownActionFields(action, params,
		"idempotency_key", "upload_id", "expected_revision", "content_sha256"); err != nil {
		return err
	}
	for _, field := range []string{"idempotency_key", "upload_id"} {
		if !canonicalActionUUID(params[field]) {
			return invalidActionRequest(action, field, "must be a canonical UUID")
		}
	}
	if !positiveInteger(params["expected_revision"]) {
		return invalidActionRequest(action, "expected_revision", "must be a positive integer")
	}
	if !canonicalActionSHA256(params["content_sha256"]) {
		return invalidActionRequest(action, "content_sha256", "must be a lowercase SHA-256 digest")
	}
	return nil
}

func actionStringSlice(value any) ([]string, bool) {
	switch values := value.(type) {
	case []string:
		return append([]string(nil), values...), true
	case []any:
		out := make([]string, 0, len(values))
		for _, value := range values {
			text, ok := value.(string)
			if !ok {
				return nil, false
			}
			out = append(out, text)
		}
		return out, true
	default:
		return nil, false
	}
}

func validChatAttachmentName(value any) bool {
	name, ok := value.(string)
	if !ok || name == "" || name != strings.TrimSpace(name) || len(name) > maxChatAttachmentNameBytes || !utf8.ValidString(name) || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsAny(name, "\\/\r\n\x00")
}

func validChatAttachmentMIME(kindValue, value any) bool {
	kind, kindOK := kindValue.(string)
	mimeType, ok := value.(string)
	if !kindOK || !ok || mimeType != strings.ToLower(strings.TrimSpace(mimeType)) {
		return false
	}
	parsed, parameters, err := mime.ParseMediaType(mimeType)
	if err != nil || parsed != mimeType || len(parameters) != 0 {
		return false
	}
	switch kind {
	case "image":
		return mimeType == "image/jpeg" || mimeType == "image/png" || mimeType == "image/webp"
	case "workspace_archive":
		return mimeType == "application/vnd.dirextalk.workspace+tar+gzip"
	case "file":
		if strings.HasPrefix(mimeType, "text/") || strings.HasSuffix(mimeType, "+json") || strings.HasSuffix(mimeType, "+xml") {
			return true
		}
		switch mimeType {
		case "application/json", "application/ld+json", "application/xml", "application/yaml",
			"application/pdf", "application/rtf", "application/octet-stream", "application/wasm",
			"application/zip", "application/msword", "application/vnd.ms-excel", "application/vnd.ms-powerpoint",
			"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
			"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
			"application/vnd.openxmlformats-officedocument.presentationml.presentation":
			return true
		}
		return false
	default:
		return false
	}
}

func canonicalActionSHA256(value any) bool {
	digest, ok := value.(string)
	if !ok || len(digest) != sha256.Size*2 || digest != strings.ToLower(strings.TrimSpace(digest)) {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

func actionIntegerInRange(value any, minimum, maximum int64) bool {
	integer, ok := actionInteger(value)
	return ok && integer >= minimum && integer <= maximum
}

func actionInteger(value any) (int64, bool) {
	switch typed := value.(type) {
	case json.Number:
		rational, ok := new(big.Rat).SetString(strings.TrimSpace(typed.String()))
		if !ok || !rational.IsInt() || !rational.Num().IsInt64() {
			return 0, false
		}
		return rational.Num().Int64(), true
	case int:
		return int64(typed), true
	case int8:
		return int64(typed), true
	case int16:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case uint:
		if uint64(typed) > math.MaxInt64 {
			return 0, false
		}
		return int64(typed), true
	case uint8:
		return int64(typed), true
	case uint16:
		return int64(typed), true
	case uint32:
		return int64(typed), true
	case uint64:
		if typed > math.MaxInt64 {
			return 0, false
		}
		return int64(typed), true
	case float32:
		integer := float64(typed)
		if math.IsNaN(integer) || math.IsInf(integer, 0) || math.Trunc(integer) != integer || integer < math.MinInt64 || integer >= -float64(math.MinInt64) {
			return 0, false
		}
		return int64(integer), true
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || math.Trunc(typed) != typed || typed < math.MinInt64 || typed >= -float64(math.MinInt64) {
			return 0, false
		}
		return int64(typed), true
	default:
		return 0, false
	}
}

// firstForbiddenChatRequestKey walks only map keys and nested map/list
// containers. In particular, ordinary string values are never inspected for
// secret-like content; a prompt containing "api_key" remains valid.
func firstForbiddenChatRequestKey(value any) string {
	return walkChatRequestKeys(reflect.ValueOf(value))
}

type chatRequestMapEntry struct {
	key   string
	value reflect.Value
}

func walkChatRequestKeys(value reflect.Value) string {
	for value.IsValid() && (value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer) {
		if value.IsNil() {
			return ""
		}
		value = value.Elem()
	}
	if !value.IsValid() {
		return ""
	}

	switch value.Kind() {
	case reflect.Map:
		if value.IsNil() {
			return ""
		}
		entries := make([]chatRequestMapEntry, 0, value.Len())
		for _, key := range value.MapKeys() {
			normalized, ok := normalizedChatRequestMapKey(key)
			if !ok {
				continue
			}
			entries = append(entries, chatRequestMapEntry{key: normalized, value: value.MapIndex(key)})
		}
		// Map iteration order is deliberately randomized. Stable traversal keeps
		// the field reported to callers deterministic when several keys are bad.
		sort.Slice(entries, func(i, j int) bool { return entries[i].key < entries[j].key })
		for _, entry := range entries {
			if forbiddenChatRequestKey(entry.key) {
				return strings.ToLower(strings.TrimSpace(entry.key))
			}
			if nested := walkChatRequestKeys(entry.value); nested != "" {
				return nested
			}
		}
	case reflect.Slice, reflect.Array:
		for index := 0; index < value.Len(); index++ {
			if nested := walkChatRequestKeys(value.Index(index)); nested != "" {
				return nested
			}
		}
	}
	return ""
}

func normalizedChatRequestMapKey(value reflect.Value) (string, bool) {
	for value.IsValid() && value.Kind() == reflect.Interface {
		if value.IsNil() {
			return "", false
		}
		value = value.Elem()
	}
	if !value.IsValid() || value.Kind() != reflect.String {
		return "", false
	}
	// Preserve ASCII case for camel/acronym tokenization; comparisons and
	// returned error fields lower-case the trimmed key at the boundary.
	return strings.TrimSpace(value.String()), true
}

func forbiddenChatRequestKey(key string) bool {
	key = strings.TrimSpace(key)
	normalized := strings.ToLower(key)
	if normalized == "" {
		return false
	}
	// These are server-derived profile pins. credential_version would otherwise
	// match the broad credential policy, so only the exact normalized spellings
	// of the immutable triple remain admissible.
	if isServerProfilePinKey(normalized) {
		return false
	}
	if isUnsupportedChatKey(key) {
		return true
	}
	return sensitiveChatKey(key)
}

func isServerProfilePinKey(key string) bool {
	switch key {
	case "model_profile_id", "model_profile_revision", "credential_version":
		return true
	default:
		return false
	}
}

func isUnsupportedChatKey(key string) bool {
	for _, unsupported := range []string{
		"tool_credentials", "model_profile", "client_model_profile_id",
		"default_profile", "default_profile_id", "default_model_profile_id", "use_default_profile",
		"messages", "history", "chat_history", "conversation_history", "attachments", "data_base64",
	} {
		if key == unsupported {
			return true
		}
	}
	joined := strings.Join(chatRequestKeyTokens(key), "_")
	switch joined {
	case "tool_credentials", "model_profile", "client_model_profile_id",
		"default_profile", "default_profile_id", "default_model_profile_id", "use_default_profile",
		"model_profile_id", "model_profile_revision",
		"messages", "history", "chat_history", "conversation_history", "attachments", "data_base64":
		// Profile pin variants (for example clientModelProfileId) are not
		// server-derived pins and must not become a nested escape hatch.
		return true
	default:
		return false
	}
}

func sensitiveChatKey(key string) bool {
	if catalogSensitiveKeyRE.MatchString(key) {
		return true
	}
	tokens := chatRequestKeyTokens(key)
	for index, token := range tokens {
		normalized := singularChatKeyToken(token)
		if sensitiveChatKeyToken(normalized) {
			return true
		}
		if index+1 < len(tokens) {
			next := singularChatKeyToken(tokens[index+1])
			if (normalized == "api" || normalized == "private" || normalized == "client") && (next == "key" || next == "secret") {
				return true
			}
		}
	}
	return false
}

func sensitiveChatKeyToken(token string) bool {
	switch token {
	case "authorization", "header", "cookie", "bearer", "basic", "secret", "token", "pass", "password", "passwd", "passphrase", "credential":
		return true
	case "apikey", "privatekey", "clientsecret", "accesstoken", "refreshtoken", "bearertoken":
		return true
	default:
		for _, suffix := range []string{
			"authorization", "authorizations", "header", "headers", "cookie", "cookies", "bearer", "bearers", "basic", "basics",
			"secret", "secrets", "token", "tokens", "pass", "passes", "password", "passwords", "passwd", "passwds",
			"passphrase", "passphrases", "credential", "credentials", "apikey", "apikeys", "privatekey", "privatekeys",
			"clientsecret", "clientsecrets", "accesstoken", "accesstokens", "refreshtoken", "refreshtokens", "bearertoken", "bearertokens",
		} {
			if strings.HasSuffix(token, suffix) && len(token) > len(suffix) {
				return true
			}
		}
		return false
	}
}

func singularChatKeyToken(token string) string {
	switch token {
	case "headers":
		return "header"
	case "cookies":
		return "cookie"
	case "secrets":
		return "secret"
	case "tokens":
		return "token"
	case "basics":
		return "basic"
	case "bearers":
		return "bearer"
	case "passes":
		return "pass"
	case "passwords":
		return "password"
	case "passwds":
		return "passwd"
	case "passphrases":
		return "passphrase"
	case "credentials":
		return "credential"
	case "authorizations":
		return "authorization"
	case "keys":
		return "key"
	case "apikeys":
		return "apikey"
	case "privatekeys":
		return "privatekey"
	case "clientsecrets":
		return "clientsecret"
	case "accesstokens":
		return "accesstoken"
	case "refreshtokens":
		return "refreshtoken"
	case "bearertokens":
		return "bearertoken"
	default:
		return token
	}
}

// chatRequestKeyTokens lowercases ASCII keys, splits separators, and handles
// both lowerCamelCase and acronym-to-word transitions (APIKey, HTTPHeaders).
// It intentionally does not inspect values.
func chatRequestKeyTokens(key string) []string {
	runes := []rune(strings.TrimSpace(key))
	if len(runes) == 0 {
		return nil
	}
	tokens := make([]string, 0, 4)
	current := make([]rune, 0, len(runes))
	flush := func() {
		if len(current) == 0 {
			return
		}
		tokens = append(tokens, strings.ToLower(string(current)))
		current = current[:0]
	}
	for index, char := range runes {
		if !isASCIIAlphaNumeric(char) {
			flush()
			continue
		}
		if isASCIIUpper(char) && len(current) > 0 {
			previous := runes[index-1]
			var next rune
			if index+1 < len(runes) {
				next = runes[index+1]
			}
			if isASCIILower(previous) || (isASCIIUpper(previous) && isASCIILower(next)) {
				flush()
			}
		}
		current = append(current, char)
	}
	flush()
	return tokens
}

func isASCIIAlphaNumeric(char rune) bool {
	return char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9'
}

func isASCIIUpper(char rune) bool { return char >= 'A' && char <= 'Z' }

func isASCIILower(char rune) bool { return char >= 'a' && char <= 'z' }

func validateModelCatalogRequest(action string, params map[string]any) error {
	if params == nil {
		return nil
	}
	if value, present := params["model_kind"]; present {
		kind, ok := value.(string)
		if !ok || !isModelKind(kind) {
			return invalidActionRequest(action, "model_kind", "must be conversation, embedding, or speech")
		}
	}

	profileFields := []string{"model_profile_id", "client_model_profile_id"}
	profilePresent := false
	for _, field := range profileFields {
		if value, present := params[field]; present {
			profilePresent = true
			profileID, ok := value.(string)
			if !ok || strings.TrimSpace(profileID) == "" {
				return invalidActionRequest(action, field, "must be a non-empty string")
			}
		}
	}
	if _, apiKeyPresent := params["api_key"]; apiKeyPresent {
		if profilePresent {
			return invalidActionRequest(action, "api_key", "cannot be combined with a model profile id")
		}
		apiKey, ok := params["api_key"].(string)
		if !ok || strings.TrimSpace(apiKey) == "" {
			return invalidActionRequest(action, "api_key", "must be a non-empty string")
		}
	}
	if _, bothProfiles := params["model_profile_id"]; bothProfiles {
		if _, legacy := params["client_model_profile_id"]; legacy {
			return invalidActionRequest(action, "model_profile_id", "cannot be combined with client_model_profile_id")
		}
	}
	return nil
}

func isModelKind(value string) bool {
	switch value {
	case "conversation", "embedding", "speech":
		return true
	default:
		return false
	}
}

func positiveInteger(value any) bool {
	switch typed := value.(type) {
	case json.Number:
		rational, ok := new(big.Rat).SetString(strings.TrimSpace(typed.String()))
		return ok && rational.Sign() > 0 && rational.IsInt() && rational.Cmp(maxInt64Rat) <= 0
	case int:
		return typed > 0
	case int8:
		return typed > 0
	case int16:
		return typed > 0
	case int32:
		return typed > 0
	case int64:
		return typed > 0
	case uint:
		return typed > 0 && uint64(typed) <= uint64(math.MaxInt64)
	case uint8:
		return typed > 0
	case uint16:
		return typed > 0
	case uint32:
		return typed > 0
	case uint64:
		return typed > 0 && typed <= uint64(math.MaxInt64)
	case float32:
		value := float64(typed)
		return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0) && value < float64(math.MaxInt64) && math.Trunc(value) == value
	case float64:
		return typed > 0 && !math.IsNaN(typed) && !math.IsInf(typed, 0) && typed < float64(math.MaxInt64) && math.Trunc(typed) == typed
	default:
		return false
	}
}

func positiveIntegerAtMost(value any, maximum int64) bool {
	if maximum <= 0 || !positiveInteger(value) {
		return false
	}
	switch typed := value.(type) {
	case json.Number:
		rational, ok := new(big.Rat).SetString(strings.TrimSpace(typed.String()))
		return ok && rational.Cmp(new(big.Rat).SetInt64(maximum)) <= 0
	case int:
		return int64(typed) <= maximum
	case int8:
		return int64(typed) <= maximum
	case int16:
		return int64(typed) <= maximum
	case int32:
		return int64(typed) <= maximum
	case int64:
		return typed <= maximum
	case uint:
		return uint64(typed) <= uint64(maximum)
	case uint8:
		return uint64(typed) <= uint64(maximum)
	case uint16:
		return uint64(typed) <= uint64(maximum)
	case uint32:
		return uint64(typed) <= uint64(maximum)
	case uint64:
		return typed <= uint64(maximum)
	case float32:
		return float64(typed) <= float64(maximum)
	case float64:
		return typed <= float64(maximum)
	default:
		return false
	}
}

// CapabilityError is the safe, structured error returned when Agent rejects
// a capability operation. The upstream message is deliberately discarded;
// Agent providers may include credential-bearing details in that field.
type CapabilityError struct {
	Code       capv1.ErrorCode
	ClientCode string
}

const (
	KnowledgeQuotaExceededCode     = "knowledge_quota_exceeded"
	ExtensionInstallBusyCode       = "extension_install_busy"
	ExtensionInstallationLimitCode = "extension_installation_limit"
	ExtensionNodeStorageQuotaCode  = "extension_node_storage_quota"
)

func (e *CapabilityError) Error() string {
	if e == nil {
		return "external native agent operation failed"
	}
	if e.Code == capv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED && e.ClientCode == KnowledgeQuotaExceededCode {
		return "knowledge quota exceeded"
	}
	switch e.ClientCode {
	case ExtensionInstallBusyCode:
		return "another extension installation is already in progress"
	case ExtensionInstallationLimitCode:
		return "extension installation limit reached"
	case ExtensionNodeStorageQuotaCode:
		return "extension Node storage quota exceeded"
	}
	switch e.Code {
	case capv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT:
		return "external native agent rejected the request"
	case capv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED:
		return "external native agent denied the request"
	case capv1.ErrorCode_ERROR_CODE_NOT_FOUND:
		return "external native agent resource was not found"
	case capv1.ErrorCode_ERROR_CODE_CONFLICT:
		return "external native agent reported a conflict"
	case capv1.ErrorCode_ERROR_CODE_PRECONDITION_FAILED:
		return "external native agent precondition failed"
	case capv1.ErrorCode_ERROR_CODE_NOT_READY:
		return "external native agent is not ready"
	case capv1.ErrorCode_ERROR_CODE_UNAVAILABLE:
		return "external native agent is unavailable"
	case capv1.ErrorCode_ERROR_CODE_UNCERTAIN:
		return "external native agent operation outcome is uncertain"
	case capv1.ErrorCode_ERROR_CODE_UPSTREAM_FAILED:
		return "external native agent upstream operation failed"
	default:
		return "external native agent operation failed"
	}
}

func capabilityError(code capv1.ErrorCode) error { return &CapabilityError{Code: code} }

func capabilityErrorFromProto(value *capv1.CapabilityError) error {
	if value == nil {
		return capabilityError(capv1.ErrorCode_ERROR_CODE_UPSTREAM_FAILED)
	}
	clientCode := ""
	switch value.GetDetails()["code"] {
	case KnowledgeQuotaExceededCode:
		if value.GetCode() == capv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED {
			clientCode = KnowledgeQuotaExceededCode
		}
	case ExtensionInstallBusyCode:
		if value.GetCode() == capv1.ErrorCode_ERROR_CODE_PRECONDITION_FAILED {
			clientCode = ExtensionInstallBusyCode
		}
	case ExtensionInstallationLimitCode:
		if value.GetCode() == capv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED {
			clientCode = ExtensionInstallationLimitCode
		}
	case ExtensionNodeStorageQuotaCode:
		if value.GetCode() == capv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED {
			clientCode = ExtensionNodeStorageQuotaCode
		}
	}
	return &CapabilityError{Code: value.GetCode(), ClientCode: clientCode}
}

// CapabilityHTTPStatus translates the structured Agent error taxonomy to the
// stable ProductCore HTTP semantics. Unknown codes fail closed as 502.
func CapabilityHTTPStatus(code capv1.ErrorCode) int {
	switch code {
	case capv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT:
		return 400
	case capv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED:
		return 403
	case capv1.ErrorCode_ERROR_CODE_NOT_FOUND:
		return 404
	case capv1.ErrorCode_ERROR_CODE_CONFLICT, capv1.ErrorCode_ERROR_CODE_UNCERTAIN, capv1.ErrorCode_ERROR_CODE_CYCLE_DETECTED:
		return 409
	case capv1.ErrorCode_ERROR_CODE_PRECONDITION_FAILED:
		return 412
	case capv1.ErrorCode_ERROR_CODE_NOT_READY, capv1.ErrorCode_ERROR_CODE_UNAVAILABLE, capv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED:
		return 503
	case capv1.ErrorCode_ERROR_CODE_TRUST_FAILED, capv1.ErrorCode_ERROR_CODE_INCOMPATIBLE, capv1.ErrorCode_ERROR_CODE_UPSTREAM_FAILED:
		return 502
	default:
		return 502
	}
}
