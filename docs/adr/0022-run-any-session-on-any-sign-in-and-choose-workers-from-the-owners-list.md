# 0022 — Run any session on any sign-in, and choose workers from the owner's list

- **Status:** Accepted
- **Date:** 2026-11-20

## Context

**ADR-0021 made API-key models serve workers alone.** A model whose provider's sign-in is an API key, `deepseek` today, ran only as a thread's worker. The engine left it out of the models the owner's sessions, the profiles and the cross-reviews choose from. Several guards kept it there:
- `pocketd login` refused a key that a session that is no worker would run.
- pocketd refused to start while such a session was there.
- The owner's message to such a session, a specialist's brief on such a model and a consult on one were refused.

**The owner did not ask for that rule.** On 2026-11-20 the owner said: "I want deepseek to be able to power any session too. I didn't ask for that ADR. What I wanted was a way to specify preference for worker models." Having seen the live run, they also want their own local inference server to power any session.

**The worker model is fixed per profile.** ADR-0021 runs the profile's `worker:` model, else `deepseek/deepseek-flash`. The owner wants:
- a team-wide list of worker models, in order of preference;
- a way to override the list when they talk to the supervisor. For example, "Launch today's shift work, use deepseek" means every worker of that work runs the DeepSeek model of the list.

## Decision

1. **Any session runs any model pocketd has a sign-in for, an API key's included.**
   - `Engine.Models` lists every model with a sign-in. `GET /v1/models` and the desktop's New Session offer DeepSeek's models. A profile may name one, and a cross-review may run one.
   - The worker-only guards of ADR-0021 go: the refusals listed above, the start check (`CheckWorkerOnlySessions`) and `Engine.WorkerModels`.
   - `pocketd login` still takes a key only for a provider of `pig.APIKeyProviders`, only a literal one, and only while no pocketd serves the data folder, as ADR-0021 has it.
2. **The worker models are the owner's ordered list.**
   - The list is `workers.json` in the agents' profile folder, which agents' commands see read-only (ADR-0013). It is `{"models": ["provider/id", …]}`.
   - The list has 1 to 8 entries, each a model reference, with none named twice. It is checked in code (`profiles.LoadWorkers`). An unknown field or a bad entry makes the file invalid, and an invalid file starts no worker; `pocketd profiles` reports it.
   - With no file, the list is `deepseek/deepseek-flash`.
   - `router.Choose` with `Request.Worker` picks a model in this order:
     1. the model the supervisor asked for (route `worker-asked`; decision 3);
     2. else the profile's `worker:` (route `worker`);
     3. else the first entry of the list pocketd can run now, under the router's aliases (route `worker-team`, or `worker-default` with no file).
   - A model it cannot run refuses the call, as before. The router never falls back from the model the supervisor asked for, or from the profile's `worker:`, to another one.
3. **The supervisor passes the owner's choice with the delegation.**
   - `team_delegate` takes an optional `worker` that names an entry of the list: a provider (`deepseek`) for that provider's first entry, or a whole reference (`deepseek/deepseek-flash`).
   - pocketd resolves it when it records the thread, and keeps the model it resolved to on the thread (work database migration 11).
   - A name that matches no entry refuses the delegation, and the refusal lists the entries.
   - `team_delegate`'s description says to set `worker` only when the owner named a worker model for that work. The supervisor's roster names the list.
   - A plan task's delivery takes `worker` too.
4. **Protocol version 11.** The routes `worker-team` and `worker-asked` are new values of a session's `route`. A version 10 desktop knows only `worker` and `worker-default` as a worker's: it would offer a composer on the new routes that pocketd refuses, so the version changes and each side refuses the other's (Codex review of #33). From version 11, a client treats every route that starts with `worker-` as a worker's, so a later worker route needs no new version. Every other change removes a refusal, and the wire types stay the same.

## Consequences

- The owner can run a whole session, or a profile, on DeepSeek, and the desktop offers it.
- The owner sets the workers' models in one file and changes them without a release. They can override them for a piece of work by saying so to the supervisor.
- An API key now pays for any session, not only workers. The cost limit is the owner's choice of models.
- The supervisor's `worker` is the owner's words relayed by a model. Code checks that it names an entry of the owner's list, but not that the owner asked for it.
- **Known limits:**
  - A thread recorded before migration 11 has no asked model, and its worker follows the profile and the list.
  - The list names models of the providers pocketd has. The owner's local inference server needs its provider first. That is the next change: a file of the owner's own providers, each with a base URL and its models.
  - A thread keeps the model the supervisor asked for, even if the list changes later.

## Alternatives considered

- **Keep API keys for workers alone:** the owner did not ask for it, and it blocked a use they want.
- **A per-profile ordered list instead of a team-wide one:** the owner chose one list for the team. A profile's `worker:` still overrides it for that profile.
- **Let the supervisor name any model:** "Only people choose models" (roadmap: what carries over). The supervisor names an entry of the owner's list, and code holds it to that list.

## References

- Shapes: [design: system architecture](../design/architecture.md#service), [specs: protocol rules 14d, 22, 26 and 47](../specs/protocol.md), [plans: roadmap](../plans/roadmap.md)
- Set up by: [design: setup guide](../design/setup.md)
- Builds on, and supersedes in part: [ADR-0021](0021-give-a-threads-specialist-a-worker-on-an-api-key-model-in-code.md) (decision 1's worker-only rule, decision 2's worker model, and the last item of decision 4)
- Builds on: [ADR-0005](0005-keep-the-agent-loop-and-model-credentials-in-the-service.md), [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0013](0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md), [ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md)
