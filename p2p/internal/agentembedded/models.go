package agentembedded

import (
	"context"
	"net/http"
	"strings"

	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

func (m *Module) backendsGet(_ context.Context, _ map[string]any) (any, *actionbase.Error) {
	capabilities := []string{}
	if m != nil && m.cfg.ModelProfiles != nil && m.cfg.ModelProfiles.ModelProfileStoreReady() && m.capabilityReady("model_profiles.server", true) {
		capabilities = append(capabilities, "model_profiles.server", "model_roles.server")
	}
	if m != nil && m.cfg.CapabilityReady != nil && m.capabilityReady("memory.server", true) {
		capabilities = append(capabilities, "memory.server")
	}
	if m != nil && m.cfg.CapabilityReady != nil && m.capabilityReady("voice.server", true) {
		capabilities = append(capabilities, "voice.server")
	}
	if m != nil && m.capabilityReady("schedules.server", m.cfg.Schedules != nil && m.cfg.ScheduleTrigger != nil) {
		capabilities = append(capabilities, "schedules.server")
	}
	if m != nil && m.capabilityReady("task", m.cfg.Tasks != nil) {
		capabilities = append(capabilities, "task")
	}
	if m != nil && m.capabilityReady("confirmation", m.cfg.Confirmations != nil) {
		capabilities = append(capabilities, "confirmation")
	}
	if m != nil && m.capabilityReady("mcp", m.cfg.MCP != nil) {
		capabilities = append(capabilities, "mcp")
	}
	if m != nil && m.capabilityReady("aws.control", m.cfg.AWS != nil) {
		capabilities = append(capabilities, "aws.control")
	}
	if m != nil && m.capabilityReady("workload.aws_ssm", m.cfg.Workloads != nil) &&
		m.capabilityReady("workload.aws_ecs", m.cfg.Workloads != nil) {
		capabilities = append(capabilities, "workload.aws_ssm", "workload.aws_ecs")
	}
	if m != nil && m.capabilityReady("deployments.server", m.cfg.Deployments != nil) {
		capabilities = append(capabilities, "deployments.server")
	}
	return map[string]any{
		"embedded": map[string]any{"available": true, "configured": true, "status": "ready", "capabilities": capabilities},
		"core":     map[string]any{"configured": false, "status": "not_configured", "instance_id": "", "api_version": "", "capabilities": []string{}, "supported_model_providers": []string{}},
	}, nil
}

func (m *Module) statusGet(_ context.Context, _ map[string]any) (any, *actionbase.Error) {
	return map[string]any{"configured": false, "status": "not_configured", "instance_id": "", "api_version": "", "capabilities": []string{}, "supported_model_providers": []string{}}, nil
}

func (m *Module) modelHandler(action string) actionbase.Handler {
	return func(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
		if _, e := m.requireCapability(ctx, params, "model_profiles.server", m.cfg.ModelProfiles != nil); e != nil {
			return nil, e
		}
		if m == nil || m.cfg.ModelProfiles == nil {
			return unavailable(ctx, params)
		}
		owner := m.owner()
		switch action[strings.LastIndex(action, ".")+1:] {
		case "sync":
			return m.modelSync(ctx, owner, params)
		case "list":
			size, token, e := page(params)
			if e != nil {
				return nil, e
			}
			out, err := m.cfg.ModelProfiles.ListModelProfiles(ctx, owner, size, token)
			if err != nil {
				return nil, modelError(err)
			}
			items := make([]any, 0, len(out.Profiles))
			for _, p := range out.Profiles {
				items = append(items, profileMap(p))
			}
			return map[string]any{"profiles": items, "next_page_token": out.NextPageToken}, nil
		case "get":
			id, e := requiredString(params, "profile_id")
			if e != nil {
				return nil, e
			}
			p, found, err := m.cfg.ModelProfiles.GetModelProfile(ctx, owner, id)
			if err != nil {
				return nil, modelError(err)
			}
			if !found {
				return nil, actionbase.CodedError(http.StatusNotFound, "model_profile_not_found", "model profile was not found")
			}
			return map[string]any{"profile": profileMap(p)}, nil
		case "delete":
			id, e := requiredString(params, "profile_id")
			if e != nil {
				return nil, e
			}
			key, e := requiredString(params, "idempotency_key")
			if e != nil {
				return nil, e
			}
			rev, e := optionalInt64(params, "expected_revision")
			if e != nil {
				return nil, e
			}
			if err := m.cfg.ModelProfiles.DeleteModelProfile(ctx, owner, key, id, ptrInt64(rev, params, "expected_revision")); err != nil {
				return nil, modelError(err)
			}
			return map[string]any{"deleted": true, "profile_id": id}, nil
		default:
			return unavailable(ctx, params)
		}
	}
}

func (m *Module) modelSync(ctx context.Context, owner string, params map[string]any) (any, *actionbase.Error) {
	idempotency, e := requiredString(params, "idempotency_key")
	if e != nil {
		return nil, e
	}
	raw, ok := params["entries"].([]any)
	if !ok {
		if raw2, ok2 := params["profiles"].([]any); ok2 {
			raw = raw2
		} else if raw2, ok2 := params["model_profiles"].([]any); ok2 {
			raw = raw2
		} else if typed, ok2 := params["entries"].([]map[string]any); ok2 {
			raw = make([]any, len(typed))
			for i := range typed {
				raw[i] = typed[i]
			}
		} else {
			return nil, actionbase.BadRequest("profiles must be an array")
		}
	}
	entries := make([]storage.ModelProfileSyncEntry, 0, len(raw))
	for _, item := range raw {
		v, ok := item.(map[string]any)
		if !ok {
			return nil, actionbase.BadRequest("profiles must contain objects")
		}
		id, e := requiredString(v, "client_profile_id")
		if e != nil {
			return nil, e
		}
		provider, e := requiredString(v, "provider")
		if e != nil {
			return nil, e
		}
		entry := storage.ModelProfileSyncEntry{ClientProfileID: id, Provider: provider}
		entry.DisplayName, e = optionalString(v, "display_name")
		if e != nil {
			return nil, e
		}
		entry.BaseURL, e = optionalString(v, "base_url")
		if e != nil {
			return nil, e
		}
		entry.Model, e = optionalString(v, "model")
		if e != nil {
			return nil, e
		}
		entry.SystemPrompt, e = optionalString(v, "system_prompt")
		if e != nil {
			return nil, e
		}
		entry.ReasoningEffort, e = optionalString(v, "reasoning_effort")
		if e != nil {
			return nil, e
		}
		if key, present := v["api_key"]; present {
			s, ok := key.(string)
			if !ok || s == "" {
				return nil, actionbase.BadRequest("api_key must be non-empty when present")
			}
			entry.APIKey = &s
		}
		if value, present := v["expected_revision"]; present {
			n, e := optionalInt64(v, "expected_revision")
			if e != nil {
				return nil, e
			}
			entry.ExpectedRevision = &n
			_ = value
		}
		entries = append(entries, entry)
	}
	defaultID, e := optionalString(params, "default_client_profile_id")
	if e != nil {
		return nil, e
	}
	result, err := m.cfg.ModelProfiles.SyncModelProfiles(ctx, owner, idempotency, defaultID, entries)
	if err != nil {
		return nil, modelError(err)
	}
	profiles := make([]any, 0, len(result.Profiles))
	for _, p := range result.Profiles {
		profiles = append(profiles, profileMap(p))
	}
	return map[string]any{"profiles": profiles, "default_client_profile_id": result.DefaultClientProfileID}, nil
}

func profileMap(p storage.ModelProfile) map[string]any {
	return map[string]any{"profile_id": p.ProfileID, "client_profile_id": p.ClientProfileID, "display_name": p.DisplayName, "provider": p.Provider, "base_url": p.BaseURL, "model": p.Model, "system_prompt": p.SystemPrompt, "api_key_configured": p.APIKeyConfigured, "max_output_tokens": p.MaxOutputTokens, "context_window": p.ContextWindow, "reasoning_effort": p.ReasoningEffort, "revision": p.Revision, "created_at": p.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"), "updated_at": p.UpdatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")}
}

func modelError(err error) *actionbase.Error {
	if errorsIs(err, storage.ErrModelProfileNotFound) {
		return actionbase.CodedError(http.StatusNotFound, "model_profile_not_found", "model profile was not found")
	}
	if errorsIs(err, storage.ErrModelProfileRevision) {
		return actionbase.CodedError(http.StatusConflict, "model_profile_revision_conflict", "model profile revision conflict")
	}
	return actionbase.InternalError(err)
}
