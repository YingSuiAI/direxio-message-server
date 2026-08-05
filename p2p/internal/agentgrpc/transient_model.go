package agentgrpc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"time"

	agentv1 "github.com/YingSuiAI/dirextalk-agent/api/gen/dirextalk/agent/v1"
	transientmodelsdk "github.com/YingSuiAI/dirextalk-agent/sdk/transientmodel"
	"github.com/google/uuid"
)

const (
	defaultClientContextWindowK = int64(64)
	defaultClientOutputTokens   = int64(4096)
	maximumClientContextWindowK = int64(97_656)
	maximumClientOutputTokens   = int64(10_000_000)
	maximumClientAPIKeyBytes    = 4096
	secretBootstrapTimeout      = 10 * time.Second
)

var transientBootstrapNamespace = uuid.MustParse("88de4f65-e1a6-4ead-8e94-788fc056e3e4")

type transientModelCredential struct {
	profile *agentv1.ModelProfile
	apiKey  []byte
	digest  [sha256.Size]byte
}

func (credential *transientModelCredential) clear() {
	if credential == nil {
		return
	}
	clear(credential.apiKey)
	credential.apiKey = nil
}

func parseTransientModelCredential(params map[string]any) (*transientModelCredential, error) {
	rawProfile, hasProfile := params["model_profile"]
	profileID := stringParam(params, "model_profile_id")
	topLevelKey, topLevelKeyPresent := params["api_key"]
	if !hasProfile && profileID == "" && !topLevelKeyPresent {
		return nil, nil
	}
	profile, ok := rawProfile.(map[string]any)
	if !ok || profile == nil {
		return nil, invalidTransientModelParameters()
	}
	if err := validateTransientProfileFields(profile); err != nil {
		return nil, err
	}
	nestedID, err := optionalProfileString(profile, "id")
	if err != nil {
		return nil, err
	}
	if profileID == "" {
		profileID = nestedID
	} else if nestedID != "" && nestedID != profileID {
		return nil, invalidTransientModelParameters()
	}
	if profileID == "" || len(profileID) > 128 || strings.ContainsAny(profileID, "\x00\r\n\t") {
		return nil, invalidTransientModelParameters()
	}
	providerName, err := requiredProfileString(profile, "provider")
	if err != nil {
		return nil, err
	}
	model, err := requiredProfileString(profile, "model")
	if err != nil || len(model) > 512 {
		return nil, invalidTransientModelParameters()
	}
	provider, baseURL, err := transientProvider(providerName, profile)
	if err != nil {
		return nil, err
	}
	profileKey, err := optionalProfileString(profile, "api_key")
	if err != nil {
		return nil, err
	}
	topKey := ""
	if topLevelKeyPresent {
		value, ok := topLevelKey.(string)
		if !ok {
			return nil, invalidTransientModelParameters()
		}
		topKey = strings.TrimSpace(value)
	}
	if profileKey != "" && topKey != "" && profileKey != topKey {
		return nil, invalidTransientModelParameters()
	}
	apiKey := profileKey
	if apiKey == "" {
		apiKey = topKey
	}
	if apiKey == "" || len(apiKey) > maximumClientAPIKeyBytes || strings.ContainsAny(apiKey, "\x00\r\n") {
		return nil, invalidTransientModelParameters()
	}
	if !validTransientProviderCredential(provider, apiKey) {
		return nil, invalidTransientModelCredential()
	}
	contextWindowK, err := optionalProfileInt64(profile, "context_window")
	if err != nil {
		return nil, err
	}
	if contextWindowK == 0 {
		contextWindowK = defaultClientContextWindowK
	}
	if contextWindowK < 1 || contextWindowK > maximumClientContextWindowK {
		return nil, invalidTransientModelParameters()
	}
	contextWindow := contextWindowK * 1024
	maxOutputTokens, err := optionalProfileInt64(profile, "max_output_tokens")
	if err != nil {
		return nil, err
	}
	if maxOutputTokens == 0 {
		maxOutputTokens = defaultClientOutputTokens
	}
	if maxOutputTokens < 1 || maxOutputTokens > maximumClientOutputTokens || maxOutputTokens > contextWindow {
		return nil, invalidTransientModelParameters()
	}
	temperature, err := optionalProfileFloat(profile, "temperature", 0, 2)
	if err != nil {
		return nil, err
	}
	topP, err := optionalProfileFloat(profile, "top_p", 0, 1)
	if err != nil {
		return nil, err
	}
	reasoningEffort, err := optionalProfileString(profile, "reasoning_effort")
	if err != nil {
		return nil, err
	}
	if reasoningEffort == "" {
		reasoningEffort, err = optionalProfileString(profile, "reasoning_mode")
		if err != nil {
			return nil, err
		}
	}
	if len(reasoningEffort) > 128 || strings.ContainsAny(reasoningEffort, "\x00\r\n\t") {
		return nil, invalidTransientModelParameters()
	}
	keyBytes := []byte(apiKey)
	result := &transientModelCredential{
		profile: &agentv1.ModelProfile{
			ProfileId: profileID, Provider: provider, Model: model, BaseUrl: baseURL,
			Temperature: temperature, TopP: topP, MaxOutputTokens: int32(maxOutputTokens),
			ContextWindow: int32(contextWindow), ReasoningEffort: reasoningEffort,
		},
		apiKey: append([]byte(nil), keyBytes...), digest: sha256.Sum256(keyBytes),
	}
	clear(keyBytes)
	return result, nil
}

func parseTransientModelDiscoveryCredential(params map[string]any) (*transientModelCredential, string, error) {
	if params == nil {
		return nil, "", invalidTransientModelDiscoveryParameters()
	}
	allowed := map[string]struct{}{"provider": {}, "base_url": {}, "api_key": {}}
	for key := range params {
		if _, ok := allowed[key]; !ok {
			return nil, "", invalidTransientModelDiscoveryParameters()
		}
	}
	providerName, err := requiredProfileString(params, "provider")
	if err != nil {
		return nil, "", invalidTransientModelDiscoveryParameters()
	}
	providerName = strings.ToLower(providerName)
	baseURLInput, err := optionalProfileString(params, "base_url")
	if err != nil {
		return nil, "", invalidTransientModelDiscoveryParameters()
	}
	apiKey, err := requiredProfileString(params, "api_key")
	if err != nil || len(apiKey) > maximumClientAPIKeyBytes {
		return nil, "", invalidTransientModelDiscoveryParameters()
	}
	profileInput := map[string]any{"base_url": baseURLInput}
	provider, baseURL, err := transientProvider(providerName, profileInput)
	if err != nil {
		return nil, "", invalidTransientModelDiscoveryParameters()
	}
	if !validTransientProviderCredential(provider, apiKey) {
		return nil, "", invalidTransientModelCredential()
	}
	profileID := "model-discovery:" + providerName
	keyBytes := []byte(apiKey)
	credential := &transientModelCredential{
		profile: &agentv1.ModelProfile{
			ProfileId: profileID, Provider: provider, Model: "model-discovery", BaseUrl: baseURL,
			MaxOutputTokens: int32(defaultClientOutputTokens), ContextWindow: int32(defaultClientContextWindowK * 1024),
		},
		apiKey: append([]byte(nil), keyBytes...), digest: sha256.Sum256(keyBytes),
	}
	clear(keyBytes)
	return credential, providerName, nil
}

func (runner *Runner) bootstrapTransientModel(ctx context.Context, requestID string, credential *transientModelCredential) (*agentv1.TransientModelInvocation, error) {
	if credential == nil {
		return nil, nil
	}
	if runner == nil || runner.secrets == nil || len(credential.apiKey) == 0 {
		return nil, errors.New("Agent transient model bootstrap is unavailable")
	}
	bindingProfile, err := transientmodelsdk.ProfileFromProto(credential.profile)
	if err != nil {
		return nil, invalidTransientModelParameters()
	}
	targetID, err := transientmodelsdk.TargetID(runner.ownerID, requestID, bindingProfile, credential.digest[:])
	if err != nil {
		return nil, invalidTransientModelParameters()
	}
	bootstrapCtx, cancel := context.WithTimeout(ctx, secretBootstrapTimeout)
	defer cancel()
	createID := uuid.NewSHA1(transientBootstrapNamespace, []byte("create\x00"+requestID)).String()
	created, err := runner.secrets.CreateSession(bootstrapCtx, &agentv1.CreateSessionRequest{
		IdempotencyKey: createID, OwnerId: runner.ownerID,
		Purpose: transientmodelsdk.CredentialPurpose, TargetId: targetID,
	})
	if err != nil {
		return nil, errors.New("Agent transient model bootstrap failed")
	}
	defer clear(created.UploadToken)
	session, err := validateCreatedTransientSession(created, runner.ownerID, targetID)
	if err != nil {
		return nil, err
	}
	switch session.GetStatus() {
	case agentv1.SecretBootstrapSessionStatus_SECRET_BOOTSTRAP_SESSION_STATUS_AWAITING_UPLOAD:
		envelope, sealErr := transientmodelsdk.Seal(session, credential.apiKey, rand.Reader)
		if sealErr != nil {
			return nil, errors.New("Agent transient model bootstrap failed")
		}
		defer clear(envelope.ClientPublicKey)
		defer clear(envelope.Nonce)
		defer clear(envelope.Ciphertext)
		uploadID := uuid.NewSHA1(transientBootstrapNamespace, []byte("upload\x00"+requestID)).String()
		uploaded, uploadErr := runner.secrets.UploadEncrypted(bootstrapCtx, &agentv1.UploadEncryptedRequest{
			SessionId: session.GetSessionId(), UploadToken: append([]byte(nil), created.GetUploadToken()...),
			ClientPublicKey: envelope.ClientPublicKey, Nonce: envelope.Nonce, Ciphertext: envelope.Ciphertext,
			IdempotencyKey: uploadID, ExpectedRevision: 1,
		})
		if uploadErr != nil || uploaded.GetRevision() != transientmodelsdk.ExpectedUploadedRevision ||
			!validTransientSession(uploaded.GetSession(), runner.ownerID, targetID,
				agentv1.SecretBootstrapSessionStatus_SECRET_BOOTSTRAP_SESSION_STATUS_UPLOADED,
				transientmodelsdk.ExpectedUploadedRevision) {
			return nil, errors.New("Agent transient model bootstrap failed")
		}
		session = uploaded.GetSession()
	case agentv1.SecretBootstrapSessionStatus_SECRET_BOOTSTRAP_SESSION_STATUS_UPLOADED:
		if session.GetRevision() != transientmodelsdk.ExpectedUploadedRevision || len(created.GetUploadToken()) != 0 {
			return nil, errors.New("Agent transient model bootstrap failed")
		}
	case agentv1.SecretBootstrapSessionStatus_SECRET_BOOTSTRAP_SESSION_STATUS_CONSUMED:
		if session.GetRevision() < transientmodelsdk.ExpectedUploadedRevision+1 || len(created.GetUploadToken()) != 0 {
			return nil, errors.New("Agent transient model bootstrap failed")
		}
	default:
		return nil, errors.New("Agent transient model bootstrap failed")
	}
	return &agentv1.TransientModelInvocation{
		Profile: credential.profile, CredentialSessionId: session.GetSessionId(),
		CredentialSessionRevision: transientmodelsdk.ExpectedUploadedRevision,
		CredentialSha256:          append([]byte(nil), credential.digest[:]...),
	}, nil
}

func validateCreatedTransientSession(response *agentv1.CreateSessionResponse, ownerID, targetID string) (*agentv1.SecretBootstrapSession, error) {
	if response == nil || response.GetSession() == nil || response.GetSessionId() != response.GetSession().GetSessionId() ||
		!bytesEqual(response.GetServerPublicKey(), response.GetSession().GetServerPublicKey()) ||
		response.GetExpiresAt() == nil || response.GetSession().GetExpiresAt() == nil ||
		!response.GetExpiresAt().AsTime().Equal(response.GetSession().GetExpiresAt().AsTime()) {
		return nil, errors.New("Agent transient model bootstrap failed")
	}
	session := response.GetSession()
	if !validTransientSession(session, ownerID, targetID, session.GetStatus(), session.GetRevision()) {
		return nil, errors.New("Agent transient model bootstrap failed")
	}
	if session.GetStatus() == agentv1.SecretBootstrapSessionStatus_SECRET_BOOTSTRAP_SESSION_STATUS_AWAITING_UPLOAD {
		if session.GetRevision() != 1 || len(response.GetUploadToken()) != 32 {
			return nil, errors.New("Agent transient model bootstrap failed")
		}
	}
	return session, nil
}

func validTransientSession(session *agentv1.SecretBootstrapSession, ownerID, targetID string, status agentv1.SecretBootstrapSessionStatus, revision int64) bool {
	if session == nil || session.GetOwnerId() != ownerID || session.GetPurpose() != transientmodelsdk.CredentialPurpose ||
		session.GetTargetId() != targetID || session.GetStatus() != status || session.GetRevision() != revision ||
		len(session.GetServerPublicKey()) != 32 || session.GetCreatedAt() == nil || session.GetExpiresAt() == nil ||
		!session.GetCreatedAt().IsValid() || !session.GetExpiresAt().IsValid() ||
		!time.Now().UTC().Before(session.GetExpiresAt().AsTime()) || strings.TrimSpace(session.GetAgentInstanceId()) == "" ||
		strings.TrimSpace(session.GetSessionSchemaVersion()) == "" || strings.TrimSpace(session.GetEnvelopeSchemaVersion()) == "" {
		return false
	}
	parsed, err := uuid.Parse(session.GetSessionId())
	return err == nil && parsed != uuid.Nil && parsed.String() == session.GetSessionId()
}

func transientProvider(raw string, profile map[string]any) (agentv1.ModelProvider, string, error) {
	name := strings.ToLower(strings.TrimSpace(raw))
	baseURL, err := optionalProfileString(profile, "base_url")
	if err != nil {
		return 0, "", err
	}
	provider := agentv1.ModelProvider_MODEL_PROVIDER_UNSPECIFIED
	switch name {
	case "openai":
		provider = agentv1.ModelProvider_MODEL_PROVIDER_OPENAI_COMPATIBLE
		if baseURL == "" {
			baseURL = "https://api.openai.com/v1"
		}
	case "openrouter":
		provider = agentv1.ModelProvider_MODEL_PROVIDER_OPENAI_COMPATIBLE
		if baseURL == "" {
			baseURL = "https://openrouter.ai/api/v1"
		}
	case "openai_compatible", "litellm":
		provider = agentv1.ModelProvider_MODEL_PROVIDER_OPENAI_COMPATIBLE
	case "deepseek":
		provider = agentv1.ModelProvider_MODEL_PROVIDER_DEEPSEEK
		if baseURL == "" {
			baseURL = "https://api.deepseek.com"
		}
	case "anthropic":
		provider = agentv1.ModelProvider_MODEL_PROVIDER_ANTHROPIC
		if baseURL == "" {
			baseURL = "https://api.anthropic.com"
		}
		baseURL = strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
	default:
		return 0, "", invalidTransientModelParameters()
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, parseErr := url.Parse(baseURL)
	if parseErr != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.Fragment != "" || unsafeTransientEndpointHost(parsed.Hostname()) {
		return 0, "", invalidTransientModelParameters()
	}
	return provider, baseURL, nil
}

func validateTransientProfileFields(profile map[string]any) error {
	allowed := map[string]struct{}{
		"id": {}, "provider": {}, "model": {}, "base_url": {}, "api_key": {},
		"temperature": {}, "top_p": {}, "max_output_tokens": {}, "context_window": {},
		"reasoning_mode": {}, "reasoning_effort": {},
	}
	for key := range profile {
		if _, ok := allowed[key]; !ok {
			return invalidTransientModelParameters()
		}
	}
	return nil
}

func requiredProfileString(profile map[string]any, key string) (string, error) {
	value, err := optionalProfileString(profile, key)
	if err != nil || value == "" {
		return "", invalidTransientModelParameters()
	}
	return value, nil
}

func optionalProfileString(profile map[string]any, key string) (string, error) {
	raw, exists := profile[key]
	if !exists || raw == nil {
		return "", nil
	}
	value, ok := raw.(string)
	if !ok {
		return "", invalidTransientModelParameters()
	}
	value = strings.TrimSpace(value)
	if len(value) > 2048 || strings.ContainsAny(value, "\x00\r\n\t") {
		return "", invalidTransientModelParameters()
	}
	return value, nil
}

func optionalProfileInt64(profile map[string]any, key string) (int64, error) {
	raw, exists := profile[key]
	if !exists || raw == nil {
		return 0, nil
	}
	value, err := nonnegativeInt64(raw)
	if err != nil {
		return 0, invalidTransientModelParameters()
	}
	return value, nil
}

func optionalProfileFloat(profile map[string]any, key string, minimum, maximum float64) (*float64, error) {
	raw, exists := profile[key]
	if !exists || raw == nil {
		return nil, nil
	}
	var value float64
	switch number := raw.(type) {
	case float64:
		value = number
	case float32:
		value = float64(number)
	case int:
		value = float64(number)
	case int64:
		value = float64(number)
	case json.Number:
		parsed, err := number.Float64()
		if err != nil {
			return nil, invalidTransientModelParameters()
		}
		value = parsed
	default:
		return nil, invalidTransientModelParameters()
	}
	if math.IsNaN(value) || math.IsInf(value, 0) || value < minimum || value > maximum {
		return nil, invalidTransientModelParameters()
	}
	return &value, nil
}

func unsafeTransientEndpointHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") || host == "host.docker.internal" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		address, ok := netip.AddrFromSlice(ip)
		return !ok || !address.Unmap().IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast()
	}
	return false
}

func validTransientProviderCredential(provider agentv1.ModelProvider, credential string) bool {
	if provider != agentv1.ModelProvider_MODEL_PROVIDER_DEEPSEEK {
		return true
	}
	if credential == "" || credential != strings.TrimSpace(credential) {
		return false
	}
	for _, character := range credential {
		if character >= '0' && character <= '9' ||
			character >= 'A' && character <= 'Z' ||
			character >= 'a' && character <= 'z' {
			continue
		}
		switch character {
		case '-', '.', '_', '~', '+', '/', '=':
			continue
		default:
			return false
		}
	}
	return true
}

func invalidTransientModelParameters() error {
	return errors.New("invalid agent chat parameters: model profile is invalid")
}

func invalidTransientModelDiscoveryParameters() error {
	return errors.New("invalid agent model discovery parameters")
}

func invalidTransientModelCredential() error {
	return codedRunnerError{
		message: "DeepSeek API key contains unsupported characters.",
		code:    "M_AGENT_MODEL_CREDENTIAL_INVALID",
	}
}

func bytesEqual(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var mismatch byte
	for index := range left {
		mismatch |= left[index] ^ right[index]
	}
	return mismatch == 0
}
