# 0072 — Watch a pull request in code and tell the session once

- **Status:** Accepted
- **Date:** 2026-11-24

## Context

**Only a shift waits for CI.** A shift with `ci` reads CI itself between its rounds ([ADR-0024](0024-run-a-delegated-thread-in-rounds-as-a-shift-driven-in-code.md), decision 3), and a launch carries a proposal's `ci` to its shift ([ADR-0047](0047-carry-a-shifts-ci-on-a-proposal-to-its-launch.md)). A thread that is no shift reports its pull request and has no tool to wait for its checks. ADR-0047's context names the cost: specialists ended their turns with CI still running, and their pull requests went green after.

**Nothing waits for a merge.** A supervisor that should start follow-up work once a pull request merges has no tool to learn of the merge. pocketd's rounds read every linked pull request every `-pr-every`, and close a thread whose pull requests merged ([ADR-0016](0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md)), but no session hears of it. `team_inbox` reads pocketd's records when the supervisor asks ([ADR-0046](0046-let-the-supervisor-read-the-owners-inbox-from-pocketds-records-in-code.md)), and `team_schedule` sends a dated reminder with no condition ([ADR-0030](0030-send-scheduled-messages-from-the-owners-file-and-the-supervisors-tool-in-code.md)).

**The old setup had both.** pi-pocket-team's `team_wait_for_ci` let any session wait for a pull request's checks, and its watches let a thread sleep outside the desk's cap until a date, an event, or a condition checked by machine: a merge, green checks, a run's success. pi-pocket's `watch_pr` did the same. The owner's global `AGENTS.md` still names `team_wait_for_ci` and `watch_pr`. The migration survey of 2026-11-24 lists them as G4 (watches) and G5 (wait for CI outside a shift).

**The owner asked for them.** On 2026-11-24, after reading the survey, the owner said: "strike G1, make a PR for G2, G4, G5, G6, G10, G11 - combine smaller changes into one PR if you can". G4 and G5 are one mechanism: a session waits until a pull request reaches a condition.

**pocketd has the parts:**
- It reads a pull request on GitHub, and CI at a commit with the failing checks' names, through `gh` in the sandbox (`github.Read`, `github.CIChecks`, `Engine.GH`).
- Its pull rounds run every `-pr-every`, 5 minutes by default, and 0 for none.
- Every message pocketd owes a session is a note, filed in the transaction of its cause, keyed once, and placed once by the outbox ([ADR-0020](0020-plan-before-building-with-the-plan-gate-and-consults-in-code.md)); a schedule's time is filed the same way.
- A tool call that blocks while it waits holds a durable task and a run for hours. The shift's rules already tell agents to end their turn rather than wait, sleep or poll.

## Decision

1. **`team_watch` asks pocketd to tell the session when a pull request reaches a condition.** The call returns at once; the session ends its run, and pocketd's one note starts the next. No tool call waits.
   - `add` takes `pull` (`owner/name#number`), `until` and `hours`. `until` is `checks` (CI at the pull request's head, read in each round, is done: green, failing or none, anything but pending), `merged`, or `closed` (merged, or closed without merging). `hours` is how long at most: above 0 and at most `team.MaxWatchHours` (168), `team.DefaultWatchHours` (24) when left out; 0 given is refused, as a shift's limits are.
   - `list` shows the session's live watches and its last `team.MaxWatchesListed` (10) ended ones, read in one transaction. `cancel` ends one of the session's own, by its id (`W3`), with no note.
   - An add is kept once per team call (`api.TeamCall`), with its answer: the call run again answers as it did and reads nothing.
2. **Who gets it is code** (`team.MayWatch`, offered in `roleTools`, checked again in `Server.TeamWatch` by `team.CheckWatchCaller`): every session that gets `team_memory`, a supervisor's, a thread's specialist, a plan's lead and a session the owner talks in of a profile that is not private, but a shift's specialist.
   - A shift's rounds read CI in code already, and a note would start a run outside them. The thread's shift is recorded with it, so its specialist's session is made with `SessionSpec.Shift`, and `Server.sessionRole` reads it again.
   - A consult, a gather's agent, a review and a worker do one fixed job, and a private agent has no team ([ADR-0042](0042-give-a-private-agent-its-own-preamble-and-no-team-in-its-sessions.md)).
3. **An add reads the pull request on GitHub once**, as pocketd reads every pull request (`github.Read` through `Engine.GH`, in the sandbox).
   - One pocketd cannot read is refused, and nothing is kept. Nor is anything kept for a call stopped while it reads, or while it waits for its admission: CI its abort cut short is no CI GitHub could not answer, and its insert takes the call's context.
   - One at its condition already, or past reaching it, is answered at once and kept as ended, with no note (`team.WatchDecide`). A `checks` watch reads CI at the head too; CI that cannot be read then leaves the watch to the rounds.
   - With `-pr-every 0` nothing would check a watch, and an add is refused (`team.WatchesOff`). A start with `-pr-every 0` ends each live watch with its note, since nothing would check it (`Server.endUncheckedWatches`). It first stops admissions, and an add's last check and its insert are one step with that (`watchAdmitMu`): an add read before the start commits before the sweep reads, or is refused. The sweep is scheduled off the start path, as its retries are, so serving waits for none of it; one that fails runs again until it has ended them (`Server.uncheckedPass`), and admissions stay closed.
   - A thread's specialist whose thread is closed keeps no live watch, checked in the transaction that would keep it, which the close comes before or after. The add first notes the thread's placed messages under `outboxMu` (`Server.noteSendsPlaced`), as a report does, so a specialist running on a message that reopens its thread keeps its watch.
4. **The work database keeps each watch** (work database migration 26, `watches`): its team call, a random owner that keys its note, its session, its pull request with GitHub's own id of it, its condition, its hours and deadline, its answer, its state, and once it ends how it ended, when, and what pocketd saw last.
   - A session keeps at most `team.MaxWatchesPerSession` (16) live watches, and pocketd at most `team.MaxWatches` (64), counted in the transaction that keeps one. The bounds guard pocketd's resources, not a policy.
5. **Each pull round checks the watches after its reads** (`Server.watchPass`, one pass at a time), in a pass on a goroutine of its own that `pullRound` starts and never waits for (`Server.watchSoon`): slow reads of watched pull requests never put off the next round. It reads each watched pull request once a pass, whichever watches name it, and CI once a pass for each repository and commit, whichever pull requests have it at their heads.
   - A watch ends when its condition holds; when its pull request can no longer reach it (closed without merging while it waits for `merged`, merged or closed while it waits for `checks`); or at the first pass past its deadline. A read whose pull request has another GitHub id than the one the watch's add read counts as failed for that watch alone.
   - A read that fails leaves the watch for the next pass, until its deadline.
   - Every GitHub request of the passes, each page of CI included (up to 10 each of check runs and statuses), takes one from the watches' budget (`team.ReadBudget`): a burst of `team.WatchReadsBurst` (400), refilled at `team.WatchReadsPerHour` (1,200) an hour, so no hour sees more than `team.WatchReadBudget` (1,600), under a third of GitHub's 5,000 an hour for the service user, which the pull rounds and the agents share. A watch's read starts only when the budget holds the whole of it, its pull request and every page of CI (`Server.watchReadNeed`, `team.ReadBudget.Has`); once one is held back, the pass makes no more requests, though a watch its reads answer whole still decides from them, the budget builds up for that read under any `-pr-every`, and the next pass starts with it, so each watch is read in turn. An add's one read is not counted. The budget starts full at each start of pocketd.
   - A note filed wakes the delivery pass at once, not after the reads still to come.
   - The end files the watch's one note to its session in the transaction that ends it (`store.EndWatch`, keyed `watch:<owner>`). It is the one code path that files a watch's note. A watch that ended already files nothing, so a pass run again, or after a restart, files no other.
   - The note's text is code (`team.WatchNoteText`): the watch, the pull request and its title, and what pocketd saw. That is CI at the head with the failing checks' names, at most `team.MaxWatchFailingBytes` of them and how many more,, the state, or, at the deadline, how long it waited and what it saw last, or why it could not read it, the pull request or its CI.
6. **The outbox places a watch's note only to an idle session** (`idleOnly`, PiG's `WhenBusyReject`), one with no run and no input waiting in PiG's inbox, read right before the submission (`Server.specialistIdle`): after a failed run an older input may wait there, and PiG would start it and queue the note behind it. So the note never waits in PiG's queue, and a close has no queued copy to withdraw. Right before the note is submitted, pocketd reads that the session's thread, if any, is not closed (`Server.watchNoteGoes`), and a note that is drops.
7. **A thread's close ends its specialist's watches with no note**, whatever closes it, in the transaction that closes it (`store.threadClosed`, and the close of a plan's own thread when its plan is done). A note an ended watch filed that has not gone is dropped there, as a close drops what goes only while its thread is open. A reopened thread's watches stay ended.
8. **The texts say how to wait**, in `team`:
   - A building thread's brief that is no shift's says to open the pull request, call `team_watch` until `checks`, and end the run (`team.WatchGuide`).
   - The supervisor's roster says it can watch a merge before it delegates follow-up work (`team.WatchRoster`).
   - Each says that only an answer that starts `Watching W` promises a message, and ends the run on that alone (`team.WatchReceipt`); an answer at once, or a refusal, comes with no message, and the agent acts on it then.
   - The tool's description says it too, so a session whose instructions were written before has it.
9. **Older sessions get the tool without a new session**, as ADR-0046 decision 5 gave `team_inbox`. At start, `UpgradeTeamTools` gives `team_watch` to every session that gets it now and lacks it (`Engine.SetWatchTool`), in the place a new session has it, with the team's extension if it lacks it: a session made before `team_memory` too, since the caller decides who may. A thread's specialist session an earlier run made before the work database recorded it gets it on the thread's start, before the brief that names it (`Server.recoveredWatchTool`). A standing session takes it with its profile at its idle refresh ([ADR-0026](0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md)).
10. **The protocol does not change.** No wire type moves, and its version stays 35. Rule 25 names the tool, and rule 39a says what it does. The desktop shows no watch yet.

## Consequences

- A specialist waits for its pull request's CI by ending its run, and starts again when pocketd says the checks are done, with the failing checks' names. It never sleeps or polls, and no tool call holds a run.
- A supervisor learns of a merge it waits for, and delegates the follow-up then, with no reminder and no `team_inbox` read in between.
- A watch is as fresh as the pull rounds: a condition met shows at the next round, up to `-pr-every` later. With `-pr-every 0` there are no watches.
- Each live watch costs a read of its pull request, and up to 20 pages of CI for `checks`, in every round. The budget caps what the passes spend an hour, so with many watches of large CI matrices each is read less often than every round, and its news comes later; a team that needs more needs a larger budget.
- The desk's cap is unchanged. pi-pocket-team's watched thread slept outside the cap; a thread whose specialist waits on a watch is on the desk and counts, as any thread not closed does.
- A shift's specialist has no `team_watch`: its rounds wait for CI with `ci`, and a shift without `ci` waits for nothing.
- A dated reminder with no condition is `team_schedule`'s. A watch of an event other than a pull request's state (a workflow run, an issue) is not built.
- A watch's note waits while its session is at work, and holds back the notes filed after it to that session until it goes.
- The owner sees no watch on the desktop yet; a session's `list` shows its own.

## Alternatives considered

- **A tool call that blocks until CI is done**, as `team_wait_for_ci` did: it holds a durable task and a run for the whole wait, a restart resumes it mid-wait, and an abort loses it. A note that starts the next run costs nothing while it waits.
- **Waiting outside the desk's cap, as pi-pocket-team's watches did:** the cap would no longer bound the threads at work, and a thread that sleeps still holds a worktree and a specialist session. The cap stays the one rule of the desk.
- **A timer of its own for the watches:** a second schedule of GitHub reads beside the pull rounds, with no gain in freshness for `merged` and `closed`, which the rounds read anyway. The rounds are the watches' beat.
- **Letting a shift's specialist watch:** its note would start a run outside the shift's rounds. `store.Owed` counts no note, so a round could end while a note is on its way, and the shift's pass would read the run the note starts as a round's. A shift's CI wait is `ci`.
- **Telling the session of every change of its pull requests:** more messages, most of them noise, and no condition in code. A watch names the one change the session waits for.

## References

- Shapes: [design/architecture.md](../design/architecture.md#service), [specs/protocol.md](../specs/protocol.md) (rules 25 and 39a)
- Built by: [plans/roadmap.md](../plans/roadmap.md) (item 42, watches)
- Builds on: [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md), [ADR-0016](0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md), [ADR-0020](0020-plan-before-building-with-the-plan-gate-and-consults-in-code.md), [ADR-0024](0024-run-a-delegated-thread-in-rounds-as-a-shift-driven-in-code.md), [ADR-0026](0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md), [ADR-0030](0030-send-scheduled-messages-from-the-owners-file-and-the-supervisors-tool-in-code.md), [ADR-0042](0042-give-a-private-agent-its-own-preamble-and-no-team-in-its-sessions.md), [ADR-0046](0046-let-the-supervisor-read-the-owners-inbox-from-pocketds-records-in-code.md), [ADR-0047](0047-carry-a-shifts-ci-on-a-proposal-to-its-launch.md)
