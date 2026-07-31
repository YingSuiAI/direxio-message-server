package executionplanning

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/agentrecipes"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	agentembedded "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentembedded"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

type sourceStub struct{}

func (sourceStub) ResolveSource(_ context.Context, _ string, _ string, input SourceInput) (SourceFacts, error) {
	blocker := "missing immutable commit"
	return SourceFacts{Analysis: coreexecution.ProjectAnalysis{Source: coreexecution.SourceRef{Kind: input.Kind, Location: input.Location, Commit: input.Commit, ArtifactID: input.ArtifactID, ArtifactDigest: input.ArtifactDigest, Immutable: input.Immutable}}, BlockingUncertainties: []string{blocker}}, nil
}

type targetStub struct{}

func (targetStub) ResolveTarget(context.Context, string, string, uint64) (coreexecution.ExecutionTarget, error) {
	return coreexecution.ExecutionTarget{}, nil
}

type plannerMemoryStore struct{ analysis coreexecution.ProjectAnalysis }

func (s *plannerMemoryStore) CreateAnalysis(_ context.Context, in storage.AnalysisCreateRequest) (coreexecution.ProjectAnalysis, error) {
	now := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	a := in.Analysis
	a.CreatedAt, a.UpdatedAt = now, now
	normalized, err := a.Normalize()
	if err == nil {
		s.analysis = normalized
	}
	return normalized, err
}
func (s *plannerMemoryStore) GetAnalysis(context.Context, string, string) (coreexecution.ProjectAnalysis, error) {
	return s.analysis, nil
}
func (*plannerMemoryStore) CreatePlan(context.Context, storage.ExecutionPlanCreate) (coreexecution.ExecutionPlan, error) {
	return coreexecution.ExecutionPlan{}, nil
}
func (*plannerMemoryStore) GetCurrentPlan(context.Context, string, string) (coreexecution.ExecutionPlan, error) {
	return coreexecution.ExecutionPlan{}, nil
}

func TestPlannerFailsClosedWithoutTrustedAnalyzerDependencies(t *testing.T) {
	s := New(Config{})
	if s.Ready() || s.PlanReady() {
		t.Fatal("planner advertised readiness without stores/resolvers")
	}
	if _, err := s.Analyze(context.Background(), "@owner:example.test", agentembedded.ExecutionV2AnalyzeRequest{}); err != ErrNotReady {
		t.Fatalf("analyze err=%v", err)
	}
}

func TestPlannerPersistsHonestBlockingAnalysisAndRejectsCompile(t *testing.T) {
	r, err := agentrecipes.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	store := &plannerMemoryStore{}
	s := New(Config{AnalysisStore: store, PlanStore: store, Sources: sourceStub{}, Targets: targetStub{}, Recipes: r})
	analysis, err := s.Analyze(context.Background(), "@owner:example.test", agentembedded.ExecutionV2AnalyzeRequest{ProjectID: "11111111-1111-4111-8111-111111111111", IdempotencyKey: "33333333-3333-4333-8333-333333333333", Source: agentembedded.ExecutionV2SourceInput{Kind: "git_https", Location: "https://source.example/repo", Commit: "0123456789abcdef0123456789abcdef01234567", Immutable: true}})
	if err != nil || len(analysis.BlockingUncertainties) != 1 || !analysis.Digest.Valid() || store.analysis.AnalysisID != analysis.AnalysisID {
		t.Fatalf("blocking analysis was not durably represented: %+v, err=%v", analysis, err)
	}
	_, err = s.compileAny(context.Background(), "@owner:example.test", map[string]any{
		"analysis_id": analysis.AnalysisID, "idempotency_key": "44444444-4444-4444-8444-444444444444",
	})
	if !errors.Is(err, ErrUncertain) {
		t.Fatalf("blocking analysis compiled: %v", err)
	}
}

func TestGenericContainerRecipeIsAvailableForTrustedCompiler(t *testing.T) {
	r, err := agentrecipes.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Select(agentrecipes.SelectionQuery{Intent: "deploy", TargetCapabilities: []string{"artifact.collect", "probe.http", "runtime.container", "target.aws_ec2_instance", "transport.aws_ssm"}, Limit: 3})
	if err != nil || len(got) == 0 {
		t.Fatalf("builtin deploy recipes=%d err=%v", len(got), err)
	}
}
