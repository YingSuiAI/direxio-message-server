package catalog

import (
	"context"
	"net/url"
	"strconv"
	"strings"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/extensions"
)

func (p *provider) searchGlama(ctx context.Context, cat *Catalog, query string, pageSize int, cur cursor) ([]extensions.Candidate, string, error) {
	requestedSize := pageSize
	if pageSize <= 0 {
		pageSize = 10
	}
	v := url.Values{"first": []string{strconv.Itoa(pageSize)}}
	if query != "" {
		v.Set("query", query)
	}
	if cur.Remote != "" {
		v.Set("after", cur.Remote)
	}
	b, err := p.c.get(ctx, "/api/mcp/v1/servers", v)
	if err != nil {
		return nil, "", err
	}
	var root map[string]any
	if err := parseJSON(b, &root); err != nil {
		return nil, "", err
	}
	container := root
	if nested := rawMap(root["data"]); nested != nil {
		container = nested
	}
	servers := rawMap(container["servers"])
	arr := rawSlice(container["servers"])
	if arr == nil && servers != nil {
		arr = rawSlice(servers["nodes"])
	}
	if arr == nil {
		return nil, "", ErrMalformed
	}
	out := make([]extensions.Candidate, 0, len(arr))
	for _, value := range arr {
		candidate, remote, ok := glamaCandidate(rawMap(value))
		if !ok || p.c.validateRemote(ctx, remote) != nil {
			continue
		}
		out = append(out, candidate)
	}
	pageInfo := rawMap(servers["pageInfo"])
	if pageInfo == nil {
		pageInfo = rawMap(container["pageInfo"])
	}
	next := rawString(pageInfo, "endCursor")
	if next == "" && pageInfo["hasNextPage"] == true {
		next = "has-next"
	}
	return out, nextToken(cat, cur, query, requestedSize, next, "glama"), nil
}

func glamaCandidate(m map[string]any) (extensions.Candidate, string, bool) {
	owner := rawString(m, "owner", "namespace", "organization")
	name := rawString(m, "name", "slug")
	if owner == "" || name == "" {
		if id := rawString(m, "id"); strings.Contains(id, "/") {
			parts := strings.SplitN(id, "/", 2)
			owner, name = parts[0], parts[1]
		}
	}
	version := rawString(m, "version", "latestVersion")
	if owner == "" || name == "" || version == "" || strings.EqualFold(version, "latest") {
		return extensions.Candidate{}, "", false
	}
	typ := strings.ToLower(rawString(m, "type", "transport"))
	if strings.Contains(typ, "sse") || strings.Contains(typ, "template") {
		return extensions.Candidate{}, "", false
	}
	remote := rawString(m, "url", "endpoint", "serverUrl", "deploymentUrl")
	if remote == "" {
		remote = remoteFromRemotes(rawSlice(m["remotes"]))
	}
	if remote == "" {
		return extensions.Candidate{}, "", false
	}
	id := owner + "/" + name
	c := extensions.Candidate{ID: id, Kind: extensions.KindMCP, Source: "glama", Name: id, Description: rawString(m, "description", "summary"), Transport: extensions.TransportRemote, Pin: extensions.SourcePin{RegistryVersion: version, RegistrySHA256: digestJSON(m)}}
	return c, remote, c.Validate() == nil
}

func (p *provider) inspectGlama(ctx context.Context, candidate extensions.Candidate) (extensions.Inspection, error) {
	parts := strings.SplitN(candidate.ID, "/", 2)
	if len(parts) != 2 {
		return extensions.Inspection{}, extensions.ErrInvalid
	}
	b, err := p.c.get(ctx, "/api/mcp/v1/servers/"+url.PathEscape(parts[0])+"/"+url.PathEscape(parts[1]), nil)
	if err != nil {
		return extensions.Inspection{}, err
	}
	var root map[string]any
	if err := parseJSON(b, &root); err != nil {
		return extensions.Inspection{}, err
	}
	m := root
	if nested := rawMap(root["data"]); nested != nil {
		m = nested
	}
	c, remote, ok := glamaCandidate(m)
	if !ok || c.ID != candidate.ID || c.Pin != candidate.Pin || p.c.validateRemote(ctx, remote) != nil {
		return extensions.Inspection{}, ErrUnsupported
	}
	return inspection(c, b, remote)
}
