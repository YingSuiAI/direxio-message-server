package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	ext "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/extensions"
)

func sqlPinnedTools() []ext.Tool {
	schema := []byte(`{"type":"object"}`)
	return []ext.Tool{{Name: "hello", Description: "say hello", InputSchemaDigest: ext.DigestBytes(schema), InputSchema: schema}}
}

func expectPinLocks(mock sqlmock.Sqlmock, raw []byte, revision int64, active string) {
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT revision,state,active_version_id FROM p2p_agent_extensions WHERE owner_id=$1 AND installation_id=$2 FOR UPDATE")).WithArgs("owner", "install").WillReturnRows(sqlmock.NewRows([]string{"revision", "state", "active_version_id"}).AddRow(revision, "installed", active))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT tools_json FROM p2p_agent_extension_versions WHERE owner_id=$1 AND installation_id=$2 AND version_id=$3 FOR UPDATE")).WithArgs("owner", "install", "version").WillReturnRows(sqlmock.NewRows([]string{"tools_json"}).AddRow(raw))
}

func TestDatabaseStorePinToolsPinsOnceAndReplaysExactList(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter())
	tools := sqlPinnedTools()
	raw, _ := json.Marshal(tools)
	expectPinLocks(mock, []byte("null"), 7, "version")
	mock.ExpectExec(regexp.QuoteMeta("UPDATE p2p_agent_extension_versions SET tools_json=")).WithArgs(string(raw), "owner", "install", "version").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	got, err := store.PinTools(context.Background(), "owner", "install", "version", 7, tools)
	if err != nil || !ext.PinnedToolsEqual(got, tools) {
		t.Fatalf("pin = %#v, %v", got, err)
	}

	expectPinLocks(mock, raw, 7, "version")
	mock.ExpectCommit()
	replay, err := store.PinTools(context.Background(), "owner", "install", "version", 7, tools)
	if err != nil || !ext.PinnedToolsEqual(replay, tools) {
		t.Fatalf("replay = %#v, %v", replay, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDatabaseStorePinToolsRejectsStaleOrDriftedRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter())
	tools := sqlPinnedTools()
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT revision,state,active_version_id FROM p2p_agent_extensions")).WithArgs("owner", "install").WillReturnRows(sqlmock.NewRows([]string{"revision", "state", "active_version_id"}).AddRow(8, "installed", "version"))
	mock.ExpectRollback()
	if _, err := store.PinTools(context.Background(), "owner", "install", "version", 7, tools); err != ext.ErrRevisionConflict {
		t.Fatalf("stale revision = %v, want revision conflict", err)
	}

	// A non-empty row is immutable: an altered provider response is rejected
	// without issuing an UPDATE.
	drift := sqlPinnedTools()
	drift[0].Description = "provider changed"
	driftRaw, _ := json.Marshal(drift)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT revision,state,active_version_id FROM p2p_agent_extensions")).WithArgs("owner", "install").WillReturnRows(sqlmock.NewRows([]string{"revision", "state", "active_version_id"}).AddRow(7, "installed", "version"))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT tools_json FROM p2p_agent_extension_versions")).WithArgs("owner", "install", "version").WillReturnRows(sqlmock.NewRows([]string{"tools_json"}).AddRow(driftRaw))
	mock.ExpectRollback()
	if _, err := store.PinTools(context.Background(), "owner", "install", "version", 7, tools); err != ext.ErrConflict {
		t.Fatalf("drift = %v, want conflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDatabaseStorePinToolsNotFoundVersion(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter())
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT revision,state,active_version_id FROM p2p_agent_extensions")).WithArgs("owner", "install").WillReturnRows(sqlmock.NewRows([]string{"revision", "state", "active_version_id"}).AddRow(7, "installed", "version"))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT tools_json FROM p2p_agent_extension_versions")).WithArgs("owner", "install", "version").WillReturnError(sql.ErrNoRows)
	mock.ExpectRollback()
	if _, err := store.PinTools(context.Background(), "owner", "install", "version", 7, sqlPinnedTools()); err != ext.ErrNotFound {
		t.Fatalf("missing version = %v, want not found", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
