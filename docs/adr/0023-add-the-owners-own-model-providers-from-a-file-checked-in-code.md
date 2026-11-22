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

**PiG's catalog is fixed in its release.** Each built-in provider lists the models of PiG's catalog, generated for the release pocketd pins (v0.7.0, generated in early November 2026, the latest release). PiG fetches no built-in provider's model list, and the module proxy serves no later commit. On 2026-11-20 the owner found the listed models stale and chose to add newer ones in the same file.

**The owner's network is private.** The server's address must not enter this public repository, so the provider list lives in the owner's folder.

## Decision

1. **The owner's providers are `providers.json` in the agents' profile folder.** Agents' commands see that folder read-only ([ADR-0013](0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md)). The file is `{"providers": [{…}, …], "builtinModels": [{…}, …]}`, with at least one of the two, checked in code (`profiles.LoadProviders`). Its keys are read as written: another case, a key named twice, or an unknown key makes the file invalid. A boolean is `true` or `false`, and a number is a whole number, never `null`. A refusal never repeats a URL, which may hold a secret. `providers` holds 1 to 8 providers with distinct ids, each with:
   - `id`: lowercase letters, digits and dashes, starting with a letter, at most 32 characters. It names no built-in provider of PiG.
   - `name`: optional, at most 60 characters.
   - `api`: `openai-completions` or `anthropic-messages`.
   - `baseUrl`: an absolute `http` or `https` URL with a host, and no user, query or fragment. It is the URL PiG joins its path to: the OpenAI API's `…/v1`, which PiG follows with `/chat/completions`; the Anthropic API's root, which PiG follows with `/v1/messages`.
   - `key`: optional, `false` by default. With `true`, the owner stores the provider's key with `pocketd login <id>`, while pocketd is stopped, and its models are listed only once a key is stored. With `false`, pocketd sends the placeholder key `none`: PiG's adapters send no request without one, and a server that takes no key ignores it. A server that checks any key it is sent needs `true`.
   - `models`: 1 to 32 models with distinct ids. Each has an `id` with no space, an optional `name` (the id when left out), a `contextWindow` of 1 to 10,000,000 tokens, a `maxTokens` of 1 to the context window, and optional `reasoning` and `images`, `false` by default.

   `builtinModels` adds models to PiG's own providers. It holds 1 to 8 entries for distinct providers, each `{"provider": "anthropic", "models": [{"id": …, "name": …, "like": …, "contextWindow": …, "maxTokens": …}]}`. Each added model:
   - runs as the catalog's model `like` does, with every exported field of it: its API, base URL, compatibility settings, thinking levels and price;
   - has its own `id`, `name` and, where they are set, limits (`pig.addModels`);
   - is listed beside the provider's own, on the provider's sign-in, adapters and filters;
   - is written as itself, since the catalog record of `like` is not copied.

   A model the catalog has now is not added: the catalog's runs, so a PiG upgrade never breaks the file. A model `like` a model the catalog does not have is not added either. `pocketd models` and `pocketd profiles` say why for each, and `pocketd serve` logs it. `pocketd models` names the day PiG's catalog was generated.
2. **pocketd reads the file when it starts**, from the profile folder it pinned and protects, and refuses a folder put at its name since. `pocketd models`, `pocketd profiles` and `pocketd login` read it too.
   - Each provider is registered with PiG beside the built-ins, so its models run any session: owner's, profile's, worker's, review's (ADR-0022).
   - A worker list (`workers.json`) may name them.
   - An invalid file registers none of its providers. pocketd still starts, logs why, and the CLI commands print it.
   - A change takes effect at pocketd's next start.
3. **Keys follow ADR-0022's rule.** A stored key counts for a provider of `pig.APIKeyProviders` or for an owner's provider with `key: true`. `pocketd login <id>` refuses a provider with `key: false`.
   - PiG reads a stored key that starts with `!` as a command, and one that names `$VAR` from the service's environment, whatever the auth context. So pocketd gives PiG the stored sign-ins through `keptCredentials`, which resolves nothing.
   - An API key is used only as stored, and only if it is a literal one (`literalKey`).
   - An owner's provider with `key: false` is never read.
   - The sign-ins are read once, and the snapshot checked is the one used.
4. **An owner's provider's requests stay on its origin.** They go through `ownClient`, on a transport with no proxy, whatever `HTTP_PROXY` says, and with PiG's own bound on silence: five minutes for the headers and between reads of a stream, never for a whole stream that keeps sending. It follows a redirect only to the origin it came from. A key in a header that Go keeps across hosts, as Anthropic's `X-Api-Key`, never reaches a host the file does not name, and no proxy gets the conversation.
5. **The file is read through the folder pocketd pinned.** pocketd reads it, and `workers.json`, through one handle of the profile folder, checked against the one pinned at start. The file must be a regular file of its own there, never a link, even to another file of the folder. A folder that is gone is an error, never a missing file. The file must be UTF-8. A refusal names a value by its field or place, never by the value: a key or a URL with a token, written in the wrong field, would reach the service's log.
   - An added model never takes an id the catalog has for a model of any type. A limit it sets is above 0: a 0 written is refused, never read as the limit of the model it is like.
   - A `baseUrl`'s port is 1 to 65535.
   - `pocketd login` refuses a provider with `key: false` at once, without the data folder's lock.
6. **No protocol change.** The models are listed and chosen like any others.

## Consequences

- The owner runs sessions and workers on their own hardware at no per-token cost, and names it first in `workers.json` with DeepSeek after it.
- pocketd sends requests, with a stored key, only to a URL the owner wrote in a folder agents cannot change.
- **Known limits:**
  - pocketd uses PiG's defaults for an OpenAI-compatible server it does not know: the developer role, `max_completion_tokens`, `reasoning_effort` and `store: false`. Halogen takes all of these. A server that does not would need its compatibility settings in the file, a later change.
  - The router's model family (`router.Family`) knows no maker in `halogen-qwen3.8-flash-next`. A pull request its sessions write gets no cross-review, and the owner decides, as for any model of no known family ([ADR-0019](0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md)).
  - A change to the file needs a restart. A server that is down fails the runs on its models with the server's error. Whether a worker list's next model is tried then is not decided here: the router checks only that a model is listed.
  - The Responses API is not offered: pocketd's adapters for the two APIs above suffice.
  - An added model costs what the model it is like costs, in pocketd's usage, whatever its real price.
  - GitHub Copilot's own filter may leave out a model added to it.

## Alternatives considered

- **pi's `models.json` through PiG's coding runtime:** it reads credentials from the environment, which `storedOnly` forbids (ADR-0005), and it loads every agent folder's files.
- **Flags or the service's environment:** the address would sit in the systemd unit, and the list would not be checked with the profiles.
- **Discover the models from `/v1/models`:** a call at every start, and a server that is down would remove its models. The owner names them once.

## References

- Shapes: [design: system architecture](../design/architecture.md#service), [specs: protocol rules 14d and 47](../specs/protocol.md), [plans: roadmap](../plans/roadmap.md)
- Set up by: [design: setup guide](../design/setup.md)
- Builds on: [ADR-0005](0005-keep-the-agent-loop-and-model-credentials-in-the-service.md), [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0013](0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md), [ADR-0021](0021-give-a-threads-specialist-a-worker-on-an-api-key-model-in-code.md), [ADR-0022](0022-run-any-session-on-any-sign-in-and-choose-workers-from-the-owners-list.md)
