package releasecontrol

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/Masterminds/semver/v3"
)

var (
	canonicalVersionPattern       = regexp.MustCompile(`^v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)$`)
	canonicalServerVersionPattern = regexp.MustCompile(`^(?:v|dev)(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)$`)
)

func parseCanonicalVersion(field, value string) (*semver.Version, error) {
	value = strings.TrimSpace(value)
	if !canonicalVersionPattern.MatchString(value) {
		return nil, fmt.Errorf("%s must be a canonical stable version such as v1.0.0", field)
	}
	parsed, err := semver.NewVersion(value)
	if err != nil {
		return nil, fmt.Errorf("%s is invalid: %w", field, err)
	}
	return parsed, nil
}

func parseCanonicalServerVersion(field, value string) (*semver.Version, error) {
	if !canonicalServerVersionPattern.MatchString(value) {
		return nil, fmt.Errorf("%s must be a canonical server version such as v1.0.0 or dev1.0.0", field)
	}
	semverValue := strings.TrimPrefix(value, "v")
	if strings.HasPrefix(value, "dev") {
		semverValue = strings.TrimPrefix(value, "dev") + "-dev"
	}
	parsed, err := semver.NewVersion(semverValue)
	if err != nil {
		return nil, fmt.Errorf("%s is invalid: %w", field, err)
	}
	return parsed, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("decode response: multiple JSON values")
		}
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
