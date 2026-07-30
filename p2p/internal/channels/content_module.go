package channels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalktransport"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
)

const (
	actionPostsList       = "channels.posts.list"
	actionPostsCreate     = "channels.posts.create"
	actionPostsRecall     = "channels.posts.recall"
	actionCommentsRecall  = "channels.comments.recall"
	actionCommentsList    = "channels.comments.list"
	actionCommentsCreate  = "channels.comments.create"
	actionPostReaction    = "channels.post_reaction.toggle"
	actionCommentReaction = "channels.comment_reaction.toggle"
	actionMyComments      = "channels.my_comments"
	actionMyReactions     = "channels.my_reactions"
)

// Post is the ProductCore response shape for a durable channel post. The
// durable record deliberately excludes request-scoped reaction and
// conversation enrichment.
type Post struct {
	PostID         string                            `json:"post_id"`
	ChannelID      string                            `json:"channel_id"`
	RoomID         string                            `json:"room_id"`
	EventID        string                            `json:"event_id"`
	AuthorMXID     string                            `json:"author_mxid"`
	AuthorName     string                            `json:"author_name"`
	Body           string                            `json:"body"`
	MessageType    string                            `json:"message_type"`
	MediaJSON      string                            `json:"media_json"`
	Visibility     string                            `json:"visibility"`
	OriginServerTS int64                             `json:"origin_server_ts"`
	CommentCount   int64                             `json:"comment_count"`
	ReactionCount  int64                             `json:"reaction_count"`
	LikeCount      int64                             `json:"like_count"`
	ReactedByMe    bool                              `json:"reacted_by_me"`
	FavoriteCount  int64                             `json:"favorite_count"`
	FavoritedByMe  bool                              `json:"favorited_by_me"`
	Operation      map[string]any                    `json:"operation,omitempty"`
	Conversation   *dirextalkdomain.ConversationView `json:"conversation,omitempty"`
}

// Comment is the ProductCore response shape for a durable channel comment.
type Comment struct {
	CommentID         string                            `json:"comment_id"`
	PostID            string                            `json:"post_id"`
	ChannelID         string                            `json:"channel_id"`
	EventID           string                            `json:"event_id"`
	AuthorMXID        string                            `json:"author_mxid"`
	AuthorName        string                            `json:"author_name"`
	Body              string                            `json:"body"`
	MessageType       string                            `json:"message_type"`
	MediaJSON         string                            `json:"media_json"`
	ReplyToCommentID  string                            `json:"reply_to_comment_id"`
	ReplyToAuthorMXID string                            `json:"reply_to_author_mxid"`
	MentionsJSON      string                            `json:"mentions_json"`
	OriginServerTS    int64                             `json:"origin_server_ts"`
	ReactionCount     int64                             `json:"reaction_count"`
	ReactedByMe       bool                              `json:"reacted_by_me"`
	Operation         map[string]any                    `json:"operation,omitempty"`
	Conversation      *dirextalkdomain.ConversationView `json:"conversation,omitempty"`
}

type ReactionHistory struct {
	Reaction dirextalkdomain.ReactionRecord `json:"reaction"`
	Channel  *Channel                       `json:"channel,omitempty"`
	Post     *Post                          `json:"post,omitempty"`
	Comment  *Comment                       `json:"comment,omitempty"`
}

// ContentStore is the durable repository used by channel content actions and
// by the Matrix projector/backfill adapters in the root package.
type ContentStore interface {
	InsertChannelPost(context.Context, dirextalkdomain.ChannelPostRecord) error
	GetChannelPostByID(context.Context, string, string) (dirextalkdomain.ChannelPostRecord, bool, error)
	GetChannelPostByEventID(context.Context, string, string) (dirextalkdomain.ChannelPostRecord, bool, error)
	ListChannelPosts(context.Context, string) ([]dirextalkdomain.ChannelPostRecord, error)
	ListChannelPostsPage(context.Context, string, int64, int64, int64, string, int) ([]dirextalkdomain.ChannelPostRecord, bool, error)
	ListChannelPostsByVisibilityPage(context.Context, string, string, int64, int) ([]dirextalkdomain.ChannelPostRecord, bool, error)
	InsertChannelComment(context.Context, dirextalkdomain.ChannelCommentRecord) error
	GetChannelCommentByID(context.Context, string, string) (dirextalkdomain.ChannelCommentRecord, bool, error)
	GetChannelCommentByEventID(context.Context, string, string) (dirextalkdomain.ChannelCommentRecord, bool, error)
	ListChannelComments(context.Context, string) ([]dirextalkdomain.ChannelCommentRecord, error)
	ListChannelCommentsPage(context.Context, string, int64, int64, int64, string, int) ([]dirextalkdomain.ChannelCommentRecord, bool, error)
	DeleteChannelPost(context.Context, string) (bool, error)
	DeleteChannelComment(context.Context, string) (bool, error)
	UpsertReaction(context.Context, dirextalkdomain.ReactionRecord) error
	GetReaction(context.Context, string, string, string, string) (dirextalkdomain.ReactionRecord, bool, error)
	DeactivateReactionByEventID(context.Context, string) (bool, error)
	CountActiveReactions(context.Context, string, string, string) (int64, error)
	ListReactions(context.Context, string) ([]dirextalkdomain.ReactionRecord, error)
}

// ChannelCatalog supplies channel room resolution and current-count
// enrichment without exposing the root Service.
type ChannelCatalog interface {
	ByIDOrRoom(context.Context, string, string) (Channel, bool, error)
	WithCurrentCounts(context.Context, Channel) (Channel, error)
}

type MatrixContentPort interface {
	SendMessage(context.Context, dirextalktransport.SendMessageRequest) (dirextalktransport.SendMessageResult, error)
	RedactEvent(context.Context, dirextalktransport.RedactEventRequest) (dirextalktransport.RedactEventResult, error)
}

type ContentConversationPort interface {
	Operation(context.Context, string, string, string) (map[string]any, *dirextalkdomain.ConversationView, error)
	AttachOperation(context.Context, map[string]any, string, string, string) error
}

type ContentOwner struct {
	MXID        string
	DisplayName string
}

// ContentConfig contains side-effect and identity boundaries. NewToken and
// NewEventID preserve the legacy synthetic identifiers when Matrix transport
// is absent; MapTransportError preserves ProductPolicy error status mapping.
type ContentConfig struct {
	Owner             func() ContentOwner
	Matrix            func() MatrixContentPort
	Now               func() time.Time
	NewToken          func(string) string
	NewEventID        func(string) string
	AuthorizeRecall   func(context.Context, string, string) *actionbase.Error
	MapTransportError func(error) *actionbase.Error
}

type ContentModule struct {
	store        ContentStore
	channels     ChannelCatalog
	matrix       MatrixContentPort
	conversation ContentConversationPort
	config       ContentConfig
}

func NewContent(store ContentStore, channels ChannelCatalog, matrix MatrixContentPort, conversation ContentConversationPort, cfg ContentConfig) *ContentModule {
	return &ContentModule{store: store, channels: channels, matrix: matrix, conversation: conversation, config: cfg}
}

func (m *ContentModule) Handlers() map[string]actionbase.Handler {
	return map[string]actionbase.Handler{
		actionPostsList:       m.Posts,
		actionPostsCreate:     m.CreatePost,
		actionPostsRecall:     m.recall(actionPostsRecall),
		actionCommentsRecall:  m.recall(actionCommentsRecall),
		actionCommentsList:    m.Comments,
		actionCommentsCreate:  m.CreateComment,
		actionPostReaction:    m.reaction(actionPostReaction),
		actionCommentReaction: m.reaction(actionCommentReaction),
		actionMyComments:      m.MyComments,
		actionMyReactions:     m.MyReactions,
	}
}

func (m *ContentModule) owner() ContentOwner {
	if m.config.Owner == nil {
		return ContentOwner{}
	}
	return m.config.Owner()
}

func (m *ContentModule) matrixPort() MatrixContentPort {
	if m.config.Matrix != nil {
		return m.config.Matrix()
	}
	return m.matrix
}

func (m *ContentModule) now() time.Time {
	if m.config.Now == nil {
		return time.Now().UTC()
	}
	return m.config.Now().UTC()
}

func (m *ContentModule) token(prefix string) string {
	if m.config.NewToken != nil {
		return m.config.NewToken(prefix)
	}
	return fmt.Sprintf("%s_%d", prefix, m.now().UnixNano())
}

func (m *ContentModule) eventID(contentID string) string {
	if m.config.NewEventID != nil {
		return m.config.NewEventID(contentID)
	}
	return "$" + strings.TrimSpace(contentID)
}

func (m *ContentModule) transportError(err error) *actionbase.Error {
	if m.config.MapTransportError != nil {
		return m.config.MapTransportError(err)
	}
	return actionbase.InternalError(err)
}

func (m *ContentModule) roomIDForChannel(ctx context.Context, channelID, fallbackRoomID string) (string, *actionbase.Error) {
	if roomID := strings.TrimSpace(fallbackRoomID); roomID != "" {
		return roomID, nil
	}
	channelID = strings.TrimSpace(channelID)
	if channelID == "" {
		return "", nil
	}
	if m.channels == nil {
		return "", actionbase.InternalError(errors.New("channel catalog is not configured"))
	}
	channel, ok, err := m.channels.ByIDOrRoom(ctx, channelID, "")
	if err != nil {
		return "", actionbase.InternalError(err)
	}
	if !ok {
		return "", nil
	}
	return channel.RoomID, nil
}

func postFromRecord(record dirextalkdomain.ChannelPostRecord) Post {
	return Post{
		PostID: record.PostID, ChannelID: record.ChannelID, RoomID: record.RoomID,
		EventID: record.EventID, AuthorMXID: record.AuthorMXID, AuthorName: record.AuthorName,
		Body: record.Body, MessageType: record.MessageType, MediaJSON: record.MediaJSON,
		Visibility:     dirextalkdomain.NormalizeChannelPostVisibility(record.Visibility),
		OriginServerTS: record.OriginServerTS, CommentCount: record.CommentCount,
	}
}

// PostFromRecord converts the durable post into its ProductCore DTO.
func PostFromRecord(record dirextalkdomain.ChannelPostRecord) Post {
	return postFromRecord(record)
}

func postRecord(post Post) dirextalkdomain.ChannelPostRecord {
	return dirextalkdomain.ChannelPostRecord{
		PostID: post.PostID, ChannelID: post.ChannelID, RoomID: post.RoomID,
		EventID: post.EventID, AuthorMXID: post.AuthorMXID, AuthorName: post.AuthorName,
		Body: post.Body, MessageType: post.MessageType, MediaJSON: post.MediaJSON,
		Visibility:     dirextalkdomain.NormalizeChannelPostVisibility(post.Visibility),
		OriginServerTS: post.OriginServerTS, CommentCount: post.CommentCount,
	}
}

// PostRecord strips request-scoped enrichment before persistence.
func PostRecord(post Post) dirextalkdomain.ChannelPostRecord {
	return postRecord(post)
}

func commentFromRecord(record dirextalkdomain.ChannelCommentRecord) Comment {
	return Comment{
		CommentID: record.CommentID, PostID: record.PostID, ChannelID: record.ChannelID,
		EventID: record.EventID, AuthorMXID: record.AuthorMXID, AuthorName: record.AuthorName,
		Body: record.Body, MessageType: record.MessageType, MediaJSON: record.MediaJSON,
		ReplyToCommentID: record.ReplyToCommentID, ReplyToAuthorMXID: record.ReplyToAuthorMXID,
		MentionsJSON: record.MentionsJSON, OriginServerTS: record.OriginServerTS,
	}
}

// CommentFromRecord converts the durable comment into its ProductCore DTO.
func CommentFromRecord(record dirextalkdomain.ChannelCommentRecord) Comment {
	return commentFromRecord(record)
}

func commentRecord(comment Comment) dirextalkdomain.ChannelCommentRecord {
	return dirextalkdomain.ChannelCommentRecord{
		CommentID: comment.CommentID, PostID: comment.PostID, ChannelID: comment.ChannelID,
		EventID: comment.EventID, AuthorMXID: comment.AuthorMXID, AuthorName: comment.AuthorName,
		Body: comment.Body, MessageType: comment.MessageType, MediaJSON: comment.MediaJSON,
		ReplyToCommentID: comment.ReplyToCommentID, ReplyToAuthorMXID: comment.ReplyToAuthorMXID,
		MentionsJSON: comment.MentionsJSON, OriginServerTS: comment.OriginServerTS,
	}
}

// CommentRecord strips request-scoped enrichment before persistence.
func CommentRecord(comment Comment) dirextalkdomain.ChannelCommentRecord {
	return commentRecord(comment)
}

func mediaPayload(value any) (string, map[string]any, error) {
	if value == nil {
		return "", nil, nil
	}
	if text, ok := value.(string); ok {
		text = strings.TrimSpace(text)
		if text == "" {
			return "", nil, nil
		}
		var media map[string]any
		if err := json.Unmarshal([]byte(text), &media); err != nil {
			return "", nil, err
		}
		raw, err := json.Marshal(media)
		return string(raw), media, err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return "", nil, err
	}
	var media map[string]any
	if err = json.Unmarshal(raw, &media); err != nil {
		return "", nil, err
	}
	normalized, err := json.Marshal(media)
	return string(normalized), media, err
}

func jsonArray(value any) (string, error) {
	if value == nil {
		return "[]", nil
	}
	if text, ok := value.(string); ok {
		text = strings.TrimSpace(text)
		if text == "" {
			return "[]", nil
		}
		var items []any
		if err := json.Unmarshal([]byte(text), &items); err != nil {
			return "", err
		}
		raw, err := json.Marshal(items)
		return string(raw), err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	var items []any
	if err = json.Unmarshal(raw, &items); err != nil {
		return "", err
	}
	normalized, err := json.Marshal(items)
	return string(normalized), err
}
