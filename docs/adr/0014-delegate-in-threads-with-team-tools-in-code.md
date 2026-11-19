# 0014 — Delegate in threads, with team tools in code

- **Status:** Accepted
- **Date:** 2026-11-19

## Context

Phase 2's Done-when starts with the owner asking the supervisor for a change, and a specialist opening a pull request in a thread. pi-pocket-team does this with model-facing tools:
- `team_delegate` opens a thread, makes a git worktree and starts the specialist's session with a brief.
- `team_message` files the specialist's report, with the decisions it needs from the owner as asks.
- A desk keeps the threads, capped at 12 by default (16 in the owner's settings).

The rules around those tools are a mix of code and instructions: the cap, who may delegate, the worktree's name, the brief, the limits on asks. pocketd runs each agent in its own durable conversation (ADR-0004), from a profile (ADR-0013). ADR-0011 puts every rule that can be stated in code.

## Decision

1. **A thread is a row in the work database** (ADR-0008), with:
   - the specialist's profile and session, and the supervisor's session
   - the repository, the folder and the branch it works in
   - the task, its state and the specialist's last report

   Its states: `starting` while pocketd sets it up, `open` while the specialist works, `done` or `blocked` once it reported so, and `closed`. An **ask** is a row too, with the thread, the asking session, the question, its options, and the owner's answer.
2. **The team's tools are Go code**, offered by role:
   - `team_delegate` and `team_close` to a session from a supervisor profile.
   - `team_message` to a thread's specialist session.
   - A tool refuses a caller of another role as well, in code.
3. **`team_delegate` checks, then acts, in code:**
   - **Refused:**
     - a caller that is not a supervisor
     - a target that is private, a supervisor, unknown, or cannot start
     - a repository the target's profile does not name
     - a task over 6,000 characters or a title over 120
     - a full desk. The desk counts the threads not closed; its cap is `-desk-cap`, default 12.
   - **Then:**
     - With a repository, it adds a git worktree at `<checkout>--<agent>-<slug>`, on the branch `<agent>/<slug>` from the checkout's `HEAD`. git runs in the sandbox (ADR-0012), so a hook or a filter the checkout configures runs with an agent's rights. Without a repository, the specialist works in its profile's home, else its workspace.
     - It starts the specialist's session from its profile, with the router's model, opens the thread, and sends the brief, which code writes. A thread that cannot start closes with why, and leaves the desk.
   - A supervisor's session names, in its instructions, the agents it can delegate to and the desk's cap, from the profiles when the session starts.
   - A retry of the same call finds the same thread: its key is the calling conversation and call.
4. **`team_message` files a report:**
   - its status is `working`, `done` or `blocked`
   - the message is at most 6,000 characters
   - at most 4 asks, each a question of at most 500 characters, with no options or 2 to 6 of them at most 40 characters each

   The report reaches the supervisor's session as a message, and the asks wait in the owner's Inbox.
5. **The owner answers an ask over the protocol.** The answer reaches the asking session as a message, once, and the ask is answered once. An answer pocketd could not send goes at its next start.
6. **`team_close`** closes a thread the supervisor delegated, freeing its place on the desk. The owner closes one over the protocol.

## Consequences

- Delegation, reports and asks work without the owner reading prompts: every rule above has a test.
- A specialist works on a branch in its own worktree, next to the checkout, as in pi-pocket-team, so the two systems' worktrees look alike.
- Protocol version 4: threads and asks, with their events. A desktop and a pocketd of different versions refuse each other.
- A supervisor's session made before this change has no team tools: the owner starts a new one.
- The team tools are not profile tools: a profile's `tools` names the coding tools its agent gets, and the role decides the team tools.
- Threads stay on the desk until someone closes them. The work board (Phase 2, slice 5) shows them, and the pull-request slice (4) closes those whose pull request merged.
- The cap counts threads, not running sessions. A specialist that reported `done` still holds its place until its thread closes.

## Alternatives considered

- **Delegation as prompt instructions to call `pocketd` over HTTP:** the rules would live in a prompt, and a model could skip them.
- **One tool set for every session:** a specialist could delegate, and a plain session could report into no thread. Offering tools by role, and checking the role in code, closes both.
- **A worktree inside the checkout:** git refuses nested worktrees at some paths, and pi-pocket-team's folders sit next to the checkout.

## References

- Shapes: [design: system architecture](../design/architecture.md#service), [specs: protocol](../specs/protocol.md), [plans: roadmap](../plans/roadmap.md)
- Builds on: [ADR-0004](0004-build-the-service-on-pigs-durable-harness.md), [ADR-0008](0008-keep-work-records-in-the-services-own-tables.md), [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0012](0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md), [ADR-0013](0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md)
