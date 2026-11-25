package team

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// An agent's own work session (ADR-0073): team_work starts a session of
// the caller's profile in a fresh worktree of one of its repositories,
// where that repository's own AGENTS.md and skills load (ADR-0043), and
// each run of it sends its answer back to the calling session through the
// outbox.

// Work is a team_work call.
type Work struct {
	// Repository is one of the caller's profile's repositories, as the
	// profile names it, in any case; "" when it names exactly one.
	Repository string `json:"repository,omitempty"`
	Task       string `json:"task"`
	Title      string `json:"title,omitempty"`
}

// MaxWorkReply is the most characters of a work session's answer that
// reach the calling session; a longer one is clipped, and says so.
const MaxWorkReply = MaxWorkerReply

// WorkReplyPrefix begins the key of a work session's answer among the
// calling session's notes: the work session and the answer's entry follow.
const WorkReplyPrefix = "work-reply:"

// WorkTaskSuffix ends the key of a work session's task among its notes,
// after its work key.
const WorkTaskSuffix = ":task"

// MayWork says whether a session of profile p (nil for none), working as
// role, gets team_work: an agent's own session, of a profile that is no
// supervisor's and not private, working as none of the roles and as no
// work session itself. A supervisor delegates with team_delegate, a
// private agent has no team tool (ADR-0042), and a work session reports
// to the session that started it, so work sessions do not nest.
func MayWork(p *profiles.Profile, role SessionRole) bool {
	return p != nil && !p.Private && p.Role != profiles.RoleSupervisor && role.none()
}

// CheckWorkCaller refuses a team_work call from a session that does not
// get the tool (MayWork), p as its profile is now: one that lists it from
// before is refused too.
func CheckWorkCaller(p *profiles.Profile, role SessionRole) error {
	switch {
	case MayWork(p, role):
		return nil
	case role.Work:
		return errors.New("team_work is refused in a work session: report the plan to the session that started you, in your answer")
	case p == nil:
		return errors.New("team_work is an agent's: this session was started from no profile")
	case p.Private:
		return fmt.Errorf("%s is private, and team tools are not a private agent's", p.ID)
	case p.Role == profiles.RoleSupervisor:
		return fmt.Errorf("team_work is an agent's, and %s is a supervisor: delegate the work with team_delegate", p.ID)
	}
	return errors.New("team_work is an agent's own session's: a thread's specialist, a review, a consult, a gather's agent and a worker work where pocketd started them")
}

// CheckWork refuses a team_work call whose task or title breaks a bound.
func CheckWork(w Work) error {
	task := strings.TrimSpace(w.Task)
	switch n := utf8.RuneCountInString(task); {
	case task == "":
		return errors.New("the task is empty: give the work session its task in full")
	case n > MaxTask:
		return fmt.Errorf("the task has %d characters, more than %d", n, MaxTask)
	}
	if n := utf8.RuneCountInString(strings.TrimSpace(w.Title)); n > MaxTitle {
		return fmt.Errorf("the title has %d characters, more than %d", n, MaxTitle)
	}
	return nil
}

// WorkRepository is the repository a team_work call of an agent of
// profile p names, as the profile names it (CheckoutKey: GitHub's names
// ignore case), and its checkout. name may be "" only when the profile
// names exactly one.
func WorkRepository(p profiles.Profile, name string) (key, checkout string, err error) {
	names := repositoryNames(p)
	name = strings.TrimSpace(name)
	switch {
	case len(names) == 0:
		return "", "", fmt.Errorf("%s works on no repository, so team_work has nowhere to start", p.ID)
	case name == "" && len(names) > 1:
		return "", "", fmt.Errorf("name the repository: %s works on %s", p.ID, strings.Join(names, ", "))
	case name == "":
		return names[0], p.Repositories[names[0]], nil
	}
	key, ok := CheckoutKey(p, name)
	if !ok {
		return "", "", fmt.Errorf("%s does not work on %s; its repositories are %s", p.ID, name, strings.Join(names, ", "))
	}
	return key, p.Repositories[key], nil
}

// WorkTitle is the work's title: the one given, else the task's first
// line, as a thread's (Title).
func WorkTitle(w Work) string {
	return Title(Delegation{Task: w.Task, Title: w.Title})
}

// WorkSessionTitle is the work session's title: the agent's name, then
// the work's title, cut to proto.MaxTitle bytes on a character's edge.
func WorkSessionTitle(agent, title string) string {
	full := agent + ": " + title
	if len(full) <= proto.MaxTitle {
		return full
	}
	cut := full[:proto.MaxTitle-len("…")]
	for !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return strings.TrimSpace(cut) + "…"
}

// WorkWorktree is the worktree of a work session on checkout, and its
// branch: an owner's session worktree's name (proto.SessionWorktree), for
// profile p's id, from the work's title and its work key in place of a
// request id, so a call run again names the same one.
func WorkWorktree(p profiles.Profile, checkout, title, key string) (path, branch string) {
	return SessionWorktree(&p, checkout, title, key)
}

// WorkBrief is a work session's first message, written by code: where it
// works, that its answers go back to the session that started it, then
// the task.
func WorkBrief(caller int64, repository, path, branch, stale, task string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You work in %s, a fresh git worktree of %s on the branch %s, started with team_work by session %d. ", path, repository, branch, caller)
	b.WriteString("The repository's own AGENTS.md and skills are in your instructions. ")
	b.WriteString("Each of your runs' final answers goes back to that session as a message, so end each run with what you did, what you found and what is left. ")
	b.WriteString("Commit to your branch and open a pull request from it when the change is ready.\n")
	if stale != "" {
		b.WriteString(StaleBase(stale) + "\n")
	}
	b.WriteString("\nThe task:\n")
	b.WriteString(strings.TrimSpace(task))
	return b.String()
}

// WorkReplyText is the message a work session's answer brings the session
// that started it: the answer, clipped to MaxWorkReply.
func WorkReplyText(session int64, title, reply string) string {
	reply = strings.TrimSpace(reply)
	if reply == "" {
		reply = "(The work session ended its run with no answer.)"
	}
	if utf8.RuneCountInString(reply) > MaxWorkReply {
		reply = string([]rune(reply)[:MaxWorkReply]) + "\n\n[pocketd clipped the answer at " + fmt.Sprint(MaxWorkReply) + " characters; the rest is in session " + fmt.Sprint(session) + "]"
	}
	return fmt.Sprintf("[S%d · work · %s] %s", session, Clip(strings.TrimSpace(title), 60), reply)
}

// WorkStarted is team_work's answer: the work session, its model, its
// worktree and branch, and how its answers come back.
func WorkStarted(session int64, model, repository, path, branch string) string {
	return fmt.Sprintf("Started work session %d on %s, in %s, a new worktree of %s on the branch %s, with your task as its first message. "+
		"Each of its answers comes back to you as a message that starts with [S%d · work · …]. pocketd leaves the worktree in place: remove it once its work is merged.",
		session, model, path, repository, branch, session)
}

// WorkGuide is the line of an agent's instructions that says where it
// changes a repository (ADR-0073): from a work session, never from its
// home.
const WorkGuide = "Your home holds your plans and notes, not your repositories' work: change a repository from a work session, never from your home. " +
	"team_work starts one in a fresh worktree of one of your repositories, where that repository's own AGENTS.md and skills load; give it the task in full, and each of its answers comes back to this session."
