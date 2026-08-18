package routing

import (
	"errors"
	"net/http"
	"testing"

	"github.com/matrix-org/gomatrixserverlib/spec"
)

func TestDownloadFailureResponsePreservesPublicErrorContract(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{
			name: "missing remote media",
			err:  errors.New(`file with media ID "media-1" does not exist on remote.example`),
			want: `Failed to download: File with media ID "media-1" does not exist on remote.example`,
		},
		{
			name: "multipart redirect",
			err:  errors.New("location header is not yet supported"),
			want: "Failed to download: Location header is not yet supported",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := downloadFailureResponse(test.err)
			if response.Code != http.StatusNotFound {
				t.Fatalf("status=%d, want %d", response.Code, http.StatusNotFound)
			}
			matrixErr, ok := response.JSON.(spec.MatrixError)
			if !ok {
				t.Fatalf("error=%T %#v, want spec.MatrixError", response.JSON, response.JSON)
			}
			if matrixErr.Err != test.want {
				t.Fatalf("error=%q, want %q", matrixErr.Err, test.want)
			}
		})
	}
}
