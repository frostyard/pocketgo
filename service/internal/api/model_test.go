package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/frostyard/pocketgo/proto"
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
	if _, found, err := h.store.StandingPick(ctx, standing.ID); err != nil || found {
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
