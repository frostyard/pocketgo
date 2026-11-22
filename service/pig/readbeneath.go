package pig

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/frostyard/pocketgo/proto"
)

// ReadBeneathHelper is the hidden pocketd command Engine.ReadIn runs in the
// sandbox to read a file of a session's checkout (ADR-0053):
// ReadBeneathHelper ROOT REL MAX. It opens ROOT, then each folder of REL
// below it, then REL's file, each by its descriptor and never through a
// symbolic link, so a folder an agent swapped for a link between a listing
// and the read never leads the read out of ROOT. It reads a regular file
// without waiting for a writer, from the descriptor it checked. It prints
// "file\n" and at most MAX bytes; "link\n" and the target of a symbolic
// link at REL; or "none\n" for no regular file or link there.
const ReadBeneathHelper = "__read-beneath"

// RunReadBeneathHelper is ReadBeneathHelper's work, with its arguments
// after the command's name, writing its answer to w: its exit status, 0
// for an answer, 2 for arguments it does not take, 1 for a read that
// failed.
func RunReadBeneathHelper(args []string, w io.Writer) int {
	if len(args) != 3 || args[0] == "" || proto.CheckChangedPath(args[1]) != nil {
		return 2
	}
	max, err := strconv.ParseInt(args[2], 10, 64)
	if err != nil || max < 0 {
		return 2
	}
	kind, data, err := readBeneath(args[0], strings.Split(args[1], "/"), max)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if _, err := io.WriteString(w, kind+"\n"); err != nil {
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

// ReadIn is the file at rel in the folder root as a command in the sandbox
// reads it with ReadBeneathHelper: kind "file" with at most limit+1 bytes, so
// the caller sees a file larger than limit; "link" with a symbolic link's
// target; or "none" for neither there, a named pipe, a folder, or a path
// that goes through a link included. rel is a path proto.CheckChangedPath
// takes (ADR-0053).
func (e *Engine) ReadIn(ctx context.Context, root, rel string, limit int) (kind string, data []byte, err error) {
	if limit < 0 {
		return "", nil, fmt.Errorf("read %s: a bound of %d bytes", rel, limit)
	}
	if err := proto.CheckChangedPath(rel); err != nil {
		return "", nil, err
	}
	self := e.opts.Self
	if self == "" {
		if self, err = os.Executable(); err != nil {
			return "", nil, err
		}
	}
	cmd, err := e.sandboxed(ctx, []string{self, ReadBeneathHelper, root, rel, strconv.Itoa(limit + 1)})
	if err != nil {
		return "", nil, err
	}
	stdout := &capped{max: max(len("file\n")+limit+1, len("link\n")+linkTarget)}
	cmd.Stdout = stdout
	if err := cmd.Run(); err != nil {
		return "", nil, fmt.Errorf("read %s: %w", rel, err)
	}
	out := string(stdout.buf)
	for _, k := range []string{"file", "link", "none"} {
		if rest, ok := strings.CutPrefix(out, k+"\n"); ok {
			if k == "none" && rest != "" {
				break
			}
			return k, []byte(rest), nil
		}
	}
	return "", nil, fmt.Errorf("read %s: no answer", rel)
}
