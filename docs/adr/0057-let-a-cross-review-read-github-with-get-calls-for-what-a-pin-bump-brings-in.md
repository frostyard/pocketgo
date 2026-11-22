# 0057 — Let a cross-review read GitHub with GET calls, for what a pin bump brings in

- **Status:** Accepted
- **Date:** 2026-11-22

## Context

**A cross-review sees one repository.** [ADR-0019](0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md) gives a review three tools, and no shell:
- `read`, for its worktree detached at the head and the sandbox's read-only paths;
- `team_diff`, for the pull request's change set;
- `team_verdict`.

Nothing it has reads another repository at a commit.

**A pin bump's change is in another repository.** On 2026-11-22, with pocketd at 8a28c75:
- frostyard/clix#136 and frostyard/std#122 each changed one line: the pin of core's reusable `pr-triage.yml`, from `981e7ae` to `f99acfd`.
- Both reviews filed `unclear` and found no defect. The std reviewer called it "an evidence limitation, not an identified code defect", and asked for core's diff `981e7ae..f99acfd`.
- The author wrote that diff into pull request comments. No commit could answer the verdict, so pocketd never added `ready-for-human` ([ADR-0017](0017-record-cross-review-verdicts-and-set-ready-for-human-in-code.md)).
- frostyard/core#287 plans the same bump in 37 more repositories. Reusable-workflow and action pin bumps come again with each core release and Dependabot run (frostyard/pocketgo#105).

**A guarded read of GitHub exists already.** [ADR-0036](0036-let-proposal-sessions-consults-and-plan-leads-read-github-with-get-calls-built-in-code.md) gives a gather's proposal session, a consult and a plan's lead `team_github`:
- It makes GET calls of GitHub's REST API alone, and pocketd builds every argument (`github.Endpoint`, `github.Get`).
- It runs through `Engine.GH`, in the sandbox, with the service user's sign-in. Its answer is bounded (`team.GitHubAnswer`).
- Its paths take a compare between two commits (`repos/<owner>/<repo>/compare/<old>...<new>`) and a file at a commit (`repos/<owner>/<repo>/contents/<path>` with the query `ref=<sha>`).

**The issue named two ways:**
1. Give a review `team_github`, with a consult's rules. The issue recommends this one.
2. Have `team_diff` also return the pinned repository's compare when the change set moves a `uses: <owner>/<repo>/...@<sha>` pin.

The first was chosen on 2026-11-22.

## Decision

1. **A cross-review's session gets `team_github`, with exactly a consult's rules** (ADR-0036):
   - GET calls alone, its path and query checked by `github.Endpoint`, and every argument of gh pocketd's own (`github.Get`).
   - It runs through `Engine.GH`, in the sandbox with the service user's sign-in, for at most `team.GitHubReadTime`, and `team.GitHubAnswer` bounds its answer.
   - It reads any repository the sign-in reaches, and no count of reads is kept.
2. **Nothing else about a review changes.**
   - `roleTools` gives a review `team_diff`, `team_verdict` and `team_github`, in the team extension's order, after `read` when its profile has it. It still has no shell and no other team tool.
   - `Server.TeamGitHub` checks the caller again by its records (`Server.readsGitHub`). It takes the session of a review thread pocketd started (`ReviewPull` set). It refuses, before gh runs, the author's thread and a thread the supervisor delegated, though it reviews the same pull request.
   - The state of the review does not matter, as for the other callers: a read writes nothing.
3. **The brief names the tool and the reads a pin bump needs** (`team.ReviewBrief`, `team.ReviewGitHubText`):
   - The review has `team_diff`, `team_github` and `team_verdict`, and `read` if its profile has it.
   - For a change that moves a pin to another repository, as a `uses: <owner>/<repo>/...@<sha>` line or a submodule does, it reads what the move brings in.
   - `repos/<owner>/<repo>/compare/<old>...<new>` shows the change from the two commits' merge base to the new one, and its `status` says how they relate. `ahead` is a plain bump. `behind` is a rollback, whose compare shows nothing, so the reviewer reads `compare/<new>...<old>` for what it takes out. `diverged` means each has commits the other lacks, so it reads both compares.
   - A compare lists at most 300 files, pages its commits, and leaves out the patch of a file too large or binary. Then the reviewer reads each commit (`repos/<owner>/<repo>/commits/<sha>`) or the file at both commits (`repos/<owner>/<repo>/contents/<path>` with the query `ref=<sha>`, its content in base64), and says in its verdict what it could not read.
   - The tool's own description stays as it is.
4. **A review started before this change gets the tool the way older sessions got earlier tools.** This follows how a review got `team_verdict` (ADR-0017, ADR-0019) and how an older plan lead got `team_github` (ADR-0036):
   - At start (`UpgradeTeamTools`, `Server.upgradeGitHubTools`), the session of each review thread not closed that lacks the tool gets it, in the place a new review has it. `Engine.SetGitHubTool` now also takes a session with `team_diff`, which only a review has. No other tool changes.
   - A review whose session an earlier run made, and which pocketd records only now, gets it before the brief that names it (`Server.attachBriefOpen`).
   - A review briefed before the change gets the tool, but its brief does not name it. Its model has the tool's own description.
5. **The protocol does not change.** No wire type moves, and its version stays 30. Rules 25, 46 and 58 name the tool.

## Consequences

- A pin bump's reviewer reads what the new pin brings in: the pinned repository's compare between the two commits, and its files at the new commit. A verdict on the bumps of frostyard/core#287 can be `ready` or `not-ready` on the code, instead of `unclear` for want of it.
- A review still writes nothing on GitHub: no comment, label, review, approval or merge. pocketd's label stays its only write (ADR-0017).
- A reviewer reads all that the service user's sign-in reads, the pull request's comments and other reviews among them. The author can write those, as it wrote the code the review reads. A verdict still counts only by code's checks (`team.CheckVerdict`, `team.CheckReviewer`, `team.CheckHead`), and only at the head, base branch and merge base it was filed on.
- Reviews add reads to the service user's rate limit, which pocketd's own reads share. Nothing caps them but the review's turns, as ADR-0036 chose for every caller. A cap waits until the limit is hit.
- A large compare is cut at 48 KiB, and says so. GitHub's own limits cut it upstream, with no note from pocketd: 300 files, its commits in pages, no patch for a file too large or binary. The reviewer of a large bump reads one commit or one file at a time, and the brief asks it to name what it could not read.
- Whether a pin's commits are a bump, a rollback or a divergence is the reviewer's reading of the compare's `status`, as the brief says, not a rule in code: pocketd does not parse pins (see the alternatives).
- A file at a commit comes in base64, since no header asks for its raw text, and a long one is cut.
- An older review open at a deploy gets the tool, which its brief does not name. Few are open at once, since a review ends at its verdict or when its pull request moves.
- A path that needs another character is still refused (ADR-0036): a file name with a space, or a fork's `owner:branch` in a compare.

## Alternatives considered

- **Extend `team_diff` with the pinned repository's compare** (the issue's second option): code would have to find each kind of pin in a change set (`uses:` lines, submodules, lock files) with its old and new commits, and run another bounded read. Each kind is a parser of its own. `team_github` reads any of them already, guarded.
- **A shell in a review:** ADR-0019 chose none, since in the sandbox it could write other checkouts and reach GitHub as the service user.
- **Leave older reviews without the tool, as ADR-0036 left older consults and proposal sessions:** a review open at a deploy on a pin bump would file `unclear`, and wait for a push to be reviewed again. Giving the tool writes nothing and changes no other tool.
- **Name the pin-bump reads in `team_github`'s own description:** that text is every caller's. The brief is the review's alone, and the issue asked the review's brief to name the tool.

## References

- Shapes: [design/architecture.md](../design/architecture.md#service), [specs/protocol.md](../specs/protocol.md) (rules 25, 46 and 58)
- Built by: [plans/roadmap.md](../plans/roadmap.md) (item 30)
- Builds on: [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0017](0017-record-cross-review-verdicts-and-set-ready-for-human-in-code.md), [ADR-0019](0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md), [ADR-0036](0036-let-proposal-sessions-consults-and-plan-leads-read-github-with-get-calls-built-in-code.md)
