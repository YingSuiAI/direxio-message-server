package catalog

import (
	"context"
	"net/url"
	"strconv"
	"strings"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/extensions"
)

func (p *provider) searchSmithery(ctx context.Context, cat *Catalog, query string, pageSize int, cur cursor) ([]extensions.Candidate, string, error) {
	v := url.Values{}
	if query != "" {
		v.Set("q", query)
	}
	if pageSize > 0 {
		v.Set("pageSize", strconv.Itoa(pageSize))
	}
	if cur.Remote != "" {
		v.Set("cursor", cur.Remote)
	}
	b, err := p.c.get(ctx, "/servers", v)
	if err != nil {
		return nil, "", err
	}
	var root map[string]any
	if err := parseJSON(b, &root); err != nil {
		return nil, "", err
	}
	arr := rawSlice(root["servers"])
	if arr == nil {
		arr = rawSlice(root["items"])
	}
	if arr == nil {
		return nil, "", ErrMalformed
	}
	out := make([]extensions.Candidate, 0, len(arr))
	for _, value := range arr {
		candidate, remote, ok := smitheryCandidate(rawMap(value))
		if !ok || p.c.validateRemote(ctx, remote) != nil {
			continue
		}
		out = append(out, candidate)
	}
	next := rawString(root, "nextCursor", "next_cursor")
	if next == "" {
		next = rawString(rawMap(root["pagination"]), "nextCursor", "next_cursor")
	}
	return out, nextToken(cat, cur, query, pageSize, next, "smithery"), nil
}

func smitheryCandidate(m map[string]any) (extensions.Candidate, string, bool) {
	id := rawString(m, "qualifiedName", "name")
	name := rawString(m, "displayName", "name")
	version := rawString(m, "version", "latestVersion")
	if id == "" || name == "" || version == "" || strings.EqualFold(version, "latest") {
		return extensions.Candidate{}, "", false
	}
	typ := strings.ToLower(rawString(m, "type", "transport"))
	if strings.Contains(typ, "sse") || strings.Contains(typ, "template") {
		return extensions.Candidate{}, "", false
	}
	remote := rawString(m, "deploymentUrl", "deploymentURL")
	if remote == "" {
		remote = remoteFromRemotes(rawSlice(m["connections"]))
	}
	if remote == "" {
		return extensions.Candidate{}, "", false
	}
	c := extensions.Candidate{ID: id, Kind: extensions.KindMCP, Source: "smithery", Name: name, Description: rawString(m, "description"), Transport: extensions.TransportRemote, Pin: extensions.SourcePin{RegistryVersion: version, RegistrySHA256: digestJSON(m)}}
	return c, remote, c.Validate() == nil
}

func (p *provider) inspectSmithery(ctx context.Context, candidate extensions.Candidate) (extensions.Inspection, error) {
	id := strings.TrimSuffix(candidate.ID, "@"+candidate.Pin.RegistryVersion)
	b, err := p.c.get(ctx, "/servers/"+url.PathEscape(id), nil)
	if err != nil {
		return extensions.Inspection{}, err
	}
	var m map[string]any
	if err := parseJSON(b, &m); err != nil {
		return extensions.Inspection{}, err
	}
	c, remote, ok := smitheryCandidate(m)
	if !ok || c.ID != candidate.ID || c.Pin != candidate.Pin || p.c.validateRemote(ctx, remote) != nil {
		return extensions.Inspection{}, ErrUnsupported
	}
	return inspection(c, b, remote)
}
