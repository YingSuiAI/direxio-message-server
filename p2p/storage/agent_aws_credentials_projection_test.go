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

func TestCredentialRevisionMetadataDoesNotDecryptSecret(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	owner := "@owner:example"
	id := "00000000-0000-4000-8000-000000000001"
	now := time.Unix(10, 0).UTC()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT credential_id::text,name,region,account_id,user_arn,verified_revision,revision,created_at,updated_at FROM core_aws_credentials WHERE owner_id=$1 AND credential_id=$2 AND revision=$3")).WithArgs(owner, id, int64(2)).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "region", "account_id", "user_arn", "verified_revision", "revision", "created_at", "updated_at"}).AddRow(id, "prod", "us-east-1", "123456789012", "arn:aws:iam::123456789012:user/a", int64(2), int64(2), now, now))
	repo := &PostgresAWSRepository{store: NewUnmigratedDatabaseStore(db, nil), ownerID: owner}
	cred, err := repo.GetCredentialRevisionMetadata(t.Context(), id, 2)
	if err != nil || cred.AccountID != "123456789012" || cred.VerifiedRevision != 2 {
		t.Fatalf("metadata = %#v err=%v", cred, err)
	}
	access, secret, session := cred.StoredSecretBytes()
	if len(access) != 0 || len(secret) != 0 || len(session) != 0 || cred.String() != "[redacted-coreaws-credentials]" {
		t.Fatal("metadata projection has unexpected secret state")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialRevisionMetadataBatchPinsOwnerIDAndRevision(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	owner := "@owner:example"
	first := "00000000-0000-4000-8000-000000000001"
	second := "00000000-0000-4000-8000-000000000002"
	now := time.Unix(10, 0).UTC()
	query := "SELECT credential_id::text,name,region,account_id,user_arn,verified_revision,revision,created_at,updated_at FROM core_aws_credentials WHERE owner_id=$1 AND ((credential_id=$2 AND revision=$3) OR (credential_id=$4 AND revision=$5))"
	mock.ExpectQuery(regexp.QuoteMeta(query)).WithArgs(owner, first, int64(2), second, int64(3)).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "region", "account_id", "user_arn", "verified_revision", "revision", "created_at", "updated_at"}).
		AddRow(first, "one", "us-east-1", "123456789012", "arn:aws:iam::123456789012:user/one", int64(2), int64(2), now, now).
		AddRow(second, "two", "us-east-1", "123456789012", "arn:aws:iam::123456789012:user/two", int64(3), int64(3), now, now))
	repo := &PostgresAWSRepository{store: NewUnmigratedDatabaseStore(db, nil), ownerID: owner}
	got, err := repo.ListCredentialRevisionMetadata(t.Context(), []agentaws.CredentialRevisionRef{{ID: first, Revision: 2}, {ID: second, Revision: 3}})
	if err != nil || got[first+":2"].UserARN == "" || got[second+":3"].VerifiedRevision != 3 {
		t.Fatalf("metadata batch = %#v err=%v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialRevisionMetadataBatchFailsClosedWhenRowMissingWithoutSecretQuery(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	owner := "@owner:example"
	first := "00000000-0000-4000-8000-000000000001"
	second := "00000000-0000-4000-8000-000000000002"
	now := time.Unix(10, 0).UTC()
	query := "SELECT credential_id::text,name,region,account_id,user_arn,verified_revision,revision,created_at,updated_at FROM core_aws_credentials WHERE owner_id=$1 AND ((credential_id=$2 AND revision=$3) OR (credential_id=$4 AND revision=$5))"
	mock.ExpectQuery(regexp.QuoteMeta(query)).WithArgs(owner, first, int64(2), second, int64(3)).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "region", "account_id", "user_arn", "verified_revision", "revision", "created_at", "updated_at"}).AddRow(first, "one", "us-east-1", "123456789012", "arn:aws:iam::123456789012:user/one", int64(2), int64(2), now, now))
	repo := &PostgresAWSRepository{store: NewUnmigratedDatabaseStore(db, nil), ownerID: owner}
	if _, err = repo.ListCredentialRevisionMetadata(t.Context(), []agentaws.CredentialRevisionRef{{ID: first, Revision: 2}, {ID: second, Revision: 3}}); err != agentaws.ErrNotFound {
		t.Fatalf("missing metadata error = %v, want not found", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func expectCredentialRevisionReadback(t *testing.T, mock sqlmock.Sqlmock, owner, id string, revision int64, enveloper *AgentSecretEnveloper, now time.Time) {
	t.Helper()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT c.credential_id::text,c.name,c.region,c.account_id,c.user_arn,c.verified_revision,c.revision,c.created_at,c.updated_at FROM core_aws_credentials c WHERE c.owner_id=$1 AND c.credential_id=$2 AND c.revision=$3")).WithArgs(owner, id, revision).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "region", "account_id", "user_arn", "verified_revision", "revision", "created_at", "updated_at"}).AddRow(id, "prod", "us-east-1", "123456789012", "arn:aws:iam::123456789012:user/prod", revision, revision, now, now))
	binding := credentialBinding(owner, id, revision)
	sealed, err := enveloper.Seal(binding, []byte(`{"Access":"a","Secret":"b","Session":""}`))
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT reference,binding_digest,envelope_version,aad_version,key_id,nonce,ciphertext FROM p2p_agent_secrets WHERE owner_id=$1 AND entity_id=$2 AND secret_domain='aws' AND purpose='credential' AND secret_revision=$3 ORDER BY created_at,key_id")).WithArgs(owner, id, revision).WillReturnRows(sqlmock.NewRows([]string{"reference", "binding_digest", "envelope_version", "aad_version", "key_id", "nonce", "ciphertext"}).AddRow(id, binding.BindingDigest[:], int64(1), int64(1), sealed.KeyID, sealed.Nonce, sealed.Ciphertext))
}

func TestPostgresRecordCredentialIdentityIsImmutableAndIdempotent(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	owner := "@owner:example"
	id := "00000000-0000-4000-8000-000000000001"
	now := time.Unix(10, 0).UTC()
	ring := &AgentSecretKeyring{activeKeyID: "active", keys: map[string][]byte{"active": bytes.Repeat([]byte{7}, 32)}}
	enveloper, err := NewAgentSecretEnveloper(ring)
	if err != nil {
		t.Fatal(err)
	}
	repo := &PostgresAWSRepository{store: NewUnmigratedDatabaseStore(db, nil), ownerID: owner, enveloper: enveloper}
	identityQuery := "SELECT account_id,user_arn,verified_revision,revision FROM core_aws_credentials WHERE owner_id=$1 AND credential_id=$2 AND revision=$3 FOR UPDATE"
	updateSQL := "UPDATE core_aws_credentials SET account_id=$1,user_arn=$2,verified_revision=revision,updated_at=clock_timestamp() WHERE owner_id=$3 AND credential_id=$4 AND revision=$5"
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(identityQuery)).WithArgs(owner, id, int64(2)).WillReturnRows(sqlmock.NewRows([]string{"account_id", "user_arn", "verified_revision", "revision"}).AddRow(nil, nil, int64(0), int64(2)))
	mock.ExpectExec(regexp.QuoteMeta(updateSQL)).WithArgs("123456789012", "arn:aws:iam::123456789012:user/prod", owner, id, int64(2)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	expectCredentialRevisionReadback(t, mock, owner, id, 2, enveloper, now)
	first, err := repo.RecordCredentialIdentity(t.Context(), id, 2, agentaws.Identity{AccountID: "123456789012", UserARN: "arn:aws:iam::123456789012:user/prod"})
	if err != nil || first.VerifiedRevision != 2 {
		t.Fatalf("first identity write = %#v err=%v", first, err)
	}
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(identityQuery)).WithArgs(owner, id, int64(2)).WillReturnRows(sqlmock.NewRows([]string{"account_id", "user_arn", "verified_revision", "revision"}).AddRow("123456789012", "arn:aws:iam::123456789012:user/prod", int64(2), int64(2)))
	mock.ExpectCommit()
	expectCredentialRevisionReadback(t, mock, owner, id, 2, enveloper, now)
	if _, err = repo.RecordCredentialIdentity(t.Context(), id, 2, agentaws.Identity{AccountID: "123456789012", UserARN: "arn:aws:iam::123456789012:user/prod"}); err != nil {
		t.Fatalf("same identity replay = %v", err)
	}
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(identityQuery)).WithArgs(owner, id, int64(2)).WillReturnRows(sqlmock.NewRows([]string{"account_id", "user_arn", "verified_revision", "revision"}).AddRow("123456789012", "arn:aws:iam::123456789012:user/prod", int64(2), int64(2)))
	mock.ExpectRollback()
	if _, err = repo.RecordCredentialIdentity(t.Context(), id, 2, agentaws.Identity{AccountID: "999999999999", UserARN: "arn:aws:iam::123456789012:user/prod"}); err != agentaws.ErrConflict {
		t.Fatalf("different identity error = %v, want conflict", err)
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
