# 0010 — Port the desktop client selectively from frostyard/pocket

- **Status:** Accepted
- **Date:** 2026-11-18

## Context

frostyard/pocket is a Go desktop client on MyGo v0.10.0 and frostui v0.10.0. It has 29k lines of non-test code and 22k lines of tests.

**Coupled to pi-pocket:**
- `internal/pocket`, 3.5k lines, copies pi-pocket's protocol.
- 41 of the 53 non-test files in `internal/app` import it directly.

**Not coupled to pi-pocket:**
- `internal/github`: approve and merge with fail-closed gates, 4.4k lines plus 6.4k of tests.
- `internal/config`.

**MyGo:**
- one maintainer
- breaking 0.x releases that come with codemods
- targets macOS, Linux, Windows, iOS and Android, and needs cgo only on the mobile targets

**frostui** is the owner's own UI kit, before version 1.

## Decision

The desktop client is built on MyGo and frostui, in the `desktop` module ([ADR-0007](0007-split-the-repository-into-three-go-modules.md)).

**Ported from frostyard/pocket:**
- `internal/github`, with the GitHub App identity as a setting
- `internal/config`
- the frostui shell
- the transcript, composer, attachment and diff views
- notification filtering
- terminal replay
- the test harness

**Not ported:** `internal/pocket`, the Arrakis scene and the wiki.

**Data layer:** the client's data layer is built on `proto` ([ADR-0006](0006-share-the-wire-protocol-as-go-types-in-one-module.md)). It and the client's state are kept apart from desktop-only views (tray, terminal, native sign-in flows), so a phone client can reuse them.

## Consequences

- **Rewrite:** about 4–6k lines of Desk logic are rewritten for the new domain model.
- **Upgrades:** MyGo upgrades are deliberate, and frostui moves with them.
- **Contained risk:** a failure of MyGo costs the clients, not the service or the protocol.
- **Phone builds:** a phone client is possible on MyGo's iOS target, and needs cgo for that build only.

## Alternatives considered

- **Move frostyard/pocket in unchanged:** its views depend on pi-pocket's types.
- **Write a new client:** this discards tested merge gates and views.
- **A web client:** a browser UI is out of scope.

## References

- Shapes: [design: system architecture](../design/architecture.md), [plan: roadmap](../plans/roadmap.md)
- Builds on: [ADR-0006](0006-share-the-wire-protocol-as-go-types-in-one-module.md), [ADR-0007](0007-split-the-repository-into-three-go-modules.md)
