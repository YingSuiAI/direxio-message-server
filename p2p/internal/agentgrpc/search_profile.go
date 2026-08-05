package agentgrpc

import (
	"context"
	"errors"
	"math"
	"net/url"
	"sort"
	"strings"

	agentv1 "github.com/YingSuiAI/dirextalk-agent/api/gen/dirextalk/agent/v1"
	agentmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agent"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const nativeWebSearchToolName = "web_search"

func (runner *Runner) GetSearchProfile(ctx context.Context) (agentmodule.SearchProfileState, error) {
	if runner == nil || runner.runtime == nil {
		return agentmodule.SearchProfileState{}, agentmodule.ErrSearchProfileUnavailable
	}
	callContext, cancel := context.WithTimeout(ctx, runner.chainTimeout)
	defer cancel()
	state, _, _, err := runner.loadSearchProfile(callContext)
	return state, err
}

func (runner *Runner) UpdateSearchProfile(ctx context.Context, request agentmodule.SearchProfileUpdate) (agentmodule.SearchProfileState, error) {
	if runner == nil || runner.runtime == nil {
		return agentmodule.SearchProfileState{}, agentmodule.ErrSearchProfileUnavailable
	}
	if !validSearchProfileUpdate(request) {
		return agentmodule.SearchProfileState{}, agentmodule.ErrSearchProfileConflict
	}
	callContext, cancel := context.WithTimeout(ctx, runner.chainTimeout)
	defer cancel()
	state, current, modelProfileIDs, err := runner.loadSearchProfile(callContext)
	if err != nil {
		return agentmodule.SearchProfileState{}, err
	}
	if !state.Available || current == nil || !containsSortedString(state.AvailableProfileIDs, request.ProfileID) ||
		state.Revision != request.ExpectedRevision {
		return agentmodule.SearchProfileState{}, agentmodule.ErrSearchProfileConflict
	}
	if state.Configured && state.Profile != nil && state.Profile.ProfileID == request.ProfileID &&
		request.MaxResults == nil && request.TimeoutSeconds == nil {
		return state, nil
	}
	spec := proto.Clone(current.GetSpec()).(*agentv1.RuntimeConfigSpec)
	spec.SearchProfile = selectedSearchProfile(current.GetSpec().GetSearchProfile(), request.ProfileID)
	applySearchProfileOverrides(spec.SearchProfile, request)
	spec.EnabledTools = appendUniqueSortedString(spec.GetEnabledTools(), nativeWebSearchToolName)
	response, err := runner.runtime.PutRuntimeConfig(callContext, &agentv1.PutRuntimeConfigRequest{
		IdempotencyKey: request.IdempotencyKey, OwnerId: runner.ownerID,
		ExpectedRevision: request.ExpectedRevision, Spec: spec,
	})
	if err != nil {
		return agentmodule.SearchProfileState{}, mapSearchProfileRPCError(callContext, err)
	}
	updated, mappedConfig, err := mapSearchProfileConfig(
		response.GetConfig(), runner.ownerID, modelProfileIDs, state.AvailableProfileIDs,
	)
	if err != nil || mappedConfig == nil || updated.Revision != request.ExpectedRevision+1 ||
		updated.Profile == nil || updated.Profile.ProfileID != request.ProfileID ||
		!sameRuntimeNonSearchSpec(spec, mappedConfig.GetSpec()) ||
		!searchProfileSelectionMatches(updated.Profile, spec.GetSearchProfile()) {
		return agentmodule.SearchProfileState{}, agentmodule.ErrInvalidSearchProfileResponse
	}
	updated.Available = true
	updated.AvailableProfileIDs = append([]string(nil), state.AvailableProfileIDs...)
	return updated, nil
}

func appendUniqueSortedString(values []string, target string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	for _, value := range result {
		if value == target {
			return result
		}
	}
	result = append(result, target)
	sort.Strings(result)
	return result
}

func (runner *Runner) loadSearchProfile(ctx context.Context) (agentmodule.SearchProfileState, *agentv1.RuntimeConfig, []string, error) {
	capabilitiesResponse, err := runner.runtime.GetCapabilities(ctx, &agentv1.RuntimeServiceGetCapabilitiesRequest{})
	if err != nil {
		return agentmodule.SearchProfileState{}, nil, nil, mapSearchProfileRPCError(ctx, err)
	}
	modelProfileIDs, err := mapRuntimeProfileCapabilities(capabilitiesResponse)
	if err != nil {
		return agentmodule.SearchProfileState{}, nil, nil, agentmodule.ErrInvalidSearchProfileResponse
	}
	profileIDs, err := mapSearchProfileCapabilities(capabilitiesResponse)
	if err != nil {
		return agentmodule.SearchProfileState{}, nil, nil, err
	}
	if len(profileIDs) == 0 {
		return agentmodule.SearchProfileState{AvailableProfileIDs: []string{}}, nil, modelProfileIDs, nil
	}
	response, err := runner.runtime.GetRuntimeConfig(ctx, &agentv1.GetRuntimeConfigRequest{OwnerId: runner.ownerID})
	if status.Code(err) == codes.NotFound {
		return agentmodule.SearchProfileState{
			Available: true, AvailableProfileIDs: profileIDs,
		}, nil, modelProfileIDs, nil
	}
	if err != nil {
		return agentmodule.SearchProfileState{}, nil, nil, mapSearchProfileRPCError(ctx, err)
	}
	state, config, err := mapSearchProfileConfig(response.GetConfig(), runner.ownerID, modelProfileIDs, profileIDs)
	if err != nil {
		return agentmodule.SearchProfileState{}, nil, nil, err
	}
	state.Available = true
	state.AvailableProfileIDs = profileIDs
	return state, config, modelProfileIDs, nil
}

func mapSearchProfileCapabilities(response *agentv1.RuntimeServiceGetCapabilitiesResponse) ([]string, error) {
	capabilities := response.GetCapabilities()
	if capabilities == nil {
		return nil, agentmodule.ErrInvalidSearchProfileResponse
	}
	ids := append([]string(nil), capabilities.GetSearchProfileIds()...)
	if len(ids) == 0 {
		return []string{}, nil
	}
	if len(ids) > 128 || !sort.StringsAreSorted(ids) {
		return nil, agentmodule.ErrInvalidSearchProfileResponse
	}
	for index, id := range ids {
		if !runtimeProfileIDPattern.MatchString(id) || agentmodule.ContainsSensitiveRuntimeProfileMaterial(id) ||
			(index > 0 && ids[index-1] == id) {
			return nil, agentmodule.ErrInvalidSearchProfileResponse
		}
	}
	return ids, nil
}

func mapSearchProfileConfig(config *agentv1.RuntimeConfig, ownerID string, modelProfileIDs, profileIDs []string) (agentmodule.SearchProfileState, *agentv1.RuntimeConfig, error) {
	if config == nil || config.GetSpec() == nil || config.GetSpec().GetModelProfile() == nil ||
		config.GetOwnerId() != ownerID || config.GetRevision() < 1 {
		return agentmodule.SearchProfileState{}, nil, agentmodule.ErrInvalidSearchProfileResponse
	}
	if _, _, err := mapRuntimeProfileConfig(config, ownerID, modelProfileIDs); err != nil {
		return agentmodule.SearchProfileState{}, nil, agentmodule.ErrInvalidSearchProfileResponse
	}
	search := config.GetSpec().GetSearchProfile()
	if search == nil {
		return agentmodule.SearchProfileState{Revision: config.GetRevision()}, config, nil
	}
	provider := searchProfileProvider(search.GetProvider())
	if provider == "" || !containsSortedString(profileIDs, search.GetProfileId()) ||
		!validDeSecretedSearchProfile(search) {
		return agentmodule.SearchProfileState{}, nil, agentmodule.ErrInvalidSearchProfileResponse
	}
	profile := &agentmodule.SearchProfile{
		ProfileID: search.GetProfileId(), Provider: provider, BaseURL: search.GetBaseUrl(),
		MaxResults: int64(search.GetMaxResults()), TimeoutSeconds: int64(search.GetTimeoutSeconds()),
	}
	return agentmodule.SearchProfileState{
		Configured: true, Revision: config.GetRevision(), Profile: profile,
	}, config, nil
}

func validDeSecretedSearchProfile(profile *agentv1.SearchProfile) bool {
	if profile == nil {
		return true
	}
	provider := searchProfileProvider(profile.GetProvider())
	baseURL := strings.TrimSpace(profile.GetBaseUrl())
	parsedURL, err := url.Parse(baseURL)
	if provider == "" || !runtimeProfileIDPattern.MatchString(profile.GetProfileId()) ||
		profile.GetSecretRef() != "" || baseURL != profile.GetBaseUrl() || len(baseURL) > 2048 ||
		err != nil || parsedURL.Scheme != "https" || parsedURL.Host == "" ||
		parsedURL.User != nil || parsedURL.Opaque != "" || parsedURL.RawQuery != "" || parsedURL.Fragment != "" ||
		profile.GetMaxResults() < 1 || profile.GetMaxResults() > 50 ||
		profile.GetTimeoutSeconds() < 1 || profile.GetTimeoutSeconds() > 60 {
		return false
	}
	for _, value := range []string{profile.GetProfileId(), provider, baseURL} {
		if agentmodule.ContainsSensitiveRuntimeProfileMaterial(value) {
			return false
		}
	}
	return true
}

func validSearchProfileUpdate(request agentmodule.SearchProfileUpdate) bool {
	parsedID, err := uuid.Parse(request.IdempotencyKey)
	if err != nil || parsedID == uuid.Nil || parsedID.String() != request.IdempotencyKey ||
		!runtimeProfileIDPattern.MatchString(request.ProfileID) ||
		agentmodule.ContainsSensitiveRuntimeProfileMaterial(request.ProfileID) ||
		request.ExpectedRevision < 0 || request.ExpectedRevision == math.MaxInt64 {
		return false
	}
	return (request.MaxResults == nil || (*request.MaxResults >= 1 && *request.MaxResults <= 50)) &&
		(request.TimeoutSeconds == nil || (*request.TimeoutSeconds >= 1 && *request.TimeoutSeconds <= 60))
}

func selectedSearchProfile(current *agentv1.SearchProfile, profileID string) *agentv1.SearchProfile {
	selected := &agentv1.SearchProfile{ProfileId: profileID}
	if current == nil || current.GetProfileId() != profileID {
		return selected
	}
	selected.MaxResults = current.GetMaxResults()
	selected.TimeoutSeconds = current.GetTimeoutSeconds()
	return selected
}

func applySearchProfileOverrides(profile *agentv1.SearchProfile, request agentmodule.SearchProfileUpdate) {
	if request.MaxResults != nil {
		profile.MaxResults = int32(*request.MaxResults)
	}
	if request.TimeoutSeconds != nil {
		profile.TimeoutSeconds = int32(*request.TimeoutSeconds)
	}
}

func sameRuntimeNonSearchSpec(expected, actual *agentv1.RuntimeConfigSpec) bool {
	if expected == nil || actual == nil {
		return false
	}
	left := proto.Clone(expected).(*agentv1.RuntimeConfigSpec)
	right := proto.Clone(actual).(*agentv1.RuntimeConfigSpec)
	left.SearchProfile = nil
	right.SearchProfile = nil
	return proto.Equal(left, right)
}

func searchProfileSelectionMatches(profile *agentmodule.SearchProfile, selection *agentv1.SearchProfile) bool {
	if profile == nil || selection == nil {
		return false
	}
	return (selection.GetMaxResults() == 0 || profile.MaxResults == int64(selection.GetMaxResults())) &&
		(selection.GetTimeoutSeconds() == 0 || profile.TimeoutSeconds == int64(selection.GetTimeoutSeconds()))
}

func searchProfileProvider(provider agentv1.SearchProvider) string {
	switch provider {
	case agentv1.SearchProvider_SEARCH_PROVIDER_TAVILY:
		return "tavily"
	case agentv1.SearchProvider_SEARCH_PROVIDER_BRAVE:
		return "brave"
	case agentv1.SearchProvider_SEARCH_PROVIDER_EXA:
		return "exa"
	case agentv1.SearchProvider_SEARCH_PROVIDER_SERPER:
		return "serper"
	case agentv1.SearchProvider_SEARCH_PROVIDER_DEEPSEEK_NATIVE:
		return "deepseek_native"
	default:
		return ""
	}
}

func mapSearchProfileRPCError(ctx context.Context, err error) error {
	if ctx == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return agentmodule.ErrSearchProfileUnavailable
	}
	switch status.Code(err) {
	case codes.Aborted, codes.AlreadyExists, codes.FailedPrecondition, codes.InvalidArgument:
		return agentmodule.ErrSearchProfileConflict
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		return agentmodule.ErrSearchProfileUnavailable
	default:
		return agentmodule.ErrInvalidSearchProfileResponse
	}
}
