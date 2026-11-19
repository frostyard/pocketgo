# System architecture

Living document. Rationale: [ADR-0004](../adr/0004-build-the-service-on-pigs-durable-harness.md), [ADR-0005](../adr/0005-keep-the-agent-loop-and-model-credentials-in-the-service.md), [ADR-0006](../adr/0006-share-the-wire-protocol-as-go-types-in-one-module.md), [ADR-0007](../adr/0007-split-the-repository-into-three-go-modules.md), [ADR-0008](../adr/0008-keep-work-records-in-the-services-own-tables.md), [ADR-0009](../adr/0009-run-agents-in-coder-workspaces-through-pi-env-and-the-rest-api.md), [ADR-0010](../adr/0010-port-the-desktop-client-selectively-from-frostyard-pocket.md), [ADR-0011](../adr/0011-put-rules-in-code-not-in-model-judgment.md).
Contracts: [specs/protocol.md](../specs/protocol.md).

**Status:** partly built. The protocol, and the service's sessions, approvals and event stream, are built (Phase 1). Every other section is intent, and moves to description as the [roadmap](../plans/roadmap.md) lands it.

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
go.work
```

All three build with `CGO_ENABLED=0`. A phone build is the one exception.

**`make ci`:**
- keeps the template's checks, which need Node.js 22
- adds `go vet`, `gofmt`, golangci-lint, tests with `CGO_ENABLED=0`, and a cross-platform build, for each module

### Protocol

`proto` holds every wire type and the Go client. The service and every client import it. [specs/protocol.md](../specs/protocol.md) is the contract; `service/internal/api` serves it.

- **Commands:** REST calls with JSON bodies.
- **Live updates:** one SSE stream.
- **Version:** the client sends the protocol version, and the service checks it.
- **Request ids:** every state-changing request carries one. The service applies each request id once and records its author.
- **Authentication:** one owner, with a token per device (`pocketd device add NAME`).

### Service

`pocketd` runs agents on PiG's `durable/harness`.

**Built (Phase 1):**
- **`service/pig`:** the only package that imports PiG; golangci-lint's depguard rule refuses an import anywhere else. It opens the harness over `durable.sqlite`, registers PiG's coding tools (read, write, edit, bash) and the approval hook, follows each session with a conversation watch, and converts entries and live state to `proto` types through Pi's JSON wire form.
- **`service/internal/store`:** the work database, `work.sqlite`, with devices, sessions, request ids and approvals, versioned by append-only migrations.
- **`service/internal/approvals`:** the approval rule (`NeedsApproval`: bash, write and edit), and the manager that records an approval, waits for the decision and wakes the call.
- **`service/internal/events`:** the event hub. Its numbers grow across restarts, it keeps 4,096 events for reconnecting clients, and it sends a resync when a client missed more.
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

The desktop client is built on MyGo and frostui, in the `desktop` module.

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
