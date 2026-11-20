# 0019 — Start each cross-review in code, with the author's agent on the reviewer model

- **Status:** Accepted
- **Date:** 2026-11-19

## Context

**Nothing started a cross-review.** ADR-0017 records verdicts and keeps `ready-for-human` in code, but it left the start of a review to the supervisor's judgment. Nothing in code asks for one.

On 2026-11-19, Sheeana reported bketelsen/pi-pocket-config#81 in thread T7 and followed pi-pocket's cross-review skill, which starts a reviewer subagent; pocketgo's sessions have no subagent. T7 was closed, so Sheeana could not ask Odrade either. No review ran. Without a verdict there was no label, and the owner could not merge #81 from the desktop (ADR-0018).

**One owner per job.** The owner's rule, given the same day after pi-pocket-team asked him things twice and told Odrade of one event twice: exactly one process or agent is responsible for each job. In pi-pocket the author started its own review. In pocketgo, the job goes to code.

**The owner's review policy exists.** pi-pocket-config's skill `cross-review` holds it:
- the reviewer model, by the family of the model that wrote the change: a Claude author gets `openai/gpt-6.1-sol`, a GPT author `github-copilot/claude-sonnet-5.5`, and another known family `openai/gpt-6.1-sol`;
- 1 review, then at most 3 re-reviews after fixes, each covering the change since the last reviewed head;
- a read-only reviewer, bound to the base and head it read.

The owner decided on 2026-11-19: pocketd starts the review, with the same agent on the cross-review model.

**What pocketd can run.** pocketd's models on the team VM include `openai-codex/gpt-6.1-sol`, but no `github-copilot/*` model is signed in. Family is the model's maker, decided in code (`router.Family`, ADR-0017), whoever serves it.

## Decision

1. **pocketd starts each cross-review, and nothing else does.** It starts one for a linked pull request when all of these hold:
   - the pull request is open, not a draft, and pocketd read its head;
   - no review was started at that head;
   - its budget is not spent.

   pocketd checks when it links a report's pull requests, and at every round of `-pr-every`, after it reads the head. A review is keyed by its pull request and head (`review:<repository>#<number>:<head>`, the repository in lower case). It is recorded only while the pull request, as stored then, is open, no draft and at that head. A verdict on a pull request GitHub reads as a draft is refused. Its thread is recorded with the key in one transaction, so a run again, a restart or a second round starts none. A head that a verdict already backs, filed before this decision, gets none. What a start needs is checked before the review takes a place on the desk: the author's checkout in its profile, matched in any case as GitHub matches names, its profile's tools, and the reviewer model on a sign-in.
2. **The reviewer is the author's own agent, on the reviewer model.** The author is the specialist of the thread that links the pull request, and its family is the family of that thread's session model. `router.Choose` picks the reviewer model by that family and records the rule `cross-review`:
   - Anthropic: `openai/gpt-6.1-sol`, on the `openai-codex` subscription (ADR-0013's alias);
   - OpenAI: `anthropic/claude-sonnet-5-5`, the same maker as the owner's `github-copilot/claude-sonnet-5.5`, which pocketd cannot run;
   - any other known family: `openai/gpt-6.1-sol`;
   - no known family: no review. The owner decides.

   ADR-0017's checks of a verdict still hold: the review is another thread, on a model of another family.
3. **The review thread is pocketd's.** Its supervisor is the author thread's supervisor, and its repository is the pull request's. Its folder is a worktree detached at the head, fetched as `pull/<number>/head` when the checkout does not hold it, and checked to be that commit with no change of its own, also when its start recovers or its brief goes again. Every git command on it ignores replacement objects, so the commit is GitHub's, and runs no hook, fsmonitor, signature program, pager or external diff of the checkout.
   - **Its base** is the merge base GitHub's compare names, read with the service user's sign-in and kept on the thread, so the brief and every diff name one change set. It must be readable in the worktree before the reviewer is briefed: a checkout that lacks it fetches the base branch, and no session starts until it is there.
   - **Its agent has no shell:** `read`, if its profile has it, `team_diff` and `team_verdict`. `team_diff` is code: `git diff` or `git log -p` from the base, or from the last reviewed head, to the head, in the review's worktree, in the sandbox, with the git options above and no signature shown, only for paths in the folder, and paged by byte offset, so no patch is too long to read. A shell, even in the sandbox, could write other checkouts and reach GitHub as the service user.
   - **No other team tool:** it gets no `edit`, `write`, `team_message` or `team_memory`, and those refuse a review's call whatever its tool list holds. No other thread gets `team_verdict`, and `team_verdict` refuses a call from any other thread: a review the supervisor delegated would be a second owner. A session that had `team_verdict` before this decision loses it at pocketd's start.
   - **Its brief is code** (`team.ReviewBrief`). It names the pull request, its base and head commits, and `team_diff`. It says the review is read-only. For a re-review, it names the last reviewed head, its verdict and its findings. It says how to file the verdict: each finding as `<severity> <blocking or non-blocking>: <the point>`. `team.CheckReviewFindings` refuses another form, a not-ready verdict with no blocking finding, and a ready one with one.
4. **A verdict ends the review.** pocketd records it (ADR-0017), closes the review thread and keeps the author's message in one transaction, so a close or a stop between them loses neither. The author's thread hears the verdict once, through the outbox, when it is open. Nobody else is told: the owner sees it on the pull request's card.
   - Every review thread still open is settled at each pass, whatever its pull request is now. It ends when its pull request closed, merged or became a draft, or its head moved: under the outbox lock its queued brief is withdrawn first, then a run that began before the withdrawal is stopped.
   - A review whose session ends with no verdict ends, and counts against the budget.
5. **The budget is the owner's policy:** at most 4 reviews of a pull request, 1 and 3 re-reviews. Once they are spent, pocketd starts none, and the owner decides.
6. **The author does not start a review.** pi-pocket-config's skill `cross-review` becomes a note to authors: report the pull request with `team_message`, and pocketd starts the review. The pocketd that starts reviews and that note go out together, so the job never has two owners.

## Consequences

- Every linked pull request gets its review with no agent asking for it, and only one per head. The owner merges from the desktop once the review is `ready` and CI is green.
- A review costs a session of the author's agent on another maker's model, at each new head, up to 4 a pull request.
- A thread's specialist must have the repository's checkout in its profile for its review too, as it had for its work.
- The review counts on the desk like any thread. A full desk delays it to the round after a place frees.
- **Known limits:**
  - The skill's exemptions are not built: an unchanged rebase, a low and non-blocking finding, and the skills sync pull requests of frostyard/core. Every new head is reviewed while the budget lasts.
  - `read` reads what the sandbox lets any agent read, read-only.
  - A head the author's checkout does not hold is fetched over HTTPS: a private repository then needs a git credential helper for the service user.
  - A review whose worktree cannot be made yet stays starting, and its place on the desk is kept, until a round makes it.
  - The protocol does not carry the budget. The desktop shows the review threads on the board and the verdict on the card, but not how many reviews remain.

## Alternatives considered

- **pocketd tells the supervisor once per head, and the supervisor delegates:** one owner still, but a model's judgment then decides whether and whom, and the supervisor may be busy or gone.
- **A specialist of the other family reviews, as ADR-0017's text allowed:** the owner chose the author's own agent, which knows the work, on the other family's model.
- **Keep the skill as the author's job:** pocketgo has no subagent, and two owners would start two reviews.

## References

- Shapes: [design: system architecture](../design/architecture.md#service), [specs: protocol rules 25, 41, 42 and 46](../specs/protocol.md), [plans: roadmap](../plans/roadmap.md)
- Supersedes in part: decision 1 of [ADR-0017](0017-record-cross-review-verdicts-and-set-ready-for-human-in-code.md), which had the supervisor delegate reviews
- Builds on: [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0013](0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md), [ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md), [ADR-0016](0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md), [ADR-0017](0017-record-cross-review-verdicts-and-set-ready-for-human-in-code.md)
