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
// again (default 5m, 0 for never, ADR-0016). serve sends the owner's
// schedules, schedules.json in the profile folder, and the supervisors'
// own (ADR-0030); a clock time with no zone is in the service's, TZ when
// set.
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
	"slices"
	"strconv"
	"strings"
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
	"github.com/frostyard/pocketgo/service/internal/schedule"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
	"github.com/frostyard/pocketgo/service/internal/uploads"
	"github.com/frostyard/pocketgo/service/pig"
)

// Files in the data folder.
const (
	workFile = "work.sqlite"
	authFile = "auth.json"
)

func main() {
	// Engine.ReadUpTo runs pocketd itself in the sandbox to read a file
	// (pig.ReadHelper); it never reaches the commands below.
	if len(os.Args) > 1 && os.Args[1] == pig.ReadHelper {
		os.Exit(pig.RunReadHelper(os.Args[2:], os.Stdout))
	}
	// Engine.ReadIn runs it there to read a file of a session's checkout,
	// through no link (pig.ReadBeneathHelper, ADR-0053).
	if len(os.Args) > 1 && os.Args[1] == pig.ReadBeneathHelper {
		os.Exit(pig.RunReadBeneathHelper(os.Args[2:], os.Stdout))
	}
	// Engine.loadContext runs it there to read the files a session's
	// instructions hold (pig.ContextHelper, ADR-0043).
	if len(os.Args) > 1 && os.Args[1] == pig.ContextHelper {
		os.Exit(pig.RunContextHelper(os.Args[2:], os.Stdout))
	}
	// Every command of the sandbox starts through pocketd itself, which
	// checks the sandbox's mounts first (pig.GateHelper, ADR-0035).
	if len(os.Args) > 1 && os.Args[1] == pig.GateHelper {
		os.Exit(pig.RunGate(os.Args[2:], os.Stderr))
	}
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

// defaultSkills is the skills folder: $POCKETD_SKILLS, else the service
// user's ~/.pi/agent/skills, where the owner's configuration puts them
// (ADR-0033).
func defaultSkills() string {
	if dir := os.Getenv("POCKETD_SKILLS"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".pi", "agent", "skills")
}

// defaultInstructions is the owner's global AGENTS.md, which every
// session's instructions hold: $POCKETD_INSTRUCTIONS, else the service
// user's ~/.pi/agent/AGENTS.md, where the owner's configuration puts it
// (ADR-0043).
func defaultInstructions() string {
	if path := os.Getenv("POCKETD_INSTRUCTIONS"); path != "" {
		return path
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".pi", "agent", "AGENTS.md")
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

// serviceZone is the service's time zone, by its IANA name where the host
// says one (ADR-0030): TZ, else the zone /etc/localtime links to, else Go's
// Local. A schedule that names no zone reads its clock times in it.
func serviceZone() *time.Location {
	return zoneOf(os.LookupEnv("TZ"))(os.Readlink("/etc/localtime"))
}

// zoneOf is serviceZone from TZ, as looked up, then from the link
// /etc/localtime makes, as read.
func zoneOf(tz string, set bool) func(link string, err error) *time.Location {
	return func(link string, err error) *time.Location {
		if set {
			name := strings.TrimPrefix(tz, ":")
			if name == "" {
				return time.UTC
			}
			// A zone's file, as /usr/share/zoneinfo/Europe/Berlin, is that
			// zone by its name, which a schedule's record loads again
			// (Codex review of #51, round 1).
			if strings.HasPrefix(name, "/") {
				if zone := zoneFile(name); zone != nil {
					return zone
				}
				return time.Local
			}
			if zone, err := time.LoadLocation(name); err == nil {
				return zone
			}
			return time.Local
		}
		if err == nil {
			if zone := zoneFile(link); zone != nil {
				return zone
			}
		}
		return time.Local
	}
}

// zoneFile is the zone a path into a zone database names, by its name:
// nil for another path.
func zoneFile(path string) *time.Location {
	if _, name, ok := strings.Cut(path, "zoneinfo/"); ok {
		if zone, err := time.LoadLocation(name); err == nil {
			return zone
		}
	}
	return nil
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
	skillsDir := flags.String("skills", defaultSkills(), "the skills folder a gather reads a skill from, which every session lists (serve, profiles)")
	instructionsFile := flags.String("instructions", defaultInstructions(), "the owner's global AGENTS.md, which every session's instructions hold; \"\" for none (serve)")
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
		return serve(*data, *listen, *profileDir, *skillsDir, *instructionsFile, limit, *prEvery)
	case "profiles":
		return listProfiles(ctx, os.Stdout, *data, *profileDir, *skillsDir)
	case "login":
		// The owner's providers matter only to a sign-in of one of
		// theirs: a broken file never stops a built-in provider's.
		own, err := ownProviders(*profileDir)
		if err != nil && !pig.Builtin(rest[0]) {
			return err
		}
		// The data folder's lock, which needs pocketd stopped, only for a
		// key that is stored: a provider that takes none is refused at once
		// (Codex review of #34, round 3).
		if pig.TakesAPIKey(rest[0]) || slices.ContainsFunc(own.Providers, func(p profiles.Provider) bool { return p.ID == rest[0] && p.Key }) {
			return loginKey(ctx, *data, rest[0], own.Providers, os.Stdin, os.Stdout)
		}
		return pig.Login(ctx, filepath.Join(*data, authFile), rest[0], own.Providers, os.Stdin, os.Stdout)
	case "models":
		return models(ctx, os.Stdout, *data, *profileDir)
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

// loginKey stores an API key for provider only while no pocketd serves the
// data folder: the running service keeps its own copy of the sign-ins, and
// writes them as it refreshes a subscription's (ADR-0021, ADR-0022).
func loginKey(ctx context.Context, data, provider string, own []profiles.Provider, in io.Reader, out io.Writer) error {
	abs, err := filepath.Abs(data)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return err
	}
	unlock, err := lockData(filepath.Join(abs, lockFile))
	if err != nil {
		return fmt.Errorf("stop pocketd before you store a key, then start it again: %w", err)
	}
	defer unlock()
	return pig.Login(ctx, filepath.Join(abs, authFile), provider, own, in, out)
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

// models lists the signed-in models from auth.json and the owner's
// providers alone: it never opens the durable store, so it cannot resume or
// change a session.
func models(ctx context.Context, w io.Writer, data, profileDir string) error {
	own, ownErr := ownProviders(profileDir)
	list, notes, err := pig.ListModels(ctx, filepath.Join(data, authFile), own)
	if err != nil {
		return err
	}
	// The first write that fails, from the first row on, is the command's
	// error (Codex review of #34, round 3).
	report := &firstError{w: w}
	if len(list) == 0 {
		_, _ = fmt.Fprintln(report, "No models: sign in with pocketd login PROVIDER.")
	}
	for _, m := range list {
		_, _ = fmt.Fprintf(report, "%s\t%s\n", m.ModelRef, m.Name)
	}
	return ownReport(report, own, ownErr, notes)
}

// ownReport says what the owner's providers file added, or why nothing,
// and how old PiG's catalog is, which it adds to (ADR-0023).
func ownReport(out io.Writer, own profiles.Own, ownErr error, notes []string) error {
	w, ok := out.(*firstError)
	if !ok {
		w = &firstError{w: out}
	}
	if at := pig.CatalogDate(); at != "" {
		_, _ = fmt.Fprintf(w, "PiG's catalog: %s; add newer models in %s, under builtinModels\n", at, profiles.ProvidersFile)
	}
	if ownErr != nil {
		_, _ = fmt.Fprintf(w, "The owner's providers: none registered: %v\n", ownErr)
		return w.err
	}
	if len(own.Providers) > 0 {
		ids := make([]string, len(own.Providers))
		for i, p := range own.Providers {
			ids[i] = p.ID
		}
		_, _ = fmt.Fprintf(w, "The owner's providers (%s): %s\n", profiles.ProvidersFile, strings.Join(ids, ", "))
	}
	for _, note := range notes {
		_, _ = fmt.Fprintf(w, "Not added: %s\n", note)
	}
	return w.err
}

// ownProviders are the owner's own model providers in the profile folder
// dir (ADR-0023): none without a folder.
func ownProviders(dir string) (profiles.Own, error) {
	if dir == "" {
		return profiles.Own{}, nil
	}
	return profiles.LoadProviders(dir, pig.Builtin)
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
func listProfiles(ctx context.Context, w io.Writer, data, dir, skills string) error {
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
	own, ownErr := ownProviders(dir)
	available, notes, err := pig.ListModels(ctx, filepath.Join(data, authFile), own)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "PROFILE\tROLE\tMODEL\tRUNS")
	for _, p := range valid {
		runs := ""
		if err := profiles.Startable(p, pig.ToolNames()); err != nil {
			runs = "none: " + err.Error()
		} else if err := profiles.CheckStandingFolder(p); err != nil {
			// A new standing session starts in its profile's folder
			// (ADR-0026).
			runs = "none: " + err.Error()
		} else if choice, err := router.Choose(router.Request{Profile: &p}, available); err != nil {
			runs = "none: " + err.Error()
		} else if err := profiles.CheckCompactAt(p, router.Window(available, choice.Model)); err != nil {
			runs = "none: " + err.Error()
		} else {
			runs = choice.Model.String() + " (" + string(choice.Rule) + ")"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", p.ID, p.Role, p.Model, runs)
	}
	for _, p := range problems {
		_, _ = fmt.Fprintf(tw, "%s\tinvalid\t\t%s\n", p.ID, p.Err)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	// The owner's worker list, and the model a worker runs now when the
	// supervisor asks for none and its profile names none (ADR-0022).
	// Each is reported whatever the other says (Codex review of #34), and
	// the first write that fails is the command's error (round 2).
	report := &firstError{w: w}
	if list, err := profiles.LoadWorkers(dir); err != nil {
		_, _ = fmt.Fprintf(report, "\nWorkers: none start: %v\n", err)
	} else {
		from := profiles.WorkersFile
		if list.Default {
			from = "the default: no " + profiles.WorkersFile
		}
		runs := ""
		if choice, err := router.Choose(router.Request{Worker: true, Workers: list}, available); err != nil {
			runs = "none: " + err.Error()
		} else {
			runs = choice.Model.String() + " (" + string(choice.Rule) + ")"
		}
		_, _ = fmt.Fprintf(report, "\nWorkers (%s): %s\nA worker runs: %s\n", from, list, runs)
	}
	// The owner's schedules, with when each goes out or why it does not
	// (ADR-0030).
	scheduleReport(report, dir, skills, valid, serviceZone())
	// The owner's saved prompts, which the desktop's composer inserts
	// (ADR-0051).
	promptReport(report, dir)
	// The owner's providers and models, which the profiles and the list
	// may name (ADR-0023).
	return ownReport(report, own, ownErr, notes)
}

// scheduleReport says what the owner's schedules file holds: each entry
// with when it goes out and the agent whose standing session it goes to,
// or why it does not, as pocketd serve reads it (ADR-0030).
func scheduleReport(w io.Writer, dir, skills string, valid []profiles.Profile, zone *time.Location) {
	list, err := profiles.LoadSchedules(dir)
	switch {
	case err != nil:
		_, _ = fmt.Fprintf(w, "\nSchedules: none go out: %v\n", err)
		return
	case len(list.Entries) == 0:
		_, _ = fmt.Fprintf(w, "\nSchedules: none (%s)\n", profiles.SchedulesFile)
		return
	}
	_, _ = fmt.Fprintf(w, "\nSchedules (%s; clock times with no zone are in %s):\n", profiles.SchedulesFile, zone)
	for _, e := range list.Entries {
		id := e.ID
		if id == "" {
			id = fmt.Sprintf("schedule %d", e.N)
		}
		if e.Problem != "" {
			_, _ = fmt.Fprintf(w, "  %s: does not go out: %s\n", id, e.Problem)
			continue
		}
		at, err := schedule.Zone(e.Zone, zone)
		if err != nil {
			_, _ = fmt.Fprintf(w, "  %s: does not go out: %v\n", id, err)
			continue
		}
		when := e.When
		if e.Spec.Repeat != nil {
			when = e.Spec.Repeat.String()
		}
		// A gather's skill is read from the skills folder at its time
		// (ADR-0033), as the agents' sandbox shows it, which this report,
		// reading with its own rights, does not check (Codex review of #57,
		// round 2); a gather is a supervisor's.
		var skillErr error
		if e.Gather != nil {
			_, skillErr = profiles.LoadSkill(skills, e.Gather.Skill)
		}
		i := slices.IndexFunc(valid, func(p profiles.Profile) bool { return p.ID == e.Profile })
		switch {
		case i < 0:
			_, _ = fmt.Fprintf(w, "  %s: %s (%s): does not go out: no valid profile %s\n", id, when, at, e.Profile)
		case !valid[i].Standing:
			_, _ = fmt.Fprintf(w, "  %s: %s (%s): does not go out: profile %s keeps no standing session\n", id, when, at, e.Profile)
		case e.Gather != nil && valid[i].Role != profiles.RoleSupervisor:
			_, _ = fmt.Fprintf(w, "  %s: %s (%s): does not go out: profile %s is not a supervisor: only a supervisor gathers proposals\n", id, when, at, e.Profile)
		case skillErr != nil:
			_, _ = fmt.Fprintf(w, "  %s: %s (%s): does not go out: %v\n", id, when, at, skillErr)
		case e.Gather != nil:
			_, _ = fmt.Fprintf(w, "  %s: %s (%s), a gather with the skill %s for %s (found here; the service reads it as the agents' sandbox shows it, which this report does not check)\n",
				id, when, at, e.Gather.Skill, valid[i].Name)
		default:
			_, _ = fmt.Fprintf(w, "  %s: %s (%s), to %s's standing session\n", id, when, at, valid[i].Name)
		}
	}
}

// promptReport says what the owner's prompts folder holds: each prompt by
// its name, with its argument hint and description, or why a file is
// none, as GET /v1/prompts lists them (ADR-0051).
func promptReport(w io.Writer, dir string) {
	list, err := profiles.LoadPrompts(dir)
	switch {
	case err != nil:
		_, _ = fmt.Fprintf(w, "\nPrompts: none show: %v\n", err)
		return
	case len(list.List) == 0:
		_, _ = fmt.Fprintf(w, "\nPrompts: none (%s/)\n", profiles.PromptsFolder)
		return
	}
	_, _ = fmt.Fprintf(w, "\nPrompts (%s/):\n", profiles.PromptsFolder)
	for _, p := range list.List {
		if p.Problem != "" {
			// An invalid name is the file's as it is, which may hold a line
			// break or a terminal's control: quoted, it stays on its line
			// (Codex review of #108, round 1). The problem quotes what it
			// names.
			_, _ = fmt.Fprintf(w, "  %q: invalid: %s\n", "/"+p.Name, p.Problem)
			continue
		}
		line := "/" + p.Name
		if p.ArgumentHint != "" {
			line += " " + p.ArgumentHint
		}
		if p.Description != "" {
			line += ": " + p.Description
		}
		_, _ = fmt.Fprintf(w, "  %s\n", line)
	}
	if list.Problem != "" {
		_, _ = fmt.Fprintf(w, "  %s\n", list.Problem)
	}
}

// firstError writes to w until a write fails, and keeps that failure: a
// report cut short is an error, not a success.
type firstError struct {
	w   io.Writer
	err error
}

func (f *firstError) Write(p []byte) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	n, err := f.w.Write(p)
	f.err = err
	return n, err
}

// lockFile is the data folder's lock: one serve process per data folder,
// or Durable tasks would run twice.
const lockFile = "pocketd.lock"

// newSandbox makes the commands' sandbox; tests replace it.
var newSandbox = pig.NewSandbox

func serve(data, listen, profileDir, skillsDir, instructionsFile string, deskCap int, prEvery time.Duration) error {
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
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	// The owner's uploads live outside the data folder, where the agents'
	// sandbox shows them (ADR-0029), read-only, as the skills folder a
	// gather reads is (ADR-0035).
	uploadsDir := uploads.Dir(home)
	// The owner's global AGENTS.md, which every session's instructions
	// hold (ADR-0043): one absolute path, whatever the working directory.
	if instructionsFile != "" {
		if instructionsFile, err = filepath.Abs(instructionsFile); err != nil {
			return err
		}
	}
	// Every command a tool starts runs in the sandbox (ADR-0012): without
	// one that works, pocketd does not start. It makes the skills folder
	// and the uploads folder when they are missing, empty, so a host
	// without them starts, and no agent can make one first; and it names
	// each as the folder it keeps, links resolved, which the gathers and
	// the uploads use from then on. It keeps the owner's global
	// instructions file read-only too, makes it empty when missing, and
	// names it as the file it keeps, which every session reads (ADR-0045).
	sandbox, err := newSandbox(data, profileDir, []*string{&instructionsFile}, &skillsDir, &uploadsDir)
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
	source, err := profileSource(profileDir, home)
	if err != nil {
		return err
	}
	server.SetProfiles(source)
	server.SetDeskCap(deskCap)
	// A data folder that holds the uploads folder hides it from the agents.
	if sandbox.Hides(uploadsDir) {
		log.Warn("the sandbox hides the uploads folder: agents cannot open the files attached to messages", "folder", uploadsDir)
	}
	// A file is staged where no agent reaches, in the data folder, when the
	// file system makes no unnamed file.
	server.SetUploads(uploads.New(uploadsDir, filepath.Join(data, "upload-staging"), nil))
	// A schedule that names no zone reads its clock times in the service's
	// (ADR-0030).
	server.SetZone(serviceZone())
	// A gather reads its skill from the skills folder (ADR-0033).
	server.SetSkills(skillsDir)
	// The owner's providers (ADR-0023), from the folder pinned above, not
	// one put at its name since (Codex review of #34): a file that breaks
	// a rule registers none of them, and pocketd serves without them.
	var own profiles.Own
	if folder, ok := source.(profiles.Folder); ok {
		if own, err = folder.Providers(pig.Builtin); err != nil {
			log.Error("the owner's providers are not registered", "err", err)
			own = profiles.Own{}
		}
	}
	// Every session's instructions hold the owner's global AGENTS.md and
	// list the skills of the skills folder, both as the sandbox above keeps
	// them (ADR-0043, ADR-0045).
	engine, err := pig.Open(ctx, pig.Options{
		DataDir: data, AuthPath: filepath.Join(data, authFile), Protected: []string{data}, Sandbox: sandbox, Own: own,
		Instructions: instructionsFile, Skills: skillsDir,
		Approver: manager, NeedsApproval: approvals.NeedsApproval,
		OnEntry: server.PublishEntry, OnLive: server.PublishLive,
	})
	if err != nil {
		return err
	}
	for _, note := range engine.Notes() {
		log.Warn("the owner's model is not added", "why", note)
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
	// A question whose task ended while pocketd stopped is skipped, as
	// such an approval is denied (ADR-0048); one whose task runs again
	// shows again.
	if err := server.RetireQuestions(ctx); err != nil {
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
		log.Warn("give the later team tools to older sessions", "err", err)
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
	// The owner's schedules and the supervisors' own go out from now
	// (ADR-0030): a time missed while pocketd stopped goes out once. The
	// pass files messages a delivery pass submits, so it starts after
	// Resume, and serving does not wait for it.
	server.StartSchedules()
	// The launches go on (ADR-0034): a launch's items start from its
	// standing session, and a start a stop interrupted finishes, after
	// Resume, as the schedules do; serving does not wait for it.
	server.StartLaunches()
	// The search index reads every session's entries after its cursor,
	// then each one an entry's event notes (ADR-0050): it reads PiG and
	// submits nothing, and serving does not wait for it, as the first
	// start with search reads every transcript.
	server.StartIndex()

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
