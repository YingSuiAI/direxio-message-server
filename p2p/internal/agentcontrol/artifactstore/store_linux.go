//go:build linux

package artifactstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

var errUnsupported = errors.New("artifactstore: secure dirfd storage unsupported")

// initializeArtifactRoot walks from / with O_NOFOLLOW for every component;
// no path lookup after a component has been accepted can follow a symlink.
func initializeArtifactRoot(root string) (int, error) {
	if !filepath.IsAbs(root) {
		return -1, errUnsupported
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Clean(root), "/"), "/") {
		if part == "" || part == "." || part == ".." {
			unix.Close(fd)
			return -1, errUnsupported
		}
		if err := unix.Mkdirat(fd, part, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
			unix.Close(fd)
			return -1, err
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		unix.Close(fd)
		if err != nil {
			return -1, err
		}
		fd = next
	}
	if err := unix.Fchmod(fd, 0700); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func openArtifactRoot(root string) (int, error) {
	return unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
}

func createArtifactTemp(root int) (*os.File, string, error) {
	for i := 0; i < 32; i++ {
		name := fmt.Sprintf(".tmp-%d-%d", os.Getpid(), i)
		fd, err := unix.Openat(root, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
		if err == nil {
			return os.NewFile(uintptr(fd), name), name, nil
		}
		if !errors.Is(err, unix.EEXIST) {
			return nil, "", err
		}
	}
	return nil, "", unix.EEXIST
}
func removeArtifactTemp(root int, name string) error { return unix.Unlinkat(root, name, 0) }

func artifactPrefix(root int, digest string, create bool) (int, error) {
	if create {
		if err := unix.Mkdirat(root, "sha256", 0700); err != nil && !errors.Is(err, unix.EEXIST) {
			return -1, err
		}
	}
	a, err := unix.Openat(root, "sha256", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
		return -1, ErrSymlink
	}
	if err != nil {
		return -1, err
	}
	if create {
		if err := unix.Mkdirat(a, digest[:2], 0700); err != nil && !errors.Is(err, unix.EEXIST) {
			unix.Close(a)
			return -1, err
		}
	}
	b, err := unix.Openat(a, digest[:2], unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
		unix.Close(a)
		return -1, ErrSymlink
	}
	unix.Close(a)
	return b, err
}
func inspectArtifact(ctx context.Context, root int, digest string) (Metadata, bool, error) {
	d, err := artifactPrefix(root, digest, false)
	if errors.Is(err, unix.ELOOP) {
		return Metadata{}, false, ErrSymlink
	}
	if errors.Is(err, unix.ENOENT) {
		return Metadata{}, false, nil
	}
	if err != nil {
		return Metadata{}, false, err
	}
	defer unix.Close(d)
	fd, err := unix.Openat(d, digest, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ELOOP) {
		return Metadata{}, false, ErrSymlink
	}
	if errors.Is(err, unix.ENOENT) {
		return Metadata{}, false, nil
	}
	if err != nil {
		return Metadata{}, false, err
	}
	f := os.NewFile(uintptr(fd), digest)
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return Metadata{}, false, ErrInvalidObject
	}
	if err = verifyOpenObject(ctx, f, digest, info.Size()); err != nil {
		return Metadata{}, false, err
	}
	return Metadata{Digest: digest, Size: info.Size(), StorageRef: objectRef(digest)}, true, nil
}
func publishArtifact(ctx context.Context, root int, tmp, digest string, size int64) (Metadata, error) {
	d, err := artifactPrefix(root, digest, true)
	if err != nil {
		return Metadata{}, err
	}
	defer unix.Close(d)
	if meta, ok, err := inspectArtifact(ctx, root, digest); err != nil {
		return Metadata{}, err
	} else if ok {
		if meta.Size != size {
			return Metadata{}, ErrInvalidObject
		}
		return meta, nil
	}
	if err = unix.Linkat(root, tmp, d, digest, 0); err != nil {
		if !errors.Is(err, unix.EEXIST) {
			return Metadata{}, err
		}
		meta, ok, e := inspectArtifact(ctx, root, digest)
		if e != nil || !ok || meta.Size != size {
			return Metadata{}, ErrInvalidObject
		}
		return meta, nil
	}
	if err = unix.Fsync(d); err != nil {
		return Metadata{}, err
	}
	return Metadata{Digest: digest, Size: size, StorageRef: objectRef(digest)}, nil
}
func openArtifact(ctx context.Context, root int, digest string) (io.ReadCloser, Metadata, error) {
	d, err := artifactPrefix(root, digest, false)
	if errors.Is(err, unix.ELOOP) {
		return nil, Metadata{}, ErrSymlink
	}
	if err != nil {
		return nil, Metadata{}, err
	}
	defer unix.Close(d)
	fd, err := unix.Openat(d, digest, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ELOOP) {
		return nil, Metadata{}, ErrSymlink
	}
	if err != nil {
		return nil, Metadata{}, err
	}
	f := os.NewFile(uintptr(fd), digest)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, Metadata{}, ErrInvalidObject
	}
	if err = verifyOpenObject(ctx, f, digest, info.Size()); err != nil {
		f.Close()
		return nil, Metadata{}, err
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, Metadata{}, err
	}
	return f, Metadata{Digest: digest, Size: info.Size(), StorageRef: objectRef(digest)}, nil
}
