# pocketgo

pocketgo is a persistent workspace for running a team of AI coding agents
across software projects, for one owner who works with a supervisor and named
specialists. It is a headless Go service built on PiG's durable harness, a
desktop client on MyGo and frostui, and the wire types they share. Nothing is
built yet. Start at [docs/design/architecture.md](docs/design/architecture.md)
and [docs/plans/roadmap.md](docs/plans/roadmap.md), then
[docs/README.md](docs/README.md).

This file (`AGENTS.md`) is the CANONICAL agent instructions — `CLAUDE.md`,
`GEMINI.md`, `.cursorrules`, and `.github/copilot-instructions.md` are symlinks
to it, and `.claude/skills` symlinks to `.agents/skills/`
([ADR-0002](docs/adr/0002-agent-portable-instruction-surface.md)). Edit only
the canonical paths; keep content tool-agnostic.

## Skills (follow these for common tasks)

Step-by-step procedures live in [.agents/skills/](.agents/skills/); follow
them rather than improvising, whichever agent you are:

<!-- One bullet per skill: **When to use it** → [.agents/skills/<name>/SKILL.md].
Add a skill whenever you find yourself re-explaining a multi-step procedure.
Start from .agents/skills/TEMPLATE/SKILL.md. -->

- *(none yet — add the first one when a procedure repeats)*

## Code conventions (live — the code exists)

<!-- The most important section. Rules here must describe the code AS IT IS,
not aspirations — an agent that follows a stale rule produces broken work.
Graduate a rule into this list only when the code enforcing or exemplifying
it has landed; until then it lives in a design doc as intent.

Write rules imperatively and concretely, each with enough mechanism to be
followed without asking ("Storage only via db.Open(slug, migrations)" — not
"use the database layer"). Point at one canonical example in the code for
every structural rule. Rules that remove a degree of freedom are the
valuable ones: every choice an agent doesn't have to make is a failure mode
removed. -->

- **Rules go in code** ([ADR-0011](docs/adr/0011-put-rules-in-code-not-in-model-judgment.md)). Write any behaviour that can be stated as a rule as Go code with a test, never only as prompt text or a model call. This covers policies, gates, state transitions, validation, routing, scheduling, limits and parsing of agent output. A model's output that feeds a decision is structured output that code validates; code makes the decision.
- **Go lives in the three modules of `go.work`:** `proto`, `service` and `desktop` ([ADR-0007](docs/adr/0007-split-the-repository-into-three-go-modules.md)).
  - Run Go commands inside a module directory, or use the `Makefile` targets, which loop over `MODULES`.
  - `service` and `desktop` carry `replace github.com/frostyard/pocketgo/proto => ../proto`, so `go mod tidy` works outside the workspace.
  - A new module needs all four of these in one change: a `use` line in `go.work`, a `MODULES` entry and a `CROSS_<module>` list in the `Makefile` (`make cross` fails without the list), and a `directories` entry in `.github/dependabot.yml`.
  - A new command `<module>/cmd/<name>` gets `.gitignore` entries for `/<module>/<name>` and `/<module>/cmd/<name>/<name>`.
  - The `Makefile`'s behaviour is pinned by `test/makefile.test.mjs`; change both together.
- **PiG stays in `service/pig`** ([ADR-0004](docs/adr/0004-build-the-service-on-pigs-durable-harness.md)). No other package imports `github.com/MichaelKinsy/PiG`; the depguard rule in `.golangci.yml` fails `make lint` when one does. PiG is pinned to a release in `service/go.mod`. A release tag is required, because the module proxy and checksum database do not serve PiG's later commits.
- **The protocol changes in one pull request.** A change touches all of these together: [docs/specs/protocol.md](docs/specs/protocol.md), the `proto` types, `service/internal/api` and its tests.
- **Work-database migrations are append-only.** `service/internal/store/store.go` applies `migrations` in order. Never edit a released migration; add a new entry.
- **`pocketd serve` starts in a fixed order:** lock the data folder, check the sandbox, bind, open, wire (the team included: `engine.SetTeam`), `RetireEnded`, `WatchAll`, `engine.Resume()`, then `UpgradeTeamTools`, which serving waits for, then `RefreshAllLessons` and `StartDelivery`, then `StartPulls`, none of which serving waits for. `pig.Open` runs no task, so a callback never fires before its receiver is wired; nothing that submits to a conversation comes before `Resume`, since PiG's `Submit` may let the scheduler run.
- **Tools reach files and commands only through `guardedEnv`** (`service/pig/env.go`). It wraps every path-taking method and `Exec`, and `Exec` runs every command in the `Sandbox` (`service/pig/sandbox.go`, [ADR-0012](docs/adr/0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md)). `pig.Open` refuses to run without one; only tests set `Unsandboxed`. A new execution adapter returns its environment wrapped the same way, with a sandbox of its own.
- **Which model runs is code:** `router.Choose` in `service/internal/router`, and every choice is recorded with its rule ([ADR-0013](docs/adr/0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md)). A new rule goes there, with its test.
- **Profiles are checked in code:** `service/internal/profiles` validates every field; a field the code does not know makes the profile invalid, and so does a repository named twice in any case. A new field goes there, with its test.
- **The team's rules are code:** who may delegate to whom, the desk's cap, the limits on tasks, reports and asks, and the texts pocketd writes live in `service/internal/team`; the team tools are offered by role in `service/pig` (`roleTools`) and check the role again in `service/internal/api` ([ADR-0014](docs/adr/0014-delegate-in-threads-with-team-tools-in-code.md)). A new rule goes in `team`, with its test. Every step of a team call is keyed by the calling session, its tool task and the call's id (`api.TeamCall`), so a call run again after a restart finds what it did, and a reused call id is a new call. A thread or ask change and its event go out together under `Server.workMu`. A message pocketd owes a session (an answer, a report) counts as delivered only once `Engine.Placed`, by its request key, finds it in the transcript, never when `Send` returns; it is read, placed and noted under `Server.outboxMu`, which is taken before `workMu`, never after.
- **Lessons are kept and approved in code** ([ADR-0015](docs/adr/0015-keep-each-agents-lessons-approved-by-the-owner-in-code.md)). Their text, limits and texts live in `service/internal/team`; an approval holds only for the text the request names, in the transaction that decides it (`store.DecideLesson`). pocketd owns only the section it wrote at the end of an agent's instructions, from the last `profiles.LessonsMarker` to the `profiles.LessonsEnd` that ends them (`team.WithoutLessons`): `Engine.SetLessons` replaces only that, and a profile whose identity holds either as a line of its own is invalid. The section depends on the lessons in force alone, never on the profile's name or role, so a refresh never rewrites a session's checked identity. A change to the lessons in force reaches the agent's sessions by a delivery pass (`Server.lessonsChanged`), never by a direct write, so two refreshes never race. A lesson change and its event go out together under `Server.workMu`.
- **Pull requests are read on GitHub in code** ([ADR-0016](docs/adr/0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md)). `service/internal/github` reads one with `gh api`, run by `Engine.GH` in the sandbox with the service user's sign-in, and checks GitHub's answer field by field; `team.CheckPull` decides whether it belongs to a thread. A report's pull requests are checked before it is filed, and linked in the transaction that files it (`store.FileReportWithPulls`); a report filed already is never checked again. A new rule goes in `team` or `github`, with its test.
- **pocketd starts every cross-review, and nothing else does** ([ADR-0019](docs/adr/0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md)): `Server.reviewPassOf`, for the pull requests just linked or read, one review thread a head and base branch (`team.ReviewKey`), at most `team.MaxReviews` a pull request, the author's agent on `router.ReviewerOf`'s model, with `read`, `team_diff` and `team_verdict` and no shell. Only those threads get and file `team_verdict`, and they file nothing else; a verdict closes its review and keeps the author's message in one transaction (`store.AddReview` with a `ReviewFinish`), and nobody else is told. A verdict backs its pull request only at the head, base branch and merge base it was filed on (`store.LatestReview`); a merge base GitHub does not name backs nothing (`errNoMergeBase`). git on a review's worktree, `team_diff` included, runs with `team.ReviewGitOptions`, the one list: no replacement object, hook, fsmonitor or filter of the checkout, checked by `worktree.attributesIsolated` for what no option turns off. A new review rule goes in `team` or `router`, with its test.
- **A thread's worker is code** ([ADR-0021](docs/adr/0021-give-a-threads-specialist-a-worker-on-an-api-key-model-in-code.md), [ADR-0022](docs/adr/0022-run-any-session-on-any-sign-in-and-choose-workers-from-the-owners-list.md)): `team_worker` for a thread's specialist, one worker a thread (`store.RecordWorkerSession`), its reply sent once through the outbox (`Server.workerRunEnded`). Its model is `router.Choose` with `Request.Worker`: the model the supervisor asked for (`team_delegate`'s `worker`, resolved by `router.AskedWorker` to an entry of the owner's list and kept on the thread), else the profile's `worker`, else the first model pocketd runs of the owner's list (`workers.json` in the profile folder, checked by `profiles.LoadWorkers`). Never add a rule that keeps a model from a kind of session unless the owner asked for it: any model with a sign-in, an API key's included, runs any session (`Engine.Models`). `pocketd login` takes a key only for a provider of `pig.APIKeyProviders` or an owner's provider with `key: true`, only a literal one (`literalKey`). A new worker rule goes in `team` or `router`, with its test.
- **A shift's rounds are code** ([ADR-0024](docs/adr/0024-run-a-delegated-thread-in-rounds-as-a-shift-driven-in-code.md)): `team_delegate`'s `shift`, checked by `team.CheckShift`, recorded with its thread (`store.CreateShiftThread`); `Server.shiftPass` ends a round only when its message is placed (found by its request key, `Engine.PlacedEntry`) and the specialist and its worker are idle with nothing owed to their sessions from any thread (`store.Owed`), commits each decision under `outboxMu` after reading the round again (`commitRound`), reads the verdict with `team.ParseShiftStatus`, and decides the stops in code. Each round's message is kept with its round's change and placed only while it begins the round under way; a shift's end files its report in the same transaction (`store.EndShift`, `store.FileReportWithPulls`), and an owner's change claims its request there. A change that makes a round's message obsolete (a stop, the owner's pause, a close) withdraws it from PiG's queue first and commits under `outboxMu`; `stopMu` comes before `outboxMu` and `workMu`, never after. Shift tests run passes and deadline wakes by hand (`runShiftPasses`, `fireShiftWakes`), never on a goroutine or a timer. Never let a model decide a stop. A new shift rule goes in `team`, with its test.
- **The owner's own model providers are code-checked config** ([ADR-0023](docs/adr/0023-add-the-owners-own-model-providers-from-a-file-checked-in-code.md)): `providers.json` in the profile folder, read with every key as written by `profiles.LoadProviders`, built by `pig.ownProvider` with PiG's public `ai` package alone, and registered at start; its `builtinModels` add models to PiG's own providers (`pig.addModels`). Stored keys reach PiG only through `keptCredentials`: from one read of the sign-ins, the snapshot checked, a literal API key as stored, never a `!command` or `$VAR`. The owner's providers' requests go through `ownClient`, which follows no redirect to another origin. A provider's address never enters this repository: it lives in the owner's private folder. A new field goes in `profiles`, with its test.
- **Cross-review verdicts and `ready-for-human` are code** ([ADR-0017](docs/adr/0017-record-cross-review-verdicts-and-set-ready-for-human-in-code.md)). A model's family is `router.Family`, its maker; `team.CheckVerdict`, `team.CheckReviewer` and `team.CheckHead` decide whether a `team_verdict` counts, and `team.WantsReady` whether a linked pull request has the label. `Server.syncReady` is the only writer of the label, and pocketd writes nothing else on GitHub: never a review, an approval or a merge. A new rule goes in `team`, `router` or `github`, with its test.
- **The data folder's two databases are one state:** back up and restore `durable.sqlite` and `work.sqlite` together. A key that crosses them is never a row number of one (use a thread's owner, an ask's key, a digest), but pocketd does not reconcile one restored apart from the other.
- **git that pocketd runs on an agent's checkout runs in the sandbox:** through `Engine.Git` (`worktree.Git`), never `exec` from pocketd itself, since a hook or filter of the checkout would run with pocketd's rights. The program is a git only root can change (`trustedGit`), never one found on the PATH, as gh is (`trustedGH`).
- **Which tool calls wait for the owner is code:** `approvals.NeedsApproval` in `service/internal/approvals`; none do today ([ADR-0012](docs/adr/0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md)). Change it there, with its test.
- **The desktop's rules live in `desktop/internal/state`, which imports no UI** ([ADR-0010](docs/adr/0010-port-the-desktop-client-selectively-from-frostyard-pocket.md)). The depguard rule `desktop-state-boundary` in `.golangci.yml` fails `make lint` when it imports MyGo or frostui; its tests may use `frostuitest`. A rule about pocketd's data goes there with a test: merging, a session's state, which approvals show where, retries, backoff, URL checks. `desktop/internal/app` only draws the state and calls `state.Store`. Example: `Data.StateOf` and its test.
- **The desktop opens a pull request only at the link `state.PullURL` builds** from its repository and number (`https://github.com/<owner>/<name>/pull/<number>`), never at a URL pocketd sent (`TestPullURLIsBuiltFromItsRepositoryAndNumber`). The board groups threads by `Data.NextMover`; a new grouping rule goes there, with its test.
- **The UI thread owns the desktop's state.** Never touch `app.App` or `state.Store` from a goroutine. A request goes through `Store.do`, which runs it on a goroutine and posts the result with `Options.Post`. Answers of an earlier sign-in, and events of an earlier stream, are dropped.
- **A fetch's answer goes through `Data.Begin` and a `Data.Set*` method with that fetch's token, and its failure through `Data.Set*Error`.** Never assign fetched data directly. An event that came after the fetch began wins over the fetch's copy, and so does a fetch begun later, per list and per session summary (`TestALateDetailDoesNotUndoAnEndedRun`, `TestAnOlderFetchNeverUndoesANewerSessionSummary`). A view says a list is empty only when its status is `ListLoaded` (`TestAnInboxThatFailedToLoadSaysSo`).
- **A desktop request that changes state takes its id from `requestIDs.For(key, action)`**, and `Done(key)` once it succeeded. A failed action tried again unchanged then reuses its id (`TestAMessageTriedAgainKeepsItsRequestID`). A failed abort keeps its id only if its session was not seen idle at any moment since the abort was sent (`Store.settleAborts`), so the next run's abort is a new request (`TestAnAbortWhoseAnswerWasLostDoesNotTargetTheNextRun`, `TestARunThatEndsWhileTheAbortIsInFlightEndsItsID`).
- **The device token goes only through `config.Credentials`:** the keyring, or a private file only when the keyring fails with `errors.ErrUnsupported`. A keyring that works but lacks the token means there is none. No environment variable or flag switches the keyring off; tests inject a keyring. The sign-in form drops the token once it signed in. Never log a token.
- **The desktop approves and merges only as the owner, behind frostyard/pocket's gates** ([ADR-0018](docs/adr/0018-approve-and-merge-as-the-owner-from-the-desktop-with-pockets-gates.md)). `desktop/internal/github` is frostyard/pocket's `internal/github`, ported whole: change a gate there first, or in a new ADR. The GitHub App sign-in lives in the keyring only (`config.GitHubApp`, never a file), behind `config.LockSignIn`, and a pair is kept only for `github.Login` and `github.AppClientID` (`state.PairProblem`). A call names a pull request by its repository and number (`state.MergeRef`), never its URL, and a merge needs pocketd's backing at the head and base branch the gates read: a ready verdict and green CI there, on an open pull request that is no draft (`state.Backed`), in pocketd's answer at the confirmation (`state.FreshPull`, which only `Store.ReadPull` makes, after pocketd reads the pull request on GitHub with `GET /v1/pulls/{id}`: that answer, the list read after it and the row merged with the events all back it). The flow's rules are in `state.GitHub`; `app` only draws them. A credential write counts as `TokenSource` work under its mutex before the mutex goes (`admit`, `Lease`'s delete), so `Close` never lets the sign-in lock go before it.
- **The token stays on the service's origin.** Every request goes through `state.HTTPClient` over `state.DirectTransport`, which uses no proxy, whatever `HTTP_PROXY` says. It follows a redirect only when `state.SameOrigin` holds (the same scheme, host and port) and the method stays the same. Only loopback and `*.ts.net` names skip the plain-HTTP warning (`state.NeedsHTTPWarning`); an address in `100.64.0.0/10` warns.
- **Signing out ends the automatic sign-in first.** `App.forgetSignIn` clears `Settings.Server` and saves it, then deletes the token; `signOutProblem` tells the owner what was left, and says the next start may sign in again only when both steps failed (`TestSigningOutEndsTheAutomaticSignInEvenIfTheKeyringKeepsTheToken`). A sign-in whose token the keyring will not keep clears `Settings.Server` too.
- **Desktop tests use no wall clock and no sleeps.** The stream's backoff waits on `state.Options.After`, so pass a fake clock. View tests draw with `ui.NewTesterWith` on a `frostuitest.Loop`, against `desktop/internal/pocketdtest`, which `Server.Transport` serves in memory: no test opens a socket. After a view change, run `make snapshot` and look at the PNGs.
- **Build without CGO** (the `Makefile` exports `CGO_ENABLED=0`). Only `make race` turns CGO on.
- **`make verify` needs three tools:** Go (go.work's go line, the only Go pin), golangci-lint (pinned in `mise.toml`) and Node.js 22 or later. `mise install` provides the last two. The desktop's view tests also need Pango and cairo, but no display (Debian and Ubuntu: `fonts-dejavu-core libpangocairo-1.0-0`; CI installs them). `make verify` also runs `go tool mygo vet .` in `desktop`.
- Run `make verify` before you call any change done. CI job `verify` runs
  `make ci`, which also checks the corrections records against the PR base: no
  record removed or renamed, only an empty `promoted_to` filled, and the old
  corrections file does not come back. Run `make ci BASE=origin/main` for the
  same check locally.
- A doc that says what a gate refuses names the command, the condition it
  runs under (for example `make ci BASE=<ref>`, not `make ci`) and each change
  the gate allows (for example a filled empty `promoted_to`), as the
  `Makefile`, the workflow and the gate's code define them.
- `mise.toml` and `mise.lock` pin every tool the gate runs (ADR-0043 in core);
  run `mise install` to get those versions. Change a pin only with `mise lock`,
  in a commit of its own.

**Test reliability.** No sleeps for timing (wait on a condition or inject a
clock). No network and no wall clock in unit tests (inject them). A flaky test
gets an issue, then a fix or a quarantine (skip with the issue link) in the
same PR. Never retry a job or a test until it is green.

**Blocker to guard.** When a review (agent cross-review, owner review, or a
human) finds a blocking point, the PR that fixes it also adds a guard: a
targeted test that fails without the fix (preferred), or an `AGENTS.md` rule
when no test can catch it. The same PR adds 1 record file under
`.memory/corrections/` (name `<date>-pr-<N>-<words>.json`; `evidence`: the PR
and the review; `promoted_to`: the test or the rule). See `.memory/README.md`.

**Risk paths.** `.github/risk-paths.json` is a level list (schema version 2):
every glob carries a level, and a PR that touches one gets that level label from
`.github/workflows/risk-paths.yml` (`level:L1` read every line, `level:L2` read
the diff, `level:L3` skim, `level:L4` title and CI), plus `risk:high` at L1 and
L2, which declares tier 3. Core computes the level from the matched globs, the
`default_level` and the org floors, and it never lowers a level label that the
PR already has. The same caller removes `ready-for-human` when the head moves
after the label. Add a path when you add a file of that kind (signing, auth,
publish, secrets, destructive operations, CI), with a `why` that names its
category. The gate requires schema version 2.

**Vendored files.** `scripts/lib/risk-paths.mjs`,
`scripts/lib/check-readme.mjs` and `scripts/check-corrections.mjs` are copies
from frostyard/core; never edit them by hand (`make verify` checks their hashes).
The template gate runs the README checker unchanged for root `README.md`
([core README spec](https://github.com/frostyard/core/blob/main/docs/specs/readme.md),
baseline B17, rules R1 to R12).

## Repository boundary

The repository is public.

- **Secrets never enter it:** provider sign-ins, Coder tokens, GitHub tokens or
  keys, or the service's data folder.
- **Real agent profiles stay private.** Profiles that name hosts or addresses
  on the owner's network stay in the private pi-pocket-config repository.
  This repository carries example profiles only.
- **Build outputs are not committed.**
- **Releases:** none yet; `main` is the only supported branch.

## Documentation rules (enforced)

Docs live in `docs/` in four categories. **Every new doc starts from its
category's `TEMPLATE.md`** and follows its structure:

- `docs/adr/` — why we decided. Immutable once Accepted; reversals are new
  ADRs that mark the old one Superseded. This includes ADRs 0001 to 0003 that
  come with the template: their `YYYY-MM-DD` date is a placeholder, not a
  draft status. A new org-wide rule goes in a core ADR listed in
  `docs/org-adrs.md`; a new project rule goes in a new ADR.
- `docs/design/` — how it fits together. Living; updated in place to match
  reality.
- `docs/specs/` — exact contracts. Change only alongside implementing code.
- `docs/plans/` — order of work. Phases with "Done when" outcomes. A
  phase is done only when its "Done when" was seen as written, through the
  path it names (the desktop, not the protocol beneath it). Evidence that
  covers part of it goes under an open checkbox.

### Cross-linking is mandatory

A doc without its required links is incomplete — do not finish a docs change
until they exist, in both directions:

- **ADR** → links every design doc/spec it shapes, and prior ADRs it builds on.
- **Design doc** → links the ADR(s) providing its rationale, the spec(s)
  pinning its contracts, and the roadmap phase that builds it.
- **Spec** → links its motivating ADR(s) and the design doc showing where it
  fits.
- **Plan** → every phase links the design docs/specs it implements; resolved
  open questions become ADRs.

When you touch a doc, verify its links still hold (targets exist, section
anchors valid) and add the back-links on the targets. Use relative paths.

### Housekeeping

- New doc ⇒ add a line to the index in [docs/README.md](docs/README.md).
- New significant decision ⇒ new ADR *first*, then update the affected design
  docs/specs in the same change.
- Convert relative dates ("next weekend") to absolute dates in all docs.
