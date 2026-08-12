package agentgateway

import (
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

var cloudWorkerConfirmationStates = map[string]bool{
	"pending": true, "confirmed": true, "consumed": true, "rejected": true, "expired": true,
}

var cloudWorkerConfirmationFields = []string{
	"confirmation_id", "owner_id", "binding", "task_id", "state", "revision",
	"created_at", "updated_at", "expires_at", "terminal_code", "terminal_note", "terminal_reason",
}

var cloudWorkerConfirmationBindingFields = []string{
	"owner_id", "account_generation", "operation_domain", "target_id", "target_revision", "target_kind",
	"source_version", "source_commit", "content_digest", "manifest_digest", "execution_digest",
	"permission_digest", "parameter_digest", "network_digest", "secret_grant_digest", "selected_tool",
	"selected_command", "network_grants", "secret_grants", "execution_id", "plan_id", "plan_revision",
	"plan_digest", "run_id", "run_revision", "run_digest", "quote_digest", "digest",
}

func validateCloudWorkerConfirmationActionResult(action string, request, output map[string]any, authority actionResultAuthority) error {
	switch action {
	case "agent.core.confirmations.get", "agent.core.confirmations.confirm", "agent.core.confirmations.reject":
		requestedID, requested := request["confirmation_id"].(string)
		returned, returnedOK := confirmationActionResultRecord(output)
		if !requested || !returnedOK || returned["confirmation_id"] != requestedID {
			return cloudWorkerResultError("confirmation_id does not match the request")
		}
		if err := validateAuthorityBoundConfirmation(returned, authority); err != nil {
			return err
		}
		confirmation, cloud, err := cloudWorkerConfirmationFromResult(output)
		if err != nil || !cloud {
			return err
		}
		if err := cloudExact(output, []string{"confirmation"}, nil, "confirmation envelope"); err != nil {
			return err
		}
		return validateCloudWorkerConfirmation(confirmation, authority)
	case "agent.core.confirmations.list":
		raw, ok := output["confirmations"].([]any)
		if !ok {
			return nil
		}
		containsCloudWorker := false
		for _, item := range raw {
			confirmation, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if err := validateAuthorityBoundConfirmation(confirmation, authority); err != nil {
				return err
			}
			if !looksLikeCloudWorkerConfirmation(confirmation) {
				continue
			}
			containsCloudWorker = true
			if err := validateCloudWorkerConfirmation(confirmation, authority); err != nil {
				return err
			}
			if err := validateCloudWorkerConfirmationListBinding(request, confirmation); err != nil {
				return err
			}
		}
		if containsCloudWorker {
			if err := cloudExact(output, []string{"confirmations", "next_page_token"}, nil, "confirmations page"); err != nil {
				return err
			}
			if _, ok := output["next_page_token"].(string); !ok {
				return cloudWorkerResultError("next_page_token must be a string")
			}
			for _, item := range raw {
				if _, ok := item.(map[string]any); !ok {
					return cloudWorkerResultError("confirmation must be an object")
				}
			}
		}
	}
	return nil
}

func confirmationActionResultRecord(output map[string]any) (map[string]any, bool) {
	if confirmation, ok := output["confirmation"].(map[string]any); ok {
		return confirmation, true
	}
	if _, ok := output["confirmation_id"]; ok {
		return output, true
	}
	return nil, false
}

func validateCloudWorkerConfirmationListBinding(request, confirmation map[string]any) error {
	binding := confirmation["binding"].(map[string]any)
	if operationDomain, ok := request["operation_domain"].(string); ok && binding["operation_domain"] != operationDomain {
		return cloudWorkerResultError("confirmation operation_domain does not match the request")
	}
	if targetID, ok := request["target_id"].(string); ok && binding["target_id"] != targetID {
		return cloudWorkerResultError("confirmation target_id does not match the request")
	}
	if states, ok := request["states"].([]any); ok && len(states) > 0 {
		state := cloudString(confirmation["state"])
		matched := false
		for _, value := range states {
			if value == state {
				matched = true
				break
			}
		}
		if !matched {
			return cloudWorkerResultError("confirmation state does not match the request")
		}
	}
	return nil
}

func cloudWorkerConfirmationFromResult(output map[string]any) (map[string]any, bool, error) {
	if output == nil {
		return nil, false, nil
	}
	if confirmation, ok := output["confirmation"].(map[string]any); ok && looksLikeCloudWorkerConfirmation(confirmation) {
		return confirmation, true, nil
	}
	if looksLikeCloudWorkerConfirmation(output) {
		return nil, true, cloudWorkerResultError("confirmation envelope is missing")
	}
	return nil, false, nil
}

func isCloudWorkerConfirmation(confirmation map[string]any) bool {
	binding, ok := confirmation["binding"].(map[string]any)
	return ok && binding["operation_domain"] == "cloud_worker.execute"
}

func looksLikeCloudWorkerConfirmation(confirmation map[string]any) bool {
	binding, ok := confirmation["binding"].(map[string]any)
	if !ok {
		return false
	}
	if binding["operation_domain"] == "cloud_worker.execute" || binding["target_kind"] == "ephemeral_pi_worker" || binding["selected_tool"] == "cloud_worker_propose" {
		return true
	}
	for _, field := range []string{"execution_id", "plan_id", "plan_revision", "plan_digest", "run_id", "run_revision", "run_digest", "quote_digest", "digest"} {
		if _, present := binding[field]; present {
			return true
		}
	}
	return false
}

func validateAuthorityBoundConfirmation(confirmation map[string]any, authority actionResultAuthority) error {
	binding, ok := confirmation["binding"].(map[string]any)
	if !ok {
		return nil
	}
	domain, _ := binding["operation_domain"].(string)
	switch domain {
	case "cloud_worker.execute", "execution_v2.run", "extension.execute":
	default:
		return nil
	}
	if !authority.valid() {
		return cloudWorkerResultError("prepared owner authority is missing")
	}
	owner, ownerOK := confirmation["owner_id"].(string)
	bindingOwner, bindingOwnerOK := binding["owner_id"].(string)
	generation, generationOK := cloudInteger(binding["account_generation"])
	if !ownerOK || !bindingOwnerOK || owner != authority.ownerID || bindingOwner != authority.ownerID ||
		owner != strings.TrimSpace(owner) || bindingOwner != strings.TrimSpace(bindingOwner) ||
		!generationOK || generation != authority.accountGeneration {
		return cloudWorkerResultError("confirmation owner authority does not match the prepared request")
	}
	return nil
}

func validateCloudWorkerConfirmation(confirmation map[string]any, authority actionResultAuthority) error {
	if !authority.valid() {
		return cloudWorkerResultError("prepared owner authority is missing")
	}
	if err := cloudExact(confirmation, cloudWorkerConfirmationFields, nil, "confirmation"); err != nil {
		return err
	}
	binding, ok := confirmation["binding"].(map[string]any)
	if !ok {
		return cloudWorkerResultError("confirmation binding must be an object")
	}
	if err := cloudExact(binding, cloudWorkerConfirmationBindingFields, nil, "confirmation binding"); err != nil {
		return err
	}
	if binding["operation_domain"] != "cloud_worker.execute" {
		return cloudWorkerResultError("confirmation operation_domain is invalid")
	}
	owner, ownerOK := confirmation["owner_id"].(string)
	bindingOwner, bindingOwnerOK := binding["owner_id"].(string)
	generation, generationOK := cloudInteger(binding["account_generation"])
	if !ownerOK || !bindingOwnerOK || owner != authority.ownerID || bindingOwner != authority.ownerID ||
		owner != strings.TrimSpace(owner) || bindingOwner != strings.TrimSpace(bindingOwner) ||
		!generationOK || generation != authority.accountGeneration {
		return cloudWorkerResultError("confirmation owner authority does not match the prepared request")
	}

	for _, field := range []string{"confirmation_id", "task_id"} {
		if !cloudNonNilUUID(confirmation[field]) {
			return cloudWorkerResultError("confirmation %s is not a canonical UUID", field)
		}
	}
	for _, field := range []string{"target_id", "execution_id", "plan_id", "run_id"} {
		if !cloudNonNilUUID(binding[field]) {
			return cloudWorkerResultError("confirmation binding %s is not a canonical UUID", field)
		}
	}
	if binding["target_id"] != binding["execution_id"] || binding["target_id"] != binding["run_id"] {
		return cloudWorkerResultError("confirmation execution identities differ")
	}
	revisions := make(map[string]int64, 3)
	for _, field := range []string{"target_revision", "plan_revision", "run_revision"} {
		revision, ok := cloudInteger(binding[field])
		if !ok || revision <= 0 {
			return cloudWorkerResultError("confirmation binding %s is not positive", field)
		}
		revisions[field] = revision
	}
	if revisions["target_revision"] != revisions["plan_revision"] {
		return cloudWorkerResultError("confirmation target_revision does not match plan_revision")
	}
	if revision, ok := cloudInteger(confirmation["revision"]); !ok || revision <= 0 {
		return cloudWorkerResultError("confirmation revision is not positive")
	}
	for _, field := range []string{
		"source_commit", "content_digest", "manifest_digest", "execution_digest", "permission_digest",
		"parameter_digest", "network_digest", "secret_grant_digest", "plan_digest", "run_digest",
		"quote_digest", "digest",
	} {
		if !cloudDigest(binding[field]) {
			return cloudWorkerResultError("confirmation binding %s is not a digest", field)
		}
	}
	if binding["target_kind"] != "ephemeral_pi_worker" || binding["source_version"] != "ephemeral-pi-task" ||
		binding["selected_tool"] != "cloud_worker_propose" {
		return cloudWorkerResultError("confirmation worker recipe binding is invalid")
	}
	if !cloudWorkerConfirmationStates[cloudString(confirmation["state"])] {
		return cloudWorkerResultError("confirmation state is invalid")
	}
	for _, field := range []string{"terminal_code", "terminal_note", "terminal_reason"} {
		if _, ok := confirmation[field].(string); !ok {
			return cloudWorkerResultError("confirmation %s must be a string", field)
		}
	}
	created, createdOK := cloudTime(confirmation["created_at"])
	updated, updatedOK := cloudTime(confirmation["updated_at"])
	expires, expiresOK := cloudTime(confirmation["expires_at"])
	state := cloudString(confirmation["state"])
	if !createdOK || !updatedOK || !expiresOK || updated.Before(created) || !expires.After(created) ||
		((state == "pending" || state == "confirmed") && updated.After(expires)) {
		return cloudWorkerResultError("confirmation timestamps are invalid")
	}
	selectedCommand, ok := binding["selected_command"].([]any)
	if !ok || len(selectedCommand) != 0 {
		return cloudWorkerResultError("confirmation selected_command must be an empty array")
	}
	if err := cloudStringArray(binding["network_grants"], "network_grants"); err != nil {
		return err
	}
	return validateCloudConfirmationSecretGrants(binding["secret_grants"])
}

func validateCloudConfirmationSecretGrants(value any) error {
	items, ok := value.([]any)
	if !ok {
		return cloudWorkerResultError("confirmation secret_grants must be an array")
	}
	seen := make(map[string]bool, len(items))
	for _, value := range items {
		grant, ok := value.(map[string]any)
		if !ok {
			return cloudWorkerResultError("confirmation secret grant must be an object")
		}
		if err := cloudExact(grant, []string{"purpose"}, nil, "confirmation secret grant"); err != nil {
			return err
		}
		purpose, ok := grant["purpose"].(string)
		if !ok || purpose == "" || purpose != strings.TrimSpace(purpose) || !utf8.ValidString(purpose) || len(purpose) > 64 || seen[purpose] {
			return cloudWorkerResultError("confirmation secret grant purpose is invalid or duplicate")
		}
		seen[purpose] = true
	}
	return nil
}

func cloudNonNilUUID(value any) bool {
	raw, ok := value.(string)
	if !ok || raw != strings.TrimSpace(raw) {
		return false
	}
	parsed, err := uuid.Parse(raw)
	return err == nil && parsed != uuid.Nil && parsed.String() == raw
}

func projectCloudWorkerConfirmation(confirmation map[string]any) map[string]any {
	if !isCloudWorkerConfirmation(confirmation) {
		return confirmation
	}
	projected := make(map[string]any, len(cloudWorkerConfirmationFields))
	for _, field := range cloudWorkerConfirmationFields {
		if field != "binding" {
			projected[field] = confirmation[field]
		}
	}
	binding := confirmation["binding"].(map[string]any)
	publicBinding := make(map[string]any, len(cloudWorkerConfirmationBindingFields))
	for _, field := range cloudWorkerConfirmationBindingFields {
		if field != "secret_grants" {
			publicBinding[field] = binding[field]
		}
	}
	rawGrants := binding["secret_grants"].([]any)
	grants := make([]any, 0, len(rawGrants))
	for _, value := range rawGrants {
		grant := value.(map[string]any)
		grants = append(grants, map[string]any{"purpose": grant["purpose"]})
	}
	publicBinding["secret_grants"] = grants
	projected["binding"] = publicBinding
	return projected
}
