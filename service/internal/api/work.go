package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/router"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
	"github.com/frostyard/pocketgo/service/internal/worktree"
)

// An agent's work session (ADR-0073): team_work starts a session of the
// caller's profile in a fresh worktree of one of its repositories, named
// and made in code from the call's key as an owner's session worktree is
// (ADR-0027), gives it the task through the outbox, and sends each answer
// of its runs back to the calling session, once, as a worker's replies go
// to its specialist (ADR-0021). pocketd never removes its worktree.

// workKeyPrefix begins every work key, and so the key of a work session's
// task among its notes.
const workKeyPrefix = "work:"

// workKey is team_work call's work key: its record's, its worktree's
// claim's, and its session's in the engine and the work database. It is
// the team call's key, so a call run again finds what its first run made.
func workKey(call TeamCall) string { return workKeyPrefix + call.key() }

// workTaskKey is the key of the task note of the work keyed key.
func workTaskKey(key string) string { return key + team.WorkTaskSuffix }

// isWorkTask says note key is a work session's task.
func isWorkTask(key string) bool {
	return strings.HasPrefix(key, workKeyPrefix) && strings.HasSuffix(key, team.WorkTaskSuffix)
}

// workReplyKey is the key of work session session's answer whose entry is
// entry, among its caller's notes.
func workReplyKey(session, entry int64) string {
	return team.WorkReplyPrefix + strconv.FormatInt(session, 10) + ":" + strconv.FormatInt(entry, 10)
}

// workWho names a work session in the answers pocketd writes for a run
// that ended with none (runReplies).
const workWho = "work session"

// TeamWork runs team_work for session call.Session: it checks the caller
// and the call (team.CheckWorkCaller, team.CheckWork), records the work by
// its key (store.StartWork), makes its worktree, starts the work session
// of the caller's profile there, records it with its task
// (store.RecordWorkSession), and sends the task through the outbox. Every
// step is keyed by the work key, so a call run again after a restart
// finds the worktree, the session and the task its first run made, makes
// none of them twice, and answers as the first did. ctx is the call's: a
// call stopped before the session is created creates none.
func (s *Server) TeamWork(ctx context.Context, call TeamCall, w team.Work) (string, error) {
	db := context.WithoutCancel(ctx)
	key := workKey(call)
	defer s.keys.lock(key)()
	row, err := s.store.WorkByKey(db, key)
	fresh := false
	switch {
	case err == nil && row.Session != 0:
		return s.workAnswer(db, row)
	case errors.Is(err, store.ErrNotFound):
		if row, err = s.newWork(ctx, call, key, w); err != nil {
			return "", err
		}
		fresh = true
	case err != nil:
		return "", err
	}
	if row, err = s.startWork(ctx, row, fresh); err != nil {
		return "", err
	}
	return s.workAnswer(db, row)
}

// newWork checks a team_work call and records its work: the caller
// (workCaller), the task and title (team.CheckWork), the repository
// (team.WorkRepository), a profile a session can start from, and a
// checkout that is the top of a git work tree, all before anything is
// recorded: a refused call starts nothing. The worktree is named from the
// work key (team.WorkWorktree).
func (s *Server) newWork(ctx context.Context, call TeamCall, key string, w team.Work) (store.WorkRow, error) {
	caller, err := s.workCaller(ctx, call.Session)
	if err != nil {
		return store.WorkRow{}, err
	}
	if err := team.CheckWork(w); err != nil {
		return store.WorkRow{}, err
	}
	repository, checkout, err := team.WorkRepository(*caller, w.Repository)
	if err != nil {
		return store.WorkRow{}, err
	}
	if _, err := s.workModel(ctx, *caller); err != nil {
		return store.WorkRow{}, err
	}
	checkout = filepath.Clean(checkout)
	title := team.WorkTitle(w)
	path, branch := team.WorkWorktree(*caller, checkout, title, key)
	// A fresh worktree is the point: only there do the repository's own
	// AGENTS.md and skills load. A checkout that is no git work tree is
	// refused, and nothing starts.
	check, cancel := s.withTimeout(ctx, checkTimeout)
	defer cancel()
	if _, err := worktree.Top(check, s.worktreeTools(), checkout); err != nil {
		return store.WorkRow{}, fmt.Errorf("no worktree for %s: %w. team_work starts a session in a fresh worktree only, where %s's own AGENTS.md loads. Nothing was started", repository, err, repository)
	}
	if err := ctx.Err(); err != nil {
		return store.WorkRow{}, fmt.Errorf("team_work was stopped: %w", err)
	}
	return s.store.StartWork(context.WithoutCancel(ctx), store.WorkRow{Key: key, Caller: call.Session, Agent: caller.ID, Repository: repository,
		Checkout: checkout, Path: path, Branch: branch, Title: title, Task: strings.TrimSpace(w.Task)})
}

// workCaller is session's profile as it is now, when that session may call
// team_work (team.CheckWorkCaller, with its role from pocketd's records):
// a refusal otherwise, one whose profile is gone or invalid now included.
func (s *Server) workCaller(ctx context.Context, session int64) (*profiles.Profile, error) {
	if s.profiles == nil {
		return nil, errors.New("pocketd has no profiles, so no agent starts work")
	}
	caller, role, err := s.sessionCaller(ctx, session)
	if err != nil {
		return nil, err
	}
	if caller == nil {
		// A profile that is gone or invalid now is no session of no
		// profile: the refusal says which.
		if row, err := s.store.Session(ctx, session); err != nil {
			return nil, err
		} else if row.Profile != "" {
			return nil, fmt.Errorf("this session's profile %s is gone or invalid now, so it starts no work", row.Profile)
		}
	}
	if err := team.CheckWorkCaller(caller, role); err != nil {
		return nil, err
	}
	return caller, nil
}

// recheckWorkCaller checks a work its call recorded, run again before its
// worktree is made (Codex review of #174, round 1): its caller may still
// call team_work, as its profile is now, and that profile still names its
// repository with the checkout the work recorded, and can start a session.
// A replay never makes a branch and a folder a profile no longer takes.
func (s *Server) recheckWorkCaller(ctx context.Context, row store.WorkRow) error {
	caller, err := s.workCaller(ctx, row.Caller)
	if err != nil {
		return err
	}
	if caller.ID != row.Agent {
		return fmt.Errorf("session %d's profile is %s now, not %s, which the work was recorded for", row.Caller, caller.ID, row.Agent)
	}
	if _, checkout, err := team.WorkRepository(*caller, row.Repository); err != nil {
		return err
	} else if filepath.Clean(checkout) != row.Checkout {
		return fmt.Errorf("%s's checkout of %s is %s now, not %s, which the work was recorded from", caller.ID, row.Repository, filepath.Clean(checkout), row.Checkout)
	}
	_, err = s.workModel(ctx, *caller)
	return err
}

// workModel is the model a work session of profile p runs now, from the
// models pocketd runs now (workChoice).
func (s *Server) workModel(ctx context.Context, p profiles.Profile) (router.Choice, error) {
	models, err := s.engine.Models(ctx)
	if err != nil {
		return router.Choice{}, err
	}
	return s.workChoice(p, models)
}

// workChoice is the model a work session of profile p runs among models
// (router.Choose, as any session of the profile), refused for a profile no
// session can start from, as Server.startable refuses one: a tool pocketd
// does not have, no model it runs, or a standing profile's compactAt that
// is not below the routed model's window (profiles.CheckCompactAt; Codex
// review of #174, round 1).
func (s *Server) workChoice(p profiles.Profile, models []proto.Model) (router.Choice, error) {
	if err := s.checkTools(p); err != nil {
		return router.Choice{}, fmt.Errorf("%s cannot start a session: %w", p.ID, err)
	}
	choice, err := router.Choose(router.Request{Profile: &p}, models)
	if err != nil {
		return router.Choice{}, fmt.Errorf("%s cannot start a session: %w", p.ID, err)
	}
	if err := profiles.CheckCompactAt(p, router.Window(models, choice.Model)); err != nil {
		return router.Choice{}, fmt.Errorf("%s cannot start a session: %w", p.ID, err)
	}
	return choice, nil
}

// startWork makes row's worktree and starts its session there, or
// finishes what an earlier run of its call began. A session the engine
// made for the work key, whose record failed, is recorded as it was made,
// without its profile. Otherwise a run again of the call (fresh false)
// checks its caller and repository again first (recheckWorkCaller); the
// worktree is made, outside createMu (git may take minutes), then checked
// again as it is now, with the profile read again, under createMu, which
// orders every session's start, right before the session starts at the
// worktree's real place. The checks and the creation take the call's own
// context, ctx, so a call stopped before the session exists starts none,
// as a delegation's does (attachBriefOpen); only a session PiG created is
// recorded whatever ctx does (Codex review of #174, round 1).
func (s *Server) startWork(ctx context.Context, row store.WorkRow, fresh bool) (store.WorkRow, error) {
	db := context.WithoutCancel(ctx)
	made, exists, err := s.engine.Created(db, row.Key)
	if err != nil {
		return store.WorkRow{}, err
	}
	if exists {
		return s.recordMadeWork(db, row, made)
	}
	if !fresh {
		if err := s.recheckWorkCaller(ctx, row); err != nil {
			return store.WorkRow{}, err
		}
	}
	if row, err = s.makeWorkWorktree(ctx, row); err != nil {
		return store.WorkRow{}, err
	}
	if s.beforeCreateLock != nil {
		s.beforeCreateLock()
	}
	s.createMu.Lock()
	defer s.createMu.Unlock()
	// The wait for the lock may have been long: a call stopped meanwhile
	// starts nothing.
	if err := ctx.Err(); err != nil {
		return store.WorkRow{}, fmt.Errorf("team_work was stopped: %w", err)
	}
	place, err := s.usableWorkWorktree(ctx, row)
	if err != nil {
		return store.WorkRow{}, err
	}
	spec, err := s.recheckWork(ctx, row, place)
	if err != nil {
		return store.WorkRow{}, err
	}
	if err := ctx.Err(); err != nil {
		return store.WorkRow{}, fmt.Errorf("team_work was stopped: %w", err)
	}
	id, err := s.engine.CreateSession(ctx, row.Key, spec)
	if err != nil {
		// A session PiG made all the same, its answer lost to the stop, is
		// recorded as it was made: no work names a session the work
		// database lacks.
		if made, exists, lookErr := s.engine.Created(db, row.Key); lookErr == nil && exists {
			return s.recordMadeWork(db, row, made)
		}
		return store.WorkRow{}, err
	}
	return s.recordWork(db, row, id, spec, spec.Profile.ID, false)
}

// recordMadeWork records the session the engine made for row's work key
// as it was made, without its profile (recordWork).
func (s *Server) recordMadeWork(ctx context.Context, row store.WorkRow, made Created) (store.WorkRow, error) {
	spec := SessionSpec{Title: made.Title, Cwd: made.Cwd, Model: made.Model, Thinking: made.Thinking, Route: made.Route, Work: true}
	return s.recordWork(ctx, row, made.ID, spec, made.Profile, true)
}

// recordWork records row's session id, made from spec for its work key,
// with its task in one transaction (store.RecordWorkSession), gives it its
// profile's MCP tools, publishes it and watches it. A watch that fails
// holds the task, which goes only to a watched session, and the delivery
// pass watches it again (workDue). A session recovered as it was made, its
// lessons section unknown, has its profile's lessons refreshed by the
// delivery pass (Codex review of #174, round 1). It answers the work as
// recorded, with the model and folder spec created the session with. A
// record that fails leaves the work pending: the delivery pass records it
// once the engine's creation record shows the session (reconcileWork;
// round 3).
func (s *Server) recordWork(ctx context.Context, row store.WorkRow, id int64, spec SessionSpec, profile string, recovered bool) (store.WorkRow, error) {
	title := spec.Title
	if title == "" {
		title = team.WorkSessionTitle(row.Agent, row.Title)
	}
	brief := team.WorkBrief(row.Caller, row.Repository, spec.Cwd, row.Branch, row.Stale, row.Task)
	recorded, err := s.recordWorkSession(ctx, row.Key, store.SessionRow{ID: id, Title: title, Cwd: spec.Cwd, Provider: spec.Model.Provider,
		Model: spec.Model.ID, Thinking: spec.Thinking, Profile: profile, Route: spec.Route, CreatedBy: teamCreator}, store.NoteRow{Key: workTaskKey(row.Key), Text: brief})
	if err != nil {
		s.workPending()
		return store.WorkRow{}, err
	}
	// Its profile's MCP tools as they are now: it builds, as the owner's
	// session of its profile does (ADR-0063).
	s.mcpAfterCreate(ctx, id, profile, team.SessionRole{Work: true})
	s.publishSession(id, true)
	switch {
	case spec.Profile != nil:
		s.lessonsSince(ctx, profile, spec.Lessons)
	case recovered && profile != "":
		s.lessonsChanged(profile)
	}
	if err := s.engine.Watch(ctx, id); err != nil {
		s.log.Warn("watch a work session", "session", id, "err", err)
		s.workDue(id)
	}
	return recorded, nil
}

// workAnswer is team_work's answer for row's work session, whose task it
// sends now when it can; one that cannot go now, a delivery pass sends.
// The session's model and folder are those it was created with, kept with
// the work's record in the transaction that recorded the session
// (store.RecordWorkSession), which no later change of its model touches:
// a call and a run of it again answer the same, and neither looks
// anything up once the task may have gone, so no failed lookup reports a
// failed start of work that runs (Codex review of #174, rounds 1 to 3).
func (s *Server) workAnswer(ctx context.Context, row store.WorkRow) (string, error) {
	if done, err := s.sendNotes(ctx, row.Session); err != nil || !done {
		if err != nil {
			s.log.Warn("send a work session its task", "session", row.Session, "err", err)
		}
		s.deliverLater()
	}
	model := proto.ModelRef{Provider: row.Provider, ID: row.Model}
	return team.WorkStarted(row.Session, model.String(), row.Repository, row.Cwd, row.Branch), nil
}

// makeWorkWorktree makes row's worktree, or finishes the one an earlier run
// of its call made, as an owner's session worktree is made
// (makeSessionWorktree, ADR-0027): the checkout as git names it and the
// worktree's real place, both from one resolution of their folder
// (workPlace), are bound to the work key with the path and the branch
// before git runs (store.ClaimWorktree); the folder is made at that place,
// alone, and git makes its branch there or fails (worktree.AddNew), each
// step recorded (store.MarkWorktree), so a run again checks a worktree its
// own git made, and takes no folder or branch something else made. git
// runs to its end, or worktreeTimeout, even when the call is stopped. Why
// its branch starts from the checkout's HEAD is kept with the work
// (store.KeepWorkStale) as soon as the base is decided, before the folder
// is made (worktree.AddNewKeepingBase): a run again that finds the
// worktree made decides no base, and its task still says so (Codex review
// of #174, round 2). The row it answers carries it.
func (s *Server) makeWorkWorktree(ctx context.Context, row store.WorkRow) (store.WorkRow, error) {
	db := context.WithoutCancel(ctx)
	gitCtx, cancel := s.withTimeout(db, worktreeTimeout)
	defer cancel()
	place, err := s.workPlace(gitCtx, row)
	if err != nil {
		return row, err
	}
	if err := s.store.ClaimWorktree(db, row.Key, place); err != nil {
		return row, workWorktreeFailed(row, err)
	}
	claim, _, err := s.store.WorktreeOf(db, row.Key)
	if err != nil {
		return row, err
	}
	record := func(step worktree.Step) error {
		if err := s.markWorktree(db, row.Key, int(step)); err != nil {
			return progressFailed{err}
		}
		return nil
	}
	// Each base newly decided replaces what an earlier one kept, a fresh
	// fetch's clearing a failed one's reason; a resumed worktree decides
	// none, and keeps its own (Codex review of #174, round 3).
	keep := func(start worktree.Start) error {
		if start.Stale != "" {
			s.log.Warn("a work session's worktree starts from the checkout's HEAD", "path", row.Path, "why", start.Stale)
		}
		if err := s.keepWorkStale(db, row.Key, start.Stale); err != nil {
			return fmt.Errorf("keep the base of the worktree %s: %w", row.Path, err)
		}
		row.Stale = start.Stale
		return nil
	}
	if _, err := worktree.AddNewKeepingBase(gitCtx, s.worktreeTools(), place.Top, place.Dest, row.Branch, worktreeToken(row.Key), worktree.Step(claim.Made), keep, record); err != nil {
		return row, workWorktreeFailed(row, err)
	}
	return row, nil
}

// workPlace is where row's worktree is made now: its real place, its
// folder's parent resolved with every link followed (Engine.Real), and its
// checkout as git names it in that same folder (worktree.Top), as
// worktreePlace has them for an owner's session worktree. A folder or a
// checkout that is not as it must be is folderChanged; a look that could
// not tell is any other error.
func (s *Server) workPlace(ctx context.Context, row store.WorkRow) (store.SessionWorktree, error) {
	parent, ok, err := s.engine.Real(ctx, filepath.Dir(row.Path))
	switch {
	case err != nil:
		return store.SessionWorktree{}, fmt.Errorf("make the worktree: resolve %s: %w", filepath.Dir(row.Path), err)
	case !ok:
		return store.SessionWorktree{}, folderChanged{fmt.Errorf("make the worktree: %s is not a folder", filepath.Dir(row.Path))}
	}
	top, err := worktree.Top(ctx, s.worktreeTools(), filepath.Join(parent, filepath.Base(row.Checkout)))
	if err != nil {
		return store.SessionWorktree{}, settledChange(fmt.Errorf("make the worktree: %w", err))
	}
	return store.SessionWorktree{Path: row.Path, Branch: row.Branch, Top: top, Dest: filepath.Join(parent, filepath.Base(row.Path)), Target: workTarget(row)}, nil
}

// settledChange is err as folderChanged when a worktree check said the
// worktree or its checkout is not as it must be, and err as it is when the
// check could not tell (worktree.Unsure).
func settledChange(err error) error {
	if worktree.Unsure(err) {
		return err
	}
	return folderChanged{err}
}

// workTarget is a digest of what row's work names, which its worktree's
// claim keeps: a run again of its call names the same, from its record.
func workTarget(row store.WorkRow) string {
	fields, _ := json.Marshal([]string{"team_work", row.Agent, row.Repository, row.Checkout, row.Title})
	sum := sha256.Sum256(fields)
	return hex.EncodeToString(sum[:])
}

// workWorktreeFailed is the error team_work answers when row's worktree
// could not be made or checked: git's word, or why the name is taken.
func workWorktreeFailed(row store.WorkRow, err error) error {
	var progress progressFailed
	switch {
	case errors.As(err, &progress):
		return fmt.Errorf("record the making of the worktree %s: %w", row.Path, err)
	case errors.Is(err, worktree.ErrTaken), errors.Is(err, store.ErrWorktreeTaken):
		return fmt.Errorf("the worktree %s for %s: %w. Call team_work again with another title", row.Path, row.Repository, err)
	}
	return fmt.Errorf("no worktree for %s at %s: %w", row.Repository, row.Path, err)
}

// usableWorkWorktree checks the worktree row's work made, as it is now,
// bound by checkTimeout, and is its real place, where the session works:
// its checkout and folder still name the checkout and place it was made
// at, it is git's registered worktree there, on its branch, not locked
// (worktree.Usable), and it is the one the work made, by its mark
// (worktree.Marked), as usableSessionWorktree checks an owner's. It runs
// under createMu before the session starts, and before each new
// submission of a task not yet placed (workTaskCheck). A worktree that is
// not as it must be is folderChanged; a check that could not tell is any
// other error.
func (s *Server) usableWorkWorktree(ctx context.Context, row store.WorkRow) (string, error) {
	check, cancel := s.withTimeout(ctx, checkTimeout)
	defer cancel()
	claim, held, err := s.store.WorktreeOf(ctx, row.Key)
	if err != nil {
		return "", err
	}
	if !held {
		return "", folderChanged{fmt.Errorf("the work holds no worktree")}
	}
	place, err := s.workPlace(check, row)
	if err != nil {
		return "", err
	}
	if place.Top != claim.Top || place.Dest != claim.Dest {
		return "", folderChanged{fmt.Errorf("%w: %s, on %s, from the checkout %s, at %s, which %s and %s no longer name", store.ErrRequestHasWorktree, claim.Path, claim.Branch,
			claim.Top, claim.Dest, row.Checkout, row.Path)}
	}
	if err := worktree.Usable(check, s.worktreeTools(), claim.Top, claim.Dest, row.Branch); err != nil {
		return "", settledChange(workWorktreeFailed(row, fmt.Errorf("the worktree changed since it was made: %w", err)))
	}
	if err := worktree.Marked(check, s.worktreeTools(), claim.Dest, worktreeToken(row.Key)); err != nil {
		return "", settledChange(workWorktreeFailed(row, err))
	}
	return claim.Dest, nil
}

// recheckWork is what row's work session starts from, at place: the
// models pocketd runs now, read first, then its profile, read again now,
// last, right before the session starts, as recheckSpecialist reads a
// specialist's (Codex review of #174, round 1): it must still be an
// agent's that works (team.MayWork), name the repository with the
// checkout the worktree was made from, and start a session on a model of
// that snapshot (workChoice); its lessons in force are read before both.
func (s *Server) recheckWork(ctx context.Context, row store.WorkRow, place string) (SessionSpec, error) {
	if s.profiles == nil {
		return SessionSpec{}, errors.New("pocketd has no profiles, so the work session cannot start")
	}
	lessons, err := s.store.ActiveLessons(ctx, row.Agent)
	if err != nil {
		return SessionSpec{}, err
	}
	models, err := s.engine.Models(ctx)
	if err != nil {
		return SessionSpec{}, err
	}
	p, err := s.profiles.Find(row.Agent)
	if err != nil {
		return SessionSpec{}, fmt.Errorf("the work session cannot start: %w", err)
	}
	if !team.MayWork(&p, team.SessionRole{}) {
		return SessionSpec{}, fmt.Errorf("%s takes no work of its own now: it is private or a supervisor", p.ID)
	}
	if _, checkout, err := team.WorkRepository(p, row.Repository); err != nil {
		return SessionSpec{}, err
	} else if filepath.Clean(checkout) != row.Checkout {
		return SessionSpec{}, fmt.Errorf("%s's checkout of %s changed while its worktree was made", p.ID, row.Repository)
	}
	choice, err := s.workChoice(p, models)
	if err != nil {
		return SessionSpec{}, err
	}
	return SessionSpec{Title: team.WorkSessionTitle(p.Name, row.Title), Cwd: place, Model: choice.Model, Profile: &p, Route: string(choice.Rule),
		Lessons: sectionOf(lessons), Work: true}, nil
}

// workTaskCheck is the check before each new submission of session's
// task, its first or a retry's (Codex review of #174, round 1): the
// session is watched, so the end of each of its runs reaches pocketd, and
// its worktree is still the one its work made, at its place, on its
// branch, by its mark (usableWorkWorktree), as placeBrief checks a
// specialist's folder again. A task already placed is not checked: place
// finds it first.
func (s *Server) workTaskCheck(session int64) func(context.Context) error {
	return func(ctx context.Context) error {
		if err := s.engine.Watch(ctx, session); err != nil {
			return fmt.Errorf("watch the work session: %w", err)
		}
		row, err := s.store.WorkOfSession(ctx, session)
		if err != nil {
			return err
		}
		_, err = s.usableWorkWorktree(ctx, row)
		return err
	}
}

// dropWorkTask drops session's task, which never reached it, since its
// worktree is no longer the one its work made (moved), and tells the
// session that called team_work so once, in one transaction
// (store.DropWorkTask), then has a delivery pass bring that notice.
// outboxMu is held.
func (s *Server) dropWorkTask(ctx context.Context, session int64, moved error) error {
	row, err := s.store.WorkOfSession(ctx, session)
	if err != nil {
		return err
	}
	if err := s.store.DropWorkTask(ctx, row.Key, workTaskKey(row.Key), store.NoteRow{Key: row.Key + workDroppedSuffix, SessionID: row.Caller,
		Text: team.WorkTaskDropped(session, row.Title, team.Clip(moved.Error(), team.MaxFailure))}); err != nil {
		return err
	}
	// The notice is a note of another session, which the pass under way
	// may have read its list of sessions before: another pass brings it
	// (Codex review of #174, round 2).
	s.deliverLater()
	return nil
}

// workDroppedSuffix ends the key of the note that tells a work's caller
// its task was dropped, after its work key.
const workDroppedSuffix = ":dropped"

// workTaskWaits says whether session is a work session whose task is not
// in its transcript yet: until it is, the owner's message waits, so the
// task is the work session's first message (protocol rule 10).
func (s *Server) workTaskWaits(ctx context.Context, session int64) (bool, error) {
	row, err := s.store.WorkOfSession(ctx, session)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	note, err := s.store.Note(ctx, workTaskKey(row.Key))
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return note.SentAt == nil && note.DroppedAt == nil, nil
}

// followWork sends the answers of session's runs to its caller when
// session is a work session. A look that fails is tried again by the
// delivery pass.
func (s *Server) followWork(session int64) {
	row, err := s.store.WorkOfSession(s.passCtx, session)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return
	case err != nil:
		s.log.Warn("look at a session's work", "session", session, "err", err)
		s.workDue(session)
		return
	}
	s.workRunEnded(s.passCtx, row)
}

// workRunEnded follows a work session's run that ended, or the start of
// its next: each answer its runs ended with is kept for its caller, once
// (queueWorkReplies), and sent now, through the outbox; what cannot be
// read or kept now, or sent now, the delivery pass does.
func (s *Server) workRunEnded(ctx context.Context, row store.WorkRow) {
	if err := s.queueWorkReplies(ctx, row); err != nil {
		s.log.Warn("keep a work session's answers", "session", row.Session, "err", err)
		s.workDue(row.Session)
		return
	}
	if done, err := s.sendNotes(ctx, row.Caller); err != nil || !done {
		if err != nil {
			s.log.Warn("send a work session's answer", "session", row.Session, "caller", row.Caller, "err", err)
		}
		s.deliverLater()
	}
}

// queueWorkReplies keeps each answer of the runs of row's work session
// after its cursor as a note to its caller, keyed by the session and the
// answer's entry, with the cursor's move, in one transaction
// (store.QueueWorkReplies): the runs are read as a worker's are
// (runReplies), from the transcript's own word on whether the session is
// at work. One follow of a session reads at a time, from the cursor as it
// is then.
func (s *Server) queueWorkReplies(ctx context.Context, row store.WorkRow) error {
	defer s.workFollows.lock(strconv.FormatInt(row.Session, 10))()
	current, err := s.store.WorkOfSession(ctx, row.Session)
	if err != nil {
		return err
	}
	entries, live, err := s.engine.TranscriptAfter(ctx, current.Session, current.Cursor)
	if err != nil {
		return err
	}
	replies, cursor := runReplies(entries, live, current.Cursor, workWho)
	if len(replies) == 0 && cursor <= current.Cursor {
		return nil
	}
	notes := make([]store.NoteRow, 0, len(replies))
	for _, r := range replies {
		notes = append(notes, store.NoteRow{Key: workReplyKey(current.Session, r.entry), SessionID: current.Caller,
			Text: team.WorkReplyText(current.Session, current.Title, r.text)})
	}
	return s.store.QueueWorkReplies(ctx, current.Session, notes, cursor)
}

// workPending has the delivery pass record every pending work the engine
// made a session for (reconcileWork): a record that failed is tried again
// without a restart.
func (s *Server) workPending() {
	s.workDueMu.Lock()
	s.workPendingDue = true
	s.workDueMu.Unlock()
	s.deliverLater()
}

// reconcileWork records the session of each pending work, one whose
// session is not recorded yet, that the engine's creation record shows it
// made for the work's key (Engine.Created), as it was made, with its task
// (recordMadeWork): a start whose record failed, or that a stop cut off
// after the engine made its session, is finished, at the first delivery
// pass after a start and after a record that failed (Codex review of #174,
// round 3). It creates no session: a work the engine made none for, as a
// call stopped before the session's creation, stays as it is. A work a
// call holds now is left to that call. done says nothing failed.
func (s *Server) reconcileWork(ctx context.Context) bool {
	keys, err := s.store.PendingWorkKeys(ctx)
	if err != nil {
		s.log.Warn("read the pending works", "err", err)
		return false
	}
	done := true
	for _, key := range keys {
		unlock, ok := s.keys.tryLock(key)
		if !ok {
			continue
		}
		if err := s.reconcileOne(ctx, key); err != nil {
			s.log.Warn("record a pending work's session", "key", key, "err", err)
			done = false
		}
		unlock()
	}
	return done
}

// reconcileOne records pending work key's session, when the engine made
// one for it; the work's key is held.
func (s *Server) reconcileOne(ctx context.Context, key string) error {
	row, err := s.store.WorkByKey(ctx, key)
	if err != nil || row.Session != 0 {
		return err
	}
	made, exists, err := s.engine.Created(ctx, key)
	if err != nil || !exists {
		return err
	}
	_, err = s.recordMadeWork(ctx, row, made)
	return err
}

// workDue keeps work session session for the delivery pass to watch and
// read again.
func (s *Server) workDue(session int64) {
	s.workDueMu.Lock()
	if s.worksDue == nil {
		s.worksDue = map[int64]bool{}
	}
	s.worksDue[session] = true
	s.workDueMu.Unlock()
	s.deliverLater()
}

// followDueWork, in a delivery pass, watches and reads again each work
// session whose answers could not be read or kept, and, at the first pass
// after a start, every work session, since a run may have ended while
// pocketd stopped. The answers it keeps go in the pass's notes. done says
// none is left.
func (s *Server) followDueWork(ctx context.Context) bool {
	s.workDueMu.Lock()
	due := make([]int64, 0, len(s.worksDue))
	for session := range s.worksDue {
		due = append(due, session)
	}
	s.worksDue = nil
	scan := s.workScanDue
	s.workScanDue = false
	pending := s.workPendingDue
	s.workPendingDue = false
	s.workDueMu.Unlock()
	// The works whose session is not recorded yet first: one recorded now
	// is followed below, at the next pass (Codex review of #174, round 3).
	if pending && !s.reconcileWork(ctx) {
		s.workDueMu.Lock()
		s.workPendingDue = true
		s.workDueMu.Unlock()
	}
	if scan {
		// The sessions alone: each work's record is read as it is followed.
		ids, err := s.store.WorkSessionIDs(ctx)
		if err != nil {
			s.log.Warn("read the work sessions", "err", err)
			s.workDueMu.Lock()
			s.workScanDue = true
			s.workDueMu.Unlock()
		}
		due = append(due, ids...)
	}
	seen := map[int64]bool{}
	for _, session := range due {
		if seen[session] {
			continue
		}
		seen[session] = true
		row, err := s.store.WorkOfSession(ctx, session)
		switch {
		case errors.Is(err, store.ErrNotFound):
			continue
		case err != nil:
			s.workDue(session)
			continue
		}
		// Watched first (Watch is idempotent): a watch that failed is tried
		// again, and the read after it finds a run that ended unwatched.
		if err := s.engine.Watch(ctx, session); err != nil {
			s.log.Warn("watch a work session", "session", session, "err", err)
			s.workDue(session)
			continue
		}
		if err := s.queueWorkReplies(ctx, row); err != nil {
			s.log.Warn("keep a work session's answers", "session", session, "err", err)
			s.workDue(session)
		}
	}
	s.workDueMu.Lock()
	defer s.workDueMu.Unlock()
	return len(s.worksDue) == 0 && !s.workScanDue && !s.workPendingDue
}

// upgradeWorkTools gives team_work to every session that gets it now
// (team.MayWork: an agent's own session, of a profile that is no
// supervisor's and not private, working as none of the roles) and lacks
// it, but a standing one, which takes its tools at its idle refresh
// (ADR-0073). Each profile is read once a pass, one it cannot read too,
// whose other sessions wait for the next pass, as upgradeSupervisorTools
// reads them (Codex review of #174, round 1), and a session's role only
// for a profile whose sessions may work; a profile it cannot read now, or
// a session it cannot change, is an error, and the others go on.
func (s *Server) upgradeWorkTools(ctx context.Context) error {
	if s.profiles == nil {
		return nil
	}
	rows, err := s.store.Sessions(ctx)
	if err != nil {
		return err
	}
	read := map[string]*profiles.Profile{}
	unreadable := map[string]bool{}
	var failures []error
	for _, row := range rows {
		if row.Profile == "" || row.Standing || unreadable[row.Profile] {
			continue
		}
		p, known := read[row.Profile]
		if !known {
			found, err := s.profiles.Find(row.Profile)
			if profiles.Unreadable(err) {
				unreadable[row.Profile] = true
				failures = append(failures, fmt.Errorf("profile %s: %w", row.Profile, err))
				continue
			}
			if err == nil {
				p = &found
			}
			read[row.Profile] = p
		}
		if !team.MayWork(p, team.SessionRole{}) {
			continue
		}
		role, err := s.sessionRole(ctx, row.ID)
		if err != nil {
			failures = append(failures, fmt.Errorf("session %d: %w", row.ID, err))
			continue
		}
		if !team.MayWork(p, role) {
			continue
		}
		if changed, err := s.engine.SetWorkTool(ctx, row.ID); err != nil {
			failures = append(failures, fmt.Errorf("session %d: %w", row.ID, err))
		} else if changed {
			s.log.Info("gave an agent's session team_work", "session", row.ID)
		}
	}
	return errors.Join(failures...)
}
