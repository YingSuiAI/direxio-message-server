package artifactstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestPutOpenRoundTrip(t *testing.T) {
	s := newTestStore(t)
	data := []byte("hello artifact")
	digest := digestOf(data)
	meta, err := s.Put(context.Background(), strings.NewReader(string(data)), PutOptions{MaxSize: testArtifactMaxSize})
	if err != nil {
		t.Fatal(err)
	}
	if meta.Digest != digest || meta.Size != int64(len(data)) {
		t.Fatalf("unexpected metadata: %+v", meta)
	}
	f, opened, err := s.Open(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) || opened != meta {
		t.Fatalf("round trip mismatch: got %q, metadata %+v", got, opened)
	}
}

func TestPutConstraints(t *testing.T) {
	s := newTestStore(t)
	data := []byte("payload")
	wantSize := int64(len(data) + 1)
	if _, err := s.Put(context.Background(), strings.NewReader(string(data)), PutOptions{ExpectedSize: &wantSize, MaxSize: testArtifactMaxSize}); !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("expected size mismatch, got %v", err)
	}
	if _, err := s.Put(context.Background(), strings.NewReader(string(data)), PutOptions{ExpectedDigest: strings.Repeat("0", 64), MaxSize: testArtifactMaxSize}); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("expected digest mismatch, got %v", err)
	}
	if _, err := s.Put(context.Background(), strings.NewReader(string(data)), PutOptions{MaxSize: 2}); !errors.Is(err, ErrSizeLimit) {
		t.Fatalf("expected size limit, got %v", err)
	}
	if _, err := s.Put(context.Background(), strings.NewReader(string(data)), PutOptions{}); !errors.Is(err, ErrInvalidLimit) {
		t.Fatalf("expected missing size limit rejection, got %v", err)
	}
	if _, err := s.Put(context.Background(), strings.NewReader(string(data)), PutOptions{MaxSize: testArtifactMaxSize + 1}); !errors.Is(err, ErrInvalidLimit) {
		t.Fatalf("expected widening rejection, got %v", err)
	}
	if _, err := s.Put(context.Background(), stallReader{}, PutOptions{MaxSize: testArtifactMaxSize}); !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("expected no-progress reader rejection, got %v", err)
	}
}

func TestNewRequiresPositiveBoundedLimit(t *testing.T) {
	root := filepath.Join(t.TempDir(), "artifacts")
	for _, maxSize := range []int64{0, -1, MaxArtifactSize + 1} {
		if _, err := New(root, maxSize); !errors.Is(err, ErrInvalidLimit) {
			t.Fatalf("New(maxSize=%d) error = %v, want ErrInvalidLimit", maxSize, err)
		}
	}
}

type stallReader struct{}

func (stallReader) Read([]byte) (int, error) { return 0, nil }

func TestConcurrentIdempotentPuts(t *testing.T) {
	s := newTestStore(t)
	data := []byte("same bytes")
	const n = 16
	results := make(chan Metadata, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := s.Put(context.Background(), strings.NewReader(string(data)), PutOptions{MaxSize: testArtifactMaxSize})
			results <- m
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first Metadata
	for m := range results {
		if first.StorageRef == "" {
			first = m
		} else if m != first {
			t.Fatalf("non-idempotent result: %+v vs %+v", m, first)
		}
	}
}

func TestRejectCorruptPreexistingAndSymlink(t *testing.T) {
	root := t.TempDir()
	d := digestOf([]byte("expected"))
	path := filepath.Join(root, "sha256", d[:2], d)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("corrupt!"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := New(root, testArtifactMaxSize)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(context.Background(), strings.NewReader("expected"), PutOptions{MaxSize: testArtifactMaxSize}); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("expected corrupt object rejection, got %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "outside"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stat(context.Background(), d); !errors.Is(err, ErrSymlink) {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
}

func TestCancellationAndPermissions(t *testing.T) {
	root := t.TempDir()
	s, err := New(filepath.Join(root, "store"), testArtifactMaxSize)
	if err != nil {
		t.Fatal(err)
	}
	if mode := fileMode(t, s.Root()); mode != 0700 {
		t.Fatalf("root mode = %o", mode)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Put(ctx, strings.NewReader("cancelled"), PutOptions{MaxSize: testArtifactMaxSize}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	reader := &cancelReader{cancel: cancel, data: []byte("first chunk")}
	if _, err := s.Put(ctx, reader, PutOptions{MaxSize: testArtifactMaxSize}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected mid-stream cancellation, got %v", err)
	}
	meta, err := s.Put(context.Background(), strings.NewReader("ok"), PutOptions{MaxSize: testArtifactMaxSize})
	if err != nil {
		t.Fatal(err)
	}
	if meta.StorageRef != "sha256/"+meta.Digest[:2]+"/"+meta.Digest {
		t.Fatalf("storage ref = %q", meta.StorageRef)
	}
	if filepath.IsAbs(meta.StorageRef) || strings.Contains(meta.StorageRef, `\`) {
		t.Fatalf("storage ref is not canonical and relative: %q", meta.StorageRef)
	}
	if mode := fileMode(t, s.objectPath(meta.Digest)); mode != 0600 {
		t.Fatalf("object mode = %o", mode)
	}
	if mode := fileMode(t, filepath.Dir(s.objectPath(meta.Digest))); mode != 0700 {
		t.Fatalf("prefix mode = %o", mode)
	}
}

type cancelReader struct {
	cancel context.CancelFunc
	data   []byte
	done   bool
}

func (r *cancelReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	copy(p, r.data)
	r.cancel()
	return len(r.data), nil
}

func TestInvalidDigest(t *testing.T) {
	s := newTestStore(t)
	for _, digest := range []string{"", strings.Repeat("a", 63), strings.Repeat("A", 64), strings.Repeat("g", 64), "../" + strings.Repeat("a", 60)} {
		if _, err := s.Stat(context.Background(), digest); !errors.Is(err, ErrInvalidDigest) {
			t.Fatalf("digest %q: expected invalid digest, got %v", digest, err)
		}
	}
}

// Replacing the configured pathname after New must not redirect an operation:
// all object traversal is anchored at the root directory fd acquired by New.
func TestRootDirectoryReplacementCannotEscapeOpenOrPut(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "store")
	s, err := New(root, testArtifactMaxSize)
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(base, "moved")
	outside := filepath.Join(base, "outside")
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}
	data := []byte("fd anchored")
	meta, err := s.Put(context.Background(), strings.NewReader(string(data)), PutOptions{MaxSize: testArtifactMaxSize})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, meta.StorageRef)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("artifact escaped replacement root: %v", err)
	}
	f, _, err := s.Open(context.Background(), meta.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := io.ReadAll(f)
	if err != nil || string(got) != string(data) {
		t.Fatalf("anchored open = %q, %v", got, err)
	}
}

func TestContentDirectoryReplacementSymlinkFailsClosed(t *testing.T) {
	base := t.TempDir()
	s, err := New(filepath.Join(base, "store"), testArtifactMaxSize)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("existing object")
	meta, err := s.Put(context.Background(), strings.NewReader(string(data)), PutOptions{MaxSize: testArtifactMaxSize})
	if err != nil {
		t.Fatal(err)
	}
	content := filepath.Join(s.Root(), "sha256")
	if err := os.Rename(content, filepath.Join(base, "saved-sha256")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside")
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, content); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(context.Background(), strings.NewReader("new object"), PutOptions{MaxSize: testArtifactMaxSize}); !errors.Is(err, ErrSymlink) {
		t.Fatalf("Put component swap error = %v, want ErrSymlink", err)
	}
	if f, _, err := s.Open(context.Background(), meta.Digest); !errors.Is(err, ErrSymlink) {
		if f != nil {
			f.Close()
		}
		t.Fatalf("Open component swap error = %v, want ErrSymlink", err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("component swap escaped into outside: %v", entries)
	}
}

func TestNewRejectsIntermediateParentSymlink(t *testing.T) {
	base := t.TempDir()
	realParent := filepath.Join(base, "real")
	if err := os.Mkdir(realParent, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(realParent, link); err != nil {
		t.Fatal(err)
	}
	if _, err := New(filepath.Join(link, "artifacts"), testArtifactMaxSize); !errors.Is(err, ErrSymlink) && !strings.Contains(err.Error(), "too many levels") && !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("New intermediate symlink error = %v", err)
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "artifacts"), testArtifactMaxSize)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

const testArtifactMaxSize int64 = 1024

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
