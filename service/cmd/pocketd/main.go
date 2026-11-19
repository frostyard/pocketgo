// Command pocketd is the headless pocketgo service.
//
//	pocketd serve                  run the service (the default)
//	pocketd device add NAME        sign a device in; prints its token once
//	pocketd device list            list devices
//	pocketd device revoke NAME     revoke a device's token
//	pocketd login PROVIDER         sign in to a subscription (anthropic, openai-codex, github-copilot)
//	pocketd models                 list the models the sign-ins can run
//	pocketd version                print the version
//
// Every command takes -data DIR (default $POCKETD_DATA, else
// ~/.local/state/pocketgo); serve also takes -listen ADDR (default
// $POCKETD_LISTEN, else 127.0.0.1:8788).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/api"
	"github.com/frostyard/pocketgo/service/internal/approvals"
	"github.com/frostyard/pocketgo/service/internal/auth"
	"github.com/frostyard/pocketgo/service/internal/events"
	"github.com/frostyard/pocketgo/service/internal/store"
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

func defaultListen() string {
	if addr := os.Getenv("POCKETD_LISTEN"); addr != "" {
		return addr
	}
	return "127.0.0.1:8788"
}

func run(args []string) error {
	command := "serve"
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		command, args = args[0], args[1:]
	}
	flags := flag.NewFlagSet("pocketd "+command, flag.ContinueOnError)
	data := flags.String("data", defaultData(), "data folder")
	listen := flags.String("listen", defaultListen(), "address to listen on (serve)")
	rest, err := parseInterleaved(flags, args)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*data, 0o700); err != nil {
		return err
	}
	ctx := context.Background()
	switch command {
	case "serve":
		return serve(*data, *listen)
	case "version":
		fmt.Println(version(), "protocol", proto.Version)
		return nil
	case "login":
		if len(rest) != 1 {
			return errors.New("usage: pocketd login PROVIDER (anthropic, openai-codex, github-copilot)")
		}
		return pig.Login(ctx, filepath.Join(*data, authFile), rest[0], os.Stdin, os.Stdout)
	case "models":
		return models(ctx, *data)
	case "device":
		return device(ctx, *data, rest)
	}
	return fmt.Errorf("unknown command %q", command)
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

// noApprovals is the approver of the read-only models command, which runs no
// tool.
type noApprovals struct{}

func (noApprovals) Await(context.Context, approvals.Call) (approvals.Decision, error) {
	return approvals.Decision{}, errors.New("no approvals in this command")
}

func models(ctx context.Context, data string) error {
	engine, err := pig.Open(ctx, pig.Options{
		DataDir: data, AuthPath: filepath.Join(data, authFile),
		Approver: noApprovals{}, NeedsApproval: approvals.NeedsApproval,
	})
	if err != nil {
		return err
	}
	defer func() { _ = engine.Close(context.Background()) }()
	list, err := engine.Models(ctx)
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

func serve(data, listen string) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	s, err := store.Open(ctx, filepath.Join(data, workFile), nil)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	hub := events.NewHub(4096, time.Now())
	server := api.New(s, hub, version(), nil, log)
	manager := approvals.New(s, server.PublishApproval)
	server.SetApprovals(manager)
	engine, err := pig.Open(ctx, pig.Options{
		DataDir: data, AuthPath: filepath.Join(data, authFile),
		Approver: manager, NeedsApproval: approvals.NeedsApproval,
		OnEntry: server.PublishEntry, OnLive: server.PublishLive,
	})
	if err != nil {
		return err
	}
	server.SetEngine(engine)
	if err := server.WatchAll(ctx); err != nil {
		_ = engine.Close(context.Background())
		return err
	}

	listener, err := net.Listen("tcp", listen)
	if err != nil {
		_ = engine.Close(context.Background())
		return err
	}
	httpServer := &http.Server{Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- httpServer.Serve(listener) }()
	log.Info("pocketd serving", "listen", listener.Addr().String(), "data", data, "version", version())

	select {
	case <-ctx.Done():
	case err = <-served:
	}
	log.Info("pocketd stopping")
	shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdown)
	if closeErr := engine.Close(shutdown); closeErr != nil {
		log.Error("close engine", "err", closeErr)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

var _ api.Engine = (*pig.Engine)(nil)
