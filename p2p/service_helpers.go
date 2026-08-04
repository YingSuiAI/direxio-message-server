package p2p

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
)

func (s *Service) portalStateLocked() portalState {
	return portalState{
		Initialized:    s.initialized,
		Password:       s.password,
		AccessToken:    s.accessToken,
		MatrixDeviceID: cleanMatrixDeviceID(s.matrixDeviceID),
		AgentToken:     s.agentToken,
		OwnerMXID:      s.ownerMXID,
		AgentRoomID:    s.agentRoomID,
		SystemRoomID:   s.systemRoomID,
		Profile:        s.profile,
		AgentConfig:    s.agentConfig,
		ClientBuild:    s.clientBuild,
	}
}

func trimString(value any) string {
	return actionbase.String(value)
}

// cloneAnyMap remains the shared root shallow-copy helper for cross-domain
// adapters until those transports move into their owning modules.
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

func stringSliceParam(value any) []string {
	return actionbase.Strings(value)
}

func int64Param(value any) int64 {
	return actionbase.Int64(value)
}

func channelJoinServerNames(value any, roomID string) []string {
	names := stringSliceParam(value)
	result := make([]string, 0, len(names))
	for _, name := range names {
		if text := strings.TrimSpace(name); text != "" {
			result = append(result, text)
		}
	}
	if len(result) > 0 {
		return result
	}
	if serverName, ok := roomServerFromMatrixRoomID(roomID); ok {
		return []string{serverName}
	}
	return nil
}

func boolParam(value any) bool {
	return actionbase.Bool(value)
}

func domainFromMXID(mxid string) string {
	return dirextalkdomain.DomainFromMXID(mxid)
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

func displayNameFromMXID(mxid string) string {
	return dirextalkdomain.DisplayNameFromMXID(mxid)
}

func firstMemberID(params map[string]any) string {
	values := actionbase.Params(params)
	if userID := values.FirstString("user_id", "user_mxid", "peer_mxid", "mxid"); userID != "" {
		return userID
	}
	return values.FirstListString("user_ids", "user_mxids", "peer_mxids", "invitees")
}

func memberHidden(membership string) bool {
	return dirextalkdomain.MemberHidden(membership)
}

func memberRemoved(membership string) bool {
	switch strings.ToLower(strings.TrimSpace(membership)) {
	case "remove", "removed", "ban", "banned":
		return true
	default:
		return false
	}
}

func memberLeft(membership string) bool {
	switch strings.ToLower(strings.TrimSpace(membership)) {
	case "leave", "left":
		return true
	default:
		return false
	}
}

func contactAccepted(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), "accepted")
}

func contactDeleted(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), "deleted")
}

func contactPendingInbound(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), "pending_inbound")
}

func randomToken(prefix string) string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(buf[:])
}

func randomNumericPassword() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%08d", time.Now().UnixNano()%100000000)
	}
	for i := range buf {
		buf[i] = '0' + (buf[i] % 10)
	}
	return string(buf[:])
}

func cleanMatrixDeviceID(deviceID string) string {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return matrixPortalDeviceID
	}
	return deviceID
}

func requestedMatrixDeviceID(params map[string]any) string {
	deviceID := strings.TrimSpace(trimString(params["device_id"]))
	if deviceID != "" {
		return cleanMatrixDeviceID(deviceID)
	}
	return "PORTALIM" + strings.TrimPrefix(randomToken("device"), "device_")
}

func fallbackString(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return fallback
}

func defaultPortalPassword() string {
	if path := strings.TrimSpace(os.Getenv("P2P_PORTAL_PASSWORD_FILE")); path != "" {
		if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o077 == 0 {
			if data, readErr := os.ReadFile(path); readErr == nil {
				password := strings.TrimSpace(string(data))
				if password != "" && len(password) <= 1024 && !strings.ContainsAny(password, "\x00\r\n") {
					return password
				}
			}
		}
		// A configured file is fail-closed: do not fall back to plaintext env
		// material when the protected file is missing or malformed.
		return randomNumericPassword()
	}
	if password := strings.TrimSpace(os.Getenv("P2P_PORTAL_PASSWORD")); password != "" {
		return password
	}
	return randomNumericPassword()
}
