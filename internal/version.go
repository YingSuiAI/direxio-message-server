package internal

import (
	"runtime/debug"
	"strings"
)

const (
	SchemaVersion       = 2
	SchemaCompatVersion = 1
)

// These values are overridden for release builds with -ldflags -X.
var (
	version   = "v1.1.69"
	commit    string
	buildTime string
)

type BuildInfo struct {
	Version             string `json:"version"`
	Commit              string `json:"commit,omitempty"`
	BuildTime           string `json:"build_time,omitempty"`
	SchemaVersion       int    `json:"schema_version"`
	SchemaCompatVersion int    `json:"schema_compat_version"`
}

func CurrentBuildInfo() BuildInfo {
	resolvedCommit := strings.TrimSpace(commit)
	if resolvedCommit == "" {
		resolvedCommit = vcsRevision()
	}
	return BuildInfo{
		Version:             strings.TrimSpace(version),
		Commit:              resolvedCommit,
		BuildTime:           strings.TrimSpace(buildTime),
		SchemaVersion:       SchemaVersion,
		SchemaCompatVersion: SchemaCompatVersion,
	}
}

func VersionString() string {
	return CurrentBuildInfo().Version
}

func vcsRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			return strings.TrimSpace(setting.Value)
		}
	}
	return ""
}
