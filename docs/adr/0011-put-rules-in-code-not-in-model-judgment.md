# 0011 — Put rules in code, not in model judgment

- **Status:** Accepted
- **Date:** 2026-11-18

## Context

In the pi-pocket setup, several workflow rules exist only as prompt text in the agents' `AGENTS.md`: cross-review, planning that starts with brainstorming, the plan-ready gate and leases. That file says of cross-review that it is "not a mechanical guard". pi-pocket-team enforces other rules in code: the plan gate, the desk cap, memory approval and the shift stop rules.

A model's compliance with a prompt rule varies from run to run, and a prompt rule has no test. Code returns the same result for the same input, and a test pins it.

## Decision

Any behaviour that can be stated as a rule is code in the service, with tests. That includes:
- policies and gates
- state transitions
- validation
- routing rules
- scheduling, retries and limits
- formatting
- parsing of agent output

A model does only open-ended work: writing and changing code, reviewing, planning and answering.

When a model's output feeds a decision, the model returns structured output against a schema, code validates it, and code makes the decision. A prompt may tell an agent what code enforces, as information; it never stands in for the enforcement.

## Consequences

- Policies are testable and return the same result on every run.
- A new rule costs code and tests, not a prompt edit.
- The prompt-only rules of pi-pocket-config become service features: see the "Policies enforced in code" section of the architecture design.
- The model router decides by rules in code. A model's role in routing is limited to producing structured features that code evaluates.
- Judgments that stay with models, such as review quality, are recorded as structured verdicts that code reads.

## Alternatives considered

- **Rules as prompt text:** compliance varies from run to run, and nothing tests it.
- **A model as the judge of a gate:** this is nondeterministic, and costs tokens on every check.

## References

- Shapes: [design: system architecture](../design/architecture.md), [plan: roadmap](../plans/roadmap.md)
- Builds on: [ADR-0004](0004-build-the-service-on-pigs-durable-harness.md), [ADR-0008](0008-keep-work-records-in-the-services-own-tables.md)
