package storage

import (
	"context"
	"sort"
	"strings"
)

func (s *MemoryStore) InsertChannelPost(ctx context.Context, post channelPostRecord) error {
	post = normalizeChannelPostRecord(post)
	s.mu.Lock()
	for i := range s.posts {
		if s.posts[i].PostID == post.PostID {
			s.posts[i] = post
			s.mu.Unlock()
			return nil
		}
	}
	s.posts = append(s.posts, post)
	s.mu.Unlock()
	return nil
}

func (s *MemoryStore) GetChannelPostByID(ctx context.Context, postID, channelID string) (channelPostRecord, bool, error) {
	if postID == "" {
		return channelPostRecord{}, false, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, post := range s.posts {
		if post.PostID == postID && (channelID == "" || post.ChannelID == channelID) {
			return post, true, nil
		}
	}
	return channelPostRecord{}, false, nil
}

func (s *MemoryStore) GetChannelPostByEventID(ctx context.Context, eventID, channelID string) (channelPostRecord, bool, error) {
	if eventID == "" {
		return channelPostRecord{}, false, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, post := range s.posts {
		if post.EventID == eventID && (channelID == "" || post.ChannelID == channelID) {
			return post, true, nil
		}
	}
	return channelPostRecord{}, false, nil
}

func (s *MemoryStore) ListChannelPosts(ctx context.Context, channelID string) ([]channelPostRecord, error) {
	s.mu.RLock()
	posts := make([]channelPostRecord, 0, len(s.posts))
	for _, post := range s.posts {
		if channelID == "" || post.ChannelID == channelID {
			posts = append(posts, post)
		}
	}
	s.mu.RUnlock()
	return posts, nil
}

func (s *MemoryStore) ListChannelPostsPage(ctx context.Context, channelID string, fromTS, snapshotTS, cursorTS int64, cursorID string, limit int) ([]channelPostRecord, bool, error) {
	s.mu.RLock()
	posts := append([]channelPostRecord(nil), s.posts...)
	s.mu.RUnlock()
	posts, hasMore := memoryKeysetPage(posts, fromTS, snapshotTS, cursorTS, cursorID, limit,
		func(post channelPostRecord) bool { return channelID == "" || post.ChannelID == channelID },
		func(post channelPostRecord) (int64, string) { return post.OriginServerTS, post.PostID },
	)
	return posts, hasMore, nil
}

func (s *MemoryStore) ListChannelPostsByVisibilityPage(ctx context.Context, channelID, visibility string, offset int64, limit int) ([]channelPostRecord, bool, error) {
	visibility = normalizeChannelPostRecord(channelPostRecord{Visibility: visibility}).Visibility
	s.mu.RLock()
	posts := make([]channelPostRecord, 0, len(s.posts))
	for _, post := range s.posts {
		if post.ChannelID == channelID && normalizeChannelPostRecord(post).Visibility == visibility {
			posts = append(posts, post)
		}
	}
	s.mu.RUnlock()
	sort.Slice(posts, func(i, j int) bool {
		if posts[i].OriginServerTS != posts[j].OriginServerTS {
			return posts[i].OriginServerTS > posts[j].OriginServerTS
		}
		return posts[i].PostID > posts[j].PostID
	})
	if offset >= int64(len(posts)) {
		return []channelPostRecord{}, false, nil
	}
	posts = posts[offset:]
	hasMore := len(posts) > limit
	if hasMore {
		posts = posts[:limit]
	}
	return posts, hasMore, nil
}

func (s *MemoryStore) InsertChannelComment(ctx context.Context, comment channelCommentRecord) error {
	s.mu.Lock()
	for i := range s.comments {
		if s.comments[i].CommentID == comment.CommentID {
			s.comments[i] = comment
			s.mu.Unlock()
			return nil
		}
	}
	s.comments = append(s.comments, comment)
	s.mu.Unlock()
	return nil
}

func (s *MemoryStore) GetChannelCommentByID(ctx context.Context, commentID, postID string) (channelCommentRecord, bool, error) {
	if commentID == "" {
		return channelCommentRecord{}, false, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, comment := range s.comments {
		if comment.CommentID == commentID && (postID == "" || comment.PostID == postID) {
			return comment, true, nil
		}
	}
	return channelCommentRecord{}, false, nil
}

func (s *MemoryStore) GetChannelCommentByEventID(ctx context.Context, eventID, channelID string) (channelCommentRecord, bool, error) {
	if eventID == "" {
		return channelCommentRecord{}, false, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, comment := range s.comments {
		if comment.EventID == eventID && (channelID == "" || comment.ChannelID == channelID) {
			return comment, true, nil
		}
	}
	return channelCommentRecord{}, false, nil
}

func (s *MemoryStore) ListChannelComments(ctx context.Context, postID string) ([]channelCommentRecord, error) {
	s.mu.RLock()
	comments := make([]channelCommentRecord, 0, len(s.comments))
	for _, comment := range s.comments {
		if postID == "" || comment.PostID == postID {
			comments = append(comments, comment)
		}
	}
	s.mu.RUnlock()
	return comments, nil
}

func (s *MemoryStore) ListChannelCommentsPage(ctx context.Context, postID string, fromTS, snapshotTS, cursorTS int64, cursorID string, limit int) ([]channelCommentRecord, bool, error) {
	s.mu.RLock()
	comments := append([]channelCommentRecord(nil), s.comments...)
	s.mu.RUnlock()
	comments, hasMore := memoryKeysetPage(comments, fromTS, snapshotTS, cursorTS, cursorID, limit,
		func(comment channelCommentRecord) bool { return postID == "" || comment.PostID == postID },
		func(comment channelCommentRecord) (int64, string) { return comment.OriginServerTS, comment.CommentID },
	)
	return comments, hasMore, nil
}

func memoryKeysetPage[T any](records []T, fromTS, snapshotTS, cursorTS int64, cursorID string, limit int, matches func(T) bool, key func(T) (int64, string)) ([]T, bool) {
	filtered := make([]T, 0, len(records))
	for _, record := range records {
		ts, id := key(record)
		if !matches(record) || (fromTS > 0 && ts < fromTS) || (snapshotTS > 0 && ts > snapshotTS) {
			continue
		}
		if cursorTS > 0 && !(ts < cursorTS || (ts == cursorTS && strings.TrimSpace(id) < strings.TrimSpace(cursorID))) {
			continue
		}
		filtered = append(filtered, record)
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		leftTS, leftID := key(filtered[i])
		rightTS, rightID := key(filtered[j])
		if leftTS == rightTS {
			return leftID > rightID
		}
		return leftTS > rightTS
	})
	hasMore := limit >= 0 && len(filtered) > limit
	if hasMore {
		filtered = filtered[:limit]
	}
	return filtered, hasMore
}

func (s *MemoryStore) DeleteChannelPost(ctx context.Context, postID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := false
	posts := s.posts[:0]
	for _, post := range s.posts {
		if post.PostID == postID || post.EventID == postID {
			removed = true
			continue
		}
		posts = append(posts, post)
	}
	s.posts = posts
	return removed, nil
}

func (s *MemoryStore) DeleteChannelComment(ctx context.Context, commentID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := false
	comments := s.comments[:0]
	for _, comment := range s.comments {
		if comment.CommentID == commentID || comment.EventID == commentID {
			removed = true
			continue
		}
		comments = append(comments, comment)
	}
	s.comments = comments
	return removed, nil
}
