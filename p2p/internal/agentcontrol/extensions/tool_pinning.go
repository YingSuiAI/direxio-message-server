package extensions

import (
	"bytes"
	"context"
	"strings"
)

// ToolPinner durably pins a remote MCP tools/list projection to an installed
// version. Implementations must scope every read and write by owner and fence
// the installation revision and active version.
type ToolPinner interface {
	PinTools(context.Context, string, string, string, int64, []Tool) ([]Tool, error)
}

// ValidatePinnedTools validates the immutable tool projection before it is
// persisted. A digest is required even when the provider omits an input
// schema; when a schema is present its bytes must hash to that digest.
func ValidatePinnedTools(tools []Tool) error {
	if len(tools) == 0 {
		return ErrInvalid
	}
	seen := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		if strings.TrimSpace(tool.Name) == "" || tool.Name != strings.TrimSpace(tool.Name) {
			return ErrInvalid
		}
		if _, ok := seen[tool.Name]; ok {
			return ErrInvalid
		}
		seen[tool.Name] = struct{}{}
		if !validDigest(tool.InputSchemaDigest) {
			return ErrInvalid
		}
		if len(tool.InputSchema) > 0 && DigestBytes(tool.InputSchema) != tool.InputSchemaDigest {
			return ErrInvalid
		}
	}
	return nil
}

// PinnedToolsEqual compares the persisted projection without treating a
// missing schema (nil) and an empty schema as different byte slices.
func PinnedToolsEqual(a, b []Tool) bool {
	if len(a) != len(b) {
		return false
	}
	for n := range a {
		if a[n].Name != b[n].Name || a[n].Description != b[n].Description || a[n].InputSchemaDigest != b[n].InputSchemaDigest || !bytes.Equal(a[n].InputSchema, b[n].InputSchema) {
			return false
		}
	}
	return true
}

// PinTools implements ToolPinner for the in-memory lifecycle store used by
// focused tests and non-durable callers.
func (s *MemoryStore) PinTools(ctx context.Context, owner, installationID, versionID string, expectedRevision int64, tools []Tool) ([]Tool, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(owner) == "" || strings.TrimSpace(installationID) == "" || strings.TrimSpace(versionID) == "" || expectedRevision <= 0 {
		return nil, ErrInvalid
	}
	if err := ValidatePinnedTools(tools); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := owner + "\x00" + installationID
	item, ok := s.items[key]
	if !ok {
		return nil, ErrNotFound
	}
	if item.Revision != expectedRevision {
		return nil, ErrRevisionConflict
	}
	if item.State != "installed" || item.ActiveVersionID != versionID {
		return nil, ErrConflict
	}
	for n := range item.Versions {
		if item.Versions[n].VersionID != versionID {
			continue
		}
		if len(item.Versions[n].Tools) > 0 {
			if ValidatePinnedTools(item.Versions[n].Tools) != nil {
				return nil, ErrConflict
			}
			if PinnedToolsEqual(item.Versions[n].Tools, tools) {
				return clonePinnedTools(item.Versions[n].Tools), nil
			}
			return nil, ErrConflict
		}
		item.Versions[n].Tools = clonePinnedTools(tools)
		s.items[key] = cloneInstallation(item)
		return clonePinnedTools(item.Versions[n].Tools), nil
	}
	return nil, ErrNotFound
}

func clonePinnedTools(tools []Tool) []Tool {
	out := make([]Tool, len(tools))
	for n, tool := range tools {
		out[n] = tool
		out[n].InputSchema = append([]byte(nil), tool.InputSchema...)
	}
	return out
}

var _ ToolPinner = (*MemoryStore)(nil)
