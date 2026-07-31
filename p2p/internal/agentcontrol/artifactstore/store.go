// Package artifactstore provides a small filesystem-backed, content-addressed
// store for agent artifacts.
package artifactstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrInvalidDigest  = errors.New("artifactstore: invalid digest")
	ErrDigestMismatch = errors.New("artifactstore: digest mismatch")
	ErrSizeMismatch   = errors.New("artifactstore: size mismatch")
	ErrSizeLimit      = errors.New("artifactstore: size limit exceeded")
	ErrInvalidLimit   = errors.New("artifactstore: invalid size limit")
	ErrInvalidObject  = errors.New("artifactstore: invalid object")
	ErrSymlink        = errors.New("artifactstore: symlink is not allowed")
)

// Metadata describes a published artifact. StorageRef is a canonical relative
// content address safe to persist; the configured filesystem root never enters
// the database or a public DTO.
type Metadata struct {
	Digest     string
	Size       int64
	StorageRef string
}

// MaxArtifactSize is the largest deployment artifact this local backend can
// accept. A production caller may select a smaller limit but never widen it.
const MaxArtifactSize int64 = 1 << 30

// PutOptions constrains a put operation. A nil ExpectedSize means that no
// exact size was required. MaxSize is mandatory and may only narrow the
// store's configured maximum.
type PutOptions struct {
	ExpectedDigest string
	ExpectedSize   *int64
	MaxSize        int64
}

// Options is kept as a concise alias for callers that prefer the generic name.
type Options = PutOptions

// Store is a content-addressed artifact store rooted at an explicitly chosen
// directory.
type Store struct {
	root    string
	maxSize int64
	rootFD  int
}

// New creates (or opens) a store rooted at root. Existing path components must
// be real directories; symlinks are rejected.
func New(root string, maxSize int64) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("artifactstore: root is empty")
	}
	if maxSize <= 0 || maxSize > MaxArtifactSize {
		return nil, ErrInvalidLimit
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("artifactstore: resolve root: %w", err)
	}
	fd, err := initializeArtifactRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("artifactstore: open root: %w", err)
	}
	return &Store{root: abs, maxSize: maxSize, rootFD: fd}, nil
}

// NewStore is an explicit synonym for New.
func NewStore(root string, maxSize int64) (*Store, error) { return New(root, maxSize) }

// Root returns the canonical filesystem root supplied to New.
func (s *Store) Root() string {
	if s == nil {
		return ""
	}
	return s.root
}

// Put consumes r, computes its SHA-256 address, and publishes it atomically.
// Concurrent puts of identical content are idempotent.
func (s *Store) Put(ctx context.Context, r io.Reader, opts PutOptions) (Metadata, error) {
	if s == nil || s.root == "" {
		return Metadata{}, fmt.Errorf("artifactstore: nil store")
	}
	if r == nil {
		return Metadata{}, fmt.Errorf("artifactstore: nil reader")
	}
	if err := ctx.Err(); err != nil {
		return Metadata{}, err
	}
	if opts.ExpectedDigest != "" {
		if err := validateDigest(opts.ExpectedDigest); err != nil {
			return Metadata{}, err
		}
	}
	if opts.MaxSize <= 0 || opts.MaxSize > s.maxSize {
		return Metadata{}, ErrInvalidLimit
	}
	if opts.ExpectedSize != nil && *opts.ExpectedSize < 0 {
		return Metadata{}, fmt.Errorf("artifactstore: negative expected size")
	}
	if opts.ExpectedSize != nil && *opts.ExpectedSize > opts.MaxSize {
		return Metadata{}, ErrSizeLimit
	}

	tmp, tmpName, err := createArtifactTemp(s.rootFD)
	if err != nil {
		return Metadata{}, fmt.Errorf("artifactstore: create temporary object: %w", err)
	}
	defer func() {
		_ = tmp.Close()
		_ = removeArtifactTemp(s.rootFD, tmpName)
	}()

	h := sha256.New()
	w := io.MultiWriter(tmp, h)
	buf := make([]byte, 32*1024)
	var size int64
	emptyReads := 0
	for {
		if err := ctx.Err(); err != nil {
			return Metadata{}, err
		}
		n, readErr := r.Read(buf)
		if n > 0 {
			emptyReads = 0
			if size+int64(n) > opts.MaxSize {
				return Metadata{}, ErrSizeLimit
			}
			if _, err := w.Write(buf[:n]); err != nil {
				return Metadata{}, fmt.Errorf("artifactstore: write temporary object: %w", err)
			}
			size += int64(n)
		} else if readErr == nil {
			emptyReads++
			if emptyReads >= 100 {
				return Metadata{}, io.ErrNoProgress
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return Metadata{}, fmt.Errorf("artifactstore: read artifact: %w", readErr)
		}
	}
	if err := ctx.Err(); err != nil {
		return Metadata{}, err
	}
	if opts.ExpectedSize != nil && size != *opts.ExpectedSize {
		return Metadata{}, ErrSizeMismatch
	}
	digest := hex.EncodeToString(h.Sum(nil))
	if opts.ExpectedDigest != "" && digest != opts.ExpectedDigest {
		return Metadata{}, ErrDigestMismatch
	}
	if err := tmp.Sync(); err != nil {
		return Metadata{}, fmt.Errorf("artifactstore: sync temporary object: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Metadata{}, fmt.Errorf("artifactstore: close temporary object: %w", err)
	}

	return publishArtifact(ctx, s.rootFD, tmpName, digest, size)
}

// Stat verifies and returns metadata for digest.
func (s *Store) Stat(ctx context.Context, digest string) (Metadata, error) {
	if s == nil || s.root == "" {
		return Metadata{}, fmt.Errorf("artifactstore: nil store")
	}
	if err := ctx.Err(); err != nil {
		return Metadata{}, err
	}
	if err := validateDigest(digest); err != nil {
		return Metadata{}, err
	}
	meta, exists, err := inspectArtifact(ctx, s.rootFD, digest)
	if err != nil {
		return Metadata{}, err
	}
	if !exists {
		return Metadata{}, os.ErrNotExist
	}
	return meta, nil
}

// Open verifies and opens digest for reading. The caller owns the returned
// file and must close it.
func (s *Store) Open(ctx context.Context, digest string) (io.ReadCloser, Metadata, error) {
	if s == nil || s.root == "" {
		return nil, Metadata{}, fmt.Errorf("artifactstore: nil store")
	}
	if err := ctx.Err(); err != nil {
		return nil, Metadata{}, err
	}
	if err := validateDigest(digest); err != nil {
		return nil, Metadata{}, err
	}
	return openArtifact(ctx, s.rootFD, digest)
}

func (s *Store) objectPath(digest string) string {
	return filepath.Join(s.root, "sha256", digest[:2], digest)
}

func objectRef(digest string) string {
	return filepath.ToSlash(filepath.Join("sha256", digest[:2], digest))
}

func (s *Store) inspect(ctx context.Context, path, digest string) (Metadata, bool, error) {
	if err := ensurePathComponents(filepath.Dir(path)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Metadata{}, false, nil
		}
		return Metadata{}, false, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Metadata{}, false, nil
		}
		return Metadata{}, false, fmt.Errorf("artifactstore: inspect object: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return Metadata{}, false, ErrSymlink
	}
	if !info.Mode().IsRegular() {
		return Metadata{}, false, ErrInvalidObject
	}
	if err := verifyObject(ctx, path, digest, info.Size()); err != nil {
		return Metadata{}, false, err
	}
	return Metadata{Digest: digest, Size: info.Size(), StorageRef: objectRef(digest)}, true, nil
}

func verifyObject(ctx context.Context, path, digest string, expectedSize int64) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("artifactstore: verify object: %w", err)
	}
	defer f.Close()
	return verifyOpenObject(ctx, f, digest, expectedSize)
}

func verifyOpenObject(ctx context.Context, f *os.File, digest string, expectedSize int64) error {
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		if err != nil {
			return fmt.Errorf("artifactstore: stat object: %w", err)
		}
		return ErrInvalidObject
	}
	if info.Size() != expectedSize {
		return fmt.Errorf("%w: object size", ErrInvalidObject)
	}
	h := sha256.New()
	buf := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := f.Read(buf)
		if n > 0 {
			_, _ = h.Write(buf[:n])
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return fmt.Errorf("artifactstore: verify object contents: %w", readErr)
		}
	}
	if hex.EncodeToString(h.Sum(nil)) != digest {
		return fmt.Errorf("%w: object contents", ErrInvalidObject)
	}
	return nil
}

func validateDigest(digest string) error {
	if len(digest) != sha256.Size*2 {
		return ErrInvalidDigest
	}
	for _, c := range digest {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return ErrInvalidDigest
		}
	}
	return nil
}

func createTemp(root string) (*os.File, error) {
	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return nil, err
	}
	name := filepath.Join(root, ".tmp-"+hex.EncodeToString(suffix[:]))
	return os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
}

func ensureDir(path string) error {
	clean, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	vol := filepath.VolumeName(clean)
	rest := strings.TrimPrefix(clean, vol)
	parts := strings.FieldsFunc(rest, func(r rune) bool { return r == filepath.Separator })
	cur := vol
	if strings.HasPrefix(rest, string(filepath.Separator)) {
		cur += string(filepath.Separator)
	}
	for _, part := range parts {
		cur = filepath.Join(cur, part)
		info, statErr := os.Lstat(cur)
		if statErr != nil {
			if !errors.Is(statErr, os.ErrNotExist) {
				return statErr
			}
			if err := os.Mkdir(cur, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, statErr = os.Lstat(cur)
			if statErr != nil {
				return statErr
			}
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return ErrSymlink
		}
		if !info.IsDir() {
			return ErrInvalidObject
		}
	}
	return os.Chmod(clean, 0700)
}

func ensurePathComponents(path string) error {
	clean := filepath.Clean(path)
	vol := filepath.VolumeName(clean)
	rest := strings.TrimPrefix(clean, vol)
	if rest == "" {
		return nil
	}
	parts := strings.FieldsFunc(rest, func(r rune) bool { return r == filepath.Separator })
	cur := vol
	if strings.HasPrefix(rest, string(filepath.Separator)) {
		cur += string(filepath.Separator)
	}
	for _, part := range parts {
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return err
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return ErrSymlink
		}
		if !info.IsDir() {
			return ErrInvalidObject
		}
	}
	return nil
}

func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
