package pocketdtest

import (
	"context"
	"errors"
	"net/http"
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
