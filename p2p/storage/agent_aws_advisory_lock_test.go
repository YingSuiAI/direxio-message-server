package storage

import (
	"bytes"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	agentaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
)

func TestCanonicalAdvisoryLockIdentityIsDeterministicSeparatedAndNULFree(t *testing.T) {
	got := canonicalAdvisoryLockIdentity("aws", "owner", "credential-save", "key")
	if got != canonicalAdvisoryLockIdentity("aws", "owner", "credential-save", "key") {
		t.Fatal("canonical advisory lock identity is not deterministic")
	}
	if bytes.IndexByte([]byte(got), 0) >= 0 {
		t.Fatalf("canonical advisory lock identity contains NUL: %q", got)
	}
	if got == canonicalAdvisoryLockIdentity("other", "owner", "credential-save", "key") {
		t.Fatal("advisory lock domains are not separated")
	}
	if got == canonicalAdvisoryLockIdentity("aws", "owner", "credential-save", "ke", "y") {
		t.Fatal("length-prefixed advisory lock parts are not separated")
	}
}

func TestCredentialIdempotentMutationsPropagateReplayLockErrors(t *testing.T) {
	const (
		credentialID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
		key          = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	)
	lockErr := errors.New("advisory lock failed")

	tests := []struct {
		name string
		call func(*PostgresAWSRepository) error
	}{
		{
			name: "save",
			call: func(repo *PostgresAWSRepository) error {
				credential := agentaws.RehydrateCredentials(credentialID, "prod", "us-east-1", "", "", []byte("access"), []byte("secret"), nil, 0, 1, time.Now().UTC(), time.Now().UTC())
				_, err := repo.SaveCredentialIdempotent(t.Context(), credential, key, "digest")
				return err
			},
		},
		{
			name: "replace",
			call: func(repo *PostgresAWSRepository) error {
				credential := agentaws.RehydrateCredentials(credentialID, "prod", "us-east-1", "", "", []byte("access"), []byte("secret"), nil, 0, 2, time.Now().UTC(), time.Now().UTC())
				_, err := repo.ReplaceCredentialIdempotent(t.Context(), credential, 1, key, "digest")
				return err
			},
		},
		{
			name: "delete",
			call: func(repo *PostgresAWSRepository) error {
				return repo.DeleteCredentialIdempotent(t.Context(), credentialID, 1, key, "digest")
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()

			var repo *PostgresAWSRepository
			if test.name == "delete" {
				repo, err = NewAgentAWSRepository(NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter()), testAWSOwnerID)
			} else {
				keyring, keyringErr := LoadOrCreateAgentSecretKeyring(t.TempDir() + "/keyring.json")
				if keyringErr != nil {
					t.Fatal(keyringErr)
				}
				enveloper, enveloperErr := NewAgentSecretEnveloper(keyring)
				if enveloperErr != nil {
					t.Fatal(enveloperErr)
				}
				repo, err = NewAgentAWSRepositoryWithEnveloper(NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter()), testAWSOwnerID, enveloper)
			}
			if err != nil {
				t.Fatal(err)
			}

			mock.ExpectBegin()
			mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1, 0))")).
				WithArgs(canonicalAdvisoryLockIdentity("aws", testAWSOwnerID, "credential-"+test.name, key)).
				WillReturnError(lockErr)
			mock.ExpectRollback()
			if err := test.call(repo); !errors.Is(err, lockErr) {
				t.Fatalf("error = %v, want replay lock error", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
