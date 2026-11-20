# 0017 — Record cross-review verdicts and set ready-for-human in code

- **Status:** Accepted; decision 1, who starts a review, superseded by [ADR-0019](0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md)
- **Date:** 2026-11-19

## Context

**The team's rule today is prompt text.** In the pi-pocket setup, the author of a pull request follows the `cross-review` skill:
- It spawns a reviewer of another model family. The family is the maker of the model, not the provider that serves it.
- The reviewer answers Ready or Not ready.
- The author adds `ready-for-human` itself, after it checks the head and CI again.

Nothing checks that a review happened, at which head, or from which family. [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md) moves such rules into code.

**What is already in place:**
- Core's `pr-triage` workflow removes `ready-for-human` when a pull request's head moves after the label, and when the label comes while CI at the head is not green. Its own check runs are left out of that check ([core ADR-0077](https://github.com/frostyard/core/blob/main/docs/adr/0077-bind-ready-for-human-to-the-head-commit.md)).
- pocketd links the pull requests a thread reports, and reads them on GitHub every 5 minutes ([ADR-0016](0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md)).
- The team has specialists on Claude and on GPT models.

**The approval and the merge are the owner's.** frostyard/pocket approves and merges as the owner, through a GitHub App user token, behind fail-closed gates that require `ready-for-human` ([ADR-0010](0010-port-the-desktop-client-selectively-from-frostyard-pocket.md)). The shared bot account must never both approve and merge one pull request.

## Decision

1. **A cross-review is a thread.** The supervisor delegates the review of a linked pull request to a specialist, as any other task.
   - The reviewer files its verdict with the team tool `team_verdict`: the pull request, the head commit it reviewed, a verdict (`ready`, `not-ready` or `unclear`), a summary, and its findings. A `not-ready` verdict names at least one finding.
   - Every thread's specialist has the tool, offered by role and checked again in `service/internal/api`. The call is keyed by the session, its tool task and the call's id: run again, it answers as it did.
2. **pocketd checks a verdict in code before it records it.** It refuses each of these:
   - a pull request that no thread links;
   - one the reviewer's own thread links;
   - a pull request that is not open;
   - a reviewer whose model is of the same family as the author's (the linked thread's specialist), or a family that `router.Family` cannot tell;
   - a head that is not the one pocketd reads on GitHub at the call.
3. **The family is the model's maker, decided in code (`router.Family`):** by the model id (`claude…` is Anthropic, `gpt…`, `o1…` to `o4…` and `codex…` are OpenAI, `gemini…` is Google). The provider tells nothing: one may serve another maker's models, as GitHub Copilot does, and Mistral serves a Z.ai model. A namespace in the id, as `meta-llama/…`, names the maker, and an unknown one, as `nvidia/…`, gives no family; so does a derivative of another maker's model. An id no rule names is of no known family, and its review does not count.
4. **pocketd keeps `ready-for-human` in step with the verdicts, as the bot account, in the sandbox ([ADR-0012](0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md)).** A linked pull request that is open and not a draft has the label exactly when both of these hold:
   - the newest verdict at its current head is `ready`;
   - CI at that head is green.

   pocketd adds the label, or removes it, when it records a verdict and at every round of `-pr-every`. It removes a label that no such verdict backs, whoever added it. It writes nothing else on GitHub: no review, no approval, no comment and no merge. A pull request that is closed or merged keeps its labels.
   - GitHub labels a number, not a commit. So the label goes on only after a read finds the pull request at the reviewed head, open and not a draft, and comes off again when the read after the write finds a new head.
   - CI that pocketd cannot read is not green, so the label comes off.
5. **CI is green** when every check run at the head is completed with `success`, `neutral` or `skipped`, every commit status there is `success`, and at least one of them exists. Check runs named exactly `pr-triage`, or `pr-triage / pr-triage` (core's reusable workflow), are left out, as core leaves them out: the label's own `labeled` event runs that job. A list GitHub truncates is not green, and a run or status with no name is no answer.
6. **What the owner sees:** a linked pull request carries whether it has `ready-for-human`, the CI state pocketd last read at its head, and the newest verdict at that head. That is protocol version 7.

## Consequences

- A `ready-for-human` label on a linked pull request names a recorded review: by a model of another family, at the head the pull request has now, with green CI. An agent that adds the label by hand loses it within one round.
- A push after a review voids it: the next verdict must name the new head. Core's workflow and pocketd both remove the label then.
- pocketd now writes to GitHub: one label, on linked pull requests only, with the bot account's sign-in. The bot still never approves or merges through pocketd. Approve and merge as the owner, from the desktop, behind the ported gates, is the second part of the slice; until then, the owner merges on GitHub.
- Agents keep raw `gh` in the sandbox. Code cannot stop an agent's own `gh pr merge` or `gh pr review --approve`. GitHub's rulesets hold that, and pocketd's own writes never do either.
- A review costs a thread and a place on the desk, and the supervisor chooses the reviewer: pocketd checks the choice, it does not make it.
- A pull request with no recorded verdict never gets the label from pocketd, also in a repository where the owner used to add it by hand.

## Alternatives considered

- **pocketd starts the reviewer session itself:** it would choose the model and brief the reviewer in code, but it needs a new kind of session, a read-only checkout at the head, and a delivery path for the verdict. Threads give all three today.
- **Trust the author's label, as the skill does:** the rule stays prompt text, which ADR-0011 rules out.
- **Post the verdict as a GitHub review by the bot:** an approving review by the bot account, followed by a merge by the same account, is what the merge rule forbids. A label approves nothing.
- **Leave the removal to core's workflow:** it removes the label only on a head change or a red CI at the label, not when no review backs it.

## References

- Shapes: [design: system architecture](../design/architecture.md#policies-enforced-in-code), [specs: protocol](../specs/protocol.md), [plans: roadmap](../plans/roadmap.md)
- Builds on: [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0012](0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md), [ADR-0013](0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md), [ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md), [ADR-0016](0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md)
