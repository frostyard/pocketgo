# 0033 — Gather proposals from the team in read-only sessions, in code

- **Status:** Accepted; decision 5's tools, `read` and `team_propose` alone, are superseded by [0036](0036-let-proposal-sessions-consults-and-plan-leads-read-github-with-get-calls-built-in-code.md), which adds `team_github`
- **Date:** 2026-11-21

## Context

**The night shift's second step is a gather.** The owner's old system asked every agent for proposals each evening. Each agent added rows to a goal board, and the supervisor ordered and trimmed them for the owner to pick from (pi-pocket-team's `_desk-board.ts`).

**The old gather's lessons:**
- Its asks re-opened finished threads: 31 re-opens did no work, cost $16, and filled the desk's cap.
- The rotation of gather kinds and its limits lived in prompt text.
- A grooming row checked by a pull request stalled when the answer was "nothing to groom".

**ADR-0030 built the schedules,** with a message as their only action, and an action designed so a gather could follow. Its schedule to a profile with no standing session open missed its time.

**pocketgo has the parts:**
- Consults ([ADR-0020](0020-plan-before-building-with-the-plan-gate-and-consults-in-code.md)) are sessions pocketd starts from an agent's profile, read-only, keyed by a random owner. Their question is a note filed with the session, and they end by a deadline.
- The outbox places a note once ([ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md)).
- Team tools are offered by role and checked again in code.

**The owner's choices:**
- 2026-11-20:
  - A gather makes a batch of proposals whenever it runs, 3 or 4 times a day.
  - pocketd runs the scheduled gathers, and the supervisor gets a gather tool for an ad-hoc one.
  - Gather kinds are no list in code: each gather names a skill, and pocketd puts the skill's text into each asked agent's brief.
  - Skills live where the owner's configuration syncs them for the service user, `~/.pi/agent/skills/<name>/SKILL.md`, in a folder pocketd reads as a checked setting.
  - Each asked agent answers in a read-only proposal session that takes no place on the desk and re-opens no thread. It files rows with a proposal tool.
  - The supervisor orders, edits and drops the rows, and is told once when the gather's proposals are in.
  - Unlaunched proposals stay on the board, marked with their gather's time, until the owner or the supervisor drops them.
- 2026-11-21: a schedule whose profile has no standing session open yet opens it, as New Session would, then delivers.

## Decision

1. **A schedule opens its standing session.** When a schedule's time comes and its profile, valid and `standing`, has no standing session yet, pocketd opens it as New Session would (`Server.openStandingFor`), from the profile as it is under the lock every session's start takes, then delivers to it once it is watched, one the owner's New Session opened meanwhile included. The session is made under the profile's key, `standing:<profile>`, which is the request's key too, and recorded as the profile's standing session in one transaction. A profile that cannot start one misses the time, and says why. This replaces ADR-0030's miss for a session not open yet.
2. **A schedule's second action is a gather.** A `schedules.json` entry takes exactly one of `message` and `gather: {"skill", "agents", "note"}`, each key as written. `agents` is optional: none asks every agent the supervisor can delegate to. At its time pocketd starts the gather for the profile's standing session, in the transaction that moves the schedule on, keyed as its note would be (`store.FireSchedule`). A repeat whose last gather still runs skips its time, decided before its next gather is prepared (`store.SkipIfHeld`), so no check of the next one decides a time already held. A schedule's gather is a supervisor's, as `team_gather` is: one whose profile is not a supervisor's misses its time, with no session opened.
3. **The supervisor gathers at any time** with `team_gather` (skill, agents, note), offered to a supervisor by role and checked again in `service/internal/api`. It is recorded once per team call, in a transaction under the call's context: a call whose run the owner aborted before it committed starts none. The agents' 60 minutes start when the gather is recorded, by the store's clock.
4. **A skill is read from the skills folder:** `-skills DIR`, else `$POCKETD_SKILLS`, else the service user's `~/.pi/agent/skills`. A skill's name is its folder's name (1 to 64 letters, digits, dashes and underscores).
   - pocketd reads `<dir>/<name>/SKILL.md` when the gather starts, as the agents' sandbox shows it, never with its own rights (`Server.loadSkill`, `Engine.ReadUpTo`, which runs pocketd's own `pig.ReadHelper` in the sandbox: it opens without waiting and reads only what it checked on that descriptor): a regular file after the links the sandbox follows, UTF-8, at most 64 KiB (`profiles.ParseSkill`). A link to a file the sandbox hides, or a named pipe, is no skill. It keeps that text with the gather, so every asked agent gets one text.
   - A skill that is not there refuses a supervisor's gather, misses a schedule's time, and shows as the schedule's problem. A check pocketd cannot make now (the sandbox did not start, a profile or the models could not be read) keeps a schedule's time due for a later pass. `pocketd profiles` reads a schedule's skill with its own rights, and says that it does not check it as the sandbox shows it.
5. **Each asked agent proposes in a read-only proposal session of its own** (an ask; `team.Askable`: a delegation's target with a home or a workspace).
   - pocketd starts the session from the agent's profile in its home, else its workspace, with `read` and `team_propose` alone. It is keyed by the ask's random owner, takes no place on the desk, and opens no thread. A new session starts from the profile as it is under the team's locks (`Server.recheckAsk`), its brief kept with its ask before the session is made (`store.PrepareAskBrief`); one made before a stop keeps the profile it was made from and gets that brief, or, past the deadline, is recorded and gets none.
   - Its brief is a note filed with the session (`team.ProposalBrief`): the skill's text, the supervisor's note, the agent's repositories with their checkouts, and the rules of a proposal. It is placed by the outbox only while the ask runs and its time lasts. The owner's message waits for it.
   - An ask ends when its agent calls `team_propose done`, when its run ends once its brief is in (its session idle), or when its 60 minutes pass (done with its proposals, else failed); the store's clock decides which in the transaction that ends it (`store.EndGatherAsk`). A proposal session keeps no lessons: `team_memory` refuses it. The delivery pass follows the asks, at each run's end, at the next deadline, and every two minutes while one runs.
6. **A proposal is a row of the board, checked in code** (`team.ProposalFields.Apply`):
   - its agent; a repository of its agent's, or none for its home; a title of at most 120 characters; the task in full, at most 6,000; `stop_when`, at most 3,000; `max_rounds`, 1 to 100 (20 by default); and a note of at most 500;
   - its kind, the gather's skill; its order; and its rev, which each change of its text moves.
   - It stays `proposed` until the supervisor or the owner drops it.
   - An agent adds, edits and drops only its own, and only while its part runs and before its gather's deadline, decided in the transaction that changes it by the store's clock (`store.ChangeProposal`, `store.AddProposal`). Its `done` past the deadline ends its ask as the deadline does. Every edit and drop names the rev it read: a stale rev is refused.
7. **The supervisor keeps the board** with `team_board`: `list` in pages, `read` one whole, `edit` or `drop` one at its rev, and `order` rows to the top. It sees and changes only the proposals of gathers that sessions of its own profile asked for, chosen in the store's query (`store.BoardOf`, `store.ProposalBoard`), and learns nothing of another's rows: ownership is checked before a row's state or rev is told. An add, an edit, a drop and an order keep their answer with their call's key (`proposal_calls`), which answers a call run again before its caller is checked.
8. **The supervisor is told once.** The last ask's end ends the gather and files the supervisor's note in the same transaction (`store.EndGatherAsk`): `[G<n> · <skill> · proposals in]`, each proposal by number with its agent and title, and the agents that proposed nothing or did not answer.
9. **An older supervisor's session gets the supervisor's later tools at start,** each it lacks, in the order a new one has them: `team_schedule`, `team_send`, `team_gather` and `team_board` (`Engine.SetSupervisorTools`, which takes the place of ADR-0030's `SetScheduleTool` and ADR-0032's `SetSendTool`). A standing session takes them when it takes its profile.
10. **The protocol shows them.** Protocol version 21 adds `GET /v1/gathers` (running, and ended in the last seven days), `GET /v1/proposals` (not dropped, in the board's order, each with its gather's time) and their `gather` and `proposal` events. A schedule shows its gather ([spec rules 57 and 58](../specs/protocol.md)). Work database migration 17 adds the gathers, their asks, the proposals and their calls. The board's launch, the owner's pick and the desktop's board are the next slice.

## Consequences

- The owner adds a gather kind by adding a skill to their configuration and naming it in a schedule, with no change to pocketd.
- A gather re-opens no thread and fills no place on the desk: an agent with nothing to propose costs one short read-only run.
- Every agent asked runs a session at the gather's time. A gather of the whole team starts as many sessions at once as the team has agents.
- A proposal session cannot run commands, so a skill that needs `gh`, as an issue triage would, cannot list issues: its agent reads only files.
- The skills folder is the service user's, which the agents' commands can write, so an agent could change the text another agent's brief quotes.
- A standing session can now start without the owner's New Session, at a schedule's time, from its profile as it is then.

## Alternatives considered

- **Re-opening each agent's last thread to ask, as before:** that is what wasted the desk and the money.
- **Gather kinds as a list in code:** the owner chose skills, named in config, so new kinds need no release.
- **Bash in the proposal session, for `gh`:** the owner chose read-only sessions; a skill that needs commands is a question for them.
- **Missing the time of a standing session not opened yet, as ADR-0030 did:** the owner chose on 2026-11-21 to have pocketd open it.

## References

- Shapes: [design/architecture.md](../design/architecture.md#service), [specs/protocol.md](../specs/protocol.md) (rules 1, 10, 25, 57 and 58)
- Built by: [plans/roadmap.md](../plans/roadmap.md) (item 15, the night shift, slice 2)
- Builds on: [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md), [ADR-0020](0020-plan-before-building-with-the-plan-gate-and-consults-in-code.md), [ADR-0024](0024-run-a-delegated-thread-in-rounds-as-a-shift-driven-in-code.md), [ADR-0026](0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md), [ADR-0030](0030-send-scheduled-messages-from-the-owners-file-and-the-supervisors-tool-in-code.md)
