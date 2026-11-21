package pocketdtest

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	// The launches that run and those that ended in the last 7 days by
	// the fake's clock, the newest first, as pocketd lists them (Codex
	// review of #58, round 7).
	mux.HandleFunc("GET "+proto.PathLaunches, func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		list := []proto.Launch{}
		for _, l := range s.launches {
			if l.State == proto.LaunchRunning || (l.EndedAt != nil && !l.EndedAt.Before(Now.Add(-LaunchesShown))) {
				list = append(list, l)
			}
		}
		s.mu.Unlock()
		slices.SortFunc(list, func(a, b proto.Launch) int { return cmp.Compare(b.ID, a.ID) })
		writeJSON(w, http.StatusOK, list)
	})
	mux.HandleFunc("POST "+proto.PathLaunches, s.createLaunch)
}

// decodeStrict reads r's body into v as pocketd's decode does: one JSON
// object, no unknown field and nothing after it, and its request id, *id,
// 1 to proto.MaxRequestIDSize bytes. Else it answers 400 and is false
// (Codex review of #58, round 6).
func decodeStrict(w http.ResponseWriter, r *http.Request, v any, id *string) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "body: "+err.Error())
		return false
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "body: data after the JSON object")
		return false
	}
	if *id == "" || len(*id) > proto.MaxRequestIDSize {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, fmt.Sprintf("requestId must be 1 to %d bytes", proto.MaxRequestIDSize))
		return false
	}
	return true
}

// LaunchesShown is how long an ended launch stays listed, as pocketd's
// launchesShown.
const LaunchesShown = 7 * 24 * time.Hour

// LaunchRounds is a launch's round budget, as pocketd's team.LaunchRounds.
const LaunchRounds = 200

// dropProposal drops a proposed proposal at the rev the owner saw, as
// pocketd does (protocol rule 59): a stale rev, or one not proposed, is
// conflict; none is not found. The same requestId again answers as it did.
func (s *Server) dropProposal(w http.ResponseWriter, r *http.Request) {
	// A proposal id is a positive number, checked first, as pocketd's
	// pathIDOf does (Codex review of #58, round 7).
	if id, err := strconv.ParseInt(r.PathValue("id"), 10, 64); err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "the path's id is not a positive number")
		return
	}
	var request proto.DropProposal
	if !decodeStrict(w, r, &request, &request.RequestID) {
		return
	}
	// The rev is part of the drop, as pocketd's request kind has it: the
	// same id at another rev is another request (Codex review of #58,
	// round 3).
	op := r.Method + " " + r.URL.Path + "@" + strconv.Itoa(request.Rev)
	id := pathID(r)
	// The receipt is checked, the row changed and the reply kept under one
	// lock, as pocketd's transaction does (Codex review of #58, round 4).
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.replayOpLocked(w, op, request.RequestID) {
		return
	}
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
	// A drop moves the rev on, as pocketd's change does (Codex review of
	// #58, round 5).
	p.State, p.Rev, p.UpdatedAt = proto.ProposalDropped, p.Rev+1, movedOn(p.UpdatedAt)
	s.proposals[i] = p
	s.emitLocked(proto.Event{Type: proto.EventProposal, Proposal: &p})
	s.rememberOpLocked(op, request.RequestID, p)
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
	if !decodeStrict(w, r, &request, &request.RequestID) {
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
		// The replay answers as the launch did, created (Codex review of
		// #58, round 5).
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
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
			// A rule the launch breaks as asked, as pocketd's check says
			// (Codex review of #58, round 3).
			writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, fmt.Sprintf("#%d is %s: only a proposed one launches", id, s.proposals[i].State))
			return
		}
		picked = append(picked, i)
	}
	// The round budget, as pocketd's check has it (Codex review of #58,
	// round 4).
	rounds := 0
	for _, i := range picked {
		rounds += s.proposals[i].MaxRounds
	}
	if rounds > LaunchRounds {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, fmt.Sprintf("the proposals may take %d rounds together, more than a launch's %d", rounds, LaunchRounds))
		return
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
