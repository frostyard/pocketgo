//go:build unix

package files

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/frostyard/pocketgo/proto"
	"golang.org/x/sys/unix"
)

// pathError maps missing or inaccessible entries to the viewer's error.
func pathError(what string, err error) error {
	for _, e := range []error{unix.ENOENT, unix.ENOTDIR, unix.EACCES, unix.EPERM, unix.ENXIO, unix.ELOOP} {
		if errors.Is(err, e) {
			return fmt.Errorf("%s: %w", what, ErrNotFound)
		}
	}
	return err
}

// openRoot follows the owner's configured root, not a viewer-supplied link.
func openRoot(root string) (int, error) {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	return fd, pathError("home", err)
}

// resolve walks relative links using a stack of pinned folders, never kernel '..'.
// The caller keeps rootfd and owns the returned descriptor.
func resolve(rootfd int, rel string) (int, string, error) {
	first, err := unix.Openat(rootfd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, "", pathError("home", err)
	}
	stack := []int{first}
	names := []string{}
	defer func() {
		for _, fd := range stack {
			_ = unix.Close(fd)
		}
	}()
	queue := strings.Split(rel, "/")
	links := 0
	for len(queue) > 0 {
		part := queue[0]
		queue = queue[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			if len(names) == 0 {
				return -1, "", fmt.Errorf("link target: %w", ErrOutside)
			}
			_ = unix.Close(stack[len(stack)-1])
			stack = stack[:len(stack)-1]
			names = names[:len(names)-1]
			continue
		}
		if proto.HiddenName(part) || proto.WithheldName(part) {
			return -1, "", fmt.Errorf("link target: %w", ErrHidden)
		}
		top := stack[len(stack)-1]
		var st unix.Stat_t
		if err := unix.Fstatat(top, part, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return -1, "", pathError("entry", err)
		}
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFLNK:
			links++
			if links > MaxLinks {
				return -1, "", fmt.Errorf("too many links: %w", ErrOutside)
			}
			buf := make([]byte, 4096)
			n, err := unix.Readlinkat(top, part, buf)
			if err != nil {
				return -1, "", pathError("link", err)
			}
			target := string(buf[:n])
			if strings.HasPrefix(target, "/") || n == len(buf) {
				return -1, "", fmt.Errorf("link target: %w", ErrOutside)
			}
			queue = append(strings.Split(target, "/"), queue...)
		case unix.S_IFDIR:
			fd, err := unix.Openat(top, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				return -1, "", pathError("folder", err)
			}
			stack = append(stack, fd)
			names = append(names, part)
		default:
			if len(queue) > 0 {
				return -1, "", fmt.Errorf("entry in path: %w", ErrNotFound)
			}
			if st.Mode&unix.S_IFMT != unix.S_IFREG {
				return -1, "", fmt.Errorf("entry: %w", ErrNotFile)
			}
			fd, err := unix.Openat(top, part, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
			if err != nil {
				return -1, "", pathError("file", err)
			}
			if err := unix.Fstat(fd, &st); err != nil {
				_ = unix.Close(fd)
				return -1, "", err
			}
			if st.Mode&unix.S_IFMT != unix.S_IFREG {
				_ = unix.Close(fd)
				return -1, "", fmt.Errorf("changed entry: %w", ErrNotFile)
			}
			return fd, strings.Join(append(names, part), "/"), nil
		}
	}
	fd := stack[len(stack)-1]
	stack = stack[:len(stack)-1]
	return fd, strings.Join(names, "/"), nil
}

// entryStat fills the kind, size and modification time without opening an entry.
func entryStat(e *Entry, st *unix.Stat_t) {
	e.Kind = KindOther
	e.Size = 0
	e.Modified = mtime(st)
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		e.Kind = KindFolder
	case unix.S_IFREG:
		e.Kind = KindFile
		e.Size = st.Size
	}
}

// List reads a bounded, case-insensitively sorted listing below root.
func List(root, rel string, maxEntries, maxNames int) (Folder, error) {
	clean, err := proto.CheckFilePath(rel)
	if err != nil {
		return Folder{}, codedError{msg: err.Error(), base: ErrBadPath}
	}
	rootfd, err := openRoot(root)
	if err != nil {
		return Folder{}, err
	}
	defer func() { _ = unix.Close(rootfd) }()
	fd, real, err := resolve(rootfd, clean)
	if err != nil {
		return Folder{}, err
	}
	f := os.NewFile(uintptr(fd), "folder")
	defer func() { _ = f.Close() }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return Folder{}, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return Folder{}, fmt.Errorf("entry: %w", ErrNotFolder)
	}
	// Nonpositive limits give an empty bounded listing rather than unbounded reads.
	maxEntries = max(0, maxEntries)
	maxNames = max(0, maxNames)
	names, err := f.Readdirnames(maxNames + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return Folder{}, err
	}
	out := Folder{Path: real, Entries: []Entry{}, Partial: len(names) > maxNames}
	if out.Partial {
		names = names[:maxNames]
	}
	visible := names[:0]
	for _, name := range names {
		if !proto.HiddenName(name) {
			visible = append(visible, name)
		}
	}
	sort.Slice(visible, func(i, j int) bool {
		a, b := strings.ToLower(visible[i]), strings.ToLower(visible[j])
		if a == b {
			return visible[i] < visible[j]
		}
		return a < b
	})
	if len(visible) > maxEntries {
		out.More = len(visible) - maxEntries
		visible = visible[:maxEntries]
	}
	for _, name := range visible {
		if err := unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			continue
		}
		e := Entry{Name: name, Withheld: proto.WithheldName(name), Link: st.Mode&unix.S_IFMT == unix.S_IFLNK}
		entryStat(&e, &st)
		if e.Link && !e.Withheld {
			target := name
			if real != "" {
				target = real + "/" + name
			}
			linkfd, _, err := resolve(rootfd, target)
			if err == nil {
				if err := unix.Fstat(linkfd, &st); err == nil {
					entryStat(&e, &st)
				}
				_ = unix.Close(linkfd)
			}
		}
		out.Entries = append(out.Entries, e)
	}
	return out, nil
}

// Read reads a whole regular file only when it fits within max bytes.
func Read(root, rel string, max int64) (File, error) {
	clean, err := proto.CheckFilePath(rel)
	if err != nil {
		return File{}, codedError{msg: err.Error(), base: ErrBadPath}
	}
	rootfd, err := openRoot(root)
	if err != nil {
		return File{}, err
	}
	defer func() { _ = unix.Close(rootfd) }()
	fd, real, err := resolve(rootfd, clean)
	if err != nil {
		return File{}, err
	}
	f := os.NewFile(uintptr(fd), "file")
	defer func() { _ = f.Close() }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return File{}, err
	}
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		return File{}, fmt.Errorf("it is a folder: %w", ErrNotFile)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return File{}, fmt.Errorf("entry: %w", ErrNotFile)
	}
	out := File{Path: real, Size: st.Size, Modified: mtime(&st)}
	if st.Size > max {
		out.Over = true
		return out, nil
	}
	// Avoid overflowing the extra-byte bound for the largest int64.
	limit := max
	if max < 1<<63-1 {
		limit++
	}
	data, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return File{}, err
	}
	if int64(len(data)) > max {
		out.Over = true
	} else {
		out.Data = data
	}
	return out, nil
}
