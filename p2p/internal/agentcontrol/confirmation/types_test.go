package confirmation

import "testing"

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
		"source version":    func(b *Binding) { b.SourceVersion = "v1" },
		"source commit":     func(b *Binding) { b.SourceCommit = "commit" },
		"content digest":    func(b *Binding) { b.ContentDigest = Digest("content") },
		"manifest digest":   func(b *Binding) { b.ManifestDigest = Digest("manifest") },
		"execution digest":  func(b *Binding) { b.ExecutionDigest = Digest("execution") },
		"permission digest": func(b *Binding) { b.PermissionDigest = Digest("permission") },
		"parameter digest":  func(b *Binding) { b.ParameterDigest = Digest("parameter") },
		"network digest":    func(b *Binding) { b.NetworkDigest = Digest("network") },
		"secret digest":     func(b *Binding) { b.SecretGrantDigest = Digest("secret") },
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
