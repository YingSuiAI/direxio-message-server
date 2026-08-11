package p2p

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentartifact"
)

const testArtifactID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

type artifactDownloadSourceStub struct {
	download agentartifact.Download
	calls    int
}

func (stub *artifactDownloadSourceStub) DownloadTeamArtifact(
	context.Context,
	string,
) (agentartifact.Download, error) {
	stub.calls++
	return stub.download, nil
}

func TestAgentArtifactDownloadRequiresOwnerAndReturnsExactBytes(t *testing.T) {
	t.Parallel()
	content := []byte("complete presentation bytes")
	digest := sha256.Sum256(content)
	download, err := agentartifact.NewDownload(
		testArtifactID,
		"executive-summary.pptx",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation",
		int64(len(content)),
		fmt.Sprintf("sha256:%x", digest),
		content,
	)
	if err != nil {
		t.Fatal(err)
	}
	source := &artifactDownloadSourceStub{download: download}
	service := NewService(Config{
		ServerName:          "example.com",
		AgentArtifactSource: source,
	})
	router := newP2PTestRouter(service)
	path := "/_p2p/agent-artifacts/" + testArtifactID

	unauthorized := httptest.NewRequest(http.MethodGet, path, nil)
	unauthorizedRecorder := httptest.NewRecorder()
	router.ServeHTTP(unauthorizedRecorder, unauthorized)
	if unauthorizedRecorder.Code != http.StatusUnauthorized || source.calls != 0 {
		t.Fatalf("unauthorized status=%d calls=%d", unauthorizedRecorder.Code, source.calls)
	}

	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("Authorization", "Bearer "+service.AccessToken())
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK ||
		recorder.Header().Get("Cache-Control") != "no-store" ||
		recorder.Header().Get("X-Artifact-SHA256") != download.SHA256 ||
		recorder.Header().Get("Content-Type") != download.MediaType ||
		recorder.Body.String() != string(content) || source.calls != 1 {
		t.Fatalf(
			"status=%d headers=%v body=%q calls=%d",
			recorder.Code,
			recorder.Header(),
			recorder.Body.String(),
			source.calls,
		)
	}
}
