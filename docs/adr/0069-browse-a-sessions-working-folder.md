# 0069 — Browse a session's working folder

- **Status:** Accepted
- **Date:** 2026-11-23

## Context

A session may work in an agent home, a checkout or a separate worktree. The owner needs to read files alongside the conversation on desktop and web.

## Decision

The authenticated file viewer accepts an optional session root identified by its numeric session ID. The service selects the recorded working directory. It permits visible directories below the owner's home, configured profile homes, workspaces and repositories, and the recorded worktree beside a configured checkout. It applies the viewer's private-path exclusions, descriptor-based confinement, filename filters and read budgets. The homes listing keeps its existing meaning.

## Consequences

Existing clients continue to browse homes. A new client can use a home that contains a session folder on older services. A folder outside those homes needs the updated service. A missing working tree produces an unavailable view.

## Alternatives considered

The changes endpoint describes a diff and omits unchanged files.

## References

- [File viewer](0066-let-the-owner-read-the-agents-homes-in-a-read-only-file-viewer.md)
- [Protocol](../specs/protocol.md)
- [Architecture](../design/architecture.md)
