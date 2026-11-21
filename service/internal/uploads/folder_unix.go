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
	"regexp"
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
		err := unix.Mkdirat(dir, name, 0o700)
		if err != nil && !errors.Is(err, unix.EEXIST) {
			return -1, &os.PathError{Op: "mkdir", Path: name, Err: err}
		}
		// The folder's entry is written to disk in its parent before a
		// file of it counts, whether this call made it or found it: a
		// sync that failed, or that another call still makes, after the
		// folder was made leaves it unsynced (Codex review of #49, rounds
		// 5 and 7).
		if err := syncDir(dir); err != nil {
			return -1, err
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
		if err == nil {
			err = sameDevice(s.staging, dir)
		}
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
	staging, err := f.openStaging(true)
	if err != nil {
		return err
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

// openStaging opens the staging folder, made first when create says so,
// from the data folder that holds it, as a folder and not through a link
// (openSub): a link in its place leads nowhere (Codex review of #49,
// round 5).
func (f *Folder) openStaging(create bool) (int, error) {
	if f.staging == "" {
		return -1, errNoStaging
	}
	parent := filepath.Dir(f.staging)
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
	return openSub(top, filepath.Base(f.staging), create)
}

// stagedName is the form of a staged file's name: a run's 16 hexadecimal
// characters, "-", and 16 more.
var stagedName = regexp.MustCompile(`^[0-9a-f]{16}-[0-9a-f]{16}$`)

// Link gives the staged file its name, file, where no entry is: a name an
// entry holds already is ErrNameTaken, and that entry stays as it is. A
// staged file is linked from the staging folder, by its own name there,
// which no agent can change, and that name goes. The name is written to
// disk in its folder before Link answers, so a file counted stored is
// there after a power loss too (Codex review of #49, round 5).
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
	} else if err != nil {
		return err
	}
	// Named: a sync that fails leaves the file named, which its upload's
	// claim must not forget (Codex review of #49, round 6).
	if err := syncDir(s.dir); err != nil {
		return fmt.Errorf("%s: %w: %w", file, ErrNotSynced, err)
	}
	return nil
}

// sameDevice is nil when folders staging and dir are on one file system,
// where a file staged in one can be named in the other, and else
// ErrStagingElsewhere.
func sameDevice(staging, dir int) error {
	a, err := deviceOf(staging)
	if err != nil {
		return err
	}
	b, err := deviceOf(dir)
	if err != nil {
		return err
	}
	if a != b {
		return ErrStagingElsewhere
	}
	return nil
}

// deviceOf is the file system of the open file fd; tests set it.
var deviceOf = func(fd int) (uint64, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return 0, err
	}
	return uint64(st.Dev), nil //nolint:unconvert // Dev's type differs by system
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
// folder: regular files named as pocketd stages one (stagedName), a stop
// cut off before Link, through the folder's descriptor. Anything else
// there stays, and a folder that is a link is not read (Codex review of
// #49, round 5).
func (f *Folder) ClearStaging() error {
	if f.staging == "" {
		return nil
	}
	fd, err := f.openStaging(false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	dir := os.NewFile(uintptr(fd), f.staging)
	defer func() { _ = dir.Close() }()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return err
	}
	var failures []error
	for _, name := range names {
		if !stagedName.MatchString(name) || strings.HasPrefix(name, f.run+"-") {
			continue
		}
		if _, err := entryOf(fd, name); err != nil {
			continue // no regular file: not pocketd's
		}
		if err := unix.Unlinkat(fd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			failures = append(failures, err)
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
	return f.openFileIf(session, file, nil)
}

// errNoMatch is an entry that is not the one openFileIf was to open.
var errNoMatch = errors.New("the name holds another file")

// openFileIf is openFile of an entry that match takes, read before the
// file is opened (nil takes any): another one is errNoMatch, never
// opened, so a file pocketd may not read is told apart by what its entry
// says (Codex review of #49, round 6).
func (f *Folder) openFileIf(session int64, file string, match func(unix.Stat_t) bool) (*os.File, unix.Stat_t, error) {
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
	if match != nil && !match(held) {
		return nil, held, errNoMatch
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
// that holds nothing, or another entry, holds false, told by its entry
// before it is opened, so a file pocketd may not read holds false too. A
// name it holds is written to disk in its folder first.
func (f *Folder) Holds(session int64, file string, id Identity) (bool, error) {
	is := func(st unix.Stat_t) bool {
		return uint64(st.Dev) == id.Dev && uint64(st.Ino) == id.Ino && st.Size == id.Size //nolint:unconvert // Dev's type differs by system
	}
	in, st, err := f.openFileIf(session, file, is)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrNotRegular) || errors.Is(err, ErrNotItsFolder) || errors.Is(err, ErrNotOurs) || errors.Is(err, errNoMatch) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	defer func() { _ = in.Close() }()
	if !is(st) {
		return false, nil
	}
	sum := sha256.New()
	if _, err := io.Copy(sum, io.LimitReader(in, id.Size+1)); err != nil {
		return false, err
	}
	if hex.EncodeToString(sum.Sum(nil)) != id.Digest {
		return false, nil
	}
	if holdsHashed != nil {
		holdsHashed()
	}
	// The name is written to disk before its upload counts: a sync after
	// Link may have failed (Codex review of #49, round 6). The folder
	// synced is the one whose entry names the file now: a file or a folder
	// replaced as the file was read holds false (Codex review of #49, round
	// 8).
	dir, err := f.openSession(session, false)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrNotItsFolder) || errors.Is(err, ErrNotOurs) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	defer func() { _ = unix.Close(dir) }()
	held, err := entryOf(dir, file)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrNotRegular) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if !is(held) {
		return false, nil
	}
	if err := syncDir(dir); err != nil {
		return false, err
	}
	return true, nil
}

// holdsHashed runs as Holds read a file's bytes; tests set it.
var holdsHashed func()

// Names says whether session's file, as the uploads folder names it now,
// is the file id tells, by its device and inode: a session's folder an
// agent moved while the file was named leaves the file elsewhere, not at
// the upload's path (Codex review of #49, round 7).
func (f *Folder) Names(session int64, file string, id Identity) (bool, error) {
	if !oneName(file) {
		return false, fmt.Errorf("%q is no upload's file name", file)
	}
	dir, err := f.openSession(session, false)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrNotItsFolder) || errors.Is(err, ErrNotOurs) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	defer func() { _ = unix.Close(dir) }()
	st, err := entryOf(dir, file)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrNotRegular) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return uint64(st.Dev) == id.Dev && uint64(st.Ino) == id.Ino, nil //nolint:unconvert // Dev's type differs by system
}
