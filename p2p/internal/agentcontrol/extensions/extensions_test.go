package extensions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func d() string { h := sha256.Sum256([]byte("x")); return hex.EncodeToString(h[:]) }
func candidate() Candidate {
	return Candidate{ID: "c", Kind: KindMCP, Source: "github", Name: "remote", Transport: TransportRemote, Pin: SourcePin{GitCommit: "0123456789012345678901234567890123456789", GitSHA256: d()}}
}
func inspection() Inspection {
	c := candidate()
	return Inspection{Candidate: c, ContentDigest: d(), ManifestDigest: d(), ExecutionDigest: d(), NetworkDigest: d(), SecretDigest: d(), Execution: Execution{Remote: &Endpoint{URL: "https://mcp.example/mcp", CredentialReferenceID: "00000000-0000-0000-0000-000000000001"}}, NetworkGrants: []NetworkGrant{{Scheme: "https", Host: "mcp.example", Port: 443, PathPrefix: "/mcp", Digest: d()}}, SecretGrants: []SecretGrant{{ReferenceID: "00000000-0000-0000-0000-000000000001", Purpose: "mcp_credential", BindingDigest: d(), Configured: true}}}
}

type fakeConfirm struct {
	n       int
	binding ConfirmationBinding
}

func (f *fakeConfirm) Request(_ context.Context, r ConfirmationRequest) (Confirmation, error) {
	f.n++
	f.binding = r.Binding
	return Confirmation{ID: "00000000-0000-0000-0000-000000000002", TaskID: r.TaskID, Revision: 1, State: "pending", Binding: r.Binding}, nil
}
func (f *fakeConfirm) Consume(context.Context, ConsumeRequest) (Confirmation, error) {
	return Confirmation{}, nil
}

type fakeTask struct{ n int }

func (f *fakeTask) CreateWaitingUser(context.Context, TaskRequest) (Task, error) {
	f.n++
	return Task{ID: "00000000-0000-0000-0000-000000000003", Status: "waiting_user", Revision: 1}, nil
}
func (f *fakeTask) Run(context.Context, Task, Confirmation) error     { return nil }
func (f *fakeTask) MarkUncertain(context.Context, Task, string) error { return nil }

type fakeSecretStager struct{}

func (fakeSecretStager) Stage(context.Context, string, string, int64, string, []SecretInput) error {
	return nil
}

func TestRemoteInspectionRequiresExactGrantAndRejectsStdio(t *testing.T) {
	i := inspection()
	if err := i.Validate(); err != nil {
		t.Fatal(err)
	}
	i.NetworkGrants[0].PathPrefix = "/other"
	if err := i.Validate(); err == nil {
		t.Fatal("expected grant mismatch")
	}
	c := candidate()
	c.Transport = "stdio"
	if err := c.Validate(); err == nil {
		t.Fatal("stdio candidate must be rejected")
	}
	if got := (SkillService{}).Capability(context.Background()); got != ErrLocalExecutionDisabled {
		t.Fatalf("skill capability: %v", got)
	}
}
func TestLifecycleIsOwnerScopedAndReplays(t *testing.T) {
	st := NewMemoryStore()
	cf := &fakeConfirm{}
	tk := &fakeTask{}
	s := &Service{Store: st, Confirmations: cf, Tasks: tk, SecretWriter: fakeSecretStager{}, Now: func() time.Time { return time.Unix(1, 0) }}
	m := Mutation{
		OwnerID:        "owner",
		IdempotencyKey: "00000000-0000-0000-0000-000000000010",
		Candidate:      candidate(),
		Inspection:     inspection(),
		SecretInputs: []SecretInput{{
			ReferenceID: "00000000-0000-0000-0000-000000000001",
			Purpose:     "mcp_credential",
			Value:       "secret",
		}},
		Content: []byte{},
	}
	one, e := s.Install(context.Background(), m)
	if e != nil {
		t.Fatal(e)
	}
	two, e := s.Install(context.Background(), m)
	if e != nil || one.TaskID != two.TaskID {
		t.Fatalf("replay: %#v %v", two, e)
	}
	if tk.n != 1 || cf.n != 1 {
		t.Fatalf("side effects repeated")
	}
	m.OwnerID = "other"
	if _, e = s.Install(context.Background(), m); e != nil {
		t.Fatalf("other owner should have independent replay namespace: %v", e)
	}
}

func TestSecretInputsRequireExactGrantFingerprint(t *testing.T) {
	i := inspection()
	m := Mutation{Candidate: candidate(), Inspection: i, SecretInputs: []SecretInput{{ReferenceID: "00000000-0000-0000-0000-000000000001", Purpose: "mcp_credential", Value: "secret"}}}
	i.SecretGrants[0].BindingDigest = DigestBytes([]byte("secret"))
	m.Inspection = i
	if err := validateSecretInputs(m); err != nil {
		t.Fatalf("matching secret grant rejected: %v", err)
	}
	m.SecretInputs[0].Value = "tampered"
	if err := validateSecretInputs(m); err == nil {
		t.Fatal("tampered secret input accepted")
	}
}

func TestInspectionAllowsUnconfiguredDescriptorAndLifecycleBindsSecret(t *testing.T) {
	i := inspection()
	i.SecretGrants[0].Configured = false
	i.SecretGrants[0].BindingDigest = DigestBytes([]byte("credential:" + i.SecretGrants[0].ReferenceID))
	if err := i.Validate(); err != nil {
		t.Fatalf("catalog inspection descriptor rejected: %v", err)
	}
	m := Mutation{
		Inspection: i,
		SecretInputs: []SecretInput{{
			ReferenceID: i.SecretGrants[0].ReferenceID,
			Purpose:     "mcp_credential",
			Value:       "secret",
		}},
	}
	if err := bindSecretInputs(&m); err != nil {
		t.Fatalf("secret binding rejected: %v", err)
	}
	grant := m.Inspection.SecretGrants[0]
	if !grant.Configured || grant.BindingDigest != DigestBytes([]byte("secret")) {
		t.Fatalf("secret grant was not bound: %#v", grant)
	}
}

func TestExecutionBindingUsesCanonicalInputAndPinnedToolSchema(t *testing.T) {
	i := inspection()
	versionID := "11111111-1111-4111-8111-111111111111"
	installationID := "22222222-2222-4222-8222-222222222222"
	schema := json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}}}`)
	schemaDigest := DigestBytes(schema)
	version := Version{VersionID: versionID, Pin: i.Candidate.Pin, ContentDigest: i.ContentDigest, ManifestDigest: i.ManifestDigest, ExecutionDigest: i.ExecutionDigest, NetworkDigest: i.NetworkDigest, SecretDigest: i.SecretDigest, SecretGrants: []SecretGrant{{ReferenceID: "z-ref", Purpose: "mcp_credential", BindingDigest: strings.Repeat("z", 64), Configured: true}, {ReferenceID: "a-ref", Purpose: "mcp_credential", BindingDigest: strings.Repeat("a", 64), Configured: true}}, Tools: []Tool{{Name: "echo", InputSchema: schema, InputSchemaDigest: schemaDigest}}}
	installation := Installation{ID: installationID, OwnerID: "owner", Candidate: i.Candidate, Revision: 3, State: "installed", ActiveVersionID: versionID, Versions: []Version{version}}
	first, err := ExecutionBinding("owner", installation, version, "echo", []byte(`{"b":2,"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(first.SecretGrants) != 2 || first.SecretGrants[0].ReferenceID != "a-ref" || first.SecretGrants[1].ReferenceID != "z-ref" {
		t.Fatalf("secret grants were not canonicalized: %#v", first.SecretGrants)
	}
	if first.ParameterDigest != DigestBytes([]byte(`{"a":1,"b":2}`)) || first.ToolSchemaDigest != schemaDigest {
		t.Fatalf("binding digests = %#v", first)
	}
	second, err := ExecutionBinding("owner", installation, version, "echo", []byte(`{"a":2,"b":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if first.ParameterDigest == second.ParameterDigest {
		t.Fatal("different parameters shared a confirmation digest")
	}
	version.Tools[0].InputSchema = json.RawMessage(`{"type":"object"}`)
	version.Tools[0].InputSchemaDigest = DigestBytes(version.Tools[0].InputSchema)
	third, err := ExecutionBinding("owner", installation, version, "echo", []byte(`{"b":2,"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if first.ToolSchemaDigest == third.ToolSchemaDigest || first.Equal(third) {
		t.Fatal("different tool schema shared a confirmation binding")
	}
	if _, err := ExecutionBinding("owner", installation, version, "echo", []byte("not-json")); err != ErrInvalid {
		t.Fatalf("invalid input error = %v, want ErrInvalid", err)
	}
	if !strings.HasPrefix(first.Operation, "execute") {
		t.Fatalf("operation = %q", first.Operation)
	}
}
