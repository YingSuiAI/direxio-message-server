package routing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	roomserverAPI "github.com/YingSuiAI/dirextalk-message-server/roomserver/api"
	userapi "github.com/YingSuiAI/dirextalk-message-server/userapi/api"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
)

type errorContractRoomserver struct {
	roomserverAPI.ClientRoomserverAPI
	peekErr    error
	upgradeErr error
}

func (s errorContractRoomserver) PerformPeek(context.Context, *roomserverAPI.PerformPeekRequest) (string, error) {
	return "", s.peekErr
}

func (s errorContractRoomserver) PerformRoomUpgrade(context.Context, string, spec.UserID, gomatrixserverlib.RoomVersion, []string) (string, error) {
	return "", s.upgradeErr
}

func TestPeekRoomPreservesEncryptedRoomErrorContract(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/_matrix/client/v3/peek/!room:example.com", nil)
	response := PeekRoomByIDOrAlias(req, &userapi.Device{UserID: "@alice:example.com", ID: "DEVICE"}, errorContractRoomserver{
		peekErr: roomserverAPI.ErrNotAllowed{Err: errCannotPeekEncryptedRoom{}},
	}, "!room:example.com")
	assertMatrixErrorContract(t, response, http.StatusForbidden, "Cannot peek into an encrypted room")
}

type errCannotPeekEncryptedRoom struct{}

func (errCannotPeekEncryptedRoom) Error() string { return "cannot peek into an encrypted room" }

func TestUpgradeRoomPreservesPermissionErrorContract(t *testing.T) {
	version := string(gomatrixserverlib.RoomVersionV11)
	req := httptest.NewRequest(http.MethodPost, "/_matrix/client/v3/rooms/!room:example.com/upgrade", strings.NewReader(`{"new_version":"`+version+`"}`))
	response := UpgradeRoom(req, &userapi.Device{UserID: "@alice:example.com"}, nil, "!room:example.com", nil, errorContractRoomserver{
		upgradeErr: roomserverAPI.ErrNotAllowed{Err: errUpgradePermissionDenied{}},
	}, nil)
	assertMatrixErrorContract(t, response, http.StatusForbidden, "You don't have permission to upgrade the room, power level too low.")
}

type errUpgradePermissionDenied struct{}

func (errUpgradePermissionDenied) Error() string {
	return "you don't have permission to upgrade the room, power level too low"
}

func assertMatrixErrorContract(t *testing.T, response util.JSONResponse, wantCode int, wantMessage string) {
	t.Helper()
	if response.Code != wantCode {
		t.Fatalf("response status=%d, want %d", response.Code, wantCode)
	}
	matrixErr, ok := response.JSON.(spec.MatrixError)
	if !ok {
		t.Fatalf("response error=%T %#v, want spec.MatrixError", response.JSON, response.JSON)
	}
	if matrixErr.Err != wantMessage {
		t.Fatalf("response error=%q, want %q", matrixErr.Err, wantMessage)
	}
}
