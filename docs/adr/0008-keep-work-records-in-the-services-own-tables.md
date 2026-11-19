# 0008 — Keep work records in the service's own tables

- **Status:** Accepted
- **Date:** 2026-11-18

## Context

pi-pocket stores its state as Pi Durable documents in `pocket.sqlite`, and defines no tables of its own.

pi-pocket-team stores Desk threads, plans, lessons and usage as Durable documents too.
- **Untyped run map:** a thread carries a `run` map with about 40 untyped keys.
- **Reads around the API:** its daily audit opens `pocket.sqlite` directly to count review rounds and outcomes.

The service must record model choices, token usage, cost, review rounds and outcomes, so the owner can compare ways of working.

PiG's durable harness manages its own SQLite store ([ADR-0004](0004-build-the-service-on-pigs-durable-harness.md)).

## Decision

**Durable keeps** conversations, tasks, submissions and the usage ledger.

**The service keeps the work** in its own SQLite database, a separate file in the same data folder. Its tables are typed and versioned by migrations:
- threads
- plans and plan tasks
- asks
- pull requests
- review verdicts
- lessons
- rounds
- run records

A run record names the Durable conversation it describes.

## Consequences

- **Metrics are queries:** metrics and audits are SQL queries over the service's tables, not reads of a store's internal layout.
- **Migrations:** schema changes are migrations.
- **Typed ports:** pi-pocket-team's thread and plan schemas port as tables, and the `run` map becomes typed columns.
- **No shared transactions:** no transaction spans both stores. A write caused by a Durable event is keyed by its request or task id, so writing it again changes nothing.
- **Backups:** cover two files.

## Alternatives considered

- **Durable documents, as pi-pocket-team uses:** they aren't queryable without reading the store's internal layout.
- **A database server such as PostgreSQL:** a second service to run, for one owner.

## References

- Shapes: [design: system architecture](../design/architecture.md), [plan: roadmap](../plans/roadmap.md)
- Builds on: [ADR-0004](0004-build-the-service-on-pigs-durable-harness.md)
