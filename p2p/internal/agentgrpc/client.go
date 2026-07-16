// Package agentgrpc adapts the versioned Agent gRPC runtime contract to the
// Message Server's NativeAgentRunner boundary.
package agentgrpc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"strings"
	"time"

	agentv1 "github.com/YingSuiAI/dirextalk-agent/api/gen/dirextalk/agent/v1"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/nativeagent"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

const (
	defaultUnaryTimeout    = 90 * time.Second
	defaultStreamTimeout   = 10 * time.Minute
	defaultMaxMessageBytes = 4 << 20
	maximumSecretFileBytes = 4096
	authorizationScheme    = "DTX-Service-Key"
)

var serviceKeyIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

var (
	errUnrepresentableChatParameters = errors.New("agent chat parameters cannot be represented by the remote runtime contract")
	errInvalidStreamSequence         = errors.New("agent service returned an invalid stream sequence")
)

// Config contains only transport configuration and one trusted owner binding.
// OwnerID is never accepted from user-controlled action parameters.
type Config struct {
	Target          string
	CAFile          string
	ServerName      string
	ServiceKeyFile  string
	OwnerID         string
	UnaryTimeout    time.Duration
	StreamTimeout   time.Duration
	MaxSendBytes    int
	MaxReceiveBytes int
}

// Runner implements the Message Server NativeAgentRunner contract over the
// versioned Agent RuntimeService gRPC API.
type Runner struct {
	connection    *grpc.ClientConn
	runtime       agentv1.RuntimeServiceClient
	ownerID       string
	chainTimeout  time.Duration
	streamTimeout time.Duration
}

// New validates mounted trust material and creates a TLS 1.3-only gRPC client.
func New(ctx context.Context, config Config) (*Runner, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	config.Target = strings.TrimSpace(config.Target)
	config.CAFile = strings.TrimSpace(config.CAFile)
	config.ServerName = strings.TrimSpace(config.ServerName)
	config.ServiceKeyFile = strings.TrimSpace(config.ServiceKeyFile)
	config.OwnerID = strings.TrimSpace(config.OwnerID)
	if config.Target == "" || config.CAFile == "" || config.ServerName == "" || config.ServiceKeyFile == "" || config.OwnerID == "" {
		return nil, errors.New("agent gRPC target, CA, server name, service key file, and owner are required")
	}
	if strings.ContainsAny(config.ServerName, "\x00/\\") {
		return nil, errors.New("agent gRPC server name is invalid")
	}
	roots, err := loadCertPool(config.CAFile)
	if err != nil {
		return nil, err
	}
	if err = validateMountedServiceKey(config.ServiceKeyFile); err != nil {
		return nil, err
	}

	unaryTimeout := positiveDurationOr(config.UnaryTimeout, defaultUnaryTimeout)
	streamTimeout := positiveDurationOr(config.StreamTimeout, defaultStreamTimeout)
	maxSend := positiveIntOr(config.MaxSendBytes, defaultMaxMessageBytes)
	maxReceive := positiveIntOr(config.MaxReceiveBytes, defaultMaxMessageBytes)
	tlsConfig := &tls.Config{
		RootCAs: roots, ServerName: config.ServerName,
		MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
	}
	connection, err := grpc.NewClient(
		config.Target,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
		grpc.WithPerRPCCredentials(mountedServiceKeyCredentials{path: config.ServiceKeyFile}),
		grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(maxSend), grpc.MaxCallRecvMsgSize(maxReceive)),
	)
	if err != nil {
		return nil, errors.New("create agent gRPC client: transport configuration rejected")
	}
	return &Runner{
		connection: connection, runtime: agentv1.NewRuntimeServiceClient(connection), ownerID: config.OwnerID,
		chainTimeout: unaryTimeout, streamTimeout: streamTimeout,
	}, nil
}

// Close releases the underlying gRPC connection.
func (runner *Runner) Close() error {
	if runner == nil || runner.connection == nil {
		return nil
	}
	return runner.connection.Close()
}

// Apply is deliberately unsupported: RuntimeService has no implicit lifecycle
// mutation corresponding to the legacy in-process enable/disable hook.
func (runner *Runner) Apply(context.Context, string) error {
	return errors.New("agent service action is not supported")
}

// Invoke maps agent.chat to RuntimeService.Chat.
func (runner *Runner) Invoke(ctx context.Context, action string, params map[string]any) (map[string]any, error) {
	if runner == nil || runner.runtime == nil {
		return nil, errors.New("agent service client is unavailable")
	}
	if strings.TrimSpace(action) != "agent.chat" {
		return nil, errors.New("agent service action is not supported")
	}
	request, err := runner.chatRequest(params)
	if err != nil {
		return nil, err
	}
	callContext, cancel := context.WithTimeout(ctx, runner.chainTimeout)
	defer cancel()
	response, err := runner.runtime.Chat(callContext, request)
	if err != nil {
		return nil, sanitizeRPCError(callContext, err)
	}
	return mapChatResponse(response)
}

// Stream maps agent.chat.stream to RuntimeService.StreamChat and preserves
// caller cancellation through the gRPC stream context.
func (runner *Runner) Stream(ctx context.Context, action string, params map[string]any, emit func(nativeagent.Event) error) error {
	if runner == nil || runner.runtime == nil {
		return errors.New("agent service client is unavailable")
	}
	if strings.TrimSpace(action) != "agent.chat.stream" {
		return errors.New("agent service action is not supported")
	}
	if emit == nil {
		return errors.New("agent stream emitter is required")
	}
	request, err := runner.streamChatRequest(params)
	if err != nil {
		return err
	}
	callContext, cancel := context.WithTimeout(ctx, runner.streamTimeout)
	defer cancel()
	stream, err := runner.runtime.StreamChat(callContext, request)
	if err != nil {
		return sanitizeRPCError(callContext, err)
	}
	var terminal *nativeagent.Event
	for {
		response, receiveErr := stream.Recv()
		if errors.Is(receiveErr, io.EOF) {
			if terminal == nil {
				return errInvalidStreamSequence
			}
			return emit(*terminal)
		}
		if receiveErr != nil {
			return sanitizeRPCError(callContext, receiveErr)
		}
		if terminal != nil {
			return errInvalidStreamSequence
		}
		event, mapErr := mapStreamResponse(response)
		if mapErr != nil {
			return mapErr
		}
		if event.Event == "done" {
			terminal = &event
			continue
		}
		if err := emit(event); err != nil {
			return err
		}
	}
}

func (runner *Runner) chatRequest(params map[string]any) (*agentv1.ChatRequest, error) {
	request, err := runner.requestFields(params)
	if err != nil {
		return nil, err
	}
	return &agentv1.ChatRequest{
		IdempotencyKey: request.idempotencyKey, OwnerId: runner.ownerID, ConversationId: request.conversationID,
		Message: request.message, MemoryDisabled: request.memoryDisabled, ExpectedConversationRevision: request.expectedRevision,
	}, nil
}

func (runner *Runner) streamChatRequest(params map[string]any) (*agentv1.StreamChatRequest, error) {
	request, err := runner.requestFields(params)
	if err != nil {
		return nil, err
	}
	return &agentv1.StreamChatRequest{
		IdempotencyKey: request.idempotencyKey, OwnerId: runner.ownerID, ConversationId: request.conversationID,
		Message: request.message, MemoryDisabled: request.memoryDisabled, ExpectedConversationRevision: request.expectedRevision,
	}, nil
}

type chatRequestFields struct {
	idempotencyKey   string
	conversationID   string
	message          string
	memoryDisabled   bool
	expectedRevision int64
}

func (*Runner) requestFields(params map[string]any) (chatRequestFields, error) {
	if err := validateLegacyCompatibilityEnvelope(params); err != nil {
		return chatRequestFields{}, err
	}
	idempotencyKey := stringParam(params, "idempotency_key")
	if idempotencyKey == "" {
		idempotencyKey = uuid.NewString()
	} else if _, err := uuid.Parse(idempotencyKey); err != nil {
		return chatRequestFields{}, errors.New("invalid agent chat parameters: idempotency_key must be a UUID")
	}
	message := stringParam(params, "prompt")
	if message == "" {
		message = stringParam(params, "message")
	}
	if message == "" {
		return chatRequestFields{}, errors.New("invalid agent chat parameters: prompt is required")
	}
	expectedRevision, err := nonnegativeInt64(params["expected_conversation_revision"])
	if err != nil {
		return chatRequestFields{}, errors.New("invalid agent chat parameters: expected_conversation_revision must be a non-negative integer")
	}
	memoryDisabled, _ := params["memory_disabled"].(bool)
	return chatRequestFields{
		idempotencyKey: idempotencyKey, conversationID: stringParam(params, "conversation_id"), message: message,
		memoryDisabled: memoryDisabled, expectedRevision: expectedRevision,
	}, nil
}

func validateLegacyCompatibilityEnvelope(params map[string]any) error {
	for key, value := range params {
		switch key {
		case "prompt", "message", "owner_id", "conversation_id", "idempotency_key", "model_profile_id":
			if _, ok := value.(string); !ok {
				return errUnrepresentableChatParameters
			}
		case "memory_disabled":
			if _, ok := value.(bool); !ok {
				return errUnrepresentableChatParameters
			}
		case "expected_conversation_revision":
			// Parsed below without coercing strings or fractional JSON numbers.
		case "conversation_context", "model_profile":
			// These legacy envelopes may contain history or request-scoped model
			// credentials. The independent Agent owns both concerns, so validate
			// only the outer type and never inspect, copy, or serialize the values.
			if value != nil {
				if _, ok := value.(map[string]any); !ok {
					return errUnrepresentableChatParameters
				}
			}
		case "cloud_dialogue_mode", "knowledge_enabled":
			enabled, ok := value.(bool)
			if !ok || enabled {
				return errUnrepresentableChatParameters
			}
		case "attachments":
			if value == nil {
				continue
			}
			attachments, ok := value.([]any)
			if !ok || len(attachments) != 0 {
				return errUnrepresentableChatParameters
			}
		case "embedding_profile", "cloud_connection_id", "cloud_recipe_id", "cloud_recipe_revision",
			"messages", "system_prompt", "enabled_tools":
			return errUnrepresentableChatParameters
		default:
			return errUnrepresentableChatParameters
		}
	}
	return nil
}

func mapChatResponse(response *agentv1.ChatResponse) (map[string]any, error) {
	if response == nil || response.GetMessage() == nil {
		return nil, errors.New("agent service returned an invalid response")
	}
	steps := make([]map[string]any, 0, len(response.GetSteps()))
	for _, step := range response.GetSteps() {
		if step == nil {
			continue
		}
		steps = append(steps, map[string]any{
			"kind": runtimeStepKind(step.GetKind()), "tool_call_id": step.GetToolCallId(),
			"tool_name": step.GetToolName(), "is_error": step.GetIsError(),
		})
	}
	return map[string]any{
		"ok": true, "native": true, "framework": "eino", "text": response.GetMessage().GetContent(),
		"message_id": response.GetMessage().GetMessageId(), "conversation_id": response.GetConversationId(),
		"conversation_revision": response.GetConversationRevision(), "steps": steps,
		"related_task_ids": append([]string(nil), response.GetRelatedTaskIds()...),
		"related_plan_ids": append([]string(nil), response.GetRelatedPlanIds()...),
	}, nil
}

func mapStreamResponse(response *agentv1.StreamChatResponse) (nativeagent.Event, error) {
	if response == nil {
		return nativeagent.Event{}, errors.New("agent service returned an invalid stream response")
	}
	switch event := response.GetEvent().(type) {
	case *agentv1.StreamChatResponse_Delta:
		if event.Delta == nil {
			break
		}
		return nativeagent.Event{Event: "delta", Data: map[string]any{"message_id": event.Delta.GetMessageId(), "text": event.Delta.GetContent()}}, nil
	case *agentv1.StreamChatResponse_Tool:
		if event.Tool == nil {
			break
		}
		return nativeagent.Event{Event: "tool", Data: map[string]any{
			"tool_call_id": event.Tool.GetToolCallId(), "tool_name": event.Tool.GetToolName(),
			"finished": event.Tool.GetFinished(), "is_error": event.Tool.GetIsError(),
		}}, nil
	case *agentv1.StreamChatResponse_Done:
		if event.Done == nil {
			break
		}
		data, err := mapChatResponse(event.Done.GetResponse())
		if err != nil {
			return nativeagent.Event{}, err
		}
		return nativeagent.Event{Event: "done", Data: data}, nil
	}
	return nativeagent.Event{}, errors.New("agent service returned an invalid stream response")
}

func runtimeStepKind(kind agentv1.RuntimeStepKind) string {
	switch kind {
	case agentv1.RuntimeStepKind_RUNTIME_STEP_KIND_MODEL:
		return "model"
	case agentv1.RuntimeStepKind_RUNTIME_STEP_KIND_TOOL_CALL:
		return "tool_call"
	case agentv1.RuntimeStepKind_RUNTIME_STEP_KIND_TOOL_RESULT:
		return "tool_result"
	default:
		return "unspecified"
	}
}

type mountedServiceKeyCredentials struct{ path string }

func (credentials mountedServiceKeyCredentials) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	key, err := readMountedServiceKey(credentials.path)
	if err != nil {
		return nil, err
	}
	return map[string]string{"authorization": authorizationScheme + " " + key}, nil
}

func (mountedServiceKeyCredentials) RequireTransportSecurity() bool { return true }

func readMountedServiceKey(path string) (string, error) {
	raw, err := loadMountedServiceKey(path)
	if err != nil {
		return "", err
	}
	defer clear(raw)
	value := bytes.TrimSpace(raw)
	if err := validateServiceKey(value); err != nil {
		return "", err
	}
	// gRPC metadata requires an immutable string. This is the only intentional
	// secret copy; the mounted-file buffer and decoded validation bytes are
	// cleared before the RPC begins.
	return string(value), nil
}

func validateMountedServiceKey(path string) error {
	raw, err := loadMountedServiceKey(path)
	if err != nil {
		return err
	}
	defer clear(raw)
	return validateServiceKey(bytes.TrimSpace(raw))
}

func loadMountedServiceKey(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("load mounted agent service key: unavailable")
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maximumSecretFileBytes+1))
	if err != nil || len(raw) > maximumSecretFileBytes {
		clear(raw)
		return nil, errors.New("load mounted agent service key: unavailable")
	}
	return raw, nil
}

func validateServiceKey(value []byte) error {
	separator := bytes.IndexByte(value, '.')
	if separator < 1 || separator > 128 || bytes.IndexByte(value[separator+1:], '.') >= 0 || !serviceKeyIDPattern.Match(value[:separator]) {
		return errors.New("load mounted agent service key: invalid")
	}
	encodedSecret := value[separator+1:]
	secret := make([]byte, base64.RawURLEncoding.DecodedLen(len(encodedSecret)))
	decoded, decodeErr := base64.RawURLEncoding.Decode(secret, encodedSecret)
	defer clear(secret)
	if decodeErr != nil || decoded != sha256.Size {
		return errors.New("load mounted agent service key: invalid")
	}
	return nil
}

func loadCertPool(path string) (*x509.CertPool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("load agent gRPC CA: unavailable")
	}
	defer clear(raw)
	pool := x509.NewCertPool()
	if len(raw) == 0 || !pool.AppendCertsFromPEM(raw) {
		return nil, errors.New("load agent gRPC CA: invalid PEM")
	}
	return pool, nil
}

func sanitizeRPCError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	code := status.Code(err)
	if code == codes.OK {
		code = codes.Unknown
	}
	return fmt.Errorf("agent service request failed (%s)", strings.ToLower(code.String()))
}

func stringParam(params map[string]any, key string) string {
	if params == nil {
		return ""
	}
	value, _ := params[key].(string)
	return strings.TrimSpace(value)
}

func nonnegativeInt64(value any) (int64, error) {
	if value == nil {
		return 0, nil
	}
	switch number := value.(type) {
	case int:
		if number < 0 {
			return 0, errors.New("negative")
		}
		return int64(number), nil
	case int64:
		if number < 0 {
			return 0, errors.New("negative")
		}
		return number, nil
	case float64:
		if number < 0 || number > math.MaxInt64 || math.Trunc(number) != number {
			return 0, errors.New("invalid")
		}
		return int64(number), nil
	case json.Number:
		parsed, err := number.Int64()
		if err != nil || parsed < 0 {
			return 0, errors.New("invalid")
		}
		return parsed, nil
	default:
		return 0, errors.New("invalid")
	}
}

func positiveDurationOr(value, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	return value
}

func positiveIntOr(value, fallback int) int {
	if value <= 0 {
		return fallback
	}
	return value
}
