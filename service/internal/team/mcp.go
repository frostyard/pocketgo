package team

// MayUseMCP says whether a session of a profile, working as role, takes
// its profile's MCP servers' tools (ADR-0063): a session the owner talks
// in, or a thread's specialist that builds. A cross-review, a consult, a
// gather's agent and a plan's lead read and change nothing, and an MCP
// tool may change what a server holds (send a mail, write a note), so
// they take none; nor does a thread's worker, which works for its
// specialist with the coding tools alone (ADR-0021). Only the profile's
// own servers are offered: pocketd holds them in the profile's own
// extension.
func MayUseMCP(role SessionRole) bool {
	return !role.Review && !role.Worker && !role.Consult && !role.Gather && !role.Plan
}
