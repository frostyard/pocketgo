package pocketdtest

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
)

// A lesson decision run again with its request id answers with the lesson
// as it is now and decides nothing, as the service's store does: a change
// proposed since stays (Codex review of #18, round 3).
func TestALessonDecisionReplayAnswersTheLessonAsItIsNow(t *testing.T) {
	s := New()
	inForce := proto.Lesson{ID: 1, Agent: "vera", Text: "a", State: proto.LessonActive, Change: &proto.LessonChange{Text: "b"}}
	s.AddLesson(inForce)
	client, err := proto.NewClient(s.URL, Token, &http.Client{Transport: s.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	request := proto.DecideLesson{RequestID: "r1", Decision: proto.LessonApprove, Text: "b"}
	if l, err := client.DecideLesson(ctx, 1, request); err != nil || l.Text != "b" || l.Change != nil {
		t.Fatalf("approve: %+v, %v", l, err)
	}
	again := inForce
	again.Text, again.Change = "b", &proto.LessonChange{Text: "c"}
	s.AddLesson(again)
	l, err := client.DecideLesson(ctx, 1, request)
	if err != nil || l.Change == nil || l.Change.Text != "c" {
		t.Fatalf("the request again: %+v, %v", l, err)
	}
	if l, _ := client.Lessons(ctx); len(l) != 1 || l[0].Change == nil || l[0].Change.Text != "c" {
		t.Fatalf("the replay decided again: %+v", l)
	}
}

// A decision of the wrong shape is refused as the service refuses it,
// before anything is decided (Codex review of #18, round 5).
func TestALessonDecisionOfTheWrongShapeIsRefused(t *testing.T) {
	s := New()
	s.AddLesson(proto.Lesson{ID: 1, Agent: "vera", Text: "a", State: proto.LessonActive, Change: &proto.LessonChange{Forget: true}})
	client, err := proto.NewClient(s.URL, Token, &http.Client{Transport: s.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []proto.DecideLesson{
		{RequestID: "r1", Decision: proto.LessonDiscard, Text: "a"},
		{RequestID: "r2", Decision: proto.LessonApprove, Text: "a", Forget: true},
		{RequestID: "r3", Decision: proto.LessonApprove},
		{RequestID: "r4", Decision: proto.LessonForget, Forget: true},
		{RequestID: "r5", Decision: "keep"},
	} {
		_, err := client.DecideLesson(context.Background(), 1, d)
		var pe *proto.Error
		if !errors.As(err, &pe) || pe.Code != proto.ErrorCodeBadRequest {
			t.Errorf("%+v: %v", d, err)
		}
	}
	if l, _ := client.Lessons(context.Background()); len(l) != 1 || l[0].Change == nil {
		t.Fatalf("a refused decision decided: %+v", l)
	}
}

// A retried approval answers with the plan as it is now, as pocketd's does,
// not the first answer (Codex review of #25, round 7).
func TestARetriedApprovalAnswersThePlanAsItIs(t *testing.T) {
	s := New()
	rev := strings.Repeat("a", 64)
	s.AddPlan(proto.Plan{ID: 1, State: proto.PlanReview, Rev: rev, Graph: &proto.PlanGraph{Version: 1, Rev: rev}})
	client, err := proto.NewClient(URL, Token, &http.Client{Transport: s.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	request := proto.ApprovePlan{RequestID: "r1", Rev: rev, Graph: 1}
	if got, err := client.ApprovePlan(ctx, 1, request); err != nil || got.State != proto.PlanApproved {
		t.Fatalf("approve %+v %v", got, err)
	}
	building := proto.Plan{ID: 1, State: proto.PlanBuilding, Rev: rev, Graph: &proto.PlanGraph{Version: 1, Rev: rev, Approved: true}}
	s.AddPlan(building)
	if got, err := client.ApprovePlan(ctx, 1, request); err != nil || got.State != proto.PlanBuilding {
		t.Fatalf("a retry %+v %v", got, err)
	}
}

// An approval looks its request id up and claims it under one hold of the
// lock, as pocketd claims it in the approval's transaction: two requests
// of one id never both miss the record (Codex review of #25, round 8).
func TestAnApprovalClaimsItsRequestIDAtomically(t *testing.T) {
	s := New()
	rev := strings.Repeat("a", 64)
	s.AddPlan(proto.Plan{ID: 1, State: proto.PlanReview, Rev: rev, Graph: &proto.PlanGraph{Version: 1, Rev: rev}})
	gap := false
	s.afterReplayLookup = func() {
		// Held by the approval, the lock cannot be taken here.
		if s.mu.TryLock() {
			gap = true
			s.mu.Unlock()
		}
	}
	client, err := proto.NewClient(URL, Token, &http.Client{Transport: s.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ApprovePlan(context.Background(), 1, proto.ApprovePlan{RequestID: "r1", Rev: rev, Graph: 1}); err != nil {
		t.Fatal(err)
	}
	if gap {
		t.Fatal("the lock was let go between the request id's lookup and its claim")
	}
}
