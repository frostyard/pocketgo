# 0004 — Build the service on PiG's durable harness

- **Status:** Accepted
- **Date:** 2026-11-18

## Context

The owner runs a team of agents across four repositories:
- **pi-pocket:** a Node server built on Pi Durable (`@earendil-works/pi-durable`).
- **pi-pocket-team:** drop-in modules that add the team, the Desk, shifts and a model router to pi-pocket.
- **pi-pocket-config:** the agent profiles and rules.
- **frostyard/pocket:** a Go desktop client.

The drop-in modules depend on pi-pocket in several fragile ways:
- They read pi-pocket documents marked "not public API".
- They parse its version string, including fork tags.
- They link to a sibling checkout.
- They find each other through `globalThis`.

About 45k of pi-pocket's 111k lines of server TypeScript are features this system doesn't use.

The service must keep work running while no client is connected, and resume it after a restart.

[PiG](https://github.com/MichaelKinsy/PiG) is an MIT-licensed Go port of Pi, pinned to Pi 1.5.0.
- **Pi Durable port:** its `durable/` packages port pi-durable. Its parity report lists 87.4% of pi-durable's files as ported, and 98.7% of those as behaviour-verified.
- **Storage:** pure-Go SQLite (`modernc.org/sqlite`), with no CGO.
- **Store compatibility:** `durable/interop` tests that Go and Node pi-durable open and resume each other's stores.
- **Two agent stacks:**
  - the `coding` SDK, which the `pig` binary uses: extensions, MCP, skills, sessions in JSONL files, and no task recovery
  - `durable/harness`: durable, with built-in tools read, write, edit and bash
- **Two durable engines:**
  - `harness.OpenHarness`, the port
  - `durable/core/driver`, an engine for Cloudflare Durable Objects; PiG's ADR-0001 for it is Proposed
- **Missing pieces:** PiG has no HTTP server, authentication or multi-client layer.
- **Stability:** PiG's SDK documentation says "Do not assume API stability before the first public release". Its changelog lists 14 breaking Go API changes, and one maintainer wrote most of its commits.

## Decision

The service, `pocketd`, runs agents on PiG's `durable/harness`, opened with `harness.OpenHarness` over `durable/storage/sqlite/node`.

- **Packages:** it also uses PiG's `env`, `ai` and `mcp` packages. It uses neither the `coding` SDK nor `durable/core/driver`.
- **Pinning:** the service module pins PiG to an exact commit.
- **One import point:** only `service/pig/` imports PiG; the rest of the service uses that package's types.
- **Service code:**
  - connecting MCP servers and skills to durable conversations
  - tools beyond read, write, edit and bash
  - the HTTP server, authentication and client streaming
- **Approvals:** a tool approval that waits for the owner is stored, and a `BeforeTool` hook that runs again after a restart reads the stored answer.

## Consequences

- **From PiG:** restart recovery, idempotent submissions, sleeping tasks, tool hooks and a per-conversation usage ledger.
- **Upgrades:** a PiG upgrade changes one pin, and API renames land in `service/pig/`.
- **Service code to write:** MCP, skills and further tools, which `pig` gets from its `coding` SDK.
- **Provider layer:** it is used unforked, so the subscription sign-ins of [ADR-0005](0005-keep-the-agent-loop-and-model-credentials-in-the-service.md) receive upstream fixes.
- **Dependency risk:** the service depends on a project with a bus factor near 1. If PiG stops, the service owns a fork of the packages it imports.
- **Existing stores:** a pi-pocket store is readable through the same harness. pi-pocket's own document kinds need an importer.

## Alternatives considered

- **Keep pi-pocket and its drop-in modules:** the modules depend on pi-pocket internals and its weekly releases, and pi-pocket carries features this system doesn't use.
- **PiG's `coding` SDK:** sessions are JSONL files with no task recovery.
- **PiG's `durable/core/driver`:** a Proposed engine for Cloudflare Durable Objects.
- **`pig --mode rpc` subprocesses:** stdio JSONL per process, with no durability.
- **A new agent runtime:** duplicates a ported, parity-tested runtime and its provider sign-ins.

## References

- Shapes: [design: system architecture](../design/architecture.md), [plan: roadmap](../plans/roadmap.md)
- Builds on: [ADR-0001](0001-record-architecture-decisions.md)
- PiG: [README](https://github.com/MichaelKinsy/PiG#readme), [port map](https://github.com/MichaelKinsy/PiG/blob/main/docs/parity/PORT_MAP.md), [parity coverage](https://github.com/MichaelKinsy/PiG/blob/main/test/parity/coverage.md)
