//go:build unix

package api

import (
	"context"
	"errors"
	"fmt"
	"github.com/frostyard/pocketgo/service/internal/files"
	"io"
	"os"
	"strings"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/changes"
	"golang.org/x/sys/unix"
)

// folderID is the device and inode of the folder fd names.
func folderID(fd int) (string, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d", uint64(st.Dev), uint64(st.Ino)), nil //nolint:unconvert // the fields' types differ by system
}

// DirID looks outside any sandbox, as only a test does, by the rule of
// pig's ReadBeneathHelper.
func (f *fakeEngine) DirID(_ context.Context, root string) (string, bool, error) {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", false, nil
	}
	defer func() { _ = unix.Close(fd) }()
	id, err := folderID(fd)
	return id, err == nil, err
}

// ReadIn reads outside any sandbox, as only a test does, by the rule of
// pig's ReadBeneathHelper: root checked to be the folder id names, each
// folder of rel opened from the one above it, none through a link, the
// file without waiting for a writer.
func (f *fakeEngine) ReadIn(_ context.Context, root, id, rel string, max int) (string, []byte, error) {
	if err := proto.CheckChangedPath(rel); err != nil {
		return "", nil, err
	}
	dir, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return changes.KindMoved, nil, nil
	}
	if got, err := folderID(dir); err != nil || got != id {
		_ = unix.Close(dir)
		return changes.KindMoved, nil, err
	}
	parts := strings.Split(rel, "/")
	for _, part := range parts[:len(parts)-1] {
		next, err := unix.Openat(dir, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(dir)
		if err != nil {
			return changes.KindNone, nil, nil
		}
		dir = next
	}
	defer func() { _ = unix.Close(dir) }()
	name := parts[len(parts)-1]
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ELOOP) {
		buf := make([]byte, 4096)
		n, err := unix.Readlinkat(dir, name, buf)
		if err != nil {
			return changes.KindNone, nil, nil
		}
		return changes.KindLink, buf[:n], nil
	}
	if err != nil {
		return changes.KindNone, nil, nil
	}
	file := os.NewFile(uintptr(fd), name)
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return changes.KindNone, nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(max)+1))
	if info.Mode().Perm()&0o100 != 0 {
		return changes.KindExec, data, err
	}
	return changes.KindFile, data, err
}

// ListFiles lists a fixture outside any sandbox, as only a test does.
func (f *fakeEngine) ListFiles(_ context.Context, root, rel string, maxEntries, maxNames int) (files.Folder, error) {
	return files.List(root, rel, maxEntries, maxNames)
}

// ReadFile reads a fixture outside any sandbox, as only a test does.
func (f *fakeEngine) ReadFile(_ context.Context, root, rel string, max int64) (files.File, error) {
	return files.Read(root, rel, max)
}
