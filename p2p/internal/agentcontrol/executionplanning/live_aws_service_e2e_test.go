//go:build liveaws

package executionplanning

import (
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/agentrecipes"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/artifactstore"
	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/executionrunner"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/executiontarget"
	agentembedded "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentembedded"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/test"
	awsapi "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/google/uuid"
)

type liveTargetCredentialStore struct {
	owner string
	repo  *storage.PostgresAWSRepository
}

func (s liveTargetCredentialStore) GetCredentialRevision(ctx context.Context, owner, id string, revision int64) (coreaws.Credentials, error) {
	if owner != s.owner || s.repo == nil {
		return coreaws.Credentials{}, coreaws.ErrNotFound
	}
	return s.repo.GetCredentialRevision(ctx, id, revision)
}

type liveSecretParameterRevoker struct {
	store       *storage.DatabaseExecutionSecretParameterRuntime
	executor    *coreaws.SecretParameterProvisionExecutor
	credentials *storage.PostgresAWSRepository
}

func (r *liveSecretParameterRevoker) RevokeAuthorizedSecretParameter(ctx context.Context, lease coreaws.SecretParameterLease) error {
	if r == nil || r.store == nil || r.executor == nil || r.credentials == nil || strings.TrimSpace(lease.OwnerID) == "" || !lease.FenceDigest.Valid() {
		return coreaws.ErrSecretParameterInvalid
	}
	record, err := r.store.GetSecretParameterIntent(ctx, lease.OwnerID, lease.FenceDigest)
	if err != nil {
		return err
	}
	if record.Status == "revoked" {
		return nil
	}
	req := record.Intent.Request
	if record.Status != "completed" || record.Intent.ParameterName != lease.ParameterName ||
		req.OwnerID != lease.OwnerID || req.RunID != lease.RunID || req.StageID != lease.ProvisionStageID ||
		req.AttemptID != lease.ProvisionAttemptID || req.Target.ID != lease.TargetID ||
		req.Target.Revision != lease.TargetRevision || req.Target.Digest != lease.TargetDigest ||
		req.SecretRef != lease.SecretRef || req.FenceDigest != lease.FenceDigest || req.RequestDigest != lease.RequestDigest {
		return coreaws.ErrSecretParameterUncertain
	}
	credential, err := r.credentials.GetCredentialRevision(ctx, req.CredentialID, int64(req.CredentialRevision))
	if err != nil || credential.ID != req.CredentialID || credential.Revision != int64(req.CredentialRevision) || credential.VerifiedRevision != credential.Revision {
		return coreaws.ErrSecretParameterUncertain
	}
	req.Credential = credential
	return r.executor.Revoke(ctx, req)
}

func TestLivePinnedOCIImageAnalysis(t *testing.T) {
	images := liveImageList(os.Getenv("DIREXTALK_LIVE_OCI_IMAGES"))
	if openClaw := strings.TrimSpace(os.Getenv("DIREXTALK_LIVE_OPENCLAW_IMAGE")); openClaw != "" {
		images = append(images, openClaw)
	}
	if len(images) == 0 {
		t.Skip("set a digest-pinned live OCI image for the explicit registry acceptance test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	analyzer := NewPublicOCIRegistryAnalyzer()
	for index, image := range images {
		facts, err := analyzer.AnalyzePinnedImage(ctx, image)
		if err != nil || len(facts.BlockingUncertainties) != 0 {
			t.Fatalf("image %d analysis blocked: blockers=%v err=%v", index+1, facts.BlockingUncertainties, err)
		}
	}
}

// This test is deliberately excluded from ordinary builds. It is the operator
// acceptance path for the real execution.v2 control plane: encrypted AWS
// credential storage, signed catalog pricing, immutable planning, durable
// confirmations, CloudFormation provisioning, and typed SSM execution. It
// never opens public inbound traffic and never logs credential material.
func TestLiveAWSGenericContainerServices(t *testing.T) {
	if os.Getenv("DIREXTALK_LIVE_AWS") != "1" {
		t.Skip("set DIREXTALK_LIVE_AWS=1 for the explicitly authorized live test")
	}
	credentialFile := strings.TrimSpace(os.Getenv("DIREXTALK_LIVE_AWS_CREDENTIAL_CSV"))
	images := liveImageList(os.Getenv("DIREXTALK_LIVE_OCI_IMAGES"))
	openClawImage := strings.TrimSpace(os.Getenv("DIREXTALK_LIVE_OPENCLAW_IMAGE"))
	openRouterKeyFile := strings.TrimSpace(os.Getenv("DIREXTALK_LIVE_OPENROUTER_KEY_FILE"))
	if credentialFile == "" || len(images) == 0 {
		t.Fatal("live AWS credential file and at least one digest-pinned image are required")
	}
	if (openClawImage == "") != (openRouterKeyFile == "") {
		t.Fatal("OpenClaw image and private OpenRouter key file must be supplied together")
	}
	accessKey, secretKey, sessionToken := readLiveAWSCredential(t, credentialFile)
	defer zeroStrings(&accessKey, &secretKey, &sessionToken)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	preflightConfig := awsapi.Config{
		Region:           "us-east-1",
		Credentials:      awsapi.NewCredentialsCache(credentials.NewStaticCredentialsProvider(accessKey, secretKey, sessionToken)),
		RetryMode:        awsapi.RetryModeStandard,
		RetryMaxAttempts: 3,
	}
	preflightIdentity, err := sts.NewFromConfig(preflightConfig).GetCallerIdentity(ctx, nil)
	if err != nil || preflightIdentity == nil || preflightIdentity.Account == nil || preflightIdentity.Arn == nil || preflightIdentity.UserId == nil {
		t.Fatal("live AWS SDK credential preflight failed")
	}
	owner := "@execution-v2-live:example.invalid"
	var openRouterSecret storage.ExecutionSecretMetadata

	conn, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	databaseOptions := config.DatabaseOptions{ConnectionString: config.DataSource(conn)}
	database, err := storage.NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, databaseOptions), &databaseOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	parameterClient := ssm.NewFromConfig(preflightConfig)
	defer func() {
		cleanupLiveSecretParameters(t, database.DB(), parameterClient, owner, openRouterSecret.SecretRef)
	}()
	store := storage.NewDatabaseExecutionStore(database.DB(), time.Now)
	coordinator := storage.NewDatabaseExecutionCoordinator(database.DB(), time.Now)

	secretRoot := filepath.Join(t.TempDir(), "secret")
	if err = os.MkdirAll(secretRoot, 0700); err != nil {
		t.Fatal(err)
	}
	keyring, err := storage.LoadOrCreateAgentSecretKeyring(filepath.Join(secretRoot, "keyring.json"))
	if err != nil {
		t.Fatal(err)
	}
	enveloper, err := storage.NewAgentSecretEnveloper(keyring)
	if err != nil {
		t.Fatal(err)
	}
	credentialRepo, err := storage.NewAgentAWSRepositoryWithEnveloper(database, owner, enveloper)
	if err != nil {
		t.Fatal(err)
	}
	factory := coreaws.NewSDKFactory()
	stsProvider, err := coreaws.NewSDKProvider(factory)
	if err != nil {
		t.Fatal(err)
	}
	awsService := coreaws.NewService(credentialRepo, nil, nil, stsProvider, nil, time.Now)
	credentialView, err := awsService.SaveCredential(ctx, coreaws.CredentialInput{
		Name: "execution-v2-live", Region: "us-east-1", AccessKeyID: accessKey,
		SecretAccessKey: secretKey, SessionToken: sessionToken, IdempotencyKey: uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = awsService.TestCredential(ctx, credentialView.ID, credentialView.Revision, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	credential, err := credentialRepo.GetCredentialRevision(ctx, credentialView.ID, credentialView.Revision)
	if err != nil || credential.VerifiedRevision != credential.Revision {
		t.Fatalf("verified credential unavailable: %v", err)
	}

	var executionSecrets *storage.DatabaseExecutionSecretStore
	var secretParameters *storage.DatabaseExecutionSecretParameterRuntime
	var secretExecutor *coreaws.SecretParameterProvisionExecutor
	var secretRevoker *liveSecretParameterRevoker
	if openClawImage != "" {
		executionSecrets = storage.NewDatabaseExecutionSecretStore(database.DB(), enveloper, time.Now)
		openRouterKey := readLiveSecretValue(t, openRouterKeyFile)
		openRouterSecret, err = executionSecrets.CreateExecutionSecret(ctx, storage.ExecutionSecretCreateRequest{
			OwnerID: owner, Provider: "openrouter", Purpose: coreexecution.AISecretPurposeProviderAPIKey,
			Value: openRouterKey, IdempotencyID: uuid.NewString(),
		})
		zeroStrings(&openRouterKey)
		if err != nil {
			t.Fatal("cannot persist the encrypted OpenRouter execution secret")
		}
		secretParameters = storage.NewDatabaseExecutionSecretParameterRuntime(database.DB(), executionSecrets)
		parameterProvider, providerErr := coreaws.NewSDKSecretParameterProvider(factory)
		if providerErr != nil {
			t.Fatal(providerErr)
		}
		secretExecutor, err = coreaws.NewSecretParameterProvisionExecutor(secretParameters, parameterProvider, secretParameters)
		if err != nil {
			t.Fatal(err)
		}
		secretRevoker = &liveSecretParameterRevoker{store: secretParameters, executor: secretExecutor, credentials: credentialRepo}
	}
	availability := storage.ExecutionExecutorAvailability{AWSSSM: true, ComputeProvision: true, SecretProvision: secretExecutor != nil && secretRevoker != nil}
	store.SetExecutorAvailability(availability)
	coordinator.SetExecutorAvailability(availability)

	artifacts, err := artifactstore.New(t.TempDir(), artifactstore.MaxArtifactSize)
	if err != nil {
		t.Fatal(err)
	}
	artifactResolver := storage.NewFilesystemArtifactResolver(store, artifacts)
	receiptResolver := storage.NewDatabaseDispatchReceiptResolver(store, credentialRepo)
	transportOptions := []coreaws.SSMTransportOption{
		coreaws.WithImmutableArtifactResolver(artifactResolver),
		coreaws.WithDispatchReceiptResolver(receiptResolver),
	}
	if secretParameters != nil && secretRevoker != nil {
		transportOptions = append(transportOptions, coreaws.WithSecretParameterRuntime(secretParameters, secretRevoker))
	}
	transport, err := coreaws.NewSSMTransport(factory, transportOptions...)
	if err != nil {
		t.Fatal(err)
	}
	provisionProvider, err := coreaws.NewCloudFormationProvisionProvider(factory)
	if err != nil {
		t.Fatal(err)
	}
	provisioner, err := coreaws.NewEC2ProvisionExecutor(store, provisionProvider, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	outputs := storage.NewExecutionServiceOutputMaterializer(store, artifacts, owner)
	stageStore := storage.NewExecutionStageStoreAdapterWithOutputs(store, outputs)
	runner, err := executionrunner.NewRunner(executionrunner.Config{
		Store: stageStore, Resolver: storage.NewExecutionStepResolver(store, credentialRepo, artifactResolver),
		Transport: transport, EC2Provisioner: provisioner, Holder: "execution-v2-live",
		LeaseTTL: 10 * time.Minute, PollInterval: 5 * time.Second, Now: time.Now,
		ReceiptResolver: receiptResolver, SecretProvisioner: secretExecutor,
	})
	if err != nil {
		t.Fatal(err)
	}

	targetService := executiontarget.New(executiontarget.Config{
		Targets: store, Credentials: liveTargetCredentialStore{owner: owner, repo: credentialRepo},
		Reservations: executiontarget.NewAWSReservationCatalog(factory, time.Now), Now: time.Now,
	})
	reservation, err := targetService.Reserve(ctx, owner, executiontarget.ReserveRequest{
		CredentialID: credentialView.ID, CredentialRevision: uint64(credentialView.Revision),
		InstanceType: "t3.medium", VolumeGiB: 20, IdempotencyKey: uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	stackName := coreaws.EC2ProvisionOperationKey(reservation.ID)
	if stackName == "" {
		t.Fatal("empty deterministic CloudFormation stack name")
	}
	cleanupConfig := awsapi.Config{
		Region:      "us-east-1",
		Credentials: awsapi.NewCredentialsCache(credentials.NewStaticCredentialsProvider(accessKey, secretKey, sessionToken)),
		RetryMode:   awsapi.RetryModeStandard, RetryMaxAttempts: 3,
	}
	cleanupClient := cloudformation.NewFromConfig(cleanupConfig)
	if os.Getenv("DIREXTALK_LIVE_KEEP_STACK") == "1" {
		t.Logf("operator cleanup required for stack=%s", stackName)
	} else {
		t.Cleanup(func() { cleanupLiveStack(t, cleanupClient, stackName) })
	}
	t.Logf("reserved target=%s stack=%s az=%s quote=%s %s/hour", reservation.ID, stackName, reservation.ComputeReservation.AvailabilityZone, reservation.ComputeReservation.CostQuote.Amount, reservation.ComputeReservation.CostQuote.Currency)

	recipes, err := agentrecipes.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	planner := New(Config{
		AnalysisStore: store, PlanStore: store, RevisionWriter: store,
		Sources: NewProductionSourceResolver(store, artifacts), Targets: NewDatabaseTargetResolver(store),
		Bindings: NewProductionBindingResolver(store, time.Now), Executors: NewArtifactExecutorSealer(artifacts),
		Recipes: recipes, ExecutionSecrets: executionSecrets, Now: time.Now,
	})
	if !planner.PlanReady() {
		t.Fatal("production planner is not ready")
	}

	bootstrapProject := uuid.NewString()
	bootstrapAnalysis := analyzeLiveImage(t, ctx, planner, owner, bootstrapProject, images[0])
	provisionPlan, err := planner.Compile(ctx, owner, agentembedded.ExecutionV2PlanCreateRequest{
		ProjectID: bootstrapProject, AnalysisID: bootstrapAnalysis.AnalysisID,
		Intent: "provision", RecipeID: "aws-ec2-provision", TargetID: reservation.ID,
		TargetRevision: reservation.Revision, Purpose: coreexecution.PurposeService,
		IdempotencyKey: uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	provisionRun := driveLiveRun(t, ctx, owner, provisionPlan, store, coordinator, runner)
	if provisionRun.Run.Status != coreexecution.RunSucceeded {
		t.Fatalf("provision run status=%s", provisionRun.Run.Status)
	}
	target, err := store.GetTarget(ctx, owner, reservation.ID, 2)
	if err != nil || target.Kind != coreexecution.TargetKindAWSEC2Instance {
		t.Fatalf("provisioned target unavailable: %v", err)
	}
	instanceID := ""
	for _, capability := range target.Capabilities {
		if strings.HasPrefix(capability, coreaws.TargetInstanceCapabilityPrefix) {
			instanceID = strings.TrimPrefix(capability, coreaws.TargetInstanceCapabilityPrefix)
		}
	}
	if !coreaws.ValidEC2InstanceID(instanceID) {
		t.Fatal("provisioned target lacks exact instance identity")
	}
	t.Logf("provisioned instance=%s target_revision=%d", instanceID, target.Revision)

	for index, image := range images {
		projectID := uuid.NewString()
		analysis := analyzeLiveImage(t, ctx, planner, owner, projectID, image)
		plan, planErr := planner.Compile(ctx, owner, agentembedded.ExecutionV2PlanCreateRequest{
			ProjectID: projectID, AnalysisID: analysis.AnalysisID, Intent: "deploy",
			RecipeID: "generic-container-service", TargetID: target.ID, TargetRevision: target.Revision,
			Purpose: coreexecution.PurposeService, IdempotencyKey: uuid.NewString(),
		})
		if planErr != nil {
			t.Fatalf("compile image %d: %v", index+1, planErr)
		}
		run := driveLiveRun(t, ctx, owner, plan, store, coordinator, runner)
		if run.Run.Status != coreexecution.RunSucceeded {
			t.Fatalf("deploy image %d status=%s", index+1, run.Run.Status)
		}
		bindings, _, listErr := store.ListServiceBindings(ctx, owner, projectID, "", 10)
		if listErr != nil || len(bindings) != 1 || bindings[0].RunID != run.Run.RunID {
			t.Fatalf("service binding image %d: count=%d err=%v", index+1, len(bindings), listErr)
		}
		t.Logf("service %d succeeded plan=%s run=%s binding=%s endpoint=%s", index+1, plan.ID, run.Run.RunID, bindings[0].BindingID, bindings[0].Endpoint)
	}
	if openClawImage != "" {
		projectID := uuid.NewString()
		analysis := analyzeLiveImage(t, ctx, planner, owner, projectID, openClawImage)
		configuration := &coreexecution.AIConfiguration{
			Mode: coreexecution.AIAuthModeAPIKey, Provider: "openrouter",
			SecretRef: openRouterSecret.SecretRef, SecretRevision: openRouterSecret.Revision,
			SecretPurpose: openRouterSecret.Purpose, SecretBindingDigest: openRouterSecret.BindingDigest,
		}
		plan, planErr := planner.Compile(ctx, owner, agentembedded.ExecutionV2PlanCreateRequest{
			ProjectID: projectID, AnalysisID: analysis.AnalysisID, Intent: "deploy",
			RecipeID: "generic-container-service", TargetID: target.ID, TargetRevision: target.Revision,
			Purpose: coreexecution.PurposeService, AIConfiguration: configuration, IdempotencyKey: uuid.NewString(),
		})
		if planErr != nil {
			t.Fatalf("compile OpenClaw API-key deployment: %v", planErr)
		}
		if !hasLiveAIStages(plan) {
			t.Fatal("OpenClaw plan lacks the independent secret authorization and container consumer stages")
		}
		run := driveLiveRun(t, ctx, owner, plan, store, coordinator, runner)
		if run.Run.Status != coreexecution.RunSucceeded {
			t.Fatalf("OpenClaw deployment status=%s", run.Run.Status)
		}
		bindings, _, listErr := store.ListServiceBindings(ctx, owner, projectID, "", 10)
		if listErr != nil || len(bindings) != 1 || bindings[0].RunID != run.Run.RunID {
			t.Fatalf("OpenClaw service binding: count=%d err=%v", len(bindings), listErr)
		}
		var parameterName, parameterStatus string
		if queryErr := database.DB().QueryRowContext(ctx, `SELECT parameter_name,status FROM core_execution_secret_parameter_intents WHERE owner_id=$1 AND run_id=$2`, owner, run.Run.RunID).Scan(&parameterName, &parameterStatus); queryErr != nil || parameterStatus != "revoked" {
			t.Fatalf("OpenClaw parameter lease status=%q err=%v", parameterStatus, queryErr)
		}
		if _, getErr := parameterClient.GetParameter(ctx, &ssm.GetParameterInput{Name: awsapi.String(parameterName), WithDecryption: awsapi.Bool(false)}); !isLiveParameterNotFound(getErr) {
			t.Fatalf("OpenClaw parameter was not revoked from AWS: %v", getErr)
		}
		t.Logf("OpenClaw API-key smoke succeeded plan=%s run=%s binding=%s endpoint=%s", plan.ID, run.Run.RunID, bindings[0].BindingID, bindings[0].Endpoint)
	}
	if err = ctx.Err(); err != nil {
		t.Fatal(err)
	}
}

func hasLiveAIStages(plan coreexecution.ExecutionPlan) bool {
	authorization := false
	consumer := false
	for _, stage := range plan.Stages {
		if stage.StageKey == "authorize-ai" && stage.Gate == coreexecution.GateSecretAccess && len(stage.Steps) == 1 && stage.Steps[0].Kind == coreexecution.StepSecretProvision {
			authorization = true
		}
		for _, step := range stage.Steps {
			if step.Kind == coreexecution.StepContainerApply && len(step.SecretRefs) == 1 && step.SecretRefs[0].Purpose == coreexecution.AISecretPurposeProviderAPIKey {
				for _, dependency := range stage.DependsOn {
					consumer = consumer || dependency == "authorize-ai"
				}
			}
		}
	}
	return authorization && consumer
}

func analyzeLiveImage(t *testing.T, ctx context.Context, planner *Service, owner, projectID, image string) coreexecution.ProjectAnalysis {
	t.Helper()
	analysis, err := planner.Analyze(ctx, owner, agentembedded.ExecutionV2AnalyzeRequest{
		ProjectID: projectID, Source: agentembedded.ExecutionV2SourceInput{Kind: "oci_image", Location: image, Immutable: true},
		IdempotencyKey: uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(analysis.BlockingUncertainties) != 0 {
		t.Fatalf("image analysis blocked: %v", analysis.BlockingUncertainties)
	}
	return analysis
}

func driveLiveRun(t *testing.T, ctx context.Context, owner string, plan coreexecution.ExecutionPlan, store *storage.DatabaseExecutionStore, coordinator *storage.DatabaseExecutionCoordinator, runner *executionrunner.Runner) storage.ExecutionRunView {
	t.Helper()
	materialized, err := coordinator.CreateRun(ctx, coreexecution.CreateRunCommand{
		OwnerID: owner, PlanID: plan.ID, PlanRevision: plan.Revision,
		Operation: coreexecution.RunOperationDeploy, IdempotencyKey: uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for iteration := 0; iteration < 30; iteration++ {
		view, readErr := store.GetExecutionRun(ctx, owner, materialized.Run.RunID)
		if readErr != nil {
			t.Fatal(readErr)
		}
		switch view.Run.Status {
		case coreexecution.RunSucceeded:
			return view
		case coreexecution.RunFailed, coreexecution.RunUncertain, coreexecution.RunCanceled,
			coreexecution.RunRejected, coreexecution.RunExpired:
			t.Fatalf("run %s terminal status=%s", view.Run.RunID, view.Run.Status)
		}
		pending, listErr := store.ListV2Confirmations(ctx, owner, "", []coreconfirmation.State{coreconfirmation.StatePending}, 100)
		if listErr != nil {
			t.Fatal(listErr)
		}
		confirmed := false
		for _, record := range pending.Items {
			if record.Confirmation.Binding.RunID != view.Run.RunID {
				continue
			}
			if _, confirmErr := coordinator.ConfirmStage(ctx, coreexecution.ConfirmStageCommand{
				OwnerID: owner, ConfirmationID: record.Confirmation.ID,
				ExpectedRevision: record.Confirmation.Revision, IdempotencyKey: uuid.NewString(),
			}); confirmErr != nil {
				t.Fatal(confirmErr)
			}
			confirmed = true
			break
		}
		if confirmed {
			continue
		}
		if runErr := runner.RunOnce(ctx); runErr != nil {
			if errors.Is(runErr, coreexecution.ErrNotFound) {
				t.Fatalf("run %s has no claimable stage", view.Run.RunID)
			}
			t.Fatalf("run %s current_stage=%s runner=%v", view.Run.RunID, view.Run.CurrentStage, runErr)
		}
	}
	t.Fatalf("run %s did not finish", materialized.Run.RunID)
	return storage.ExecutionRunView{}
}

func liveImageList(raw string) []string {
	var out []string
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

func readLiveAWSCredential(t *testing.T, path string) (string, string, string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		t.Fatal("live AWS credential file must be a private regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal("cannot open live AWS credential file")
	}
	defer f.Close()
	records, err := csv.NewReader(f).ReadAll()
	if err != nil || len(records) != 2 || len(records[0]) != len(records[1]) {
		t.Fatal("live AWS credential CSV has an invalid shape")
	}
	values := map[string]string{}
	for i, header := range records[0] {
		header = strings.TrimPrefix(strings.TrimSpace(header), "\ufeff")
		values[strings.ToLower(header)] = strings.TrimSpace(records[1][i])
	}
	access := firstLiveValue(values, "access key id", "access_key_id", "aws_access_key_id")
	secret := firstLiveValue(values, "secret access key", "secret_access_key", "aws_secret_access_key")
	session := firstLiveValue(values, "session token", "session_token", "aws_session_token")
	if access == "" || secret == "" {
		t.Fatal("live AWS credential CSV is missing required columns")
	}
	return access, secret, session
}

func readLiveSecretValue(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < 1 || info.Size() > 4096 {
		t.Fatal("live provider key file must be a bounded private regular file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("cannot read live provider key file")
	}
	defer clear(raw)
	value := strings.TrimSpace(string(raw))
	if value == "" || len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n") {
		t.Fatal("live provider key file has an invalid value")
	}
	return value
}

func firstLiveValue(values map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := values[key]; value != "" {
			return value
		}
	}
	return ""
}

func zeroStrings(values ...*string) {
	for _, value := range values {
		if value != nil {
			*value = strings.Repeat("\x00", len(*value))
			*value = ""
		}
	}
}

func cleanupLiveSecretParameters(t *testing.T, db *sql.DB, client *ssm.Client, owner, secretRef string) {
	t.Helper()
	if db == nil || client == nil || strings.TrimSpace(owner) == "" || !coreexecution.ValidateUUID(secretRef) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rows, err := db.QueryContext(ctx, `SELECT parameter_name FROM core_execution_secret_parameter_intents WHERE owner_id=$1 AND secret_ref=$2 ORDER BY parameter_name`, owner, secretRef)
	if err != nil {
		t.Errorf("cannot list exact live parameter cleanup handles: %v", err)
		return
	}
	var names []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			t.Errorf("cannot read exact live parameter cleanup handle: %v", err)
			return
		}
		if strings.HasPrefix(name, "/dirextalk/execution-v2/") {
			names = append(names, name)
		}
	}
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Errorf("cannot finish exact live parameter cleanup query: %v", err)
		return
	}
	for _, name := range names {
		_, deleteErr := client.DeleteParameter(ctx, &ssm.DeleteParameterInput{Name: awsapi.String(name)})
		if deleteErr != nil && !isLiveParameterNotFound(deleteErr) {
			t.Errorf("cannot remove exact live parameter handle %s: %v", name, deleteErr)
		}
	}
}

func isLiveParameterNotFound(err error) bool {
	if err == nil {
		return false
	}
	var missing *ssmtypes.ParameterNotFound
	return errors.As(err, &missing)
}

func cleanupLiveStack(t *testing.T, client *cloudformation.Client, stackName string) {
	t.Helper()
	if client == nil || stackName == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	if _, err := client.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: awsapi.String(stackName)}); err != nil {
		return
	}
	if _, err := client.DeleteStack(ctx, &cloudformation.DeleteStackInput{StackName: awsapi.String(stackName)}); err != nil {
		t.Errorf("cleanup stack %s: delete request failed", stackName)
		return
	}
	waiter := cloudformation.NewStackDeleteCompleteWaiter(client)
	if err := waiter.Wait(ctx, &cloudformation.DescribeStacksInput{StackName: awsapi.String(stackName)}, 10*time.Minute); err != nil {
		t.Errorf("cleanup stack %s: delete did not complete", stackName)
		return
	}
	t.Logf("cleaned stack=%s", stackName)
}
