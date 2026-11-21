package pocketdtest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
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

// A launch over the round budget is refused and changes nothing, as
// pocketd's check does (Codex review of #58, round 4).
func TestAFakeLaunchOverTheBudgetIsRefused(t *testing.T) {
	s := New()
	for id := int64(1); id <= 3; id++ {
		s.AddProposal(proto.Proposal{ID: id, Agent: "vera", Title: "Fix it", MaxRounds: 100, Order: int(id), Rev: 1, State: proto.ProposalProposed})
	}
	client, err := proto.NewClient(s.URL, Token, &http.Client{Transport: s.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	var e *proto.Error
	if _, err := client.Launch(context.Background(), proto.CreateLaunch{RequestID: "l1", Proposals: []int64{1, 2, 3}, End: Now.Add(time.Hour)}); !errors.As(err, &e) || e.Status != http.StatusBadRequest {
		t.Fatalf("a launch over the budget: %v", err)
	}
	if p, _ := s.Proposal(1); p.State != proto.ProposalProposed {
		t.Fatalf("#1 after the refusal: %+v", p)
	}
}

// A drop moves the proposal's rev on, as pocketd's does, and its replay
// answers the same; a launch's replay answers created, as its first answer
// did (Codex review of #58, round 5).
func TestAFakeDropAndLaunchReplayAsPocketdDoes(t *testing.T) {
	s := New()
	for _, id := range []int64{1, 2} {
		s.AddProposal(proto.Proposal{ID: id, Agent: "vera", Title: "Fix it", MaxRounds: 5, Order: int(id), Rev: 1, State: proto.ProposalProposed})
	}
	post := func(path string, body any) (int, []byte) {
		t.Helper()
		data, _ := json.Marshal(body)
		r, err := http.NewRequest(http.MethodPost, s.URL+path, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+Token)
		r.Header.Set(proto.HeaderProtocol, strconv.Itoa(proto.Version))
		r.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Transport: s.Transport()}).Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, out
	}
	drop := proto.DropProposal{RequestID: "d1", Rev: 1}
	status, first := post(proto.Path(proto.PathDropProposal, 1), drop)
	var p proto.Proposal
	if err := json.Unmarshal(first, &p); status != http.StatusOK || err != nil || p.Rev != 2 || p.State != proto.ProposalDropped {
		t.Fatalf("the drop: %d %s", status, first)
	}
	if held, _ := s.Proposal(1); held.Rev != 2 {
		t.Errorf("#1 after the drop: %+v", held)
	}
	if status, again := post(proto.Path(proto.PathDropProposal, 1), drop); status != http.StatusOK || !bytes.Equal(again, first) {
		t.Errorf("the drop again: %d %s, want %s", status, again, first)
	}
	launch := proto.CreateLaunch{RequestID: "l1", Proposals: []int64{2}, End: Now.Add(time.Hour)}
	for i := range 2 {
		if status, body := post(proto.PathLaunches, launch); status != http.StatusCreated {
			t.Errorf("launch %d: %d %s, want created", i+1, status, body)
		}
	}
}

// The fake refuses, as pocketd's decode does, a launch or a drop with an
// unknown field, data after its object or a request id over
// proto.MaxRequestIDSize, and changes nothing (Codex review of #58, round
// 6).
func TestTheFakeRefusesWhatPocketdRefuses(t *testing.T) {
	s := New()
	s.AddProposal(proto.Proposal{ID: 1, Agent: "vera", Title: "Fix it", MaxRounds: 5, Order: 1, Rev: 1, State: proto.ProposalProposed})
	post := func(path, body string) int {
		t.Helper()
		r, err := http.NewRequest(http.MethodPost, s.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+Token)
		r.Header.Set(proto.HeaderProtocol, strconv.Itoa(proto.Version))
		r.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Transport: s.Transport()}).Do(r)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	end := Now.Add(time.Hour).Format(time.RFC3339)
	long := strings.Repeat("x", proto.MaxRequestIDSize+1)
	for _, c := range []struct{ path, body string }{
		{proto.PathLaunches, `{"requestId":"l1","proposals":[1],"end":"` + end + `","extra":1}`},
		{proto.PathLaunches, `{"requestId":"l2","proposals":[1],"end":"` + end + `"} {}`},
		{proto.PathLaunches, `{"requestId":"` + long + `","proposals":[1],"end":"` + end + `"}`},
		{proto.Path(proto.PathDropProposal, 1), `{"requestId":"d1","rev":1,"extra":1}`},
		{proto.Path(proto.PathDropProposal, 1), `{"requestId":"d2","rev":1} {}`},
		{proto.Path(proto.PathDropProposal, 1), `{"requestId":"` + long + `","rev":1}`},
	} {
		if got := post(c.path, c.body); got != http.StatusBadRequest {
			t.Errorf("%s %s: %d, want 400", c.path, c.body[:20], got)
		}
	}
	if p, _ := s.Proposal(1); p.State != proto.ProposalProposed || p.Rev != 1 || len(s.Launches()) != 0 {
		t.Errorf("a refused request changed the board: %+v, launches %d", p, len(s.Launches()))
	}
}
