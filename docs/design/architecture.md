# System architecture

Living document. Rationale: [ADR-0004](../adr/0004-build-the-service-on-pigs-durable-harness.md), [ADR-0005](../adr/0005-keep-the-agent-loop-and-model-credentials-in-the-service.md), [ADR-0006](../adr/0006-share-the-wire-protocol-as-go-types-in-one-module.md), [ADR-0007](../adr/0007-split-the-repository-into-three-go-modules.md), [ADR-0008](../adr/0008-keep-work-records-in-the-services-own-tables.md), [ADR-0009](../adr/0009-run-agents-in-coder-workspaces-through-pi-env-and-the-rest-api.md), [ADR-0010](../adr/0010-port-the-desktop-client-selectively-from-frostyard-pocket.md), [ADR-0011](../adr/0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0014](../adr/0014-delegate-in-threads-with-team-tools-in-code.md), [ADR-0015](../adr/0015-keep-each-agents-lessons-approved-by-the-owner-in-code.md), [ADR-0016](../adr/0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md), [ADR-0017](../adr/0017-record-cross-review-verdicts-and-set-ready-for-human-in-code.md), [ADR-0018](../adr/0018-approve-and-merge-as-the-owner-from-the-desktop-with-pockets-gates.md), [ADR-0019](../adr/0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md).
Contracts: [specs/protocol.md](../specs/protocol.md).

**Status:** partly built. The protocol, the service's sessions, approvals and event stream, and the desktop client's sign-in, sessions, Inbox and session view are built (Phase 1). Profiles, the router, delegation in threads with asks, lessons, linked pull requests, and the desktop's work board, with asks, lessons and pull requests in the Inbox, are built (Phase 2, slices 1 to 5). Every other section is intent, and moves to description as the [roadmap](../plans/roadmap.md) lands it.

## Overview

The system is a persistent workspace for running a team of AI agents across software projects.

**Working with agents:**
- A person works directly with an agent in a shared session, or asks a supervisor to delegate a task to a specialist.
- Each agent has a profile, a working directory, a set of tools it can use, and memory it can propose for the owner to approve.
- Agents read and change files, run commands and tests, consult other agents, and report back to whoever assigned the work.
- For longer jobs, an agent works in rounds toward a stated objective. It stops when it meets a completion condition, reaches a limit or deadline, or someone stops it.

**The service** keeps the work running while the desktop is closed, and records enough progress to recover after a restart.

**The desktop** organizes the work around two questions: what needs my attention, and what is still in motion?
- **Inbox:** questions, approval requests, proposed lessons and pull requests ready for review.
- **Work board:** each task is a thread connecting its plan, agents, sessions, decisions, pull requests and final report.
- **From a thread,** the owner can inspect a diff, answer a question, redirect or stop work, or schedule a follow-up.

**Events and records:** external events wake the responsible agent. The service records model choices, token usage, cost, review rounds and outcomes, so the owner can judge which ways of working produce good results.

There is one owner, working from any of their devices. A shared session is the owner and an agent in the same session.

```
desktop (MyGo) ──┐                    ┌──► model providers (subscription sign-ins)
phone (later) ───┼── REST + SSE ──► pocketd ──► PiG durable harness ──► Durable store (SQLite)
                 │   (proto)          │   └──► work database (SQLite)
                                      └──► execution adapters ──► local git worktree
                                                              └──► Coder workspace (coder ssh → pi-env)
```

## Design

### Repository layout

Three Go modules, joined by a root `go.work`. They sit beside the template's files: `AGENTS.md`, `docs/`, `scripts/`, `test/`, `.github/` and `.memory/`. `service` and `desktop` also carry a `replace` for `proto`, so `go mod tidy` resolves it outside the workspace.

```
proto/                 wire types and the Go client
service/
  cmd/pocketd/         the headless service
  pig/                 the only package that imports PiG
  exec/local/          execution adapter: a git worktree on the service host
  exec/coder/          execution adapter: a Coder workspace
desktop/
  cmd/pocket/          the desktop app
  internal/state/      the data layer and state, no UI (a phone client reuses it)
  internal/app/        the window: views on MyGo and frostui
  internal/config/     settings, and the device token in the keyring
go.work
```

All three build with `CGO_ENABLED=0`. A phone build is the one exception.

**`make ci`:**
- keeps the template's checks, which need Node.js 22
- adds `go vet`, `gofmt`, golangci-lint, tests with `CGO_ENABLED=0`, and a cross-platform build, for each module
- adds `go tool mygo vet` for the desktop, which checks MyGo's build lifetimes. The desktop's view tests need Pango and cairo, but no display; CI installs them.

### Protocol

`proto` holds every wire type and the Go client. The service and every client import it. [specs/protocol.md](../specs/protocol.md) is the contract; `service/internal/api` serves it.

- **Commands:** REST calls with JSON bodies.
- **Live updates:** one SSE stream. It opens with a `hello`, at the client's `Last-Event-ID` or at the newest event, so a client always has an event to come back from; before a `resync`, at a number that resyncs again. Each entry is followed by its session, with the new `updatedAt` and usage. A session's live state carries the messages that wait for a run.
- **Version:** the client sends the protocol version, and the service checks it. The version is 2: a client and a service of different versions refuse each other, so neither keeps code for the other's.
- **Request ids:** every state-changing request carries one. The service applies each request id once and records its author.
- **Authentication:** one owner, with a token per device (`pocketd device add NAME`).

### Service

`pocketd` runs agents on PiG's `durable/harness`.

**Built (Phase 1):**
- **`service/pig`:** the only package that imports PiG; golangci-lint's depguard rule refuses an import anywhere else. It opens the harness over `durable.sqlite`, registers PiG's coding tools (read, write, edit, bash) and the approval hook, runs every command under bubblewrap ([ADR-0012](../adr/0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md)), follows each session with a conversation watch, and converts entries and live state to `proto` types through Pi's JSON wire form. It gives each user entry the submission that placed it, read from the durable store's submission records, and each model the thinking levels PiG reads from its catalog entry. A session's live state lists its queued messages from the conversation's `pi.inbox`. A watch update it fails to read is read again after a second, not at the next commit, and so is a session event `service/internal/api` fails to read.
- **`service/internal/store`:** the work database, `work.sqlite`, with devices, sessions, request ids, approvals, threads and asks, versioned by append-only migrations.
- **`service/internal/approvals`:** the approval rule (`NeedsApproval`: no tool waits, [ADR-0012](../adr/0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md)), and the manager that records an approval, waits for the decision and wakes the call.
- **`service/internal/events`:** the event hub. Its numbers grow across restarts, it keeps 4,096 events for reconnecting clients, and it sends a resync when a client missed more. It gives each stream the seq of its `hello`, and a client whose last event came just before a lost one gets a resync.
- **`service/internal/api`:** the HTTP server of the protocol.
- **`service/internal/auth`:** device tokens. Only their SHA-256 is stored.
- **`service/internal/profiles`:** reads the agents' profiles from the profile folder, `-profiles` (default pi-pocket-team's `~/.config/pi-pocket-team/agents`), and checks every field in code; only `<id>.md` files are profiles ([ADR-0013](../adr/0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md)).
- **`service/internal/router`:** chooses every model and names the rule that chose it: the owner's model, the profile's, or the profile's model under a provider alias (`openai` to `openai-codex`). The session records the rule as its `route`.
- **`service/internal/team`:** the rules of delegation, reports and asks ([ADR-0014](../adr/0014-delegate-in-threads-with-team-tools-in-code.md)): who may delegate to whom, the desk's cap, the limits on tasks, reports and asks, the thread's worktree and branch names, and the texts pocketd writes: the brief, a supervisor's roster, a report and an answer. It also holds the lessons' rules ([ADR-0015](../adr/0015-keep-each-agents-lessons-approved-by-the-owner-in-code.md)): their text, their limits, `team_memory`'s answers and the lessons section of an agent's instructions.
- **`service/internal/github`:** reads a pull request, its labels and CI at a commit on GitHub with `gh`, through a runner pocketd gives it (`Engine.GH`, in the sandbox), and checks GitHub's answer field by field ([ADR-0016](../adr/0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md)). It writes one thing: the `ready-for-human` label (`SetLabel`, [ADR-0017](../adr/0017-record-cross-review-verdicts-and-set-ready-for-human-in-code.md)). `team.CheckPull` holds the rules of which pull requests belong to a thread.
- **`service/internal/worktree`:** makes a thread's git worktree next to the repository's checkout, on its own branch from `HEAD`. git runs in the sandbox, through the engine, so a hook or filter of the checkout runs with an agent's rights.
- **`cmd/pocketd`:** the commands `serve`, `device add|list|revoke`, `login PROVIDER`, `models`, `profiles` and `version`. `serve` takes `-desk-cap` (default 12), and at start sends each report and answer that did not reach its session.

**Intended:**

- **Durable store:** holds conversations, tasks, submissions and the usage ledger.
- **Work database:** a separate SQLite file holding threads, plans, plan tasks, asks, pull requests, review verdicts, lessons, rounds and run records.
- **`service/pig/`:** wraps the harness, PiG's `env`, `ai` and `mcp` packages, and the sign-ins.

**Agents and profiles:**
- **Profile fields:** `model`, `role` (supervisor, clerk or agent), `repositories`, `workspace`, `home`, `mcp`, `tools` and `private`. The body of the profile is the agent's identity prompt.
- **Applying a profile (built):** a session from a profile gets the routed model, exactly the profile's tools, the profile's identity after pocketd's instructions, and the profile's home as its folder; a private profile works only there. The profile folder is read-only to agents, as pocketd's start files are.
- **MCP and skills:** the service connects these to durable conversations itself.

**Delegation (built) and consults:**
- The supervisor delegates a task to a specialist in a thread ([ADR-0014](../adr/0014-delegate-in-threads-with-team-tools-in-code.md), [protocol rules 24 to 30](../specs/protocol.md)). The team tools are Go code in `service/pig`, offered by role: `team_delegate` and `team_close` to a supervisor's session, `team_message` to a thread's specialist session. Each calls `service/internal/api`, which checks the rules in `service/internal/team`, and every step is keyed by the calling session, its tool task and the call's id, so a call run again after a restart finds what it did.
- `team_delegate` records the thread, checks the target's profile again, makes its worktree (marked with the thread, so an older branch of the same name is never reused), starts the specialist's session with the router's model, gives the thread the session, sends the brief, then opens the thread. A thread that cannot start, or is closed while it starts, gets no brief and leaves the desk.
- `team_message` files the report and its asks in one transaction, once per call, puts the asks in the owner's Inbox, and sends the report to the supervisor's session as a message; one that cannot go now is sent again until it does. The owner's answer to an ask reaches the asking session as a message, once; one that cannot go now is sent again until it does.
- Thread and ask events go out with their change, in commit order.
- A consult asks another agent a question and records its finding.
- Each piece of work reports back to whoever assigned it.

**Rounds:**
- An objective, a completion condition, a round limit, a deadline, a spend limit and a stop.
- A round that fails the same check three times ends the job.
- The job pauses when the owner writes in the session.

**Approvals (built):**
- `BeforeTool` blocks a call that needs the owner, and the request appears in the inbox.
- The answer is stored in the work database and pinned in the task's Durable memo, so a hook that runs again after a restart reads the stored answer.

**Lessons (built, [ADR-0015](../adr/0015-keep-each-agents-lessons-approved-by-the-owner-in-code.md), [protocol rules 31 to 35](../specs/protocol.md)):**
- An agent keeps lessons with `team_memory`, a Go tool every session started from a profile gets. Every change waits for the owner; a change to a lesson in force waits as a proposal, and the lesson stays in force until then.
- The owner approves exactly the text they were shown, from the Inbox: `service/internal/store` holds an approval to the text the request names, in the transaction that decides it.
- Limits, in `service/internal/team`: 300 characters per lesson, once flattened to one visible line; 40 lessons in force per agent; 20 waiting per writer for one agent.
- The lessons in force are a section pocketd writes at the end of the agent's instructions (`team.LessonsSection`, from `profiles.LessonsMarker` on): in a new session, and, by a delivery pass, in every session of the agent once they change and at every start (`Engine.SetLessons`).

**Pull requests (built, [ADR-0016](../adr/0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md), [protocol rules 36 to 40](../specs/protocol.md)):**
- A specialist reports the pull requests it opened with `team_message`. `service/internal/github` reads each with `gh api` in the sandbox (`Engine.GH`), with the service user's sign-in, and checks GitHub's answer; `team.CheckPull` decides whether it belongs to the thread: its repository, its branch, not a fork.
- The work database links each to its thread. pocketd reads the open ones again every 5 minutes, and a thread whose pull requests merged closes.

**Cross-review (built, [ADR-0017](../adr/0017-record-cross-review-verdicts-and-set-ready-for-human-in-code.md), [ADR-0019](../adr/0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md), [protocol rules 41 to 46](../specs/protocol.md)):**
- pocketd starts every review, and nothing else does (`Server.reviewPassOf`, for the pull requests a report links and those each round read): one review thread a head and base branch, at most `team.MaxReviews` a pull request. The reviewer is the author's own agent on the reviewer model of another family (`router.ReviewerOf`), in a worktree detached at the head (`worktree.AddDetached`), with read-only tools, `team_verdict` and pocketd's brief (`team.ReviewBrief`); git on that worktree runs no hook, filter or replacement object of the checkout (`worktree.ReviewGitOptions`). A push or a retarget ends the review of the old head; a session that ends with no verdict ends its review, as soon as its run ends.
- The reviewer files its verdict with `team_verdict`, which refuses any thread that is no review pocketd started. pocketd checks the reviewer's model family against the author's, reads the pull request on GitHub, and records the verdict with that read only at its current head. The verdict closes the review thread, and the author's thread hears it once; the supervisor is not told.
- After a verdict, and at every round, `Server.syncReady` keeps `ready-for-human` in step: it reads CI at the head for a ready verdict, and writes the label only when it must change. A linked pull request carries the label's state, CI's state and the newest verdict at its head, base branch and merge base to the desktop, which shows them on its cards; a merge base that moved since the verdict, or that GitHub does not name, leaves none. The desktop's merge confirmation has pocketd read the pull request on GitHub first (`GET /v1/pulls/{id}`).

**External events:**
- Signed GitHub webhooks and NATS events become submissions with request ids, delivered to the responsible agent.
- Outside text is fenced and defanged before an agent sees it.

**Model router:**
- Model choice for each delegation and each round goes through one router seam.
- It records the choice, the features of the brief and the outcome.
- It picks only models the owner allows.
- The first rule is the profile's model (built: `service/internal/router`, [ADR-0013](../adr/0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md)). How the router decides beyond that is an open question in the [roadmap](../plans/roadmap.md#open-questions).

**Run records:**
- the model and thinking level
- the profile or brief version
- tokens and cost
- review rounds and verdicts
- the outcome

### Policies enforced in code

Every rule below is service code with tests; prompts only inform agents of them. A model's output that feeds a decision is structured output that code validates, and code decides.

- **Cross-review (built, [ADR-0017](../adr/0017-record-cross-review-verdicts-and-set-ready-for-human-in-code.md), [ADR-0019](../adr/0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md)):** pocketd starts each review in code, one a head, within the owner's budget; the review thread files a structured verdict (`ready`, `not-ready`, `unclear`) with `team_verdict`. Code refuses one from the author's thread, from the author's model family (`router.Family`, the model's maker), or at a head that is not the pull request's head on GitHub.
- **The plan gate:** a plan is settled when no ask, comment or consult is open, and the owner approves an exact revision.
- **Approve or merge:** the shared bot account never does both on one pull request. pocketd itself writes only the label: no review, approval or merge. Approve and merge as the owner, from the desktop, is next (Phase 2, slice 6b).
- **`ready-for-human` (built, ADR-0017):** pocketd puts the label on a linked open pull request, no draft, exactly while the newest verdict at its head is `ready` and CI there is green (`pr-triage` left out), and takes it off otherwise, whoever put it on: a head that moves takes it off. Core's `pr-triage` removes it on a push too.
- **Leases:** on shared test resources.
- **Private agents:** they work only in sessions the owner starts in their home. They take no delegation and report to nobody (built: `team.CheckDelegation`).
- **The desk:** at most `-desk-cap` threads that are not closed (built).
- **Per-profile limits:** on MCP servers and tools.

### Desktop client

The desktop client is built on MyGo and frostui, in the `desktop` module. It talks to `pocketd` only through `proto` ([specs/protocol.md](../specs/protocol.md)).

**Built (Phases 1 and 2):**
- **`desktop/internal/state`:** the data layer and state, with no UI import (a depguard rule in `.golangci.yml` refuses MyGo and frostui there), so a phone client can reuse it ([ADR-0010](../adr/0010-port-the-desktop-client-selectively-from-frostyard-pocket.md)). Every rule in it is code with a test ([ADR-0011](../adr/0011-put-rules-in-code-not-in-model-judgment.md)):
  - **`Store`:** one sign-in. Each request runs on a goroutine and posts its result to the UI thread. Answers of an earlier sign-in are dropped, and a 401 signs out.
  - **`Link`:** the event stream. When it drops, it waits as `Backoff` says (1 s, doubling to 30 s, on an injected clock) and reconnects with `Last-Event-ID` set to the last event applied. The store fetches the sessions, the pending approvals, the open asks, the lessons, the threads, their pull requests, the profiles and the open session when a stream opens with no event to come back from, and on `resync`. A stream that comes back from an event, the `hello`'s included, misses nothing, so the store fetches again only what failed to load. No fetch waits for the `hello`, which an older `pocketd` never sends.
  - **`Data`:** what the client knows. An event that arrives while a fetch is in flight wins over that fetch's copy. The answer of a fetch begun before one already applied is dropped, per list and per session summary, which both the list and a session's detail carry. A session moves up the list with the session event that follows each of its entries. The messages a session holds for a run show queued at the end of its transcript, from its live state. Each list records whether its last fetch failed, so the Inbox never says that nothing waits when it does not know (`InboxNote`). A session with a pending approval shows as waiting. The open asks merge by the same rule as the approvals, and so do the threads and their linked pull requests, except that a closed thread and a merged pull request stay in their lists.
  - **The board's rule (`Data.NextMover`):** a thread is grouped by who moves it next. Nobody moves a closed thread. The owner moves one with an open ask, or with an open pull request that is no draft (`ToReview`). Otherwise its specialist moves it while it starts or is open, and its supervisor once the specialist reported it done or blocked. `Data.BoardNote` claims a count of the threads on the desk only when they loaded, and how many wait for the owner only when the asks and the pull requests loaded too.
  - **Which pull requests show (`Data.Pulls`):** as `GET /v1/pulls` lists them, the open ones and those of the threads not closed, whichever order the merge's and the close's events came in.
  - **`PullURL`:** the desktop opens a pull request at the link it builds from the repository and number, `https://github.com/<owner>/<name>/pull/<number>`, never at the URL `pocketd` sent; a repository that is no GitHub name gets no link.
  - **`BuildRows`:** the transcript as rows. Each tool result shows with its own call occurrence, since a model may reuse a call id, followed by the reply being written, the tools at work and the calls that wait for an approval. Only the last occurrence of an id takes an approval or a live state, and a busy run queues only the calls of the current round.
  - **Request ids:** a failed send, abort, decision, answer or new session tried again unchanged keeps its request id, so the service applies it once. A failed abort keeps its id only if its session was not seen idle since the abort was sent: the next run's abort is a new request.
  - **`NeedsHTTPWarning`:** the sign-in asks before it sends the token over plain HTTP, unless the host is loopback or a MagicDNS name (`*.ts.net`), which resolves only inside the tailnet. An address in `100.64.0.0/10` still warns: it is shared address space, not proof of a tunnel. Host names are never resolved.
  - **`SameOrigin`:** every request carries the token, so the client follows a redirect only within the service's scheme, host and port, and with the same method. `DirectTransport` sends it without a proxy.
- **`desktop/internal/app`:** the window, on frostui's `PaneShell`:
  - the sign-in screen;
  - a sidebar with the Inbox, the work board and the sessions with their state;
  - the Inbox: every open ask from a thread's specialist, with its suggested answers as buttons and a field for any other answer; every lesson that waits, with Approve and Discard, and the lessons in force, folded, with Forget; every pull request that waits for review, with its thread and Open on GitHub; then every pending approval, with Allow, and Deny with an optional reason. A card previews the call's arguments, and Show All and Copy All give every byte of them before Allow. When a list failed to load, the Inbox says so and offers Try Again, instead of saying that nothing waits;
  - the work board: the threads in four groups, waits for you, with the specialists, with the supervisors, and closed (folded). A thread's card shows its specialist and where it works (its repository and branch, else its folder), why it is in its group, the last report (one longer than 280 characters or four lines folds to four, `ReportFolds`), its pull requests as `GET /v1/pulls` lists them (a closed thread's only while open), and the ways to its specialist's and supervisor's sessions. The owner closes a thread from it, after confirming; a confirmation goes with the sign-in it was asked under;
  - the session view: Markdown text, folded thinking and tool calls, the streaming reply, the owner's queued messages, the run's failure, approval cards docked above the composer, Send and Abort;
  - the New Session dialog: profile, working directory, model, title and thinking level. It asks pocketd for its models and profiles each time it opens, offers Reload Models when there are none, and offers only the thinking levels the chosen model takes. A profile fills in its home and offers its own model first, which pocketd's router resolves. A profile that cannot start is named with its problem.
  - Headless tests drive it against `desktop/internal/pocketdtest`, a fake `pocketd` served in memory, and `make snapshot` writes its views as PNGs.
- **`desktop/internal/config`:** settings in `pocketgo/settings.json` under the user's config folder. The device token goes in the desktop keyring, or in a file only the user can read where there is no keyring; its name carries a SHA-256 of the service's origin.
- **`desktop/cmd/pocket`:** the program. Its app ID is `org.frostyard.PocketGo` and its title PocketGo, so it runs beside frostyard/pocket. `make run` starts it, and `make app` packages it with `go tool mygo build`.

**Intended:**

**Places:**
- **Inbox:** asks, approval requests, lessons and pull requests to review are built ([ADR-0016](../adr/0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md)). Merging from the Inbox waits for the merge gates (Phase 2, slice 6).
- **Work board:** threads grouped by who moves them next is built. A thread's plan and its decisions join it with plans and consults (Phase 2, slice 7).
- **Session detail:** transcript, composer, attachments, diffs and files.
- **The team.**
- **Run records and metrics.**

**Approve and merge** runs from the desktop as the owner, through a GitHub App user token, behind fail-closed merge gates (built, [ADR-0018](../adr/0018-approve-and-merge-as-the-owner-from-the-desktop-with-pockets-gates.md)): `desktop/internal/github` is frostyard/pocket's, ported whole; the sign-in is the App's device flow, kept in the keyring only behind `config.LockSignIn`; `state.GitHub` holds the flow's rules, and the Inbox's pull request card draws Check to Merge, Open Diff for `risk:high`, and Approve + Merge after a confirmation.

**Data layer:** the client's data layer and state are built on `proto`, kept apart from desktop-only views (tray, terminal, native sign-in flows). A phone client on MyGo's iOS target can reuse them.

### Execution adapters

Both adapters implement PiG's `durableenv.ExecutionEnv`: files, exec and watch, with terminals separate.

- **Local:** a git worktree on the service host (built for threads: `service/internal/worktree`). Its commands run in pocketd's bubblewrap sandbox ([ADR-0012](../adr/0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md)), which hides pocketd's files but is not a boundary between agents.
- **Coder:**
  - **Connection:** `env.ConnectionOptions.CommandFunc` runs `coder ssh <workspace> -- pi-env serve`. Templates include a `pi-env` built from the PiG commit the service pins.
  - **Lifecycle:** the service creates, starts and stops workspaces through Coder's REST API, as a Coder service account.
  - **Prebuilds** give warm starts, and **Boundary** limits network egress.
  - **No model credential** enters a workspace.

## Operational notes

- **Where it runs:** `pocketd` runs on the team VM under systemd, as user `team`, the way `pi-pocket@.service` runs today.
- **Deploy:** `make deploy DEPLOY_HOST=team@<vm>` (over ssh), or `make deploy DEPLOY_INCUS=<remote>:<instance> DEPLOY_INCUS_PROJECT=<project> DEPLOY_USER=team` (over `incus exec`, acting as the user with `runuser` and that user's systemd), runs `deploy/deploy.sh`. The script builds `pocketd` for the host's architecture, installs it as `~/.local/bin/pocketd`, installs `deploy/systemd/pocketd.service` as a systemd user unit of that user, and restarts it. Settings go in `~/.config/pocketd.env`, for example `POCKETD_LISTEN=<tailnet address>:8788`. The owner's own commands (`pocketd login`, `pocketd device`) must use the same data folder: pass `-data` when the env file sets `POCKETD_DATA`. The unit is `Type=notify`: pocketd reports ready once its start order completes, so a failed start fails the deploy. The user needs lingering (`loginctl enable-linger`) so the service runs with nobody logged in. Re-running the target upgrades in place.
- **Data folder:** `-data` (default `$POCKETD_DATA`, else `~/.local/state/pocketgo`) holds `durable.sqlite`, `work.sqlite` and `auth.json`. `-listen` defaults to `127.0.0.1:8788`.
- **Sign-ins:** `pocketd login PROVIDER` writes the data folder's own `auth.json`, in Pi's format. It never shares Pi's file: two processes refreshing one OAuth refresh token can sign each other out.
- **One process per data folder.** If two ran on one store, durable tasks would run twice. `pocketd serve` takes an exclusive `flock` on `pocketd.lock` in the data folder and refuses to start without it.
- **Start order:**
  1. take the lock
  2. check the sandbox: one trial command under `bwrap`, or no start
  3. bind the port
  4. open the work database and the harness, with no task running yet
  5. wire the API and the team, deny pending approvals of ended tasks, and watch every session
  6. resume tasks
  7. start the first delivery pass over reports, answers and briefs that did not reach their session, which serving does not wait for
- **Stop order:** on SIGTERM, end the event streams and the delivery passes (waiting for one that runs), shut down HTTP (10 seconds), then close the harness (20 seconds). Running tasks resume at the next start.
- **Agents and the sign-ins:** `service/pig`'s guarded environment wraps PiG's local one.
  - Every file access checks the path PiG will open, after normalization and symbolic links, and refuses the data folder.
  - Every command gets an allowlisted environment, never pocketd's.
  - Model credentials come only from the stored sign-ins; environment and file credential sources are disabled.
  - Every command runs under `bwrap` (`service/pig/sandbox.go`, [ADR-0012](../adr/0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md)):
    - the data folder and `/run/user`, which holds the runtime folders with the D-Bus and systemd sockets, are empty `tmpfs` mounts, under every path a mount shows them at. On the VM, `/home` is a bind mount of `/var/home`.
    - pocketd's program, `~/.config/pocketd.env`, the user's systemd configuration and the agents' profile folder are read-only
    - every folder above them that the user could rename is bound onto itself, so it cannot be renamed or removed
    - the command sees only its own processes
    - everything else, the network included, is as the service user has it: the VM is the boundary
  - The file tools refuse the same paths, through links too, and a rename or removal refuses a folder that holds one.
  - A symbolic link the user can change on the way to a protected name stops the start, and so does a link in the systemd folders to a file the user can change.
  - Before binding, `pocketd serve` proves the sandbox: `bwrap` must show itself as process 1 and every hidden path empty.
  - No tool call waits for the owner, except one that asked under an earlier rule. The host needs bubblewrap, installed by the system, and unprivileged user namespaces (`apt install bubblewrap` on Debian). pocketd uses only a `bwrap` that root alone can change, and `pocketd serve` refuses to start without a working one.
- **Coexisting with pi-pocket:** while both run on the VM, they never share a data folder or a store.
- **Restore the data folder as a whole.** `durable.sqlite` and `work.sqlite` describe one state: the threads, asks and reports of the one name the conversations and submissions of the other. Stop pocketd and copy, or restore, the folder at once. pocketd takes care that a key from one never names the wrong object in the other (thread owners, ask keys, answer digests, attempt checks), but it does not reconcile one database restored to an earlier point than the other.
- **Back up separately, never in this repository:**
  - the data folder's two databases, `durable.sqlite` and `work.sqlite`, as one item: stop pocketd, then copy both
  - provider sign-ins
  - the Coder service-account token
  - the GitHub App token
- **The Coder adapter** needs the `coder` CLI on the VM, signed in as the service account.

## References

- Rationale: [ADR-0004](../adr/0004-build-the-service-on-pigs-durable-harness.md), [ADR-0005](../adr/0005-keep-the-agent-loop-and-model-credentials-in-the-service.md), [ADR-0006](../adr/0006-share-the-wire-protocol-as-go-types-in-one-module.md), [ADR-0007](../adr/0007-split-the-repository-into-three-go-modules.md), [ADR-0008](../adr/0008-keep-work-records-in-the-services-own-tables.md), [ADR-0009](../adr/0009-run-agents-in-coder-workspaces-through-pi-env-and-the-rest-api.md), [ADR-0010](../adr/0010-port-the-desktop-client-selectively-from-frostyard-pocket.md), [ADR-0011](../adr/0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0012](../adr/0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md), [ADR-0013](../adr/0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md), [ADR-0014](../adr/0014-delegate-in-threads-with-team-tools-in-code.md), [ADR-0015](../adr/0015-keep-each-agents-lessons-approved-by-the-owner-in-code.md), [ADR-0016](../adr/0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md), [ADR-0017](../adr/0017-record-cross-review-verdicts-and-set-ready-for-human-in-code.md), [ADR-0018](../adr/0018-approve-and-merge-as-the-owner-from-the-desktop-with-pockets-gates.md), [ADR-0019](../adr/0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md)
- Contracts: [specs/protocol.md](../specs/protocol.md)
- Built in: [roadmap — Phases 1 to 5](../plans/roadmap.md)
