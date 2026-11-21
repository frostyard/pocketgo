//go:build unix

package uploads

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

// openSession opens session's folder, from base's parent down, making the
// folders that are missing when create says so (openSub). The parent,
// ~/.local/share, is the service user's own and may be reached through
// the host's links.
func (f *Folder) openSession(session int64, create bool) (int, error) {
	parent := filepath.Dir(f.base)
	if create {
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return -1, err
		}
	}
	top, err := unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, &os.PathError{Op: "open", Path: parent, Err: err}
	}
	defer func() { _ = unix.Close(top) }()
	base, err := openSub(top, filepath.Base(f.base), create)
	if err != nil {
		return -1, err
	}
	defer func() { _ = unix.Close(base) }()
	return openSub(base, strconv.FormatInt(session, 10), create)
}

// openSub opens folder name of dir, made first when create says so, by
// its descriptor: as a folder, not through a link and without waiting on
// a named pipe, so a link or anything but a folder there is
// ErrNotItsFolder at once (Codex review of #49, round 3). It must be the
// service user's (ErrNotOurs), and is left open to no one else: one that
// was is made 0700 again, through its descriptor.
func openSub(dir int, name string, create bool) (int, error) {
	if create {
		if err := unix.Mkdirat(dir, name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			return -1, &os.PathError{Op: "mkdir", Path: name, Err: err}
		}
	}
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR), errors.Is(err, unix.EMLINK):
		return -1, fmt.Errorf("%s: %w", name, ErrNotItsFolder)
	case err != nil:
		return -1, &os.PathError{Op: "open", Path: name, Err: err}
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	if int(st.Uid) != os.Getuid() {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("%s: %w", name, ErrNotOurs)
	}
	if st.Mode&0o077 != 0 {
		if err := unix.Fchmod(fd, 0o700); err != nil {
			_ = unix.Close(fd)
			return -1, err
		}
	}
	return fd, nil
}

// Staged is an upload's file, written whole and not named yet: Link names
// it. Close drops it, and an unnamed one with it.
type Staged struct {
	dir  int
	file *os.File
	// part is the visible name the file has until Link, where the host
	// makes no unnamed file ("" for none).
	part     string
	Identity Identity
}

// Stage writes body into a new file of session's folder that has no name
// yet, where the host makes one (createUnnamed), and says what tells it
// apart. A body that fails to read to its end, as a cut-off or too large
// upload does, leaves nothing; so does a stop of pocketd before Link.
func (f *Folder) Stage(session int64, body io.Reader) (*Staged, error) {
	dir, err := f.openSession(session, true)
	if err != nil {
		return nil, err
	}
	file, part, err := createUnnamed(dir, f.random)
	if err != nil {
		_ = unix.Close(dir)
		return nil, err
	}
	s := &Staged{dir: dir, file: file, part: part}
	size, err := io.Copy(file, body)
	if err == nil {
		err = file.Sync()
	}
	var st unix.Stat_t
	if err == nil {
		err = unix.Fstat(int(file.Fd()), &st)
	}
	if err != nil {
		s.Close()
		return nil, err
	}
	s.Identity = Identity{Dev: uint64(st.Dev), Ino: uint64(st.Ino), Size: size} //nolint:unconvert // Dev's type differs by system
	return s, nil
}

// Link gives the staged file its name, file, where no entry is: a name an
// entry holds already is ErrNameTaken, and that entry stays as it is.
func (s *Staged) Link(file string) error {
	if !oneName(file) {
		return fmt.Errorf("%q is no upload's file name", file)
	}
	err := linkUnnamed(s.dir, s.file, s.part, file)
	if errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("%s: %w", file, ErrNameTaken)
	}
	return err
}

// Close drops the staged file's descriptors, and the file itself when it
// has no name of its own yet.
func (s *Staged) Close() {
	if s.part != "" {
		_ = unix.Unlinkat(s.dir, s.part, 0)
		s.part = ""
	}
	_ = s.file.Close()
	_ = unix.Close(s.dir)
}

// openFile opens session's file as the regular file its name holds, by
// its descriptor, not through a link and without waiting on a named pipe.
func (f *Folder) openFile(session int64, file string) (*os.File, unix.Stat_t, error) {
	var st unix.Stat_t
	if !oneName(file) {
		return nil, st, fmt.Errorf("%q is no upload's file name", file)
	}
	dir, err := f.openSession(session, false)
	if err != nil {
		return nil, st, err
	}
	defer func() { _ = unix.Close(dir) }()
	fd, err := unix.Openat(dir, file, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.EMLINK):
		return nil, st, ErrNotRegular
	case err != nil:
		return nil, st, &os.PathError{Op: "open", Path: file, Err: err}
	}
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, st, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return nil, st, ErrNotRegular
	}
	return os.NewFile(uintptr(fd), file), st, nil
}

// Read reads session's file, at most max bytes (ErrGrown for more): only
// the regular file its name holds in the session's folder, through no
// link at all, and without waiting on a named pipe. A link at the name to
// another upload would give the model that file's bytes under this one's
// name (Codex review of #49, round 1).
func (f *Folder) Read(session int64, file string, max int64) ([]byte, error) {
	in, _, err := f.openFile(session, file)
	if err != nil {
		return nil, err
	}
	defer func() { _ = in.Close() }()
	data, err := io.ReadAll(io.LimitReader(in, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, ErrGrown
	}
	return data, nil
}

// Holds says whether session's file is the one id tells: the regular file
// its name holds, pocketd's own as it named it. A name that holds nothing,
// or another entry, holds false.
func (f *Folder) Holds(session int64, file string, id Identity) (bool, error) {
	in, st, err := f.openFile(session, file)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrNotRegular) || errors.Is(err, ErrNotItsFolder) || errors.Is(err, ErrNotOurs) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	_ = in.Close()
	return uint64(st.Dev) == id.Dev && uint64(st.Ino) == id.Ino && st.Size == id.Size, nil //nolint:unconvert // Dev's type differs by system
}
