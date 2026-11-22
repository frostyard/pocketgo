package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/frostyard/pocketgo/service/internal/team"
)

// StartMCP connects the MCP servers the profiles name (ADR-0063), then
// gives every session that takes its profile's servers their tools as the
// servers listed them: a pass that serving does not wait for, and that a
// server that is down holds up no longer than its timeout. A server that
// is down keeps the tools it listed last, whose calls say it is down.
func (s *Server) StartMCP() {
	if len(s.engine.MCPProfiles()) == 0 {
		return
	}
	s.sessionMu.Lock()
	if s.stopped {
		s.sessionMu.Unlock()
		return
	}
	s.passes.Add(1)
	s.sessionMu.Unlock()
	go func() {
		defer s.passes.Done()
		s.engine.ConnectMCP(s.passCtx)
		if err := s.upgradeMCPTools(s.passCtx); err != nil {
			s.log.Warn("give sessions their profiles' MCP tools", "err", err)
		}
	}()
}

// upgradeMCPTools gives each session of a profile with an MCP server, that
// takes them (team.MayUseMCP), the tools of its profile's servers as
// pocketd holds them now: a session made before them, or before a server
// listed new ones, and one whose profile lost a server since loses its
// tools. A standing session takes its tools at its refresh, when idle and
// under refreshMu (ADR-0026), which this pass asks for. A session it
// cannot read or change is an error, and the others go on.
func (s *Server) upgradeMCPTools(ctx context.Context) error {
	with := map[string]bool{}
	for _, p := range s.engine.MCPProfiles() {
		with[p] = true
	}
	if len(with) == 0 {
		return nil
	}
	rows, err := s.store.Sessions(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, row := range rows {
		if ctx.Err() != nil {
			return errors.Join(append(failures, ctx.Err())...)
		}
		if !with[row.Profile] {
			continue
		}
		if row.Standing {
			if err := s.refreshStanding(ctx, row.ID); err != nil && !errors.Is(err, errStandingDeferred) {
				failures = append(failures, fmt.Errorf("session %d: %w", row.ID, err))
			}
			continue
		}
		role, err := s.sessionRole(ctx, row.ID)
		if err != nil {
			failures = append(failures, fmt.Errorf("session %d: %w", row.ID, err))
			continue
		}
		if !team.MayUseMCP(role) {
			continue
		}
		if changed, err := s.engine.SetMCPTools(ctx, row.ID, row.Profile); err != nil {
			failures = append(failures, fmt.Errorf("session %d: %w", row.ID, err))
		} else if changed {
			s.log.Info("gave a session its profile's MCP tools", "session", row.ID, "profile", row.Profile)
		}
	}
	return errors.Join(failures...)
}
