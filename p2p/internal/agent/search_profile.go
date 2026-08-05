package agent

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"

	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	"github.com/google/uuid"
)

const (
	actionSearchProfileGet    = "agent.search.profile.get"
	actionSearchProfileUpdate = "agent.search.profile.update"
)

var (
	ErrSearchProfileUnavailable     = errors.New("Agent search profile is unavailable")
	ErrSearchProfileConflict        = errors.New("Agent search profile revision conflicts")
	ErrInvalidSearchProfileResponse = errors.New("Agent search profile response is invalid")
)

// SearchProfile is the de-secreted public projection of one server-controlled
// search provider profile. Credential references are intentionally absent.
type SearchProfile struct {
	ProfileID      string
	Provider       string
	BaseURL        string
	MaxResults     int64
	TimeoutSeconds int64
}

type SearchProfileState struct {
	Available           bool
	Configured          bool
	Revision            int64
	AvailableProfileIDs []string
	Profile             *SearchProfile
}

// SearchProfileUpdate accepts only a catalog ID and limits bounded by that
// catalog. Provider endpoints and credentials cannot be supplied by clients.
type SearchProfileUpdate struct {
	IdempotencyKey   string
	ProfileID        string
	ExpectedRevision int64
	MaxResults       *int64
	TimeoutSeconds   *int64
}

type SearchProfileClient interface {
	GetSearchProfile(context.Context) (SearchProfileState, error)
	UpdateSearchProfile(context.Context, SearchProfileUpdate) (SearchProfileState, error)
}

func (m *Module) getSearchProfile(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if len(params) != 0 {
		return nil, actionbase.BadRequest("search profile get does not accept parameters")
	}
	if m == nil || m.searchProfiles == nil {
		return unavailableSearchProfileState().Response(), nil
	}
	state, err := m.searchProfiles.GetSearchProfile(ctx)
	if err != nil {
		return nil, searchProfileActionError(err)
	}
	if err := validateSearchProfileState(state); err != nil {
		return nil, searchProfileActionError(err)
	}
	return state.Response(), nil
}

func (m *Module) updateSearchProfile(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	request, err := parseSearchProfileUpdate(params)
	if err != nil {
		return nil, actionbase.BadRequest("invalid Agent search profile update")
	}
	if m == nil || m.searchProfiles == nil {
		return nil, searchProfileActionError(ErrSearchProfileUnavailable)
	}
	state, err := m.searchProfiles.UpdateSearchProfile(ctx, request)
	if err != nil {
		return nil, searchProfileActionError(err)
	}
	if err := validateSearchProfileState(state); err != nil {
		return nil, searchProfileActionError(err)
	}
	return state.Response(), nil
}

func parseSearchProfileUpdate(params map[string]any) (SearchProfileUpdate, error) {
	if params == nil {
		return SearchProfileUpdate{}, errors.New("missing params")
	}
	allowed := map[string]struct{}{
		"idempotency_key": {}, "profile_id": {}, "expected_revision": {},
		"max_results": {}, "timeout_seconds": {},
	}
	for key := range params {
		if _, ok := allowed[key]; !ok {
			return SearchProfileUpdate{}, errors.New("unknown field")
		}
	}
	idempotencyKey, ok := params["idempotency_key"].(string)
	parsedID, err := uuid.Parse(idempotencyKey)
	if !ok || strings.TrimSpace(idempotencyKey) != idempotencyKey || err != nil ||
		parsedID == uuid.Nil || parsedID.String() != idempotencyKey {
		return SearchProfileUpdate{}, errors.New("invalid idempotency key")
	}
	profileID, ok := params["profile_id"].(string)
	if !ok || strings.TrimSpace(profileID) != profileID || !runtimeProfileIDPattern.MatchString(profileID) ||
		ContainsSensitiveRuntimeProfileMaterial(profileID) {
		return SearchProfileUpdate{}, errors.New("invalid profile ID")
	}
	expectedRevision, err := exactNonnegativeInt64(params["expected_revision"])
	if err != nil || expectedRevision == math.MaxInt64 {
		return SearchProfileUpdate{}, errors.New("invalid expected revision")
	}
	request := SearchProfileUpdate{
		IdempotencyKey: idempotencyKey, ProfileID: profileID, ExpectedRevision: expectedRevision,
	}
	if value, present := params["max_results"]; present {
		parsed, parseErr := exactNonnegativeInt64(value)
		if parseErr != nil || parsed < 1 || parsed > 50 {
			return SearchProfileUpdate{}, errors.New("invalid max results")
		}
		request.MaxResults = &parsed
	}
	if value, present := params["timeout_seconds"]; present {
		parsed, parseErr := exactNonnegativeInt64(value)
		if parseErr != nil || parsed < 1 || parsed > 60 {
			return SearchProfileUpdate{}, errors.New("invalid timeout")
		}
		request.TimeoutSeconds = &parsed
	}
	return request, nil
}

func validateSearchProfileState(state SearchProfileState) error {
	if !state.Available {
		if state.Configured || state.Revision != 0 || len(state.AvailableProfileIDs) != 0 || state.Profile != nil {
			return ErrInvalidSearchProfileResponse
		}
		return nil
	}
	ids := state.AvailableProfileIDs
	if len(ids) == 0 || len(ids) > 128 || !sort.StringsAreSorted(ids) {
		return ErrInvalidSearchProfileResponse
	}
	for index, id := range ids {
		if !runtimeProfileIDPattern.MatchString(id) || ContainsSensitiveRuntimeProfileMaterial(id) ||
			(index > 0 && ids[index-1] == id) {
			return ErrInvalidSearchProfileResponse
		}
	}
	if !state.Configured {
		if state.Revision < 0 || state.Profile != nil {
			return ErrInvalidSearchProfileResponse
		}
		return nil
	}
	profile := state.Profile
	if state.Revision < 1 || profile == nil || !containsSortedProfileID(ids, profile.ProfileID) ||
		(profile.Provider != "tavily" && profile.Provider != "brave" && profile.Provider != "exa" &&
			profile.Provider != "serper" && profile.Provider != "deepseek_native") ||
		profile.MaxResults < 1 || profile.MaxResults > 50 || profile.TimeoutSeconds < 1 || profile.TimeoutSeconds > 60 {
		return ErrInvalidSearchProfileResponse
	}
	baseURL := strings.TrimSpace(profile.BaseURL)
	parsedURL, err := url.Parse(baseURL)
	if err != nil || baseURL != profile.BaseURL || len(baseURL) > 2048 || parsedURL.Scheme != "https" ||
		parsedURL.Host == "" || parsedURL.User != nil || parsedURL.Opaque != "" ||
		parsedURL.RawQuery != "" || parsedURL.Fragment != "" {
		return ErrInvalidSearchProfileResponse
	}
	for _, value := range []string{profile.ProfileID, profile.Provider, profile.BaseURL} {
		if ContainsSensitiveRuntimeProfileMaterial(value) {
			return ErrInvalidSearchProfileResponse
		}
	}
	return nil
}

func unavailableSearchProfileState() SearchProfileState {
	return SearchProfileState{AvailableProfileIDs: []string{}}
}

func (state SearchProfileState) Response() map[string]any {
	ids := append([]string(nil), state.AvailableProfileIDs...)
	if ids == nil {
		ids = []string{}
	}
	var profile any
	if state.Profile != nil {
		profile = state.Profile.Response()
	}
	return map[string]any{
		"available": state.Available, "configured": state.Configured,
		"revision": state.Revision, "available_profile_ids": ids, "profile": profile,
	}
}

func (profile SearchProfile) Response() map[string]any {
	return map[string]any{
		"profile_id": profile.ProfileID, "provider": profile.Provider,
		"base_url": profile.BaseURL, "max_results": profile.MaxResults,
		"timeout_seconds": profile.TimeoutSeconds,
	}
}

func searchProfileActionError(err error) *actionbase.Error {
	switch {
	case errors.Is(err, ErrSearchProfileUnavailable):
		return actionbase.StatusError(http.StatusServiceUnavailable, "Agent search profile is unavailable")
	case errors.Is(err, ErrSearchProfileConflict):
		return actionbase.StatusError(http.StatusConflict, "Agent search profile revision conflicts")
	case errors.Is(err, ErrInvalidSearchProfileResponse):
		return actionbase.StatusError(http.StatusBadGateway, "Agent search profile response is invalid")
	default:
		return actionbase.StatusError(http.StatusBadGateway, "Agent search profile request failed")
	}
}
