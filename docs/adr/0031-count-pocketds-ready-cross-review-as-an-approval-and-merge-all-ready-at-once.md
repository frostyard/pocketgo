# 0031 — Count pocketd's ready cross-review as an approval, and merge all ready pull requests at once

- **Status:** Accepted
- **Date:** 2026-11-20

## Context

**The owner could not merge Dependabot's pull requests.** On 2026-11-20, about 30 pull requests waited in the Inbox, each with green CI, `ready-for-human` and pocketd's ready cross-review at its head. Each card said "the rules need 2 approvals at the head; PocketGo counts 1". One was frostyard/porter#53, by `dependabot[bot]`, a `Bot` on GitHub, with no review on GitHub.

**The rules ask 2 approvals of a bot's pull request.** The frostyard org ruleset on `main` (24434288) has a `pull_request` rule with these parameters:
- `required_approving_review_count: 1`;
- `require_extra_approval_for_unattributed_changes: true`, so an author that is not a `User` needs one more (`approvalsNeeded`, [ADR-0018](0018-approve-and-merge-as-the-owner-from-the-desktop-with-pockets-gates.md));
- `require_last_push_approval: true`;
- `dismiss_stale_reviews_on_push: true`.

**PocketGo merges by bypass, only once it proves each rule itself.** The owner can bypass the ruleset. PocketGo merges only when its own reads prove each rule the bypass skips (frostyard/pocket's ADR-0011, ported by ADR-0018). For approvals, it counts the GitHub approvals at the head, by accounts that can push, plus the owner's own. A bot's pull request with no review on GitHub has the owner's approval alone: 1 of 2.

**pocketd's cross-review is the one agent review.** pocketd starts every cross-review in code, with the author's agent on a reviewer model of another family ([ADR-0019](0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md)). A verdict counts only at its head, base branch and merge base, and the protocol shows only such a verdict (protocol rule 45). pocketd writes nothing on GitHub but `ready-for-human` ([ADR-0017](0017-record-cross-review-verdicts-and-set-ready-for-human-in-code.md)), so GitHub never shows the verdict as an approval, and the gates never counted it.

**One pull request at a time is 90 clicks for 30.** Each card needs Check to Merge, Approve + Merge and a confirmation (ADR-0018). ADR-0018 left out pocket's batches, which need its operation journal.

## Decision

The owner decided both on 2026-11-20.

1. **The gates count pocketd's ready cross-review as one approval.** This amends ADR-0018's approval gate in pocketgo's copy of `desktop/internal/github`.
   - `github.CrossReview` is pocketd's verdict as the desktop read it: the verdict, its head and the base branch. `state.CrossReviewOf` builds it from pocketd's row: the newest verdict at the head pocketd read, on an open pull request that is no draft. pocketd shows only a verdict at the pull request's head, base branch and merge base.
   - `bypassGate` counts it once, next to the GitHub approvals at the head and the owner's own, and only when all of these hold:
     - its verdict is `ready`;
     - its head is the head the gate reads;
     - its base is the base branch the gate reads.
   - It never counts where a pull_request rule of a ruleset the owner cannot bypass applies. GitHub enforces that rule at the merge, with its own reviews alone.
   - **GitHub's own view after the approval:** GitHub counts only its own reviews. Where the verdict makes up the approvals a bypassable rule needs, GitHub still says `REVIEW_REQUIRED` and `BLOCKED` after the owner's approval (`verdictFills`). The read after the approval takes those two then, and only then: `BLOCKED` only with `REVIEW_REQUIRED`, on a merge-queue base as on any other, since a merge GitHub blocks while it calls the review approved is blocked by something else. The owner's bypass merges the pull request, gate 11 has proven every other bypassable rule, and GitHub enforces at the merge each rule the owner cannot bypass.
   - The preview, Check to Merge, counts the verdict of the desktop's row. Approve + Merge counts the verdict of pocketd's answer at the confirmation (`state.FreshPull`), which backs the confirmed head (`state.Backed`). Act reads the gates again before each mutation, and each read counts only that verdict.
   - Every other gate stays as it is: a review that requests changes, who pushed last, unresolved threads, a base ahead, the merge methods, another pull request at the same head, the owner's approval in the read after the approval, and pocketd's backing of the button.
   - When the count falls short, the refusal says whether it counted a ready cross-review: "PocketGo counts 1: no ready cross-review by pocketd at <head>".
2. **Merge All Ready.** The Inbox's pull requests get one button, Merge All Ready.
   - **Check:** it reads the gates of each pull request pocketd backs at its own head, as Check to Merge does, 8 at a time (`mergeAllChecks`), so one click stays well under GitHub's limit on concurrent requests. A check that counted another cross-review than the row's is read again, such as one begun before pocketd's verdict came. Only a check the run read lists a pull request.
   - **Confirm:** once every read has ended, one confirmation lists each pull request whose Approve + Merge is on (`GitHub.CanMerge`): its reference, the title GitHub read, its head and method. The confirmation opens when the last read ends, not at a click, so the focus starts on Cancel: Enter meant for something else never confirms the list. A pull request whose gates or backing fail is not listed, and its card says why. So is a `risk:high` one whose diff the owner has not opened, unless decision 4 waives it.
   - **Merge:** on the confirmation, PocketGo approves and merges the listed pull requests one at a time, as Approve + Merge does. Each is read from pocketd fresh at its turn (`Store.ReadPull`). `ApproveAndMerge` takes it with the check the confirmation listed, and refuses a check read since. Act then reads the gates again before each mutation.
   - **A failure:** a pull request that fails at its turn stays in the Inbox, its card showing the result and its reason, and the rest go on.
   - **Stop and sign-in:** Stop ends the run once the merge under way ends. A new GitHub sign-in, or a sign-out, ends the run too, and the rest are not tried. So does a change of pocketd's sign-in (`Options.SignInChanged`): the store drops an answer of the sign-in before, and a run's rows and reads belong to the sign-in it began under. The run, and its summary, leave the view with that sign-in at once, even with a merge under way on GitHub; the activity log keeps how the run ended, and the merge under way ends on its own and shows on its card. GitHub's rate limit on the owner's token ends the run too, whether a check or a merge met it, and the rest are not tried.
   - **The summary:** it counts apart the pull requests merged, those added to the merge queue, which may yet leave it unmerged, those whose end is unknown, which may have merged, those that stayed, and those not tried.
   - **Where:** the flow's rules are in `state.GitHub` (`MergeAllReady`, `ConfirmMergeAll`, `StopMergeAll`, `MergeAll`); `app` only draws them.
3. **Still no batches.** pocket's batches and the operation journal stay out (ADR-0018, decision 6). The run is one Approve + Merge after another, each with `Intent.Mode` `single`. A run cut off by a stop or a crash leaves GitHub's own record, and the next Check reads where each pull request stands.
4. **The list waives Open Diff for a dependency bot's `risk:high` pull request.** The owner chose this on 2026-11-21: Dependabot's pull requests that touch workflows or `go.mod` are L1, so they carry `risk:high`, and opening each one's diff kept them out of the list.
   - **Who counts:** a dependency bot is an author GitHub's GraphQL names a `Bot` whose login is on a list in code (`github.DependencyBot`): `dependabot` and `renovate`. A Bot's login is its App's slug, which no other account has. An App the agents act through, such as `frostyard-machines`, is a Bot too, and is not on the list. The PR query reads the author (`author { __typename login }`).
   - **Where:** only Merge All Ready's list (`GitHub.canMerge` with the waiver, `WaivesDiff`). The confirmation says which pull request goes without its diff opened. A card's Approve + Merge still needs Open Diff, also for a dependency bot's pull request, and so does any other author's `risk:high` pull request in the list.
   - **Checked again on GitHub:** the run's merge sends `Intent.WaiveDiff`. `CheckIntent` skips the diff rule only while the preview and that read of Act both find the author on the list. Every other gate, the base binding included, stays.

## Consequences

- A pull request by Dependabot, or by any author that is not a `User`, merges from the desktop under the org ruleset. The owner's approval and pocketd's ready cross-review at the head make the 2 approvals.
- A rule that needs 3 approvals still refuses with these 2, and a `User`'s pull request that needs 1 merges as before.
- **The verdict now counts for more.** Beyond the label, a reviewer model's `ready` is one of a bot's two approvals. ADR-0019's limits hold it:
  - a reviewer of another family;
  - the head, base branch and merge base;
  - at most 4 reviews a pull request.
- **GitHub's record shows the owner's approval alone,** and a bypassed rule. The verdict lives in pocketd's work database. Someone who reads only GitHub sees one approval.
- **The merge now relies on the owner's bypass of the approval count,** which before this ADR GitHub's own review decision had to agree with. Whether GitHub's merge mutation goes through for the owner with 1 of 2 approvals is seen only on GitHub. If it does not, the attempt ends `approved`, not merged, as a refused merge always has.
- A pull_request rule of a ruleset the owner cannot bypass keeps the verdict out of the count on that base.
- **Known limit:** the desktop pairs pocketd's verdict with GitHub's read by repository, number and head (ADR-0018's known limit). The count inherits it.
- Merge All Ready reads the gates of every backed pull request, 8 at a time: one preview each, as many requests as 30 Check to Merge clicks.
- A run of 30 takes about as long as 30 single merges, one after another. The owner can stop it between merges.

## Alternatives considered

- **pocketd approves on GitHub with the bot's account:** pocketd writes nothing but the label (ADR-0017). An approval would put the bot's token on the path to every merge.
- **Change the org ruleset:** the extra approval is frostyard's rule for every repository, and not pocketgo's to drop.
- **Count a verdict at any head:** a verdict on an older head is no review of the code merged.
- **Port pocket's batches:** they need the journal and the Desk, which pocketgo does not have (ADR-0018).
- **List the pull requests checked already, with no new read:** a list of stale checks would confirm what the gates said minutes ago. The owner asked for the pull requests whose gates pass now.

## References

- Shapes: [design/architecture.md](../design/architecture.md#desktop-client)
- Built by: [plans/roadmap.md](../plans/roadmap.md) (item 16, Merge All Ready and the verdict as an approval)
- Builds on: [ADR-0010](0010-port-the-desktop-client-selectively-from-frostyard-pocket.md), [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0017](0017-record-cross-review-verdicts-and-set-ready-for-human-in-code.md), [ADR-0018](0018-approve-and-merge-as-the-owner-from-the-desktop-with-pockets-gates.md), [ADR-0019](0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md)
