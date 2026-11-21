# 0036 — Let a gather's proposal sessions read GitHub with GET calls built in code

- **Status:** Accepted
- **Date:** 2026-11-21

## Context

**A proposal session reads files alone.** [ADR-0033](0033-gather-proposals-from-the-team-in-read-only-sessions-in-code.md) gives each agent a gather asks a read-only proposal session, with `read` and `team_propose` alone. Its consequences named the gap: a skill that needs `gh`, as an issue triage would, cannot list issues.

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

**The owner's choice, 2026-11-21.** Asked "the read-only proposal sessions a gather opens can only read files today; should they get more, for skills like issue triage?", the owner chose: "Read-only gh — a tool that runs only GET calls of `gh api`: they can read issues and PRs, but can't comment, label or push."

## Decision

1. **A proposal session gets `team_github`,** after `team_propose`, and no other session does (`roleTools`). `service/internal/api` checks it again: its caller is a gather's ask's session (`store.GatherAskOfSession`), as `team_propose`'s is. The ask's state does not matter, as for `team_propose list`: a read writes nothing.
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
5. **A proposal session made before this change keeps its tools.** pocketd does not give it `team_github` at start, as `Engine.SetSupervisorTools` upgrades a supervisor: an ask lasts at most 60 minutes, and its brief names `read` and `team_propose` alone. A session made after the change from a brief kept before it has the tool, which its brief does not name.
6. **The brief names it** (`team.ProposalBrief`): the agent has `read`, `team_github` and `team_propose`, changes nothing, and reads GitHub's REST API with GET calls alone.
7. **The protocol does not change.** No wire type moves, and its version stays 23. Rules 25 and 58 name the tool.

## Consequences

- An issue-triage skill works in a gather: its agents list and read issues, pull requests, comments, checks and files on GitHub.
- A proposal session still writes nothing on GitHub: no comment, label, review, merge or push.
- It reads what the service user's sign-in reads: every repository the sign-in reaches, not its agent's alone, and the account's own GET endpoints, as its notifications or email addresses. The agents with a shell read all of it already, with the same gh.
- Each read counts against the service user's GitHub rate limit, which pocketd's own reads share (pull rounds, CI, cross-review). Nothing caps an agent's reads but its 60 minutes and its turns.
- A list GitHub pages, or an answer over 48 KiB, takes more than one call. With no `--jq`, the agent cannot ask for fewer fields.
- A path that needs another character is refused: a file name with a space, or a fork's `owner:branch` in a compare.

## Alternatives considered

- **gh itself, with a check of the agent's arguments:** gh's flags, their abbreviations, aliases and extensions are many. With every argument pocketd's, only a path and a query are left to check.
- **A shell in the proposal session:** the owner chose read-only sessions in ADR-0033, and GET calls alone here.
- **gh's `--jq` to trim answers:** gh's jq runs in its process, whose environment holds the token. pocketd offers no filter.
- **Only the endpoints of issues and pull requests:** the owner asked for GET calls of `gh api`. A narrower list is a question for them.
- **Upgrading older proposal sessions at start, as supervisors are:** an ask lasts an hour, and its brief does not name the tool.
- **Paging the answer by byte offset, as `team_diff` does:** GitHub pages its own lists, and a REST answer read again may differ, so an offset into it is not stable.

## References

- Shapes: [design/architecture.md](../design/architecture.md#service), [specs/protocol.md](../specs/protocol.md) (rules 25 and 58)
- Built by: [plans/roadmap.md](../plans/roadmap.md) (item 15, the night shift, slice 2)
- Builds on: [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0012](0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md), [ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md), [ADR-0016](0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md), [ADR-0033](0033-gather-proposals-from-the-team-in-read-only-sessions-in-code.md)
