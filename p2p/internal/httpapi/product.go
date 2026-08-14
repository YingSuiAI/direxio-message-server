package httpapi

import (
	"context"
	"net/http"
	"strings"

	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/serviceapi"
)

// ProductPort is the narrow root capability required by the ProductCore HTTP
// adapter. Authorize may return a context carrying an authenticated session.
type ProductPort interface {
	HasAction(action string) bool
	Authorize(ctx context.Context, token, action string) (context.Context, bool)
	Handle(ctx context.Context, action string, params map[string]any) (any, *actionbase.Error)
}

type productEnvelope struct {
	Action string         `json:"action"`
	Params map[string]any `json:"params"`
}

// ProductHandler handles both ProductCore query and command envelopes. Route
// method selection remains owned by the outer router.
func ProductHandler(port ProductPort) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		SetCORSHeaders(w, r)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		var req productEnvelope
		if err := decodeJSONBody(w, r, &req); err != nil {
			WriteError(w, actionbase.BadRequest("invalid json"))
			return
		}
		if req.Params == nil {
			req.Params = map[string]any{}
		}
		action := strings.TrimSpace(req.Action)
		if action == "" {
			WriteError(w, actionbase.BadRequest("action is required"))
			return
		}
		if _, ok := serviceapi.ActionSpecFor(action); !ok {
			WriteError(w, actionbase.BadRequest("unknown action"))
			return
		}
		if port == nil || !port.HasAction(action) {
			WriteError(w, actionbase.BadRequest("unknown action"))
			return
		}
		if !serviceapi.HTTPAction(action) {
			WriteError(w, actionbase.StatusError(http.StatusBadRequest, "action is not available over Product HTTP"))
			return
		}

		token := BearerToken(r.Header.Get("Authorization"))
		ctx := r.Context()
		if !serviceapi.PublicAction(action) {
			var authorized bool
			if port != nil {
				ctx, authorized = port.Authorize(ctx, token, action)
			}
			if !authorized {
				WriteError(w, actionbase.StatusError(http.StatusUnauthorized, "M_UNKNOWN_TOKEN"))
				return
			}
		}

		response, err := port.Handle(ctx, action, req.Params)
		if err != nil {
			WriteError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, ResponseForRequest(r, response))
	}
}

// HealthHandler exposes the additive build and schema metadata contract. The
// optional readiness callback lets deployments fail closed while a required
// external capability peer/catalog is unavailable without changing existing
// callers that only need build metadata.
func HealthHandler(buildInfo BuildInfoProvider, readiness ...func() error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		SetCORSHeaders(w, r)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		build := currentBuildInfo(buildInfo)
		status := http.StatusOK
		statusValue := "ok"
		if len(readiness) > 0 && readiness[0] != nil {
			if err := readiness[0](); err != nil {
				status = http.StatusServiceUnavailable
				statusValue = "not_ready"
			}
		}
		WriteJSON(w, status, map[string]any{
			"status":                statusValue,
			"version":               build.Version,
			"commit":                build.Commit,
			"build_time":            build.BuildTime,
			"schema_version":        build.SchemaVersion,
			"schema_compat_version": build.SchemaCompatVersion,
		})
	}
}

// WellKnownHandler exposes the public owner profile discovery payload.
func WellKnownHandler(wellKnown func() any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		SetCORSHeaders(w, r)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		WriteJSON(w, http.StatusOK, wellKnown())
	}
}
