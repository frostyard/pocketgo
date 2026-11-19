# 0006 — Share the wire protocol as Go types in one module

- **Status:** Accepted
- **Date:** 2026-11-18

## Context

frostyard/pocket copies pi-pocket's JSON by hand.
- **Size:** `internal/pocket` holds 122 type declarations and 88 client methods.
- **Coupling:** 41 of the 53 files in `internal/app` import it directly.
- **Drift:** decoding is tolerant and logs fields in unexpected shapes, because the server changes underneath it.

pi-pocket's protocol has no written specification. It is at protocol version 6 and changes with weekly releases.

Both programs use REST for commands and Server-Sent Events, or long polling, for live updates. Neither uses WebSocket.

pi-pocket puts a request id on each submission, so a retried request does nothing twice and records whom the work was for.

## Decision

The wire types and the Go client live in the `proto` module, which the service and every client import. No client declares its own copy of a wire type.

- **Transport:** commands are REST calls with JSON bodies, and live updates are one SSE stream.
- **Versioning:** the protocol has a version number. The client sends it and the service checks it.
- **Request ids:** every request that changes state carries a request id. The service applies each one once and records its author.

## Consequences

- **One change:** a protocol change is a change to `proto`, compiled against the service and the clients in the same pull request.
- **Risk path:** `proto/` is a risk path at L2.
- **Written down:** the protocol gets a spec under `docs/specs/` with its first endpoint, and the spec changes alongside the code.
- **Other languages:** a non-Go client needs its own types, generated or hand-written from the spec.

## Alternatives considered

- **Hand-written types in each client:** this is the current coupling in frostyard/pocket.
- **WebSocket:** both existing programs already work over REST and SSE, and one owner's traffic needs nothing more.
- **gRPC or protobuf:** this adds a code generator and its toolchain to a pure-Go repository.

## References

- Shapes: [design: system architecture](../design/architecture.md), [plan: roadmap](../plans/roadmap.md)
- Builds on: [ADR-0004](0004-build-the-service-on-pigs-durable-harness.md)
- Spec: [specs/protocol.md](../specs/protocol.md)
