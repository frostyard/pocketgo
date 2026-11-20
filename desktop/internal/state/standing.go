package state

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	"github.com/frostyard/pocketgo/proto"
)

// A profile's standing session (ADR-0026, protocol rules 7c and 7d) is the
// one session its agent keeps: New Session opens it, and Start Over gives
// it a fresh context in the same session.

// NewSessionRequest is the CreateSession the New Session form sends: for a
// standing profile, the profile alone, since its standing session has its
// own folder, model and title (rule 7c); otherwise what the form says.
func NewSessionRequest(profile *proto.Profile, cwd, title string, model proto.ModelRef, thinking string) proto.CreateSession {
	if profile != nil && profile.Standing {
		return proto.CreateSession{Profile: profile.ID}
	}
	request := proto.CreateSession{Cwd: cwd, Title: title, Model: model, Thinking: thinking}
	if profile != nil {
		request.Profile = profile.ID
	}
	return request
}

// OpenableProfiles are profiles as New Session offers them: a standing
// profile whose standing session the client knows (sessions) opens that
// session whatever its problem now, since opening one needs none of a new
// one's checks (rule 7c), so its problem is dropped. nil stays nil.
func OpenableProfiles(profiles []proto.Profile, sessions []proto.Session) []proto.Profile {
	if profiles == nil {
		return nil
	}
	open := map[string]bool{}
	for _, s := range sessions {
		if s.Standing && s.Profile != "" {
			open[s.Profile] = true
		}
	}
	out := slices.Clone(profiles)
	for i := range out {
		if out[i].Standing && open[out[i].ID] {
			out[i].Problem = ""
		}
	}
	return out
}

// StartOverBlocked says why the owner cannot start session s over now,
// "" when they can (rule 7d): only a standing session starts over, and
// only once its detail loaded (o, the open session) and showed no run and
// no message waiting for one, with the session list's state idle too. A
// detail still loading, or whose load or reload failed, shows nothing yet.
// A run
// that failed leaves its queued messages waiting, with the session idle:
// the next message's run takes them first.
func StartOverBlocked(s proto.Session, o *OpenSession) string {
	switch {
	case !s.Standing:
		return "Only a standing session starts over"
	case o == nil || o.ID != s.ID || !o.Loaded:
		return "Start Over waits until the session has loaded"
	case o.Err != "":
		// A reload that failed leaves what loaded before, which may miss
		// what happened since (Codex review of #43, round 3).
		return "Start Over waits until the session's transcript reloads"
	case o.Live.Busy || s.State == proto.SessionRunning || s.State == proto.SessionWaiting:
		return "Start Over waits until the run ends"
	case len(o.Live.Queued) > 0 || o.Live.QueuedMore > 0 || o.Live.Pending:
		return "Start Over waits until the queued messages have run: the next message runs them first"
	}
	return ""
}

// StandingNote says, under a standing profile chosen in New Session, what
// starting it does.
func StandingNote(p proto.Profile) string {
	name := p.Name
	if name == "" {
		name = p.ID
	}
	return name + " keeps one standing session: this opens it, with its own folder, model and title. Start Over in it gives it a fresh context."
}

// StartOverQuestion and StartOverBody are the confirmation Start Over asks
// first.
func StartOverQuestion(s proto.Session) string {
	return "Start " + titleOf(s) + " over?"
}

const StartOverBody = "The agent's context starts afresh in this session: it no longer sees the conversation so far, " +
	"which stays in the transcript. Its threads keep reporting here, and its profile and lessons stay. " +
	"A session at work cannot start over: wait until it is idle, or abort its run."

func titleOf(s proto.Session) string {
	if s.Title != "" {
		return s.Title
	}
	return "session " + strconv.FormatInt(s.ID, 10)
}

// startOverTarget is what a Start Over of session id acts on: the context
// as its newest entry the client has leaves it. A Start Over tried again
// unchanged keeps its request id; one after the session wrote more is a
// new request, even when the first one's answer was lost after it took
// effect, so a later Start Over is never only a replay of the first.
func (d *Data) startOverTarget(id int64) string {
	if o := d.open; o != nil && o.ID == id && len(o.Entries) > 0 {
		return "after:" + strconv.FormatInt(o.Entries[len(o.Entries)-1].ID, 10)
	}
	return "after:none"
}

// StartOver gives standing session id a fresh context (POST
// /v1/sessions/{id}/start-over); StartingOver is where it stands. The
// reset reaches the transcript through the event stream.
func (s *Store) StartOver(id int64) {
	p := pending(s.startOvers, id)
	if p.Busy {
		return
	}
	// Checked again at the click, which a confirmation may hold a while:
	// a reload that failed meanwhile, or a run that began, holds it too
	// (Codex review of #43, round 4).
	session, _ := s.Data.Session(id)
	if why := StartOverBlocked(session, s.Data.Open()); why != "" {
		p.Err = why
		return
	}
	key := fmt.Sprintf("startover:%d", id)
	request := proto.StartOver{RequestID: s.ids.For(key, s.Data.startOverTarget(id))}
	p.Busy, p.Err = true, ""
	s.log(LevelCommand, "Start session %d over", id)
	s.do(fmt.Sprintf("Starting session %d over", id), func(ctx context.Context, c *proto.Client) error {
		_, err := c.StartOver(ctx, id, request)
		return err
	}, func(err error) {
		p.Busy = false
		if err != nil {
			p.Err = err.Error()
			return
		}
		s.ids.Done(key)
	})
}

// StartingOver is where the owner's Start Over of session id stands.
func (s *Store) StartingOver(id int64) Pending { return valueOf(s.startOvers[id]) }
