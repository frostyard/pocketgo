//go:build unix

package profiles

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// refusedLink says err is openNoFollow's refusal of a symbolic link
// (O_NOFOLLOW's ELOOP): a rule the profile breaks, never a read that may
// work later (Codex review of #51, round 3).
func refusedLink(err error) bool { return errors.Is(err, syscall.ELOOP) }

// openNoFollow opens path for reading, failing on a symbolic link, and
// without waiting for a writer when it is a named pipe: the caller checks
// it is a regular file.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

// openFollow opens path for reading, following its links, without waiting
// for a writer when it is a named pipe: the caller checks it is a regular
// file.
func openFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

// openInRoot opens name in root, failing on a symbolic link: os.Root
// resolves a final link whose target stays in the folder, whatever
// O_NOFOLLOW says (Codex review of #34, round 3). The file opened must be
// the one looked at.
func openInRoot(root *os.Root, name string) (*os.File, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("a symbolic link")
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		_ = f.Close()
		return nil, errors.New("the file changed while it was opened")
	}
	return f, nil
}

// links is how many names the file has: a profile with another name could
// be changed through it, outside the protected folder.
func links(info fs.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink)
	}
	return 1
}
