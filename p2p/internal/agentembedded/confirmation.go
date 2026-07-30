package agentembedded

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
)

func (m *Module) confirmationHandler(action string) actionbase.Handler {
	return func(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
		if _, e := m.requireCapability(ctx, p, "confirmation", m.cfg.Confirmations != nil); e != nil {
			return nil, e
		}
		if m == nil || m.cfg.Confirmations == nil {
			return unavailable(ctx, p)
		}
		o := m.owner()
		op := action[strings.LastIndex(action, ".")+1:]
		switch op {
		case "list":
			size, token, e := page(p)
			if e != nil {
				return nil, e
			}
			domain, e := optionalString(p, "operation_domain")
			if e != nil {
				return nil, e
			}
			target, e := optionalString(p, "target_id")
			if e != nil {
				return nil, e
			}
			states := []string{}
			if raw, ok := p["states"].([]any); ok {
				for _, x := range raw {
					s, ok := x.(string)
					if !ok {
						return nil, actionbase.BadRequest("states must contain strings")
					}
					states = append(states, s)
				}
			}
			parsedStates := make([]confirmation.State, 0, len(states))
			for _, s := range states {
				parsedStates = append(parsedStates, confirmation.State(s))
			}
			pageResult, err := m.cfg.Confirmations.List(ctx, confirmation.ListQuery{OwnerID: o, PageSize: size, PageToken: token, Domain: domain, TargetID: target, States: parsedStates})
			if err != nil {
				return nil, confirmationError(err)
			}
			out := make([]any, 0, len(pageResult.Confirmations))
			for _, v := range pageResult.Confirmations {
				out = append(out, confirmationMap(v))
			}
			return map[string]any{"confirmations": out, "next_page_token": pageResult.NextPageToken}, nil
		case "get":
			id, e := requiredString(p, "confirmation_id")
			if e != nil {
				return nil, e
			}
			v, err := m.cfg.Confirmations.Get(ctx, id)
			if err != nil {
				return nil, confirmationError(err)
			}
			if strings.TrimSpace(v.OwnerID) != o {
				return nil, confirmationError(confirmation.ErrNotFound)
			}
			return map[string]any{"confirmation": confirmationMap(v)}, nil
		case "confirm", "reject":
			id, e := requiredString(p, "confirmation_id")
			if e != nil {
				return nil, e
			}
			key, e := requiredString(p, "idempotency_key")
			if e != nil {
				return nil, e
			}
			rev, e := requiredPositiveInt64(p, "expected_revision")
			if e != nil {
				return nil, e
			}
			stored, err := m.cfg.Confirmations.Get(ctx, id)
			if err != nil {
				return nil, confirmationError(err)
			}
			if strings.TrimSpace(stored.OwnerID) != o {
				return nil, confirmationError(confirmation.ErrNotFound)
			}
			var v confirmation.Confirmation
			if op == "confirm" {
				v, err = m.cfg.Confirmations.Confirm(ctx, confirmation.ConfirmCommand{
					OwnerID: o, ID: id, ConfirmationID: id, IdempotencyKey: key,
					ExpectedRevision: rev, Binding: stored.Binding, At: time.Now().UTC(),
				})
			} else {
				reason, _ := optionalString(p, "reason")
				v, err = m.cfg.Confirmations.Reject(ctx, confirmation.RejectCommand{
					OwnerID: o, ID: id, ConfirmationID: id, IdempotencyKey: key,
					ExpectedRevision: rev, Reason: reason, At: time.Now().UTC(),
				})
			}
			if err != nil {
				return nil, confirmationError(err)
			}
			return map[string]any{"confirmation": confirmationMap(v)}, nil
		default:
			return unavailable(ctx, p)
		}
	}
}
func confirmationMap(v confirmation.Confirmation) map[string]any {
	id := v.ID
	if id == "" {
		id = v.ConfirmationID
	}
	grants := make([]any, 0, len(v.Binding.SecretGrants))
	for _, grant := range v.Binding.SecretGrants {
		item := map[string]any{
			"reference_id":   grant.ReferenceID,
			"purpose":        string(grant.Purpose),
			"binding_digest": string(grant.BindingDigest),
		}
		if grant.Revision > 0 {
			item["secret_revision"] = grant.Revision
		}
		grants = append(grants, item)
	}
	networkGrants := append([]string{}, v.Binding.NetworkGrants...)
	selectedCommand := append([]string{}, v.Binding.SelectedCommand...)
	return map[string]any{"confirmation_id": id, "task_id": v.TaskID, "state": string(v.State), "revision": v.Revision, "created_at": v.CreatedAt.UTC().Format(time.RFC3339Nano), "updated_at": v.UpdatedAt.UTC().Format(time.RFC3339Nano), "expires_at": v.ExpiresAt.UTC().Format(time.RFC3339Nano), "terminal_reason": v.TerminalReason, "terminal_code": v.TerminalCode, "terminal_note": v.TerminalNote, "binding": map[string]any{"operation_domain": v.Binding.OperationDomain, "target_id": v.Binding.TargetID, "target_revision": v.Binding.TargetRevision, "content_digest": v.Binding.ContentDigest, "parameter_digest": v.Binding.ParameterDigest, "network_digest": v.Binding.NetworkDigest, "secret_grant_digest": v.Binding.SecretGrantDigest, "network_grants": networkGrants, "secret_grants": grants, "owner_id": v.Binding.OwnerID, "target_kind": v.Binding.TargetKind, "source_version": v.Binding.SourceVersion, "source_commit": v.Binding.SourceCommit, "manifest_digest": v.Binding.ManifestDigest, "execution_digest": v.Binding.ExecutionDigest, "permission_digest": v.Binding.PermissionDigest, "selected_tool": v.Binding.SelectedTool, "selected_command": selectedCommand}}
}
func confirmationError(err error) *actionbase.Error {
	if errors.Is(err, confirmation.ErrNotFound) {
		return actionbase.CodedError(http.StatusNotFound, "confirmation_not_found", "confirmation was not found")
	}
	if errors.Is(err, confirmation.ErrRevisionConflict) || errors.Is(err, confirmation.ErrConflict) {
		return actionbase.CodedError(http.StatusConflict, "confirmation_conflict", "confirmation revision conflict")
	}
	return actionbase.InternalError(err)
}
