package storage

import (
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestTaskReplayLockUsesCanonicalIdentityAndStopsOnError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const (
		owner = "@owner:example.test"
		key   = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	)
	lockErr := errors.New("task advisory lock failed")
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1,0))")).
		WithArgs(canonicalAdvisoryLockIdentity("agent-task", owner, "create", key)).
		WillReturnError(lockErr)
	mock.ExpectRollback()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := lockTaskReplayTx(t.Context(), tx, owner, "create", key); !errors.Is(err, lockErr) {
		t.Fatalf("error = %v, want advisory lock error", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestConfirmationReplayLockUsesCanonicalIdentityAndStopsOnError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const (
		owner = "@owner:example.test"
		key   = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	)
	lockErr := errors.New("confirmation advisory lock failed")
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1,0))")).
		WithArgs(canonicalAdvisoryLockIdentity("agent-confirmation", owner, "confirm", key)).
		WillReturnError(lockErr)
	mock.ExpectRollback()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := lockConfirmationReplayTx(t.Context(), tx, owner, "confirm", key); !errors.Is(err, lockErr) {
		t.Fatalf("error = %v, want advisory lock error", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestScheduleConfirmationLockUsesCanonicalIdentityAndStopsBeforeMutation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const (
		owner        = "@owner:example.test"
		conversation = "!conversation:example.test"
	)
	lockErr := errors.New("schedule confirmation advisory lock failed")
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1,0))")).
		WithArgs(canonicalAdvisoryLockIdentity("schedule-confirmation", owner, conversation)).
		WillReturnError(lockErr)
	mock.ExpectRollback()
	store := NewUnmigratedDatabaseStore(db, nil)
	_, _, err = store.ReserveScheduleConfirmation(t.Context(), ScheduleConfirmation{
		OwnerID:        owner,
		ConversationID: conversation,
	})
	if !errors.Is(err, lockErr) {
		t.Fatalf("error = %v, want advisory lock error", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
