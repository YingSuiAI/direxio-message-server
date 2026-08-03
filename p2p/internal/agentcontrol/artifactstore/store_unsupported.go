//go:build !linux

package artifactstore

import (
	"context"
	"errors"
	"io"
	"os"
)

var errUnsupported = errors.New("artifactstore: secure dirfd storage unsupported")

func initializeArtifactRoot(string) (int, error) { return -1, errUnsupported }

func openArtifactRoot(string) (int, error)             { return -1, errUnsupported }
func createArtifactTemp(int) (*os.File, string, error) { return nil, "", errUnsupported }
func removeArtifactTemp(int, string) error             { return errUnsupported }
func inspectArtifact(context.Context, int, string) (Metadata, bool, error) {
	return Metadata{}, false, errUnsupported
}
func publishArtifact(context.Context, int, string, string, int64) (Metadata, error) {
	return Metadata{}, errUnsupported
}
func openArtifact(context.Context, int, string) (io.ReadCloser, Metadata, error) {
	return nil, Metadata{}, errUnsupported
}
