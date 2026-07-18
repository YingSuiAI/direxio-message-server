package agentgrpc

import (
	"context"
	"errors"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strings"

	agentv1 "github.com/YingSuiAI/dirextalk-agent/api/gen/dirextalk/agent/v1"
	agentmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agent"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const (
	initialRuntimeContextMessageLimit = 48
	initialRuntimeMemoryMessageLimit  = 12
	initialRuntimeMaxSteps            = 24
)

var runtimeProfileIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// GetRuntimeProfile reads the independent Agent's immutable public catalog
// identifiers and this Message Server owner's durable de-secreted selection.
func (runner *Runner) GetRuntimeProfile(ctx context.Context) (agentmodule.RuntimeProfileState, error) {
	if runner == nil || runner.runtime == nil {
		return agentmodule.RuntimeProfileState{}, agentmodule.ErrRuntimeProfileUnavailable
	}
	callContext, cancel := context.WithTimeout(ctx, runner.chainTimeout)
	defer cancel()
	state, _, err := runner.loadRuntimeProfile(callContext)
	return state, err
}

// UpdateRuntimeProfile applies only an owner-selected immutable catalog ID and
// bounded model overrides. All unrelated Agent-owned runtime configuration is
// preserved through one revision-fenced full-spec PutRuntimeConfig call.
func (runner *Runner) UpdateRuntimeProfile(ctx context.Context, request agentmodule.RuntimeProfileUpdate) (agentmodule.RuntimeProfileState, error) {
	if runner == nil || runner.runtime == nil {
		return agentmodule.RuntimeProfileState{}, agentmodule.ErrRuntimeProfileUnavailable
	}
	if !validRuntimeProfileUpdate(request) {
		return agentmodule.RuntimeProfileState{}, agentmodule.ErrRuntimeProfileConflict
	}
	callContext, cancel := context.WithTimeout(ctx, runner.chainTimeout)
	defer cancel()
	state, current, err := runner.loadRuntimeProfile(callContext)
	if err != nil {
		return agentmodule.RuntimeProfileState{}, err
	}
	if !containsSortedString(state.AvailableProfileIDs, request.ProfileID) || state.Revision != request.ExpectedRevision {
		return agentmodule.RuntimeProfileState{}, agentmodule.ErrRuntimeProfileConflict
	}
	if state.Configured && state.Profile != nil && state.Profile.ProfileID == request.ProfileID &&
		request.Temperature == nil && request.TopP == nil && request.MaxOutputTokens == nil {
		return state, nil
	}

	spec := initialRuntimeSpec(request.ProfileID)
	if current != nil {
		spec = proto.Clone(current.GetSpec()).(*agentv1.RuntimeConfigSpec)
		spec.ModelProfile = selectedRuntimeModelProfile(current.GetSpec().GetModelProfile(), request.ProfileID)
	}
	applyRuntimeProfileOverrides(spec.ModelProfile, request)
	response, err := runner.runtime.PutRuntimeConfig(callContext, &agentv1.PutRuntimeConfigRequest{
		IdempotencyKey: request.IdempotencyKey, OwnerId: runner.ownerID,
		ExpectedRevision: request.ExpectedRevision, Spec: spec,
	})
	if err != nil {
		return agentmodule.RuntimeProfileState{}, mapRuntimeProfileRPCError(callContext, err)
	}
	updated, mappedConfig, err := mapRuntimeProfileConfig(response.GetConfig(), runner.ownerID, state.AvailableProfileIDs)
	if err != nil || mappedConfig == nil || updated.Revision != request.ExpectedRevision+1 || updated.Profile == nil || updated.Profile.ProfileID != request.ProfileID ||
		!sameRuntimeNonProfileSpec(spec, mappedConfig.GetSpec()) || !runtimeProfileSelectionMatches(updated.Profile, spec.GetModelProfile()) {
		return agentmodule.RuntimeProfileState{}, agentmodule.ErrInvalidRuntimeProfileResponse
	}
	updated.Available = true
	updated.AvailableProfileIDs = append([]string(nil), state.AvailableProfileIDs...)
	return updated, nil
}

func runtimeProfileSelectionMatches(profile *agentmodule.RuntimeProfile, selection *agentv1.ModelProfile) bool {
	if profile == nil || selection == nil || !sameOptionalRuntimeFloat(profile.Temperature, selection.Temperature) ||
		!sameOptionalRuntimeFloat(profile.TopP, selection.TopP) {
		return false
	}
	return selection.GetMaxOutputTokens() == 0 || profile.MaxOutputTokens == int64(selection.GetMaxOutputTokens())
}

func sameOptionalRuntimeFloat(left, right *float64) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}

func (runner *Runner) loadRuntimeProfile(ctx context.Context) (agentmodule.RuntimeProfileState, *agentv1.RuntimeConfig, error) {
	capabilitiesResponse, err := runner.runtime.GetCapabilities(ctx, &agentv1.RuntimeServiceGetCapabilitiesRequest{})
	if err != nil {
		return agentmodule.RuntimeProfileState{}, nil, mapRuntimeProfileRPCError(ctx, err)
	}
	profileIDs, err := mapRuntimeProfileCapabilities(capabilitiesResponse)
	if err != nil {
		return agentmodule.RuntimeProfileState{}, nil, err
	}
	response, err := runner.runtime.GetRuntimeConfig(ctx, &agentv1.GetRuntimeConfigRequest{OwnerId: runner.ownerID})
	if status.Code(err) == codes.NotFound {
		return agentmodule.RuntimeProfileState{
			Available: true, AvailableProfileIDs: profileIDs,
		}, nil, nil
	}
	if err != nil {
		return agentmodule.RuntimeProfileState{}, nil, mapRuntimeProfileRPCError(ctx, err)
	}
	state, config, err := mapRuntimeProfileConfig(response.GetConfig(), runner.ownerID, profileIDs)
	if err != nil {
		return agentmodule.RuntimeProfileState{}, nil, err
	}
	state.Available = true
	state.AvailableProfileIDs = profileIDs
	return state, config, nil
}

func mapRuntimeProfileCapabilities(response *agentv1.RuntimeServiceGetCapabilitiesResponse) ([]string, error) {
	capabilities := response.GetCapabilities()
	if capabilities == nil || !capabilities.GetRuntimeConfig() || !capabilities.GetChat() || !capabilities.GetStreamChat() {
		return nil, agentmodule.ErrInvalidRuntimeProfileResponse
	}
	ids := append([]string(nil), capabilities.GetModelProfileIds()...)
	if len(ids) == 0 || len(ids) > 128 || !sort.StringsAreSorted(ids) {
		return nil, agentmodule.ErrInvalidRuntimeProfileResponse
	}
	for index, id := range ids {
		if !runtimeProfileIDPattern.MatchString(id) || agentmodule.ContainsSensitiveRuntimeProfileMaterial(id) || (index > 0 && ids[index-1] == id) {
			return nil, agentmodule.ErrInvalidRuntimeProfileResponse
		}
	}
	return ids, nil
}

func mapRuntimeProfileConfig(config *agentv1.RuntimeConfig, ownerID string, profileIDs []string) (agentmodule.RuntimeProfileState, *agentv1.RuntimeConfig, error) {
	if config == nil || config.GetSpec() == nil || config.GetSpec().GetModelProfile() == nil || config.GetOwnerId() != ownerID || config.GetRevision() < 1 {
		return agentmodule.RuntimeProfileState{}, nil, agentmodule.ErrInvalidRuntimeProfileResponse
	}
	spec := config.GetSpec()
	profile := spec.GetModelProfile()
	provider := runtimeProfileProvider(profile.GetProvider())
	baseURL := strings.TrimSpace(profile.GetBaseUrl())
	parsedURL, urlErr := url.Parse(baseURL)
	if provider == "" || !containsSortedString(profileIDs, profile.GetProfileId()) || profile.GetSecretRef() != "" ||
		strings.TrimSpace(profile.GetModel()) == "" || strings.TrimSpace(profile.GetModel()) != profile.GetModel() || len(profile.GetModel()) > 512 ||
		urlErr != nil || parsedURL.Scheme != "https" || parsedURL.Host == "" || parsedURL.User != nil || parsedURL.Opaque != "" || parsedURL.RawQuery != "" || parsedURL.Fragment != "" ||
		baseURL != profile.GetBaseUrl() || profile.GetContextWindow() < 1 || profile.GetMaxOutputTokens() < 1 || profile.GetMaxOutputTokens() > profile.GetContextWindow() ||
		len(profile.GetReasoningEffort()) > 128 || strings.ContainsAny(profile.GetReasoningEffort(), "\r\n\t") ||
		!validRuntimeFloat(profile.Temperature, 0, 2) || !validRuntimeFloat(profile.TopP, 0, 1) || !validRuntimeNonProfileSpec(spec) {
		return agentmodule.RuntimeProfileState{}, nil, agentmodule.ErrInvalidRuntimeProfileResponse
	}
	for _, value := range []string{profile.GetProfileId(), provider, profile.GetModel(), baseURL, profile.GetReasoningEffort(), spec.GetProjectProfile()} {
		if agentmodule.ContainsSensitiveRuntimeProfileMaterial(value) {
			return agentmodule.RuntimeProfileState{}, nil, agentmodule.ErrInvalidRuntimeProfileResponse
		}
	}
	result := agentmodule.RuntimeProfile{
		ProfileID: profile.GetProfileId(), Provider: provider, Model: profile.GetModel(), BaseURL: baseURL,
		MaxOutputTokens: int64(profile.GetMaxOutputTokens()), ContextWindow: int64(profile.GetContextWindow()), ReasoningEffort: profile.GetReasoningEffort(),
	}
	if profile.Temperature != nil {
		value := profile.GetTemperature()
		result.Temperature = &value
	}
	if profile.TopP != nil {
		value := profile.GetTopP()
		result.TopP = &value
	}
	return agentmodule.RuntimeProfileState{
		Configured: true, Revision: config.GetRevision(), Profile: &result,
	}, config, nil
}

func validRuntimeNonProfileSpec(spec *agentv1.RuntimeConfigSpec) bool {
	if spec.GetContextMessageLimit() < 1 || spec.GetContextMessageLimit() > 4096 || spec.GetMemoryMessageLimit() < 1 || spec.GetMemoryMessageLimit() > 4096 ||
		spec.GetMaxSteps() < 1 || spec.GetMaxSteps() > 120 || len(spec.GetProjectProfile()) > 64*1024 {
		return false
	}
	for _, values := range [][]string{spec.GetEnabledTools(), spec.GetKnowledgeRefs(), spec.GetMcpServerIds(), spec.GetRecipeIds()} {
		if len(values) > 512 || !sort.StringsAreSorted(values) {
			return false
		}
		for index, value := range values {
			if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value || len(value) > 512 || strings.ContainsAny(value, "\r\n\t") ||
				agentmodule.ContainsSensitiveRuntimeProfileMaterial(value) || (index > 0 && values[index-1] == value) {
				return false
			}
		}
	}
	return !agentmodule.ContainsSensitiveRuntimeProfileMaterial(spec.GetProjectProfile())
}

func validRuntimeFloat(value *float64, minimum, maximum float64) bool {
	if value == nil {
		return true
	}
	return !math.IsNaN(*value) && !math.IsInf(*value, 0) && *value >= minimum && *value <= maximum
}

func validRuntimeProfileUpdate(request agentmodule.RuntimeProfileUpdate) bool {
	parsedID, err := uuid.Parse(request.IdempotencyKey)
	if err != nil || parsedID == uuid.Nil || parsedID.String() != request.IdempotencyKey || !runtimeProfileIDPattern.MatchString(request.ProfileID) ||
		agentmodule.ContainsSensitiveRuntimeProfileMaterial(request.ProfileID) || request.ExpectedRevision < 0 || request.ExpectedRevision == math.MaxInt64 ||
		!validRuntimeFloat(request.Temperature, 0, 2) || !validRuntimeFloat(request.TopP, 0, 1) {
		return false
	}
	return request.MaxOutputTokens == nil || (*request.MaxOutputTokens >= 1 && *request.MaxOutputTokens <= 10_000_000)
}

func initialRuntimeSpec(profileID string) *agentv1.RuntimeConfigSpec {
	return &agentv1.RuntimeConfigSpec{
		ModelProfile:        &agentv1.ModelProfile{ProfileId: profileID},
		ContextMessageLimit: initialRuntimeContextMessageLimit,
		MemoryMessageLimit:  initialRuntimeMemoryMessageLimit,
		MaxSteps:            initialRuntimeMaxSteps,
		EnabledTools:        []string{},
		KnowledgeRefs:       []string{},
		McpServerIds:        []string{},
		RecipeIds:           []string{},
	}
}

func selectedRuntimeModelProfile(current *agentv1.ModelProfile, profileID string) *agentv1.ModelProfile {
	selected := &agentv1.ModelProfile{ProfileId: profileID}
	if current == nil || current.GetProfileId() != profileID {
		return selected
	}
	if current.Temperature != nil {
		value := current.GetTemperature()
		selected.Temperature = &value
	}
	if current.TopP != nil {
		value := current.GetTopP()
		selected.TopP = &value
	}
	selected.MaxOutputTokens = current.GetMaxOutputTokens()
	return selected
}

func applyRuntimeProfileOverrides(profile *agentv1.ModelProfile, request agentmodule.RuntimeProfileUpdate) {
	if request.Temperature != nil {
		value := *request.Temperature
		profile.Temperature = &value
	}
	if request.TopP != nil {
		value := *request.TopP
		profile.TopP = &value
	}
	if request.MaxOutputTokens != nil {
		profile.MaxOutputTokens = int32(*request.MaxOutputTokens)
	}
}

func sameRuntimeNonProfileSpec(expected, actual *agentv1.RuntimeConfigSpec) bool {
	if expected == nil || actual == nil {
		return false
	}
	left := proto.Clone(expected).(*agentv1.RuntimeConfigSpec)
	right := proto.Clone(actual).(*agentv1.RuntimeConfigSpec)
	left.ModelProfile = nil
	right.ModelProfile = nil
	return proto.Equal(left, right)
}

func containsSortedString(values []string, target string) bool {
	index := sort.SearchStrings(values, target)
	return index < len(values) && values[index] == target
}

func runtimeProfileProvider(provider agentv1.ModelProvider) string {
	switch provider {
	case agentv1.ModelProvider_MODEL_PROVIDER_OPENAI_COMPATIBLE:
		return "openai_compatible"
	case agentv1.ModelProvider_MODEL_PROVIDER_DEEPSEEK:
		return "deepseek"
	case agentv1.ModelProvider_MODEL_PROVIDER_ANTHROPIC:
		return "anthropic"
	default:
		return ""
	}
}

func mapRuntimeProfileRPCError(ctx context.Context, err error) error {
	if ctx == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return agentmodule.ErrRuntimeProfileUnavailable
	}
	switch status.Code(err) {
	case codes.Aborted, codes.AlreadyExists, codes.FailedPrecondition, codes.InvalidArgument:
		return agentmodule.ErrRuntimeProfileConflict
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		return agentmodule.ErrRuntimeProfileUnavailable
	default:
		return agentmodule.ErrInvalidRuntimeProfileResponse
	}
}
