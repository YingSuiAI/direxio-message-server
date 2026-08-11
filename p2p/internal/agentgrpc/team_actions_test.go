package agentgrpc

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/YingSuiAI/dirextalk-agent/api/gen/dirextalk/agent/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	testTeamTaskID          = "88888888-8888-4888-8888-888888888888"
	testTeamPlanID          = "99999999-9999-4999-8999-999999999999"
	testTeamExecutionID     = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	testTeamApprovalID      = "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff"
	testTeamChallengeID     = "cccccccc-dddd-4eee-8fff-aaaaaaaaaaaa"
	testTeamAuthorizationID = "dddddddd-eeee-4fff-8aaa-bbbbbbbbbbbb"
)

func TestRunnerGetsOwnerBoundPiTeamPlan(t *testing.T) {
	t.Parallel()
	server := startRuntimeServer(t)
	runner := newTestRunner(t, server, Config{UnaryTimeout: time.Second})

	result, err := runner.Invoke(
		context.Background(),
		actionTeamPlansGet,
		map[string]any{
			"plan_id":       testTeamPlanID,
			"plan_revision": float64(1),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	server.team.mu.Lock()
	request := proto.Clone(server.team.getPlanRequest).(*agentv1.GetTeamPlanV3Request)
	server.team.mu.Unlock()
	if request.GetOwnerId() != "owner-from-config" ||
		request.GetPlanId() != testTeamPlanID ||
		request.GetPlanRevision() != 1 {
		t.Fatalf("unexpected Team Plan request: %#v", request)
	}
	assignments, ok := result["assignments"].([]map[string]any)
	if !ok || len(assignments) != 1 {
		t.Fatalf("assignments = %#v", result["assignments"])
	}
	runtime, ok := assignments[0]["runtime"].(map[string]any)
	if !ok ||
		runtime["family"] != "pi" ||
		runtime["adapter"] != "pi_json_task_v1" {
		t.Fatalf("runtime = %#v", assignments[0]["runtime"])
	}
	if result["execution_id"] != "" {
		t.Fatalf("unapproved Plan execution_id = %#v", result["execution_id"])
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, hidden := range []string{
		"private.registry.example",
		"ami-private",
		"model-credential",
	} {
		if strings.Contains(string(encoded), hidden) {
			t.Fatalf("Team Plan leaked hidden value %q: %s", hidden, encoded)
		}
	}
}

func TestRunnerRecoversExecutionIDFromApprovedTeamPlan(t *testing.T) {
	t.Parallel()
	server := startRuntimeServer(t)
	server.team.mu.Lock()
	server.team.plan.Status =
		agentv1.TeamPlanStatusV3_TEAM_PLAN_STATUS_V3_APPROVED
	server.team.getPlanExecutionID = testTeamExecutionID
	server.team.mu.Unlock()
	runner := newTestRunner(t, server, Config{UnaryTimeout: time.Second})

	result, err := runner.Invoke(
		context.Background(),
		actionTeamPlansGet,
		map[string]any{
			"plan_id":       testTeamPlanID,
			"plan_revision": 1,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result["execution_id"] != testTeamExecutionID ||
		result["status"] != "approved" {
		t.Fatalf("approved Team Plan = %#v", result)
	}
}

func TestRunnerBootstrapsOwnerBoundFirstTeamApprovalDevice(
	t *testing.T,
) {
	t.Parallel()
	server := startRuntimeServer(t)
	runner := newTestRunner(
		t,
		server,
		Config{UnaryTimeout: time.Second},
	)
	publicKey := make([]byte, ed25519.PublicKeySize)
	for index := range publicKey {
		publicKey[index] = byte(index + 1)
	}
	digest := sha256.Sum256(publicKey)
	keyID := "cloud-device-" +
		hex.EncodeToString(digest[:])[:24]
	idempotencyKey := "eeeeeeee-ffff-4000-8111-222222222222"

	result, err := runner.Invoke(
		context.Background(),
		actionTeamApprovalDeviceBootstrap,
		map[string]any{
			"idempotency_key": idempotencyKey,
			"key_id":          keyID,
			"public_key_base64url": base64.RawURLEncoding.
				EncodeToString(publicKey),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	server.team.mu.Lock()
	request := proto.Clone(
		server.team.bootstrapDeviceRequest,
	).(*agentv1.BootstrapFirstTeamApprovalDeviceV3Request)
	server.team.mu.Unlock()
	if request.GetOwnerId() != "owner-from-config" ||
		request.GetIdempotencyKey() != idempotencyKey ||
		request.GetKeyId() != keyID ||
		!strings.EqualFold(
			base64.RawURLEncoding.EncodeToString(
				request.GetPublicKey(),
			),
			base64.RawURLEncoding.EncodeToString(publicKey),
		) {
		t.Fatalf("unexpected Team device request: %#v", request)
	}
	if result["key_id"] != keyID ||
		result["revision"] != int64(1) ||
		result["status"] != "active" ||
		result["expires_at"] == "" {
		t.Fatalf("device result = %#v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "public_key") ||
		strings.Contains(
			string(encoded),
			base64.RawURLEncoding.EncodeToString(publicKey),
		) {
		t.Fatalf("device response leaked public key: %s", encoded)
	}
}

func TestRunnerReturnsStableCodeWhenApprovalDeviceMustBeRelinked(
	t *testing.T,
) {
	t.Parallel()
	server := startRuntimeServer(t)
	server.team.mu.Lock()
	server.team.bootstrapDeviceErr = status.Error(
		codes.FailedPrecondition,
		"another approval device is already linked",
	)
	server.team.mu.Unlock()
	runner := newTestRunner(
		t,
		server,
		Config{UnaryTimeout: time.Second},
	)
	publicKey := make([]byte, ed25519.PublicKeySize)
	for index := range publicKey {
		publicKey[index] = byte(index + 1)
	}
	digest := sha256.Sum256(publicKey)
	keyID := "cloud-device-" + hex.EncodeToString(digest[:])[:24]

	_, err := runner.Invoke(
		context.Background(),
		actionTeamApprovalDeviceBootstrap,
		map[string]any{
			"idempotency_key": "eeeeeeee-ffff-4000-8111-222222222222",
			"key_id":          keyID,
			"public_key_base64url": base64.RawURLEncoding.
				EncodeToString(publicKey),
		},
	)
	if err == nil || err.Error() != "approval device relink required" {
		t.Fatalf("relink error = %v", err)
	}
	var coded interface{ ErrorCode() string }
	if !errors.As(err, &coded) ||
		coded.ErrorCode() != "M_AGENT_APPROVAL_DEVICE_RELINK_REQUIRED" {
		t.Fatalf("relink error code = %v", err)
	}
}

func TestRunnerPreparesOwnerBoundTeamApprovalChallenge(t *testing.T) {
	t.Parallel()
	server := startRuntimeServer(t)
	runner := newTestRunner(t, server, Config{UnaryTimeout: time.Second})
	idempotencyKey := "eeeeeeee-ffff-4000-8111-222222222222"

	result, err := runner.Invoke(
		context.Background(),
		actionTeamPlanApprovalPrepare,
		map[string]any{
			"idempotency_key":               idempotencyKey,
			"plan_id":                       testTeamPlanID,
			"plan_revision":                 1,
			"expected_plan_record_revision": 3,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	server.team.mu.Lock()
	request := proto.Clone(server.team.challengeRequest).(*agentv1.CreateTeamApprovalChallengeV3Request)
	bootstrap := proto.Clone(
		server.team.bootstrapDeviceRequest,
	).(*agentv1.BootstrapFirstTeamApprovalDeviceV3Request)
	server.team.mu.Unlock()
	if request.GetOwnerId() != "owner-from-config" ||
		request.GetIdempotencyKey() != idempotencyKey ||
		request.GetExpectedPlanRecordRevision() != 3 ||
		request.GetSignerKeyId() != bootstrap.GetKeyId() ||
		len(bootstrap.GetPublicKey()) != ed25519.PublicKeySize {
		t.Fatalf("unexpected Team challenge request: %#v", request)
	}
	if result["signing_payload_base64url"] !=
		base64.RawURLEncoding.EncodeToString(
			server.team.challenge.GetSigningPayloadCbor(),
		) {
		t.Fatalf(
			"signing payload = %#v",
			result["signing_payload_base64url"],
		)
	}
	if result["launch_authorization_id"] !=
		testTeamAuthorizationID ||
		result["record_revision"] != int64(2) {
		t.Fatalf("challenge result = %#v", result)
	}
	authorization, ok := result["authorization"].(map[string]any)
	if !ok {
		t.Fatalf("authorization = %#v", result["authorization"])
	}
	retention, ok := authorization["retention"].(map[string]any)
	if !ok || retention["auto_destroy"] != true ||
		retention["maximum_lifetime_seconds"] != uint64(3600) {
		t.Fatalf("retention = %#v", authorization["retention"])
	}
}

func TestRunnerApprovesTeamPlanAndReturnsStableExecutionID(t *testing.T) {
	t.Parallel()
	server := startRuntimeServer(t)
	runner := newTestRunner(t, server, Config{UnaryTimeout: time.Second})
	prepareID := "eeeeeeee-ffff-4000-8111-222222222222"
	prepared, err := runner.Invoke(
		context.Background(),
		actionTeamPlanApprovalPrepare,
		map[string]any{
			"idempotency_key":               prepareID,
			"plan_id":                       testTeamPlanID,
			"plan_revision":                 1,
			"expected_plan_record_revision": 3,
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	result, err := runner.Invoke(
		context.Background(),
		actionTeamPlansApprove,
		map[string]any{
			"idempotency_key":                      "ffffffff-0000-4111-8222-333333333333",
			"approval_prepare_idempotency_key":     prepareID,
			"plan_id":                              testTeamPlanID,
			"plan_revision":                        1,
			"expected_plan_record_revision":        3,
			"expected_challenge_record_revision":   2,
			"expected_challenge_id":                prepared["challenge_id"],
			"expected_plan_digest":                 prepared["plan_digest"],
			"expected_signer_key_id":               prepared["signer_key_id"],
			"expected_launch_authorization_id":     prepared["launch_authorization_id"],
			"expected_launch_authorization_digest": prepared["launch_authorization_digest"],
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	server.team.mu.Lock()
	request := proto.Clone(server.team.approveRequest).(*agentv1.ApproveTeamPlanV3Request)
	bootstrap := proto.Clone(
		server.team.bootstrapDeviceRequest,
	).(*agentv1.BootstrapFirstTeamApprovalDeviceV3Request)
	payload := append([]byte(nil), server.team.challenge.GetSigningPayloadCbor()...)
	server.team.mu.Unlock()
	if request.GetOwnerId() != "owner-from-config" ||
		request.GetExpectedPlanRecordRevision() != 3 ||
		request.GetExpectedChallengeRecordRevision() != 2 ||
		request.GetApproval().GetSignerKeyId() != bootstrap.GetKeyId() ||
		!ed25519.Verify(
			ed25519.PublicKey(bootstrap.GetPublicKey()),
			payload,
			request.GetApproval().GetSignature(),
		) {
		t.Fatalf("unexpected Team approval request: %#v", request)
	}
	if result["execution_id"] != testTeamExecutionID {
		t.Fatalf("execution_id = %#v", result["execution_id"])
	}
	plan, ok := result["plan"].(map[string]any)
	if !ok ||
		plan["status"] != "approved" ||
		plan["execution_id"] != testTeamExecutionID {
		t.Fatalf("approved plan = %#v", result["plan"])
	}
}

func TestRunnerGetsPiFinalAndVerifiedCleanupWithoutInternalRefs(
	t *testing.T,
) {
	t.Parallel()
	server := startRuntimeServer(t)
	runner := newTestRunner(t, server, Config{UnaryTimeout: time.Second})

	result, err := runner.Invoke(
		context.Background(),
		actionTeamExecutionsGet,
		map[string]any{"execution_id": testTeamExecutionID},
	)
	if err != nil {
		t.Fatal(err)
	}
	server.team.mu.Lock()
	request := proto.Clone(server.team.getExecutionRequest).(*agentv1.GetTeamExecutionV3Request)
	server.team.mu.Unlock()
	if request.GetOwnerId() != "owner-from-config" ||
		request.GetExecutionId() != testTeamExecutionID {
		t.Fatalf("unexpected Team execution request: %#v", request)
	}
	if result["status"] != "completed" ||
		result["cleanup_verified"] != true {
		t.Fatalf("execution result = %#v", result)
	}
	report, ok := result["report"].(map[string]any)
	if !ok {
		t.Fatalf("report = %#v", result["report"])
	}
	roles, ok := report["roles"].([]map[string]any)
	if !ok || len(roles) != 1 ||
		roles[0]["runtime_family"] != "pi" {
		t.Fatalf("roles = %#v", report["roles"])
	}
	finals, ok := roles[0]["finals"].([]map[string]any)
	if !ok || len(finals) != 1 ||
		finals[0]["summary"] != "Pi completed the requested change." {
		t.Fatalf("finals = %#v", roles[0]["finals"])
	}
	artifacts, ok := result["artifacts"].([]map[string]any)
	if !ok || len(artifacts) != 1 ||
		artifacts[0]["name"] != "final.json" ||
		artifacts[0]["verification"] != "passed" ||
		artifacts[0]["sha256"] !=
			"sha256:"+strings.Repeat("2", 64) {
		t.Fatalf("artifacts = %#v", result["artifacts"])
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, hidden := range []string{
		"s3://",
		"secret/",
		"private.registry.example",
	} {
		if strings.Contains(string(encoded), hidden) {
			t.Fatalf("execution leaked hidden value %q: %s", hidden, encoded)
		}
	}
}

func TestRunnerReturnsEmptyArtifactListWhileTeamExecutionIsRunning(
	t *testing.T,
) {
	t.Parallel()
	server := startRuntimeServer(t)
	server.team.mu.Lock()
	server.team.execution.Status =
		agentv1.TeamExecutionStatusV3_TEAM_EXECUTION_STATUS_V3_RUNNING
	server.team.execution.Report = nil
	server.team.execution.Artifacts = nil
	server.team.mu.Unlock()
	runner := newTestRunner(t, server, Config{UnaryTimeout: time.Second})

	result, err := runner.Invoke(
		context.Background(),
		actionTeamExecutionsGet,
		map[string]any{"execution_id": testTeamExecutionID},
	)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, ok := result["artifacts"].([]map[string]any)
	if !ok || len(artifacts) != 0 {
		t.Fatalf("artifacts = %#v", result["artifacts"])
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"artifacts":[]`) ||
		strings.Contains(string(encoded), `"artifacts":null`) {
		t.Fatalf("execution JSON = %s", encoded)
	}
}

func TestRunnerRejectsInvalidTeamParamsBeforeRPC(t *testing.T) {
	t.Parallel()
	server := startRuntimeServer(t)
	runner := newTestRunner(t, server, Config{})

	if _, err := runner.Invoke(
		context.Background(),
		actionTeamPlansGet,
		map[string]any{
			"plan_id":       testTeamPlanID,
			"plan_revision": 1,
			"owner_id":      "attacker",
		},
	); err == nil ||
		!strings.Contains(err.Error(), "unsupported field") {
		t.Fatalf("owner override error = %v", err)
	}
	if _, err := runner.Invoke(
		context.Background(),
		actionTeamApprovalDeviceBootstrap,
		map[string]any{
			"idempotency_key": "ffffffff-0000-4111-8222-333333333333",
			"key_id":          testApprovalKeyID,
			"public_key_base64url": base64.RawURLEncoding.
				EncodeToString(
					make(
						[]byte,
						ed25519.PublicKeySize,
					),
				),
			"owner_id": "attacker",
		},
	); err == nil ||
		!strings.Contains(err.Error(), "unsupported field") {
		t.Fatalf("device owner override error = %v", err)
	}
	if _, err := runner.Invoke(
		context.Background(),
		actionTeamPlansApprove,
		map[string]any{
			"idempotency_key":                    "ffffffff-0000-4111-8222-333333333333",
			"approval_prepare_idempotency_key":   "eeeeeeee-ffff-4000-8111-222222222222",
			"plan_id":                            testTeamPlanID,
			"plan_revision":                      1,
			"expected_plan_record_revision":      3,
			"expected_challenge_record_revision": 2,
			"expected_challenge_id":              testTeamChallengeID,
			"expected_plan_digest":               "not-a-digest",
			"expected_signer_key_id":             testApprovalKeyID,
			"expected_launch_authorization_id":   testTeamAuthorizationID,
			"expected_launch_authorization_digest": server.team.challenge.
				GetLaunchAuthorizationDigest(),
		},
	); err == nil ||
		!strings.Contains(err.Error(), "expected_plan_digest") {
		t.Fatalf("confirmation error = %v", err)
	}
	server.team.mu.Lock()
	defer server.team.mu.Unlock()
	if server.team.getPlanRequest != nil ||
		server.team.bootstrapDeviceRequest != nil ||
		server.team.challengeRequest != nil ||
		server.team.approveRequest != nil {
		t.Fatal("invalid Team parameters crossed the gRPC boundary")
	}
}

type teamTestService struct {
	agentv1.UnimplementedTeamPlanServiceServer
	mu                     sync.Mutex
	plan                   *agentv1.TeamPlanV3
	challenge              *agentv1.TeamApprovalChallengeV3
	authorization          *agentv1.TeamLaunchAuthorizationV3
	execution              *agentv1.TeamExecutionV3
	getPlanRequest         *agentv1.GetTeamPlanV3Request
	bootstrapDeviceRequest *agentv1.BootstrapFirstTeamApprovalDeviceV3Request
	challengeRequest       *agentv1.CreateTeamApprovalChallengeV3Request
	approveRequest         *agentv1.ApproveTeamPlanV3Request
	getExecutionRequest    *agentv1.GetTeamExecutionV3Request
	getPlanExecutionID     string
	bootstrapDeviceErr     error
}

func (service *teamTestService) GetTeamPlanV3(
	_ context.Context,
	request *agentv1.GetTeamPlanV3Request,
) (*agentv1.GetTeamPlanV3Response, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.getPlanRequest = proto.Clone(request).(*agentv1.GetTeamPlanV3Request)
	if request.GetOwnerId() != "owner-from-config" ||
		request.GetPlanId() != testTeamPlanID ||
		request.GetPlanRevision() != 1 {
		return nil, status.Error(codes.NotFound, "not found")
	}
	return &agentv1.GetTeamPlanV3Response{
		Plan:        proto.Clone(service.plan).(*agentv1.TeamPlanV3),
		ExecutionId: service.getPlanExecutionID,
	}, nil
}

func (service *teamTestService) BootstrapFirstTeamApprovalDeviceV3(
	_ context.Context,
	request *agentv1.BootstrapFirstTeamApprovalDeviceV3Request,
) (*agentv1.BootstrapFirstTeamApprovalDeviceV3Response, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.bootstrapDeviceRequest = proto.Clone(request).(*agentv1.BootstrapFirstTeamApprovalDeviceV3Request)
	if service.bootstrapDeviceErr != nil {
		return nil, service.bootstrapDeviceErr
	}
	if request.GetOwnerId() != "owner-from-config" {
		return nil, status.Error(
			codes.PermissionDenied,
			"wrong owner",
		)
	}
	return &agentv1.BootstrapFirstTeamApprovalDeviceV3Response{
		KeyId:     request.GetKeyId(),
		Revision:  1,
		ExpiresAt: timestamppb.New(time.Now().UTC().Add(365 * 24 * time.Hour)),
	}, nil
}

func (service *teamTestService) CreateTeamApprovalChallengeV3(
	_ context.Context,
	request *agentv1.CreateTeamApprovalChallengeV3Request,
) (*agentv1.CreateTeamApprovalChallengeV3Response, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.challengeRequest = proto.Clone(request).(*agentv1.CreateTeamApprovalChallengeV3Request)
	if request.GetOwnerId() != "owner-from-config" {
		return nil, status.Error(codes.PermissionDenied, "wrong owner")
	}
	challenge := proto.Clone(service.challenge).(*agentv1.TeamApprovalChallengeV3)
	challenge.SignerKeyId = request.GetSignerKeyId()
	return &agentv1.CreateTeamApprovalChallengeV3Response{
		Challenge:     challenge,
		Authorization: proto.Clone(service.authorization).(*agentv1.TeamLaunchAuthorizationV3),
	}, nil
}

func (service *teamTestService) ApproveTeamPlanV3(
	_ context.Context,
	request *agentv1.ApproveTeamPlanV3Request,
) (*agentv1.ApproveTeamPlanV3Response, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.approveRequest = proto.Clone(request).(*agentv1.ApproveTeamPlanV3Request)
	if request.GetOwnerId() != "owner-from-config" {
		return nil, status.Error(codes.PermissionDenied, "wrong owner")
	}
	plan := proto.Clone(service.plan).(*agentv1.TeamPlanV3)
	plan.Status = agentv1.TeamPlanStatusV3_TEAM_PLAN_STATUS_V3_APPROVED
	plan.RecordRevision++
	plan.UpdatedAt = timestamppb.New(
		plan.GetUpdatedAt().AsTime().Add(time.Second),
	)
	return &agentv1.ApproveTeamPlanV3Response{
		Plan: plan, ExecutionId: testTeamExecutionID,
	}, nil
}

func (service *teamTestService) GetTeamExecutionV3(
	_ context.Context,
	request *agentv1.GetTeamExecutionV3Request,
) (*agentv1.GetTeamExecutionV3Response, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.getExecutionRequest = proto.Clone(request).(*agentv1.GetTeamExecutionV3Request)
	if request.GetOwnerId() != "owner-from-config" ||
		request.GetExecutionId() != testTeamExecutionID {
		return nil, status.Error(codes.NotFound, "not found")
	}
	return &agentv1.GetTeamExecutionV3Response{
		Execution: proto.Clone(service.execution).(*agentv1.TeamExecutionV3),
	}, nil
}

func newTeamTestService() *teamTestService {
	now := time.Now().UTC().Truncate(time.Second).Add(-2 * time.Minute)
	digest := func(value string) string {
		return "sha256:" + strings.Repeat(value, 64)
	}
	providerScope := &agentv1.TeamProviderScopeV3{
		Provider:                agentv1.TeamCloudProviderV3_TEAM_CLOUD_PROVIDER_V3_AWS,
		CloudConnectionId:       testCloudConnectionID,
		CloudConnectionRevision: 4,
		AccountId:               "066107820442",
	}
	permissions := &agentv1.TeamWorkerPermissionSetV3{
		Workspace:       "isolated",
		NetworkServices: []string{"github.com", "api.openai.com"},
		ToolScopes:      []string{"shell", "git", "test"},
		MaxTempDiskMib:  4096,
	}
	marketplace := &agentv1.TeamWorkerMarketplaceBindingV3{
		SchemaVersion:            "dirextalk.agent.worker-marketplace-binding/v1",
		RegistryId:               "registry-private",
		RegistryRevision:         digest("1"),
		ReleaseId:                "pi-v0.83.0",
		WorkerTypeId:             "pi-software-worker",
		PublisherId:              "dirextalk",
		PublisherDisplayName:     "Dirextalk",
		PublisherTier:            "verified",
		OrganizationId:           "dirextalk",
		ManifestDigest:           digest("2"),
		ImageRepository:          "private.registry.example/pi-worker",
		ImageDigest:              digest("3"),
		ImageSignatureDigest:     digest("4"),
		SbomDigest:               digest("5"),
		ProvenanceEnvelopeDigest: digest("6"),
		ReviewId:                 "review-pi-v1",
		ReviewPolicyRevision:     digest("7"),
		ReviewRiskClass:          "standard",
		ReviewValidUntil:         timestamppb.New(now.Add(30 * 24 * time.Hour)),
		GrantedPermissions:       permissions,
	}
	assignment := &agentv1.TeamWorkerAssignmentV3{
		RoleId:    "implementer",
		Title:     "Pi implementation worker",
		Objective: "Implement and verify the requested repository change.",
		WorkClass: agentv1.TeamWorkClassV3_TEAM_WORK_CLASS_V3_SOFTWARE_IMPLEMENTATION,
		RequiredCapabilities: []agentv1.TeamCapabilityV3{
			agentv1.TeamCapabilityV3_TEAM_CAPABILITY_V3_REPOSITORY_READ,
			agentv1.TeamCapabilityV3_TEAM_CAPABILITY_V3_REPOSITORY_WRITE,
			agentv1.TeamCapabilityV3_TEAM_CAPABILITY_V3_SHELL,
			agentv1.TeamCapabilityV3_TEAM_CAPABILITY_V3_GIT,
			agentv1.TeamCapabilityV3_TEAM_CAPABILITY_V3_TEST,
			agentv1.TeamCapabilityV3_TEAM_CAPABILITY_V3_STRUCTURED_RESULTS,
		},
		WorkspaceMode:      agentv1.TeamWorkspaceModeV3_TEAM_WORKSPACE_MODE_V3_ISOLATED,
		RuntimeReleaseId:   "pi-v0.83.0",
		RuntimeFamily:      agentv1.TeamRuntimeFamilyV3_TEAM_RUNTIME_FAMILY_V3_PI,
		RuntimeVersion:     "0.83.0",
		RuntimeImageDigest: digest("3"),
		RuntimeAdapter:     agentv1.TeamRuntimeAdapterV3_TEAM_RUNTIME_ADAPTER_V3_PI_JSON_TASK_V1,
		ModelProfileId:     "model-credential-private",
		ModelProvider:      "openai",
		Model:              "gpt-5",
		ModelInterface:     agentv1.TeamModelInterfaceV3_TEAM_MODEL_INTERFACE_V3_OPENAI_RESPONSES,
		ComputeOfferId:     "aws-t3-small",
		InstanceType:       "t3.small",
		Resources: &agentv1.TeamResourceEnvelopeV3{
			Vcpu: 2, MemoryMib: 2048, DiskGib: 16,
			Architecture: agentv1.TeamArchitectureV3_TEAM_ARCHITECTURE_V3_AMD64,
		},
		Duration: &agentv1.TeamDurationEstimateV3{
			MinimumSeconds: 300, ExpectedSeconds: 900,
			MaximumSeconds: 1800,
		},
		Tokens: &agentv1.TeamTokenEstimateV3{
			InputMinimum: 5000, InputExpected: 10000,
			InputMaximum: 20000, OutputMinimum: 1000,
			OutputExpected: 4000, OutputMaximum: 8000,
		},
		ColdStartSeconds: 45,
		Marketplace:      marketplace,
	}
	plan := &agentv1.TeamPlanV3{
		SchemaVersion:          "dirextalk.agent.team-plan/v3",
		TaskId:                 testTeamTaskID,
		PlanId:                 testTeamPlanID,
		PlanRevision:           1,
		OwnerId:                "owner-from-config",
		GoalDigest:             digest("8"),
		ProviderScope:          proto.Clone(providerScope).(*agentv1.TeamProviderScopeV3),
		Region:                 "ap-northeast-3",
		RuntimeCatalogRevision: digest("9"),
		PolicyRevision:         digest("a"),
		PricingSnapshotId:      "eeeeeeee-ffff-4000-8111-222222222222",
		PricingSnapshotDigest:  digest("b"),
		QuotedAt:               timestamppb.New(now),
		ValidUntil:             timestamppb.New(now.Add(15 * time.Minute)),
		ProposalConfidence:     92,
		ProposalRationale:      "This task needs repository writes and tests, so one isolated Pi Worker is appropriate.",
		WorkerCount:            1,
		MaxConcurrentWorkers:   1,
		Assignments:            []*agentv1.TeamWorkerAssignmentV3{assignment},
		Schedule: &agentv1.TeamScheduleEstimateV3{
			MinimumWallSeconds: 345, ExpectedWallSeconds: 945,
			MaximumWallSeconds: 1845,
		},
		Cost: &agentv1.TeamCostEstimateV3{
			Currency: "USD", MinimumMicros: 2500,
			ExpectedMicros: 8000, MaximumMicros: 18000,
			HardBudgetMicros: 20000,
			Roles: []*agentv1.TeamRoleCostEstimateV3{{
				RoleId:                "implementer",
				ComputeMinimumMicros:  1000,
				ComputeExpectedMicros: 3000,
				ComputeMaximumMicros:  7000,
				ModelMinimumMicros:    1500,
				ModelExpectedMicros:   5000,
				ModelMaximumMicros:    11000,
				TotalMinimumMicros:    2500,
				TotalExpectedMicros:   8000,
				TotalMaximumMicros:    18000,
			}},
			Assumptions: []string{"One on-demand Pi Worker"},
			Exclusions:  []string{"External paid APIs"},
		},
		PlanDigest:     digest("c"),
		Status:         agentv1.TeamPlanStatusV3_TEAM_PLAN_STATUS_V3_READY_FOR_CONFIRMATION,
		RecordRevision: 3,
		CreatedAt:      timestamppb.New(now),
		UpdatedAt:      timestamppb.New(now),
		TaskInput: &agentv1.TeamTaskInputBindingV3{
			SchemaVersion: "dirextalk.agent.team-task-input/v2",
			InputId:       "ffffffff-0000-4111-8222-333333333333",
			InputDigest:   digest("d"),
			SourceKind:    agentv1.TeamInputSourceKindV3_TEAM_INPUT_SOURCE_KIND_V3_GITHUB_REPOSITORY,
			Repository: &agentv1.TeamGitRepositorySourceV3{
				Provider: "github", Host: "github.com",
				ConnectionId: "github-private", RepositoryId: "repo-private",
				Owner: "YingSuiAI", Name: "dirextalk-message-server",
				BaseCommitSha: strings.Repeat("e", 40),
				BaseRef:       "refs/heads/codex/native-agent-v2",
			},
			SourceDigest: digest("e"),
		},
	}
	challenge := &agentv1.TeamApprovalChallengeV3{
		SchemaVersion:             "dirextalk.agent.team-plan-challenge/v2",
		ChallengeRevision:         1,
		ApprovalId:                testTeamApprovalID,
		ChallengeId:               testTeamChallengeID,
		AgentInstanceId:           "ffffffff-aaaa-4bbb-8ccc-dddddddddddd",
		OwnerId:                   "owner-from-config",
		PlanId:                    testTeamPlanID,
		PlanRevision:              1,
		PlanDigest:                plan.GetPlanDigest(),
		GoalDigest:                plan.GetGoalDigest(),
		ProviderScope:             proto.Clone(providerScope).(*agentv1.TeamProviderScopeV3),
		RuntimeCatalogRevision:    plan.GetRuntimeCatalogRevision(),
		PolicyRevision:            plan.GetPolicyRevision(),
		PricingSnapshotId:         plan.GetPricingSnapshotId(),
		PricingSnapshotDigest:     plan.GetPricingSnapshotDigest(),
		QuotedAt:                  plan.GetQuotedAt(),
		QuoteValidUntil:           plan.GetValidUntil(),
		WorkerCount:               1,
		MaxConcurrentWorkers:      1,
		Currency:                  "USD",
		MinimumCostMicros:         2500,
		ExpectedCostMicros:        8000,
		MaximumCostMicros:         18000,
		HardBudgetMicros:          20000,
		MinimumWallSeconds:        345,
		ExpectedWallSeconds:       945,
		MaximumWallSeconds:        1845,
		SignerKeyId:               testApprovalKeyID,
		IssuedAt:                  timestamppb.New(now.Add(time.Minute)),
		ExpiresAt:                 timestamppb.New(now.Add(10 * time.Minute)),
		SigningPayloadCbor:        []byte("opaque-team-plan-cbor"),
		RecordRevision:            2,
		CreatedAt:                 timestamppb.New(now.Add(time.Minute)),
		UpdatedAt:                 timestamppb.New(now.Add(time.Minute)),
		LaunchAuthorizationId:     testTeamAuthorizationID,
		LaunchAuthorizationDigest: digest("f"),
	}
	authorization := &agentv1.TeamLaunchAuthorizationV3{
		SchemaVersion:   "dirextalk.agent.team-launch-authorization/v1",
		AuthorizationId: testTeamAuthorizationID,
		AgentInstanceId: challenge.GetAgentInstanceId(),
		OwnerId:         "owner-from-config",
		PlanId:          testTeamPlanID,
		PlanRevision:    1,
		PlanDigest:      plan.GetPlanDigest(),
		ApprovalId:      testTeamApprovalID,
		ProviderScope:   proto.Clone(providerScope).(*agentv1.TeamProviderScopeV3),
		Region:          "ap-northeast-3",
		Network: &agentv1.TeamLaunchNetworkV3{
			ConnectivityMode: "private_egress_only",
			VpcId:            "vpc-private", SubnetId: "subnet-private",
			SecurityGroupMode: "managed", PublicIpv4: false,
			PublicInbound: false,
		},
		Retention: &agentv1.TeamLaunchRetentionV3{
			RetentionClass: "ephemeral", AutoDestroy: true,
			MaximumLifetimeSeconds: 3600, DestroyGraceSeconds: 60,
		},
		WorkerCount:                  1,
		MaxConcurrentBillableWorkers: 1,
		Currency:                     "USD",
		HardBudgetMicros:             20000,
		RequiresFreshQuote:           true,
		MaximumQuoteAgeSeconds:       900,
		LaunchNotBefore:              timestamppb.New(now.Add(time.Minute)),
		LaunchNotAfter:               timestamppb.New(now.Add(10 * time.Minute)),
		Roles: []*agentv1.TeamRoleLaunchAuthorizationV3{{
			RoleId: "implementer", RuntimeReleaseId: "pi-v0.83.0",
			RuntimeImageDigest: digest("3"),
			ComputeOfferId:     "aws-t3-small",
			InstanceType:       "t3.small",
			Architecture:       agentv1.TeamArchitectureV3_TEAM_ARCHITECTURE_V3_AMD64,
			Vcpu:               2, MemoryMib: 2048, MaximumApprovedCostMicros: 20000,
			WorkerImage: &agentv1.TeamLaunchWorkerImageV3{
				ImageId: "ami-private", ImageDigest: digest("3"),
			},
			Marketplace: proto.Clone(marketplace).(*agentv1.TeamWorkerMarketplaceBindingV3),
		}},
	}
	usage := &agentv1.TeamRuntimeUsageV3{
		InputTokens: 9000, CachedInputTokens: 2000,
		OutputTokens: 3500, ReasoningOutputTokens: 500,
	}
	report := &agentv1.TeamExecutionReportV3{
		SchemaVersion: "dirextalk.agent.team-execution-report/v1",
		ExecutionId:   testTeamExecutionID,
		OwnerId:       "owner-from-config",
		TaskId:        testTeamTaskID,
		PlanId:        testTeamPlanID,
		PlanRevision:  1,
		PlanDigest:    plan.GetPlanDigest(),
		Roles: []*agentv1.TeamExecutionRoleReportV3{{
			RoleId:               "implementer",
			Title:                "Pi implementation worker",
			RuntimeFamily:        agentv1.TeamRuntimeFamilyV3_TEAM_RUNTIME_FAMILY_V3_PI,
			RuntimeAdapter:       agentv1.TeamRuntimeAdapterV3_TEAM_RUNTIME_ADAPTER_V3_PI_JSON_TASK_V1,
			Outcome:              "succeeded",
			ResultEvidenceDigest: digest("1"),
			Finals: []*agentv1.TeamExecutionFinalV3{{
				ActionId:       "implement",
				RuntimeAdapter: agentv1.TeamRuntimeAdapterV3_TEAM_RUNTIME_ADAPTER_V3_PI_JSON_TASK_V1,
				Usage:          proto.Clone(usage).(*agentv1.TeamRuntimeUsageV3),
				Status:         "completed",
				Summary:        "Pi completed the requested change.",
				Deliverables:   []string{"Verified patch"},
				Tests:          []string{"go test ./..."},
				Risks:          []string{"No known residual risk"},
				ArtifactSha256: digest("2"),
			}},
		}},
		TotalUsage:   usage,
		ReportDigest: digest("3"),
		GeneratedAt:  timestamppb.New(now.Add(20 * time.Minute)),
	}
	execution := &agentv1.TeamExecutionV3{
		SchemaVersion: "dirextalk.agent.team-execution/v3",
		ExecutionId:   testTeamExecutionID,
		OwnerId:       "owner-from-config",
		TaskId:        testTeamTaskID,
		PlanId:        testTeamPlanID,
		PlanRevision:  1,
		PlanDigest:    plan.GetPlanDigest(),
		Status:        agentv1.TeamExecutionStatusV3_TEAM_EXECUTION_STATUS_V3_COMPLETED,
		WorkerCount:   1, MaxConcurrentWorkers: 1, RecordRevision: 8,
		CreatedAt: timestamppb.New(now.Add(2 * time.Minute)),
		UpdatedAt: timestamppb.New(now.Add(20 * time.Minute)),
		Report:    report,
		TaskInput: proto.Clone(plan.GetTaskInput()).(*agentv1.TeamTaskInputBindingV3),
		Artifacts: []*agentv1.TeamExecutionArtifactV3{{
			SchemaVersion:      teamArtifactSchemaV1,
			ArtifactId:         "cccccccc-dddd-4eee-8fff-000000000001",
			RoleId:             "implementer",
			ActionId:           "implement",
			Name:               "final.json",
			Kind:               "result",
			MediaType:          "application/json",
			SizeBytes:          256,
			Sha256:             digest("2"),
			Verification:       "passed",
			CreatedAt:          timestamppb.New(now.Add(19 * time.Minute)),
			RetentionExpiresAt: timestamppb.New(now.Add(90 * 24 * time.Hour)),
		}},
	}
	return &teamTestService{
		plan: plan, challenge: challenge,
		authorization: authorization, execution: execution,
	}
}

func TestMapTeamMarketplaceAcceptsPermanentOfficialReviewOnly(t *testing.T) {
	t.Parallel()
	marketplace := &agentv1.TeamWorkerMarketplaceBindingV3{
		PublisherDisplayName: "Dirextalk Official",
		PublisherTier:        "dirextalk_official",
		WorkerTypeId:         "pi-worker",
		ReviewRiskClass:      "high",
		GrantedPermissions: &agentv1.TeamWorkerPermissionSetV3{
			Workspace: "isolated",
		},
	}
	mapped, err := mapTeamMarketplace(marketplace)
	if err != nil || mapped["review_valid_until"] != nil {
		t.Fatalf("permanent official Marketplace=%#v error=%v", mapped, err)
	}
	marketplace.PublisherTier = "verified_partner"
	if _, err := mapTeamMarketplace(marketplace); err == nil {
		t.Fatal("accepted permanent non-official Marketplace review")
	}
}
