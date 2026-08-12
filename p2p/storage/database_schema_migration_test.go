package storage

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/test"
)

func TestDatabaseStoreRejectsSQLiteConnectionString(t *testing.T) {
	ctx := context.Background()
	dbOpts := config.DatabaseOptions{ConnectionString: "file::memory:?cache=shared"}
	_, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err == nil || !strings.Contains(err.Error(), "SQLite") {
		t.Fatalf("expected SQLite connection string to be rejected, got %v", err)
	}
}

func TestFreshProductBaselineIsSingleVersionAndReopenIdempotent(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}

	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("second baseline run: %v", err)
	}
	wantMigrations := []string{"p2p: drop retired completion result message v2", "p2p: fresh ProductCore baseline v1"}
	assertP2PMigrationSet(t, store.DB(), wantMigrations)

	for _, table := range []string{
		"p2p_portal", "p2p_read_markers", "p2p_channels", "p2p_channel_posts", "p2p_channel_post_settings", "p2p_channel_comments",
		"p2p_contacts", "p2p_groups", "p2p_calls", "p2p_favorites", "p2p_follows", "p2p_reactions",
		"p2p_members", "p2p_events", "p2p_channel_invite_grants", "p2p_conversations", "p2p_operations",
		"p2p_plugins", "p2p_plugin_jobs", "p2p_plugin_secrets", "p2p_reports", "p2p_blocks",
		"p2p_capability_operations", "p2p_capability_operation_events", "p2p_capability_matrix_prepared_events",
		"p2p_agent_execution_completion_receipts",
	} {
		assertRelationPresent(t, store.DB(), table)
	}

	for _, forbidden := range []string{
		"p2p_legacy_agent_invocations", "p2p_native_agent_turns", "p2p_native_agent_turn_events",
		"p2p_agent_core_turns", "p2p_native_agent_conversations", "p2p_native_agent_messages",
		"p2p_native_agent_memory_records", "p2p_native_agent_memory_turns", "p2p_native_agent_memory_embeddings",
		"p2p_native_agent_knowledge_sources", "p2p_native_agent_knowledge_uploads", "p2p_agent_model_profiles",
		"p2p_agent_model_profile_credentials", "p2p_agent_model_profile_revisions", "p2p_agent_model_profile_defaults",
		"p2p_agent_model_profile_syncs", "p2p_agent_model_profile_deletes", "p2p_agent_schedules",
		"p2p_agent_schedule_runs", "p2p_agent_schedule_mutations", "p2p_agent_schedule_confirmations",
		"p2p_agent_secrets", "p2p_agent_secret_key_usage", "p2p_agent_secret_rotations", "agent_tasks",
		"agent_confirmations", "core_aws_credentials", "core_aws_credential_current", "core_aws_replays",
		"core_execution_projects", "core_execution_source_artifacts", "core_execution_runs", "core_execution_run_stages",
		"core_execution_run_revisions", "core_execution_deployments", "core_execution_deployment_events",
		"core_execution_secrets", "core_execution_secret_parameter_intents", "core_execution_reconciliation_resolutions",
	} {
		assertRelationAbsent(t, store.DB(), forbidden)
	}
	for _, column := range []string{"visibility", "comments_enabled", "settings_updated"} {
		var present bool
		if err := store.DB().QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='p2p_channel_posts' AND column_name=$1)`, column).Scan(&present); err != nil {
			t.Fatal(err)
		}
		if !present {
			t.Fatalf("fresh ProductCore baseline missing p2p_channel_posts.%s", column)
		}
	}
	assertRelationPresent(t, store.DB(), "p2p_channel_posts_channel_visibility_idx")
	assertColumnAbsent(t, store.DB(), "p2p_agent_execution_completion_receipts", "result_message_id")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatalf("reopen fresh baseline: %v", err)
	}
	defer reopened.Close()
	assertP2PMigrationSet(t, reopened.DB(), wantMigrations)
}

func TestCompletionResultMessageForwardMigrationDropsRetiredColumn(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}

	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `ALTER TABLE p2p_agent_execution_completion_receipts ADD COLUMN result_message_id UUID NOT NULL DEFAULT '00000000-0000-4000-8000-000000000005'::uuid`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `DELETE FROM db_migrations WHERE version='p2p: drop retired completion result message v2'`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatalf("forward migration: %v", err)
	}
	defer reopened.Close()
	assertColumnAbsent(t, reopened.DB(), "p2p_agent_execution_completion_receipts", "result_message_id")
}

func assertP2PMigrationSet(t *testing.T, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, want []string) {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `SELECT version FROM db_migrations WHERE version LIKE 'p2p:%' ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var versions []string
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(versions) != len(want) {
		t.Fatalf("P2P migration ledger=%v, want %v", versions, want)
	}
	for index := range want {
		if versions[index] != want[index] {
			t.Fatalf("P2P migration ledger=%v, want %v", versions, want)
		}
	}
}

func assertColumnAbsent(t *testing.T, db interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, table, column string) {
	t.Helper()
	var present bool
	if err := db.QueryRowContext(context.Background(), `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name=$1 AND column_name=$2)`, table, column).Scan(&present); err != nil {
		t.Fatal(err)
	}
	if present {
		t.Fatalf("retired column %s.%s is still present", table, column)
	}
}

func assertRelationPresent(t *testing.T, db interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, relation string) {
	t.Helper()
	var present bool
	if err := db.QueryRowContext(context.Background(), `SELECT to_regclass($1) IS NOT NULL`, relation).Scan(&present); err != nil {
		t.Fatal(err)
	}
	if !present {
		t.Fatalf("fresh ProductCore relation %s is missing", relation)
	}
}

func assertRelationAbsent(t *testing.T, db interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, relation string) {
	t.Helper()
	var present bool
	if err := db.QueryRowContext(context.Background(), `SELECT to_regclass($1) IS NOT NULL`, relation).Scan(&present); err != nil {
		t.Fatal(err)
	}
	if present {
		t.Fatalf("forbidden Agent/execution relation %s exists in message-server baseline", relation)
	}
}
