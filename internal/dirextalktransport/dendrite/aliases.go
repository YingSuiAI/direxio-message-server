package dendrite

import (
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalktransport"
	"github.com/YingSuiAI/dirextalk-message-server/internal/productpolicy"
)

const (
	DirextalkRoomTypeChannel       = productpolicy.DirextalkRoomTypeChannel
	DirextalkRoomProfileEventType  = productpolicy.DirextalkRoomProfileEventType
	DirextalkJoinRequestEventType  = productpolicy.DirextalkJoinRequestEventType
	DirextalkMemberPolicyEventType = productpolicy.DirextalkMemberPolicyEventType
)

type channel = dirextalktransport.RoomChannel
type memberRecord = dirextalktransport.RoomMember

type CreateRoomRequest = dirextalktransport.CreateRoomRequest
type RoomStateEvent = dirextalktransport.RoomStateEvent
type SendStateEventRequest = dirextalktransport.SendStateEventRequest
type CreateRoomResult = dirextalktransport.CreateRoomResult
type SendMessageRequest = dirextalktransport.SendMessageRequest
type SendMessageResult = dirextalktransport.SendMessageResult
type PreparedMessage = dirextalktransport.PreparedMessage
type MatrixEventDisposition = dirextalktransport.MatrixEventDisposition

const (
	MatrixEventAccepted = dirextalktransport.MatrixEventAccepted
	MatrixEventRejected = dirextalktransport.MatrixEventRejected
	MatrixEventUnknown  = dirextalktransport.MatrixEventUnknown
)

type InviteUserRequest = dirextalktransport.InviteUserRequest
type JoinRoomRequest = dirextalktransport.JoinRoomRequest
type JoinRoomResult = dirextalktransport.JoinRoomResult
type LeaveRoomRequest = dirextalktransport.LeaveRoomRequest
type KickUserRequest = dirextalktransport.KickUserRequest
type UpdateMemberProfileRequest = dirextalktransport.UpdateMemberProfileRequest
type RedactEventRequest = dirextalktransport.RedactEventRequest
type RedactEventResult = dirextalktransport.RedactEventResult
