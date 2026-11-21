# 0034 — Launch the owner's picks from the supervisor's standing session, in code

- **Status:** Accepted
- **Date:** 2026-11-21

## Context

**The night shift's last step is the launch.** [ADR-0030](0030-send-scheduled-messages-from-the-owners-file-and-the-supervisors-tool-in-code.md) sends scheduled messages and [ADR-0033](0033-gather-proposals-from-the-team-in-read-only-sessions-in-code.md) gathers proposals onto a board. The owner picks from that board before bed, and the picked work should run until a time they choose, without them.

**The old launch's lessons:** pi-pocket-team launched picks from prompt text. It started as many as the model chose, retried starts it was unsure of, and once filled the desk so that the morning's own work had no place.

**The owner's decisions (2026-11-20 and 2026-11-21):**
- The board's Proposals section goes at the top of the Work Board, with a Pick checkbox on each proposal and a "Launch picks (N)" button.
- An end chooser offers 4 hours by default, or a clock time.
- A launch runs from the supervisor's standing session, with an absolute end, a limit on how many start at once, the desk's cap minus a reserve, and a round budget.
- pocketd never retries an uncertain start.
- A Schedules section on the Work Board shows each schedule's next time, last run and any problem, below the Proposals section.
- The owner drops proposals from the board too.

## Decision

1. **A launch is a set of proposals and an end.** The owner starts one with `POST /v1/launches` (the proposals in order, and an absolute end). The supervisor starts one with `team_launch` (rows of its board, and `hours` or a clock time `until`, 4 hours when it names neither). The checks, all in `service/internal/team`:
   - 1 to 20 distinct proposals, each still `proposed`, all on one supervisor's board;
   - their `max_rounds` add up to at most the launch's round budget, 200 (`team.LaunchRounds`);
   - the end is after now and at most 24 hours ahead (`team.MaxShiftHours`);
   - each agent still takes a delegation in the proposal's repository (`team.CheckTarget`, `team.CheckDestination`).

   A launch is recorded once per request key: the owner's request id, or the supervisor's team call. Its proposals become `launched` in the same transaction, so nobody edits or drops them while they wait.
2. **The supervisor's standing session delegates them.** The board's supervisor profile must keep a standing session (`session: standing`); pocketd opens it as a schedule would when it is not open (`Server.openStandingFor`). Each launch item becomes one shift thread delegated from that session, so its reports go there:
   - the thread takes the proposal's agent, repository, title, task, stop condition and rounds;
   - its deadline is the launch's end (an absolute shift deadline, [ADR-0024](0024-run-a-delegated-thread-in-rounds-as-a-shift-driven-in-code.md)).
3. **The launch pass starts items in order, within limits.** An item starts only while:
   - the launch's end has not passed;
   - fewer than 3 of the launch's threads are open (`team.LaunchAtOnce`);
   - the desk holds fewer open threads than its cap minus 2 (`team.LaunchReserve`), a reserve for the owner's and the supervisor's own work.

   An item whose rule no longer holds is skipped with why, and its proposal goes back on the board. Rules that end it this way: the launch's end passed, or its agent no longer takes the delegation. A check pocketd cannot make now waits for a later pass.
4. **pocketd never retries an uncertain start.**
   - An item's thread is created in the transaction that marks the item started, keyed by the item's random owner, and the item never gets another.
   - A start that fails before the specialist may have its brief closes its thread with why, as `team_delegate`'s does.
   - One that may have started stays on the desk, and its reports settle it.
   - A start a stop of pocketd interrupted is finished by the next pass, with the same thread and the same keyed session, never a second.
5. **The pass runs on its own.** It starts after `StartSchedules` (`StartLaunches`), and runs again after each launch, when a thread closes, at a launch's end, and every minute while a launch has items waiting. The launch ends when no item waits.
6. **The supervisor is told once of an owner's launch.** pocketd files one note through the outbox, with each proposal by number and the end. A supervisor's own launch answers its call.
7. **The owner drops a proposal** with `POST /v1/proposals/{id}/drop`, at the rev the board shows, recorded once per request id.
8. **The protocol shows them.** Protocol version 23 adds `POST` and `GET /v1/launches`, `POST /v1/proposals/{id}/drop`, the `launch` event, and a proposal's `launched` state with its `launch` and `thread` ([spec rule 59](../specs/protocol.md)). Work database migration 19 adds the launches and their items. The Schedules section reads `GET /v1/schedules`, as before.
9. **The desktop's board** shows, first, the Proposals section:
   - the proposals still `proposed`, in the board's order, each with its gather's time, a Pick checkbox and a Drop button;
   - "Launch picks (N)", with the end chooser: in 4 hours, or until a clock time, the next such time in the desktop's zone.

   The Schedules section comes next, then the threads. The rules (picks, the end, which proposals show) live in `desktop/internal/state`.

## Consequences

- The owner launches a night's work in one click, and the morning finds the desk's reserve free.
- A launch never starts a second thread for one proposal, so an uncertain start costs at most one thread.
- Proposals waiting in a launch are off the board; one skipped comes back to it.
- The limits are constants in code; the owner changes them by a change of code, not by prompt text.

## Alternatives considered

- **Each pick as a `team_delegate` the supervisor makes:** its turns would launch them, in prompt text, the old way.
- **Retrying a failed start:** the owner chose never to retry an uncertain one; a failed start closes its thread and shows why.
- **A round budget spent as rounds run:** the budget counts the rounds each item may take, checked when the launch is made, so the owner sees at once what does not fit.

## References

- Shapes: [design/architecture.md](../design/architecture.md#service), [specs/protocol.md](../specs/protocol.md) (rules 1, 25 and 59)
- Built by: [plans/roadmap.md](../plans/roadmap.md) (item 15, the night shift, slice 3)
- Builds on: [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md), [ADR-0024](0024-run-a-delegated-thread-in-rounds-as-a-shift-driven-in-code.md), [ADR-0026](0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md), [ADR-0030](0030-send-scheduled-messages-from-the-owners-file-and-the-supervisors-tool-in-code.md), [ADR-0033](0033-gather-proposals-from-the-team-in-read-only-sessions-in-code.md)
