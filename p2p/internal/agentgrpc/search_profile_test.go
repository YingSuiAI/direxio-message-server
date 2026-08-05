package agentgrpc

import (
	"context"
	"errors"
	"reflect"
	"testing"

	agentv1 "github.com/YingSuiAI/dirextalk-agent/api/gen/dirextalk/agent/v1"
	agentmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agent"
)

func TestRunnerSearchProfileGetAndUpdatePreserveRuntimeConfig(t *testing.T) {
	server := startRuntimeServer(t)
	server.service.getRuntimeConfig = func(*agentv1.GetRuntimeConfigRequest) (*agentv1.GetRuntimeConfigResponse, error) {
		config := validRemoteRuntimeConfig("deepseek-v4", 7)
		config.Spec.SearchProfile = &agentv1.SearchProfile{
			ProfileId: "brave-default", Provider: agentv1.SearchProvider_SEARCH_PROVIDER_BRAVE,
			BaseUrl:    "https://api.search.brave.com/res/v1/web/search",
			MaxResults: 10, TimeoutSeconds: 20,
		}
		return &agentv1.GetRuntimeConfigResponse{Config: config}, nil
	}
	server.service.putRuntimeConfig = func(request *agentv1.PutRuntimeConfigRequest) (*agentv1.PutRuntimeConfigResponse, error) {
		return validRuntimeConfigPutResponse(request), nil
	}
	runner := newTestRunner(t, server, Config{})

	state, err := runner.GetSearchProfile(context.Background())
	if err != nil || !state.Available || !state.Configured || state.Revision != 7 || state.Profile == nil ||
		state.Profile.ProfileID != "brave-default" || state.Profile.Provider != "brave" ||
		!reflect.DeepEqual(state.AvailableProfileIDs, []string{"brave-default", "tavily-default"}) {
		t.Fatalf("search profile state = %#v, %v", state, err)
	}
	maxResults, timeoutSeconds := int64(6), int64(12)
	state, err = runner.UpdateSearchProfile(context.Background(), agentmodule.SearchProfileUpdate{
		IdempotencyKey: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		ProfileID:      "tavily-default", ExpectedRevision: 7,
		MaxResults: &maxResults, TimeoutSeconds: &timeoutSeconds,
	})
	if err != nil {
		t.Fatal(err)
	}
	server.service.mu.Lock()
	putRequest := server.service.putRuntimeRequest
	server.service.mu.Unlock()
	if putRequest.GetOwnerId() != "owner-from-config" || putRequest.GetExpectedRevision() != 7 ||
		putRequest.GetSpec().GetModelProfile().GetProfileId() != "deepseek-v4" ||
		putRequest.GetSpec().GetProjectProfile() != "existing project" ||
		putRequest.GetSpec().GetSearchProfile().GetProfileId() != "tavily-default" ||
		putRequest.GetSpec().GetSearchProfile().GetProvider() != agentv1.SearchProvider_SEARCH_PROVIDER_UNSPECIFIED ||
		putRequest.GetSpec().GetSearchProfile().GetBaseUrl() != "" ||
		putRequest.GetSpec().GetSearchProfile().GetSecretRef() != "" ||
		putRequest.GetSpec().GetSearchProfile().GetMaxResults() != 6 ||
		putRequest.GetSpec().GetSearchProfile().GetTimeoutSeconds() != 12 ||
		!containsSortedString(putRequest.GetSpec().GetEnabledTools(), nativeWebSearchToolName) {
		t.Fatalf("search profile PutRuntimeConfig = %#v", putRequest)
	}
	if state.Revision != 8 || state.Profile == nil || state.Profile.ProfileID != "tavily-default" ||
		state.Profile.Provider != "tavily" || state.Profile.MaxResults != 6 || state.Profile.TimeoutSeconds != 12 {
		t.Fatalf("updated search profile state = %#v", state)
	}
}

func TestRunnerSearchProfileCanBeAddedAfterModelConfiguration(t *testing.T) {
	server := startRuntimeServer(t)
	server.service.putRuntimeConfig = func(request *agentv1.PutRuntimeConfigRequest) (*agentv1.PutRuntimeConfigResponse, error) {
		return validRuntimeConfigPutResponse(request), nil
	}
	runner := newTestRunner(t, server, Config{})
	state, err := runner.GetSearchProfile(context.Background())
	if err != nil || !state.Available || state.Configured || state.Revision != 7 || state.Profile != nil {
		t.Fatalf("unconfigured search state = %#v, %v", state, err)
	}
	state, err = runner.UpdateSearchProfile(context.Background(), agentmodule.SearchProfileUpdate{
		IdempotencyKey: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		ProfileID:      "brave-default", ExpectedRevision: 7,
	})
	if err != nil || !state.Configured || state.Revision != 8 || state.Profile == nil || state.Profile.ProfileID != "brave-default" {
		t.Fatalf("configured search state = %#v, %v", state, err)
	}
}

func TestSearchProfileProviderMapsDeepSeekNative(t *testing.T) {
	if got := searchProfileProvider(
		agentv1.SearchProvider_SEARCH_PROVIDER_DEEPSEEK_NATIVE,
	); got != "deepseek_native" {
		t.Fatalf("DeepSeek native search provider = %q", got)
	}
}

func TestRunnerSearchProfileFailsClosedForMissingCatalogOrSecretResponse(t *testing.T) {
	server := startRuntimeServer(t)
	server.service.getCapabilities = func(*agentv1.RuntimeServiceGetCapabilitiesRequest) (*agentv1.RuntimeServiceGetCapabilitiesResponse, error) {
		response := validRuntimeCapabilities()
		response.Capabilities.SearchProfileIds = nil
		return response, nil
	}
	runner := newTestRunner(t, server, Config{})
	state, err := runner.GetSearchProfile(context.Background())
	if err != nil || state.Available || state.Configured || len(state.AvailableProfileIDs) != 0 {
		t.Fatalf("missing search catalog state = %#v, %v", state, err)
	}
	server.service.mu.Lock()
	getRequest := server.service.runtimeConfigRequest
	server.service.mu.Unlock()
	if getRequest != nil {
		t.Fatalf("missing search catalog reached owner config read: %#v", getRequest)
	}

	server = startRuntimeServer(t)
	server.service.getRuntimeConfig = func(*agentv1.GetRuntimeConfigRequest) (*agentv1.GetRuntimeConfigResponse, error) {
		config := validRemoteRuntimeConfig("deepseek-v4", 7)
		config.Spec.SearchProfile = &agentv1.SearchProfile{
			ProfileId: "brave-default", Provider: agentv1.SearchProvider_SEARCH_PROVIDER_BRAVE,
			BaseUrl:   "https://api.search.brave.com/res/v1/web/search",
			SecretRef: "mounted:must-not-cross", MaxResults: 10, TimeoutSeconds: 20,
		}
		return &agentv1.GetRuntimeConfigResponse{Config: config}, nil
	}
	runner = newTestRunner(t, server, Config{})
	if _, err := runner.GetSearchProfile(context.Background()); !errors.Is(err, agentmodule.ErrInvalidSearchProfileResponse) {
		t.Fatalf("secret-bearing search response error = %v", err)
	}
}
