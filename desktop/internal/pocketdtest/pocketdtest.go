// Package pocketdtest is a fake pocketd for the desktop client's tests: the
// endpoints of the protocol (docs/specs/protocol.md) over in-memory
// state, served in memory by Transport, so the tests open no socket. Tests
// shape its state, emit events to its streams, drop the streams or restart
// it, make requests fail, and read the requests it got. Only tests import
// it.
package pocketdtest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/proto"
)

// Token is the device token the server takes.
const Token = "pgd_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// URL is the server's service URL. Its host is loopback, so signing in to
// it asks for no confirmation; Transport serves it without a socket.
const URL = "http://localhost:8788"

// Now is the server's clock, which never moves.
var Now = time.Date(2026, 11, 18, 22, 0, 0, 0, time.UTC)

// Request is a request the server got.
type Request struct {
	Method, Path string
	// Query is the request's query string, as an upload's.
	Query string
	// LastEventID is the stream request's Last-Event-ID header.
	LastEventID string
	Body        []byte
}

// Server is the fake pocketd.
type Server struct {
	// URL is the service URL to sign in to (the URL constant).
	URL string

	handler  http.Handler
	mu       sync.Mutex
	token    string
	models   []proto.Model
	profiles []proto.Profile
	// prompts are what GET /v1/prompts answers (ADR-0051).
	prompts  proto.Prompts
	sessions map[int64]*session
	// questions are the sessions' questions (ADR-0048), waiting and
	// settled, by id (question.go).
	questions map[string]proto.Question
	approvals []proto.Approval
	asks      []proto.Ask
	lessons   []proto.Lesson
	threads   []proto.Thread
	pulls     []proto.PullRequest
	plans     []proto.Plan
	// proposals are the board's, schedules what GET /v1/schedules
	// answers, launches the launches made, oldest first, and
	// launchRequests the bodies of the launches' requests (nightshift.go).
	proposals      []proto.Proposal
	schedules      proto.Schedules
	launches       []proto.Launch
	launchRequests []proto.CreateLaunch
	// usage is what GET /v1/usage answers (usage.go).
	usage proto.UsageReport
	// shells are the owner's terminals, oldest first (terminals.go), and
	// nextShell numbers the next one.
	shells     []*fakeShell
	nextShell  int
	shellReply func(id string, keys []byte) string
	// startOutput is what a shell prints as it starts (PrintOnStart).
	startOutput string
	// lessonRoom is how many lessons in force an agent may keep; 0, any.
	lessonRoom int
	nextID     int64
	submitted  int64
	seq        int64
	kept       []proto.Event
	streams    map[chan proto.Event]struct{}
	requests   []Request
	replies    map[string]reply         // request id → its operation and first answer
	failures   map[string]int           // "METHOD path" → status to answer with once
	lose       map[string]chan struct{} // "METHOD path" → do it, wait for the channel, then answer 502 once
	hold       map[string]chan struct{} // "METHOD path" → do it, wait for the channel, then give its answer once
	holding    map[string]int           // "METHOD path" → answers made and held back now (Holding)
	// queueing makes a message sent to a session queue (QueueMessages):
	// it waits in the session's Live.Queued until PlaceQueued.
	queueing bool
	// noHello makes the streams send no hello (NoHello).
	noHello bool
	// protocol is the protocol version the server speaks (SpeakProtocol).
	protocol int
	// afterReplayLookup runs right after an approval looked its request id
	// up: a test sees whether the lock is still held there.
	afterReplayLookup func()
	// uploads are the owner's uploads, oldest first, and nextUpload
	// numbers the next (uploads.go); asImage is the base64 an upload a
	// message carries as an image takes of the message's images.
	uploads    []proto.Upload
	nextUpload int64
	asImage    map[int64]int
	// queuedEntries are the entries the queued messages of the fake's
	// sending get as they are placed, by submission: their whole text,
	// images and files.
	queuedEntries map[int64]proto.Entry
	// searchIndexing makes a search say pocketd still indexes
	// (SetSearchIndexing).
	searchIndexing bool
	// changes are the sessions' changes, by session, and diffs their
	// files' diffs, by session and path (changes.go, ADR-0053).
	changes map[int64]proto.Changes
	diffs   map[string]proto.FileDiff
}

// session is a session with its transcript and its runs: run counts the
// runs the session started, and busy says whether the last one works.
type session struct {
	detail proto.SessionDetail
	run    int
}

// reply is what a request id did: its operation ("POST path") and the
// body of its answer.
type reply struct {
	op   string
	body []byte
	// match is the body of a request whose replay compares it (a model
	// change's): the same id with another body is conflict.
	match string
}

// New returns a server.
func New() *Server {
	s := &Server{
		URL:       URL,
		token:     Token,
		protocol:  proto.Version,
		sessions:  map[int64]*session{},
		questions: map[string]proto.Question{},
		nextID:    1,
		seq:       Now.UnixMilli() * 1000,
		streams:   map[chan proto.Event]struct{}{},
		replies:   map[string]reply{},
		failures:  map[string]int{},
		lose:      map[string]chan struct{}{},
		hold:      map[string]chan struct{}{},
		holding:   map[string]int{},
	}
	s.handler = s.routes()
	return s
}

// Handler serves the protocol, for a Hosts of a test's own.
func (s *Server) Handler() http.Handler { return s.handler }

// Transport serves the server's URL in memory.
func (s *Server) Transport() http.RoundTripper {
	return Hosts{strings.TrimPrefix(URL, "http://"): s.handler}
}

// AddModel adds a model GET /v1/models lists.
func (s *Server) AddModel(m proto.Model) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.models = append(s.models, m)
}

// SetProfiles replaces the profiles GET /v1/profiles lists, as an edit of
// the profile folder does.
func (s *Server) SetProfiles(profiles []proto.Profile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.profiles = slices.Clone(profiles)
}

// AddProfile adds a profile GET /v1/profiles lists.
func (s *Server) AddProfile(p proto.Profile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.profiles = append(s.profiles, p)
}

// SetPrompts replaces what GET /v1/prompts answers, as an edit of the
// owner's prompts folder does (ADR-0051).
func (s *Server) SetPrompts(prompts proto.Prompts) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prompts = proto.Prompts{Problem: prompts.Problem, Prompts: slices.Clone(prompts.Prompts)}
}

// SetModels replaces the models GET /v1/models lists, as a provider's
// sign-in does.
func (s *Server) SetModels(models []proto.Model) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.models = slices.Clone(models)
}

// AddSession adds a session with its transcript, without an event. A
// thread's worker names its specialist's session in StartedBy, as
// pocketd's does (protocol rule 23a): the list, the detail and the
// session's events carry it as given.
func (s *Server) AddSession(sess proto.Session, entries ...proto.Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess.State == "" {
		sess.State = proto.SessionIdle
	}
	s.sessions[sess.ID] = &session{detail: proto.SessionDetail{Session: sess, Entries: entries, Live: proto.Live{SessionID: sess.ID}}}
	s.nextID = max(s.nextID, sess.ID+1)
}

// AddApproval adds an approval, or replaces the one with its id, without
// an event.
func (s *Server) AddApproval(a proto.Approval) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := slices.IndexFunc(s.approvals, func(x proto.Approval) bool { return x.ID == a.ID }); i >= 0 {
		s.approvals[i] = a
		return
	}
	s.approvals = append(s.approvals, a)
}

// AddAsk adds an ask, or replaces the one with its id, without an event.
func (s *Server) AddAsk(a proto.Ask) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := slices.IndexFunc(s.asks, func(x proto.Ask) bool { return x.ID == a.ID }); i >= 0 {
		s.asks[i] = a
		return
	}
	s.asks = append(s.asks, a)
}

// AddLesson adds a lesson, or replaces the one with its id, without an
// event.
func (s *Server) AddLesson(l proto.Lesson) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := slices.IndexFunc(s.lessons, func(x proto.Lesson) bool { return x.ID == l.ID }); i >= 0 {
		s.lessons[i] = l
		return
	}
	s.lessons = append(s.lessons, l)
}

// AddThread adds a thread, or replaces the one with its id, without an
// event.
func (s *Server) AddThread(t proto.Thread) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := slices.IndexFunc(s.threads, func(x proto.Thread) bool { return x.ID == t.ID }); i >= 0 {
		s.threads[i] = t
		return
	}
	s.threads = append(s.threads, t)
}

// AddPlan adds a plan, or replaces the one with its id, without an event.
func (s *Server) AddPlan(p proto.Plan) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := slices.IndexFunc(s.plans, func(x proto.Plan) bool { return x.ID == p.ID }); i >= 0 {
		s.plans[i] = p
		return
	}
	s.plans = append(s.plans, p)
}

// AddPull adds a linked pull request, or replaces the one with its id,
// without an event.
func (s *Server) AddPull(p proto.PullRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := slices.IndexFunc(s.pulls, func(x proto.PullRequest) bool { return x.ID == p.ID }); i >= 0 {
		s.pulls[i] = p
		return
	}
	s.pulls = append(s.pulls, p)
}

// SetLive sets a session's live state, without an event.
func (s *Server) SetLive(live proto.Live) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setLiveLocked(live)
}

// setLiveLocked sets a session's live state; a busy one after an idle one
// starts a new run.
func (s *Server) setLiveLocked(live proto.Live) {
	if sess := s.sessions[live.SessionID]; sess != nil {
		if live.Busy && !sess.detail.Live.Busy {
			sess.run++
		}
		sess.detail.Live = live
	}
}

// Reinstate makes the server take the token again, as a device added
// back after Revoke.
func (s *Server) Reinstate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = Token
}

// Revoke makes the server refuse the token, as a revoked device's.
func (s *Server) Revoke() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = ""
}

// FailOnce makes the next request of method on p answer status, with an
// Error body, without doing anything.
func (s *Server) FailOnce(method, p string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[method+" "+p] = status
}

// LoseAnswerOnce makes the next request of method on p do what it does,
// then answer 502 instead of its answer, as when the answer is lost on the
// way back.
func (s *Server) LoseAnswerOnce(method, p string) {
	s.HoldAndLoseAnswerOnce(method, p)()
}

// HoldAnswerOnce makes the next request of method on p do what it does,
// then hold its answer back until the test calls release: what the request
// did, and the events it emitted, reach the client before its answer.
func (s *Server) HoldAnswerOnce(method, p string) (release func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan struct{})
	s.hold[method+" "+p] = ch
	var once sync.Once
	return func() { once.Do(func() { close(ch) }) }
}

// Holding says whether an answer HoldAnswerOnce holds back of method on p
// was made: the request did what it does, and its answer waits.
func (s *Server) Holding(method, p string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.holding[method+" "+p] > 0
}

// HoldAndLoseAnswerOnce is LoseAnswerOnce, with the lost answer held back
// until the test calls release: what the request did, and the events it
// emitted, reach the client first.
func (s *Server) HoldAndLoseAnswerOnce(method, p string) (release func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan struct{})
	s.lose[method+" "+p] = ch
	var once sync.Once
	return func() { once.Do(func() { close(ch) }) }
}

// SpeakProtocol makes the server speak protocol version v alone, as a
// pocketd of another version does: it refuses every other.
func (s *Server) SpeakProtocol(v int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.protocol = v
}

// NoHello makes the streams opened from now on send no hello, to check
// that a client loads without waiting for it.
func (s *Server) NoHello() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noHello = true
}

// QueueMessages makes the messages sent from now on queue, as they do
// while the session runs: each gets its submission at once and shows in
// the session's Live.Queued, and its entry comes only at PlaceQueued.
func (s *Server) QueueMessages(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queueing = on
}

// PlaceQueued writes the entries of session id's queued messages, each
// with its session event, then the live state without them, as the run
// that reaches them does.
func (s *Server) PlaceQueued(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[id]
	if sess == nil {
		return
	}
	for _, q := range sess.detail.Live.Queued {
		entry, ok := s.queuedEntries[q.SubmissionID]
		if !ok {
			entry = proto.Entry{Text: q.Text}
		}
		delete(s.queuedEntries, q.SubmissionID)
		s.writeEntryLocked(proto.Entry{SessionID: id, Kind: proto.EntryUser, Text: entry.Text, At: Now, SubmissionID: q.SubmissionID,
			Images: entry.Images, Files: entry.Files})
		s.reopenLocked(id)
	}
	live := sess.detail.Live
	live.Queued = nil
	s.emitLocked(proto.Event{Type: proto.EventLive, Live: &live})
}

// writeEntryLocked writes entry as pocketd does: the entry's event, then
// its session's (protocol rule 16b).
func (s *Server) writeEntryLocked(entry proto.Entry) {
	sess := s.sessions[entry.SessionID]
	if sess == nil {
		return
	}
	if n := len(sess.detail.Entries); n > 0 {
		entry.ID = sess.detail.Entries[n-1].ID + 1
	} else {
		entry.ID = 1
	}
	s.emitLocked(proto.Event{Type: proto.EventEntry, Entry: &entry})
	summary := sess.detail.Session
	summary.UpdatedAt = Now
	s.emitLocked(proto.Event{Type: proto.EventSession, Session: &summary})
}

// Emit numbers event, applies its payload to the server's state, keeps it
// for Last-Event-ID and sends it to every open stream.
func (s *Server) Emit(event proto.Event) proto.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.emitLocked(event)
}

func (s *Server) emitLocked(event proto.Event) proto.Event {
	s.seq++
	event.Seq = s.seq
	switch {
	case event.Session != nil:
		if sess := s.sessions[event.Session.ID]; sess != nil {
			sess.detail.Session = *event.Session
		} else {
			s.sessions[event.Session.ID] = &session{detail: proto.SessionDetail{Session: *event.Session, Live: proto.Live{SessionID: event.Session.ID}}}
		}
	case event.Entry != nil:
		if sess := s.sessions[event.Entry.SessionID]; sess != nil {
			sess.detail.Entries = append(sess.detail.Entries, *event.Entry)
		}
	case event.Live != nil:
		s.setLiveLocked(*event.Live)
	case event.Approval != nil:
		i := slices.IndexFunc(s.approvals, func(a proto.Approval) bool { return a.ID == event.Approval.ID })
		if i >= 0 {
			s.approvals[i] = *event.Approval
		} else {
			s.approvals = append(s.approvals, *event.Approval)
		}
	case event.Ask != nil:
		i := slices.IndexFunc(s.asks, func(a proto.Ask) bool { return a.ID == event.Ask.ID })
		if i >= 0 {
			s.asks[i] = *event.Ask
		} else {
			s.asks = append(s.asks, *event.Ask)
		}
	case event.Lesson != nil:
		i := slices.IndexFunc(s.lessons, func(l proto.Lesson) bool { return l.ID == event.Lesson.ID })
		if i >= 0 {
			s.lessons[i] = *event.Lesson
		} else {
			s.lessons = append(s.lessons, *event.Lesson)
		}
	case event.Thread != nil:
		i := slices.IndexFunc(s.threads, func(t proto.Thread) bool { return t.ID == event.Thread.ID })
		if i >= 0 {
			s.threads[i] = *event.Thread
		} else {
			s.threads = append(s.threads, *event.Thread)
		}
	case event.Pull != nil:
		i := slices.IndexFunc(s.pulls, func(p proto.PullRequest) bool { return p.ID == event.Pull.ID })
		if i >= 0 {
			s.pulls[i] = *event.Pull
		} else {
			s.pulls = append(s.pulls, *event.Pull)
		}
	case event.Plan != nil:
		i := slices.IndexFunc(s.plans, func(p proto.Plan) bool { return p.ID == event.Plan.ID })
		if i >= 0 {
			s.plans[i] = *event.Plan
		} else {
			s.plans = append(s.plans, *event.Plan)
		}
	case event.Proposal != nil:
		i := slices.IndexFunc(s.proposals, func(p proto.Proposal) bool { return p.ID == event.Proposal.ID })
		if i >= 0 {
			s.proposals[i] = *event.Proposal
		} else {
			s.proposals = append(s.proposals, *event.Proposal)
		}
	case event.Launch != nil:
		i := slices.IndexFunc(s.launches, func(l proto.Launch) bool { return l.ID == event.Launch.ID })
		if i >= 0 {
			s.launches[i] = *event.Launch
		} else {
			s.launches = append(s.launches, *event.Launch)
		}
	}
	s.kept = append(s.kept, event)
	for ch := range s.streams {
		ch <- event
	}
	return event
}

// EmitOnly sends event to the streams without applying it to the server's
// lists: an event newer than an answer the server computed already, as a
// change that lands while a fetch's answer is on its way.
func (s *Server) EmitOnly(event proto.Event) proto.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	event.Seq = s.seq
	s.kept = append(s.kept, event)
	for ch := range s.streams {
		ch <- event
	}
	return event
}

// Seq is the newest event's seq.
func (s *Server) Seq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq
}

// Streams is how many event streams are open.
func (s *Server) Streams() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.streams)
}

// DropStreams ends every open stream, as a network failure would.
func (s *Server) DropStreams() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.streams {
		close(ch)
		delete(s.streams, ch)
	}
}

// Restart ends every stream and forgets the kept events, as pocketd
// restarting does: a client's Last-Event-ID is then answered with resync.
// Seq numbers keep growing (protocol rule 17).
func (s *Server) Restart() {
	s.DropStreams()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kept = nil
	s.seq += 1000
}

// Requests are the requests the server got, in order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// RequestsTo are the requests of method on p.
func (s *Server) RequestsTo(method, p string) []Request {
	var out []Request
	for _, r := range s.Requests() {
		if r.Method == method && r.Path == p {
			out = append(out, r)
		}
	}
	return out
}

// Session returns the server's copy of session id.
func (s *Server) Session(id int64) (proto.SessionDetail, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[id]
	if sess == nil {
		return proto.SessionDetail{}, false
	}
	return sess.detail, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code proto.ErrorCode, message string) {
	writeJSON(w, status, proto.Error{Code: code, Message: message})
}

// capture records an answer instead of sending it, so a lost answer can be
// replaced after the request did its work.
type capture struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (c *capture) Header() http.Header { return c.header }
func (c *capture) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}
func (c *capture) Write(b []byte) (int, error) {
	c.WriteHeader(http.StatusOK)
	return c.body.Write(b)
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+proto.PathHello, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, proto.Hello{Protocol: proto.Version, Server: "test", Device: "laptop", Now: Now})
	})
	mux.HandleFunc("GET "+proto.PathModels, func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		models := slices.Clone(s.models)
		s.mu.Unlock()
		if models == nil {
			models = []proto.Model{}
		}
		writeJSON(w, http.StatusOK, models)
	})
	mux.HandleFunc("GET "+proto.PathProfiles, func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		profiles := slices.Clone(s.profiles)
		s.mu.Unlock()
		if profiles == nil {
			profiles = []proto.Profile{}
		}
		writeJSON(w, http.StatusOK, profiles)
	})
	mux.HandleFunc("GET "+proto.PathPrompts, func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		list := proto.Prompts{Problem: s.prompts.Problem, Prompts: slices.Clone(s.prompts.Prompts)}
		s.mu.Unlock()
		if list.Prompts == nil {
			list.Prompts = []proto.Prompt{}
		}
		writeJSON(w, http.StatusOK, list)
	})
	mux.HandleFunc("GET "+proto.PathSessions, func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		list := make([]proto.Session, 0, len(s.sessions))
		for _, sess := range s.sessions {
			list = append(list, sess.detail.Session)
		}
		s.mu.Unlock()
		sort.Slice(list, func(i, j int) bool {
			if !list[i].UpdatedAt.Equal(list[j].UpdatedAt) {
				return list[i].UpdatedAt.After(list[j].UpdatedAt)
			}
			return list[i].ID > list[j].ID
		})
		writeJSON(w, http.StatusOK, list)
	})
	mux.HandleFunc("POST "+proto.PathSessions, s.createSession)
	mux.HandleFunc("GET "+proto.PathSession, func(w http.ResponseWriter, r *http.Request) {
		d, ok := s.Session(pathID(r))
		if !ok {
			writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such session")
			return
		}
		if d.Entries == nil {
			d.Entries = []proto.Entry{}
		}
		writeJSON(w, http.StatusOK, d)
	})
	mux.HandleFunc("POST "+proto.PathMessages, s.send)
	mux.HandleFunc("POST "+proto.PathAbort, s.abort)
	mux.HandleFunc("POST "+proto.PathStartOver, s.startOver)
	mux.HandleFunc("POST "+proto.PathModel, s.changeModel)
	mux.HandleFunc("POST "+proto.PathTitle, s.renameSession)
	mux.HandleFunc("POST "+proto.PathQuestion, s.answerQuestion)
	mux.HandleFunc("GET "+proto.PathApprovals, func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		list := []proto.Approval{}
		for _, a := range s.approvals {
			if a.State == proto.ApprovalPending {
				list = append(list, a)
			}
		}
		s.mu.Unlock()
		sort.SliceStable(list, func(i, j int) bool { return list[i].RequestedAt.Before(list[j].RequestedAt) })
		writeJSON(w, http.StatusOK, list)
	})
	mux.HandleFunc("POST "+proto.PathApproval, s.decide)
	mux.HandleFunc("GET "+proto.PathAsks, func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		list := []proto.Ask{}
		for _, a := range s.asks {
			if a.State == proto.AskOpen {
				list = append(list, a)
			}
		}
		s.mu.Unlock()
		sort.SliceStable(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
		writeJSON(w, http.StatusOK, list)
	})
	mux.HandleFunc("POST "+proto.PathAsk, s.answer)
	mux.HandleFunc("GET "+proto.PathLessons, func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		list := []proto.Lesson{}
		for _, l := range s.lessons {
			if l.State == proto.LessonPending || l.State == proto.LessonActive {
				list = append(list, l)
			}
		}
		s.mu.Unlock()
		sort.SliceStable(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
		writeJSON(w, http.StatusOK, list)
	})
	mux.HandleFunc("POST "+proto.PathLesson, s.decideLesson)
	mux.HandleFunc("GET "+proto.PathThreads, func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		list := slices.Clone(s.threads)
		s.mu.Unlock()
		if list == nil {
			list = []proto.Thread{}
		}
		sort.SliceStable(list, func(i, j int) bool {
			if !list[i].UpdatedAt.Equal(list[j].UpdatedAt) {
				return list[i].UpdatedAt.After(list[j].UpdatedAt)
			}
			return list[i].ID > list[j].ID
		})
		writeJSON(w, http.StatusOK, list)
	})
	mux.HandleFunc("POST "+proto.PathCloseThread, s.closeThread)
	mux.HandleFunc("POST "+proto.PathStopShift, s.changeShift)
	mux.HandleFunc("POST "+proto.PathResumeShift, s.changeShift)
	mux.HandleFunc("GET "+proto.PathPulls, func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		closed := map[int64]bool{}
		for _, t := range s.threads {
			closed[t.ID] = t.State == proto.ThreadClosed
		}
		list := []proto.PullRequest{}
		for _, p := range s.pulls {
			if p.State == proto.PullOpen || !closed[p.Thread] {
				list = append(list, p)
			}
		}
		s.mu.Unlock()
		sort.SliceStable(list, func(i, j int) bool { return list[i].ID < list[j].ID })
		writeJSON(w, http.StatusOK, list)
	})
	// pocketd reads the pull request on GitHub before it answers; the
	// fake's GitHub is its list (rule 45).
	mux.HandleFunc("GET "+proto.PathPull, func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		s.mu.Lock()
		closed := map[int64]bool{}
		for _, t := range s.threads {
			closed[t.ID] = t.State == proto.ThreadClosed
		}
		var found *proto.PullRequest
		for _, p := range s.pulls {
			if p.ID == id && (p.State == proto.PullOpen || !closed[p.Thread]) {
				found = &p
			}
		}
		s.mu.Unlock()
		if found == nil {
			writeJSON(w, http.StatusNotFound, proto.Error{Code: proto.ErrorCodeNotFound, Message: "no pull request"})
			return
		}
		writeJSON(w, http.StatusOK, *found)
	})
	mux.HandleFunc("GET "+proto.PathPlans, func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		list := slices.Clone(s.plans)
		s.mu.Unlock()
		if list == nil {
			list = []proto.Plan{}
		}
		sort.SliceStable(list, func(i, j int) bool {
			if !list[i].UpdatedAt.Equal(list[j].UpdatedAt) {
				return list[i].UpdatedAt.After(list[j].UpdatedAt)
			}
			return list[i].ID > list[j].ID
		})
		writeJSON(w, http.StatusOK, list)
	})
	mux.HandleFunc("POST "+proto.PathApprovePlan, s.approvePlan)
	mux.HandleFunc("GET "+proto.PathSearch, s.search)
	mux.HandleFunc("GET "+proto.PathEvents, s.events)
	s.terminalRoutes(mux)
	s.nightShiftRoutes(mux)
	s.uploadRoutes(mux)
	s.usageRoutes(mux)
	s.changesRoutes(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// An upload's body is the file, up to proto.MaxUpload, as
		// pocketd takes it; every other is JSON (Codex review of #49,
		// round 2).
		limit := int64(1 << 20)
		upload := r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/uploads")
		if upload {
			limit = proto.MaxUpload
		}
		body, readErr := io.ReadAll(io.LimitReader(r.Body, limit+1))
		key := r.Method + " " + r.URL.Path
		s.mu.Lock()
		s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, LastEventID: r.Header.Get("Last-Event-ID"), Body: body})
		token := s.token
		failure := s.failures[key]
		delete(s.failures, key)
		lose, losing := s.lose[key]
		delete(s.lose, key)
		hold, holding := s.hold[key]
		delete(s.hold, key)
		protocol := s.protocol
		s.mu.Unlock()
		if r.Header.Get(proto.HeaderProtocol) != strconv.Itoa(protocol) {
			writeError(w, http.StatusBadRequest, proto.ErrorCodeProtocol, fmt.Sprintf("send Pocket-Protocol: %d", protocol))
			return
		}
		if token == "" || r.Header.Get("Authorization") != "Bearer "+token {
			writeError(w, http.StatusUnauthorized, proto.ErrorCodeUnauthorized, "unknown token")
			return
		}
		if failure != 0 {
			writeError(w, failure, proto.ErrorCodeInternal, "made to fail")
			return
		}
		if int64(len(body)) > limit {
			status := http.StatusBadRequest
			if upload {
				status = http.StatusRequestEntityTooLarge
			}
			writeError(w, status, proto.ErrorCodeBadRequest, "the body is too large")
			return
		}
		// A body cut off reads as cut off: its handler answers as pocketd
		// does, never with what it got of it (Codex review of #49, round 7).
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), errorReader{readErr}))
		if losing {
			mux.ServeHTTP(&capture{header: http.Header{}}, r)
			select {
			case <-lose:
			case <-r.Context().Done():
				return
			}
			writeError(w, http.StatusBadGateway, proto.ErrorCodeInternal, "the answer was lost")
			return
		}
		if holding {
			answer := &capture{header: http.Header{}}
			mux.ServeHTTP(answer, r)
			s.mu.Lock()
			s.holding[key]++
			s.mu.Unlock()
			defer func() {
				s.mu.Lock()
				s.holding[key]--
				s.mu.Unlock()
			}()
			select {
			case <-hold:
			case <-r.Context().Done():
				return
			}
			for k, v := range answer.header {
				w.Header()[k] = v
			}
			w.WriteHeader(answer.status)
			_, _ = w.Write(answer.body.Bytes())
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

// replay answers a request id seen before (protocol rule 5): with its
// first answer for the same operation, and conflict for another. It
// reports whether it answered.
func (s *Server) replay(w http.ResponseWriter, r *http.Request, id string) bool {
	return s.replayOp(w, r.Method+" "+r.URL.Path, id)
}

// replayOp is replay for a request whose op names more than its method and
// path, as a drop's rev.
func (s *Server) replayOp(w http.ResponseWriter, op, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.replayOpLocked(w, op, id)
}

// replayOpLocked is replayOp with s.mu held.
func (s *Server) replayOpLocked(w http.ResponseWriter, op, id string) bool {
	prior, ok := s.replies[id]
	switch {
	case !ok:
		return false
	case prior.op != op:
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, "the requestId was used for a different request")
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(prior.body)
	}
	return true
}

func (s *Server) rememberLocked(r *http.Request, id string, v any) {
	s.rememberOpLocked(r.Method+" "+r.URL.Path, id, v)
}

func (s *Server) rememberOpLocked(op, id string, v any) {
	body, _ := json.Marshal(v)
	s.replies[id] = reply{op: op, body: append(body, '\n')}
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var request proto.CreateSession
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.RequestID == "" {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "bad body")
		return
	}
	if s.replay(w, r, request.RequestID) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// A profile gives the folder and the model when the request names
	// none, as pocketd's router does (ADR-0013).
	route := "owner"
	standing, renamable := false, false
	if request.Profile != "" {
		// A standing profile's request names the profile alone, and opens
		// its standing session once it has one, whatever the profile's
		// problem now (rule 7c): only a new one needs the profile to start.
		if k := slices.IndexFunc(s.profiles, func(p proto.Profile) bool { return p.ID == request.Profile && p.Standing }); k >= 0 {
			if request.Cwd != "" || request.Model != (proto.ModelRef{}) || request.Thinking != "" || request.Title != "" || request.Repository != "" || request.Worktree {
				writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "profile "+request.Profile+" keeps a standing session: name the profile alone")
				return
			}
			for _, sess := range s.sessions {
				if sess.detail.Session.Standing && sess.detail.Session.Profile == request.Profile {
					s.rememberLocked(r, request.RequestID, sess.detail.Session)
					writeJSON(w, http.StatusOK, sess.detail.Session)
					return
				}
			}
		}
		j := slices.IndexFunc(s.profiles, func(p proto.Profile) bool { return p.ID == request.Profile && p.Problem == "" })
		if j < 0 {
			writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "no profile "+request.Profile)
			return
		}
		if standing = s.profiles[j].Standing; standing {
			request.Title = s.profiles[j].Name
		}
		// A private agent's session may be renamed, as pocketd's
		// team.CheckRename says of one the owner starts (ADR-0058; Codex
		// review of #118, round 1).
		renamable = s.profiles[j].Private && s.profiles[j].Role != proto.RoleSupervisor
		if request.Cwd == "" && request.Repository == "" {
			if request.Cwd = s.profiles[j].Cwd; request.Cwd == "" {
				request.Cwd = s.profiles[j].Home
			}
		}
		if request.Model == (proto.ModelRef{}) {
			request.Model, route = s.profiles[j].Runs, "profile"
		}
	}
	// A repository's checkout, as its profile, or any profile, names it;
	// a worktree beside it, named from the request (rule 6a).
	if request.Worktree && request.Repository == "" {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "worktree needs a repository")
		return
	}
	if request.Repository != "" {
		// As team.SessionCheckout: the folder must be one a profile names,
		// and the request names which when the profiles name two (Codex
		// review of #46, round 1).
		var folders []string
		for _, p := range s.profiles {
			if request.Profile != "" && p.ID != request.Profile {
				continue
			}
			for _, r := range p.Repositories {
				if strings.EqualFold(r.Name, request.Repository) && (request.Cwd == "" || path.Clean(request.Cwd) == r.Folder) &&
					!slices.Contains(folders, r.Folder) {
					folders = append(folders, r.Folder)
				}
			}
		}
		if len(folders) != 1 {
			writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, fmt.Sprintf("profiles name %s in %d folders there", request.Repository, len(folders)))
			return
		}
		checkout := folders[0]
		request.Cwd = checkout
		if request.Worktree {
			agent := request.Profile
			if agent == "" {
				agent = proto.OwnerAgent
			}
			request.Cwd, _ = proto.SessionWorktree(checkout, agent, strings.TrimSpace(request.Title), request.RequestID)
		}
	}
	i := slices.IndexFunc(s.models, func(m proto.Model) bool { return m.ModelRef == request.Model })
	if !path.IsAbs(request.Cwd) || i < 0 {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "cwd must be absolute and the model one of /v1/models")
		return
	}
	if request.Thinking != "" && !slices.Contains(s.models[i].ThinkingLevels, request.Thinking) {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "thinking must be one of the model's levels")
		return
	}
	id := s.nextID
	s.nextID++
	title := request.Title
	if title == "" {
		title = fmt.Sprintf("Session %d", id)
	}
	sess := proto.Session{ID: id, Title: title, Cwd: path.Clean(request.Cwd), Model: request.Model, Thinking: request.Thinking,
		Profile: request.Profile, Route: route, Standing: standing, Renamable: renamable, State: proto.SessionIdle, CreatedAt: Now, UpdatedAt: Now}
	s.emitLocked(proto.Event{Type: proto.EventSession, Session: &sess})
	s.rememberLocked(r, request.RequestID, sess)
	writeJSON(w, http.StatusCreated, sess)
}

func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	var request proto.SendMessage
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.RequestID == "" || (strings.TrimSpace(request.Text) == "" && len(request.Attachments) == 0) {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "bad body")
		return
	}
	if s.replay(w, r, request.RequestID) {
		return
	}
	id := pathID(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[id]
	if sess == nil {
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such session")
		return
	}
	// A message names its attached files after its text, as pocketd
	// writes them (rule 10a).
	files, images, ok := s.attachedLocked(id, request.Attachments)
	if !ok {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "an attachment is no upload of the session")
		return
	}
	// A closed plan's thread takes no message, as pocketd refuses one
	// that could not reopen it (protocol rule 10, ADR-0061).
	if t := s.threadOfSessionLocked(id); t != nil && t.State == proto.ThreadClosed && t.Plan != 0 {
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, fmt.Sprintf("thread T%d is plan P%d's and stays closed", t.ID, t.Plan))
		return
	}
	text := proto.WithAttachments(request.Text, files)
	s.submitted++
	answer := proto.SentMessage{SubmissionID: s.submitted}
	if s.queueing {
		live := sess.detail.Live
		// The queue shows its first proto.MaxQueuedText bytes, cut at a
		// character, as pocketd's does; its entry, once placed, has the
		// whole text and counts its images and files (Codex review of #49,
		// rounds 10 and 12).
		preview, cut := queuedPreview(text)
		live.Queued = append(slices.Clone(live.Queued), proto.QueuedMessage{SubmissionID: s.submitted, Text: preview, Cut: cut, Files: len(files)})
		if s.queuedEntries == nil {
			s.queuedEntries = map[int64]proto.Entry{}
		}
		s.queuedEntries[s.submitted] = proto.Entry{Text: text, Images: images, Files: len(files)}
		s.emitLocked(proto.Event{Type: proto.EventLive, Live: &live})
	} else {
		s.writeEntryLocked(proto.Entry{SessionID: id, Kind: proto.EntryUser, Text: text, At: Now, SubmissionID: s.submitted, Images: images, Files: len(files)})
		s.reopenLocked(id)
	}
	s.rememberLocked(r, request.RequestID, answer)
	writeJSON(w, http.StatusOK, answer)
}

// threadOfSessionLocked is the thread whose specialist works in session,
// nil for none.
func (s *Server) threadOfSessionLocked(session int64) *proto.Thread {
	if i := slices.IndexFunc(s.threads, func(t proto.Thread) bool { return t.Session == session && session != 0 }); i >= 0 {
		return &s.threads[i]
	}
	return nil
}

// reopenLocked takes the closed thread whose specialist works in session
// back to open, as a message in its transcript does (rule 24, ADR-0061):
// its reopens go up, with its event. A plan's thread stays closed.
func (s *Server) reopenLocked(session int64) {
	t := s.threadOfSessionLocked(session)
	if t == nil || t.State != proto.ThreadClosed || t.Plan != 0 {
		return
	}
	at := Now
	t.State, t.Reopens, t.ReopenedAt, t.UpdatedAt = proto.ThreadOpen, t.Reopens+1, &at, Now
	reopened := *t
	s.emitLocked(proto.Event{Type: proto.EventThread, Thread: &reopened})
}

// abort follows protocol rule 5 for Abort: the first call records the run
// it targets, and stops it if it still works; a retry finishes that abort
// and never stops a run started since.
func (s *Server) abort(w http.ResponseWriter, r *http.Request) {
	var request proto.Abort
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.RequestID == "" {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "bad body")
		return
	}
	id := pathID(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[id]
	if sess == nil {
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such session")
		return
	}
	op := r.Method + " " + r.URL.Path
	prior, seen := s.replies[request.RequestID]
	if seen && prior.op != op {
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, "the requestId was used for a different request")
		return
	}
	target := sess.run
	if seen {
		target, _ = strconv.Atoi(string(prior.body))
	} else {
		s.replies[request.RequestID] = reply{op: op, body: []byte(strconv.Itoa(target))}
	}
	if sess.run == target && sess.detail.Live.Busy {
		s.emitLocked(proto.Event{Type: proto.EventLive, Live: &proto.Live{SessionID: id}})
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

// startOver writes a reset in an idle standing session, once a request id
// (rule 7d).
func (s *Server) startOver(w http.ResponseWriter, r *http.Request) {
	var request proto.StartOver
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.RequestID == "" {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "bad body")
		return
	}
	if s.replay(w, r, request.RequestID) {
		return
	}
	id := pathID(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[id]
	switch {
	case sess == nil:
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such session")
		return
	case !sess.detail.Session.Standing:
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "only a standing session starts over")
		return
	case sess.detail.Live.Busy || len(sess.detail.Live.Queued) > 0 || sess.detail.Live.Pending:
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, "the session has a run: start over once it is idle")
		return
	}
	s.writeEntryLocked(proto.Entry{SessionID: id, Kind: proto.EntryReset, At: Now})
	summary := s.sessions[id].detail.Session
	s.rememberLocked(r, request.RequestID, summary)
	writeJSON(w, http.StatusOK, summary)
}

// changeModel changes a session's model or thinking level as pocketd does
// (rule 7e): a model GET /v1/models lists, a level it takes; without one,
// the session's level when the model takes it, else the default. The
// session's event goes out before the answer. A request id seen before
// answers the session as it is now, and changes nothing again, for the
// same session and body; another body or session with it is conflict
// (Codex review of #48, round 1).
func (s *Server) changeModel(w http.ResponseWriter, r *http.Request) {
	var request proto.ChangeModel
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.RequestID == "" || request.Model == nil && request.Thinking == "" {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "bad body")
		return
	}
	match := strconv.Quote(request.Thinking)
	if request.Model != nil {
		match = strconv.Quote(request.Model.Provider) + " " + strconv.Quote(request.Model.ID) + " " + match
	}
	id := pathID(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[id]
	if prior, ok := s.replies[request.RequestID]; ok {
		switch {
		case prior.op != r.Method+" "+r.URL.Path || prior.match != match:
			writeError(w, http.StatusConflict, proto.ErrorCodeConflict, "the requestId was used for a different request")
		case sess == nil:
			writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such session")
		default:
			writeJSON(w, http.StatusOK, sess.detail.Session)
		}
		return
	}
	if sess == nil {
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such session")
		return
	}
	summary := sess.detail.Session
	target := summary.Model
	if request.Model != nil {
		target = *request.Model
	}
	i := slices.IndexFunc(s.models, func(m proto.Model) bool { return m.ModelRef == target })
	if i < 0 {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "model "+target.String()+" is not available")
		return
	}
	levels, thinking := s.models[i].ThinkingLevels, request.Thinking
	switch {
	case thinking != "" && !slices.Contains(levels, thinking):
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "the model takes no thinking level "+thinking)
		return
	case thinking == "" && slices.Contains(levels, summary.Thinking):
		thinking = summary.Thinking
	}
	if target != summary.Model || summary.Standing {
		summary.Route = "owner"
	}
	summary.Model, summary.Thinking = target, thinking
	s.emitLocked(proto.Event{Type: proto.EventSession, Session: &summary})
	s.replies[request.RequestID] = reply{op: r.Method + " " + r.URL.Path, match: match}
	writeJSON(w, http.StatusOK, summary)
}

// renameSession renames a session as pocketd does (rule 68, ADR-0058):
// only one whose summary is renamable, a title of at most proto.MaxTitle
// bytes as sent, trimmed, the default for an empty one; its updatedAt
// stays. The session's event goes out before the answer. A request id
// seen before answers the session as it is now, and renames nothing
// again, for the same session and title; another title or session with it
// is conflict, and publishes the session again, as pocketd does. A body
// with no title, or a null one, is refused.
func (s *Server) renameSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RequestID string  `json:"requestId"`
		Title     *string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.RequestID == "" || body.Title == nil {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "bad body: title is required")
		return
	}
	request := proto.RenameSession{RequestID: body.RequestID, Title: *body.Title}
	if len(request.Title) > proto.MaxTitle {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "title must be at most 200 bytes")
		return
	}
	id := pathID(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[id]
	if prior, ok := s.replies[request.RequestID]; ok {
		switch {
		case prior.op != r.Method+" "+r.URL.Path || prior.match != request.Title:
			writeError(w, http.StatusConflict, proto.ErrorCodeConflict, "the requestId was used for a different request")
		case sess == nil:
			writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such session")
		default:
			// The session as it is now goes out again, for a client that
			// missed the first event (Codex review of #118, round 1).
			current := sess.detail.Session
			s.emitLocked(proto.Event{Type: proto.EventSession, Session: &current})
			writeJSON(w, http.StatusOK, current)
		}
		return
	}
	switch {
	case sess == nil:
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such session")
		return
	case !sess.detail.Session.Renamable:
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, fmt.Sprintf("session %d is not a private agent's: only a private agent's session may be renamed", id))
		return
	}
	summary := sess.detail.Session
	summary.Title = strings.TrimSpace(request.Title)
	if summary.Title == "" {
		summary.Title = proto.DefaultTitle(id)
	}
	s.emitLocked(proto.Event{Type: proto.EventSession, Session: &summary})
	s.replies[request.RequestID] = reply{op: r.Method + " " + r.URL.Path, match: request.Title}
	writeJSON(w, http.StatusOK, summary)
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request) {
	var raw map[string]json.RawMessage
	body, _ := io.ReadAll(r.Body)
	var request proto.Decide
	if json.Unmarshal(body, &raw) != nil || json.Unmarshal(body, &request) != nil || request.RequestID == "" ||
		(string(raw["allow"]) != "true" && string(raw["allow"]) != "false") {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "bad body: allow is required")
		return
	}
	if s.replay(w, r, request.RequestID) {
		return
	}
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.approvals, func(a proto.Approval) bool { return a.ID == id })
	if i < 0 {
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such approval")
		return
	}
	a := s.approvals[i]
	if a.State != proto.ApprovalPending {
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, "already decided")
		return
	}
	a.State, a.Reason, a.DecidedBy = proto.ApprovalDenied, request.Reason, "laptop"
	if request.Allow {
		a.State = proto.ApprovalAllowed
	}
	decided := Now
	a.DecidedAt = &decided
	s.emitLocked(proto.Event{Type: proto.EventApproval, Approval: &a})
	s.rememberLocked(r, request.RequestID, a)
	writeJSON(w, http.StatusOK, a)
}

// answer answers an ask once, as the service does.
func (s *Server) answer(w http.ResponseWriter, r *http.Request) {
	var request proto.AnswerAsk
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.RequestID == "" || strings.TrimSpace(request.Answer) == "" {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "bad body: requestId and answer are required")
		return
	}
	if s.replay(w, r, request.RequestID) {
		return
	}
	id := pathID(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.asks, func(a proto.Ask) bool { return a.ID == id })
	if i < 0 {
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such ask")
		return
	}
	a := s.asks[i]
	if a.State == proto.AskWithdrawn {
		// As pocketd refuses an answer to an ask a supervisor's message
		// withdrew (ADR-0037).
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, fmt.Sprintf("A%d was withdrawn: the supervisor's message reached T%d's specialist "+
			"and unblocked the thread before your answer came, so pocketd did not send your answer", a.ID, a.Thread))
		return
	}
	if a.State != proto.AskOpen {
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, fmt.Sprintf("ask %d is already answered", a.ID))
		return
	}
	answered := Now
	a.State, a.Answer, a.AnsweredBy, a.AnsweredAt = proto.AskAnswered, strings.TrimSpace(request.Answer), "laptop", &answered
	s.emitLocked(proto.Event{Type: proto.EventAsk, Ask: &a})
	s.rememberLocked(r, request.RequestID, a)
	writeJSON(w, http.StatusOK, a)
}

// closeThread closes a thread as the owner does: closing a closed one
// changes nothing (protocol rule 28). A close's updatedAt is the clock's,
// as pocketd's store.CloseThreadAs writes it, which may be the thread's
// own or before it (Codex review of #117).
func (s *Server) closeThread(w http.ResponseWriter, r *http.Request) {
	var request proto.CloseThread
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.RequestID == "" {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "bad body: requestId is required")
		return
	}
	id := pathID(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.threads, func(t proto.Thread) bool { return t.ID == id })
	// The same request again closes nothing again, and answers with the
	// thread as it is now, as pocketd's does: a message may have reopened
	// it (ADR-0061).
	if prior, retried := s.replies[request.RequestID]; retried {
		switch {
		case prior.op != r.Method+" "+r.URL.Path:
			writeError(w, http.StatusConflict, proto.ErrorCodeConflict, "the requestId was used for a different request")
		case i < 0:
			writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such thread")
		default:
			writeJSON(w, http.StatusOK, s.threads[i])
		}
		return
	}
	if i < 0 {
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such thread")
		return
	}
	t := s.threads[i]
	if t.State != proto.ThreadClosed {
		t.State, t.UpdatedAt = proto.ThreadClosed, Now
		s.threads[i] = t
		s.emitLocked(proto.Event{Type: proto.EventThread, Thread: &t})
	}
	s.rememberLocked(r, request.RequestID, t)
	writeJSON(w, http.StatusOK, t)
}

// changeShift stops or resumes a thread's shift as the owner does
// (protocol rule 55): a stop ends a shift not ended; a resume goes on with
// a paused one, or one ended for a reason other than done or closed, with
// rounds and time left, at its next round, else 409. A stop files the
// blocked report pocketd files. A thread still starting is 409. The same requestId again changes nothing and answers
// with the thread as it is now, as pocketd's does, never the first answer
// kept (Codex review of #37); it is looked up and claimed under one hold
// of the lock, as pocketd claims it in the change's transaction. Each
// change moves the thread's updatedAt on, never back.
func (s *Server) changeShift(w http.ResponseWriter, r *http.Request) {
	var request proto.StopShift
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.RequestID == "" {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "bad body: requestId is required")
		return
	}
	id := pathID(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.threads, func(t proto.Thread) bool { return t.ID == id })
	if prior, retried := s.replies[request.RequestID]; retried {
		switch {
		case prior.op != r.Method+" "+r.URL.Path:
			writeError(w, http.StatusConflict, proto.ErrorCodeConflict, "the requestId was used for a different request")
		case i < 0:
			writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such thread")
		default:
			writeJSON(w, http.StatusOK, s.threads[i])
		}
		return
	}
	if i < 0 {
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such thread")
		return
	}
	t := s.threads[i]
	if t.Shift == nil {
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, fmt.Sprintf("thread T%d runs no shift", id))
		return
	}
	if t.State == proto.ThreadStarting || t.Session == 0 {
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, fmt.Sprintf("thread T%d's specialist does not have its brief yet: change its shift once it has", id))
		return
	}
	sh := *t.Shift
	if strings.HasSuffix(r.URL.Path, "/stop") {
		if sh.State == proto.ShiftEnded {
			// An ended shift's stop changes nothing, and spends its
			// requestId all the same.
			s.rememberLocked(r, request.RequestID, t)
			writeJSON(w, http.StatusOK, t)
			return
		}
		ended := Now
		sh.State, sh.Reason, sh.Why, sh.EndedAt = proto.ShiftEnded, "stopped", "the owner stopped it", &ended
		// pocketd files its report with the stop, blocked, and a resume
		// leaves the thread so until the specialist reports again.
		t.State = proto.ThreadBlocked
		t.Report = fmt.Sprintf("The shift ended: %s. It took %d of %d rounds.", sh.Why, sh.Round, sh.MaxRounds)
	} else {
		resumable := sh.State == proto.ShiftPaused || sh.State == proto.ShiftEnded && sh.Reason != "done" && sh.Reason != "closed"
		late := sh.Deadline != nil && !Now.Before(*sh.Deadline)
		// A closed thread takes no round, whatever its shift ended for, as
		// pocketd's queueShiftMessage refuses it (Codex review of #37,
		// round 2).
		if !resumable || sh.Round >= sh.MaxRounds || late || t.State == proto.ThreadClosed {
			writeError(w, http.StatusConflict, proto.ErrorCodeConflict, "the shift cannot go on: it is running, ended done or with its thread, or has no rounds or time left")
			return
		}
		sh.State, sh.Reason, sh.Why, sh.EndedAt, sh.Round = proto.ShiftRunning, "", "", nil, sh.Round+1
	}
	t.Shift, t.UpdatedAt = &sh, movedOn(t.UpdatedAt)
	s.threads[i] = t
	s.emitLocked(proto.Event{Type: proto.EventThread, Thread: &t})
	s.rememberLocked(r, request.RequestID, t)
	writeJSON(w, http.StatusOK, t)
}

// movedOn is a thread's updatedAt after a change at Now: Now, or a
// millisecond on when it already is Now or later, as pocketd's changes
// move it, never back.
func movedOn(at time.Time) time.Time {
	if Now.After(at) {
		return Now
	}
	return at.Add(time.Millisecond)
}

// approvePlan approves a plan as the owner does (protocol: ApprovePlan):
// only one in review, at the revision and graph version the owner saw.
// The plan's tasks with no dependency are ready, and its event goes out.
func (s *Server) approvePlan(w http.ResponseWriter, r *http.Request) {
	var request proto.ApprovePlan
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.RequestID == "" {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "bad body: requestId is required")
		return
	}
	id := pathID(r)
	// A retry answers with the plan as it is now, as pocketd's does
	// (protocol rule 50), never the first answer kept (Codex review of
	// #25, round 7). The request id is looked up and claimed under one
	// hold of the lock, as pocketd claims it in the approval's transaction
	// (round 8).
	s.mu.Lock()
	defer s.mu.Unlock()
	prior, retried := s.replies[request.RequestID]
	if s.afterReplayLookup != nil {
		s.afterReplayLookup()
	}
	if retried {
		if prior.op != r.Method+" "+r.URL.Path {
			writeError(w, http.StatusConflict, proto.ErrorCodeConflict, "the requestId was used for a different request")
			return
		}
		if i := slices.IndexFunc(s.plans, func(p proto.Plan) bool { return p.ID == id }); i >= 0 {
			writeJSON(w, http.StatusOK, s.plans[i])
			return
		}
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such plan")
		return
	}
	i := slices.IndexFunc(s.plans, func(p proto.Plan) bool { return p.ID == id })
	if i < 0 {
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such plan")
		return
	}
	p := s.plans[i]
	switch {
	case p.State != proto.PlanReview || p.Graph == nil:
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, "plan P"+strconv.FormatInt(id, 10)+" is "+string(p.State)+", and does not wait for an approval")
		return
	case request.Rev != p.Rev || request.Graph != p.Graph.Version:
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, "plan P"+strconv.FormatInt(id, 10)+" changed since you saw it")
		return
	}
	graph := *p.Graph
	graph.Approved = true
	graph.Tasks = slices.Clone(graph.Tasks)
	for j, t := range graph.Tasks {
		if len(t.Deps) == 0 {
			graph.Tasks[j].State = proto.TaskReady
		}
	}
	at := Now
	p.State, p.Graph, p.ApprovedBy, p.ApprovedAt, p.UpdatedAt = proto.PlanApproved, &graph, "laptop", &at, Now
	s.plans[i] = p
	s.emitLocked(proto.Event{Type: proto.EventPlan, Plan: &p})
	s.rememberLocked(r, request.RequestID, p)
	writeJSON(w, http.StatusOK, p)
}

// SetLessonRoom makes approving a lesson refuse once its agent keeps n in
// force, as the service does at its limit; 0 is no limit.
func (s *Server) SetLessonRoom(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lessonRoom = n
}

// lessonsFull says whether agent keeps the most lessons in force.
func (s *Server) lessonsFull(agent string) bool {
	if s.lessonRoom == 0 {
		return false
	}
	n := 0
	for _, l := range s.lessons {
		if l.Agent == agent && l.State == proto.LessonActive {
			n++
		}
	}
	return n >= s.lessonRoom
}

// decideLesson runs the owner's decision on a lesson, as the service does:
// an approval holds only for what waits as the request names it.
func (s *Server) decideLesson(w http.ResponseWriter, r *http.Request) {
	var request proto.DecideLesson
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.RequestID == "" {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "bad body: requestId and decision are required")
		return
	}
	// The shape of a decision, checked as the service checks it, before
	// its request id or the lesson.
	switch {
	case request.Decision == proto.LessonApprove && (request.Text == "") == !request.Forget:
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "an approval names what it approves: the text shown, or forget")
		return
	case request.Decision == proto.LessonApprove:
	case request.Decision == proto.LessonDiscard, request.Decision == proto.LessonForget:
		if request.Text != "" || request.Forget {
			writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "only an approval names text or forget")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "decision must be approve, discard or forget")
		return
	}
	id := pathID(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.lessons, func(l proto.Lesson) bool { return l.ID == id })
	// A decision made once answers again with the lesson as it is now, as
	// the service's store.DecideLesson does, and decides nothing.
	if prior, ok := s.replies[request.RequestID]; ok {
		switch {
		case prior.op != r.Method+" "+r.URL.Path:
			writeError(w, http.StatusConflict, proto.ErrorCodeConflict, "the requestId was used for a different request")
		case i < 0:
			writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such lesson")
		default:
			writeJSON(w, http.StatusOK, s.lessons[i])
		}
		return
	}
	if i < 0 {
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such lesson")
		return
	}
	l := s.lessons[i]
	waiting, forget := l.Text, false
	if l.State == proto.LessonActive && l.Change != nil {
		waiting, forget = l.Change.Text, l.Change.Forget
	}
	switch {
	case !l.Waits() && request.Decision != proto.LessonForget:
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, "nothing waits")
		return
	case request.Decision == proto.LessonApprove && (request.Forget != forget || (!forget && request.Text != waiting)):
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, fmt.Sprintf("lesson L%d changed since you looked: check it again", id))
		return
	case request.Decision == proto.LessonApprove && l.State == proto.LessonPending && s.lessonsFull(l.Agent):
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, "this agent's lessons are full: forget one first, or fold two into one")
		return
	case request.Decision == proto.LessonApprove && l.State == proto.LessonPending:
		l.State = proto.LessonActive
	case request.Decision == proto.LessonApprove && forget:
		l.State, l.Change = proto.LessonForgotten, nil
	case request.Decision == proto.LessonApprove:
		l.Text, l.Change = waiting, nil
	case request.Decision == proto.LessonDiscard && l.State == proto.LessonPending:
		l.State = proto.LessonDiscarded
	case request.Decision == proto.LessonDiscard:
		l.Change = nil
	case request.Decision == proto.LessonForget && l.State == proto.LessonActive:
		l.State, l.Change = proto.LessonForgotten, nil
	default:
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, "not this decision")
		return
	}
	l.UpdatedAt = Now
	s.emitLocked(proto.Event{Type: proto.EventLesson, Lesson: &l})
	s.rememberLocked(r, request.RequestID, l)
	writeJSON(w, http.StatusOK, l)
}

// events serves GET /v1/events as the service's hub does: a hello at where
// the stream starts, the kept events after Last-Event-ID, or one resync
// when they are gone, then the events emitted while the stream is open.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	var last int64
	if h := r.Header.Get("Last-Event-ID"); h != "" {
		var err error
		if last, err = strconv.ParseInt(h, 10, 64); err != nil || last < 0 {
			writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "bad Last-Event-ID")
			return
		}
	}
	ch := make(chan proto.Event, 1024)
	s.mu.Lock()
	start, noHello := last, s.noHello
	var backlog []proto.Event
	switch {
	case last == 0:
		start = s.seq
	case last == s.seq:
	case last > s.seq || len(s.kept) == 0 || last < s.kept[0].Seq-1:
		backlog = []proto.Event{{Seq: s.seq, Type: proto.EventResync}}
	default:
		for _, ev := range s.kept {
			if ev.Seq > last {
				backlog = append(backlog, ev)
			}
		}
	}
	s.streams[ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.streams, ch)
		s.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	if !noHello {
		backlog = append([]proto.Event{{Seq: start, Type: proto.EventHello}}, backlog...)
	}
	for _, ev := range backlog {
		if proto.WriteEvent(w, ev) != nil {
			return
		}
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, open := <-ch:
			if !open {
				return
			}
			if proto.WriteEvent(w, ev) != nil {
				return
			}
		}
	}
}

// errorReader reads err, or the end where it is nil.
type errorReader struct{ err error }

func (e errorReader) Read([]byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	return 0, io.EOF
}

// queuedPreview is text as a queued message shows it: its first
// proto.MaxQueuedText bytes, cut at a character, and whether it was cut,
// as pocketd's queue gives it.
func queuedPreview(text string) (string, bool) {
	if len(text) <= proto.MaxQueuedText {
		return text, false
	}
	n := proto.MaxQueuedText
	for n > 0 && !utf8.RuneStart(text[n]) {
		n--
	}
	return text[:n], true
}
