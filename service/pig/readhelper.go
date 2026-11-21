package pig

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"syscall"
)

// ReadHelper is the hidden pocketd command Engine.ReadUpTo runs in the
// sandbox to read a regular file: ReadHelper PATH MAX. It opens PATH
// without waiting for a writer, checks that the descriptor it opened is a
// regular file, and copies at most MAX bytes from that same descriptor, so
// a file that changes between a check and a read is never read as another
// kind, and a named pipe never holds it (Codex review of #57, round 4).
// It prints "file\n" and the bytes, or "none\n" for no regular file it may
// read there.
const ReadHelper = "__read-regular"

// RunReadHelper is ReadHelper's work, with its arguments after the
// command's name, writing its answer to w: its exit status, 0 for an
// answer, 2 for arguments it does not take, 1 for a read that failed.
func RunReadHelper(args []string, w io.Writer) int {
	if len(args) != 2 {
		return 2
	}
	max, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil || max < 0 {
		return 2
	}
	f, err := openNonblocking(args[0])
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.ELOOP), errors.Is(err, syscall.ENOTDIR):
		_, _ = io.WriteString(w, "none\n")
		return 0
	case err != nil:
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if !info.Mode().IsRegular() {
		_, _ = io.WriteString(w, "none\n")
		return 0
	}
	if _, err := io.WriteString(w, "file\n"); err != nil {
		return 1
	}
	if _, err := io.Copy(w, io.LimitReader(f, max)); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
