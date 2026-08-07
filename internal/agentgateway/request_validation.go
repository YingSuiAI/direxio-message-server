package agentgateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"regexp"
	"sort"
	"strings"

	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
	"github.com/google/uuid"
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

var maxInt64Rat = new(big.Rat).SetInt64(math.MaxInt64)

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
	case "agent.chat", "agent.chat.stream":
		return validateChatRequest(action, params)
	case "agent.models.list":
		return validateModelCatalogRequest(action, params)
	case "agent.chat.turn.stop":
		return validateTurnStopRequest(action, params)
	case "agent.chat.turns.list":
		return validateTurnsListRequest(action, params)
	default:
		return nil
	}
}

func validateTurnStopRequest(action string, params map[string]any) error {
	if err := rejectUnknownActionFields(action, params, "turn_id"); err != nil {
		return err
	}
	if params == nil || !canonicalActionUUID(params["turn_id"]) {
		return invalidActionRequest(action, "turn_id", "must be a canonical UUID")
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
		return invalidActionRequest(action, "model_profile_id", "is required")
	}
	// These fields were accepted by older embedded-Agent clients. Keeping them
	// out of the request is important: the selected profile and its secret
	// revision must be resolved server-side and pinned by the caller.
	for _, field := range []string{
		"client_model_profile_id",
		"model_profile",
		"default_profile",
		"default_profile_id",
		"default_model_profile_id",
		"use_default_profile",
		"tool_credentials",
		"messages",
		"history",
		"chat_history",
		"conversation_history",
	} {
		if _, present := params[field]; present {
			return invalidActionRequest(action, field, "is not supported")
		}
	}
	if field := firstForbiddenChatRequestKey(params); field != "" {
		return invalidActionRequest(action, field, "is not supported")
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
	return nil
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
		"messages", "history", "chat_history", "conversation_history",
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
		"messages", "history", "chat_history", "conversation_history":
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

const KnowledgeQuotaExceededCode = "knowledge_quota_exceeded"

func (e *CapabilityError) Error() string {
	if e == nil {
		return "external native agent operation failed"
	}
	if e.Code == capv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED && e.ClientCode == KnowledgeQuotaExceededCode {
		return "knowledge quota exceeded"
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
	if value.GetCode() == capv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED && value.GetDetails()["code"] == KnowledgeQuotaExceededCode {
		clientCode = KnowledgeQuotaExceededCode
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
