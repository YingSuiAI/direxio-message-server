package storage

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestDatabaseStoreListFilteredUsesBoundOwnerSourceStateAndCursor(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewUnmigratedDatabaseStore(db, nil)
	query := `SELECT installation_id FROM p2p_agent_extensions WHERE owner_id=$1 AND installation_id>$2 AND ($3='' OR candidate_json->>'source'=$3) AND ($4='' OR state=$4) ORDER BY installation_id LIMIT $5`
	mock.ExpectQuery(regexp.QuoteMeta(query)).WithArgs("owner", "cursor", "github", "installed", 2).WillReturnRows(sqlmock.NewRows([]string{"installation_id"}))
	got, next, err := store.ListFiltered(context.Background(), "owner", 1, "cursor", "github", "installed")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 || next != "" {
		t.Fatalf("result = %#v next=%q", got, next)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDatabaseStoreListFilteredRejectsInvalidFilterBeforeQuery(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewUnmigratedDatabaseStore(db, nil)
	if _, _, err := store.ListFiltered(context.Background(), "owner", 10, "", "stdio", ""); err == nil {
		t.Fatal("expected invalid source")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
