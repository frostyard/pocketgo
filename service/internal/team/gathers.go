package team

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// A gather (ADR-0033) asks agents of the team for proposals: work each
// could do as a shift, which the owner picks from and launches. Each agent
// answers in a read-only proposal session of its own, which takes no place
// on the desk and opens no thread, and files its proposals with
// team_propose; the supervisor orders, edits and drops them with
// team_board, and is told once when the gather's proposals are in.

// The bounds of a gather and its proposals (ADR-0033).
const (
	// GatherLimit is how long an asked agent has to propose.
	GatherLimit = 60 * time.Minute
	// MaxGatherNote bounds a gather's note to the agents.
	MaxGatherNote = profiles.MaxGatherNote
	// MaxProposalStop and MaxProposalNote bound a proposal's stop
	// condition and note; its title and task are a thread's (MaxTitle,
	// MaxTask).
	MaxProposalStop = MaxShiftStop
	MaxProposalNote = 500
	// MaxDoneLines bounds the proposals the supervisor's note names one by
	// one.
	MaxDoneLines = 40
	// MaxBoardBytes and MaxBoardLines bound team_board's answer, so the
	// tool's output limits let a read of one proposal whole through: its
	// line and its text, each character up to four bytes and each a line
	// at most (Codex review of #57, round 2). A list's page is smaller.
	MaxBoardBytes = 4*(MaxTitle+MaxTask+MaxProposalStop+MaxProposalNote) + 2048
	MaxBoardLines = MaxTitle + MaxTask + MaxProposalStop + MaxProposalNote + 32
)

// GatherTimeWhy is why an ask ended at its gather's deadline.
var GatherTimeWhy = fmt.Sprintf("its %d minutes passed", int(GatherLimit/time.Minute))

// A gather's states.
const (
	GatherRunning = "running"
	GatherDone    = "done"
)

// An asked agent's states: starting until its proposal session exists,
// running until it says it is done, its run ends, or its time passes.
const (
	AskStarting = "starting"
	AskRunning  = "running"
	AskDone     = "done"
	AskFailed   = "failed"
)

// AskOpen says an asked agent may still propose, or start to.
func AskOpen(state string) bool { return state == AskStarting || state == AskRunning }

// A proposal's states: proposed until the owner or the supervisor drops
// it. The launch's states come with the board's launch.
const (
	ProposalProposed = "proposed"
	ProposalDropped  = "dropped"
)

// CheckSkillName refuses a name that is no skill's folder name
// (profiles.CheckSkillName).
func CheckSkillName(name string) error { return profiles.CheckSkillName(name) }

// Gather is a gather as team_gather and a schedule's gather action name
// it: the skill whose text each asked agent gets, the agents (every agent
// the supervisor can delegate to when none), and a note to them.
type Gather struct {
	Skill  string   `json:"skill"`
	Agents []string `json:"agents,omitempty"`
	Note   string   `json:"note,omitempty"`
}

// CheckGather checks a gather's shape: a skill's name, distinct agents by
// id, and a note of at most MaxGatherNote characters.
func CheckGather(g Gather) error {
	if err := CheckSkillName(g.Skill); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, agent := range g.Agents {
		if agent == "" {
			return errors.New("an agent's id is empty")
		}
		if seen[agent] {
			return fmt.Errorf("%s is named twice", agent)
		}
		seen[agent] = true
	}
	if n := utf8.RuneCountInString(g.Note); n > MaxGatherNote {
		return fmt.Errorf("the note has %d characters, more than %d", n, MaxGatherNote)
	}
	return nil
}

// Askable says why agent p takes no gather's ask, nil when it does: the
// target rules of a delegation (CheckTarget), and a home or workspace to
// propose in, read-only.
func Askable(p profiles.Profile, startable func() error) error {
	if err := CheckTarget(p, startable); err != nil {
		return err
	}
	if p.Start() == "" {
		return fmt.Errorf("%s has no home or workspace to propose in", p.ID)
	}
	return nil
}

// Proposal fields as a call names them: a field left out is left as it is
// (an edit) or takes its default (an add); "" clears the repository.
type ProposalFields struct {
	Repository *string `json:"repository,omitempty"`
	Title      *string `json:"title,omitempty"`
	Task       *string `json:"task,omitempty"`
	StopWhen   *string `json:"stop_when,omitempty"`
	MaxRounds  *int    `json:"max_rounds,omitempty"`
	Note       *string `json:"note,omitempty"`
}

// Proposal is a proposal's text as checked.
type Proposal struct {
	Repository, Title, Task, StopWhen, Note string
	MaxRounds                               int
}

// Apply checks f against p, the proposal as it is (zero for an add), for
// an agent whose repositories are repos: the title 1 to MaxTitle
// characters, the task 1 to MaxTask, the stop condition 1 to
// MaxProposalStop, the rounds 1 to MaxShiftRounds (DefaultShiftRounds on an
// add), a repository of the agent's or none, and a note of at most
// MaxProposalNote.
func (f ProposalFields) Apply(p Proposal, repos []string, adding bool) (Proposal, error) {
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = strings.TrimSpace(*v)
		}
	}
	set(&p.Repository, f.Repository)
	set(&p.Title, f.Title)
	set(&p.Task, f.Task)
	set(&p.StopWhen, f.StopWhen)
	set(&p.Note, f.Note)
	if f.MaxRounds != nil {
		p.MaxRounds = *f.MaxRounds
	} else if adding {
		p.MaxRounds = DefaultShiftRounds
	}
	count := utf8.RuneCountInString
	switch {
	case p.Title == "":
		return Proposal{}, errors.New("a proposal has a title")
	case count(p.Title) > MaxTitle:
		return Proposal{}, fmt.Errorf("the title has %d characters, more than %d", count(p.Title), MaxTitle)
	case p.Task == "":
		return Proposal{}, errors.New("a proposal has its task in full")
	case count(p.Task) > MaxTask:
		return Proposal{}, fmt.Errorf("the task has %d characters, more than %d", count(p.Task), MaxTask)
	case p.StopWhen == "":
		return Proposal{}, errors.New("a proposal says when its work is done: stop_when")
	case count(p.StopWhen) > MaxProposalStop:
		return Proposal{}, fmt.Errorf("stop_when has %d characters, more than %d", count(p.StopWhen), MaxProposalStop)
	case p.MaxRounds < 1 || p.MaxRounds > MaxShiftRounds:
		return Proposal{}, fmt.Errorf("max_rounds is %d, not 1 to %d", p.MaxRounds, MaxShiftRounds)
	case count(p.Note) > MaxProposalNote:
		return Proposal{}, fmt.Errorf("the note has %d characters, more than %d", count(p.Note), MaxProposalNote)
	case p.Repository != "" && !slices.Contains(repos, p.Repository):
		if len(repos) == 0 {
			return Proposal{}, fmt.Errorf("%s is none of the agent's repositories: it has none, so leave repository out", p.Repository)
		}
		return Proposal{}, fmt.Errorf("%s is none of the agent's repositories: %s", p.Repository, strings.Join(repos, ", "))
	}
	return p, nil
}

// Actions of team_propose and team_board.
const (
	ProposeAdd  = "add"
	ProposeEdit = "edit"
	ProposeDrop = "drop"
	ProposeList = "list"
	ProposeDone = "done"
	BoardList   = "list"
	BoardRead   = "read"
	BoardEdit   = "edit"
	BoardOrder  = "order"
	BoardDrop   = "drop"
)

// ProposeCall is a team_propose call: an asked agent's own proposals.
type ProposeCall struct {
	Action string `json:"action"`
	Row    int64  `json:"row,omitempty"`
	Rev    int    `json:"rev,omitempty"`
	// Page is list's page, from 1; 0 is the first (Codex review of #57,
	// round 5).
	Page int `json:"page,omitempty"`
	ProposalFields
}

// CheckPropose checks a call's shape: each action with what it takes.
func CheckPropose(c ProposeCall) error {
	fields := c.ProposalFields != (ProposalFields{})
	if c.Page != 0 && c.Action != ProposeList || c.Page < 0 {
		return errors.New("only list takes a page, from 1")
	}
	switch c.Action {
	case ProposeAdd:
		if c.Row != 0 || c.Rev != 0 {
			return errors.New("add takes the proposal's fields, no row")
		}
	case ProposeEdit:
		if c.Row <= 0 || c.Rev <= 0 || !fields {
			return errors.New("edit takes the row, its rev as list shows it, and the fields that change")
		}
	case ProposeDrop:
		if c.Row <= 0 || c.Rev <= 0 || fields {
			return errors.New("drop takes the row and its rev as list shows it, and nothing else")
		}
	case ProposeList, ProposeDone:
		if c.Row != 0 || c.Rev != 0 || fields {
			return fmt.Errorf("%s takes nothing else", c.Action)
		}
	default:
		return fmt.Errorf("the action %q is none of add, edit, drop, list and done", c.Action)
	}
	return nil
}

// BoardCall is a team_board call: the supervisor's proposals.
type BoardCall struct {
	Action string  `json:"action"`
	Row    int64   `json:"row,omitempty"`
	Rev    int     `json:"rev,omitempty"`
	Rows   []int64 `json:"rows,omitempty"`
	Page   int     `json:"page,omitempty"`
	ProposalFields
}

// CheckBoard checks a call's shape: each action with what it takes.
func CheckBoard(c BoardCall) error {
	fields := c.ProposalFields != (ProposalFields{})
	if c.Page != 0 && c.Action != BoardList {
		return fmt.Errorf("%s takes no page: only list does", c.Action)
	}
	switch c.Action {
	case BoardList:
		if c.Row != 0 || c.Rev != 0 || len(c.Rows) > 0 || fields || c.Page < 0 {
			return errors.New("list takes a page alone, from 1")
		}
	case BoardRead:
		if c.Row <= 0 || c.Rev != 0 || len(c.Rows) > 0 || fields {
			return errors.New("read takes the row alone")
		}
	case BoardEdit:
		if c.Row <= 0 || c.Rev <= 0 || !fields || len(c.Rows) > 0 {
			return errors.New("edit takes the row, its rev as list shows it, and the fields that change")
		}
	case BoardDrop:
		if c.Row <= 0 || c.Rev <= 0 || fields || len(c.Rows) > 0 {
			return errors.New("drop takes the row and its rev as list shows it, and nothing else")
		}
	case BoardOrder:
		if len(c.Rows) == 0 || c.Row != 0 || c.Rev != 0 || fields {
			return errors.New("order takes rows: the proposals' numbers in the order you want them")
		}
		seen := map[int64]bool{}
		for _, row := range c.Rows {
			if row <= 0 || seen[row] {
				return fmt.Errorf("rows names %d twice, or a number that is no proposal's", row)
			}
			seen[row] = true
		}
	default:
		return fmt.Errorf("the action %q is none of list, read, edit, order and drop", c.Action)
	}
	return nil
}

// ProposalBrief is an asked agent's first message: the supervisor's
// gather, the skill's text, the agent's repositories, the rules of a
// proposal, and its time.
func ProposalBrief(gather int64, supervisor, skill, skillText, note string, repos map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[G%d · %s] %s, the team's supervisor, gathers proposals from the team: work you could do as a shift, in rounds, which the owner picks from and launches later.\n\n", gather, skill, supervisor)
	if note = strings.TrimSpace(note); note != "" {
		fmt.Fprintf(&b, "The supervisor's note:\n%s\n\n", note)
	}
	fmt.Fprintf(&b, "What to look for, from the skill %q:\n\n%s\n\n", skill, strings.TrimSpace(skillText))
	b.WriteString("You work read-only: you have read and team_propose, and nothing else, and you change nothing. ")
	if len(repos) > 0 {
		names := make([]string, 0, len(repos))
		for name := range repos {
			names = append(names, name)
		}
		slices.Sort(names)
		b.WriteString("Your repositories, each at its checkout:")
		for _, name := range names {
			fmt.Fprintf(&b, "\n- %s at %s", name, repos[name])
		}
		b.WriteString("\n\n")
	} else {
		b.WriteString("You have no repository: your proposals are work in your home.\n\n")
	}
	fmt.Fprintf(&b, "File each proposal with team_propose add: a title (at most %d characters); the task in full (at most %d), which is all the shift's brief will say; stop_when, when the work is done, in words you can check (at most %d); max_rounds, 1 to %d (%d when left out); the repository it works in, one of yours, or none for your home; and a note for the owner (at most %d). ",
		MaxTitle, MaxTask, MaxProposalStop, MaxShiftRounds, DefaultShiftRounds, MaxProposalNote)
	b.WriteString("Change or drop one of yours with team_propose edit or drop, naming its row and its rev as team_propose list shows them. ")
	fmt.Fprintf(&b, "When you have nothing more to propose, call team_propose done; nothing at all is a fine answer. You have %d minutes.", int(GatherLimit/time.Minute))
	return b.String()
}

// GatherStartedText is team_gather's answer: it depends only on the gather
// as recorded, so a call run again answers as the first did.
func GatherStartedText(gather int64, skill string, agents []string) string {
	return fmt.Sprintf("Gather G%d started with the skill %s: pocketd asks %s, each in a read-only proposal session of its own, for %d minutes at most. You hear once when their proposals are in; then order, edit or drop them with team_board, and the owner picks from the board.",
		gather, skill, strings.Join(agents, ", "), int(GatherLimit/time.Minute))
}

// AskOutcome is one asked agent's part in a gather, for the supervisor's
// note.
type AskOutcome struct {
	Agent string
	State string
	Why   string
	// Proposals are its proposals that were not dropped, by number, each
	// with its title.
	Proposals []ProposalTitle
}

// ProposalTitle is a proposal's number and title.
type ProposalTitle struct {
	ID    int64
	Title string
}

// GatherDoneText is the supervisor's one message when a gather's
// proposals are in: each asked agent's proposals by number, at most
// MaxDoneLines named, and the agents that filed none or failed.
func GatherDoneText(gather int64, skill string, asks []AskOutcome) string {
	var b strings.Builder
	total := 0
	for _, a := range asks {
		total += len(a.Proposals)
	}
	fmt.Fprintf(&b, "[G%d · %s · proposals in] %d proposals from %d agents asked.", gather, skill, total, len(asks))
	named := 0
	var none, failed []string
	for _, a := range asks {
		for _, p := range a.Proposals {
			if named == MaxDoneLines {
				break
			}
			fmt.Fprintf(&b, "\n#%d %s: %s", p.ID, a.Agent, p.Title)
			named++
		}
		switch {
		case a.State == AskFailed:
			failed = append(failed, fmt.Sprintf("%s (%s)", a.Agent, a.Why))
		case len(a.Proposals) == 0:
			none = append(none, a.Agent)
		}
	}
	if total > named {
		fmt.Fprintf(&b, "\n… and %d more: team_board list shows them all.", total-named)
	}
	if len(none) > 0 {
		fmt.Fprintf(&b, "\nNothing to propose: %s.", strings.Join(none, ", "))
	}
	if len(failed) > 0 {
		fmt.Fprintf(&b, "\nNo answer: %s.", strings.Join(failed, "; "))
	}
	if total > 0 {
		b.WriteString("\nOrder, edit or drop them with team_board; the owner picks from the board.")
	}
	return b.String()
}
