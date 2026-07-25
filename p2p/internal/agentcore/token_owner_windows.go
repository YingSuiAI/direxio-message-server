//go:build windows

package agentcore

import (
	"errors"
	"os"
)

func validateTokenFileInfo(info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return errors.New("token file is not a regular file")
	}
	return nil
}
