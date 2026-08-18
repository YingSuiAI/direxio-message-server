package test

import "testing"

func TestDatabaseNameIsStableOnlyWithinOneTestProcess(t *testing.T) {
	first := testDatabaseName("/workspace/package", "TestFeature/postgres", 100)
	if again := testDatabaseName("/workspace/package", "TestFeature/postgres", 100); again != first {
		t.Fatalf("same test process database name = %q, want %q", again, first)
	}
	for name, got := range map[string]string{
		"process": testDatabaseName("/workspace/package", "TestFeature/postgres", 101),
		"package": testDatabaseName("/workspace/other", "TestFeature/postgres", 100),
		"test":    testDatabaseName("/workspace/package", "TestOther/postgres", 100),
	} {
		if got == first {
			t.Errorf("%s change reused database name %q", name, got)
		}
	}
}

func TestWithAllDatabasesRunsOnlyPostgres(t *testing.T) {
	WithAllDatabases(t, func(t *testing.T, dbType DBType) {
		if dbType != DBTypePostgres {
			t.Fatalf("WithAllDatabases returned db type %d, want PostgreSQL only", dbType)
		}
	})
}
