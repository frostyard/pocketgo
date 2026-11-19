<!-- readme-kind: app -->
# pocketgo

A persistent workspace for running a team of AI coding agents across software projects, for an owner who works with a supervisor and named specialists. A headless Go service keeps the work moving while the desktop is closed, and the desktop client shows what needs the owner and what is still in motion.

**Status:** pre-release. `pocketd` runs durable agent sessions with tool approvals over the protocol; the desktop client is next.

**Documentation:** [docs/README.md](docs/README.md)

## What it does

Planned, in the order of the [roadmap](docs/plans/roadmap.md):

- **Agents with profiles.** Each agent has a profile, a working directory, a set of tools and memory it proposes for the owner to approve.
- **Delegation.** The owner works with an agent directly, or asks a supervisor to delegate to a specialist. Agents consult each other and report back.
- **Rounds.** A longer job runs in rounds toward an objective until it meets its completion condition, hits a round limit, deadline or spend limit, or someone stops it.
- **Durable.** The service keeps working while no client is connected, and resumes interrupted runs after a restart.
- **Inbox.** Questions, approval requests, proposed lessons and pull requests ready for review, in one queue.
- **Work board.** Each task is a thread that connects its plan, agents, sessions, decisions, pull requests and final report.
- **Your subscriptions.** Agents run on the owner's OpenAI Codex, Anthropic Claude and GitHub Copilot subscriptions.
- **Where agents run.** A git worktree on the service host, or a Coder workspace.
- **Records.** Model choices, tokens, cost, review rounds and outcomes, to compare ways of working.

## Install

There is no release yet. Build from source with `make build`, which writes `build/pocketd` and `build/pocket`. Then sign in and add a device:

```sh
build/pocketd login anthropic      # or openai-codex, github-copilot
build/pocketd device add laptop    # prints the device's token once
build/pocketd serve -listen 127.0.0.1:8788
```

## How it works

The service, `pocketd`, runs agents on PiG's durable harness, a Go port of Pi Durable. The service and the desktop share one module of wire types: REST for commands, SSE for live updates. The desktop client is built on MyGo and frostui. The full design is in [docs/design/architecture.md](docs/design/architecture.md).

## Develop

```sh
mise install   # the pinned tools
make ci        # what CI runs
```

`make verify` runs the Go gates in each module and the template gate. It needs Go (go.work's go line), plus golangci-lint and Node.js from `mise install`.

## Where to go next

- [AGENTS.md](AGENTS.md): the rules for agents and contributors.
- [docs/README.md](docs/README.md): the decisions, designs, specs and plans.
- [SECURITY.md](SECURITY.md): report a vulnerability privately.
- [LICENSE](LICENSE): MIT.
