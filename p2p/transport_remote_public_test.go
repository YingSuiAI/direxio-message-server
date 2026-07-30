package p2p

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/productpolicy"
	operationsmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/operations"
	p2pstorage "github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

type armedMemberLookupStore struct {
	*p2pstorage.MemoryStore
	roomID  string
	userID  string
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (s *armedMemberLookupStore) LookupMember(ctx context.Context, roomID, userID string) (memberRecord, bool, error) {
	member, found, err := s.MemoryStore.LookupMember(ctx, roomID, userID)
	if roomID == s.roomID && userID == s.userID && s.armed.CompareAndSwap(true, false) {
		close(s.entered)
		<-s.release
	}
	return member, found, err
}

type remoteChannelJoinRequestOutcome struct {
	result  map[string]any
	handled bool
	err     *apiError
}

type observedMatrixJoinWorkflowStore struct {
	*p2pstorage.MemoryStore
	workflowClaims atomic.Int32
	secondClaim    chan struct{}
	secondOnce     sync.Once
}

func (s *observedMatrixJoinWorkflowStore) ClaimOperation(
	ctx context.Context,
	record operationsmodule.Record,
	owner string,
	leaseDurationMillis int64,
) (operationsmodule.Record, bool, error) {
	if record.Action == "_workflow.matrix_join" && s.workflowClaims.Add(1) >= 2 {
		s.secondOnce.Do(func() { close(s.secondClaim) })
	}
	return s.MemoryStore.ClaimOperation(ctx, record, owner, leaseDurationMillis)
}

type blockingMatrixJoinTransport struct {
	recordingTransport
	mu      sync.Mutex
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (t *blockingMatrixJoinTransport) JoinRoom(ctx context.Context, req JoinRoomRequest) (JoinRoomResult, error) {
	if t.calls.Add(1) == 1 {
		close(t.entered)
		select {
		case <-t.release:
		case <-ctx.Done():
			return JoinRoomResult{}, ctx.Err()
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.joins = append(t.joins, req.UserMXID+" in "+req.RoomIDOrAlias)
	t.joinRequests = append(t.joinRequests, req)
	t.roomMembers = append(t.roomMembers, memberRecord{
		RoomID: req.RoomIDOrAlias, UserID: req.UserMXID, DisplayName: req.DisplayName,
		AvatarURL: req.AvatarURL, Membership: "join", Role: "member",
	})
	return JoinRoomResult{RoomID: req.RoomIDOrAlias}, nil
}

func (t *blockingMatrixJoinTransport) ListRoomMembers(_ context.Context, roomID string) ([]memberRecord, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	members := make([]memberRecord, 0, len(t.roomMembers))
	for _, member := range t.roomMembers {
		if member.RoomID == "" {
			member.RoomID = roomID
		}
		if member.RoomID == roomID {
			members = append(members, member)
		}
	}
	return members, nil
}

func TestPublicChannelGetBackfillsRoomStateFromTransport(t *testing.T) {
	transport := &recordingTransport{
		roomChannel: channel{
			ChannelID:   "remote_ch",
			RoomID:      "!remote:example.com",
			Name:        "Remote Public",
			Visibility:  "public",
			JoinPolicy:  "open",
			ChannelType: "chat",
		},
	}
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)
	bootstrapService(t, service)

	got := mustHandle[channel](t, service, "channels.public.get", map[string]any{
		"room_id": "!remote:example.com",
	})
	if got.ChannelID != "remote_ch" || got.RoomID != "!remote:example.com" {
		t.Fatalf("expected public channel fetched from transport, got %#v", got)
	}
	channels := mustHandle[map[string]any](t, service, "channels.public.search", map[string]any{"q": "remote"})["channels"].([]channel)
	if len(channels) != 1 || channels[0].ChannelID != "remote_ch" {
		t.Fatalf("expected fetched channel cached for public search, got %#v", channels)
	}
}

func TestRemotePublicChannelGetUnavailableOwnerNodeReturnsBadGateway(t *testing.T) {
	service := NewServiceWithTransport(Config{
		ServerName:                     "dendrite-b:8448",
		RemoteNodeAllowPrivateBaseURLs: true,
	}, &recordingTransport{})
	bootstrapService(t, service)

	_, apiErr := service.Handle(context.Background(), "channels.public.get", map[string]any{
		"room_id":              "!remote:dendrite-a:8448",
		"remote_node_base_url": "http://127.0.0.1:9/_p2p",
	})
	if apiErr == nil || apiErr.Status != 502 {
		t.Fatalf("expected unavailable remote owner node to return 502, got %#v", apiErr)
	}
}

func TestRemotePublicChannelGetFetchesOwnerNodeByRoomID(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_p2p/query" {
			t.Fatalf("expected remote public query path, got %s", r.URL.Path)
		}
		var req envelope
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode remote request: %v", err)
		}
		if req.Action != "channels.public.get" || trimString(req.Params["room_id"]) != "!remote:remote.example" {
			t.Fatalf("unexpected remote request %#v", req)
		}
		_ = json.NewEncoder(w).Encode(channel{
			ChannelID:   "remote_ch",
			RoomID:      "!remote:remote.example",
			Name:        "Remote Public",
			Visibility:  "public",
			JoinPolicy:  "approval",
			ChannelType: "chat",
		})
	}))
	defer remote.Close()

	service := NewService(Config{
		ServerName:                     "local.example",
		RemoteNodeAllowPrivateBaseURLs: true,
	})
	bootstrapService(t, service)

	got := mustHandle[channel](t, service, "channels.public.get", map[string]any{
		"room_id":              "!remote:remote.example",
		"remote_node_base_url": remote.URL + "/_p2p",
	})
	if got.ChannelID != "remote_ch" || got.JoinPolicy != "approval" {
		t.Fatalf("expected remote public channel, got %#v", got)
	}

	search := mustHandle[map[string]any](t, service, "channels.public.search", map[string]any{
		"q":                    "!remote:remote.example",
		"remote_node_base_url": remote.URL + "/_p2p",
	})
	channels := search["channels"].([]channel)
	if len(channels) != 1 || channels[0].ChannelID != "remote_ch" {
		t.Fatalf("expected Matrix room id search to use remote public get, got %#v", search)
	}
}

func TestRemotePublicChannelPostsFetchesOnlyPublicOwnerNodePage(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_p2p/query" {
			t.Fatalf("expected remote public query path, got %s", r.URL.Path)
		}
		var req envelope
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode remote request: %v", err)
		}
		if req.Action != "channels.public.posts.list" ||
			trimString(req.Params["room_id"]) != "!posts:remote.example" ||
			trimString(req.Params["remote_node_base_url"]) != "" {
			t.Fatalf("unexpected remote request %#v", req)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"channel_id": "remote_posts",
			"room_id":    "!posts:remote.example",
			"visibility": "public",
			"page":       1,
			"page_size":  5,
			"has_more":   false,
			"posts": []map[string]any{{
				"post_id":        "post_remote",
				"channel_id":     "remote_posts",
				"room_id":        "!posts:remote.example",
				"visibility":     "public",
				"body":           "remote public post",
				"comment_count":  2,
				"like_count":     3,
				"favorite_count": 4,
			}},
		})
	}))
	defer remote.Close()

	service := NewService(Config{
		ServerName:                     "local.example",
		RemoteNodeAllowPrivateBaseURLs: true,
	})
	bootstrapService(t, service)

	got := mustHandle[publicChannelPostsPage](t, service, "channels.public.posts.list", map[string]any{
		"channel_id":           "remote_posts",
		"room_id":              "!posts:remote.example",
		"remote_node_base_url": remote.URL + "/_p2p",
	})
	if len(got.Posts) != 1 || got.Posts[0].PostID != "post_remote" ||
		got.Posts[0].Visibility != "public" || got.Posts[0].CommentCount != 2 ||
		got.Posts[0].LikeCount != 3 || got.Posts[0].FavoriteCount != 4 {
		t.Fatalf("unexpected remote public posts page %#v", got)
	}
}

func TestRemotePublicChannelGetUsesClientProvidedOwnerNodeBaseURL(t *testing.T) {
	calls := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/_p2p/query" {
			t.Fatalf("expected remote public query path, got %s", r.URL.Path)
		}
		var req envelope
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode remote request: %v", err)
		}
		if req.Action != "channels.public.get" || trimString(req.Params["remote_node_base_url"]) != "" {
			t.Fatalf("unexpected remote request %#v", req)
		}
		_ = json.NewEncoder(w).Encode(channel{
			ChannelID:   "remote_ch",
			RoomID:      "!remote:remote.example",
			Name:        "Remote Public",
			Visibility:  "public",
			JoinPolicy:  "open",
			ChannelType: "chat",
		})
	}))
	defer remote.Close()

	service := NewService(Config{
		ServerName:                     "local.example",
		RemoteNodeAllowPrivateBaseURLs: true,
	})
	bootstrapService(t, service)

	got := mustHandle[channel](t, service, "channels.public.get", map[string]any{
		"room_id":              "!remote:remote.example",
		"remote_node_base_url": remote.URL + "/_p2p",
	})
	if got.ChannelID != "remote_ch" {
		t.Fatalf("expected remote public channel via client-provided owner node, got %#v", got)
	}
	if calls != 1 {
		t.Fatalf("expected one remote owner node call, got %d", calls)
	}
}

func TestUserPublicChannelsForwardsToOwnerNodeBaseURL(t *testing.T) {
	calls := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/_p2p/query" {
			t.Fatalf("expected remote public query path, got %s", r.URL.Path)
		}
		var req envelope
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode remote request: %v", err)
		}
		if req.Action != "users.public_channels" ||
			trimString(req.Params["user_id"]) != "@owner:remote.example" ||
			trimString(req.Params["remote_node_base_url"]) != "" {
			t.Fatalf("unexpected remote request %#v", req)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"user_id": "@owner:remote.example",
			"channels": []channel{{
				ChannelID:   "remote_owned",
				RoomID:      "!remote-owned:remote.example",
				Name:        "Remote Owned",
				Visibility:  "public",
				JoinPolicy:  "open",
				ChannelType: "chat",
			}},
		})
	}))
	defer remote.Close()

	service := NewService(Config{
		ServerName:                     "local.example",
		RemoteNodeAllowPrivateBaseURLs: true,
	})
	bootstrapService(t, service)

	result := mustHandle[map[string]any](t, service, "users.public_channels", map[string]any{
		"user_id":              "@owner:remote.example",
		"remote_node_base_url": remote.URL + "/_p2p",
	})
	channels := result["channels"].([]channel)
	if len(channels) != 1 || channels[0].ChannelID != "remote_owned" {
		t.Fatalf("expected remote owner public channels, got %#v", result)
	}
	if calls != 1 {
		t.Fatalf("expected one remote owner node call, got %d", calls)
	}
}

func TestUserPublicChannelsRemoteLookupRequiresValidUserID(t *testing.T) {
	service := NewService(Config{
		ServerName:                     "local.example",
		RemoteNodeAllowPrivateBaseURLs: true,
	})
	bootstrapService(t, service)

	if _, apiErr := service.Handle(context.Background(), "users.public_channels", map[string]any{
		"user_id":              "owner",
		"remote_node_base_url": "https://remote.example/_p2p",
	}); apiErr == nil || apiErr.Status != http.StatusBadRequest || apiErr.Error != "valid user_id is required" {
		t.Fatalf("expected invalid remote user id to return targeted 400, got %#v", apiErr)
	}
}

func TestRemotePublicChannelJoinRequestForwardsToOwnerNode(t *testing.T) {
	requests := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req envelope
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode remote request: %v", err)
		}
		requests++
		switch req.Action {
		case "channels.public.get":
			_ = json.NewEncoder(w).Encode(channel{
				ChannelID:   "remote_ch",
				RoomID:      "!remote:remote.example",
				Name:        "Remote Public",
				Visibility:  "public",
				JoinPolicy:  "approval",
				ChannelType: "chat",
			})
		case "channels.public.join_request":
			if trimString(req.Params["user_id"]) != "@owner:local.example" {
				t.Fatalf("unexpected forwarded join params %#v", req.Params)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "pending",
				"member": memberRecord{
					RoomID:      "!remote:remote.example",
					ChannelID:   "remote_ch",
					UserID:      "@owner:local.example",
					Membership:  "pending",
					Role:        "member",
					DisplayName: "Local Owner",
				},
			})
		default:
			t.Fatalf("unexpected remote action %s", req.Action)
		}
	}))
	defer remote.Close()

	service := NewService(Config{
		ServerName:                     "local.example",
		RemoteNodeAllowPrivateBaseURLs: true,
	})
	bootstrapService(t, service)

	res := mustHandle[map[string]any](t, service, "channels.public.join_request", map[string]any{
		"room_id":              "!remote:remote.example",
		"user_id":              "@owner:local.example",
		"display_name":         "Local Owner",
		"remote_node_base_url": remote.URL + "/_p2p",
	})
	if res["status"] != "pending" {
		t.Fatalf("expected pending remote join request, got %#v", res)
	}
	responseMember, ok := res["member"].(memberRecord)
	if !ok || responseMember.RequestID == "" {
		t.Fatalf("legacy owner response cleared the requester generation: %#v", res)
	}
	members := mustHandle[map[string]any](t, service, "channels.members", map[string]any{
		"room_id": "!remote:remote.example",
	})["members"].([]memberRecord)
	if len(members) != 1 || members[0].Membership != "pending" || members[0].ChannelID != "remote_ch" ||
		members[0].RequestID != responseMember.RequestID {
		t.Fatalf("expected local pending member cache, got %#v", members)
	}
	if requests < 2 {
		t.Fatalf("expected remote detail and join request calls, got %d", requests)
	}
}

func TestRemotePublicChannelJoinRequestLatePendingResponseKeepsJoinedGeneration(t *testing.T) {
	const (
		roomID    = "!remote:remote.example"
		channelID = "remote_ch"
		userID    = "@owner:local.example"
	)
	joinRequestEntered := make(chan string, 1)
	joinRequestRelease := make(chan struct{})
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req envelope
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		switch req.Action {
		case "channels.public.get":
			writeJSON(w, http.StatusOK, channel{
				ChannelID: channelID, RoomID: roomID, Name: "Remote Public",
				Visibility: "public", JoinPolicy: "approval", ChannelType: "chat",
			})
		case "channels.public.join_request":
			requestID := trimString(req.Params["request_id"])
			joinRequestEntered <- requestID
			<-joinRequestRelease
			writeJSON(w, http.StatusOK, map[string]any{
				"status": "pending",
				"member": memberRecord{
					RoomID: roomID, ChannelID: channelID, UserID: userID,
					Membership: "pending", Role: "member", RequestID: requestID,
				},
			})
		default:
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unexpected action " + req.Action})
		}
	}))
	defer remote.Close()
	released := false
	defer func() {
		if !released {
			close(joinRequestRelease)
		}
	}()

	transport := &statefulJoinTransport{recordingTransport: recordingTransport{roomID: roomID}}
	service := NewServiceWithTransport(Config{
		ServerName:                     "local.example",
		RemoteNodeAllowPrivateBaseURLs: true,
	}, transport)
	bootstrapService(t, service)

	type outcome struct {
		result any
		err    *apiError
	}
	requestDone := make(chan outcome, 1)
	go func() {
		result, apiErr := service.Handle(context.Background(), "channels.public.join_request", map[string]any{
			"room_id": roomID, "channel_id": channelID, "user_id": userID,
			"remote_node_base_url": remote.URL + "/_p2p",
		})
		requestDone <- outcome{result: result, err: apiErr}
	}()

	var requestID string
	select {
	case requestID = <-joinRequestEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("remote public join request was not dispatched")
	}
	if requestID == "" {
		t.Fatal("remote public join request did not carry the server-owned generation")
	}
	pending, found, err := service.lookupMember(context.Background(), roomID, userID)
	if err != nil || !found || pending.Membership != "pending" || pending.RequestID != requestID {
		t.Fatalf("expected requester to persist the dispatched pending generation: found=%v member=%#v err=%v", found, pending, err)
	}

	callback, apiErr := service.Handle(context.Background(), "channels.public.join_result", map[string]any{
		"room_id": roomID, "channel_id": channelID, "user_id": userID,
		"status": "approved", "request_id": requestID,
	})
	if apiErr != nil {
		t.Fatalf("join-result callback failed: %#v", apiErr)
	}
	callbackResult := callback.(map[string]any)
	if callbackResult["status"] != "joined" || len(transport.joinRequests) != 1 {
		t.Fatalf("expected callback to complete exactly one Matrix join, result=%#v joins=%#v", callbackResult, transport.joinRequests)
	}
	joined, found, err := service.lookupMember(context.Background(), roomID, userID)
	if err != nil || !found || joined.Membership != "join" || joined.RequestID != requestID {
		t.Fatalf("expected callback to persist the joined generation before the old response: found=%v member=%#v err=%v", found, joined, err)
	}

	close(joinRequestRelease)
	released = true
	var delayed outcome
	select {
	case delayed = <-requestDone:
	case <-time.After(5 * time.Second):
		t.Fatal("delayed remote join-request response did not return")
	}
	if delayed.err != nil {
		t.Fatalf("delayed remote join-request response failed: %#v", delayed.err)
	}
	delayedResult := delayed.result.(map[string]any)
	if delayedResult["status"] != "joined" {
		t.Fatalf("late pending response downgraded the completed join result: %#v", delayedResult)
	}
	current, found, err := service.lookupMember(context.Background(), roomID, userID)
	if err != nil || !found || current.Membership != "join" || current.RequestID != requestID {
		t.Fatalf("late pending response downgraded the persisted generation: found=%v member=%#v err=%v", found, current, err)
	}
	if len(transport.joinRequests) != 1 || len(transport.kicks) != 0 {
		t.Fatalf("late response repeated Matrix side effects: joins=%#v kicks=%#v", transport.joinRequests, transport.kicks)
	}
}

func TestRemotePublicChannelJoinRequestPreSaveCannotOverwriteConcurrentCallbackJoin(t *testing.T) {
	const (
		roomID    = "!remote:remote.example"
		channelID = "remote_ch"
		userID    = "@owner:local.example"
		requestID = "request-a"
	)
	var remoteJoinRequests atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		remoteJoinRequests.Add(1)
		panic(http.ErrAbortHandler)
	}))
	defer remote.Close()

	store := &armedMemberLookupStore{
		MemoryStore: p2pstorage.NewMemoryStore(),
		roomID:      roomID,
		userID:      userID,
		entered:     make(chan struct{}),
		release:     make(chan struct{}),
	}
	transport := &statefulJoinTransport{recordingTransport: recordingTransport{roomID: roomID}}
	service := newService(Config{
		ServerName:                     "local.example",
		RemoteNodeAllowPrivateBaseURLs: true,
	}, store, transport, portalState{}, false)
	bootstrapService(t, service)
	ch := channel{
		ChannelID: channelID, RoomID: roomID, Name: "Remote Public",
		Visibility: "public", JoinPolicy: "approval", ChannelType: "chat",
	}
	if err := service.saveChannel(context.Background(), ch); err != nil {
		t.Fatal(err)
	}
	if err := service.saveMember(context.Background(), memberRecord{
		RoomID: roomID, ChannelID: channelID, UserID: userID,
		Membership: "pending", Role: "member", RequestID: requestID,
	}); err != nil {
		t.Fatal(err)
	}

	store.armed.Store(true)
	released := false
	defer func() {
		if !released {
			close(store.release)
		}
	}()
	requestDone := make(chan remoteChannelJoinRequestOutcome, 1)
	go func() {
		result, handled, apiErr := service.remoteChannelJoinRequest(
			withPublicJoinPreflightChannel(context.Background(), ch),
			map[string]any{
				"room_id": roomID, "channel_id": channelID, "user_id": userID,
				"request_id": requestID, "remote_node_base_url": remote.URL + "/_p2p",
			},
		)
		requestDone <- remoteChannelJoinRequestOutcome{result: result, handled: handled, err: apiErr}
	}()
	select {
	case <-store.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("join-request retry did not capture the existing generation snapshot")
	}

	callback := mustHandle[map[string]any](t, service, "channels.public.join_result", map[string]any{
		"room_id": roomID, "channel_id": channelID, "user_id": userID,
		"status": "approved", "request_id": requestID,
	})
	if callback["status"] != "joined" || len(transport.joinRequests) != 1 {
		t.Fatalf("expected callback to complete generation A before pre-save resumes, result=%#v joins=%#v", callback, transport.joinRequests)
	}
	close(store.release)
	released = true

	var retry remoteChannelJoinRequestOutcome
	select {
	case retry = <-requestDone:
	case <-time.After(5 * time.Second):
		t.Fatal("join-request retry did not finish after releasing its stale snapshot")
	}
	if retry.err != nil || !retry.handled || retry.result["status"] != "joined" {
		t.Fatalf("retry did not return the concurrently completed join: handled=%v result=%#v err=%#v", retry.handled, retry.result, retry.err)
	}
	current, found, err := service.lookupMember(context.Background(), roomID, userID)
	if err != nil || !found || current.Membership != "join" || current.RequestID != requestID {
		t.Fatalf("retry pre-save overwrote the callback join: found=%v member=%#v err=%v", found, current, err)
	}
	if remoteJoinRequests.Load() != 0 || len(transport.joinRequests) != 1 {
		t.Fatalf("stale retry repeated remote or Matrix side effects: remote=%d joins=%#v", remoteJoinRequests.Load(), transport.joinRequests)
	}
}

func TestRemotePublicChannelJoinResponseAndCallbackShareSingleMatrixJoin(t *testing.T) {
	const (
		roomID    = "!remote:remote.example"
		channelID = "remote_ch"
		userID    = "@owner:local.example"
		requestID = "request-a"
	)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req envelope
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if req.Action != "channels.public.join_request" || trimString(req.Params["request_id"]) != requestID {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unexpected join request"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "joined",
			"member": memberRecord{
				RoomID: roomID, ChannelID: channelID, UserID: userID,
				Membership: "join", Role: "member", RequestID: requestID,
			},
		})
	}))
	defer remote.Close()

	store := &observedMatrixJoinWorkflowStore{
		MemoryStore: p2pstorage.NewMemoryStore(),
		secondClaim: make(chan struct{}),
	}
	transport := &blockingMatrixJoinTransport{
		recordingTransport: recordingTransport{
			roomID: roomID,
			roomChannel: channel{
				ChannelID: channelID, RoomID: roomID, Name: "Remote Public",
				Visibility: "public", JoinPolicy: "approval", ChannelType: "chat",
			},
		},
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	service := newService(Config{
		ServerName:                     "local.example",
		RemoteNodeAllowPrivateBaseURLs: true,
	}, store, transport, portalState{}, false)
	bootstrapService(t, service)
	ch := channel{
		ChannelID: channelID, RoomID: roomID, Name: "Remote Public",
		Visibility: "public", JoinPolicy: "approval", ChannelType: "chat",
	}
	if err := service.saveChannel(context.Background(), ch); err != nil {
		t.Fatal(err)
	}
	if err := service.saveMember(context.Background(), memberRecord{
		RoomID: roomID, ChannelID: channelID, UserID: userID,
		Membership: "pending", Role: "member", RequestID: requestID,
	}); err != nil {
		t.Fatal(err)
	}

	released := false
	defer func() {
		if !released {
			close(transport.release)
		}
	}()
	remoteDone := make(chan remoteChannelJoinRequestOutcome, 1)
	go func() {
		result, handled, apiErr := service.remoteChannelJoinRequest(
			withPublicJoinPreflightChannel(context.Background(), ch),
			map[string]any{
				"room_id": roomID, "channel_id": channelID, "user_id": userID,
				"request_id": requestID, "remote_node_base_url": remote.URL + "/_p2p",
			},
		)
		remoteDone <- remoteChannelJoinRequestOutcome{result: result, handled: handled, err: apiErr}
	}()
	select {
	case <-transport.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("remote join response did not reach the Matrix join")
	}

	type callbackOutcome struct {
		result any
		err    *apiError
	}
	callbackDone := make(chan callbackOutcome, 1)
	go func() {
		result, apiErr := service.Handle(context.Background(), "channels.public.join_result", map[string]any{
			"room_id": roomID, "channel_id": channelID, "user_id": userID,
			"status": "approved", "request_id": requestID,
		})
		callbackDone <- callbackOutcome{result: result, err: apiErr}
	}()
	select {
	case <-store.secondClaim:
	case <-time.After(5 * time.Second):
		t.Fatal("callback did not contend for the same Matrix join workflow")
	}
	if calls := transport.calls.Load(); calls != 1 {
		t.Fatalf("same generation issued concurrent Matrix joins before lease release: calls=%d", calls)
	}

	close(transport.release)
	released = true
	var response remoteChannelJoinRequestOutcome
	select {
	case response = <-remoteDone:
	case <-time.After(5 * time.Second):
		t.Fatal("remote join response did not settle after Matrix join release")
	}
	if response.err != nil || !response.handled || response.result["status"] != "joined" {
		t.Fatalf("remote join response did not settle joined: handled=%v result=%#v err=%#v", response.handled, response.result, response.err)
	}
	var callback callbackOutcome
	select {
	case callback = <-callbackDone:
	case <-time.After(5 * time.Second):
		t.Fatal("join-result callback did not settle after Matrix join release")
	}
	if callback.err != nil {
		t.Fatalf("join-result callback failed: %#v", callback.err)
	}
	callbackResult, ok := callback.result.(map[string]any)
	if !ok || callbackResult["status"] != "joined" {
		t.Fatalf("join-result callback did not converge joined: %#v", callback.result)
	}
	current, found, err := service.lookupMember(context.Background(), roomID, userID)
	if err != nil || !found || current.Membership != "join" || current.RequestID != requestID {
		t.Fatalf("same generation did not persist one joined projection: found=%v member=%#v err=%v", found, current, err)
	}
	if calls := transport.calls.Load(); calls != 1 {
		t.Fatalf("same generation repeated the Matrix join: calls=%d", calls)
	}
	if len(transport.kicks) != 0 {
		t.Fatalf("same-generation recovery must never kick: %#v", transport.kicks)
	}
}

func TestRetainedRoomJoinWaiterCannotReportNewGenerationAsJoining(t *testing.T) {
	const (
		roomID     = "!remote:remote.example"
		channelID  = "remote_ch"
		userID     = "@owner:local.example"
		requestIDA = "request-a"
		requestIDB = "request-b"
	)
	store := &observedMatrixJoinWorkflowStore{
		MemoryStore: p2pstorage.NewMemoryStore(),
		secondClaim: make(chan struct{}),
	}
	transport := &blockingMatrixJoinTransport{
		recordingTransport: recordingTransport{roomID: roomID},
		entered:            make(chan struct{}),
		release:            make(chan struct{}),
	}
	service := newService(Config{ServerName: "local.example"}, store, transport, portalState{}, false)
	memberA := memberRecord{
		RoomID: roomID, ChannelID: channelID, UserID: userID,
		Membership: "approved", Role: "member", RequestID: requestIDA,
	}
	if err := service.saveMember(context.Background(), memberA); err != nil {
		t.Fatal(err)
	}

	type joinOutcome struct {
		attempt retainedRoomJoinAttempt
		err     *apiError
	}
	firstDone := make(chan joinOutcome, 1)
	go func() {
		candidate := memberA
		attempt, apiErr := service.joinAndProjectRetainedRoomGeneration(context.Background(), "channel", &candidate, nil)
		firstDone <- joinOutcome{attempt: attempt, err: apiErr}
	}()
	released := false
	defer func() {
		if !released {
			close(transport.release)
		}
	}()
	select {
	case <-transport.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("generation A did not reach its fenced Matrix join")
	}

	waitCtx, cancelWait := context.WithCancel(context.Background())
	secondDone := make(chan joinOutcome, 1)
	go func() {
		candidate := memberA
		attempt, apiErr := service.joinAndProjectRetainedRoomGeneration(waitCtx, "channel", &candidate, nil)
		secondDone <- joinOutcome{attempt: attempt, err: apiErr}
	}()
	select {
	case <-store.secondClaim:
	case <-time.After(5 * time.Second):
		cancelWait()
		t.Fatal("generation A waiter did not contend for the workflow")
	}
	memberB := memberA
	memberB.RequestID = requestIDB
	memberB.Membership = "pending"
	saved, err := service.saveMemberIfState(context.Background(), memberB, requestIDA, "joining")
	if err != nil || !saved {
		cancelWait()
		t.Fatalf("could not install generation B while A was in flight: saved=%v err=%v", saved, err)
	}
	cancelWait()
	var waiter joinOutcome
	select {
	case waiter = <-secondDone:
	case <-time.After(5 * time.Second):
		t.Fatal("old-generation workflow waiter did not stop")
	}
	if waiter.err != nil || !waiter.attempt.Stale || waiter.attempt.Busy ||
		waiter.attempt.Member.RequestID != requestIDB || waiter.attempt.Member.Membership != "pending" {
		t.Fatalf("old-generation waiter mislabeled generation B as joining: attempt=%#v err=%#v", waiter.attempt, waiter.err)
	}

	close(transport.release)
	released = true
	var first joinOutcome
	select {
	case first = <-firstDone:
	case <-time.After(5 * time.Second):
		t.Fatal("generation A join did not settle")
	}
	if first.err != nil || !first.attempt.Stale || first.attempt.Member.RequestID != requestIDB {
		t.Fatalf("generation A settlement overwrote generation B: attempt=%#v err=%#v", first.attempt, first.err)
	}
	current, found, err := service.lookupMember(context.Background(), roomID, userID)
	if err != nil || !found || current.RequestID != requestIDB || current.Membership != "pending" {
		t.Fatalf("generation B was not preserved: found=%v member=%#v err=%v", found, current, err)
	}
	if calls := transport.calls.Load(); calls != 1 {
		t.Fatalf("generation change repeated Matrix join: calls=%d", calls)
	}
}

func TestRemotePublicChannelJoinRequestStaleJoinResponseCannotRevivePriorGeneration(t *testing.T) {
	for _, remoteStatus := range []string{"approved", "joining", "join_failed", "joined"} {
		t.Run(remoteStatus, func(t *testing.T) {
			const (
				roomID     = "!remote:remote.example"
				channelID  = "remote_ch"
				userID     = "@owner:local.example"
				requestIDA = "request-a"
				requestIDB = "request-b"
			)
			joinRequestAEntered := make(chan struct{}, 1)
			joinRequestARelease := make(chan struct{})
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req envelope
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
					return
				}
				requestID := trimString(req.Params["request_id"])
				switch requestID {
				case requestIDA:
					joinRequestAEntered <- struct{}{}
					<-joinRequestARelease
					writeJSON(w, http.StatusOK, map[string]any{
						"status": remoteStatus,
						"member": memberRecord{
							RoomID: roomID, ChannelID: channelID, UserID: userID,
							Membership: remoteStatus, Role: "member",
						},
					})
				case requestIDB:
					writeJSON(w, http.StatusOK, map[string]any{
						"status": "pending",
						"member": memberRecord{
							RoomID: roomID, ChannelID: channelID, UserID: userID,
							Membership: "pending", Role: "member",
						},
					})
				default:
					writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unexpected request generation"})
				}
			}))
			defer remote.Close()
			released := false
			defer func() {
				if !released {
					close(joinRequestARelease)
				}
			}()

			transport := &statefulJoinTransport{recordingTransport: recordingTransport{roomID: roomID}}
			service := NewServiceWithTransport(Config{
				ServerName:                     "local.example",
				RemoteNodeAllowPrivateBaseURLs: true,
			}, transport)
			bootstrapService(t, service)
			ch := channel{
				ChannelID: channelID, RoomID: roomID, Name: "Remote Public",
				Visibility: "public", JoinPolicy: "approval", ChannelType: "chat",
			}
			if err := service.saveChannel(context.Background(), ch); err != nil {
				t.Fatal(err)
			}
			if err := service.saveMember(context.Background(), memberRecord{
				RoomID: roomID, ChannelID: channelID, UserID: userID,
				Membership: "pending", Role: "member", RequestID: requestIDA,
			}); err != nil {
				t.Fatal(err)
			}

			requestADone := make(chan remoteChannelJoinRequestOutcome, 1)
			go func() {
				result, handled, apiErr := service.remoteChannelJoinRequest(
					withPublicJoinPreflightChannel(context.Background(), ch),
					map[string]any{
						"room_id": roomID, "channel_id": channelID, "user_id": userID,
						"request_id": requestIDA, "remote_node_base_url": remote.URL + "/_p2p",
					},
				)
				requestADone <- remoteChannelJoinRequestOutcome{result: result, handled: handled, err: apiErr}
			}()
			select {
			case <-joinRequestAEntered:
			case <-time.After(5 * time.Second):
				t.Fatal("generation A was not dispatched")
			}

			callback := mustHandle[map[string]any](t, service, "channels.public.join_result", map[string]any{
				"room_id": roomID, "channel_id": channelID, "user_id": userID,
				"status": "approved", "request_id": requestIDA,
			})
			if callback["status"] != "joined" || len(transport.joinRequests) != 1 {
				t.Fatalf("expected generation A callback to join once, result=%#v joins=%#v", callback, transport.joinRequests)
			}
			left := mustHandle[map[string]any](t, service, "channels.leave", map[string]any{
				"room_id": roomID, "channel_id": channelID,
			})
			leftMember := left["member"].(memberRecord)
			if leftMember.Membership != "leave" {
				t.Fatalf("expected generation A to leave before reapplying, got %#v", left)
			}
			transport.roomMembers = nil

			requestB, handled, apiErr := service.remoteChannelJoinRequest(
				withPublicJoinPreflightChannel(context.Background(), ch),
				map[string]any{
					"room_id": roomID, "channel_id": channelID, "user_id": userID,
					"request_id": requestIDB, "remote_node_base_url": remote.URL + "/_p2p",
				},
			)
			if apiErr != nil || !handled || requestB["status"] != "pending" {
				t.Fatalf("generation B did not become pending: handled=%v result=%#v err=%#v", handled, requestB, apiErr)
			}
			generationB, found, err := service.lookupMember(context.Background(), roomID, userID)
			if err != nil || !found || generationB.Membership != "pending" || generationB.RequestID != requestIDB {
				t.Fatalf("generation B was not persisted before A resumed: found=%v member=%#v err=%v", found, generationB, err)
			}

			close(joinRequestARelease)
			released = true
			var delayedA remoteChannelJoinRequestOutcome
			select {
			case delayedA = <-requestADone:
			case <-time.After(5 * time.Second):
				t.Fatal("generation A response did not settle")
			}
			if delayedA.err != nil || !delayedA.handled || delayedA.result["status"] != "pending" {
				t.Fatalf("late generation A did not return generation B: handled=%v result=%#v err=%#v", delayedA.handled, delayedA.result, delayedA.err)
			}
			current, found, err := service.lookupMember(context.Background(), roomID, userID)
			if err != nil || !found || current.Membership != "pending" || current.RequestID != requestIDB {
				t.Fatalf("late generation A overwrote generation B: found=%v member=%#v err=%v", found, current, err)
			}
			if len(transport.joinRequests) != 1 || len(transport.leaves) != 1 || len(transport.kicks) != 0 {
				t.Fatalf("late generation A repeated Matrix effects: joins=%#v leaves=%#v kicks=%#v", transport.joinRequests, transport.leaves, transport.kicks)
			}
		})
	}
}

func TestRemotePublicChannelApprovalCallsRequesterNodeFromStoredJoinRequest(t *testing.T) {
	requesterTransport := &recordingTransport{roomID: "!remote:c.example"}
	requesterService := NewServiceWithTransport(Config{
		ServerName:                     "b.example",
		RemoteNodeAllowPrivateBaseURLs: true,
	}, requesterTransport)
	bootstrapService(t, requesterService)
	requesterServer := httptest.NewServer(newP2PTestRouter(requesterService))
	defer requesterServer.Close()
	requesterService.homeserver = requesterServer.URL

	ownerService := NewService(Config{
		ServerName:                     "c.example",
		RemoteNodeAllowPrivateBaseURLs: true,
	})
	bootstrapService(t, ownerService)
	ownerServer := httptest.NewServer(newP2PTestRouter(ownerService))
	defer ownerServer.Close()

	ch := mustHandle[channel](t, ownerService, "channels.create", map[string]any{
		"channel_id":       "remote_ch",
		"room_id":          "!remote:c.example",
		"name":             "Remote Public",
		"visibility":       "public",
		"join_policy":      "approval",
		"channel_type":     "chat",
		"comments_enabled": true,
	})

	pending := mustHandle[map[string]any](t, requesterService, "channels.public.join_request", map[string]any{
		"room_id":              ch.RoomID,
		"user_id":              "@owner:b.example",
		"display_name":         "Requester",
		"remote_node_base_url": ownerServer.URL + "/_p2p",
	})
	if pending["status"] != "pending" {
		t.Fatalf("expected forwarded public join request to stay pending, got %#v", pending)
	}
	storedOwnerMember, ok, err := ownerService.lookupMember(context.Background(), ch.RoomID, "@owner:b.example")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || storedOwnerMember.RequesterNodeBaseURL != requesterServer.URL+"/_p2p" {
		t.Fatalf("expected owner node to store requester callback URL, got ok=%v member=%#v", ok, storedOwnerMember)
	}

	approved := mustHandle[map[string]any](t, ownerService, "channels.join_request.approve", map[string]any{
		"channel_id": ch.ChannelID,
		"room_id":    ch.RoomID,
		"user_id":    "@owner:b.example",
	})
	if approved["status"] != "joined" {
		t.Fatalf("expected approval to call requester node and join, got %#v", approved)
	}
	if len(requesterTransport.joins) != 1 || requesterTransport.joins[0] != "@owner:b.example in !remote:c.example" {
		t.Fatalf("expected requester node Matrix join, got %#v", requesterTransport.joins)
	}
	if len(requesterTransport.joinRequests) != 1 || len(requesterTransport.joinRequests[0].ServerNames) != 1 || requesterTransport.joinRequests[0].ServerNames[0] != "c.example" {
		t.Fatalf("expected requester node Matrix join to carry owner room server name, got %#v", requesterTransport.joinRequests)
	}
	requesterMembers := mustHandle[map[string]any](t, requesterService, "channels.members", map[string]any{
		"room_id": ch.RoomID,
	})["members"].([]memberRecord)
	requesterMember := findMember(requesterMembers, "@owner:b.example")
	if requesterMember.Membership != "join" {
		t.Fatalf("expected requester member to become joined, got %#v", requesterMembers)
	}
}

func TestPublicChannelNewGenerationRefreshesRequesterCallbackAfterOwnerRestart(t *testing.T) {
	requesterStore := p2pstorage.NewMemoryStore()
	requesterTransport := &recordingTransport{roomID: "!remote:c.example"}
	requesterService := newService(Config{
		ServerName:                     "b.example",
		RemoteNodeAllowPrivateBaseURLs: true,
	}, requesterStore, requesterTransport, portalState{}, false)
	bootstrapService(t, requesterService)

	oldCallbackCalls := 0
	oldRouter := newP2PTestRouter(requesterService)
	oldCallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oldCallbackCalls++
		oldRouter.ServeHTTP(w, r)
	}))
	defer oldCallback.Close()
	requesterService.homeserver = oldCallback.URL

	ownerStore := p2pstorage.NewMemoryStore()
	ownerService := newService(Config{
		ServerName:                     "c.example",
		RemoteNodeAllowPrivateBaseURLs: true,
	}, ownerStore, nil, portalState{}, false)
	bootstrapService(t, ownerService)
	ownerServer := httptest.NewServer(newP2PTestRouter(ownerService))
	defer ownerServer.Close()

	ch := mustHandle[channel](t, ownerService, "channels.create", map[string]any{
		"channel_id": "remote_ch", "room_id": "!remote:c.example", "name": "Remote Public",
		"visibility": "public", "join_policy": "approval", "channel_type": "chat",
	})
	requestParams := map[string]any{
		"room_id": ch.RoomID, "user_id": "@owner:b.example", "display_name": "Requester",
		"remote_node_base_url": ownerServer.URL + "/_p2p",
	}
	if pending := mustHandle[map[string]any](t, requesterService, "channels.public.join_request", cloneParams(requestParams)); pending["status"] != "pending" {
		t.Fatalf("expected first generation pending, got %#v", pending)
	}
	if rejected := mustHandle[map[string]any](t, ownerService, "channels.join_request.reject", map[string]any{
		"room_id": ch.RoomID, "channel_id": ch.ChannelID, "user_id": "@owner:b.example",
	}); rejected["status"] != "rejected" {
		t.Fatalf("expected first generation rejected, got %#v", rejected)
	}
	oldCallsAfterReject := oldCallbackCalls

	newCallbackCalls := 0
	newRouter := newP2PTestRouter(requesterService)
	newCallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		newCallbackCalls++
		newRouter.ServeHTTP(w, r)
	}))
	defer newCallback.Close()
	requesterService.homeserver = newCallback.URL

	if pending := mustHandle[map[string]any](t, requesterService, "channels.public.join_request", cloneParams(requestParams)); pending["status"] != "pending" {
		t.Fatalf("expected replacement generation pending, got %#v", pending)
	}
	requesterMember, requesterFound, err := requesterService.lookupMember(context.Background(), ch.RoomID, "@owner:b.example")
	if err != nil || !requesterFound || requesterMember.RequesterNodeBaseURL != newCallback.URL+"/_p2p" {
		t.Fatalf("expected requester generation to store new callback URL, found=%v member=%#v err=%v", requesterFound, requesterMember, err)
	}
	ownerMember, ownerFound, err := ownerService.lookupMember(context.Background(), ch.RoomID, "@owner:b.example")
	if err != nil || !ownerFound || ownerMember.RequesterNodeBaseURL != newCallback.URL+"/_p2p" {
		t.Fatalf("expected owner generation to store new callback URL, found=%v member=%#v err=%v", ownerFound, ownerMember, err)
	}

	restartedOwner := newService(Config{
		ServerName:                     "c.example",
		RemoteNodeAllowPrivateBaseURLs: true,
	}, ownerStore, nil, portalState{}, false)
	approved := mustHandle[map[string]any](t, restartedOwner, "channels.join_request.approve", map[string]any{
		"room_id": ch.RoomID, "channel_id": ch.ChannelID, "user_id": "@owner:b.example",
	})
	if approved["status"] != "joined" {
		t.Fatalf("expected replacement generation approval to join after restart, got %#v", approved)
	}
	if oldCallbackCalls != oldCallsAfterReject || newCallbackCalls == 0 {
		t.Fatalf("expected approval callback only at new URL, old=%d (after reject=%d) new=%d", oldCallbackCalls, oldCallsAfterReject, newCallbackCalls)
	}
	requesterMember, requesterFound, err = requesterService.lookupMember(context.Background(), ch.RoomID, "@owner:b.example")
	if err != nil || !requesterFound || requesterMember.Membership != "join" {
		t.Fatalf("expected requester joined after callback, found=%v member=%#v err=%v", requesterFound, requesterMember, err)
	}
	ownerMember, ownerFound, err = restartedOwner.lookupMember(context.Background(), ch.RoomID, "@owner:b.example")
	if err != nil || !ownerFound || ownerMember.Membership != "join" {
		t.Fatalf("expected restarted owner joined projection, found=%v member=%#v err=%v", ownerFound, ownerMember, err)
	}
}

func TestChannelPublicJoinResultApprovedJoinsRequesterNode(t *testing.T) {
	transport := &recordingTransport{roomID: "!remote:remote.example"}
	service := NewServiceWithTransport(Config{ServerName: "local.example"}, transport)
	bootstrapService(t, service)
	mustHandle[ownerProfile](t, service, "profile.update", map[string]any{
		"display_name": "Local Owner",
		"avatar_url":   "mxc://local.example/owner",
	})
	ch := channel{
		ChannelID:  "remote_ch",
		RoomID:     "!remote:remote.example",
		Name:       "Remote Public",
		Visibility: "public",
		JoinPolicy: "approval",
	}
	if err := service.saveChannel(context.Background(), ch); err != nil {
		t.Fatal(err)
	}
	if err := service.saveMember(context.Background(), memberRecord{
		RoomID:     ch.RoomID,
		ChannelID:  ch.ChannelID,
		UserID:     "@owner:local.example",
		Domain:     "local.example",
		Membership: "pending",
		Role:       "member",
	}); err != nil {
		t.Fatal(err)
	}

	result := mustHandle[map[string]any](t, service, "channels.public.join_result", map[string]any{
		"room_id":      ch.RoomID,
		"channel_id":   ch.ChannelID,
		"user_id":      "@owner:local.example",
		"status":       "approved",
		"server_names": []string{"remote.example"},
		"request_id":   "req-1",
	})
	if result["status"] != "joined" {
		t.Fatalf("expected approved join result to join requester node, got %#v", result)
	}
	if len(transport.joins) != 1 || transport.joins[0] != "@owner:local.example in !remote:remote.example" {
		t.Fatalf("expected requester node Matrix join, got %#v", transport.joins)
	}
	if len(transport.joinRequests) != 1 || len(transport.joinRequests[0].ServerNames) != 1 || transport.joinRequests[0].ServerNames[0] != "remote.example" {
		t.Fatalf("expected join to carry remote server_names, got %#v", transport.joinRequests)
	}
	if transport.joinRequests[0].DisplayName != "Local Owner" || transport.joinRequests[0].AvatarURL != "mxc://local.example/owner" {
		t.Fatalf("expected channel join result to carry local owner profile, got %#v", transport.joinRequests[0])
	}
	members := mustHandle[map[string]any](t, service, "channels.members", map[string]any{
		"channel_id": ch.ChannelID,
	})["members"].([]memberRecord)
	owner := findMember(members, "@owner:local.example")
	if owner.Membership != "join" {
		t.Fatalf("expected local projection to become joined, got %#v", members)
	}
	if owner.DisplayName != "Local Owner" || owner.AvatarURL != "mxc://local.example/owner" {
		t.Fatalf("expected local member to keep owner profile after join result, got %#v", owner)
	}
}

func TestChannelPublicJoinResultApprovedJoinsRequesterAfterInviteProjection(t *testing.T) {
	transport := &recordingTransport{roomID: "!remote:remote.example"}
	service := NewServiceWithTransport(Config{ServerName: "local.example"}, transport)
	bootstrapService(t, service)
	ch := channel{
		ChannelID:  "remote_ch",
		RoomID:     "!remote:remote.example",
		Name:       "Remote Public",
		Visibility: "public",
		JoinPolicy: "approval",
	}
	if err := service.saveChannel(context.Background(), ch); err != nil {
		t.Fatal(err)
	}
	if err := service.saveMember(context.Background(), memberRecord{
		RoomID:     ch.RoomID,
		ChannelID:  ch.ChannelID,
		UserID:     "@owner:local.example",
		Domain:     "local.example",
		Membership: "invite",
		Role:       "member",
	}); err != nil {
		t.Fatal(err)
	}

	result := mustHandle[map[string]any](t, service, "channels.public.join_result", map[string]any{
		"room_id":      ch.RoomID,
		"channel_id":   ch.ChannelID,
		"user_id":      "@owner:local.example",
		"status":       "approved",
		"server_names": []string{"remote.example"},
	})
	if result["status"] != "joined" {
		t.Fatalf("expected approved invite projection to join requester node, got %#v", result)
	}
	if len(transport.joins) != 1 || transport.joins[0] != "@owner:local.example in !remote:remote.example" {
		t.Fatalf("expected requester node Matrix join, got %#v", transport.joins)
	}
	member, ok, err := service.lookupMember(context.Background(), ch.RoomID, "@owner:local.example")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || member.Membership != "join" {
		t.Fatalf("expected requester member to become joined, ok=%v member=%#v", ok, member)
	}
}

func TestChannelPublicJoinResultCreatesConversationFromRefreshedChannel(t *testing.T) {
	transport := &recordingTransport{
		roomID: "!remote:remote.example",
		roomChannel: channel{
			ChannelID:       "remote_ch",
			RoomID:          "!remote:remote.example",
			Name:            "Remote Public",
			Visibility:      "public",
			JoinPolicy:      "approval",
			ChannelType:     "chat",
			CommentsEnabled: true,
		},
	}
	service := NewServiceWithTransport(Config{ServerName: "local.example"}, transport)
	bootstrapService(t, service)
	if err := service.saveMember(context.Background(), memberRecord{
		RoomID:     "!remote:remote.example",
		ChannelID:  "remote_ch",
		UserID:     "@owner:local.example",
		Domain:     "local.example",
		Membership: "pending",
		Role:       "member",
	}); err != nil {
		t.Fatal(err)
	}

	result := mustHandle[map[string]any](t, service, "channels.public.join_result", map[string]any{
		"room_id":      "!remote:remote.example",
		"channel_id":   "remote_ch",
		"user_id":      "@owner:local.example",
		"status":       "approved",
		"server_names": []string{"remote.example"},
	})
	if result["status"] != "joined" {
		t.Fatalf("expected approved join result to join requester node, got %#v", result)
	}

	list := mustHandle[map[string]any](t, service, "conversations.list", nil)
	conversations := list["conversations"].([]conversationView)
	if len(conversations) != 1 || conversations[0].MatrixRoomID != "!remote:remote.example" || conversations[0].Kind != conversationKindChannel {
		t.Fatalf("expected refreshed joined channel to create channel conversation, got %#v", conversations)
	}
	if conversations[0].Membership != "join" || conversations[0].Title != "Remote Public" {
		t.Fatalf("expected joined channel conversation facts, got %#v", conversations[0])
	}
}

func TestChannelPublicJoinResultApprovedFallsBackToRoomServerName(t *testing.T) {
	transport := &recordingTransport{roomID: "!remote:remote.example"}
	service := NewServiceWithTransport(Config{ServerName: "local.example"}, transport)
	bootstrapService(t, service)
	ch := channel{
		ChannelID:  "remote_ch",
		RoomID:     "!remote:remote.example",
		Name:       "Remote Public",
		Visibility: "public",
		JoinPolicy: "approval",
	}
	if err := service.saveChannel(context.Background(), ch); err != nil {
		t.Fatal(err)
	}
	if err := service.saveMember(context.Background(), memberRecord{
		RoomID:     ch.RoomID,
		ChannelID:  ch.ChannelID,
		UserID:     "@owner:local.example",
		Domain:     "local.example",
		Membership: "pending",
		Role:       "member",
	}); err != nil {
		t.Fatal(err)
	}

	result := mustHandle[map[string]any](t, service, "channels.public.join_result", map[string]any{
		"room_id":    ch.RoomID,
		"channel_id": ch.ChannelID,
		"user_id":    "@owner:local.example",
		"status":     "approved",
	})
	if result["status"] != "joined" {
		t.Fatalf("expected approved join result to join requester node, got %#v", result)
	}
	if len(transport.joinRequests) != 1 || len(transport.joinRequests[0].ServerNames) != 1 || transport.joinRequests[0].ServerNames[0] != "remote.example" {
		t.Fatalf("expected join result to fall back to room server name, got %#v", transport.joinRequests)
	}
}

func TestChannelPublicJoinResultRejectedUpdatesRequesterNode(t *testing.T) {
	service := NewService(Config{ServerName: "local.example"})
	bootstrapService(t, service)
	ch := channel{
		ChannelID:  "remote_ch",
		RoomID:     "!remote:remote.example",
		Name:       "Remote Public",
		Visibility: "public",
		JoinPolicy: "approval",
	}
	if err := service.saveChannel(context.Background(), ch); err != nil {
		t.Fatal(err)
	}
	if err := service.saveMember(context.Background(), memberRecord{
		RoomID:     ch.RoomID,
		ChannelID:  ch.ChannelID,
		UserID:     "@owner:local.example",
		Domain:     "local.example",
		Membership: "pending",
		Role:       "member",
	}); err != nil {
		t.Fatal(err)
	}

	result := mustHandle[map[string]any](t, service, "channels.public.join_result", map[string]any{
		"room_id":    ch.RoomID,
		"channel_id": ch.ChannelID,
		"user_id":    "@owner:local.example",
		"status":     "rejected",
		"reason":     "not now",
	})
	if result["status"] != "rejected" {
		t.Fatalf("expected rejected join result, got %#v", result)
	}
	member := result["member"].(memberRecord)
	if member.Membership != "reject" {
		t.Fatalf("expected local projection to become rejected, got %#v", member)
	}
	events := mustListP2PEvents(t, service)
	if len(events) != 1 || events[0].Type != "channel.join_request.changed" {
		t.Fatalf("expected P2P event for rejected join request, got %#v", events)
	}
}

func TestChannelJoinRequestApprovalJoinsLocalRequesterThroughTransport(t *testing.T) {
	transport := &recordingTransport{roomID: "!channel:example.com"}
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)
	bootstrapService(t, service)

	ch := mustHandle[channel](t, service, "channels.create", map[string]any{
		"channel_id":  "ch",
		"name":        "Channel",
		"join_policy": "approval",
	})
	mustHandle[map[string]any](t, service, "channels.public.join_request", map[string]any{
		"channel_id": ch.ChannelID,
		"room_id":    ch.RoomID,
		"user_mxid":  "@alice:example.com",
	})
	mustHandle[map[string]any](t, service, "channels.join_request.approve", map[string]any{
		"channel_id": ch.ChannelID,
		"room_id":    ch.RoomID,
		"user_mxid":  "@alice:example.com",
	})
	member := mustHandle[map[string]any](t, service, "channels.members", map[string]any{
		"channel_id": ch.ChannelID,
	})["members"].([]memberRecord)
	alice := findMember(member, "@alice:example.com")
	if alice.Membership != "join" {
		t.Fatalf("expected approved local join request to join through Matrix, got %#v", member)
	}

	if len(transport.invites) != 0 {
		t.Fatalf("expected approved join request not to invite through transport, got %#v", transport.invites)
	}
	if len(transport.joins) != 1 || transport.joins[0] != "@alice:example.com in !channel:example.com" {
		t.Fatalf("expected approved join request to join through transport, got %#v", transport.joins)
	}
	joinRequestStates := recordedStatesOfType(transport.stateEvents, DirextalkJoinRequestEventType)
	if len(joinRequestStates) != 2 {
		t.Fatalf("expected pending and approved join request state events, got %#v", joinRequestStates)
	}
	if joinRequestStates[0].Event.StateKey != productpolicy.UserStateKey("@alice:example.com") || joinRequestStates[0].Event.Content["status"] != "pending" {
		t.Fatalf("expected pending join request state, got %#v", joinRequestStates[0])
	}
	if joinRequestStates[1].Event.StateKey != productpolicy.UserStateKey("@alice:example.com") || joinRequestStates[1].Event.Content["status"] != "approved" {
		t.Fatalf("expected approved join request state, got %#v", joinRequestStates[1])
	}
}

func TestChannelInviteGrantInvitesJoinedShareRoomMembers(t *testing.T) {
	transport := &recordingTransport{}
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)
	bootstrapService(t, service)
	createdChannel := mustHandle[channel](t, service, "channels.create", map[string]any{
		"channel_id":  "private",
		"room_id":     "!private:example.com",
		"name":        "Private",
		"visibility":  "private",
		"join_policy": "invite",
	})
	shareRoomID := "!share:example.com"
	if err := service.saveGroup(context.Background(), groupRecord{
		RoomID: shareRoomID,
		Name:   "Share Room",
	}); err != nil {
		t.Fatal(err)
	}
	for _, member := range []memberRecord{
		{RoomID: shareRoomID, UserID: "@owner:example.com", Domain: "example.com", Membership: "join", Role: "owner"},
		{RoomID: shareRoomID, UserID: "@alice:remote.example", Domain: "remote.example", Membership: "join", Role: "member"},
		{RoomID: shareRoomID, UserID: "@bob:remote.example", Domain: "remote.example", Membership: "invite", Role: "member"},
	} {
		if err := service.saveMember(context.Background(), member); err != nil {
			t.Fatal(err)
		}
	}

	result := mustHandle[map[string]any](t, service, "channels.invite_grant.create", map[string]any{
		"room_id":       createdChannel.RoomID,
		"channel_id":    createdChannel.ChannelID,
		"share_room_id": shareRoomID,
	})

	if result["share_room_id"] != shareRoomID || result["room_id"] != createdChannel.RoomID {
		t.Fatalf("expected grant to echo channel and share room, got %#v", result)
	}
	if len(transport.invites) != 1 || transport.invites[0] != "@owner:example.com -> @alice:remote.example in !private:example.com" {
		t.Fatalf("expected grant to invite only joined non-owner share-room member, got %#v", transport.invites)
	}
}

func TestChannelInviteGrantUsesMatrixShareRoomMembersWhenProjectionMissing(t *testing.T) {
	shareRoomID := "!share:example.com"
	transport := &recordingTransport{
		roomMembers: []memberRecord{
			{RoomID: shareRoomID, UserID: "@owner:example.com", Domain: "example.com", Membership: "join", Role: "owner"},
			{RoomID: shareRoomID, UserID: "@alice:remote.example", Domain: "remote.example", Membership: "join", Role: "member"},
		},
	}
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)
	bootstrapService(t, service)
	createdChannel := mustHandle[channel](t, service, "channels.create", map[string]any{
		"channel_id":  "private",
		"room_id":     "!private:example.com",
		"name":        "Private",
		"visibility":  "private",
		"join_policy": "invite",
	})
	if err := service.saveGroup(context.Background(), groupRecord{
		RoomID: shareRoomID,
		Name:   "Share Room",
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.saveMember(context.Background(), memberRecord{
		RoomID:     shareRoomID,
		UserID:     "@owner:example.com",
		Domain:     "example.com",
		Membership: "join",
		Role:       "owner",
	}); err != nil {
		t.Fatal(err)
	}

	result := mustHandle[map[string]any](t, service, "channels.invite_grant.create", map[string]any{
		"room_id":       createdChannel.RoomID,
		"channel_id":    createdChannel.ChannelID,
		"share_room_id": shareRoomID,
	})

	members := result["members"].([]memberRecord)
	if len(members) != 1 || members[0].UserID != "@alice:remote.example" || members[0].Membership != "invite" {
		t.Fatalf("expected Matrix share-room member to be invited, got %#v", result)
	}
	if len(transport.invites) != 1 || transport.invites[0] != "@owner:example.com -> @alice:remote.example in !private:example.com" {
		t.Fatalf("expected grant to invite Matrix share-room member, got %#v", transport.invites)
	}
}
