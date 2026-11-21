package pig

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// GateHelper is the hidden pocketd command every command of a sandbox
// that shows folders starts through, inside the sandbox: GateHelper
// FOLDER... -- COMMAND.... It reads the sandbox's own mount table, and
// runs COMMAND in its place only when every path a mount shows a FOLDER,
// or a part of one, at is on a read-only mount there (ADR-0035). So a
// command admitted before a mount showed a shown folder somewhere new,
// and started after, refuses to run, rather than run with binds that do
// not cover it (Codex review of #63, round 6).
const GateHelper = "__sandbox-gate"

// gateExec replaces the gate with the command (execve(2)); tests never
// let it.
var gateExec = syscall.Exec

// gateMounts reads the sandbox's mount table; tests give it one.
var gateMounts = readMountinfo

// gateRefused is the exit status of a command the gate refused.
const gateRefused = 125

// RunGate is GateHelper's work, with its arguments after the command's
// name: it replaces itself with the command, or writes why it refused to
// w and gives its exit status.
func RunGate(args []string, w io.Writer) int {
	split := slices.Index(args, "--")
	if split < 0 || split == len(args)-1 {
		_, _ = fmt.Fprintln(w, "pocketd's sandbox gate: no command to run")
		return 2
	}
	folders, command := args[:split], args[split+1:]
	var masks []string
	if i := slices.Index(folders, gateMasked); i >= 0 {
		folders, masks = folders[:i], folders[i+1:]
	}
	if err := gateCheck(folders, masks); err != nil {
		_, _ = fmt.Fprintf(w, "pocketd's sandbox refused the command: %v; restart pocketd\n", err)
		return gateRefused
	}
	program := command[0]
	if !filepath.IsAbs(program) {
		found, err := exec.LookPath(program)
		if err != nil {
			_, _ = fmt.Fprintf(w, "pocketd's sandbox gate: %v\n", err)
			return 127
		}
		program = found
	}
	//nolint:gosec // G204: the command is the sandbox's own, given by pocketd.
	err := gateExec(program, command, os.Environ())
	_, _ = fmt.Fprintf(w, "pocketd's sandbox gate: run %s: %v\n", program, err)
	return 126
}

// gateMasked comes between the gate's folders and the masks inside them:
// the paths the sandbox empties with a tmpfs of its own. A path is
// absolute, so no folder is this word.
const gateMasked = "masked"

// gateCheck is nil when, in this process's mount table, each folder is on
// a read-only mount, and so is every other path a mount shows one, or a
// part of one, at, and every mount seen inside one, but the masks.
func gateCheck(folders, masks []string) error {
	mounts, err := gateMounts()
	if err != nil {
		return err
	}
	for _, path := range withAliases(folders, mounts) {
		m, ok := mountAt(mounts, path)
		if !ok {
			return fmt.Errorf("%s: %w", path, errNoMount)
		}
		if !m.readOnly() {
			return fmt.Errorf("a mount shows a shown folder at %s, which is not read-only here (%s)", path, strings.TrimSpace(m.Options))
		}
	}
	// A mount inside a folder shows its own files there, which no alias
	// names: one seen there must be read-only too, but a mask, an empty
	// tmpfs that shows nothing of the owner's (Codex review of #63, round
	// 7).
	for _, m := range mounts {
		inside := false
		for _, folder := range folders {
			inside = inside || m.Point != folder && within(m.Point, folder)
		}
		if !inside || slices.Contains(masks, m.Point) {
			continue
		}
		if at, ok := mountAt(mounts, m.Point); !ok || at.ID != m.ID {
			continue // covered: not seen
		}
		if !m.readOnly() {
			return fmt.Errorf("a mount shows its files at %s, inside a shown folder, and is not read-only here (%s)", m.Point, strings.TrimSpace(m.Options))
		}
	}
	return nil
}
