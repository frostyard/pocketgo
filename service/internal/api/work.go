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
// none of them twice, and answers as the first did.
func (s *Server) TeamWork(ctx context.Context, call TeamCall, w team.Work) (string, error) {
	db := context.WithoutCancel(ctx)
	key := workKey(call)
	defer s.keys.lock(key)()
	row, err := s.store.WorkByKey(db, key)
	switch {
	case err == nil && row.Session != 0:
		return s.workAnswer(db, row)
	case errors.Is(err, store.ErrNotFound):
		if row, err = s.newWork(ctx, call, key, w); err != nil {
			return "", err
		}
	case err != nil:
		return "", err
	}
	if row, err = s.startWork(ctx, row); err != nil {
		return "", err
	}
	return s.workAnswer(db, row)
}

// newWork checks a team_work call and records its work: the caller's
// profile as it is now and its role (team.CheckWorkCaller), the task and
// title (team.CheckWork), the repository (team.WorkRepository), a profile
// a session can start from, and a checkout that is the top of a git work
// tree, all before anything is recorded: a refused call starts nothing.
// The worktree is named from the work key (team.WorkWorktree).
func (s *Server) newWork(ctx context.Context, call TeamCall, key string, w team.Work) (store.WorkRow, error) {
	if s.profiles == nil {
		return store.WorkRow{}, errors.New("pocketd has no profiles, so no agent starts work")
	}
	caller, role, err := s.sessionCaller(ctx, call.Session)
	if err != nil {
		return store.WorkRow{}, err
	}
	if caller == nil {
		// A profile that is gone or invalid now is no session of no
		// profile: the refusal says which.
		if row, err := s.store.Session(ctx, call.Session); err != nil {
			return store.WorkRow{}, err
		} else if row.Profile != "" {
			return store.WorkRow{}, fmt.Errorf("this session's profile %s is gone or invalid now, so it starts no work", row.Profile)
		}
	}
	if err := team.CheckWorkCaller(caller, role); err != nil {
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

// workModel is the model a work session of profile p runs now
// (router.Choose, as any session of the profile), refused for a profile no
// session can start from: a tool pocketd does not have.
func (s *Server) workModel(ctx context.Context, p profiles.Profile) (router.Choice, error) {
	if err := s.checkTools(p); err != nil {
		return router.Choice{}, fmt.Errorf("%s cannot start a session: %w", p.ID, err)
	}
	models, err := s.engine.Models(ctx)
	if err != nil {
		return router.Choice{}, err
	}
	choice, err := router.Choose(router.Request{Profile: &p}, models)
	if err != nil {
		return router.Choice{}, fmt.Errorf("%s cannot start a session: %w", p.ID, err)
	}
	return choice, nil
}

// startWork makes row's worktree and starts its session there, or
// finishes what an earlier run of its call began. A session the engine
// made for the work key, whose record failed, is recorded as it was made,
// without its profile. Otherwise the worktree is made first, outside
// createMu (git may take minutes), then checked again as it is now, with
// the profile read again, under createMu, which orders every session's
// start, right before the session starts at the worktree's real place.
func (s *Server) startWork(ctx context.Context, row store.WorkRow) (store.WorkRow, error) {
	db := context.WithoutCancel(ctx)
	made, exists, err := s.engine.Created(db, row.Key)
	if err != nil {
		return store.WorkRow{}, err
	}
	if exists {
		spec := SessionSpec{Title: made.Title, Cwd: made.Cwd, Model: made.Model, Thinking: made.Thinking, Route: made.Route, Work: true}
		return s.recordWork(ctx, row, spec, made.Profile, "")
	}
	stale, err := s.makeWorkWorktree(ctx, row)
	if err != nil {
		return store.WorkRow{}, err
	}
	if err := ctx.Err(); err != nil {
		return store.WorkRow{}, fmt.Errorf("team_work was stopped: %w", err)
	}
	s.createMu.Lock()
	defer s.createMu.Unlock()
	place, err := s.usableWorkWorktree(db, row)
	if err != nil {
		return store.WorkRow{}, err
	}
	spec, err := s.recheckWork(db, row, place)
	if err != nil {
		return store.WorkRow{}, err
	}
	return s.recordWork(ctx, row, spec, spec.Profile.ID, stale)
}

// recordWork creates row's session from spec for its work key, records it
// with its task in one transaction (store.RecordWorkSession), gives it its
// profile's MCP tools, publishes it and watches it. A watch that fails
// holds the task, which goes only to a watched session, and the delivery
// pass watches it again (workDue).
func (s *Server) recordWork(ctx context.Context, row store.WorkRow, spec SessionSpec, profile, stale string) (store.WorkRow, error) {
	db := context.WithoutCancel(ctx)
	id, err := s.engine.CreateSession(db, row.Key, spec)
	if err != nil {
		return store.WorkRow{}, err
	}
	title := spec.Title
	if title == "" {
		title = team.WorkSessionTitle(row.Agent, row.Title)
	}
	brief := team.WorkBrief(row.Caller, row.Repository, spec.Cwd, row.Branch, stale, row.Task)
	recorded, err := s.store.RecordWorkSession(db, row.Key, store.SessionRow{ID: id, Title: title, Cwd: spec.Cwd, Provider: spec.Model.Provider,
		Model: spec.Model.ID, Thinking: spec.Thinking, Profile: profile, Route: spec.Route, CreatedBy: teamCreator}, store.NoteRow{Key: workTaskKey(row.Key), Text: brief})
	if err != nil {
		return store.WorkRow{}, err
	}
	// Its profile's MCP tools as they are now: it builds, as the owner's
	// session of its profile does (ADR-0063).
	s.mcpAfterCreate(db, id, profile, team.SessionRole{Work: true})
	s.publishSession(id, true)
	if spec.Profile != nil {
		s.lessonsSince(db, profile, spec.Lessons)
	}
	if err := s.engine.Watch(db, id); err != nil {
		s.log.Warn("watch a work session", "session", id, "err", err)
		s.workDue(id)
	}
	return recorded, nil
}

// workAnswer is team_work's answer for row's work session, whose task it
// sends now when it can; one that cannot go now, a delivery pass sends.
// The session's model and folder are read from its record, so a call and
// a run of it again answer the same.
func (s *Server) workAnswer(ctx context.Context, row store.WorkRow) (string, error) {
	if done, err := s.sendNotes(ctx, row.Session); err != nil || !done {
		if err != nil {
			s.log.Warn("send a work session its task", "session", row.Session, "err", err)
		}
		s.deliverLater()
	}
	session, err := s.store.Session(ctx, row.Session)
	if err != nil {
		return "", err
	}
	return team.WorkStarted(row.Session, modelOf(session).String(), row.Repository, session.Cwd, row.Branch), nil
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
// runs to its end, or worktreeTimeout, even when the call is stopped. It
// answers the stale base's reason, "" for a fresh fetch.
func (s *Server) makeWorkWorktree(ctx context.Context, row store.WorkRow) (string, error) {
	db := context.WithoutCancel(ctx)
	gitCtx, cancel := s.withTimeout(db, worktreeTimeout)
	defer cancel()
	place, err := s.workPlace(gitCtx, row)
	if err != nil {
		return "", err
	}
	if err := s.store.ClaimWorktree(db, row.Key, place); err != nil {
		return "", workWorktreeFailed(row, err)
	}
	claim, _, err := s.store.WorktreeOf(db, row.Key)
	if err != nil {
		return "", err
	}
	record := func(step worktree.Step) error {
		if err := s.markWorktree(db, row.Key, int(step)); err != nil {
			return progressFailed{err}
		}
		return nil
	}
	start, err := worktree.AddNew(gitCtx, s.worktreeTools(), place.Top, place.Dest, row.Branch, worktreeToken(row.Key), worktree.Step(claim.Made), record)
	if err != nil {
		return "", workWorktreeFailed(row, err)
	}
	if start.Stale != "" {
		s.log.Warn("a work session's worktree starts from the checkout's HEAD", "path", row.Path, "why", start.Stale)
	}
	return start.Stale, nil
}

// workPlace is where row's worktree is made now: its real place, its
// folder's parent resolved with every link followed (Engine.Real), and its
// checkout as git names it in that same folder (worktree.Top), as
// worktreePlace has them for an owner's session worktree.
func (s *Server) workPlace(ctx context.Context, row store.WorkRow) (store.SessionWorktree, error) {
	parent, ok, err := s.engine.Real(ctx, filepath.Dir(row.Path))
	switch {
	case err != nil:
		return store.SessionWorktree{}, fmt.Errorf("make the worktree: resolve %s: %w", filepath.Dir(row.Path), err)
	case !ok:
		return store.SessionWorktree{}, fmt.Errorf("make the worktree: %s is not a folder", filepath.Dir(row.Path))
	}
	top, err := worktree.Top(ctx, s.worktreeTools(), filepath.Join(parent, filepath.Base(row.Checkout)))
	if err != nil {
		return store.SessionWorktree{}, fmt.Errorf("make the worktree: %w", err)
	}
	return store.SessionWorktree{Path: row.Path, Branch: row.Branch, Top: top, Dest: filepath.Join(parent, filepath.Base(row.Path)), Target: workTarget(row)}, nil
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

// usableWorkWorktree checks, under createMu, the worktree row's work made,
// as it is now, bound by checkTimeout, and is its real place, where the
// session works: its checkout and folder still name the checkout and
// place it was made at, it is git's registered worktree there, on its
// branch, not locked (worktree.Usable), and it is the one the work made,
// by its mark (worktree.Marked), as usableSessionWorktree checks an
// owner's.
func (s *Server) usableWorkWorktree(ctx context.Context, row store.WorkRow) (string, error) {
	check, cancel := s.withTimeout(ctx, checkTimeout)
	defer cancel()
	claim, held, err := s.store.WorktreeOf(ctx, row.Key)
	if err != nil {
		return "", err
	}
	if !held {
		return "", fmt.Errorf("the work holds no worktree")
	}
	place, err := s.workPlace(check, row)
	if err != nil {
		return "", err
	}
	if place.Top != claim.Top || place.Dest != claim.Dest {
		return "", fmt.Errorf("%w: %s, on %s, from the checkout %s, at %s, which %s and %s no longer name", store.ErrRequestHasWorktree, claim.Path, claim.Branch,
			claim.Top, claim.Dest, row.Checkout, row.Path)
	}
	if err := worktree.Usable(check, s.worktreeTools(), claim.Top, claim.Dest, row.Branch); err != nil {
		return "", workWorktreeFailed(row, fmt.Errorf("the worktree changed since it was made: %w", err))
	}
	if err := worktree.Marked(check, s.worktreeTools(), claim.Dest, worktreeToken(row.Key)); err != nil {
		return "", workWorktreeFailed(row, err)
	}
	return claim.Dest, nil
}

// recheckWork is what row's work session starts from, at place: its profile
// read again now, last, right before the session starts, which must still
// be an agent's that works (team.MayWork) and name the repository with
// the checkout the worktree was made from; its lessons in force, read
// before it; and the router's model for it now.
func (s *Server) recheckWork(ctx context.Context, row store.WorkRow, place string) (SessionSpec, error) {
	if s.profiles == nil {
		return SessionSpec{}, errors.New("pocketd has no profiles, so the work session cannot start")
	}
	lessons, err := s.store.ActiveLessons(ctx, row.Agent)
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
	choice, err := s.workModel(ctx, p)
	if err != nil {
		return SessionSpec{}, err
	}
	return SessionSpec{Title: team.WorkSessionTitle(p.Name, row.Title), Cwd: place, Model: choice.Model, Profile: &p, Route: string(choice.Rule),
		Lessons: sectionOf(lessons), Work: true}, nil
}

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
	s.workDueMu.Unlock()
	if scan {
		rows, err := s.store.WorkSessions(ctx)
		if err != nil {
			s.log.Warn("read the work sessions", "err", err)
			s.workDueMu.Lock()
			s.workScanDue = true
			s.workDueMu.Unlock()
		}
		for _, row := range rows {
			due = append(due, row.Session)
		}
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
	return len(s.worksDue) == 0 && !s.workScanDue
}

// upgradeWorkTools gives team_work to every session that gets it now
// (team.MayWork: an agent's own session, of a profile that is no
// supervisor's and not private, working as none of the roles) and lacks
// it, but a standing one, which takes its tools at its idle refresh
// (ADR-0073). Each profile is read once a pass, and a session's role only
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
	var failures []error
	for _, row := range rows {
		if row.Profile == "" || row.Standing {
			continue
		}
		p, known := read[row.Profile]
		if !known {
			found, err := s.profiles.Find(row.Profile)
			if profiles.Unreadable(err) {
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
