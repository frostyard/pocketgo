package pocketdtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// The night shift (ADR-0030, ADR-0033, ADR-0034): the board's proposals,
// the owner's drops and launches of them, and the schedules list. The fake
// has no supervisors' boards: every proposal is on one board, and a launch
// starts no thread.

// AddProposal adds a proposal, or replaces the one with its id, without an
// event.
func (s *Server) AddProposal(p proto.Proposal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := slices.IndexFunc(s.proposals, func(x proto.Proposal) bool { return x.ID == p.ID }); i >= 0 {
		s.proposals[i] = p
		return
	}
	s.proposals = append(s.proposals, p)
}

// Proposal returns the server's copy of proposal id.
func (s *Server) Proposal(id int64) (proto.Proposal, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := slices.IndexFunc(s.proposals, func(x proto.Proposal) bool { return x.ID == id }); i >= 0 {
		return s.proposals[i], true
	}
	return proto.Proposal{}, false
}

// SetSchedules replaces what GET /v1/schedules answers, without an event:
// emit an EventSchedules to tell the clients.
func (s *Server) SetSchedules(list proto.Schedules) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list.Schedules = slices.Clone(list.Schedules)
	s.schedules = list
}

// LaunchRequests are the bodies of the POST /v1/launches requests the
// server handled, in order, retries included; a request FailOnce failed
// is not among them (Requests has it).
func (s *Server) LaunchRequests() []proto.CreateLaunch {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.launchRequests)
}

// Launches are the launches the server made, oldest first.
func (s *Server) Launches() []proto.Launch {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.launches)
}

func (s *Server) nightShiftRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+proto.PathProposals, func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		list := []proto.Proposal{}
		for _, p := range s.proposals {
			if p.State != proto.ProposalDropped {
				list = append(list, p)
			}
		}
		s.mu.Unlock()
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].Order != list[j].Order {
				return list[i].Order < list[j].Order
			}
			return list[i].ID < list[j].ID
		})
		writeJSON(w, http.StatusOK, list)
	})
	mux.HandleFunc("POST "+proto.PathDropProposal, s.dropProposal)
	mux.HandleFunc("GET "+proto.PathSchedules, func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		list := s.schedules
		list.Schedules = slices.Clone(list.Schedules)
		s.mu.Unlock()
		if list.Schedules == nil {
			list.Schedules = []proto.Schedule{}
		}
		if list.Zone == "" {
			list.Zone = "UTC"
		}
		writeJSON(w, http.StatusOK, list)
	})
	mux.HandleFunc("GET "+proto.PathLaunches, func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		list := slices.Clone(s.launches)
		s.mu.Unlock()
		if list == nil {
			list = []proto.Launch{}
		}
		slices.Reverse(list)
		writeJSON(w, http.StatusOK, list)
	})
	mux.HandleFunc("POST "+proto.PathLaunches, s.createLaunch)
}

// dropProposal drops a proposed proposal at the rev the owner saw, as
// pocketd does (protocol rule 59): a stale rev, or one not proposed, is
// conflict; none is not found. The same requestId again answers as it did.
func (s *Server) dropProposal(w http.ResponseWriter, r *http.Request) {
	var request proto.DropProposal
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.RequestID == "" {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "bad body: requestId is required")
		return
	}
	if s.replay(w, r, request.RequestID) {
		return
	}
	id := pathID(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.proposals, func(p proto.Proposal) bool { return p.ID == id })
	if i < 0 {
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such proposal")
		return
	}
	p := s.proposals[i]
	switch {
	case p.State != proto.ProposalProposed:
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, fmt.Sprintf("proposal #%d is %s: only a proposed one is dropped", id, p.State))
		return
	case p.Rev != request.Rev:
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, fmt.Sprintf("proposal #%d is at rev %d now: look at it again", id, p.Rev))
		return
	}
	p.State, p.UpdatedAt = proto.ProposalDropped, movedOn(p.UpdatedAt)
	s.proposals[i] = p
	s.emitLocked(proto.Event{Type: proto.EventProposal, Proposal: &p})
	s.rememberLocked(r, request.RequestID, p)
	writeJSON(w, http.StatusOK, p)
}

// createLaunch launches proposed proposals for the owner, as pocketd does
// (protocol rule 59): 1 to 20 distinct proposals, each proposed, and an
// end after Now and at most 24 hours ahead. Each becomes launched with its
// event, then the launch's event goes out. The same requestId with the
// same proposals and end answers the launch it made, and changes nothing;
// with others it is conflict.
func (s *Server) createLaunch(w http.ResponseWriter, r *http.Request) {
	var request proto.CreateLaunch
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.RequestID == "" {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "bad body: requestId is required")
		return
	}
	op := r.Method + " " + r.URL.Path
	parts := make([]string, len(request.Proposals))
	for i, id := range request.Proposals {
		parts[i] = strconv.FormatInt(id, 10)
	}
	match := strings.Join(parts, ",") + " " + request.End.UTC().Format(time.RFC3339Nano)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.launchRequests = append(s.launchRequests, request)
	if prior, ok := s.replies[request.RequestID]; ok {
		if prior.op != op || prior.match != match {
			writeError(w, http.StatusConflict, proto.ErrorCodeConflict, "the requestId was used for a different request")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(prior.body)
		return
	}
	switch {
	case len(request.Proposals) < 1 || len(request.Proposals) > 20:
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "a launch names 1 to 20 proposals")
		return
	case !request.End.After(Now) || request.End.After(Now.Add(24*time.Hour)):
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "the end must be after now and at most 24 hours ahead")
		return
	}
	var picked []int
	for _, id := range request.Proposals {
		i := slices.IndexFunc(s.proposals, func(p proto.Proposal) bool { return p.ID == id })
		switch {
		case i < 0 || slices.Contains(picked, i):
			writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, fmt.Sprintf("no proposal #%d, or named twice", id))
			return
		case s.proposals[i].State != proto.ProposalProposed:
			writeError(w, http.StatusConflict, proto.ErrorCodeConflict, fmt.Sprintf("proposal #%d is %s", id, s.proposals[i].State))
			return
		}
		picked = append(picked, i)
	}
	launch := proto.Launch{ID: int64(len(s.launches) + 1), End: request.End.UTC(), State: proto.LaunchRunning, By: proto.LaunchByOwner,
		Items: []proto.LaunchItem{}, CreatedAt: Now}
	for _, i := range picked {
		p := s.proposals[i]
		p.State, p.Launch, p.UpdatedAt = proto.ProposalLaunched, launch.ID, movedOn(p.UpdatedAt)
		s.proposals[i] = p
		s.emitLocked(proto.Event{Type: proto.EventProposal, Proposal: &p})
		launch.Items = append(launch.Items, proto.LaunchItem{Proposal: p.ID, Agent: p.Agent, Title: p.Title, State: proto.LaunchItemWaiting})
	}
	s.emitLocked(proto.Event{Type: proto.EventLaunch, Launch: &launch})
	body, _ := json.Marshal(launch)
	s.replies[request.RequestID] = reply{op: op, body: append(body, '\n'), match: match}
	writeJSON(w, http.StatusCreated, launch)
}
