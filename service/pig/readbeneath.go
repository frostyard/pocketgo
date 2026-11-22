package pig

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/frostyard/pocketgo/proto"
)

// ReadBeneathHelper is the hidden pocketd command Engine.DirID and
// Engine.ReadIn run in the sandbox to read a session's checkout (ADR-0053):
//
//   - ReadBeneathHelper ROOT opens the folder ROOT and prints "dir ID\n",
//     ID its device and inode, which name it while it stays, or "none\n".
//   - ReadBeneathHelper ROOT ID REL MAX opens ROOT, checks it is the folder
//     ID names, then opens each folder of REL below it, then REL's file,
//     each by its descriptor and never through a symbolic link, so neither
//     a root nor a folder an agent swapped for a link since git listed the
//     file leads the read elsewhere. It reads a regular file without
//     waiting for a writer, from the descriptor it checked. It prints
//     "file\n", or "exec\n" for one its owner may run, and at most MAX
//     bytes; "link\n" and the target of a symbolic link at REL; "moved\n"
//     when ROOT is no longer the folder ID names; or "none\n" for no
//     regular file or link there.
const ReadBeneathHelper = "__read-beneath"

// dirID is how a folder's identity is written: its device and its inode.
var dirID = regexp.MustCompile(`^[0-9]+:[0-9]+$`)

// RunReadBeneathHelper is ReadBeneathHelper's work, with its arguments
// after the command's name, writing its answer to w: its exit status, 0
// for an answer, 2 for arguments it does not take, 1 for a read that
// failed.
func RunReadBeneathHelper(args []string, w io.Writer) int {
	var answer string
	var data []byte
	var err error
	switch {
	case len(args) == 1 && args[0] != "":
		var id string
		if id, err = identify(args[0]); err == nil {
			answer = "none\n"
			if id != "" {
				answer = "dir " + id + "\n"
			}
		}
	case len(args) == 4 && args[0] != "" && dirID.MatchString(args[1]) && proto.CheckChangedPath(args[2]) == nil:
		max, perr := strconv.ParseInt(args[3], 10, 64)
		if perr != nil || max < 0 {
			return 2
		}
		var kind string
		kind, data, err = readBeneath(args[0], args[1], strings.Split(args[2], "/"), max)
		answer = kind + "\n"
	default:
		return 2
	}
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if _, err := io.WriteString(w, answer); err != nil {
		return 1
	}
	if _, err := w.Write(data); err != nil {
		return 1
	}
	return 0
}

// linkTarget is the most bytes of a symbolic link's target the helper
// reads, a path's longest on Linux.
const linkTarget = 4096

// helper runs ReadBeneathHelper with args in the sandbox and gives what it
// printed, at most room bytes.
func (e *Engine) helper(ctx context.Context, room int, args ...string) (string, error) {
	self := e.opts.Self
	if self == "" {
		var err error
		if self, err = os.Executable(); err != nil {
			return "", err
		}
	}
	cmd, err := e.sandboxed(ctx, append([]string{self, ReadBeneathHelper}, args...))
	if err != nil {
		return "", err
	}
	stdout := &capped{max: room}
	cmd.Stdout = stdout
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return string(stdout.buf), nil
}

// DirID is the identity of the folder root as a command in the sandbox
// opens it, its device and inode; ok is false for no folder there. A read
// of a session's checkout names its top by it, so a top swapped for
// another folder since is told apart (ADR-0053).
func (e *Engine) DirID(ctx context.Context, root string) (id string, ok bool, err error) {
	out, err := e.helper(ctx, 64, root)
	if err != nil {
		return "", false, fmt.Errorf("look at %s: %w", root, err)
	}
	if out == "none\n" {
		return "", false, nil
	}
	if id, found := strings.CutPrefix(out, "dir "); found && dirID.MatchString(strings.TrimSuffix(id, "\n")) && strings.HasSuffix(id, "\n") {
		return strings.TrimSuffix(id, "\n"), true, nil
	}
	return "", false, fmt.Errorf("look at %s: no answer", root)
}

// ReadIn is the file at rel in the folder root, which DirID named id, as a
// command in the sandbox reads it with ReadBeneathHelper: kind "file", or
// "exec" for one its owner may run, with at most limit+1 bytes, so the
// caller sees a file larger than limit; "link" with a symbolic link's
// target; "moved" when root is no longer the folder id names; or "none"
// for neither there, a named pipe, a folder, or a path that goes through a
// link included. rel is a path proto.CheckChangedPath takes (ADR-0053).
func (e *Engine) ReadIn(ctx context.Context, root, id, rel string, limit int) (kind string, data []byte, err error) {
	if limit < 0 {
		return "", nil, fmt.Errorf("read %s: a bound of %d bytes", rel, limit)
	}
	if err := proto.CheckChangedPath(rel); err != nil {
		return "", nil, err
	}
	if !dirID.MatchString(id) {
		return "", nil, fmt.Errorf("read %s: the folder's identity %q", rel, id)
	}
	out, err := e.helper(ctx, max(len("file\n")+limit+1, len("link\n")+linkTarget), root, id, rel, strconv.Itoa(limit+1))
	if err != nil {
		return "", nil, fmt.Errorf("read %s: %w", rel, err)
	}
	for _, k := range []string{"file", "exec", "link", "moved", "none"} {
		if rest, ok := strings.CutPrefix(out, k+"\n"); ok {
			if (k == "none" || k == "moved") && rest != "" {
				break
			}
			return k, []byte(rest), nil
		}
	}
	return "", nil, fmt.Errorf("read %s: no answer", rel)
}
