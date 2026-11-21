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
	// keys are the pull requests the run checks; rows reads pocketd's rows
	// as they are now; read is Store.ReadPull.
	keys []string
	rows func() []proto.PullRequest
	read func(id int64, done func(fresh FreshPull, found bool, err error))
	// halt is why the run ends once the merge under way ended; "" while
	// it goes on.
	halt string
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

// mergeAllCandidates are the rows a run checks: pull requests that wait
// for review, that pocketd backs at the head and base branch it read, and
// whose merge is not running. pocketd backs no other, so CanMerge refuses
// every other whatever GitHub says (Backed).
func (g *GitHub) mergeAllCandidates(rows []proto.PullRequest) []proto.PullRequest {
	var out []proto.PullRequest
	seen := map[string]bool{}
	for _, p := range rows {
		ref, ok := MergeRef(p)
		if !ok || !ToReview(p) || !Backed(p, p.HeadSHA, p.Base) {
			continue
		}
		key := ref.String()
		if seen[key] || g.inFlight[key] > 0 {
			continue
		}
		if m := g.merges[key]; m != nil && m.Phase == MergeActing {
			continue
		}
		seen[key] = true
		out = append(out, p)
	}
	return out
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
	case len(g.mergeAllCandidates(rows)) == 0:
		return false, "pocketd backs no pull request here now: none has a ready cross-review with green CI at its head"
	}
	return true, ""
}

// MergeAllReady begins Merge All Ready: it reads the gates of every pull
// request of rows() pocketd backs, as Check to Merge does, all at once.
// Once every read ended, the run lists those whose Approve + Merge is on
// then, from rows() as they are then, and waits for ConfirmMergeAll.
func (g *GitHub) MergeAllReady(rows func() []proto.PullRequest) {
	if ok, _ := g.CanMergeAll(rows()); !ok {
		return
	}
	cands := g.mergeAllCandidates(rows())
	g.all.runs++
	g.all.MergeAll = MergeAll{Phase: MergeAllChecking, ID: g.all.runs}
	g.all.cred, g.all.rows, g.all.read, g.all.halt = g.cred, rows, nil, ""
	g.all.keys = g.all.keys[:0]
	for _, p := range cands {
		ref, _ := MergeRef(p)
		g.all.keys = append(g.all.keys, ref.String())
	}
	g.log(LevelCommand, "Merge all ready: check "+strconv.Itoa(len(cands))+" pull requests")
	for _, p := range cands {
		g.Check(p)
	}
	g.mergeAllChecked()
}

// mergeAllChecked lists what passes once the run's last read ended: each
// row it checked whose Approve + Merge is on now (CanMerge), with the check
// it shows. A run that lists none ends.
func (g *GitHub) mergeAllChecked() {
	a := &g.all
	if a.Phase != MergeAllChecking {
		return
	}
	waiting := 0
	for _, key := range a.keys {
		if m := g.merges[key]; m != nil && m.Phase == MergeChecking {
			waiting++
		}
	}
	a.Checking, a.Checked = waiting, len(a.keys)-waiting
	if waiting > 0 {
		return
	}
	// A check that counted another cross-review than the row's reads
	// again with the row's: one begun before pocketd's verdict came
	// counted none (Codex review of #53, round 1).
	for _, p := range a.rows() {
		ref, ok := MergeRef(p)
		if !ok || !slices.Contains(a.keys, ref.String()) || !Backed(p, p.HeadSHA, p.Base) {
			continue
		}
		if m := g.merges[ref.String()]; m != nil && m.Phase == MergeChecked && m.cr != CrossReviewOf(p) {
			g.Check(p)
			if m.Phase == MergeChecking {
				waiting++
			}
		}
	}
	if waiting > 0 {
		a.Checking, a.Checked = waiting, len(a.keys)-waiting
		return
	}
	var items []MergeAllItem
	listed := map[string]bool{}
	for _, p := range a.rows() {
		ref, ok := MergeRef(p)
		if !ok || listed[ref.String()] || !slices.Contains(a.keys, ref.String()) {
			continue
		}
		if ok, _ := g.CanMerge(p); !ok {
			continue
		}
		listed[ref.String()] = true
		items = append(items, MergeAllItem{Pull: p, Ref: ref, Merge: *g.merges[ref.String()], Note: g.MergeNote(ref)})
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
	a.Phase, a.End, a.Stopping, a.Checking = MergeAllEnded, why, false, 0
	a.rows, a.read, a.halt = nil, nil, ""
	text := "Merge all ready: " + a.Counts().String()
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
// answer of it, so a turn waiting on one would wait forever. A run that
// checks or waits for its confirmation ends at once. One that merges ends
// at once while its turn reads pocketd, and once the merge under way ended
// while one runs on GitHub, so its end is recorded (Options.SignInChanged;
// Codex review of #53, round 1).
func (g *GitHub) PocketdSignInChanged() {
	a := &g.all
	switch a.Phase {
	case MergeAllChecking, MergeAllConfirming:
		g.endMergeAll("pocketd's sign-in changed: nothing was merged")
	case MergeAllMerging:
		why := "pocketd's sign-in changed: the rest were not tried"
		if a.Turn < len(a.Items) && g.inFlight[a.Items[a.Turn].Ref.String()] > 0 {
			a.halt = why
			return
		}
		g.endMergeAll(why)
	}
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
		case err != nil:
			g.mergeAllEnded(id, turn, github.Result{Kind: "refused", Reason: "reading it from pocketd failed, nothing was sent: " + err.Error()})
			return
		case !found:
			g.mergeAllEnded(id, turn, github.Result{Kind: "refused", Reason: "pocketd no longer lists it: nothing was sent"})
			return
		case g.cred != g.all.cred || g.all.Stopping:
			// Stopped, or signed in again, while pocketd answered: this
			// one is not tried.
			g.mergeAllTurn()
			return
		}
		// state decides, as at a single confirmation: the check listed,
		// pocketd's answers backing its head, and the gates read again on
		// GitHub before each mutation.
		g.ApproveAndMerge(fresh, item.Merge, func(res github.Result) { g.mergeAllEnded(id, turn, res) })
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
	if res.Kind == "" {
		res.Kind = "failed"
	}
	item := &a.Items[turn]
	item.Result = res
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
