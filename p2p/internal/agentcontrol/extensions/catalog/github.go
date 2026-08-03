package catalog

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"sort"
	"strings"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/extensions"
)

type githubTreeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
	URL  string `json:"url"`
	Size int64  `json:"size"`
}
type githubTree struct {
	SHA       string            `json:"sha"`
	URL       string            `json:"url"`
	Size      int64             `json:"size"`
	Tree      []githubTreeEntry `json:"tree"`
	Truncated bool              `json:"truncated"`
}

const maxGitHubBlobs = 256

func (p *provider) searchGitHub(ctx context.Context, cat *Catalog, query string, pageSize int, cur cursor) ([]extensions.Candidate, string, error) {
	requestedSize := pageSize
	if pageSize <= 0 {
		pageSize = 30
	}
	v := url.Values{"q": []string{query}, "per_page": []string{itoa(pageSize)}}
	if cur.Offset > 0 {
		v.Set("page", itoa(cur.Offset/pageSize+1))
	}
	b, err := p.c.get(ctx, "/search/repositories", v)
	if err != nil {
		return nil, "", err
	}
	var root struct {
		Items []struct {
			FullName      string `json:"full_name"`
			Description   string `json:"description"`
			DefaultBranch string `json:"default_branch"`
		} `json:"items"`
	}
	if err := parseJSON(b, &root); err != nil {
		return nil, "", err
	}
	out := make([]extensions.Candidate, 0, len(root.Items))
	for _, repo := range root.Items {
		if !validRepo(repo.FullName) {
			return nil, "", ErrMalformed
		}
		branch := repo.DefaultBranch
		if branch == "" {
			branch = "HEAD"
		}
		commitBody, err := p.c.get(ctx, "/repos/"+repo.FullName+"/commits/"+url.PathEscape(branch), nil)
		if err != nil {
			return nil, "", err
		}
		var commit struct {
			SHA string `json:"sha"`
		}
		if parseJSON(commitBody, &commit) != nil || !validCommit(commit.SHA) {
			return nil, "", ErrMalformed
		}
		result, err := p.inspectRepo(ctx, repo.FullName, strings.ToLower(commit.SHA), repo.Description)
		if err != nil {
			// GitHub search contains many stdio-only repositories.  They are
			// filtered at the catalog boundary and never become candidates.
			if err == ErrUnsupported {
				continue
			}
			return nil, "", err
		}
		out = append(out, result.Candidate)
	}
	next := ""
	if len(root.Items) == pageSize {
		next = "page"
	}
	return out, nextToken(cat, cur, query, requestedSize, next, "github"), nil
}

func validRepo(repo string) bool {
	return strings.Count(repo, "/") == 1 && !strings.ContainsAny(repo, " \t\r\n") && !strings.Contains(repo, "..")
}

func (p *provider) inspectGitHub(ctx context.Context, candidate extensions.Candidate) (extensions.Inspection, error) {
	if !validRepo(candidate.ID) || candidate.Pin.GitCommit == "" || !validCommit(candidate.Pin.GitCommit) {
		return extensions.Inspection{}, extensions.ErrInvalid
	}
	result, err := p.inspectRepo(ctx, candidate.ID, strings.ToLower(candidate.Pin.GitCommit), candidate.Description)
	if err != nil {
		return extensions.Inspection{}, err
	}
	if result.Candidate.ID != candidate.ID || result.Candidate.Pin != candidate.Pin {
		return extensions.Inspection{}, ErrUnsupported
	}
	return inspection(result.Candidate, result.Manifest, result.Remote)
}

// inspectRepo gets the immutable commit tree and all blobs needed to produce
// a stable content digest.  Symlinks/submodules and oversized trees are not
// executable artifacts and are rejected.
type repoResult struct {
	Candidate extensions.Candidate
	Remote    string
	Manifest  []byte
}

func (p *provider) inspectRepo(ctx context.Context, repo, commit, description string) (repoResult, error) {
	b, err := p.c.get(ctx, "/repos/"+repo+"/git/trees/"+commit, url.Values{"recursive": []string{"1"}})
	if err != nil {
		return repoResult{}, err
	}
	var tree githubTree
	if decodeStrict(b, &tree) != nil || tree.Truncated || tree.SHA == "" || len(tree.Tree) > 10000 {
		return repoResult{}, ErrMalformed
	}
	entries := make([]githubTreeEntry, 0, len(tree.Tree))
	for _, entry := range tree.Tree {
		if entry.Type == "tree" {
			continue
		}
		if entry.Type != "blob" || entry.Mode == "120000" || entry.Mode == "160000" || !validCommit(entry.SHA) || entry.Path == "" || strings.Contains(entry.Path, "..") {
			return repoResult{}, ErrUnsupported
		}
		if entry.Size < 0 || entry.Size > p.c.max {
			return repoResult{}, ErrOversize
		}
		entries = append(entries, entry)
		if len(entries) > maxGitHubBlobs {
			return repoResult{}, ErrOversize
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	files := make([]repoFile, 0, len(entries))
	var declaredContent int64
	for _, entry := range entries {
		if declaredContent > p.c.max-entry.Size {
			return repoResult{}, ErrOversize
		}
		declaredContent += entry.Size
		if manifestSize(files, entry.Path, entry.Size) > p.c.max-declaredContent {
			return repoResult{}, ErrOversize
		}
		data, err := p.fetchBlob(ctx, repo, commit, entry)
		if err != nil {
			return repoResult{}, err
		}
		if int64(len(data)) > p.c.max {
			return repoResult{}, ErrOversize
		}
		if entry.Size > 0 && int64(len(data)) != entry.Size {
			return repoResult{}, ErrMalformed
		}
		files = append(files, repoFile{Path: entry.Path, Content: data})
		if content := decodedContentSize(files); content > p.c.max || manifestSize(files, "", -1) > p.c.max-content {
			return repoResult{}, ErrOversize
		}
	}
	var remote string
	for _, file := range files {
		lower := strings.ToLower(file.Path)
		if !strings.HasSuffix(lower, "mcp.json") && !strings.HasSuffix(lower, "manifest.json") {
			continue
		}
		var manifest map[string]any
		if parseJSON(file.Content, &manifest) != nil {
			continue
		}
		typ := strings.ToLower(rawString(manifest, "transport", "type"))
		if strings.Contains(typ, "sse") || strings.Contains(typ, "template") {
			return repoResult{}, ErrUnsupported
		}
		remote = rawString(manifest, "url", "endpoint", "deploymentUrl")
		if remote != "" {
			break
		}
	}
	if remote == "" || p.c.validateRemote(ctx, remote) != nil {
		return repoResult{}, ErrUnsupported
	}
	manifestPayload, err := json.Marshal(files)
	if err != nil {
		return repoResult{}, ErrMalformed
	}
	if total := decodedContentSize(files); total > p.c.max || int64(len(manifestPayload)) > p.c.max-total {
		return repoResult{}, ErrOversize
	}
	contentDigest := extensions.DigestBytes(manifestPayload)
	pin := extensions.SourcePin{GitCommit: strings.ToLower(commit), GitSHA256: contentDigest}
	candidate := extensions.Candidate{ID: repo, Kind: extensions.KindMCP, Source: "github", Name: repo, Description: description, Transport: extensions.TransportRemote, Pin: pin}
	return repoResult{Candidate: candidate, Remote: remote, Manifest: manifestPayload}, nil
}

type repoFile struct {
	Path    string
	Content []byte
}

// manifestSize computes the exact JSON size produced by json.Marshal for the
// repository manifest without allocating a placeholder content blob. A
// negative size asks for the size of the supplied files using their actual
// decoded content.
func manifestSize(files []repoFile, nextPath string, nextSize int64) int64 {
	size := int64(2) // []
	for index, file := range files {
		if index > 0 {
			size++
		}
		pathBytes, _ := json.Marshal(file.Path)
		contentSize := int64(base64.StdEncoding.EncodedLen(len(file.Content)))
		size += int64(len(`{"Path":`)) + int64(len(pathBytes)) + int64(len(`,"Content":"`)) + contentSize + int64(len(`"}`))
	}
	if nextSize >= 0 {
		if len(files) > 0 {
			size++
		}
		pathBytes, _ := json.Marshal(nextPath)
		size += int64(len(`{"Path":`)) + int64(len(pathBytes)) + int64(len(`,"Content":"`)) + int64(base64.StdEncoding.EncodedLen(int(nextSize))) + int64(len(`"}`))
	}
	return size
}

func decodedContentSize(files []repoFile) int64 {
	var total int64
	for _, file := range files {
		if int64(len(file.Content)) > (1<<63-1)-total {
			return 1<<63 - 1
		}
		total += int64(len(file.Content))
	}
	return total
}

func (p *provider) fetchBlob(ctx context.Context, repo, commit string, entry githubTreeEntry) ([]byte, error) {
	b, err := p.c.get(ctx, "/repos/"+repo+"/git/blobs/"+entry.SHA, nil)
	if errors.Is(err, ErrOversize) {
		return nil, ErrOversize
	}
	if err == nil {
		var blob struct {
			Content  string `json:"content"`
			Encoding string `json:"encoding"`
		}
		if parseJSON(b, &blob) == nil && blob.Encoding == "base64" {
			data, decodeErr := decodeBlobContent(blob.Content, p.c.max)
			if errors.Is(decodeErr, ErrOversize) {
				return nil, ErrOversize
			}
			if decodeErr == nil && fullBlobSHA(data) == strings.ToLower(entry.SHA) {
				return data, nil
			}
		}
	}
	// GitHub occasionally refuses the Git Blob endpoint for large/restricted
	// repositories; the pinned contents endpoint is an equivalent read-only
	// fallback and is still verified against the tree SHA.
	b, err = p.c.get(ctx, "/repos/"+repo+"/contents/"+url.PathEscape(entry.Path), url.Values{"ref": []string{strings.ToLower(commit)}})
	if err != nil {
		return nil, err
	}
	var content struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if parseJSON(b, &content) != nil || content.Encoding != "base64" {
		return nil, ErrMalformed
	}
	data, err := decodeBlobContent(content.Content, p.c.max)
	if errors.Is(err, ErrOversize) {
		return nil, ErrOversize
	}
	if err != nil || fullBlobSHA(data) != strings.ToLower(entry.SHA) {
		return nil, ErrMalformed
	}
	return data, nil
}

func decodeBlobContent(raw string, max int64) ([]byte, error) {
	encoded := strings.NewReplacer("\n", "", "\r", "").Replace(raw)
	if int64(base64.StdEncoding.DecodedLen(len(encoded))) > max {
		return nil, ErrOversize
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, ErrOversize
	}
	return data, nil
}

func decodeStrict(b []byte, out any) error {
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return ErrMalformed
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF {
		return ErrMalformed
	}
	return nil
}
