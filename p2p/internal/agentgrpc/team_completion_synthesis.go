package agentgrpc

import (
	"context"
	"errors"
	"strings"

	agentv1 "github.com/YingSuiAI/dirextalk-agent/api/gen/dirextalk/agent/v1"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcompletion"
)

// SynthesizeTeamCompletion asks Central to author and durably persist the
// assistant reply. Message Server never receives evidence from which it could
// independently compose user-visible text.
func (runner *Runner) SynthesizeTeamCompletion(
	ctx context.Context,
	ownerID string,
	sourceEventID string,
) (agentcompletion.Synthesis, error) {
	if runner == nil || runner.runtime == nil ||
		strings.TrimSpace(ownerID) != runner.ownerID ||
		!canonicalUUID(sourceEventID) {
		return agentcompletion.Synthesis{}, errors.New("Agent completion synthesis request is invalid")
	}
	callContext, cancel := context.WithTimeout(ctx, runner.chainTimeout)
	defer cancel()
	response, err := runner.runtime.SynthesizeTeamCompletion(
		callContext,
		&agentv1.SynthesizeTeamCompletionRequest{
			OwnerId:       runner.ownerID,
			SourceEventId: sourceEventID,
		},
	)
	if err != nil {
		return agentcompletion.Synthesis{}, sanitizeRPCError(callContext, err)
	}
	if response == nil || response.GetMessage() == nil {
		return agentcompletion.Synthesis{}, errors.New("agent service returned an invalid Team completion synthesis")
	}
	message := response.GetMessage()
	result := agentcompletion.Synthesis{
		SourceEventID:  response.GetSourceEventId(),
		ConversationID: response.GetConversationId(),
		Message: agentcompletion.AssistantMessage{
			MessageID: message.GetMessageId(),
			Content:   message.GetContent(),
		},
		ConversationRevision: response.GetConversationRevision(),
	}
	if result.SourceEventID != sourceEventID ||
		!teamConversationIDPattern.MatchString(result.ConversationID) ||
		!canonicalUUID(result.Message.MessageID) ||
		!agentcompletion.ValidAssistantContent(result.Message.Content) ||
		result.ConversationRevision < 1 {
		return agentcompletion.Synthesis{}, errors.New("agent service returned an invalid Team completion synthesis")
	}
	return result, nil
}

// GetConversationState returns Central's authoritative durable revision for a
// conversation. Owner identity is always injected from trusted Runner config.
func (runner *Runner) GetConversationState(
	ctx context.Context,
	conversationID string,
) (int64, bool, error) {
	if runner == nil || runner.runtime == nil ||
		!teamConversationIDPattern.MatchString(conversationID) {
		return 0, false, errors.New("Agent conversation state request is invalid")
	}
	callContext, cancel := context.WithTimeout(ctx, runner.chainTimeout)
	defer cancel()
	response, err := runner.runtime.GetConversationState(
		callContext,
		&agentv1.GetConversationStateRequest{
			OwnerId:        runner.ownerID,
			ConversationId: conversationID,
		},
	)
	if err != nil {
		return 0, false, sanitizeRPCError(callContext, err)
	}
	if response == nil {
		return 0, false, errors.New("agent service returned an invalid conversation state")
	}
	found := response.GetFound()
	revision := response.GetConversationRevision()
	if revision < 0 || (!found && revision != 0) {
		return 0, false, errors.New("agent service returned an invalid conversation state")
	}
	return revision, found, nil
}
