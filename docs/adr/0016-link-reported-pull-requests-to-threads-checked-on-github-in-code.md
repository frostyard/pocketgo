# 0016 — Link reported pull requests to their threads, checked on GitHub in code

- **Status:** Accepted; decision 5's close of a thread whose pull requests merged amended by [ADR-0061](0061-reopen-a-closed-thread-when-a-message-reaches-its-specialist.md) for a reopened thread, closed so only by a merge after its reopen
- **Date:** 2026-11-19

## Context

Phase 2's Done-when has a specialist open a pull request in a thread, and the pull request appear in the owner's Inbox. ADR-0014's threads end when someone closes them, and its consequences leave to this slice the closing of threads whose pull request merged. pi-pocket-team reads pull requests with the `gh` CLI, signed in as the service user, and keeps their state on its desk.

On the team VM the service user's `gh` is signed in as the bot account, with the `repo` scope: agents open pull requests from the sandbox with it today (ADR-0012). The GitHub App token of the design is for the owner's approve and merge, a later slice. ADR-0011 puts every rule that can be stated in code.

## Decision

1. **A specialist reports its pull requests with `team_message`,** as `pull_requests`: at most 10, each a number, and the repository when the thread has none. A worktree thread's pull requests are in its repository.
2. **pocketd checks each one on GitHub before it files the report,** in code:
   - It reads the pull request with `gh api`, read only, in the sandbox, with the service user's sign-in, as git runs there (ADR-0012): bounded output, a 30-second deadline. The `gh` it runs is one only root can change, never one found on the `PATH`: its answer stands for GitHub's.
   - The answer is parsed and checked field by field. The pull request must be in a repository the specialist's profile names (the thread's own, for a worktree thread), and a worktree thread's pull request must come from the thread's branch in that same repository, not a fork.
   - A pull request another thread links is refused.
   - A report with a pull request that fails a check is refused as a whole, with why: nothing is filed, and the specialist can report again.
3. **A linked pull request is a row of the work database** (ADR-0008), with its thread, repository and number, title, link, state (`open`, `closed` or `merged`), whether it is a draft, its head branch and commit, its base branch, and when it was last read.
4. **pocketd keeps the open ones current:** every 5 minutes by default (`-pr-every`), it reads each open pull request again, and records and publishes what changed. One that cannot be read keeps its last state until the next round.
5. **A thread whose linked pull requests are all merged or closed, one at least merged, closes,** with its report saying so: its place on the desk is free.

## Consequences

- The owner sees a thread's pull requests as GitHub has them, never as an agent describes them, and a merged one frees its desk place without anyone closing the thread.
- Protocol version 6: pull requests and their events. The desktop shows them in slice 5.
- Reading pull requests needs no new credential; it shares the bot account's rate limit, about one read per open pull request every 5 minutes.
- A pull request opened before its thread reported it, or one reported by no thread, is not tracked.
- Polling is late by up to 5 minutes; signed webhooks (Phase 5) make it prompt.

## Alternatives considered

- **Trust the agent's report:** a model could report a pull request that does not exist, or another repository's, and the Inbox would show it.
- **The GitHub App token for reading:** it is not set up yet, and reading needs no more than the sign-in agents already use.
- **Webhooks now:** they need a public endpoint and a signing secret; Phase 5 adds them.

## References

- Shapes: [design: system architecture](../design/architecture.md#service), [specs: protocol](../specs/protocol.md), [plans: roadmap](../plans/roadmap.md)
- Set up by: [design: setup guide](../design/setup.md)
- Builds on: [ADR-0008](0008-keep-work-records-in-the-services-own-tables.md), [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0012](0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md), [ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md)
