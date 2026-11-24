# 0068 — Build a shared NSGo client beside the desktop client

- **Status:** Accepted
- **Date:** 2026-11-23

## Context

PocketGo's owner needs the same workflows in a native window and a browser. The desktop already separates the service protocol and its state transitions from its views.

## Decision

Build a second entry point in the desktop module using NSGo. Both native and browser builds share the view tree. Compose the conversation from NSGo chat controls. Keep reusable UI in an importable package that depends on NSGo and accepts application content and callbacks. PocketGo owns its state, service adapters and commands.

## Consequences

The existing client provides a reference for parity. Changes to either platform need target testing of input, navigation and host integrations. An independent package can later move to another module without bringing PocketGo's protocol with it.

The package now has its own Go module at `github.com/frostyard/pocketgo/desktop/workspaceui`. Its tags start with `desktop/workspaceui/v`. Other apps can depend on the controls without depending on PocketGo or its protocol. The desktop builds against the same source in its checkout.

## Alternatives considered

- Two independently maintained frontends would require implementing every interaction twice.

## References

- [System architecture](../design/architecture.md)
- [ADR-0010](0010-port-the-desktop-client-selectively-from-frostyard-pocket.md)
- [Shared owner merge service proposal](../plans/shared-merge-service.md)
