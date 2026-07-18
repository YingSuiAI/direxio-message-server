package agent

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	"github.com/google/uuid"
)

const (
	actionRuntimeProfileGet    = "agent.runtime.profile.get"
	actionRuntimeProfileUpdate = "agent.runtime.profile.update"
)

var (
	ErrRuntimeProfileUnavailable     = errors.New("Agent runtime profile is unavailable")
	ErrRuntimeProfileConflict        = errors.New("Agent runtime profile revision conflicts")
	ErrInvalidRuntimeProfileResponse = errors.New("Agent runtime profile response is invalid")
	runtimeProfileIDPattern          = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	runtimeAWSAccessKeyIDPattern     = regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`)
	runtimeGitHubTokenPattern        = regexp.MustCompile(`\b(?:ghp_[A-Za-z0-9]{36}|github_pat_[A-Za-z0-9_]{22,})\b`)
	runtimeModelTokenPattern         = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`)
	runtimeSecretAssignmentPattern   = regexp.MustCompile(`(?i)\b(?:aws[_-]?(?:access[_-]?key[_-]?id|secret[_-]?access[_-]?key|session[_-]?token)|github[_-]?token|api[_-]?(?:key|token)|model(?:[_-]?api)?[_-]?(?:key|token)|access[_-]?token|authorization)\s*[:=]\s*([^\s,;]+)`)
)

// ContainsSensitiveRuntimeProfileMaterial rejects credential-shaped values at
// both the ProductCore and gRPC mapping boundaries.
func ContainsSensitiveRuntimeProfileMaterial(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	upper := strings.ToUpper(value)
	if strings.Contains(upper, "-----BEGIN") && strings.Contains(upper, "PRIVATE KEY-----") {
		return true
	}
	if runtimeAWSAccessKeyIDPattern.MatchString(value) || runtimeGitHubTokenPattern.MatchString(value) || runtimeModelTokenPattern.MatchString(value) {
		return true
	}
	return runtimeSecretAssignmentPattern.MatchString(value)
}

// RuntimeProfile is the exact de-secreted ProductCore projection of one
// immutable Agent catalog entry. Credential references and presence are not
// representable at this boundary.
type RuntimeProfile struct {
	ProfileID       string
	Provider        string
	Model           string
	BaseURL         string
	Temperature     *float64
	TopP            *float64
	MaxOutputTokens int64
	ContextWindow   int64
	ReasoningEffort string
}

// RuntimeProfileState exposes whether the independent Agent runtime is wired,
// whether this owner has selected a catalog profile, and the public catalog
// identifiers. It deliberately omits the protocol-neutral Agent owner ID.
type RuntimeProfileState struct {
	Available           bool
	Configured          bool
	Revision            int64
	AvailableProfileIDs []string
	Profile             *RuntimeProfile
}

// RuntimeProfileUpdate is the complete owner intent accepted by ProductCore.
// Immutable provider metadata and credential material cannot be expressed.
type RuntimeProfileUpdate struct {
	IdempotencyKey   string
	ProfileID        string
	ExpectedRevision int64
	Temperature      *float64
	TopP             *float64
	MaxOutputTokens  *int64
}

// RuntimeProfileClient is the narrow independent-Agent capability consumed by
// the owner-only ProductCore façade.
type RuntimeProfileClient interface {
	GetRuntimeProfile(context.Context) (RuntimeProfileState, error)
	UpdateRuntimeProfile(context.Context, RuntimeProfileUpdate) (RuntimeProfileState, error)
}

func (m *Module) getRuntimeProfile(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if len(params) != 0 {
		return nil, actionbase.BadRequest("runtime profile get does not accept parameters")
	}
	if m == nil || m.runtimeProfiles == nil {
		return unavailableRuntimeProfileState().Response(), nil
	}
	state, err := m.runtimeProfiles.GetRuntimeProfile(ctx)
	if err != nil {
		return nil, runtimeProfileActionError(err)
	}
	if err := validateRuntimeProfileState(state); err != nil {
		return nil, runtimeProfileActionError(err)
	}
	return state.Response(), nil
}

func (m *Module) updateRuntimeProfile(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	request, err := parseRuntimeProfileUpdate(params)
	if err != nil {
		return nil, actionbase.BadRequest("invalid Agent runtime profile update")
	}
	if m == nil || m.runtimeProfiles == nil {
		return nil, runtimeProfileActionError(ErrRuntimeProfileUnavailable)
	}
	state, err := m.runtimeProfiles.UpdateRuntimeProfile(ctx, request)
	if err != nil {
		return nil, runtimeProfileActionError(err)
	}
	if err := validateRuntimeProfileState(state); err != nil {
		return nil, runtimeProfileActionError(err)
	}
	return state.Response(), nil
}

func parseRuntimeProfileUpdate(params map[string]any) (RuntimeProfileUpdate, error) {
	if params == nil {
		return RuntimeProfileUpdate{}, errors.New("missing params")
	}
	allowed := map[string]struct{}{
		"idempotency_key": {}, "profile_id": {}, "expected_revision": {},
		"temperature": {}, "top_p": {}, "max_output_tokens": {},
	}
	for key := range params {
		if _, ok := allowed[key]; !ok {
			return RuntimeProfileUpdate{}, errors.New("unknown field")
		}
	}
	idempotencyKey, ok := params["idempotency_key"].(string)
	if !ok || strings.TrimSpace(idempotencyKey) != idempotencyKey {
		return RuntimeProfileUpdate{}, errors.New("invalid idempotency key")
	}
	parsedID, err := uuid.Parse(idempotencyKey)
	if err != nil || parsedID == uuid.Nil || parsedID.String() != idempotencyKey {
		return RuntimeProfileUpdate{}, errors.New("invalid idempotency key")
	}
	profileID, ok := params["profile_id"].(string)
	if !ok || strings.TrimSpace(profileID) != profileID || !runtimeProfileIDPattern.MatchString(profileID) || ContainsSensitiveRuntimeProfileMaterial(profileID) {
		return RuntimeProfileUpdate{}, errors.New("invalid profile ID")
	}
	expectedRevision, err := exactNonnegativeInt64(params["expected_revision"])
	if err != nil {
		return RuntimeProfileUpdate{}, err
	}
	if expectedRevision == math.MaxInt64 {
		return RuntimeProfileUpdate{}, errors.New("invalid expected revision")
	}
	request := RuntimeProfileUpdate{IdempotencyKey: idempotencyKey, ProfileID: profileID, ExpectedRevision: expectedRevision}
	if value, present := params["temperature"]; present {
		parsed, parseErr := exactFloat64(value, 0, 2)
		if parseErr != nil {
			return RuntimeProfileUpdate{}, parseErr
		}
		request.Temperature = &parsed
	}
	if value, present := params["top_p"]; present {
		parsed, parseErr := exactFloat64(value, 0, 1)
		if parseErr != nil {
			return RuntimeProfileUpdate{}, parseErr
		}
		request.TopP = &parsed
	}
	if value, present := params["max_output_tokens"]; present {
		parsed, parseErr := exactNonnegativeInt64(value)
		if parseErr != nil || parsed < 1 || parsed > 10_000_000 {
			return RuntimeProfileUpdate{}, errors.New("invalid max output tokens")
		}
		request.MaxOutputTokens = &parsed
	}
	return request, nil
}

func validateRuntimeProfileState(state RuntimeProfileState) error {
	if !state.Available {
		if state.Configured || state.Revision != 0 || len(state.AvailableProfileIDs) != 0 || state.Profile != nil {
			return ErrInvalidRuntimeProfileResponse
		}
		return nil
	}
	ids := state.AvailableProfileIDs
	if len(ids) == 0 || len(ids) > 128 || !sort.StringsAreSorted(ids) {
		return ErrInvalidRuntimeProfileResponse
	}
	for index, id := range ids {
		if !runtimeProfileIDPattern.MatchString(id) || ContainsSensitiveRuntimeProfileMaterial(id) || (index > 0 && ids[index-1] == id) {
			return ErrInvalidRuntimeProfileResponse
		}
	}
	if !state.Configured {
		if state.Revision != 0 || state.Profile != nil {
			return ErrInvalidRuntimeProfileResponse
		}
		return nil
	}
	profile := state.Profile
	if state.Revision < 1 || profile == nil || !containsSortedProfileID(ids, profile.ProfileID) ||
		(profile.Provider != "openai_compatible" && profile.Provider != "deepseek" && profile.Provider != "anthropic") ||
		strings.TrimSpace(profile.Model) == "" || strings.TrimSpace(profile.Model) != profile.Model || len(profile.Model) > 512 ||
		profile.MaxOutputTokens < 1 || profile.ContextWindow < 1 || profile.ContextWindow > 100_000_000 || profile.MaxOutputTokens > profile.ContextWindow ||
		len(profile.ReasoningEffort) > 128 || strings.ContainsAny(profile.ReasoningEffort, "\r\n\t") ||
		!validOptionalRuntimeFloat(profile.Temperature, 0, 2) || !validOptionalRuntimeFloat(profile.TopP, 0, 1) {
		return ErrInvalidRuntimeProfileResponse
	}
	baseURL := strings.TrimSpace(profile.BaseURL)
	parsedURL, err := url.Parse(baseURL)
	if err != nil || baseURL != profile.BaseURL || len(baseURL) > 2048 || parsedURL.Scheme != "https" || parsedURL.Host == "" ||
		parsedURL.User != nil || parsedURL.Opaque != "" || parsedURL.RawQuery != "" || parsedURL.Fragment != "" {
		return ErrInvalidRuntimeProfileResponse
	}
	for _, value := range []string{profile.ProfileID, profile.Provider, profile.Model, profile.BaseURL, profile.ReasoningEffort} {
		if ContainsSensitiveRuntimeProfileMaterial(value) {
			return ErrInvalidRuntimeProfileResponse
		}
	}
	return nil
}

func containsSortedProfileID(values []string, target string) bool {
	index := sort.SearchStrings(values, target)
	return index < len(values) && values[index] == target
}

func validOptionalRuntimeFloat(value *float64, minimum, maximum float64) bool {
	return value == nil || (!math.IsNaN(*value) && !math.IsInf(*value, 0) && *value >= minimum && *value <= maximum)
}

func exactNonnegativeInt64(value any) (int64, error) {
	switch number := value.(type) {
	case int:
		if number < 0 {
			return 0, errors.New("negative integer")
		}
		return int64(number), nil
	case int64:
		if number < 0 {
			return 0, errors.New("negative integer")
		}
		return number, nil
	case float64:
		if number < 0 || number > math.MaxInt64 || math.Trunc(number) != number {
			return 0, errors.New("invalid integer")
		}
		return int64(number), nil
	case json.Number:
		parsed, err := number.Int64()
		if err != nil || parsed < 0 {
			return 0, errors.New("invalid integer")
		}
		return parsed, nil
	default:
		return 0, errors.New("invalid integer")
	}
}

func exactFloat64(value any, minimum, maximum float64) (float64, error) {
	var parsed float64
	var err error
	switch number := value.(type) {
	case int:
		parsed = float64(number)
	case int64:
		parsed = float64(number)
	case float64:
		parsed = number
	case json.Number:
		parsed, err = strconv.ParseFloat(number.String(), 64)
	default:
		err = errors.New("invalid number")
	}
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < minimum || parsed > maximum {
		return 0, errors.New("invalid number")
	}
	return parsed, nil
}

func unavailableRuntimeProfileState() RuntimeProfileState {
	return RuntimeProfileState{AvailableProfileIDs: []string{}}
}

func (state RuntimeProfileState) Response() map[string]any {
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

func (profile RuntimeProfile) Response() map[string]any {
	return map[string]any{
		"profile_id": profile.ProfileID, "provider": profile.Provider, "model": profile.Model,
		"base_url": profile.BaseURL, "temperature": optionalFloat(profile.Temperature), "top_p": optionalFloat(profile.TopP),
		"max_output_tokens": profile.MaxOutputTokens, "context_window": profile.ContextWindow,
		"reasoning_effort": profile.ReasoningEffort,
	}
}

func optionalFloat(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

func runtimeProfileActionError(err error) *actionbase.Error {
	switch {
	case errors.Is(err, ErrRuntimeProfileUnavailable):
		return actionbase.StatusError(http.StatusServiceUnavailable, "Agent runtime profile is unavailable")
	case errors.Is(err, ErrRuntimeProfileConflict):
		return actionbase.StatusError(http.StatusConflict, "Agent runtime profile revision conflicts")
	case errors.Is(err, ErrInvalidRuntimeProfileResponse):
		return actionbase.StatusError(http.StatusBadGateway, "Agent runtime profile response is invalid")
	default:
		return actionbase.StatusError(http.StatusBadGateway, "Agent runtime profile request failed")
	}
}
