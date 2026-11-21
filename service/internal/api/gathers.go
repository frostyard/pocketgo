package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/router"
	"github.com/frostyard/pocketgo/service/internal/schedule"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// Gathers (ADR-0033) ask agents of the team for proposals. Each asked agent
// answers in a read-only proposal session of its own, started from its
// profile in its home, else its workspace, with read and team_propose
// alone: it takes no place on the desk and opens no thread. Its brief, the
// skill's text with the rules of a proposal, is a note filed with its
// session and placed by the outbox. An ask ends when its agent says it is
// done, when its run ends once its brief is in, or when its time passes;
// the last ask's end ends the gather and files the supervisor's one note,
// in the transaction that ends it. The delivery pass follows them.

// gatherRecheck is how long a running ask waits for a pass of its own: a
// run's end the engine's events missed is seen then.
const gatherRecheck = 2 * time.Minute

// gathersShown is how far back GET /v1/gathers lists the gathers that
// ended.
const gathersShown = 7 * 24 * time.Hour

// gatherCreator is the created_by of a proposal session.
const gatherCreator = "gather"

// SetSkills sets the skills folder a gather reads a skill from
// (ADR-0033): <dir>/<name>/SKILL.md.
func (s *Server) SetSkills(dir string) { s.skills = dir }

// TeamGather is team_gather (ADR-0033): the supervisor's gather, at any
// time, of the agents it names, or of every agent it can delegate to. It
// is recorded once per call (api.TeamCall), and the call run again
// answers as the first did.
func (s *Server) TeamGather(ctx context.Context, call TeamCall, g team.Gather) (string, error) {
	// The call's own context bounds what comes before the gather is
	// recorded: a run the owner aborted meanwhile starts none (Codex
	// review of #57, round 3). A gather recorded already answers by key.
	live := ctx
	ctx = context.WithoutCancel(ctx)
	if facts, err := s.gatherByKey(ctx, call.key()); err == nil {
		return team.GatherStartedText(facts.Gather.ID, facts.Gather.Skill, askedAgents(facts)), nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	if err := team.CheckGather(g); err != nil {
		return "", err
	}
	caller, err := s.callerProfile(ctx, call.Session)
	if err != nil {
		return "", err
	}
	if caller == nil || caller.Role != profiles.RoleSupervisor {
		return "", errors.New("only a supervisor gathers proposals: this session's profile is not one")
	}
	start, err := s.gatherStart(live, g)
	if err != nil {
		return "", err
	}
	start.Key, start.SupervisorSession = call.key(), call.Session
	// The gather's transaction runs under the call's context too: an
	// abort before it commits rolls it back (Codex review of #57, round
	// 4).
	s.workMu.Lock()
	facts, replayed, err := s.startGather(live, start)
	if err == nil && !replayed {
		s.publishGatherFacts(facts)
	}
	s.workMu.Unlock()
	if err != nil && live.Err() != nil {
		return "", fmt.Errorf("the gather was stopped: %w", live.Err())
	} else if err != nil {
		return "", err
	}
	s.deliverLater()
	return team.GatherStartedText(facts.Gather.ID, facts.Gather.Skill, askedAgents(facts)), nil
}

// gatherByKey is the gather key started, with its facts.
func (s *Server) gatherByKey(ctx context.Context, key string) (store.GatherFacts, error) {
	id, err := s.store.GatherIDByKey(ctx, key)
	if err != nil {
		return store.GatherFacts{}, err
	}
	return s.gatherRead(ctx, id)
}

// gatherJSON is a schedule's gather as its record keeps it: team.Gather's
// JSON, "" for a message.
func gatherJSON(g *profiles.GatherAction) string {
	if g == nil {
		return ""
	}
	encoded, err := json.Marshal(team.Gather{Skill: g.Skill, Agents: g.Agents, Note: strings.TrimSpace(g.Note)})
	if err != nil {
		return ""
	}
	return string(encoded)
}

// wireScheduleGather is a schedule's gather as the protocol has it, from
// its record's JSON: nil for a message.
func wireScheduleGather(record string) *proto.ScheduleGather {
	if record == "" {
		return nil
	}
	var g team.Gather
	if json.Unmarshal([]byte(record), &g) != nil {
		return nil
	}
	return &proto.ScheduleGather{Skill: g.Skill, Agents: g.Agents, Note: g.Note}
}

// publishGatherOf publishes the gather key started, under workMu.
func (s *Server) publishGatherOf(ctx context.Context, key string) {
	s.workMu.Lock()
	defer s.workMu.Unlock()
	s.publishGatherLocked(ctx, key)
}

// publishGatherLocked publishes the gather key started, read with workMu
// held: an ask a pass started meanwhile is never sent back to its start
// (Codex review of #57, round 1). A read that fails leaves the gather to a
// delivery pass (republishGathers).
func (s *Server) publishGatherLocked(ctx context.Context, key string) {
	facts, err := s.gatherByKey(ctx, key)
	if err != nil {
		s.log.Warn("read a gather to publish", "key", key, "err", err)
		s.gatherRetry[key] = true
		s.deliverLater()
		return
	}
	delete(s.gatherRetry, key)
	s.publishGatherFacts(facts)
}

func askedAgents(facts store.GatherFacts) []string {
	agents := make([]string, len(facts.Asks))
	for i, a := range facts.Asks {
		agents[i] = a.Agent
	}
	return agents
}

// gatherStart is the gather g asks for: its skill's text, read now from the
// skills folder as the agents' sandbox shows it (loadSkill), and its
// agents, each one a gather may ask (team.Askable), or every such agent of
// the team when g names none. A check that could not tell now, as a
// profile or the models pocketd could not read, is errNotNow: a
// schedule's time waits for a later pass (Codex review of #57, round 1).
func (s *Server) gatherStart(ctx context.Context, g team.Gather) (store.GatherStart, error) {
	skill, err := s.loadSkill(ctx, g.Skill)
	if err != nil {
		return store.GatherStart{}, err
	}
	if s.profiles == nil {
		return store.GatherStart{}, errors.New("pocketd has no profiles: it has no agent to ask")
	}
	models, err := s.engine.Models(ctx)
	if err != nil {
		return store.GatherStart{}, fmt.Errorf("%w: list the models: %v", errNotNow, err)
	}
	askable := func(p profiles.Profile) error { return team.Askable(p, func() error { return s.startable(p, models) }) }
	var agents []string
	if len(g.Agents) > 0 {
		for _, id := range g.Agents {
			p, err := s.profiles.Find(id)
			if profiles.Unreadable(err) {
				return store.GatherStart{}, fmt.Errorf("%w: read %s's profile: %v", errNotNow, id, err)
			} else if err != nil {
				return store.GatherStart{}, fmt.Errorf("no agent %s to ask: %w", id, err)
			}
			if err := askable(p); err != nil {
				return store.GatherStart{}, err
			}
			agents = append(agents, p.ID)
		}
	} else {
		valid, problems, err := s.profiles.Load()
		if err != nil {
			return store.GatherStart{}, fmt.Errorf("%w: read the profiles: %v", errNotNow, err)
		}
		// A profile the folder could not read now may be an agent to ask:
		// the gather waits for it, never leaves it out (Codex review of
		// #57, round 2). One that is invalid is no agent.
		for _, problem := range problems {
			p, err := s.profiles.Find(problem.ID)
			if profiles.Unreadable(err) {
				return store.GatherStart{}, fmt.Errorf("%w: read %s's profile: %v", errNotNow, problem.ID, err)
			} else if err == nil {
				// It reads now: it is asked as any other (Codex review of
				// #57, round 3).
				valid = append(valid, p)
			}
		}
		for _, p := range valid {
			if askable(p) == nil {
				agents = append(agents, p.ID)
			}
		}
		if len(agents) == 0 {
			return store.GatherStart{}, errors.New("no agent of the team can be asked: none takes a delegation with a home or workspace to propose in")
		}
		slices.Sort(agents)
	}
	return store.GatherStart{Skill: skill.Name, SkillText: skill.Text, Note: strings.TrimSpace(g.Note), Agents: agents}, nil
}

// loadSkill reads skill name from the skills folder as the agents'
// sandbox shows it (Engine.ReadUpTo), never with pocketd's own rights: a
// link an agent put there leads nowhere the agents' own tools could not
// read, and a named pipe is no skill (Codex review of #57, round 1). A
// skill that is no regular file there is profiles.ErrNoSkill; a read the
// sandbox could not make is errNotNow.
func (s *Server) loadSkill(ctx context.Context, name string) (profiles.Skill, error) {
	path, err := profiles.SkillPath(s.skills, name)
	if err != nil {
		return profiles.Skill{}, err
	}
	check, cancel := s.withTimeout(ctx, checkTimeout)
	defer cancel()
	data, ok, err := s.engine.ReadUpTo(check, path, profiles.MaxSkill)
	if err != nil {
		return profiles.Skill{}, fmt.Errorf("%w: read the skill %s: %v", errNotNow, name, err)
	}
	if !ok {
		return profiles.Skill{}, fmt.Errorf("%w: there is no regular file %s the agents can read", profiles.ErrNoSkill, path)
	}
	return profiles.ParseSkill(name, data)
}

// followGathers ends each open ask that is over, starts each that waits
// to start, and arms a pass at the next deadline, or at gatherRecheck while
// an ask runs. done says no start failed.
func (s *Server) followGathers(ctx context.Context) (done bool, err error) {
	asks, deadlines, err := s.store.OpenGatherAsks(ctx)
	if err != nil {
		return false, err
	}
	var failures []error
	done = true
	now := s.now()
	var next time.Time
	for i, ask := range asks {
		deadline := deadlines[i]
		switch {
		case !now.Before(deadline):
			// A session made before a stop is recorded first, its brief
			// dropped with the ask's end: the deadline stops a new session,
			// not the record of one made (Codex review of #57, round 5).
			if ask.State == team.AskStarting {
				if err := s.recoverAsk(ctx, ask, deadline); err != nil {
					failures = append(failures, fmt.Errorf("the ask of %s in G%d: %w", ask.Agent, ask.GatherID, err))
					continue
				}
			}
			if err := s.endAskOverTime(ctx, ask); err != nil {
				failures = append(failures, fmt.Errorf("the ask of %s in G%d: %w", ask.Agent, ask.GatherID, err))
			}
			continue
		case ask.State == team.AskStarting:
			if err := s.startAsk(ctx, ask, deadline); err != nil {
				failures = append(failures, fmt.Errorf("the ask of %s in G%d: %w", ask.Agent, ask.GatherID, err))
				done = false
			}
		case ask.State == team.AskRunning:
			if err := s.endAskIfIdle(ctx, ask); err != nil {
				failures = append(failures, fmt.Errorf("the ask of %s in G%d: %w", ask.Agent, ask.GatherID, err))
			}
		}
		wake := now.Add(gatherRecheck)
		if deadline.Before(wake) {
			wake = deadline
		}
		if next.IsZero() || wake.Before(next) {
			next = wake
		}
	}
	s.armGathers(next)
	return done && len(failures) == 0, errors.Join(failures...)
}

// armGathers has a delivery pass run at next, or none for zero.
func (s *Server) armGathers(next time.Time) {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	if s.gatherCancel != nil {
		s.gatherCancel()
		s.gatherCancel = nil
	}
	if s.stopped || next.IsZero() {
		return
	}
	s.gatherCancel = s.gatherTimer(max(next.Sub(s.now()), 0)+time.Second, s.deliverLater)
}

// recoverAsk records the session the engine made for a starting ask, if
// it made one (startAsk, which starts no new one past the deadline).
func (s *Server) recoverAsk(ctx context.Context, ask store.GatherAskRow, deadline time.Time) error {
	if _, exists, err := s.engine.Created(ctx, ask.SessionKey()); err != nil || !exists {
		return err
	}
	return s.startAsk(ctx, ask, deadline)
}

// endAskOverTime ends an ask whose time passed: done when it filed a
// proposal, else failed, as the store decides a timed end
// (store.EndGatherAsk).
func (s *Server) endAskOverTime(ctx context.Context, ask store.GatherAskRow) error {
	return s.endAsk(ctx, ask, team.AskDone, "", true)
}

// endAskIfIdle ends a running ask whose brief is in its session's
// transcript and whose session is idle: its run ended.
func (s *Server) endAskIfIdle(ctx context.Context, ask store.GatherAskRow) error {
	note, err := s.store.Note(ctx, ask.BriefKey())
	if errors.Is(err, store.ErrNotFound) {
		return nil
	} else if err != nil || note.SentAt == nil {
		return err
	}
	idle, err := s.engine.Idle(ctx, ask.Session)
	if err != nil || !idle {
		return err
	}
	return s.endAsk(ctx, ask, team.AskDone, "", true)
}

// endAsk ends ask with state and why (store.EndGatherAsk), or, timed, as
// the store's clock decides it in its transaction (Codex review of #57,
// round 3): its brief, still queued, is withdrawn first, under outboxMu,
// so the agent never spends a run on an ask that takes no proposal; one
// whose withdrawal fails ends at a later pass. The last ask's end files
// the supervisor's note.
func (s *Server) endAsk(ctx context.Context, ask store.GatherAskRow, state, why string, timed bool) error {
	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()
	if err := s.withdrawNote(ctx, ask.BriefKey()); err != nil {
		return fmt.Errorf("withdraw the ask's queued brief: %w", err)
	}
	s.workMu.Lock()
	facts, did, ended, err := s.store.EndGatherAsk(ctx, ask.ID, state, why, timed, func(f store.GatherFacts) string {
		return team.GatherDoneText(f.Gather.ID, f.Gather.Skill, askOutcomes(f))
	})
	if err == nil && did {
		s.publishGatherFacts(facts)
	}
	s.workMu.Unlock()
	if ended {
		s.deliverLater() // the supervisor's note
	}
	return err
}

// askOutcomes are a gather's asks with their proposals, for its note.
func askOutcomes(f store.GatherFacts) []team.AskOutcome {
	out := make([]team.AskOutcome, len(f.Asks))
	for i, a := range f.Asks {
		out[i] = team.AskOutcome{Agent: a.Agent, State: a.State, Why: a.Why}
		for _, p := range f.Proposals {
			if p.AskID == a.ID {
				out[i].Proposals = append(out[i].Proposals, team.ProposalTitle{ID: p.ID, Title: p.Title})
			}
		}
		slices.SortFunc(out[i].Proposals, func(x, y team.ProposalTitle) int { return int(x.ID - y.ID) })
	}
	return out
}

// withdrawNote withdraws every attempt of note key still queued in its
// session; outboxMu is held.
func (s *Server) withdrawNote(ctx context.Context, key string) error {
	note, err := s.store.Note(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	} else if err != nil || note.SentAt != nil || note.DroppedAt != nil {
		return err
	}
	for attempt := 0; attempt <= note.Attempt; attempt++ {
		if _, err := s.engine.Withdraw(ctx, note.SessionID, attemptKey(note.Key, attempt)); err != nil {
			return err
		}
	}
	return nil
}

// askGone is why an ask's session cannot start: its agent no longer takes
// one. The ask fails.
type askGone struct{ error }

// startAsk starts ask's proposal session, from its agent's profile in its
// home, else its workspace, with read and team_propose alone, and files
// its brief in the transaction that records the session
// (store.RecordGatherSession). The session is keyed by the ask's random
// owner, so a start run again after a restart finds the one made; no
// fresh session starts past the gather's deadline, and a fresh one starts
// from the profile as it is under the team's locks (recheckAsk).
func (s *Server) startAsk(ctx context.Context, ask store.GatherAskRow, deadline time.Time) error {
	key := ask.SessionKey()
	made, exists, err := s.engine.Created(ctx, key)
	if err != nil {
		return err
	}
	var spec SessionSpec
	profile, kept := "", ""
	if exists {
		// A session made by an earlier run keeps the profile it was made
		// from, as recorded, whatever the profile folder says now, and the
		// brief its start kept, naming that profile's repositories as they
		// were (Codex review of #57, rounds 1 and 2). With none kept, the
		// brief names the profile's repositories now: a read that fails
		// waits for a later pass, and a profile that is gone fails the ask.
		spec = SessionSpec{Title: made.Title, Cwd: made.Cwd, Model: made.Model, Thinking: made.Thinking, Route: made.Route, Gather: ask.ID}
		profile = made.Profile
		if kept, err = s.store.AskBrief(ctx, ask.ID); err != nil {
			return err
		}
		if profile != "" && kept == "" {
			if s.profiles == nil {
				return s.endAsk(ctx, ask, team.AskFailed, "it could not start: pocketd has no profiles", false)
			}
			p, err := s.profiles.Find(profile)
			if profiles.Unreadable(err) {
				return fmt.Errorf("read %s's profile: %w", profile, err)
			} else if err != nil {
				return s.endAsk(ctx, ask, team.AskFailed, team.Clip("it could not start: "+err.Error(), team.MaxFailure), false)
			}
			spec.Profile = &p
		}
	} else if spec, err = s.askSpec(ctx, ask); err != nil {
		var gone askGone
		if errors.As(err, &gone) {
			return s.endAsk(ctx, ask, team.AskFailed, team.Clip("it could not start: "+gone.Error(), team.MaxFailure), false)
		}
		return err
	}
	if s.afterAskSpec != nil {
		s.afterAskSpec()
	}
	facts, err := s.store.GatherFacts(ctx, ask.GatherID)
	if err != nil {
		return err
	}
	supervisor := "The supervisor"
	if row, err := s.store.Session(ctx, facts.Gather.SupervisorSession); err == nil && row.Profile != "" {
		supervisor = s.agentName(row.Profile)
	}
	brief := func(spec SessionSpec) string {
		if kept != "" {
			return kept
		}
		repos := map[string]string{}
		if spec.Profile != nil {
			repos = spec.Profile.Repositories
		}
		g := facts.Gather
		return team.ProposalBrief(g.ID, supervisor, g.Skill, g.SkillText, g.Note, repos)
	}
	session, spec, err := s.createAskSession(ctx, ask.ID, key, spec, !exists, profile, deadline, facts.Gather.Key, brief)
	var gone askGone
	switch {
	case errors.As(err, &gone):
		return s.endAsk(ctx, ask, team.AskFailed, team.Clip("it could not start: "+gone.Error(), team.MaxFailure), false)
	case err != nil:
		return err
	case session == 0:
		return nil
	}
	// A session made by an earlier run had lessons this run cannot tell:
	// a refresh brings them.
	if spec.Profile != nil {
		profile = spec.Profile.ID
	}
	if profile != "" {
		s.lessonsSince(ctx, profile, spec.Lessons)
	}
	// The watch starts before the brief goes, so its entry and the agent's
	// proposals reach the event stream; the brief waits for a watch that
	// works (sendOldestNote).
	if err := s.engine.Watch(ctx, session); err != nil {
		return fmt.Errorf("watch the proposal session: %w", err)
	}
	// Its brief, filed now, goes at the next pass.
	s.deliverLater()
	return nil
}

// createAskSession creates ask id's session from spec and records it with
// its brief, under outboxMu and workMu, from a fresh read of the ask: one
// that ended meanwhile starts no session (0). A new session (fresh) starts
// from its agent's profile as it is now (recheckAsk), never past
// deadline; a rule that no longer holds is askGone. profile is the
// profile a session made by an earlier run was made from. brief is the
// brief of the session spec starts; a new session's is kept with its ask
// before the session is made (store.PrepareAskBrief), so a start run again
// after a stop files the same one.
func (s *Server) createAskSession(ctx context.Context, id int64, key string, spec SessionSpec, fresh bool, profile string, deadline time.Time, gatherKey string, brief func(SessionSpec) string) (int64, SessionSpec, error) {
	s.outboxMu.Lock()
	defer s.outboxMu.Unlock()
	s.workMu.Lock()
	defer s.workMu.Unlock()
	ask, err := s.store.GatherAsk(ctx, id)
	if err != nil || ask.State != team.AskStarting {
		return 0, spec, err
	}
	if fresh {
		if spec, err = s.recheckAsk(ctx, ask, spec); err != nil {
			return 0, spec, err
		}
		if !s.now().Before(deadline) {
			return 0, spec, nil
		}
	}
	text := brief(spec)
	if fresh {
		if err := s.store.PrepareAskBrief(ctx, id, text); err != nil {
			return 0, spec, err
		}
		if s.afterAskBrief != nil {
			s.afterAskBrief()
		}
		// The write may have waited: no new session past the deadline
		// (Codex review of #57, round 5).
		if !s.now().Before(deadline) {
			return 0, spec, nil
		}
	}
	session, err := s.engine.CreateSession(ctx, key, spec)
	if err != nil {
		return 0, spec, err
	}
	if spec.Profile != nil {
		profile = spec.Profile.ID
	}
	title := spec.Title
	if title == "" {
		title = "Session " + strconv.FormatInt(session, 10)
	}
	recorded, err := s.store.RecordGatherSession(ctx, key, store.SessionRow{ID: session, Title: title, Cwd: spec.Cwd, Provider: spec.Model.Provider,
		Model: spec.Model.ID, Thinking: spec.Thinking, Profile: profile, Route: spec.Route, CreatedBy: gatherCreator}, id, func(store.GatherRow) string { return text })
	if err != nil {
		return 0, spec, err
	}
	s.publishSession(session, true)
	s.publishGatherLocked(ctx, gatherKey)
	if recorded.State != team.AskRunning {
		return 0, spec, nil
	}
	return session, spec, nil
}

// recheckAsk reads ask's agent's profile again, right before its new
// session starts, under the team's locks: it must still be one a gather
// asks (team.Askable), in the folder spec was made for; the session starts
// from it, with the router's model and its lessons as they are now (Codex
// review of #57, round 1). A rule that no longer holds is askGone.
func (s *Server) recheckAsk(ctx context.Context, ask store.GatherAskRow, spec SessionSpec) (SessionSpec, error) {
	if s.profiles == nil {
		return SessionSpec{}, askGone{errors.New("pocketd has no profiles")}
	}
	// The slow check first, with a deadline of its own, since the team's
	// locks are held.
	check, cancel := s.withTimeout(ctx, checkTimeout)
	defer cancel()
	dir, err := s.engine.Dir(check, spec.Cwd)
	if err != nil {
		return SessionSpec{}, fmt.Errorf("look at %s: %w", spec.Cwd, err)
	}
	if !dir {
		return SessionSpec{}, askGone{fmt.Errorf("%s is not a folder", spec.Cwd)}
	}
	// Then the lessons, the models and the profile, read last.
	lessons, err := s.store.ActiveLessons(ctx, ask.Agent)
	if err != nil {
		return SessionSpec{}, err
	}
	models, err := s.engine.Models(ctx)
	if err != nil {
		return SessionSpec{}, err
	}
	p, err := s.profiles.Find(ask.Agent)
	if profiles.Unreadable(err) {
		return SessionSpec{}, fmt.Errorf("read %s's profile: %w", ask.Agent, err)
	} else if err != nil {
		return SessionSpec{}, askGone{err}
	}
	if err := team.Askable(p, func() error { return s.startable(p, models) }); err != nil {
		return SessionSpec{}, askGone{err}
	}
	if p.Start() != spec.Cwd {
		return SessionSpec{}, askGone{fmt.Errorf("%s's home changed while its proposal session started", p.ID)}
	}
	choice, err := router.Choose(router.Request{Profile: &p}, models)
	if err != nil {
		return SessionSpec{}, askGone{err}
	}
	spec.Profile, spec.Model, spec.Route, spec.Lessons = &p, choice.Model, string(choice.Rule), sectionOf(lessons)
	return spec, nil
}

// askSpec is what ask's new proposal session starts from: its agent's
// profile, checked now (team.Askable), the router's model, and its home,
// else its workspace, a folder as the sandbox sees it. A rule that no
// longer holds is askGone; a check that could not tell is another error.
func (s *Server) askSpec(ctx context.Context, ask store.GatherAskRow) (SessionSpec, error) {
	if s.profiles == nil {
		return SessionSpec{}, askGone{errors.New("pocketd has no profiles")}
	}
	p, err := s.profiles.Find(ask.Agent)
	if profiles.Unreadable(err) {
		return SessionSpec{}, fmt.Errorf("read %s's profile: %w", ask.Agent, err)
	} else if err != nil {
		return SessionSpec{}, askGone{err}
	}
	models, err := s.engine.Models(ctx)
	if err != nil {
		return SessionSpec{}, err
	}
	if err := team.Askable(p, func() error { return s.startable(p, models) }); err != nil {
		return SessionSpec{}, askGone{err}
	}
	cwd := p.Start()
	check, cancel := s.withTimeout(ctx, checkTimeout)
	defer cancel()
	dir, err := s.engine.Dir(check, cwd)
	if err != nil {
		return SessionSpec{}, fmt.Errorf("look at %s: %w", cwd, err)
	}
	if !dir {
		return SessionSpec{}, askGone{fmt.Errorf("%s is not a folder", cwd)}
	}
	choice, err := router.Choose(router.Request{Profile: &p}, models)
	if err != nil {
		return SessionSpec{}, askGone{err}
	}
	lessons, err := s.store.ActiveLessons(ctx, p.ID)
	if err != nil {
		return SessionSpec{}, err
	}
	facts, err := s.store.GatherFacts(ctx, ask.GatherID)
	if err != nil {
		return SessionSpec{}, err
	}
	return SessionSpec{Title: fmt.Sprintf("G%d · %s proposals", ask.GatherID, facts.Gather.Skill), Cwd: cwd, Model: choice.Model, Profile: &p,
		Route: string(choice.Rule), Gather: ask.ID, Lessons: sectionOf(lessons)}, nil
}

// briefWaits says whether session is a proposal session whose brief is not
// in its transcript yet, nor dropped: no other message goes before it.
func (s *Server) briefWaits(ctx context.Context, session int64) (bool, store.GatherAskRow, error) {
	ask, err := s.store.GatherAskOfSession(ctx, session)
	if errors.Is(err, store.ErrNotFound) {
		return false, ask, nil
	} else if err != nil {
		return false, ask, err
	}
	note, err := s.store.Note(ctx, ask.BriefKey())
	if errors.Is(err, store.ErrNotFound) {
		return false, ask, nil
	} else if err != nil {
		return false, ask, err
	}
	return note.SentAt == nil && note.DroppedAt == nil, ask, nil
}

// briefCheck is the check before an ask's brief is submitted: the ask
// runs, its gather's time has not passed, and its session is watched.
func (s *Server) briefCheck(ctx context.Context, owner string, session int64) (bool, func(context.Context) error, error) {
	ask, err := s.store.GatherAskByOwner(ctx, owner)
	if err != nil {
		return false, nil, err
	}
	facts, err := s.store.GatherFacts(ctx, ask.GatherID)
	if err != nil {
		return false, nil, err
	}
	if ask.State != team.AskRunning {
		return false, nil, nil // its end drops the brief
	}
	if !s.now().Before(facts.Gather.Deadline) {
		return false, nil, nil // past its time, the brief waits for the ask's end
	}
	if err := s.engine.Watch(ctx, session); err != nil {
		return false, nil, fmt.Errorf("watch the proposal session: %w", err)
	}
	check := func(context.Context) error {
		ask, err := s.store.GatherAskByOwner(ctx, owner)
		if err != nil {
			return err
		}
		if ask.State != team.AskRunning || !s.now().Before(facts.Gather.Deadline) {
			return fmt.Errorf("the ask of %s in G%d takes no proposal now: its brief waits for its end", ask.Agent, ask.GatherID)
		}
		return nil
	}
	return true, check, nil
}

// TeamPropose is team_propose (ADR-0033): an asked agent's own proposals,
// from its proposal session alone. An add is recorded once per call, and
// an edit or a drop answers as it did when its call runs again.
func (s *Server) TeamPropose(ctx context.Context, call TeamCall, c team.ProposeCall) (string, error) {
	ctx = context.WithoutCancel(ctx)
	if err := team.CheckPropose(c); err != nil {
		return "", err
	}
	ask, err := s.store.GatherAskOfSession(ctx, call.Session)
	if errors.Is(err, store.ErrNotFound) {
		return "", errors.New("team_propose is a proposal session's: this session answers no gather")
	} else if err != nil {
		return "", err
	}
	switch c.Action {
	case team.ProposeAdd:
		if text, err := s.store.ProposalCall(ctx, call.key()); err == nil {
			return text, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return "", err
		}
		repos, err := s.agentRepositories(ask.Agent)
		if err != nil {
			return "", err
		}
		p, err := c.Apply(team.Proposal{}, repos, true)
		if err != nil {
			return "", err
		}
		s.workMu.Lock()
		row, text, replayed, err := s.store.AddProposal(ctx, call.key(), ask.ID, p, ask.Agent, proposedText)
		if err == nil && !replayed {
			s.publishProposalLocked(ctx, row)
		}
		s.workMu.Unlock()
		if err != nil {
			return "", conflictText(err)
		}
		return text, nil
	case team.ProposeEdit, team.ProposeDrop:
		// Its own, while its part runs and before its gather's time,
		// decided in the change's transaction (store.ChangeProposal).
		return s.changeProposal(ctx, call.key(), c.Row, c.Rev, c.Action == team.ProposeDrop, ask.ID, c.ProposalFields, nil)
	case team.ProposeList:
		mine, err := s.store.ProposalsOfAsk(ctx, ask.ID)
		if err != nil {
			return "", err
		}
		lines := make([]string, len(mine))
		for i, p := range mine {
			lines[i] = proposalLine(p, time.Time{}, s.zone)
		}
		return linesPage(lines, c.Page, "You have filed no proposal in this gather.", "your proposals", "team_propose")
	}
	// done: past the gather's time, the ask ends as its time's end does,
	// whether or not a pass has ended it yet, decided by the store's clock
	// (Codex review of #57, rounds 1 and 3).
	current, err := s.store.GatherAsk(ctx, ask.ID)
	if err != nil {
		return "", err
	}
	if team.AskOpen(current.State) {
		if err := s.endAsk(ctx, current, team.AskDone, "", true); err != nil {
			return "", err
		}
		if current, err = s.store.GatherAsk(ctx, ask.ID); err != nil {
			return "", err
		}
	}
	mine, err := s.store.ProposalsOfAsk(ctx, ask.ID)
	if err != nil {
		return "", err
	}
	if current.Why == team.GatherTimeWhy {
		return fmt.Sprintf("G%d's %d minutes had passed: your part ended with %d proposals.", ask.GatherID, int(team.GatherLimit/time.Minute), len(mine)), nil
	}
	return fmt.Sprintf("Your part in G%d is done, with %d proposals. The supervisor hears once every agent asked has answered.", ask.GatherID, len(mine)), nil
}

// agentRepositories are agent's repositories by name, from its profile now.
func (s *Server) agentRepositories(agent string) ([]string, error) {
	if s.profiles == nil {
		return nil, errors.New("pocketd has no profiles")
	}
	p, err := s.profiles.Find(agent)
	if err != nil {
		return nil, fmt.Errorf("read %s's profile: %w", agent, err)
	}
	names := make([]string, 0, len(p.Repositories))
	for name := range p.Repositories {
		names = append(names, name)
	}
	slices.Sort(names)
	return names, nil
}

// changeProposal edits or drops proposal id at rev, once per key, after
// allowed passes; an edit's fields are checked against its agent's
// repositories. ask is the ask of the agent that changes it, 0 for its
// supervisor (store.ProposalChange).
func (s *Server) changeProposal(ctx context.Context, key string, id int64, rev int, drop bool, ask int64, fields team.ProposalFields, allowed func(store.ProposalRow) error) (string, error) {
	change := func(p store.ProposalRow) (store.ProposalRow, error) {
		if drop {
			p.State = team.ProposalDropped
			return p, nil
		}
		repos, err := s.agentRepositories(p.Agent)
		if err != nil {
			return p, err
		}
		text, err := fields.Apply(p.Text(), repos, false)
		if err != nil {
			return p, err
		}
		p.Repository, p.Title, p.Task, p.StopWhen, p.Note, p.MaxRounds = text.Repository, text.Title, text.Task, text.StopWhen, text.Note, text.MaxRounds
		return p, nil
	}
	answer := func(p store.ProposalRow) string {
		if drop {
			return fmt.Sprintf("Dropped #%d: it leaves the board.", p.ID)
		}
		return fmt.Sprintf("Edited #%d: it is at rev %d now.", p.ID, p.Rev)
	}
	s.workMu.Lock()
	row, text, replayed, err := s.store.ChangeProposal(ctx, store.ProposalChange{Key: key, ID: id, Rev: rev, Ask: ask, Allowed: allowed, Change: change, Answer: answer})
	if err == nil && !replayed {
		s.publishProposalLocked(ctx, row)
	}
	s.workMu.Unlock()
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("there is no proposal #%d", id)
	} else if err != nil {
		return "", conflictText(err)
	}
	return text, nil
}

// conflictText is err as an agent reads it, without the store's word for
// a conflict.
func conflictText(err error) error {
	if errors.Is(err, store.ErrConflict) {
		return errors.New(strings.TrimPrefix(err.Error(), store.ErrConflict.Error()+": "))
	}
	return err
}

func proposedText(p store.ProposalRow) string {
	return fmt.Sprintf("Proposed #%d at rev %d: %s", p.ID, p.Rev, p.Title)
}

// proposalLine is a proposal as a list shows it: its number and rev, its
// agent, repository, kind and rounds, its gather and when it started, and
// its title.
func proposalLine(p store.ProposalRow, gathered time.Time, zone *time.Location) string {
	where := "home"
	if p.Repository != "" {
		where = p.Repository
	}
	line := fmt.Sprintf("#%d rev %d · %s · %s · %s · %d rounds · G%d", p.ID, p.Rev, p.Agent, where, p.Kind, p.MaxRounds, p.GatherID)
	if !gathered.IsZero() {
		line += " (" + schedule.When(gathered, zone) + ")"
	}
	return line + " · " + p.Title
}

// TeamBoard is team_board (ADR-0033): the supervisor's proposals, those of
// the gathers its profile's sessions asked for. It lists and reads them,
// edits and drops one at its rev, and orders them; an edit, a drop or an
// order answers as it did when its call runs again.
func (s *Server) TeamBoard(ctx context.Context, call TeamCall, c team.BoardCall) (string, error) {
	ctx = context.WithoutCancel(ctx)
	if err := team.CheckBoard(c); err != nil {
		return "", err
	}
	// A change recorded already answers as it did, before the caller's
	// profile is read again: its board changed, whatever the profile says
	// now (Codex review of #57, round 2).
	if c.Action == team.BoardEdit || c.Action == team.BoardDrop || c.Action == team.BoardOrder {
		if text, err := s.store.ProposalCall(ctx, call.key()); err == nil {
			return text, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return "", err
		}
	}
	caller, err := s.callerProfile(ctx, call.Session)
	if err != nil {
		return "", err
	}
	if caller == nil || caller.Role != profiles.RoleSupervisor {
		return "", errors.New("only a supervisor keeps the board: this session's profile is not one")
	}
	// A proposal is on the board of the profile whose session asked for
	// its gather, chosen in the store's query (Codex review of #57, round
	// 1).
	allowed := func(p store.ProposalRow) error {
		profile, _, err := s.store.ProposalBoard(ctx, p.ID)
		if err != nil {
			return err
		}
		if profile != caller.ID {
			return fmt.Errorf("#%d is on another supervisor's board", p.ID)
		}
		return nil
	}
	switch c.Action {
	case team.BoardList:
		rows, err := s.store.BoardOf(ctx, caller.ID)
		if err != nil {
			return "", err
		}
		lines := make([]string, len(rows))
		for i, p := range rows {
			lines[i] = proposalLine(p.ProposalRow, p.GatheredAt, s.zone)
		}
		return boardPage(lines, c.Page)
	case team.BoardRead:
		p, err := s.store.Proposal(ctx, c.Row)
		if errors.Is(err, store.ErrNotFound) {
			return "", fmt.Errorf("there is no proposal #%d", c.Row)
		} else if err != nil {
			return "", err
		}
		profile, gathered, err := s.store.ProposalBoard(ctx, p.ID)
		if err != nil {
			return "", err
		}
		if profile != caller.ID {
			return "", fmt.Errorf("#%d is on another supervisor's board", p.ID)
		}
		return proposalText(p, gathered, s.zone), nil
	case team.BoardOrder:
		// The order and its events go out together under workMu, with the
		// rows as the order left them; a call run again sends none (Codex
		// review of #57, round 1).
		s.workMu.Lock()
		rows, text, replayed, err := s.store.OrderProposals(ctx, call.key(), c.Rows, allowed, func(rows []store.ProposalRow) string {
			ids := make([]string, len(rows))
			for i, r := range rows {
				ids[i] = "#" + strconv.FormatInt(r.ID, 10)
			}
			return "Ordered: " + strings.Join(ids, ", ") + " are first on the board, in that order."
		})
		if err == nil && !replayed {
			s.publishOrderLocked(ctx, rows)
		}
		s.workMu.Unlock()
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrConflict) {
			return "", conflictText(err)
		} else if err != nil {
			return "", err
		}
		return text, nil
	}
	return s.changeProposal(ctx, call.key(), c.Row, c.Rev, c.Action == team.BoardDrop, 0, c.ProposalFields, allowed)
}

// boardPage is page page (from 1) of the board's lines (linesPage).
func boardPage(lines []string, page int) (string, error) {
	return linesPage(lines, page, "The board holds no proposal of yours.", "the board", "team_board")
}

// linesPage is page page (from 1) of a list's lines, in pages of at most
// team.MaxPageBytes bytes and team.MaxPageLines lines, each saying whether
// one follows, and tool's list call that reads it; empty is the answer for
// no line.
func linesPage(lines []string, page int, empty, list, tool string) (string, error) {
	if len(lines) == 0 {
		return empty, nil
	}
	pages := team.Pages(strings.Join(lines, "\n"))
	if page == 0 {
		page = 1
	}
	if page > len(pages) {
		return "", fmt.Errorf("%s has %d pages: there is no page %d", list, len(pages), page)
	}
	text := strings.TrimSuffix(pages[page-1], "\n")
	if len(pages) > 1 {
		text = fmt.Sprintf("Page %d of %d of %s.\n%s", page, len(pages), list, text)
		if page < len(pages) {
			text += fmt.Sprintf("\n(More: %s list with page %d.)", tool, page+1)
		}
	}
	return text, nil
}

// proposalText is one proposal whole, as team_board read shows it.
func proposalText(p store.ProposalRow, gathered time.Time, zone *time.Location) string {
	var b strings.Builder
	b.WriteString(proposalLine(p, gathered, zone))
	fmt.Fprintf(&b, "\n\nTask:\n%s\n\nStop when:\n%s", p.Task, p.StopWhen)
	if p.Note != "" {
		fmt.Fprintf(&b, "\n\nNote: %s", p.Note)
	}
	return b.String()
}

// publishGatherFacts sends a gather as facts read it; workMu is held, so
// gather events leave in the order the gathers changed.
func (s *Server) publishGatherFacts(facts store.GatherFacts) {
	g := wireGather(facts)
	s.emit(proto.Event{Type: proto.EventGather, Gather: &g})
}

// publishProposalLocked sends a proposal, and its gather, whose counts it
// changed; workMu is held. A read of its gather that fails leaves the
// proposal to a delivery pass (republishGathers): its change stands, so
// its event must still go (Codex review of #57, round 1).
func (s *Server) publishProposalLocked(ctx context.Context, row store.ProposalRow) {
	facts, err := s.gatherRead(ctx, row.GatherID)
	if err != nil {
		s.log.Warn("read a proposal's gather to publish", "proposal", row.ID, "err", err)
		s.proposalRetry[row.ID] = true
		s.deliverLater()
		return
	}
	delete(s.proposalRetry, row.ID)
	p := wireProposal(row, facts.Gather.CreatedAt)
	s.emit(proto.Event{Type: proto.EventProposal, Proposal: &p})
	s.publishGatherFacts(facts)
}

// publishOrderLocked sends the proposals an order moved, each with its
// gather's start, read once a gather; an order changes no gather's
// counts, so no gather goes (Codex review of #57, round 3). workMu is
// held; a read that fails leaves its proposals to a delivery pass.
func (s *Server) publishOrderLocked(ctx context.Context, rows []store.ProposalRow) {
	started := map[int64]time.Time{}
	for _, row := range rows {
		at, ok := started[row.GatherID]
		if !ok {
			var err error
			if at, err = s.gatherStartedAt(ctx, row.GatherID); err != nil {
				s.log.Warn("read an ordered proposal's gather to publish", "proposal", row.ID, "err", err)
				// Its retry owes the proposal alone, unless an earlier
				// change owes its gather too (Codex review of #57, round 4).
				if _, owed := s.proposalRetry[row.ID]; !owed {
					s.proposalRetry[row.ID] = false
				}
				s.deliverLater()
				continue
			}
			started[row.GatherID] = at
		}
		if !s.proposalRetry[row.ID] {
			delete(s.proposalRetry, row.ID)
		}
		p := wireProposal(row, at)
		s.emit(proto.Event{Type: proto.EventProposal, Proposal: &p})
	}
}

// republishGathers publishes each gather and proposal whose event a
// failed read held back (publishGatherLocked, publishProposalLocked), as
// they are now, under workMu; done says none is left.
func (s *Server) republishGathers(ctx context.Context) (done bool) {
	s.workMu.Lock()
	defer s.workMu.Unlock()
	for key := range s.gatherRetry {
		if facts, err := s.gatherByKey(ctx, key); err == nil {
			delete(s.gatherRetry, key)
			s.publishGatherFacts(facts)
		}
	}
	for id, gather := range s.proposalRetry {
		row, err := s.store.Proposal(ctx, id)
		switch {
		case err != nil:
		case gather:
			s.publishProposalLocked(ctx, row)
		default:
			s.publishOrderLocked(ctx, []store.ProposalRow{row})
		}
	}
	return len(s.gatherRetry) == 0 && len(s.proposalRetry) == 0
}

func wireGather(f store.GatherFacts) proto.Gather {
	g := proto.Gather{ID: f.Gather.ID, Supervisor: f.Gather.SupervisorSession, Skill: f.Gather.Skill, Note: f.Gather.Note, Schedule: f.Gather.Schedule,
		State: proto.GatherState(f.Gather.State), Deadline: f.Gather.Deadline, CreatedAt: f.Gather.CreatedAt, EndedAt: f.Gather.EndedAt, Asks: []proto.GatherAsk{}}
	for _, a := range f.Asks {
		ask := proto.GatherAsk{Agent: a.Agent, State: proto.GatherAskState(a.State), Session: a.Session, Why: a.Why}
		for _, p := range f.Proposals {
			if p.AskID == a.ID {
				ask.Proposals++
			}
		}
		g.Asks = append(g.Asks, ask)
	}
	return g
}

func wireProposal(p store.ProposalRow, gathered time.Time) proto.Proposal {
	return proto.Proposal{ID: p.ID, Gather: p.GatherID, GatheredAt: gathered, Agent: p.Agent, Repository: p.Repository, Title: p.Title, Task: p.Task,
		StopWhen: p.StopWhen, MaxRounds: p.MaxRounds, Kind: p.Kind, Note: p.Note, Order: p.Order, Rev: p.Rev, State: proto.ProposalState(p.State), By: p.By,
		Launch: p.LaunchID, Thread: p.ThreadID,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt}
}

// listGathers answers GET /v1/gathers: the gathers that run, and those
// that ended in the last gathersShown, the newest first.
func (s *Server) listGathers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.GathersSince(r.Context(), s.now().Add(-gathersShown))
	if err != nil {
		s.internal(w, "list gathers", err)
		return
	}
	out := make([]proto.Gather, 0, len(rows))
	for _, f := range rows {
		out = append(out, wireGather(f))
	}
	writeJSON(w, http.StatusOK, out)
}

// listProposals answers GET /v1/proposals: the proposals not dropped, in
// the board's order, each with its gather's start, read with one query.
func (s *Server) listProposals(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.Board(r.Context())
	if err != nil {
		s.internal(w, "list proposals", err)
		return
	}
	out := make([]proto.Proposal, 0, len(rows))
	for _, p := range rows {
		out = append(out, wireProposal(p.ProposalRow, p.GatheredAt))
	}
	writeJSON(w, http.StatusOK, out)
}
