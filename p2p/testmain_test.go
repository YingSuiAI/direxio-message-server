package p2p

import (
	"fmt"
	"os"
	"testing"
)

// TestMain keeps database-backed service tests from creating the production
// Native Agent key under /var. Production configuration is intentionally left
// unchanged; this only supplies an isolated directory for this test process.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "dirextalk-p2p-native-agent-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create p2p native agent test directory: %v\n", err)
		os.Exit(1)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		fmt.Fprintf(os.Stderr, "secure p2p native agent test directory: %v\n", err)
		os.Exit(1)
	}

	previous, wasSet := os.LookupEnv("P2P_NATIVE_AGENT_DATA_DIR")
	if err := os.Setenv("P2P_NATIVE_AGENT_DATA_DIR", dir); err != nil {
		_ = os.RemoveAll(dir)
		fmt.Fprintf(os.Stderr, "set p2p native agent test directory: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()
	if wasSet {
		_ = os.Setenv("P2P_NATIVE_AGENT_DATA_DIR", previous)
	} else {
		_ = os.Unsetenv("P2P_NATIVE_AGENT_DATA_DIR")
	}
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintf(os.Stderr, "remove p2p native agent test directory: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
