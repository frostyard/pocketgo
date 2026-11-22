# 0054 — Remove a closed thread's worktree in code, with no GitHub write

- **Status:** Accepted
- **Date:** 2026-11-22

## Context

**pocketd makes a worktree for every thread on a repository, and removes none.** A delegated thread works in `<checkout>--<agent>-<slug>` on its own branch ([ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md) §3, [ADR-0056](0056-start-a-new-worktrees-branch-from-a-fresh-fetch-of-the-remotes-default-branch.md)); a cross-review works in a worktree detached at the head it reviews ([ADR-0019](0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md) §3). ADR-0014 says pocketd removes no half-made or locked worktree it refuses, and states no rule for one whose thread closed. ADR-0016 §5 closes a thread whose pull requests merged and ADR-0024 ends its shift; neither says anything about its folder.

**Nobody else can remove them in time.** The event that makes cleanup due, a merge, also closes the thread, and a closed thread takes no message from its supervisor (`team_send` refuses it), so the after-merge steps never reach the specialist. On 2026-11-22 the team host held 28 clean worktrees of merged pull requests, 13 of them cross-reviews', removed by hand ([frostyard/pocketgo#94](https://github.com/frostyard/pocketgo/issues/94)).

**pocketd writes nothing on GitHub but the label.** ADR-0017 limits pocketd's writes on GitHub to `ready-for-human`. GitHub's "automatically delete head branches" is on in the team's repositories, so a merged head branch goes on its own; the supervisor removes a rare leftover.

**An owner's session worktree stays until the owner removes it** ([ADR-0027](0027-start-the-owners-session-on-a-repository-in-a-new-worktree-named-in-code.md) §6).

## Decision

1. **pocketd removes a closed thread's worktree, and its local branch, in code.** It writes nothing on GitHub: no branch, no comment. ADR-0017's limit stands.
2. **When:** each thread whose close is published (a verdict, a review that ends without one, a merge close, `team_close`, the owner's close, any other close) is queued (`Server.cleanLater`), and the review pass tries it at its end (`Server.cleanPass`), under `reviewMu`, outside `outboxMu` and `workMu`. A thread whose specialist's or worker's session is busy waits for a later pass, so a review's worktree goes only after its run stopped.
3. **What it takes:** only a folder named for the thread (its name ends with `-t<id>-<owner>`, `team.Slug`'s suffix), never one the work database records as an owner's session worktree (`store.SessionWorktreeAt`). git runs in the sandbox, through `Engine.Git`, a review's with `team.ReviewGitOptions`, bounded by `worktreeTimeout`.
4. **The checks before a removal** (`worktree.Remove`): the folder is a worktree git registered, not the checkout itself, and not locked; a task's HEAD is on its branch, a review's detached at the head it reviewed; `git status --porcelain` shows no change and no untracked file, and no index entry is marked assume-unchanged or skip-worktree, which would hide a change from status and from `git worktree remove`'s own check; and a task thread's HEAD is the recorded head of one of its linked pull requests or an ancestor of one (`git merge-base --is-ancestor`). A thread with no linked pull request passes only when HEAD has no commit beyond its branch's start, the oldest entry of the branch's reflog.
5. **The removal:** `git worktree remove <path>`, never `--force`, then `git branch -d <branch>`, never `-D`. A branch git will not delete stays, and pocketd logs why.
6. **A worktree pocketd keeps is named in the thread's report.** When a check or git refuses, pocketd keeps the folder, logs why, adds one line to the closed thread's report (`team.WorktreeKept`, the path and the reason) and decides that thread no more in this run. A write of that line that fails keeps the line for the next pass, so the thread is decided only once its report names the folder. It creates no item in the owner's Inbox, and never tries again with force.
7. **A check that could not tell is tried again** at the next review pass (`worktree.Unsure`), as a review run's stop that fails is (`abortsDue`).

## Consequences

- Worktrees of merged pull requests and finished reviews no longer pile up beside the checkouts. The supervisor's sweep is left with what pocketd kept, each named in its thread's report, and with merged head branches GitHub did not delete.
- A local branch whose tip the checkout's own `HEAD` or its upstream does not contain stays, as `git branch -d` refuses it: a pull request merged by squash or rebase, or a branch pushed without `-u`, leaves its branch.
- The queue lives in memory. A close that a restart of pocketd cuts off before its pass leaves the worktree, as before this decision.
- A closed thread's specialist session keeps the folder as its working directory. Once the folder is gone, a run in that session, the owner's own message included, starts in a folder that is not there.
- A worktree with a sparse checkout, whose index marks entries skip-worktree, is always kept and named in its thread's report.
- Ignored files are not counted as changes: `git worktree remove` deletes them with the worktree.

## Alternatives considered

- **Let the after-merge steps reach the agent** (a `team_send` to a thread closed by a merge, for a bounded time): it depends on a model following the steps, against [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), and leaves a window where the step is lost.
- **Delete the merged head branch on GitHub too:** a new GitHub write beyond ADR-0017's label; GitHub's own setting covers it.
- **Remove with `--force`, or `git branch -D`:** it could delete work that is in no pull request.
- **Ask the owner in the Inbox for each kept worktree:** the report is where the thread's supervisor already looks; the owner's Inbox holds decisions only the owner makes.
- **Keep the queue in the work database:** a migration for a rare restart case; the supervisor's sweep covers it.

## References

- Shapes: [specs: protocol rule 28a](../specs/protocol.md), [design: system architecture](../design/architecture.md#service), [plans: roadmap](../plans/roadmap.md)
- Builds on: [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0012](0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md), [ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md), [ADR-0016](0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md), [ADR-0017](0017-record-cross-review-verdicts-and-set-ready-for-human-in-code.md), [ADR-0019](0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md), [ADR-0024](0024-run-a-delegated-thread-in-rounds-as-a-shift-driven-in-code.md), [ADR-0027](0027-start-the-owners-session-on-a-repository-in-a-new-worktree-named-in-code.md), [ADR-0056](0056-start-a-new-worktrees-branch-from-a-fresh-fetch-of-the-remotes-default-branch.md)
- Issue: [frostyard/pocketgo#94](https://github.com/frostyard/pocketgo/issues/94)
