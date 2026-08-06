package releasecontrol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const CentralServerVersionURL = "https://imadmin.dirextalk.ai/api/appVersion/current?appId=1&channelId=server"

const maxCentralVersionResponseBytes = 64 * 1024

const (
	CentralVersionUnavailableCode = "central_version_unavailable"
	CentralVersionInvalidCode     = "central_version_invalid"
)

// CentralServerVersion is the narrowly validated part of the centrally owned
// server release record. The message server intentionally does not use the
// release URL, image name, digest, or other infrastructure fields from this
// endpoint.
type CentralServerVersion struct {
	AppID       string
	ChannelID   string
	Version     string
	PreVersion  string
	UpdateNotes string
}

// CentralVersionSource retrieves the fixed appId=1/server release record.
// Its small interface lets ProductCore tests exercise compatibility gates
// without making network requests.
type CentralVersionSource interface {
	CurrentServerVersion(context.Context) (CentralServerVersion, error)
}

type CentralVersionSourceConfig struct {
	HTTPClient *http.Client
}

type centralVersionSource struct {
	client *http.Client
}

// NewCentralVersionSource always targets the configured Dirextalk admin
// endpoint. Callers cannot change this URL through ProductCore parameters.
func NewCentralVersionSource(config CentralVersionSourceConfig) CentralVersionSource {
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return &centralVersionSource{client: client}
}

func (s *centralVersionSource) CurrentServerVersion(ctx context.Context) (CentralServerVersion, error) {
	if s == nil || s.client == nil {
		return CentralServerVersion{}, centralVersionError(CentralVersionUnavailableCode, "central version service is unavailable", nil)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, CentralServerVersionURL, nil)
	if err != nil {
		return CentralServerVersion{}, centralVersionError(CentralVersionUnavailableCode, "central version request could not be created", err)
	}
	response, err := s.client.Do(req)
	if err != nil {
		return CentralServerVersion{}, centralVersionError(CentralVersionUnavailableCode, "central version service is unavailable", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return CentralServerVersion{}, centralVersionError(CentralVersionUnavailableCode, "central version service returned an unexpected status", nil)
	}

	data, err := io.ReadAll(io.LimitReader(response.Body, maxCentralVersionResponseBytes+1))
	if err != nil || len(data) > maxCentralVersionResponseBytes {
		return CentralServerVersion{}, centralVersionError(CentralVersionInvalidCode, "central version response is invalid", err)
	}
	return decodeCentralServerVersion(data)
}

func decodeCentralServerVersion(data []byte) (CentralServerVersion, error) {
	var response struct {
		Code *int `json:"code"`
		Data *struct {
			AppID         string `json:"appId"`
			ChannelID     string `json:"channelId"`
			Version       string `json:"version"`
			PreVersion    string `json:"preVersion"`
			UpdateContent string `json:"updateContent"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&response); err != nil {
		return CentralServerVersion{}, centralVersionError(CentralVersionInvalidCode, "central version response is invalid", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return CentralServerVersion{}, centralVersionError(CentralVersionInvalidCode, "central version response is invalid", err)
	}
	if response.Code == nil || *response.Code != 0 || response.Data == nil {
		return CentralServerVersion{}, centralVersionError(CentralVersionInvalidCode, "central version response is invalid", nil)
	}
	if response.Data.AppID != "1" || response.Data.ChannelID != "server" {
		return CentralServerVersion{}, centralVersionError(CentralVersionInvalidCode, "central version response is invalid", nil)
	}
	version, err := CanonicalServerVersion("version", response.Data.Version)
	if err != nil {
		return CentralServerVersion{}, centralVersionError(CentralVersionInvalidCode, "central version response is invalid", err)
	}
	preVersion, err := CanonicalStableVersion("pre_version", response.Data.PreVersion)
	if err != nil {
		return CentralServerVersion{}, centralVersionError(CentralVersionInvalidCode, "central version response is invalid", err)
	}
	return CentralServerVersion{
		AppID:       response.Data.AppID,
		ChannelID:   response.Data.ChannelID,
		Version:     version,
		PreVersion:  preVersion,
		UpdateNotes: response.Data.UpdateContent,
	}, nil
}

// CanonicalStableVersion rejects whitespace, prereleases, and build metadata.
// It is stricter than the historical client-report normalizer because central
// records that require stable versions are unambiguous release identifiers.
func CanonicalStableVersion(field, value string) (string, error) {
	if value != strings.TrimSpace(value) {
		return "", fmt.Errorf("%s must not contain surrounding whitespace", field)
	}
	if _, err := parseCanonicalVersion(field, value); err != nil {
		return "", err
	}
	return value, nil
}

// CanonicalServerVersion accepts canonical stable and development server
// versions while continuing to reject whitespace and other SemVer variants.
func CanonicalServerVersion(field, value string) (string, error) {
	if value != strings.TrimSpace(value) {
		return "", fmt.Errorf("%s must not contain surrounding whitespace", field)
	}
	if _, err := parseCanonicalServerVersion(field, value); err != nil {
		return "", err
	}
	return value, nil
}

// CompareCanonicalStableVersions compares two canonical stable SemVer values.
func CompareCanonicalStableVersions(left, right string) (int, error) {
	left, err := CanonicalStableVersion("left_version", left)
	if err != nil {
		return 0, err
	}
	right, err = CanonicalStableVersion("right_version", right)
	if err != nil {
		return 0, err
	}
	leftVersion, err := parseCanonicalVersion("left_version", left)
	if err != nil {
		return 0, err
	}
	rightVersion, err := parseCanonicalVersion("right_version", right)
	if err != nil {
		return 0, err
	}
	switch {
	case leftVersion.LessThan(rightVersion):
		return -1, nil
	case leftVersion.GreaterThan(rightVersion):
		return 1, nil
	default:
		return 0, nil
	}
}

// CompareCanonicalServerVersions compares canonical server versions within the
// same release channel. Stable and development channels are intentionally not
// comparable because servers cannot upgrade across channels.
func CompareCanonicalServerVersions(left, right string) (int, error) {
	left, err := CanonicalServerVersion("left_version", left)
	if err != nil {
		return 0, err
	}
	right, err = CanonicalServerVersion("right_version", right)
	if err != nil {
		return 0, err
	}
	if strings.HasPrefix(left, "dev") != strings.HasPrefix(right, "dev") {
		return 0, fmt.Errorf("server versions must use the same release channel")
	}
	leftVersion, err := parseCanonicalServerVersion("left_version", left)
	if err != nil {
		return 0, err
	}
	rightVersion, err := parseCanonicalServerVersion("right_version", right)
	if err != nil {
		return 0, err
	}
	switch {
	case leftVersion.LessThan(rightVersion):
		return -1, nil
	case leftVersion.GreaterThan(rightVersion):
		return 1, nil
	default:
		return 0, nil
	}
}

type CentralVersionError struct {
	Code    string
	Message string
	err     error
}

func (e *CentralVersionError) Error() string {
	if e == nil || e.Message == "" {
		return "central version request failed"
	}
	return e.Message
}

func (e *CentralVersionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func centralVersionError(code, message string, err error) error {
	return &CentralVersionError{Code: code, Message: message, err: err}
}

func AsCentralVersionError(err error) (*CentralVersionError, bool) {
	var centralErr *CentralVersionError
	if !errors.As(err, &centralErr) {
		return nil, false
	}
	return centralErr, true
}
