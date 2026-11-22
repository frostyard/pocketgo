package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// renameSession serves POST /v1/sessions/{id}/title (rule 68, ADR-0058):
// the owner renames a private agent's session. team.CheckRename decides,
// from the session's record and its profile as loaded now; the title is
// checked as a new session's (rule 9), trimmed, and an empty one gives the
// default. The work database writes the title with the request's record in
// one transaction, and the session's updated_at stays: a rename is not
// activity. The session is published after the commit. A retry answers
// the session as it is now, renames nothing again, and publishes it again,
// since a stop after the commit may have kept its event from going.
func (s *Server) renameSession(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var request proto.RenameSession
	if !decode(w, r, &request) || !validRequestID(w, request.RequestID) {
		return
	}
	if len(request.Title) > proto.MaxTitle {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, fmt.Sprintf("title must be at most %d bytes", proto.MaxTitle))
		return
	}
	key := requestKey(r, request.RequestID)
	defer s.keys.lock(key)()
	kind := renameKind(id, request.Title)
	// The rename completes even if the client goes away: its answer is
	// read after the commit, and a retry answers it.
	ctx := context.WithoutCancel(r.Context())
	_, existed, err := s.store.LookupRequest(ctx, key, kind)
	switch {
	case errors.Is(err, store.ErrRequestReused):
		reused(w)
		return
	case err != nil:
		s.internal(w, "read request", err)
		return
	case existed:
		s.publishSession(id, true)
		s.writeSession(w, r, id, http.StatusOK)
		return
	}
	row, err := s.store.Session(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, fmt.Sprintf("no session %d", id))
		return
	} else if err != nil {
		s.internal(w, "read session", err)
		return
	}
	facts, err := s.renameFacts(row, s.findProfile())
	switch {
	case errors.Is(err, profiles.ErrFolderChanged):
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, err.Error())
		return
	case err != nil:
		s.internal(w, "read the session's profile", err)
		return
	}
	if err := team.CheckRename(facts); err != nil {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, err.Error())
		return
	}
	title := strings.TrimSpace(request.Title)
	if title == "" {
		title = proto.DefaultTitle(id)
	}
	switch _, err := s.store.RenameSession(ctx, key, kind, id, title); {
	case errors.Is(err, store.ErrRequestReused):
		reused(w)
		return
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, fmt.Sprintf("no session %d", id))
		return
	case err != nil:
		s.internal(w, "rename the session", err)
		return
	}
	s.publishSession(id, true)
	s.writeSession(w, r, id, http.StatusOK)
}

// renameKind is the request record's kind of a rename: its session and
// its title as sent, so the same id with another title, or for another
// session, is another request (rule 5).
func renameKind(id int64, title string) string {
	return "title:" + strconv.FormatInt(id, 10) + ":" + title
}

// renameFacts are what team.CheckRename decides row's rename from: its
// profile read with find, as it is now. A profile pocketd cannot read now
// is an error, and so is a profile folder that is no longer the one it
// protects (profiles.ErrFolderChanged); a profile that is gone or invalid
// is none.
func (s *Server) renameFacts(row store.SessionRow, find func(string) (profiles.Profile, error)) (team.RenameFacts, error) {
	f := team.RenameFacts{Session: row.ID, Profile: row.Profile, Team: row.CreatedBy == teamCreator || row.CreatedBy == gatherCreator}
	if row.Profile == "" || f.Team {
		return f, nil
	}
	if find == nil {
		f.NoProfiles = true
		return f, nil
	}
	p, err := find(row.Profile)
	switch {
	case errors.Is(err, profiles.ErrFolderChanged), profiles.Unreadable(err):
		return f, fmt.Errorf("profile %s: %w", row.Profile, err)
	case err == nil:
		f.Now = &p
	}
	return f, nil
}

// renamable says whether the owner may rename row's session now, its
// profile read with find (Session.Renamable): a profile that reads badly
// says it may not, and the rename itself reads it again.
func (s *Server) renamable(row store.SessionRow, find func(string) (profiles.Profile, error)) bool {
	f, err := s.renameFacts(row, find)
	return err == nil && team.CheckRename(f) == nil
}

// findProfile reads one profile as it is now, nil while pocketd runs
// without profiles.
func (s *Server) findProfile() func(string) (profiles.Profile, error) {
	if s.profiles == nil {
		return nil
	}
	return s.profiles.Find
}

// errNoSuchProfile is a profile a folder's load did not list as valid: it
// is gone or invalid, as Find's error for it would say.
var errNoSuchProfile = errors.New("no valid profile of that id")

// loadedProfiles reads the profile folder once, at the first profile
// asked for, and finds each profile in that read: a list of sessions reads
// the folder once for all of them. nil while pocketd runs without
// profiles.
func (s *Server) loadedProfiles() func(string) (profiles.Profile, error) {
	if s.profiles == nil {
		return nil
	}
	var (
		read    bool
		byID    map[string]profiles.Profile
		readErr error
	)
	return func(id string) (profiles.Profile, error) {
		if !read {
			read = true
			var list []profiles.Profile
			list, _, readErr = s.profiles.Load()
			byID = make(map[string]profiles.Profile, len(list))
			for _, p := range list {
				byID[p.ID] = p
			}
		}
		if readErr != nil {
			return profiles.Profile{}, readErr
		}
		p, ok := byID[id]
		if !ok {
			return profiles.Profile{}, errNoSuchProfile
		}
		return p, nil
	}
}
