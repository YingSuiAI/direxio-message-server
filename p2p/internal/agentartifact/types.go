package agentartifact

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
)

const MaximumBytes = 8 << 20

var (
	ErrInvalid     = errors.New("Agent artifact is invalid")
	ErrNotFound    = errors.New("Agent artifact was not found")
	ErrUnavailable = errors.New("Agent artifact is unavailable")

	uuidPattern   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	namePattern   = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,127}$`)
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

type Download struct {
	ArtifactID string
	Name       string
	MediaType  string
	SizeBytes  int64
	SHA256     string
	Content    []byte
}

func NewDownload(
	artifactID,
	name,
	mediaType string,
	sizeBytes int64,
	digest string,
	content []byte,
) (Download, error) {
	value := Download{
		ArtifactID: artifactID,
		Name:       name,
		MediaType:  mediaType,
		SizeBytes:  sizeBytes,
		SHA256:     digest,
		Content:    append([]byte(nil), content...),
	}
	if value.Validate() != nil {
		clear(value.Content)
		return Download{}, ErrInvalid
	}
	return value, nil
}

func (value Download) Validate() error {
	digest := sha256.Sum256(value.Content)
	if !uuidPattern.MatchString(value.ArtifactID) ||
		!namePattern.MatchString(value.Name) ||
		!validMediaType(value.MediaType) ||
		value.SizeBytes < 1 || value.SizeBytes > MaximumBytes ||
		int64(len(value.Content)) != value.SizeBytes ||
		!digestPattern.MatchString(value.SHA256) ||
		fmt.Sprintf("sha256:%x", digest) != value.SHA256 {
		return ErrInvalid
	}
	return nil
}

func validMediaType(value string) bool {
	return value == "application/json" ||
		value == "text/plain; charset=utf-8" ||
		value == "application/vnd.openxmlformats-officedocument.presentationml.presentation"
}

type Source interface {
	DownloadTeamArtifact(context.Context, string) (Download, error)
}
