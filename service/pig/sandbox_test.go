package pig

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/MichaelKinsy/PiG/durable/env"
	envnode "github.com/MichaelKinsy/PiG/durable/env/node"
	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/approvals"
)

func TestSandboxCommandLine(t *testing.T) {
	s := &Sandbox{Bwrap: "/usr/bin/bwrap", Shell: "/bin/bash", Hide: []string{"/data", "/run/user/7"}, ReadOnly: []string{"/bin/pocketd"}}
	prefix := []string{"/usr/bin/bwrap", "--dev-bind", "/", "/", "--unshare-pid", "--proc", "/proc", "--die-with-parent",
		"--ro-bind", "/bin/pocketd", "/bin/pocketd", "--tmpfs", "/data", "--tmpfs", "/run/user/7", "--"}
	for _, tc := range []struct {
		command any
		want    []string
	}{
		{"cat x | wc -l", append(slices.Clone(prefix), "/bin/bash", "-c", "cat x | wc -l")},
		{[]string{"git", "status", "--short"}, append(slices.Clone(prefix), "git", "status", "--short")},
	} {
		got, err := s.Command(tc.command)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("Command(%q) = %q, %v\nwant %q", tc.command, got, err, tc.want)
		}
	}
	for _, bad := range []any{[]string{}, 42, nil} {
		if got, err := s.Command(bad); err == nil {
			t.Errorf("Command(%#v) = %q, want an error", bad, got)
		}
	}
}

// The sandbox hides the data folder and the runtime folder, binds what
// decides pocketd's next start read-only, and creates the configuration
// paths a command could otherwise create first.
func TestNewSandboxNamesPocketdsFiles(t *testing.T) {
	root := t.TempDir()
	home, data, runtime := filepath.Join(root, "home"), filepath.Join(root, "data"), filepath.Join(root, "run")
	program := filepath.Join(root, "bin", "pocketd")
	for _, dir := range []string{home, data, runtime, filepath.Dir(program)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(program, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := newSandbox("/usr/bin/bwrap", "/bin/bash", data, home, runtime, program)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{data, runtime}; !slices.Equal(s.Hide, want) {
		t.Errorf("Hide %q, want %q", s.Hide, want)
	}
	envFile := filepath.Join(home, ".config", "pocketd.env")
	want := []string{program, envFile,
		filepath.Join(home, ".config", "systemd"),
		filepath.Join(home, ".local", "share", "systemd"),
		filepath.Join(home, ".config", "environment.d")}
	if !slices.Equal(s.ReadOnly, want) {
		t.Errorf("ReadOnly %q, want %q", s.ReadOnly, want)
	}
	for _, path := range want[1:] {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was not created: %v", path, err)
		}
	}

	// A runtime folder that does not exist has nothing to hide.
	s, err = newSandbox("/usr/bin/bwrap", "/bin/bash", data, home, filepath.Join(root, "absent"), program)
	if err != nil || !slices.Equal(s.Hide, []string{data}) {
		t.Errorf("Hide %q, %v, want only the data folder", s.Hide, err)
	}
}

func TestOpenRequiresASandbox(t *testing.T) {
	f := newFixture(t)
	_, err := open(context.Background(), Options{DataDir: f.dataDir, Approver: f.approve, NeedsApproval: approveChanges}, fauxModels())
	if err == nil || !strings.Contains(err.Error(), "Sandbox") {
		t.Fatalf("open without a sandbox: %v", err)
	}
}

// usableSandbox is a sandbox for data on this host, with its home and
// runtime folders under root, or a skip where bwrap cannot run.
func usableSandbox(t *testing.T, root, data string) *Sandbox {
	t.Helper()
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("bwrap is not installed")
	}
	runtime := filepath.Join(root, "run")
	if err := os.MkdirAll(runtime, 0o700); err != nil {
		t.Fatal(err)
	}
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s, err := newSandbox(bwrap, shell(), data, filepath.Join(root, "home"), runtime, program)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Check(context.Background()); err != nil {
		t.Skipf("bwrap cannot run here (user namespaces may be off): %v", err)
	}
	return s
}

// A command in the sandbox cannot see the data folder, its read-only paths,
// or pocketd's processes, and still runs in its working directory.
func TestCommandsCannotReachPocketdsFiles(t *testing.T) {
	root, work := t.TempDir(), t.TempDir()
	data := filepath.Join(root, "data")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "auth.json"), []byte(`{"anthropic":"sk-secret-value"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := usableSandbox(t, root, data)
	g := &guardedEnv{ExecutionEnv: envnode.NewNodeExecutionEnv(envnode.NodeExecutionEnvOptions{Cwd: work}), commands: allowedEnv(), sandbox: s}
	run := func(command any) (string, int) {
		t.Helper()
		var mu sync.Mutex
		var out strings.Builder
		result, err := g.Exec(context.Background(), command, &env.ShellExecOptions{OnOutput: func(_ context.Context, text string, _ env.ShellOutputInfo) {
			mu.Lock()
			out.WriteString(text)
			mu.Unlock()
		}})
		if err != nil {
			t.Fatalf("Exec(%q): %v", command, err)
		}
		return out.String(), result.ExitCode
	}

	envFile := filepath.Join(root, "home", ".config", "pocketd.env")
	out, _ := run(`cat "` + data + `/auth.json"; echo "files=$(ls -A "` + data + `" | wc -l)"; pwd; ` +
		`echo "init=$(cat /proc/1/comm)"; echo POCKETD_LISTEN=0.0.0.0:8788 >> "` + envFile + `" && echo wrote-env`)
	// Process 1 is bwrap's, not the host's init: a new process namespace.
	for _, want := range []string{"No such file", "files=0", work + "\n", "init=bwrap\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	for _, leak := range []string{"sk-secret-value", "wrote-env"} {
		if strings.Contains(out, leak) {
			t.Errorf("output has %q:\n%s", leak, out)
		}
	}
	if b, err := os.ReadFile(envFile); err != nil || len(b) != 0 {
		t.Errorf("the env file changed: %q, %v", b, err)
	}

	// An argv runs in the sandbox too.
	if out, code := run([]string{"cat", filepath.Join(data, "auth.json")}); code == 0 || strings.Contains(out, "sk-secret-value") {
		t.Errorf("argv cat exited %d with %q", code, out)
	}
}

// With pocketd's rule, a session's bash call runs at once, without an
// approval, and in the sandbox.
func TestABashCallRunsSandboxedWithoutWaiting(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(filepath.Join(f.dataDir, "auth.json"), []byte(`{"anthropic":"sk-secret-value"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := usableSandbox(t, t.TempDir(), f.dataDir)
	e, err := open(context.Background(), Options{
		DataDir: f.dataDir, Protected: []string{f.dataDir}, Approver: f.approve,
		NeedsApproval: approvals.NeedsApproval, Sandbox: s,
	}, fauxModels(toolTurn("bash", map[string]any{"command": "cat " + filepath.Join(f.dataDir, "auth.json")}, "b1"), answer("done")))
	if err != nil {
		t.Fatal(err)
	}
	e.Resume()
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	id, err := e.CreateSession(ctx, t.Name(), f.work, fauxRef, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Send(ctx, id, "u:dev:cat", "read the sign-ins"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, e, id)
	select {
	case a := <-f.requests:
		t.Fatalf("the call waited for an approval: %+v", a)
	default:
	}
	entries, _, _ := e.Transcript(ctx, id)
	var result *proto.ToolResult
	for _, entry := range entries {
		if entry.ToolResult != nil {
			result = entry.ToolResult
		}
	}
	if result == nil || strings.Contains(result.Text, "sk-secret-value") || !strings.Contains(result.Text, "No such file") {
		t.Fatalf("bash result %+v", result)
	}
}
