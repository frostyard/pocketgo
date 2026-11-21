package api

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// two is a second model, with fewer thinking levels than faux-1's.
var two = proto.Model{ModelRef: proto.ModelRef{Provider: "faux", ID: "faux-2"}, Name: "Two", Reasoning: true, ThinkingLevels: []string{"off", "low", "high"}}

// The owner changes a session's model and its thinking level (rule 7e,
// ADR-0028): pocketd applies and records them before it answers, with the
// owner's route once the model changed. A new model keeps a level it
// takes, else runs at its default; a level alone keeps the model and its
// route. A retry answers the session as it is, and changes nothing again;
// the id reused with another body is refused.
func TestTheOwnerChangesASessionsModel(t *testing.T) {
	h, _, _ := standingHarness(t)
	h.engine.more = append(h.engine.more, two)
	ctx := context.Background()
	vera, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "v1", Profile: "vera", Thinking: "medium"})
	if err != nil || vera.Route != "profile" {
		t.Fatalf("create: %+v, %v", vera, err)
	}
	high, err := h.client.ChangeModel(ctx, vera.ID, proto.ChangeModel{RequestID: "c1", Thinking: "high"})
	if err != nil || high.Model != model || high.Thinking != "high" || high.Route != "profile" {
		t.Fatalf("a level alone: %+v, %v", high, err)
	}
	moved, err := h.client.ChangeModel(ctx, vera.ID, proto.ChangeModel{RequestID: "c2", Model: &two.ModelRef})
	if err != nil || moved.Model != two.ModelRef || moved.Thinking != "high" || moved.Route != "owner" {
		t.Fatalf("a new model that takes the level: %+v, %v", moved, err)
	}
	if _, err := h.client.ChangeModel(ctx, vera.ID, proto.ChangeModel{RequestID: "c3", Thinking: "low"}); err != nil {
		t.Fatal(err)
	}
	back, err := h.client.ChangeModel(ctx, vera.ID, proto.ChangeModel{RequestID: "c4", Model: &model})
	if err != nil || back.Model != model || back.Thinking != "low" {
		t.Fatalf("back to faux-1: %+v, %v", back, err)
	}
	medium, err := h.client.ChangeModel(ctx, vera.ID, proto.ChangeModel{RequestID: "c5", Thinking: "medium"})
	if err != nil || medium.Thinking != "medium" {
		t.Fatalf("medium: %+v, %v", medium, err)
	}
	lacks, err := h.client.ChangeModel(ctx, vera.ID, proto.ChangeModel{RequestID: "c6", Model: &two.ModelRef})
	if err != nil || lacks.Model != two.ModelRef || lacks.Thinking != "" {
		t.Fatalf("a new model that lacks the level: %+v, %v", lacks, err)
	}
	h.engine.mu.Lock()
	calls := len(h.engine.standing.setModels)
	last := h.engine.standing.setModels[calls-1]
	h.engine.mu.Unlock()
	if calls != 6 || last != (setModel{vera.ID, two.ModelRef, ""}) {
		t.Fatalf("the engine took %d changes, the last %+v", calls, last)
	}
	again, err := h.client.ChangeModel(ctx, vera.ID, proto.ChangeModel{RequestID: "c1", Thinking: "high"})
	if err != nil || again.Model != two.ModelRef || again.Thinking != "" {
		t.Fatalf("a retry answers the session as it is: %+v, %v", again, err)
	}
	h.engine.mu.Lock()
	calls = len(h.engine.standing.setModels)
	h.engine.mu.Unlock()
	if calls != 6 {
		t.Fatalf("a retry changed the session again: %d changes", calls)
	}
	_, err = h.client.ChangeModel(ctx, vera.ID, proto.ChangeModel{RequestID: "c1", Thinking: "low"})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if row, err := h.store.Session(ctx, vera.ID); err != nil || row.Model != "faux-2" || row.Thinking != "" || row.Route != "owner" {
		t.Fatalf("recorded %+v, %v", row, err)
	}
}

// A change that names nothing, a model pocketd does not run or a level
// the model does not take is refused, and changes nothing.
func TestAChangeOfModelIsChecked(t *testing.T) {
	h, _, _ := standingHarness(t)
	ctx := context.Background()
	vera, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "v1", Profile: "vera"})
	if err != nil {
		t.Fatal(err)
	}
	gone := proto.ModelRef{Provider: "faux", ID: "faux-gone"}
	for i, request := range []proto.ChangeModel{
		{},
		{Model: &gone},
		{Thinking: "xhigh"},
		{Model: &big.ModelRef, Thinking: "high"},
	} {
		request.RequestID = "c" + string(rune('a'+i))
		_, err := h.client.ChangeModel(ctx, vera.ID, request)
		asError(t, err, http.StatusBadRequest, proto.ErrorCodeBadRequest)
	}
	_, err = h.client.ChangeModel(ctx, 999, proto.ChangeModel{RequestID: "n", Thinking: "high"})
	asError(t, err, http.StatusNotFound, proto.ErrorCodeNotFound)
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if len(h.engine.standing.setModels) != 0 {
		t.Fatalf("a refused change reached the engine: %+v", h.engine.standing.setModels)
	}
}

// A worker's session and a cross-review's take no model from the owner,
// as they take no message: the review's model is of another family than
// its author's.
func TestAWorkerOrAReviewTakesNoModelFromTheOwner(t *testing.T) {
	wh, thread := workerHarness(t)
	worker := startedWorker(t, wh, thread)
	_, err := wh.client.ChangeModel(context.Background(), worker, proto.ChangeModel{RequestID: "w", Thinking: "high"})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)

	rh := newReviewHarness(t)
	_, err = rh.client.ChangeModel(context.Background(), rh.teg.Session, proto.ChangeModel{RequestID: "r", Thinking: "high"})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
}

// The owner's pick in a standing session holds in place of its profile's
// model through every refresh, as before a message and at a run's end,
// until Start Over, which goes back to the
// profile's model at its default level and drops the pick (ADR-0028). A
// pick whose window cannot hold the profile's compactAt is refused, since
// the profile could no longer be applied.
func TestTheOwnersPickHoldsInAStandingSessionUntilStartOver(t *testing.T) {
	h, _, _ := standingHarness(t)
	wide := proto.Model{ModelRef: proto.ModelRef{Provider: "faux", ID: "faux-wide"}, Name: "Wide", Reasoning: true,
		ThinkingLevels: []string{"off", "high"}, ContextWindow: 400_000}
	h.engine.more = append(h.engine.more, wide)
	ctx := context.Background()
	standing, _ := h.createStatus(proto.CreateSession{RequestID: "r1", Profile: "odrade"})
	_, err := h.client.ChangeModel(ctx, standing.ID, proto.ChangeModel{RequestID: "small", Model: &model})
	asError(t, err, http.StatusBadRequest, proto.ErrorCodeBadRequest)
	picked, err := h.client.ChangeModel(ctx, standing.ID, proto.ChangeModel{RequestID: "p1", Model: &wide.ModelRef, Thinking: "high"})
	if err != nil || picked.Model != wide.ModelRef || picked.Thinking != "high" || picked.Route != "owner" {
		t.Fatalf("the pick: %+v, %v", picked, err)
	}
	if err := h.server.refreshStanding(ctx, standing.ID); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	runs := h.engine.standing.runs[standing.ID]
	h.engine.mu.Unlock()
	if runs.model != wide.ModelRef || runs.thinking != "high" {
		t.Fatalf("after a refresh the session runs %+v, want the pick", runs)
	}
	if row, err := h.store.Session(ctx, standing.ID); err != nil || row.Model != "faux-wide" || row.Thinking != "high" || row.Route != "owner" {
		t.Fatalf("recorded %+v, %v", row, err)
	}
	if _, err := h.client.StartOver(ctx, standing.ID, proto.StartOver{RequestID: "s1"}); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	runs = h.engine.standing.runs[standing.ID]
	h.engine.mu.Unlock()
	if runs.model != big.ModelRef || runs.thinking != "" {
		t.Fatalf("after Start Over the session runs %+v, want its profile's model at its default level", runs)
	}
	if _, found, err := h.engine.StandingPick(ctx, standing.ID); err != nil || found {
		t.Fatalf("the pick outlived Start Over: %v %v", found, err)
	}
	row, err := h.store.Session(ctx, standing.ID)
	if err != nil || row.Model != "faux-big" || row.Thinking != "" || row.Route == "owner" {
		t.Fatalf("recorded after Start Over %+v, %v", row, err)
	}
	if err := h.server.refreshStanding(ctx, standing.ID); err != nil && !errors.Is(err, errStandingDeferred) {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	runs = h.engine.standing.runs[standing.ID]
	h.engine.mu.Unlock()
	if runs.model != big.ModelRef {
		t.Fatalf("a refresh after Start Over runs %+v, want the profile's model", runs)
	}
}

// runsAndRecorded is what the engine runs for session id and what the work
// database recorded.
func runsAndRecorded(t *testing.T, h *harness, id int64) (ModelChoice, ModelChoice) {
	t.Helper()
	runs, err := h.engine.SessionModel(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	row, err := h.store.Session(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return runs, ModelChoice{Model: proto.ModelRef{Provider: row.Provider, ID: row.Model}, Thinking: row.Thinking}
}

// Two changes of one session's model, with two request ids, never cross:
// the second waits for the first's record, so the work database records
// the model PiG runs. Here the first holds between its read of what the
// session runs and its record (Codex review of #48, round 1).
func TestTwoChangesOfOneSessionNeverCross(t *testing.T) {
	h, _, _ := standingHarness(t)
	h.engine.more = append(h.engine.more, two)
	ctx := context.Background()
	vera, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "v1", Profile: "vera"})
	if err != nil {
		t.Fatal(err)
	}
	applied, release := make(chan struct{}), make(chan struct{})
	var armed atomic.Bool
	h.engine.mu.Lock()
	h.engine.standing.afterSetModel = func(key string) {
		if key == requestKeyOf(h, "a") {
			armed.Store(true)
		}
	}
	h.engine.standing.afterSessionModel = func(int64) {
		if armed.CompareAndSwap(true, false) {
			close(applied)
			<-release
		}
	}
	h.engine.mu.Unlock()
	errs := make(chan error, 2)
	go func() {
		_, err := h.client.ChangeModel(ctx, vera.ID, proto.ChangeModel{RequestID: "a", Model: &two.ModelRef})
		errs <- err
	}()
	<-applied
	bDone := make(chan struct{})
	go func() {
		defer close(bDone)
		_, err := h.client.ChangeModel(ctx, vera.ID, proto.ChangeModel{RequestID: "b", Model: &model, Thinking: "low"})
		errs <- err
	}()
	// B waits on the session's lock, which A holds, or, without it, ends.
	waiting := func() bool {
		h.server.modelLocks.mu.Lock()
		defer h.server.modelLocks.mu.Unlock()
		l := h.server.modelLocks.held[strconv.FormatInt(vera.ID, 10)]
		return l != nil && l.users == 2
	}
	for !waiting() {
		select {
		case <-bDone:
		default:
			runtime.Gosched()
			continue
		}
		break
	}
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	runs, recorded := runsAndRecorded(t, h, vera.ID)
	if runs != recorded || runs != (ModelChoice{Model: model, Thinking: "low"}) {
		t.Fatalf("the engine runs %+v, the work database recorded %+v, want B's change in both", runs, recorded)
	}
}

// A try that stopped after PiG took its change, before its record, is
// finished by its retry, which changes nothing again: a later change made
// in between stays what the session runs, and what is recorded (Codex
// review of #48, round 1).
func TestARetryAfterTheChangeWentNeverUndoesALaterOne(t *testing.T) {
	h, _, _ := standingHarness(t)
	h.engine.more = append(h.engine.more, two)
	ctx := context.Background()
	vera, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "v1", Profile: "vera"})
	if err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	h.engine.standing.sessionModelErr = errors.New("pocketd stops")
	h.engine.mu.Unlock()
	_, err = h.client.ChangeModel(ctx, vera.ID, proto.ChangeModel{RequestID: "a", Model: &two.ModelRef})
	asError(t, err, http.StatusInternalServerError, proto.ErrorCodeInternal)
	h.engine.mu.Lock()
	h.engine.standing.sessionModelErr = nil
	h.engine.mu.Unlock()
	later := ModelChoice{Model: model, Thinking: "high"}
	if _, err := h.client.ChangeModel(ctx, vera.ID, proto.ChangeModel{RequestID: "b", Model: &model, Thinking: "high"}); err != nil {
		t.Fatal(err)
	}
	again, err := h.client.ChangeModel(ctx, vera.ID, proto.ChangeModel{RequestID: "a", Model: &two.ModelRef})
	if err != nil || again.Model != model || again.Thinking != "high" {
		t.Fatalf("the retry answers %+v, %v, want the session as the later change left it", again, err)
	}
	runs, recorded := runsAndRecorded(t, h, vera.ID)
	if runs != later || recorded != later {
		t.Fatalf("the engine runs %+v, the work database recorded %+v, want the later change %+v", runs, recorded, later)
	}
	if result, existed, err := h.store.LookupRequest(ctx, requestKeyOf(h, "a"), modelKind(vera.ID, proto.ChangeModel{Model: &two.ModelRef})); err != nil || !existed || result == "" {
		t.Fatalf("the retry's request: %q %v %v", result, existed, err)
	}

	// With nothing in between, the retry records the change it finds.
	h.engine.mu.Lock()
	h.engine.standing.sessionModelErr = errors.New("pocketd stops")
	h.engine.mu.Unlock()
	_, err = h.client.ChangeModel(ctx, vera.ID, proto.ChangeModel{RequestID: "c", Model: &two.ModelRef})
	asError(t, err, http.StatusInternalServerError, proto.ErrorCodeInternal)
	h.engine.mu.Lock()
	h.engine.standing.sessionModelErr = nil
	h.engine.mu.Unlock()
	if done, err := h.client.ChangeModel(ctx, vera.ID, proto.ChangeModel{RequestID: "c", Model: &two.ModelRef}); err != nil || done.Model != two.ModelRef || done.Route != "owner" {
		t.Fatalf("the retry with nothing in between: %+v, %v", done, err)
	}
	h.engine.mu.Lock()
	calls := len(h.engine.standing.setModels)
	h.engine.mu.Unlock()
	if calls != 3 {
		t.Fatalf("the engine took %d changes, want 3: each request once", calls)
	}
}

// A change that failed before PiG took it frees its request id, as rule 5
// says of a request that failed: another body with it is a new request
// (Codex review of #48, round 1).
func TestAChangeThatFailedBeforeItsCommitFreesItsRequestID(t *testing.T) {
	h, _, _ := standingHarness(t)
	h.engine.more = append(h.engine.more, two)
	ctx := context.Background()
	vera, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "v1", Profile: "vera"})
	if err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	h.engine.standing.setModelErr = errors.New("the conversation would not open")
	h.engine.mu.Unlock()
	_, err = h.client.ChangeModel(ctx, vera.ID, proto.ChangeModel{RequestID: "c1", Model: &two.ModelRef})
	asError(t, err, http.StatusInternalServerError, proto.ErrorCodeInternal)
	h.engine.mu.Lock()
	h.engine.standing.setModelErr = nil
	h.engine.mu.Unlock()
	if low, err := h.client.ChangeModel(ctx, vera.ID, proto.ChangeModel{RequestID: "c1", Thinking: "low"}); err != nil || low.Thinking != "low" || low.Model != model {
		t.Fatalf("the id with another body after a failure that changed nothing: %+v, %v", low, err)
	}
}

// A request's kind names each field apart, so two bodies whose fields
// join to the same text, as a model id with a slash in it, are two
// requests: the id reused with the second is refused (Codex review of
// #48, round 1).
func TestAModelChangesRequestIsKnownByEachField(t *testing.T) {
	h, _, _ := standingHarness(t)
	slashed := proto.ModelRef{Provider: "faux", ID: "x/y"}
	other := proto.ModelRef{Provider: "faux/x", ID: "y"}
	h.engine.more = append(h.engine.more, proto.Model{ModelRef: slashed, Name: "Slashed"}, proto.Model{ModelRef: other, Name: "Other"})
	ctx := context.Background()
	vera, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "v1", Profile: "vera"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.ChangeModel(ctx, vera.ID, proto.ChangeModel{RequestID: "c1", Model: &slashed}); err != nil {
		t.Fatal(err)
	}
	_, err = h.client.ChangeModel(ctx, vera.ID, proto.ChangeModel{RequestID: "c1", Model: &other})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if modelKind(1, proto.ChangeModel{Model: &slashed}) == modelKind(1, proto.ChangeModel{Model: &other}) {
		t.Fatal("two bodies share a kind")
	}
	for _, request := range []proto.ChangeModel{{Model: &slashed}, {Thinking: "high"}, {Model: &other, Thinking: "low"}} {
		if id, ok := modelKindSession(modelKind(42, request)); !ok || id != 42 {
			t.Fatalf("the session of %+v's kind: %d %v", request, id, ok)
		}
	}
}

// A Start Over that reset the session, then failed before it completed,
// is finished by its retry, which drops no pick the owner made since: the
// reset's commit dropped the one it replaced (Codex review of #48, round
// 1).
func TestARetriedStartOverKeepsAPickMadeSince(t *testing.T) {
	h, _, _ := standingHarness(t)
	wide := proto.Model{ModelRef: proto.ModelRef{Provider: "faux", ID: "faux-wide"}, Name: "Wide", Reasoning: true,
		ThinkingLevels: []string{"off", "high"}, ContextWindow: 400_000}
	h.engine.more = append(h.engine.more, wide)
	ctx := context.Background()
	standing, _ := h.createStatus(proto.CreateSession{RequestID: "r1", Profile: "odrade"})
	if _, err := h.client.ChangeModel(ctx, standing.ID, proto.ChangeModel{RequestID: "p1", Model: &wide.ModelRef}); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	h.engine.standing.sessionModelErr = errors.New("pocketd stops")
	h.engine.mu.Unlock()
	_, err := h.client.StartOver(ctx, standing.ID, proto.StartOver{RequestID: "s1"})
	asError(t, err, http.StatusInternalServerError, proto.ErrorCodeInternal)
	h.engine.mu.Lock()
	h.engine.standing.sessionModelErr = nil
	h.engine.mu.Unlock()
	if _, err := h.client.ChangeModel(ctx, standing.ID, proto.ChangeModel{RequestID: "p2", Model: &wide.ModelRef, Thinking: "high"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.StartOver(ctx, standing.ID, proto.StartOver{RequestID: "s1"}); err != nil {
		t.Fatal(err)
	}
	if err := h.server.refreshStanding(ctx, standing.ID); err != nil {
		t.Fatal(err)
	}
	want := ModelChoice{Model: wide.ModelRef, Thinking: "high"}
	if pick, found, err := h.engine.StandingPick(ctx, standing.ID); err != nil || !found || pick != want {
		t.Fatalf("the pick after the retried Start Over: %+v %v %v", pick, found, err)
	}
	if runs, recorded := runsAndRecorded(t, h, standing.ID); runs != want || recorded != want {
		t.Fatalf("the engine runs %+v, the work database recorded %+v, want the pick made since", runs, recorded)
	}
}

// A pick is checked against the standing session's profile as pocketd
// reads it: a profile it cannot read now refuses the change, which reaches
// no engine, rather than taking it unchecked (Codex review of #48, round
// 1).
func TestAPickIsNotTakenUncheckedWhenTheProfileCannotBeRead(t *testing.T) {
	h, dir, home := standingHarness(t)
	ctx := context.Background()
	standing, _ := h.createStatus(proto.CreateSession{RequestID: "r1", Profile: "odrade"})
	// A folder that is a file: the read fails, and may work later.
	h.server.SetProfiles(profiles.Folder{Dir: filepath.Join(dir, "odrade.md"), Home: home})
	if _, err := h.server.profiles.Find("odrade"); !profiles.Unreadable(err) {
		t.Fatalf("the read: %v, want one that may work later", err)
	}
	_, err := h.client.ChangeModel(ctx, standing.ID, proto.ChangeModel{RequestID: "small", Model: &model})
	asError(t, err, http.StatusInternalServerError, proto.ErrorCodeInternal)
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if len(h.engine.standing.setModels) != 0 {
		t.Fatalf("an unchecked pick reached the engine: %+v", h.engine.standing.setModels)
	}
}

// A pick's model and level are checked together each time the standing
// session takes it: a level its model no longer takes gives way to the
// model's default, as a new model's does, and the session records what it
// runs (Codex review of #48, round 1).
func TestAPicksLevelTheModelNoLongerTakesGivesWayToItsDefault(t *testing.T) {
	h, _, _ := standingHarness(t)
	wide := proto.Model{ModelRef: proto.ModelRef{Provider: "faux", ID: "faux-wide"}, Name: "Wide", Reasoning: true,
		ThinkingLevels: []string{"off", "high"}, ContextWindow: 400_000}
	h.engine.more = append(h.engine.more, wide)
	ctx := context.Background()
	standing, _ := h.createStatus(proto.CreateSession{RequestID: "r1", Profile: "odrade"})
	if _, err := h.client.ChangeModel(ctx, standing.ID, proto.ChangeModel{RequestID: "p1", Model: &wide.ModelRef, Thinking: "high"}); err != nil {
		t.Fatal(err)
	}
	// The provider no longer takes a level for the model.
	h.engine.mu.Lock()
	h.engine.more = []proto.Model{big, {ModelRef: wide.ModelRef, Name: "Wide", ContextWindow: 400_000}}
	h.engine.mu.Unlock()
	if err := h.server.refreshStanding(ctx, standing.ID); err != nil {
		t.Fatal(err)
	}
	want := ModelChoice{Model: wide.ModelRef}
	if runs, recorded := runsAndRecorded(t, h, standing.ID); runs != want || recorded != want {
		t.Fatalf("the engine runs %+v, the work database recorded %+v, want the pick's model at its default level", runs, recorded)
	}
	if _, err := h.client.Send(ctx, standing.ID, proto.SendMessage{RequestID: "m1", Text: "hello"}); err != nil {
		t.Fatalf("the owner's message: %v", err)
	}
}

// A cross-review keeps the author's model its reviewer was chosen by: the
// owner's change of the author's model to the reviewer's family while it
// works counts from the next review, and this review's verdict counts
// (Codex review of #48, round 1).
func TestAReviewKeepsTheAuthorsFamilyItWasChosenBy(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	if h.teg.ReviewAuthor != (proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}) {
		t.Fatalf("the review kept the author's model %+v", h.teg.ReviewAuthor)
	}
	gpt := proto.ModelRef{Provider: "openai-codex", ID: "gpt-6-astra"}
	if _, err := h.client.ChangeModel(ctx, h.vera.Session, proto.ChangeModel{RequestID: "m1", Model: &gpt}); err != nil {
		t.Fatal(err)
	}
	if answer, err := h.verdict(h.teg.Session, "v1", headA, team.VerdictReady); err != nil {
		t.Fatalf("the verdict after the author's change: %q, %v", answer, err)
	}
}
