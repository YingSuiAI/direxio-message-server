package executionplanning

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/artifactstore"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
)

const (
	maxStaticArchiveBytes     int64 = 32 << 20
	maxStaticExpandedBytes    int64 = 64 << 20
	maxStaticArchiveEntries         = 4096
	maxStaticManifestEntries        = 64
	maxStaticManifestBytes    int64 = 256 << 10
	maxStaticAllManifestBytes int64 = 1 << 20
	maxStaticArchivePathBytes       = 512
	maxStaticArchivePathDepth       = 16
)

var requirementsManifestName = regexp.MustCompile(`^requirements(?:[-_.][a-z0-9_-]+)?\.txt$`)

type sourceArchiveOpener interface {
	OpenSource(context.Context, string, string, string) (io.ReadCloser, artifactstore.SourceArtifact, error)
}

type sourceArchiveAnalyzer interface {
	AnalyzeSourceArchive(context.Context, string, string, string) (SourceFacts, error)
}

// StaticSourceArchiveAnalyzer reads only allowlisted manifest files from a
// verified content address. It never extracts an entry, follows an archive
// link, executes project code, or invokes a local process.
type StaticSourceArchiveAnalyzer struct{ opener sourceArchiveOpener }

func NewStaticSourceArchiveAnalyzer(catalog *artifactstore.SourceCatalog) *StaticSourceArchiveAnalyzer {
	if catalog == nil {
		return nil
	}
	return &StaticSourceArchiveAnalyzer{opener: catalog}
}

func (a *StaticSourceArchiveAnalyzer) AnalyzeSourceArchive(ctx context.Context, owner, projectID, artifactID string) (SourceFacts, error) {
	if a == nil || a.opener == nil {
		return SourceFacts{}, ErrSourceIntegrity
	}
	reader, metadata, err := a.opener.OpenSource(ctx, owner, projectID, artifactID)
	if err != nil {
		if errors.Is(err, coreexecution.ErrNotFound) {
			return SourceFacts{}, err
		}
		return SourceFacts{}, fmt.Errorf("%w: verified source open failed", ErrSourceIntegrity)
	}
	defer reader.Close()
	source := coreexecution.SourceRef{
		Kind: "uploaded_artifact", ArtifactID: metadata.ArtifactID,
		ArtifactDigest: coreexecution.Digest(metadata.ContentDigest), Immutable: true,
	}
	collector := newStaticAnalysisCollector(source)
	if metadata.SizeBytes > maxStaticArchiveBytes {
		collector.block("source archive exceeds the static analyzer compressed-size limit")
		return collector.facts(), nil
	}
	archiveBytes, err := readExactBounded(ctx, reader, metadata.SizeBytes, maxStaticArchiveBytes)
	if err != nil {
		return SourceFacts{}, fmt.Errorf("%w: source archive read failed", ErrSourceIntegrity)
	}
	manifests, blockers := scanStaticArchive(ctx, archiveBytes, metadata.MediaType)
	for _, blocker := range blockers {
		collector.block(blocker)
	}
	if len(manifests) == 0 {
		collector.block("source archive contains no supported project manifest")
	}
	for _, manifest := range manifests {
		if err := ctx.Err(); err != nil {
			return SourceFacts{}, err
		}
		collector.inspect(manifest)
	}
	return collector.facts(), nil
}

type staticManifest struct {
	kind string
	name string
	data []byte
}

func readExactBounded(ctx context.Context, r io.Reader, expected, limit int64) ([]byte, error) {
	if expected < 0 || expected > limit {
		return nil, ErrSourceIntegrity
	}
	limited := &io.LimitedReader{R: &contextArchiveReader{ctx: ctx, r: r}, N: limit + 1}
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != expected || int64(len(data)) > limit {
		return nil, ErrSourceIntegrity
	}
	return data, nil
}

type contextArchiveReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextArchiveReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func scanStaticArchive(ctx context.Context, data []byte, mediaType string) ([]staticManifest, []string) {
	format := detectArchiveFormat(data, mediaType)
	switch format {
	case "zip":
		return scanStaticZip(ctx, data)
	case "tar":
		return scanStaticTar(ctx, bytes.NewReader(data), int64(len(data)))
	case "tar.gz":
		gz, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, []string{"source archive gzip stream is malformed"}
		}
		gz.Multistream(false)
		decompressed := &io.LimitedReader{R: &contextArchiveReader{ctx: ctx, r: gz}, N: maxStaticExpandedBytes + 1}
		manifests, blockers := scanStaticTar(ctx, decompressed, maxStaticExpandedBytes)
		if decompressed.N <= 0 {
			blockers = append(blockers, "source archive exceeds the static analyzer expansion limit")
		}
		if err := gz.Close(); err != nil {
			blockers = append(blockers, "source archive gzip stream is malformed")
		}
		return manifests, stableStrings(blockers, 128)
	default:
		return nil, []string{"source archive format is unsupported or does not match its media type"}
	}
}

func detectArchiveFormat(data []byte, mediaType string) string {
	zipMagic := len(data) >= 4 && data[0] == 'P' && data[1] == 'K' &&
		((data[2] == 3 && data[3] == 4) || (data[2] == 5 && data[3] == 6) || (data[2] == 7 && data[3] == 8))
	gzipMagic := len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b
	tarMagic := len(data) >= 262 && (string(data[257:262]) == "ustar" || allZero(data[257:262]))
	switch mediaType {
	case "application/zip", "application/x-zip-compressed":
		if zipMagic {
			return "zip"
		}
	case "application/x-tar":
		if tarMagic {
			return "tar"
		}
	case "application/gzip", "application/x-gzip":
		if gzipMagic {
			return "tar.gz"
		}
	case "application/octet-stream":
		if zipMagic {
			return "zip"
		}
		if gzipMagic {
			return "tar.gz"
		}
		if tarMagic {
			return "tar"
		}
	}
	return ""
}

func allZero(value []byte) bool {
	for _, b := range value {
		if b != 0 {
			return false
		}
	}
	return true
}

func scanStaticZip(ctx context.Context, data []byte) ([]staticManifest, []string) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, []string{"source archive ZIP directory is malformed"}
	}
	if len(zr.File) > maxStaticArchiveEntries {
		return nil, []string{"source archive contains too many entries"}
	}
	var expanded uint64
	var manifestBytes int64
	seen := map[string]bool{}
	manifests := make([]staticManifest, 0)
	blockers := make([]string, 0)
	for _, file := range zr.File {
		if err := ctx.Err(); err != nil {
			return nil, append(blockers, "source archive analysis was canceled")
		}
		name, safe := safeArchiveName(file.Name)
		if !safe || file.NonUTF8 {
			blockers = append(blockers, "source archive contains an unsafe entry path")
			continue
		}
		if file.Mode()&fs.ModeSymlink != 0 {
			blockers = append(blockers, "source archive contains link entries that are not analyzed")
			continue
		}
		if file.FileInfo().IsDir() {
			continue
		}
		if !file.Mode().IsRegular() {
			blockers = append(blockers, "source archive contains unsupported special entries")
			continue
		}
		if file.UncompressedSize64 > uint64(maxStaticExpandedBytes) || expanded > uint64(maxStaticExpandedBytes)-file.UncompressedSize64 {
			blockers = append(blockers, "source archive exceeds the static analyzer expansion limit")
			break
		}
		expanded += file.UncompressedSize64
		kind := staticManifestKind(name)
		if kind == "" {
			continue
		}
		if seen[name] {
			blockers = append(blockers, "source archive contains duplicate supported manifest paths")
			continue
		}
		seen[name] = true
		if len(manifests) >= maxStaticManifestEntries {
			blockers = append(blockers, "source archive contains too many supported manifests")
			break
		}
		if file.UncompressedSize64 > uint64(maxStaticManifestBytes) {
			blockers = append(blockers, "a supported source manifest exceeds the per-file analysis limit")
			continue
		}
		r, openErr := file.Open()
		if openErr != nil {
			blockers = append(blockers, "a supported source manifest cannot be read")
			continue
		}
		content, readErr := io.ReadAll(io.LimitReader(r, maxStaticManifestBytes+1))
		closeErr := r.Close()
		if readErr != nil || closeErr != nil || int64(len(content)) > maxStaticManifestBytes || uint64(len(content)) != file.UncompressedSize64 {
			blockers = append(blockers, "a supported source manifest cannot be read within limits")
			continue
		}
		if manifestBytes+int64(len(content)) > maxStaticAllManifestBytes {
			blockers = append(blockers, "supported source manifests exceed the aggregate analysis limit")
			break
		}
		manifestBytes += int64(len(content))
		manifests = append(manifests, staticManifest{kind: kind, name: name, data: content})
	}
	sort.Slice(manifests, func(i, j int) bool { return manifests[i].name < manifests[j].name })
	return manifests, stableStrings(blockers, 128)
}

func scanStaticTar(ctx context.Context, r io.Reader, expansionLimit int64) ([]staticManifest, []string) {
	tr := tar.NewReader(r)
	var expanded int64
	var manifestBytes int64
	entries := 0
	seen := map[string]bool{}
	manifests := make([]staticManifest, 0)
	blockers := make([]string, 0)
	for {
		if err := ctx.Err(); err != nil {
			return nil, append(blockers, "source archive analysis was canceled")
		}
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			blockers = append(blockers, "source TAR archive is malformed or exceeds its expansion limit")
			break
		}
		entries++
		if entries > maxStaticArchiveEntries {
			blockers = append(blockers, "source archive contains too many entries")
			break
		}
		name, safe := safeArchiveName(header.Name)
		if !safe {
			blockers = append(blockers, "source archive contains an unsafe entry path")
			continue
		}
		if header.Typeflag == tar.TypeSymlink || header.Typeflag == tar.TypeLink {
			if _, linkSafe := safeArchiveName(header.Linkname); !linkSafe {
				blockers = append(blockers, "source archive contains an unsafe link target")
			} else {
				blockers = append(blockers, "source archive contains link entries that are not analyzed")
			}
			continue
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			blockers = append(blockers, "source archive contains unsupported special entries")
			continue
		}
		if header.Size < 0 || header.Size > expansionLimit || expanded > expansionLimit-header.Size {
			blockers = append(blockers, "source archive exceeds the static analyzer expansion limit")
			break
		}
		expanded += header.Size
		kind := staticManifestKind(name)
		if kind == "" {
			continue
		}
		if seen[name] {
			blockers = append(blockers, "source archive contains duplicate supported manifest paths")
			continue
		}
		seen[name] = true
		if len(manifests) >= maxStaticManifestEntries {
			blockers = append(blockers, "source archive contains too many supported manifests")
			break
		}
		if header.Size > maxStaticManifestBytes {
			blockers = append(blockers, "a supported source manifest exceeds the per-file analysis limit")
			continue
		}
		content, readErr := io.ReadAll(io.LimitReader(tr, maxStaticManifestBytes+1))
		if readErr != nil || int64(len(content)) != header.Size {
			blockers = append(blockers, "a supported source manifest cannot be read within limits")
			continue
		}
		if manifestBytes+int64(len(content)) > maxStaticAllManifestBytes {
			blockers = append(blockers, "supported source manifests exceed the aggregate analysis limit")
			break
		}
		manifestBytes += int64(len(content))
		manifests = append(manifests, staticManifest{kind: kind, name: name, data: content})
	}
	sort.Slice(manifests, func(i, j int) bool { return manifests[i].name < manifests[j].name })
	return manifests, stableStrings(blockers, 128)
}

func safeArchiveName(raw string) (string, bool) {
	if raw == "" || len(raw) > maxStaticArchivePathBytes || !utf8.ValidString(raw) ||
		strings.ContainsAny(raw, "\\\r\n\x00") || strings.HasPrefix(raw, "/") {
		return "", false
	}
	cleaned := path.Clean(strings.TrimPrefix(raw, "./"))
	if cleaned == "." {
		return cleaned, true
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, ":") ||
		len(strings.Split(cleaned, "/")) > maxStaticArchivePathDepth {
		return "", false
	}
	return cleaned, true
}

func staticManifestKind(name string) string {
	base := path.Base(name)
	switch base {
	case "Dockerfile":
		return "dockerfile"
	case "compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml":
		return "compose"
	case "go.mod":
		return "go.mod"
	case "package.json":
		return "package.json"
	case "pyproject.toml":
		return "pyproject.toml"
	case "Cargo.toml":
		return "Cargo.toml"
	case "pubspec.yaml", "pubspec.yml":
		return "pubspec"
	}
	if requirementsManifestName.MatchString(strings.ToLower(base)) {
		return "requirements"
	}
	return ""
}
