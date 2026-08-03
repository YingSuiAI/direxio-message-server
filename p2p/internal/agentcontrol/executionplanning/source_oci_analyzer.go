package executionplanning

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
)

const (
	maxOCIManifestBytes int64 = 1 << 20
	maxOCIConfigBytes   int64 = 2 << 20
	maxOCIIndexEntries        = 128
	maxOCILayers              = 512
	maxOCIJSONDepth           = 64
	maxOCIJSONTokens          = 100_000
	maxOCIEnvironment         = 256
	maxOCIVolumes             = 128
	maxOCICommandParts        = 256
)

const (
	ociManifestV1        = "application/vnd.oci.image.manifest.v1+json"
	ociIndexV1           = "application/vnd.oci.image.index.v1+json"
	dockerManifestV2     = "application/vnd.docker.distribution.manifest.v2+json"
	dockerManifestListV2 = "application/vnd.docker.distribution.manifest.list.v2+json"
	ociConfigV1          = "application/vnd.oci.image.config.v1+json"
	dockerConfigV1       = "application/vnd.docker.container.image.v1+json"
	genericOctetStream   = "application/octet-stream"
	ociLayerTar          = "application/vnd.oci.image.layer.v1.tar"
	ociLayerGzip         = "application/vnd.oci.image.layer.v1.tar+gzip"
	ociLayerZstd         = "application/vnd.oci.image.layer.v1.tar+zstd"
	ociLayerNonDistTar   = "application/vnd.oci.image.layer.nondistributable.v1.tar"
	ociLayerNonDistGzip  = "application/vnd.oci.image.layer.nondistributable.v1.tar+gzip"
	ociLayerNonDistZstd  = "application/vnd.oci.image.layer.nondistributable.v1.tar+zstd"
	dockerLayerTar       = "application/vnd.docker.image.rootfs.diff.tar"
	dockerLayerGzip      = "application/vnd.docker.image.rootfs.diff.tar.gzip"
	dockerLayerForeign   = "application/vnd.docker.image.rootfs.foreign.diff.tar.gzip"
)

var (
	ociRepositoryRE = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`)
	ociPortRE       = regexp.MustCompile(`^([1-9][0-9]{0,4})/(tcp|udp)$`)
	ociHealthPathRE = regexp.MustCompile(`^/[A-Za-z0-9._~!$&*+,:@=/-]*$`)

	errOCITransport    = errors.New("OCI registry transport failed")
	errOCIUnauthorized = errors.New("OCI registry requires authentication")
	errOCIAuthToken    = errors.New("OCI registry anonymous pull authorization failed")
	errOCIStatus       = errors.New("OCI registry returned a non-success status")
	errOCITooLarge     = errors.New("OCI registry document exceeds its analysis limit")
	errOCISize         = errors.New("OCI registry descriptor size mismatch")
	errOCIMedia        = errors.New("OCI registry returned an unsupported media type")
	errOCIDigest       = errors.New("OCI registry content digest mismatch")
	errOCIDocument     = errors.New("OCI registry document is malformed")
)

type sourceOCIAnalyzer interface {
	AnalyzePinnedImage(context.Context, string) (SourceFacts, error)
}

type ociRegistryDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// PublicOCIRegistryAnalyzer performs a bounded, no-auth metadata inspection of
// one digest-pinned public image. It fetches only the manifest and config blob;
// image layers are never downloaded and no image or command is executed.
type PublicOCIRegistryAnalyzer struct {
	client ociRegistryDoer
}

func NewPublicOCIRegistryAnalyzer() *PublicOCIRegistryAnalyzer {
	return &PublicOCIRegistryAnalyzer{client: newPublicOCIHTTPClient(net.DefaultResolver.LookupIPAddr, (&net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext)}
}

func (a *PublicOCIRegistryAnalyzer) AnalyzePinnedImage(ctx context.Context, location string) (SourceFacts, error) {
	ref, err := parsePinnedOCIReference(location)
	if err != nil {
		return SourceFacts{}, ErrSourceInvalid
	}
	source := coreexecution.SourceRef{Kind: "oci_image", Location: location, Immutable: true}
	if a == nil || a.client == nil {
		return sourceUncertainty(source, "public OCI registry analysis is not configured"), nil
	}

	manifestURL := registryDocumentURL(ref, "manifests", "sha256:"+ref.digest)
	manifestBytes, mediaType, err := a.fetchDocument(ctx, manifestURL, []string{ociManifestV1, dockerManifestV2, ociIndexV1, dockerManifestListV2}, maxOCIManifestBytes, ref.digest, 0)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return SourceFacts{}, ctxErr
		}
		return sourceUncertainty(source, ociFetchBlocker("manifest", err)), nil
	}
	var selectedPlatform *ociPlatform
	if allowedMedia(mediaType, ociIndexV1, dockerManifestListV2) {
		var index ociIndexDocument
		if validateJSONDocument(manifestBytes) != nil || json.Unmarshal(manifestBytes, &index) != nil ||
			index.SchemaVersion != 2 || index.MediaType != mediaType || len(index.Manifests) < 1 || len(index.Manifests) > maxOCIIndexEntries {
			return sourceUncertainty(source, "OCI image index is malformed or outside the supported schema"), nil
		}
		descriptor, platform, ok := selectOCIIndexManifest(index.Manifests)
		if !ok {
			return sourceUncertainty(source, "OCI image index must contain exactly one supported linux platform manifest"), nil
		}
		selectedPlatform = &platform
		manifestBytes, mediaType, err = a.fetchDocument(
			ctx,
			registryDocumentURL(ref, "manifests", descriptor.Digest),
			[]string{descriptor.MediaType},
			maxOCIManifestBytes,
			strings.TrimPrefix(descriptor.Digest, "sha256:"),
			descriptor.Size,
		)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return SourceFacts{}, ctxErr
			}
			return sourceUncertainty(source, ociFetchBlocker("platform manifest", err)), nil
		}
	}
	var manifest ociManifestDocument
	if validateJSONDocument(manifestBytes) != nil || json.Unmarshal(manifestBytes, &manifest) != nil ||
		manifest.SchemaVersion != 2 || (manifest.MediaType != "" && manifest.MediaType != mediaType) ||
		!allowedMedia(manifest.Config.MediaType, ociConfigV1, dockerConfigV1) ||
		manifest.Config.Size < 1 || manifest.Config.Size > maxOCIConfigBytes ||
		!validOCIDigest(manifest.Config.Digest) || len(manifest.Config.URLs) != 0 || len(manifest.Layers) > maxOCILayers ||
		!validOCILayerDescriptors(manifest.Layers) {
		return sourceUncertainty(source, "OCI image manifest is malformed or outside the supported single-platform schema"), nil
	}

	configDigest := strings.TrimPrefix(manifest.Config.Digest, "sha256:")
	configURL := registryDocumentURL(ref, "blobs", manifest.Config.Digest)
	configBytes, _, err := a.fetchDocument(ctx, configURL, []string{manifest.Config.MediaType, genericOctetStream}, maxOCIConfigBytes, configDigest, manifest.Config.Size)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return SourceFacts{}, ctxErr
		}
		return sourceUncertainty(source, ociFetchBlocker("config", err)), nil
	}
	var config ociImageConfig
	if validateJSONDocument(configBytes) != nil || json.Unmarshal(configBytes, &config) != nil {
		return sourceUncertainty(source, "OCI image config is malformed or exceeds parser limits"), nil
	}
	if selectedPlatform != nil && (config.OS != selectedPlatform.OS || config.Architecture != selectedPlatform.Architecture) {
		return sourceUncertainty(source, "OCI image config platform does not match the selected index manifest"), nil
	}
	return analyzeOCIConfig(source, manifest, config), nil
}

func (a *PublicOCIRegistryAnalyzer) fetchDocument(ctx context.Context, endpoint string, accepted []string, limit int64, expectedDigest string, expectedSize int64) ([]byte, string, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		data, mediaType, err := a.fetchDocumentOnce(ctx, endpoint, accepted, limit, expectedDigest, expectedSize)
		if err == nil {
			return data, mediaType, nil
		}
		lastErr = err
		// Registry metadata reads are side-effect free. A bounded retry is safe
		// for transient/non-success CDN responses, unlike any remote execution
		// mutation. Authentication, validation, digest, and redirect failures
		// remain immediate and are never retried.
		if !errors.Is(err, errOCIStatus) || attempt == 2 {
			break
		}
		delay := time.Duration(250*(1<<attempt)) * time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, "", ctx.Err()
		case <-timer.C:
		}
	}
	return nil, "", lastErr
}

func (a *PublicOCIRegistryAnalyzer) fetchDocumentOnce(ctx context.Context, endpoint string, accepted []string, limit int64, expectedDigest string, expectedSize int64) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "", errOCITransport
	}
	req.Header.Set("Accept", strings.Join(accepted, ", "))
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, "", errOCITransport
	}
	if resp == nil || resp.Body == nil {
		return nil, "", errOCITransport
	}
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		_ = resp.Body.Close()
		token, tokenErr := a.fetchAnonymousPullToken(ctx, challenge)
		if tokenErr != nil {
			return nil, "", tokenErr
		}
		retry, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if requestErr != nil {
			return nil, "", errOCITransport
		}
		retry.Header.Set("Accept", strings.Join(accepted, ", "))
		retry.Header.Set("Accept-Encoding", "identity")
		retry.Header.Set("Authorization", "Bearer "+token)
		resp, err = a.client.Do(retry)
		if err != nil || resp == nil || resp.Body == nil {
			return nil, "", errOCITransport
		}
	}
	if expectedSize > 0 && ociDocumentRedirect(resp.StatusCode) {
		location := resp.Header.Get("Location")
		_ = resp.Body.Close()
		redirectURL, redirectErr := validateOCIDocumentRedirect(location)
		if redirectErr != nil {
			return nil, "", errOCITransport
		}
		redirect, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, redirectURL, nil)
		if requestErr != nil {
			return nil, "", errOCITransport
		}
		redirect.Header.Set("Accept", strings.Join(accepted, ", "))
		redirect.Header.Set("Accept-Encoding", "identity")
		// Authorization is intentionally not copied to a registry-selected
		// object host. The redirect URL is short-lived and the final bytes are
		// still bound to the manifest's exact digest and size.
		resp, err = a.client.Do(redirect)
		if err != nil || resp == nil || resp.Body == nil {
			return nil, "", errOCITransport
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, "", errOCIUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", errOCIStatus
	}
	mediaType, err := exactResponseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || !allowedMedia(mediaType, accepted...) {
		return nil, "", errOCIMedia
	}
	if resp.ContentLength > limit || expectedSize > limit {
		return nil, "", errOCITooLarge
	}
	if expectedSize > 0 && resp.ContentLength >= 0 && resp.ContentLength != expectedSize {
		return nil, "", errOCISize
	}
	data, err := io.ReadAll(io.LimitReader(&contextArchiveReader{ctx: ctx, r: resp.Body}, limit+1))
	if err != nil {
		return nil, "", errOCITransport
	}
	if int64(len(data)) > limit {
		return nil, "", errOCITooLarge
	}
	if expectedSize > 0 && int64(len(data)) != expectedSize {
		return nil, "", errOCISize
	}
	digest := sha256.Sum256(data)
	actual := hex.EncodeToString(digest[:])
	if actual != expectedDigest {
		return nil, "", errOCIDigest
	}
	if declared := resp.Header.Get("Docker-Content-Digest"); declared != "" && declared != "sha256:"+expectedDigest {
		return nil, "", errOCIDigest
	}
	return data, mediaType, nil
}

func ociDocumentRedirect(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	default:
		return false
	}
}

func validateOCIDocumentRedirect(raw string) (string, error) {
	if raw == "" || len(raw) > 16<<10 || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, "\r\n\x00") {
		return "", errOCITransport
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Hostname() == "" || u.Port() != "" && u.Port() != "443" ||
		u.Fragment != "" || u.Path == "" || u.Hostname() != strings.ToLower(u.Hostname()) || !validPublicRegistryHostname(u.Hostname()) {
		return "", errOCITransport
	}
	return u.String(), nil
}

const maxOCIAnonymousTokenBytes int64 = 64 << 10

func (a *PublicOCIRegistryAnalyzer) fetchAnonymousPullToken(ctx context.Context, rawChallenge string) (string, error) {
	challenge, err := parseOCIBearerChallenge(rawChallenge)
	if err != nil || a == nil || a.client == nil {
		return "", errOCIUnauthorized
	}
	u, err := url.Parse(challenge.realm)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Hostname() == "" ||
		u.Port() != "" && u.Port() != "443" || u.Fragment != "" || u.Path == "" ||
		!validPublicRegistryHostname(strings.ToLower(u.Hostname())) || u.Hostname() != strings.ToLower(u.Hostname()) ||
		len(u.String()) > 2048 {
		return "", errOCIAuthToken
	}
	query := u.Query()
	if len(query["service"]) != 0 || len(query["scope"]) != 0 {
		return "", errOCIAuthToken
	}
	query.Set("service", challenge.service)
	query.Set("scope", challenge.scope)
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", errOCIAuthToken
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := a.client.Do(req)
	if err != nil || resp == nil || resp.Body == nil {
		return "", errOCIAuthToken
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength > maxOCIAnonymousTokenBytes {
		return "", errOCIAuthToken
	}
	mediaType, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" || len(params) > 1 || len(params) == 1 && !strings.EqualFold(params["charset"], "utf-8") {
		return "", errOCIAuthToken
	}
	body, err := io.ReadAll(io.LimitReader(&contextArchiveReader{ctx: ctx, r: resp.Body}, maxOCIAnonymousTokenBytes+1))
	if err != nil || int64(len(body)) > maxOCIAnonymousTokenBytes || validateJSONDocument(body) != nil {
		return "", errOCIAuthToken
	}
	var document struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(body, &document) != nil || document.Token != "" && document.AccessToken != "" {
		return "", errOCIAuthToken
	}
	token := document.Token
	if token == "" {
		token = document.AccessToken
	}
	if len(token) < 16 || len(token) > 32<<10 || !utf8.ValidString(token) || strings.TrimSpace(token) != token || strings.ContainsAny(token, "\r\n\x00 \t") {
		return "", errOCIAuthToken
	}
	return token, nil
}

type ociBearerChallenge struct {
	realm, service, scope string
}

var (
	ociAuthServiceRE = regexp.MustCompile(`^[A-Za-z0-9.-]{1,253}$`)
	ociAuthScopeRE   = regexp.MustCompile(`^[A-Za-z0-9:/_.-]{1,512}$`)
)

func parseOCIBearerChallenge(raw string) (ociBearerChallenge, error) {
	const prefix = "Bearer "
	if !strings.HasPrefix(raw, prefix) || len(raw) > 4096 || strings.ContainsAny(raw, "\r\n\x00") {
		return ociBearerChallenge{}, errOCIUnauthorized
	}
	values := map[string]string{}
	for _, part := range strings.Split(strings.TrimPrefix(raw, prefix), ",") {
		key, quoted, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || (key != "realm" && key != "service" && key != "scope") || values[key] != "" || len(quoted) < 2 || quoted[0] != '"' || quoted[len(quoted)-1] != '"' || strings.Contains(quoted[1:len(quoted)-1], "\\") {
			return ociBearerChallenge{}, errOCIUnauthorized
		}
		value, err := strconv.Unquote(quoted)
		if err != nil || value == "" {
			return ociBearerChallenge{}, errOCIUnauthorized
		}
		values[key] = value
	}
	out := ociBearerChallenge{realm: values["realm"], service: values["service"], scope: values["scope"]}
	if out.realm == "" || !ociAuthServiceRE.MatchString(out.service) || !ociAuthScopeRE.MatchString(out.scope) {
		return ociBearerChallenge{}, errOCIUnauthorized
	}
	return out, nil
}

type pinnedOCIReference struct {
	registry   string
	repository string
	digest     string
}

func parsePinnedOCIReference(location string) (pinnedOCIReference, error) {
	const marker = "@sha256:"
	if location == "" || len(location) > 1024 || strings.TrimSpace(location) != location ||
		strings.ContainsAny(location, "\r\n\x00?#") || location != strings.ToLower(location) ||
		strings.Count(location, marker) != 1 {
		return pinnedOCIReference{}, ErrSourceInvalid
	}
	i := strings.LastIndex(location, marker)
	if i < 1 || !isHexPin(location[i+len(marker):], 64) {
		return pinnedOCIReference{}, ErrSourceInvalid
	}
	name := location[:i]
	slash := strings.IndexByte(name, '/')
	if slash < 1 || slash == len(name)-1 {
		return pinnedOCIReference{}, ErrSourceInvalid
	}
	hostPort, repository := name[:slash], name[slash+1:]
	if len(repository) > 255 || !ociRepositoryRE.MatchString(repository) || strings.Contains(repository, "..") {
		return pinnedOCIReference{}, ErrSourceInvalid
	}
	u, err := url.Parse("https://" + hostPort)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host != hostPort || u.Path != "" || u.RawQuery != "" || u.Fragment != "" ||
		u.Hostname() == "" || u.Port() != "" && u.Port() != "443" {
		return pinnedOCIReference{}, ErrSourceInvalid
	}
	host := u.Hostname()
	if !validPublicRegistryHostname(host) {
		return pinnedOCIReference{}, ErrSourceInvalid
	}
	return pinnedOCIReference{registry: hostPort, repository: repository, digest: location[i+len(marker):]}, nil
}

func validPublicRegistryHostname(host string) bool {
	if host == "" || len(host) > 253 || host != strings.ToLower(host) || strings.HasSuffix(host, ".") ||
		host == "localhost" || !strings.Contains(host, ".") || net.ParseIP(host) != nil ||
		strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") ||
		strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".home") || strings.HasSuffix(host, ".lan") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if r > 127 || !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

func registryDocumentURL(ref pinnedOCIReference, kind, identity string) string {
	return (&url.URL{Scheme: "https", Host: ref.registry, Path: "/v2/" + ref.repository + "/" + kind + "/" + identity}).String()
}

func exactResponseMediaType(raw string) (string, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return "", errOCIMedia
	}
	base, params, err := mime.ParseMediaType(raw)
	if err != nil || len(params) != 0 || base != raw {
		return "", errOCIMedia
	}
	return base, nil
}

func allowedMedia(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func validOCIDigest(value string) bool {
	return strings.HasPrefix(value, "sha256:") && isHexPin(strings.TrimPrefix(value, "sha256:"), 64)
}

type ociDescriptor struct {
	MediaType string   `json:"mediaType"`
	Digest    string   `json:"digest"`
	Size      int64    `json:"size"`
	URLs      []string `json:"urls,omitempty"`
}

type ociPlatform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Variant      string `json:"variant,omitempty"`
}

type ociIndexDescriptor struct {
	MediaType string       `json:"mediaType"`
	Digest    string       `json:"digest"`
	Size      int64        `json:"size"`
	URLs      []string     `json:"urls,omitempty"`
	Platform  *ociPlatform `json:"platform,omitempty"`
}

type ociIndexDocument struct {
	SchemaVersion int                  `json:"schemaVersion"`
	MediaType     string               `json:"mediaType"`
	Manifests     []ociIndexDescriptor `json:"manifests"`
}

func selectOCIIndexManifest(manifests []ociIndexDescriptor) (ociIndexDescriptor, ociPlatform, bool) {
	var selected ociIndexDescriptor
	var platform ociPlatform
	candidates := 0
	for _, descriptor := range manifests {
		if !allowedMedia(descriptor.MediaType, ociManifestV1, dockerManifestV2) || !validOCIDigest(descriptor.Digest) ||
			descriptor.Size < 1 || descriptor.Size > maxOCIManifestBytes || len(descriptor.URLs) != 0 || descriptor.Platform == nil {
			return ociIndexDescriptor{}, ociPlatform{}, false
		}
		candidate := *descriptor.Platform
		if candidate.OS != "linux" || candidate.Architecture != "amd64" && candidate.Architecture != "arm64" {
			continue
		}
		if candidate.Architecture == "amd64" && candidate.Variant != "" ||
			candidate.Architecture == "arm64" && candidate.Variant != "" && candidate.Variant != "v8" {
			return ociIndexDescriptor{}, ociPlatform{}, false
		}
		candidates++
		selected = descriptor
		platform = candidate
	}
	return selected, platform, candidates == 1
}

type ociManifestDocument struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	Config        ociDescriptor   `json:"config"`
	Layers        []ociDescriptor `json:"layers"`
}

type ociImageConfig struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Config       struct {
		Env          []string                   `json:"Env"`
		Entrypoint   []string                   `json:"Entrypoint"`
		Cmd          []string                   `json:"Cmd"`
		ExposedPorts map[string]json.RawMessage `json:"ExposedPorts"`
		Volumes      map[string]json.RawMessage `json:"Volumes"`
		Healthcheck  *struct {
			Test []string `json:"Test"`
		} `json:"Healthcheck"`
	} `json:"config"`
}

func validOCILayerDescriptors(layers []ociDescriptor) bool {
	var total int64
	for _, layer := range layers {
		if !allowedMedia(layer.MediaType, ociLayerTar, ociLayerGzip, ociLayerZstd, ociLayerNonDistTar, ociLayerNonDistGzip, ociLayerNonDistZstd, dockerLayerTar, dockerLayerGzip, dockerLayerForeign) ||
			!validOCIDigest(layer.Digest) || len(layer.URLs) != 0 || layer.Size < 0 || layer.Size > 1<<40 || total > 1<<40-layer.Size {
			return false
		}
		total += layer.Size
	}
	return true
}

func analyzeOCIConfig(source coreexecution.SourceRef, manifest ociManifestDocument, config ociImageConfig) SourceFacts {
	blockers := map[string]bool{}
	assumptions := map[string]bool{
		"OCI analysis read only the digest-verified manifest and config; image layers were not fetched":        true,
		"container commands and image contents were not executed on the Message Server":                        true,
		"CPU and memory requirements are not declared by OCI metadata and require target placement validation": true,
		"the service remains target-local unless a separately confirmed exposure stage is added":               true,
	}
	environments := map[string]bool{}
	secretPurposes := map[string]bool{}
	volumes := map[string]bool{}
	ports := map[int]bool{}
	probes := map[string]bool{}
	dependencies := map[string]bool{}
	architecture := ""

	if config.OS != "linux" || config.Architecture != "amd64" && config.Architecture != "arm64" {
		blockers["OCI image platform is missing or unsupported; only linux/amd64 and linux/arm64 are enabled"] = true
	} else {
		dependencies["OCI platform "+config.OS+"/"+config.Architecture] = true
		architecture = config.Architecture
		if architecture == "amd64" {
			architecture = "x86_64"
		}
	}
	if !validOCICommand(config.Config.Entrypoint) || !validOCICommand(config.Config.Cmd) || len(config.Config.Entrypoint) == 0 && len(config.Config.Cmd) == 0 {
		blockers["OCI image has no safe, explicit Entrypoint or Cmd metadata"] = true
	}
	if len(config.Config.Env) > maxOCIEnvironment {
		blockers["OCI image declares too many environment variables"] = true
	} else {
		for _, item := range config.Config.Env {
			name, value, found := strings.Cut(item, "=")
			if !found || !safeEnvironmentName(name) {
				blockers["OCI image contains an environment entry that cannot be safely cataloged"] = true
				continue
			}
			environments[name] = true
			if ociEnvironmentNeedsRuntimeBinding(value) {
				blockers["OCI image environment contains empty or placeholder values that require explicit runtime bindings"] = true
			}
			if sensitiveNameRE.MatchString(name) {
				secretPurposes["environment secret for "+name] = true
			}
		}
	}
	if len(secretPurposes) != 0 {
		blockers["generic container deployment does not yet support OCI secret grants"] = true
	}
	if len(config.Config.Volumes) > maxOCIVolumes {
		blockers["OCI image declares too many volumes"] = true
	} else {
		for volume := range config.Config.Volumes {
			if !safeOCIContainerPath(volume) {
				blockers["OCI image contains a volume path that cannot be safely cataloged"] = true
				continue
			}
			volumes[volume] = true
		}
	}
	if len(volumes) != 0 {
		blockers["generic container deployment does not yet support OCI persistent volumes"] = true
	}
	for declared := range config.Config.ExposedPorts {
		match := ociPortRE.FindStringSubmatch(declared)
		if len(match) != 3 || match[2] != "tcp" {
			blockers["OCI image contains a dynamic or unsupported exposed port"] = true
			continue
		}
		port, err := strconv.Atoi(match[1])
		if err != nil || port < 1 || port > 65535 || len(ports) >= 128 {
			blockers["OCI image contains an invalid or excessive port declaration"] = true
			continue
		}
		ports[port] = true
	}
	if len(ports) != 1 {
		blockers["generic container deployment requires exactly one declared TCP service port"] = true
	}
	portList := make([]int, 0, len(ports))
	for port := range ports {
		portList = append(portList, port)
	}
	sort.Ints(portList)
	if len(portList) == 1 {
		if probe, ok := exactOCIHealthProbe(config.Config.Healthcheck, portList[0]); ok {
			probes[probe] = true
		} else {
			blockers["OCI image healthcheck does not declare one exact target-local HTTP probe"] = true
		}
	}

	var compressedSize int64 = manifest.Config.Size
	for _, layer := range manifest.Layers {
		compressedSize += layer.Size
	}
	// OCI descriptors expose compressed sizes, not the expanded filesystem
	// footprint. V2 therefore binds a documented conservative policy: four
	// times compressed bytes plus 2 GiB working headroom, rounded up, with an
	// 8 GiB floor. The target resolver must still prove these exact structured
	// requirements against an authoritative fresh observation.
	const gib int64 = 1 << 30
	requiredBytes := compressedSize*4 + 2*gib
	requiredGiB := (requiredBytes + gib - 1) / gib
	if requiredGiB < 8 {
		requiredGiB = 8
	}
	runtime := coreexecution.ResourceRequirement{
		CPU: "2", Memory: "2048MiB", Disk: strconv.FormatInt(requiredGiB, 10) + "GiB", Architecture: architecture,
	}
	assumptions["resource minimum uses 2 vCPU, 2048 MiB memory, and four times compressed OCI bytes plus 2 GiB working disk headroom with an 8 GiB floor"] = true
	analysis := coreexecution.ProjectAnalysis{
		Source:                source,
		DetectedStacks:        []string{"container", "oci_image"},
		Runtime:               runtime,
		Dependencies:          mapKeys(dependencies, 128),
		Ports:                 portList,
		EnvironmentNames:      mapKeys(environments, 128),
		SecretPurposes:        mapKeys(secretPurposes, 128),
		Volumes:               mapKeys(volumes, 128),
		Probes:                mapKeys(probes, 128),
		Exposure:              "target_local",
		Assumptions:           mapKeys(assumptions, 128),
		BlockingUncertainties: mapKeys(blockers, 128),
	}
	return SourceFacts{Analysis: analysis, BlockingUncertainties: append([]string(nil), analysis.BlockingUncertainties...)}
}

func validOCICommand(parts []string) bool {
	if len(parts) > maxOCICommandParts {
		return false
	}
	for _, part := range parts {
		if part == "" || len(part) > 4096 || !utf8.ValidString(part) || strings.ContainsAny(part, "\r\n\x00") {
			return false
		}
	}
	return true
}

// ociEnvironmentNeedsRuntimeBinding examines an image-baked value only long
// enough to classify whether deployment input is still required. The value is
// never copied into ProjectAnalysis, errors, events, or a persisted artifact.
func ociEnvironmentNeedsRuntimeBinding(value string) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || strings.Contains(trimmed, "$") || strings.Contains(trimmed, "{{") || strings.Contains(trimmed, "}}") {
		return true
	}
	normalized := strings.ToLower(strings.Trim(trimmed, " _-<>[]{}()"))
	switch normalized {
	case "required", "require", "changeme", "change-me", "change_me", "replace-me", "replace_me", "replaceme", "todo", "tbd", "insert-value", "insert_value":
		return true
	}
	return strings.HasPrefix(normalized, "your-") || strings.HasPrefix(normalized, "your_")
}

func safeOCIContainerPath(value string) bool {
	return len(value) >= 2 && len(value) <= 512 && strings.HasPrefix(value, "/") &&
		!strings.ContainsAny(value, "\r\n\x00\\") && path.Clean(value) == value
}

func exactOCIHealthProbe(health *struct {
	Test []string `json:"Test"`
}, expectedPort int) (string, bool) {
	if health == nil || len(health.Test) < 3 || len(health.Test) > 5 || health.Test[0] != "CMD" {
		return "", false
	}
	for _, part := range health.Test {
		if part == "" || len(part) > 4096 || !utf8.ValidString(part) || strings.ContainsAny(part, "\r\n\x00") {
			return "", false
		}
	}
	var rawURL string
	switch health.Test[1] {
	case "curl", "/usr/bin/curl", "/bin/curl":
		if len(health.Test) == 4 && (health.Test[2] == "-f" || health.Test[2] == "--fail") {
			rawURL = health.Test[3]
		} else {
			return "", false
		}
	case "wget", "/usr/bin/wget", "/bin/wget":
		if len(health.Test) == 4 && health.Test[2] == "--spider" {
			rawURL = health.Test[3]
		} else if len(health.Test) == 5 && health.Test[2] == "--spider" && health.Test[3] == "-q" {
			rawURL = health.Test[4]
		} else {
			return "", false
		}
	default:
		return "", false
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		u.RawPath != "" || u.ForceQuery || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") ||
		u.Path == "" || !ociHealthPathRE.MatchString(u.Path) {
		return "", false
	}
	port := 80
	if u.Port() != "" {
		port, err = strconv.Atoi(u.Port())
		if err != nil {
			return "", false
		}
	}
	if port != expectedPort {
		return "", false
	}
	u.Host = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	return u.String(), true
}

func ociFetchBlocker(kind string, err error) string {
	switch {
	case errors.Is(err, errOCIUnauthorized):
		return "public no-auth OCI registry access to the " + kind + " is unavailable"
	case errors.Is(err, errOCIAuthToken):
		return "public OCI registry anonymous pull authorization for the " + kind + " is unavailable"
	case errors.Is(err, errOCITooLarge):
		return "OCI image " + kind + " exceeds its bounded analysis limit"
	case errors.Is(err, errOCISize):
		return "OCI image " + kind + " failed exact descriptor size verification"
	case errors.Is(err, errOCIMedia):
		return "OCI image " + kind + " uses an unsupported or non-canonical media type"
	case errors.Is(err, errOCIDigest):
		return "OCI image " + kind + " failed exact SHA-256 verification"
	case errors.Is(err, errOCIStatus):
		return "OCI registry did not return the pinned image " + kind
	default:
		return "OCI registry " + kind + " could not be retrieved through the restricted HTTPS analyzer"
	}
}

type ociLookupIP func(context.Context, string) ([]net.IPAddr, error)
type ociDialContext func(context.Context, string, string) (net.Conn, error)

func newPublicOCIHTTPClient(lookup ociLookupIP, dial ociDialContext) *http.Client {
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           safeOCIDialContext(lookup, dial),
		ForceAttemptHTTP2:     true,
		DisableCompression:    true,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   2,
		MaxConnsPerHost:       4,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			// Return the 3xx response to fetchDocument for explicit one-hop
			// validation. A generic error makes net/http discard the response
			// before the digest-bound blob redirect can be inspected.
			return http.ErrUseLastResponse
		},
	}
}

func safeOCIDialContext(lookup ociLookupIP, dial ociDialContext) ociDialContext {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if lookup == nil || dial == nil {
			return nil, errOCITransport
		}
		host, port, err := net.SplitHostPort(address)
		if err != nil || port != "443" || !validPublicRegistryHostname(strings.ToLower(host)) || host != strings.ToLower(host) {
			return nil, errOCITransport
		}
		resolved, err := lookup(ctx, host)
		if err != nil || len(resolved) == 0 || len(resolved) > 32 {
			return nil, errOCITransport
		}
		addresses := make([]netip.Addr, 0, len(resolved))
		for _, candidate := range resolved {
			addr, ok := netip.AddrFromSlice(candidate.IP)
			if !ok {
				return nil, errOCITransport
			}
			addr = addr.Unmap()
			if !publicOCIAddress(addr) {
				return nil, errOCITransport
			}
			addresses = append(addresses, addr)
		}
		sort.Slice(addresses, func(i, j int) bool { return addresses[i].Less(addresses[j]) })
		return dial(ctx, network, net.JoinHostPort(addresses[0].String(), port))
	}
}

var deniedOCIPrefixes = func() []netip.Prefix {
	values := []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"::/128", "::1/128", "fc00::/7", "fe80::/10", "ff00::/8", "2001:db8::/32",
	}
	out := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		out = append(out, netip.MustParsePrefix(value))
	}
	return out
}()

func publicOCIAddress(addr netip.Addr) bool {
	if !addr.IsValid() || !addr.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range deniedOCIPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

// validateJSONDocument rejects duplicate object keys and excessive nesting or
// token counts before decoding into a typed OCI structure. This avoids two
// components assigning different meaning to the same digest-pinned document.
func validateJSONDocument(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	count := 0
	if err := walkJSONValue(decoder, 0, &count); err != nil {
		return errOCIDocument
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errOCIDocument
	}
	return nil
}

func walkJSONValue(decoder *json.Decoder, depth int, count *int) error {
	if depth > maxOCIJSONDepth || *count >= maxOCIJSONTokens {
		return errOCIDocument
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	(*count)++
	delimiter, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			(*count)++
			key, ok := keyToken.(string)
			if !ok || seen[key] {
				return errOCIDocument
			}
			seen[key] = true
			if err := walkJSONValue(decoder, depth+1, count); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errOCIDocument
		}
	case '[':
		for decoder.More() {
			if err := walkJSONValue(decoder, depth+1, count); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errOCIDocument
		}
	default:
		return errOCIDocument
	}
	return nil
}
