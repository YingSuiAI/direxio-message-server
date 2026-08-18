package dendrite

import (
	"errors"
	"net/http"
	"strings"

	"github.com/YingSuiAI/dirextalk-message-server/internal/productpolicy"
)

func trimString(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	default:
		return ""
	}
}

func fallbackString(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return fallback
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

func domainFromMXID(mxid string) string {
	return domainFromMatrixID(mxid, "@")
}

func domainFromMatrixID(id, sigil string) string {
	trimmed := strings.TrimPrefix(id, sigil)
	if idx := strings.Index(trimmed, ":"); idx >= 0 {
		idx += len(id) - len(trimmed)
		if idx+1 >= len(id) {
			return ""
		}
		return id[idx+1:]
	}
	return ""
}

func isDirectRoomJoinRequiresInvite(err error) bool {
	var policyErr *productpolicy.PolicyError
	return errors.As(err, &policyErr) &&
		policyErr.Code == http.StatusForbidden &&
		policyErr.Message == "direct room join requires invite"
}

func matrixMessageType(messageType string, media bool) string {
	if !media {
		return "m.text"
	}
	switch strings.TrimSpace(messageType) {
	case "image", "m.image":
		return "m.image"
	case "video", "m.video":
		return "m.video"
	case "audio", "m.audio":
		return "m.audio"
	default:
		return "m.file"
	}
}
