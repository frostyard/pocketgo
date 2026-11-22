# 0059 — Count the extra approval for an unattributed change as GitHub's review decision shows it

- **Status:** Accepted
- **Date:** 2026-11-22

## Context

**The gates ask a bot's pull request for an approval GitHub does not ask.**
- The frostyard org ruleset on `main` (24434288) has a `pull_request` rule with `required_approving_review_count: 1`, `require_extra_approval_for_unattributed_changes: true`, `require_last_push_approval: true` and `dismiss_stale_reviews_on_push: true`. The owner can bypass it (`current_user_can_bypass: always`).
- `approvalsNeeded`, ported from frostyard/pocket's `internal/github` with [ADR-0018](0018-approve-and-merge-as-the-owner-from-the-desktop-with-pockets-gates.md), adds one approval to a rule's count whenever the rule has that flag on and the author's `__typename` is not `User`. `rules.go` takes an absent key as on.
- So PocketGo asked 2 approvals of every pull request by Dependabot, Renovate or github-actions. [ADR-0031](0031-count-pocketds-ready-cross-review-as-an-approval-and-merge-all-ready-at-once.md) made pocketd's ready cross-review count as the second, on the belief that GitHub asks 2.
- ADR-0031 never counts the verdict where a ruleset the owner cannot bypass also has a `pull_request` rule, as frostyard/updex's `main` has. There the gate could not reach 2.
- frostyard/pocketgo#52: on 2026-11-21, about 25 reviewed bot pull requests labelled `ready-for-human` could not merge from the desktop. Each said "the rules need 2 approvals at the head; PocketGo counts 1". The owner merged them with `gh pr merge --admin`. A pull request by `frostyard-machines`, a `User`, needs 1 and merges from the desktop (frostyard/intuneme#303).

**GitHub asks 1 of Dependabot's pull request.**
- frostyard/frostui#46, by `dependabot` (a `Bot`), merged with exactly 1 approval, the owner's, at its head. Its `reviewDecision` is `APPROVED`, read as the owner on 2026-11-22. A pull request GitHub held at 2 would show `REVIEW_REQUIRED`.
- Before the owner's approval, open Dependabot pull requests under the same rule show `REVIEW_REQUIRED` and `BLOCKED` (frostyard/lab#275 and frostyard/lodge#112, read as the owner on 2026-11-22). GitHub's review decision cannot count an approval that was not given yet.
- [GitHub's documentation of the rule](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/available-rules-for-rulesets) calls the setting "Require an additional approval for unattributed Copilot pull requests":
  - it applies when Copilot opens a pull request that is not attributed to a person;
  - such a pull request then needs one more approval than the rule's count, and a count of 0 stays 0;
  - it is on by default, for new and existing rulesets;
  - it is in public preview and subject to change.

  It names no other bot.

**No read of PocketGo's shows that attribution.** The PR query and the bypass query read the author's `__typename` and login. Neither says whether GitHub attributes a Copilot pull request to a person.

**The gates are frostyard/pocket's.** ADR-0018 says a gate changes in frostyard/pocket first, or in a new ADR here. The owner decided on 2026-11-22 to fix it in pocketgo, with this ADR, as a deliberate difference from pocket's port. frostyard/pocket can follow later.

## Decision

1. **The extra approval counts as GitHub's own review decision shows it** (`extraCounts` in `desktop/internal/github/gates.go`). This changes ADR-0018's approval gate (gate 11) in pocketgo's copy of `desktop/internal/github`. For a `pull_request` rule that asks the extra approval (the key true, or absent, which stays on), gate 11 counts one more approval:
   - **Never** for a pull request whose author is a `User`, as before.
   - **Not** when GitHub's `reviewDecision` is `APPROVED`. GitHub counts its review rules met, the extra among them, with its own reviews alone.
   - **Not yet** at the preview while the decision is `REVIEW_REQUIRED` and the owner's approval is not at the head. The preview is Check to Merge, and Act's read before the approval. GitHub cannot count an approval that was not given, so the read after the approval decides.
   - **On every other answer.** That is `REVIEW_REQUIRED` after the owner's approval, or at a preview that finds the owner's approval at the head already. It is also a decision that is null or missing (no review required, which a rule asking for approvals contradicts), and any value PocketGo does not know. A review that asks for changes is refused by gates 8 and 11 first.
   - **One decision, read with the approvals.** The bypass query (`PocketBypass`), which reads the approvals gate 11 counts, reads `reviewDecision` too (`BypassFacts.ReviewDecision`). The PR query's decision must be the same. Two reads that differ, as when a review came or went between them, count the extra approval.
2. **Only the extra is GitHub's to count.** Each rule's own count stays PocketGo's: approvals at the head, by accounts that can push, and the owner's. GitHub's `APPROVED` never lowers it.
3. **Every other gate stays as it is.** These include:
   - the owner's token, the bypass and classic protection;
   - an open pull request that is no draft, whose head is its last commit and that is not in the merge queue;
   - `ready-for-human`, mergeable, every required check present and green, no blocking review;
   - the merge state, and the merge method;
   - who pushed last, unresolved threads, a base ahead, another pull request at the same head;
   - the owner's approval at the head in the read after the approval;
   - the head and base branch bound by the confirmation (`CheckIntent`);
   - pocketd's backing of the button at the head and base branch the gates read (`state.Backed`), and pocketd's fresh answer at the confirmation (`state.FreshPull`).
4. **pocketd's verdict counts as ADR-0031 says.** It counts once, on top, where a rule the owner can bypass asks more than GitHub's approvals at the head make. It never counts where a ruleset the owner cannot bypass asks for reviews. `verdictFills` takes the count of decision 1. So where GitHub says `APPROVED`, the verdict makes up nothing, and the read after the approval holds GitHub's merge state as it does for any pull request: `CLEAN` or `HAS_HOOKS`, and on a merge-queue base the states the queue allows.
5. **Merge All Ready** ([ADR-0031](0031-count-pocketds-ready-cross-review-as-an-approval-and-merge-all-ready-at-once.md), [ADR-0038](0038-check-bot-pull-requests-again-in-the-run-once-a-merge-moves-their-base.md)) takes the same gates on each read. Nothing in `desktop/internal/state` changes.
6. **A deliberate difference from frostyard/pocket's port.** In this one rule, pocketgo's `desktop/internal/github` is no longer pocket's `internal/github`. Every other gate stays as ported.

## Consequences

- **A bot's pull request that GitHub asks 1 approval of merges from the desktop with the owner's approval.** Dependabot's under the org ruleset is one (frostyard/frostui#46). By GitHub's documentation, so is every other bot's but an unattributed Copilot pull request. pocketd's backing is still needed for the button, and the verdict is no longer the second approval.
- **A pull request that GitHub holds at the extra approval** is still offered at the preview. GitHub documents this for an unattributed Copilot pull request. The read after the owner's approval refuses the merge, unless pocketd's verdict makes up the second where ADR-0031 lets it. The attempt then ends `approved`, not merged, with "the rules need 2 approvals at the head; PocketGo counts 1". A later Check to Merge finds the owner's approval at the head and the review still required, and refuses at the preview.
- **The owner's approval can land on GitHub without a merge.** It is the approval the owner asked for, of a pull request labelled `ready-for-human` that passed every other gate. A merge from GitHub's own page, or a second approval, completes it. Act has always ended a refused read after the approval this way.
- **What fails closed:** a missing, null or unknown review decision counts the extra approval, so does a decision the two reads disagree on, and a rule's own count never relaxes on GitHub's word.
- **The bypass query asks one more field.** `reviewDecision` joins `testdata/bypass-query.graphql`, which the recorder sends. The recorded bypass answers predate it, so the tests' fake answers it from the pull request as it is then.
- **GitHub's answer drives the count.** No change here is needed if GitHub changes the preview feature, for example by widening it to other agents or turning it off.
- **Known limit:** GitHub's `APPROVED` can rest on an approval at an older commit where stale reviews are not dismissed. Only the extra approval can rest on such an approval, since the rule's own count needs approvals at the head. The frostyard org ruleset dismisses stale reviews.
- **Not seen yet:** a Dependabot pull request merged from the desktop under this rule ([roadmap item 30](../plans/roadmap.md)).
- frostyard/pocket's gate still asks 2 of a bot's pull request, until pocket takes this rule.

## Alternatives considered

- **Apply GitHub's documented meaning** (frostyard/pocketgo#52, option 1): count the extra approval only for a pull request Copilot opened under its own identity. PocketGo reads no attribution, so it would guess from the login. The feature is a preview that GitHub says may change. A guess that misses a case GitHub adds would count too few and pass a merge that GitHub's rule does not allow, which fails open. Taking GitHub's decision follows GitHub.
- **Trust `reviewDecision` for the whole count:** this drops PocketGo's own count of approvals at the head, which ADR-0018 keeps stricter than GitHub's, for example against an approval at an older commit. Only the extra approval is in doubt.
- **Take `mergeStateStatus` for the count too** (#52, option 2): the merge state already gates the read after the approval. It says nothing about approvals that `reviewDecision` does not say.
- **Keep the extra approval at the preview until GitHub says `APPROVED`:** GitHub can say so only after the owner's approval, which the preview gates. A bot's pull request would then need pocketd's verdict to reach the preview's count. The verdict never counts where a ruleset the owner cannot bypass asks for reviews, as on frostyard/updex, so those pull requests could still never be offered.
- **Turn the flag off in the org ruleset:** it is frostyard's rule (ADR-0031), and GitHub already asks Dependabot's pull requests for 1.
- **Change frostyard/pocket first:** the owner chose pocketgo first, on 2026-11-22.

## References

- Shapes: [design/architecture.md](../design/architecture.md#desktop-client)
- Built by: [plans/roadmap.md](../plans/roadmap.md) (item 30, the extra approval counted as GitHub counts it)
- Builds on: [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0018](0018-approve-and-merge-as-the-owner-from-the-desktop-with-pockets-gates.md), [ADR-0031](0031-count-pocketds-ready-cross-review-as-an-approval-and-merge-all-ready-at-once.md), [ADR-0038](0038-check-bot-pull-requests-again-in-the-run-once-a-merge-moves-their-base.md)
- Issue: [frostyard/pocketgo#52](https://github.com/frostyard/pocketgo/issues/52)
