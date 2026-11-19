# System architecture

Living document. Rationale: [ADR-0004](../adr/0004-build-the-service-on-pigs-durable-harness.md), [ADR-0005](../adr/0005-keep-the-agent-loop-and-model-credentials-in-the-service.md), [ADR-0006](../adr/0006-share-the-wire-protocol-as-go-types-in-one-module.md), [ADR-0007](../adr/0007-split-the-repository-into-three-go-modules.md), [ADR-0008](../adr/0008-keep-work-records-in-the-services-own-tables.md), [ADR-0009](../adr/0009-run-agents-in-coder-workspaces-through-pi-env-and-the-rest-api.md), [ADR-0010](../adr/0010-port-the-desktop-client-selectively-from-frostyard-pocket.md), [ADR-0011](../adr/0011-put-rules-in-code-not-in-model-judgment.md).
Contracts: [specs/protocol.md](../specs/protocol.md).

**Status:** partly built. The protocol, the service's sessions, approvals and event stream, and the desktop client's sign-in, sessions, Inbox and session view are built (Phase 1). Every other section is intent, and moves to description as the [roadmap](../plans/roadmap.md) lands it.

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
- **Live updates:** one SSE stream. It opens with a `hello`, at the client's `Last-Event-ID` or at the newest event, so a client always has an event to come back from. Each entry is followed by its session, with the new `updatedAt` and usage. A session's live state carries the messages that wait for a run.
- **Version:** the client sends the protocol version, and the service checks it. The version is 2: a client and a service of different versions refuse each other, so neither keeps code for the other's.
- **Request ids:** every state-changing request carries one. The service applies each request id once and records its author.
- **Authentication:** one owner, with a token per device (`pocketd device add NAME`).

### Service

`pocketd` runs agents on PiG's `durable/harness`.

**Built (Phase 1):**
- **`service/pig`:** the only package that imports PiG; golangci-lint's depguard rule refuses an import anywhere else. It opens the harness over `durable.sqlite`, registers PiG's coding tools (read, write, edit, bash) and the approval hook, follows each session with a conversation watch, and converts entries and live state to `proto` types through Pi's JSON wire form. It gives each user entry the submission that placed it, read from the durable store's submission records, and each model the thinking levels PiG reads from its catalog entry. A session's live state lists its queued messages from the conversation's `pi.inbox`. A watch update it fails to read is read again after a second, not at the next commit, and so is a session event `service/internal/api` fails to read.
- **`service/internal/store`:** the work database, `work.sqlite`, with devices, sessions, request ids and approvals, versioned by append-only migrations.
- **`service/internal/approvals`:** the approval rule (`NeedsApproval`: bash, write and edit), and the manager that records an approval, waits for the decision and wakes the call.
- **`service/internal/events`:** the event hub. Its numbers grow across restarts, it keeps 4,096 events for reconnecting clients, and it sends a resync when a client missed more. It gives each stream the seq of its `hello`, and a client whose last event came just before a lost one gets a resync.
- **`service/internal/api`:** the HTTP server of the protocol.
- **`service/internal/auth`:** device tokens. Only their SHA-256 is stored.
- **`cmd/pocketd`:** the commands `serve`, `device add|list|revoke`, `login PROVIDER`, `models` and `version`.

**Intended:**

- **Durable store:** holds conversations, tasks, submissions and the usage ledger.
- **Work database:** a separate SQLite file holding threads, plans, plan tasks, asks, pull requests, review verdicts, lessons, rounds and run records.
- **`service/pig/`:** wraps the harness, PiG's `env`, `ai` and `mcp` packages, and the sign-ins.

**Agents and profiles:**
- **Profile fields:** `model`, `role` (supervisor, clerk or agent), `repositories`, `workspace`, `home`, `mcp`, `tools` and `private`. The body of the profile is the agent's identity prompt.
- **Applying a profile:** maps onto the harness's `AgentChange` (model, tools, instructions, working directory, turn limit).
- **MCP and skills:** the service connects these to durable conversations itself.

**Delegation and consults:**
- The supervisor delegates a task to a specialist in a thread.
- A consult asks another agent a question and records its finding.
- Each piece of work reports back to whoever assigned it.

**Rounds:**
- An objective, a completion condition, a round limit, a deadline, a spend limit and a stop.
- A round that fails the same check three times ends the job.
- The job pauses when the owner writes in the session.

**Approvals (built):**
- `BeforeTool` blocks a call that needs the owner, and the request appears in the inbox.
- The answer is stored in the work database and pinned in the task's Durable memo, so a hook that runs again after a restart reads the stored answer.

**Lessons:**
- An agent proposes a lesson, and the owner approves exactly the text they were shown.
- Limits per agent: 300 characters per lesson, 40 approved lessons, and 20 pending per writer.

**External events:**
- Signed GitHub webhooks and NATS events become submissions with request ids, delivered to the responsible agent.
- Outside text is fenced and defanged before an agent sees it.

**Model router:**
- Model choice for each delegation and each round goes through one router seam.
- It records the choice, the features of the brief and the outcome.
- It picks only models the owner allows.
- The first rule is the profile's model. How the router decides beyond that is an open question in the [roadmap](../plans/roadmap.md#open-questions).

**Run records:**
- the model and thinking level
- the profile or brief version
- tokens and cost
- review rounds and verdicts
- the outcome

### Policies enforced in code

Every rule below is service code with tests; prompts only inform agents of them. A model's output that feeds a decision is structured output that code validates, and code decides.

- **Cross-review:** rounds with structured verdicts (`ready`, `not-ready`, `unclear`), from a reviewer of another model family.
- **The plan gate:** a plan is settled when no ask, comment or consult is open, and the owner approves an exact revision.
- **Approve or merge:** the shared bot account never does both on one pull request.
- **`ready-for-human`:** set only after cross-review and green CI, and cleared when the pull request's head moves.
- **Leases:** on shared test resources.
- **Private agents:** they work only in sessions the owner starts in their home. They take no delegation and report to nobody.
- **Per-profile limits:** on MCP servers and tools.

### Desktop client

The desktop client is built on MyGo and frostui, in the `desktop` module. It talks to `pocketd` only through `proto` ([specs/protocol.md](../specs/protocol.md)).

**Built (Phase 1):**
- **`desktop/internal/state`:** the data layer and state, with no UI import (a depguard rule in `.golangci.yml` refuses MyGo and frostui there), so a phone client can reuse it ([ADR-0010](../adr/0010-port-the-desktop-client-selectively-from-frostyard-pocket.md)). Every rule in it is code with a test ([ADR-0011](../adr/0011-put-rules-in-code-not-in-model-judgment.md)):
  - **`Store`:** one sign-in. Each request runs on a goroutine and posts its result to the UI thread. Answers of an earlier sign-in are dropped, and a 401 signs out.
  - **`Link`:** the event stream. When it drops, it waits as `Backoff` says (1 s, doubling to 30 s, on an injected clock) and reconnects with `Last-Event-ID` set to the last event applied. The store fetches the sessions, the pending approvals and the open session when a stream opens with no event to come back from, and on `resync`. A stream that comes back from an event, the `hello`'s included, misses nothing, so the store fetches again only what failed to load. No fetch waits for the `hello`, which an older `pocketd` never sends.
  - **`Data`:** what the client knows. An event that arrives while a fetch is in flight wins over that fetch's copy. The answer of a fetch begun before one already applied is dropped, per list and per session summary, which both the list and a session's detail carry. A session moves up the list with the session event that follows each of its entries. The messages a session holds for a run show queued at the end of its transcript, from its live state. Each list records whether its last fetch failed, so the Inbox never says that nothing waits when it does not know. A session with a pending approval shows as waiting.
  - **`BuildRows`:** the transcript as rows. Each tool result shows with its own call occurrence, since a model may reuse a call id, followed by the reply being written, the tools at work and the calls that wait for an approval. Only the last occurrence of an id takes an approval or a live state, and a busy run queues only the calls of the current round.
  - **Request ids:** a failed send, abort, decision or new session tried again unchanged keeps its request id, so the service applies it once. A failed abort keeps its id only if its session was not seen idle since the abort was sent: the next run's abort is a new request.
  - **`NeedsHTTPWarning`:** the sign-in asks before it sends the token over plain HTTP, unless the host is loopback or a MagicDNS name (`*.ts.net`), which resolves only inside the tailnet. An address in `100.64.0.0/10` still warns: it is shared address space, not proof of a tunnel. Host names are never resolved.
  - **`SameOrigin`:** every request carries the token, so the client follows a redirect only within the service's scheme, host and port, and with the same method. `DirectTransport` sends it without a proxy.
- **`desktop/internal/app`:** the window, on frostui's `PaneShell`:
  - the sign-in screen;
  - a sidebar with the Inbox and the sessions with their state;
  - the Inbox: every pending approval, with Allow, and Deny with an optional reason. A card previews the call's arguments, and Show All and Copy All give every byte of them before Allow. When the approvals failed to load, the Inbox says so and offers Try Again, instead of saying that nothing waits;
  - the session view: Markdown text, folded thinking and tool calls, the streaming reply, the owner's queued messages, the run's failure, approval cards docked above the composer, Send and Abort;
  - the New Session dialog: working directory, model, title and thinking level. It asks pocketd for its models each time it opens, offers Reload Models when there are none, and offers only the thinking levels the chosen model takes.
  - Headless tests drive it against `desktop/internal/pocketdtest`, a fake `pocketd` served in memory, and `make snapshot` writes its views as PNGs.
- **`desktop/internal/config`:** settings in `pocketgo/settings.json` under the user's config folder. The device token goes in the desktop keyring, or in a file only the user can read where there is no keyring; its name carries a SHA-256 of the service's origin.
- **`desktop/cmd/pocket`:** the program. Its app ID is `org.frostyard.PocketGo` and its title PocketGo, so it runs beside frostyard/pocket. `make run` starts it, and `make app` packages it with `go tool mygo build`.

**Intended:**

**Places:**
- **Inbox:** asks, approval requests, lessons, and pull requests ready to merge.
- **Work board:** threads grouped by who moves them next.
- **Session detail:** transcript, composer, attachments, diffs and files.
- **The team.**
- **Run records and metrics.**

**Approve and merge** runs from the desktop as the owner, through a GitHub App user token, behind fail-closed merge gates.

**Data layer:** the client's data layer and state are built on `proto`, kept apart from desktop-only views (tray, terminal, native sign-in flows). A phone client on MyGo's iOS target can reuse them.

### Execution adapters

Both adapters implement PiG's `durableenv.ExecutionEnv`: files, exec and watch, with terminals separate.

- **Local:** a git worktree on the service host. It is not a sandbox.
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
  2. bind the port
  3. open the work database and the harness, with no task running yet
  4. wire the API, deny pending approvals of ended tasks, and watch every session
  5. resume tasks
- **Stop order:** on SIGTERM, end the event streams, shut down HTTP (10 seconds), then close the harness (20 seconds). Running tasks resume at the next start.
- **Agents and the sign-ins:** `service/pig`'s guarded environment wraps PiG's local one.
  - Every file access checks the path PiG will open, after normalization and symbolic links, and refuses the data folder.
  - Every command gets an allowlisted environment, never pocketd's.
  - Model credentials come only from the stored sign-ins; environment and file credential sources are disabled.
  - `bash` can still read the data folder as the service user, but every bash call waits for the owner. The local adapter is not a sandbox; the Coder adapter is the isolation (Phase 4).
- **Coexisting with pi-pocket:** while both run on the VM, they never share a data folder or a store.
- **Back up separately, never in this repository:**
  - the Durable store
  - the work database
  - provider sign-ins
  - the Coder service-account token
  - the GitHub App token
- **The Coder adapter** needs the `coder` CLI on the VM, signed in as the service account.

## References

- Rationale: [ADR-0004](../adr/0004-build-the-service-on-pigs-durable-harness.md), [ADR-0005](../adr/0005-keep-the-agent-loop-and-model-credentials-in-the-service.md), [ADR-0006](../adr/0006-share-the-wire-protocol-as-go-types-in-one-module.md), [ADR-0007](../adr/0007-split-the-repository-into-three-go-modules.md), [ADR-0008](../adr/0008-keep-work-records-in-the-services-own-tables.md), [ADR-0009](../adr/0009-run-agents-in-coder-workspaces-through-pi-env-and-the-rest-api.md), [ADR-0010](../adr/0010-port-the-desktop-client-selectively-from-frostyard-pocket.md), [ADR-0011](../adr/0011-put-rules-in-code-not-in-model-judgment.md)
- Contracts: [specs/protocol.md](../specs/protocol.md)
- Built in: [roadmap — Phases 1 to 5](../plans/roadmap.md)
