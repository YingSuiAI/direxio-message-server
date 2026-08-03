package catalog

import (
	"context"
	"strings"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/extensions"
)

// Search returns only immutable, HTTPS Streamable HTTP MCP candidates.  The
// source and query are bound into the signed page token.
func (c *Catalog) Search(ctx context.Context, source, query string, pageSize int, pageToken string) ([]extensions.Candidate, string, error) {
	if c == nil {
		return nil, "", extensions.ErrUnavailable
	}
	source = strings.TrimSpace(source)
	query = strings.TrimSpace(query)
	if pageSize < 0 || pageSize > MaxPageSize {
		return nil, "", extensions.ErrInvalid
	}
	p, ok := c.providers[source]
	if !ok {
		if source == "" {
			return c.searchAll(ctx, query, pageSize, pageToken)
		}
		return nil, "", extensions.ErrInvalid
	}
	cur, err := c.decodeCursor(pageToken, source, query, pageSize)
	if err != nil {
		return nil, "", err
	}
	return p.search(ctx, c, query, pageSize, cur)
}

var aggregateSources = []string{SourceOfficialRegistry, SourceSmithery, SourceGlama, SourceGitHub}

func (c *Catalog) searchAll(ctx context.Context, query string, pageSize int, pageToken string) ([]extensions.Candidate, string, error) {
	cur := cursor{Source: "", Query: query, Size: pageSize, Tokens: map[string]string{}, Done: map[string]bool{}}
	if pageToken != "" {
		var err error
		cur, err = c.decodeCursor(pageToken, "", query, pageSize)
		if err != nil {
			return nil, "", err
		}
		if cur.Tokens == nil {
			cur.Tokens = map[string]string{}
		}
		if cur.Done == nil {
			cur.Done = map[string]bool{}
		}
	}
	if pageSize == 0 {
		pageSize = 20
	}
	base, remainder := pageSize/len(aggregateSources), pageSize%len(aggregateSources)
	seen := map[string]bool{}
	out := make([]extensions.Candidate, 0, pageSize)
	consume := func(index, quota int) error {
		source := aggregateSources[index]
		if cur.Done[source] {
			return nil
		}
		if quota == 0 {
			return nil
		}
		candidates, next, err := c.Search(ctx, source, query, quota, cur.Tokens[source])
		if err != nil {
			return err
		}
		for _, candidate := range candidates {
			key := candidate.Source + "\x00" + candidate.ID
			if !seen[key] {
				seen[key] = true
				out = append(out, candidate)
			}
		}
		if next == "" {
			cur.Done[source] = true
			delete(cur.Tokens, source)
		} else {
			cur.Tokens[source] = next
		}
		return nil
	}
	if pageSize < len(aggregateSources) {
		start := cur.Offset % len(aggregateSources)
		for step := 0; step < len(aggregateSources); step++ {
			index := (start + step) % len(aggregateSources)
			if cur.Done[aggregateSources[index]] {
				continue
			}
			if err := consume(index, pageSize); err != nil {
				return nil, "", err
			}
			cur.Offset = index + 1
			break
		}
	} else {
		for index := range aggregateSources {
			quota := base
			if index < remainder {
				quota++
			}
			if err := consume(index, quota); err != nil {
				return nil, "", err
			}
		}
	}
	for _, source := range aggregateSources {
		if !cur.Done[source] {
			return out, c.encodeCursor(cur), nil
		}
	}
	return out, "", nil
}

// Inspect resolves a candidate at its exact immutable pin.  Local, stdio,
// SSE, and non-HTTPS candidates never reach a source request.
func (c *Catalog) Inspect(ctx context.Context, candidate extensions.Candidate) (extensions.Inspection, error) {
	if c == nil {
		return extensions.Inspection{}, extensions.ErrUnavailable
	}
	if candidate.Kind != extensions.KindMCP || candidate.Transport != extensions.TransportRemote || candidate.Validate() != nil {
		return extensions.Inspection{}, extensions.ErrInvalid
	}
	p, ok := c.providers[candidate.Source]
	if !ok {
		return extensions.Inspection{}, extensions.ErrInvalid
	}
	return p.inspect(ctx, candidate)
}
