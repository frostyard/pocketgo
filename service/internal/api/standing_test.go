package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
)

// standingCalls is what the fake engine's standing calls did.
type standingCalls struct {
	// compacts are the CompactIfOver calls, by session, with their
	// threshold.
	compacts [][2]int64
	// resets are the StartOver keys whose reset is in, by session.
	resets map[string]int64
	// reconfigured are the Reconfigure calls that changed a session.
	reconfigured []reconfigured
	// setModels are the SetModel calls that changed a session, in order;
	// runs is the model each session took last, by any of them, and its
	// level. changes are the changes SetModel kept, by request key, and
	// picks the owner's picks of standing sessions, which a reset drops.
	setModels []setModel
	runs      map[int64]setModel
	changes   map[string]setModel
	routes    map[string]string
	last      map[int64]string
	picks     map[int64]ModelChoice
	// modelChangeErr fails every ModelChange; setModelErr fails every
	// SetModel before it changes anything;
	// afterSetModel sees each change of SetModel's, after it, and
	// afterSessionModel each SessionModel's answer, with f.mu free;
	// sessionModelErr fails every SessionModel.
	modelChangeErr    error
	setModelErr       error
	afterSetModel     func(key string)
	afterSessionModel func(session int64)
	sessionModelErr   error
	// onReconfigure sees each Reconfigure, before it answers; onStartOver
	// each StartOver's key, before it resets; onCompact each CompactIfOver.
	// reconfigureErr fails every Reconfigure.
	onReconfigure  func(session int64)
	onStartOver    func(key string)
	onCompact      func(session int64)
	reconfigureErr error
}

type reconfigured struct {
	session int64
	spec    SessionSpec
}

type setModel struct {
	session  int64
	model    proto.ModelRef
	thinking string
}

// took notes that session runs model at thinking now; f.mu is held.
func (f *fakeEngine) took(session int64, model proto.ModelRef, thinking string) {
	if f.standing.runs == nil {
		f.standing.runs = map[int64]setModel{}
	}
	f.standing.runs[session] = setModel{session, model, thinking}
}

// SetModel records the owner's change, which applies at once, under its
// request key, as the engine does: a key whose change is in changes
// nothing again. pick keeps it as the session's pick.
func (f *fakeEngine) SetModel(_ context.Context, session int64, key string, choice ModelChoice, route string, pick bool) error {
	f.mu.Lock()
	if f.standing.setModelErr != nil {
		f.mu.Unlock()
		return f.standing.setModelErr
	}
	if _, in := f.standing.changes[key]; in {
		f.mu.Unlock()
		return nil
	}
	change := setModel{session, choice.Model, choice.Thinking}
	if f.standing.changes == nil {
		f.standing.changes = map[string]setModel{}
	}
	f.standing.changes[key] = change
	if f.standing.routes == nil {
		f.standing.routes = map[string]string{}
	}
	f.standing.routes[key] = route
	if f.standing.last == nil {
		f.standing.last = map[int64]string{}
	}
	f.standing.last[session] = key
	if pick {
		if f.standing.picks == nil {
			f.standing.picks = map[int64]ModelChoice{}
		}
		f.standing.picks[session] = choice
	}
	f.standing.setModels = append(f.standing.setModels, change)
	f.took(session, choice.Model, choice.Thinking)
	hook := f.standing.afterSetModel
	f.mu.Unlock()
	if hook != nil {
		hook(key)
	}
	return nil
}

// ModelChange is the change SetModel kept under key, for session, its
// route, and whether it is the session's latest.
func (f *fakeEngine) ModelChange(_ context.Context, session int64, key string) (KeptChange, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.standing.modelChangeErr != nil {
		return KeptChange{}, false, f.standing.modelChangeErr
	}
	c, in := f.standing.changes[key]
	if !in || c.session != session {
		return KeptChange{}, false, nil
	}
	return KeptChange{Choice: ModelChoice{Model: c.model, Thinking: c.thinking}, Route: f.standing.routes[key], Latest: f.standing.last[session] == key}, true, nil
}

// StandingPick is session's pick, as SetModel kept it and no reset
// dropped it.
func (f *fakeEngine) StandingPick(_ context.Context, session int64) (ModelChoice, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.standing.picks[session]
	return p, ok, nil
}

// CompactIfOver records the check; the fake has no context to estimate.
func (f *fakeEngine) CompactIfOver(_ context.Context, session int64, compactAt int) (bool, error) {
	f.mu.Lock()
	hook := f.standing.onCompact
	f.mu.Unlock()
	if hook != nil {
		hook(session)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.standing.compacts = append(f.standing.compacts, [2]int64{session, int64(compactAt)})
	return true, nil
}

// StartOver resets an idle session once a key, as the engine does, and
// takes spec in the same step: one refused changes nothing.
func (f *fakeEngine) StartOver(_ context.Context, session int64, key string, spec *SessionSpec) (Reconfigured, error) {
	f.mu.Lock()
	hook := f.standing.onStartOver
	f.mu.Unlock()
	if hook != nil {
		hook(key)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, done := f.standing.resets[key]; done {
		return ReconfigureSame, nil
	}
	// A run, or a message waiting for one, as a failed run leaves it.
	queued := false
	for k, sub := range f.sent {
		queued = queued || f.queued[sub] && f.to[k] == session
	}
	if f.runFirst != 0 || f.busy[session] || queued {
		return ReconfigureSame, ErrSessionBusy
	}
	if f.standing.resets == nil {
		f.standing.resets = map[string]int64{}
	}
	f.standing.resets[key] = session
	delete(f.standing.picks, session)
	if spec == nil {
		return ReconfigureSame, nil
	}
	f.standing.reconfigured = append(f.standing.reconfigured, reconfigured{session, *spec})
	f.took(session, spec.Model, spec.Thinking)
	return ReconfigureApplied, nil
}

// SessionModel is the model the session took last, and its level: its
// newest reconfiguration's or change's, else the one it was made with.
func (f *fakeEngine) SessionModel(ctx context.Context, session int64) (ModelChoice, error) {
	f.mu.Lock()
	hook := f.standing.afterSessionModel
	f.mu.Unlock()
	if hook != nil {
		defer hook(session)
	}
	return f.sessionModel(ctx, session)
}

func (f *fakeEngine) sessionModel(_ context.Context, session int64) (ModelChoice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.standing.sessionModelErr != nil {
		return ModelChoice{}, f.standing.sessionModelErr
	}
	if r, ok := f.standing.runs[session]; ok {
		return ModelChoice{Model: r.model, Thinking: r.thinking}, nil
	}
	for _, made := range f.made {
		if made.ID == session {
			return ModelChoice{Model: made.Model, Thinking: made.Thinking}, nil
		}
	}
	return ModelChoice{}, nil
}

// Reconfigure records the new spec of a session with no run; one with a
// run changes nothing, and its change waits.
func (f *fakeEngine) Reconfigure(_ context.Context, session int64, spec SessionSpec) (Reconfigured, error) {
	f.mu.Lock()
	hook := f.standing.onReconfigure
	f.mu.Unlock()
	if hook != nil {
		hook(session)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.standing.reconfigureErr != nil {
		return ReconfigureSame, f.standing.reconfigureErr
	}
	if f.runFirst != 0 || f.busy[session] {
		return ReconfigureDeferred, nil
	}
	f.standing.reconfigured = append(f.standing.reconfigured, reconfigured{session, spec})
	f.took(session, spec.Model, spec.Thinking)
	return ReconfigureApplied, nil
}

// big is a model with a context window, for a standing profile's
// threshold.
var big = proto.Model{ModelRef: proto.ModelRef{Provider: "faux", ID: "faux-big"}, Name: "Big", ThinkingLevels: []string{"off"}, ContextWindow: 1_000_000}

const odrade = "---\nrole: supervisor\nmodel: faux/faux-big\nhome: ~\nsession: standing\ncompactAt: 300000\n---\n# Odrade\n\nShe leads.\n"

// standingHarness is a harness whose profiles keep Odrade's standing
// session, and Vera's plain ones.
func standingHarness(t *testing.T) (*harness, string, string) {
	t.Helper()
	h := newHarness(t)
	h.engine.more = []proto.Model{big}
	dir, home := h.writeProfiles(map[string]string{
		"odrade.md": odrade,
		"vera.md":   "---\nmodel: faux/faux-1\nhome: ~\n---\n# Vera\n",
	})
	return h, dir, home
}

// New Session on a standing profile opens its standing session: the first
// makes it, in the profile's home on the router's model, titled with the
// agent's name; every later one, and a retry, answers with it, and the
// engine makes no other. Other profiles start a session each time.
func TestNewSessionOnAStandingProfileOpensItsSession(t *testing.T) {
	h, _, home := standingHarness(t)
	ctx := context.Background()
	first, status := h.createStatus(proto.CreateSession{RequestID: "r1", Profile: "odrade"})
	if status != http.StatusCreated {
		t.Fatalf("the first: %d", status)
	}
	if !first.Standing || first.Title != "Odrade" || first.Cwd != home || first.Model != big.ModelRef || first.Route != "profile" || first.Profile != "odrade" {
		t.Fatalf("the standing session %+v", first)
	}
	if _, ok := h.engine.byKey["standing:odrade"]; !ok {
		t.Fatalf("the engine's keys %v", h.engine.byKey)
	}
	if spec := h.engine.specs[0]; spec.Profile == nil || spec.Profile.ID != "odrade" || spec.Roster == "" || spec.Lessons == "" {
		t.Fatalf("spec %+v", spec)
	}
	again, status := h.createStatus(proto.CreateSession{RequestID: "r2", Profile: "odrade"})
	if status != http.StatusOK || again.ID != first.ID || !again.Standing {
		t.Fatalf("again: %d %+v", status, again)
	}
	retry, status := h.createStatus(proto.CreateSession{RequestID: "r1", Profile: "odrade"})
	if status != http.StatusOK || retry.ID != first.ID {
		t.Fatalf("a retry: %d %+v", status, retry)
	}
	if h.engine.created != 1 {
		t.Fatalf("the engine made %d sessions", h.engine.created)
	}
	// It opens though its profile's model no longer runs: only a new one
	// needs that.
	h.engine.mu.Lock()
	h.engine.signedOut = true
	h.engine.mu.Unlock()
	if signedOut, status := h.createStatus(proto.CreateSession{RequestID: "r3", Profile: "odrade"}); status != http.StatusOK || signedOut.ID != first.ID {
		t.Fatalf("with no model: %d %+v", status, signedOut)
	}
	h.engine.mu.Lock()
	h.engine.signedOut = false
	h.engine.mu.Unlock()
	// Vera keeps no standing session.
	a, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "v1", Profile: "vera"})
	b, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "v2", Profile: "vera"})
	if a.ID == b.ID || a.Standing || b.Standing {
		t.Fatalf("vera's sessions %+v %+v", a, b)
	}
	list, err := h.client.Sessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list {
		if s.Standing != (s.ID == first.ID) {
			t.Errorf("session %d standing %v", s.ID, s.Standing)
		}
	}
	profiles, err := h.client.Profiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range profiles {
		if p.Standing != (p.ID == "odrade") || p.CompactAt != map[string]int{"odrade": 300000}[p.ID] {
			t.Errorf("profile %+v", p)
		}
	}
}

// A standing session has its own folder, model, thinking level and title:
// a request that names one is refused, and makes nothing.
func TestAStandingProfileTakesTheProfileAlone(t *testing.T) {
	h, _, _ := standingHarness(t)
	for name, request := range map[string]proto.CreateSession{
		"a folder":   {RequestID: "a", Profile: "odrade", Cwd: h.work},
		"a model":    {RequestID: "b", Profile: "odrade", Model: big.ModelRef},
		"a thinking": {RequestID: "c", Profile: "odrade", Thinking: "off"},
		"a title":    {RequestID: "d", Profile: "odrade", Title: "Odrade 2"},
	} {
		_, err := h.client.CreateSession(context.Background(), request)
		asError(t, err, http.StatusBadRequest, proto.ErrorCodeBadRequest)
		if !strings.Contains(err.Error(), "standing session") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if h.engine.created != 0 {
		t.Fatalf("the engine made %d sessions", h.engine.created)
	}
}

// A standing profile's compactAt is below the window of the model its
// session runs: one that is not lists a problem, and starts nothing.
func TestACompactAtPastTheWindowIsAProblem(t *testing.T) {
	h, dir, _ := standingHarness(t)
	if err := os.WriteFile(filepath.Join(dir, "odrade.md"), []byte(strings.Replace(odrade, "compactAt: 300000", "compactAt: 1000000", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := h.client.Profiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range list {
		if p.ID == "odrade" && (!strings.Contains(p.Problem, "context window") || p.Runs != (proto.ModelRef{})) {
			t.Errorf("odrade %+v", p)
		}
	}
	_, err = h.client.CreateSession(context.Background(), proto.CreateSession{RequestID: "r1", Profile: "odrade"})
	asError(t, err, http.StatusBadRequest, proto.ErrorCodeBadRequest)
	if h.engine.created != 0 {
		t.Fatal("a session started")
	}
}

// A conversation made for the standing profile whose record failed is
// recorded as it was made, not as the router would choose now (rule 23).
func TestAStandingSessionMadeButNotRecordedIsRecordedAsMade(t *testing.T) {
	h, _, home := standingHarness(t)
	made := SessionSpec{Title: "Odrade", Cwd: home, Model: model, Route: "profile"}
	id, err := h.engine.CreateSession(context.Background(), "standing:odrade", made)
	if err != nil {
		t.Fatal(err)
	}
	session, status := h.createStatus(proto.CreateSession{RequestID: "r1", Profile: "odrade"})
	if status != http.StatusCreated || session.ID != id || session.Model != model || !session.Standing {
		t.Fatalf("%d %+v", status, session)
	}
}

// Start Over gives an idle standing session a fresh context, once a
// request: a retry answers as the first did. A session at work, a session
// that is no standing one and another target for the same request are
// refused.
func TestStartOver(t *testing.T) {
	h, _, _ := standingHarness(t)
	ctx := context.Background()
	standing, _ := h.createStatus(proto.CreateSession{RequestID: "r1", Profile: "odrade"})
	plain, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "v1", Profile: "vera"})
	if err != nil {
		t.Fatal(err)
	}
	h.engine.busy = map[int64]bool{standing.ID: true}
	_, err = h.client.StartOver(ctx, standing.ID, proto.StartOver{RequestID: "s1"})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	h.engine.mu.Lock()
	h.engine.busy = nil
	h.engine.standing.reconfigured = nil
	h.engine.mu.Unlock()
	got, err := h.client.StartOver(ctx, standing.ID, proto.StartOver{RequestID: "s1"})
	if err != nil || got.ID != standing.ID {
		t.Fatalf("start over: %+v, %v", got, err)
	}
	// It took what its profile says now first.
	if len(h.engine.standing.reconfigured) != 1 || h.engine.standing.reconfigured[0].session != standing.ID {
		t.Fatalf("reconfigured %+v", h.engine.standing.reconfigured)
	}
	if len(h.engine.standing.resets) != 1 {
		t.Fatalf("resets %v", h.engine.standing.resets)
	}
	// A retry, even with the session at work since, answers as the first.
	h.engine.busy = map[int64]bool{standing.ID: true}
	if again, err := h.client.StartOver(ctx, standing.ID, proto.StartOver{RequestID: "s1"}); err != nil || again.ID != standing.ID {
		t.Fatalf("a retry: %+v, %v", again, err)
	}
	h.engine.busy = nil
	_, err = h.client.StartOver(ctx, plain.ID, proto.StartOver{RequestID: "s2"})
	asError(t, err, http.StatusBadRequest, proto.ErrorCodeBadRequest)
	_, err = h.client.StartOver(ctx, 999, proto.StartOver{RequestID: "s3"})
	asError(t, err, http.StatusNotFound, proto.ErrorCodeNotFound)
	_, err = h.client.StartOver(ctx, plain.ID, proto.StartOver{RequestID: "s1"})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if len(h.engine.standing.resets) != 1 {
		t.Fatalf("resets %v", h.engine.standing.resets)
	}
}

// When a standing session's run ends, it takes what its profile says now,
// its new model recorded and published, then pocketd checks its context
// against the profile's compactAt; no other session does either.
func TestARunEndRefreshesAndCompactsAStandingSession(t *testing.T) {
	h, dir, _ := standingHarness(t)
	ctx := context.Background()
	standing, _ := h.createStatus(proto.CreateSession{RequestID: "r1", Profile: "odrade"})
	plain, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "v1", Profile: "vera"})
	moved := proto.Model{ModelRef: proto.ModelRef{Provider: "faux", ID: "faux-huge"}, ThinkingLevels: []string{"off"}, ContextWindow: 2_000_000}
	h.engine.mu.Lock()
	h.engine.more = append(h.engine.more, moved)
	h.engine.mu.Unlock()
	if err := os.WriteFile(filepath.Join(dir, "odrade.md"), []byte(strings.Replace(odrade, "faux/faux-big", "faux/faux-huge", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	ended := make(chan int64, 4)
	h.server.afterRunEnded = func(id int64) { ended <- id }
	for _, id := range []int64{standing.ID, plain.ID} {
		h.server.PublishLive(proto.Live{SessionID: id, Busy: true})
		h.server.PublishLive(proto.Live{SessionID: id})
		for got := <-ended; got != id; got = <-ended {
		}
	}
	h.engine.mu.Lock()
	reconfigured, compacts := slices.Clone(h.engine.standing.reconfigured), slices.Clone(h.engine.standing.compacts)
	h.engine.mu.Unlock()
	if len(reconfigured) != 1 || reconfigured[0].session != standing.ID || reconfigured[0].spec.Model != moved.ModelRef ||
		reconfigured[0].spec.Roster == "" || reconfigured[0].spec.Lessons != "" {
		t.Fatalf("reconfigured %+v", reconfigured)
	}
	if want := [][2]int64{{standing.ID, 300000}}; !slices.Equal(compacts, want) {
		t.Fatalf("compacts %v, want %v", compacts, want)
	}
	row, err := h.store.Session(ctx, standing.ID)
	if err != nil || row.Model != "faux-huge" || row.Route != "profile" {
		t.Fatalf("the recorded model %+v, %v", row, err)
	}
}

// A standing session at work keeps its model: the work database records
// none the engine did not take.
func TestABusyStandingSessionKeepsItsModel(t *testing.T) {
	h, dir, _ := standingHarness(t)
	ctx := context.Background()
	standing, _ := h.createStatus(proto.CreateSession{RequestID: "r1", Profile: "odrade"})
	moved := proto.Model{ModelRef: proto.ModelRef{Provider: "faux", ID: "faux-huge"}, ThinkingLevels: []string{"off"}, ContextWindow: 2_000_000}
	h.engine.mu.Lock()
	h.engine.more = append(h.engine.more, moved)
	h.engine.busy = map[int64]bool{standing.ID: true}
	h.engine.mu.Unlock()
	if err := os.WriteFile(filepath.Join(dir, "odrade.md"), []byte(strings.Replace(odrade, "faux/faux-big", "faux/faux-huge", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	h.server.refreshStanding(ctx, standing.ID)
	if row, err := h.store.Session(ctx, standing.ID); err != nil || row.Model != "faux-big" {
		t.Fatalf("the recorded model %+v, %v", row, err)
	}
}

// The owner's message to a standing session follows its profile's
// refresh, so the run it starts has what the profile says now.
func TestAMessageToAStandingSessionRefreshesItFirst(t *testing.T) {
	h, _, _ := standingHarness(t)
	ctx := context.Background()
	standing, _ := h.createStatus(proto.CreateSession{RequestID: "r1", Profile: "odrade"})
	var order []string
	h.engine.standing.onReconfigure = func(int64) { order = append(order, "reconfigure") }
	h.engine.onSend = func(int64, string) { order = append(order, "send") }
	if _, err := h.client.Send(ctx, standing.ID, proto.SendMessage{RequestID: "m1", Text: "Hello"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(order, []string{"reconfigure", "send"}) {
		t.Fatalf("order %q", order)
	}
}

// writeOdrade rewrites Odrade's profile with old replaced by new.
func writeOdrade(t *testing.T, dir, old, new string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "odrade.md"), []byte(strings.Replace(odrade, old, new, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The owner's message to a standing session whose profile cannot be
// applied is refused, not run with the profile's old model or tools; a
// retry of a message that went answers as it did (Codex review of #43,
// round 1).
func TestAMessageToAStandingSessionWhoseProfileFailsIsRefused(t *testing.T) {
	h, dir, _ := standingHarness(t)
	ctx := context.Background()
	standing, _ := h.createStatus(proto.CreateSession{RequestID: "r1", Profile: "odrade"})
	sent, err := h.client.Send(ctx, standing.ID, proto.SendMessage{RequestID: "m1", Text: "Hello"})
	if err != nil {
		t.Fatal(err)
	}
	// Its model is one pocketd does not run.
	writeOdrade(t, dir, "faux/faux-big", "faux/faux-gone")
	_, err = h.client.Send(ctx, standing.ID, proto.SendMessage{RequestID: "m2", Text: "Again"})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if !strings.Contains(err.Error(), "profile cannot be applied") {
		t.Fatalf("the refusal: %v", err)
	}
	h.engine.mu.Lock()
	_, went := h.engine.sent[requestKeyOf(h, "m2")]
	h.engine.mu.Unlock()
	if went {
		t.Fatal("the message went")
	}
	if again, err := h.client.Send(ctx, standing.ID, proto.SendMessage{RequestID: "m1", Text: "Hello"}); err != nil || again.SubmissionID != sent.SubmissionID {
		t.Fatalf("a retry of the message that went: %+v, %v", again, err)
	}
}

// requestKeyOf is the request key of requestID from the harness's device.
func requestKeyOf(h *harness, requestID string) string { return "u:" + h.device + ":" + requestID }

// A refresh reads what decides it under refreshMu: an older refresh never
// applies its profile after a newer one did (Codex review of #43, round
// 1).
func TestAStandingRefreshReadsUnderTheLock(t *testing.T) {
	h, _, _ := standingHarness(t)
	standing, _ := h.createStatus(proto.CreateSession{RequestID: "r1", Profile: "odrade"})
	held := false
	h.server.afterStandingRead = func() {
		if h.server.refreshMu.TryLock() {
			h.server.refreshMu.Unlock()
			return
		}
		held = true
	}
	if _, err := h.server.reconfigureStanding(context.Background(), standing.ID); err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Fatal("the refresh read its profile without refreshMu")
	}
}

// A Start Over's request is claimed with its target before the engine
// resets: if pocketd stops once the reset is in, the request id stays
// bound to that session, and never resets another (Codex review of #43,
// round 1).
func TestAStartOverIsClaimedBeforeTheReset(t *testing.T) {
	h, _, _ := standingHarness(t)
	ctx := context.Background()
	standing, _ := h.createStatus(proto.CreateSession{RequestID: "r1", Profile: "odrade"})
	kind := "startOver:" + strconv.FormatInt(standing.ID, 10)
	claimed := false
	h.engine.standing.onStartOver = func(key string) {
		_, claimed, _ = h.store.LookupRequest(ctx, key, kind)
	}
	if _, err := h.client.StartOver(ctx, standing.ID, proto.StartOver{RequestID: "s1"}); err != nil {
		t.Fatal(err)
	}
	if !claimed {
		t.Fatal("the engine reset the session before its request was claimed")
	}
}

// A busy Start Over frees its request id: a failed request leaves no
// record, so the same id may start over once the session is idle.
func TestABusyStartOverFreesItsID(t *testing.T) {
	h, _, _ := standingHarness(t)
	ctx := context.Background()
	standing, _ := h.createStatus(proto.CreateSession{RequestID: "r1", Profile: "odrade"})
	h.engine.mu.Lock()
	h.engine.busy = map[int64]bool{standing.ID: true}
	h.engine.mu.Unlock()
	_, err := h.client.StartOver(ctx, standing.ID, proto.StartOver{RequestID: "s1"})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if _, ok, err := h.store.LookupRequest(ctx, requestKeyOf(h, "s1"), "startOver:"+strconv.FormatInt(standing.ID, 10)); err != nil || ok {
		t.Fatalf("the busy request's record: %v, %v", ok, err)
	}
}

// A standing session made for a request whose record failed is recorded
// as its profile's standing session when that request is tried again,
// whatever its profile says by then (Codex review of #43, round 1).
func TestAStandingCreateIsRecoveredWhateverItsProfileSaysNow(t *testing.T) {
	h, dir, home := standingHarness(t)
	ctx := context.Background()
	made := SessionSpec{Title: "Odrade", Cwd: home, Model: big.ModelRef, Route: "profile", StandingKey: "standing:odrade"}
	id, err := h.engine.CreateSession(ctx, requestKeyOf(h, "r1"), made)
	if err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	h.engine.made[requestKeyOf(h, "r1")] = Created{ID: id, Title: "Odrade", Cwd: home, Model: big.ModelRef, Profile: "odrade", Route: "profile", Standing: true}
	h.engine.mu.Unlock()
	// The owner since made Odrade keep no standing session.
	writeOdrade(t, dir, "session: standing\ncompactAt: 300000\n", "")
	session, status := h.createStatus(proto.CreateSession{RequestID: "r1", Profile: "odrade"})
	if status != http.StatusCreated || session.ID != id || !session.Standing {
		t.Fatalf("%d %+v", status, session)
	}
	if got, err := h.store.StandingSession(ctx, "odrade"); err != nil || got != id {
		t.Fatalf("the standing session %d, %v", got, err)
	}
	if h.engine.created != 1 {
		t.Fatalf("the engine made %d sessions", h.engine.created)
	}
}

// A private profile works only in its home: one whose home is not its
// standing session's folder is not applied there, and the owner's message
// is refused (Codex review of #43, round 1).
func TestAPrivateProfileIsNotAppliedOutsideItsHome(t *testing.T) {
	h, dir, _ := standingHarness(t)
	ctx := context.Background()
	standing, _ := h.createStatus(proto.CreateSession{RequestID: "r1", Profile: "odrade"})
	elsewhere := t.TempDir()
	writeOdrade(t, dir, "home: ~", "home: "+elsewhere+"\nprivate: true")
	if _, err := h.server.reconfigureStanding(ctx, standing.ID); err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("the refresh: %v", err)
	}
	h.engine.mu.Lock()
	n := len(h.engine.standing.reconfigured)
	h.engine.mu.Unlock()
	if n != 0 {
		t.Fatalf("reconfigured %d times", n)
	}
	_, err := h.client.Send(ctx, standing.ID, proto.SendMessage{RequestID: "m1", Text: "Hello"})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
}

// pocketd compacts only at a threshold the profile's checks passed: one
// past its model's window leaves PiG's own threshold alone (Codex review
// of #43, round 1).
func TestARejectedThresholdCompactsNothing(t *testing.T) {
	h, dir, _ := standingHarness(t)
	standing, _ := h.createStatus(proto.CreateSession{RequestID: "r1", Profile: "odrade"})
	writeOdrade(t, dir, "compactAt: 300000", "compactAt: 1500000")
	h.server.standingRunEnded(standing.ID)
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if len(h.engine.standing.compacts) != 0 {
		t.Fatalf("compacts %v", h.engine.standing.compacts)
	}
}

// A standing profile whose threshold is past its model's window cannot
// start, for the team's rules too: no roster offers it, and no delegation
// or consult takes it (Codex review of #43, round 1).
func TestAStandingProfilePastItsWindowCannotStart(t *testing.T) {
	h, dir, _ := standingHarness(t)
	writeOdrade(t, dir, "compactAt: 300000", "compactAt: 1000000")
	p, err := h.server.profiles.Find("odrade")
	if err != nil {
		t.Fatal(err)
	}
	models, _ := h.engine.Models(context.Background())
	if err := h.server.startable(p, models); err == nil || !strings.Contains(err.Error(), "context window") {
		t.Fatalf("startable: %v", err)
	}
}

// A standing session takes no owner's message while pocketd runs without
// profiles: its profile cannot be read, so it cannot be applied (Codex
// review of #43, round 2).
func TestAStandingSessionTakesNoMessageWithoutProfiles(t *testing.T) {
	h, _, _ := standingHarness(t)
	standing, _ := h.createStatus(proto.CreateSession{RequestID: "r1", Profile: "odrade"})
	h.server.SetProfiles(nil)
	_, err := h.client.Send(context.Background(), standing.ID, proto.SendMessage{RequestID: "m1", Text: "Hello"})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
}

// A message PiG admitted, whose record a stop of pocketd lost, answers as
// it went when tried again, whatever its profile says by then (Codex
// review of #43, round 2).
func TestAMessageWhoseRecordWasLostAnswersAsItWent(t *testing.T) {
	h, dir, _ := standingHarness(t)
	ctx := context.Background()
	standing, _ := h.createStatus(proto.CreateSession{RequestID: "r1", Profile: "odrade"})
	key := requestKeyOf(h, "m1")
	if _, _, err := h.store.ClaimRequest(ctx, key, "send:"+strconv.FormatInt(standing.ID, 10)); err != nil {
		t.Fatal(err)
	}
	submission, err := h.engine.Send(ctx, standing.ID, key, "Hello")
	if err != nil {
		t.Fatal(err)
	}
	writeOdrade(t, dir, "faux/faux-big", "faux/faux-gone")
	sent, err := h.client.Send(ctx, standing.ID, proto.SendMessage{RequestID: "m1", Text: "Hello"})
	if err != nil || sent.SubmissionID != submission {
		t.Fatalf("the retry: %+v, %v", sent, err)
	}
}

// pocketd compacts at the threshold of the profile the run's end just
// applied, under refreshMu, so no refresh comes between; a refresh that
// failed compacts nothing (Codex review of #43, round 2).
func TestACompactionFollowsTheRefreshThatApplied(t *testing.T) {
	h, _, _ := standingHarness(t)
	standing, _ := h.createStatus(proto.CreateSession{RequestID: "r1", Profile: "odrade"})
	held := false
	h.engine.standing.onCompact = func(int64) {
		if h.server.refreshMu.TryLock() {
			h.server.refreshMu.Unlock()
			return
		}
		held = true
	}
	h.server.standingRunEnded(standing.ID)
	if !held {
		t.Fatal("the compaction ran outside refreshMu")
	}
	h.engine.mu.Lock()
	h.engine.standing.compacts = nil
	h.engine.standing.reconfigureErr = errors.New("the durable store is locked")
	h.engine.mu.Unlock()
	h.server.standingRunEnded(standing.ID)
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if len(h.engine.standing.compacts) != 0 {
		t.Fatalf("compacts after a failed refresh %v", h.engine.standing.compacts)
	}
}

// A busy Start Over whose release fails says so, and a delivery pass
// frees its request id later; a claim whose reset is in is never freed
// (Codex review of #43, round 2).
func TestAStartOverWhoseReleaseFailedIsFreedLater(t *testing.T) {
	h, _, _ := standingHarness(t)
	ctx := context.Background()
	standing, _ := h.createStatus(proto.CreateSession{RequestID: "r1", Profile: "odrade"})
	kind := "startOver:" + strconv.FormatInt(standing.ID, 10)
	h.engine.mu.Lock()
	h.engine.busy = map[int64]bool{standing.ID: true}
	h.engine.mu.Unlock()
	release := h.server.releaseRequest
	h.server.releaseRequest = func(context.Context, string, string) error { return errors.New("database is locked") }
	_, err := h.client.StartOver(ctx, standing.ID, proto.StartOver{RequestID: "s1"})
	asError(t, err, http.StatusInternalServerError, proto.ErrorCodeInternal)
	if !strings.Contains(err.Error(), "could not be freed yet") {
		t.Fatalf("the answer: %v", err)
	}
	h.server.releaseRequest = release
	// A claim of another Start Over whose reset is in waits to be freed
	// too: the pass leaves it.
	done := requestKeyOf(h, "s2")
	if _, _, err := h.store.ClaimRequest(ctx, done, kind); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	h.engine.busy = nil
	h.engine.mu.Unlock()
	if _, err := h.engine.StartOver(ctx, standing.ID, done, nil); err != nil {
		t.Fatal(err)
	}
	h.server.releaseLater(done, kind)
	if err := h.server.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := h.store.LookupRequest(ctx, requestKeyOf(h, "s1"), kind); err != nil || ok {
		t.Fatalf("the busy Start Over's claim: %v, %v", ok, err)
	}
	if _, ok, err := h.store.LookupRequest(ctx, done, kind); err != nil || !ok {
		t.Fatalf("the claim whose reset is in: %v, %v", ok, err)
	}
}

// A message to a standing session at work whose profile changed since its
// run began is refused: PiG could start the run that takes it as the
// current one ends, before pocketd applies the profile (Codex review of
// #43, round 4).
func TestAMessageToABusyStandingSessionWithAChangeWaitingIsRefused(t *testing.T) {
	h, dir, _ := standingHarness(t)
	ctx := context.Background()
	standing, _ := h.createStatus(proto.CreateSession{RequestID: "r1", Profile: "odrade"})
	writeOdrade(t, dir, "She leads.", "She leads, and listens.")
	h.engine.mu.Lock()
	h.engine.busy = map[int64]bool{standing.ID: true}
	h.engine.mu.Unlock()
	_, err := h.client.Send(ctx, standing.ID, proto.SendMessage{RequestID: "m1", Text: "Hello"})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	h.engine.mu.Lock()
	_, went := h.engine.sent[requestKeyOf(h, "m1")]
	h.engine.mu.Unlock()
	if went {
		t.Fatal("the message went")
	}
	// Once the run ended, it takes the profile, and goes.
	h.engine.mu.Lock()
	h.engine.busy = nil
	h.engine.mu.Unlock()
	if _, err := h.client.Send(ctx, standing.ID, proto.SendMessage{RequestID: "m1", Text: "Hello"}); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if len(h.engine.standing.reconfigured) != 1 {
		t.Fatalf("reconfigured %+v", h.engine.standing.reconfigured)
	}
}

// A message refused for its standing session's profile, whose claim an
// earlier try left and whose submission PiG never took, frees its request
// id (Codex review of #43, round 4).
func TestARefusedRetryFreesItsUnsentClaim(t *testing.T) {
	h, dir, _ := standingHarness(t)
	ctx := context.Background()
	standing, _ := h.createStatus(proto.CreateSession{RequestID: "r1", Profile: "odrade"})
	kind := "send:" + strconv.FormatInt(standing.ID, 10)
	if _, _, err := h.store.ClaimRequest(ctx, requestKeyOf(h, "m1"), kind); err != nil {
		t.Fatal(err)
	}
	writeOdrade(t, dir, "faux/faux-big", "faux/faux-gone")
	_, err := h.client.Send(ctx, standing.ID, proto.SendMessage{RequestID: "m1", Text: "Hello"})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if _, ok, err := h.store.LookupRequest(ctx, requestKeyOf(h, "m1"), kind); err != nil || ok {
		t.Fatalf("the unsent claim: %v, %v", ok, err)
	}
}

// A Start Over refused for a session whose failed run left a message
// queued changes nothing, its profile's change included: the profile and
// the reset go in one step (Codex review of #43, round 4).
func TestARefusedStartOverChangesNothing(t *testing.T) {
	h, dir, _ := standingHarness(t)
	ctx := context.Background()
	standing, _ := h.createStatus(proto.CreateSession{RequestID: "r1", Profile: "odrade"})
	writeOdrade(t, dir, "She leads.", "She leads, and listens.")
	submission, err := h.engine.Send(ctx, standing.ID, requestKeyOf(h, "m1"), "Hello")
	if err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	h.engine.queued = map[int64]bool{submission: true}
	h.engine.mu.Unlock()
	_, err = h.client.StartOver(ctx, standing.ID, proto.StartOver{RequestID: "s1"})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if len(h.engine.standing.reconfigured) != 0 || len(h.engine.standing.resets) != 0 {
		t.Fatalf("reconfigured %+v resets %v", h.engine.standing.reconfigured, h.engine.standing.resets)
	}
}

// Start Over watches its session before the reset, so the reset's entry
// reaches the event stream; a watch that fails consumes nothing (Codex
// review of #43, round 4).
func TestStartOverWatchesFirst(t *testing.T) {
	h, _, _ := standingHarness(t)
	ctx := context.Background()
	standing, _ := h.createStatus(proto.CreateSession{RequestID: "r1", Profile: "odrade"})
	h.engine.mu.Lock()
	delete(h.engine.watching, standing.ID)
	h.engine.watchErr = errors.New("the durable store is locked")
	h.engine.mu.Unlock()
	_, err := h.client.StartOver(ctx, standing.ID, proto.StartOver{RequestID: "s1"})
	asError(t, err, http.StatusInternalServerError, proto.ErrorCodeInternal)
	kind := "startOver:" + strconv.FormatInt(standing.ID, 10)
	if _, ok, err := h.store.LookupRequest(ctx, requestKeyOf(h, "s1"), kind); err != nil || ok {
		t.Fatalf("the claim after a failed watch: %v, %v", ok, err)
	}
	h.engine.mu.Lock()
	h.engine.watchErr = nil
	h.engine.mu.Unlock()
	watched := false
	h.engine.standing.onStartOver = func(string) {
		h.engine.mu.Lock()
		watched = h.engine.watching[standing.ID]
		h.engine.mu.Unlock()
	}
	if _, err := h.client.StartOver(ctx, standing.ID, proto.StartOver{RequestID: "s1"}); err != nil {
		t.Fatal(err)
	}
	if !watched {
		t.Fatal("the reset went before the watch")
	}
}

// A message whose claim has no result, and whose submission PiG cannot be
// asked about, is never checked as a new one: its retry fails, and says
// so, as an internal failure (Codex review of #43, round 3).
func TestAMessageWhoseLookupFailsIsNotTakenForNew(t *testing.T) {
	h, dir, _ := standingHarness(t)
	ctx := context.Background()
	standing, _ := h.createStatus(proto.CreateSession{RequestID: "r1", Profile: "odrade"})
	if _, _, err := h.store.ClaimRequest(ctx, requestKeyOf(h, "m1"), "send:"+strconv.FormatInt(standing.ID, 10)); err != nil {
		t.Fatal(err)
	}
	writeOdrade(t, dir, "faux/faux-big", "faux/faux-gone")
	h.engine.mu.Lock()
	h.engine.placedErr = errors.New("the durable store is locked")
	h.engine.mu.Unlock()
	_, err := h.client.Send(ctx, standing.ID, proto.SendMessage{RequestID: "m1", Text: "Hello"})
	asError(t, err, http.StatusInternalServerError, proto.ErrorCodeInternal)
}

// A session that is no standing one never takes refreshMu: a lessons
// refresh or a standing session's compaction never holds up its messages
// or its runs' ends (Codex review of #43, round 3).
func TestAnOrdinarySessionNeverTakesTheStandingLock(t *testing.T) {
	h, _, _ := standingHarness(t)
	ctx := context.Background()
	standing, _ := h.createStatus(proto.CreateSession{RequestID: "r1", Profile: "odrade"})
	plain, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "v1", Profile: "vera"})
	if err != nil {
		t.Fatal(err)
	}
	locks := 0
	h.server.onRefreshLock = func() { locks++ }
	if _, err := h.client.Send(ctx, plain.ID, proto.SendMessage{RequestID: "m1", Text: "Hello"}); err != nil {
		t.Fatal(err)
	}
	h.server.standingRunEnded(plain.ID)
	if locks != 0 {
		t.Fatalf("an ordinary session took refreshMu %d times", locks)
	}
	h.server.standingRunEnded(standing.ID)
	if locks != 1 {
		t.Fatalf("the standing session took refreshMu %d times", locks)
	}
}

// A standing profile whose folder is missing lists a problem: a new
// standing session starts there, and takes no other (Codex review of #43,
// round 3).
func TestAStandingProfileWithoutItsFolderIsAProblem(t *testing.T) {
	h, dir, _ := standingHarness(t)
	writeOdrade(t, dir, "home: ~", "home: /nonexistent/pocketgo-odrade")
	list, err := h.client.Profiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range list {
		if p.ID == "odrade" && !strings.Contains(p.Problem, "not an existing directory") {
			t.Fatalf("odrade %+v", p)
		}
	}
}

// The first delivery pass after start looks after every standing session,
// once: a run's end pocketd did not see, as it stopped, is made up for
// (Codex review of #43, round 5).
func TestTheFirstPassLooksAfterTheStandingSessions(t *testing.T) {
	h, _, _ := standingHarness(t)
	ctx := context.Background()
	standing, _ := h.createStatus(proto.CreateSession{RequestID: "r1", Profile: "odrade"})
	for range 2 {
		if err := h.server.DeliverPending(ctx); err != nil {
			t.Fatal(err)
		}
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if want := [][2]int64{{standing.ID, 300000}}; !slices.Equal(h.engine.standing.compacts, want) {
		t.Fatalf("compacts %v, want %v", h.engine.standing.compacts, want)
	}
}

// A standing session's final answer looks after it, as its run's end
// does: a short run's busy state may never reach PublishLive (Codex review
// of #43, round 5).
func TestAFinalAnswerLooksAfterAStandingSession(t *testing.T) {
	h, _, _ := standingHarness(t)
	standing, _ := h.createStatus(proto.CreateSession{RequestID: "r1", Profile: "odrade"})
	h.server.PublishEntry(proto.Entry{ID: 7, SessionID: standing.ID, Kind: proto.EntryAssistant, Text: "Done.", StopReason: "stop"})
	h.server.passes.Wait()
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if want := [][2]int64{{standing.ID, 300000}}; !slices.Equal(h.engine.standing.compacts, want) {
		t.Fatalf("compacts %v, want %v", h.engine.standing.compacts, want)
	}
}

// A Start Over records the model PiG runs before it completes: one whose
// reset and model PiG took, but whose record a stop of pocketd lost,
// records it when tried again (Codex review of #43, round 5).
func TestAStartOverRecordsTheModelPiGRuns(t *testing.T) {
	h, dir, _ := standingHarness(t)
	ctx := context.Background()
	standing, _ := h.createStatus(proto.CreateSession{RequestID: "r1", Profile: "odrade"})
	huge := proto.Model{ModelRef: proto.ModelRef{Provider: "faux", ID: "faux-huge"}, ThinkingLevels: []string{"off"}, ContextWindow: 2_000_000}
	h.engine.mu.Lock()
	h.engine.more = append(h.engine.more, huge)
	h.engine.mu.Unlock()
	writeOdrade(t, dir, "faux/faux-big", "faux/faux-huge")
	key := requestKeyOf(h, "s1")
	if _, _, err := h.store.ClaimRequest(ctx, key, "startOver:"+strconv.FormatInt(standing.ID, 10)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.StartOver(ctx, standing.ID, key, &SessionSpec{Model: huge.ModelRef, Route: "profile"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.StartOver(ctx, standing.ID, proto.StartOver{RequestID: "s1"}); err != nil {
		t.Fatal(err)
	}
	if row, err := h.store.Session(ctx, standing.ID); err != nil || row.Model != "faux-huge" {
		t.Fatalf("the recorded model %+v, %v", row, err)
	}
}

// createStatus creates a session and says the status it answered with.
func (h *harness) createStatus(request proto.CreateSession) (proto.Session, int) {
	h.t.Helper()
	body := `{"requestId":"` + request.RequestID + `","profile":"` + request.Profile + `"}`
	response, _ := h.raw(http.MethodPost, proto.PathSessions, body, map[string]string{
		proto.HeaderProtocol: strconv.Itoa(proto.Version), "Authorization": "Bearer " + h.token, "Content-Type": "application/json"})
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		h.t.Fatalf("create: %d", response.StatusCode)
	}
	session, err := h.client.CreateSession(context.Background(), request)
	if err != nil {
		h.t.Fatal(err)
	}
	return session, response.StatusCode
}
