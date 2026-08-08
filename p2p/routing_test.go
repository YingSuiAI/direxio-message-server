package p2p

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rootinternal "github.com/YingSuiAI/dirextalk-message-server/internal"
	"github.com/YingSuiAI/dirextalk-message-server/internal/httputil"
	"github.com/gorilla/mux"
)

func TestHTTPProductActionAllowsOwnerFallbackAfterLogin(t *testing.T) {
	service := NewService(Config{
		ServerName: "example.com",
	})
	router := newP2PTestRouter(service)

	reqBody := map[string]any{
		"action": "profile.get",
		"params": map[string]any{},
	}
	req := jsonRequest(t, "/_p2p/command", reqBody)
	req.Header.Set("Authorization", "Bearer "+service.AccessToken())
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || got["user_id"] != "@owner:example.com" {
		t.Fatalf("expected owner HTTP fallback to succeed, got %d body=%#v", rec.Code, got)
	}
}

func TestAgentMatrixSessionCreateAllowsAgentToken(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	router := newP2PTestRouter(service)
	agentToken := service.AgentToken()

	if !service.Authorize(agentToken, "agent.matrix_session.create") {
		t.Fatal("expected agent token to create an agent Matrix session")
	}

	req := jsonRequest(t, "/_p2p/command", map[string]any{
		"action": "agent.matrix_session.create",
		"params": map[string]any{"device_id": "DIREXTALK_AGENT_GATEWAY"},
	})
	req.Header.Set("Authorization", "Bearer "+agentToken)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected agent Matrix session create to succeed, got %d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["user_id"] != "@agent:example.com" || got["device_id"] != "DIREXTALK_AGENT_GATEWAY" {
		t.Fatalf("expected local agent Matrix session metadata, got %#v", got)
	}
}

func TestProtectedHTTPRetainedActionRejectsMissingBearer(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	router := newP2PTestRouter(service)

	req := jsonRequest(t, "/_p2p/command", map[string]any{
		"action": "portal.password",
		"params": map[string]any{},
	})
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestEventsEndpointIsRemoved(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	router := newP2PTestRouter(service)

	req := httptest.NewRequest(http.MethodGet, "/_p2p/events?since=0", nil)
	req.Header.Set("Authorization", "Bearer "+service.AccessToken())
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected removed events endpoint to return 404, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAutoHomeserverResponseUsesRequestHostAndBootstrapTokenAlias(t *testing.T) {
	service := NewService(Config{
		ServerName: "example.com",
		Homeserver: "http://auto",
	})
	router := newP2PTestRouter(service)

	req := jsonRequest(t, "/_p2p/query", map[string]any{
		"action": "portal.bootstrap",
		"params": map[string]any{"token": service.password},
	})
	req.Host = "10.0.2.2:18008"
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("auth expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["homeserver"] != "http://10.0.2.2:18008" {
		t.Fatalf("expected request host homeserver, got %#v", got["homeserver"])
	}
}

func TestAutoHomeserverResponseUsesForwardedHost(t *testing.T) {
	service := NewService(Config{
		ServerName: "example.com",
		Homeserver: "https://auto",
	})
	router := newP2PTestRouter(service)

	req := jsonRequest(t, "/_p2p/query", map[string]any{
		"action": "portal.auth",
		"params": map[string]any{"password": service.password},
	})
	req.Host = "127.0.0.1:18008"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "portal.example.test")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("auth expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["homeserver"] != "https://portal.example.test" {
		t.Fatalf("expected forwarded homeserver, got %#v", got["homeserver"])
	}
}

func TestPortalOwnerWellKnownIsPublic(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	router := mux.NewRouter()
	Register(router.PathPrefix(PathPrefix).Subrouter(), service)
	RegisterWellKnown(router.PathPrefix("/.well-known/portal/").Subrouter(), service)

	mustHandle[ownerProfile](t, service, "profile.update", map[string]any{
		"display_name": "Alice",
		"avatar_url":   "mxc://example.com/avatar",
	})

	req := httptest.NewRequest(http.MethodGet, "/.well-known/portal/owner.json", nil)
	req.Header.Set("Origin", "http://127.0.0.1:3001")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "http://127.0.0.1:3001" {
		t.Fatalf("expected CORS origin echo, got %q", rec.Header().Get("Access-Control-Allow-Origin"))
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["matrix_user_id"] != "@owner:example.com" || got["display_name"] != "Alice" || got["avatar_url"] != "mxc://example.com/avatar" {
		t.Fatalf("unexpected owner well-known response %#v", got)
	}
}

func TestPublicChannelActionsDoNotRequireBearer(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	router := newP2PTestRouter(service)
	channelRaw := mustHandle[channel](t, service, "channels.create", map[string]any{
		"channel_id":  "public-news",
		"room_id":     "!public-news:example.com",
		"name":        "Public News",
		"visibility":  "public",
		"join_policy": "approval",
	})
	roomID := channelRaw.RoomID
	publicPost := mustHandle[channelPostRecord](t, service, "channels.posts.create", map[string]any{
		"channel_id": channelRaw.ChannelID,
		"body":       "public update",
		"visibility": "public",
	})
	mustHandle[channelPostRecord](t, service, "channels.posts.create", map[string]any{
		"channel_id": channelRaw.ChannelID,
		"body":       "members only",
		"visibility": "private",
	})

	detailReq := jsonRequest(t, "/_p2p/query", map[string]any{
		"action": "channels.public.get",
		"params": map[string]any{"room_id": roomID},
	})
	detailRec := httptest.NewRecorder()
	router.ServeHTTP(detailRec, detailReq)
	if detailRec.Code != http.StatusOK {
		t.Fatalf("expected public channel detail without bearer, got %d body=%s", detailRec.Code, detailRec.Body.String())
	}
	var detail map[string]any
	if err := json.Unmarshal(detailRec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail["channel_id"] != "public-news" {
		t.Fatalf("expected public channel detail, got %#v", detail)
	}

	postsReq := jsonRequest(t, "/_p2p/query", map[string]any{
		"action": "channels.public.posts.list",
		"params": map[string]any{"room_id": roomID},
	})
	postsRec := httptest.NewRecorder()
	router.ServeHTTP(postsRec, postsReq)
	if postsRec.Code != http.StatusOK {
		t.Fatalf("expected public posts without bearer, got %d body=%s", postsRec.Code, postsRec.Body.String())
	}
	var publicPosts map[string]any
	if err := json.Unmarshal(postsRec.Body.Bytes(), &publicPosts); err != nil {
		t.Fatal(err)
	}
	posts, ok := publicPosts["posts"].([]any)
	if !ok || len(posts) != 1 || posts[0].(map[string]any)["post_id"] != publicPost.PostID ||
		posts[0].(map[string]any)["visibility"] != "public" {
		t.Fatalf("expected only public posts, got %#v", publicPosts)
	}

	joinReq := jsonRequest(t, "/_p2p/command", map[string]any{
		"action": "channels.public.join_request",
		"params": map[string]any{"room_id": roomID, "user_mxid": "@guest:remote.example"},
	})
	joinRec := httptest.NewRecorder()
	router.ServeHTTP(joinRec, joinReq)
	if joinRec.Code != http.StatusOK {
		t.Fatalf("expected public join request without bearer, got %d body=%s", joinRec.Code, joinRec.Body.String())
	}
	var joined map[string]any
	if err := json.Unmarshal(joinRec.Body.Bytes(), &joined); err != nil {
		t.Fatal(err)
	}
	if joined["status"] != "pending" {
		t.Fatalf("expected pending join request, got %#v", joined)
	}

	userChannelsReq := jsonRequest(t, "/_p2p/query", map[string]any{
		"action": "users.public_channels",
		"params": map[string]any{"user_mxid": "@owner:example.com"},
	})
	userChannelsRec := httptest.NewRecorder()
	router.ServeHTTP(userChannelsRec, userChannelsReq)
	if userChannelsRec.Code != http.StatusOK {
		t.Fatalf("expected public user channels without bearer, got %d body=%s", userChannelsRec.Code, userChannelsRec.Body.String())
	}
	var userChannels map[string]any
	if err := json.Unmarshal(userChannelsRec.Body.Bytes(), &userChannels); err != nil {
		t.Fatal(err)
	}
	channels, ok := userChannels["channels"].([]any)
	if !ok || len(channels) != 1 || channels[0].(map[string]any)["room_id"] != roomID {
		t.Fatalf("expected owner public channel list, got %#v", userChannels)
	}
}

func TestRemovedMCPBodyActionIsUnknownOverHTTP(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	router := newP2PTestRouter(service)

	mcpReq := jsonRequest(t, "/_p2p/query", map[string]any{
		"action": "mcp.rooms.search",
		"params": map[string]any{"q": "none"},
	})
	mcpReq.Header.Set("Authorization", "Bearer "+service.AgentToken())
	mcpRec := httptest.NewRecorder()
	router.ServeHTTP(mcpRec, mcpReq)
	if mcpRec.Code != http.StatusBadRequest || !strings.Contains(mcpRec.Body.String(), "unknown action") {
		t.Fatalf("expected removed HTTP MCP body action to be unknown, got %d body=%s", mcpRec.Code, mcpRec.Body.String())
	}
}

func TestSyncBootstrapOmitsDeprecatedAgentOnline(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})

	bootstrap := mustHandle[map[string]any](t, service, "sync.bootstrap", nil)
	if _, ok := bootstrap["agent_online"]; ok {
		t.Fatalf("expected sync.bootstrap to omit deprecated agent_online, got %#v", bootstrap)
	}
	if _, ok := bootstrap["agent_presence"]; ok {
		t.Fatalf("expected sync.bootstrap to omit deprecated agent_presence, got %#v", bootstrap["agent_presence"])
	}
}

func TestHealthReportsAdditiveBuildInfo(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/_p2p/health", nil)

	newP2PTestRouter(nil).ServeHTTP(rec, req)

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || got["status"] != "ok" {
		t.Fatalf("health contract changed: status=%d body=%#v", rec.Code, got)
	}
	if got["version"] != rootinternal.VersionString() || got["schema_version"] != float64(rootinternal.SchemaVersion) || got["schema_compat_version"] != float64(rootinternal.SchemaCompatVersion) {
		t.Fatalf("health build info missing: %#v", got)
	}
}

func TestHealthDoesNotConflateNativeAgentReadiness(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/_p2p/health", nil)

	newP2PTestRouter(service).ServeHTTP(rec, req)

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || got["status"] != "ok" {
		t.Fatalf("Message Server health must remain independent of Agent readiness: status=%d body=%#v", rec.Code, got)
	}
}

func TestProductionMountPreservesMethodAndPathErrors(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	routers := httputil.NewRouters()
	Register(routers.P2P, service)
	RegisterMCP(routers.MCP, service)
	RegisterWellKnown(routers.PortalWellKnown, service)

	external := mux.NewRouter().SkipClean(true).UseEncodedPath()
	external.Handle(httputil.MCPPath, routers.MCP)
	external.PathPrefix(httputil.P2PPathPrefix).Handler(routers.P2P)
	external.PathPrefix(httputil.PublicPortalWellKnownPrefix).Handler(routers.PortalWellKnown)
	external.NotFoundHandler = httputil.NotFoundCORSHandler
	external.MethodNotAllowedHandler = httputil.NotAllowedHandler

	for _, test := range []struct {
		name   string
		method string
		path   string
		status int
	}{
		{name: "P2P HEAD", method: http.MethodHead, path: "/_p2p/health", status: http.StatusMethodNotAllowed},
		{name: "P2P PUT", method: http.MethodPut, path: "/_p2p/query", status: http.StatusMethodNotAllowed},
		{name: "MCP HEAD", method: http.MethodHead, path: "/mcp", status: http.StatusMethodNotAllowed},
		{name: "trailing slash", method: http.MethodGet, path: "/_p2p/health/", status: http.StatusNotFound},
		{name: "encoded path", method: http.MethodGet, path: "/_p2p/%68ealth", status: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(test.method, test.path, nil)
			rec := httptest.NewRecorder()
			external.ServeHTTP(rec, req)
			if rec.Code != test.status {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, test.status, rec.Body.String())
			}
			if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
				t.Fatalf("global CORS changed: %#v", rec.Header())
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body["errcode"] != "M_UNRECOGNIZED" || body["error"] != "Unrecognized request" {
				t.Fatalf("global route error changed: %#v", body)
			}
		})
	}
}
