package pig

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/MichaelKinsy/PiG/durable/env"
)

// commandEnv lists the environment variables a tool's command inherits from
// pocketd. Every other variable, provider keys included, stays out of the
// commands agents run (ADR-0005).
var commandEnv = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "LC_CTYPE",
	"TERM", "TZ", "TMPDIR", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME",
	"GOPATH", "GOCACHE", "GOMODCACHE", "GOFLAGS",
}

// allowedEnv is the values of commandEnv in pocketd's environment.
func allowedEnv() map[string]string {
	out := map[string]string{}
	for _, name := range commandEnv {
		if value, ok := os.LookupEnv(name); ok {
			out[name] = value
		}
	}
	return out
}

// errProtected is the error of a file access in a protected folder.
var errProtected = errors.New("this path belongs to pocketd and is not available to agents")

// errReadOnly is the error of a change in a folder agents may only read
// (ADR-0035).
var errReadOnly = errors.New("this folder is the owner's and read-only to agents: read it, but change nothing in it")

// errReadOnlyFile is the error of a change to the owner's global
// instructions file, which every session's instructions hold
// (ADR-0043) and agents may only read (ADR-0044).
var errReadOnlyFile = errors.New("this file holds the owner's rules for every session and is read-only to agents: read it, but change nothing in it")

// readOnlyErr is the error for a change to the read-only path info
// describes: the owner's global instructions file's, or another's.
func (g *guardedEnv) readOnlyErr(info fs.FileInfo) error {
	if info != nil {
		for _, file := range g.files {
			if kept, err := os.Lstat(file); err == nil && os.SameFile(info, kept) {
				return errReadOnlyFile
			}
		}
	}
	return errReadOnly
}

// guardedEnv is an execution environment whose file access refuses the
// protected folders, whose changes refuse the read-only folders too, and
// whose commands get only allowedEnv and run in the sandbox. It checks the
// path PiG will open: AbsolutePath applies the environment's own
// normalization (relative paths, "~", file URLs), then symbolic links are
// resolved, at the moment of access.
type guardedEnv struct {
	env.ExecutionEnv
	protected []string
	// readOnly are paths whose files agents read but never change: the
	// sandbox's Shown paths, the skills and the uploads folders (ADR-0035)
	// and the global instructions file (ADR-0044). The file tools run in
	// pocketd, outside the sandbox, so they refuse a change there
	// themselves.
	readOnly []string
	// files are the sandbox's files among readOnly, the global instructions
	// file (ADR-0044): a change refused there gets errReadOnlyFile.
	files    []string
	commands map[string]string
	// sandbox runs every command (ADR-0012); nil only in tests that asked
	// for Options.Unsandboxed.
	sandbox *Sandbox
}

// resolve is path as PiG will open it, absolute, and as the kernel
// resolves it.
func (g *guardedEnv) resolve(ctx context.Context, path string) (absolute, resolved string, err error) {
	absolute, err = g.AbsolutePath(ctx, path)
	if err != nil {
		return "", "", err
	}
	resolved, err = resolvePath(absolute)
	return absolute, resolved, err
}

// check refuses a path in a protected folder, for any access.
func (g *guardedEnv) check(ctx context.Context, path string) error {
	_, resolved, err := g.resolve(ctx, path)
	if err != nil {
		return err
	}
	for _, dir := range g.protected {
		if within(resolved, dir) {
			return errProtected
		}
	}
	return nil
}

// checkChange is check for a call that writes, creates or flushes path: it
// also refuses a path in a read-only folder, through links too.
func (g *guardedEnv) checkChange(ctx context.Context, path string) error {
	if err := g.readOnlyKept(ctx, path); err != nil {
		return err
	}
	_, resolved, err := g.resolve(ctx, path)
	if err != nil {
		return err
	}
	for _, dir := range g.readOnly {
		if within(resolved, dir) {
			info, _ := os.Lstat(dir)
			return g.readOnlyErr(info)
		}
	}
	// A path is in a read-only folder when the folder is the path, or a
	// folder above it, by identity too: on a filesystem that looks names
	// up without regard to case, another spelling names the same folder,
	// which no comparison of the names finds (Codex review of #63, round
	// 9).
	if id, ok := g.matchOne(chain(resolved), g.readOnlyIDs(false)); ok {
		return g.readOnlyErr(id)
	}
	return nil
}

// readOnlyKept is what every change checks first: path is in no protected
// folder (check), no link leads a read-only folder away (redirected), and
// the sandbox keeps its shown folders now.
func (g *guardedEnv) readOnlyKept(ctx context.Context, path string) error {
	if err := g.check(ctx, path); err != nil {
		return err
	}
	if err := g.redirected(); err != nil {
		return err
	}
	// The sandbox keeps its shown folders first: a change it cannot keep
	// read-only, as a replaced folder a mount shows elsewhere too, stops
	// the file tools' changes as it stops commands (Codex review of #63,
	// round 3).
	if g.sandbox != nil {
		if err := g.sandbox.keepShown(ctx); err != nil {
			return fmt.Errorf("%w; %w", errReadOnly, err)
		}
	}
	return nil
}

// readOnlyIDs are the read-only folders as they are now, and, when above
// is true, every folder above each as well.
func (g *guardedEnv) readOnlyIDs(above bool) []fs.FileInfo {
	var out []fs.FileInfo
	for _, dir := range g.readOnly {
		if above {
			out = append(out, chain(dir)...)
		} else if info, err := os.Lstat(dir); err == nil {
			out = append(out, info)
		}
	}
	return out
}

// matchOne is the first of ids that is the same file as one of infos.
func (g *guardedEnv) matchOne(infos, ids []fs.FileInfo) (fs.FileInfo, bool) {
	for _, info := range infos {
		for _, id := range ids {
			if os.SameFile(info, id) {
				return id, true
			}
		}
	}
	return nil, false
}

// chain is path and each folder above it, as they are now, those that
// exist.
func chain(path string) []fs.FileInfo {
	var out []fs.FileInfo
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		if info, err := os.Lstat(p); err == nil {
			out = append(out, info)
		}
		if p == filepath.Dir(p) {
			return out
		}
	}
}

// redirected refuses every change while a symbolic link stands on the way
// to a read-only folder since pocketd started, as one the owner put in its
// place: the folder's path leads elsewhere, which the sandbox's binds now
// follow (Sandbox.keepShown stops every command until a restart), and the
// folder it leads to may show under other paths a mount gives it, which
// only the start's mount table named (Codex review of #63, rounds 1 and
// 2).
func (g *guardedEnv) redirected() error {
	for _, dir := range g.readOnly {
		if now, err := resolvePath(dir); err != nil || now != dir {
			return fmt.Errorf("%w; a symbolic link has stood on the way to %s since pocketd started, so nothing changes until pocketd restarts", errReadOnly, dir)
		}
	}
	return nil
}

// checkTree is checkChange for a call that moves or removes path with
// everything below it: it also refuses a folder that holds a protected or
// a read-only path, so a rename or a removal cannot take one along. It
// looks at the entry itself, not where a link there leads: removing or
// renaming a link to an upload or a skill changes neither (Codex review
// of #63, round 10).
func (g *guardedEnv) checkTree(ctx context.Context, path string) error {
	if err := g.readOnlyKept(ctx, path); err != nil {
		return err
	}
	absolute, resolved, err := g.resolve(ctx, path)
	if err != nil {
		return err
	}
	// Rename and remove act on the last entry itself, not on where a link
	// there leads: the folder that holds it must not be protected either.
	parent, err := resolvePath(filepath.Dir(absolute))
	if err != nil {
		return err
	}
	entry := filepath.Join(parent, filepath.Base(absolute))
	for _, dir := range g.protected {
		if within(dir, resolved) || within(entry, dir) {
			return errProtected
		}
	}
	for _, dir := range g.readOnly {
		if within(dir, entry) || within(entry, dir) {
			info, _ := os.Lstat(dir)
			return g.readOnlyErr(info)
		}
	}
	// By identity too, for another spelling of a name (checkChange): the
	// entry may not be a read-only folder or a folder above one, nor lie
	// in one.
	if entryInfo, err := os.Lstat(entry); err == nil {
		if id, ok := g.matchOne([]fs.FileInfo{entryInfo}, g.readOnlyIDs(true)); ok {
			return g.readOnlyErr(id)
		}
	}
	if id, ok := g.matchOne(chain(parent), g.readOnlyIDs(false)); ok {
		return g.readOnlyErr(id)
	}
	return nil
}

func (g *guardedEnv) ReadTextFile(ctx context.Context, path string) (string, error) {
	if err := g.check(ctx, path); err != nil {
		return "", err
	}
	return g.ExecutionEnv.ReadTextFile(ctx, path)
}

func (g *guardedEnv) OpenTextLineReader(ctx context.Context, path string) (env.TextLineReader, error) {
	if err := g.check(ctx, path); err != nil {
		return nil, err
	}
	return g.ExecutionEnv.OpenTextLineReader(ctx, path)
}

func (g *guardedEnv) ReadTextLines(ctx context.Context, path string, options *env.ReadTextLinesOptions) ([]string, error) {
	if err := g.check(ctx, path); err != nil {
		return nil, err
	}
	return g.ExecutionEnv.ReadTextLines(ctx, path, options)
}

func (g *guardedEnv) ReadBinaryFile(ctx context.Context, path string) ([]byte, error) {
	if err := g.check(ctx, path); err != nil {
		return nil, err
	}
	return g.ExecutionEnv.ReadBinaryFile(ctx, path)
}

func (g *guardedEnv) OpenBinaryReader(ctx context.Context, path string, options *env.OpenBinaryReaderOptions) (env.BinaryReader, error) {
	if err := g.check(ctx, path); err != nil {
		return nil, err
	}
	return g.ExecutionEnv.OpenBinaryReader(ctx, path, options)
}

func (g *guardedEnv) WriteFile(ctx context.Context, path string, content any) error {
	if err := g.checkChange(ctx, path); err != nil {
		return err
	}
	return g.ExecutionEnv.WriteFile(ctx, path, content)
}

func (g *guardedEnv) AppendFile(ctx context.Context, path string, content any) error {
	if err := g.checkChange(ctx, path); err != nil {
		return err
	}
	return g.ExecutionEnv.AppendFile(ctx, path, content)
}

func (g *guardedEnv) TruncateFile(ctx context.Context, path string, size int64) error {
	if err := g.checkChange(ctx, path); err != nil {
		return err
	}
	return g.ExecutionEnv.TruncateFile(ctx, path, size)
}

func (g *guardedEnv) FlushFile(ctx context.Context, path string) error {
	if err := g.checkChange(ctx, path); err != nil {
		return err
	}
	return g.ExecutionEnv.FlushFile(ctx, path)
}

func (g *guardedEnv) RenameFile(ctx context.Context, sourcePath, destinationPath string) error {
	if err := g.checkTree(ctx, sourcePath); err != nil {
		return err
	}
	if err := g.checkTree(ctx, destinationPath); err != nil {
		return err
	}
	return g.ExecutionEnv.RenameFile(ctx, sourcePath, destinationPath)
}

func (g *guardedEnv) FileInfo(ctx context.Context, path string) (env.FileInfo, error) {
	if err := g.check(ctx, path); err != nil {
		return env.FileInfo{}, err
	}
	return g.ExecutionEnv.FileInfo(ctx, path)
}

func (g *guardedEnv) ListDir(ctx context.Context, path string) ([]env.FileInfo, error) {
	if err := g.check(ctx, path); err != nil {
		return nil, err
	}
	return g.ExecutionEnv.ListDir(ctx, path)
}

func (g *guardedEnv) ListFiles(ctx context.Context, path string, options *env.ListFilesOptions) (env.FileListing, error) {
	if err := g.check(ctx, path); err != nil {
		return env.FileListing{}, err
	}
	return g.ExecutionEnv.ListFiles(ctx, path, options)
}

func (g *guardedEnv) OpenDirReader(ctx context.Context, path string) (env.DirReader, error) {
	if err := g.check(ctx, path); err != nil {
		return nil, err
	}
	return g.ExecutionEnv.OpenDirReader(ctx, path)
}

func (g *guardedEnv) Watch(ctx context.Context, targets []env.WatchTarget, onChange func(env.FileChange)) (env.FileWatcher, error) {
	for _, target := range targets {
		if err := g.check(ctx, target.Path); err != nil {
			return nil, err
		}
	}
	return g.ExecutionEnv.Watch(ctx, targets, onChange)
}

func (g *guardedEnv) CanonicalPath(ctx context.Context, path string) (string, error) {
	if err := g.check(ctx, path); err != nil {
		return "", err
	}
	return g.ExecutionEnv.CanonicalPath(ctx, path)
}

func (g *guardedEnv) Exists(ctx context.Context, path string) (bool, error) {
	if err := g.check(ctx, path); err != nil {
		return false, err
	}
	return g.ExecutionEnv.Exists(ctx, path)
}

func (g *guardedEnv) CreateDir(ctx context.Context, path string, options *env.CreateDirOptions) error {
	if err := g.checkChange(ctx, path); err != nil {
		return err
	}
	return g.ExecutionEnv.CreateDir(ctx, path, options)
}

func (g *guardedEnv) Remove(ctx context.Context, path string, options *env.RemoveOptions) error {
	if err := g.checkTree(ctx, path); err != nil {
		return err
	}
	return g.ExecutionEnv.Remove(ctx, path, options)
}

// Exec runs a command in the sandbox, with allowedEnv plus the variables the
// tool sets itself, never pocketd's whole environment.
func (g *guardedEnv) Exec(ctx context.Context, command any, options *env.ShellExecOptions) (env.ShellExecResult, error) {
	if g.sandbox != nil {
		wrapped, err := g.sandbox.CommandContext(ctx, command)
		if err != nil {
			return env.ShellExecResult{}, env.NewExecutionError(env.ExecutionErrorSpawnError, err.Error(), err)
		}
		command = wrapped
	}
	var opts env.ShellExecOptions
	if options != nil {
		opts = *options
	}
	merged := maps.Clone(g.commands)
	maps.Copy(merged, opts.Env)
	opts.Env = merged
	inherit := false
	opts.InheritEnv = &inherit
	return g.ExecutionEnv.Exec(ctx, command, &opts)
}

// resolvePath is path made absolute and resolved as the kernel resolves
// it when PiG opens it, so a link cannot lead around a protected folder
// (walkPath).
func resolvePath(path string) (string, error) {
	return walkPath(path, nil)
}

// walkPath resolves path the way the kernel does, one component at a time:
// a link is followed where it stands, so a ".." after it leaves the link's
// target, and a link to a path that does not exist yet leads where creating
// it would. Components that do not exist resolve as written, since creating
// them makes real folders. step, when set, sees each entry it passes: the
// resolved folder that holds it, and the entry itself, before a link is
// followed.
func walkPath(path string, step func(dir string, entry fs.FileInfo) error) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	pending := strings.Split(path, string(filepath.Separator))
	current, missing, hops := string(filepath.Separator), false, 0
	for len(pending) > 0 {
		name := pending[0]
		pending = pending[1:]
		switch name {
		case "", ".":
			continue
		case "..":
			current = filepath.Dir(current)
			continue
		}
		next := filepath.Join(current, name)
		if missing {
			current = next
			continue
		}
		info, err := os.Lstat(next)
		if errors.Is(err, fs.ErrNotExist) {
			missing, current = true, next
			continue
		}
		if err != nil {
			return "", err
		}
		if step != nil {
			if err := step(current, info); err != nil {
				return "", err
			}
		}
		if info.Mode()&os.ModeSymlink == 0 {
			current = next
			continue
		}
		if hops++; hops > 40 {
			return "", fmt.Errorf("%s: too many symbolic links", path)
		}
		target, err := os.Readlink(next)
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(target) {
			current = string(filepath.Separator)
		}
		pending = append(strings.Split(target, string(filepath.Separator)), pending...)
	}
	return current, nil
}

// within reports whether path is dir or inside it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
