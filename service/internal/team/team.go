// Package team holds the rules of delegation, reports and asks
// (ADR-0014): who may delegate to whom, the desk's cap, the limits on what
// agents send, where a thread works, and the texts pocketd writes for
// them. It decides nothing by model: every rule here has a test.
package team

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// Limits on what agents send (ADR-0014).
const (
	MaxTask        = 6000
	MaxTitle       = 120
	MaxMessage     = 6000
	MaxAsks        = 4
	MaxQuestion    = 500
	MinOptions     = 2
	MaxOptions     = 6
	MaxOption      = 40
	DefaultDeskCap = 12
	// MaxFailure is the most characters of why a thread could not start
	// that its report keeps.
	MaxFailure = 2000
)

// Report statuses.
const (
	StatusWorking = "working"
	StatusDone    = "done"
	StatusBlocked = "blocked"
)

// Delegation is a team_delegate call.
type Delegation struct {
	Agent      string `json:"agent"`
	Task       string `json:"task"`
	Title      string `json:"title,omitempty"`
	Repository string `json:"repository,omitempty"`
}

// CheckDelegation refuses a delegation that breaks a rule. caller is the
// calling session's profile (nil for a session without one); target the
// profile delegated to; startable why no session can start from target
// (nil when one can); onDesk the threads not closed, and cap the desk's
// size.
func CheckDelegation(caller *profiles.Profile, target profiles.Profile, startable error, d Delegation, onDesk, cap int) error {
	if caller == nil || caller.Role != profiles.RoleSupervisor {
		return errors.New("only a supervisor delegates: this session's profile is not one")
	}
	if err := CheckTarget(target, startable); err != nil {
		return err
	}
	switch {
	case strings.TrimSpace(d.Task) == "":
		return errors.New("the task is empty")
	case utf8.RuneCountInString(d.Task) > MaxTask:
		return fmt.Errorf("the task has %d characters, more than %d", utf8.RuneCountInString(d.Task), MaxTask)
	case utf8.RuneCountInString(d.Title) > MaxTitle:
		return fmt.Errorf("the title has %d characters, more than %d", utf8.RuneCountInString(d.Title), MaxTitle)
	case onDesk >= cap:
		return fmt.Errorf("the desk is full: %d threads are not closed, and the cap is %d; close a finished one with team_close", onDesk, cap)
	}
	return CheckDestination(target, d.Repository)
}

// CheckTarget refuses a profile that takes no delegation, whatever the
// task: a private agent, a supervisor, or one no session can start from
// (startable). pocketd checks it when a thread is delegated, and again on
// the profile it starts the specialist's session from.
func CheckTarget(target profiles.Profile, startable error) error {
	switch {
	case target.Private:
		return fmt.Errorf("%s is private and takes no delegation", target.ID)
	case target.Role == profiles.RoleSupervisor:
		return fmt.Errorf("%s is a supervisor and takes no delegation", target.ID)
	}
	return startable
}

// CheckDestination refuses where a thread would work: a repository the
// target's profile does not name, or no repository for a target with no
// home or workspace.
func CheckDestination(target profiles.Profile, repository string) error {
	names := make([]string, 0, len(target.Repositories))
	for name := range target.Repositories {
		names = append(names, name)
	}
	slices.Sort(names)
	switch {
	case repository == "" && target.Start() == "" && len(names) == 0:
		return fmt.Errorf("%s has no home, workspace or repository to work in", target.ID)
	case repository == "" && target.Start() == "":
		return fmt.Errorf("%s has no home or workspace to work in: name one of its repositories, %s", target.ID, strings.Join(names, ", "))
	case repository == "":
		return nil
	case len(names) == 0:
		return fmt.Errorf("%s works on no repository; leave the repository out", target.ID)
	case target.Repositories[repository] == "":
		return fmt.Errorf("%s does not work on %s; its repositories are %s", target.ID, repository, strings.Join(names, ", "))
	}
	return nil
}

// Delegable says whether a supervisor may delegate to p at all, model
// aside: not private, not a supervisor, and with somewhere to work.
func Delegable(p profiles.Profile) bool {
	return CheckTarget(p, nil) == nil && (p.Start() != "" || len(p.Repositories) > 0)
}

// Roster is the part of a supervisor's instructions that names whom it can
// delegate to: every delegable profile no rule stops (startable returns
// nil), with its repositories, by id. capacity is the desk's cap.
func Roster(list []profiles.Profile, startable func(profiles.Profile) error, capacity int) string {
	var b strings.Builder
	b.WriteString("Delegate work with team_delegate: pocketd starts the specialist in a thread, and its reports come back to you as messages that start with [T<thread> · <agent> · <status>]. ")
	fmt.Fprintf(&b, "The desk holds at most %d threads that are not closed; close a finished one with team_close. ", capacity)
	sorted := slices.Clone(list)
	slices.SortFunc(sorted, func(a, b profiles.Profile) int { return strings.Compare(a.ID, b.ID) })
	var lines []string
	for _, p := range sorted {
		if !Delegable(p) || startable(p) != nil {
			continue
		}
		names := make([]string, 0, len(p.Repositories))
		for name := range p.Repositories {
			names = append(names, name)
		}
		slices.Sort(names)
		where := "works in its home"
		if len(names) > 0 {
			where = "repositories " + strings.Join(names, ", ")
		}
		lines = append(lines, fmt.Sprintf("- %s (%s): %s", p.ID, p.Name, where))
	}
	if len(lines) == 0 {
		b.WriteString("No agent can take a delegation now.")
		return b.String()
	}
	b.WriteString("The agents you can delegate to, by id:\n")
	b.WriteString(strings.Join(lines, "\n"))
	return b.String()
}

// Title is the thread's title: the one given, else the task's first line,
// cut to 60 characters.
func Title(d Delegation) string {
	if t := strings.TrimSpace(d.Title); t != "" {
		return t
	}
	line, _, _ := strings.Cut(strings.TrimSpace(d.Task), "\n")
	line = strings.TrimSpace(line)
	if r := []rune(line); len(r) > 60 {
		line = strings.TrimSpace(string(r[:59])) + "…"
	}
	if line == "" {
		return "Task"
	}
	return line
}

// Slug is the thread's title as a folder and branch name: lowercase
// letters and digits joined by dashes, at most 40 characters, then the
// thread's number and six characters of its random owner, so two threads
// never share one, across data folders too, and a name nothing else made
// is the thread's own.
func Slug(title string, thread int64, owner string) string {
	var words []string
	var word strings.Builder
	flush := func() {
		if word.Len() > 0 {
			words = append(words, word.String())
			word.Reset()
		}
	}
	for _, r := range strings.ToLower(title) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			word.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	slug := ""
	for _, w := range words {
		if len(slug)+len(w)+1 > 40 {
			break
		}
		if slug != "" {
			slug += "-"
		}
		slug += w
	}
	if slug == "" {
		slug = "task"
	}
	if len(owner) > 6 {
		owner = owner[:6]
	}
	return slug + "-t" + strconv.FormatInt(thread, 10) + "-" + owner
}

// Worktree is where a thread on a repository works: a git worktree next to
// the checkout, <checkout>--<agent>-<slug>, on the branch <agent>/<slug>.
func Worktree(checkout, agent, slug string) (path, branch string) {
	return filepath.Clean(checkout) + "--" + agent + "-" + slug, agent + "/" + slug
}

// Brief is the first message of a thread's specialist session, written by
// code: who asked, the task, where to work, and how to report.
func Brief(supervisor string, thread int64, title, task, cwd, branch string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s, the team's supervisor, delegated this to you in thread T%d: %s\n\n", supervisor, thread, title)
	b.WriteString(strings.TrimSpace(task))
	b.WriteString("\n\n")
	if branch != "" {
		fmt.Fprintf(&b, "Work in %s, a git worktree on the branch %s. Open a pull request from that branch when the change is ready.\n", cwd, branch)
	} else {
		fmt.Fprintf(&b, "Work in %s.\n", cwd)
	}
	b.WriteString("Report with team_message: status working for progress, done when the work is finished, blocked when you cannot go on. Put each decision you need from the owner in asks; the answers come back to this session.")
	return b.String()
}

// Ask is a question a report puts to the owner.
type Ask struct {
	Question string   `json:"question"`
	Options  []string `json:"options,omitempty"`
}

// Report is a team_message call.
type Report struct {
	Message string `json:"message"`
	Status  string `json:"status"`
	Asks    []Ask  `json:"asks,omitempty"`
}

// CheckReport refuses a report that breaks a rule.
func CheckReport(r Report) error {
	switch r.Status {
	case StatusWorking, StatusDone, StatusBlocked:
	default:
		return fmt.Errorf("status %q is not working, done or blocked", r.Status)
	}
	if strings.TrimSpace(r.Message) == "" {
		return errors.New("the message is empty")
	}
	if n := utf8.RuneCountInString(r.Message); n > MaxMessage {
		return fmt.Errorf("the message has %d characters, more than %d", n, MaxMessage)
	}
	if len(r.Asks) > MaxAsks {
		return fmt.Errorf("%d asks, more than %d", len(r.Asks), MaxAsks)
	}
	for i, a := range r.Asks {
		if strings.TrimSpace(a.Question) == "" {
			return fmt.Errorf("ask %d has no question", i+1)
		}
		if n := utf8.RuneCountInString(a.Question); n > MaxQuestion {
			return fmt.Errorf("ask %d's question has %d characters, more than %d", i+1, n, MaxQuestion)
		}
		if len(a.Options) != 0 && (len(a.Options) < MinOptions || len(a.Options) > MaxOptions) {
			return fmt.Errorf("ask %d has %d options: give none, or %d to %d", i+1, len(a.Options), MinOptions, MaxOptions)
		}
		seen := map[string]bool{}
		for _, o := range a.Options {
			o = strings.TrimSpace(o)
			if o == "" || utf8.RuneCountInString(o) > MaxOption {
				return fmt.Errorf("ask %d has an option that is empty or over %d characters", i+1, MaxOption)
			}
			if seen[o] {
				return fmt.Errorf("ask %d gives the option %q twice", i+1, o)
			}
			seen[o] = true
		}
	}
	return nil
}

// Clip is text cut to at most n characters, with … where it was cut.
func Clip(text string, n int) string {
	if r := []rune(text); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return text
}

// ThreadState is the thread's state after a report of status.
func ThreadState(status string) string {
	switch status {
	case StatusDone:
		return "done"
	case StatusBlocked:
		return "blocked"
	}
	return "open"
}

// ReportText is the message the supervisor's session gets for a report.
func ReportText(thread int64, agent, status, message string, asks []int64) string {
	text := fmt.Sprintf("[T%d · %s · %s] %s", thread, agent, status, strings.TrimSpace(message))
	if len(asks) > 0 {
		ids := make([]string, len(asks))
		for i, id := range asks {
			ids[i] = "A" + strconv.FormatInt(id, 10)
		}
		text += "\n\nAsks waiting for the owner: " + strings.Join(ids, ", ")
	}
	return text
}

// AnswerText is the message the asking session gets for the owner's answer.
func AnswerText(ask int64, question, answer string) string {
	return fmt.Sprintf("The owner answered A%d (%q): %s", ask, question, strings.TrimSpace(answer))
}
