package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/schedule"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// Launches (ADR-0034) start proposals of a supervisor's board as shift
// threads delegated from its standing session, in order, until one end:
// at most team.LaunchAtOnce of a launch's threads open at once, and the
// desk's open threads below its cap less team.LaunchReserve. The launch
// pass starts them; an item's one thread is created in the transaction
// that starts it (store.StartLaunchItem), and never another.

// launchCheck is how long a launch with items waiting waits for a pass of
// its own: a place a missed event freed is seen then.
const launchCheck = time.Minute

// launchesShown is how far back GET /v1/launches lists the launches that
// ended.
const launchesShown = 7 * 24 * time.Hour

// launchRefused is a launch a rule refuses: the caller's to change.
type launchRefused struct{ error }

func refuse(format string, args ...any) error { return launchRefused{fmt.Errorf(format, args...)} }

// StartLaunches starts the launch pass: a launch made before a stop goes
// on, and a start a stop interrupted is finished. It comes after Resume,
// as the schedules do: a start submits a brief.
func (s *Server) StartLaunches() {
	s.sessionMu.Lock()
	s.launchesOn = true
	s.sessionMu.Unlock()
	s.launchSoon()
}

// launchSoon runs a launch pass on its own goroutine, once the launches
// started; one asked for while a pass runs runs after it.
func (s *Server) launchSoon() {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	if s.stopped || !s.launchesOn {
		return
	}
	if s.launching {
		s.launchAgain = true
		return
	}
	s.launching = true
	s.goLaunch(func() {
		for {
			s.sessionMu.Lock()
			if s.stopped {
				s.launching = false
				s.sessionMu.Unlock()
				return
			}
			s.passes.Add(1)
			s.sessionMu.Unlock()
			s.launchPass(s.passCtx)
			s.passes.Done()
			s.sessionMu.Lock()
			again := s.launchAgain && !s.stopped
			s.launchAgain, s.launching = false, again
			s.sessionMu.Unlock()
			if !again {
				return
			}
		}
	})
}

// launchPass follows each launch that runs, and wakes at the soonest end
// of one, or after launchCheck while one runs.
func (s *Server) launchPass(ctx context.Context) {
	// A start a stop interrupted finishes first, with its one thread,
	// whatever its launch's state: its launch may have no item waiting.
	var next time.Time
	// The starts left unsettled first, a thread with a session included
	// (Codex review of #58, round 3).
	if !s.settleLaunchThreads(ctx) {
		next = s.now().Add(launchCheck)
	}
	if unstarted, err := s.launchUnstarted(ctx); err != nil {
		// A later pass reads them again, whatever launch runs (Codex
		// review of #58, round 1).
		s.log.Warn("read the launches' starting threads", "err", err)
		next = s.now().Add(launchCheck)
	} else {
		for _, thread := range unstarted {
			s.workMu.Lock()
			_, settling := s.launchSettle[thread.ID]
			s.workMu.Unlock()
			if settling {
				continue
			}
			// One that is not settled is looked at again by a later pass
			// (Codex review of #58, round 2).
			if !s.startLaunchThread(ctx, thread) {
				next = s.now().Add(launchCheck)
			}
		}
	}
	launches, err := s.store.RunningLaunches(ctx)
	if err != nil {
		s.log.Warn("read the launches", "err", err)
		s.armLaunches(s.now().Add(launchCheck))
		return
	}
	for _, l := range launches {
		if err := s.followLaunch(ctx, l); err != nil {
			s.log.Warn("follow a launch", "launch", l.Launch.ID, "err", err)
		}
		wake := s.now().Add(launchCheck)
		if l.Launch.End.Before(wake) {
			wake = l.Launch.End
		}
		if next.IsZero() || wake.Before(next) {
			next = wake
		}
	}
	s.armLaunches(next)
}

// armLaunches has a launch pass run at next, or none for zero.
func (s *Server) armLaunches(next time.Time) {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	if s.launchCancel != nil {
		s.launchCancel()
		s.launchCancel = nil
	}
	if s.stopped || next.IsZero() {
		return
	}
	s.launchCancel = s.launchTimer(max(next.Sub(s.now()), 0)+time.Second, s.launchSoon)
}

// followLaunch skips each item of facts that waits, past the launch's
// end; else starts items in order while the launch's and the desk's room
// lasts.
func (s *Server) followLaunch(ctx context.Context, facts store.LaunchFacts) error {
	var waiting []store.LaunchItemRow
	for _, item := range facts.Items {
		if item.State == team.LaunchItemWaiting {
			waiting = append(waiting, item)
		}
	}
	if len(waiting) == 0 {
		return nil
	}
	if !s.now().Before(facts.Launch.End) {
		for _, item := range waiting {
			if err := s.skipLaunchItem(ctx, item, team.LaunchEndPassed); err != nil {
				return err
			}
		}
		return nil
	}
	for _, item := range waiting {
		step, err := s.startLaunchItem(ctx, facts.Launch, item)
		if err != nil || step == itemWaits {
			return err
		}
		// A skipped item lets the next one start now (Codex review of
		// #58, round 1).
	}
	return nil
}

// launchStep is what startLaunchItem did with an item.
type launchStep int

const (
	itemWaits launchStep = iota
	itemStarted
	itemSkipped
)

// launchRoom is how many of launch id's items may start now
// (team.LaunchRoom); teamMu is held, so no delegation takes a place
// meanwhile.
func (s *Server) launchRoom(ctx context.Context, id int64) (int, error) {
	open, err := s.store.OpenThreads(ctx)
	if err != nil {
		return 0, err
	}
	running, err := s.store.LaunchThreadsOpen(ctx, id)
	if err != nil {
		return 0, err
	}
	return team.LaunchRoom(open, s.deskCap, running), nil
}

// startLaunchItem starts item of launch as its one thread, when its agent
// still takes the delegation in its repository and the launch and the desk
// have room: itemStarted. An agent that no longer takes it skips the item,
// and its proposal goes back on the board: itemSkipped. No room, or a
// check pocketd cannot make now, leaves it waiting: itemWaits.
func (s *Server) startLaunchItem(ctx context.Context, launch store.LaunchRow, item store.LaunchItemRow) (launchStep, error) {
	skip := func(why string) (launchStep, error) {
		if err := s.skipLaunchItem(ctx, item, why); err != nil {
			return itemWaits, err
		}
		return itemSkipped, nil
	}
	p, err := s.store.Proposal(ctx, item.ProposalID)
	if err != nil {
		return itemWaits, err
	}
	if s.profiles == nil {
		return skip("pocketd has no profiles")
	}
	target, err := s.profiles.Find(p.Agent)
	if profiles.Unreadable(err) {
		return itemWaits, fmt.Errorf("read %s's profile: %w", p.Agent, err)
	} else if err != nil {
		return skip(err.Error())
	}
	models, err := s.engine.Models(ctx)
	if err != nil {
		return itemWaits, err
	}
	if err := team.CheckTarget(target, func() error { return s.startable(target, models) }); err != nil {
		return skip(err.Error())
	}
	if err := team.CheckDestination(target, p.Repository); err != nil {
		return skip(err.Error())
	}
	s.teamMu.Lock()
	room, err := s.launchRoom(ctx, launch.ID)
	if err != nil || room == 0 {
		s.teamMu.Unlock()
		return itemWaits, err
	}
	row := store.ThreadRow{Title: p.Title, Agent: target.ID, Repository: p.Repository, Task: p.Task}
	s.workMu.Lock()
	thread, facts, proposal, started, err := s.store.StartLaunchItem(ctx, item.ID, row, store.ShiftRow{Stop: p.StopWhen, MaxRounds: p.MaxRounds})
	if err == nil {
		if started {
			s.publishThread(thread)
		}
		s.publishLaunchLocked(facts)
		if proposal.ID != 0 {
			s.publishOrderLocked(ctx, []store.ProposalRow{proposal})
		}
	}
	s.workMu.Unlock()
	s.teamMu.Unlock()
	switch {
	case err != nil:
		return itemWaits, err
	case !started:
		// It no longer waits, or its end came and it was skipped.
		return itemSkipped, nil
	}
	// A pass sets the shift's deadline wake, whatever its brief does.
	s.shiftSoon()
	s.startLaunchThread(ctx, thread)
	return itemStarted, nil
}

// startLaunchThread starts a launch item's thread (startThread), each step
// keyed, so a start a stop interrupted finishes with the same session.
// A start that fails is settled (settleLaunchThread), never tried again.
// The item never gets another thread (ADR-0034). While it runs, no
// delivery pass takes the thread over (launchStarting). settled is false
// when the thread is left for a later pass to settle (launchSettle).
func (s *Server) startLaunchThread(ctx context.Context, thread store.ThreadRow) (settled bool) {
	db := context.WithoutCancel(ctx)
	s.workMu.Lock()
	why, pending := s.launchSettle[thread.ID]
	s.launchStarting[thread.ID] = true
	s.workMu.Unlock()
	defer func() {
		s.workMu.Lock()
		delete(s.launchStarting, thread.ID)
		s.workMu.Unlock()
	}()
	if pending {
		return s.settleLaunchThread(db, thread.ID, why)
	}
	_, err := s.startThread(ctx, thread)
	if err == nil {
		return true
	}
	s.log.Warn("start a launch's thread", "thread", thread.ID, "err", err)
	if s.afterLaunchStart != nil {
		s.afterLaunchStart()
	}
	if s.stopping() {
		return true // the next start finishes it
	}
	why = team.Clip(err.Error(), team.MaxFailure)
	// Kept in the work database too, so a restart settles it, never briefs
	// or starts it (Codex review of #58, round 4).
	if err := s.store.MarkLaunchStartFailed(db, thread.ID, why); err != nil {
		s.log.Warn("keep a launch start's failure", "thread", thread.ID, "err", err)
	}
	return s.settleLaunchThread(db, thread.ID, why)
}

// settleLaunchThread settles launch thread id, whose start failed with
// why: one whose specialist may have its brief stays, and a delivery pass
// follows its brief and reports; one that surely has not is closed with
// why, as team_delegate's is. While pocketd cannot tell, or cannot close
// it yet, it stays in launchSettle with why, which no delivery pass takes
// over, and a later pass settles it again, never starting it (Codex review
// of #58, rounds 2 and 3).
func (s *Server) settleLaunchThread(ctx context.Context, id int64, why string) bool {
	keep := func() bool {
		s.workMu.Lock()
		s.launchSettle[id] = why
		s.workMu.Unlock()
		return false
	}
	current, err := s.store.Thread(ctx, id)
	if err != nil {
		return keep()
	}
	may, err := s.mayHaveBrief(ctx, current)
	if err != nil {
		return keep()
	}
	if !may {
		if err := s.failThread(ctx, id, why); err != nil {
			s.log.Warn("close a launch's thread that could not start", "thread", id, "err", err)
			return keep()
		}
	}
	if err := s.store.ClearLaunchStartFailed(ctx, id); err != nil && may {
		return keep()
	}
	s.workMu.Lock()
	delete(s.launchSettle, id)
	s.workMu.Unlock()
	if may {
		// Its brief may be in, or on its way: delivery follows it now.
		s.deliverLater()
	}
	return true
}

// settleLaunchThreads settles each launch start left unsettled, whatever
// its thread's session; done says none is left.
func (s *Server) settleLaunchThreads(ctx context.Context) (done bool) {
	s.workMu.Lock()
	pending := maps.Clone(s.launchSettle)
	s.workMu.Unlock()
	done = true
	// Those a stop of pocketd left unsettled, from the work database.
	kept, err := s.store.LaunchStartFailures(ctx)
	if err != nil {
		s.log.Warn("read the launches' failed starts", "err", err)
		done = false
	}
	for id, why := range kept {
		if _, ok := pending[id]; !ok {
			pending[id] = why
		}
	}
	for id, why := range pending {
		if !s.settleLaunchThread(ctx, id, why) {
			done = false
		}
	}
	return done
}

// skipLaunchItem skips item with why: its proposal goes back on the board.
func (s *Server) skipLaunchItem(ctx context.Context, item store.LaunchItemRow, why string) error {
	s.workMu.Lock()
	defer s.workMu.Unlock()
	facts, proposal, did, err := s.store.SkipLaunchItem(ctx, item.ID, why)
	if err != nil || !did {
		return err
	}
	s.publishLaunchLocked(facts)
	s.publishOrderLocked(ctx, []store.ProposalRow{proposal})
	return nil
}

// planLaunch checks proposals ids for a launch until end (ADR-0034): each
// proposed, all on one supervisor's board (profile's, when profile is not
// ""), their rounds within the launch's budget, each agent one that takes
// the delegation in its repository; and opens the board's supervisor's
// standing session, which the threads are delegated from. A rule that
// does not hold is launchRefused.
func (s *Server) planLaunch(ctx context.Context, ids []int64, end time.Time, profile string) (store.LaunchStart, error) {
	if err := team.CheckLaunchRows(ids); err != nil {
		return store.LaunchStart{}, launchRefused{err}
	}
	if err := team.CheckLaunchEnd(end, s.now()); err != nil {
		return store.LaunchStart{}, launchRefused{err}
	}
	if s.profiles == nil {
		return store.LaunchStart{}, refuse("pocketd has no profiles, so no agent takes a launch")
	}
	models, err := s.engine.Models(ctx)
	if err != nil {
		return store.LaunchStart{}, err
	}
	var rounds []int
	revs := map[int64]int{}
	for _, id := range ids {
		p, err := s.store.Proposal(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			return store.LaunchStart{}, refuse("there is no proposal #%d", id)
		} else if err != nil {
			return store.LaunchStart{}, err
		}
		board, _, err := s.store.ProposalBoard(ctx, id)
		if err != nil {
			return store.LaunchStart{}, err
		}
		switch {
		case profile == "":
			profile = board
		case board != profile:
			return store.LaunchStart{}, refuse("#%d is on another supervisor's board: a launch takes one supervisor's proposals", id)
		}
		if p.State != team.ProposalProposed {
			return store.LaunchStart{}, refuse("#%d is %s: only a proposed one launches", id, p.State)
		}
		target, err := s.profiles.Find(p.Agent)
		if profiles.Unreadable(err) {
			return store.LaunchStart{}, fmt.Errorf("read %s's profile: %w", p.Agent, err)
		} else if err != nil {
			return store.LaunchStart{}, refuse("#%d: %v", id, err)
		}
		if err := team.CheckTarget(target, func() error { return s.startable(target, models) }); err != nil {
			return store.LaunchStart{}, refuse("#%d: %v", id, err)
		}
		if err := team.CheckDestination(target, p.Repository); err != nil {
			return store.LaunchStart{}, refuse("#%d: %v", id, err)
		}
		rounds = append(rounds, p.MaxRounds)
		// The rev checked here is the one the launch records: an edit
		// meanwhile refuses it (Codex review of #58, round 1).
		revs[id] = p.Rev
	}
	if err := team.CheckLaunchRounds(rounds); err != nil {
		return store.LaunchStart{}, launchRefused{err}
	}
	supervisor, err := s.profiles.Find(profile)
	if profiles.Unreadable(err) {
		return store.LaunchStart{}, fmt.Errorf("read %s's profile: %w", profile, err)
	} else if err != nil {
		return store.LaunchStart{}, refuse("the board's supervisor %s: %v", profile, err)
	}
	if supervisor.Role != profiles.RoleSupervisor || !supervisor.Standing {
		return store.LaunchStart{}, refuse("the board's supervisor %s keeps no standing session (session: standing): a launch is delegated from it", profile)
	}
	// The read that opens or finds the standing session checks it is still
	// a supervisor's (Codex review of #58, round 1).
	session, why, err := s.launchStanding(ctx, profile, true)
	switch {
	case errors.Is(err, errInternal) || errors.Is(err, errNotNow):
		return store.LaunchStart{}, err
	case err != nil:
		// The profile cannot start one: the owner's to fix (Codex review
		// of #58, round 2).
		return store.LaunchStart{}, refuse("pocketd could not open %s's standing session: %v", supervisor.Name, err)
	case why != "":
		return store.LaunchStart{}, refuse("%s", why)
	}
	if s.afterLaunchPlan != nil {
		s.afterLaunchPlan()
	}
	return store.LaunchStart{SupervisorSession: session, Profile: profile, End: end, Proposals: ids, Revs: revs}, nil
}

// createLaunch records launch start (store.CreateLaunch), and publishes
// it and its proposals with it, under workMu; a new one wakes the launch
// pass, and the delivery pass for its note.
func (s *Server) createLaunch(ctx context.Context, start store.LaunchStart) (store.LaunchFacts, error) {
	s.workMu.Lock()
	facts, proposals, replayed, err := s.store.CreateLaunch(ctx, start)
	if err == nil && !replayed {
		s.publishLaunchLocked(facts)
		// A launch changes no gather's counts: its proposals go alone, each
		// gather's start read once (Codex review of #58, round 3).
		s.publishOrderLocked(ctx, proposals)
	}
	s.workMu.Unlock()
	if err != nil {
		return store.LaunchFacts{}, err
	}
	if !replayed {
		s.launchSoon()
		if start.Note != nil {
			s.deliverLater()
		}
	}
	return facts, nil
}

// launchLines are a launch's proposals, as its texts name them.
func launchLines(f store.LaunchFacts) []team.LaunchLine {
	lines := make([]team.LaunchLine, len(f.Items))
	for i, item := range f.Items {
		lines[i] = team.LaunchLine{ID: item.ProposalID, Agent: item.Agent, Title: item.Title}
	}
	return lines
}

// TeamLaunch is team_launch (ADR-0034): the supervisor's launch of rows of
// its board, until hours from now or a clock time, DefaultLaunchHours by
// default. It is recorded once per call (api.TeamCall), and the call run
// again answers as the first did.
func (s *Server) TeamLaunch(ctx context.Context, call TeamCall, c team.LaunchCall) (string, error) {
	// The call's own context bounds the checks and the record: a run the
	// owner aborted before the launch committed launches nothing (Codex
	// review of #58, round 3). A launch recorded already answers by key.
	live := ctx
	ctx = context.WithoutCancel(ctx)
	if facts, err := s.store.LaunchByKey(ctx, call.key()); err == nil {
		return team.LaunchedText(facts.Launch.ID, schedule.When(facts.Launch.End, s.zone), launchLines(facts)), nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	if err := team.CheckLaunchRows(c.Rows); err != nil {
		return "", err
	}
	caller, err := s.callerProfile(ctx, call.Session)
	if err != nil {
		return "", err
	}
	if caller == nil || caller.Role != profiles.RoleSupervisor {
		return "", errors.New("only a supervisor launches proposals: this session's profile is not one")
	}
	end, err := team.LaunchEnd(c.Hours, c.Until, s.now(), s.zone)
	if err != nil {
		return "", err
	}
	start, err := s.planLaunch(live, c.Rows, end, caller.ID)
	if err != nil {
		return "", launchError(err)
	}
	start.Key, start.By = call.key(), team.LaunchBySupervisor
	facts, err := s.createLaunch(live, start)
	if err != nil && live.Err() != nil {
		return "", fmt.Errorf("the launch was stopped: %w", live.Err())
	} else if err != nil {
		return "", launchError(err)
	}
	return team.LaunchedText(facts.Launch.ID, schedule.When(facts.Launch.End, s.zone), launchLines(facts)), nil
}

// launchError is err as a caller reads it.
func launchError(err error) error {
	var refused launchRefused
	if errors.As(err, &refused) {
		return refused.error
	}
	return conflictText(err)
}

// launchKind is the kind of the owner's launch request: its proposals in
// order and its end, so the same request id for another launch is
// refused.
func launchKind(request proto.CreateLaunch) string {
	ids := make([]string, len(request.Proposals))
	for i, id := range request.Proposals {
		ids[i] = strconv.FormatInt(id, 10)
	}
	// The end whole, whatever precision the launch keeps (Codex review of
	// #58, round 1).
	return "launch:" + strings.Join(ids, ",") + "@" + request.End.UTC().Format(time.RFC3339Nano)
}

// ownerLaunch answers POST /v1/launches: the owner's launch, recorded once
// per request id (rule 59).
func (s *Server) ownerLaunch(w http.ResponseWriter, r *http.Request) {
	var request proto.CreateLaunch
	if !decode(w, r, &request) || !validRequestID(w, request.RequestID) {
		return
	}
	ctx := context.WithoutCancel(r.Context())
	key := requestKey(r, request.RequestID)
	defer s.keys.lock(key)()
	kind := launchKind(request)
	// A launch made already answers as it did, whatever the board says now.
	if receipt, existed, err := s.store.LookupRequest(ctx, key, kind); errors.Is(err, store.ErrRequestReused) {
		reused(w)
		return
	} else if err != nil {
		s.internal(w, "launch", err)
		return
	} else if existed && receipt != "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, receipt)
		return
	}
	start, err := s.planLaunch(ctx, request.Proposals, request.End, "")
	if err == nil {
		start.Key, start.RequestKind, start.By = key, kind, team.LaunchByOwner
		start.Note = func(f store.LaunchFacts) string {
			return team.LaunchNote(f.Launch.ID, schedule.When(f.Launch.End, s.zone), launchLines(f))
		}
		start.Receipt = func(f store.LaunchFacts) string {
			encoded, err := json.Marshal(wireLaunch(f))
			if err != nil {
				return ""
			}
			return string(encoded)
		}
		var facts store.LaunchFacts
		if facts, err = s.createLaunch(ctx, start); err == nil {
			writeJSON(w, http.StatusCreated, wireLaunch(facts))
			return
		}
	}
	var refused launchRefused
	switch {
	case errors.As(err, &refused):
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, refused.Error())
	case errors.Is(err, store.ErrRequestReused):
		reused(w)
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, conflictText(err).Error())
	default:
		s.internal(w, "launch", err)
	}
}

// ownerDropProposal answers POST /v1/proposals/{id}/drop: the owner's drop
// of a proposed proposal at the rev the board shows, recorded once per
// request id (rule 59).
func (s *Server) ownerDropProposal(w http.ResponseWriter, r *http.Request) {
	id, ok := pathIDOf(w, r, "proposal")
	if !ok {
		return
	}
	var request proto.DropProposal
	if !decode(w, r, &request) || !validRequestID(w, request.RequestID) {
		return
	}
	ctx := context.WithoutCancel(r.Context())
	key := requestKey(r, request.RequestID)
	defer s.keys.lock(key)()
	// The request is claimed in the drop's transaction, and the change
	// keyed by it (proposal_calls): a retry answers as the drop did, and
	// the id names this drop alone (Codex review of #58, round 1).
	kind := "dropProposal:" + strconv.FormatInt(id, 10) + "@" + strconv.Itoa(request.Rev)
	s.workMu.Lock()
	row, _, replayed, err := s.dropChange(ctx, store.ProposalChange{Key: key, ID: id, Rev: request.Rev, RequestKind: kind,
		Change: func(p store.ProposalRow) (store.ProposalRow, error) { p.State = team.ProposalDropped; return p, nil },
		Answer: func(p store.ProposalRow) string { return fmt.Sprintf("Dropped #%d: it leaves the board.", p.ID) }})
	if err == nil && !replayed {
		s.publishProposalLocked(ctx, row)
	}
	s.workMu.Unlock()
	switch {
	case err == nil:
	case errors.Is(err, store.ErrRequestReused):
		reused(w)
		return
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, fmt.Sprintf("there is no proposal #%d", id))
		return
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, conflictText(err).Error())
		return
	default:
		s.internal(w, "drop a proposal", err)
		return
	}
	p, err := s.store.Proposal(ctx, id)
	if err != nil {
		s.internal(w, "read a proposal", err)
		return
	}
	_, gathered, err := s.store.ProposalBoard(ctx, id)
	if err != nil {
		s.internal(w, "read a proposal", err)
		return
	}
	writeJSON(w, http.StatusOK, wireProposal(p, gathered))
}

// publishLaunchLocked sends a launch as facts read it; workMu is held, so
// launch events leave in the order the launches changed.
func (s *Server) publishLaunchLocked(facts store.LaunchFacts) {
	l := wireLaunch(facts)
	s.emit(proto.Event{Type: proto.EventLaunch, Launch: &l})
}

func wireLaunch(f store.LaunchFacts) proto.Launch {
	l := proto.Launch{ID: f.Launch.ID, Supervisor: f.Launch.SupervisorSession, Profile: f.Launch.Profile, End: f.Launch.End, State: proto.LaunchState(f.Launch.State),
		By: proto.LaunchBy(f.Launch.By), CreatedAt: f.Launch.CreatedAt, EndedAt: f.Launch.EndedAt, Items: []proto.LaunchItem{}}
	for _, item := range f.Items {
		l.Items = append(l.Items, proto.LaunchItem{Proposal: item.ProposalID, Agent: item.Agent, Title: item.Title, State: proto.LaunchItemState(item.State), Thread: item.ThreadID, Why: item.Why})
	}
	return l
}

// listLaunches answers GET /v1/launches: the launches that run, and those
// that ended in the last launchesShown, the newest first.
func (s *Server) listLaunches(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.LaunchesSince(r.Context(), s.now().Add(-launchesShown))
	if err != nil {
		s.internal(w, "list launches", err)
		return
	}
	out := make([]proto.Launch, 0, len(rows))
	for _, f := range rows {
		out = append(out, wireLaunch(f))
	}
	writeJSON(w, http.StatusOK, out)
}
