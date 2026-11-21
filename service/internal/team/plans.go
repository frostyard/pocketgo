package team

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// Plan states (ADR-0020).
const (
	PlanDrafting = "drafting"
	PlanReview   = "review"
	PlanApproved = "approved"
	PlanBuilding = "building"
	PlanDone     = "done"
	PlanDropped  = "dropped"
)

// PlanEnded says whether a plan in state is over: done or dropped.
func PlanEnded(state string) bool { return state == PlanDone || state == PlanDropped }

// Task states.
const (
	TaskWaiting   = "waiting"
	TaskReady     = "ready"
	TaskDelegated = "delegated"
	TaskDone      = "done"
)

// Consult states.
const (
	ConsultStarting = "starting"
	ConsultRunning  = "running"
	ConsultAnswered = "answered"
	ConsultFailed   = "failed"
	ConsultStopped  = "stopped"
)

// ConsultOpen says whether a consult in state holds its plan: it starts
// or runs.
func ConsultOpen(state string) bool { return state == ConsultStarting || state == ConsultRunning }

// A finding's stances.
const (
	StanceSupports = "supports"
	StanceObjects  = "objects"
	StanceNeutral  = "neutral"
)

// team_plan's actions.
const (
	PlanRead    = "read"
	PlanWrite   = "write"
	PlanTasks   = "tasks"
	PlanReady   = "ready"
	PlanConsult = "consult"
)

// Limits on plans and consults (ADR-0020).
const (
	MaxPlansOpen       = 3
	MaxPlanText        = 20000
	MaxGraphTasks      = 30
	MaxAcceptance      = 8
	MaxAcceptanceLine  = 300
	MaxConsultQuestion = 2000
	MaxConsultAgents   = 3
	MaxConsultsRunning = 3
	MaxConsults        = 8
	MaxFindingSummary  = 2000
	// ConsultLimit is how long a consult has for its finding, from its call.
	ConsultLimit = 60 * time.Minute
)

// PlanRef is team_delegate's plan argument: the start of a plan, or the
// delivery of one task of an approved plan.
type PlanRef struct {
	Start bool   `json:"start,omitempty"`
	ID    int64  `json:"id,omitempty"`
	Task  string `json:"task,omitempty"`
}

// CheckPlanRef refuses a plan argument that is neither a start nor a
// task's delivery.
func CheckPlanRef(p PlanRef) error {
	switch {
	case p.Start && (p.ID != 0 || p.Task != ""):
		return errors.New("plan is either {start: true}, or {id, task}: not both")
	case p.Start:
		return nil
	case p.ID <= 0 || p.Task == "":
		return errors.New("plan needs {start: true} to start a plan, or the plan's id and a task's id to deliver it, as {id: 1, task: \"t2\"}")
	}
	return nil
}

// CheckPlanStart refuses the start of a plan that breaks a rule: a caller
// that is not a supervisor, a lead that takes no delegation (CheckTarget)
// or has no home or workspace, a repository, a task or title out of their
// limits, or open plans at their cap. The plan thread takes no place on
// the desk.
func CheckPlanStart(caller *profiles.Profile, lead profiles.Profile, startable func() error, d Delegation, open int) error {
	if caller == nil || caller.Role != profiles.RoleSupervisor {
		return errors.New("only a supervisor starts a plan: this session's profile is not one")
	}
	if err := CheckTarget(lead, startable); err != nil {
		return err
	}
	switch {
	case d.Repository != "":
		return errors.New("a plan's lead plans in its home, and changes no repository: leave the repository out")
	case lead.Start() == "":
		return fmt.Errorf("%s has no home or workspace to plan in", lead.ID)
	case strings.TrimSpace(d.Task) == "":
		return errors.New("the task is empty: give the idea to plan")
	case utf8.RuneCountInString(d.Task) > MaxTask:
		return fmt.Errorf("the task has %d characters, more than %d", utf8.RuneCountInString(d.Task), MaxTask)
	case utf8.RuneCountInString(d.Title) > MaxTitle:
		return fmt.Errorf("the title has %d characters, more than %d", utf8.RuneCountInString(d.Title), MaxTitle)
	case open >= MaxPlansOpen:
		return fmt.Errorf("%d plans are open, and at most %d may be: wait for one to be done, or drop one by closing its thread", open, MaxPlansOpen)
	}
	return nil
}

// Task is one task of a plan's graph.
type Task struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Agent string `json:"agent"`
	// Repository is owner/name of a repository of the agent's, "" for its
	// home.
	Repository string   `json:"repository,omitempty"`
	Acceptance []string `json:"acceptance"`
	Deps       []string `json:"deps,omitempty"`
}

// PlanCall is a team_plan call's arguments.
type PlanCall struct {
	Action string `json:"action"`
	// Text and Rev are write's: the whole new text, and the revision it
	// was written from ("" for the first). Asks are the answered asks it
	// writes in, by number.
	Text string  `json:"text,omitempty"`
	Rev  string  `json:"rev,omitempty"`
	Asks []int64 `json:"asks,omitempty"`
	// Tasks are the graph tasks drafts.
	Tasks []Task `json:"tasks,omitempty"`
	// Agents, Question and Ask are consult's: the experts, what they are
	// asked, and the plan's ask it is on (0 for none).
	Agents   []string `json:"agents,omitempty"`
	Question string   `json:"question,omitempty"`
	Ask      int64    `json:"ask,omitempty"`
	// Page is read's page, from 1; 0 is the first. Snapshot, for a page
	// past the first, names the read page 1 came from: a read that changed
	// since is refused, so no page is skipped or read twice (Codex review
	// of #25, round 7).
	Page     int    `json:"page,omitempty"`
	Snapshot string `json:"snapshot,omitempty"`
}

// Snapshot names a whole read: its digest's first 12 hex characters.
func Snapshot(read string) string { return Rev(read)[:12] }

// Read pages: a tool result stays far below what PiG keeps of one (50 KiB
// or 2,000 lines), so no part of a plan is lost to a cut (Codex review of
// #25, round 5).
const (
	MaxPageBytes = 16 << 10
	MaxPageLines = 400
)

// Pages cuts text into pages of at most MaxPageBytes bytes and
// MaxPageLines lines, at line ends, and a line longer than a page at a
// character. Joined, the pages are text.
func Pages(text string) []string {
	var pages []string
	var b strings.Builder
	lines := 0
	flush := func() {
		if b.Len() > 0 {
			pages = append(pages, b.String())
			b.Reset()
			lines = 0
		}
	}
	for _, line := range strings.SplitAfter(text, "\n") {
		for len(line) > MaxPageBytes {
			flush()
			cut := MaxPageBytes
			for cut > 0 && !utf8.RuneStart(line[cut]) {
				cut--
			}
			pages = append(pages, line[:cut])
			line = line[cut:]
		}
		if b.Len()+len(line) > MaxPageBytes || lines+1 > MaxPageLines {
			flush()
		}
		b.WriteString(line)
		lines++
	}
	flush()
	if len(pages) == 0 {
		pages = []string{""}
	}
	return pages
}

var revPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// CheckPlanCall checks a call's shape: an action it knows, with exactly
// what that action takes.
func CheckPlanCall(c PlanCall) error {
	text, rev, asks := c.Text != "", c.Rev != "", len(c.Asks) > 0
	tasks, consult := len(c.Tasks) > 0, len(c.Agents) > 0 || c.Question != "" || c.Ask != 0
	if (c.Page != 0 || c.Snapshot != "") && c.Action != PlanRead {
		return fmt.Errorf("%s takes no page: only read does", c.Action)
	}
	switch c.Action {
	case PlanRead:
		if text || rev || asks || tasks || consult {
			return errors.New("read takes nothing but the page")
		}
		if c.Page < 0 {
			return fmt.Errorf("page %d is not a page: give 1 for the first", c.Page)
		}
		if c.Page > 1 && c.Snapshot == "" {
			return errors.New("a page past the first needs the snapshot page 1 named")
		}
	case PlanReady:
		if text || rev || asks || tasks || consult || c.Page != 0 {
			return fmt.Errorf("%s takes nothing but the action", c.Action)
		}
	case PlanWrite:
		if tasks || consult {
			return errors.New("write takes the text, the rev it read, and the asks it writes in")
		}
		if strings.TrimSpace(c.Text) == "" {
			return errors.New("write needs the plan's whole new text")
		}
		if n := utf8.RuneCountInString(c.Text); n > MaxPlanText {
			return fmt.Errorf("the text has %d characters, more than %d", n, MaxPlanText)
		}
		if rev && !revPattern.MatchString(c.Rev) {
			return fmt.Errorf("rev %q is not a revision: give the rev team_plan read shows, or none for the first write", c.Rev)
		}
		seen := map[int64]bool{}
		for _, id := range c.Asks {
			if id <= 0 {
				return fmt.Errorf("ask %d is not an ask's number: give 3 for A3", id)
			}
			if seen[id] {
				return fmt.Errorf("ask A%d is named twice", id)
			}
			seen[id] = true
		}
	case PlanTasks:
		if text || rev || asks || consult {
			return errors.New("tasks takes only the tasks")
		}
		if !tasks {
			return errors.New("tasks needs the graph's tasks")
		}
	case PlanConsult:
		if text || rev || asks || tasks {
			return errors.New("consult takes the agents, the question, and the ask it is on")
		}
		if len(c.Agents) == 0 || len(c.Agents) > MaxConsultAgents {
			return fmt.Errorf("consult asks 1 to %d agents at once, not %d", MaxConsultAgents, len(c.Agents))
		}
		seen := map[string]bool{}
		for _, a := range c.Agents {
			if seen[a] {
				return fmt.Errorf("%s is named twice", a)
			}
			seen[a] = true
		}
		if strings.TrimSpace(c.Question) == "" {
			return errors.New("consult needs the question")
		}
		if n := utf8.RuneCountInString(c.Question); n > MaxConsultQuestion {
			return fmt.Errorf("the question has %d characters, more than %d", n, MaxConsultQuestion)
		}
		if c.Ask < 0 {
			return fmt.Errorf("ask %d is not an ask's number: give 3 for A3", c.Ask)
		}
	default:
		return fmt.Errorf("action %q is not read, write, tasks, ready or consult", c.Action)
	}
	return nil
}

// Rev is a plan text's revision: its SHA-256, in hex.
func Rev(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// ShortRev is a revision's first 7 characters, as the texts show it.
func ShortRev(rev string) string {
	if len(rev) > 7 {
		return rev[:7]
	}
	return rev
}

var taskID = regexp.MustCompile(`^t[1-9][0-9]{0,2}$`)

// CleanTasks is tasks with each field trimmed, and no nil list.
func CleanTasks(tasks []Task) []Task {
	out := make([]Task, len(tasks))
	for i, t := range tasks {
		out[i] = Task{ID: strings.TrimSpace(t.ID), Title: strings.TrimSpace(t.Title), Agent: strings.TrimSpace(t.Agent),
			Repository: strings.TrimSpace(t.Repository), Acceptance: []string{}, Deps: []string{}}
		for _, line := range t.Acceptance {
			out[i].Acceptance = append(out[i].Acceptance, strings.TrimSpace(line))
		}
		for _, dep := range t.Deps {
			out[i].Deps = append(out[i].Deps, strings.TrimSpace(dep))
		}
	}
	return out
}

// CheckGraph refuses a graph (cleaned, CleanTasks) that breaks a rule: 1
// to MaxGraphTasks tasks with unique ids t1 to t999; each with a title, 1
// to MaxAcceptance acceptance lines, and an agent that takes a delegation
// (find gives its profile) in one of its repositories or its home; and
// dependencies on other tasks of the graph, with no cycle.
func CheckGraph(tasks []Task, find func(id string) (profiles.Profile, error)) error {
	if len(tasks) == 0 || len(tasks) > MaxGraphTasks {
		return fmt.Errorf("a graph has 1 to %d tasks, not %d", MaxGraphTasks, len(tasks))
	}
	ids := map[string]bool{}
	for _, t := range tasks {
		if !taskID.MatchString(t.ID) {
			return fmt.Errorf("task id %q is not t1 to t999", t.ID)
		}
		if ids[t.ID] {
			return fmt.Errorf("task id %s is used twice", t.ID)
		}
		ids[t.ID] = true
	}
	for _, t := range tasks {
		if t.Title == "" {
			return fmt.Errorf("%s has no title", t.ID)
		}
		if n := utf8.RuneCountInString(t.Title); n > MaxTitle {
			return fmt.Errorf("%s's title has %d characters, more than %d", t.ID, n, MaxTitle)
		}
		if len(t.Acceptance) == 0 || len(t.Acceptance) > MaxAcceptance {
			return fmt.Errorf("%s has %d acceptance lines: give 1 to %d, each a check its work must pass", t.ID, len(t.Acceptance), MaxAcceptance)
		}
		for i, line := range t.Acceptance {
			if line == "" {
				return fmt.Errorf("%s's acceptance line %d is empty", t.ID, i+1)
			}
			if n := utf8.RuneCountInString(line); n > MaxAcceptanceLine {
				return fmt.Errorf("%s's acceptance line %d has %d characters, more than %d", t.ID, i+1, n, MaxAcceptanceLine)
			}
		}
		agent, err := find(t.Agent)
		if err != nil {
			return fmt.Errorf("%s: %w", t.ID, err)
		}
		if err := CheckTarget(agent, nil); err != nil {
			return fmt.Errorf("%s: %w", t.ID, err)
		}
		if err := CheckDestination(agent, t.Repository); err != nil {
			return fmt.Errorf("%s: %w", t.ID, err)
		}
		deps := map[string]bool{}
		for _, dep := range t.Deps {
			switch {
			case dep == t.ID:
				return fmt.Errorf("%s depends on itself", t.ID)
			case !ids[dep]:
				return fmt.Errorf("%s depends on %q, which is no task of the graph", t.ID, dep)
			case deps[dep]:
				return fmt.Errorf("%s names %s twice", t.ID, dep)
			}
			deps[dep] = true
		}
	}
	if cycle := findCycle(tasks); cycle != nil {
		return fmt.Errorf("the dependencies have a cycle: %s", strings.Join(cycle, " → "))
	}
	return nil
}

// findCycle is a cycle of the graph's dependencies, as the ids along it
// with the first again at the end, or nil.
func findCycle(tasks []Task) []string {
	deps := map[string][]string{}
	for _, t := range tasks {
		deps[t.ID] = t.Deps
	}
	const (
		unseen = iota
		open
		closed
	)
	mark := map[string]int{}
	var path []string
	var visit func(id string) []string
	visit = func(id string) []string {
		mark[id] = open
		path = append(path, id)
		for _, dep := range deps[id] {
			switch mark[dep] {
			case open:
				start := slices.Index(path, dep)
				return append(slices.Clone(path[start:]), dep)
			case unseen:
				if cycle := visit(dep); cycle != nil {
					return cycle
				}
			}
		}
		path = path[:len(path)-1]
		mark[id] = closed
		return nil
	}
	for _, t := range tasks {
		if mark[t.ID] == unseen {
			if cycle := visit(t.ID); cycle != nil {
				return cycle
			}
		}
	}
	return nil
}

// TaskFacts is a task of an approved graph with its state.
type TaskFacts struct {
	Task
	State string
	// Thread is the thread of its newest delivery, 0 before one.
	Thread int64
}

// NowReady is the tasks of the graph that are waiting and whose every
// dependency is done: they become ready. The tasks are in graph order.
func NowReady(tasks []TaskFacts) []string {
	done := map[string]bool{}
	for _, t := range tasks {
		if t.State == TaskDone {
			done[t.ID] = true
		}
	}
	var out []string
	for _, t := range tasks {
		if t.State != TaskWaiting {
			continue
		}
		ready := true
		for _, dep := range t.Deps {
			ready = ready && done[dep]
		}
		if ready {
			out = append(out, t.ID)
		}
	}
	return out
}

// GateAsk is an ask of a plan's thread, as the gate reads it.
type GateAsk struct {
	ID      int64
	Open    bool
	Written bool
	// Withdrawn says a supervisor's message withdrew it (ADR-0037): it
	// waits for no answer, and has none to write in.
	Withdrawn bool
}

// GateConsult is a consult of a plan, as the gate reads it.
type GateConsult struct {
	ID    int64
	State string
}

// Gate is what a plan waits for before it is settled, in the gate's
// order: each open ask, each answered ask no write has named since, and
// each consult that starts or runs. A withdrawn ask holds nothing. Empty
// means settled.
func Gate(asks []GateAsk, consults []GateConsult) []string {
	var open, unwritten, running []string
	for _, a := range asks {
		switch {
		case a.Withdrawn:
		case a.Open:
			open = append(open, "A"+strconv.FormatInt(a.ID, 10)+" open")
		case !a.Written:
			unwritten = append(unwritten, "A"+strconv.FormatInt(a.ID, 10)+" not written")
		}
	}
	for _, c := range consults {
		if ConsultOpen(c.State) {
			running = append(running, "K"+strconv.FormatInt(c.ID, 10)+" "+c.State)
		}
	}
	return slices.Concat(open, unwritten, running)
}

// PlanGate is what a plan in state waits for (Gate): before the approval,
// every item; once approved, its open asks alone, since its text no longer
// changes and no consult starts; once ended, nothing.
func PlanGate(state string, asks []GateAsk, consults []GateConsult) []string {
	switch {
	case PlanEnded(state):
		return nil
	case state == PlanApproved || state == PlanBuilding:
		var open []GateAsk
		for _, a := range asks {
			if a.Open {
				open = append(open, a)
			}
		}
		return Gate(open, nil)
	}
	return Gate(asks, consults)
}

// Waiting is the gate's list as one line.
func Waiting(items []string) string {
	if len(items) == 0 {
		return "settled"
	}
	return "waiting: " + strings.Join(items, ", ")
}

// CheckDraft refuses a write, a graph or a consult on a plan in state:
// they are taken only before the approval (a consult only in drafting).
func CheckDraft(plan int64, state, action string) error {
	switch {
	case state == PlanDrafting:
		return nil
	case state == PlanReview && action != PlanConsult:
		return nil
	case state == PlanReview:
		return fmt.Errorf("plan P%d waits for the owner's approval: a consult is for a plan in drafting; write or draft the tasks again to take it back to drafting", plan)
	case PlanEnded(state):
		return fmt.Errorf("plan P%d is %s", plan, state)
	}
	return fmt.Errorf("plan P%d is %s: its text, its tasks and its consults no longer change", plan, state)
}

// CheckWrite refuses a write from a revision that is not the plan's
// current one, or one that changes nothing.
func CheckWrite(plan int64, current, read, text string) error {
	switch {
	case read != current && current == "":
		return fmt.Errorf("plan P%d has no text yet: write it with no rev", plan)
	case read != current:
		return fmt.Errorf("plan P%d changed since rev %s: it is at rev %s now. Read it again with team_plan read, and write from that", plan, ShortRev(read), ShortRev(current))
	case Rev(text) == current:
		return fmt.Errorf("the text is plan P%d's as it is: a write changes it", plan)
	}
	return nil
}

// CheckReady refuses ready on a plan that is not in drafting, has no
// text, is not settled, or has no graph drafted for its current revision.
func CheckReady(plan int64, state, rev, graphRev string, waiting []string) error {
	switch {
	case state != PlanDrafting && PlanEnded(state):
		return fmt.Errorf("plan P%d is %s", plan, state)
	case state == PlanReview:
		return fmt.Errorf("plan P%d waits for the owner's approval already", plan)
	case state != PlanDrafting:
		return fmt.Errorf("plan P%d is %s already", plan, state)
	case rev == "":
		return fmt.Errorf("plan P%d has no text: write it first with team_plan write", plan)
	case len(waiting) > 0:
		return fmt.Errorf("plan P%d is not settled, %s", plan, Waiting(waiting))
	case graphRev != rev:
		return fmt.Errorf("plan P%d has no task graph for its rev %s: draft one with team_plan tasks", plan, ShortRev(rev))
	}
	return nil
}

// CheckApprove refuses the owner's approval unless the plan waits for it
// as the owner saw it: in review, settled, at the revision and with the
// graph version the owner names.
func CheckApprove(plan int64, state, rev string, graph int, waiting []string, seenRev string, seenGraph int) error {
	switch {
	case state != PlanReview:
		return fmt.Errorf("plan P%d is %s, and does not wait for an approval", plan, state)
	case len(waiting) > 0:
		return fmt.Errorf("plan P%d is not settled, %s", plan, Waiting(waiting))
	case seenRev != rev:
		return fmt.Errorf("plan P%d is at rev %s, not the rev %s you saw", plan, ShortRev(rev), ShortRev(seenRev))
	case seenGraph != graph:
		return fmt.Errorf("plan P%d's tasks are version %d, not the version %d you saw", plan, graph, seenGraph)
	}
	return nil
}

// CheckDelivery refuses the delivery of a plan's task d names, unless the
// plan is approved or building with no open ask (openAsks), and the task
// is in its approved graph (task, nil when it is not), ready, and for d's
// agent and repository.
func CheckDelivery(plan int64, state string, openAsks []string, task *TaskFacts, d Delegation) error {
	switch {
	case state == PlanDrafting || state == PlanReview:
		return fmt.Errorf("plan P%d is not approved: it is %s", plan, state)
	case PlanEnded(state):
		return fmt.Errorf("plan P%d is %s", plan, state)
	case len(openAsks) > 0:
		return fmt.Errorf("plan P%d is not settled, %s", plan, Waiting(openAsks))
	case task == nil:
		return fmt.Errorf("plan P%d's approved graph has no task %q", plan, d.Plan.Task)
	}
	switch task.State {
	case TaskReady:
	case TaskWaiting:
		return fmt.Errorf("%s waits for %s", task.ID, strings.Join(task.Deps, ", "))
	case TaskDelegated:
		return fmt.Errorf("%s is delivered already, in T%d", task.ID, task.Thread)
	default:
		return fmt.Errorf("%s is %s", task.ID, task.State)
	}
	switch {
	case d.Agent != task.Agent:
		return fmt.Errorf("%s is for %s, not %s", task.ID, task.Agent, d.Agent)
	case d.Repository != task.Repository && task.Repository == "":
		return fmt.Errorf("%s works in %s's home: leave the repository out", task.ID, task.Agent)
	case d.Repository != task.Repository:
		return fmt.Errorf("%s works in %s: name that repository", task.ID, task.Repository)
	}
	return nil
}

// CheckConsult refuses a consult of agents (CheckPlanCall checked the
// call's shape) that breaks a rule: an expert that is the lead, takes no
// delegation (CheckTarget, with startable), or has no home or workspace;
// or more consults than the plan may run at once (running) or in all
// (total).
func CheckConsult(lead string, agents []string, find func(id string) (profiles.Profile, error), startable func(profiles.Profile) error, running, total int) error {
	for _, id := range agents {
		if id == lead {
			return fmt.Errorf("%s leads the plan: consult another agent", id)
		}
		p, err := find(id)
		if err != nil {
			return err
		}
		if err := CheckTarget(p, func() error { return startable(p) }); err != nil {
			return err
		}
		if p.Start() == "" {
			return fmt.Errorf("%s has no home or workspace to answer in", id)
		}
	}
	switch {
	case running+len(agents) > MaxConsultsRunning:
		return fmt.Errorf("%d consults run, and at most %d may at once: wait for a finding", running, MaxConsultsRunning)
	case total+len(agents) > MaxConsults:
		return fmt.Errorf("the plan has had %d consults, and may have %d in all", total, MaxConsults)
	}
	return nil
}

// Finding is a team_finding call: a consult's one answer.
type Finding struct {
	Stance  string `json:"stance"`
	Summary string `json:"summary"`
}

// CheckFinding refuses a finding with a stance it does not know or a
// summary out of its limits.
func CheckFinding(f Finding) error {
	switch f.Stance {
	case StanceSupports, StanceObjects, StanceNeutral:
	default:
		return fmt.Errorf("the stance is supports, objects or neutral, not %q", f.Stance)
	}
	if strings.TrimSpace(f.Summary) == "" {
		return errors.New("the summary is empty")
	}
	if n := utf8.RuneCountInString(f.Summary); n > MaxFindingSummary {
		return fmt.Errorf("the summary has %d characters, more than %d", n, MaxFindingSummary)
	}
	return nil
}

// TaskWorkDone says whether a task's thread closed with its work done: a
// linked pull request merged and none open, or with none linked, its last
// report done (lastState, the thread's state before it closed). pulls are
// the states of the thread's linked pull requests.
func TaskWorkDone(lastState string, pulls []string) bool {
	if len(pulls) == 0 {
		return lastState == StatusDone
	}
	merged := false
	for _, p := range pulls {
		switch p {
		case "open":
			return false
		case "merged":
			merged = true
		}
	}
	return merged
}

// PlanBrief is the first message of a planning session, written by code.
func PlanBrief(supervisor string, thread, plan int64, title, idea, cwd string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s, the team's supervisor, asked you to lead plan P%d in thread T%d: %s\n\n", supervisor, plan, thread, title)
	b.WriteString(strings.TrimSpace(idea))
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "You plan; you build nothing. You work in %s with read alone, and change no file: the plan is written only with team_plan. ", cwd)
	b.WriteString("team_plan read shows the plan, and the agents a task can go to or a consult can ask, with where each works. ")
	b.WriteString("Write the plan with team_plan write: its whole text, with the rev team_plan read shows (none for the first write). ")
	b.WriteString("Put each decision you need from the owner in the asks of team_message. Each answer comes back to this session; write it into the plan, naming its ask in asks. ")
	b.WriteString("Ask another specialist with team_plan consult; its finding comes back here. ")
	b.WriteString("Draft the tasks with team_plan tasks: each a pull request's worth of work for one agent, with the lines that check it. ")
	b.WriteString("When nothing is open, call team_plan ready: the owner approves the plan and its tasks, and the supervisor then delivers them. ")
	b.WriteString("pocketd tells the owner and the supervisor what they need to know; report to the supervisor with team_message only when you are blocked.")
	return b.String()
}

// PlanTaskText is the task a delivered plan task's thread works on: the
// supervisor's text, then the task as the owner approved it. Only a task
// on a repository is asked for a pull request (the live check of
// 2026-11-20 asked one of a task in its agent's home).
// The plan's text, as approved, follows: a decision written in the plan
// and not repeated in the task's lines reaches its specialist too (Codex
// review of #25, round 3).
func PlanTaskText(task string, plan int64, title, rev, text string, t Task) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(task))
	fmt.Fprintf(&b, "\n\nThis is task %s of plan P%d (%s), as the owner approved it at rev %s: %s\n", t.ID, plan, title, ShortRev(rev), t.Title)
	b.WriteString("Its work is done when each of these holds:\n")
	for _, line := range t.Acceptance {
		fmt.Fprintf(&b, "- %s\n", line)
	}
	if t.Repository != "" {
		b.WriteString("Open one pull request for it. Report done with no pull request only when the work needs no change.\n\n")
	} else {
		// A task with no repository works in its agent's home or
		// workspace, which has no branch to open a pull request from: its
		// thread is done when it closes reported done (TaskWorkDone).
		b.WriteString("It has no repository: do it where you work, open no pull request, and report done once each line holds.\n\n")
	}
	// As approved, byte for byte: see ConsultQuestion.
	fmt.Fprintf(&b, "The plan, as the owner approved it, of which this task is one part; follow its decisions:\n\n%s", text)
	return b.String()
}

// PlanAnswerText is the message the planning session gets for the owner's
// answer to one of the plan's asks.
func PlanAnswerText(ask int64, question, answer string) string {
	return AnswerText(ask, question, answer) + fmt.Sprintf("\n\nWrite it into the plan with team_plan write, naming %d in asks.", ask)
}

// deliverLine is how a note tells the supervisor to deliver tasks.
// It names each task with what its delivery takes: its title, its agent
// and its repository, since only the lead reads the graph, and the gate
// refuses another agent or repository.
func deliverLine(plan int64, tasks []Task) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Deliver each with team_delegate, plan {\"id\": %d, \"task\": \"<id>\"}, and the task's agent and repository:", plan)
	for _, t := range tasks {
		where := "no repository (its home)"
		if t.Repository != "" {
			where = "repository " + t.Repository
		}
		fmt.Fprintf(&b, "\n- %s · %s: agent %s, %s", t.ID, t.Title, t.Agent, where)
	}
	return b.String()
}

// ApprovedNote is the supervisor's message when the owner approves a plan.
func ApprovedNote(plan int64, title, rev string, graph int, ready []Task) string {
	return fmt.Sprintf("[P%d · approved] The owner approved plan P%d (%s) at rev %s, with tasks version %d. %s",
		plan, plan, title, ShortRev(rev), graph, deliverLine(plan, ready))
}

// ReadyNote is the supervisor's message when a task's done makes others
// ready.
func ReadyNote(plan int64, done string, thread int64, ready []Task) string {
	return fmt.Sprintf("[P%d · %s done] T%d closed with %s's work done. Ready now. %s",
		plan, done, thread, done, deliverLine(plan, ready))
}

// AgainNote is the supervisor's message when a task's thread closed
// without its work done; ready are the tasks to deliver, the task among
// them.
func AgainNote(plan int64, task Task, thread int64, ready []Task) string {
	return fmt.Sprintf("[P%d · %s ready again] T%d closed without %s's work done. %s",
		plan, task.ID, thread, task.ID, deliverLine(plan, ready))
}

// TasksNamed are the tasks of the graph with these ids, in the ids' order.
func TasksNamed(tasks []TaskFacts, ids []string) []Task {
	out := make([]Task, 0, len(ids))
	for _, id := range ids {
		for _, t := range tasks {
			if t.ID == id {
				out = append(out, t.Task)
			}
		}
	}
	return out
}

// ClearedNote is the supervisor's message when the owner's answer clears
// the last open ask of an approved plan with ready tasks.
func ClearedNote(plan, ask int64, ready []Task) string {
	return fmt.Sprintf("[P%d · A%d answered] Plan P%d has no open question now: its ready tasks may go. %s",
		plan, ask, plan, deliverLine(plan, ready))
}

// WithdrawnNote is ClearedNote when a supervisor's message withdrew the
// last open ask, ask, as it unblocked the plan's thread (ADR-0037).
func WithdrawnNote(plan, ask int64, ready []Task) string {
	return fmt.Sprintf("[P%d · A%d withdrawn] A supervisor's message unblocked the plan's thread, so plan P%d has no open question now: its ready tasks may go. %s",
		plan, ask, plan, deliverLine(plan, ready))
}

// FreeNote is the supervisor's message when a place on the desk frees
// after the cap refused a ready task of the plan.
func FreeNote(plan int64, thread int64, ready []Task) string {
	return fmt.Sprintf("[P%d · a place on the desk is free] T%d closed, and plan P%d has ready tasks the desk's cap held back. %s",
		plan, thread, plan, deliverLine(plan, ready))
}

// DoneNote is the supervisor's message when every task of a plan is done.
func DoneNote(plan int64, title string, thread int64) string {
	return fmt.Sprintf("[P%d · done] Every task of plan P%d (%s) is done, and its thread T%d closed.", plan, plan, title, thread)
}

// ClosedText is team_close's answer, from the thread alone, so a call run
// again answers as the first did: a plan's own thread takes no place on
// the desk, and closing it drops a plan not done (Codex review of #25,
// round 7).
func ClosedText(thread, plan int64, planThread bool) string {
	if planThread {
		return fmt.Sprintf("Closed thread T%d, plan P%d's own: a plan not done is dropped with it, and its tasks not delivered can no longer be. "+
			"The desk did not change: a plan's thread takes no place on it.", thread, plan)
	}
	return fmt.Sprintf("Closed thread T%d: its place on the desk is free.", thread)
}

// PlanClosedReport is the plan thread's report once every task is done.
func PlanClosedReport(plan int64) string {
	return fmt.Sprintf("Closed: every task of plan P%d is done.", plan)
}

// ConsultQuestion is the first message of a consult's session, written by
// code: who asks, on which plan, the question, the plan's text at rev, and
// how to answer. The expert has no other way to read the plan: without it,
// the live check of 2026-11-20 found the change unspecified.
func ConsultQuestion(lead string, consult, plan int64, title, question string, ask int64, askQuestion, rev, text string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s, who leads plan P%d (%s), consults you as K%d:\n\n%s\n\n", lead, plan, title, consult, strings.TrimSpace(question))
	if ask != 0 {
		fmt.Fprintf(&b, "It is on the owner's decision A%d: %q\n\n", ask, askQuestion)
	}
	// The plan is quoted as written, byte for byte, as its rev hashes it:
	// trimmed, an indented first line would lose its indent (Codex review
	// of #31).
	if strings.TrimSpace(text) == "" {
		b.WriteString("The plan has no text yet.\n\n")
	} else {
		fmt.Fprintf(&b, "The plan as it stands at rev %s, which you cannot read otherwise:\n\n%s\n\n", ShortRev(rev), text)
	}
	b.WriteString("Read what you need: you have read and team_finding, and nothing else. ")
	b.WriteString("Answer once with team_finding: your stance (supports, objects or neutral) and a summary of at most 2000 characters. Your finding goes to the plan's lead.")
	return b.String()
}

// FindingNote is the planning session's message for a consult's finding.
func FindingNote(consult int64, agent string, plan int64, ask int64, stance, summary string) string {
	on := ""
	if ask != 0 {
		on = fmt.Sprintf(", on A%d", ask)
	}
	return fmt.Sprintf("[K%d · %s · consult on P%d%s] %s: %s", consult, agent, plan, on, stance, strings.TrimSpace(summary))
}

// ConsultEndNote is the planning session's message for a consult that
// ended without a finding: why says how.
func ConsultEndNote(consult int64, agent string, plan int64, why string) string {
	return fmt.Sprintf("[K%d · %s · consult on P%d] It ended without a finding: %s.", consult, agent, plan, why)
}

// PlanAgents is the part of team_plan read that names whom a plan's task
// can go to, and whom a consult can ask: every delegable profile no rule
// stops (startable returns nil), by id, with its repositories and whether
// it has a home or workspace (a consult needs one). lead is the plan's
// lead, whom no consult asks.
func PlanAgents(list []profiles.Profile, startable func(profiles.Profile) error, lead string) string {
	sorted := slices.Clone(list)
	slices.SortFunc(sorted, func(a, b profiles.Profile) int { return strings.Compare(a.ID, b.ID) })
	var lines []string
	for _, p := range sorted {
		if !Delegable(p) || startable(p) != nil {
			continue
		}
		var where []string
		if p.Start() != "" {
			where = append(where, "its home")
		}
		names := make([]string, 0, len(p.Repositories))
		for name := range p.Repositories {
			names = append(names, name)
		}
		slices.Sort(names)
		if len(names) > 0 {
			where = append(where, "repositories "+strings.Join(names, ", "))
		}
		line := fmt.Sprintf("- %s (%s): %s", p.ID, p.Name, strings.Join(where, "; "))
		switch {
		case p.ID == lead:
			line += "; it leads this plan, so no consult"
		case p.Start() == "":
			line += "; no home, so no consult"
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return "No agent can take a task or a consult now."
	}
	return "Agents a task can go to (in its home, or one of its repositories), and a consult can ask, by id:\n" + strings.Join(lines, "\n")
}

// PlanRoster is the part of a supervisor's instructions on plans.
func PlanRoster() string {
	return fmt.Sprintf("Plan before building when the owner asks for a plan, or the work needs decisions first: team_delegate with plan {\"start\": true} starts a plan, led by the agent you name, in its home; at most %d plans are open. "+
		"The owner approves a plan in their Inbox, and pocketd then tells you its ready tasks. Deliver each with team_delegate and plan {\"id\": <plan>, \"task\": \"<id>\"}, to the task's agent and repository: "+
		"pocketd refuses a task that is not ready. Plan work always names its plan.", MaxPlansOpen)
}
