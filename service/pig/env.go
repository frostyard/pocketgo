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

// guardedEnv is an execution environment whose file access refuses the
// protected folders and whose commands get only allowedEnv and run in the
// sandbox. It checks the path PiG will open: AbsolutePath applies the
// environment's own normalization (relative paths, "~", file URLs), then
// symbolic links are resolved, at the moment of access.
type guardedEnv struct {
	env.ExecutionEnv
	protected []string
	commands  map[string]string
	// sandbox runs every command (ADR-0012); nil only in tests that asked
	// for Options.Unsandboxed.
	sandbox *Sandbox
}

func (g *guardedEnv) check(ctx context.Context, path string) error {
	absolute, err := g.AbsolutePath(ctx, path)
	if err != nil {
		return err
	}
	resolved, err := resolvePath(absolute)
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

// checkTree is check for a call that moves or removes path with everything
// below it: it also refuses a folder that holds a protected path, so a
// rename or a removal cannot take a protected path along.
func (g *guardedEnv) checkTree(ctx context.Context, path string) error {
	if err := g.check(ctx, path); err != nil {
		return err
	}
	absolute, err := g.AbsolutePath(ctx, path)
	if err != nil {
		return err
	}
	resolved, err := resolvePath(absolute)
	if err != nil {
		return err
	}
	for _, dir := range g.protected {
		if within(dir, resolved) {
			return errProtected
		}
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
	if err := g.check(ctx, path); err != nil {
		return err
	}
	return g.ExecutionEnv.WriteFile(ctx, path, content)
}

func (g *guardedEnv) AppendFile(ctx context.Context, path string, content any) error {
	if err := g.check(ctx, path); err != nil {
		return err
	}
	return g.ExecutionEnv.AppendFile(ctx, path, content)
}

func (g *guardedEnv) TruncateFile(ctx context.Context, path string, size int64) error {
	if err := g.check(ctx, path); err != nil {
		return err
	}
	return g.ExecutionEnv.TruncateFile(ctx, path, size)
}

func (g *guardedEnv) FlushFile(ctx context.Context, path string) error {
	if err := g.check(ctx, path); err != nil {
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
	if err := g.check(ctx, path); err != nil {
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
		wrapped, err := g.sandbox.Command(command)
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
