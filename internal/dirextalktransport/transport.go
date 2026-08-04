package dirextalktransport

import (
	"context"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
)

type RoomChannel = dirextalkdomain.Channel
type RoomMember = dirextalkdomain.MemberRecord

type Transport interface {
	CreateRoom(ctx context.Context, req CreateRoomRequest) (CreateRoomResult, error)
	SendMessage(ctx context.Context, req SendMessageRequest) (SendMessageResult, error)
	SendStateEvent(ctx context.Context, req SendStateEventRequest) error
	InviteUser(ctx context.Context, req InviteUserRequest) error
	JoinRoom(ctx context.Context, req JoinRoomRequest) (JoinRoomResult, error)
	LeaveRoom(ctx context.Context, req LeaveRoomRequest) error
	KickUser(ctx context.Context, req KickUserRequest) error
	GetRoomChannel(ctx context.Context, roomID string) (RoomChannel, bool, error)
	ListRoomMembers(ctx context.Context, roomID string) ([]RoomMember, error)
	UpdateMemberProfile(ctx context.Context, req UpdateMemberProfileRequest) error
	RedactEvent(ctx context.Context, req RedactEventRequest) (RedactEventResult, error)
}

// RoomCreatorReader is an optional Matrix read boundary for recovering the
// authoritative creator from current m.room.create state. An empty creator
// with a nil error means the create event is absent or its sender cannot be
// resolved to a valid Matrix user ID.
type RoomCreatorReader interface {
	ReadRoomCreator(ctx context.Context, roomID string) (creatorMXID string, err error)
}

type CreateRoomRequest struct {
	CreatorMXID        string
	CreatorDisplayName string
	CreatorAvatarURL   string
	Name               string
	Topic              string
	Visibility         string
	RoomType           string
	CreationContent    map[string]any
	IsDirect           bool
	InviteMXIDs        []string
	InitialState       []RoomStateEvent
	// IdempotencyKey makes room creation restart-safe for callers that must
	// recover after a committed CreateRoom response is lost. It is never sent
	// into room state; the Dendrite adapter hashes it into a stable room ID.
	IdempotencyKey string
}

type RoomStateEvent struct {
	Type     string
	StateKey string
	Content  map[string]any
}

type SendStateEventRequest struct {
	RoomID     string
	SenderMXID string
	Event      RoomStateEvent
	Timestamp  time.Time
}

type CreateRoomResult struct {
	RoomID string
}

type SendMessageRequest struct {
	SenderMXID  string
	RoomID      string
	EventType   string
	MessageType string
	Content     map[string]any
	Timestamp   time.Time
	// LogicalID/PostID are optional ProductCore identifiers persisted with a
	// prepared event (for deterministic channel-comment reconciliation).
	LogicalID string
	PostID    string
}

type SendMessageResult struct {
	EventID        string
	OriginServerTS int64
}

// PreparedMessage is the exact, already-authenticated Matrix PDU that will
// be submitted by SendPreparedMessage.  The JSON is persisted before the
// external roomserver write so a process restart can reconcile or submit the
// identical event without rebuilding it against a different DAG/state.
type PreparedMessage struct {
	EventID        string
	RoomID         string
	SenderMXID     string
	EventType      string
	MessageType    string
	OriginServerTS int64
	RoomVersion    string
	EventJSON      []byte
	ContentDigest  []byte
}

// PreparedMessagePort is the crash-safe Matrix mutation boundary used by
// Product Capability writes.  Implementations must build/sign exactly once,
// persist the returned PDU through PreparedMatrixMutationStore, and submit
// that same PDU on retries.
type PreparedMessagePort interface {
	PrepareMessage(context.Context, SendMessageRequest) (PreparedMessage, error)
	SendPreparedMessage(context.Context, PreparedMessage) (SendMessageResult, error)
	EventExists(context.Context, string, string) (bool, error)
}

// MatrixEventDisposition is the result of reconciling a staged event after a
// process crash.  Unknown is deliberately distinct from absent: an event ID
// returned by Matrix without an acceptance/rejection bit must not be treated
// as proof that it is safe to submit the PDU again.
type MatrixEventDisposition string

const (
	MatrixEventAccepted MatrixEventDisposition = "accepted"
	MatrixEventRejected MatrixEventDisposition = "rejected"
	MatrixEventUnknown  MatrixEventDisposition = "unknown"
)

// MatrixEventStatusPort is an optional stronger reconciliation boundary.  The
// legacy EventExists method remains for small test doubles and transports that
// can only answer presence; production Dendrite implements this extension and
// validates the persisted event identity/content before returning a status.
type MatrixEventStatusPort interface {
	EventStatus(context.Context, PreparedMessage) (MatrixEventDisposition, error)
}

// PreparedMatrixEvent is the durable staging record for one Matrix mutation.
// Owner/generation/root digest are immutable fences; they are checked on every
// read and write so an operation UUID cannot cross an account recreation.
type PreparedMatrixEvent struct {
	OperationID string
	Capability  string
	Operation   string
	OwnerID     string
	Generation  int64
	RootDigest  []byte
	LogicalID   string
	PostID      string
	State       string
	Attempt     int64
	Revision    int64
	ReceiptJSON []byte
	Event       PreparedMessage
}

// PreparedMatrixMutationStore persists the prepared Matrix event before the
// roomserver side effect.  DeleteByOperation is an internal terminal cleanup;
// callers handling retries must always use Get with the full owner fence.
type PreparedMatrixMutationStore interface {
	PrepareMatrixEvent(context.Context, PreparedMatrixEvent) error
	GetMatrixPreparedEvent(context.Context, string, string, int64, []byte) (*PreparedMatrixEvent, error)
	DeleteMatrixPreparedEvent(context.Context, string, string, int64, []byte) error
	DeleteMatrixPreparedEventByOperation(context.Context, string) error
}

// PreparedMatrixMutationPresenceStore is the owner-fenced probe used by
// operation cancellation.  A cancellation may proceed only when this probe
// proves that no prepared PDU exists; implementations must use the same
// operation/owner/generation/root fence as replay reads.
type PreparedMatrixMutationPresenceStore interface {
	HasMatrixPreparedEvent(context.Context, string, string, int64, []byte) (bool, error)
}

// PreparedMatrixMutationReceiptStore is an optional extension implemented by
// the PostgreSQL store. Keeping it optional preserves small deterministic
// in-memory test doubles while production records the post-SendEvents receipt
// before the operation ledger reaches terminal state.
type PreparedMatrixMutationReceiptStore interface {
	MarkMatrixPreparedEventSent(context.Context, string, string, int64, []byte, SendMessageResult) error
}

type InviteUserRequest struct {
	RoomID              string
	InviterMXID         string
	InviteeMXID         string
	Reason              string
	IsDirect            bool
	PublicJoinRequestID string
	InviteRoomState     []RoomStateEvent
}

type JoinRoomRequest struct {
	RoomIDOrAlias             string
	UserMXID                  string
	DisplayName               string
	AvatarURL                 string
	ServerNames               []string
	DirectContactReactivation bool
}

type JoinRoomResult struct {
	RoomID    string
	JoinedVia string
}

type LeaveRoomRequest struct {
	RoomID   string
	UserMXID string
}

type KickUserRequest struct {
	RoomID     string
	SenderMXID string
	TargetMXID string
	Reason     string
	Timestamp  time.Time
}

type UpdateMemberProfileRequest struct {
	RoomID      string
	UserMXID    string
	DisplayName string
	AvatarURL   string
	Timestamp   time.Time
}

type RedactEventRequest struct {
	RoomID     string
	EventID    string
	SenderMXID string
	Reason     string
	Timestamp  time.Time
}

type RedactEventResult struct {
	EventID string
}
