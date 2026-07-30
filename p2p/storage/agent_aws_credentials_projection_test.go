package storage

import (
	"bytes"
	"database/sql"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	agentaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
)

func TestCredentialEnvelopeConfiguredFailsClosedOnTamperOrUnknownKey(t *testing.T) {
	ring := &AgentSecretKeyring{activeKeyID: "active", keys: map[string][]byte{"active": bytes.Repeat([]byte{7}, 32)}}
	enveloper, err := NewAgentSecretEnveloper(ring)
	if err != nil {
		t.Fatal(err)
	}
	binding := credentialBinding("@owner:example", "00000000-0000-4000-8000-000000000001", 2)
	sealed, err := enveloper.Seal(binding, []byte(`{"Access":"AKIA","Secret":"secret","Session":""}`))
	if err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte(nil), sealed.Ciphertext...)
	tampered[0] ^= 0xff
	if _, _, _, err := credentialEnvelopeConfigured(enveloper, "@owner:example", binding.EntityID, 2, sealed.KeyID, sealed.Nonce, tampered); err == nil {
		t.Fatal("tampered envelope accepted")
	}
	if _, _, _, err := credentialEnvelopeConfigured(enveloper, "@owner:example", binding.EntityID, 2, "unknown", sealed.Nonce, sealed.Ciphertext); err == nil {
		t.Fatal("unknown key accepted")
	}
	if _, _, _, err := credentialEnvelopeConfigured(enveloper, "@owner:example", binding.EntityID, 2, "", nil, nil); err == nil {
		t.Fatal("missing envelope accepted")
	}
	empty, err := enveloper.Seal(binding, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := credentialEnvelopeConfigured(enveloper, "@owner:example", binding.EntityID, 2, empty.KeyID, empty.Nonce, empty.Ciphertext); err == nil {
		t.Fatal("empty credential payload accepted")
	}
	unknown, err := enveloper.Seal(binding, []byte(`{"Access":"a","Secret":"b","Unexpected":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := credentialEnvelopeConfigured(enveloper, "@owner:example", binding.EntityID, 2, unknown.KeyID, unknown.Nonce, unknown.Ciphertext); err == nil {
		t.Fatal("unknown credential payload field accepted")
	}
	if _, _, _, err := credentialEnvelopeConfigured(enveloper, "@other:example", binding.EntityID, 2, sealed.KeyID, sealed.Nonce, sealed.Ciphertext); err == nil {
		t.Fatal("cross-owner envelope accepted")
	}
	trailing, err := enveloper.Seal(binding, []byte(`{"Access":"a","Secret":"b"} {}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := credentialEnvelopeConfigured(enveloper, "@owner:example", binding.EntityID, 2, trailing.KeyID, trailing.Nonce, trailing.Ciphertext); err == nil {
		t.Fatal("trailing credential payload accepted")
	}
}

func TestCredentialSecretRowRejectsSiblingReference(t *testing.T) {
	binding := credentialBinding("@owner:example", "00000000-0000-4000-8000-000000000001", 2)
	if err := validateCredentialSecretRow("@owner:example", binding.EntityID, 2, "sibling", binding.BindingDigest[:], 1, 1); err == nil {
		t.Fatal("sibling secret reference accepted")
	}
}

func TestGetCredentialFailsClosedWithoutKeyring(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := &PostgresAWSRepository{store: NewUnmigratedDatabaseStore(db, nil), ownerID: "@owner:example"}
	if _, err := repo.GetCredential(t.Context(), "00000000-0000-4000-8000-000000000001"); err != ErrAgentSecretKeyringUnavailable {
		t.Fatalf("GetCredential error = %v, want %v", err, ErrAgentSecretKeyringUnavailable)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetCredentialRejectsSealedEmptyPayload(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	owner := "@owner:example"
	id := "00000000-0000-4000-8000-000000000001"
	ring := &AgentSecretKeyring{activeKeyID: "active", keys: map[string][]byte{"active": bytes.Repeat([]byte{9}, 32)}}
	enveloper, err := NewAgentSecretEnveloper(ring)
	if err != nil {
		t.Fatal(err)
	}
	binding := credentialBinding(owner, id, 1)
	sealed, err := enveloper.Seal(binding, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(10, 0).UTC()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT c.credential_id::text,c.name,c.region,c.account_id,c.user_arn,c.verified_revision,c.revision,c.created_at,c.updated_at FROM core_aws_credentials c WHERE c.owner_id=$1 AND c.credential_id=$2 AND EXISTS(SELECT 1 FROM core_aws_credential_current cur WHERE cur.owner_id=c.owner_id AND cur.credential_id=c.credential_id AND cur.revision=c.revision AND cur.deleted_at IS NULL)")).WithArgs(owner, id).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "region", "account_id", "user_arn", "verified_revision", "revision", "created_at", "updated_at"}).AddRow(id, "prod", "us-east-1", "", "", int64(0), int64(1), now, now))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT reference,binding_digest,envelope_version,aad_version,key_id,nonce,ciphertext FROM p2p_agent_secrets WHERE owner_id=$1 AND entity_id=$2 AND secret_domain='aws' AND purpose='credential' AND secret_revision=$3 ORDER BY created_at,key_id")).WithArgs(owner, id, int64(1)).WillReturnRows(sqlmock.NewRows([]string{"reference", "binding_digest", "envelope_version", "aad_version", "key_id", "nonce", "ciphertext"}).AddRow(id, binding.BindingDigest[:], int64(1), int64(1), sealed.KeyID, sealed.Nonce, sealed.Ciphertext))
	repo := &PostgresAWSRepository{store: NewUnmigratedDatabaseStore(db, nil), ownerID: owner, enveloper: enveloper}
	if _, err := repo.GetCredential(t.Context(), id); err != agentaws.ErrInvalid {
		t.Fatalf("GetCredential error = %v, want invalid", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyAWSCredentialSecretRowsRejectsDuplicateAndMalformed(t *testing.T) {
	tests := []struct {
		name string
		rows int
		body []byte
		want bool
	}{
		{name: "duplicate", rows: 2, body: []byte(`{"Access":"a","Secret":"b"}`), want: true},
		{name: "empty-payload", rows: 1, body: []byte(`{}`), want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			owner := "@owner:example"
			id := "00000000-0000-4000-8000-000000000001"
			ring := &AgentSecretKeyring{activeKeyID: "active", keys: map[string][]byte{"active": bytes.Repeat([]byte{8}, 32)}}
			enveloper, err := NewAgentSecretEnveloper(ring)
			if err != nil {
				t.Fatal(err)
			}
			binding := credentialBinding(owner, id, 1)
			sealed, err := enveloper.Seal(binding, tc.body)
			if err != nil {
				t.Fatal(err)
			}
			mock.ExpectQuery(regexp.QuoteMeta("SELECT 1 FROM p2p_agent_secrets s WHERE s.secret_domain='aws' AND s.purpose='credential' AND NOT EXISTS (SELECT 1 FROM core_aws_credentials c WHERE c.owner_id=s.owner_id AND c.credential_id::text=s.entity_id AND c.revision=s.secret_revision) LIMIT 1")).WillReturnError(sql.ErrNoRows)
			mock.ExpectQuery(regexp.QuoteMeta("SELECT owner_id,credential_id::text,revision FROM core_aws_credentials ORDER BY owner_id,credential_id,revision")).WillReturnRows(sqlmock.NewRows([]string{"owner_id", "credential_id", "revision"}).AddRow(owner, id, int64(1)))
			secretRows := sqlmock.NewRows([]string{"reference", "binding_digest", "envelope_version", "aad_version", "key_id", "nonce", "ciphertext"})
			for i := 0; i < tc.rows; i++ {
				secretRows.AddRow(id, binding.BindingDigest[:], int64(1), int64(1), sealed.KeyID, sealed.Nonce, sealed.Ciphertext)
			}
			mock.ExpectQuery(regexp.QuoteMeta("SELECT reference,binding_digest,envelope_version,aad_version,key_id,nonce,ciphertext FROM p2p_agent_secrets WHERE owner_id=$1 AND entity_id=$2 AND secret_domain='aws' AND purpose='credential' AND secret_revision=$3 ORDER BY created_at,key_id")).WithArgs(owner, id, int64(1)).WillReturnRows(secretRows)
			if err := verifyAWSCredentialSecretRows(t.Context(), db, enveloper); err == nil {
				t.Fatal("malformed AWS credential secret accepted")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestVerifyAWSCredentialSecretRowsRejectsOrphan(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ring := &AgentSecretKeyring{activeKeyID: "active", keys: map[string][]byte{"active": bytes.Repeat([]byte{6}, 32)}}
	enveloper, err := NewAgentSecretEnveloper(ring)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT 1 FROM p2p_agent_secrets s WHERE s.secret_domain='aws' AND s.purpose='credential' AND NOT EXISTS (SELECT 1 FROM core_aws_credentials c WHERE c.owner_id=s.owner_id AND c.credential_id::text=s.entity_id AND c.revision=s.secret_revision) LIMIT 1")).WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))
	if err := verifyAWSCredentialSecretRows(t.Context(), db, enveloper); err == nil {
		t.Fatal("orphan AWS credential secret accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
