package catalog

import (
	"context"
	"net/url"
	"strconv"
	"strings"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/extensions"
)

func (p *provider) search(ctx context.Context, cat *Catalog, query string, pageSize int, cur cursor) ([]extensions.Candidate, string, error) {
	switch p.source {
	case "official_registry":
		return p.searchOfficial(ctx, cat, query, pageSize, cur)
	case "smithery":
		return p.searchSmithery(ctx, cat, query, pageSize, cur)
	case "glama":
		return p.searchGlama(ctx, cat, query, pageSize, cur)
	case "github":
		return p.searchGitHub(ctx, cat, query, pageSize, cur)
	default:
		return nil, "", extensions.ErrInvalid
	}
}
func (p *provider) inspect(ctx context.Context, candidate extensions.Candidate) (extensions.Inspection, error) {
	switch p.source {
	case "official_registry":
		return p.inspectOfficial(ctx, candidate)
	case "smithery":
		return p.inspectSmithery(ctx, candidate)
	case "glama":
		return p.inspectGlama(ctx, candidate)
	case "github":
		return p.inspectGitHub(ctx, candidate)
	default:
		return extensions.Inspection{}, extensions.ErrInvalid
	}
}

func (p *provider) searchOfficial(ctx context.Context, cat *Catalog, query string, pageSize int, cur cursor) ([]extensions.Candidate, string, error) {
	v := url.Values{}
	if query != "" {
		v.Set("search", query)
	}
	if pageSize > 0 {
		v.Set("limit", itoa(pageSize))
	}
	if cur.Remote != "" {
		v.Set("cursor", cur.Remote)
	} else if cur.Offset > 0 {
		v.Set("cursor", itoa(cur.Offset))
	}
	b, err := p.c.get(ctx, "/v0.1/servers", v)
	if err != nil {
		return nil, "", err
	}
	var root map[string]any
	if err := parseJSON(b, &root); err != nil {
		return nil, "", err
	}
	arr := rawSlice(root["servers"])
	if arr == nil {
		arr = rawSlice(root["data"])
	}
	if arr == nil {
		return nil, "", ErrMalformed
	}
	out := make([]extensions.Candidate, 0, len(arr))
	for _, value := range arr {
		m := rawMap(value)
		if nested := rawMap(m["server"]); nested != nil {
			m = nested
		}
		candidate, remote, ok := officialCandidate(m)
		if !ok || p.c.validateRemote(ctx, remote) != nil {
			continue
		}
		out = append(out, candidate)
	}
	next := rawString(root, "nextCursor", "next_cursor")
	if next == "" {
		next = rawString(rawMap(root["pagination"]), "nextCursor", "next_cursor")
	}
	return out, nextToken(cat, cur, query, pageSize, next, "official_registry"), nil
}

func officialCandidate(m map[string]any) (extensions.Candidate, string, bool) {
	name := rawString(m, "name", "qualifiedName")
	version := rawString(m, "version", "latestVersion")
	if version == "" {
		if nested := rawMap(m["version"]); nested != nil {
			version = rawString(nested, "version", "name")
		}
	}
	if name == "" || version == "" || strings.EqualFold(version, "latest") {
		return extensions.Candidate{}, "", false
	}
	remote := remoteFromRemotes(rawSlice(m["remotes"]))
	if remote == "" {
		return extensions.Candidate{}, "", false
	}
	c := extensions.Candidate{ID: name + "@" + version, Kind: extensions.KindMCP, Source: "official_registry", Name: name, Description: rawString(m, "description", "displayName"), Transport: extensions.TransportRemote, Pin: extensions.SourcePin{RegistryVersion: version, RegistrySHA256: digestJSON(m)}}
	return c, remote, c.Validate() == nil
}

func (p *provider) inspectOfficial(ctx context.Context, candidate extensions.Candidate) (extensions.Inspection, error) {
	name := candidate.Name
	b, err := p.c.get(ctx, "/v0.1/servers/"+url.PathEscape(name)+"/versions/"+url.PathEscape(candidate.Pin.RegistryVersion), nil)
	if err != nil {
		return extensions.Inspection{}, err
	}
	var m map[string]any
	if err := parseJSON(b, &m); err != nil {
		return extensions.Inspection{}, err
	}
	if nested := rawMap(m["server"]); nested != nil {
		m = nested
	}
	c, remote, ok := officialCandidate(m)
	if !ok || c.ID != candidate.ID || c.Pin != candidate.Pin || p.c.validateRemote(ctx, remote) != nil {
		return extensions.Inspection{}, ErrUnsupported
	}
	return inspection(c, b, remote)
}

func remoteFromRemotes(items []any) string {
	for _, value := range items {
		m := rawMap(value)
		typ := strings.ToLower(rawString(m, "type", "transport"))
		if strings.Contains(typ, "sse") || strings.Contains(typ, "template") || (!strings.Contains(typ, "streamable") && !strings.Contains(typ, "http")) {
			continue
		}
		if raw := rawString(m, "url", "endpoint"); raw != "" {
			if u, err := remoteURL(raw); err == nil {
				return u.String()
			}
		}
	}
	return ""
}

func nextToken(cat *Catalog, cur cursor, query string, size int, remote, source string) string {
	if remote == "" || len(remote) > 2048 {
		return ""
	}
	return cat.encodeCursor(cursor{Source: source, Query: query, Size: size, Offset: cur.Offset, Remote: remote})
}
func itoa(v int) string {
	if v <= 0 {
		return ""
	}
	return strconv.Itoa(v)
}
