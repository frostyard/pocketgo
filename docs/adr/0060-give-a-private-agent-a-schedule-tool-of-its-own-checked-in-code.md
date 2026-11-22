# 0060 — Give a private agent a schedule tool of its own, checked in code

- **Status:** Accepted
- **Date:** 2026-11-22

## Context

**A private agent has no schedule tool.** frostyard/pocketgo#88, filed on 2026-11-21 at the owner's request, asks for one for the owner's private agent, Chani, as Pi's `schedule` tool gave her. Her weekly memory pulse ran that way: a session titled "Chani: memory pulse" kept its own repeat.

**The supervisor has the right engine already** ([ADR-0030](0030-send-scheduled-messages-from-the-owners-file-and-the-supervisors-tool-in-code.md), decision 4):
- `team_schedule`'s `add` sends a message to the calling session, at a time or on a repeat. It works in any session, not only a standing one.
- It has `list` and `cancel`, at most 50 schedules a session that still go out, and an add recorded once per team call (`api.TeamCall`).
- Each time goes out as `[scheduled] <message>` through the outbox.
- It is the supervisor's alone: `roleTools` offers it by role, and `Server.TeamSchedule` refuses any other caller with "only a supervisor keeps schedules".

**A private agent is kept out of the team** ([ADR-0042](0042-give-a-private-agent-its-own-preamble-and-no-team-in-its-sessions.md)):
- Its sessions get no team tool (`roleTools`).
- Every team tool refuses its session before the tool runs (`Server.TeamAllows`), an older session's included.

**The owner's schedules do not reach Chani.** The owner's `schedules.json` reaches only a profile's standing session. Chani has none, by the owner's choice so far, since a standing profile's New Session always opens its one session (protocol rule 7c).

**A tool can reach a private agent without being a team tool.** `question` ([ADR-0048](0048-put-the-supervisors-and-a-private-agents-questions-to-the-owner-inline-in-code.md)) did:
- It lives in an extension of its own, after the team tools. A supervisor's and a private profile's sessions get it (`team.MayQuestion`), and `Server.Question` checks the caller again.
- It refuses a run the owner's own message did not start, a `[scheduled]` one included.

**Older sessions get later tools in two ways** ([ADR-0026](0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md), [ADR-0046](0046-let-the-supervisor-read-the-owners-inbox-from-pocketds-records-in-code.md)):
- An ordinary session keeps the tools it was made with. pocketd gives it a later tool at start (`UpgradeTeamTools`, through `Engine.SetSupervisorTools` and `Engine.SetQuestionTool`). The supervisors' upgrade is tried again by each schedules pass while it fails (`toolsDue`).
- A standing session takes its profile's tools at its idle refresh and at Start Over (`Engine.Reconfigure`, `Engine.StartOver`).

**The issue leaves one question open:** a separate `schedule` tool, or one tool under one name for both roles.

## Decision

1. **The tool is `schedule`, a private agent's. The supervisor keeps `team_schedule`.**
   - One name for both roles would mean one of two things. It could be `team_schedule` in a private agent's session: a tool named for the team, in a session ADR-0042 keeps out of it, which `TeamAllows` would have to let through by name. Or it could be `schedule` for the supervisor, which would change a tool every supervisor's session lists, Odrade's standing one included, and gain nothing.
   - `schedule` has no `team_` prefix, since it is no team machinery: it sends to the calling session alone.

2. **Who gets it, in code** (`team.MaySchedule`): a session of a private profile that works as no thread's specialist, review, worker, consult or gather session. These are the sessions of a private profile that get `question`.
   - `service/pig` offers it in an extension of its own, `pocketgo-schedule`, after the question tool (`toolsOf`). A private agent's session has its profile's coding tools, `question` and `schedule`, and no team tool.
   - A supervisor's session gets no `schedule`, and neither do a team agent's, a thread's specialist's, a worker's, a review's, a consult's or a gather's session.

3. **It is the one exception ADR-0042 names, and its checks are code:**
   - It is no team tool. It is not in the team extension, so `TeamAllows` does not run before it, and `roleTools` still gives a private profile nothing.
   - `Server.Schedule` checks the caller again with its profile as it is now and what the session works as (`team.CheckScheduleCaller`), before anything is kept. Only a private profile's or a supervisor's session, working as none of those roles, passes. A supervisor's passes so that a session made while its profile was private keeps its schedules once the profile is a supervisor's, which keeps its own anyway.
   - Every other session is refused: "schedule is a private agent's and the supervisor's alone, in a session the owner talks in: this session keeps no schedules". So is a session whose profile is gone or invalid. A profile pocketd cannot read now refuses the call, which a later call tries again.
   - It reaches nothing of the team. An add sends to the calling session alone. `list` shows that session's own, and, for a standing session, the owner's that go to it. `cancel` takes only that session's own. No other agent, session, thread or Inbox sees it or is written.

4. **The engine and the limits are `team_schedule`'s** (`Server.keepSchedules`):
   - The grammar of `when`, at most 50 schedules a session that still go out, counted where one is added, at most 4,000 characters a message, and repeats at least 10 minutes apart.
   - A list in pages of at most 16 KiB and 400 lines.
   - An add recorded once per call and answered as it was, whatever the rules say by then, the caller's check included. A cancel's answer depends only on the schedule.
   - The work database keeps them as session schedules, as it keeps a supervisor's: no migration.
   - Each time goes out as `[scheduled] <message>`, through the outbox, to the session that made it. `question` refuses in that run (ADR-0048, decision 3). The tool's description is `team_schedule`'s, with `schedule.Guide`: a scheduled message may have nobody watching.
   - An add's answer and a list's "More" line name the tool that was called (`schedule.AddedText`, `schedule.ListText`).

5. **`team_schedule` does not change.** It is offered by role to a supervisor and refused for a private agent by `TeamAllows`. Its own check and its answers stay as they were.

6. **Older sessions:**
   - An ordinary private session made before the tool gets it at start, after its other tools (`UpgradeTeamTools`, `Engine.SetScheduleTool`). A session whose profile pocketd cannot read then gets it from a later schedules pass (`toolsDue`), as a supervisor's later tools do.
   - A session made before the question tool too ends with `question`, then `schedule`, whichever upgrade runs first: `SetQuestionTool` places `question` before `schedule`.
   - A standing private session takes the tool with its profile at its idle refresh and at Start Over. A standing session whose profile stops being private gives it up then, and takes `team_schedule` back if its profile is a supervisor's.

7. **The protocol does not change.** No type, path or event moves, and its version stays 31. Rules 7b, 25 and 57 name the tool and its caller. `GET /v1/schedules` lists a private agent's own as a session's own (`source` `session`), as it lists a supervisor's.

## Consequences

- Chani keeps fresh chats. Her pulse session schedules itself, "every sun 9:00", with no standing session. This closes most of the pulse half of the standing-session gap left open in frostyard/pocketgo#82.
- Event wakes (#82) still need a target session. That decision stays open.
- A private agent's session now keeps something in pocketd's work database: its schedules. The owner sees them in `GET /v1/schedules`, and the desktop's Night Shift view lists them with the supervisors'. No other agent sees them: a supervisor's `team_schedule list` shows its own session's alone.
- One engine has two names. A change to the engine's rules reaches both tools. A change to one tool's surface, its schema or description, needs the same thought for the other.
- **Known limits:**
  - An ordinary session whose profile becomes private while pocketd runs gets `schedule` at pocketd's next start. One whose profile stops being private keeps listing `schedule`, and each call is refused, unless the profile is a supervisor's.
  - A schedule a session keeps goes on after its profile changes, as a supervisor's does today: it belongs to its session. The session can cancel it only while it may call a schedule tool, and the owner cannot cancel it from the desktop yet.

## Alternatives considered

- **`team_schedule` for both roles, let through `TeamAllows` by name:** a team-named tool in a session with no team, and an exception inside the one gate that holds ADR-0042 for every team tool, a later one included.
- **`schedule` for both roles:** the supervisor's tool renamed in every session that lists it, for no behaviour gained.
- **A standing session for Chani, so the owner's schedules file reaches her:** the owner chose fresh chats, and rule 7c would make every New Session open her one session.
- **The owner's schedules file naming any session, not a profile:** a session's id is no stable name in the owner's configuration, and the agent could not add, list or cancel its own reminders.

## References

- Shapes: [design/architecture.md](../design/architecture.md#service) (the schedule tool), [specs/protocol.md](../specs/protocol.md) (rules 7b, 25 and 57)
- Built by: [plans/roadmap.md](../plans/roadmap.md) (item 33, a private agent's own schedules)
- Builds on: [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0026](0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md), [ADR-0030](0030-send-scheduled-messages-from-the-owners-file-and-the-supervisors-tool-in-code.md), [ADR-0042](0042-give-a-private-agent-its-own-preamble-and-no-team-in-its-sessions.md), [ADR-0046](0046-let-the-supervisor-read-the-owners-inbox-from-pocketds-records-in-code.md), [ADR-0048](0048-put-the-supervisors-and-a-private-agents-questions-to-the-owner-inline-in-code.md)
