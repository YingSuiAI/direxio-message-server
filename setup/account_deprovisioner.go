package setup

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/setup/process"
	"github.com/sirupsen/logrus"
)

const accountDeprovisionShutdownDelay = 2 * time.Second

type accountDeprovisioner struct {
	processCtx *process.ProcessContext
	cm         *sqlutil.Connections
	databases  []config.DatabaseOptions
}

func newAccountDeprovisioner(processCtx *process.ProcessContext, cfg *config.Dendrite, cm *sqlutil.Connections) *accountDeprovisioner {
	return &accountDeprovisioner{
		processCtx: processCtx,
		cm:         cm,
		databases:  accountDeprovisionDatabases(cfg),
	}
}

func (d *accountDeprovisioner) DeprovisionAccount(ctx context.Context) error {
	if d == nil || d.cm == nil {
		return fmt.Errorf("database connection manager unavailable")
	}
	for _, dbOptions := range d.databases {
		if strings.TrimSpace(string(dbOptions.ConnectionString)) == "" {
			continue
		}
		options := dbOptions
		db, writer, err := d.cm.Connection(&options)
		if err != nil {
			return fmt.Errorf("open database %q: %w", options.ConnectionString, err)
		}
		if options.ConnectionString.IsSQLite() {
			return fmt.Errorf("sqlite database %q is not supported", options.ConnectionString)
		}
		if err := writer.Do(db, nil, func(txn *sql.Tx) error {
			return resetPostgresDatabase(ctx, txn)
		}); err != nil {
			return fmt.Errorf("reset postgres database %q: %w", options.ConnectionString, err)
		}
	}
	d.scheduleShutdown()
	return nil
}

func (d *accountDeprovisioner) scheduleShutdown() {
	if d.processCtx == nil {
		return
	}
	go func() {
		time.Sleep(accountDeprovisionShutdownDelay)
		logrus.Warn("Dirextalk account deprovision complete; shutting down message server")
		d.processCtx.ShutdownDendrite()
	}()
}

func accountDeprovisionDatabases(cfg *config.Dendrite) []config.DatabaseOptions {
	if cfg == nil {
		return nil
	}
	if cfg.Global.DatabaseOptions.ConnectionString != "" {
		return []config.DatabaseOptions{cfg.Global.DatabaseOptions}
	}
	candidates := []config.DatabaseOptions{
		cfg.RoomServer.Database,
		cfg.SyncAPI.Database,
		cfg.UserAPI.AccountDatabase,
		cfg.KeyServer.Database,
		cfg.FederationAPI.Database,
		cfg.MediaAPI.Database,
		cfg.RelayAPI.Database,
		cfg.MSCs.Database,
	}
	seen := map[config.DataSource]struct{}{}
	result := make([]config.DatabaseOptions, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.ConnectionString == "" {
			continue
		}
		if _, ok := seen[candidate.ConnectionString]; ok {
			continue
		}
		seen[candidate.ConnectionString] = struct{}{}
		result = append(result, candidate)
	}
	return result
}

func resetPostgresDatabase(ctx context.Context, txn *sql.Tx) error {
	if txn == nil {
		return fmt.Errorf("postgres reset requires transaction")
	}
	var tables sql.NullString
	if err := txn.QueryRowContext(ctx, `
SELECT string_agg(format('%I.%I', schemaname, tablename), ', ')
FROM pg_tables
WHERE schemaname = 'public'

-- Agent owns its Core v1 tables in the same PostgreSQL database only when a
-- deployment deliberately shares the physical database. Account deletion is
-- a message-server operation and must never truncate those Agent-owned tables;
-- the Agent volume/service is destroyed separately by the deployment runbook.
-- The shared schema ledger is infrastructure metadata rather than account
-- data. Keeping it is required because the tables remain in place and the
-- supervised message-server process starts again after deprovisioning.
AND tablename <> 'db_migrations'
AND tablename NOT LIKE 'agent\_%' ESCAPE '\'
AND tablename NOT LIKE 'core\_%' ESCAPE '\'
`).Scan(&tables); err != nil {
		return err
	}
	if !tables.Valid || strings.TrimSpace(tables.String) == "" {
		return nil
	}
	_, err := txn.ExecContext(ctx, "TRUNCATE TABLE "+tables.String+" RESTART IDENTITY CASCADE")
	return err
}
