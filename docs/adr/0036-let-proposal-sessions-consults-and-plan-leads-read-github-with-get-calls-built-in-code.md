# 0036 — Let proposal sessions, consults and plan leads read GitHub with GET calls built in code

- **Status:** Accepted; decision 1's callers amended by [ADR-0057](0057-let-a-cross-review-read-github-with-get-calls-for-what-a-pin-bump-brings-in.md), which gives a cross-review `team_github` with the same rules
- **Date:** 2026-11-21

## Context

**Three kinds of session read files alone.**
- [ADR-0033](0033-gather-proposals-from-the-team-in-read-only-sessions-in-code.md) gives each agent a gather asks a read-only proposal session, with `read` and `team_propose` alone. Its consequences named the gap: a skill that needs `gh`, as an issue triage would, cannot list issues.
- [ADR-0020](0020-plan-before-building-with-the-plan-gate-and-consults-in-code.md) gives a consult exactly `read` and `team_finding`, and a plan's lead `read` as its only coding tool, with `team_message` and `team_plan`. Neither can read an issue or a pull request a plan is about.

**pocketd reads GitHub with gh already.** It reads pull requests, CI and merge bases through `Engine.GH` ([ADR-0016](0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md)):
- The program is a gh only root can change (`trustedGH`), run in the sandbox ([ADR-0012](0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md)).
- Its configuration folder is pocketd's own, which the sandbox shows empty. Its host is github.com, and its sign-in is the token the service user's gh holds.
- Agents with a shell use the same sign-in, with gh in the sandbox.

**gh api writes in several ways:**
- `--method`, any field (`-f`, `-F`, `--raw-field`, `--field`) and `--input` make a call a write.
- The endpoint `graphql` posts.
- An endpoint that holds `://` is a whole URL, to any host.
- An endpoint that holds `{owner}`, `:owner`, `:repo` or `:branch` makes gh run git in its folder to fill it in.
- `--jq` and `--template` run in gh's process, which holds the token in its environment (`GH_TOKEN`).

**The owner's choices, 2026-11-21:**
- Asked "the read-only proposal sessions a gather opens can only read files today; should they get more, for skills like issue triage?", the owner chose: "Read-only gh — a tool that runs only GET calls of `gh api`: they can read issues and PRs, but can't comment, label or push."
- Asked, on frostyard/pocketgo#61, whether the tool should read the asking agent's repositories alone, whether reads should be capped, and whether consults and plan leads should get it: "team_github: don't limit repositories, don't cap reads until it's needed, consults and plan leads should get team_github".

## Decision

1. **A gather's proposal session, a consult's session and a plan's lead get `team_github`, and no other session does** (`roleTools`). It comes after their other team tools but `team_memory`, in the team extension's order.
   - `service/internal/api` checks it again by the calling session's records (`Server.readsGitHub`): a gather's ask's session (`store.GatherAskOfSession`), a consult's (`store.ConsultOfSession`), or the specialist session of a plan's own thread (`store.ThreadOfSession`, `IsPlanThread`). Any other caller is refused before gh runs.
   - The state of the ask, the consult or the plan does not matter: a read writes nothing.
2. **The agent names a path and query parameters, and pocketd builds every argument** (`github.Endpoint`, `github.Get`): `gh api --hostname github.com --method GET -- <path>?<query>`.
   - The path is a relative REST path, its one leading `/` dropped. It has 1 to 32 segments and at most 512 bytes. Each segment holds letters, digits, `.`, `_`, `~`, `@`, `+` and `-` alone, and is neither `.` nor `..`. The first starts with a letter and is not `graphql`, in any case. So no scheme, host, `?`, `#`, `%` escape, placeholder or flag gets through.
   - The query has at most 20 parameters. A name is a letter, then letters, digits and `_`, at most 64. A value is UTF-8 of at most 1,000 characters. pocketd encodes them (`url.Values`), so a value adds no parameter and holds no placeholder. The whole endpoint has at most 8 KiB.
   - No field, body, input file, header, `--jq`, `--template`, `--paginate` or `--include` is ever passed. The endpoint comes after `--`.
3. **It runs as pocketd's other gh reads run:** through `Engine.GH`, in the sandbox, with the trusted gh, pocketd's empty configuration folder, the host github.com and the service user's sign-in. A read takes at most 30 seconds (`team.GitHubReadTime`, asked through `Server.withTimeout`), and the call's own context bounds it, so an abort stops gh. A read keeps nothing: a call run again after a restart reads again.
4. **Its answer is bounded in code** (`team.GitHubAnswer`):
   - gh's output past 1 MiB is an error, as for every gh read, and gh is stopped at its first byte past it. Output that is not UTF-8 is refused.
   - JSON is compacted. Other text comes as gh printed it, its whitespace included.
   - An answer over 48 KiB keeps the whole items of a top-level array that fit, else its first 48 KiB cut at a whole character. A note says so, and names `per_page` and `page`.
   - An empty answer says the call succeeded with no body.
   - The tool's output limits let a whole answer and its note through, one long line or many short ones.
5. **It reads any repository the sign-in reaches, and nothing caps the reads,** as the owner chose: no list of the agent's repositories, and no count of reads an ask, a consult or a plan may make. A cap comes once the service user's rate limit is actually hit, in a new decision.
6. **A plan's lead started before this change gets the tool at start; a proposal session or a consult does not.**
   - At start (`UpgradeTeamTools`), each plan thread not closed has its lead's session given `team_github` when it has `team_plan` and lacks it (`Server.upgradeGitHubTools`, `Engine.SetGitHubTool`), in the place a new lead has it. A plan lives for days.
   - An ask and a consult last at most 60 minutes, and their briefs name their tools without it, so they keep their tools.
   - A session an earlier run made, which pocketd records only now, gets the tool before the text that names it: a plan's lead before its brief, a consult before its question (`Engine.SetGitHubTool`, which takes a session with `team_plan` or `team_finding`). A proposal session's brief is kept with its ask before its session is made, so a brief that names the tool never reaches a session made without it.
   - A session made after the change from a brief kept before it has the tool, which its brief does not name. So does an older lead, whose brief came before.
7. **The briefs name it** (`team.GitHubToolText`): a proposal session's (`team.ProposalBrief`), a consult's question (`team.ConsultQuestion`) and a plan's lead's (`team.PlanBrief`) say it has `team_github`, which reads GitHub's REST API with GET calls alone.
8. **The protocol does not change.** No wire type moves, and its version stays 23. Rules 25, 49, 52 and 58 name the tool.

## Consequences

- An issue-triage skill works in a gather: its agents list and read issues, pull requests, comments, checks and files on GitHub.
- A plan's lead reads the issues and pull requests its plan is about, and an expert consulted on it reads them too.
- None of these sessions writes on GitHub: no comment, label, review, merge or push.
- They read what the service user's sign-in reads: every repository the sign-in reaches, not their agent's alone, and the account's own GET endpoints, as its notifications or email addresses. The agents with a shell read all of it already, with the same gh.
- Each read counts against the service user's GitHub rate limit, which pocketd's own reads share (pull rounds, CI, cross-review). Nothing caps an agent's reads but its turns, and for an ask or a consult its 60 minutes. A gather of the whole team, or a long plan, could use up the limit, and pocketd's own reads would then fail until it resets.
- A list GitHub pages, or an answer over 48 KiB, takes more than one call. With no `--jq`, the agent cannot ask for fewer fields.
- A path that needs another character is refused: a file name with a space, or a fork's `owner:branch` in a compare.

## Alternatives considered

- **gh itself, with a check of the agent's arguments:** gh's flags, their abbreviations, aliases and extensions are many. With every argument pocketd's, only a path and a query are left to check.
- **A shell in these sessions:** the owner chose read-only sessions in ADR-0020 and ADR-0033, and GET calls alone here.
- **gh's `--jq` to trim answers:** gh's jq runs in its process, whose environment holds the token. pocketd offers no filter.
- **Only the endpoints of issues and pull requests, or the agent's own repositories alone:** the owner asked for GET calls of `gh api`, and chose no repository limit.
- **A cap on reads now:** the owner chose to wait until the rate limit is hit.
- **Upgrading older proposal sessions and consults at start, as plan leads are:** they last an hour, and their briefs do not name the tool.
- **Paging the answer by byte offset, as `team_diff` does:** GitHub pages its own lists, and a REST answer read again may differ, so an offset into it is not stable.

## References

- Shapes: [design/architecture.md](../design/architecture.md#service), [specs/protocol.md](../specs/protocol.md) (rules 25, 49, 52 and 58)
- Built by: [plans/roadmap.md](../plans/roadmap.md) (item 15, the night shift, slice 2)
- Builds on: [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0012](0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md), [ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md), [ADR-0016](0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md), [ADR-0020](0020-plan-before-building-with-the-plan-gate-and-consults-in-code.md), [ADR-0033](0033-gather-proposals-from-the-team-in-read-only-sessions-in-code.md)
