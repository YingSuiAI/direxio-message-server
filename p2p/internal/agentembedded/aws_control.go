package agentembedded

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
)

type AWSServiceResolver func(string) (*coreaws.Service, error)

// NewAWSActionPort exposes only owner-scoped credential management. Plans and
// changes are intentionally not part of the embedded action surface.
func NewAWSActionPort(resolve AWSServiceResolver) (ActionPort, error) {
	if resolve == nil {
		return nil, ErrUnavailable
	}
	return ActionPortFunc(func(ctx context.Context, owner, actionName string, params map[string]any) (any, *action.Error) {
		if strings.TrimSpace(owner) == "" {
			return nil, action.CodedError(http.StatusUnauthorized, "owner_required", "owner is required")
		}
		if !awsCredentialAction(actionName) {
			return nil, action.CodedError(http.StatusNotFound, "aws_action_not_found", "unsupported AWS credential action")
		}
		service, err := resolve(strings.TrimSpace(owner))
		if err != nil || service == nil || !service.ReadyForEmbedded() {
			return nil, statusUnavailable()
		}
		switch actionName {
		case "agent.core.aws.credentials.create":
			return awsCredentialCreate(ctx, service, params)
		case "agent.core.aws.credentials.update":
			return awsCredentialUpdate(ctx, service, params)
		case "agent.core.aws.credentials.delete":
			return awsCredentialDelete(ctx, service, params)
		case "agent.core.aws.credentials.list":
			return awsCredentialList(ctx, service, params)
		case "agent.core.aws.credentials.test":
			return awsCredentialTest(ctx, service, params)
		default:
			return nil, action.CodedError(http.StatusNotFound, "aws_action_not_found", "unsupported AWS credential action")
		}
	}), nil
}

func awsCredentialAction(name string) bool {
	switch name {
	case "agent.core.aws.credentials.create", "agent.core.aws.credentials.update", "agent.core.aws.credentials.delete", "agent.core.aws.credentials.list", "agent.core.aws.credentials.test":
		return true
	default:
		return false
	}
}

func rejectUnknownAWSFields(params map[string]any, allowed map[string]struct{}) *action.Error {
	for key := range params {
		if _, ok := allowed[key]; !ok {
			return action.CodedError(http.StatusBadRequest, "unknown_field", "unknown field: "+key)
		}
	}
	return nil
}

func awsCredentialCreate(ctx context.Context, service *coreaws.Service, params map[string]any) (any, *action.Error) {
	if e := rejectUnknownAWSFields(params, map[string]struct{}{"idempotency_key": {}, "name": {}, "region": {}, "access_key_id": {}, "secret_access_key": {}, "session_token": {}}); e != nil {
		return nil, e
	}
	idem, e := requiredString(params, "idempotency_key")
	if e != nil {
		return nil, e
	}
	name, e := requiredString(params, "name")
	if e != nil {
		return nil, e
	}
	region, e := requiredString(params, "region")
	if e != nil {
		return nil, e
	}
	access, e := requiredString(params, "access_key_id")
	if e != nil {
		return nil, e
	}
	secret, e := requiredString(params, "secret_access_key")
	if e != nil {
		return nil, e
	}
	session, e := optionalString(params, "session_token")
	if e != nil {
		return nil, e
	}
	view, err := service.SaveCredential(ctx, coreaws.CredentialInput{Name: name, Region: region, AccessKeyID: access, SecretAccessKey: secret, SessionToken: session, IdempotencyKey: idem})
	if err != nil {
		return nil, mapAWSServiceError(err)
	}
	return map[string]any{"credential": awsCredentialView(view)}, nil
}

func awsCredentialUpdate(ctx context.Context, service *coreaws.Service, params map[string]any) (any, *action.Error) {
	if e := rejectUnknownAWSFields(params, map[string]struct{}{"idempotency_key": {}, "credential_id": {}, "expected_revision": {}, "name": {}, "region": {}, "access_key_id": {}, "secret_access_key": {}, "session_token": {}}); e != nil {
		return nil, e
	}
	idem, e := requiredString(params, "idempotency_key")
	if e != nil {
		return nil, e
	}
	id, e := requiredString(params, "credential_id")
	if e != nil {
		return nil, e
	}
	expected, e := requiredPositiveInt64(params, "expected_revision")
	if e != nil {
		return nil, e
	}
	current, err := service.GetCredential(ctx, id)
	if err != nil {
		return nil, mapAWSServiceError(err)
	}
	name, e := optionalString(params, "name")
	if e != nil {
		return nil, e
	}
	if _, ok := params["name"]; !ok {
		name = current.Name
	}
	region, e := optionalString(params, "region")
	if e != nil {
		return nil, e
	}
	if _, ok := params["region"]; !ok {
		region = current.Region
	}
	access, e := optionalString(params, "access_key_id")
	if e != nil {
		return nil, e
	}
	secret, e := optionalString(params, "secret_access_key")
	if e != nil {
		return nil, e
	}
	session, e := optionalString(params, "session_token")
	if e != nil {
		return nil, e
	}
	view, err := service.ReplaceCredential(ctx, coreaws.CredentialInput{ID: id, Name: name, Region: region, AccessKeyID: access, SecretAccessKey: secret, SessionToken: session}, expected, idem)
	if err != nil {
		return nil, mapAWSServiceError(err)
	}
	return map[string]any{"credential": awsCredentialView(view)}, nil
}

func awsCredentialDelete(ctx context.Context, service *coreaws.Service, params map[string]any) (any, *action.Error) {
	if e := rejectUnknownAWSFields(params, map[string]struct{}{"idempotency_key": {}, "credential_id": {}, "expected_revision": {}}); e != nil {
		return nil, e
	}
	idem, e := requiredString(params, "idempotency_key")
	if e != nil {
		return nil, e
	}
	id, e := requiredString(params, "credential_id")
	if e != nil {
		return nil, e
	}
	expected, e := requiredPositiveInt64(params, "expected_revision")
	if e != nil {
		return nil, e
	}
	if err := service.DeleteCredential(ctx, id, expected, idem); err != nil {
		return nil, mapAWSServiceError(err)
	}
	return map[string]any{"deleted": true, "credential_id": id}, nil
}

func awsCredentialList(ctx context.Context, service *coreaws.Service, params map[string]any) (any, *action.Error) {
	if e := rejectUnknownAWSFields(params, map[string]struct{}{"page_size": {}, "page_token": {}}); e != nil {
		return nil, e
	}
	size, token, e := page(params)
	if e != nil {
		return nil, e
	}
	result, err := service.ListCredentials(ctx, size, token)
	if err != nil {
		return nil, mapAWSServiceError(err)
	}
	items := make([]any, 0, len(result.Items))
	for _, view := range result.Items {
		items = append(items, awsCredentialView(view))
	}
	return map[string]any{"credentials": items, "next_page_token": result.NextPageToken}, nil
}

func awsCredentialTest(ctx context.Context, service *coreaws.Service, params map[string]any) (any, *action.Error) {
	if e := rejectUnknownAWSFields(params, map[string]struct{}{"credential_id": {}, "idempotency_key": {}, "expected_revision": {}}); e != nil {
		return nil, e
	}
	id, e := requiredString(params, "credential_id")
	if e != nil {
		return nil, e
	}
	idem, e := requiredString(params, "idempotency_key")
	if e != nil {
		return nil, e
	}
	expected, e := requiredPositiveInt64(params, "expected_revision")
	if e != nil {
		return nil, e
	}
	tested, err := service.TestCredential(ctx, id, expected, idem)
	if err != nil {
		return nil, mapAWSServiceError(err)
	}
	return map[string]any{"credential_id": tested.CredentialID, "account_id": tested.Identity.AccountID, "user_arn": tested.Identity.UserARN, "principal_id": tested.Identity.PrincipalID, "credential_revision": tested.CredentialRevision, "tested_at": tested.TestedAt}, nil
}

func awsCredentialView(view coreaws.CredentialView) map[string]any {
	out := map[string]any{"credential_id": view.ID, "name": view.Name, "region": view.Region, "account_id": view.AccountID, "user_arn": view.UserARN, "access_key_configured": view.AccessKeyConfigured, "secret_access_key_configured": view.SecretAccessKeyConfigured, "session_token_configured": view.SessionTokenConfigured, "verified_revision": view.VerifiedRevision, "revision": view.Revision, "created_at": view.CreatedAt, "updated_at": view.UpdatedAt}
	if view.VerifiedRevision == view.Revision && !view.UpdatedAt.IsZero() {
		out["tested_at"] = view.UpdatedAt
	}
	return out
}

func mapAWSServiceError(err error) *action.Error {
	switch {
	case errors.Is(err, coreaws.ErrInvalid):
		return action.CodedError(http.StatusBadRequest, "invalid_request", "invalid AWS credential request")
	case errors.Is(err, coreaws.ErrNotFound):
		return action.CodedError(http.StatusNotFound, "not_found", "AWS credential not found")
	case errors.Is(err, coreaws.ErrRevisionConflict):
		return action.CodedError(http.StatusConflict, "revision_conflict", "credential revision conflict")
	case errors.Is(err, coreaws.ErrIdempotencyConflict):
		return action.CodedError(http.StatusConflict, "idempotency_conflict", "idempotency key conflict")
	case errors.Is(err, coreaws.ErrConflict):
		return action.CodedError(http.StatusConflict, "conflict", "AWS credential conflict")
	case errors.Is(err, coreaws.ErrProvider):
		return action.CodedError(http.StatusBadGateway, "provider_error", "AWS credential test failed")
	default:
		return action.CodedError(http.StatusInternalServerError, "internal_error", "AWS credential operation failed")
	}
}
