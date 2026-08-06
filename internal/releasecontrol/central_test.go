package releasecontrol

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type centralRoundTripper func(*http.Request) (*http.Response, error)

func (fn centralRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestCentralVersionSourceValidatesFixedServerRecord(t *testing.T) {
	client := &http.Client{Transport: centralRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.String() != CentralServerVersionURL {
			t.Fatalf("unexpected central request: %s %s", request.Method, request.URL)
		}
		return centralHTTPResponse(http.StatusOK, `{
			"code":0,
			"data":{"appId":"1","channelId":"server","version":"v1.0.3","preVersion":"v1.0.0","updateContent":"first\nsecond","url":"https://github.com/YingSuiAI/dirextalk-message-server/releases/tag/v1.0.3"},
			"msg":"success"
		}`), nil
	})}
	source := NewCentralVersionSource(CentralVersionSourceConfig{HTTPClient: client})
	version, err := source.CurrentServerVersion(context.Background())
	if err != nil {
		t.Fatalf("CurrentServerVersion: %v", err)
	}
	if version.AppID != "1" || version.ChannelID != "server" || version.Version != "v1.0.3" || version.PreVersion != "v1.0.0" || version.UpdateNotes != "first\nsecond" {
		t.Fatalf("unexpected central version: %#v", version)
	}
}

func TestCentralVersionSourceAcceptsDevelopmentServerVersion(t *testing.T) {
	client := &http.Client{Transport: centralRoundTripper(func(*http.Request) (*http.Response, error) {
		return centralHTTPResponse(http.StatusOK, `{
			"code":0,
			"data":{"appId":"1","channelId":"server","version":"dev1.1.7","preVersion":"v1.1.6","updateContent":"development"}
		}`), nil
	})}
	version, err := NewCentralVersionSource(CentralVersionSourceConfig{HTTPClient: client}).CurrentServerVersion(context.Background())
	if err != nil {
		t.Fatalf("CurrentServerVersion: %v", err)
	}
	if version.Version != "dev1.1.7" || version.PreVersion != "v1.1.6" {
		t.Fatalf("unexpected central version: %#v", version)
	}
}

func TestCentralAgentVersionSourceValidatesFixedAgentRecord(t *testing.T) {
	client := &http.Client{Transport: centralRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.String() != CentralAgentVersionURL {
			t.Fatalf("unexpected central Agent request: %s %s", request.Method, request.URL)
		}
		return centralHTTPResponse(http.StatusOK, `{
			"code":0,
			"data":{"appId":"1","channelId":"agents","version":"v1.0.1","preVersion":"v1.1.1","url":"https://untrusted.invalid/agent","image":"must-not-forward","digest":"must-not-forward"},
			"msg":"success"
		}`), nil
	})}
	version, err := NewCentralAgentVersionSource(CentralVersionSourceConfig{HTTPClient: client}).CurrentAgentVersion(context.Background())
	if err != nil {
		t.Fatalf("CurrentAgentVersion: %v", err)
	}
	if version != (CentralAgentVersion{AppID: "1", ChannelID: "agents", Version: "v1.0.1", PreVersion: "v1.1.1"}) {
		t.Fatalf("unexpected central Agent version: %#v", version)
	}
}

func TestCentralAgentVersionSourceRejectsMalformedAndUntrustedRecords(t *testing.T) {
	for name, response := range map[string]*http.Response{
		"business_error":      centralHTTPResponse(http.StatusOK, `{"code":7,"data":{"appId":"1","channelId":"agents","version":"v1.0.1","preVersion":"v1.1.1"}}`),
		"wrong_app":           centralHTTPResponse(http.StatusOK, `{"code":0,"data":{"appId":"2","channelId":"agents","version":"v1.0.1","preVersion":"v1.1.1"}}`),
		"wrong_channel":       centralHTTPResponse(http.StatusOK, `{"code":0,"data":{"appId":"1","channelId":"server","version":"v1.0.1","preVersion":"v1.1.1"}}`),
		"development_version": centralHTTPResponse(http.StatusOK, `{"code":0,"data":{"appId":"1","channelId":"agents","version":"dev1.0.1","preVersion":"v1.1.1"}}`),
		"bad_minimum":         centralHTTPResponse(http.StatusOK, `{"code":0,"data":{"appId":"1","channelId":"agents","version":"v1.0.1","preVersion":"1.1.1"}}`),
		"bad_type":            centralHTTPResponse(http.StatusOK, `{"code":"0","data":{"appId":"1","channelId":"agents","version":"v1.0.1","preVersion":"v1.1.1"}}`),
		"trailing_json":       centralHTTPResponse(http.StatusOK, `{"code":0,"data":{"appId":"1","channelId":"agents","version":"v1.0.1","preVersion":"v1.1.1"}} {}`),
	} {
		t.Run(name, func(t *testing.T) {
			client := &http.Client{Transport: centralRoundTripper(func(*http.Request) (*http.Response, error) { return response, nil })}
			_, err := NewCentralAgentVersionSource(CentralVersionSourceConfig{HTTPClient: client}).CurrentAgentVersion(context.Background())
			centralErr, ok := AsCentralVersionError(err)
			if !ok || centralErr.Code != CentralVersionInvalidCode {
				t.Fatalf("unexpected error: %#v", err)
			}
		})
	}
}

func TestCentralAgentVersionSourceDoesNotFollowRedirects(t *testing.T) {
	followed := false
	client := &http.Client{
		Transport: centralRoundTripper(func(*http.Request) (*http.Response, error) {
			response := centralHTTPResponse(http.StatusFound, "redirect")
			response.Header.Set("Location", "https://attacker.invalid/version")
			return response, nil
		}),
		CheckRedirect: func(*http.Request, []*http.Request) error { followed = true; return nil },
	}
	_, err := NewCentralAgentVersionSource(CentralVersionSourceConfig{HTTPClient: client}).CurrentAgentVersion(context.Background())
	centralErr, ok := AsCentralVersionError(err)
	if !ok || centralErr.Code != CentralVersionUnavailableCode || followed {
		t.Fatalf("redirect must fail closed without following: followed=%v err=%#v", followed, err)
	}
}

func TestCentralAgentVersionSourceRejectsOversizedResponse(t *testing.T) {
	client := &http.Client{Transport: centralRoundTripper(func(*http.Request) (*http.Response, error) {
		return centralHTTPResponse(http.StatusOK, strings.Repeat("x", maxCentralVersionResponseBytes+1)), nil
	})}
	_, err := NewCentralAgentVersionSource(CentralVersionSourceConfig{HTTPClient: client}).CurrentAgentVersion(context.Background())
	centralErr, ok := AsCentralVersionError(err)
	if !ok || centralErr.Code != CentralVersionInvalidCode {
		t.Fatalf("unexpected oversized-response error: %#v", err)
	}
}

func TestCentralVersionSourceRejectsMalformedAndUntrustedRecords(t *testing.T) {
	for name, response := range map[string]*http.Response{
		"business_error":         centralHTTPResponse(http.StatusOK, `{"code":7,"data":{"appId":"1","channelId":"server","version":"v1.0.3","preVersion":"v1.0.0"}}`),
		"wrong_channel":          centralHTTPResponse(http.StatusOK, `{"code":0,"data":{"appId":"1","channelId":"google","version":"v1.0.3","preVersion":"v1.0.0"}}`),
		"noncanonical":           centralHTTPResponse(http.StatusOK, `{"code":0,"data":{"appId":"1","channelId":"server","version":"1.0.3","preVersion":"v1.0.0"}}`),
		"development_preversion": centralHTTPResponse(http.StatusOK, `{"code":0,"data":{"appId":"1","channelId":"server","version":"dev1.0.3","preVersion":"dev1.0.0"}}`),
		"bad_type":               centralHTTPResponse(http.StatusOK, `{"code":"0","data":{"appId":"1","channelId":"server","version":"v1.0.3","preVersion":"v1.0.0"}}`),
		"http_status":            centralHTTPResponse(http.StatusBadGateway, `{"code":0}`),
	} {
		t.Run(name, func(t *testing.T) {
			client := &http.Client{Transport: centralRoundTripper(func(*http.Request) (*http.Response, error) {
				return response, nil
			})}
			_, err := NewCentralVersionSource(CentralVersionSourceConfig{HTTPClient: client}).CurrentServerVersion(context.Background())
			centralErr, ok := AsCentralVersionError(err)
			if !ok {
				t.Fatalf("expected central version error, got %v", err)
			}
			if name == "http_status" {
				if centralErr.Code != CentralVersionUnavailableCode {
					t.Fatalf("unexpected code: %#v", centralErr)
				}
				return
			}
			if centralErr.Code != CentralVersionInvalidCode {
				t.Fatalf("unexpected code: %#v", centralErr)
			}
		})
	}
}

func TestCentralVersionSourceMapsTransportFailureWithoutLeakingDetails(t *testing.T) {
	client := &http.Client{Transport: centralRoundTripper(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("private socket secret")
	})}
	_, err := NewCentralVersionSource(CentralVersionSourceConfig{HTTPClient: client}).CurrentServerVersion(context.Background())
	centralErr, ok := AsCentralVersionError(err)
	if !ok || centralErr.Code != CentralVersionUnavailableCode {
		t.Fatalf("unexpected error: %#v", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("transport details leaked: %v", err)
	}
}

func TestCanonicalStableVersionAndComparison(t *testing.T) {
	for _, value := range []string{"v1.0.0", "v1.2.3"} {
		if _, err := CanonicalStableVersion("version", value); err != nil {
			t.Fatalf("expected %q to be valid: %v", value, err)
		}
	}
	for _, value := range []string{"1.0.0", "dev1.0.0", "v01.0.0", "v1.0.0-beta", "v1.0.0+build", " v1.0.0"} {
		if _, err := CanonicalStableVersion("version", value); err == nil {
			t.Fatalf("expected %q to be invalid", value)
		}
	}
	comparison, err := CompareCanonicalStableVersions("v1.0.2", "v1.0.1")
	if err != nil || comparison <= 0 {
		t.Fatalf("comparison=%d err=%v", comparison, err)
	}
}

func TestCanonicalServerVersionAndComparison(t *testing.T) {
	for _, value := range []string{"v1.0.0", "dev1.1.7"} {
		if _, err := CanonicalServerVersion("version", value); err != nil {
			t.Fatalf("expected %q to be valid: %v", value, err)
		}
	}
	for _, value := range []string{"1.1.7", "dev01.1.7", "dev1.1.7-beta", "dev1.1.7+build", " dev1.1.7"} {
		if _, err := CanonicalServerVersion("version", value); err == nil {
			t.Fatalf("expected %q to be invalid", value)
		}
	}
	for _, testCase := range []struct {
		left  string
		right string
		want  int
	}{
		{left: "dev1.1.8", right: "dev1.1.7", want: 1},
		{left: "v1.1.8", right: "v1.1.7", want: 1},
	} {
		comparison, err := CompareCanonicalServerVersions(testCase.left, testCase.right)
		if err != nil || comparison != testCase.want {
			t.Fatalf("CompareCanonicalServerVersions(%q, %q)=%d err=%v, want %d", testCase.left, testCase.right, comparison, err, testCase.want)
		}
	}
	for _, testCase := range [][2]string{{"dev1.1.7", "v1.1.6"}, {"v1.1.7", "dev1.1.6"}} {
		if _, err := CompareCanonicalServerVersions(testCase[0], testCase[1]); err == nil {
			t.Fatalf("expected cross-channel comparison %q and %q to fail", testCase[0], testCase[1])
		}
	}
}

func centralHTTPResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
