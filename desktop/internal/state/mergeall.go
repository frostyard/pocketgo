package state

import (
	"slices"
	"strconv"

	"github.com/frostyard/pocketgo/desktop/internal/github"
	"github.com/frostyard/pocketgo/proto"
)

// Merge All Ready (ADR-0031): the Inbox's one action that reads the gates
// of every pull request pocketd backs, lists those whose Approve + Merge is
// on in one confirmation, and approves and merges them one at a time, each
// as Approve + Merge does: read from pocketd fresh at its turn
// (Store.ReadPull), ApproveAndMerge with the check the confirmation listed,
// and the gates read again on GitHub before each mutation (github.Act). One
// that fails stays in the Inbox with its reason, and the rest go on.

// MergeAllPhase is where Merge All Ready stands.
type MergeAllPhase int

const (
	MergeAllNone MergeAllPhase = iota
	// MergeAllChecking: the gates of every pull request pocketd backs are
	// read on GitHub, as Check to Merge reads them.
	MergeAllChecking
	// MergeAllConfirming: the list of those whose Approve + Merge is on
	// waits for the owner's one confirmation.
	MergeAllConfirming
	// MergeAllMerging: they are approved and merged, one at a time.
	MergeAllMerging
	// MergeAllEnded: the run ended; its items say how each did.
	MergeAllEnded
)

// MergeAllItem is one pull request a Merge All Ready confirmation lists.
type MergeAllItem struct {
	Pull proto.PullRequest
	Ref  github.Ref
	// Merge is the check the confirmation lists: its merge holds to that
	// check and its head (ApproveAndMerge).
	Merge Merge
	// Note is CheckApp's warning for its owner, which blocks nothing.
	Note string
	// Waived says it is listed without its diff opened: risk:high, by a
	// dependency bot (WaivesDiff; ADR-0031, the owner's choice of
	// 2026-11-21).
	Waived bool
	// Result is how its turn ended; Kind is "" until it had one.
	Result github.Result
}

// Tried says whether its turn came and ended.
func (i MergeAllItem) Tried() bool { return i.Result.Kind != "" }

// MergeAll is Merge All Ready as the window shows it.
type MergeAll struct {
	Phase MergeAllPhase
	// ID names the run: its confirmation holds for that run only.
	ID int
	// Checking is how many of its reads of the gates wait; Checked how
	// many pull requests it read.
	Checking, Checked int
	// Items are what the confirmation lists, and how each did.
	Items []MergeAllItem
	// Turn is the item whose turn it is, while merging.
	Turn int
	// Stopping says the owner stopped it: no turn starts after the one
	// under way.
	Stopping bool
	// End is why the run ended before its last item, or found none; ""
	// when it ran to its end.
	End string
}

// MergeAllCounts are how a run's items ended. A pull request in the merge
// queue may yet leave it unmerged, and one whose end is unknown may have
// merged: each is apart from those merged and those that stayed, so the
// owner checks it again (Codex review of #53, round 1).
type MergeAllCounts struct {
	Merged, Queued, Unknown, Stayed, Untried int
}

// String is the counts in words: those in the queue and those whose end is
// unknown only when there are some.
func (c MergeAllCounts) String() string {
	out := strconv.Itoa(c.Merged) + " merged, "
	if c.Queued > 0 {
		out += strconv.Itoa(c.Queued) + " in the merge queue, "
	}
	if c.Unknown > 0 {
		out += strconv.Itoa(c.Unknown) + " to check again, "
	}
	return out + strconv.Itoa(c.Stayed) + " stayed, " + strconv.Itoa(c.Untried) + " not tried"
}

// Counts are how the run's items ended.
func (m MergeAll) Counts() MergeAllCounts {
	var c MergeAllCounts
	for _, it := range m.Items {
		switch {
		case !it.Tried():
			c.Untried++
		case it.Result.Kind == "merged":
			c.Merged++
		case it.Result.Kind == "queued":
			c.Queued++
		case it.Result.Kind == "unknown":
			c.Unknown++
		default:
			c.Stayed++
		}
	}
	return c
}

// allRun is GitHub's own record of the run: what the window sees, and
// what it needs to go on.
type allRun struct {
	MergeAll
	runs int
	cred int
	// keys are the pull requests the run checks, pending those whose check
	// waits for its turn, and since the last check read before the run;
	// rows reads pocketd's rows as they are now; read is Store.ReadPull.
	keys, pending []string
	since         int
	rows          func() []proto.PullRequest
	read          func(id int64, done func(fresh FreshPull, found bool, err error))
	// halt is why the run ends before its next turn, as GitHub's rate
	// limit; "" while it goes on.
	halt string
	// acting says the turn handed its pull request to ApproveAndMerge and
	// waits for its end: a run dropped then logs that merge as under way,
	// not as untried (Codex review of #53, round 5).
	acting bool
}

// MergeAll is Merge All Ready as it stands.
func (g *GitHub) MergeAll() MergeAll {
	m := g.all.MergeAll
	m.Items = slices.Clone(m.Items)
	return m
}

// mergeAllRunning says a run has not ended.
func (g *GitHub) mergeAllRunning() bool {
	switch g.all.Phase {
	case MergeAllChecking, MergeAllConfirming, MergeAllMerging:
		return true
	}
	return false
}

// mergeAllChecks is how many pull requests' gates a run reads at once:
// one click never goes past GitHub's limit on concurrent requests (Codex
// review of #53, round 3).
const mergeAllChecks = 8

// mergeAllBacked are the rows pocketd backs: pull requests that wait for
// review, that pocketd backs at the head and base branch it read, one a
// reference. pocketd backs no other, so CanMerge refuses every other
// whatever GitHub says (Backed).
func mergeAllBacked(rows []proto.PullRequest) []proto.PullRequest {
	var out []proto.PullRequest
	seen := map[string]bool{}
	for _, p := range rows {
		ref, ok := MergeRef(p)
		if !ok || !ToReview(p) || !Backed(p, p.HeadSHA, p.Base) || seen[ref.String()] {
			continue
		}
		seen[ref.String()] = true
		out = append(out, p)
	}
	return out
}

// mergeAllCandidates are the rows a run checks: those pocketd backs whose
// merge is not running.
func (g *GitHub) mergeAllCandidates(rows []proto.PullRequest) []proto.PullRequest {
	var out []proto.PullRequest
	for _, p := range mergeAllBacked(rows) {
		if !g.merging(p) {
			out = append(out, p)
		}
	}
	return out
}

// merging says p's merge runs, from its card or a run, under this
// credential or an earlier one.
func (g *GitHub) merging(p proto.PullRequest) bool {
	ref, _ := MergeRef(p)
	key := ref.String()
	m := g.merges[key]
	return g.inFlight[key] > 0 || (m != nil && m.Phase == MergeActing)
}

// CanMergeAll says whether Merge All Ready can begin with rows now, and
// why not.
func (g *GitHub) CanMergeAll(rows []proto.PullRequest) (bool, string) {
	switch {
	case !g.On():
		return false, "approve and merge are off: " + g.Off()
	case g.phase != SignedIn:
		return false, "sign in to GitHub first"
	case g.mergeAllRunning():
		return false, "Merge All Ready is running"
	case len(g.mergeAllCandidates(rows)) == 0 && len(mergeAllBacked(rows)) > 0:
		// Backed, and merging already from their cards (Codex review of
		// #53, round 3).
		return false, "every pull request pocketd backs is merging now"
	case len(g.mergeAllCandidates(rows)) == 0:
		return false, "pocketd backs no pull request here now: none has a ready cross-review with green CI at its head"
	}
	return true, ""
}

// MergeAllReady begins Merge All Ready: it reads the gates of every pull
// request of rows() pocketd backs, as Check to Merge does, mergeAllChecks
// at a time. Once every read ended, the run lists those whose Approve +
// Merge is on then, from rows() as they are then, and waits for
// ConfirmMergeAll.
func (g *GitHub) MergeAllReady(rows func() []proto.PullRequest) {
	if ok, _ := g.CanMergeAll(rows()); !ok {
		return
	}
	cands := g.mergeAllCandidates(rows())
	g.all.runs++
	g.all.MergeAll = MergeAll{Phase: MergeAllChecking, ID: g.all.runs}
	g.all.cred, g.all.rows, g.all.read, g.all.halt, g.all.acting = g.cred, rows, nil, "", false
	g.all.since = g.checks
	g.all.keys, g.all.pending = nil, nil
	for _, p := range cands {
		ref, _ := MergeRef(p)
		g.all.keys = append(g.all.keys, ref.String())
	}
	g.all.pending = slices.Clone(g.all.keys)
	g.log(LevelCommand, "Merge all ready: check "+strconv.Itoa(len(cands))+" pull requests")
	g.mergeAllChecked()
}

// mergeAllFeed starts the run's waiting checks while fewer than
// mergeAllChecks run, each with its row as it is now, and counts what
// waits. A pull request no longer backed, or merging now, leaves the run.
func (g *GitHub) mergeAllFeed() {
	a := &g.all
	running := 0
	for _, key := range a.keys {
		if m := g.merges[key]; m != nil && m.Phase == MergeChecking {
			running++
		}
	}
	now := map[string]proto.PullRequest{}
	for _, p := range mergeAllBacked(a.rows()) {
		ref, _ := MergeRef(p)
		now[ref.String()] = p
	}
	// A key whose check runs already, begun from its card, is counted as
	// running, never as waiting too (Codex review of #53, round 4).
	a.pending = slices.DeleteFunc(a.pending, func(key string) bool {
		m := g.merges[key]
		return m != nil && m.Phase == MergeChecking
	})
	for running < mergeAllChecks && len(a.pending) > 0 {
		key := a.pending[0]
		a.pending = a.pending[1:]
		p, ok := now[key]
		if m := g.merges[key]; m != nil && m.Phase == MergeChecking {
			continue // a check begun elsewhere runs: it is counted already
		}
		if !ok || g.merging(p) {
			a.keys = slices.DeleteFunc(a.keys, func(k string) bool { return k == key })
			continue
		}
		g.Check(p)
		if m := g.merges[key]; m != nil && m.Phase == MergeChecking {
			running++
		} else {
			a.keys = slices.DeleteFunc(a.keys, func(k string) bool { return k == key })
		}
	}
	a.Checking = running + len(a.pending)
	a.Checked = len(a.keys) - a.Checking
}

// mergeAllChecked lists what passes once the run's last read ended: each
// row it checked whose Approve + Merge is on now (CanMerge), with the check
// this run read. A run that lists none ends.
func (g *GitHub) mergeAllChecked() {
	a := &g.all
	if a.Phase != MergeAllChecking {
		return
	}
	g.mergeAllFeed()
	if a.Checking > 0 {
		return
	}
	// A check that counted another cross-review than the row's reads
	// again with the row's: one begun before pocketd's verdict came
	// counted none (Codex review of #53, round 1).
	for _, p := range mergeAllBacked(a.rows()) {
		ref, _ := MergeRef(p)
		key := ref.String()
		if m := g.merges[key]; m != nil && m.Phase == MergeChecked && m.cr != CrossReviewOf(p) && slices.Contains(a.keys, key) {
			a.pending = append(a.pending, key)
		}
	}
	if len(a.pending) > 0 {
		g.mergeAllFeed()
		if a.Checking > 0 {
			return
		}
	}
	var items []MergeAllItem
	listed := map[string]bool{}
	for _, p := range a.rows() {
		ref, ok := MergeRef(p)
		if !ok || listed[ref.String()] || !slices.Contains(a.keys, ref.String()) {
			continue
		}
		// Only a check this run read: an older one says what the gates
		// said before the owner asked.
		if m := g.merges[ref.String()]; m == nil || m.CheckID <= a.since {
			continue
		}
		// A dependency bot's risk:high pull request is listed without its
		// diff opened (ADR-0031, the owner's choice of 2026-11-21).
		if ok, _ := g.canMerge(p, true); !ok {
			continue
		}
		m := *g.merges[ref.String()]
		waived := WaivesDiff(m) && (m.AckHead != m.Preview.Head || m.AckBase != m.Preview.BaseOid)
		listed[ref.String()] = true
		items = append(items, MergeAllItem{Pull: p, Ref: ref, Merge: m, Note: g.MergeNote(ref), Waived: waived})
	}
	if len(items) == 0 {
		g.endMergeAll("no pull request's gates pass now: each card says why")
		return
	}
	a.Items, a.Phase = items, MergeAllConfirming
}

// ConfirmMergeAll is the owner's one confirmation of run id's list: its
// items are approved and merged one at a time, each read from pocketd
// fresh with read (Store.ReadPull) at its turn. A confirmation of another
// run, or of none, does nothing.
func (g *GitHub) ConfirmMergeAll(id int, read func(id int64, done func(fresh FreshPull, found bool, err error))) {
	a := &g.all
	if a.Phase != MergeAllConfirming || a.ID != id || read == nil {
		return
	}
	a.Phase, a.Turn, a.read = MergeAllMerging, 0, read
	g.log(LevelCommand, "Merge all ready: approve and merge "+strconv.Itoa(len(a.Items))+" pull requests as "+github.Login)
	g.mergeAllTurn()
}

// CancelMergeAll drops run id's list unconfirmed: nothing was sent.
func (g *GitHub) CancelMergeAll(id int) {
	if g.all.Phase == MergeAllConfirming && g.all.ID == id {
		g.all.MergeAll = MergeAll{}
	}
}

// StopMergeAll stops the run: one that checks ends at once, and one that
// merges ends once the merge under way ended. The rest are not tried.
func (g *GitHub) StopMergeAll() {
	switch g.all.Phase {
	case MergeAllChecking:
		g.endMergeAll("you stopped it: nothing was merged")
	case MergeAllMerging:
		g.all.Stopping = true
	}
}

// DismissMergeAll clears an ended run from view.
func (g *GitHub) DismissMergeAll() {
	if g.all.Phase == MergeAllEnded {
		g.all.MergeAll = MergeAll{}
	}
}

// endMergeAll ends the run, saying why when it ended early.
func (g *GitHub) endMergeAll(why string) {
	a := &g.all
	c := a.Counts()
	// A merge under way on GitHub may land: it is no untried one, and
	// ApproveAndMerge logs how it ends (Codex review of #53, round 5).
	underWay := ""
	if a.acting && a.Turn < len(a.Items) && !a.Items[a.Turn].Tried() {
		c.Untried--
		underWay = ", 1 under way on GitHub (" + a.Items[a.Turn].Ref.String() + ")"
	}
	a.Phase, a.End, a.Stopping, a.Checking = MergeAllEnded, why, false, 0
	a.rows, a.read, a.halt, a.acting = nil, nil, "", false
	text := "Merge all ready: " + c.String() + underWay
	if why != "" {
		text += ": " + why
	}
	g.log(LevelInfo, text)
}

// mergeAllNewCredential ends a run that has not begun merging when the
// sign-in changes: its checks are gone with the credential. A run that
// merges ends once the merge under way ended (mergeAllTurn).
func (g *GitHub) mergeAllNewCredential() {
	switch g.all.Phase {
	case MergeAllChecking, MergeAllConfirming:
		g.endMergeAll("the GitHub sign-in changed: nothing was merged")
	}
}

// PocketdSignInChanged ends a run begun under pocketd's sign-in before:
// its rows and its reads belong to that sign-in, and the store drops an
// answer of it, so a turn waiting on one would wait forever (Options.
// SignInChanged; Codex review of #53, round 1).
//
// The run, and its summary, leave the view at once, a merge under way on
// GitHub included: the next sign-in's Inbox never shows them (Codex review
// of #53, rounds 3 and 4). The log keeps how the run ended, and the merge
// under way ends on its own: ApproveAndMerge logs it, and its card shows it.
func (g *GitHub) PocketdSignInChanged() {
	switch g.all.Phase {
	case MergeAllChecking, MergeAllConfirming:
		g.dropMergeAll("pocketd's sign-in changed: nothing was merged")
	case MergeAllMerging:
		g.dropMergeAll("pocketd's sign-in changed: the rest were not tried")
	case MergeAllEnded:
		g.all.MergeAll = MergeAll{}
	}
}

// dropMergeAll ends the run, logs how, and clears it from view.
func (g *GitHub) dropMergeAll(why string) {
	g.endMergeAll(why)
	g.all.MergeAll = MergeAll{}
}

// mergeAllTurn starts the next item's turn, or ends the run.
func (g *GitHub) mergeAllTurn() {
	a := &g.all
	if a.Phase != MergeAllMerging {
		return
	}
	switch {
	case a.halt != "":
		g.endMergeAll(a.halt)
		return
	case g.cred != a.cred:
		g.endMergeAll("the GitHub sign-in changed: the rest were not tried")
		return
	case a.Stopping:
		g.endMergeAll("you stopped it: the rest were not tried")
		return
	case a.Turn >= len(a.Items):
		g.endMergeAll("")
		return
	}
	id, turn, item := a.ID, a.Turn, a.Items[a.Turn]
	// The pull request as pocketd has it now, read fresh: a verdict filed
	// since the list was read counts, as at a single confirmation.
	a.read(item.Pull.ID, func(fresh FreshPull, found bool, err error) {
		if g.all.Phase != MergeAllMerging || g.all.ID != id || g.all.Turn != turn {
			return
		}
		switch {
		case g.cred != g.all.cred || g.all.Stopping || g.all.halt != "":
			// Stopped, or signed in again, while pocketd answered: this
			// one is not tried, whatever the read found (Codex review of
			// #53, round 2).
			g.mergeAllTurn()
			return
		case err != nil:
			g.mergeAllEnded(id, turn, github.Result{Kind: "refused", Reason: "reading it from pocketd failed, nothing was sent: " + err.Error()})
			return
		case !found:
			g.mergeAllEnded(id, turn, github.Result{Kind: "refused", Reason: "pocketd no longer lists it: nothing was sent"})
			return
		}
		// state decides, as at a single confirmation: the check listed,
		// pocketd's answers backing its head, and the gates read again on
		// GitHub before each mutation.
		g.all.acting = true
		g.approveAndMerge(fresh, item.Merge, true, func(res github.Result) { g.mergeAllEnded(id, turn, res) })
	})
}

// mergeAllEnded records how turn of run id ended, and starts the next on
// the UI thread. A refusal before any mutation leaves the card's check as
// it was: the card then shows the refusal and its reason, as it shows an
// attempt's end, unless a check read since took its place.
func (g *GitHub) mergeAllEnded(id, turn int, res github.Result) {
	a := &g.all
	if a.Phase != MergeAllMerging || a.ID != id || a.Turn != turn {
		return
	}
	a.acting = false
	if res.Kind == "" {
		res.Kind = "failed"
	}
	item := &a.Items[turn]
	item.Result = res
	// GitHub's rate limit on the owner's token ends the run: the rest
	// would only meet it (Codex review of #53, round 4).
	if why := rateLimitOf(res.Err); why != "" {
		a.halt = why + ": the rest were not tried"
	}
	if m := g.merges[item.Ref.String()]; m != nil && m.Phase == MergeChecked && m.CheckID == item.Merge.CheckID {
		m.Phase, m.Result = MergeDone, res
	}
	if res.Op == "" {
		// Refused before any request went to GitHub: ApproveAndMerge logs
		// only an attempt.
		g.log(LevelInfo, item.Ref.String()+": "+res.Kind+resultReason(res))
	}
	a.Turn++
	g.opts.Post(g.mergeAllTurn)
}

// rateLimitOf says why GitHub refuses the owner's requests for now, "" when
// err is no rate limit: a 429, or a 403 that names when the limit ends.
func rateLimitOf(err error) string {
	// github.RateLimited knows each shape, a GraphQL answer's RATE_LIMITED
	// included (Codex review of #53, round 5).
	until, ok := github.RateLimited(err)
	switch {
	case !ok:
		return ""
	case until.IsZero():
		return "GitHub limits the owner's requests for now"
	}
	return "GitHub limits the owner's requests until " + until.UTC().Format("2006-01-02 15:04") + " UTC"
}

// mergeAllLimited ends a run that checks when a read of the gates met
// GitHub's rate limit on the owner's token: no more checks start (Codex
// review of #53, round 4).
func (g *GitHub) mergeAllLimited(err error) {
	if why := rateLimitOf(err); why != "" && g.all.Phase == MergeAllChecking {
		g.endMergeAll(why + ": nothing was merged")
	}
}
