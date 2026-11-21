# 0039 — Count a bot's pull request the run left in conflict as waiting for its rebase

- **Status:** Accepted
- **Date:** 2026-11-21

## Context

**A run's own merges make some of its bot pull requests fail.** Merge All Ready checks a dependency bot's waived pull request again once an earlier turn of the run merged into its base ([ADR-0038](0038-check-bot-pull-requests-again-in-the-run-once-a-merge-moves-their-base.md)). Some still stop:
- Dependabot pull requests that change the same lines, such as `go.sum`, conflict once the first one merged.
- Where the rules need a branch up to date, the others are behind the base.

Both clear only when the bot rebases its pull request. The summary counted them as stayed, with the real failures, so the owner could not tell the failures to look at from the pull requests that only wait.

**The owner chose on 2026-11-21.** "Count them apart — the summary says 'N wait for Dependabot to rebase' separately from real failures, so the list of failures stays meaningful." Asked whether an enqueue should move the base for the run too, the owner answered "Leave as is".

## Decision

This amends ADR-0038's summary. Every other part of ADR-0038 holds.

1. **Which item waits for a rebase** (`state.rebaseWait`, decided when its turn ends): all of these hold.
   - It is a dependency bot's pull request: `github.Preview.Bot`, the list's entry for the author as its check read it (`github.DependencyBotOf`).
   - An earlier turn of the same run merged into its base, and the merge was seen to land (`MergeAllItem.Moved`). A merge whose end is unknown is enough to check the item again, never to blame the run for a conflict.
   - Its turn was refused, or approved without a merge.
   - The reason is the gates' own for a conflict with the base or a branch behind it: "the PR has conflicts" or "the PR is behind its base" (`rebaseReasons`, pinned to `github.Check` by a test).
   - The read whose gates refused it found the head and base branch the owner confirmed. `github.Result` now carries that read's `Head` and `Base`, and so does a refused check again. A pull request retargeted since conflicts with a branch the run never moved.
2. **Everything else stays a failure:**
   - a conflict on a base no merge of the run moved, or before its first merge;
   - a person's pull request;
   - a bot's that fails for another reason, such as "GitHub has not computed mergeability".

   A conflict that was there before the run began failed its check, so it was never listed.
3. **The summary counts it apart** (`MergeAllCounts.Rebase`, `RebaseBy`), by the bot's name as its maker writes it: "2 wait for Dependabot to rebase", "1 waits for Renovate to rebase", "Dependabot or Renovate" for both. These items no longer count as stayed.
4. **The list shows it apart:** the ended run lists the failures first, then each wait on its own row with a `rebase` pill. A wait whose approval went in before the merge was refused says "approved as bketelsen, not merged".
5. **Where:** the rule is in `state.GitHub` (`desktop/internal/state/mergeall.go`), and `app` draws it. `desktop/internal/github` gains `Preview.Bot`, `DependencyBotOf` and `Result.Head` and `Base`, and no gate changes.

## Consequences

- The run's list of failures holds only what the owner must look at. The pull requests that wait need no action: Dependabot or Renovate rebases them, pocketd reviews the new heads, and a later run takes them.
- The pull request still stays in the Inbox, its card showing the gates' reason, as before.
- **Known limit:** a conflict an outside merge caused during the run, on a branch the run also moved, counts as a wait too. The run cannot tell the two apart.

## Alternatives considered

- **Keep them as failures (ADR-0038 as accepted):** the owner's list of failures would hold several pull requests that need nothing.
- **Count every conflict of a bot's pull request as a wait:** a conflict the run did not cause, such as one from before the run or on a branch it never moved, may need a person.
- **Put a merge whose end is unknown down to the run:** that merge may not have landed. A conflict after it may have another cause.

## References

- Shapes: [design/architecture.md](../design/architecture.md#desktop-client)
- Built by: [plans/roadmap.md](../plans/roadmap.md) (item 16, Merge All Ready and the verdict as an approval)
- Builds on: [ADR-0038](0038-check-bot-pull-requests-again-in-the-run-once-a-merge-moves-their-base.md), whose summary it amends, [ADR-0031](0031-count-pocketds-ready-cross-review-as-an-approval-and-merge-all-ready-at-once.md), [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md)
