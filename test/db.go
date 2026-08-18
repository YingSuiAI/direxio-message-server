// Copyright 2024 New Vector Ltd.
// Copyright 2022 The Matrix.org Foundation C.I.C.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package test

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/lib/pq"
)

type DBType int

var DBTypePostgres DBType = 1

var Quiet = false
var Required = os.Getenv("DENDRITE_TEST_SKIP_NODB") == ""

func fatalError(t *testing.T, format string, args ...interface{}) {
	if Required {
		t.Fatalf(format, args...)
	} else {
		t.Skipf(format, args...)
	}
}

func createRemoteDB(t *testing.T, dbName, user, connStr, postgresDB string) {
	db, err := sql.Open("postgres", connStr+" dbname="+postgresDB)
	if err != nil {
		fatalError(t, "failed to open postgres conn with connstr=%s : %s", connStr, err)
	}
	if err = db.Ping(); err != nil {
		fatalError(t, "failed to open postgres conn with connstr=%s : %s", connStr, err)
	}
	_, err = db.Exec(fmt.Sprintf(`CREATE DATABASE %s;`, dbName))
	if err != nil {
		pqErr, ok := err.(*pq.Error)
		if !ok {
			t.Fatalf("failed to CREATE DATABASE: %s", err)
		}
		// we ignore duplicate database error as we expect this
		if pqErr.Code != "42P04" {
			t.Fatalf("failed to CREATE DATABASE with code=%s msg=%s", pqErr.Code, pqErr.Message)
		}
	}
	_, err = db.Exec(fmt.Sprintf(`GRANT ALL PRIVILEGES ON DATABASE %s TO %s`, dbName, user))
	if err != nil {
		t.Fatalf("failed to GRANT: %s", err)
	}
	_ = db.Close()
}

// Prepare a postgres connection string for testing.
// Returns the connection string to use and a close function which must be called when the test finishes.
// Calling this function twice will return the same database, which will have data from previous tests
// unless close() is called.
func PrepareDBConnectionString(t *testing.T, dbType DBType) (connStr string, close func()) {
	// Required vars: user and db
	// We'll try to infer from the local env if they are missing
	user := os.Getenv("POSTGRES_USER")
	if user == "" {
		user = "postgres"
	}
	connStr = fmt.Sprintf(
		"user=%s sslmode=disable",
		user,
	)
	// optional vars, used in CI
	password := os.Getenv("POSTGRES_PASSWORD")
	if password == "" {
		password = "123789"
	}
	connStr += fmt.Sprintf(" password=%s", password)
	host := os.Getenv("POSTGRES_HOST")
	if host == "" {
		host = "localhost"
	}
	connStr += fmt.Sprintf(" host=%s", host)
	port := os.Getenv("POSTGRES_PORT")
	if port == "" {
		port = "5432"
	}
	connStr += fmt.Sprintf(" port=%s", port)

	// superuser database
	postgresDB := os.Getenv("POSTGRES_DB")
	if postgresDB == "" {
		postgresDB = "postgres"
	}
	adminConnStr := connStr + fmt.Sprintf(" dbname=%s", postgresDB)
	// Use a unique database per test process so parallel package tests and
	// overlapping full-suite invocations cannot drop or reset each other's state
	// while still cleaning up after themselves. Calls from the same test process
	// retain the documented shared-database behavior.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("cannot get working directory: %s", err)
	}
	dbName := testDatabaseName(wd, t.Name(), os.Getpid())
	createRemoteDB(t, dbName, user, connStr, postgresDB)
	connStr += fmt.Sprintf(" dbname=%s", dbName)

	return connStr, func() {
		// Drop the isolated database after the test finishes so local PostgreSQL
		// instances do not accumulate stale DBs.
		db, err := sql.Open("postgres", adminConnStr)
		if err != nil {
			t.Fatalf("failed to connect to postgres admin db '%s': %s", adminConnStr, err)
		}
		defer db.Close()
		_, err = db.Exec(`
			SELECT pg_terminate_backend(pid)
			FROM pg_stat_activity
			WHERE datname = $1 AND pid <> pg_backend_pid()
		`, dbName)
		if err != nil {
			t.Fatalf("failed to terminate connections to postgres db '%s': %s", dbName, err)
		}
		if _, err = db.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS %s", pq.QuoteIdentifier(dbName))); err != nil {
			t.Fatalf("failed to drop postgres db '%s': %s", dbName, err)
		}
	}
}

func testDatabaseName(workingDirectory, testName string, processID int) string {
	hash := sha256.Sum256([]byte(workingDirectory + "/" + testName + "/" + strconv.Itoa(processID)))
	return fmt.Sprintf("dendrite_test_%s", hex.EncodeToString(hash[:16]))
}

// Creates subtests with each known DBType
func WithAllDatabases(t *testing.T, testFn func(t *testing.T, db DBType)) {
	dbs := map[string]DBType{
		"postgres": DBTypePostgres,
	}
	for dbName, dbType := range dbs {
		dbt := dbType
		t.Run(dbName, func(tt *testing.T) {
			tt.Parallel()
			testFn(tt, dbt)
		})
	}
}
