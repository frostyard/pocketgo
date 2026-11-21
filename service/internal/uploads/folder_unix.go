//go:build unix

package uploads

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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

// errNoStaging is a host that makes no unnamed file, for a folder with no
// staging folder.
var errNoStaging = errors.New("this host makes no unnamed file, and pocketd has no staging folder")

// Staged is an upload's file, written whole and not named yet: Link names
// it. Close drops it, and its staged name with it.
type Staged struct {
	dir  int
	file *os.File
	// staging and staged are the staging folder and the file's name there,
	// where the host makes no unnamed file (-1 and "" for none).
	staging  int
	staged   string
	Identity Identity
}

// Stage writes body into a new file that no agent can reach until Link
// names it in session's folder: one with no name in that folder
// (O_TMPFILE), else one in the staging folder, in pocketd's data folder,
// never a name in the uploads folder (Codex review of #49, round 4). It
// says what tells the file apart. A body that fails to read to its end, as
// a cut-off or too large upload does, leaves nothing in the uploads
// folder; so does a stop of pocketd before Link.
func (f *Folder) Stage(session int64, body io.Reader) (*Staged, error) {
	dir, err := f.openSession(session, true)
	if err != nil {
		return nil, err
	}
	s := &Staged{dir: dir, staging: -1}
	if !f.noUnnamed {
		s.file, err = createUnnamed(dir)
	}
	if f.noUnnamed || errors.Is(err, errNoUnnamed) {
		err = f.createStaged(s)
	}
	if err != nil {
		s.Close()
		return nil, err
	}
	sum := sha256.New()
	size, err := io.Copy(io.MultiWriter(s.file, sum), body)
	if err == nil {
		err = s.file.Sync()
	}
	var st unix.Stat_t
	if err == nil {
		err = unix.Fstat(int(s.file.Fd()), &st)
	}
	if err != nil {
		s.Close()
		return nil, err
	}
	s.Identity = Identity{Dev: uint64(st.Dev), Ino: uint64(st.Ino), Size: size, Digest: hex.EncodeToString(sum.Sum(nil))} //nolint:unconvert // Dev's type differs by system
	return s, nil
}

// createStaged makes s's file in the staging folder, named after this run.
func (f *Folder) createStaged(s *Staged) error {
	if f.staging == "" {
		return errNoStaging
	}
	if err := os.MkdirAll(f.staging, 0o700); err != nil {
		return err
	}
	staging, err := unix.Open(f.staging, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: f.staging, Err: err}
	}
	s.staging, s.staged = staging, f.run+"-"+f.random()+f.random()
	fd, err := unix.Openat(staging, s.staged, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		s.staged = ""
		return err
	}
	s.file = os.NewFile(uintptr(fd), s.staged)
	return nil
}

// Link gives the staged file its name, file, where no entry is: a name an
// entry holds already is ErrNameTaken, and that entry stays as it is. A
// staged file is linked from the staging folder, by its own name there,
// which no agent can change, and that name goes.
func (s *Staged) Link(file string) error {
	if !oneName(file) {
		return fmt.Errorf("%q is no upload's file name", file)
	}
	var err error
	if s.staged != "" {
		err = unix.Linkat(s.staging, s.staged, s.dir, file, 0)
		if errors.Is(err, unix.EXDEV) {
			return errors.New("the staging folder is on another file system than the uploads folder")
		}
		if err == nil {
			_ = unix.Unlinkat(s.staging, s.staged, 0)
			s.staged = ""
		}
	} else {
		err = linkUnnamed(s.dir, s.file, file)
	}
	if errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("%s: %w", file, ErrNameTaken)
	}
	return err
}

// Close drops the staged file's descriptors, and its staged name.
func (s *Staged) Close() {
	if s.staged != "" {
		_ = unix.Unlinkat(s.staging, s.staged, 0)
		s.staged = ""
	}
	if s.staging >= 0 {
		_ = unix.Close(s.staging)
		s.staging = -1
	}
	if s.file != nil {
		_ = s.file.Close()
	}
	_ = unix.Close(s.dir)
}

// ClearStaging removes what another run of pocketd left in the staging
// folder: files a stop cut off before Link. No agent reaches the folder.
func (f *Folder) ClearStaging() error {
	if f.staging == "" {
		return nil
	}
	entries, err := os.ReadDir(f.staging)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	var failures []error
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), f.run+"-") {
			if err := os.Remove(filepath.Join(f.staging, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				failures = append(failures, err)
			}
		}
	}
	return errors.Join(failures...)
}

// entryOf is what session's file names, as its folder holds it, read
// without opening it: an entry that is no regular file, a socket or a
// link included, is ErrNotRegular (Codex review of #49, round 4).
func entryOf(dir int, file string) (unix.Stat_t, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(dir, file, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return st, &os.PathError{Op: "stat", Path: file, Err: err}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return st, ErrNotRegular
	}
	return st, nil
}

// openFile opens session's file as the regular file its name holds: its
// entry is read first, and only a regular file is opened, by its
// descriptor, not through a link and without waiting, and must be that
// entry.
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
	held, err := entryOf(dir, file)
	if err != nil {
		return nil, st, err
	}
	fd, err := unix.Openat(dir, file, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.EMLINK), errors.Is(err, unix.ENXIO):
		return nil, st, ErrNotRegular
	case err != nil:
		return nil, st, &os.PathError{Op: "open", Path: file, Err: err}
	}
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, st, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Dev != held.Dev || st.Ino != held.Ino {
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
// its name holds, of id's device, inode and size, and of its bytes'
// digest, since an inode is used again once its file is gone. A name
// that holds nothing, or another entry, holds false.
func (f *Folder) Holds(session int64, file string, id Identity) (bool, error) {
	in, st, err := f.openFile(session, file)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrNotRegular) || errors.Is(err, ErrNotItsFolder) || errors.Is(err, ErrNotOurs) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	defer func() { _ = in.Close() }()
	if uint64(st.Dev) != id.Dev || uint64(st.Ino) != id.Ino || st.Size != id.Size { //nolint:unconvert // Dev's type differs by system
		return false, nil
	}
	sum := sha256.New()
	if _, err := io.Copy(sum, io.LimitReader(in, id.Size+1)); err != nil {
		return false, err
	}
	return hex.EncodeToString(sum.Sum(nil)) == id.Digest, nil
}
