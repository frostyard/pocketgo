<!-- readme-kind: app -->
# pocketgo

A persistent workspace for running a team of AI coding agents across software projects, for an owner who works with a supervisor and named specialists. A headless Go service keeps the work moving while the desktop is closed, and the desktop client shows what needs the owner and what is still in motion.

**Status:** pre-release. `pocketd` runs a team of agents in durable sessions, every command in a bubblewrap sandbox. The desktop client shows the Inbox, the work board and each session, and merges pull requests as the owner.

**Documentation:** [docs/README.md](docs/README.md)

## What it does

Built (the [roadmap](docs/plans/roadmap.md) records each part once it is seen working end to end):

- **Agents with profiles.** Each agent has a profile, a working directory, a set of tools, and lessons it proposes for the owner to approve.
- **Delegation.** The owner works with an agent directly, or asks a supervisor to delegate to a specialist in a thread. Agents consult each other, give work to a worker on a cheaper model, and report back.
- **Rounds.** A longer job runs as a shift, in rounds toward an objective, until it meets its completion condition, hits its round limit or deadline, or someone stops it.
- **Durable.** The service keeps working while no client is connected, and resumes interrupted runs after a restart.
- **Inbox.** Asks, questions, proposed lessons and pull requests ready for review, in one queue, with approve and merge behind merge gates.
- **Work board.** Each task is a thread that connects its plan, agents, sessions, pull requests and final report.
- **Your subscriptions.** Agents run on the owner's OpenAI Codex, Anthropic Claude and GitHub Copilot subscriptions, and on API keys and the owner's own model providers.
- **Where agents run.** A git worktree on the service host.

Planned:

- **Coder workspaces** as a second place where agents run.
- **Events.** GitHub webhooks and other events wake the responsible agent.
- **Records.** Run records and metrics views, review rounds and outcomes beside the model choices, tokens and cost recorded today, to compare ways of working.
- **A spend limit** for a shift.

## Install

Build a [Linux Flatpak and web release candidate](packaging/README.md) from the same commit with `make rc`. Build from source with `make build`, which writes `build/pocketd` and `build/pocket`. Then sign in and add a device:

```sh
build/pocketd login anthropic      # or openai-codex, github-copilot
build/pocketd device add laptop    # prints the device's token once
build/pocketd serve -listen 127.0.0.1:8788
```

Start the desktop client with `build/pocket` (or `make run`), and sign in with the service URL and the device's token. Linux uses NSGo and GTK 4. `make app` packages the native client. `make build-mygo`, `make run-mygo` and `make app-mygo` select the MyGo fallback.

The [NSGo port](desktop/cmd/pocket-nsgo/README.md) builds a GTK 4 desktop client and a web/WASM client from the same Go UI. Its preview runs both against disposable data.

To run pocketgo on a host of your own, follow the [setup guide](docs/design/setup.md): the service host and its sandbox, deploying, the tailnet, model sign-ins, devices, the profile folder, GitHub, the desktop, and a first delegated thread.

## How it works

The service, `pocketd`, runs agents on PiG's durable harness, a Go port of Pi Durable. The service and the desktop share one module of wire types: REST for commands, SSE for live updates. Linux and web use the shared NSGo interface. The MyGo client remains available on Linux and serves the other native platforms. The full design is in [docs/design/architecture.md](docs/design/architecture.md).

## Develop

```sh
mise install   # the pinned tools
make ci        # what CI runs
```

`make verify` runs the Go gates in each module and the template gate. It needs Go (go.work's go line), plus golangci-lint and Node.js from `mise install`.

## Where to go next

- [docs/design/setup.md](docs/design/setup.md): set pocketgo up on your own host.
- [AGENTS.md](AGENTS.md): the rules for agents and contributors.
- [docs/README.md](docs/README.md): the decisions, designs, specs and plans.
- [SECURITY.md](SECURITY.md): report a vulnerability privately.
- [LICENSE](LICENSE): MIT.
