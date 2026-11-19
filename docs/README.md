# Documentation

Docs are split by the question they answer:

| Directory | Question | Contents |
| --- | --- | --- |
| [adr/](adr/) | **Why** did we choose this? | Architecture Decision Records — immutable once accepted; superseded, never edited |
| [design/](design/) | **How** does it fit together? | Living documents describing the current architecture |
| [specs/](specs/) | **What exactly** is the contract? | Precise, testable interface definitions |
| [plans/](plans/) | **When/in what order** do we build? | Roadmaps and phase plans; updated as work lands |

## Index

### Decisions (ADRs)

- [0001 — Record architecture decisions](adr/0001-record-architecture-decisions.md)
- [0002 — Agent-portable instruction surface](adr/0002-agent-portable-instruction-surface.md)
- [0003 — Verify gate, risk paths and corrections](adr/0003-verify-gate-risk-paths-and-corrections.md)
- [0004 — Build the service on PiG's durable harness](adr/0004-build-the-service-on-pigs-durable-harness.md)
- [0005 — Keep the agent loop and model credentials in the service](adr/0005-keep-the-agent-loop-and-model-credentials-in-the-service.md)
- [0006 — Share the wire protocol as Go types in one module](adr/0006-share-the-wire-protocol-as-go-types-in-one-module.md)
- [0007 — Split the repository into three Go modules](adr/0007-split-the-repository-into-three-go-modules.md)
- [0008 — Keep work records in the service's own tables](adr/0008-keep-work-records-in-the-services-own-tables.md)
- [0009 — Run agents in Coder workspaces through pi-env and the REST API](adr/0009-run-agents-in-coder-workspaces-through-pi-env-and-the-rest-api.md)
- [0010 — Port the desktop client selectively from frostyard/pocket](adr/0010-port-the-desktop-client-selectively-from-frostyard-pocket.md)
- [0011 — Put rules in code, not in model judgment](adr/0011-put-rules-in-code-not-in-model-judgment.md)
- [0012 — Run tools without approval, in a sandbox that hides pocketd's files](adr/0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md)

### Org-wide decisions

- [Org-wide decisions (frostyard/core ADRs)](org-adrs.md)

### Guides

- [After you create a repository from this template](new-repository.md)

### Design

- [Agent-ready repository](design/agent-ready-repository.md)
- [System architecture](design/architecture.md)

### Specs

- [Protocol, version 1](specs/protocol.md)

### Plans

- [Roadmap](plans/roadmap.md)

## Conventions

- **New docs start from their category's `TEMPLATE.md`** (in each directory).
- New decision → new ADR with the next number; if it reverses an old one, mark
  the old one `Superseded by NNNN` rather than editing it.
- Design docs are updated in place to always reflect reality.
- Specs change only alongside the code that implements them.
- Cross-links between categories are mandatory in both directions — see the
  documentation rules in [AGENTS.md](../AGENTS.md) (CLAUDE.md/GEMINI.md are
  symlinks to it, ADR-0002).
- Adding a doc means adding it to the index above.
