# 0007 — Split the repository into three Go modules

- **Status:** Accepted
- **Date:** 2026-11-18

## Context

**PiG's root module is large.**
- Its release budget allows 185 MB uncompressed.
- Its `go.mod` requires lint and analysis tools.
- It sets Go 1.26 with toolchain 1.27.4.

**MyGo is a large UI framework.** It loads native libraries at run time: GTK 3, WebKitGTK and libghostty-vt on Linux.

**Neither side needs the other's dependencies.** The desktop client doesn't use PiG, and the service doesn't use MyGo.

**Both build without CGO,** except MyGo's iOS and Android targets, which need cgo.

The system's code lives in one repository.

## Decision

The repository holds three Go modules:

| Module | Holds | Depends on |
| --- | --- | --- |
| `proto/` | wire types and the Go client ([ADR-0006](0006-share-the-wire-protocol-as-go-types-in-one-module.md)) | the standard library |
| `service/` | `cmd/pocketd`, the PiG adapter in `pig/`, the execution adapters in `exec/` | `proto` and PiG |
| `desktop/` | `cmd/pocket` and the client | `proto`, MyGo and frostui |

- **Workspace:** a `go.work` file at the root joins the modules for local builds and is committed.
- **Commands:** each lives inside its own module.
- **CGO:** builds use `CGO_ENABLED=0`. A phone build is the one exception.

## Consequences

- **Separate dependency graphs:** each module's `go.sum` and module graph carry only that module's dependencies. PiG never enters the desktop's build, and MyGo never enters the service's.
- **CI:** `make ci` builds, vets and tests each module.
- **Dependabot:** gets one `gomod` entry per module once the modules exist.
- **Releases:** releasing a module would need tags with the module's path prefix.
- **Cross-module changes:** they land in one pull request, because the modules are in one repository.

## Alternatives considered

- **One module:** every build and `go.sum` would carry both PiG's and MyGo's dependency graphs.
- **Separate repositories:** this brings back the multi-repository coupling this system removes ([ADR-0004](0004-build-the-service-on-pigs-durable-harness.md)).

## References

- Shapes: [design: system architecture](../design/architecture.md), [plan: roadmap](../plans/roadmap.md)
- Builds on: [ADR-0004](0004-build-the-service-on-pigs-durable-harness.md), [ADR-0006](0006-share-the-wire-protocol-as-go-types-in-one-module.md)
