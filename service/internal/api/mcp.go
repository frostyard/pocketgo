package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/team"
)

// StartMCP connects the MCP servers the profiles name (ADR-0063), then
// gives every session that takes its profile's servers their tools as the
// servers listed them: a pass that serving does not wait for, and that a
// server that is down holds up no longer than its timeout a request. A server that
// is down keeps the tools it listed last, whose calls say it is down, and
// pocketd tries it again later. The connections are made before the
// pass, and a new session's record waits for no pass (mcpAfterCreate): no
// session waits on a server.
func (s *Server) StartMCP() {
	if len(s.engine.MCPProfiles()) == 0 {
		return
	}
	s.mcpGo(func(ctx context.Context) {
		s.engine.ConnectMCP(ctx)
		s.mcpUpgrade(ctx, "", mcpRetryFirst)
	})
}

// MCPToolsChanged gives the sessions of profile its MCP servers' tools,
// which just changed (pig.Options.OnMCPTools), in a pass of its own that
// nothing waits for. It does not block.
func (s *Server) MCPToolsChanged(profile string) {
	s.mcpGo(func(ctx context.Context) { s.mcpUpgrade(ctx, profile, mcpRetryFirst) })
}

// The waits before a pass that failed is tried again: mcpRetryFirst,
// doubled each time, up to mcpRetryMost.
const (
	mcpRetryFirst = time.Minute
	mcpRetryMost  = 30 * time.Minute
)

// mcpGo runs f in the background unless the server stopped; Stop waits
// for it and ends its context.
func (s *Server) mcpGo(f func(context.Context)) {
	s.sessionMu.Lock()
	if s.stopped {
		s.sessionMu.Unlock()
		return
	}
	s.passes.Add(1)
	s.sessionMu.Unlock()
	go func() {
		defer s.passes.Done()
		f(s.passCtx)
	}()
}

// mcpUpgrade runs a pass (upgradeMCPTools) under mcpMu, numbered as it
// begins, before it reads the sessions. A pass that failed for a session
// is tried again after wait (mcpRetryLater); one that succeeds ends the
// retry its scope waits on.
func (s *Server) mcpUpgrade(ctx context.Context, only string, wait time.Duration) {
	s.mcpMu.Lock()
	pass := s.mcpPassNo.Add(1)
	err := s.upgradeMCPTools(ctx, only)
	s.mcpMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		s.log.Warn("give sessions their profiles' MCP tools; tried again later", "err", err, "in", wait)
		s.mcpRetryLater(only, wait, pass)
		return
	}
	// A pass ends only the retry of failures before it: one that failed
	// after it, between its unlock and here, keeps its retry.
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	if r, ok := s.mcpRetries[only]; ok && r.pass < pass {
		r.stop()
		delete(s.mcpRetries, only)
	}
}

// mcpRetryLater runs a pass for only's sessions (every profile's for "")
// after wait, then after twice the wait each time it fails, up to
// mcpRetryMost, until it succeeds: no new listing may come to start
// another. A scope waits on one retry at most; a retry that fired lets go
// of its place.
func (s *Server) mcpRetryLater(only string, wait time.Duration, pass uint64) {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	if s.stopped {
		return
	}
	if r, ok := s.mcpRetries[only]; ok {
		// The retry that waits covers this failure too: it is a failure
		// of pass, which no earlier pass's success ends.
		r.pass = max(r.pass, pass)
		s.mcpRetries[only] = r
		return
	}
	if s.mcpRetries == nil {
		s.mcpRetries = map[string]mcpRetry{}
	}
	// Each retry knows itself: one that fires after it was stopped, and
	// another took its place, leaves the other's place and runs nothing.
	s.mcpRetryNo++
	id := s.mcpRetryNo
	s.mcpRetries[only] = mcpRetry{id: id, pass: pass, stop: s.mcpTimer(wait, func() {
		s.sessionMu.Lock()
		if r, ok := s.mcpRetries[only]; !ok || r.id != id {
			s.sessionMu.Unlock()
			return
		}
		delete(s.mcpRetries, only)
		s.sessionMu.Unlock()
		s.mcpGo(func(ctx context.Context) { s.mcpUpgrade(ctx, only, min(2*wait, mcpRetryMost)) })
	})}
}

// mcpRetry is a pass that waits to be tried again: pass is the newest
// failure it covers, by the number of the pass or record it failed in
// (mcpPassNo), and stop cancels it.
type mcpRetry struct {
	id   uint64
	pass uint64
	stop func()
}

// mcpAfterCreate gives session id, just recorded, its profile's MCP tools
// as they are now when it takes them (team.MayUseMCP), and takes any from
// one that does not. The engine made it with the tools of that moment; a
// listing between then and its record would reach it by no pass, since a
// pass reads only the sessions recorded before it. It writes under the
// session's own lock, as a pass does (mcpWrite), so the two never write
// its tools past one another, and each writes the tools pocketd holds as
// it writes; it waits for no pass, since its caller may hold the outbox
// and the work locks. Its number comes after the record: a pass numbered
// later read the session. A failure is logged: the next pass tries
// again.
func (s *Server) mcpAfterCreate(ctx context.Context, id int64, profile string, role team.SessionRole) {
	if profile == "" || !slices.Contains(s.engine.MCPProfiles(), profile) {
		return
	}
	scope := profile
	if !team.MayUseMCP(role) {
		profile = ""
	}
	pass := s.mcpPassNo.Add(1)
	_, err := s.mcpWrite(ctx, id, profile)
	if err != nil {
		// No other pass may come: its profile's pass is tried again.
		s.log.Warn("give a new session its profile's MCP tools; tried again later", "session", id, "err", err)
		s.mcpRetryLater(scope, mcpRetryFirst, pass)
	}
}

// mcpWriteLocks is how many locks the sessions' MCP tool writes share, by
// session number.
const mcpWriteLocks = 32

// mcpWrite gives session id profile's MCP tools as pocketd holds them now
// (Engine.SetMCPTools; "" for none), under the lock of its number: one
// session's tools are written one write at a time, and a write waits for
// no other session's but one that shares its lock.
func (s *Server) mcpWrite(ctx context.Context, id int64, profile string) (bool, error) {
	mu := &s.mcpWriteMu[uint64(id)%mcpWriteLocks]
	mu.Lock()
	defer mu.Unlock()
	return s.engine.SetMCPTools(ctx, id, profile)
}

// roleOfSpec is what a session made from spec works as.
func roleOfSpec(spec SessionSpec) team.SessionRole {
	return team.SessionRole{Thread: spec.Thread != 0, Review: spec.Review, Worker: spec.Worker, Consult: spec.Consult != 0,
		Gather: spec.Gather != 0, Plan: spec.Plan != 0}
}

// threadRole is what thread's specialist session works as, as the work
// database records the thread: a spec made again for a session an earlier
// run made says less.
func threadRole(thread store.ThreadRow) team.SessionRole {
	return team.SessionRole{Thread: true, Review: thread.ReviewPull != 0, Plan: thread.IsPlanThread()}
}

// upgradeMCPTools gives each session of a profile with an MCP server (only
// that profile's, when only names one), that takes them
// (team.MayUseMCP), the tools of its profile's servers as pocketd holds
// them now: a session made before them, or before a server listed new
// ones, and one whose profile lost a server since loses its tools. A
// session that takes none loses any it has. A standing session takes its
// tools at its refresh, when idle and under refreshMu (ADR-0026), which
// this pass asks for. A session it cannot read or change is an error, and
// the others go on. mcpMu is held.
func (s *Server) upgradeMCPTools(ctx context.Context, only string) error {
	with := map[string]bool{}
	for _, p := range s.engine.MCPProfiles() {
		if only == "" || p == only {
			with[p] = true
		}
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
		profile := row.Profile
		if !team.MayUseMCP(role) {
			profile = ""
		}
		if changed, err := s.mcpWrite(ctx, row.ID, profile); err != nil {
			failures = append(failures, fmt.Errorf("session %d: %w", row.ID, err))
		} else if changed {
			s.log.Info("gave a session its profile's MCP tools, or took them", "session", row.ID, "profile", row.Profile)
		}
	}
	return errors.Join(failures...)
}
