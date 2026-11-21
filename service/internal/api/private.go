package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// TeamAllows refuses every team tool's call from session, before the tool
// runs, when the session's profile is private as it is now
// (team.CheckTeamCaller, ADR-0042): a private agent's session gets no team
// tool, but one made before, or before its profile became private, may
// still list one. A session from no profile, or whose profile is gone or
// invalid, is left to each tool's own checks; a profile pocketd cannot
// read now refuses the call, which a later call tries again.
func (s *Server) TeamAllows(ctx context.Context, session int64) error {
	row, err := s.store.Session(ctx, session)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	if row.Profile == "" || s.profiles == nil {
		return nil
	}
	p, err := s.profiles.Find(row.Profile)
	if profiles.Unreadable(err) {
		return fmt.Errorf("this session's profile %s: %w", row.Profile, err)
	} else if err != nil {
		return nil
	}
	return team.CheckTeamCaller(&p)
}

// privateAgent says whether agent's profile is private now (ADR-0042): its
// sessions keep no lessons section. A profile that is gone or invalid is
// not; one pocketd cannot read now is an error, and the refresh that asked
// tries again.
func (s *Server) privateAgent(agent string) (bool, error) {
	if s.profiles == nil {
		return false, nil
	}
	p, err := s.profiles.Find(agent)
	if profiles.Unreadable(err) {
		return false, fmt.Errorf("profile %s: %w", agent, err)
	}
	return err == nil && p.Private, nil
}
