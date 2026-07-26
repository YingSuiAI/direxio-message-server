package agentcore

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcorev1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// The handler tests intentionally use the same authenticated TLS transport as
// production. This keeps the test at the ProductCore boundary without adding
// a production-only connection seam.
type extensionProbeService struct {
	agentv1.UnimplementedAgentServiceServer
}

func (extensionProbeService) GetInstanceInfo(context.Context, *agentv1.GetInstanceInfoRequest) (*agentv1.GetInstanceInfoResponse, error) {
	return &agentv1.GetInstanceInfoResponse{InstanceId: "00000000-0000-4000-8000-000000000001", ApiVersion: "v1"}, nil
}

func (extensionProbeService) GetCapabilities(context.Context, *agentv1.GetCapabilitiesRequest) (*agentv1.GetCapabilitiesResponse, error) {
	return &agentv1.GetCapabilitiesResponse{ApiVersion: "v1", Capabilities: []*agentv1.AgentCapability{
		{Name: "agent.info", Enabled: true}, {Name: "model.profile", Enabled: true}, {Name: "conversation", Enabled: true},
		{Name: "mcp", Enabled: true}, {Name: "skill", Enabled: true},
	}}, nil
}

type extensionMCPService struct {
	agentv1.UnimplementedMCPServiceServer
	mu         sync.Mutex
	inspection *agentv1.CoreExtensionInspection
	inspectReq *agentv1.MCPServiceInspectRequest
	installReq *agentv1.MCPServiceRequestInstallRequest
	updateReq  *agentv1.MCPServiceRequestUpdateRequest
}

func (s *extensionMCPService) Inspect(_ context.Context, req *agentv1.MCPServiceInspectRequest) (*agentv1.MCPServiceInspectResponse, error) {
	s.mu.Lock()
	s.inspectReq = proto.Clone(req).(*agentv1.MCPServiceInspectRequest)
	s.mu.Unlock()
	return &agentv1.MCPServiceInspectResponse{Inspection: proto.Clone(s.inspection).(*agentv1.CoreExtensionInspection)}, nil
}

func (s *extensionMCPService) RequestInstall(_ context.Context, req *agentv1.MCPServiceRequestInstallRequest) (*agentv1.MCPServiceRequestInstallResponse, error) {
	s.mu.Lock()
	s.installReq = proto.Clone(req).(*agentv1.MCPServiceRequestInstallRequest)
	s.mu.Unlock()
	return &agentv1.MCPServiceRequestInstallResponse{Installation: &agentv1.CoreInstallation{InstallationId: "mcp-install", Kind: agentv1.CoreExtensionKind_CORE_EXTENSION_KIND_MCP, State: agentv1.CoreExtensionState_CORE_EXTENSION_STATE_INSTALLING}, ConfirmationId: "confirmation-mcp", TaskId: "task-mcp"}, nil
}

func (s *extensionMCPService) RequestUpdate(_ context.Context, req *agentv1.MCPServiceRequestUpdateRequest) (*agentv1.MCPServiceRequestUpdateResponse, error) {
	s.mu.Lock()
	s.updateReq = proto.Clone(req).(*agentv1.MCPServiceRequestUpdateRequest)
	s.mu.Unlock()
	return &agentv1.MCPServiceRequestUpdateResponse{Installation: &agentv1.CoreInstallation{InstallationId: "mcp-install", Kind: agentv1.CoreExtensionKind_CORE_EXTENSION_KIND_MCP, State: agentv1.CoreExtensionState_CORE_EXTENSION_STATE_UPDATING}, ConfirmationId: "confirmation-mcp-update", TaskId: "task-mcp-update"}, nil
}

type extensionSkillService struct {
	agentv1.UnimplementedSkillServiceServer
	mu         sync.Mutex
	inspection *agentv1.CoreExtensionInspection
	inspectReq *agentv1.SkillServiceInspectRequest
	installReq *agentv1.SkillServiceRequestInstallRequest
	updateReq  *agentv1.SkillServiceRequestUpdateRequest
}

func (s *extensionSkillService) Inspect(_ context.Context, req *agentv1.SkillServiceInspectRequest) (*agentv1.SkillServiceInspectResponse, error) {
	s.mu.Lock()
	s.inspectReq = proto.Clone(req).(*agentv1.SkillServiceInspectRequest)
	s.mu.Unlock()
	return &agentv1.SkillServiceInspectResponse{Inspection: proto.Clone(s.inspection).(*agentv1.CoreExtensionInspection)}, nil
}

func (s *extensionSkillService) RequestInstall(_ context.Context, req *agentv1.SkillServiceRequestInstallRequest) (*agentv1.SkillServiceRequestInstallResponse, error) {
	s.mu.Lock()
	s.installReq = proto.Clone(req).(*agentv1.SkillServiceRequestInstallRequest)
	s.mu.Unlock()
	return &agentv1.SkillServiceRequestInstallResponse{Installation: &agentv1.CoreInstallation{InstallationId: "skill-install", Kind: agentv1.CoreExtensionKind_CORE_EXTENSION_KIND_SKILL, State: agentv1.CoreExtensionState_CORE_EXTENSION_STATE_INSTALLING}, ConfirmationId: "confirmation-skill", TaskId: "task-skill"}, nil
}

func (s *extensionSkillService) RequestUpdate(_ context.Context, req *agentv1.SkillServiceRequestUpdateRequest) (*agentv1.SkillServiceRequestUpdateResponse, error) {
	s.mu.Lock()
	s.updateReq = proto.Clone(req).(*agentv1.SkillServiceRequestUpdateRequest)
	s.mu.Unlock()
	return &agentv1.SkillServiceRequestUpdateResponse{Installation: &agentv1.CoreInstallation{InstallationId: "skill-install", Kind: agentv1.CoreExtensionKind_CORE_EXTENSION_KIND_SKILL, State: agentv1.CoreExtensionState_CORE_EXTENSION_STATE_UPDATING}, ConfirmationId: "confirmation-skill-update", TaskId: "task-skill-update"}, nil
}

func newExtensionHandlerClient(t *testing.T, mcp *extensionMCPService, skill *extensionSkillService) *Client {
	t.Helper()
	caPEM, certPEM, keyPEM := testCertificate(t, "core.example")
	dir := t.TempDir()
	caPath, tokenPath := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "token")
	if err := os.WriteFile(caPath, caPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte(base64.RawURLEncoding.EncodeToString(make([]byte, 32))), 0600); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(server, extensionProbeService{})
	agentv1.RegisterMCPServiceServer(server, mcp)
	agentv1.RegisterSkillServiceServer(server, skill)
	tlsListener := tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, NextProtos: []string{"h2"}})
	go server.Serve(tlsListener)
	t.Cleanup(func() { server.Stop(); _ = ln.Close() })
	cfg := completeConfig()
	cfg.Address, cfg.CAFile, cfg.TokenFile = ln.Addr().String(), caPath, tokenPath
	cfg.UnaryTimeout = 2 * time.Second
	client, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := client.Snapshot().Status; got != StatusReady {
		t.Fatalf("snapshot status = %q, want ready", got)
	}
	return client
}

func extensionCandidateParams(kind string) map[string]any {
	transport := "streamable-http"
	if kind == "skill" {
		transport = "skill-static"
	}
	return map[string]any{
		"id": "candidate-" + kind, "kind": kind, "source": "github", "name": "Example " + strings.ToUpper(kind),
		"description": "immutable " + kind + " candidate", "transport": transport,
		"pin": map[string]any{"registry_version": "", "registry_sha256": "", "git_commit": strings.Repeat("a", 40), "git_sha256": strings.Repeat("b", 64)},
	}
}

func extensionInspectionParams(candidate map[string]any, kind string) map[string]any {
	execution := map[string]any{"remote": map[string]any{"url": "https://extensions.example.test/mcp", "credential_reference_id": "mcp-credential"}}
	if kind == "skill" {
		execution = map[string]any{"skill": map[string]any{"relative_path": "skills/run.sh", "digest": strings.Repeat("f", 64), "executable": true, "argv": []any{"--safe", "--mode=run"}}}
	}
	purpose := "mcp-credential"
	if kind == "skill" {
		purpose = "skill-secret"
	}
	return map[string]any{
		"candidate":      candidate,
		"content_digest": strings.Repeat("1", 64), "manifest_digest": strings.Repeat("2", 64), "execution_digest": strings.Repeat("3", 64),
		"network_schema_digest": strings.Repeat("4", 64), "secret_schema_digest": strings.Repeat("5", 64),
		"execution":      execution,
		"network_grants": []any{map[string]any{"scheme": "https", "host": "extensions.example.test", "port": int64(443), "path_prefix": "/mcp", "digest": strings.Repeat("6", 64)}},
		"secret_grants":  []any{map[string]any{"reference_id": "secret-ref-" + kind, "purpose": purpose, "binding_digest": strings.Repeat("7", 64), "configured": false}},
	}
}

func extensionInspectionProto(candidate *agentv1.CoreExtensionCandidate, kind string) *agentv1.CoreExtensionInspection {
	execution := &agentv1.CoreExecution{Descriptor_: &agentv1.CoreExecution_Remote{Remote: &agentv1.CoreRemoteEndpoint{Url: "https://extensions.example.test/mcp", CredentialReferenceId: "mcp-credential"}}}
	if kind == "skill" {
		execution = &agentv1.CoreExecution{Descriptor_: &agentv1.CoreExecution_Skill{Skill: &agentv1.CoreSkillEntry{RelativePath: "skills/run.sh", Digest: strings.Repeat("f", 64), Executable: true, Argv: []string{"--safe", "--mode=run"}}}}
	}
	purpose := agentv1.CoreSecretPurpose_CORE_SECRET_PURPOSE_MCP_CREDENTIAL
	if kind == "skill" {
		purpose = agentv1.CoreSecretPurpose_CORE_SECRET_PURPOSE_SKILL_SECRET
	}
	return &agentv1.CoreExtensionInspection{Candidate: candidate, ContentDigest: strings.Repeat("1", 64), ManifestDigest: strings.Repeat("2", 64), ExecutionDigest: strings.Repeat("3", 64), NetworkSchemaDigest: strings.Repeat("4", 64), SecretSchemaDigest: strings.Repeat("5", 64), Execution: execution, NetworkGrants: []*agentv1.CoreNetworkGrant{{Scheme: "https", Host: "extensions.example.test", Port: 443, PathPrefix: "/mcp", Digest: strings.Repeat("6", 64)}}, SecretGrants: []*agentv1.CoreExtensionSecretGrantDescriptor{{ReferenceId: "secret-ref-" + kind, Purpose: purpose, BindingDigest: strings.Repeat("7", 64), Configured: false}}}
}

func assertCandidate(t *testing.T, got, want *agentv1.CoreExtensionCandidate) {
	t.Helper()
	if !proto.Equal(got, want) {
		t.Fatalf("candidate mismatch:\n got=%s\nwant=%s", got, want)
	}
}

func TestExtensionHandlersPropagateInspectionAndImmutableCandidate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		kind  string
		enum  agentv1.CoreExtensionKind
		setup func(*extensionMCPService, *extensionSkillService, *agentv1.CoreExtensionInspection)
	}{
		{name: "mcp", kind: "mcp", enum: agentv1.CoreExtensionKind_CORE_EXTENSION_KIND_MCP, setup: func(m *extensionMCPService, _ *extensionSkillService, i *agentv1.CoreExtensionInspection) {
			m.inspection = i
		}},
		{name: "skill", kind: "skill", enum: agentv1.CoreExtensionKind_CORE_EXTENSION_KIND_SKILL, setup: func(_ *extensionMCPService, s *extensionSkillService, i *agentv1.CoreExtensionInspection) {
			s.inspection = i
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidateParams := extensionCandidateParams(tc.kind)
			candidate, err := candidateFromParams(map[string]any{"candidate": candidateParams}, "candidate", tc.enum)
			if err != nil {
				t.Fatal(err)
			}
			inspection := extensionInspectionProto(candidate, tc.kind)
			mcp, skill := &extensionMCPService{}, &extensionSkillService{}
			tc.setup(mcp, skill, inspection)
			client := newExtensionHandlerClient(t, mcp, skill)
			handlers := client.Handlers()
			prefix := tc.kind
			if tc.kind == "skill" {
				prefix = "skills"
			}

			inspectResult, inspectErr := handlers["agent.core."+prefix+".inspect"](context.Background(), map[string]any{"candidate": candidateParams})
			if inspectErr != nil {
				t.Fatalf("inspect error: %#v", inspectErr)
			}
			if inspectResult == nil {
				t.Fatal("inspect result is nil")
			}
			inspectionFromInspect, ok := inspectResult.(map[string]any)["inspection"].(map[string]any)
			if !ok {
				t.Fatal("inspect result omitted inspection object")
			}
			projectedCandidate, parseErr := candidateFromParams(map[string]any{"candidate": inspectionFromInspect["candidate"]}, "candidate", tc.enum)
			if parseErr != nil {
				t.Fatalf("inspect candidate projection invalid: %#v", parseErr)
			}
			assertCandidate(t, projectedCandidate, candidate)
			for _, field := range []string{"content_digest", "manifest_digest", "execution_digest", "network_schema_digest", "secret_schema_digest", "execution", "network_grants", "secret_grants"} {
				if _, exists := inspectionFromInspect[field]; !exists {
					t.Fatalf("inspect result omitted %q", field)
				}
			}

			installParams := map[string]any{"idempotency_key": "11111111-1111-4111-8111-111111111111", "candidate": candidateParams, "inspection": inspectionFromInspect, "secret_inputs": []any{map[string]any{"reference_id": "secret-ref-" + tc.kind, "purpose": func() string {
				if tc.kind == "skill" {
					return "skill-secret"
				}
				return "mcp-credential"
			}(), "secret_value": "write-only-secret"}}}
			if _, actionErr := handlers["agent.core."+prefix+".install"](context.Background(), installParams); actionErr != nil {
				t.Fatalf("install error: %#v", actionErr)
			}
			updateParams := map[string]any{"idempotency_key": "22222222-2222-4222-8222-222222222222", "installation_id": "installation-" + tc.kind, "expected_revision": int64(4), "candidate": candidateParams, "inspection": inspectionFromInspect}
			if _, actionErr := handlers["agent.core."+prefix+".update"](context.Background(), updateParams); actionErr != nil {
				t.Fatalf("update error: %#v", actionErr)
			}

			if tc.kind == "mcp" {
				mcp.mu.Lock()
				inspectReq, installReq, updateReq := mcp.inspectReq, mcp.installReq, mcp.updateReq
				mcp.mu.Unlock()
				if inspectReq == nil || installReq == nil || updateReq == nil {
					t.Fatalf("mcp requests missing: inspect=%v install=%v update=%v", inspectReq != nil, installReq != nil, updateReq != nil)
				}
				if inspectReq.Id != candidate.GetId() || inspectReq.Source != candidate.GetSource() || !proto.Equal(inspectReq.Pin, candidate.GetPin()) {
					t.Fatalf("mcp inspect request = %s", inspectReq)
				}
				assertCandidate(t, installReq.Candidate, candidate)
				assertCandidate(t, updateReq.GetMutation().GetCandidate(), candidate)
				if !proto.Equal(installReq.Inspection, inspection) || !proto.Equal(updateReq.GetMutation().GetInspection(), inspection) {
					t.Fatalf("mcp inspection propagation mismatch")
				}
				if !proto.Equal(installReq.SecretInputs[0], &agentv1.CoreExtensionSecretInput{ReferenceId: "secret-ref-mcp", Purpose: agentv1.CoreSecretPurpose_CORE_SECRET_PURPOSE_MCP_CREDENTIAL, SecretValue: "write-only-secret"}) {
					t.Fatal("mcp secret input propagation mismatch")
				}
			} else {
				skill.mu.Lock()
				inspectReq, installReq, updateReq := skill.inspectReq, skill.installReq, skill.updateReq
				skill.mu.Unlock()
				if inspectReq == nil || installReq == nil || updateReq == nil {
					t.Fatalf("skill requests missing: inspect=%v install=%v update=%v", inspectReq != nil, installReq != nil, updateReq != nil)
				}
				if inspectReq.Id != candidate.GetId() || inspectReq.Source != candidate.GetSource() || !proto.Equal(inspectReq.Pin, candidate.GetPin()) {
					t.Fatalf("skill inspect request = %s", inspectReq)
				}
				assertCandidate(t, installReq.Candidate, candidate)
				assertCandidate(t, updateReq.GetMutation().GetCandidate(), candidate)
				if !proto.Equal(installReq.Inspection, inspection) || !proto.Equal(updateReq.GetMutation().GetInspection(), inspection) {
					t.Fatalf("skill inspection propagation mismatch")
				}
				if !proto.Equal(installReq.SecretInputs[0], &agentv1.CoreExtensionSecretInput{ReferenceId: "secret-ref-skill", Purpose: agentv1.CoreSecretPurpose_CORE_SECRET_PURPOSE_SKILL_SECRET, SecretValue: "write-only-secret"}) {
					t.Fatal("skill secret input propagation mismatch")
				}
			}
		})
	}
}

func TestInspectionParserRejectsCandidatePinMismatchAndExecutionType(t *testing.T) {
	candidateParams := extensionCandidateParams("mcp")
	candidate, err := candidateFromParams(map[string]any{"candidate": candidateParams}, "candidate", agentv1.CoreExtensionKind_CORE_EXTENSION_KIND_MCP)
	if err != nil {
		t.Fatal(err)
	}
	inspectionParams := extensionInspectionParams(candidateParams, "mcp")
	badPin := inspectionParams["candidate"].(map[string]any)["pin"].(map[string]any)
	badPin["git_commit"] = "different"
	if _, actionErr := inspectionFromParams(map[string]any{"inspection": inspectionParams}, "inspection", candidate.GetKind(), candidate); actionErr == nil {
		t.Fatal("candidate pin mismatch accepted")
	}
	inspectionParams = extensionInspectionParams(candidateParams, "mcp")
	inspectionParams["execution"] = "remote"
	if _, actionErr := inspectionFromParams(map[string]any{"inspection": inspectionParams}, "inspection", candidate.GetKind(), candidate); actionErr == nil {
		t.Fatal("execution type mismatch accepted")
	}
}
