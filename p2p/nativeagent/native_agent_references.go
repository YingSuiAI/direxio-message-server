package nativeagent

import (
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	coreworkload "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload"
	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
)

const nativeAgentReferencePreviewRunes = 240

type nativeReferenceSecretGrant struct {
	ReferenceID   string `json:"reference_id"`
	Purpose       string `json:"purpose"`
	Revision      int64  `json:"secret_revision"`
	BindingDigest string `json:"binding_digest"`
}

// ReferencesFromToolMessages projects only server-authored tool results into
// durable, redacted Native Agent references.
func ReferencesFromToolMessages(produced []*schema.Message) []map[string]any {
	return nativeAgentReferences(produced)
}

const (
	nativeGeoLibreReleaseVersion = "geolibre-2856ef8-v1"
	nativeGeoLibreCommitSHA      = "2856ef8c0b227ad18ecf43d4623cf00013c1740e"
	nativeGeoLibreImageDigest    = "bd18a93768087e5619e75e2e8282ce347aed9179987ee8a7f471df862b72d64d"
	nativeGeoLibreManifestDigest = "849a9977a72efe5b4e70d28517c1edf038d7ca91c0fc4f53183a3ee3b1095a86"
	nativeGeoLibreCommandDigest  = "f0263e3ae0f0ad857da924ade38f9bd24e6643a0ed37829b4bcaf2152d7bd582"
)

func nativeAgentReferences(produced []*schema.Message) []map[string]any {
	references := make([]map[string]any, 0)
	seenRooms := map[string]struct{}{}
	seenPosts := map[string]struct{}{}
	seenConfirmations := map[string]struct{}{}

	addRoom := func(roomID, roomType, title, preview string) {
		roomID = strings.TrimSpace(roomID)
		roomType = normalizedReferenceRoomType(roomType)
		if roomID == "" {
			return
		}
		if _, exists := seenRooms[roomID]; exists {
			return
		}
		seenRooms[roomID] = struct{}{}
		reference := map[string]any{
			"kind":    "room",
			"room_id": roomID,
		}
		if roomType != "" {
			reference["room_type"] = roomType
		}
		if title = strings.TrimSpace(title); title != "" {
			reference["title"] = title
		}
		if preview = referencePreview(preview); preview != "" {
			reference["preview"] = preview
		}
		references = append(references, reference)
	}

	addPost := func(roomID, channelID, postID, title, preview string) {
		roomID = strings.TrimSpace(roomID)
		channelID = strings.TrimSpace(channelID)
		postID = strings.TrimSpace(postID)
		if roomID == "" || channelID == "" || postID == "" {
			return
		}
		key := channelID + "\x00" + postID
		if _, exists := seenPosts[key]; exists {
			return
		}
		seenPosts[key] = struct{}{}
		reference := map[string]any{
			"kind":       "channel_post",
			"room_id":    roomID,
			"channel_id": channelID,
			"post_id":    postID,
		}
		if title = strings.TrimSpace(title); title != "" {
			reference["title"] = title
		}
		if preview = referencePreview(preview); preview != "" {
			reference["preview"] = preview
		}
		references = append(references, reference)
	}

	addPendingConfirmation := func(result json.RawMessage, action string, fixedGeo bool) {
		var envelope struct {
			ProvisionID       string `json:"provision_id"`
			ProvisionRevision int64  `json:"provision_revision"`
			Operation         struct {
				OperationID              string `json:"operation_id"`
				WorkloadID               string `json:"workload_id"`
				PlanID                   string `json:"plan_id"`
				TaskID                   string `json:"task_id"`
				ConfirmationID           string `json:"confirmation_id"`
				Revision                 int64  `json:"revision"`
				PlanRevision             int64  `json:"plan_revision"`
				PlanDigest               string `json:"plan_digest"`
				ExpectedWorkloadRevision int64  `json:"expected_workload_revision"`
				TargetKind               string `json:"target_kind"`
				Summary                  string `json:"summary"`
				Kind                     string `json:"kind"`
				DesiredPlan              struct {
					SecretGrants []nativeReferenceSecretGrant `json:"secret_grants"`
				} `json:"desired_plan"`
				SecretGrantRefs []nativeReferenceSecretGrant `json:"secret_grant_refs"`
			} `json:"operation"`
			Confirmation struct {
				ConfirmationID string `json:"confirmation_id"`
				TaskID         string `json:"task_id"`
				State          string `json:"state"`
				Revision       int64  `json:"revision"`
				ExpiresAt      string `json:"expires_at"`
				Binding        struct {
					OperationDomain   string                       `json:"operation_domain"`
					TargetID          string                       `json:"target_id"`
					TargetRevision    int64                        `json:"target_revision"`
					ContentDigest     string                       `json:"content_digest"`
					ParameterDigest   string                       `json:"parameter_digest"`
					NetworkDigest     string                       `json:"network_digest"`
					SecretGrantDigest string                       `json:"secret_grant_digest"`
					SecretGrants      []nativeReferenceSecretGrant `json:"secret_grants"`
				} `json:"binding"`
			} `json:"confirmation"`
			Plan struct {
				PlanID      string `json:"plan_id"`
				Revision    int64  `json:"revision"`
				Digest      string `json:"digest"`
				TypedTarget struct {
					ProvisionID        string `json:"provision_id"`
					ProvisionRevision  any    `json:"provision_revision"`
					CredentialID       string `json:"credential_id"`
					CredentialRevision any    `json:"credential_revision"`
					AccountID          string `json:"account_id"`
					Region             string `json:"region"`
					InstanceID         string `json:"instance_id"`
					PublicEndpoint     string `json:"public_endpoint"`
				} `json:"typed_target"`
				Release struct {
					Version        string `json:"version"`
					Commit         string `json:"commit"`
					ImageDigest    string `json:"image_digest"`
					ManifestDigest string `json:"manifest_digest"`
					CommandDigest  string `json:"command_digest"`
				} `json:"release"`
			} `json:"plan"`
			ExpectedWorkloadRevision int64 `json:"expected_workload_revision"`
		}
		if json.Unmarshal(result, &envelope) != nil {
			return
		}
		op := envelope.Operation
		confirmation := envelope.Confirmation
		action = strings.ToLower(strings.TrimSpace(action))
		expiresAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(confirmation.ExpiresAt))
		// SecretGrantDigest is the aggregate digest of the complete grant set.
		// A grant's BindingDigest is its immutable owner/AAD binding and must not
		// be compared to that aggregate value. GeoLibre carries typed refs in
		// operation.desired_plan, so validate the complete pinned grant instead.
		geoCredentialGrantOK := false
		typedSecretGrantsOK := true
		expectedSecretGrants := envelope.Operation.SecretGrantRefs
		projectedSecretRefsPresent := envelope.Operation.SecretGrantRefs != nil
		if len(expectedSecretGrants) == 0 {
			expectedSecretGrants = envelope.Operation.DesiredPlan.SecretGrants
			projectedSecretRefsPresent = projectedSecretRefsPresent || envelope.Operation.DesiredPlan.SecretGrants != nil
		}
		if expected := expectedSecretGrants; len(expected) > 0 {
			if len(expected) != len(confirmation.Binding.SecretGrants) {
				typedSecretGrantsOK = false
			} else {
				used := make([]bool, len(expected))
				for _, grant := range confirmation.Binding.SecretGrants {
					matched := false
					for i, want := range expected {
						if used[i] {
							continue
						}
						if grant.ReferenceID == want.ReferenceID && grant.Purpose == want.Purpose && grant.Revision == want.Revision && grant.BindingDigest == want.BindingDigest && validReferenceDigest(grant.BindingDigest) && validReferenceDigest(want.BindingDigest) {
							used[i], matched = true, true
							break
						}
					}
					if !matched {
						typedSecretGrantsOK = false
						break
					}
				}
			}
		}
		if fixedGeo && strings.TrimSpace(envelope.Plan.TypedTarget.CredentialID) != "" && positiveRevisionValue(envelope.Plan.TypedTarget.CredentialRevision) {
			expected := expectedSecretGrants
			if len(expected) == 1 && len(confirmation.Binding.SecretGrants) == 1 {
				want := expected[0]
				grant := confirmation.Binding.SecretGrants[0]
				geoCredentialGrantOK = grant.ReferenceID == envelope.Plan.TypedTarget.CredentialID &&
					grant.Purpose == "aws_credential" &&
					grant.Revision == int64Value(envelope.Plan.TypedTarget.CredentialRevision) &&
					grant.ReferenceID == want.ReferenceID && grant.Purpose == want.Purpose &&
					grant.Revision == want.Revision && grant.BindingDigest == want.BindingDigest &&
					validReferenceDigest(grant.BindingDigest) && validReferenceDigest(want.BindingDigest)
			}
		}
		typedAggregateDigestOK := true
		if projectedSecretRefsPresent {
			refs := make([]coreworkload.SecretGrantRef, 0, len(expectedSecretGrants))
			for _, expected := range expectedSecretGrants {
				refs = append(refs, coreworkload.SecretGrantRef{
					ReferenceID:   expected.ReferenceID,
					Purpose:       coreconfirmation.SecretPurpose(expected.Purpose),
					Revision:      expected.Revision,
					BindingDigest: coreconfirmation.Digest(expected.BindingDigest),
				})
			}
			typedAggregateDigestOK = confirmation.Binding.SecretGrantDigest == coreworkload.SecretGrantAggregateDigestForTypedRefs(refs)
		}
		if (action != "apply" && action != "destroy") || strings.ToLower(strings.TrimSpace(op.Kind)) != action ||
			!validReferenceUUID(op.OperationID) || !validReferenceUUID(op.WorkloadID) ||
			!validReferenceUUID(op.PlanID) || !validReferenceUUID(op.TaskID) ||
			!validReferenceUUID(op.ConfirmationID) || !validReferenceUUID(confirmation.ConfirmationID) ||
			op.ConfirmationID != confirmation.ConfirmationID || confirmation.State != "pending" ||
			op.Revision <= 0 || op.PlanRevision <= 0 || confirmation.Revision <= 0 ||
			confirmation.TaskID != op.TaskID || confirmation.Binding.OperationDomain != "workload:"+action ||
			confirmation.Binding.TargetID != op.WorkloadID || confirmation.Binding.TargetRevision != op.PlanRevision ||
			confirmation.Binding.ContentDigest != op.PlanDigest || !validReferenceDigest(op.PlanDigest) ||
			!validReferenceDigest(confirmation.Binding.ParameterDigest) || !validReferenceDigest(confirmation.Binding.NetworkDigest) || !validReferenceDigest(confirmation.Binding.SecretGrantDigest) ||
			(!fixedGeo && !typedSecretGrantsOK) ||
			(fixedGeo && (envelope.Plan.PlanID != op.PlanID || envelope.Plan.Revision != op.PlanRevision || envelope.Plan.Digest != op.PlanDigest ||
				!validReferenceUUID(envelope.ProvisionID) || envelope.ProvisionRevision <= 0 ||
				envelope.Plan.TypedTarget.ProvisionID != envelope.ProvisionID ||
				!samePositiveRevisionValue(envelope.Plan.TypedTarget.ProvisionRevision, envelope.ProvisionRevision) ||
				!validReferenceUUID(envelope.Plan.TypedTarget.CredentialID) || !positiveRevisionValue(envelope.Plan.TypedTarget.CredentialRevision) ||
				(fixedGeo && !geoCredentialGrantOK) ||
				(!typedAggregateDigestOK) ||
				(envelope.ExpectedWorkloadRevision <= 0 || op.ExpectedWorkloadRevision != envelope.ExpectedWorkloadRevision) ||
				envelope.Plan.Release.Version != nativeGeoLibreReleaseVersion || envelope.Plan.Release.Commit != nativeGeoLibreCommitSHA || envelope.Plan.Release.ImageDigest != nativeGeoLibreImageDigest || envelope.Plan.Release.ManifestDigest != nativeGeoLibreManifestDigest || envelope.Plan.Release.CommandDigest != nativeGeoLibreCommandDigest)) ||
			strings.TrimSpace(confirmation.ExpiresAt) == "" || err != nil || !expiresAt.After(time.Now()) {
			return
		}
		id := strings.TrimSpace(op.ConfirmationID)
		if _, exists := seenConfirmations[id]; exists {
			return
		}
		seenConfirmations[id] = struct{}{}
		reference := map[string]any{
			"kind":                "pending_confirmation",
			"confirmation_id":     id,
			"operation_id":        strings.TrimSpace(op.OperationID),
			"workload_id":         strings.TrimSpace(op.WorkloadID),
			"task_id":             strings.TrimSpace(op.TaskID),
			"plan_id":             strings.TrimSpace(op.PlanID),
			"action":              action,
			"revision":            confirmation.Revision,
			"expires_at":          strings.TrimSpace(confirmation.ExpiresAt),
			"operation_domain":    confirmation.Binding.OperationDomain,
			"target_id":           confirmation.Binding.TargetID,
			"target_revision":     confirmation.Binding.TargetRevision,
			"content_digest":      confirmation.Binding.ContentDigest,
			"parameter_digest":    confirmation.Binding.ParameterDigest,
			"network_digest":      confirmation.Binding.NetworkDigest,
			"secret_grant_digest": confirmation.Binding.SecretGrantDigest,
		}
		if fixedGeo {
			reference["release"] = envelope.Plan.Release.Version
			reference["commit"] = envelope.Plan.Release.Commit
			reference["image_digest"] = envelope.Plan.Release.ImageDigest
			reference["manifest_digest"] = envelope.Plan.Release.ManifestDigest
			reference["command_digest"] = envelope.Plan.Release.CommandDigest
			reference["credential_id"] = envelope.Plan.TypedTarget.CredentialID
			reference["credential_revision"] = int64Value(envelope.Plan.TypedTarget.CredentialRevision)
			reference["provision_revision"] = envelope.ProvisionRevision
			reference["expected_workload_revision"] = envelope.ExpectedWorkloadRevision
		}
		if provisionID := strings.TrimSpace(envelope.ProvisionID); validReferenceUUID(provisionID) {
			reference["provision_id"] = provisionID
		}
		if target := strings.TrimSpace(op.TargetKind); target != "" {
			reference["target_kind"] = target
		}
		if summary := strings.TrimSpace(op.Summary); summary != "" {
			reference["summary"] = referencePreview(summary)
		}
		if digest := strings.TrimSpace(op.PlanDigest); digest != "" {
			reference["plan_digest"] = digest
		}
		references = append(references, reference)
	}

	addPendingAWSConfirmation := func(result json.RawMessage, action string) {
		var envelope struct {
			Provision struct {
				ProvisionID             string `json:"provision_id"`
				PlanID                  string `json:"plan_id"`
				PlanRevision            int64  `json:"plan_revision"`
				Revision                int64  `json:"revision"`
				CredentialID            string `json:"credential_id"`
				CredentialRevision      int64  `json:"credential_revision"`
				TargetID                string `json:"target_id"`
				TemplateSHA256          string `json:"template_sha256"`
				CredentialBindingDigest string `json:"credential_binding_digest"`
				ParameterDigest         string `json:"parameter_digest"`
				NetworkDigest           string `json:"network_digest"`
				SecretGrantDigest       string `json:"secret_grant_digest"`
			} `json:"provision"`
			Change struct {
				ChangeID       string `json:"change_id"`
				PlanID         string `json:"plan_id"`
				ProvisionID    string `json:"provision_id"`
				TaskID         string `json:"task_id"`
				ConfirmationID string `json:"confirmation_id"`
				Operation      string `json:"operation"`
				Status         string `json:"status"`
				Revision       int64  `json:"revision"`
			} `json:"change"`
			Confirmation struct {
				ConfirmationID string `json:"confirmation_id"`
				TaskID         string `json:"task_id"`
				State          string `json:"state"`
				Revision       int64  `json:"revision"`
				ExpiresAt      string `json:"expires_at"`
				Binding        struct {
					OperationDomain   string `json:"operation_domain"`
					TargetID          string `json:"target_id"`
					TargetRevision    int64  `json:"target_revision"`
					ContentDigest     string `json:"content_digest"`
					ParameterDigest   string `json:"parameter_digest"`
					NetworkDigest     string `json:"network_digest"`
					SecretGrantDigest string `json:"secret_grant_digest"`
					SecretGrants      []struct {
						ReferenceID   string `json:"reference_id"`
						Purpose       string `json:"purpose"`
						Revision      int64  `json:"secret_revision"`
						BindingDigest string `json:"binding_digest"`
					} `json:"secret_grants"`
				} `json:"binding"`
			} `json:"confirmation"`
		}
		if json.Unmarshal(result, &envelope) != nil {
			return
		}
		change := envelope.Change
		confirmation := envelope.Confirmation
		action = strings.ToLower(strings.TrimSpace(action))
		expiresAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(confirmation.ExpiresAt))
		provisionPlanMatchesChange := envelope.Provision.PlanID == change.PlanID
		if action == "destroy" && validReferenceUUID(envelope.Provision.ProvisionID) {
			// Destroy requests use a derived delete plan while the durable
			// provision keeps the original create plan id. Bind the projected
			// change to that deterministic delete plan instead of requiring the
			// two plan ids to be equal.
			provisionPlanMatchesChange = change.PlanID == uuid.NewSHA1(uuid.Nil, []byte("ec2-destroy:"+envelope.Provision.ProvisionID)).String()
		}
		credentialGrantOK := false
		for _, grant := range confirmation.Binding.SecretGrants {
			if grant.ReferenceID == envelope.Provision.CredentialID && grant.Purpose == "aws_credential" && grant.Revision == envelope.Provision.CredentialRevision && grant.BindingDigest == envelope.Provision.CredentialBindingDigest && validReferenceDigest(grant.BindingDigest) && validReferenceDigest(envelope.Provision.CredentialBindingDigest) {
				credentialGrantOK = true
				break
			}
		}
		if (action != "create" && action != "destroy") || (action == "create" && change.Operation != "create") || (action == "destroy" && change.Operation != "delete") ||
			change.Status != "waiting_user" || !validReferenceUUID(change.ChangeID) || !validReferenceUUID(change.PlanID) ||
			!validReferenceUUID(change.TaskID) || !validReferenceUUID(change.ConfirmationID) || !validReferenceUUID(confirmation.ConfirmationID) ||
			change.ConfirmationID != confirmation.ConfirmationID || confirmation.State != "pending" || change.Revision <= 0 ||
			confirmation.Revision <= 0 || !validReferenceUUID(confirmation.TaskID) || confirmation.TaskID != change.TaskID || confirmation.Binding.OperationDomain != "aws" ||
			confirmation.Binding.TargetRevision <= 0 || strings.TrimSpace(confirmation.Binding.TargetID) == "" || !validReferenceDigest(confirmation.Binding.ContentDigest) ||
			!validReferenceDigest(confirmation.Binding.ParameterDigest) || !validReferenceDigest(confirmation.Binding.NetworkDigest) || !validReferenceDigest(confirmation.Binding.SecretGrantDigest) ||
			!validReferenceUUID(envelope.Provision.ProvisionID) || !validReferenceUUID(envelope.Provision.PlanID) || !provisionPlanMatchesChange || change.ProvisionID != envelope.Provision.ProvisionID ||
			envelope.Provision.PlanRevision <= 0 || envelope.Provision.Revision <= 0 || confirmation.Binding.TargetRevision != envelope.Provision.PlanRevision ||
			strings.TrimSpace(envelope.Provision.TargetID) == "" || confirmation.Binding.TargetID != envelope.Provision.TargetID ||
			!validReferenceDigest(envelope.Provision.TemplateSHA256) || confirmation.Binding.ContentDigest != envelope.Provision.TemplateSHA256 ||
			!validReferenceDigest(envelope.Provision.ParameterDigest) || confirmation.Binding.ParameterDigest != envelope.Provision.ParameterDigest ||
			!validReferenceDigest(envelope.Provision.NetworkDigest) || confirmation.Binding.NetworkDigest != envelope.Provision.NetworkDigest ||
			!validReferenceDigest(envelope.Provision.SecretGrantDigest) || confirmation.Binding.SecretGrantDigest != envelope.Provision.SecretGrantDigest ||
			!validReferenceUUID(envelope.Provision.CredentialID) || envelope.Provision.CredentialRevision <= 0 || len(confirmation.Binding.SecretGrants) != 1 || !credentialGrantOK ||
			strings.TrimSpace(confirmation.ExpiresAt) == "" || err != nil || !expiresAt.After(time.Now()) {
			return
		}
		id := strings.TrimSpace(confirmation.ConfirmationID)
		if _, exists := seenConfirmations[id]; exists {
			return
		}
		seenConfirmations[id] = struct{}{}
		reference := map[string]any{
			"kind":                "pending_confirmation",
			"confirmation_id":     id,
			"change_id":           strings.TrimSpace(change.ChangeID),
			"task_id":             strings.TrimSpace(change.TaskID),
			"plan_id":             strings.TrimSpace(change.PlanID),
			"action":              action,
			"revision":            confirmation.Revision,
			"expires_at":          strings.TrimSpace(confirmation.ExpiresAt),
			"operation_domain":    confirmation.Binding.OperationDomain,
			"target_id":           confirmation.Binding.TargetID,
			"target_revision":     confirmation.Binding.TargetRevision,
			"content_digest":      confirmation.Binding.ContentDigest,
			"parameter_digest":    confirmation.Binding.ParameterDigest,
			"network_digest":      confirmation.Binding.NetworkDigest,
			"secret_grant_digest": confirmation.Binding.SecretGrantDigest,
			"target_kind":         "aws-ec2",
			"credential_id":       envelope.Provision.CredentialID,
			"credential_revision": envelope.Provision.CredentialRevision,
		}
		provisionID := strings.TrimSpace(envelope.Provision.ProvisionID)
		if validReferenceUUID(provisionID) {
			reference["provision_id"] = provisionID
		}
		references = append(references, reference)
	}

	for _, message := range produced {
		if message == nil || message.Role != schema.Tool {
			continue
		}
		var envelope struct {
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal([]byte(message.Content), &envelope); err != nil || len(envelope.Result) == 0 || string(envelope.Result) == "null" {
			continue
		}

		switch strings.TrimSpace(message.ToolName) {
		case "native_agent_workloads_apply":
			addPendingConfirmation(envelope.Result, "apply", false)
		case "native_agent_workloads_destroy":
			addPendingConfirmation(envelope.Result, "destroy", false)
		case "native_agent_aws_ec2_provisions_create_request":
			addPendingAWSConfirmation(envelope.Result, "create")
		case "native_agent_aws_ec2_provisions_destroy_request":
			addPendingAWSConfirmation(envelope.Result, "destroy")
		case "native_agent_aws_ec2_geolibre_install_request":
			addPendingConfirmation(envelope.Result, "apply", true)
		case "dirextalk_contacts_list", "dirextalk_contacts_search":
			var result struct {
				Contacts []struct {
					RoomID      string `json:"room_id"`
					DisplayName string `json:"display_name"`
				} `json:"contacts"`
			}
			if json.Unmarshal(envelope.Result, &result) != nil {
				continue
			}
			for _, contact := range result.Contacts {
				addRoom(contact.RoomID, "direct", contact.DisplayName, "")
			}
		case "dirextalk_rooms_search":
			var result struct {
				Rooms []struct {
					Type   string `json:"type"`
					Name   string `json:"name"`
					RoomID string `json:"room_id"`
				} `json:"rooms"`
			}
			if json.Unmarshal(envelope.Result, &result) != nil {
				continue
			}
			for _, room := range result.Rooms {
				if normalizedReferenceRoomType(room.Type) == "" {
					continue
				}
				addRoom(room.RoomID, room.Type, room.Name, "")
			}
		case "dirextalk_messages_list":
			var result struct {
				RoomID   string `json:"room_id"`
				Name     string `json:"name"`
				Messages []struct {
					Msg string `json:"msg"`
				} `json:"messages"`
			}
			if json.Unmarshal(envelope.Result, &result) != nil {
				continue
			}
			preview := ""
			if len(result.Messages) > 0 {
				preview = result.Messages[0].Msg
			}
			addRoom(result.RoomID, "", result.Name, preview)
		case "dirextalk_channel_posts_list":
			var result struct {
				RoomID    string `json:"room_id"`
				ChannelID string `json:"channel_id"`
				Name      string `json:"name"`
				Posts     []struct {
					PostID string `json:"post_id"`
					Msg    string `json:"msg"`
				} `json:"posts"`
			}
			if json.Unmarshal(envelope.Result, &result) != nil {
				continue
			}
			for _, post := range result.Posts {
				addPost(result.RoomID, result.ChannelID, post.PostID, result.Name, post.Msg)
			}
		}
	}
	return references
}

func validReferenceUUID(value string) bool {
	value = strings.TrimSpace(value)
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}

func validReferenceDigest(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func samePositiveRevisionValue(value any, expected int64) bool {
	if expected <= 0 {
		return false
	}
	switch typed := value.(type) {
	case string:
		got, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		return err == nil && got == expected
	case float64:
		return typed == float64(expected)
	case int64:
		return typed == expected
	default:
		return false
	}
}

func positiveRevisionValue(value any) bool {
	switch typed := value.(type) {
	case string:
		got, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		return err == nil && got > 0
	case float64:
		return typed > 0 && typed == float64(int64(typed))
	case int64:
		return typed > 0
	default:
		return false
	}
}

func int64Value(value any) int64 {
	switch typed := value.(type) {
	case string:
		got, _ := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		return got
	case float64:
		return int64(typed)
	case int64:
		return typed
	default:
		return 0
	}
}

func normalizedReferenceRoomType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "contact", "direct":
		return "direct"
	case "group":
		return "group"
	case "channel":
		return "channel"
	default:
		return ""
	}
}

func referencePreview(value string) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= nativeAgentReferencePreviewRunes {
		return value
	}
	return string(runes[:nativeAgentReferencePreviewRunes]) + "..."
}
