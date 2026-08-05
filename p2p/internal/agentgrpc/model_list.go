package agentgrpc

import (
	"context"
	"errors"
	"strings"

	agentv1 "github.com/YingSuiAI/dirextalk-agent/api/gen/dirextalk/agent/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const maximumModelListItems = 10_000

func (runner *Runner) invokeModelList(ctx context.Context, params map[string]any) (map[string]any, error) {
	if runner == nil || runner.runtime == nil {
		return nil, errors.New("agent service client is unavailable")
	}
	credential, requestedProvider, err := parseTransientModelDiscoveryCredential(params)
	if err != nil {
		return nil, err
	}
	defer credential.clear()
	requestID := uuid.NewString()
	transientModel, err := runner.bootstrapTransientModel(ctx, requestID, credential)
	if err != nil {
		return nil, err
	}
	defer clear(transientModel.CredentialSha256)
	callContext, cancel := context.WithTimeout(ctx, runner.chainTimeout)
	defer cancel()
	response, err := runner.runtime.ListModels(callContext, &agentv1.ListModelsRequest{
		RequestId: requestID, OwnerId: runner.ownerID, TransientModel: transientModel,
	})
	if err != nil {
		return nil, sanitizeModelListRPCError(callContext, err)
	}
	if response == nil || len(response.GetModels()) > maximumModelListItems {
		return nil, errors.New("agent service returned an invalid model list")
	}
	models := make([]map[string]any, 0, len(response.GetModels()))
	seen := make(map[string]struct{}, len(response.GetModels()))
	for _, model := range response.GetModels() {
		if model == nil {
			return nil, errors.New("agent service returned an invalid model list")
		}
		id := strings.TrimSpace(model.GetId())
		name := strings.TrimSpace(model.GetName())
		if id == "" || len(id) > 512 || strings.ContainsAny(id, "\x00\r\n\t") ||
			len(name) > 512 || strings.ContainsAny(name, "\x00\r\n\t") ||
			model.GetContextWindow() < 0 || model.GetMaxOutputTokens() < 0 {
			return nil, errors.New("agent service returned an invalid model list")
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		if name == "" {
			name = id
		}
		item := map[string]any{"id": id, "name": name, "provider": requestedProvider}
		if model.GetContextWindow() > 0 {
			item["context_window"] = model.GetContextWindow()
		}
		if model.GetMaxOutputTokens() > 0 {
			item["max_output_tokens"] = model.GetMaxOutputTokens()
		}
		if len(model.GetReasoningModes()) > 0 {
			modes := make([]string, 0, len(model.GetReasoningModes()))
			for _, mode := range model.GetReasoningModes() {
				mode = strings.TrimSpace(mode)
				if mode == "" || len(mode) > 128 || strings.ContainsAny(mode, "\x00\r\n\t") {
					return nil, errors.New("agent service returned an invalid model list")
				}
				modes = append(modes, mode)
			}
			item["reasoning_modes"] = modes
		}
		models = append(models, item)
	}
	if len(models) == 0 {
		return nil, errors.New("model provider returned no selectable models")
	}
	return map[string]any{"models": models}, nil
}

func sanitizeModelListRPCError(ctx context.Context, err error) error {
	grpcStatus, _ := status.FromError(err)
	switch {
	case grpcStatus.Code() == codes.PermissionDenied &&
		grpcStatus.Message() == "model provider rejected the supplied credential":
		return codedRunnerError{
			message: "model provider rejected the API key",
			code:    "M_AGENT_MODEL_CREDENTIAL_REJECTED",
		}
	case grpcStatus.Code() == codes.FailedPrecondition &&
		grpcStatus.Message() == "model provider does not expose a compatible model list":
		return codedRunnerError{
			message: "model provider does not support model discovery",
			code:    "M_AGENT_MODEL_LIST_UNSUPPORTED",
		}
	default:
		return sanitizeRPCError(ctx, err)
	}
}
