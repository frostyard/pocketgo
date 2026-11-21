package state

import (
	"slices"
	"strconv"
	"strings"

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
//
// A merge moves its base branch, and a dependency bot's pull request listed
// without its diff opened merges only at the base commit its check read
// (WaivesDiff). So once a turn merged, each such pull request of the run
// on that base is checked again at its own turn, at the new base, through
// every gate, before it merges (ADR-0038, the owner's choice of
// 2026-11-21).

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
	// Recheck says an earlier turn merged into its base, which its waived
	// merge is bound to: its turn checks it again at the new base first
	// (ADR-0038).
	Recheck bool
	// Rechecked says its turn checked it again: Merge is that check, when
	// it passed.
	Rechecked bool
	// Moved says an earlier turn of the run merged into its base, seen to
	// land on the branch GitHub named (github.Result.MergedInto): one whose
	// end is unknown may have, which is enough to check it again (Recheck),
	// never to put a conflict down to the run (ADR-0039).
	Moved bool
	// Rebase is the dependency bot it waits for to rebase it, by name
	// ("Dependabot"), when its turn stopped as it conflicts with the base
	// an earlier turn moved, or is behind it (rebaseWait); "" otherwise.
	Rebase string
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
	// Rechecking says the turn's item is checked again on GitHub, at the
	// base an earlier turn moved, before it merges (ADR-0038).
	Rechecking bool
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
	// Rechecked are those checked again at a new base, however they ended
	// (ADR-0038).
	Rechecked int
	// Rebase are those that wait for their dependency bot to rebase them,
	// apart from those that stayed, and RebaseBy names those bots, joined
	// with "or" (the owner's choice of 2026-11-21).
	Rebase   int
	RebaseBy string
}

// String is the counts in words: those in the queue, those whose end is
// unknown, those that wait for a rebase and those checked again only when
// there are some.
func (c MergeAllCounts) String() string {
	out := strconv.Itoa(c.Merged) + " merged, "
	if c.Queued > 0 {
		out += strconv.Itoa(c.Queued) + " in the merge queue, "
	}
	if c.Unknown > 0 {
		out += strconv.Itoa(c.Unknown) + " to check again, "
	}
	switch {
	case c.Rebase == 1:
		out += "1 waits for " + c.RebaseBy + " to rebase, "
	case c.Rebase > 1:
		out += strconv.Itoa(c.Rebase) + " wait for " + c.RebaseBy + " to rebase, "
	}
	out += strconv.Itoa(c.Stayed) + " stayed, " + strconv.Itoa(c.Untried) + " not tried"
	if c.Rechecked > 0 {
		out += "; " + strconv.Itoa(c.Rechecked) + " checked again at a new base"
	}
	return out
}

// Counts are how the run's items ended.
func (m MergeAll) Counts() MergeAllCounts {
	var c MergeAllCounts
	for _, it := range m.Items {
		if it.Rechecked {
			c.Rechecked++
		}
		switch {
		case !it.Tried():
			c.Untried++
		case it.Result.Kind == "merged":
			c.Merged++
		case it.Result.Kind == "queued":
			c.Queued++
		case it.Result.Kind == "unknown":
			c.Unknown++
		case it.Rebase != "":
			c.Rebase++
			if !slices.Contains(strings.Split(c.RebaseBy, " or "), it.Rebase) {
				c.RebaseBy = strings.TrimPrefix(c.RebaseBy+" or "+it.Rebase, " or ")
			}
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
	// wait is the check of the turn's item the run waits for while it
	// checks it again (ADR-0038): one begun from its card before, which ends
	// first, or the run's own, begun at the new base once checks was from,
	// when own. Only its end goes on (Codex review of #60, round 1).
	wait *Merge
	own  bool
	from int
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
	g.all.wait, g.all.own, g.all.from = nil, false, 0
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
		listed[ref.String()] = true
		items = append(items, MergeAllItem{Pull: p, Ref: ref, Merge: m, Note: g.MergeNote(ref), Waived: waived(m)})
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
	a.Phase, a.End, a.Stopping, a.Checking, a.Rechecking = MergeAllEnded, why, false, 0, false
	a.rows, a.read, a.halt, a.acting, a.wait, a.own = nil, nil, "", false, nil, false
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
	case a.Items[a.Turn].Recheck && !a.Items[a.Turn].Rechecked:
		// An earlier turn moved the base its waived merge is bound to: it
		// is checked again there first (ADR-0038).
		a.Rechecking, a.wait, a.own = true, nil, false
		g.mergeAllRecheck(nil)
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
	// A dependency bot's pull request this run's merges left in conflict,
	// or behind, waits for its bot to rebase it: no failure (the owner's
	// choice of 2026-11-21).
	item.Rebase = rebaseWait(*item, res)
	// A merge moved its base, or may have: each waived pull request of the
	// run on that base is checked again at its turn (ADR-0038).
	if movesBase(res) {
		for j := turn + 1; j < len(a.Items); j++ {
			if it := &a.Items[j]; !it.Tried() && sameBase(it.Merge.Preview, item.Merge.Preview) {
				it.Recheck = it.Recheck || it.Waived
			}
		}
	}
	// Only a merge seen to land moved a base for certain, and only the
	// branch GitHub says it merged into: a conflict is put down to the
	// run's own merge only then (Codex review of #64, rounds 1 and 2).
	if res.Kind == "merged" && res.MergedInto != "" {
		into := item.Merge.Preview
		into.BaseRef = res.MergedInto
		for j := turn + 1; j < len(a.Items); j++ {
			if it := &a.Items[j]; !it.Tried() && sameBase(it.Merge.Preview, into) {
				it.Moved = true
			}
		}
	}
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
// review of #53, round 4). A run that checks its turn's item again at a new
// base ends once that check ended: no more turns start (ADR-0038).
func (g *GitHub) mergeAllLimited(err error) {
	why := rateLimitOf(err)
	switch {
	case why == "":
	case g.all.Phase == MergeAllChecking:
		g.endMergeAll(why + ": nothing was merged")
	case g.all.Phase == MergeAllMerging && g.all.Rechecking:
		g.all.halt = why + ": the rest were not tried"
	}
}

// mergeAllCheckEnded is what a run does when the read of the gates into m
// ended: list what passes once its checks ended, or go on with a turn whose
// item it checked again.
func (g *GitHub) mergeAllCheckEnded(m *Merge) {
	g.mergeAllChecked()
	g.mergeAllRecheck(m)
}

// waived says a run's list takes m without its diff opened: a dependency
// bot's risk:high pull request whose diff the owner did not open at the
// check's base and head (WaivesDiff).
func waived(m Merge) bool {
	return WaivesDiff(m) && (m.AckHead != m.Preview.Head || m.AckBase != m.Preview.BaseOid)
}

// movesBase says a turn that ended with res moved its base branch, or may
// have: a merge, or a merge whose end is unknown. An approval whose end is
// unknown sent no merge (Codex review of #60, round 1), and an enqueue moves
// the base only when the merge queue merges, which the run does not see
// (ADR-0038).
func movesBase(res github.Result) bool {
	return res.Kind == "merged" || res.Kind == "unknown" && (res.Step == "merge" || res.Step == "reconcile merge")
}

// rebaseReasons are the gates' reasons (github.Check) for a pull request
// whose base moved under it: it conflicts with the base now, or is behind
// it where the rules need it up to date, or GitHub has not computed its
// mergeability or merge state at the new base yet (the owner's choice of
// 2026-11-21).
var rebaseReasons = []string{"the PR has conflicts", "the PR is behind its base", "GitHub has not computed mergeability",
	"GitHub has not computed the merge state"}

// rebaseWait is the dependency bot an item whose turn ended with res waits
// for to rebase it, by name, "" when it waits for none (the owner's choice
// of 2026-11-21): a bot's pull request that an earlier turn of the run
// moved the base of (Moved), refused or approved without a merge because it
// conflicts with that base now or is behind it, or because GitHub has not
// computed its mergeability or merge state there yet. One that conflicted, or was
// behind, before the run began failed its check and was never listed, and
// one whose base no turn of the run moved stays a failure.
//
// The read whose gates refused it must be of the head and base branch the
// owner confirmed: a pull request retargeted since conflicts with a branch
// the run never moved (Codex review of #64, round 1).
func rebaseWait(it MergeAllItem, res github.Result) string {
	p := it.Merge.Preview
	if !it.Moved || !p.DependencyBot || p.Bot == "" || res.Kind != "refused" && res.Kind != "approved" ||
		!slices.Contains(rebaseReasons, res.Reason) || res.Head != p.Head || res.Base != p.BaseRef {
		return ""
	}
	return botName(p.Bot)
}

// botName is a dependency bot of github.DependencyBotOf's list as its maker
// writes it.
func botName(bot string) string {
	switch bot {
	case "dependabot":
		return "Dependabot"
	case "renovate":
		return "Renovate"
	}
	return bot
}

// sameBase says two checks read pull requests that merge into the same
// branch of the same repository. GitHub names a repository in any case.
func sameBase(a, b github.Preview) bool {
	return strings.EqualFold(a.Ref.Owner, b.Ref.Owner) && strings.EqualFold(a.Ref.Name, b.Ref.Name) && a.BaseRef == b.BaseRef
}

// mergeAllRow is the row of key pocketd backs now, as the run's checks
// read it.
func (g *GitHub) mergeAllRow(key string) (proto.PullRequest, bool) {
	for _, p := range mergeAllBacked(g.all.rows()) {
		if ref, _ := MergeRef(p); ref.String() == key {
			return p, true
		}
	}
	return proto.PullRequest{}, false
}

// mergeAllRecheck checks the turn's item again on GitHub at the base an
// earlier turn moved (ADR-0038): every gate, with its row as pocketd backs
// it now, as the run's first checks read them. ended is the check that just
// ended, nil at the turn's start: the run goes on only once the check it
// waits for ended, whatever other checks end meanwhile (Codex review of
// #60, round 1). Only the run's own check counts: one begun from its card
// ends first, since it may have read the base before it moved. Once its
// own ended, the turn goes on with it, a waived merge bound to the base it
// read, or the item stays with why not. An item is checked again at its own
// turn only, so once at most, and the run always ends.
func (g *GitHub) mergeAllRecheck(ended *Merge) {
	a := &g.all
	if a.Phase != MergeAllMerging || !a.Rechecking || ended != a.wait {
		return
	}
	a.wait = nil
	id, turn := a.ID, a.Turn
	item := &a.Items[turn]
	key := item.Ref.String()
	if g.cred != a.cred || a.Stopping || a.halt != "" {
		// Stopped, signed in again or limited while it checked: this one
		// is not tried, as at a read of pocketd.
		a.Rechecking = false
		g.mergeAllTurn()
		return
	}
	stay := func(why string) {
		a.Rechecking = false
		g.mergeAllEnded(id, turn, github.Result{Kind: "refused", Reason: why})
	}
	if !a.own {
		if m := g.merges[key]; m != nil && m.Phase == MergeChecking {
			a.wait = m // its card's check ends first
			return
		}
		p, ok := g.mergeAllRow(key)
		switch {
		case !ok:
			stay("pocketd no longer backs it: nothing was sent")
			return
		case g.merging(p):
			stay("an approve and merge of it is running: nothing was sent")
			return
		}
		from := g.checks
		g.log(LevelCommand, "Merge all ready: check "+key+" again, at the base an earlier merge moved")
		g.Check(p)
		m := g.merges[key]
		if m == nil || m.Phase != MergeChecking {
			stay("it could not be checked again: nothing was sent")
			return
		}
		a.wait, a.own, a.from = m, true, from
		item.Rechecked = true
		return
	}
	m := ended
	why := ""
	switch p, ok := g.mergeAllRow(key); {
	case m.Err != "":
		why = m.Err
	case m.Phase != MergeChecked || m.CheckID <= a.from:
		why = "it could not be checked again: check it again"
	case !ok:
		why = "pocketd no longer backs it: nothing was sent"
	// The merge holds the sign-in's token the confirmation's check read
	// with, as ApproveAndMerge does: a refresh since is another (Codex
	// review of #60, round 1).
	case m.tokenGen != item.Merge.tokenGen:
		why = "the GitHub sign-in changed since the check: check it again"
	default:
		why = changedSince(item.Merge.Preview, m.Preview)
		if why == "" {
			_, why = g.canMerge(p, true)
		}
	}
	if why != "" {
		res := github.Result{Kind: "refused", Reason: why}
		// Its card keeps the refusal, against the run's own check (Codex
		// review of #60, round 1), which names the head and base branch it
		// read (ADR-0039).
		if m.Phase == MergeChecked && m.CheckID > a.from {
			res.Head, res.Base = m.Preview.Head, m.Preview.BaseRef
			m.Phase, m.Result = MergeDone, res
		}
		a.Rechecking = false
		g.mergeAllEnded(id, turn, res)
		return
	}
	a.Rechecking = false
	item.Merge, item.Waived, item.Note = *m, waived(*m), g.MergeNote(item.Ref)
	g.mergeAllTurn()
}

// changedSince says how a check read again at a new base differs from the
// one the owner confirmed, "" when it differs in its base commit alone
// (ADR-0038): the pull request, its head and base branch, the method and
// the queue, risk:high and a dependency bot's authorship stay as the owner
// confirmed them.
func changedSince(confirmed, now github.Preview) string {
	switch {
	case now.Ref != confirmed.Ref || now.NodeID != confirmed.NodeID:
		return "this is not the pull request you confirmed"
	case now.Head != confirmed.Head:
		return "new commit since you confirmed (" + shortSHA(now.Head) + "): check it again"
	case now.BaseRef != confirmed.BaseRef:
		return "the base branch changed to " + now.BaseRef + " since you confirmed: check it again"
	case now.Method != confirmed.Method || now.Queue != confirmed.Queue:
		return "the merge method changed since you confirmed: check it again"
	case now.RiskHigh != confirmed.RiskHigh:
		return "risk:high changed since you confirmed: check it again"
	case now.DependencyBot != confirmed.DependencyBot:
		return "its author is no dependency bot of the list now: check it again"
	}
	return ""
}
