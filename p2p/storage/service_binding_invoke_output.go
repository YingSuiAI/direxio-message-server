package storage

import (
	"encoding/json"
	"regexp"
	"strings"
)

// SafeServiceBindingInvokeOutput canonicalizes an adapter result at the
// service-binding invocation boundary. Invocation results can be forwarded to
// ProductCore clients and Native Agent tools, so they are never a secret
// transport. The returned map contains JSON-only values suitable for direct
// response serialization.
func SafeServiceBindingInvokeOutput(value any) (map[string]any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, ErrExecutionStoreInvalid
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, ErrExecutionStoreInvalid
	}
	result, ok := decoded.(map[string]any)
	if !ok || !safeServiceBindingInvokeValue(result, "") {
		return nil, ErrExecutionStoreInvalid
	}
	return result, nil
}

var safeServiceBindingPurposeRE = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)
var safeServiceBindingRefRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]{0,255}$`)

func safeServiceBindingInvokeValue(value any, key string) bool {
	normalized := strings.ToLower(strings.NewReplacer("-", "_", ".", "_").Replace(strings.TrimSpace(key)))
	switch normalized {
	case "secret_ref", "secret_refs":
		return safeServiceBindingSecretRef(value)
	case "purpose", "purposes":
		return safeServiceBindingPurpose(value)
	}
	if key != "" && catalogSensitiveKeyRE.MatchString(normalized) {
		return false
	}
	switch v := value.(type) {
	case map[string]any:
		for childKey, child := range v {
			if !safeServiceBindingInvokeValue(child, childKey) {
				return false
			}
		}
		return true
	case []any:
		for _, child := range v {
			if !safeServiceBindingInvokeValue(child, "") {
				return false
			}
		}
		return true
	case string:
		return !catalogSensitiveString(v)
	case nil, bool, json.Number:
		return true
	default:
		return false
	}
}

func safeServiceBindingSecretRef(value any) bool {
	switch v := value.(type) {
	case string:
		return safeServiceBindingRefRE.MatchString(v) && !catalogSensitiveString(v)
	case []any:
		for _, item := range v {
			if !safeServiceBindingSecretRef(item) {
				return false
			}
		}
		return true
	case map[string]any:
		if len(v) == 0 {
			return false
		}
		for key, item := range v {
			switch strings.ToLower(strings.NewReplacer("-", "_", ".", "_").Replace(strings.TrimSpace(key))) {
			case "ref", "reference_id", "binding_digest":
				text, ok := item.(string)
				if !ok || !safeServiceBindingRefRE.MatchString(text) || catalogSensitiveString(text) {
					return false
				}
			case "purpose":
				if !safeServiceBindingPurpose(item) {
					return false
				}
			case "revision":
				if _, ok := item.(json.Number); !ok {
					return false
				}
			default:
				return false
			}
		}
		return true
	default:
		return false
	}
}

func safeServiceBindingPurpose(value any) bool {
	switch v := value.(type) {
	case string:
		return safeServiceBindingPurposeRE.MatchString(v)
	case []any:
		for _, item := range v {
			if !safeServiceBindingPurpose(item) {
				return false
			}
		}
		return true
	default:
		return false
	}
}
