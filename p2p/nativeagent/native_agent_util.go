package nativeagent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

func trimString(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case fmt.Stringer:
		return strings.TrimSpace(v.String())
	default:
		return ""
	}
}

func fallbackString(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return strings.TrimSpace(fallback)
}

func cloneAnyMap(values map[string]any) map[string]any {
	if values == nil {
		return map[string]any{}
	}
	cloned := make(map[string]any, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func int64Param(value any) int64 {
	switch v := value.(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case int8:
		return int64(v)
	case int16:
		return int64(v)
	case int32:
		return int64(v)
	case uint:
		return int64(v)
	case uint8:
		return int64(v)
	case uint16:
		return int64(v)
	case uint32:
		return int64(v)
	case uint64:
		if v <= math.MaxInt64 {
			return int64(v)
		}
		return 0
	case float32:
		return int64(v)
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return n
	default:
		return 0
	}
}

func boolParam(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true") || strings.TrimSpace(v) == "1"
	case float64:
		return v != 0
	case int:
		return v != 0
	case int64:
		return v != 0
	default:
		return false
	}
}

func stringSliceParam(value any) []string {
	switch v := value.(type) {
	case []string:
		return normalizedStringSlice(v)
	case []any:
		values := make([]string, 0, len(v))
		for _, item := range v {
			values = append(values, trimString(item))
		}
		return normalizedStringSlice(values)
	default:
		return nil
	}
}

func normalizedStringSlice(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func pluginConfigString(config map[string]any, key string) string {
	if config == nil {
		return ""
	}
	return trimString(config[key])
}

func savedAgentModelProfileByID(config map[string]any, profileID string) map[string]any {
	profileID = strings.TrimSpace(profileID)
	if profileID == "" {
		return nil
	}
	profiles, ok := config["model_profiles"].([]any)
	if !ok {
		return nil
	}
	for _, rawProfile := range profiles {
		profile, ok := rawProfile.(map[string]any)
		if ok && pluginConfigString(profile, "id") == profileID {
			return profile
		}
	}
	return nil
}

func jsonValue(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(data)
}

func randomToken(prefix string) string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return strings.TrimSpace(prefix) + "_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return hex.EncodeToString(buf[:])
	}
	return prefix + "_" + hex.EncodeToString(buf[:])
}

func sanitizeConfig(config map[string]any) map[string]any {
	sanitized := cloneAnyMap(config)
	delete(sanitized, "api_key")
	delete(sanitized, "api_key_ref")
	if profiles, ok := sanitized["model_profiles"].([]any); ok {
		sanitized["model_profiles"] = sanitizeModelProfiles(profiles)
	}
	return sanitized
}

func sanitizeModelProfiles(profiles []any) []any {
	sanitized := make([]any, 0, len(profiles))
	for _, rawProfile := range profiles {
		profile, ok := rawProfile.(map[string]any)
		if !ok {
			sanitized = append(sanitized, rawProfile)
			continue
		}
		cloned := cloneAnyMap(profile)
		delete(cloned, "api_key")
		delete(cloned, "api_key_ref")
		sanitized = append(sanitized, cloned)
	}
	return sanitized
}

func configList(config map[string]any, key string) []map[string]any {
	switch rawList := config[key].(type) {
	case []map[string]any:
		records := make([]map[string]any, 0, len(rawList))
		for _, record := range rawList {
			records = append(records, cloneAnyMap(record))
		}
		return records
	case []any:
		records := make([]map[string]any, 0, len(rawList))
		for _, raw := range rawList {
			record, ok := raw.(map[string]any)
			if ok {
				records = append(records, cloneAnyMap(record))
			}
		}
		return records
	default:
		return nil
	}
}

func upsertConfigRecord(records []map[string]any, record map[string]any) []any {
	id := sanitizeNativeID(trimString(record["id"]))
	out := make([]any, 0, len(records)+1)
	replaced := false
	for _, existing := range records {
		if sanitizeNativeID(trimString(existing["id"])) == id {
			out = append(out, record)
			replaced = true
			continue
		}
		out = append(out, existing)
	}
	if !replaced {
		out = append(out, record)
	}
	return out
}

func nestedOrSelf(params map[string]any, key string) map[string]any {
	if nested, ok := params[key].(map[string]any); ok {
		return cloneAnyMap(nested)
	}
	return cloneAnyMap(params)
}

func sanitizeNativeID(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ""
	}
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		allowed := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-'
		if !allowed {
			if !lastDash {
				b.WriteRune('-')
				lastDash = true
			}
			continue
		}
		b.WriteRune(r)
		lastDash = r == '-'
	}
	return strings.Trim(b.String(), "-_")
}

func envMapToList(value any) []string {
	raw, _ := value.(map[string]any)
	env := make([]string, 0, len(raw))
	for key, value := range raw {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		env = append(env, key+"="+trimString(value))
	}
	sort.Strings(env)
	return env
}

func durationSeconds(value any, fallback int) time.Duration {
	seconds := int64Param(value)
	if seconds <= 0 {
		seconds = int64(fallback)
	}
	return time.Duration(seconds) * time.Second
}

func fallbackInt(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}

func anyToMap(value any) (map[string]any, error) {
	if value == nil {
		return map[string]any{}, nil
	}
	if mapped, ok := value.(map[string]any); ok {
		return mapped, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}
