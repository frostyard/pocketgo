# 0023 — Add the owner's own model providers from a file checked in code

- **Status:** Accepted
- **Date:** 2026-11-20

## Context

**The owner runs a local inference server:** [halogen-flash-server](https://github.com/peonist-ai/halogen-flash-server). One port serves the OpenAI chat-completions API (`/v1/chat/completions`), the OpenAI Responses API and the Anthropic Messages API (`/v1/messages`).
- It serves one model, `halogen-qwen3.8-flash-next`, with a context of 262,144 tokens and at most 65,536 output tokens.
- Its key is optional. With none set it takes any request.
- On 2026-11-20 the owner chose that its models power any session, as DeepSeek's do since [ADR-0022](0022-run-any-session-on-any-sign-in-and-choose-workers-from-the-owners-list.md).

**pocketd knows only PiG's built-in providers.** [ADR-0021](0021-give-a-threads-specialist-a-worker-on-an-api-key-model-in-code.md) left "a file of the owner's providers, each a base URL and its models, checked in code" as a later change.

**PiG can take a provider pocketd builds.** PiG v0.7.0's public `ai` package can build one: `ai.CreateProvider`, with models of exported fields and the built-in OpenAI-completions and Anthropic-Messages adapters by API. pocketd registers it beside the built-ins (`ai.Models.SetProvider`). Its base URL goes on each model, and its key comes from the data folder's sign-ins, as DeepSeek's does.

**The owner's network is private.** The server's address must not enter this public repository, so the provider list lives in the owner's folder.

## Decision

1. **The owner's providers are `providers.json` in the agents' profile folder.** Agents' commands see that folder read-only ([ADR-0013](0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md)). The file is `{"providers": [{…}, …]}`, checked in code (`profiles.LoadProviders`). Its keys are read as written: another case, a key named twice, or an unknown key makes the file invalid. It holds 1 to 8 providers with distinct ids, each with:
   - `id`: lowercase letters, digits and dashes, starting with a letter, at most 32 characters. It names no built-in provider of PiG.
   - `name`: optional, at most 60 characters.
   - `api`: `openai-completions` or `anthropic-messages`.
   - `baseUrl`: an absolute `http` or `https` URL with a host, and no user, query or fragment. It is the URL PiG joins its path to: the OpenAI API's `…/v1`, which PiG follows with `/chat/completions`; the Anthropic API's root, which PiG follows with `/v1/messages`.
   - `key`: optional, `false` by default. With `true`, the owner stores the provider's key with `pocketd login <id>`, while pocketd is stopped, and its models are listed only once a key is stored. With `false`, no key is sent.
   - `models`: 1 to 32 models with distinct ids. Each has an `id` with no space, an optional `name`, a `contextWindow` of 1 to 10,000,000 tokens, a `maxTokens` of 1 to the context window, and optional `reasoning` and `images`, `false` by default.
2. **pocketd reads the file when it starts**, and `pocketd models`, `pocketd profiles` and `pocketd login` read it too.
   - Each provider is registered with PiG beside the built-ins, so its models run any session: owner's, profile's, worker's, review's (ADR-0022).
   - A worker list (`workers.json`) may name them.
   - An invalid file registers none of its providers. pocketd still starts, logs why, and the CLI commands print it.
   - A change takes effect at pocketd's next start.
3. **Keys follow ADR-0022's rule.** A stored key counts for a provider of `pig.APIKeyProviders` or for an owner's provider with `key: true`. `pocketd login <id>` refuses a provider with `key: false`.
4. **No protocol change.** The models are listed and chosen like any others.

## Consequences

- The owner runs sessions and workers on their own hardware at no per-token cost, and names it first in `workers.json` with DeepSeek after it.
- pocketd sends requests, with a stored key, only to a URL the owner wrote in a folder agents cannot change.
- **Known limits:**
  - pocketd uses PiG's defaults for an OpenAI-compatible server it does not know: the developer role, `max_completion_tokens`, `reasoning_effort` and `store: false`. Halogen takes all of these. A server that does not would need its compatibility settings in the file, a later change.
  - The router's model family (`router.Family`) knows no maker in `halogen-qwen3.8-flash-next`. A pull request its sessions write gets no cross-review, and the owner decides, as for any model of no known family ([ADR-0019](0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md)).
  - A change to the file needs a restart. A server that is down fails the runs on its models with the server's error. Whether a worker list's next model is tried then is not decided here: the router checks only that a model is listed.
  - The Responses API is not offered: pocketd's adapters for the two APIs above suffice.

## Alternatives considered

- **pi's `models.json` through PiG's coding runtime:** it reads credentials from the environment, which `storedOnly` forbids (ADR-0005), and it loads every agent folder's files.
- **Flags or the service's environment:** the address would sit in the systemd unit, and the list would not be checked with the profiles.
- **Discover the models from `/v1/models`:** a call at every start, and a server that is down would remove its models. The owner names them once.

## References

- Shapes: [design: system architecture](../design/architecture.md#service), [specs: protocol rules 14d and 47](../specs/protocol.md), [plans: roadmap](../plans/roadmap.md)
- Builds on: [ADR-0005](0005-keep-the-agent-loop-and-model-credentials-in-the-service.md), [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0013](0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md), [ADR-0021](0021-give-a-threads-specialist-a-worker-on-an-api-key-model-in-code.md), [ADR-0022](0022-run-any-session-on-any-sign-in-and-choose-workers-from-the-owners-list.md)
