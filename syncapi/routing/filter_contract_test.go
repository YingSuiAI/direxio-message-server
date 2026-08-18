package routing

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/syncapi/storage"
	userapi "github.com/YingSuiAI/dirextalk-message-server/userapi/api"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

type filterContractStorage struct{ storage.Database }

func TestPutFilterPreservesInvalidEventFormatErrorContract(t *testing.T) {
	const userID = "@alice:example.com"
	req := httptest.NewRequest(http.MethodPost, "/_matrix/client/v3/user/"+userID+"/filter", strings.NewReader(`{"event_format":"invalid"}`))
	response := PutFilter(req, &userapi.Device{UserID: userID}, filterContractStorage{}, userID)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("PutFilter status=%d, want %d", response.Code, http.StatusBadRequest)
	}
	matrixErr, ok := response.JSON.(spec.MatrixError)
	if !ok {
		t.Fatalf("PutFilter error=%T %#v, want spec.MatrixError", response.JSON, response.JSON)
	}
	const want = `Invalid filter: Bad event_format value. Must be one of ["client", "federation"]`
	if matrixErr.Err != want {
		t.Fatalf("PutFilter error=%q, want %q", matrixErr.Err, want)
	}
}
