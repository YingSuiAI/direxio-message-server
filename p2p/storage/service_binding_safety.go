package storage

import (
	"errors"
	"math"
	"regexp"
	"strings"
	"unicode"
)

// ErrExecutionStoreInvalid is retained as the generic invalid-payload marker
// used by the service-binding output sanitizer. Execution persistence itself
// no longer lives in message-server.
var ErrExecutionStoreInvalid = errors.New("execution store: invalid")

var (
	catalogSensitiveKeyRE        = regexp.MustCompile(`(?i)(^|[^a-z0-9])(access[_-]?token|refresh[_-]?token|client[_-]?secret|authorization|headers?|cookies?|bearer|basic|secret|token|pass(?:word|wd|phrase)|credential|api[_-]?key|private[_-]?key)([^a-z0-9]|$)`)
	catalogBearerBasicRE         = regexp.MustCompile(`(?i)(?:^|[\s:=])(?:bearer|basic)\s+\S+`)
	catalogSensitiveAssignmentRE = regexp.MustCompile(`(?i)(?:^|[\s,;])(?:access[_-]?token|refresh[_-]?token|client[_-]?secret|authorization|cookies?|set-cookie)\s*[:=]\s*\S+`)
	catalogPrivateKeyRE          = regexp.MustCompile(`(?i)-----begin(?: [a-z0-9]+)* private key-----`)
)

func catalogSensitiveString(value string) bool {
	v := strings.TrimSpace(value)
	if v == "" || len(v) < 24 {
		return false
	}
	if catalogBearerBasicRE.MatchString(v) || catalogSensitiveAssignmentRE.MatchString(v) || catalogPrivateKeyRE.MatchString(v) {
		return true
	}
	for _, r := range v {
		if unicode.IsSpace(r) {
			return false
		}
	}
	counts := make(map[rune]int)
	for _, r := range v {
		counts[r]++
	}
	length := float64(len([]rune(v)))
	entropy := 0.0
	for _, count := range counts {
		p := float64(count) / length
		entropy -= p * math.Log2(p)
	}
	return entropy >= 3.75
}
