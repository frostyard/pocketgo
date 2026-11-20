# 0028 — Let the owner change a session's model from the composer

- **Status:** Accepted
- **Date:** 2026-11-20

## Context

**The owner picks a model only when a session starts.** pocketgo's New Session dialog names a model and a thinking level, and so does a profile through the router ([ADR-0013](0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md)). After that, no request changes them: the protocol has no call for it.

**frostyard/pocket has a model selector the owner likes.** Its composer holds a chip that shows the session's model and its thinking level. The chip opens a menu with one submenu per provider, listing that provider's models with the current one checked. When the model has more than one level, a Thinking submenu follows. A choice changes the session at once. The owner asked on 2026-11-20 to "grab the whole thing" ([ADR-0010](0010-port-the-desktop-client-selectively-from-frostyard-pocket.md)).

**PiG can change a running conversation.** `Conversation.Configure` applies an `AgentChange` in one commit, with `Model` and `ThinkingLevel` among its fields. A change made during a run applies when the current round ends: the model's call and the tools it asked for. Every agent field works this way.

**A standing session takes its profile again and again.** Before each of the owner's messages, and at the end of each run, a standing session takes its profile's identity, tools and model ([ADR-0026](0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md)). Without a rule, a model the owner picked for Odrade would be undone at once. The owner chose on 2026-11-20 that their pick holds until they pick again or start over.

**Some sessions take no message from a client.** A thread's worker ([ADR-0021](0021-give-a-threads-specialist-a-worker-on-an-api-key-model-in-code.md)) and a cross-review ([ADR-0019](0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md)) have no composer. A review's model must stay of another family than the author's.

## Decision

1. **`POST /v1/sessions/{id}/model` changes a session's model, its thinking level, or both** (protocol version 17, rule 7e).
   - The model is checked as a New Session's is: `router.Choose` with the owner's model, which must be one `GET /v1/models` lists.
   - The thinking level is one the model takes. Without a level, a new model keeps the session's level when it takes it, else runs at its default.
   - A worker's session or a cross-review is `conflict`, as a message to it is.
   - pocketd applies the change with `Configure` (`Engine.SetModel`). It records the model, the level and the route `owner` (when the model changed) before it answers, and publishes the session.
   - The request's id makes a retry answer the session as it is, without changing it again.
2. **In a standing session, the owner's pick holds until Start Over.** pocketd keeps the pick, model and level, with the standing session (work database migration 14, `standing_picks`).
   - Each time the session takes its profile, it takes the pick's model in place of the profile's, so the pick holds through compactions and profile edits.
   - Start Over drops the pick in the step that applies the profile, so the session goes back to the profile's model.
   - A model whose context window cannot hold the profile's `compactAt` is refused (`bad_request`), since the profile could no longer be applied.
3. **The desktop ports pocket's chip whole** into the composer's row, before the send button. It shows the model's name and any level other than off, and offers each provider's models and the model's levels. A session with no composer has no chip. A change that failed says why above the composer.

## Consequences

- The owner can move a session to a cheaper or a stronger model without starting a new one. Its context carries over, and the next model reads it as it is.
- A session's `route` tells whether the owner changed its model: `owner` from then on.
- A specialist the owner moves to another family gets its next cross-review from the reviewer of that family: the review reads the session's model when it starts.
- A pick in a standing session outlives every profile edit until Start Over. The chip always shows the model that runs, so a forgotten pick is in sight.
- A change during a run lands between rounds, so one run can use two models. The transcript names each entry's model.
- Another table in the work database, kept with `durable.sqlite` like the rest.

## Alternatives considered

- **A model per message:** pocket changes the session, and so does PiG's `Configure`. A model per message would need a change around every run.
- **The pick lasts until the profile's model changes:** the owner chose to hold it until Start Over.
- **No chip on a standing session:** the owner wants Odrade's model to be one click away too.
- **Refuse a change during a run:** pocket allows it, and PiG applies it between rounds safely.

## References

- Shapes: [specs/protocol.md](../specs/protocol.md) (rules 7c, 7d, 7e and 23), [design/architecture.md](../design/architecture.md)
- Builds on: [ADR-0010](0010-port-the-desktop-client-selectively-from-frostyard-pocket.md), [ADR-0013](0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md), [ADR-0019](0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md), [ADR-0021](0021-give-a-threads-specialist-a-worker-on-an-api-key-model-in-code.md), [ADR-0026](0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md)
