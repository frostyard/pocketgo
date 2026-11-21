package pocketdtest

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// A launch tried again with its request id, proposals and end answers the
// launch it made, and makes none, as pocketd does (protocol rule 59); the
// id with other proposals or another end is conflict. A drop at a stale
// rev is conflict.
func TestALaunchTriedAgainAnswersTheLaunchItMade(t *testing.T) {
	s := New()
	for _, id := range []int64{1, 2, 3} {
		s.AddProposal(proto.Proposal{ID: id, Agent: "vera", Title: "Fix it", MaxRounds: 5, Order: int(id), Rev: 1, State: proto.ProposalProposed})
	}
	client, err := proto.NewClient(s.URL, Token, &http.Client{Transport: s.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	end := Now.Add(4 * time.Hour)
	first := proto.CreateLaunch{RequestID: "l1", Proposals: []int64{2, 1}, End: end}
	made, err := client.Launch(ctx, first)
	if err != nil || made.ID != 1 || len(made.Items) != 2 || made.Items[0].Proposal != 2 {
		t.Fatalf("the launch: %+v, %v", made, err)
	}
	if p, _ := s.Proposal(1); p.State != proto.ProposalLaunched || p.Launch != 1 {
		t.Errorf("#1 after the launch: %+v", p)
	}
	again, err := client.Launch(ctx, first)
	if err != nil || again.ID != made.ID || len(s.Launches()) != 1 {
		t.Fatalf("the launch again: %+v, %v; launches %d", again, err, len(s.Launches()))
	}
	for _, other := range []proto.CreateLaunch{
		{RequestID: "l1", Proposals: []int64{3}, End: end},
		{RequestID: "l1", Proposals: []int64{2, 1}, End: end.Add(time.Minute)},
	} {
		var e *proto.Error
		if _, err := client.Launch(ctx, other); !errors.As(err, &e) || e.Status != http.StatusConflict {
			t.Errorf("%+v: %v, want conflict", other, err)
		}
	}
	var e *proto.Error
	if _, err := client.Launch(ctx, proto.CreateLaunch{RequestID: "l2", Proposals: []int64{1}, End: end}); !errors.As(err, &e) ||
		e.Status != http.StatusBadRequest {
		t.Errorf("a launched proposal launched again: %v, want a refusal", err)
	}
	if _, err := client.DropProposal(ctx, 3, proto.DropProposal{RequestID: "d1", Rev: 2}); !errors.As(err, &e) || e.Status != http.StatusConflict {
		t.Errorf("a drop at a stale rev: %v, want conflict", err)
	}
	if p, err := client.DropProposal(ctx, 3, proto.DropProposal{RequestID: "d2", Rev: 1}); err != nil || p.State != proto.ProposalDropped {
		t.Errorf("the drop: %+v, %v", p, err)
	}
	// The same request id at another rev is another drop: refused, as
	// pocketd does (Codex review of #58, round 3).
	if _, err := client.DropProposal(ctx, 3, proto.DropProposal{RequestID: "d2", Rev: 7}); !errors.As(err, &e) || e.Status != http.StatusConflict {
		t.Errorf("the same id at another rev: %v, want conflict", err)
	}
	if list, err := client.Proposals(ctx); err != nil || len(list) != 2 {
		t.Errorf("proposals not dropped: %+v, %v", list, err)
	}
}
