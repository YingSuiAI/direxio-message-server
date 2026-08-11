package agentgrpc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"
)

func TestRunnerDownloadsAndReverifiesTeamArtifact(t *testing.T) {
	t.Parallel()
	server := startRuntimeServer(t)
	content := bytes.Repeat([]byte("presentation"), 7000)
	digest := sha256.Sum256(content)
	server.team.mu.Lock()
	artifact := server.team.execution.GetArtifacts()[0]
	artifact.Name = "executive-summary.pptx"
	artifact.Kind = "file"
	artifact.MediaType = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	artifact.SizeBytes = int64(len(content))
	artifact.Sha256 = fmt.Sprintf("sha256:%x", digest)
	server.team.artifactContent = append([]byte(nil), content...)
	server.team.mu.Unlock()
	runner := newTestRunner(t, server, Config{UnaryTimeout: time.Second})
	download, err := runner.DownloadTeamArtifact(
		context.Background(),
		artifact.GetArtifactId(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if download.Name != "executive-summary.pptx" ||
		download.MediaType != artifact.GetMediaType() ||
		download.SHA256 != artifact.GetSha256() ||
		!bytes.Equal(download.Content, content) {
		t.Fatalf("download=%#v", download)
	}
}
