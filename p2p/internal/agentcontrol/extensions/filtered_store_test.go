package extensions

import (
	"context"
	"github.com/google/uuid"
	"testing"
)

func filteredInstallation(t *testing.T, owner, source, state string) Installation {
	t.Helper()
	i := inspection()
	inst := Installation{ID: uuid.NewString(), OwnerID: owner, Candidate: i.Candidate, Revision: 1, State: state, Versions: []Version{{VersionID: uuid.NewString(), Pin: i.Candidate.Pin, ContentDigest: i.ContentDigest, ManifestDigest: i.ManifestDigest, ExecutionDigest: i.ExecutionDigest, NetworkDigest: i.NetworkDigest, SecretDigest: i.SecretDigest, Execution: i.Execution, NetworkGrants: i.NetworkGrants, SecretGrants: i.SecretGrants}}}
	inst.Candidate.Source = source
	if err := inst.Validate(); err != nil {
		t.Fatalf("invalid fixture: %v", err)
	}
	return inst
}

func TestMemoryStoreListFilteredOwnerSourceStateAndCursor(t *testing.T) {
	store := NewMemoryStore()
	items := []Installation{
		filteredInstallation(t, "owner", "github", "installed"),
		filteredInstallation(t, "owner", "github", "installed"),
		filteredInstallation(t, "owner", "github", "failed"),
		filteredInstallation(t, "owner", "glama", "installed"),
		filteredInstallation(t, "other", "github", "installed"),
	}
	for _, item := range items {
		if err := store.Put(context.Background(), item); err != nil {
			t.Fatal(err)
		}
	}
	got, next, err := store.ListFiltered(context.Background(), "owner", 1, "", "github", "installed")
	if err != nil || len(got) != 1 || next == "" || got[0].Candidate.Source != "github" || got[0].State != "installed" {
		t.Fatalf("first page = %#v next=%q err=%v", got, next, err)
	}
	got, next, err = store.ListFiltered(context.Background(), "owner", 1, next, "github", "installed")
	if err != nil || len(got) != 1 || next != "" {
		t.Fatalf("cursor page = %#v next=%q err=%v", got, next, err)
	}
	if _, _, err = store.ListFiltered(context.Background(), "owner", 1, "", "stdio", ""); err != ErrInvalid {
		t.Fatalf("invalid source error = %v", err)
	}
}
