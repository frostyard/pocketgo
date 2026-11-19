// Command pocketd is the headless pocketgo service.
//
//	pocketd serve                  run the service (the default)
//	pocketd device add NAME        sign a device in; prints its token once
//	pocketd device list            list devices
//	pocketd device revoke NAME     revoke a device's token
//	pocketd login PROVIDER         sign in to a subscription (anthropic, openai-codex, github-copilot)
//	pocketd models                 list the models the sign-ins can run
//	pocketd profiles               list the agents' profiles, with the model each runs or its problem
//	pocketd version                print the version
//
// Every command takes -data DIR (default $POCKETD_DATA, else
// ~/.local/state/pocketgo); serve also takes -listen ADDR (default
// $POCKETD_LISTEN, else 127.0.0.1:8788). serve and profiles take -profiles
// DIR, the agents' profile folder (default $POCKETD_PROFILES, else
// ~/.config/pi-pocket-team/agents, ADR-0013). serve takes -desk-cap N, the
// most threads not closed (default $POCKETD_DESK_CAP, else 12, ADR-0014),
// and -pr-every D, how often the open pull requests are read on GitHub
// again (default 5m, 0 for never, ADR-0016).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/api"
	"github.com/frostyard/pocketgo/service/internal/approvals"
	"github.com/frostyard/pocketgo/service/internal/auth"
	"github.com/frostyard/pocketgo/service/internal/events"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/router"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
	"github.com/frostyard/pocketgo/service/pig"
)

// Files in the data folder.
const (
	workFile = "work.sqlite"
	authFile = "auth.json"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "pocketd:", err)
		os.Exit(1)
	}
}

func version() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		revision, dirty := "", ""
		for _, setting := range info.Settings {
			switch {
			case setting.Key == "vcs.revision" && len(setting.Value) >= 12:
				revision = setting.Value[:12]
			case setting.Key == "vcs.modified" && setting.Value == "true":
				dirty = "+dirty"
			}
		}
		if revision != "" {
			return "pocketd " + revision + dirty
		}
		if info.Main.Version != "" {
			return "pocketd " + info.Main.Version
		}
	}
	return "pocketd dev"
}

func defaultData() string {
	if dir := os.Getenv("POCKETD_DATA"); dir != "" {
		return dir
	}
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "pocketgo")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "pocketgo")
}

func defaultProfiles() string {
	if dir := os.Getenv("POCKETD_PROFILES"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "pi-pocket-team", "agents")
}

// deskCap is the desk's cap: -desk-cap when given (flag), else
// $POCKETD_DESK_CAP, else team.DefaultDeskCap. It is at least 1. A flag
// given wins, so an environment value it overrides is never read.
func deskCap(flag string, given bool) (int, error) {
	value, from := flag, "-desk-cap"
	if !given {
		set := false
		value, set = os.LookupEnv("POCKETD_DESK_CAP")
		if !set {
			return team.DefaultDeskCap, nil
		}
		from = "POCKETD_DESK_CAP"
	}
	n, err := strconv.Atoi(value)
	switch {
	case err != nil:
		return 0, fmt.Errorf("%s %q is not a number", from, value)
	case n < 1:
		return 0, fmt.Errorf("%s is %d: the desk holds at least 1 thread", from, n)
	}
	return n, nil
}

func defaultListen() string {
	if addr := os.Getenv("POCKETD_LISTEN"); addr != "" {
		return addr
	}
	return "127.0.0.1:8788"
}

// commands lists each command and how many arguments it takes after its
// name (-1: checked by the command).
var commands = map[string]int{"serve": 0, "version": 0, "models": 0, "profiles": 0, "login": 1, "device": -1}

// parseCommand reads the flags wherever they are and returns the command
// (serve when none is named) and its arguments.
func parseCommand(flags *flag.FlagSet, args []string) (string, []string, error) {
	positional, err := parseInterleaved(flags, args)
	if err != nil {
		return "", nil, err
	}
	command := "serve"
	if len(positional) > 0 {
		command, positional = positional[0], positional[1:]
	}
	want, ok := commands[command]
	if !ok {
		return "", nil, fmt.Errorf("unknown command %q", command)
	}
	if want >= 0 && len(positional) != want {
		return "", nil, fmt.Errorf("pocketd %s takes %d argument(s), got %q", command, want, positional)
	}
	return command, positional, nil
}

func run(args []string) error {
	flags := flag.NewFlagSet("pocketd", flag.ContinueOnError)
	data := flags.String("data", defaultData(), "data folder")
	listen := flags.String("listen", defaultListen(), "address to listen on (serve)")
	profileDir := flags.String("profiles", defaultProfiles(), "the agents' profile folder (serve, profiles)")
	capFlag := flags.String("desk-cap", "", "the most threads not closed (serve; default $POCKETD_DESK_CAP, else 12)")
	prEvery := flags.Duration("pr-every", 5*time.Minute, "how often open pull requests are read on GitHub again (serve; 0 for never)")
	command, rest, err := parseCommand(flags, args)
	if err != nil {
		return err
	}
	if command == "version" {
		fmt.Println(version(), "protocol", proto.Version)
		return nil
	}
	if err := os.MkdirAll(*data, 0o700); err != nil {
		return err
	}
	ctx := context.Background()
	switch command {
	case "serve":
		// The desk's cap is serve's alone: a bad one stops no other command.
		given := false
		flags.Visit(func(f *flag.Flag) { given = given || f.Name == "desk-cap" })
		limit, err := deskCap(*capFlag, given)
		if err != nil {
			return err
		}
		if *prEvery < 0 {
			return fmt.Errorf("-pr-every %v is negative", *prEvery)
		}
		return serve(*data, *listen, *profileDir, limit, *prEvery)
	case "profiles":
		return listProfiles(ctx, os.Stdout, *data, *profileDir)
	case "login":
		return pig.Login(ctx, filepath.Join(*data, authFile), rest[0], os.Stdin, os.Stdout)
	case "models":
		return models(ctx, *data)
	}
	return device(ctx, *data, rest)
}

// parseInterleaved parses flags wherever they appear among the positional
// arguments ("device add NAME -data DIR" as well as "-data DIR device add
// NAME") and returns the positional arguments in order.
func parseInterleaved(flags *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := flags.Parse(args); err != nil {
			return nil, err
		}
		args = flags.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

func device(ctx context.Context, data string, args []string) error {
	usage := errors.New("usage: pocketd device add NAME | list | revoke NAME")
	valid := (len(args) == 2 && (args[0] == "add" || args[0] == "revoke")) || (len(args) == 1 && args[0] == "list")
	if !valid {
		return usage
	}
	s, err := store.Open(ctx, filepath.Join(data, workFile), nil)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	switch {
	case len(args) == 2 && args[0] == "add":
		d, token, err := auth.CreateDevice(ctx, s, args[1])
		if err != nil {
			return err
		}
		fmt.Printf("device %s (%s)\ntoken %s\nThe token is shown only now; give it to the device.\n", d.Name, d.ID, token)
		return nil
	case len(args) == 1 && args[0] == "list":
		devices, err := s.Devices(ctx)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "NAME\tID\tCREATED\tREVOKED")
		for _, d := range devices {
			revoked := ""
			if d.RevokedAt != nil {
				revoked = d.RevokedAt.Format(time.RFC3339)
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", d.Name, d.ID, d.CreatedAt.Format(time.RFC3339), revoked)
		}
		return w.Flush()
	case len(args) == 2 && args[0] == "revoke":
		return s.RevokeDevice(ctx, args[1])
	}
	return usage
}

// models lists the signed-in models from auth.json alone: it never opens the
// durable store, so it cannot resume or change a session.
func models(ctx context.Context, data string) error {
	list, err := pig.ListModels(ctx, filepath.Join(data, authFile))
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("No models: sign in with pocketd login PROVIDER.")
	}
	for _, m := range list {
		fmt.Printf("%s\t%s\n", m.ModelRef, m.Name)
	}
	return nil
}

// profileSource is the server's profile folder: none for an empty
// -profiles, which turns profiles off for the sandbox and the server alike,
// rather than reading the working directory. The folder is verified now,
// after the sandbox protected it, and refused if it is replaced later.
func profileSource(dir, home string) (api.ProfileSource, error) {
	if dir == "" {
		return nil, nil
	}
	folder, err := profiles.Pin(dir, home)
	if err != nil {
		return nil, fmt.Errorf("the profile folder: %w", err)
	}
	return folder, nil
}

// listProfiles prints every profile in dir with the model a session from it
// runs, as the router chooses it, or its problem.
func listProfiles(ctx context.Context, w io.Writer, data, dir string) error {
	if dir == "" {
		_, err := fmt.Fprintln(w, "Profiles are off: -profiles is empty.")
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	valid, problems, err := profiles.Load(dir, home)
	if err != nil {
		return err
	}
	available, err := pig.ListModels(ctx, filepath.Join(data, authFile))
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "PROFILE\tROLE\tMODEL\tRUNS")
	for _, p := range valid {
		runs := ""
		if err := profiles.Startable(p, pig.ToolNames()); err != nil {
			runs = "none: " + err.Error()
		} else if choice, err := router.Choose(router.Request{Profile: &p}, available); err != nil {
			runs = "none: " + err.Error()
		} else {
			runs = choice.Model.String() + " (" + string(choice.Rule) + ")"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", p.ID, p.Role, p.Model, runs)
	}
	for _, p := range problems {
		_, _ = fmt.Fprintf(tw, "%s\tinvalid\t\t%s\n", p.ID, p.Err)
	}
	return tw.Flush()
}

// lockFile is the data folder's lock: one serve process per data folder,
// or Durable tasks would run twice.
const lockFile = "pocketd.lock"

// newSandbox makes the commands' sandbox; tests replace it.
var newSandbox = pig.NewSandbox

func serve(data, listen, profileDir string, deskCap int, prEvery time.Duration) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// One absolute path for the lock, the stores and the sandbox, whatever
	// the working directory (ADR-0012).
	abs, err := filepath.Abs(data)
	if err != nil {
		return err
	}
	data = abs
	unlock, err := lockData(filepath.Join(data, lockFile))
	if err != nil {
		return err
	}
	defer unlock()
	// Every command a tool starts runs in the sandbox (ADR-0012): without
	// one that works, pocketd does not start.
	sandbox, err := newSandbox(data, profileDir)
	if err != nil {
		return err
	}
	if err := sandbox.Check(ctx); err != nil {
		return err
	}
	// Bind before recovering anything, so a taken port fails first.
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()

	s, err := store.Open(ctx, filepath.Join(data, workFile), nil)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	hub, err := events.NewHub(4096, time.Now(), func(floor, block int64) (int64, error) {
		return s.ReserveSeq(context.Background(), floor, block)
	}, func(err error) { log.Error("reserve event numbers", "err", err) })
	if err != nil {
		return err
	}
	server := api.New(s, hub, version(), nil, log)
	manager := approvals.New(s, server.PublishApproval)
	server.SetApprovals(manager)
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	source, err := profileSource(profileDir, home)
	if err != nil {
		return err
	}
	server.SetProfiles(source)
	server.SetDeskCap(deskCap)
	engine, err := pig.Open(ctx, pig.Options{
		DataDir: data, AuthPath: filepath.Join(data, authFile), Protected: []string{data}, Sandbox: sandbox,
		Approver: manager, NeedsApproval: approvals.NeedsApproval,
		OnEntry: server.PublishEntry, OnLive: server.PublishLive,
	})
	if err != nil {
		return err
	}
	closeEngine := func() {
		closing, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := engine.Close(closing); err != nil {
			log.Error("close engine", "err", err)
		}
	}
	// Wire everything, then let tasks run.
	server.SetEngine(engine)
	engine.SetTeam(server)
	if err := manager.RetireEnded(ctx, 0, engine.TaskEnded); err != nil {
		closeEngine()
		return err
	}
	if err := server.WatchAll(ctx); err != nil {
		closeEngine()
		return err
	}
	engine.Resume()
	// A thread's session started before team_verdict existed gets it
	// (ADR-0017): its specialist may be asked for a review.
	if err := server.UpgradeTeamTools(ctx); err != nil {
		log.Warn("give team_verdict to older sessions", "err", err)
	}
	// A report or an answer that did not reach its session before a stop
	// goes in a pass that starts now and that serving does not wait for.
	// It comes after Resume: a submission may let PiG's scheduler run.
	// The first pass gives every agent's sessions its lessons in force
	// (ADR-0015), then brings what did not reach its session.
	server.RefreshAllLessons()
	server.StartDelivery()
	// The open pull requests are read on GitHub again every prEvery
	// (ADR-0016), in rounds Stop ends.
	server.StartPulls(prEvery)

	httpServer := &http.Server{Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- httpServer.Serve(listener) }()
	log.Info("pocketd serving", "listen", listener.Addr().String(), "data", data, "version", version())
	// systemd (Type=notify) learns that the start order completed.
	if err := sdNotify("READY=1"); err != nil {
		log.Warn("notify systemd", "err", err)
	}

	select {
	case <-ctx.Done():
	case err = <-served:
	}
	log.Info("pocketd stopping")
	_ = sdNotify("STOPPING=1")
	server.Stop() // event streams end, so Shutdown does not wait on them
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdown)
	closeEngine()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

var _ api.Engine = (*pig.Engine)(nil)
