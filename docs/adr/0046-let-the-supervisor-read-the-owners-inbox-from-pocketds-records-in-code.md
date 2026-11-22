# 0046 — Let the supervisor read the owner's Inbox from pocketd's records, in code

- **Status:** Accepted
- **Date:** 2026-11-21

## Context

**The supervisor cannot see what the owner already did.** On 2026-11-21 the owner said: "the supervisor needs a read tool so they can see the status of asks and PRs in the inbox. Odrade keeps reminding me to do things i've already done."
- Odrade, the supervisor, runs in one standing session ([ADR-0026](0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md)), compacted early. What she was told about an ask or a pull request may be gone from her context.
- An owner's answer reaches the asking specialist's session, not the supervisor's ([ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md)). A merge on GitHub reaches no session: pocketd's round reads it, and closes a thread whose pull requests all merged ([ADR-0016](0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md)).
- The supervisor's team tools delegate, close, send, schedule, gather, keep the board and launch. None of them reads the owner's Inbox.

**pocketd records most of it already**, in the work database:
- An ask keeps its question, when it was made, its state, its answer, the device that answered it and when (`answered_at`), and when a supervisor's message withdrew it (`withdrawn_at`, [ADR-0037](0037-unblock-a-thread-when-the-supervisors-message-reaches-it.md)).
- A pull request linked to a thread keeps its state (open, merged or closed), its draft flag, its `ready-for-human` label and CI at its head as pocketd last saw them ([ADR-0017](0017-record-cross-review-verdicts-and-set-ready-for-human-in-code.md)), when pocketd last read it on GitHub (`checked_at`), and when pocketd last recorded a change of it (`updated_at`). Its verdict at its head is a review row ([ADR-0019](0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md)).
- pocketd keeps **no time of a merge or a close**. GitHub's answer names `merged_at`, but pocketd keeps only the state. A merged pull request is read no more, so its `updated_at` is the read that found it merged; a closed one of a thread not closed is read in every round, and its `updated_at` moves only with a change.
- pocketd reads each open pull request again every `-pr-every`, 5 minutes by default, and 0 for no rounds. A merge on GitHub shows in pocketd's records at the next read.
- A pull request no thread reported, as one the owner or a bot opened, is not in pocketd's records at all.
- The desktop's Inbox is built from `GET /v1/asks`, `/v1/pulls`, `/v1/plans`, `/v1/lessons` and `/v1/approvals`. It counts an open pull request that is no draft (`state.ToReview`), a plan in review with its revision's graph and an empty gate (`state.ToApprove`), a lesson that waits (`proto.Lesson.Waits`), every open ask and every pending approval.

## Decision

1. **A supervisor's session gets `team_inbox`, and no other session does** (`roleTools`). It comes after `team_launch` in the team extension's order. `service/internal/api` checks the caller again (`Server.TeamInbox`, `team.CheckInboxCaller`): only a session whose profile is a supervisor's, as every other supervisor tool checks it. A private agent's session is refused before, as for every team tool ([ADR-0042](0042-give-a-private-agent-its-own-preamble-and-no-team-in-its-sessions.md)).
2. **It takes no arguments, reads pocketd's records alone, and writes nothing.** It reads the queries the Inbox's endpoints answer from (those of `store.OpenAsks`, `store.PullsWithReviews`, `store.Plans`, `store.LiveLessons` and `store.PendingApprovals`), what was settled since the window's start (those of `store.SettledAsks` and `store.SettledPulls`), and the threads they name, all in one read transaction (`store.Inbox`): an answer, a merge or a close recorded while it reads is in none of its lists or in all, so no ask or pull request is both waiting and settled in one answer. It runs no gh and calls no GitHub. A call run again reads again.
3. **Its answer is text in code** (`team.InboxText`), in two parts:
   - **Waiting for the owner now**, as the desktop's Inbox counts it (`team.PullWaits`, `team.PlanWaits`, `proto.Lesson.Waits`): each open ask with its thread, who asked, its question and how long ago; each pull request to review as `owner/name#number` with its thread, title, state, `ready-for-human`, the verdict at its head, CI when pocketd read it, and when pocketd last read it; each plan to approve; each lesson to decide; each tool call to approve.
   - **Settled in the last 7 days** (`team.InboxWindow`), counted back from pocketd's clock: each ask answered, with when, the device and its answer cut short, or withdrawn by a supervisor's message, with when; each pull request merged or closed whose record changed in the window, with that time, named as the record's last change. The answer says pocketd keeps no time of a merge or close: the time is the read that found it merged or closed, or a later change of its title, label or other fields.
   - Its first line says the answer comes from pocketd's records alone, and how fresh the pull requests are: every `-pr-every` interval, no rounds, or rounds not begun since pocketd started.
4. **Its answer is bounded in code:** at most `team.MaxInboxBytes` (24 KiB) and `team.MaxInboxLines` (400 lines). A longer answer keeps the same number of first items of every list, the most that fit, so the settled lists are never pushed out by the waiting ones. Its top says it was cut, and each cut list's heading says how many items it has. Each question, answer, title and answering device's name is one line, cut to a fixed number of characters, so no item pushes the others out. The tool's output limits let a whole answer through.
5. **A supervisor's session started before the tool gets it** without a new session:
   - An older session that is not standing gets it at start (`UpgradeTeamTools`, `Engine.SetSupervisorTools`, which gives every later supervisor tool it lacks, in its place).
   - A standing session takes its profile's tools at its idle refresh (`Engine.Reconfigure`, [ADR-0026](0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md)), which the first delivery pass after a start runs. So Odrade's standing session gets `team_inbox` after a deploy and restart, once it is idle.
6. **The supervisor checks it first.** The supervisor's instructions say, in the text pocketd writes (`team.InboxGuide`, in `team.Roster`): before you remind the owner of an ask or a pull request, check `team_inbox`. The tool's description says it too, so a session whose instructions were written before has the rule.
7. **The protocol does not change.** No wire type moves, and its version stays 24. Rule 25 names the tool, and rule 25a says what it reads and answers.

## Consequences

- The supervisor can tell, before it reminds the owner, that an ask was answered, and when, and that a pull request merged or closed.
- What waits in the supervisor's answer is what the desktop's Inbox counts, by rules written twice: in `desktop/internal/state` and in `service/internal/team`, which cannot import the desktop's module. A change to one needs the same change to the other.
- A pull request merged on GitHub shows as open until pocketd's next read: up to `-pr-every` later, or never in rounds with `-pr-every 0`. Each pull request says when pocketd last read it, so the supervisor can tell a stale one.
- A merged or closed pull request's time is pocketd's, not GitHub's: when its record last changed. A pull request merged while pocketd stopped shows the time of its first read after the start. A closed one of a thread not closed is read in every round, so a later change of its title or label moves its time, and one closed long ago can be listed as settled in the window.
- A pull request no thread reported is not listed, open or merged.
- Settled plans, lessons and tool calls are not listed: only asks and pull requests, which the owner named.
- The window is fixed at 7 days, and the answer has no filter. A busy week cuts every list to its first items, and the answer says so.
- Every call reads the whole Inbox: five list queries, two settled queries, and one read of each thread named.

## Alternatives considered

- **GitHub read at call time:** fresher, but each call would spend the service user's rate limit, and it could fail or hang. pocketd's rounds read open pull requests already, and the answer says how fresh they are.
- **A new column for GitHub's merge or close time:** a migration and a change to every read. Not needed to stop a reminder: the state is what settles it, and the answer is honest about the time it gives.
- **Telling the supervisor of every answer and merge as a message:** more messages in a standing session that is compacted early, and the reminder comes from context it lost. A read when it needs one costs nothing between.
- **Only the asks and pull requests of the supervisor's own threads:** the owner's Inbox shows everything, and asked for the inbox's state.
- **Paging, as `team_board` does:** the answer is one view of the Inbox; a page read later could differ. A bounded answer that says it was cut is enough at the team's size.

## References

- Shapes: [design/architecture.md](../design/architecture.md#service), [specs/protocol.md](../specs/protocol.md) (rules 25 and 25a)
- Built by: [plans/roadmap.md](../plans/roadmap.md) (item 21, the supervisor's view of the owner's Inbox)
- Builds on: [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md), [ADR-0016](0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md), [ADR-0017](0017-record-cross-review-verdicts-and-set-ready-for-human-in-code.md), [ADR-0019](0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md), [ADR-0026](0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md), [ADR-0037](0037-unblock-a-thread-when-the-supervisors-message-reaches-it.md), [ADR-0042](0042-give-a-private-agent-its-own-preamble-and-no-team-in-its-sessions.md)
