# 0009 — Run agents in Coder workspaces through pi-env and the REST API

- **Status:** Accepted
- **Date:** 2026-11-18

## Context

Agents run commands with the permissions of the user who runs the service, and a git worktree is not a sandbox.

The owner runs a Coder deployment at v2.39.1. Its licence includes service accounts, workspace prebuilds and Boundary, which controls network egress.

PiG's `env` package defines `durableenv.ExecutionEnv`: files, command execution and watches, with terminals separate.
- **Remote implementation:** it speaks to a `pi-env` daemon over the standard input and output of any command, through `env.ConnectionOptions.CommandFunc`.
- **SSH-target route:** PiG's other remote route forces strict host-key checking against PiG's own known-hosts file.

`codersdk` lives in coder/coder, which is licensed AGPL-3.0. This repository is MIT.

## Decision

The service has two execution adapters, each implementing `durableenv.ExecutionEnv`:
- **Local:** a git worktree on the service host.
- **Coder:** a Coder workspace.

The Coder adapter works like this:
- **Connection:** it starts `coder ssh <workspace> -- pi-env serve` through `CommandFunc`. Workspace templates include the `pi-env` binary.
- **Lifecycle:** the service creates, starts and stops workspaces through Coder's REST API, over plain HTTP, authenticated as a Coder service account.
- **No `codersdk`:** the repository doesn't import it.
- **No model credentials:** workspaces never hold one ([ADR-0005](0005-keep-the-agent-loop-and-model-credentials-in-the-service.md)).

## Consequences

- **Choosing where an agent runs** is a setting, not a separate code path.
- **Prebuilds** shorten workspace start time, and **Boundary** restricts what a workspace can reach.
- **Service host:** it needs the `coder` CLI, signed in as the service account.
- **Version match:** templates ship a `pi-env` built from the PiG commit the service pins, so the daemon and the service speak the same protocol version.
- **REST client:** hand-written, and it follows changes to Coder's API.
- **Risk path:** `service/exec/coder/` is a risk path.

## Alternatives considered

- **PiG's SSH-target route through `coder config-ssh`:** PiG's strict host-key checking conflicts with the configuration Coder generates.
- **Importing `codersdk`:** this links AGPL-3.0 code into an MIT repository.
- **Coder Agents:** runs Coder's own agent loop and model path, not the service's.

## References

- Shapes: [design: system architecture](../design/architecture.md), [plan: roadmap](../plans/roadmap.md)
- Builds on: [ADR-0004](0004-build-the-service-on-pigs-durable-harness.md), [ADR-0005](0005-keep-the-agent-loop-and-model-credentials-in-the-service.md)
- Coder: [Coder Agents architecture](https://coder.com/docs/ai-coder/agents/architecture)
