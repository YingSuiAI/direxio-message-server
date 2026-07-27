package nativeagent

import (
	"regexp"
	"strings"
)

const scheduledResultLimit = 8192

var scheduledSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/-]{8,}`),
	regexp.MustCompile(`(?i)((?:api[_-]?key|token|secret)\s*[=:]\s*)[^\s,;]+`),
}

// SanitizeScheduledText is the single durable boundary for outputs/errors
// that may be recorded in schedule storage. It accepts the in-memory model
// key only to remove it; it never returns or logs that key.
func SanitizeScheduledText(value, apiKey string) string {
	value = strings.TrimSpace(value)
	if apiKey = strings.TrimSpace(apiKey); apiKey != "" {
		value = strings.ReplaceAll(value, apiKey, "[redacted]")
	}
	for _, pattern := range scheduledSecretPatterns {
		value = pattern.ReplaceAllString(value, "$1[redacted]")
	}
	if len(value) > scheduledResultLimit {
		value = value[:scheduledResultLimit]
	}
	return value
}
