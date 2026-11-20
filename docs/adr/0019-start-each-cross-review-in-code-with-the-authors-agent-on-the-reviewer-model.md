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

   pocketd checks the pull requests a report links when it links them, after it reads each again with its merge base, and at every round of `-pr-every` the pull requests that round read: one whose read failed starts no review on what pocketd last knew. Every pass also reads on GitHub, then starts, each open pull request that needs its review by what pocketd holds, so a stop between a report's link and its pass loses none; a read counts for the pass only while the pull request stored is that read. A report's reads with the merge base are the pass's, off the author's tool call. A review is keyed by its pull request, base branch and head (`review:<repository>#<number>:<base>:<head>`, the repository in lower case): a pull request retargeted at the same head has another change set. It is recorded only while the pull request, as stored then, is open, no draft and at that head. A verdict on a pull request GitHub reads as a draft is refused, and so is one whose pull request GitHub now has at another head or base branch, or whose merge base GitHub names differently from the one the review read. Its thread is recorded with the key in one transaction, so a run again, a restart or a second round starts none. A head that a verdict already backs, filed before this decision, gets none. What a start needs is checked before the review takes a place on the desk: the author's checkout in its profile, matched in any case as GitHub matches names, its profile's tools, and the reviewer model on a sign-in.
2. **The reviewer is the author's own agent, on the reviewer model.** The author is the specialist of the thread that links the pull request, and its family is the family of that thread's session model. `router.Choose` picks the reviewer model by that family and records the rule `cross-review`:
   - Anthropic: `openai/gpt-6.1-sol`, on the `openai-codex` subscription (ADR-0013's alias);
   - OpenAI: `anthropic/claude-sonnet-5-5`, the same maker as the owner's `github-copilot/claude-sonnet-5.5`, which pocketd cannot run;
   - any other known family: `openai/gpt-6.1-sol`;
   - no known family: no review. The owner decides.

   ADR-0017's checks of a verdict still hold: the review is another thread, on a model of another family.
3. **The review thread is pocketd's.** Its supervisor is the author thread's supervisor, and its repository is the pull request's. Its folder is a worktree detached at the head, fetched as `pull/<number>/head` when the checkout does not hold it, and checked to be that commit with no change of its own, no untracked file and no ignored file, also when its start recovers or its brief goes again. Every git command on it ignores replacement objects, so the commit is GitHub's, and runs no hook, fsmonitor, signature program, pager or external diff of the checkout; the git that runs is one only root can change, never one on the PATH. It reads attributes from the empty tree and from no file the configuration names, with no line-ending conversion, so no filter of the checkout runs and the folder holds the commit's blobs. What no option turns off is checked before git writes a file, at each start and before each `team_diff`: git must be 2.43 or later, which knows `attr.tree`, and neither the checkout's `info/attributes` nor a system attributes file may exist.
   - **Its base** is the merge base GitHub's compare names, read with the service user's sign-in and kept on the thread, so the brief and every diff name one change set. It must be readable in the worktree before the reviewer is briefed: a checkout that lacks it fetches the base branch, and no session starts until it is there.
   - **Its agent has no shell:** `read`, if its profile has it, `team_diff` and `team_verdict`. `team_diff` is code: `git diff` or `git log -p` from the base, or from the last reviewed head, to the head, in the review's worktree, in the sandbox, with the git options above and no signature shown, only for paths in the folder, each named path taken literally, and paged by byte offset, git stopped once a page is full, so no patch is too long to read. The tool's output limits let a whole page and its footer through PiG. A diff shows submodule pointers whatever the checkout ignores. A shell, even in the sandbox, could write other checkouts and reach GitHub as the service user.
   - **No other team tool, and no one else's message:** it gets no `edit`, `write`, `team_message` or `team_memory`, and those refuse a review's call whatever its tool list holds. Its session takes no message from the owner either, so every run in it is the review's, and stopping one stops no other work. The desktop shows a session of the route `cross-review` with no composer, only its stop. No supervisor closes it with `team_close`. No other thread gets `team_verdict`, and `team_verdict` refuses a call from any other thread: a review the supervisor delegated would be a second owner. A session that had `team_verdict` before this decision loses it at pocketd's start.
   - **Its brief is code** (`team.ReviewBrief`). It names the pull request, its base and head commits, and `team_diff`. It says the review is read-only. For a re-review, it names the last reviewed head, its verdict and its findings. It says how to file the verdict: each finding as `<severity> <blocking or non-blocking>: <the point>`. `team.CheckReviewFindings` refuses another form, a not-ready verdict with no blocking finding, and a ready one with one.
4. **A verdict ends the review.** pocketd records it (ADR-0017) with the base branch and merge base it was filed on, closes the review thread and keeps the author's message in one transaction, so a close or a stop between them loses neither. The label, the verdict the owner sees, the start of a review and the desktop's merge (`state.Backed`) all match a verdict to the pull request's head and base branch: a retarget at the same head leaves no verdict backing it, and the reads around a label's write check the base branch as they check the head. pocketd reads the merge base again at each read of an open pull request, the reads around a label's write included, and a verdict counts only at the merge base it was filed on: a base branch reset under the same head and name takes the label off and the verdict from the owner's card. A merge base GitHub does not name is unknown, and backs nothing; the read counts as failed. The desktop's confirmation has pocketd read the pull request on GitHub first (`GET /v1/pulls/{id}`), whatever state pocketd last saw, and checks that answer and the list it reads after, so its backing is GitHub's at the owner's click, not at the last round. Protocol 8 carries this: pocketd refuses a desktop of version 7, which merged on the head alone. A verdict filed before this decision names no base, and counts by its head alone, as it did, while pocketd knows the merge base. A re-review reads only the change since the last reviewed head when that review had the same merge base; else it is a full review. The author's thread hears the verdict once, through the outbox, when it is open. Nobody else is told: the owner sees it on the pull request's card.
   - Every review thread still open is settled at each pass, whatever its pull request is now. A pass runs when a review's run ends, its verdict's included, when a thread closes, when the desktop's confirmation reads a pull request, at pocketd's start and at each round, so a review settles, and one that waited for a place starts, with no round of `-pr-every`. It ends when its pull request closed, merged or became a draft, or its head or base branch moved: under the outbox lock its queued brief is withdrawn and the thread closes; then, outside the team's locks, since PiG's abort waits for the run and a verdict of that run may wait for them, a run still going is stopped. A stop that fails, or whose look at the session fails, is tried again at each pass.
   - A review whose session ends with no verdict ends, and counts against the budget, also when a pass finds its brief in the transcript before its delivery was recorded.
   - A review not briefed yet reads its pull request on GitHub before it starts again, so none starts on what pocketd held before a stop.
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
  - A merge base that moves with neither a new head nor a retarget, when the base branch took part of the pull request's history or was reset, refuses a verdict filed after it and takes the backing of one filed before; that head gets no new review, and the owner decides.
  - Between pocketd's read at the desktop's confirmation and the merge, a base branch reset goes unseen: GitHub's merge pins the head, not the base.
  - A checkout with its own `info/attributes`, or a host with a system attributes file or git older than 2.43, gets no review until the owner changes it.
  - A stop of an ended review's run that failed is retried from memory: a restart of pocketd forgets it, and the run, whose verdict is refused, goes on until it ends.
  - A re-review whose last reviewed head the checkout no longer holds, after a force-push, is a full review from the base.
  - The protocol does not carry the budget. The desktop shows the review threads on the board and the verdict on the card, but not how many reviews remain.

## Alternatives considered

- **pocketd tells the supervisor once per head, and the supervisor delegates:** one owner still, but a model's judgment then decides whether and whom, and the supervisor may be busy or gone.
- **A specialist of the other family reviews, as ADR-0017's text allowed:** the owner chose the author's own agent, which knows the work, on the other family's model.
- **Keep the skill as the author's job:** pocketgo has no subagent, and two owners would start two reviews.

## References

- Shapes: [design: system architecture](../design/architecture.md#service), [specs: protocol rules 25, 41, 42 and 46](../specs/protocol.md), [plans: roadmap](../plans/roadmap.md)
- Supersedes in part: decision 1 of [ADR-0017](0017-record-cross-review-verdicts-and-set-ready-for-human-in-code.md), which had the supervisor delegate reviews
- Builds on: [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0013](0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md), [ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md), [ADR-0016](0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md), [ADR-0017](0017-record-cross-review-verdicts-and-set-ready-for-human-in-code.md)
