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

**The owner's choices, 2026-11-20.** The supervisor's delegation starts a shift first, and a desktop start comes later. There is no spend limit yet: rounds and the deadline bound a shift. The owner's Stop ends the shift only: an earlier draft had it abort the specialist's and worker's runs and drop their pending work, and seven review rounds kept finding races between that abort and the owner's messages, worker calls and restarts, so the owner chose the simpler Stop.

## Decision

1. **A shift is a delegated thread that runs in rounds.** `team_delegate` takes an optional `shift`, checked in code (`team.CheckShift`):
   - `stop`: the completion condition, 1 to 3,000 characters.
   - `max_rounds`: 1 to 100, 20 by default.
   - `hours`: optional, the deadline after the thread is recorded, above 0 and at most 24.
   - `ci`: optional. With `true`, done also needs CI green on each pull request the thread links, and at least one such pull request.

   A limit given is a limit: `max_rounds` or `hours` given as 0 is refused, and only one left out takes its default.

   The thread's task is the objective. The thread keeps its shift (work database migration 12): its rules, its round, its state, its consecutive CI failures, and each round's verdict. A plan's start takes no shift, and neither does a review.
2. **pocketd drives the rounds** (`Server.shiftPass`, one pass at a time, after each run of the specialist or its worker that ends, after a round's message or a shift's brief is noted placed, and at start):
   - The brief is round 1, and goes only before the deadline, checked last before each submission: a brief not in the transcript by then is never submitted, one still queued is withdrawn, and the thread closes with its shift ended for its deadline. Each later round is one message, `[shift] Round N of M …`, kept in the outbox and placed once, keyed by the shift and the round, and only while it begins the round under way: the message of a round that ended, or of a shift that paused or ended, goes to no one. One not in the transcript by the deadline is never submitted again, and the shift stops for its deadline.
   - A round begins at the transcript entry PiG recorded for its message's request key (`Engine.PlacedEntry`), whatever pocketd noted of the submission.
   - A round ends when the specialist is idle, its worker is idle, and nothing is owed to either, by session, whatever thread filed it: no message pocketd sends (a cross-review's verdict included), no reply of the worker's ended run (kept first if it was not), and no answer to the specialist's ask that has not reached it. A decision commits under the outbox's lock, after the round is read again, in a transaction that finds nothing owed: one that moved meanwhile, as while CI was read, waits for the next pass. A round that waits on its worker is not over. Only its own entries are read, and none while the specialist works.
   - pocketd reads the round's last run's final response, empty or not, after that run's own input (an entry that ends in a tool call is none), for its last line `STATUS: DONE|CONTINUE|BLOCKED[: reason]` (`team.ParseShiftStatus`). No line is continue; a last run that failed with no text fails the round. No model judges anything.
   - Each round's verdict is recorded with the round. The count moves in the transaction that keeps the next round's message.
3. **The stops are code, checked between rounds, in this order:**
   1. The owner's message to the specialist's session pauses the shift (reason `person`). The message runs as any. pocketd's own messages (a brief, a round, an answer, a worker's reply) never count. The owner resumes it.
   2. `BLOCKED` ends it (reason `blocked`).
   3. `DONE` without `ci` ends it (reason `done`). With `ci`, pocketd reads each linked pull request on GitHub, GitHub's answer alone, one recorded closed included, so CI is read at the head pushed last, then its CI, each read bounded:
      - all green, with at least one: reason `done`, pinned to those pull requests at those heads, the only ones a report of the summary round may link;
      - CI still running, or a pull request or CI that cannot be read: the round waits for the next read, at most one every two minutes, which a wake of its own brings, until the deadline;
      - a pull request GitHub names with another id than the one linked: its check fails;
      - failing: the next round says which checks fail. The third `DONE` round in a row that sees the same failing checks ends the shift (reason `failed-check`); any other round ends the streak.
   4. The round limit (reason `max-rounds`).
   5. The deadline (reason `deadline`). It is checked before each round, so a round that runs finishes.
   6. A run that failed with no answer ends the shift (reason `failed`), its round recorded with no verdict.
4. **Every end but the owner's stop gets one summary round.**
   - The summary round says why the shift stopped, and asks for one `team_message` with status `done` or `blocked`: what was done, what is left, the pull requests, and the decisions the owner must make.
   - Until that round, `team_message` with status `done` or `blocked` is refused, which ends the twinned reports. A `working` report with asks stays allowed. In that round, a report with another status than its stop asks for is refused (`team.CheckShiftReport`), and so is one filed before the summary's message is in the specialist's transcript, one whose shift left the summary round it was checked in (`store.ShiftSeen`), and any other report from the run that filed it, also after a resume, or checked in the summary round after the shift left it; the report ends the shift in the transaction that files it.
   - A summary round that ends with no report gets one filed by pocketd: `The shift ended: <why>. It took N of M rounds.`, in the transaction that ends the shift, so the supervisor hears once whatever stops pocketd.
5. **The owner stops and resumes a shift.**
   - `POST /v1/threads/{id}/shift/stop` ends the shift (reason `stopped`), with no summary, and files pocketd's report in one transaction: the supervisor is told once. The shift's own message still queued in PiG is withdrawn first, and pocketd sends no more rounds. The stop ends the shift only: it aborts no run and drops nothing pocketd owes the specialist or its worker, whose runs go on to their ends; the owner stops a session with its own abort. A stop of a shift that ended succeeds and spends its request id.
   - `POST /v1/threads/{id}/shift/resume` goes on with a paused shift, or one that ended for a reason other than `done` or `closed`, while rounds and time are left. It keeps the failure count.
   - Each claims its request id in the transaction that changes the shift. A shift whose specialist does not have its brief yet is neither stopped nor resumed.
   - The owner's message pauses a shift as its request id is claimed, before it goes: the same message tried again pauses nothing, a message that failed keeps its claim, and a message PiG has never leaves its shift running. The round's message still queued is withdrawn first.
   - A thread's close stops its shift (reason `closed`) and, for a shift not ended, notes it to the supervisor in the same transaction.
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
  - The owner's Stop lets the current turn of the specialist, and of its worker, run to its end; the owner aborts a session to cut it short.

## Alternatives considered

- **A judge model decides done:** rules go in code ([ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md)). The agent says its status, and code checks what it can check, as CI.
- **A timer drives the rounds:** a round would start while the agent or its worker still works. The end of a run, and the worker's, is the signal.
- **The supervisor drives the rounds:** each round would cost a supervisor turn, and the supervisor would decide when to stop, which is a rule.

## References

- Shapes: [design: system architecture](../design/architecture.md#service), [specs: protocol](../specs/protocol.md), [plans: roadmap, Phase 3](../plans/roadmap.md)
- Builds on: [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md), [ADR-0016](0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md), [ADR-0017](0017-record-cross-review-verdicts-and-set-ready-for-human-in-code.md), [ADR-0021](0021-give-a-threads-specialist-a-worker-on-an-api-key-model-in-code.md), [ADR-0022](0022-run-any-session-on-any-sign-in-and-choose-workers-from-the-owners-list.md)
