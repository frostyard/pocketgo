# 0025 — Keep the supervisor in one standing session, compacted early in code

- **Status:** Accepted
- **Date:** 2026-11-20

## Context

**The owner talks with one Odrade.** In pi-pocket-team the supervisor kept one session for good, compacted early, and the owner never started a new one. In pocketgo every New Session from Odrade's profile is a new session: what she knew stays behind in the last one, and her threads report to whichever session delegated them (`thread.SupervisorSession`).

**PiG's durable harness compacts on its own.** PiG v0.7.0's `DefaultCompactionPolicy` compacts a context that reaches its model's window less 16,384 tokens, keeping the last 20,000 tokens as they are, and a context overflow compacts too. Its settings are harness-wide (`HarnessOptions.Settings`) or per model (`ModelOverrides`), never per session. `Conversation.Compact` starts a compaction by hand, and `Conversation.Reset` starts a context afresh in the same conversation. A compaction deletes no entry.

**PiG's summary budget is too small for a long session.** PiG writes a summary in at most 80% of the reserve, thinking included: 13,107 tokens by default. Each summary folds in the one before it. pi-pocket found that a long session's summary outgrows the budget and is cut off, and that from then on every run of that session fails. Its fix gives each model a reserve of `max(16384, min(maxTokens / 0.8, 32768, window / 4))`.

**The owner's choices, 2026-11-20:**
- The threshold is per profile: `compactAt`, in tokens, about 300,000 for Odrade on Claude Opus 5.5. Every other session keeps PiG's own threshold.
- New Session on a standing profile opens its session. Start Over, behind a confirmation, gives that session a fresh context in the same session id, so its threads keep reporting there.
- Any profile may keep a standing session; only Odrade's will.
- The transcript shows a compaction as a row with its summary folded. A context meter comes later.
- A change of the profile's model applies to the standing session when it is next idle.
- The Clerk and digests come later. The team VM's data is wiped at deploy, so no older Odrade session is adopted.

## Decision

1. **A profile may keep a standing session.** `session: standing`, with an optional `compactAt`, are profile fields checked in `service/internal/profiles`: `compactAt` is a whole number of tokens from 1 to 10,000,000, legal only with `session: standing`, and a standing profile has a home or a workspace to start in. `profiles.CheckCompactAt` refuses a `compactAt` that is not below the context window of the model the router chooses for the profile ([ADR-0013](0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md)), and an unknown window; such a profile lists a problem.
2. **New Session opens it.** `POST /v1/sessions` with a standing profile names the profile alone. The first one makes the session: in the profile's folder, on the router's model for the profile at its default thinking level, titled with the agent's name. The engine records it under the request's key and the profile's key, `standing:<profile>`, so a retry of that request recovers it whatever the profile says by then. It is recorded as the profile's standing session in the same transaction as its request (`store.CreateStandingSession`; work database migration 13). Every later one answers 200 with it, whatever the profile's model or folder are now. A standing profile whose `compactAt` is past its model's window cannot start, for New Session, a delegation or a consult.
3. **pocketd compacts it early, in code.** When a run of a standing session ends, the engine estimates its context (`harness.EstimateContext`). It starts one compaction when `team.ShouldCompact(estimate, compactAt, window)` holds and PiG finds an older part to summarize (`harness.SelectCut`). The compaction starts in one PiG transaction that finds the session idle, no compaction listed in its live state, and no entry written since the estimate, so a check run again never compacts twice. PiG's own threshold stays the backstop.
4. **Every session's summary has room.** `HarnessOptions.Settings` gives each model of the catalog the reserve pi-pocket found (`summaryReserve`, ported in `service/pig`), as a `ModelOverrides` entry.
5. **It follows its profile when idle.** At each run end, before the owner's message, and when New Session or Start Over opens it, pocketd gives the standing session its profile's identity, roster, tools and model as they are now. The refresh runs whole under `refreshMu`, its reads included, so two never cross. The engine makes the change in one PiG transaction that finds the session with no run and nothing queued (`Engine.Reconfigure`), and keeps the lessons section, which only a delivery pass writes ([ADR-0015](0015-keep-each-agents-lessons-approved-by-the-owner-in-code.md)); the work database then records the model. A busy session keeps what it has until the next idle moment. A profile that cannot be applied changes nothing, a private one whose home is not the session's folder included, and the owner's message is then refused, never run with what the profile said before. pocketd compacts only at a threshold the profile's checks passed.
6. **Start Over resets it in place.** `POST /v1/sessions/{id}/start-over` takes a standing session that has no run and nothing queued. It claims its request for that session first, then writes PiG's reset entry, keyed by the request, in the transaction that finds the session idle. The session id stays, so its threads, asks and reports keep reaching it ([ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md)). The desktop offers it only when pocketd would take it, and asks first.
7. **The protocol shows both.** Protocol version 14 adds a profile's and a session's `standing`, a profile's `compactAt`, a model's `contextWindow`, the entry kinds `compaction` (its summary) and `reset`, and a live state's `compacting` and `compactionFailure` ([spec rules 7c, 7d and 16c](../specs/protocol.md)). The desktop draws a compaction as a folded row, a reset as a note, and a compaction at work as working.

Example frontmatter of a standing supervisor:

```yaml
role: supervisor
model: anthropic/claude-opus-5-5
home: ~/agents/supervisor
session: standing
compactAt: 300000
```

## Consequences

- The owner opens one session with the supervisor, and its threads report to it for good.
- The supervisor's context stays well under the model's window, and a long session's summary has room.
- Every session's reserve grows for models that write long answers: PiG compacts them up to 16,384 tokens earlier than before.
- A standing session's profile changes reach it only while it is idle: a run in progress keeps what it started with.
- A standing session keeps its folder: a profile's new home applies only to a profile's next standing session, and there is none while one exists.
- The owner can no longer choose a folder, model, thinking level or title for a standing profile's session.
- Start Over drops the supervisor's knowledge of its open threads from its context: they stay on the desk, and their next reports tell it again.

## Alternatives considered

- **PiG's own threshold for the supervisor:** PiG's settings cannot be set per session, and a per-model override would compact every session on Opus 5.5 at 300,000 tokens, not the supervisor's alone.
- **A `beforeCompact` hook:** it can decline a compaction or supply its summary, but it cannot start one.
- **Start Over as a new session, by a generation in the key:** the threads report to their supervisor's session id, which would leave them reporting to the old session.
- **A digest the Clerk writes:** the owner chose to build the Clerk and digests later.

## References

- Shapes: [design/architecture.md](../design/architecture.md#service), [specs/protocol.md](../specs/protocol.md), [plans/roadmap.md](../plans/roadmap.md#phase-2--team-work-board-and-inbox)
- Builds on: [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0013](0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md), [ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md), [ADR-0015](0015-keep-each-agents-lessons-approved-by-the-owner-in-code.md), [ADR-0004](0004-build-the-service-on-pigs-durable-harness.md)
