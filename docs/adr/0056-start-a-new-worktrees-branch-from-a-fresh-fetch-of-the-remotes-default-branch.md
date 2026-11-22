# 0056 — Start a new worktree's branch from a fresh fetch of the remote's default branch

- **Status:** Accepted
- **Date:** 2026-11-22

## Context

**A new branch starts from the checkout's `HEAD`.** `team_delegate` with a repository makes the thread's worktree with `git worktree add -b <branch> <path> HEAD` in the profile's checkout ([ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md) §3), and a session the owner starts in a new worktree does the same ([ADR-0027](0027-start-the-owners-session-on-a-repository-in-a-new-worktree-named-in-code.md) §3), with no fetch. Only a cross-review fetches, the pull request's head ([ADR-0019](0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md)).

**That `HEAD` can be stale.** The checkout's local `HEAD` is wherever the last person or agent left it. On 2026-11-21 a thread on frostyard/intuneme started at `2cde560`, behind `origin/main` (`5e221ea`), without the change its task needed; its specialist noticed and reset by hand ([frostyard/pocketgo#50](https://github.com/frostyard/pocketgo/issues/50)). A specialist that does not notice builds on old code, and its pull request shows unrelated diffs or conflicts.

**A fetch can fail.** The remote may be unreachable, its sign-in missing in the sandbox, or the checkout may have no `origin`. Refusing the delegation then would stop work that a stale base only slows.

**ADR-0027 chose threads' behaviour for a session's worktree.** It rejected a fresh fetch for sessions alone, so that a session's worktree branches as a thread's does.

## Decision

1. **A new branch starts at the tip of `origin`'s default branch, fetched first.** `worktree.Add` (a thread's worktree) and `worktree.AddNew` (a session's), when they make a new branch, run `freshBase` in the checkout first: `git ls-remote --symref origin HEAD` names the default branch, `git check-ref-format` checks its name, `git fetch --no-tags --no-write-fetch-head origin +refs/heads/<default>:refs/remotes/origin/<default>` fetches it, and `git rev-parse` reads the fetched commit. The branch starts at that commit's full id, so git sets no upstream that tracks the default branch. FETCH_HEAD is not written, since a cross-review reads it in the same checkout.
2. **git runs as every other git of a worktree:** in the sandbox, through `Engine.Git`, with its bounded output, inside the call's own deadline. `worktree.FetchTimeout` (2 minutes) bounds the lookup and the fetch within it, so a fetch that hangs leaves time to go on.
3. **A failed lookup or fetch starts from `HEAD`, and says so.** When the caller's deadline still holds, the branch starts at the checkout's `HEAD`, and `worktree.Start.Stale` gives the reason on one line of at most `worktree.MaxStale` bytes. A thread's brief then carries `team.StaleBase`, one line that names the reason. A session's start logs it. A call whose own deadline ended makes nothing, as any check that could not tell (`worktree.Unsure`).
4. **The retry rule does not change.** A folder or a branch of the name is what an earlier try of the same thread made: `Add` finishes from it, fetches nothing and moves no branch. A session's request resumes from its recorded step as before.
5. **This supersedes "from the checkout's `HEAD`" in ADR-0014 §3 and "from the checkout's `HEAD`, with no fetch" in ADR-0027 §3.** The rest of both stands.

## Consequences

- A thread starts from the code its task was written against, and its pull request shows only its own change.
- A delegation and a session start make two network calls in the sandbox before git makes the worktree. A slow remote delays the start by at most `FetchTimeout`.
- The fetch updates `refs/remotes/origin/<default>` in the shared checkout, as a plain `git fetch` would; it leaves the checkout's own branch and `HEAD` as they are.
- A specialist whose brief says the branch starts from `HEAD` knows to fetch and compare first. A session's owner sees it in pocketd's log alone: the protocol does not carry it.
- The note reaches the brief only from the try that made the branch. A restart between the worktree and the brief's record leaves the brief without it.
- The remote is `origin` by name. A checkout whose remote has another name starts from `HEAD`, with the reason.

## Alternatives considered

- **Refuse the delegation when the fetch fails:** a stale base only slows the work, which a refusal would stop. The supervisor would delegate again, to the same result.
- **Branch from `origin/<default>` by name:** git would set the default branch as the new branch's upstream, and a plain `git push` would aim at it.
- **Read the default branch from the local `refs/remotes/origin/HEAD`:** a checkout made without `git clone` lacks it, and it does not follow a rename of the remote's default branch.
- **Fetch the remote's `HEAD` into FETCH_HEAD and read it back:** one call fewer, but another fetch in the checkout, a cross-review's included, could write FETCH_HEAD in between.

## References

- Shapes: [specs: protocol rules 6a and 26](../specs/protocol.md), [design: system architecture](../design/architecture.md#service), [plans: roadmap](../plans/roadmap.md)
- Set up by: [design: setup guide](../design/setup.md)
- Builds on: [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0012](0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md), [ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md), [ADR-0019](0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md), [ADR-0027](0027-start-the-owners-session-on-a-repository-in-a-new-worktree-named-in-code.md)
- Issue: [frostyard/pocketgo#50](https://github.com/frostyard/pocketgo/issues/50)
