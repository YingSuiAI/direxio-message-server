package nativeagent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func runtimePATH(dataDir string) string {
	binDir := filepath.Join(dataDir, "runtime", "bin")
	if current := os.Getenv("PATH"); current != "" {
		return binDir + string(os.PathListSeparator) + current
	}
	return binDir
}

func runtimeEnv(dataDir string) []string {
	env := []string{"PATH=" + runtimePATH(dataDir)}
	for _, key := range runtimeEnvPassthroughKeys() {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func runtimeEnvPassthroughKeys() []string {
	if runtime.GOOS == "windows" {
		return []string{"SystemDrive", "SystemRoot", "WINDIR", "ComSpec", "PATHEXT", "TEMP", "TMP", "USERPROFILE"}
	}
	return []string{"HOME", "TMPDIR", "TEMP", "TMP"}
}
