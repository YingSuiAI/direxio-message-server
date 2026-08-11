package agentgrpc

import (
	"context"
	"errors"
	"io"
	"strings"

	agentv1 "github.com/YingSuiAI/dirextalk-agent/api/gen/dirextalk/agent/v1"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentartifact"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const maximumTeamArtifactChunkBytes = 64 << 10

func (runner *Runner) DownloadTeamArtifact(
	ctx context.Context,
	artifactID string,
) (agentartifact.Download, error) {
	if runner == nil || runner.team == nil ||
		!canonicalUUID(artifactID) {
		return agentartifact.Download{}, agentartifact.ErrInvalid
	}
	callContext, cancel := context.WithTimeout(ctx, runner.chainTimeout)
	defer cancel()
	stream, err := runner.team.DownloadTeamArtifactV3(
		callContext,
		&agentv1.DownloadTeamArtifactV3Request{
			OwnerId:    runner.ownerID,
			ArtifactId: artifactID,
		},
	)
	if err != nil {
		return agentartifact.Download{}, artifactRPCError(callContext, err)
	}
	first, err := stream.Recv()
	if err != nil {
		return agentartifact.Download{}, artifactRPCError(callContext, err)
	}
	metadata := first.GetArtifact()
	if metadata == nil || len(first.GetData()) != 0 ||
		first.GetOffset() != 0 || first.GetComplete() ||
		metadata.GetSchemaVersion() != "dirextalk.agent.team-artifact/v1" ||
		metadata.GetArtifactId() != artifactID ||
		!canonicalUUID(metadata.GetArtifactId()) ||
		metadata.GetVerification() != "passed" ||
		metadata.GetSizeBytes() < 1 ||
		metadata.GetSizeBytes() > agentartifact.MaximumBytes ||
		strings.TrimSpace(metadata.GetName()) != metadata.GetName() {
		return agentartifact.Download{}, agentartifact.ErrInvalid
	}
	content := make([]byte, 0, int(metadata.GetSizeBytes()))
	defer clear(content)
	var complete bool
	for !complete {
		response, receiveErr := stream.Recv()
		if receiveErr != nil {
			return agentartifact.Download{}, artifactRPCError(
				callContext,
				receiveErr,
			)
		}
		chunk := response.GetData()
		if response.GetArtifact() != nil ||
			response.GetOffset() != int64(len(content)) ||
			len(chunk) < 1 || len(chunk) > maximumTeamArtifactChunkBytes ||
			len(content)+len(chunk) > int(metadata.GetSizeBytes()) ||
			(response.GetComplete() &&
				len(content)+len(chunk) != int(metadata.GetSizeBytes())) {
			return agentartifact.Download{}, agentartifact.ErrInvalid
		}
		content = append(content, chunk...)
		complete = response.GetComplete()
	}
	if trailing, receiveErr := stream.Recv(); !errors.Is(receiveErr, io.EOF) || trailing != nil {
		return agentartifact.Download{}, agentartifact.ErrInvalid
	}
	download, err := agentartifact.NewDownload(
		metadata.GetArtifactId(),
		metadata.GetName(),
		metadata.GetMediaType(),
		metadata.GetSizeBytes(),
		metadata.GetSha256(),
		content,
	)
	if err != nil {
		return agentartifact.Download{}, err
	}
	return download, nil
}

func artifactRPCError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return agentartifact.ErrUnavailable
	}
	switch status.Code(err) {
	case codes.NotFound:
		return agentartifact.ErrNotFound
	case codes.InvalidArgument:
		return agentartifact.ErrInvalid
	default:
		return agentartifact.ErrUnavailable
	}
}
