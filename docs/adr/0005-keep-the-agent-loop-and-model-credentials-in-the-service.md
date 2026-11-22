# 0005 — Keep the agent loop and model credentials in the service

- **Status:** Accepted; API keys, rejected below, pay for threads' workers alone since [ADR-0021](0021-give-a-threads-specialist-a-worker-on-an-api-key-model-in-code.md); the desktop signs pocketd in to GitHub Copilot, the sign-in still stored and refreshed on the service host in one place, since [ADR-0062](0062-sign-pocketd-in-to-github-copilot-from-the-desktop-with-a-device-flow-it-runs.md)
- **Date:** 2026-11-18

## Context

The owner pays for models through subscriptions: OpenAI Codex (ChatGPT), Anthropic Claude and GitHub Copilot.

PiG's `ai` package signs in to all three. Its OAuth registry (`ai/oauth_registry.go`) names `openai-codex`, `anthropic` and `github-copilot`, and it reports tokens and cost for each call.

PiG's divergence record documents that the Anthropic and Copilot sign-ins send the vendor's own client identity: the `claude-cli` user agent, and the VS Code Copilot Chat headers.

Coder's AI features serve two other setups:
- **AI Bridge:** a gateway for model calls made by agent CLIs inside a workspace.
- **Coder Agents:** Coder's own agent loop.

Agents may run in execution environments on other hosts ([ADR-0009](0009-run-agents-in-coder-workspaces-through-pi-env-and-the-rest-api.md)).

## Decision

The service makes every model call.
- **Sign-ins:** it signs in to the owner's subscriptions with PiG's `ai` sign-ins.
- **Tokens:** it stores and refreshes the tokens on the service host.
- **Execution environments:** they get files, commands and terminals only. No model credential or token enters one.
- **Coder:** its AI features are not in the model path.

## Consequences

- **One credential store:** credentials are stored and refreshed in one place, outside the repository.
- **Untrusted compute:** a workspace can be treated as untrusted, since it holds nothing that reaches a model provider.
- **Usage and cost:** come from PiG's per-call usage, not from a gateway.
- **Vendor changes:** a vendor-side change to client checks breaks that sign-in until PiG ships a fix. The service takes PiG's provider layer unforked ([ADR-0004](0004-build-the-service-on-pigs-durable-harness.md)).

## Alternatives considered

- **API keys:** the owner's models are paid for through subscriptions.
- **Coder AI Bridge:** it routes model calls made from inside workspaces, not calls from a loop that runs in the service.
- **Vendor CLIs inside each workspace:** this puts a credential in every workspace.

## References

- Shapes: [design: system architecture](../design/architecture.md), [plan: roadmap](../plans/roadmap.md)
- Set up by: [design: setup guide](../design/setup.md)
- Builds on: [ADR-0004](0004-build-the-service-on-pigs-durable-harness.md)
- PiG: [divergences](https://github.com/MichaelKinsy/PiG/blob/main/docs/parity/DIVERGENCES.md)
