# 0021 — Give a thread's specialist a worker on an API-key model, in code

- **Status:** Accepted
- **Date:** 2026-11-20

## Context

**The owner's operating model:** frontier models plan, and cheap, fast worker models do the work. In pi-pocket an agent planned, then spawned 1 worker subagent with a full brief. The worker's answer came back as a message that woke the agent. The agent reviewed `git diff`, sent fixes to the same worker, and stopped and told the owner when the worker model could not run.

**pocketgo has no worker.** A session has no subagent, so all of a thread's work runs on its profile's own model, a frontier one. The owner pointed this out on 2026-11-19: the worker is the main cost saving, and must not be dropped.

**Workers run on the DeepSeek API, which takes an API key.** [ADR-0005](0005-keep-the-agent-loop-and-model-credentials-in-the-service.md) keeps every credential in the service and pays for models through subscriptions; it rejected API keys. On 2026-11-19 the owner made DeepSeek the one exception, for workers only. They also sometimes use a local inference server. Both speak the OpenAI chat-completions API.

**What PiG gives:** PiG v0.7.0 has a built-in `deepseek` provider. Its catalog holds `deepseek-flash` (DeepSeek V4.1 Flash, $0.30 in and $1.20 out per million tokens) and `deepseek-v4-pro`, both at `https://api.deepseek.com`. Its credential is an `api_key` entry in the sign-ins file pocketd already keeps in its data folder. PiG can also make a provider from a base URL and a list of models.

## Decision

1. **API-key providers serve workers only.** `pocketd login PROVIDER` signs a subscription in by OAuth, as before. A provider with no OAuth sign-in, as `deepseek`, takes an API key, read from standard input into the data folder's sign-ins, so a key never replaces a subscription's sign-in. A model whose provider's stored sign-in is an API key runs only as a worker:
   - the engine lists it only among the worker models (`Engine.WorkerModels`), never among the models the owner's sessions, the profiles and the cross-reviews choose from (`Engine.Models`);
   - `GET /v1/models` leaves it out, so the desktop never offers it.

   The owner's rule that MAI-Code models run only as workers is code too (`router.WorkerOnly`): the router refuses a `mai-code-*` model for the owner's session and a profile's model, and `GET /v1/models` leaves it out.
2. **A thread's specialist has one worker, through `team_worker`.**
   - The first call starts the thread's worker and gives it the call's message as its brief. The worker runs in the thread's folder, with the coding tools of the specialist's profile and no team tool, in the sandbox like any session.
   - A later call sends its message to the same worker. A message sent while the worker runs waits in its queue, as the owner's would.
   - The worker model is code (`router.Choose` with `Request.Worker`): the profile's `worker:` field when it has one (rule `worker`), else `deepseek/deepseek-flash` (rule `worker-default`). A worker model pocketd cannot run refuses the call, and its answer tells the specialist to stop and tell the owner.
3. **The worker's answer comes back once.** When a worker's run ends with a reply, pocketd sends the specialist session that reply, as `[T<id> · worker] <reply>`, clipped to `team.MaxWorkerReply`. It goes through the outbox as a report does, keyed by the thread and the reply's entry, so it is kept once and counts as delivered only once `Engine.Placed` finds it in the transcript. At its start pocketd sends any such reply a stop left unsent.
4. **The worker is the thread's.**
   - It takes no message from the owner (`409 conflict`), and the desktop shows its session (route `worker` or `worker-default`) read-only.
   - It takes no place on the desk.
   - It stops when its thread closes: a run still going is stopped, and a reply that comes after reaches no one.
5. **Protocol 9** carries the routes `worker` and `worker-default`, the worker's session that takes no message, and the models list without worker-only models.

## Consequences

- A thread's work can run on DeepSeek at a fraction of a frontier model's price, with the specialist planning and reviewing.
- An API key now pays for model calls, which ADR-0005 avoided. It lives in the data folder with the subscriptions' sign-ins, and pays for workers alone.
- The specialist still pays for its own turns: planning, briefing, reviewing the worker's diff.
- **Known limits:**
  - A cross-review takes the author's family from the specialist's model (ADR-0019), not from the worker's that wrote the code: a Claude specialist's worker on DeepSeek gets a GPT reviewer.
  - Nothing stops the specialist from editing the folder while its worker runs. The brief and the skill tell it to wait for the answer.
  - The worker model is per profile or the team default. A per-task choice and a budget come later.
  - The owner's local inference server needs a provider pocketd does not have built in: a file of the owner's providers, each a base URL and its models, checked in code, is a later change.
  - A thread started before this change has no `team_worker`: its specialist's session keeps the tools it was made with.
  - A message the specialist queued for its worker before its thread closed may still run; its reply reaches no one.

## Alternatives considered

- **A worker as a thread the supervisor delegates:** it takes a place on the desk, and the supervisor must relay every fix. The specialist that planned the work is the one that reviews it.
- **Run the specialist itself on DeepSeek:** the planning and review would lose the frontier model.
- **Keep API keys out, and run workers on a subscription model:** the owner's current practice is DeepSeek, and the subscriptions' cheapest models cost more per task.

## References

- Shapes: [design: system architecture](../design/architecture.md#service), [specs: protocol rules 14d, 22, 25 and 47](../specs/protocol.md), [plans: roadmap](../plans/roadmap.md)
- Builds on: [ADR-0005](0005-keep-the-agent-loop-and-model-credentials-in-the-service.md), [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0012](0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md), [ADR-0013](0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md), [ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md), [ADR-0019](0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md)
