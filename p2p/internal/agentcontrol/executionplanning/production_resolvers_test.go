package executionplanning

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/artifactstore"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
)

const (
	resolverOwner     = "@resolver-owner:example.org"
	resolverProjectID = "11111111-1111-4111-8111-111111111111"
	resolverTargetID  = "22222222-2222-4222-8222-222222222222"
	resolverArtifact  = "33333333-3333-4333-8333-333333333333"
)

type targetReaderStub struct {
	target   coreexecution.ExecutionTarget
	owner    string
	targetID string
	revision uint64
}

func (s *targetReaderStub) GetTarget(
	_ context.Context,
	owner string,
	targetID string,
	revision uint64,
) (coreexecution.ExecutionTarget, error) {
	s.owner, s.targetID, s.revision = owner, targetID, revision
	return s.target, nil
}

func TestDatabaseTargetResolverPinsExactOwnerAndRevision(t *testing.T) {
	target, err := (coreexecution.ExecutionTarget{
		ID: resolverTargetID, Provider: "aws", Kind: "aws_ec2_instance",
		AccountID: "123456789012", Region: "us-east-1", Revision: 7,
	}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	reader := &targetReaderStub{target: target}
	resolver := &DatabaseTargetResolver{store: reader}

	got, err := resolver.ResolveTarget(context.Background(), resolverOwner, resolverTargetID, 7)
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest != target.Digest || reader.owner != resolverOwner || reader.targetID != resolverTargetID || reader.revision != 7 {
		t.Fatalf("target was not resolved by exact identity: got=%+v lookup=%s/%s/%d", got, reader.owner, reader.targetID, reader.revision)
	}
	if _, err = resolver.ResolveTarget(context.Background(), resolverOwner, resolverTargetID, 0); !errors.Is(err, coreexecution.ErrInvalid) {
		t.Fatalf("latest target lookup was not rejected: %v", err)
	}
}

func TestDatabaseTargetResolverRejectsCatalogIdentityDrift(t *testing.T) {
	target, err := (coreexecution.ExecutionTarget{
		ID: resolverTargetID, Provider: "aws", Kind: "aws_ec2_instance",
		AccountID: "123456789012", Region: "us-east-1", Revision: 8,
	}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	resolver := &DatabaseTargetResolver{store: &targetReaderStub{target: target}}
	if _, err = resolver.ResolveTarget(context.Background(), resolverOwner, resolverTargetID, 7); !errors.Is(err, ErrUncertain) {
		t.Fatalf("catalog identity drift was accepted: %v", err)
	}
}

type artifactMetadataStub struct {
	record     artifactstore.SourceArtifact
	owner      string
	projectID  string
	artifactID string
}

func (s *artifactMetadataStub) RegisterSourceArtifact(
	context.Context,
	artifactstore.SourceArtifactRegistration,
) (artifactstore.SourceArtifact, error) {
	return artifactstore.SourceArtifact{}, errors.New("unexpected source registration")
}

func (s *artifactMetadataStub) GetSourceArtifact(
	_ context.Context,
	owner string,
	projectID string,
	artifactID string,
) (artifactstore.SourceArtifact, error) {
	s.owner, s.projectID, s.artifactID = owner, projectID, artifactID
	if s.record.OwnerID != owner || s.record.ProjectID != projectID || s.record.ArtifactID != artifactID {
		return artifactstore.SourceArtifact{}, coreexecution.ErrNotFound
	}
	return s.record, nil
}

func TestProductionSourceResolverVerifiesUploadedArtifactAgainstCAS(t *testing.T) {
	content, err := artifactstore.New(t.TempDir(), artifactstore.MaxArtifactSize)
	if err != nil {
		t.Fatal(err)
	}
	payload := makeZIPArchive(t, map[string]string{
		"repo/package.json": `{"scripts":{"start":"node server.js"},"dependencies":{"fastify":"4.28.1"}}`,
	})
	stored, err := content.Put(context.Background(), bytes.NewReader(payload), artifactstore.PutOptions{MaxSize: int64(len(payload))})
	if err != nil {
		t.Fatal(err)
	}
	metadata := &artifactMetadataStub{record: artifactstore.SourceArtifact{
		OwnerID: resolverOwner, ArtifactID: resolverArtifact, ProjectID: resolverProjectID,
		ContentDigest: stored.Digest, StorageBackend: "filesystem", SchemaVersion: "execution-source-artifact/v2",
		StorageRef: stored.StorageRef, SizeBytes: stored.Size, MediaType: "application/zip", Revision: 1, Status: "available",
		CreatedAt: time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC),
	}}
	resolver := &ProductionSourceResolver{archives: NewStaticSourceArchiveAnalyzer(artifactstore.NewSourceCatalog(content, metadata))}

	facts, err := resolver.ResolveSource(context.Background(), resolverOwner, resolverProjectID, SourceInput{
		Kind: "uploaded_artifact", ArtifactID: resolverArtifact, Immutable: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if metadata.owner != resolverOwner || metadata.projectID != resolverProjectID || metadata.artifactID != resolverArtifact {
		t.Fatalf("metadata was not owner/project scoped: %s/%s/%s", metadata.owner, metadata.projectID, metadata.artifactID)
	}
	if facts.Analysis.Source.ArtifactDigest != coreexecution.Digest(stored.Digest) ||
		facts.Analysis.Source.ArtifactID != resolverArtifact || len(facts.BlockingUncertainties) != 0 ||
		!containsString(facts.Analysis.DetectedStacks, "node") {
		t.Fatalf("unexpected uploaded source facts: %+v", facts)
	}
}

func TestProductionSourceResolverRejectsUploadedArtifactScopeAndIntegrityDrift(t *testing.T) {
	content, err := artifactstore.New(t.TempDir(), artifactstore.MaxArtifactSize)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := content.Put(context.Background(), bytes.NewBufferString("payload"), artifactstore.PutOptions{MaxSize: 7})
	if err != nil {
		t.Fatal(err)
	}
	record := artifactstore.SourceArtifact{
		OwnerID: resolverOwner, ArtifactID: resolverArtifact,
		ProjectID:     "44444444-4444-4444-8444-444444444444",
		ContentDigest: stored.Digest, StorageBackend: "filesystem", SchemaVersion: "execution-source-artifact/v2",
		StorageRef: stored.StorageRef, SizeBytes: stored.Size, MediaType: "application/zip", Revision: 1, Status: "available",
		CreatedAt: time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	metadata := &artifactMetadataStub{record: record}
	resolver := &ProductionSourceResolver{archives: NewStaticSourceArchiveAnalyzer(artifactstore.NewSourceCatalog(content, metadata))}
	input := SourceInput{Kind: "uploaded_artifact", ArtifactID: resolverArtifact, Immutable: true}
	if _, err = resolver.ResolveSource(context.Background(), resolverOwner, resolverProjectID, input); !errors.Is(err, coreexecution.ErrNotFound) {
		t.Fatalf("cross-project artifact was accepted: %v", err)
	}

	record.ProjectID = resolverProjectID
	record.SizeBytes++
	metadata = &artifactMetadataStub{record: record}
	resolver.archives = NewStaticSourceArchiveAnalyzer(artifactstore.NewSourceCatalog(content, metadata))
	if _, err = resolver.ResolveSource(context.Background(), resolverOwner, resolverProjectID, input); !errors.Is(err, ErrSourceIntegrity) {
		t.Fatalf("artifact metadata/CAS drift was accepted: %v", err)
	}

	input.ArtifactDigest = coreexecution.Digest(stored.Digest)
	if _, err = resolver.ResolveSource(context.Background(), resolverOwner, resolverProjectID, input); !errors.Is(err, ErrSourceInvalid) {
		t.Fatalf("client-supplied artifact digest was accepted: %v", err)
	}
}

func TestProductionSourceResolverReturnsExplicitRemoteInspectionUncertainties(t *testing.T) {
	resolver := &ProductionSourceResolver{}
	git, err := resolver.ResolveSource(context.Background(), resolverOwner, resolverProjectID, SourceInput{
		Kind: "git_https", Location: "https://example.org/repository.git",
		Commit: "0123456789abcdef0123456789abcdef01234567", Immutable: true,
	})
	if err != nil || git.Analysis.Source.Kind != "git_https" || len(git.BlockingUncertainties) != 1 {
		t.Fatalf("git metadata result=%+v err=%v", git, err)
	}
	privateGit, err := resolver.ResolveSource(context.Background(), resolverOwner, resolverProjectID, SourceInput{
		Kind: "git_https", Location: "https://example.org/private.git",
		Commit: "0123456789abcdef0123456789abcdef01234567", Immutable: true,
		CredentialRef: "55555555-5555-4555-8555-555555555555", CredentialRevision: 3,
	})
	if err != nil || len(privateGit.BlockingUncertainties) != 1 ||
		!stringsContain(privateGit.BlockingUncertainties[0], "private git") {
		t.Fatalf("private git metadata result=%+v err=%v", privateGit, err)
	}
	oci, err := resolver.ResolveSource(context.Background(), resolverOwner, resolverProjectID, SourceInput{
		Kind: "oci_image", Location: "registry.example/project@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Immutable: true,
	})
	if err != nil || oci.Analysis.Source.Kind != "oci_image" || len(oci.BlockingUncertainties) != 1 {
		t.Fatalf("OCI metadata result=%+v err=%v", oci, err)
	}
	if _, err = resolver.ResolveSource(context.Background(), resolverOwner, resolverProjectID, SourceInput{
		Kind: "git_https", Location: "https://example.org/repository.git?token=secret",
		Commit: "0123456789abcdef0123456789abcdef01234567", Immutable: true,
	}); !errors.Is(err, ErrSourceInvalid) {
		t.Fatalf("query-bearing source URL was accepted: %v", err)
	}
}

type sourceOCIAnalyzerStub struct {
	location string
	facts    SourceFacts
}

func (s *sourceOCIAnalyzerStub) AnalyzePinnedImage(_ context.Context, location string) (SourceFacts, error) {
	s.location = location
	return s.facts, nil
}

func TestProductionSourceResolverRoutesPinnedOCIThroughTrustedAnalyzer(t *testing.T) {
	location := "registry.example/project@sha256:" + strings.Repeat("a", 64)
	stub := &sourceOCIAnalyzerStub{facts: SourceFacts{Analysis: coreexecution.ProjectAnalysis{
		Source:         coreexecution.SourceRef{Kind: "oci_image", Location: location, Immutable: true},
		DetectedStacks: []string{"oci_image"},
	}}}
	resolver := &ProductionSourceResolver{oci: stub}
	facts, err := resolver.ResolveSource(context.Background(), resolverOwner, resolverProjectID, SourceInput{
		Kind: "oci_image", Location: location, Immutable: true,
	})
	if err != nil || stub.location != location || facts.Analysis.Source.Location != location {
		t.Fatalf("trusted OCI analyzer was not used: facts=%+v location=%q err=%v", facts, stub.location, err)
	}
}

func stringsContain(value, fragment string) bool {
	return len(value) >= len(fragment) && bytes.Contains([]byte(value), []byte(fragment))
}
