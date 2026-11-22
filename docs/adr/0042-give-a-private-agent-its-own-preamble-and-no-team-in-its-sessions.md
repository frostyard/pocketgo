# 0042 — Give a private agent its own preamble, and no team in its sessions

- **Status:** Accepted; a private agent's own schedules, kept with `schedule`, no team tool and the one exception to decision 3, added by [ADR-0060](0060-give-a-private-agent-a-schedule-tool-of-its-own-checked-in-code.md)
- **Date:** 2026-11-21

## Context

**A private agent is not on the team.** A profile with `private: true` (today one, the owner's personal agent) works only in its home, in sessions the owner starts ([ADR-0013](0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md)), and takes no delegation (`team.CheckTarget`, [ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md)). Nothing else in pocketd told it apart (frostyard/pocketgo#81):
- `sessionInstructions` (`service/pig`) began its identity with "You are <Name>, the team's <role>".
- A supervisor's session got the roster, and every session from a profile got the lessons section ([ADR-0015](0015-keep-each-agents-lessons-approved-by-the-owner-in-code.md)), which `Engine.SetLessons` writes again at every refresh, and which tells the agent to keep lessons with `team_memory`.
- `roleTools` gave `team_memory` to every session started from a profile, whatever its `tools`; it worked for a private agent (`Server.TeamMemory`), and its lessons went to the shared Inbox. A private supervisor would have got the supervisor's tools.

The old Pi Pocket team gave a private agent a prompt line of its own ("you work for the owner directly, in sessions the owner starts; you have no supervisor and no desk") and blocked every team tool in its sessions with a hook. The owner asked for the same here (frostyard/pocketgo#81, filed 2026-11-21).

**Facts the design must keep:**
- pocketd reads the profile folder at each request: a profile may become private, or stop being private, while its sessions live.
- A standing session takes its profile's identity, roster, tools and model at each refresh and at Start Over, keeping its lessons section ([ADR-0026](0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md)). An ordinary session keeps the identity and tools it was made with; pocketd only rewrites the lessons section at its end.
- The rules are code ([ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md)): a line in the prompt that says "never call team tools" decides nothing.

## Decision

This decision supersedes, of [ADR-0015](0015-keep-each-agents-lessons-approved-by-the-owner-in-code.md), `team_memory` for every session started from a profile (decision 3) and the lessons section in every session of an agent (decision 5), for a private profile. The rest of ADR-0015 stands.

1. **A private agent's instructions are its own.** After pocketd's preamble, its identity starts with `team.PrivateIntro`: "You are <Name>, the owner's personal agent: you work for the owner directly, in sessions the owner starts. You have no supervisor, no team and no desk. Your profile follows." Then its profile. No roster and no lessons section follow, whatever the session's spec carries (`sessionInstructions`). A team profile's instructions stay exactly as they were.
2. **A private agent's sessions keep no lessons section.** A refresh reads the agent's profile then (`Server.privateAgent`), and for a private one writes no section (`Engine.SetLessons` with none), so a section already there goes. A standing session's refresh and its Start Over build its instructions from the profile as it is now, so they drop the section too (`Engine.Reconfigure`, `Engine.StartOver`). A profile pocketd cannot read now fails the refresh, which a later pass tries again; a profile that is gone or invalid keeps the section, as before.
3. **A private agent's sessions get no team tool,** `team_memory` included, whatever its role (`roleTools`): only its profile's coding tools, or every coding tool, always as an explicit list (`toolsOf`), so a standing session whose profile had a filter and became private with none gets every coding tool at its refresh and Start Over, which take a tool list only when it is one (cross-review of #84, round 1). A private supervisor's older session gets none of the supervisor's later tools at start (`Server.upgradeSupervisorTools`). A read-only role keeps its narrowing (`toolsOf`), though no private profile can take one: a review, a consult, a gather and a thread all check `team.CheckTarget`.
4. **Every team tool refuses a private agent's session, with one error, before it runs.** `Engine.runTeam` asks `Team.TeamAllows` first; `Server.TeamAllows` reads the session's profile as it is now and refuses a private one (`team.CheckTeamCaller`): "<id> is private, and team tools are not a private agent's: you work for the owner directly, with no supervisor, team or desk, and pocketd keeps no lessons for you". This holds for a session that lists a team tool from before: made before this decision, or before its profile became private. A session from no profile, or whose profile is gone or invalid, goes on to the tool's own checks; a profile pocketd cannot read now refuses the call.
5. **A supervisor keeps and lists no lessons for a private agent** (`team.CheckLessonsFor` in `Server.TeamMemory`): they would wait in the Inbox for sessions that never show them.
6. **A private session made before this decision** (no migration, no protocol version):
   - A standing one takes the private preamble, loses its roster, its lessons section and its team tools at its next refresh (its run's end, the owner's message, New Session) or Start Over.
   - An ordinary one keeps the preamble and the tool list it was made with, since pocketd never rewrites an ordinary session's identity or tools. It loses its lessons section at the first delivery pass after pocketd starts (`RefreshAllLessons`), and every team tool it still lists is refused at once. A new session from the profile is private in full.
   - Lessons kept for a private agent before stay in the work database and in the Inbox, as before; approving one changes no session.
7. **No wire change.** The protocol's rules 7b, 25, 32 and 34 say what a private profile's session gets; no type, path or event changes, so the protocol version stays.

## Consequences

- A private agent's prompt says who it works for, and carries nothing of the team to contradict its own rules: no roster, no lessons, no `team_memory` guidance.
- Nothing a private agent does reaches the team: every team tool refuses it in code, so an older session's tool list holds no way out.
- A profile made private loses its team tools at once and its lessons section at the next refresh; one made team again gets its lessons section back at the next refresh, and its tools in a new session or, for a standing session, at its next refresh.
- **Known limits:**
  - An ordinary session made before keeps "the team's" preamble until the owner starts a new one. Rewriting it would change an identity pocketd never rewrites.
  - A profile changed to private while pocketd runs keeps its sessions' lessons section until the agent's next lesson change or pocketd's next start: pocketd does not watch the profile folder.
  - A private agent has no lessons at all: what it keeps across sessions is its own (its home), outside pocketd.

## Alternatives considered

- **Refuse team tools in the handlers, but keep offering them:** the agent would see tools it may not call, and the prompt would still say to keep lessons with `team_memory`.
- **Offer no team tool, and no refusal:** an older session, or one whose profile became private, lists `team_memory` still, and its lessons would reach the Inbox.
- **A check at the top of each team tool's handler:** sixteen copies of one rule; one gate before every team tool (`TeamAllows`) cannot miss a tool added later.
- **Keep a private agent's lessons, shown only to it:** its own memory lives in its home, and the owner's Inbox would fill with lessons of an agent that is not on the team.

## References

- Shapes: [design/architecture.md](../design/architecture.md#service), [specs/protocol.md](../specs/protocol.md) (rules 7b, 25, 32 and 34)
- Built by: [plans/roadmap.md](../plans/roadmap.md) (item 2, threads, delegation and asks: the private-agent rule)
- Supersedes in part: [ADR-0015](0015-keep-each-agents-lessons-approved-by-the-owner-in-code.md) (decisions 3 and 5, for a private profile)
- Builds on: [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0013](0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md), [ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md), [ADR-0015](0015-keep-each-agents-lessons-approved-by-the-owner-in-code.md), [ADR-0026](0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md)
