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
	"time"

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

// StartOver resets an idle session once a key, as the engine does.
func (f *fakeEngine) StartOver(_ context.Context, session int64, key string) error {
	f.mu.Lock()
	hook := f.standing.onStartOver
	f.mu.Unlock()
	if hook != nil {
		hook(key)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, done := f.standing.resets[key]; done {
		return nil
	}
	if f.runFirst != 0 || f.busy[session] {
		return ErrSessionBusy
	}
	if f.standing.resets == nil {
		f.standing.resets = map[string]int64{}
	}
	f.standing.resets[key] = session
	return nil
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
		select {
		case <-ended:
		case <-time.After(10 * time.Second):
			t.Fatal("the run's end was not followed")
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
	if err := h.engine.StartOver(ctx, standing.ID, done); err != nil {
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
