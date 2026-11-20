# 0024 — Run a delegated thread in rounds as a shift, driven in code

- **Status:** Accepted
- **Date:** 2026-11-20

## Context

**The owner runs work in shifts.** pi-pocket-team's night shift ran a session in rounds toward an objective, until a stop condition held, a limit ran out, or the owner stopped it. The owner launches them through the supervisor, as in "Launch today's shift work, use deepseek". Its parts:
- **The driver.** It sent `[shift] Round N of M`, waited for the agent's turn to end, and read the turn's last `STATUS: DONE|CONTINUE|BLOCKED: reason` line, with no judge model.
- **The stops.** A shift stopped:
  - on done, with an optional check;
  - after a round limit (default 20, at most 100);
  - at a deadline (at most 24 hours ahead);
  - after three failures of the same check in a row;
  - on blocked;
  - at the owner's stop.
- **Pausing.** The owner's message paused it.
- **The report.** A summary round wrote the report.

**The lessons it left:**
- Rounds were wasted while the agent waited on its subagents.
- Half its reports were twinned by a separate `team_message`.
- An automated message counted as the owner's and paused shifts.
- A check cut off by a restart had to read as cut off, not run again.

**pocketgo has the parts a shift needs:**
- threads with a specialist session ([ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md));
- the outbox, which places a message once;
- a pass that follows each run that ends (`Server.runEnded`);
- the specialist's worker ([ADR-0021](0021-give-a-threads-specialist-a-worker-on-an-api-key-model-in-code.md), [ADR-0022](0022-run-any-session-on-any-sign-in-and-choose-workers-from-the-owners-list.md));
- linked pull requests with their CI ([ADR-0016](0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md), [ADR-0017](0017-record-cross-review-verdicts-and-set-ready-for-human-in-code.md)).

**The owner's choices, 2026-11-20.** The supervisor's delegation starts a shift first, and a desktop start comes later. There is no spend limit yet: rounds and the deadline bound a shift.

## Decision

1. **A shift is a delegated thread that runs in rounds.** `team_delegate` takes an optional `shift`, checked in code (`team.CheckShift`):
   - `stop`: the completion condition, 1 to 3,000 characters.
   - `max_rounds`: 1 to 100, 20 by default.
   - `hours`: optional, the deadline after the thread is recorded, above 0 and at most 24.
   - `ci`: optional. With `true`, done also needs CI green on each pull request the thread links, and at least one such pull request.

   The thread's task is the objective. The thread keeps its shift (work database migration 12): its rules, its round, its state, its consecutive CI failures, and each round's verdict. A plan's start takes no shift, and neither does a review.
2. **pocketd drives the rounds** (`Server.shiftPass`, after each run of the specialist or its worker that ends, and at start):
   - The brief is round 1. Each later round is one message, `[shift] Round N of M …`, kept in the outbox and placed once, keyed by the shift and the round.
   - A round ends when the specialist is idle, its worker is idle, and no reply of the worker is still owed. A round that waits on its worker is not over.
   - pocketd reads the round's last entry of the model's, after the message that began the round, for its last line `STATUS: DONE|CONTINUE|BLOCKED[: reason]` (`team.ParseShiftStatus`). No line is continue. No model judges anything.
   - Each round's verdict is recorded with the round. The count moves in the transaction that keeps the next round's message.
3. **The stops are code, checked between rounds, in this order:**
   1. The owner's message to the specialist's session pauses the shift (reason `person`). The message runs as any. pocketd's own messages (a brief, a round, an answer, a worker's reply) never count. The owner resumes it.
   2. `BLOCKED` ends it (reason `blocked`).
   3. `DONE` without `ci` ends it (reason `done`). With `ci`, pocketd reads the linked pull requests on GitHub:
      - all green, with at least one: reason `done`;
      - CI still running: the round waits for the next read;
      - failing: the next round says which checks fail. The third time in a row that the same failing checks are seen, the shift ends (reason `failed-check`).
   4. The round limit (reason `max-rounds`).
   5. The deadline (reason `deadline`). It is checked before each round, so a round that runs finishes.
4. **Every end but the owner's stop gets one summary round.**
   - The summary round says why the shift stopped, and asks for one `team_message` with status `done` or `blocked`: what was done, what is left, the pull requests, and the decisions the owner must make.
   - Until that round, `team_message` with status `done` or `blocked` is refused, which ends the twinned reports. A `working` report with asks stays allowed.
   - A summary round that ends with no report gets one filed by pocketd: `The shift ended: <why>. It took N of M rounds.`
5. **The owner stops and resumes a shift.**
   - `POST /v1/threads/{id}/shift/stop` aborts the running round and ends the shift (reason `stopped`), with no summary. The supervisor is told once.
   - `POST /v1/threads/{id}/shift/resume` goes on with a paused shift, or one that ended for a reason other than `done`, while rounds and time are left. It keeps the failure count.
   - A thread's close stops its shift (reason `closed`).
6. **Protocol version 12.** A thread carries its shift: rules, round, state, the reason and why it stopped, and each round's verdict. The two endpoints are new. The desktop shows a shift on the work board, with Stop and Resume, in the next change.

## Consequences

- The supervisor runs long work through the night, as the owner did with Pocket: bounded in rounds and time, decided in code, and reported once.
- A shift's thread keeps its place on the desk while it runs.
- **Known limits:**
  - There is no spend limit: the owner chose rounds and the deadline first. A limit from the usage pocketd records, or a subscription meter, comes later.
  - The owner cannot start a shift from the desktop yet.
  - There is no command check other than CI. A free command check, the owner's alone in Pocket, comes with the desktop start.
  - The goal board (proposals, the owner's picks, a launch pass) is not ported.
  - An agent that never ends its turn holds its round: pocketd waits on the run, as on any.

## Alternatives considered

- **A judge model decides done:** rules go in code ([ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md)). The agent says its status, and code checks what it can check, as CI.
- **A timer drives the rounds:** a round would start while the agent or its worker still works. The end of a run, and the worker's, is the signal.
- **The supervisor drives the rounds:** each round would cost a supervisor turn, and the supervisor would decide when to stop, which is a rule.

## References

- Shapes: [design: system architecture](../design/architecture.md#service), [specs: protocol](../specs/protocol.md), [plans: roadmap, Phase 3](../plans/roadmap.md)
- Builds on: [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md), [ADR-0016](0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md), [ADR-0017](0017-record-cross-review-verdicts-and-set-ready-for-human-in-code.md), [ADR-0021](0021-give-a-threads-specialist-a-worker-on-an-api-key-model-in-code.md), [ADR-0022](0022-run-any-session-on-any-sign-in-and-choose-workers-from-the-owners-list.md)
