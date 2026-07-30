package aws

import (
	"context"
	"testing"
	"time"
)

func TestMemoryCredentialListZeroSizeUsesDefaultPage(t *testing.T) {
	r := NewMemoryRepository()
	c := RehydrateCredentials("00000000-0000-4000-8000-000000000001", "aws", "us-east-1", "", "", []byte("a"), []byte("b"), nil, 0, 1, time.Now(), time.Now())
	if _, err := r.CreateCredential(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	page, err := r.ListCredentials(context.Background(), 0, "")
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("zero-size page = %#v err=%v", page, err)
	}
}

func TestCredentialViewCarriesVerificationAndConfiguredFlagsWithoutSecrets(t *testing.T) {
	c := RehydrateCredentials("00000000-0000-4000-8000-000000000001", "aws", "us-east-1", "", "", []byte("AKIA"), []byte("secret"), nil, 0, 2, time.Time{}, time.Time{})
	v := c.View()
	if !v.AccessKeyConfigured || !v.SecretAccessKeyConfigured || v.SessionTokenConfigured || v.VerifiedRevision != 0 {
		t.Fatalf("view = %#v", v)
	}
	c = RehydrateCredentials(c.ID, c.Name, c.Region, "", "", []byte("AKIA"), []byte("secret"), nil, 2, 2, c.CreatedAt, c.UpdatedAt)
	v = c.View()
	if v.VerifiedRevision != 2 || !v.AccessKeyConfigured || !v.SecretAccessKeyConfigured {
		t.Fatalf("verified view = %#v", v)
	}
}
