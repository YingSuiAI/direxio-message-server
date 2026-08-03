package executionplanning

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

type queuedOCIResponse struct {
	status       int
	mediaType    string
	body         []byte
	contentSize  int64
	declared     string
	authenticate string
	location     string
}

type queuedOCIDoer struct {
	responses []queuedOCIResponse
	requests  []*http.Request
	err       error
}

func (d *queuedOCIDoer) Do(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	d.requests = append(d.requests, clone)
	if d.err != nil {
		return nil, d.err
	}
	if len(d.responses) == 0 {
		return nil, errors.New("unexpected OCI registry request")
	}
	next := d.responses[0]
	d.responses = d.responses[1:]
	status := next.status
	if status == 0 {
		status = http.StatusOK
	}
	contentSize := next.contentSize
	if contentSize == 0 {
		contentSize = int64(len(next.body))
	}
	header := make(http.Header)
	header.Set("Content-Type", next.mediaType)
	if next.declared != "" {
		header.Set("Docker-Content-Digest", next.declared)
	}
	if next.authenticate != "" {
		header.Set("WWW-Authenticate", next.authenticate)
	}
	if next.location != "" {
		header.Set("Location", next.location)
	}
	return &http.Response{
		StatusCode:    status,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(next.body)),
		ContentLength: contentSize,
		Request:       req,
	}, nil
}

func TestPublicOCIRegistryAnalyzerUsesBoundedAnonymousBearerChallenge(t *testing.T) {
	document := []byte("digest-pinned-manifest")
	tokenBody := []byte(`{"token":"temporary-anonymous-pull-token"}`)
	doer := &queuedOCIDoer{responses: []queuedOCIResponse{
		{status: http.StatusUnauthorized, mediaType: "application/json", authenticate: `Bearer realm="https://auth.registry.example/token",service="registry.example",scope="repository:team/service:pull"`},
		{mediaType: "application/json; charset=utf-8", body: tokenBody},
		{mediaType: ociManifestV1, body: document, declared: "sha256:" + sha256Hex(document)},
	}}
	analyzer := &PublicOCIRegistryAnalyzer{client: doer}
	body, mediaType, err := analyzer.fetchDocument(context.Background(), "https://registry.example/v2/team/service/manifests/sha256:"+sha256Hex(document), []string{ociManifestV1}, maxOCIManifestBytes, sha256Hex(document), 0)
	if err != nil || mediaType != ociManifestV1 || !bytes.Equal(body, document) || len(doer.requests) != 3 {
		t.Fatalf("body=%q media=%q requests=%d err=%v", body, mediaType, len(doer.requests), err)
	}
	if doer.requests[0].Header.Get("Authorization") != "" || doer.requests[1].Header.Get("Authorization") != "" ||
		doer.requests[1].URL.String() != "https://auth.registry.example/token?scope=repository%3Ateam%2Fservice%3Apull&service=registry.example" ||
		doer.requests[2].Header.Get("Authorization") != "Bearer temporary-anonymous-pull-token" {
		t.Fatal("unsafe anonymous pull exchange")
	}
}

func TestPublicOCIRegistryAnalyzerRejectsUnsafeBearerRealm(t *testing.T) {
	doer := &queuedOCIDoer{responses: []queuedOCIResponse{{
		status: http.StatusUnauthorized, mediaType: "application/json",
		authenticate: `Bearer realm="http://127.0.0.1/token",service="registry.example",scope="repository:team/service:pull"`,
	}}}
	_, _, err := (&PublicOCIRegistryAnalyzer{client: doer}).fetchDocument(context.Background(), "https://registry.example/v2/team/service/manifests/sha256:"+strings.Repeat("a", 64), []string{ociManifestV1}, maxOCIManifestBytes, strings.Repeat("a", 64), 0)
	if !errors.Is(err, errOCIAuthToken) || len(doer.requests) != 1 {
		t.Fatalf("unsafe challenge err=%v requests=%d", err, len(doer.requests))
	}
}

func TestPublicOCIRegistryAnalyzerFollowsOneVerifiedBlobRedirectWithoutBearer(t *testing.T) {
	document := []byte("digest-pinned-config")
	doer := &queuedOCIDoer{responses: []queuedOCIResponse{
		{status: http.StatusTemporaryRedirect, mediaType: "application/json", location: "https://objects.example/config?temporary=signature"},
		{mediaType: genericOctetStream, body: document},
	}}
	body, _, err := (&PublicOCIRegistryAnalyzer{client: doer}).fetchDocument(context.Background(), "https://registry.example/v2/team/service/blobs/sha256:"+sha256Hex(document), []string{ociConfigV1, genericOctetStream}, maxOCIConfigBytes, sha256Hex(document), int64(len(document)))
	if err != nil || !bytes.Equal(body, document) || len(doer.requests) != 2 || doer.requests[1].URL.Host != "objects.example" || doer.requests[1].Header.Get("Authorization") != "" {
		t.Fatalf("verified redirect body=%q requests=%d err=%v", body, len(doer.requests), err)
	}
}

func TestPublicOCIRegistryAnalyzerRejectsPrivateBlobRedirect(t *testing.T) {
	doer := &queuedOCIDoer{responses: []queuedOCIResponse{{status: http.StatusTemporaryRedirect, mediaType: "application/json", location: "https://127.0.0.1/config"}}}
	_, _, err := (&PublicOCIRegistryAnalyzer{client: doer}).fetchDocument(context.Background(), "https://registry.example/v2/team/service/blobs/sha256:"+strings.Repeat("a", 64), []string{ociConfigV1}, maxOCIConfigBytes, strings.Repeat("a", 64), 10)
	if !errors.Is(err, errOCITransport) || len(doer.requests) != 1 {
		t.Fatalf("private redirect err=%v requests=%d", err, len(doer.requests))
	}
}

func TestPublicOCIRegistryAnalyzerRetriesOnlyReadOnlyRegistryStatus(t *testing.T) {
	document := []byte("digest-pinned-manifest")
	doer := &queuedOCIDoer{responses: []queuedOCIResponse{
		{status: http.StatusServiceUnavailable, mediaType: ociManifestV1},
		{mediaType: ociManifestV1, body: document},
	}}
	body, mediaType, err := (&PublicOCIRegistryAnalyzer{client: doer}).fetchDocument(
		context.Background(), "https://registry.example/v2/team/service/manifests/sha256:"+sha256Hex(document),
		[]string{ociManifestV1}, maxOCIManifestBytes, sha256Hex(document), 0,
	)
	if err != nil || !bytes.Equal(body, document) || mediaType != ociManifestV1 || len(doer.requests) != 2 {
		t.Fatalf("bounded registry read retry body=%q media=%q requests=%d err=%v", body, mediaType, len(doer.requests), err)
	}
}

func TestPublicOCIRegistryAnalyzerVerifiesManifestAndConfigWithoutFetchingLayers(t *testing.T) {
	config := mustJSON(t, map[string]any{
		"architecture": "amd64",
		"os":           "linux",
		"config": map[string]any{
			"Entrypoint":   []string{"/service"},
			"ExposedPorts": map[string]any{"8080/tcp": map[string]any{}},
			"Healthcheck":  map[string]any{"Test": []string{"CMD", "curl", "-f", "http://127.0.0.1:8080/health"}},
		},
	})
	manifest, location := validOCIManifest(t, config)
	doer := &queuedOCIDoer{responses: []queuedOCIResponse{
		{mediaType: ociManifestV1, body: manifest, declared: "sha256:" + sha256Hex(manifest)},
		{mediaType: ociConfigV1, body: config},
	}}
	analyzer := &PublicOCIRegistryAnalyzer{client: doer}

	facts, err := analyzer.AnalyzePinnedImage(context.Background(), location)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.BlockingUncertainties) != 0 || facts.Analysis.Source.Location != location ||
		!containsSortedString(facts.Analysis.DetectedStacks, "container") ||
		!containsSortedString(facts.Analysis.DetectedStacks, "oci_image") ||
		len(facts.Analysis.Ports) != 1 || facts.Analysis.Ports[0] != 8080 ||
		len(facts.Analysis.Probes) != 1 || facts.Analysis.Probes[0] != "http://127.0.0.1:8080/health" ||
		facts.Analysis.Exposure != "target_local" || facts.Analysis.Runtime.CPU != "2" ||
		facts.Analysis.Runtime.Memory != "2048MiB" || facts.Analysis.Runtime.Disk != "8GiB" ||
		facts.Analysis.Runtime.Architecture != "x86_64" {
		t.Fatalf("unexpected OCI facts: %+v", facts)
	}
	if len(doer.requests) != 2 || !strings.Contains(doer.requests[0].URL.Path, "/manifests/sha256:") ||
		!strings.Contains(doer.requests[1].URL.Path, "/blobs/sha256:") {
		t.Fatalf("analyzer fetched unexpected registry documents: %#v", doer.requests)
	}
	for _, request := range doer.requests {
		if request.Method != http.MethodGet || request.URL.Scheme != "https" || request.URL.Host != "registry.example" ||
			request.Header.Get("Authorization") != "" || request.Header.Get("Accept-Encoding") != "identity" {
			t.Fatalf("unsafe OCI request: %+v headers=%v", request.URL, request.Header)
		}
	}
}

func TestPublicOCIRegistryAnalyzerSelectsOneSupportedManifestFromPinnedIndex(t *testing.T) {
	config := mustJSON(t, map[string]any{
		"architecture": "amd64",
		"os":           "linux",
		"config": map[string]any{
			"Entrypoint":   []string{"/service"},
			"ExposedPorts": map[string]any{"8080/tcp": map[string]any{}},
			"Healthcheck":  map[string]any{"Test": []string{"CMD", "curl", "-f", "http://127.0.0.1:8080/health"}},
		},
	})
	manifest, _ := validOCIManifest(t, config)
	index := mustJSON(t, map[string]any{
		"schemaVersion": 2,
		"mediaType":     ociIndexV1,
		"manifests": []any{
			map[string]any{
				"mediaType": ociManifestV1, "digest": "sha256:" + sha256Hex(manifest), "size": len(manifest),
				"platform": map[string]any{"architecture": "amd64", "os": "linux"},
			},
			map[string]any{
				"mediaType": ociManifestV1, "digest": "sha256:" + strings.Repeat("f", 64), "size": 564,
				"platform": map[string]any{"architecture": "unknown", "os": "unknown"},
			},
		},
	})
	location := "registry.example/team/service@sha256:" + sha256Hex(index)
	doer := &queuedOCIDoer{responses: []queuedOCIResponse{
		{mediaType: ociIndexV1, body: index},
		{mediaType: ociManifestV1, body: manifest},
		{mediaType: ociConfigV1, body: config},
	}}
	facts, err := (&PublicOCIRegistryAnalyzer{client: doer}).AnalyzePinnedImage(context.Background(), location)
	if err != nil || len(facts.BlockingUncertainties) != 0 || facts.Analysis.Runtime.Architecture != "x86_64" {
		t.Fatalf("indexed OCI facts=%+v err=%v", facts, err)
	}
	if len(doer.requests) != 3 || !strings.Contains(doer.requests[0].URL.Path, "/manifests/sha256:"+sha256Hex(index)) ||
		!strings.Contains(doer.requests[1].URL.Path, "/manifests/sha256:"+sha256Hex(manifest)) ||
		!strings.Contains(doer.requests[2].URL.Path, "/blobs/sha256:") {
		t.Fatalf("indexed analyzer fetched unexpected documents: %#v", doer.requests)
	}
}

func TestPublicOCIRegistryAnalyzerRejectsAmbiguousSupportedIndexPlatforms(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	index := mustJSON(t, map[string]any{
		"schemaVersion": 2,
		"mediaType":     ociIndexV1,
		"manifests": []any{
			map[string]any{"mediaType": ociManifestV1, "digest": digest, "size": 512, "platform": map[string]any{"architecture": "amd64", "os": "linux"}},
			map[string]any{"mediaType": ociManifestV1, "digest": digest, "size": 512, "platform": map[string]any{"architecture": "arm64", "os": "linux", "variant": "v8"}},
		},
	})
	doer := &queuedOCIDoer{responses: []queuedOCIResponse{{mediaType: ociIndexV1, body: index}}}
	facts, err := (&PublicOCIRegistryAnalyzer{client: doer}).AnalyzePinnedImage(
		context.Background(), "registry.example/team/service@sha256:"+sha256Hex(index),
	)
	if err != nil || !containsFragment(facts.BlockingUncertainties, "exactly one supported linux platform") || len(doer.requests) != 1 {
		t.Fatalf("ambiguous OCI index accepted: facts=%+v requests=%d err=%v", facts, len(doer.requests), err)
	}
}

func TestPublicOCIRegistryAnalyzerAllowsPinnedEnvironmentDefaultsWithoutValues(t *testing.T) {
	config := mustJSON(t, map[string]any{
		"architecture": "arm64",
		"os":           "linux",
		"config": map[string]any{
			"Cmd":          []string{"serve"},
			"Env":          []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin", "LANG=C.UTF-8", "MODE=production", "PLAYWRIGHT_BROWSERS_PATH=/home/node/.cache/ms-playwright"},
			"ExposedPorts": map[string]any{"9090/tcp": map[string]any{}},
			"Healthcheck":  map[string]any{"Test": []string{"CMD", "wget", "--spider", "-q", "http://localhost:9090/ready"}},
		},
	})
	manifest, location := validOCIManifest(t, config)
	doer := &queuedOCIDoer{responses: []queuedOCIResponse{{mediaType: ociManifestV1, body: manifest}, {mediaType: ociConfigV1, body: config}}}
	facts, err := (&PublicOCIRegistryAnalyzer{client: doer}).AnalyzePinnedImage(context.Background(), location)
	if err != nil || len(facts.BlockingUncertainties) != 0 {
		t.Fatalf("facts=%+v err=%v", facts, err)
	}
	encoded, err := json.Marshal(facts)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"/usr/local/sbin", "C.UTF-8", "production"} {
		if bytes.Contains(encoded, []byte(value)) {
			t.Fatalf("OCI environment value leaked into analysis: %s", encoded)
		}
	}
	if !containsSortedString(facts.Analysis.EnvironmentNames, "PATH") ||
		!containsSortedString(facts.Analysis.EnvironmentNames, "LANG") ||
		!containsSortedString(facts.Analysis.EnvironmentNames, "MODE") ||
		!containsSortedString(facts.Analysis.EnvironmentNames, "PLAYWRIGHT_BROWSERS_PATH") || len(facts.Analysis.SecretPurposes) != 0 ||
		len(facts.Analysis.Volumes) != 0 || facts.Analysis.Probes[0] != "http://127.0.0.1:9090/ready" ||
		facts.Analysis.Runtime.Architecture != "arm64" {
		t.Fatalf("unsafe or incomplete OCI projection: %s", encoded)
	}
}

func TestPublicOCIRegistryAnalyzerBlocksRequiredSecretEnvironmentAndVolumesWithoutValues(t *testing.T) {
	config := mustJSON(t, map[string]any{
		"architecture": "amd64",
		"os":           "linux",
		"config": map[string]any{
			"Cmd":          []string{"serve"},
			"Env":          []string{"API_TOKEN=must-not-persist", "SERVICE_API_KEY=also-must-not-persist", "DATABASE_URL=${DATABASE_URL}", "REQUIRED_VALUE="},
			"Volumes":      map[string]any{"/var/lib/service": map[string]any{}},
			"ExposedPorts": map[string]any{"8080/tcp": map[string]any{}},
			"Healthcheck":  map[string]any{"Test": []string{"CMD", "curl", "-f", "http://127.0.0.1:8080/health"}},
		},
	})
	manifest, location := validOCIManifest(t, config)
	doer := &queuedOCIDoer{responses: []queuedOCIResponse{{mediaType: ociManifestV1, body: manifest}, {mediaType: ociConfigV1, body: config}}}
	facts, err := (&PublicOCIRegistryAnalyzer{client: doer}).AnalyzePinnedImage(context.Background(), location)
	if err != nil || len(facts.BlockingUncertainties) != 3 ||
		!containsFragment(facts.BlockingUncertainties, "runtime bindings") ||
		!containsFragment(facts.BlockingUncertainties, "secret grants") ||
		!containsFragment(facts.BlockingUncertainties, "persistent volumes") {
		t.Fatalf("facts=%+v err=%v", facts, err)
	}
	encoded, err := json.Marshal(facts)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"must-not-persist", "also-must-not-persist", "${DATABASE_URL}"} {
		if bytes.Contains(encoded, []byte(value)) {
			t.Fatalf("required/secret OCI environment value leaked: %s", encoded)
		}
	}
	if !containsSortedString(facts.Analysis.EnvironmentNames, "API_TOKEN") ||
		!containsSortedString(facts.Analysis.EnvironmentNames, "SERVICE_API_KEY") ||
		!containsSortedString(facts.Analysis.EnvironmentNames, "DATABASE_URL") ||
		!containsSortedString(facts.Analysis.EnvironmentNames, "REQUIRED_VALUE") ||
		!containsSortedString(facts.Analysis.SecretPurposes, "environment secret for API_TOKEN") ||
		!containsSortedString(facts.Analysis.SecretPurposes, "environment secret for SERVICE_API_KEY") ||
		!containsSortedString(facts.Analysis.Volumes, "/var/lib/service") {
		t.Fatalf("incomplete blocked OCI environment projection: %s", encoded)
	}
}

func TestOCIEnvironmentRuntimeBindingClassifier(t *testing.T) {
	for _, value := range []string{"", "   ", "changeme", "<REQUIRED>", "replace_me", "${DATABASE_URL}", "prefix-$VALUE", "{{ runtime_value }}", "your-value"} {
		if !ociEnvironmentNeedsRuntimeBinding(value) {
			t.Fatalf("runtime placeholder accepted as pinned default: %q", value)
		}
	}
	for _, value := range []string{"/usr/local/bin:/usr/bin", "C.UTF-8", "production", "0", "false"} {
		if ociEnvironmentNeedsRuntimeBinding(value) {
			t.Fatalf("pinned image default requires a runtime binding: %q", value)
		}
	}
}

func TestPublicOCIRegistryAnalyzerRejectsDescriptorFallbackURLs(t *testing.T) {
	config := mustJSON(t, map[string]any{
		"architecture": "amd64", "os": "linux",
		"config": map[string]any{"Cmd": []string{"serve"}, "ExposedPorts": map[string]any{"8080/tcp": map[string]any{}}, "Healthcheck": map[string]any{"Test": []string{"CMD", "curl", "-f", "http://127.0.0.1:8080/health"}}},
	})
	baseManifest, _ := validOCIManifest(t, config)
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "config", mutate: func(document map[string]any) {
			document["config"].(map[string]any)["urls"] = []string{"https://fallback.example/config"}
		}},
		{name: "foreign layer", mutate: func(document map[string]any) {
			layer := document["layers"].([]any)[0].(map[string]any)
			layer["mediaType"] = dockerLayerForeign
			layer["urls"] = []string{"https://fallback.example/layer"}
		}},
		{name: "nondistributable layer", mutate: func(document map[string]any) {
			layer := document["layers"].([]any)[0].(map[string]any)
			layer["mediaType"] = ociLayerNonDistGzip
			layer["urls"] = []string{"https://fallback.example/layer"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var document map[string]any
			if err := json.Unmarshal(baseManifest, &document); err != nil {
				t.Fatal(err)
			}
			tc.mutate(document)
			manifest := mustJSON(t, document)
			location := "registry.example/team/service@sha256:" + sha256Hex(manifest)
			doer := &queuedOCIDoer{responses: []queuedOCIResponse{{mediaType: ociManifestV1, body: manifest}}}
			facts, err := (&PublicOCIRegistryAnalyzer{client: doer}).AnalyzePinnedImage(context.Background(), location)
			if err != nil || !containsFragment(facts.BlockingUncertainties, "manifest is malformed") || len(doer.requests) != 1 {
				t.Fatalf("fallback descriptor accepted: facts=%+v requests=%d err=%v", facts, len(doer.requests), err)
			}
		})
	}
}

func TestPublicOCIRegistryAnalyzerAllowsOnlyExactHealthArgv(t *testing.T) {
	tests := []struct {
		name string
		test []string
	}{
		{name: "shell", test: []string{"CMD-SHELL", "curl -f http://127.0.0.1:8080/health"}},
		{name: "arbitrary command", test: []string{"CMD", "/bin/sh", "http://127.0.0.1:8080/health"}},
		{name: "curl without fail", test: []string{"CMD", "curl", "http://127.0.0.1:8080/health"}},
		{name: "extra curl option", test: []string{"CMD", "curl", "--connect-timeout", "1", "http://127.0.0.1:8080/health"}},
		{name: "external host", test: []string{"CMD", "curl", "-f", "http://example.com:8080/health"}},
		{name: "query", test: []string{"CMD", "wget", "--spider", "http://127.0.0.1:8080/health?token=x"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			config := mustJSON(t, map[string]any{
				"architecture": "amd64", "os": "linux",
				"config": map[string]any{"Cmd": []string{"serve"}, "ExposedPorts": map[string]any{"8080/tcp": map[string]any{}}, "Healthcheck": map[string]any{"Test": tc.test}},
			})
			manifest, location := validOCIManifest(t, config)
			doer := &queuedOCIDoer{responses: []queuedOCIResponse{{mediaType: ociManifestV1, body: manifest}, {mediaType: ociConfigV1, body: config}}}
			facts, err := (&PublicOCIRegistryAnalyzer{client: doer}).AnalyzePinnedImage(context.Background(), location)
			if err != nil || !containsFragment(facts.BlockingUncertainties, "exact target-local HTTP probe") {
				t.Fatalf("unsafe health argv accepted: facts=%+v err=%v", facts, err)
			}
		})
	}
}

func TestPublicOCIRegistryAnalyzerPersistsStableFetchBlockers(t *testing.T) {
	config := mustJSON(t, map[string]any{
		"architecture": "amd64", "os": "linux",
		"config": map[string]any{"Cmd": []string{"serve"}, "ExposedPorts": map[string]any{"8080/tcp": map[string]any{}}, "Healthcheck": map[string]any{"Test": []string{"CMD", "http://127.0.0.1:8080/health"}}},
	})
	manifest, location := validOCIManifest(t, config)
	tests := []struct {
		name      string
		location  string
		responses []queuedOCIResponse
		fragment  string
	}{
		{name: "manifest digest", location: strings.Replace(location, sha256Hex(manifest), strings.Repeat("f", 64), 1), responses: []queuedOCIResponse{{mediaType: ociManifestV1, body: manifest}}, fragment: "exact SHA-256"},
		{name: "manifest media", location: location, responses: []queuedOCIResponse{{mediaType: "application/vnd.docker.distribution.manifest.v1+json", body: manifest}}, fragment: "unsupported or non-canonical media"},
		{name: "manifest bounded", location: "registry.example/team/service@sha256:" + sha256Hex(bytes.Repeat([]byte("x"), int(maxOCIManifestBytes)+1)), responses: []queuedOCIResponse{{mediaType: ociManifestV1, body: bytes.Repeat([]byte("x"), int(maxOCIManifestBytes)+1), contentSize: -1}}, fragment: "bounded analysis limit"},
		{name: "no auth", location: location, responses: []queuedOCIResponse{{status: http.StatusUnauthorized, mediaType: ociManifestV1}}, fragment: "no-auth"},
		{name: "config digest", location: location, responses: []queuedOCIResponse{{mediaType: ociManifestV1, body: manifest}, {mediaType: ociConfigV1, body: bytes.Repeat([]byte("x"), len(config))}}, fragment: "exact SHA-256"},
		{name: "config size", location: location, responses: []queuedOCIResponse{{mediaType: ociManifestV1, body: manifest}, {mediaType: ociConfigV1, body: config[:len(config)-1], contentSize: -1}}, fragment: "descriptor size"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			facts, err := (&PublicOCIRegistryAnalyzer{client: &queuedOCIDoer{responses: tc.responses}}).AnalyzePinnedImage(context.Background(), tc.location)
			if err != nil || len(facts.BlockingUncertainties) != 1 || !containsFragment(facts.BlockingUncertainties, tc.fragment) {
				t.Fatalf("facts=%+v err=%v", facts, err)
			}
		})
	}
}

func TestPublicOCIRegistryAnalyzerRejectsAmbiguousConfigJSON(t *testing.T) {
	config := []byte(`{"architecture":"amd64","architecture":"arm64","os":"linux","config":{"Cmd":["serve"]}}`)
	manifest, location := validOCIManifest(t, config)
	doer := &queuedOCIDoer{responses: []queuedOCIResponse{{mediaType: ociManifestV1, body: manifest}, {mediaType: ociConfigV1, body: config}}}
	facts, err := (&PublicOCIRegistryAnalyzer{client: doer}).AnalyzePinnedImage(context.Background(), location)
	if err != nil || !containsFragment(facts.BlockingUncertainties, "config is malformed") {
		t.Fatalf("ambiguous config facts=%+v err=%v", facts, err)
	}
}

func TestPinnedOCIReferenceAndDialFenceRejectSSRF(t *testing.T) {
	digest := strings.Repeat("a", 64)
	for _, location := range []string{
		"localhost/team/service@sha256:" + digest,
		"127.0.0.1/team/service@sha256:" + digest,
		"registry.internal/team/service@sha256:" + digest,
		"registry.example:444/team/service@sha256:" + digest,
		"HTTPS://registry.example/team/service@sha256:" + digest,
		"registry.example/team/../service@sha256:" + digest,
		"user@registry.example/team/service@sha256:" + digest,
		"registry.example/team/service:latest",
	} {
		if _, err := parsePinnedOCIReference(location); !errors.Is(err, ErrSourceInvalid) {
			t.Fatalf("unsafe OCI reference accepted: %q err=%v", location, err)
		}
	}

	dialCalls := 0
	dial := func(context.Context, string, string) (net.Conn, error) {
		dialCalls++
		return nil, errors.New("test dial")
	}
	for _, resolved := range [][]net.IPAddr{
		{{IP: net.ParseIP("127.0.0.1")}},
		{{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("169.254.169.254")}},
		{{IP: net.ParseIP("2001:db8::1")}},
	} {
		lookup := func(context.Context, string) ([]net.IPAddr, error) { return resolved, nil }
		if _, err := safeOCIDialContext(lookup, dial)(context.Background(), "tcp", "registry.example:443"); err == nil {
			t.Fatalf("unsafe resolved addresses accepted: %#v", resolved)
		}
	}
	if dialCalls != 0 {
		t.Fatalf("network dial occurred for denied OCI addresses: %d", dialCalls)
	}

	lookupPublic := func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}
	if _, err := safeOCIDialContext(lookupPublic, dial)(context.Background(), "tcp", "registry.example:443"); err == nil || dialCalls != 1 {
		t.Fatalf("validated public address did not reach pinned dial boundary: calls=%d err=%v", dialCalls, err)
	}
	client := newPublicOCIHTTPClient(lookupPublic, dial)
	if err := client.CheckRedirect(&http.Request{}, []*http.Request{{}}); err == nil {
		t.Fatal("OCI registry redirect was accepted")
	}
}

func TestPublicOCIRegistryAnalyzerHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	doer := &queuedOCIDoer{err: context.Canceled}
	_, err := (&PublicOCIRegistryAnalyzer{client: doer}).AnalyzePinnedImage(ctx, "registry.example/team/service@sha256:"+strings.Repeat("a", 64))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation was converted into a persistent blocker: %v", err)
	}
}

func validOCIManifest(t *testing.T, config []byte) ([]byte, string) {
	t.Helper()
	configDigest := sha256Hex(config)
	manifest := mustJSON(t, map[string]any{
		"schemaVersion": 2,
		"mediaType":     ociManifestV1,
		"config": map[string]any{
			"mediaType": ociConfigV1,
			"digest":    "sha256:" + configDigest,
			"size":      len(config),
		},
		"layers": []any{map[string]any{
			"mediaType": ociLayerGzip,
			"digest":    "sha256:" + strings.Repeat("b", 64),
			"size":      1234,
		}},
	})
	return manifest, "registry.example/team/service@sha256:" + sha256Hex(manifest)
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
