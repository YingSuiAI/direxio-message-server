package executionplanning

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"io/fs"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/artifactstore"
)

func TestStaticSourceArchiveAnalyzerReadsAllowlistedZIPManifests(t *testing.T) {
	digest := strings.Repeat("a", 64)
	archive := makeZIPArchive(t, map[string]string{
		"project/Dockerfile":       "FROM ghcr.io/example/api@sha256:" + digest + "\nEXPOSE 8080\nENV API_TOKEN\nHEALTHCHECK CMD curl -f http://127.0.0.1:8080/health\nCMD [\"/api\"]\n",
		"project/compose.yaml":     "services:\n  api:\n    image: ghcr.io/example/api@sha256:" + digest + "\n    ports: [\"9090:8080\"]\n    environment:\n      DB_PASSWORD: ignored-value\n    volumes: [\"data:/data\"]\n    healthcheck:\n      test: [\"CMD\", \"true\"]\n",
		"project/package.json":     `{"scripts":{"start":"node server.js","migrate":"node migrate.js"},"dependencies":{"express":"4.21.0"}}`,
		"project/pyproject.toml":   "[project]\ndependencies = [\"flask==3.0.0\"]\n[project.scripts]\nserve = \"app:main\"\n",
		"project/requirements.txt": "requests==2.32.0\n",
		"project/Cargo.toml":       "[package]\nname = \"worker\"\nversion = \"1.0.0\"\n[dependencies]\nserde = \"=1.0.210\"\n",
		"project/pubspec.yaml":     "name: mobile\nenvironment:\n  flutter: '>=3.0.0'\ndependencies:\n  flutter:\n    sdk: flutter\n  http: '=1.2.3'\n",
	})
	analyzer, _ := archiveAnalyzerForBytes(t, archive, "application/zip")
	facts, err := analyzer.AnalyzeSourceArchive(context.Background(), resolverOwner, resolverProjectID, resolverArtifact)
	if err != nil {
		t.Fatal(err)
	}
	for _, stack := range []string{"container", "docker-compose", "node", "python", "rust", "dart", "flutter"} {
		if !containsString(facts.Analysis.DetectedStacks, stack) {
			t.Fatalf("missing stack %q in %#v", stack, facts.Analysis.DetectedStacks)
		}
	}
	for _, port := range []int{8080} {
		if !containsInt(facts.Analysis.Ports, port) {
			t.Fatalf("missing port %d in %#v", port, facts.Analysis.Ports)
		}
	}
	for _, name := range []string{"API_TOKEN", "DB_PASSWORD"} {
		if !containsString(facts.Analysis.EnvironmentNames, name) {
			t.Fatalf("missing environment %q in %#v", name, facts.Analysis.EnvironmentNames)
		}
	}
	if len(facts.BlockingUncertainties) != 0 {
		t.Fatalf("valid pinned archive blockers = %#v", facts.BlockingUncertainties)
	}
	if len(facts.Analysis.SecretPurposes) != 2 || len(facts.Analysis.Probes) != 2 || len(facts.Analysis.Migrations) != 1 {
		t.Fatalf("incomplete facts: %+v", facts.Analysis)
	}
}

func TestStaticSourceArchiveAnalyzerSupportsTarAndTarGzip(t *testing.T) {
	tarBytes := makeTARArchive(t, []tarTestEntry{{name: "repo/go.mod", body: "module example.org/service\n\ngo 1.24\nrequire example.org/dependency v1.2.3\n", typeflag: tar.TypeReg}})
	tarAnalyzer, _ := archiveAnalyzerForBytes(t, tarBytes, "application/x-tar")
	tarFacts, err := tarAnalyzer.AnalyzeSourceArchive(context.Background(), resolverOwner, resolverProjectID, resolverArtifact)
	if err != nil || !containsString(tarFacts.Analysis.DetectedStacks, "go") || len(tarFacts.BlockingUncertainties) != 1 {
		t.Fatalf("tar facts=%+v err=%v", tarFacts, err)
	}

	gzipBytes := makeTARGzipArchive(t, []tarTestEntry{{name: "repo/package.json", body: `{"scripts":{"start":"node index.js"},"dependencies":{"fastify":"4.28.1"}}`, typeflag: tar.TypeReg}})
	gzipAnalyzer, _ := archiveAnalyzerForBytes(t, gzipBytes, "application/gzip")
	gzipFacts, err := gzipAnalyzer.AnalyzeSourceArchive(context.Background(), resolverOwner, resolverProjectID, resolverArtifact)
	if err != nil || !containsString(gzipFacts.Analysis.DetectedStacks, "node") || len(gzipFacts.BlockingUncertainties) != 0 {
		t.Fatalf("tar.gz facts=%+v err=%v", gzipFacts, err)
	}
}

func TestStaticSourceArchiveAnalyzerRejectsTraversalLinksBombsAndUnsafeYAML(t *testing.T) {
	zipBuffer := new(bytes.Buffer)
	zw := zip.NewWriter(zipBuffer)
	writeZIPEntry(t, zw, "../Dockerfile", "FROM scratch\n")
	symlink := &zip.FileHeader{Name: "repo/compose.yaml", Method: zip.Store}
	symlink.SetMode(fs.ModeSymlink | 0777)
	w, err := zw.CreateHeader(symlink)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(w, "../outside")
	writeZIPEntry(t, zw, "repo/pubspec.yaml", "defaults: &defaults\n  sdk: flutter\ndependencies:\n  flutter: *defaults\n")
	writeZIPEntry(t, zw, "repo/package.json", `{not-json}`)
	if err = zw.Close(); err != nil {
		t.Fatal(err)
	}
	analyzer, _ := archiveAnalyzerForBytes(t, zipBuffer.Bytes(), "application/zip")
	facts, err := analyzer.AnalyzeSourceArchive(context.Background(), resolverOwner, resolverProjectID, resolverArtifact)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"unsafe entry path", "link entries", "pubspec manifest is malformed", "package.json manifest is malformed"} {
		if !containsFragment(facts.BlockingUncertainties, fragment) {
			t.Fatalf("missing blocker %q in %#v", fragment, facts.BlockingUncertainties)
		}
	}

	tarTraversal := makeTARArchive(t, []tarTestEntry{
		{name: "../../go.mod", body: "module unsafe\n", typeflag: tar.TypeReg},
		{name: "repo/link", link: "../../outside", typeflag: tar.TypeSymlink},
	})
	tarAnalyzer, _ := archiveAnalyzerForBytes(t, tarTraversal, "application/x-tar")
	tarFacts, err := tarAnalyzer.AnalyzeSourceArchive(context.Background(), resolverOwner, resolverProjectID, resolverArtifact)
	if err != nil || !containsFragment(tarFacts.BlockingUncertainties, "unsafe entry path") || !containsFragment(tarFacts.BlockingUncertainties, "unsafe link target") {
		t.Fatalf("tar traversal facts=%+v err=%v", tarFacts, err)
	}

	oversized := makeTARHeaderOnlyGzip(t, maxStaticExpandedBytes+1)
	bombAnalyzer, _ := archiveAnalyzerForBytes(t, oversized, "application/gzip")
	bombFacts, err := bombAnalyzer.AnalyzeSourceArchive(context.Background(), resolverOwner, resolverProjectID, resolverArtifact)
	if err != nil || !containsFragment(bombFacts.BlockingUncertainties, "expansion limit") {
		t.Fatalf("tar.gz bomb facts=%+v err=%v", bombFacts, err)
	}
}

func TestStaticSourceArchiveAnalyzerBoundsManifestAndHonorsCancellation(t *testing.T) {
	archive := makeZIPArchive(t, map[string]string{"repo/package.json": strings.Repeat("x", int(maxStaticManifestBytes)+1)})
	analyzer, _ := archiveAnalyzerForBytes(t, archive, "application/zip")
	facts, err := analyzer.AnalyzeSourceArchive(context.Background(), resolverOwner, resolverProjectID, resolverArtifact)
	if err != nil || !containsFragment(facts.BlockingUncertainties, "per-file analysis limit") {
		t.Fatalf("oversized manifest facts=%+v err=%v", facts, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = analyzer.AnalyzeSourceArchive(ctx, resolverOwner, resolverProjectID, resolverArtifact); err == nil {
		t.Fatal("canceled analysis unexpectedly succeeded")
	}
}

func archiveAnalyzerForBytes(t *testing.T, data []byte, mediaType string) (*StaticSourceArchiveAnalyzer, *artifactMetadataStub) {
	t.Helper()
	content, err := artifactstore.New(t.TempDir(), artifactstore.MaxArtifactSize)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := content.Put(context.Background(), bytes.NewReader(data), artifactstore.PutOptions{MaxSize: int64(len(data))})
	if err != nil {
		t.Fatal(err)
	}
	metadata := &artifactMetadataStub{record: artifactstore.SourceArtifact{
		OwnerID: resolverOwner, ArtifactID: resolverArtifact, ProjectID: resolverProjectID,
		ContentDigest: stored.Digest, StorageBackend: "filesystem", StorageRef: stored.StorageRef,
		SizeBytes: stored.Size, MediaType: mediaType, Revision: 1, Status: "available",
		SchemaVersion: "execution-source-artifact/v2", CreatedAt: time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC),
	}}
	catalog := artifactstore.NewSourceCatalog(content, metadata)
	return NewStaticSourceArchiveAnalyzer(catalog), metadata
}

func makeZIPArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	buffer := new(bytes.Buffer)
	writer := zip.NewWriter(buffer)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		writeZIPEntry(t, writer, name, files[name])
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func writeZIPEntry(t *testing.T, writer *zip.Writer, name, body string) {
	t.Helper()
	w, err := writer.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(w, body); err != nil {
		t.Fatal(err)
	}
}

type tarTestEntry struct {
	name, body, link string
	typeflag         byte
}

func makeTARArchive(t *testing.T, entries []tarTestEntry) []byte {
	t.Helper()
	buffer := new(bytes.Buffer)
	writer := tar.NewWriter(buffer)
	for _, entry := range entries {
		size := int64(len(entry.body))
		if entry.typeflag == tar.TypeSymlink || entry.typeflag == tar.TypeLink {
			size = 0
		}
		header := &tar.Header{Name: entry.name, Typeflag: entry.typeflag, Mode: 0600, Size: size, Linkname: entry.link}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if size > 0 {
			if _, err := io.WriteString(writer, entry.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func makeTARGzipArchive(t *testing.T, entries []tarTestEntry) []byte {
	t.Helper()
	plain := makeTARArchive(t, entries)
	buffer := new(bytes.Buffer)
	writer := gzip.NewWriter(buffer)
	if _, err := writer.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func makeTARHeaderOnlyGzip(t *testing.T, size int64) []byte {
	t.Helper()
	plain := new(bytes.Buffer)
	writer := tar.NewWriter(plain)
	if err := writer.WriteHeader(&tar.Header{Name: "repo/package.json", Typeflag: tar.TypeReg, Mode: 0600, Size: size}); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close() // Deliberately truncated after the oversized header.
	buffer := new(bytes.Buffer)
	gz := gzip.NewWriter(buffer)
	_, _ = gz.Write(plain.Bytes())
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
func containsInt(values []int, want int) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
func containsFragment(values []string, fragment string) bool {
	for _, value := range values {
		if strings.Contains(value, fragment) {
			return true
		}
	}
	return false
}
