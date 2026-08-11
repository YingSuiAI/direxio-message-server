package p2p

import (
	"errors"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentartifact"
	httpapi "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/httpapi"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/serviceapi"
	"github.com/gorilla/mux"
)

func agentArtifactDownloadHandler(service *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		httpapi.SetCORSHeaders(w, r)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if service == nil {
			http.Error(w, "artifact service unavailable", http.StatusServiceUnavailable)
			return
		}
		token := httpapi.BearerToken(r.Header.Get("Authorization"))
		if _, authorized := service.authorizeProductAction(
			token,
			serviceapi.AgentTeamExecutionGetAction,
		); !authorized {
			http.Error(w, "M_UNKNOWN_TOKEN", http.StatusUnauthorized)
			return
		}
		if service.agentArtifactSource == nil {
			http.Error(w, "artifact service unavailable", http.StatusServiceUnavailable)
			return
		}
		download, err := service.agentArtifactSource.DownloadTeamArtifact(
			r.Context(),
			mux.Vars(r)["artifact_id"],
		)
		if err != nil {
			switch {
			case errors.Is(err, agentartifact.ErrNotFound):
				http.Error(w, "artifact not found", http.StatusNotFound)
			case errors.Is(err, agentartifact.ErrInvalid):
				http.Error(w, "invalid artifact", http.StatusBadRequest)
			default:
				http.Error(w, "artifact service unavailable", http.StatusServiceUnavailable)
			}
			return
		}
		if download.Validate() != nil {
			http.Error(w, "artifact service unavailable", http.StatusServiceUnavailable)
			return
		}
		disposition := mime.FormatMediaType(
			"attachment",
			map[string]string{"filename": download.Name},
		)
		w.Header().Set("Content-Type", download.MediaType)
		w.Header().Set("Content-Length", strconv.FormatInt(download.SizeBytes, 10))
		w.Header().Set("Content-Disposition", disposition)
		w.Header().Set("X-Artifact-SHA256", download.SHA256)
		w.Header().Set("ETag", `"`+strings.TrimPrefix(download.SHA256, "sha256:")+`"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(download.Content)
	}
}
