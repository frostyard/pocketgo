# 0038 — Check bot pull requests again in the run once a merge moves their base

- **Status:** Accepted; its summary amended by [ADR-0039](0039-count-a-bots-pull-request-the-run-left-in-conflict-as-waiting-for-its-rebase.md), which counts a bot's pull request the run's merges left in conflict, or behind, as waiting for its bot to rebase it
- **Date:** 2026-11-21

## Context

**A run merged one bot pull request per repository.** Merge All Ready lists a dependency bot's `risk:high` pull request without its diff opened ([ADR-0031](0031-count-pocketds-ready-cross-review-as-an-approval-and-merge-all-ready-at-once.md), decision 4). Its merge stays bound to the base commit its check read: `state.WaivesDiff` needs a base commit, and `github.CheckIntent` refuses a waived merge whose base moved with "the base moved since it was checked". A merge moves its base branch. So once one pull request of a run merged into a repository's branch, every other waived pull request of the run on that branch stopped with that reason, and needed Check Again and another run. The owner has about 30 such pull requests, several in some repositories.

**The owner chose on 2026-11-21.** Asked what a run should do, the owner chose: "Re-check in the run — after a merge moves a base, the run checks that repository's remaining bot PRs again (every gate, at the new base) and goes on; any that fail stay with their reason."

**What holds already:**
- The run's rules are code in `state.GitHub` ([ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md)).
- A merge needs pocketd's backing in a fresh answer (`state.FreshPull`, made only by `Store.ReadPull` after pocketd's `GET /v1/pulls/{id}`; [ADR-0018](0018-approve-and-merge-as-the-owner-from-the-desktop-with-pockets-gates.md)).
- `desktop/internal/github` is frostyard/pocket's gates, changed only by an ADR. Its gates refuse a pull request behind its base where the rules need it up to date (gates 9 and 11).

## Decision

This amends the last point of ADR-0031's decision 4, "a run merges at most one waived pull request per repository". The rest of ADR-0031 holds.

1. **A turn moves its base** when its merge ended `merged`, or `unknown` at the merge step (`merge` or `reconcile merge`), since that one may have merged (`movesBase`). An approval whose end is unknown sent no merge, and moves nothing.
   - An enqueue (`queued`) moves the base only when the merge queue merges, which the run does not see.
   - Neither does it see a merge from a card or from outside the desktop.
2. **Which items are checked again:** each later item of the run that is not tried yet, is listed without its diff opened (`MergeAllItem.Waived`), and merges into the same branch of the same repository.
   - The same branch is the same name (`Preview.BaseRef`). The same repository is the same owner and name, in any case, as GitHub names them (`sameBase`).
   - Such an item gets `Recheck`.
   - No other item is checked again:
     - one that is not `risk:high` is not bound to a base commit, and Act's own reads re-gate it at the new base, as before;
     - one whose diff the owner opened needs the new diff opened, and Act says "the base moved since you opened the diff".
3. **When:** at the item's own turn, not when the base moves.
   - So an item is checked again once at most, however many merges moved its base.
   - The turns stay one after another, in the order the owner confirmed.
   - The run always ends.
4. **How:** `GitHub.Check` with the item's row as pocketd backs it now, the same read as the run's first checks. It reads every gate at the new base, and counts pocketd's cross-review of the row (ADR-0031).
   - Only the run's own check counts. A check begun from the item's card may have read the base before it moved: the run waits for it to end, then begins its own.
   - The run goes on only when the check it waits for ended, whatever other check ends meanwhile.
   - While it reads, `MergeAll.Rechecking` is set, and the Inbox's bar adds "checking it again at its new base".
5. **What the new check must hold:** everything the owner confirmed but the base commit (`changedSince`):
   - the pull request and its head;
   - its base branch;
   - the method and the merge queue;
   - `risk:high`, and a dependency bot as its author;
   - the GitHub sign-in's token generation it was read with: a check read after the token was refreshed is refused, as `ApproveAndMerge` refuses one ("the GitHub sign-in changed since the check").

   Then `canMerge` with the waiver decides, as for the list. An item that fails stays in the Inbox with its reason, on its card too, and the rest go on. Some reasons:
   - "the PR is behind its base", where the rules need a branch up to date. Dependabot rebases its own pull requests; the next run takes the new head once pocketd backs it.
   - "new commit since you confirmed".
   - "the PR has conflicts".
6. **Then the turn goes on as every turn does.** pocketd is read fresh (`Store.ReadPull`, a new `FreshPull`). `ApproveAndMerge` takes the item with the new check, the one it holds to. The waived merge binds to the base commit the new check read, and Act reads every gate again before each mutation.
7. **Stop and sign-in:** Stop, a new GitHub sign-in or a GitHub sign-out while the check reads end the run once that read ended, so no new run begins while it is under way. That item and the rest are not tried.
   - GitHub's rate limit met by that read ends the run the same way.
   - A change of pocketd's sign-in drops the run at once, as before.
8. **The summary** counts the items checked again: "N checked again at a new base". An item that stayed after one says "checked again at its new base".
9. **Where:** `state.GitHub` holds the rules (`mergeAllRecheck`, `changedSince`, `sameBase` and `movesBase`, in `desktop/internal/state/mergeall.go`), and `app` draws them. Nothing changes in `desktop/internal/github`, the protocol or pocketd.

## Consequences

- **More merges a run:** a run merges each bot pull request on one branch whose gates pass at the new base, one after another.
- **The cost:** each item checked again costs one more preview: the pull request query, the rules and, where the owner can bypass, the proof. For n waived pull requests on one branch, that is n−1 more previews.
- **GitHub may lag behind the move.** When two items on one branch are next to each other in the list, the second is checked right after the first merged. GitHub computes mergeability again in the background after a base moves. The read waits 2 seconds once while it is `UNKNOWN`. An item still unknown then stays with "GitHub has not computed mergeability".
- **Conflicts:** Dependabot pull requests that change the same lines, such as `go.sum`, conflict once the first merged. They stay with "the PR has conflicts" until Dependabot rebases them.
- **Branches that must be up to date:** where the rules need a branch up to date, a run still merges one pull request a branch. The others stay "behind its base", and the owner runs again once Dependabot rebased them and pocketd backs their new heads.
- **Merge queues:** on a merge-queue base, the run does not see when the queue merges. A waived item there may still stop with "the base moved since it was checked".
- **The confirmation now covers a base commit it did not show.** It holds the head, the method and the rest as the owner confirmed them; only the base commit may move. This is the owner's choice.

## Alternatives considered

- **Check every remaining item on the branch again as soon as a merge lands:** each item would be read again after every move: n(n−1)/2 more previews for n on one branch, against n−1.
- **Check every item again at its turn, moved or not:** one more preview for each item, with nothing gained where no base moved.
- **Check items that are not waived again too:** their merge is not bound to a base commit, and Act's reads re-gate them at the new base anyway.
- **Keep one waived merge a repository a run (ADR-0031 as accepted):** the owner's 30 would take a run for each pull request in a repository.
- **Order the turns so that one repository's items are apart:** GitHub would have more time to compute mergeability. But the run would not go in the order the owner confirmed, and a run in one repository gains nothing.
- **Wait in the run for Dependabot to rebase:** a rebase is a new head, which needs pocketd's new cross-review; the run would wait minutes or hours.

## References

- Shapes: [design/architecture.md](../design/architecture.md#desktop-client)
- Built by: [plans/roadmap.md](../plans/roadmap.md) (item 16, Merge All Ready and the verdict as an approval)
- Builds on: [ADR-0031](0031-count-pocketds-ready-cross-review-as-an-approval-and-merge-all-ready-at-once.md), whose decision 4 it amends, [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0018](0018-approve-and-merge-as-the-owner-from-the-desktop-with-pockets-gates.md)
