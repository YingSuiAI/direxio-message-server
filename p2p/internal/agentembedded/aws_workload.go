package agentembedded

// This file contains the owner-scoped ProductCore adapters for the embedded
// AWS and workload surfaces. They intentionally accept only typed domain
// services; provider execution remains worker-owned and no request handler
// performs an external AWS call.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreworkload "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload"
	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	"github.com/google/uuid"
)

// AWSServiceResolver must return a service bound to owner. Keeping the owner
// binding in the resolver prevents a request from accidentally using another
// owner's repository while retaining a single embedded action port.
type AWSServiceResolver func(owner string) (*coreaws.Service, error)

// NewAWSActionPort constructs the embedded AWS facade. A nil resolver or a
// resolver that returns a service without a durable coordinator fails closed.
func NewAWSActionPort(resolve AWSServiceResolver) (ActionPort, error) {
	if resolve == nil {
		return nil, ErrUnavailable
	}
	return awsActionPort{resolve: resolve}, nil
}

type awsActionPort struct{ resolve AWSServiceResolver }

func (p awsActionPort) Handle(ctx context.Context, owner, action string, params map[string]any) (any, *actionbase.Error) {
	if p.resolve == nil || strings.TrimSpace(owner) == "" {
		return unavailable(ctx, params)
	}
	s, err := p.resolve(owner)
	if err != nil || s == nil || !s.ReadyForEmbedded() {
		return unavailable(ctx, params)
	}
	var out any
	switch action {
	case "agent.core.aws.credentials.create":
		in, ae := awsCredentialInput(params, false)
		if ae != nil {
			return nil, ae
		}
		v, err := s.SaveCredential(ctx, in)
		if err != nil {
			return nil, awsError(err)
		}
		out = map[string]any{"credential": credentialViewMap(v)}
	case "agent.core.aws.credentials.update":
		id, ae := requiredString(params, "credential_id")
		if ae != nil {
			return nil, ae
		}
		expected, ae := requiredPositiveInt64(params, "expected_revision")
		if ae != nil {
			return nil, ae
		}
		in, ae := awsCredentialInput(params, true)
		if ae != nil {
			return nil, ae
		}
		in.ID = id
		v, err := s.ReplaceCredential(ctx, in, expected, mustString(params, "idempotency_key"))
		if err != nil {
			return nil, awsError(err)
		}
		out = map[string]any{"credential": credentialViewMap(v)}
	case "agent.core.aws.credentials.delete":
		id, ae := requiredUUID(params, "credential_id")
		if ae != nil {
			return nil, ae
		}
		expected, ae := requiredPositiveInt64(params, "expected_revision")
		if ae != nil {
			return nil, ae
		}
		key, ae := requiredUUID(params, "idempotency_key")
		if ae != nil {
			return nil, ae
		}
		if err := s.DeleteCredential(ctx, id, expected, key); err != nil {
			return nil, awsError(err)
		}
		out = map[string]any{"deleted": true, "credential_id": id}
	case "agent.core.aws.credentials.list":
		size, token, ae := page(params)
		if ae != nil {
			return nil, ae
		}
		v, err := s.ListCredentials(ctx, size, token)
		if err != nil {
			return nil, awsError(err)
		}
		items := make([]any, 0, len(v.Items))
		for _, x := range v.Items {
			items = append(items, credentialViewMap(x))
		}
		out = map[string]any{"credentials": items, "next_page_token": v.NextPageToken}
	case "agent.core.aws.credentials.test":
		id, ae := requiredUUID(params, "credential_id")
		if ae != nil {
			return nil, ae
		}
		v, err := s.TestCredential(ctx, id)
		if err != nil {
			return nil, awsError(err)
		}
		out = map[string]any{"credential_id": v.CredentialID, "account_id": v.Identity.AccountID, "user_arn": v.Identity.UserARN, "principal_id": v.Identity.PrincipalID, "credential_revision": v.CredentialRevision, "tested_at": v.TestedAt.UTC().Format(time.RFC3339Nano)}
	case "agent.core.aws.plans.get":
		id, ae := requiredUUID(params, "plan_id")
		if ae != nil {
			return nil, ae
		}
		v, err := s.GetPlan(ctx, id)
		if err != nil {
			return nil, awsError(err)
		}
		out = map[string]any{"plan": planViewMap(v)}
	case "agent.core.aws.plans.list":
		size, token, ae := page(params)
		if ae != nil {
			return nil, ae
		}
		v, err := s.ListPlans(ctx, size, token)
		if err != nil {
			return nil, awsError(err)
		}
		items := make([]any, 0, len(v.Items))
		for _, x := range v.Items {
			items = append(items, planViewMap(x))
		}
		out = map[string]any{"plans": items, "next_page_token": v.NextPageToken}
	case "agent.core.aws.plans.quote":
		id, ae := requiredUUID(params, "plan_id")
		if ae != nil {
			return nil, ae
		}
		v, err := s.Quote(ctx, id)
		if err != nil {
			return nil, awsError(err)
		}
		out = map[string]any{"quote": quoteMap(v)}
	case "agent.core.aws.ec2_provisions.plan":
		in, key, ae := ec2ProvisionInput(params, owner)
		if ae != nil {
			return nil, ae
		}
		v, err := s.CreateEC2Provision(ctx, in, key)
		if err != nil {
			return nil, awsError(err)
		}
		out = map[string]any{"plan": ec2ProvisionPlanMap(v.Plan, v.Provision), "quote": quoteMap(v.Quote), "provision": provisionMap(v.Provision)}
	case "agent.core.aws.ec2_provisions.get":
		id, ae := requiredUUID(params, "provision_id")
		if ae != nil {
			return nil, ae
		}
		v, err := s.GetProvisionForOwner(ctx, id, owner)
		if err != nil {
			return nil, awsError(err)
		}
		out = map[string]any{"provision": provisionMap(v)}
	case "agent.core.aws.ec2_provisions.list":
		size, token, ae := page(params)
		if ae != nil {
			return nil, ae
		}
		state, ae := optionalString(params, "state")
		if ae != nil {
			return nil, ae
		}
		v, err := s.ListProvisions(ctx, owner, state, size, token)
		if err != nil {
			return nil, awsError(err)
		}
		items := make([]any, 0, len(v.Items))
		for _, x := range v.Items {
			if x.OwnerDigest == coreaws.OwnerBindingDigest(owner) {
				items = append(items, provisionMap(x))
			}
		}
		out = map[string]any{"provisions": items, "next_page_token": v.NextPageToken}
	case "agent.core.aws.ec2_provisions.events":
		id, ae := requiredUUID(params, "provision_id")
		if ae != nil {
			return nil, ae
		}
		if _, err := s.GetProvisionForOwner(ctx, id, owner); err != nil {
			return nil, awsError(err)
		}
		after, ae := optionalUint64(params, "after_sequence")
		if ae != nil {
			return nil, ae
		}
		limit := 0
		if _, ok := params["limit"]; ok {
			n, e := optionalInt64(params, "limit")
			if e != nil {
				return nil, e
			}
			if n < 0 {
				return nil, actionbase.BadRequest("limit must be nonnegative")
			}
			limit = int(n)
		}
		events, next, err := s.ListProvisionEvents(ctx, id, owner, after, limit)
		if err != nil {
			return nil, awsError(err)
		}
		items := make([]any, 0, len(events))
		for _, x := range events {
			items = append(items, provisionEventMap(x))
		}
		out = map[string]any{"events": items, "next_after_sequence": next}
	case "agent.core.aws.ec2_provisions.create.request":
		if ae := ensureExactFields(params, "provision_id", "expected_revision", "idempotency_key"); ae != nil {
			return nil, ae
		}
		id, ae := requiredUUID(params, "provision_id")
		if ae != nil {
			return nil, ae
		}
		expected, ae := requiredPositiveInt64(params, "expected_revision")
		if ae != nil {
			return nil, ae
		}
		key, ae := requiredUUID(params, "idempotency_key")
		if ae != nil {
			return nil, ae
		}
		v, err := s.RequestEC2Create(ctx, id, expected, key, owner)
		if err != nil {
			return nil, awsError(err)
		}
		out = changeRequestMap(v)
	case "agent.core.aws.ec2_provisions.destroy.request":
		if ae := ensureExactFields(params, "provision_id", "expected_revision", "idempotency_key"); ae != nil {
			return nil, ae
		}
		id, ae := requiredUUID(params, "provision_id")
		if ae != nil {
			return nil, ae
		}
		expected, ae := requiredPositiveInt64(params, "expected_revision")
		if ae != nil {
			return nil, ae
		}
		key, ae := requiredUUID(params, "idempotency_key")
		if ae != nil {
			return nil, ae
		}
		v, err := s.RequestEC2Destroy(ctx, id, expected, key, owner)
		if err != nil {
			return nil, awsError(err)
		}
		out = changeRequestMap(v)
	case "agent.core.aws.ec2_provisions.retry":
		if ae := ensureExactFields(params, "provision_id", "expected_revision", "idempotency_key"); ae != nil {
			return nil, ae
		}
		id, ae := requiredUUID(params, "provision_id")
		if ae != nil {
			return nil, ae
		}
		expected, ae := requiredPositiveInt64(params, "expected_revision")
		if ae != nil {
			return nil, ae
		}
		key, ae := requiredUUID(params, "idempotency_key")
		if ae != nil {
			return nil, ae
		}
		v, err := s.RetryEC2Provision(ctx, id, expected, key, owner)
		if err != nil {
			return nil, awsError(err)
		}
		out = map[string]any{"provision": provisionMap(v)}
	case "agent.core.aws.changes.get":
		id, ae := requiredUUID(params, "change_id")
		if ae != nil {
			return nil, ae
		}
		v, err := s.GetChange(ctx, id)
		if err != nil {
			return nil, awsError(err)
		}
		out = map[string]any{"change": changeMap(v)}
	case "agent.core.aws.changes.list":
		size, token, ae := page(params)
		if ae != nil {
			return nil, ae
		}
		planID, ae := optionalUUID(params, "plan_id")
		if ae != nil {
			return nil, ae
		}
		v, err := s.ListChanges(ctx, size, planID, token)
		if err != nil {
			return nil, awsError(err)
		}
		items := make([]any, 0, len(v.Items))
		for _, x := range v.Items {
			items = append(items, changeMap(x))
		}
		out = map[string]any{"changes": items, "next_page_token": v.NextPageToken}
	case "agent.core.aws.changes.status":
		id, ae := requiredUUID(params, "change_id")
		if ae != nil {
			return nil, ae
		}
		v, err := s.GetChange(ctx, id)
		if err != nil {
			return nil, awsError(err)
		}
		out = map[string]any{"change": changeMap(v), "status": string(v.Status), "stage": string(v.Stage)}
	default:
		return nil, actionbase.CodedError(http.StatusNotFound, "agent_action_not_found", "unsupported AWS action")
	}
	return out, nil
}

func awsCredentialInput(p map[string]any, update bool) (coreaws.CredentialInput, *actionbase.Error) {
	key, e := requiredUUID(p, "idempotency_key")
	if e != nil {
		return coreaws.CredentialInput{}, e
	}
	in := coreaws.CredentialInput{IdempotencyKey: key}
	for _, field := range []string{"name", "region", "access_key_id", "secret_access_key", "session_token"} {
		if raw, ok := p[field]; ok {
			s, ok := raw.(string)
			if !ok {
				return in, actionbase.BadRequest(field + " must be a string")
			}
			switch field {
			case "name":
				in.Name = s
			case "region":
				in.Region = s
			case "access_key_id":
				in.AccessKeyID = s
			case "secret_access_key":
				in.SecretAccessKey = s
			case "session_token":
				in.SessionToken = s
			}
		}
	}
	if !update && (in.Name == "" || in.Region == "" || in.AccessKeyID == "" || in.SecretAccessKey == "") {
		return in, actionbase.BadRequest("name, region, access_key_id and secret_access_key are required")
	}
	return in, nil
}

func credentialViewMap(v coreaws.CredentialView) map[string]any {
	out := map[string]any{"credential_id": v.ID, "name": v.Name, "region": v.Region, "account_id": v.AccountID, "user_arn": v.UserARN, "access_key_configured": v.AccessKeyConfigured, "secret_access_key_configured": v.SecretAccessKeyConfigured, "session_token_configured": v.SessionTokenConfigured, "has_access_key": v.AccessKeyConfigured, "has_secret_key": v.SecretAccessKeyConfigured, "has_session_token": v.SessionTokenConfigured, "revision": v.Revision, "verified_revision": v.VerifiedRevision, "created_at": v.CreatedAt.UTC().Format(time.RFC3339Nano), "updated_at": v.UpdatedAt.UTC().Format(time.RFC3339Nano)}
	if v.VerifiedRevision == v.Revision && v.VerifiedRevision > 0 {
		out["tested_at"] = v.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	return out
}
func planViewMap(v coreaws.PlanView) map[string]any {
	return map[string]any{"plan_id": v.ID, "credential_id": v.CredentialID, "credential_revision": v.CredentialRevision, "region": v.Region, "stack_name": v.StackName, "operation": string(v.Operation), "template_sha256": v.TemplateSHA256, "parameters": v.Parameters, "tags": v.Tags, "capabilities": v.Capabilities, "revision": v.Revision, "created_at": v.CreatedAt.UTC().Format(time.RFC3339Nano)}
}
func quoteMap(v coreaws.Quote) map[string]any {
	return map[string]any{"plan_id": v.PlanID, "operation": string(v.Operation), "region": v.Region, "stack_name": v.StackName, "resource_count": v.ResourceCount, "parameter_count": v.ParameterCount, "tag_count": v.TagCount, "estimated_monthly_usd": v.EstimatedMonthlyUSD, "price_status": v.PriceStatus, "summary": v.Summary, "plan_digest": v.PlanDigest}
}
func changeMap(v coreaws.Change) map[string]any {
	out := map[string]any{"change_id": v.ID, "plan_id": v.PlanID, "credential_id": v.CredentialID, "task_id": v.TaskID, "confirmation_id": v.ConfirmationID, "operation": string(v.Operation), "status": string(v.Status), "stage": string(v.Stage), "change_set_id": v.ChangeSetID, "provider_request_digest": v.ProviderRequestDigest, "revision": v.Revision, "error_code": v.ErrorCode, "error_summary": v.ErrorSummary, "created_at": v.CreatedAt.UTC().Format(time.RFC3339Nano), "updated_at": v.UpdatedAt.UTC().Format(time.RFC3339Nano)}
	if v.ProvisionID != "" { out["provision_id"] = v.ProvisionID }
	return out
}

func provisionMap(v coreaws.Provision) map[string]any {
	out := map[string]any{"provision_id": v.ID, "plan_id": v.PlanID, "credential_id": v.CredentialID, "credential_revision": v.CredentialRevision, "region": v.Region, "stack_name": v.StackName, "profile": v.Profile, "plan_revision": v.PlanRevision, "template_sha256": v.TemplateSHA256, "plan_digest": v.PlanDigest, "state": v.State, "revision": v.Revision, "create_change_id": v.CreateChangeID, "destroy_change_id": v.DestroyChangeID, "active_change_id": v.ActiveChangeID, "reconciliation_required": v.ReconciliationRequired, "error_code": v.ErrorCode, "error_summary": v.ErrorSummary, "created_at": v.CreatedAt.UTC().Format(time.RFC3339Nano), "updated_at": v.UpdatedAt.UTC().Format(time.RFC3339Nano)}
	if v.CreateChangeID == "" {
		delete(out, "create_change_id")
	}
	if v.DestroyChangeID == "" {
		delete(out, "destroy_change_id")
	}
	if v.ActiveChangeID == "" {
		delete(out, "active_change_id")
	}
	if v.ErrorCode == "" {
		delete(out, "error_code")
	}
	if v.ErrorSummary == "" {
		delete(out, "error_summary")
	}
	if v.Readback.Validate() == nil {
		out["readback"] = map[string]any{"stack_id": v.Readback.StackID, "instance_id": v.Readback.InstanceID, "public_ip": v.Readback.PublicIP, "security_group_id": v.Readback.SecurityGroupID, "output_digest": v.Readback.OutputDigest, "observed_at": v.Readback.ObservedAt.UTC().Format(time.RFC3339Nano)}
	} else {
		out["readback"] = nil
	}
	return out
}
func ec2ProvisionPlanMap(v coreaws.PlanView, p coreaws.Provision) map[string]any {
	volume, _ := strconv.ParseInt(v.Parameters["VolumeSize"], 10, 64)
	return map[string]any{"plan_id": v.ID, "credential_id": v.CredentialID, "credential_revision": v.CredentialRevision, "region": v.Region, "stack_name": v.StackName, "display_name": v.Parameters["DisplayName"], "instance_type": v.Parameters["InstanceType"], "volume_gib": volume, "public_http": v.Parameters["PublicHTTP"] == "true", "acknowledge_public_exposure": true, "operation": string(v.Operation), "template_sha256": v.TemplateSHA256, "plan_digest": p.PlanDigest, "revision": v.Revision, "created_at": v.CreatedAt.UTC().Format(time.RFC3339Nano)}
}
func provisionEventMap(v coreaws.ProvisionEvent) map[string]any {
	return map[string]any{"provision_id": v.ProvisionID, "event_id": v.EventID, "change_id": v.ChangeID, "task_id": v.TaskID, "kind": v.Kind, "sequence": v.Sequence, "revision": v.Revision, "at": v.At.UTC().Format(time.RFC3339Nano)}
}
func changeRequestMap(v coreaws.ChangeRequestResult) map[string]any {
	out := map[string]any{"change": changeMap(v.Change), "task_id": v.Task.ID, "task": map[string]any{"task_id": v.Task.ID, "status": v.Task.Status, "revision": v.Task.Revision, "plan_id": v.Task.PlanID, "confirmation_id": v.Task.ConfirmationID}, "confirmation_id": v.Confirmation.ConfirmationID, "confirmation": confirmationMap(v.Confirmation)}
	out["task"].(map[string]any)["attempt"] = v.Task.Attempt
	out["task"].(map[string]any)["lease_epoch"] = v.Task.LeaseEpoch
	out["task"].(map[string]any)["failure_code"] = v.Task.FailureCode
	out["task"].(map[string]any)["failure_summary"] = v.Task.FailureSummary
	if v.Provision.ID != "" {
		out["provision"] = provisionMap(v.Provision)
	}
	return out
}

func ec2ProvisionInput(p map[string]any, owner string) (coreaws.EC2ProvisionRequest, string, *actionbase.Error) {
	allowed := map[string]bool{"credential_id": true, "expected_credential_revision": true, "region": true, "stack_name": true, "display_name": true, "instance_type": true, "volume_gib": true, "public_http": true, "acknowledge_public_exposure": true, "idempotency_key": true}
	for key := range p {
		if !allowed[key] {
			return coreaws.EC2ProvisionRequest{}, "", actionbase.BadRequest("unsupported field: " + key)
		}
	}
	key, ae := requiredUUID(p, "idempotency_key")
	if ae != nil {
		return coreaws.EC2ProvisionRequest{}, "", ae
	}
	credential, ae := requiredUUID(p, "credential_id")
	if ae != nil {
		return coreaws.EC2ProvisionRequest{}, "", ae
	}
	expected, ae := requiredPositiveInt64(p, "expected_credential_revision")
	if ae != nil {
		return coreaws.EC2ProvisionRequest{}, "", ae
	}
	region, ae := requiredString(p, "region")
	if ae != nil {
		return coreaws.EC2ProvisionRequest{}, "", ae
	}
	stack, ae := requiredString(p, "stack_name")
	if ae != nil {
		return coreaws.EC2ProvisionRequest{}, "", ae
	}
	display, ae := requiredString(p, "display_name")
	if ae != nil {
		return coreaws.EC2ProvisionRequest{}, "", ae
	}
	instance, ae := requiredString(p, "instance_type")
	if ae != nil {
		return coreaws.EC2ProvisionRequest{}, "", ae
	}
	volume, ae := requiredPositiveInt64(p, "volume_gib")
	if ae != nil {
		return coreaws.EC2ProvisionRequest{}, "", ae
	}
	publicHTTP, ok := p["public_http"].(bool)
	if !ok {
		return coreaws.EC2ProvisionRequest{}, "", actionbase.BadRequest("public_http must be a boolean")
	}
	ack, ok := p["acknowledge_public_exposure"].(bool)
	if !ok || !ack {
		return coreaws.EC2ProvisionRequest{}, "", actionbase.BadRequest("acknowledge_public_exposure must be true")
	}
	return coreaws.EC2ProvisionRequest{OwnerID: owner, CredentialID: credential, CredentialRevision: expected, Region: region, StackName: stack, DisplayName: display, InstanceType: instance, VolumeGiB: volume, PublicHTTP: publicHTTP, AcknowledgePublicExposure: ack}, key, nil
}

func ensureExactFields(p map[string]any, fields ...string) *actionbase.Error {
	allowed := make(map[string]bool, len(fields))
	for _, key := range fields {
		allowed[key] = true
	}
	for key := range p {
		if !allowed[key] {
			return actionbase.BadRequest("unsupported field: " + key)
		}
	}
	return nil
}

// WorkloadServiceResolver returns owner-bound durable service/handler pairs.
type WorkloadServiceResolver func(owner string) (*coreworkload.Service, *coreworkload.Handler, error)

func NewWorkloadActionPort(resolve WorkloadServiceResolver) (ActionPort, error) {
	if resolve == nil {
		return nil, ErrUnavailable
	}
	return workloadActionPort{resolve: resolve}, nil
}

type workloadActionPort struct{ resolve WorkloadServiceResolver }

func (p workloadActionPort) Handle(ctx context.Context, owner, action string, params map[string]any) (any, *actionbase.Error) {
	// Gate the legacy/raw EC2 SSM plan shape before resolving an owner-bound
	// service. This keeps the public action fail-closed even when the embedded
	// runtime is unavailable and ensures no secret/provider lookup can occur.
	if action == "agent.core.workloads.plan" {
		if _, ae := rejectRawSSMWorkloadPlan(params); ae != nil {
			return nil, ae
		}
	}
	if p.resolve == nil || strings.TrimSpace(owner) == "" {
		return unavailable(ctx, params)
	}
	s, h, err := p.resolve(owner)
	if err != nil || s == nil || h == nil || !s.ReadyForEmbedded(h) {
		return unavailable(ctx, params)
	}
	switch action {
	case "agent.core.workloads.plan":
		in, ae := workloadPlanInput(params, s.PinsAWSCredentialGrants())
		if ae != nil {
			return nil, ae
		}
		v, err := s.CreatePlan(ctx, in)
		if err != nil {
			return nil, workloadError(err)
		}
		return map[string]any{"plan": workloadPlanMap(v)}, nil
	case "agent.core.workloads.get":
		id, ae := requiredUUID(params, "plan_id")
		if ae != nil {
			return nil, ae
		}
		v, err := s.GetPlan(ctx, id)
		if err != nil {
			return nil, workloadError(err)
		}
		return map[string]any{"plan": workloadPlanMap(v)}, nil
	case "agent.core.workloads.list":
		size, token, ae := page(params)
		if ae != nil {
			return nil, ae
		}
		v, next, err := s.ListPlans(ctx, size, token)
		if err != nil {
			return nil, workloadError(err)
		}
		items := make([]any, 0, len(v))
		for _, x := range v {
			items = append(items, workloadPlanMap(x))
		}
		return map[string]any{"plans": items, "next_page_token": next}, nil
	case "agent.core.workloads.quote":
		id, ae := requiredUUID(params, "plan_id")
		if ae != nil {
			return nil, ae
		}
		v, err := s.Quote(ctx, id)
		if err != nil {
			return nil, workloadError(err)
		}
		quote := map[string]any{"plan_id": v.PlanID, "plan_digest": v.PlanDigest, "summary": v.Summary}
		if v.ID != "" {
			quote["quote_id"] = v.ID
		}
		if !v.ExpiresAt.IsZero() {
			quote["expires_at"] = v.ExpiresAt.UTC().Format(time.RFC3339Nano)
		}
		if !v.CreatedAt.IsZero() {
			quote["created_at"] = v.CreatedAt.UTC().Format(time.RFC3339Nano)
		}
		return map[string]any{"quote": quote}, nil
	case "agent.core.workloads.apply", "agent.core.workloads.destroy":
		planID, ae := requiredUUID(params, "plan_id")
		if ae != nil {
			return nil, ae
		}
		key, ae := requiredUUID(params, "idempotency_key")
		if ae != nil {
			return nil, ae
		}
		workloadID, ae := optionalUUID(params, "workload_id")
		if ae != nil {
			return nil, ae
		}
		var r coreworkload.RequestResult
		if action[len(action)-5:] == "apply" {
			r, err = s.RequestApply(ctx, coreworkload.RequestApplyInput{PlanID: planID, WorkloadID: workloadID, IdempotencyKey: key})
		} else {
			r, err = s.RequestDestroy(ctx, coreworkload.RequestDestroyInput{PlanID: planID, WorkloadID: workloadID, IdempotencyKey: key})
		}
		if err != nil {
			return nil, workloadError(err)
		}
		operation, err := workloadOperationProjection(ctx, s, r.Operation)
		if err != nil {
			return nil, workloadError(err)
		}
		return map[string]any{"operation": operation, "confirmation": confirmationMap(r.Confirmation), "task_id": r.Task.ID}, nil
	case "agent.core.workloads.operations.get":
		id, ae := requiredUUID(params, "operation_id")
		if ae != nil {
			return nil, ae
		}
		v, err := s.GetOperation(ctx, id)
		if err != nil {
			return nil, workloadError(err)
		}
		operation, err := workloadOperationProjection(ctx, s, v)
		if err != nil {
			return nil, workloadError(err)
		}
		return map[string]any{"operation": operation}, nil
	case "agent.core.workloads.operations.events":
		id, ae := requiredUUID(params, "operation_id")
		if ae != nil {
			return nil, ae
		}
		after, ae := optionalUint64(params, "after_sequence")
		if ae != nil {
			return nil, ae
		}
		v, err := s.ListEvents(ctx, id, after)
		if err != nil {
			return nil, workloadError(err)
		}
		items := make([]any, 0, len(v))
		for _, x := range v {
			item, mapErr := workloadEventMap(x)
			if mapErr != nil {
				return nil, workloadError(mapErr)
			}
			items = append(items, item)
		}
		return map[string]any{"events": items}, nil
	case "agent.core.workloads.actual.get":
		id, ae := requiredUUID(params, "workload_id")
		if ae != nil {
			return nil, ae
		}
		v, err := s.GetWorkload(ctx, id)
		if err != nil {
			return nil, workloadError(err)
		}
		return map[string]any{"workload": workloadActualMap(v.Actual)}, nil
	default:
		return nil, actionbase.CodedError(http.StatusNotFound, "agent_action_not_found", "unsupported workload action")
	}
}

func workloadPlanInput(p map[string]any, awsCredentialPinningReady bool) (coreworkload.PlanInput, *actionbase.Error) {
	key, e := requiredUUID(p, "idempotency_key")
	if e != nil {
		return coreworkload.PlanInput{}, e
	}
	summary, e := requiredString(p, "summary")
	if e != nil {
		return coreworkload.PlanInput{}, e
	}
	artifact, e := requiredString(p, "artifact")
	if e != nil {
		return coreworkload.PlanInput{}, e
	}
	source, e := requiredString(p, "source")
	if e != nil {
		return coreworkload.PlanInput{}, e
	}
	targetKind, e := normalizeWorkloadTargetKind(p)
	if e != nil {
		return coreworkload.PlanInput{}, e
	}
	if targetKind == coreworkload.TargetCoreRunner {
		return coreworkload.PlanInput{}, actionbase.BadRequest("CORE_RUNNER workload targets are not supported")
	}
	if targetKind == coreworkload.TargetAWSEC2SSM {
		return coreworkload.PlanInput{}, actionbase.CodedError(http.StatusBadRequest, "agent_typed_ssm_required", "AWS EC2 SSM workloads must use the typed EC2 provision/install workflow")
	}
	raw, e := requiredMap(p, "typed_target")
	if e != nil {
		return coreworkload.PlanInput{}, e
	}
	target, targetErr := decodeTargetSettings(raw, targetKind)
	if targetErr != nil {
		return coreworkload.PlanInput{}, targetErr
	}
	expires, e := requiredTime(p, "expires_at")
	if e != nil {
		return coreworkload.PlanInput{}, e
	}
	in := coreworkload.PlanInput{IdempotencyKey: key, Summary: summary, Artifact: artifact, Source: source, TargetKind: targetKind, Target: target, ExpiresAt: expires}
	if v, ok := p["command_steps"]; ok {
		in.CommandSteps, e = stringSlice(v)
		if e != nil {
			return in, e
		}
	}
	if v, ok := p["image_digest"].(string); ok {
		in.ImageDigest = v
	}
	if v, ok := p["image_uri"].(string); ok {
		in.ImageURI = v
	}
	if raw, ok := p["typed_resource_limits"].(map[string]any); ok {
		b, _ := json.Marshal(raw)
		_ = json.Unmarshal(b, &in.ResourceLimits)
	}
	if raw, ok := p["typed_secret_grants"].([]any); ok {
		in.SecretGrantRefs = make([]coreworkload.SecretGrantRef, 0, len(raw))
		for _, item := range raw {
			m, ok := item.(map[string]any)
			if !ok {
				return in, actionbase.BadRequest("typed_secret_grants must contain objects")
			}
			ref, ok := m["reference_id"].(string)
			purpose, pok := m["purpose"].(string)
			digest, dok := m["binding_digest"].(string)
			_, digestPresent := m["binding_digest"]
			if !ok || !pok || strings.TrimSpace(ref) == "" || strings.TrimSpace(purpose) == "" {
				return in, actionbase.BadRequest("typed_secret_grants entries are invalid")
			}
			secretPurpose := coreconfirmation.SecretPurpose(purpose)
			revision, re := optionalInt64(m, "secret_revision")
			if re != nil || revision < 0 {
				return in, actionbase.BadRequest("typed_secret_grants.secret_revision must be nonnegative")
			}
			if secretPurpose == coreconfirmation.SecretPurposeAWSCredential && revision < 1 {
				return in, actionbase.BadRequest("typed_secret_grants.secret_revision is required for AWS credentials")
			}
			if secretPurpose != coreconfirmation.SecretPurposeAWSCredential && revision != 0 {
				return in, actionbase.BadRequest("typed_secret_grants.secret_revision is only valid for AWS credentials")
			}
			if secretPurpose == coreconfirmation.SecretPurposeAWSCredential && !awsCredentialPinningReady {
				return in, actionbase.CodedError(http.StatusPreconditionFailed, "agent_embedded_unavailable", "AWS credential grant pinning is unavailable")
			}
			if digestPresent && !dok {
				return in, actionbase.BadRequest("typed_secret_grants.binding_digest must be a string")
			}
			if !digestPresent {
				if secretPurpose != coreconfirmation.SecretPurposeAWSCredential {
					return in, actionbase.BadRequest("typed_secret_grants.binding_digest is required")
				}
				// Postgres pins AWS credential grants to the owner-bound encrypted
				// secret digest after this request reaches the store. Normalize still
				// requires a syntactically valid digest at the handler seam.
				digest = strings.Repeat("0", 64)
			}
			if strings.TrimSpace(digest) == "" {
				return in, actionbase.BadRequest("typed_secret_grants.binding_digest is required")
			}
			in.SecretGrantRefs = append(in.SecretGrantRefs, coreworkload.SecretGrantRef{ReferenceID: ref, Purpose: secretPurpose, Revision: revision, BindingDigest: coreconfirmation.Digest(digest)})
		}
	}
	return in, nil
}

func normalizeWorkloadTargetKind(p map[string]any) (coreworkload.TargetKind, *actionbase.Error) {
	kind, e := requiredString(p, "target_kind")
	if e != nil {
		return "", e
	}
	return coreworkload.TargetKind(strings.ToUpper(strings.ReplaceAll(kind, "-", "_"))), nil
}

func rejectRawSSMWorkloadPlan(p map[string]any) (coreworkload.TargetKind, *actionbase.Error) {
	targetKind, e := normalizeWorkloadTargetKind(p)
	if e != nil {
		return "", e
	}
	if targetKind == coreworkload.TargetAWSEC2SSM {
		return "", actionbase.CodedError(http.StatusBadRequest, "agent_typed_ssm_required", "AWS EC2 SSM workloads must use the typed EC2 provision/install workflow")
	}
	return targetKind, nil
}

func decodeTargetSettings(raw map[string]any, kind coreworkload.TargetKind) (coreworkload.TargetSettings, *actionbase.Error) {
	identityRaw, ok := raw["identity"].(map[string]any)
	if !ok {
		return coreworkload.TargetSettings{}, actionbase.BadRequest("typed_target.identity is required")
	}
	get := func(m map[string]any, key string) string { s, _ := m[key].(string); return s }
	getInt := func(m map[string]any, key string) (int64, *actionbase.Error) { return optionalInt64(m, key) }
	getBool := func(m map[string]any, key string) (bool, *actionbase.Error) {
		v, ok := m[key]
		if !ok || v == nil {
			return false, nil
		}
		value, ok := v.(bool)
		if !ok {
			return false, actionbase.BadRequest("typed_target." + key + " must be a boolean")
		}
		return value, nil
	}
	getStrings := func(m map[string]any, key string) ([]string, *actionbase.Error) {
		v, ok := m[key]
		if !ok || v == nil {
			return nil, nil
		}
		switch rawValues := v.(type) {
		case []string:
			return append([]string(nil), rawValues...), nil
		case []any:
			values := make([]string, len(rawValues))
			for i, item := range rawValues {
				value, ok := item.(string)
				if !ok {
					return nil, actionbase.BadRequest("typed_target." + key + " must contain strings")
				}
				values[i] = value
			}
			return values, nil
		default:
			return nil, actionbase.BadRequest("typed_target." + key + " must be an array")
		}
	}
	getStringMap := func(m map[string]any, key string) (map[string]string, *actionbase.Error) {
		v, ok := m[key]
		if !ok || v == nil {
			return nil, nil
		}
		values := map[string]string{}
		switch rawValues := v.(type) {
		case map[string]string:
			for name, value := range rawValues {
				values[name] = value
			}
		case map[string]any:
			for name, item := range rawValues {
				value, ok := item.(string)
				if !ok {
					return nil, actionbase.BadRequest("typed_target." + key + " values must be strings")
				}
				values[name] = value
			}
		default:
			return nil, actionbase.BadRequest("typed_target." + key + " must be an object")
		}
		return values, nil
	}
	i := coreworkload.TargetIdentity{Kind: kind, AccountID: get(identityRaw, "aws_account_id"), Region: get(identityRaw, "aws_region"), InstanceID: get(identityRaw, "instance_id"), Cluster: get(identityRaw, "cluster"), Service: get(identityRaw, "service"), TaskDefinitionRevision: get(identityRaw, "task_definition_revision"), Endpoint: get(identityRaw, "endpoint"), CoreRunnerID: get(identityRaw, "core_runner_id"), CoreRunnerService: get(identityRaw, "core_runner_service"), ImageDigest: get(identityRaw, "image_digest")}
	desiredCount, e := getInt(identityRaw, "desired_count")
	if e != nil {
		return coreworkload.TargetSettings{}, e
	}
	i.DesiredCount = desiredCount
	assignPublicIP, e := getBool(identityRaw, "aws_ecs_assign_public_ip")
	if e != nil {
		return coreworkload.TargetSettings{}, e
	}
	ecsDesiredCount, e := getInt(identityRaw, "aws_ecs_desired_count")
	if e != nil {
		return coreworkload.TargetSettings{}, e
	}
	targetGroupPort, e := getInt(identityRaw, "aws_ecs_target_group_port")
	if e != nil {
		return coreworkload.TargetSettings{}, e
	}
	if targetGroupPort < 0 || targetGroupPort > 65535 {
		return coreworkload.TargetSettings{}, actionbase.BadRequest("typed_target.aws_ecs_target_group_port must be between 0 and 65535")
	}
	subnetIDs, e := getStrings(identityRaw, "aws_ecs_subnet_ids")
	if e != nil {
		return coreworkload.TargetSettings{}, e
	}
	securityGroupIDs, e := getStrings(identityRaw, "aws_ecs_security_group_ids")
	if e != nil {
		return coreworkload.TargetSettings{}, e
	}
	requiredTags, e := getStringMap(identityRaw, "aws_ec2_required_instance_tags")
	if e != nil {
		return coreworkload.TargetSettings{}, e
	}
	t := coreworkload.TargetSettings{Identity: i, AccountID: i.AccountID, Region: i.Region, InstanceID: i.InstanceID, Cluster: i.Cluster, Service: i.Service, ECSClusterARN: get(identityRaw, "aws_ecs_cluster_arn"), ECSServiceName: get(identityRaw, "aws_ecs_service_name"), ECSTaskFamily: get(identityRaw, "aws_ecs_task_family"), ECSPlatformVersion: get(identityRaw, "aws_ecs_platform_version"), ECSSubnetIDs: subnetIDs, ECSSecurityGroupIDs: securityGroupIDs, ECSAssignPublicIP: assignPublicIP, ECSTargetGroupARN: get(identityRaw, "aws_ecs_target_group_arn"), ECSTargetGroupPort: uint32(targetGroupPort), ECSTaskRoleARN: get(identityRaw, "aws_ecs_task_role_arn"), ECSExecutionRoleARN: get(identityRaw, "aws_ecs_execution_role_arn"), ECSDesiredCount: ecsDesiredCount, ECSImageURI: get(identityRaw, "aws_ecs_image_uri"), EC2DocumentVersion: get(identityRaw, "aws_ec2_document_version"), EC2SystemdService: get(identityRaw, "aws_ec2_systemd_service"), RequiredInstanceTags: requiredTags}
	if ports, ok := raw["ports"]; ok {
		items, ok := ports.([]any)
		if !ok {
			return coreworkload.TargetSettings{}, actionbase.BadRequest("typed_target.ports must be an array")
		}
		t.PortDetails = make([]coreworkload.Port, 0, len(items))
		t.Ports = make([]int32, 0, len(items))
		for _, item := range items {
			portMap, ok := item.(map[string]any)
			if !ok {
				return coreworkload.TargetSettings{}, actionbase.BadRequest("typed_target.ports must contain objects")
			}
			port, pe := optionalInt64(portMap, "port")
			if pe != nil || port < 1 || port > 65535 {
				return coreworkload.TargetSettings{}, actionbase.BadRequest("typed_target.ports.port must be between 1 and 65535")
			}
			t.PortDetails = append(t.PortDetails, coreworkload.Port{Port: uint32(port)})
			t.Ports = append(t.Ports, int32(port))
		}
	}
	if grants, ok := raw["network_grants"]; ok {
		items, ok := grants.([]any)
		if !ok {
			return coreworkload.TargetSettings{}, actionbase.BadRequest("typed_target.network_grants must be an array")
		}
		t.NetworkGrantDetails = make([]coreworkload.NetworkGrant, 0, len(items))
		for _, item := range items {
			grant, ok := item.(map[string]any)
			if !ok {
				return coreworkload.TargetSettings{}, actionbase.BadRequest("typed_target.network_grants must contain objects")
			}
			ref, rok := grant["reference_id"].(string)
			grantKind, kok := grant["kind"].(string)
			if !rok || !kok || strings.TrimSpace(ref) == "" || strings.TrimSpace(grantKind) == "" {
				return coreworkload.TargetSettings{}, actionbase.BadRequest("typed_target.network_grants entries are invalid")
			}
			t.NetworkGrantDetails = append(t.NetworkGrantDetails, coreworkload.NetworkGrant{ReferenceID: strings.TrimSpace(ref), Kind: strings.TrimSpace(grantKind)})
		}
	}
	if labels, e := getStringMap(raw, "labels"); e != nil {
		return coreworkload.TargetSettings{}, e
	} else {
		t.Labels = labels
	}
	return t, nil
}
func workloadPlanMap(v coreworkload.Plan) map[string]any {
	return map[string]any{
		"plan_id": v.ID, "revision": v.Revision, "digest": v.Digest,
		"summary": v.Summary, "artifact": v.Artifact, "source": v.Source,
		"command_steps": append([]string{}, v.CommandSteps...),
		"image_digest":  v.ImageDigest, "image_uri": v.ImageURI,
		"target_kind":           workloadTargetKindWire(v.TargetKind),
		"expires_at":            v.ExpiresAt.UTC().Format(time.RFC3339Nano),
		"created_at":            v.CreatedAt.UTC().Format(time.RFC3339Nano),
		"typed_target":          targetSettingsMap(v.Target),
		"typed_resource_limits": resourceLimitsMap(v.ResourceLimits),
		"typed_secret_grants":   workloadSecretGrantRefsMap(v.SecretGrantRefs),
	}
}
func targetSettingsMap(v coreworkload.TargetSettings) map[string]any {
	i := v.Identity
	ports := make([]any, 0, len(v.PortDetails)+len(v.Ports))
	if len(v.PortDetails) > 0 {
		for _, port := range v.PortDetails {
			ports = append(ports, map[string]any{"port": port.Port})
		}
	} else {
		for _, port := range v.Ports {
			ports = append(ports, map[string]any{"port": port})
		}
	}
	grants := make([]any, 0, len(v.NetworkGrantDetails))
	for _, grant := range v.NetworkGrantDetails {
		grants = append(grants, map[string]any{
			"reference_id": grant.ReferenceID,
			"kind":         grant.Kind,
		})
	}
	return map[string]any{
		"identity":       workloadIdentityMap(i, &v),
		"ports":          ports,
		"network_grants": grants,
		"labels":         copyStringMap(v.Labels),
	}
}
func workloadOperationProjection(ctx context.Context, service *coreworkload.Service, operation coreworkload.Operation) (map[string]any, error) {
	plan, err := service.GetPlan(ctx, operation.PlanID)
	if err != nil {
		return nil, err
	}
	var actual *coreworkload.ActualSnapshot
	workload, workloadErr := service.GetWorkload(ctx, operation.WorkloadID)
	if workloadErr == nil && workload.Actual.WorkloadID != "" {
		snapshot := workload.Actual
		actual = &snapshot
	} else if workloadErr != nil && !errors.Is(workloadErr, coreworkload.ErrNotFound) {
		return nil, workloadErr
	}
	return workloadOperationMap(operation, plan, actual), nil
}
func workloadOperationMap(v coreworkload.Operation, plan coreworkload.Plan, actual *coreworkload.ActualSnapshot) map[string]any {
	return map[string]any{
		"operation_id": v.ID, "workload_id": v.WorkloadID, "plan_id": v.PlanID,
		"kind": string(v.Kind), "plan_revision": v.PlanRevision, "plan_digest": v.PlanDigest,
		"summary":     plan.Summary,
		"target_kind": workloadTargetKindWire(v.TargetKind),
		"task_id":     v.TaskID, "confirmation_id": v.ConfirmationID,
		"status": string(v.Status), "revision": v.Revision,
		"failure_code": v.FailureCode, "failure_summary": v.FailureSummary,
		"created_at":           v.CreatedAt.UTC().Format(time.RFC3339Nano),
		"updated_at":           v.UpdatedAt.UTC().Format(time.RFC3339Nano),
		"desired_plan":         workloadDesiredPlanMap(plan),
		"actual":               workloadActualPointerMap(actual),
		"dispatch_epoch":       v.DispatchEpoch,
		"dispatch_lease_until": v.DispatchLeaseUntil.UTC().Format(time.RFC3339Nano),
	}
}
func workloadDesiredPlanMap(plan coreworkload.Plan) map[string]any {
	return map[string]any{
		"plan_id": plan.ID, "plan_revision": plan.Revision, "plan_digest": plan.Digest,
		"target":          targetSettingsMap(plan.Target),
		"resource_limits": resourceLimitsMap(plan.ResourceLimits),
		"secret_grants":   workloadSecretGrantRefsMap(plan.SecretGrantRefs),
	}
}
func workloadActualPointerMap(v *coreworkload.ActualSnapshot) any {
	if v == nil {
		return nil
	}
	return workloadActualMap(*v)
}
func workloadActualMap(v coreworkload.ActualSnapshot) map[string]any {
	return map[string]any{
		"workload_id": v.WorkloadID, "revision": v.Revision, "state": v.State,
		"identity":        workloadIdentityMap(v.Identity, nil),
		"applied_plan_id": v.AppliedPlanID, "applied_plan_digest": v.AppliedPlanDigest,
		"readback_digest": v.ReadbackDigest, "provider_version": v.ProviderVersion,
		"observed_at": v.ObservedAt.UTC().Format(time.RFC3339Nano),
		"updated_at":  v.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}
func workloadSparseReadbackMap(v coreworkload.Readback) map[string]any {
	return map[string]any{
		"workload_id": v.WorkloadID, "state": v.State,
		"identity":        workloadIdentityMap(v.Identity, nil),
		"readback_digest": v.Digest, "provider_version": v.ProviderVersion,
		"observed_at": v.At.UTC().Format(time.RFC3339Nano),
	}
}
func workloadEventMap(v coreworkload.Event) (map[string]any, error) {
	out := map[string]any{
		"operation_id": v.OperationID, "sequence": v.Sequence, "kind": v.Kind,
		"status": string(v.Status), "message": v.Message,
		"at": v.At.UTC().Format(time.RFC3339Nano),
	}
	if len(v.Readback) == 0 || string(v.Readback) == "null" {
		return out, nil
	}
	var readback coreworkload.Readback
	if err := json.Unmarshal(v.Readback, &readback); err != nil {
		return nil, coreworkload.ErrInvalid
	}
	out["actual"] = workloadSparseReadbackMap(readback)
	return out, nil
}
func workloadTargetKindWire(kind coreworkload.TargetKind) string {
	switch kind {
	case coreworkload.TargetAWSEC2SSM:
		return "aws-ec2-ssm"
	case coreworkload.TargetAWSECS:
		return "aws-ecs"
	default:
		return ""
	}
}
func workloadIdentityMap(i coreworkload.TargetIdentity, settings *coreworkload.TargetSettings) map[string]any {
	out := map[string]any{
		"kind":                workloadTargetKindWire(i.Kind),
		"core_runner_service": i.CoreRunnerService, "image_digest": i.ImageDigest,
		"aws_account_id": i.AccountID, "aws_region": i.Region, "instance_id": i.InstanceID,
		"cluster": i.Cluster, "service": i.Service,
		"task_definition_revision": i.TaskDefinitionRevision,
		"desired_count":            i.DesiredCount, "endpoint": i.Endpoint, "core_runner_id": i.CoreRunnerID,
		"aws_ec2_document_version": "", "aws_ec2_systemd_service": "",
		"aws_ec2_required_instance_tags": map[string]string{},
		"aws_ecs_cluster_arn":            "", "aws_ecs_service_name": "", "aws_ecs_task_family": "",
		"aws_ecs_platform_version": "", "aws_ecs_subnet_ids": []string{},
		"aws_ecs_security_group_ids": []string{}, "aws_ecs_assign_public_ip": false,
		"aws_ecs_target_group_arn": "", "aws_ecs_target_group_port": uint32(0),
		"aws_ecs_task_role_arn": "", "aws_ecs_execution_role_arn": "",
		"aws_ecs_desired_count": int64(0), "aws_ecs_image_uri": "",
	}
	if settings != nil {
		out["aws_ec2_document_version"] = settings.EC2DocumentVersion
		out["aws_ec2_systemd_service"] = settings.EC2SystemdService
		out["aws_ec2_required_instance_tags"] = copyStringMap(settings.RequiredInstanceTags)
		out["aws_ecs_cluster_arn"] = settings.ECSClusterARN
		out["aws_ecs_service_name"] = settings.ECSServiceName
		out["aws_ecs_task_family"] = settings.ECSTaskFamily
		out["aws_ecs_platform_version"] = settings.ECSPlatformVersion
		out["aws_ecs_subnet_ids"] = append([]string{}, settings.ECSSubnetIDs...)
		out["aws_ecs_security_group_ids"] = append([]string{}, settings.ECSSecurityGroupIDs...)
		out["aws_ecs_assign_public_ip"] = settings.ECSAssignPublicIP
		out["aws_ecs_target_group_arn"] = settings.ECSTargetGroupARN
		out["aws_ecs_target_group_port"] = settings.ECSTargetGroupPort
		out["aws_ecs_task_role_arn"] = settings.ECSTaskRoleARN
		out["aws_ecs_execution_role_arn"] = settings.ECSExecutionRoleARN
		out["aws_ecs_desired_count"] = settings.ECSDesiredCount
		out["aws_ecs_image_uri"] = settings.ECSImageURI
	}
	return out
}
func resourceLimitsMap(v coreworkload.ResourceLimits) map[string]any {
	return map[string]any{
		"cpu": v.CPU, "memory_mb": v.MemoryMB, "processes": v.Processes,
		"disk_mb": v.DiskMB, "timeout_seconds": v.TimeoutS, "output_mb": v.OutputMB,
	}
}
func workloadSecretGrantRefsMap(refs []coreworkload.SecretGrantRef) []any {
	out := make([]any, 0, len(refs))
	for _, ref := range refs {
		item := map[string]any{
			"reference_id": ref.ReferenceID, "purpose": string(ref.Purpose),
			"binding_digest": string(ref.BindingDigest),
		}
		if ref.Revision > 0 {
			item["secret_revision"] = ref.Revision
		}
		out = append(out, item)
	}
	return out
}
func copyStringMap(source map[string]string) map[string]string {
	out := make(map[string]string, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

func requiredUUID(p map[string]any, k string) (string, *actionbase.Error) {
	s, e := requiredString(p, k)
	if e != nil {
		return "", e
	}
	id, err := uuid.Parse(s)
	if err != nil || id == uuid.Nil || id.String() != s {
		return "", actionbase.BadRequest(k + " must be a canonical UUID")
	}
	return s, nil
}
func optionalUUID(p map[string]any, k string) (string, *actionbase.Error) {
	if _, ok := p[k]; !ok {
		return "", nil
	}
	return requiredUUID(p, k)
}
func requiredPositiveInt64(p map[string]any, k string) (int64, *actionbase.Error) {
	v, e := optionalInt64(p, k)
	if e != nil {
		return 0, e
	}
	if v < 1 {
		return 0, actionbase.BadRequest(k + " must be positive")
	}
	return v, nil
}
func optionalUint64(p map[string]any, k string) (uint64, *actionbase.Error) {
	v, e := optionalInt64(p, k)
	if e != nil {
		return 0, e
	}
	if v < 0 {
		return 0, actionbase.BadRequest(k + " must be nonnegative")
	}
	return uint64(v), nil
}
func requiredMap(p map[string]any, k string) (map[string]any, *actionbase.Error) {
	v, ok := p[k].(map[string]any)
	if !ok {
		return nil, actionbase.BadRequest(k + " must be an object")
	}
	return v, nil
}
func requiredTime(p map[string]any, k string) (time.Time, *actionbase.Error) {
	s, e := requiredString(p, k)
	if e != nil {
		return time.Time{}, e
	}
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, actionbase.BadRequest(k + " must be RFC3339")
	}
	return v, nil
}
func stringSlice(v any) ([]string, *actionbase.Error) {
	a, ok := v.([]any)
	if !ok {
		return nil, actionbase.BadRequest("command_steps must be an array")
	}
	out := make([]string, len(a))
	for i, x := range a {
		s, ok := x.(string)
		if !ok {
			return nil, actionbase.BadRequest("command_steps must contain strings")
		}
		out[i] = s
	}
	return out, nil
}
func mustString(p map[string]any, k string) string { s, _ := p[k].(string); return s }

func awsError(err any) *actionbase.Error { return domainError(err, "aws_not_found", "aws_conflict") }
func workloadError(err any) *actionbase.Error {
	return domainError(err, "workload_not_found", "workload_conflict")
}
func domainError(err any, notFound, conflict string) *actionbase.Error {
	switch v := err.(type) {
	case *actionbase.Error:
		return v
	case error:
		switch {
		case errors.Is(v, coreaws.ErrNotFound), errors.Is(v, coreworkload.ErrNotFound):
			return actionbase.CodedError(http.StatusNotFound, notFound, "resource was not found")
		case errors.Is(v, coreaws.ErrRevisionConflict), errors.Is(v, coreworkload.ErrRevisionConflict), errors.Is(v, coreaws.ErrConflict), errors.Is(v, coreworkload.ErrConflict):
			return actionbase.CodedError(http.StatusConflict, conflict, "revision or idempotency conflict")
		case errors.Is(v, coreaws.ErrInvalid), errors.Is(v, coreworkload.ErrInvalid):
			return actionbase.BadRequest("invalid request")
		default:
			return actionbase.InternalError(fmt.Errorf("embedded domain failure: %w", v))
		}
	default:
		return actionbase.InternalError(fmt.Errorf("embedded domain failure: %v", err))
	}
}
