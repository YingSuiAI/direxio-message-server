package agentgrpc

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	agentv1 "github.com/YingSuiAI/dirextalk-agent/api/gen/dirextalk/agent/v1"
	agentmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agent"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (service *runtimeTestService) GetCapabilities(_ context.Context, request *agentv1.RuntimeServiceGetCapabilitiesRequest) (*agentv1.RuntimeServiceGetCapabilitiesResponse, error) {
	service.mu.Lock()
	callback := service.getCapabilities
	service.mu.Unlock()
	if callback != nil {
		return callback(request)
	}
	return validRuntimeCapabilities(), nil
}

func (service *runtimeTestService) GetRuntimeConfig(_ context.Context, request *agentv1.GetRuntimeConfigRequest) (*agentv1.GetRuntimeConfigResponse, error) {
	service.mu.Lock()
	service.runtimeConfigRequest = request
	callback := service.getRuntimeConfig
	service.mu.Unlock()
	if callback != nil {
		return callback(request)
	}
	return &agentv1.GetRuntimeConfigResponse{Config: validRemoteRuntimeConfig("deepseek-v4", 7)}, nil
}

func (service *runtimeTestService) PutRuntimeConfig(_ context.Context, request *agentv1.PutRuntimeConfigRequest) (*agentv1.PutRuntimeConfigResponse, error) {
	service.mu.Lock()
	service.putRuntimeRequest = request
	callback := service.putRuntimeConfig
	service.mu.Unlock()
	if callback != nil {
		return callback(request)
	}
	return &agentv1.PutRuntimeConfigResponse{Config: validRemoteRuntimeConfig(request.GetSpec().GetModelProfile().GetProfileId(), request.GetExpectedRevision()+1)}, nil
}

func TestRunnerRuntimeProfileGetAndUpdateBindOwnerAndPreserveAgentConfig(t *testing.T) {
	server := startRuntimeServer(t)
	server.service.putRuntimeConfig = func(request *agentv1.PutRuntimeConfigRequest) (*agentv1.PutRuntimeConfigResponse, error) {
		return validRuntimeConfigPutResponse(request), nil
	}
	runner := newTestRunner(t, server, Config{})

	state, err := runner.GetRuntimeProfile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !state.Available || !state.Configured || state.Revision != 7 || state.Profile == nil || state.Profile.ProfileID != "deepseek-v4" ||
		!reflect.DeepEqual(state.AvailableProfileIDs, []string{"deepseek-v4", "openai-default"}) {
		t.Fatalf("runtime profile state = %#v", state)
	}

	temperature, topP, maxOutputTokens := 0.4, 0.8, int64(2048)
	state, err = runner.UpdateRuntimeProfile(context.Background(), agentmodule.RuntimeProfileUpdate{
		IdempotencyKey: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", ProfileID: "openai-default", ExpectedRevision: 7,
		Temperature: &temperature, TopP: &topP, MaxOutputTokens: &maxOutputTokens,
	})
	if err != nil {
		t.Fatal(err)
	}
	server.service.mu.Lock()
	getRequest := server.service.runtimeConfigRequest
	putRequest := server.service.putRuntimeRequest
	server.service.mu.Unlock()
	if getRequest.GetOwnerId() != "owner-from-config" || putRequest.GetOwnerId() != "owner-from-config" ||
		putRequest.GetExpectedRevision() != 7 || putRequest.GetIdempotencyKey() != "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" {
		t.Fatalf("bound runtime requests get=%#v put=%#v", getRequest, putRequest)
	}
	spec := putRequest.GetSpec()
	profile := spec.GetModelProfile()
	if profile.GetProfileId() != "openai-default" || profile.GetProvider() != agentv1.ModelProvider_MODEL_PROVIDER_UNSPECIFIED ||
		profile.GetModel() != "" || profile.GetBaseUrl() != "" || profile.GetSecretRef() != "" ||
		profile.Temperature == nil || profile.GetTemperature() != temperature || profile.TopP == nil || profile.GetTopP() != topP || profile.GetMaxOutputTokens() != int32(maxOutputTokens) ||
		spec.GetProjectProfile() != "existing project" || spec.GetContextMessageLimit() != 48 || spec.GetMemoryMessageLimit() != 12 || spec.GetMaxSteps() != 24 ||
		!spec.GetMemoryDisabled() || !reflect.DeepEqual(spec.GetEnabledTools(), []string{"tool-a"}) ||
		!reflect.DeepEqual(spec.GetKnowledgeRefs(), []string{"knowledge-a"}) || !reflect.DeepEqual(spec.GetMcpServerIds(), []string{"mcp-a"}) ||
		!reflect.DeepEqual(spec.GetRecipeIds(), []string{"recipe-a"}) {
		t.Fatalf("profile update did not preserve non-profile state: %#v", spec)
	}
	if state.Revision != 8 || state.Profile == nil || state.Profile.ProfileID != "openai-default" {
		t.Fatalf("updated state = %#v", state)
	}
}

func TestRunnerRuntimeProfileInitializesExplicitProductBaseline(t *testing.T) {
	server := startRuntimeServer(t)
	server.service.getRuntimeConfig = func(*agentv1.GetRuntimeConfigRequest) (*agentv1.GetRuntimeConfigResponse, error) {
		return nil, status.Error(codes.NotFound, "not configured")
	}
	server.service.putRuntimeConfig = func(request *agentv1.PutRuntimeConfigRequest) (*agentv1.PutRuntimeConfigResponse, error) {
		return validRuntimeConfigPutResponse(request), nil
	}
	runner := newTestRunner(t, server, Config{})

	state, err := runner.GetRuntimeProfile(context.Background())
	if err != nil || !state.Available || state.Configured || state.Revision != 0 || state.Profile != nil {
		t.Fatalf("unconfigured state=%#v err=%v", state, err)
	}
	state, err = runner.UpdateRuntimeProfile(context.Background(), agentmodule.RuntimeProfileUpdate{
		IdempotencyKey: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", ProfileID: "deepseek-v4", ExpectedRevision: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	server.service.mu.Lock()
	putRequest := server.service.putRuntimeRequest
	server.service.mu.Unlock()
	if putRequest.GetSpec().GetContextMessageLimit() != 48 || putRequest.GetSpec().GetMemoryMessageLimit() != 12 || putRequest.GetSpec().GetMaxSteps() != 24 ||
		putRequest.GetExpectedRevision() != 0 || state.Revision != 1 {
		t.Fatalf("initial runtime profile request=%#v state=%#v", putRequest, state)
	}
}

func TestRunnerRuntimeProfileConflictAndNoopNeverMutate(t *testing.T) {
	zeroOutputTokens := int64(0)
	for name, request := range map[string]agentmodule.RuntimeProfileUpdate{
		"stale revision": {
			IdempotencyKey: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", ProfileID: "openai-default", ExpectedRevision: 6,
		},
		"unknown profile": {
			IdempotencyKey: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", ProfileID: "unknown-profile", ExpectedRevision: 7,
		},
		"zero output limit": {
			IdempotencyKey: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", ProfileID: "openai-default", ExpectedRevision: 7,
			MaxOutputTokens: &zeroOutputTokens,
		},
	} {
		t.Run(name, func(t *testing.T) {
			server := startRuntimeServer(t)
			runner := newTestRunner(t, server, Config{})
			_, err := runner.UpdateRuntimeProfile(context.Background(), request)
			if !errors.Is(err, agentmodule.ErrRuntimeProfileConflict) {
				t.Fatalf("UpdateRuntimeProfile() error = %v", err)
			}
			server.service.mu.Lock()
			putRequest := server.service.putRuntimeRequest
			server.service.mu.Unlock()
			if putRequest != nil {
				t.Fatalf("conflicting update reached PutRuntimeConfig: %#v", putRequest)
			}
		})
	}

	server := startRuntimeServer(t)
	runner := newTestRunner(t, server, Config{})
	state, err := runner.UpdateRuntimeProfile(context.Background(), agentmodule.RuntimeProfileUpdate{
		IdempotencyKey: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", ProfileID: "deepseek-v4", ExpectedRevision: 7,
	})
	if err != nil || state.Revision != 7 || state.Profile == nil || state.Profile.ProfileID != "deepseek-v4" {
		t.Fatalf("no-op state=%#v err=%v", state, err)
	}
	server.service.mu.Lock()
	putRequest := server.service.putRuntimeRequest
	server.service.mu.Unlock()
	if putRequest != nil {
		t.Fatalf("no-op update reached PutRuntimeConfig: %#v", putRequest)
	}
}

func TestRunnerRuntimeProfileSameSelectionPreservesUnspecifiedTuning(t *testing.T) {
	server := startRuntimeServer(t)
	existingTemperature, existingTopP := 0.3, 0.7
	server.service.getRuntimeConfig = func(*agentv1.GetRuntimeConfigRequest) (*agentv1.GetRuntimeConfigResponse, error) {
		config := validRemoteRuntimeConfig("deepseek-v4", 7)
		config.Spec.ModelProfile.Temperature = &existingTemperature
		config.Spec.ModelProfile.TopP = &existingTopP
		config.Spec.ModelProfile.MaxOutputTokens = 2048
		return &agentv1.GetRuntimeConfigResponse{Config: config}, nil
	}
	server.service.putRuntimeConfig = func(request *agentv1.PutRuntimeConfigRequest) (*agentv1.PutRuntimeConfigResponse, error) {
		return validRuntimeConfigPutResponse(request), nil
	}
	runner := newTestRunner(t, server, Config{})
	updatedTemperature := 0.5
	state, err := runner.UpdateRuntimeProfile(context.Background(), agentmodule.RuntimeProfileUpdate{
		IdempotencyKey: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", ProfileID: "deepseek-v4", ExpectedRevision: 7,
		Temperature: &updatedTemperature,
	})
	if err != nil {
		t.Fatal(err)
	}
	server.service.mu.Lock()
	putRequest := server.service.putRuntimeRequest
	server.service.mu.Unlock()
	profile := putRequest.GetSpec().GetModelProfile()
	if profile.GetProvider() != agentv1.ModelProvider_MODEL_PROVIDER_UNSPECIFIED || profile.GetModel() != "" || profile.GetBaseUrl() != "" || profile.GetSecretRef() != "" ||
		profile.Temperature == nil || profile.GetTemperature() != updatedTemperature || profile.TopP == nil || profile.GetTopP() != existingTopP ||
		profile.GetMaxOutputTokens() != 2048 || state.Profile == nil || state.Profile.TopP == nil || *state.Profile.TopP != existingTopP {
		t.Fatalf("same-profile tuning was not preserved: request=%#v state=%#v", profile, state)
	}
}

func TestRunnerRuntimeProfileRejectsInvalidCapabilitiesBeforeOwnerRead(t *testing.T) {
	for name, capabilities := range map[string]*agentv1.RuntimeCapabilities{
		"missing runtime config": {Chat: true, StreamChat: true, ModelProfileIds: []string{"deepseek-v4"}},
		"unsorted IDs":           {Chat: true, StreamChat: true, RuntimeConfig: true, ModelProfileIds: []string{"openai-default", "deepseek-v4"}},
		"duplicate IDs":          {Chat: true, StreamChat: true, RuntimeConfig: true, ModelProfileIds: []string{"deepseek-v4", "deepseek-v4"}},
		"secret-shaped ID":       {Chat: true, StreamChat: true, RuntimeConfig: true, ModelProfileIds: []string{"sk-aaaaaaaaaaaaaaaaaaaaaaaa"}},
	} {
		t.Run(name, func(t *testing.T) {
			server := startRuntimeServer(t)
			server.service.getCapabilities = func(*agentv1.RuntimeServiceGetCapabilitiesRequest) (*agentv1.RuntimeServiceGetCapabilitiesResponse, error) {
				return &agentv1.RuntimeServiceGetCapabilitiesResponse{Capabilities: capabilities}, nil
			}
			runner := newTestRunner(t, server, Config{})
			_, err := runner.GetRuntimeProfile(context.Background())
			if !errors.Is(err, agentmodule.ErrInvalidRuntimeProfileResponse) {
				t.Fatalf("GetRuntimeProfile() error = %v", err)
			}
			server.service.mu.Lock()
			getRequest := server.service.runtimeConfigRequest
			server.service.mu.Unlock()
			if getRequest != nil {
				t.Fatalf("invalid capabilities reached GetRuntimeConfig: %#v", getRequest)
			}
		})
	}
}

func TestRunnerRuntimeProfileRejectsSecretOrForeignUpstreamState(t *testing.T) {
	for name, mutate := range map[string]func(*agentv1.RuntimeConfig){
		"secret ref":    func(config *agentv1.RuntimeConfig) { config.Spec.ModelProfile.SecretRef = "mounted:must-not-cross" },
		"foreign owner": func(config *agentv1.RuntimeConfig) { config.OwnerId = "foreign-owner" },
		"unknown ID":    func(config *agentv1.RuntimeConfig) { config.Spec.ModelProfile.ProfileId = "unknown-profile" },
		"insecure URL":  func(config *agentv1.RuntimeConfig) { config.Spec.ModelProfile.BaseUrl = "http://model.example/v1" },
	} {
		t.Run(name, func(t *testing.T) {
			server := startRuntimeServer(t)
			server.service.getRuntimeConfig = func(*agentv1.GetRuntimeConfigRequest) (*agentv1.GetRuntimeConfigResponse, error) {
				config := validRemoteRuntimeConfig("deepseek-v4", 7)
				mutate(config)
				return &agentv1.GetRuntimeConfigResponse{Config: config}, nil
			}
			runner := newTestRunner(t, server, Config{})
			_, err := runner.GetRuntimeProfile(context.Background())
			if !errors.Is(err, agentmodule.ErrInvalidRuntimeProfileResponse) || strings.Contains(err.Error(), "must-not-cross") || strings.Contains(err.Error(), "foreign-owner") {
				t.Fatalf("invalid upstream error = %v", err)
			}
		})
	}
}

func validRuntimeCapabilities() *agentv1.RuntimeServiceGetCapabilitiesResponse {
	return &agentv1.RuntimeServiceGetCapabilitiesResponse{Capabilities: &agentv1.RuntimeCapabilities{
		Chat: true, StreamChat: true, RuntimeConfig: true, ModelProfileIds: []string{"deepseek-v4", "openai-default"},
	}}
}

func validRemoteRuntimeConfig(profileID string, revision int64) *agentv1.RuntimeConfig {
	provider := agentv1.ModelProvider_MODEL_PROVIDER_DEEPSEEK
	model := "deepseekv4-pro"
	baseURL := "https://api.deepseek.example/v1"
	if profileID == "openai-default" {
		provider = agentv1.ModelProvider_MODEL_PROVIDER_OPENAI_COMPATIBLE
		model = "gpt-compatible"
		baseURL = "https://api.openai.example/v1"
	}
	return &agentv1.RuntimeConfig{
		OwnerId: "owner-from-config", Revision: revision,
		Spec: &agentv1.RuntimeConfigSpec{
			ModelProfile: &agentv1.ModelProfile{
				ProfileId: profileID, Provider: provider, Model: model, BaseUrl: baseURL,
				MaxOutputTokens: 4096, ContextWindow: 65536,
			},
			ProjectProfile: "existing project", ContextMessageLimit: 48, MemoryMessageLimit: 12, MaxSteps: 24,
			MemoryDisabled: true, EnabledTools: []string{"tool-a"}, KnowledgeRefs: []string{"knowledge-a"},
			McpServerIds: []string{"mcp-a"}, RecipeIds: []string{"recipe-a"},
		},
	}
}

func validRuntimeConfigPutResponse(request *agentv1.PutRuntimeConfigRequest) *agentv1.PutRuntimeConfigResponse {
	response := validRemoteRuntimeConfig(request.GetSpec().GetModelProfile().GetProfileId(), request.GetExpectedRevision()+1)
	response.Spec.ModelProfile.Temperature = request.GetSpec().GetModelProfile().Temperature
	response.Spec.ModelProfile.TopP = request.GetSpec().GetModelProfile().TopP
	if request.GetSpec().GetModelProfile().GetMaxOutputTokens() > 0 {
		response.Spec.ModelProfile.MaxOutputTokens = request.GetSpec().GetModelProfile().GetMaxOutputTokens()
	}
	response.Spec.ProjectProfile = request.GetSpec().GetProjectProfile()
	response.Spec.ContextMessageLimit = request.GetSpec().GetContextMessageLimit()
	response.Spec.MemoryMessageLimit = request.GetSpec().GetMemoryMessageLimit()
	response.Spec.MaxSteps = request.GetSpec().GetMaxSteps()
	response.Spec.MemoryDisabled = request.GetSpec().GetMemoryDisabled()
	response.Spec.EnabledTools = append([]string(nil), request.GetSpec().GetEnabledTools()...)
	response.Spec.KnowledgeRefs = append([]string(nil), request.GetSpec().GetKnowledgeRefs()...)
	response.Spec.McpServerIds = append([]string(nil), request.GetSpec().GetMcpServerIds()...)
	response.Spec.RecipeIds = append([]string(nil), request.GetSpec().GetRecipeIds()...)
	return &agentv1.PutRuntimeConfigResponse{Config: response}
}
