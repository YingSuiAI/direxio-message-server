package confirmation

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestBindingIsZeroCoversEveryField(t *testing.T) {
	if !(Binding{}).IsZero() {
		t.Fatal("empty binding should be zero")
	}
	tests := map[string]func(*Binding){
		"digest":            func(b *Binding) { b.Digest = "digest" },
		"owner":             func(b *Binding) { b.OwnerID = "owner" },
		"domain":            func(b *Binding) { b.OperationDomain = "aws" },
		"target":            func(b *Binding) { b.TargetID = "target" },
		"target revision":   func(b *Binding) { b.TargetRevision = 1 },
		"target kind":       func(b *Binding) { b.TargetKind = "stack" },
		"extension version": func(b *Binding) { b.ExtensionVersionID = "00000000-0000-4000-8000-000000000001" },
		"source version":    func(b *Binding) { b.SourceVersion = "v1" },
		"source commit":     func(b *Binding) { b.SourceCommit = "commit" },
		"content digest":    func(b *Binding) { b.ContentDigest = Digest("content") },
		"manifest digest":   func(b *Binding) { b.ManifestDigest = Digest("manifest") },
		"execution digest":  func(b *Binding) { b.ExecutionDigest = Digest("execution") },
		"permission digest": func(b *Binding) { b.PermissionDigest = Digest("permission") },
		"parameter digest":  func(b *Binding) { b.ParameterDigest = Digest("parameter") },
		"network digest":    func(b *Binding) { b.NetworkDigest = Digest("network") },
		"secret digest":     func(b *Binding) { b.SecretGrantDigest = Digest("secret") },
		"plan id":           func(b *Binding) { b.PlanID = "plan" },
		"plan revision":     func(b *Binding) { b.PlanRevision = 1 },
		"plan digest":       func(b *Binding) { b.PlanDigest = Digest("plan") },
		"deployment id":     func(b *Binding) { b.DeploymentID = "deployment" },
		"run id":            func(b *Binding) { b.RunID = "run" },
		"run revision":      func(b *Binding) { b.RunRevision = 1 },
		"stage id":          func(b *Binding) { b.StageID = "stage" },
		"stage revision":    func(b *Binding) { b.StageRevision = 1 },
		"stage digest":      func(b *Binding) { b.StageDigest = Digest("stage") },
		"target digest":     func(b *Binding) { b.TargetDigest = Digest("target") },
		"artifact digest":   func(b *Binding) { b.ArtifactSetDigest = Digest("artifact") },
		"policy digest":     func(b *Binding) { b.PolicyDigest = Digest("policy") },
		"cost digest":       func(b *Binding) { b.CostQuoteDigest = Digest("cost") },
		"rollback digest":   func(b *Binding) { b.RollbackDigest = Digest("rollback") },
		"preview digest":    func(b *Binding) { b.PreviewDigest = Digest("preview") },
		"risk level":        func(b *Binding) { b.RiskLevel = "R2" },
		"gate type":         func(b *Binding) { b.GateType = "remote_execution" },
		"stage idempotency": func(b *Binding) { b.StageIdempotencyKey = "key" },
		"binding expiry":    func(b *Binding) { b.BindingExpiresAt = time.Unix(1, 0) },
		"selected tool":     func(b *Binding) { b.SelectedTool = "tool" },
		"selected command":  func(b *Binding) { b.SelectedCommand = []string{"run"} },
		"network grants":    func(b *Binding) { b.NetworkGrants = []string{"https://example.test"} },
		"secret grants":     func(b *Binding) { b.SecretGrants = []SecretGrant{{ReferenceID: "ref"}} },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			var b Binding
			mutate(&b)
			if b.IsZero() {
				t.Fatalf("binding with %s set was treated as zero", name)
			}
		})
	}
}

func validExecutionV2Binding() Binding {
	digest := Digest(strings.Repeat("a", 64))
	return Binding{
		OwnerID:             "@owner:example.test",
		OperationDomain:     "execution:v2:remote_execution",
		TargetID:            "00000000-0000-4000-8000-000000000001",
		TargetRevision:      4,
		TargetKind:          "aws_ec2_instance",
		ContentDigest:       digest,
		ExecutionDigest:     digest,
		ParameterDigest:     digest,
		NetworkDigest:       digest,
		SecretGrantDigest:   digest,
		PlanID:              "00000000-0000-4000-8000-000000000002",
		PlanRevision:        2,
		PlanDigest:          digest,
		DeploymentID:        "00000000-0000-4000-8000-000000000003",
		RunID:               "00000000-0000-4000-8000-000000000004",
		RunRevision:         3,
		StageID:             "00000000-0000-4000-8000-000000000005",
		StageRevision:       1,
		StageDigest:         digest,
		TargetDigest:        digest,
		ArtifactSetDigest:   digest,
		PolicyDigest:        digest,
		CostQuoteDigest:     digest,
		RollbackDigest:      digest,
		PreviewDigest:       digest,
		RiskLevel:           "R2",
		GateType:            "remote_execution",
		StageIdempotencyKey: "00000000-0000-4000-8000-000000000006",
		BindingExpiresAt:    time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

func validLegacyBinding(owner, target string) Binding {
	d := Digest(strings.Repeat("a", 64))
	return Binding{OwnerID: owner, OperationDomain: "extension.execute", TargetID: target, TargetRevision: 1, TargetKind: "mcp", ExtensionVersionID: "00000000-0000-4000-8000-000000000001", SourceVersion: "1.0.0", ContentDigest: d, ParameterDigest: d, NetworkDigest: d, SecretGrantDigest: d}
}

func TestExecutionV2BindingRequiresCompleteImmutableSnapshot(t *testing.T) {
	binding := validExecutionV2Binding()
	normalized, err := binding.Normalize()
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if normalized.SourceVersion != "" || normalized.SourceCommit != "" {
		t.Fatal("execution v2 binding unexpectedly requires a legacy source field")
	}

	tests := map[string]func(*Binding){
		"plan":     func(b *Binding) { b.PlanID = "" },
		"run":      func(b *Binding) { b.RunRevision = 0 },
		"stage":    func(b *Binding) { b.StageDigest = "" },
		"target":   func(b *Binding) { b.TargetDigest = "" },
		"artifact": func(b *Binding) { b.ArtifactSetDigest = "" },
		"policy":   func(b *Binding) { b.PolicyDigest = "" },
		"cost":     func(b *Binding) { b.CostQuoteDigest = "" },
		"rollback": func(b *Binding) { b.RollbackDigest = "" },
		"preview":  func(b *Binding) { b.PreviewDigest = "" },
		"idempotency": func(b *Binding) {
			b.StageIdempotencyKey = ""
		},
		"expiry": func(b *Binding) { b.BindingExpiresAt = time.Time{} },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := validExecutionV2Binding()
			mutate(&candidate)
			if _, err := candidate.Normalize(); err == nil {
				t.Fatal("Normalize() accepted an incomplete execution v2 binding")
			}
		})
	}
}

func TestExecutionV2BindingRejectsLegacyPassthroughFields(t *testing.T) {
	tests := map[string]func(*Binding){
		"selected tool": func(b *Binding) { b.SelectedTool = "runtime__shell" },
		"selected command": func(b *Binding) {
			b.SelectedCommand = []string{"sh", "-c", "echo unsafe"}
		},
		"network grants": func(b *Binding) {
			b.NetworkGrants = []string{"https://example.test"}
		},
		"secret grants": func(b *Binding) {
			b.SecretGrants = []SecretGrant{{
				ReferenceID:   "00000000-0000-4000-8000-000000000007",
				Purpose:       SecretPurposeAWSCredential,
				Revision:      1,
				BindingDigest: Digest(strings.Repeat("c", 64)),
			}}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			binding := validExecutionV2Binding()
			mutate(&binding)
			if _, err := binding.Normalize(); err == nil {
				t.Fatal("Normalize() accepted a legacy V2 passthrough field")
			}
		})
	}
}

func TestExecutionV2BindingRejectsRiskGateMismatch(t *testing.T) {
	binding := validExecutionV2Binding()
	binding.RiskLevel = "R3"
	if _, err := binding.Normalize(); err == nil {
		t.Fatal("Normalize() accepted an R3 risk with an R2 gate")
	}
}

func TestExecutionV2BindingRejectsAmbiguousDomainAndUnsafeIdentity(t *testing.T) {
	tests := map[string]func(*Binding){
		"ambiguous prefix": func(b *Binding) { b.OperationDomain = "execution:v2evil" },
		"missing suffix":   func(b *Binding) { b.OperationDomain = "execution:v2:" },
		"unsafe owner":     func(b *Binding) { b.OwnerID = "@owner:example.test\nforged" },
		"missing kind":     func(b *Binding) { b.TargetKind = "" },
		"unsafe kind":      func(b *Binding) { b.TargetKind = "aws ec2 instance" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			binding := validExecutionV2Binding()
			mutate(&binding)
			if _, err := binding.Normalize(); err == nil {
				t.Fatal("Normalize() accepted an ambiguous or unsafe V2 identity")
			}
		})
	}
}

func TestExecutionV2BindingEqualityIncludesPreviewDigest(t *testing.T) {
	first := validExecutionV2Binding()
	second := validExecutionV2Binding()
	second.PreviewDigest = Digest(strings.Repeat("b", 64))
	if first.Equal(second) {
		t.Fatal("bindings with different preview digests compared equal")
	}
}

func TestExecutionV2BindingEqualityIncludesBindingDigest(t *testing.T) {
	first, err := validExecutionV2Binding().Normalize()
	if err != nil {
		t.Fatal(err)
	}
	second := first
	second.Digest = strings.Repeat("b", 64)
	if first.Equal(second) {
		t.Fatal("bindings with different binding digests compared equal")
	}
}

func TestExtensionBindingRequiresExplicitVersionAndKind(t *testing.T) {
	valid := validLegacyBinding("@owner:example.test", "legacy-target")
	if _, err := valid.Normalize(); err != nil {
		t.Fatal(err)
	}
	withoutVersion := valid
	withoutVersion.ExtensionVersionID = ""
	if _, err := withoutVersion.Normalize(); err == nil {
		t.Fatal("extension binding without immutable version was accepted")
	}
	wrongKind := valid
	wrongKind.TargetKind = "stack"
	if _, err := wrongKind.Normalize(); err == nil {
		t.Fatal("extension binding with non-MCP target kind was accepted")
	}
	otherVersion := valid
	otherVersion.ExtensionVersionID = "00000000-0000-4000-8000-000000000002"
	if valid.Equal(otherVersion) {
		t.Fatal("extension bindings with different versions compared equal")
	}
}

func TestExecutionV2BindingIsCanonicallySealed(t *testing.T) {
	first, err := validExecutionV2Binding().Normalize()
	if err != nil {
		t.Fatal(err)
	}
	second, err := validExecutionV2Binding().Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if !Digest(first.Digest).Valid() || first.Digest != second.Digest {
		t.Fatalf("non-canonical seal: first=%q second=%q", first.Digest, second.Digest)
	}
	tampered := validExecutionV2Binding()
	tampered.Digest = strings.Repeat("b", 64)
	if _, err := tampered.Normalize(); err != ErrInvalid {
		t.Fatalf("tampered digest error = %v, want ErrInvalid", err)
	}
}

func TestExecutionV2BindingExpiryMustMatchConfirmation(t *testing.T) {
	binding := validExecutionV2Binding()
	if !binding.MatchesConfirmationExpiry(binding.BindingExpiresAt) {
		t.Fatal("binding rejected its exact UTC confirmation expiry")
	}
	if binding.MatchesConfirmationExpiry(binding.BindingExpiresAt.Add(time.Second)) {
		t.Fatal("binding accepted a different confirmation expiry")
	}
	legacy := Binding{OperationDomain: "aws"}
	if !legacy.MatchesConfirmationExpiry(time.Time{}) {
		t.Fatal("legacy binding unexpectedly requires a V2 expiry")
	}
}

func TestExecutionV2BindingOwnerMustMatchConfirmationOwner(t *testing.T) {
	binding := validExecutionV2Binding()
	if !binding.MatchesOwner("@owner:example.test") {
		t.Fatal("binding rejected its owner")
	}
	if binding.MatchesOwner("@other:example.test") || binding.MatchesOwner("") {
		t.Fatal("binding accepted an absent or different owner")
	}
}

func TestExecutionV2RequestRejectsCrossOwnerBinding(t *testing.T) {
	binding := validExecutionV2Binding()
	repository := NewMemoryRepository(func() time.Time {
		return time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC)
	})
	_, err := repository.Request(context.Background(), RequestCommand{
		OwnerID:        "@other:example.test",
		IdempotencyKey: "00000000-0000-4000-8000-000000000008",
		Binding:        binding,
		TaskID:         "00000000-0000-4000-8000-000000000009",
		ExpiresAt:      binding.BindingExpiresAt,
	})
	if err != ErrInvalid {
		t.Fatalf("Request() error = %v, want ErrInvalid", err)
	}
}

func TestMemoryRequestScopesLiveTargetByOwnerAndRecordsOwner(t *testing.T) {
	now := time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC)
	repository := NewMemoryRepository(func() time.Time { return now })
	firstBinding := validLegacyBinding("@owner:example.test", "legacy-target")
	first, err := repository.Request(context.Background(), RequestCommand{
		OwnerID: firstBinding.OwnerID, IdempotencyKey: "00000000-0000-4000-8000-000000000008", Binding: firstBinding,
		TaskID: "00000000-0000-4000-8000-000000000009", ExpiresAt: now.Add(time.Hour), At: now,
	})
	if err != nil {
		t.Fatalf("first Request() error = %v", err)
	}
	if first.OwnerID != firstBinding.OwnerID {
		t.Fatalf("Confirmation.OwnerID = %q, want %q", first.OwnerID, firstBinding.OwnerID)
	}
	secondBinding := validLegacyBinding("@other:example.test", "legacy-target")
	second, err := repository.Request(context.Background(), RequestCommand{
		OwnerID: secondBinding.OwnerID, IdempotencyKey: "00000000-0000-4000-8000-000000000010", Binding: secondBinding,
		TaskID: "00000000-0000-4000-8000-000000000011", ExpiresAt: now.Add(time.Hour), At: now,
	})
	if err != nil {
		t.Fatalf("different-owner Request() error = %v", err)
	}
	if second.OwnerID != secondBinding.OwnerID {
		t.Fatalf("second Confirmation.OwnerID = %q", second.OwnerID)
	}
	_, err = repository.Request(context.Background(), RequestCommand{
		OwnerID: firstBinding.OwnerID, IdempotencyKey: "00000000-0000-4000-8000-000000000012", Binding: firstBinding,
		TaskID: "00000000-0000-4000-8000-000000000013", ExpiresAt: now.Add(time.Hour), At: now,
	})
	if err != ErrConflict {
		t.Fatalf("same-owner duplicate Request() error = %v, want ErrConflict", err)
	}
}

func TestMemoryLegacyRequestReplayIsOwnerScoped(t *testing.T) {
	now := time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC)
	repository := NewMemoryRepository(func() time.Time { return now })
	binding, err := (Binding{OperationDomain: "extension.execute", TargetID: "legacy-target", TargetRevision: 1, TargetKind: "mcp", ExtensionVersionID: "00000000-0000-4000-8000-000000000001", SourceVersion: "1.0.0", ContentDigest: Digest(strings.Repeat("a", 64)), ParameterDigest: Digest(strings.Repeat("a", 64)), NetworkDigest: Digest(strings.Repeat("a", 64)), SecretGrantDigest: Digest(strings.Repeat("a", 64))}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	key := "00000000-0000-4000-8000-000000000014"
	request := func(owner, taskID string) (Confirmation, error) {
		return repository.Request(context.Background(), RequestCommand{OwnerID: owner, IdempotencyKey: key, Binding: binding, TaskID: taskID, ExpiresAt: now.Add(time.Hour), At: now})
	}
	first, err := request(" @owner:example.test ", "00000000-0000-4000-8000-000000000015")
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := request("@owner:example.test", "00000000-0000-4000-8000-000000000015")
	if err != nil || replayed.ConfirmationID != first.ConfirmationID {
		t.Fatalf("same-owner replay = %#v, %v", replayed, err)
	}
	other, err := request("@other:example.test", "00000000-0000-4000-8000-000000000016")
	if err != nil {
		t.Fatalf("cross-owner request leaked/replayed: %v", err)
	}
	if other.ConfirmationID == first.ConfirmationID || other.OwnerID != "@other:example.test" {
		t.Fatalf("cross-owner confirmation leaked: first=%#v other=%#v", first, other)
	}
}

func TestMemoryMutationOwnerAndReplayAreOwnerScoped(t *testing.T) {
	now := time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC)
	repository := NewMemoryRepository(func() time.Time { return now })
	newRequest := func(idempotency, taskID string) Confirmation {
		binding := validLegacyBinding("@owner:example.test", taskID)
		value, err := repository.Request(context.Background(), RequestCommand{OwnerID: binding.OwnerID, IdempotencyKey: idempotency, Binding: binding, TaskID: taskID, ExpiresAt: now.Add(time.Hour), At: now})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	owner := "@owner:example.test"
	other := "@other:example.test"
	first := newRequest("00000000-0000-4000-8000-000000000021", "00000000-0000-4000-8000-000000000022")
	confirm := ConfirmCommand{OwnerID: owner, ConfirmationID: first.ConfirmationID, IdempotencyKey: "00000000-0000-4000-8000-000000000023", ExpectedRevision: first.Revision, Binding: first.Binding, At: now}
	if _, err := repository.Confirm(context.Background(), confirm); err != nil {
		t.Fatal(err)
	}
	confirm.OwnerID = other
	if _, err := repository.Confirm(context.Background(), confirm); err != ErrConflict {
		t.Fatalf("cross-owner confirm/replay = %v, want ErrConflict", err)
	}

	second := newRequest("00000000-0000-4000-8000-000000000024", "00000000-0000-4000-8000-000000000025")
	if _, err := repository.Reject(context.Background(), RejectCommand{OwnerID: other, ConfirmationID: second.ConfirmationID, IdempotencyKey: "00000000-0000-4000-8000-000000000026", ExpectedRevision: second.Revision, Reason: "no", At: now}); err != ErrConflict {
		t.Fatalf("cross-owner reject = %v, want ErrConflict", err)
	}

	third := newRequest("00000000-0000-4000-8000-000000000027", "00000000-0000-4000-8000-000000000028")
	if _, err := repository.Confirm(context.Background(), ConfirmCommand{OwnerID: owner, ConfirmationID: third.ConfirmationID, IdempotencyKey: "00000000-0000-4000-8000-000000000029", ExpectedRevision: third.Revision, Binding: third.Binding, At: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Consume(context.Background(), ConsumeCommand{OwnerID: other, ConfirmationID: third.ConfirmationID, IdempotencyKey: "00000000-0000-4000-8000-000000000030", TaskID: third.TaskID, Attempt: 1, LeaseEpoch: 1, ExpectedRevision: 2, ExpectedTaskRevision: 1, Binding: third.Binding, At: now}); err != ErrConflict {
		t.Fatalf("cross-owner consume = %v, want ErrConflict", err)
	}
}

func TestMemoryListFiltersByOwner(t *testing.T) {
	now := time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC)
	repository := NewMemoryRepository(func() time.Time { return now })
	request := func(owner, key, taskID string) Confirmation {
		binding := validLegacyBinding(owner, taskID)
		value, err := repository.Request(context.Background(), RequestCommand{
			OwnerID: owner, IdempotencyKey: key, Binding: binding, TaskID: taskID,
			ExpiresAt: now.Add(time.Hour), At: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	owner := "@owner:example.test"
	other := "@other:example.test"
	request(owner, "00000000-0000-4000-8000-000000000031", "00000000-0000-4000-8000-000000000032")
	request(other, "00000000-0000-4000-8000-000000000033", "00000000-0000-4000-8000-000000000034")
	page, err := repository.List(context.Background(), ListQuery{OwnerID: owner, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Confirmations) != 1 || page.Confirmations[0].OwnerID != owner {
		t.Fatalf("owner list = %#v, want only %s", page.Confirmations, owner)
	}
	if _, err := repository.List(context.Background(), ListQuery{PageSize: 10}); err != ErrInvalid {
		t.Fatalf("ownerless list error = %v, want ErrInvalid", err)
	}
}
