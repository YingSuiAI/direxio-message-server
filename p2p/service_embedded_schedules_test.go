package p2p

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	agentruntime "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/runtime"
	p2pstorage "github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/setup/process"
	"github.com/YingSuiAI/dirextalk-message-server/test"
)

func TestAgentConfirmationSweepRetriesWithBoundedBackoff(t *testing.T) {
	const interval = 20 * time.Millisecond
	var calls atomic.Int32
	var first, second time.Time
	done := make(chan struct{})
	notifications := make(chan bool, 4)
	processCtx := process.NewProcessContext()
	ctx := processCtx.Context()
	go func() {
		runAgentConfirmationSweep(ctx, interval, func() string { return "@owner:example.com" }, func(_ context.Context, owner string, at time.Time) error {
			if owner != "@owner:example.com" {
				t.Errorf("sweep owner = %q", owner)
			}
			switch calls.Add(1) {
			case 1:
				first = at
				return errors.New("temporary database failure")
			case 2:
				second = at
				processCtx.ShutdownDendrite()
			}
			return nil
		}, func(_ error, recovered bool) { notifications <- recovered })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("confirmation sweep did not stop after cancellation")
	}
	if calls.Load() != 2 {
		t.Fatalf("sweep calls = %d, want 2", calls.Load())
	}
	if second.Sub(first) < 2*interval-5*time.Millisecond {
		t.Fatalf("retry interval = %s, want near %s", second.Sub(first), 2*interval)
	}
	if degraded, reasons := processCtx.IsDegraded(); degraded || len(reasons) != 0 {
		t.Fatalf("transient sweep failure degraded process: %v", reasons)
	}
	select {
	case recovered := <-notifications:
		if recovered {
			t.Fatal("first sweep notification was recovery")
		}
	default:
		t.Fatal("sweep failure was not observed")
	}
	select {
	case recovered := <-notifications:
		if !recovered {
			t.Fatal("second sweep notification was not recovery")
		}
	default:
		t.Fatal("sweep recovery was not observed")
	}
	select {
	case recovered := <-notifications:
		t.Fatalf("unexpected extra sweep notification, recovered=%v", recovered)
	default:
	}
}

func TestAgentConfirmationSweepSkipsUnavailableOwner(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	called := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		runAgentConfirmationSweep(ctx, time.Millisecond, func() string { return "" }, func(context.Context, string, time.Time) error {
			called <- struct{}{}
			return nil
		}, nil)
		close(done)
	}()
	time.Sleep(10 * time.Millisecond)
	select {
	case <-called:
		t.Fatal("sweep ran without an owner")
	default:
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("owner-unavailable sweep did not stop")
	}
}

func TestAgentConfirmationSweepCancellationInterruptsInFlightCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	started := make(chan struct{})
	go func() {
		runAgentConfirmationSweep(ctx, time.Millisecond, func() string { return "@owner:example.com" }, func(callCtx context.Context, _ string, _ time.Time) error {
			close(started)
			<-callCtx.Done()
			return callCtx.Err()
		}, nil)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("confirmation sweep did not tick")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("in-flight confirmation sweep did not cancel")
	}
}

func TestConfirmationSweepStartsWhenSchedulerReadinessFails(t *testing.T) {
	processCtx := process.NewProcessContext()
	started := make(chan struct{})
	service := &Service{
		servicePortalState:             servicePortalState{ownerMXID: "@owner:example.com"},
		agentConfirmationSweepInterval: time.Millisecond,
		agentConfirmationSweep: func(ctx context.Context, owner string, _ time.Time) error {
			if owner != "@owner:example.com" {
				t.Errorf("sweep owner = %q", owner)
			}
			close(started)
			<-ctx.Done()
			return ctx.Err()
		},
	}
	if service.StartEmbeddedScheduler(processCtx, "not-ready") {
		t.Fatal("StartEmbeddedScheduler succeeded without scheduler readiness")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("confirmation sweep did not start when scheduler was not ready")
	}
	if service.EmbeddedSchedulesReady() {
		t.Fatal("scheduler readiness became true without runtime dependencies")
	}
	processCtx.ShutdownDendrite()
	finished := make(chan struct{})
	go func() {
		processCtx.WaitForComponentsToFinish()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("confirmation sweep component did not stop on process cancellation")
	}
}

func TestAgentSecretFailureSuppressesSecretDependentCapabilities(t *testing.T) {
	service := &Service{
		scheduleRunning:   true,
		agentTaskRuntime:  &agentruntime.Worker{},
		agentScheduleLoop: &agentruntime.ScheduleLoop{},
		agentTaskExecutor: &embeddedTaskExecutor{},
	}
	for _, capability := range []string{
		"model_profiles.server",
		"task",
		"confirmation",
		"schedules.server",
		"mcp",
		"aws.control",
		"workload.aws_ssm",
		"workload.aws_ecs",
	} {
		if service.embeddedAgentCapabilityReady(capability) {
			t.Fatalf("capability %q published while Agent secrets are unavailable", capability)
		}
	}
}

func TestDeploymentsCapabilityRejectsLegacyOrUnboundStores(t *testing.T) {
	legacy := &Service{
		store:                 p2pstorage.NewMemoryStore(),
		deploymentSourceReady: true, // a stale/incorrect marker must not bypass the DB probe
	}
	if legacy.embeddedAgentCapabilityReady("deployments.server") {
		t.Fatal("deployments.server published for the retained in-memory deployment ledger")
	}
	unbound := &Service{deploymentSourceReady: false}
	if unbound.embeddedAgentCapabilityReady("deployments.server") {
		t.Fatal("deployments.server published without a wired v106 source")
	}
	withoutSchema := &Service{
		store:                 p2pstorage.NewUnmigratedDatabaseStore(nil, nil),
		deploymentSourceReady: true,
	}
	if withoutSchema.embeddedAgentCapabilityReady("deployments.server") {
		t.Fatal("deployments.server published without a live v106 schema")
	}
}

func TestDatabaseServiceMissingAgentKeyringInitializesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	t.Cleanup(closeDB)
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	keyring := filepath.Join(dir, "missing-keyring.json")
	service, err := NewServiceWithStore(ctx, Config{ServerName: "example.com", AgentSecretKeyringFile: keyring}, store)
	if err != nil {
		t.Fatalf("ordinary service startup must survive unavailable Agent secrets: %v", err)
	}
	if !service.agentSecretReady || service.modelProfileInitErr != nil {
		t.Fatalf("Agent secret readiness=%v error=%v", service.agentSecretReady, service.modelProfileInitErr)
	}
	info, err := os.Stat(keyring)
	if err != nil {
		t.Fatalf("server startup did not create keyring: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("keyring mode = %o, want 0600", got)
	}
	dirInfo, dirErr := os.Stat(dir)
	if dirErr != nil {
		t.Fatal(dirErr)
	}
	if got := dirInfo.Mode().Perm(); got != 0700 {
		t.Fatalf("keyring directory mode = %o, want 0700", got)
	}
	if !service.embeddedAgentCapabilityReady("model_profiles.server") {
		t.Fatalf("model_profiles.server unavailable after automatic keyring initialization: ready=%v store=%v err=%v", service.agentSecretReady, service.modelProfiles != nil && service.modelProfiles.ModelProfileStoreReady(), service.modelProfileInitErr)
	}
	if !service.embeddedAgentCapabilityReady("deployments.server") {
		t.Fatal("read-only deployments.server should remain available with a valid schema when Agent secrets are unavailable")
	}
	if !service.embeddedAgentCapabilityReady("memory.server") {
		t.Fatal("in-process PostgreSQL knowledge and memory must remain ready without an Agent keyring")
	}
	backends, apiErr := service.Handle(ctx, "agent.backends.get", nil)
	if apiErr != nil {
		t.Fatalf("agent.backends.get: %v", apiErr)
	}
	embedded, ok := backends.(map[string]any)["embedded"].(map[string]any)
	if !ok {
		t.Fatalf("embedded backend missing from response: %#v", backends)
	}
	capabilities, ok := embedded["capabilities"].([]string)
	if !ok {
		t.Fatalf("embedded capabilities have unexpected type: %#v", embedded["capabilities"])
	}
	memoryReady := false
	for _, capability := range capabilities {
		if capability == "memory.server" {
			memoryReady = true
			break
		}
	}
	if !memoryReady {
		t.Fatalf("single-image PostgreSQL backend did not advertise memory.server: %#v", capabilities)
	}
	if _, apiErr := service.Handle(ctx, "profile.get", nil); apiErr != nil {
		t.Fatalf("ordinary ProductCore remains available: %v", apiErr)
	}
	// A concurrent startup cannot steal the live process's shared guard.
	blocked, err := NewServiceWithStore(ctx, Config{ServerName: "example.com", AgentSecretKeyringFile: keyring}, store)
	if err != nil {
		t.Fatalf("concurrent startup: %v", err)
	}
	if blocked.agentSecretReady {
		t.Fatal("concurrent startup bypassed the live Agent runtime guard")
	}
	service.closeAgentSecretGuard()
	// A restart with the same database/keyring must verify the existing file
	// without replacing it or changing its permissions.
	before, err := os.ReadFile(keyring)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := NewServiceWithStore(ctx, Config{ServerName: "example.com", AgentSecretKeyringFile: keyring}, store)
	if err != nil {
		t.Fatalf("restart with initialized keyring: %v", err)
	}
	if !restarted.agentSecretReady || restarted.modelProfileInitErr != nil {
		t.Fatalf("restart Agent secret readiness=%v error=%v", restarted.agentSecretReady, restarted.modelProfileInitErr)
	}
	after, err := os.ReadFile(keyring)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("restart replaced initialized keyring")
	}
}

func TestDatabaseServiceLostAgentKeyringWithCiphertextSurvivesOrdinaryStartup(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	t.Cleanup(closeDB)
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	keyring := filepath.Join(dir, "lost-keyring.json")
	loaded, err := p2pstorage.InitializeAgentSecretKeyringForDatabase(ctx, store.DB(), keyring)
	if err != nil {
		t.Fatal(err)
	}
	enveloper, err := p2pstorage.NewAgentSecretEnveloper(loaded)
	if err != nil {
		t.Fatal(err)
	}
	binding := p2pstorage.AgentSecretBinding{
		Domain: "test", OwnerID: "owner", EntityID: "entity", Revision: 1,
		Purpose: "purpose", Reference: "reference", BindingDigest: sha256.Sum256([]byte("lost-key")),
	}
	envelope, err := enveloper.Seal(binding, []byte("unrecoverable"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO p2p_agent_secrets(
		secret_domain,owner_id,entity_id,secret_revision,purpose,reference,binding_digest,
		envelope_version,aad_version,key_id,nonce,ciphertext
	) VALUES($1,$2,$3,$4,$5,$6,$7,1,1,$8,$9,$10)`,
		binding.Domain, binding.OwnerID, binding.EntityID, binding.Revision, binding.Purpose, binding.Reference,
		binding.BindingDigest[:], envelope.KeyID, envelope.Nonce, envelope.Ciphertext); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(keyring); err != nil {
		t.Fatal(err)
	}
	service, err := NewServiceWithStore(ctx, Config{ServerName: "example.com", AgentSecretKeyringFile: keyring}, store)
	if err != nil {
		t.Fatalf("ordinary service startup must survive lost Agent keyring: %v", err)
	}
	if service.agentSecretReady || service.modelProfiles != nil {
		t.Fatalf("lost keyring unexpectedly enabled Agent secrets: ready=%v profiles=%v", service.agentSecretReady, service.modelProfiles != nil)
	}
	if !errors.Is(service.modelProfileInitErr, p2pstorage.ErrAgentSecretKeyringUnavailable) {
		t.Fatalf("lost keyring error = %v", service.modelProfileInitErr)
	}
	if _, err := os.Stat(keyring); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lost keyring must not be replaced: %v", err)
	}
	if _, apiErr := service.Handle(ctx, "profile.get", nil); apiErr != nil {
		t.Fatalf("ordinary ProductCore unavailable after lost keyring: %v", apiErr)
	}
}

func TestDatabaseServiceEmbeddedAWSWorkloadReadiness(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	t.Cleanup(closeDB)
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	keyringFile := filepath.Join(t.TempDir(), "secret-keyring.json")
	if _, err := p2pstorage.LoadOrCreateAgentSecretKeyring(keyringFile); err != nil {
		t.Fatalf("initialize Agent secret keyring: %v", err)
	}
	service, err := NewServiceWithStore(ctx, Config{
		ServerName:             "example.com",
		AgentSecretKeyringFile: keyringFile,
	}, store)
	if err != nil {
		t.Fatalf("NewServiceWithStore: %v", err)
	}

	processCtx := process.NewProcessContext()
	if !service.StartEmbeddedScheduler(processCtx, "embedded-readiness-test") {
		t.Fatal("StartEmbeddedScheduler returned false with PostgreSQL and an initialized Agent keyring")
	}
	t.Cleanup(func() {
		processCtx.ShutdownDendrite()
		processCtx.WaitForComponentsToFinish()
	})

	if !service.EmbeddedSchedulesReady() {
		t.Fatal("embedded scheduler is not ready after startup")
	}
	if !service.embeddedAgentCapabilityReady("deployments.server") {
		t.Fatal("deployments.server is not ready with the migrated v106 PostgreSQL source")
	}
	backends, apiErr := service.Handle(ctx, "agent.backends.get", nil)
	if apiErr != nil {
		t.Fatalf("agent.backends.get: %v", apiErr)
	}
	embedded, ok := backends.(map[string]any)["embedded"].(map[string]any)
	if !ok {
		t.Fatalf("embedded backend missing from response: %#v", backends)
	}
	capabilities, ok := embedded["capabilities"].([]string)
	if !ok {
		t.Fatalf("embedded capabilities have unexpected type: %#v", embedded["capabilities"])
	}
	for _, want := range []string{"aws.control", "workload.aws_ssm", "workload.aws_ecs"} {
		found := false
		for _, capability := range capabilities {
			if capability == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("capability %q missing from %#v", want, capabilities)
		}
	}

	credentials, apiErr := service.Handle(ctx, "agent.core.aws.credentials.list", map[string]any{})
	if apiErr != nil {
		t.Fatalf("agent.core.aws.credentials.list: %v", apiErr)
	}
	if got, ok := credentials.(map[string]any)["credentials"].([]any); !ok || len(got) != 0 {
		t.Fatalf("credentials.list = %#v, want an empty result", credentials)
	}
	workloads, apiErr := service.Handle(ctx, "agent.core.workloads.list", map[string]any{})
	if apiErr != nil {
		t.Fatalf("agent.core.workloads.list: %v", apiErr)
	}
	if got, ok := workloads.(map[string]any)["plans"].([]any); !ok || len(got) != 0 {
		t.Fatalf("workloads.list = %#v, want an empty result", workloads)
	}
}
