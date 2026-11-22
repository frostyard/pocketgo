# 0053 — Show a session's changes in the app, read with isolated git in code

- **Status:** Accepted
- **Date:** 2026-11-21

## Context

**The owner cannot see what a session changed until it opens a pull request.** frostyard/pocketgo#102, filed on 2026-11-21:
- A pull request has Open Diff, which opens GitHub in the browser.
- A thread's specialist works in a git worktree of its own ([ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md)), and so can the owner's session ([ADR-0027](0027-start-the-owners-session-on-a-repository-in-a-new-worktree-named-in-code.md)). Until the agent pushes and opens a pull request, the owner asks the agent or uses the Terminal ([ADR-0025](0025-give-the-owner-a-shell-on-the-service-host-from-the-desktop.md)).
- The architecture names "Session detail: … diffs and files" as intended, not built.

**git on an agent's checkout runs in the sandbox, isolated:**
- AGENTS.md: git pocketd runs on an agent's checkout goes through `Engine.Git` (`worktree.Git`), the trusted git only root can change (`trustedGit`), never `exec` from pocketd itself, since a hook or a filter of the checkout would run with pocketd's rights ([ADR-0012](0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md)).
- A cross-review reads its change set with `team_diff` ([ADR-0019](0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md)): every git runs with `team.ReviewGitOptions` (no replacement object, hook, fsmonitor, signature program, pager, external diff or attribute the checkout sets), after `worktree.AttributesIsolated` checked what no option turns off (git 2.43 or later, no `info/attributes`, no system attributes file). Its output is paged (`Engine.GitPage`) or split by record with bounded memory (`Engine.GitEach`).
- The checkout here holds what is not committed yet, which a review's detached worktree never does: git compares the files in the folder, and lists those it does not track.

**What a session works on:**
- A thread records its repository, its worktree and its branch, and the pull requests its reports linked, each with GitHub's base branch ([ADR-0016](0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md)). It records no base branch of its own: its worktree branches from the checkout's `HEAD`.
- A session records its folder (`cwd`); any session may be in a checkout or not.

## Decision

1. **Two read-only endpoints** ([protocol rules 63 and 64](../specs/protocol.md)):
   - `GET /v1/sessions/{id}/changes` answers `proto.Changes`: whether the session's folder is in a git checkout, its top, its branch and `HEAD`, the base, the merge base, and the files changed, each with its lines added and removed and whether its change is committed, not yet, or both, or new to git. An answer of a folder in no checkout says so (`repository` false), and is no error.
   - `GET /v1/sessions/{id}/changes/file?path=` answers `proto.FileDiff`: one file's unified diff, from the merge base to the folder.
   - Both write nothing, take no `requestId`, and send no event. Protocol version 27 carries them.
2. **The base is chosen in code** (`changes.ChooseBase`, from the refs `changes.Candidates` names):
   - the base branch of the newest open pull request of the session's thread, else of its newest (`changes.PullBase`), as `origin/<base>`, else as the local branch; the session is the thread's specialist, its worker, or a review of one of its pull requests;
   - else the checkout's default branch: the branch `origin/HEAD` names, else `origin/main`, `origin/master`, `main`, `master`;
   - else none: only what is not committed is counted, from `HEAD`.
   - The changes count from the merge base of that branch and `HEAD`; a branch with no commit yet counts from git's empty tree; a branch that shares no commit with its base counts from `HEAD`.
3. **git runs as a review's does, and writes nothing:**
   - every command through `Engine.Git`, `Engine.GitEach` or `Engine.GitPage`, in the sandbox, with the trusted git;
   - with `team.ReviewGitOptions`, `--no-optional-locks` (so no read writes the index), `--literal-pathspecs` and `--no-pager` (`changes.Args`); a diff with `--no-ext-diff`, `--no-textconv`, `--no-color`, `--no-relative`, `-M` and `--ignore-submodules=dirty`, so no git runs in a submodule;
   - after `worktree.AttributesIsolated`, before any file of the checkout is read: a checkout it refuses is `conflict`, with its reason.
4. **What is read is bounded, and a cut says so:**
   - at most `proto.MaxChangedFiles` (300) files listed, tracked ones in git's order then new ones, and `more` counts the rest; git's output is read record by record (`Engine.GitEach`, `changes.Numstat`), so a change of any size takes bounded memory;
   - the lines of at most `proto.MaxCountedNew` (30) new files counted, each with `git diff --no-index` against nothing; the rest are `uncounted`;
   - a file's diff is at most `proto.MaxFileDiff` (64 KB), cut after its last whole line (`changes.CutAtLine`), with `cut` set; the desktop says so (`state.CutNote`);
   - one read runs git for 30 seconds at most, and 4 reads run at once; another waits for a place, as long as its request lasts.
5. **Only a listed change is read:** a file's path is checked in code (`changes.CheckPath`: relative, no `.` or `..` part, no NUL, at most 4,096 bytes of UTF-8), then must be a file git's numstat names (a rename's diff names its old path too) or a new file git lists, else `not_found`. No path from a client reaches `git diff --no-index` unlisted.
6. **`Engine.GitPage` gives back what git printed before an error:** a diff of a new file against nothing ends with git's status 1 once it printed one, its answer (`changes.Differs`); status 1 with nothing printed stays an error.
7. **The desktop** (rules in `desktop/internal/state`, drawn in `desktop/internal/app`):
   - It keeps the changes of the open session and of the sessions of the board's threads that work on a repository and are not closed (`Data.ChangesTracked`), fetched through `Data.BeginChanges` and `Data.SetChanges`, so an older answer never undoes a newer one. It fetches them again when a run ends (`RunEnded`), at a resync, and with Refresh; a fetch asked for while one is in flight follows it once.
   - The session's header shows the count, "4 files +120 −8" (`ChangesSummary`), which opens the Changes pane beside the session: the base (`BaseNote`), the files with where each change is (`FileState`) and their lines, and a file's diff drawn by frostui's `DiffView`, its lines numbered and colored. On a compact window the count is under the top bar and opens a sheet, a file's diff in its place with a way back.
   - A thread's card shows the count of its session's changes (`CardChanges`), which opens the session with its Changes.

## Consequences

- The owner sees what a thread's specialist, or their own worktree session, changed, committed or not, before any pull request, and reads each file's diff in the app.
- A checkout git cannot read isolated shows no changes: the owner sees why, and removes the attributes file, or updates git, as a review needs too.
- A read runs about ten sandboxed git commands, and one more for each new file counted: a board of many threads asks for each one's at each run's end. The concurrency bound keeps that from crowding the host.
- The base of a thread before its pull request is the checkout's default branch, not the branch the worktree started from, which pocketd does not record: a worktree made from another branch counts that branch's commits too.
- A path that is not valid UTF-8 cannot be asked for: its diff is not shown.
- A diff past 64 KB shows its start alone; the rest is in the checkout or, once opened, the pull request.
- Changes are not pushed as events: a count is as fresh as its last fetch.

## Alternatives considered

- **Run git with pocketd's own rights, outside the sandbox:** a hook, filter or fsmonitor of the checkout would run as pocketd (AGENTS.md).
- **List the files with `git status` alone:** it counts no lines, and writes the index opportunistically.
- **Count new files' lines by reading them with `Engine.ReadUpTo`:** it follows a link at the path, where git shows the link itself, and a diff of a new file is git's to make.
- **Push the changes as events after each run:** every run of every session would pay for git; a fetch when shown costs only what the owner looks at.
- **Record each thread's base branch when its worktree is made:** a migration and a change of the thread's start for a guess the pull request corrects; the default branch is right for the team's worktrees, which start from the checkout's default.

## References

- Shapes: [specs: protocol](../specs/protocol.md), [design: system architecture](../design/architecture.md#service), [design: desktop client](../design/architecture.md#desktop-client), [plans: roadmap](../plans/roadmap.md)
- Builds on: [ADR-0012](0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md), [ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md), [ADR-0016](0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md), [ADR-0019](0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md), [ADR-0025](0025-give-the-owner-a-shell-on-the-service-host-from-the-desktop.md), [ADR-0027](0027-start-the-owners-session-on-a-repository-in-a-new-worktree-named-in-code.md), [ADR-0044](0044-navigate-a-compact-window-by-a-bar-of-tabs.md)
