# 0074 — Keep the prompt cache warm between a run's requests, in code

- **Status:** Accepted
- **Date:** 2026-11-24

## Context

**The roadmap promises it.** Its "What carries over" names, among pi-pocket's behaviours to take: "Keeping the prompt cache warm". pocketd keeps no cache warm and sets no cache retention.

**A prompt cache entry expires.** In PiG v0.7.0's catalog, Anthropic's Claude models name a cache lifetime (`Model.PromptCache`): 300 seconds for the short retention and 3,600 for the long one, through Anthropic's own provider or PiG's `radius`. PiG's providers use the short retention unless a request asks for another, or `PI_CACHE_RETENTION=long` is set. A request that finds its prompt cached pays a cache read of it, a tenth of the input price on Claude Sonnet 4.6; one that finds it gone writes it again, at 1.25 times the input price on the short tier and twice on the long one. The OpenAI Codex, GitHub Copilot and DeepSeek models name no lifetime.

**A tool call can outlast the entry.** A run sends a model request, runs the tools its answer calls, and sends the next request with their results. A test suite, a build, a worker or a team tool that waits can take longer than five minutes, and the next request then writes the whole prompt again. The standing supervisor ([ADR-0026](0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md)) holds the longest context, so it pays most.

**pi-pocket's keeper** (`src/server/cache-keeper.ts`) sends the run's last request again with a one-token output cap just before its entry expires, only between the requests of one run, after an answer that called a tool, and only when the expected saving is more than the warm's cost. A new request of the session, the run's end or stop, an error, shutdown, or 30 minutes after the request that started it stops it. In its owner's history a warm during idle time cost about what it saved. PiG's CLI has a warmer of its own (`internal/codingagent/cache_warmer.go`, which no other module can import); PiG's durable harness has none.

**What `service/pig` can see:**
- PiG's `ai.Models` calls a provider's stream functions (`ai.ModelsProvider.StreamSimple` and `Stream`) once it resolved the request's credentials. pocketd builds the providers at start and may replace one (`Models.SetProvider`), as it does to add the owner's models ([ADR-0023](0023-add-the-owners-own-model-providers-from-a-file-checked-in-code.md)).
- The harness sends a conversation's requests under one provider session id (`StreamOptions.SessionID`, the conversation's `pi.provider` document). Each carries an `OnPayload` hook and a context bound to the generation task that sent it, which end with the task.
- A provider calls `OnPayload` with its final payload before it sends it, and sends nothing when the hook fails.
- The harness's `AfterRun` hook runs after a run's answer alone, never after an abort or a failure. Every view of a conversation carries its live state (`pi.live`, with its run) and its provider session id, and pocketd watches every session from its start or its creation.

## Decision

1. **The keeper wraps every provider's stream functions when the engine opens** (`cacheKeeper.install`): PiG's own providers, with the models the owner adds to them, and the owner's own providers alike. A request with no provider session id, or that uses no cache, passes through untouched. It is code in `service/pig` alone ([ADR-0004](0004-build-the-service-on-pigs-durable-harness.md)), on by default. `pocketd serve -cache-keeper=false` turns it off, and then nothing is wrapped. No environment variable switches it.
2. **It warms only between the requests of one run.** It follows a real request's result on a goroutine of its own. Only an answer that ended in a tool call (`toolUse`) starts a warm, keyed by the provider session id. A finished answer, an error, an abort, a compaction's request (retention `none`) and an idle session start none.
3. **Its rules are pure functions, each with its test** ([ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md)):
   - `cacheLifetime`: the model's lifetime for the retention the request used, resolved as PiG's providers resolve it. Retention `none`, or no lifetime named, means no warm.
   - `refreshDelay`: 90% of the lifetime, and never closer than 10 seconds to its end. A first refresh at or after the age cap (`maxWarmAge`, 30 minutes), as the long tier's would be, never starts.
   - `decide`: warm when `toolHazard` (0.9) × missCost > warmCost, from the model's prices per million tokens. warmCost is a cache read of the whole prompt (the real request's input, cache-read and cache-write tokens) plus one output token. missCost is a cache write of it in place of a read, at the input price when the model names no write price. A model priced at nothing is never warmed.
   - `isReplayable`: never warm when a one-token cap would change the cache key, as with a thinking level on Anthropic Messages, whose budget follows the cap, unless the model forces adaptive thinking.
4. **The warm is the same request with a one-token cap.**
   - It goes to the provider's own stream function, below `ai.Models`, with the real request's model, credentials, headers and transcript, `MaxTokens` 1, no retry, and a context of the keeper's own. It carries no telemetry span, payload hook or response hook of the generation task.
   - Its own `OnPayload` hashes the final payload with the output-cap fields left out (`payloadHash`). When the hash differs from the real request's, the hook fails, so the warm goes unsent and the keeper stops for that session. The real request's hash is taken by wrapping its `OnPayload`, after the harness's own hook ran.
   - No transcript records a warm: the harness never sees it.
5. **Its stops:**
   - the session's next real request that uses the cache
   - the run's end or abort (decision 6)
   - a warm that errs, is aborted or would differ
   - a refresh that fires late, more than half the margin past its due time, as after the host slept
   - a refresh no longer worth its cost (`decide`)
   - the age cap, 30 minutes from the real request that started it, never from a warm; a warm in flight at the cap is aborted
   - `Engine.Close`, which stops the keeper first and waits for the warms in flight.
6. **The run's end is found by the engine's watch.** When a watched session's live state goes from busy to idle, the watch tells the keeper the provider session id the view carries (`cacheKeeper.runEnded`). A run that ends unseen (a session not watched, or views that skip the idle moment) leaves its warm to the session's next request or the age cap.
7. **The log names each warm's model, its tokens and its cost at the catalog's prices,** and what a session's warms came to when they stop. It holds no prompt text, key or header, and no provider's error text. A warm's usage reaches no transcript.

## Consequences

- A tool call longer than the cache's lifetime no longer makes the next request write the whole prompt again, on every model whose catalog entry names a lifetime: today the Claude models, by API key or by subscription, and the ones the owner adds like them.
- Warms cost money on an API key and usage on a subscription. pocketd's usage views leave them out, since no transcript holds them ([ADR-0052](0052-show-each-sessions-context-fill-and-usage-and-sum-the-days-usage-in-code.md)): only the log shows them.
- A warm runs on the real request's credentials. A subscription token that expired since fails the warm, which stops the keeper for that session; it never fails a real request.
- Every real request the keeper may warm after is encoded to JSON once more, for its hash, before it is sent.
- A harness hook that replaces a provider's payload (`BeforeProviderRequest`), which pocketd has none of today, would make every warm differ: none would go out, rather than a warm that writes a new entry at full price.
- A run's end the watch does not see costs warms until the next request or the age cap.

## Alternatives considered

- **Long retention for standing sessions:** an hour's entry outlasts most tool waits with no warm, but every write of the long tier costs twice the input price, on every request of the session. PiG's default retention stays, and the keeper never warms a long entry.
- **Warm idle sessions too:** in pi-pocket's history an idle warm cost about what it saved. PiG's CLI counts a 15% chance of a request while idle, at which a warm rarely pays.
- **Warm through `ai.Models` again:** it would resolve the credentials afresh, but the keeper's own wrapper would take the warm for a real request, and an OAuth refresh would write the sign-ins for a warm.
- **The harness's `AfterRun` hook for the run's end:** it does not run after an abort or a failure.
- **A replay with no hash:** a payload hook, or a provider whose payload depends on the output cap, would make the warm write a new entry at full price.

## References

- Shapes: [design/architecture.md](../design/architecture.md#service), [design/setup.md](../design/setup.md#2-build-and-deploy-pocketd), [plans/roadmap.md](../plans/roadmap.md#phase-2--team-work-board-and-inbox)
- Builds on: [ADR-0004](0004-build-the-service-on-pigs-durable-harness.md), [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0023](0023-add-the-owners-own-model-providers-from-a-file-checked-in-code.md), [ADR-0026](0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md), [ADR-0052](0052-show-each-sessions-context-fill-and-usage-and-sum-the-days-usage-in-code.md)
