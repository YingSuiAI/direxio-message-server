package mcp

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkmatrix"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkmcp"
)

func (m *Module) roomMembersList(ctx context.Context, params map[string]any) (any, *dirextalkmcp.Error) {
	roomID := dirextalkmcp.TrimString(params["room_id"])
	channelID := dirextalkmcp.TrimString(params["channel_id"])
	if roomID == "" && channelID == "" {
		return nil, dirextalkmcp.BadRequest("room_id or channel_id is required")
	}
	status := fallback(dirextalkmcp.TrimString(params["status"]), dirextalkmcp.TrimString(params["membership"]))
	role := dirextalkmcp.TrimString(params["role"])
	userID := dirextalkmcp.TrimString(params["user_id"])
	name := roomID
	knownRoom := false
	roomOwnerMXID := ""
	resolveRoomOwner := false
	if channel, ok, err := m.channels.ByIDOrRoom(ctx, channelID, roomID); err != nil {
		return nil, internalError(err)
	} else if ok {
		roomID = fallback(roomID, channel.RoomID)
		channelID = fallback(channelID, channel.ChannelID)
		name = fallback(channel.Name, roomID)
		knownRoom = true
	}
	if name == roomID {
		if group, ok, err := m.groups.ByRoom(ctx, roomID); err != nil {
			return nil, internalError(err)
		} else if ok {
			name = fallback(group.Name, roomID)
			knownRoom = true
		}
	}
	if record, ok, err := m.conversations.GetRecord(ctx, "", roomID); err != nil {
		return nil, internalError(err)
	} else if ok {
		knownRoom = true
		if record.Kind == dirextalkdomain.ConversationKindGroup || record.Kind == dirextalkdomain.ConversationKindChannel {
			roomOwnerMXID = record.CreatedByMXID
			resolveRoomOwner = m.config.ResolveRoomOwner != nil
		}
		view, viewErr := m.conversations.View(ctx, record)
		if viewErr != nil {
			return nil, internalError(viewErr)
		}
		if strings.TrimSpace(view.Title) != "" {
			name = view.Title
		}
	}
	if !knownRoom {
		return nil, dirextalkmcp.StatusError(http.StatusNotFound, "room not found")
	}
	if mcpErr := m.requireRoomAllowed(roomID); mcpErr != nil {
		return nil, mcpErr
	}
	if mcpErr := m.requireJoinedRoom(ctx, roomID); mcpErr != nil {
		return nil, mcpErr
	}
	if resolveRoomOwner {
		resolvedOwner, resolveErr := m.config.ResolveRoomOwner(ctx, roomID)
		if resolveErr != nil {
			return nil, internalError(resolveErr)
		}
		roomOwnerMXID = resolvedOwner
	}
	if m.members == nil {
		return nil, internalError(errors.New("member store is not configured"))
	}
	members, err := m.members.ListMembers(ctx, roomID, channelID)
	if err != nil {
		return nil, internalError(err)
	}
	summaries := make([]dirextalkmcp.MemberSummary, 0, len(members))
	for _, member := range members {
		if !dirextalkdomain.MemberMembershipJoined(member.Membership) {
			continue
		}
		summaries = append(summaries, memberSummary(member))
	}
	if matrixMembers, matrixErr := m.matrixRoomMembers(ctx, roomID); matrixErr != nil && len(summaries) == 0 {
		return nil, internalError(matrixErr)
	} else if matrixErr == nil {
		summaries = mergeMemberSummaries(summaries, matrixMembers)
	}
	joinedSummaries := summaries[:0]
	for _, summary := range summaries {
		if dirextalkdomain.MemberMembershipJoined(summary.Membership) {
			joinedSummaries = append(joinedSummaries, summary)
		}
	}
	summaries = joinedSummaries
	if len(summaries) == 0 {
		if directMembers, directName, mcpErr := m.directRoomMembers(ctx, roomID); mcpErr != nil {
			return nil, mcpErr
		} else if len(directMembers) > 0 {
			summaries = directMembers
			name = fallback(directName, name)
		}
	}
	if userID != "" {
		filtered := summaries[:0]
		for _, summary := range summaries {
			if memberSummaryMXID(summary) == userID {
				filtered = append(filtered, summary)
			}
		}
		summaries = filtered
	}
	summaries = canonicalizeAndSortMemberSummaries(summaries, roomOwnerMXID)
	summaries = filterMemberSummaries(summaries, status, role)
	summaries = m.enrichMemberSummariesWithProfiles(ctx, summaries)
	limit := dirextalkmcp.Limit(params)
	if len(summaries) > limit {
		summaries = summaries[:limit]
	}
	return map[string]any{
		"room_id": roomID,
		"name":    name,
		"members": summaries,
		"count":   len(summaries),
	}, nil
}

func canonicalizeAndSortMemberSummaries(members []dirextalkmcp.MemberSummary, ownerMXID string) []dirextalkmcp.MemberSummary {
	ownerMXID = strings.TrimSpace(ownerMXID)
	if ownerMXID != "" {
		for index := range members {
			userID := memberSummaryMXID(members[index])
			if userID == ownerMXID {
				members[index].Role = "owner"
			} else {
				members[index].Role = "member"
			}
		}
	}
	sort.SliceStable(members, func(i, j int) bool {
		leftID := memberSummaryMXID(members[i])
		rightID := memberSummaryMXID(members[j])
		if ownerMXID != "" {
			leftOwner := leftID == ownerMXID
			rightOwner := rightID == ownerMXID
			if leftOwner != rightOwner {
				return leftOwner
			}
		}
		leftJoined, leftHasJoined := memberSummaryJoinTime(members[i])
		rightJoined, rightHasJoined := memberSummaryJoinTime(members[j])
		if leftHasJoined != rightHasJoined {
			return leftHasJoined
		}
		if leftHasJoined && !leftJoined.Equal(rightJoined) {
			return leftJoined.Before(rightJoined)
		}
		return leftID < rightID
	})
	return members
}

func memberSummaryMXID(member dirextalkmcp.MemberSummary) string {
	return strings.TrimSpace(fallback(member.UserMXID, member.UserID))
}

func memberSummaryJoinTime(member dirextalkmcp.MemberSummary) (time.Time, bool) {
	joinedAt := strings.TrimSpace(member.JoinedAt)
	if joinedAt == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, joinedAt)
	return parsed, err == nil
}

func (m *Module) matrixRoomMembers(ctx context.Context, roomID string) ([]dirextalkdomain.MemberRecord, error) {
	roomID = strings.TrimSpace(roomID)
	if roomID == "" || m == nil || m.matrix == nil {
		return nil, nil
	}
	return m.matrix.ListRoomMembers(ctx, roomID)
}

func (m *Module) enrichMemberSummariesWithProfiles(ctx context.Context, summaries []dirextalkmcp.MemberSummary) []dirextalkmcp.MemberSummary {
	if len(summaries) == 0 {
		return summaries
	}
	profileResolver := m.profileResolver()
	if profileResolver == nil {
		return summaries
	}
	profileCache := map[string]dirextalkmatrix.Profile{}
	for index := range summaries {
		userID := strings.TrimSpace(fallback(summaries[index].UserMXID, summaries[index].UserID))
		if userID == "" {
			continue
		}
		needsDisplayName := needsProfileDisplayName(userID, summaries[index].DisplayName)
		needsAvatar := strings.TrimSpace(summaries[index].AvatarURL) == ""
		if !needsDisplayName && !needsAvatar {
			continue
		}
		profile, ok := resolveMatrixProfile(ctx, profileResolver, profileCache, userID)
		if !ok {
			continue
		}
		if needsDisplayName && strings.TrimSpace(profile.DisplayName) != "" {
			summaries[index].DisplayName = profile.DisplayName
		}
		if needsAvatar && strings.TrimSpace(profile.AvatarURL) != "" {
			summaries[index].AvatarURL = profile.AvatarURL
		}
	}
	return summaries
}

func resolveMatrixProfile(ctx context.Context, resolver ProfileResolver, cache map[string]dirextalkmatrix.Profile, userID string) (dirextalkmatrix.Profile, bool) {
	if resolver == nil {
		return dirextalkmatrix.Profile{}, false
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return dirextalkmatrix.Profile{}, false
	}
	if profile, ok := cache[userID]; ok {
		return profile, strings.TrimSpace(profile.DisplayName) != "" || strings.TrimSpace(profile.AvatarURL) != ""
	}
	profile, err := resolver.ResolveMatrixProfile(ctx, userID)
	if err != nil {
		cache[userID] = dirextalkmatrix.Profile{}
		return dirextalkmatrix.Profile{}, false
	}
	cache[userID] = profile
	return profile, strings.TrimSpace(profile.DisplayName) != "" || strings.TrimSpace(profile.AvatarURL) != ""
}

func needsProfileDisplayName(userID, displayName string) bool {
	displayName = strings.TrimSpace(displayName)
	return displayName == "" || strings.EqualFold(displayName, displayNameFromMXID(userID))
}

func mergeMemberSummaries(existing []dirextalkmcp.MemberSummary, matrixMembers []dirextalkdomain.MemberRecord) []dirextalkmcp.MemberSummary {
	indexByUser := make(map[string]int, len(existing)+len(matrixMembers))
	for index, member := range existing {
		userID := strings.TrimSpace(fallback(member.UserMXID, member.UserID))
		if userID != "" {
			indexByUser[userID] = index
		}
	}
	for _, member := range matrixMembers {
		summary := memberSummary(member)
		userID := strings.TrimSpace(fallback(summary.UserMXID, summary.UserID))
		if userID == "" {
			continue
		}
		if index, ok := indexByUser[userID]; ok {
			existing[index] = mergeMemberSummary(existing[index], summary)
			continue
		}
		if !dirextalkdomain.MemberMembershipJoined(summary.Membership) {
			continue
		}
		indexByUser[userID] = len(existing)
		existing = append(existing, summary)
	}
	return existing
}

func mergeMemberSummary(existing, incoming dirextalkmcp.MemberSummary) dirextalkmcp.MemberSummary {
	existingUserID := fallback(existing.UserMXID, existing.UserID)
	if strings.TrimSpace(existing.DisplayName) == "" || strings.TrimSpace(existing.DisplayName) == displayNameFromMXID(existingUserID) {
		existing.DisplayName = incoming.DisplayName
	}
	if strings.TrimSpace(existing.AvatarURL) == "" {
		existing.AvatarURL = incoming.AvatarURL
	}
	if strings.TrimSpace(incoming.Membership) != "" {
		existing.Membership = dirextalkdomain.NormalizeMemberMembership(incoming.Membership)
	}
	if strings.TrimSpace(existing.Role) == "" || strings.TrimSpace(existing.Role) == "member" {
		existing.Role = incoming.Role
	}
	if strings.TrimSpace(existing.JoinedAt) == "" {
		existing.JoinedAt = incoming.JoinedAt
	}
	return existing
}

func filterMemberSummaries(members []dirextalkmcp.MemberSummary, status, role string) []dirextalkmcp.MemberSummary {
	status = strings.ToLower(strings.TrimSpace(status))
	role = strings.ToLower(strings.TrimSpace(role))
	if status == "" && role == "" {
		return members
	}
	filtered := members[:0]
	for _, member := range members {
		if status != "" && strings.ToLower(strings.TrimSpace(member.Membership)) != status {
			continue
		}
		if role != "" && strings.ToLower(strings.TrimSpace(member.Role)) != role {
			continue
		}
		filtered = append(filtered, member)
	}
	return filtered
}

func (m *Module) directRoomMembers(ctx context.Context, roomID string) ([]dirextalkmcp.MemberSummary, string, *dirextalkmcp.Error) {
	record, ok, err := m.conversations.GetRecord(ctx, "", roomID)
	if err != nil {
		return nil, "", internalError(err)
	}
	if !ok || record.Kind != dirextalkdomain.ConversationKindDirect {
		return nil, "", nil
	}
	view, err := m.conversations.View(ctx, record)
	if err != nil {
		return nil, "", internalError(err)
	}
	if strings.TrimSpace(view.PeerMXID) == "" {
		return nil, fallback(view.Title, roomID), nil
	}
	identity := m.identity()
	members := make([]dirextalkmcp.MemberSummary, 0, 2)
	if strings.TrimSpace(identity.OwnerMXID) != "" {
		members = append(members, memberSummaryFromIdentity(
			identity.OwnerMXID,
			fallback(identity.OwnerProfile.DisplayName, displayNameFromMXID(identity.OwnerMXID)),
			identity.OwnerProfile.AvatarURL,
			"join",
			"owner",
			0,
		))
	}
	members = append(members, memberSummaryFromIdentity(
		view.PeerMXID,
		fallback(view.Title, displayNameFromMXID(view.PeerMXID)),
		view.AvatarURL,
		fallback(view.Membership, "join"),
		"member",
		0,
	))
	return members, fallback(view.Title, roomID), nil
}

func memberSummary(member dirextalkdomain.MemberRecord) dirextalkmcp.MemberSummary {
	return memberSummaryFromIdentity(
		member.UserID,
		member.DisplayName,
		member.AvatarURL,
		member.Membership,
		member.Role,
		member.JoinedAt,
	)
}

func memberSummaryFromIdentity(userID, displayName, avatarURL, membership, role string, joinedAt int64) dirextalkmcp.MemberSummary {
	userID = strings.TrimSpace(userID)
	return dirextalkmcp.MemberSummary{
		UserID:      userID,
		UserMXID:    userID,
		Localpart:   localpartFromMXID(userID),
		Domain:      dirextalkdomain.DomainFromMXID(userID),
		DisplayName: fallback(displayName, displayNameFromMXID(userID)),
		AvatarURL:   strings.TrimSpace(avatarURL),
		Membership:  strings.TrimSpace(membership),
		Role:        dirextalkdomain.NormalizeProductMemberRole(role),
		JoinedAt:    dirextalkmcp.FormatTime(joinedAt),
	}
}

func displayNameFromMXID(mxid string) string {
	return dirextalkdomain.DisplayNameFromMXID(mxid)
}

func localpartFromMXID(mxid string) string {
	localpart := strings.TrimPrefix(strings.TrimSpace(mxid), "@")
	if index := strings.Index(localpart, ":"); index >= 0 {
		localpart = localpart[:index]
	}
	return strings.TrimSpace(localpart)
}
