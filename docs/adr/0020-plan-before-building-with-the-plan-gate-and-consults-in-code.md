# 0020 — Plan before building, with the plan gate and consults in code

- **Status:** Accepted
- **Date:** 2026-11-19

## Context

Today an idea of the owner's reaches work in one step: the supervisor delegates it as a thread ([ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md)). Nothing turns the idea into a settled plan first, and nothing stops a delegation while a decision is still open.

**pi-pocket-team has plan threads for this,** built on 2026-10-30 after the owner decided their eight open questions:
- A plan thread has one plan file, which only the desk writes, and one planning session in a lead specialist's home.
- The planning session asks the owner each decision. An answer counts only once a later write of the plan names it.
- A gate refuses the plan's delegations while anything is open.
- The owner approves one exact revision of the plan and its task graph. Then the supervisor delegates the tasks as ordinary threads, one pull request each.
- Experts join as consults: short, read-only sessions whose one answer attaches to the plan as a finding.
- The owner decided that plan threads take no place on the desk, that at most 3 plans are open, and that the supervisor delegates the ready tasks once the owner approves.

**Its rules live in two places.** The data rules are TypeScript in a 38,000-line desk. The planning session's order of work is brief text. Consults are read-only through a hook that blocks tools by a marker in the first message. pocketd puts every rule it can state in code ([ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md)), and offers each session its tools by role, in code (ADR-0014).

**The owner reports duplicates.** pi-pocket-team asks the owner the same thing twice, and notifies the supervisor twice for one event. pocketgo must give each job one owner, and deliver each message once.

## Decision

1. **A plan is a row in the work database** ([ADR-0008](0008-keep-work-records-in-the-services-own-tables.md)), bound to one thread, the plan thread:
   - The supervisor starts it with `team_delegate` and `plan: {"start": true}`, naming the lead: a specialist that takes a delegation and has a home or a workspace, with no repository.
   - The plan thread's specialist session is the planning session. It runs in the lead's home, else its workspace, with a brief that pocketd writes. Its only coding tool is `read`: the lead builds nothing, and writes the plan only with `team_plan`.
   - Plan threads take no place on the desk. At most 3 plans are open at once.
   - States: `drafting` → `review` → `approved` → `building` → `done`, or `dropped` once the plan thread closes before `done`.
2. **The planning session writes the plan with `team_plan`**, a Go tool offered only to a plan thread's specialist session and checked again by the calling session's thread, never by an argument:
   - `write` replaces the plan's text: at most 20,000 characters, with the revision it read. A write from a stale revision is refused, so no decision is lost. The revision is the text's SHA-256. A write names the answered asks it writes in.
   - `tasks` drafts the task graph for the current revision.
   - `ready` asks for the owner's approval.
   - `consult` asks other specialists (point 7).
   - `read` shows the plan, its graph, its gate and its consults.
   - `write`, `tasks` and `consult` move a plan in `review` back to `drafting`. Once the plan is approved, they are refused.
3. **The plan's questions are its asks.** The planning session asks with `team_message`, as every specialist does. Its asks are the plan's: they wait in the Inbox, and each answer reaches the planning session once, with a line to write it into the plan. A plan thread's report takes no pull request.
4. **The gate is code** (`team.Gate`). A plan is settled when it has:
   - no open ask;
   - no answered ask that a later `write` has not named;
   - no consult starting or running.

   Each item not settled is named, as `A3 open`, `A4 not written` or `K2 running`. Four acts are checked, each in the transaction that does it:
   - **Ready** moves `drafting` to `review`. It needs a settled plan with text, and a graph drafted for the current revision.
   - **Approve** moves `review` to `approved`. Only the owner approves, over the protocol. The approval names the revision and the graph version the owner saw, and holds only for those, on a settled plan.
   - **Deliver** is `team_delegate` with `plan: {"id": N, "task": "tK"}`. The plan must be `approved` or `building`, with no open ask. The task must be in the approved graph and `ready`, and the call's agent and repository must be the task's. The first delivery moves the plan to `building`.
   - A new ask on the plan thread moves a plan in `review` back to `drafting`.
5. **A graph is checked in code** (`team.CheckGraph`):
   - It has 1 to 30 tasks, with unique ids `t1` to `t999`.
   - Each task has a title of 1 to 120 characters, and 1 to 8 acceptance lines of at most 300 characters each.
   - Each task's agent takes a delegation, in one of its repositories or in its home.
   - Each dependency names another task of the graph, and the dependencies have no cycle.

   A new graph replaces the draft. The approved graph is never changed.
6. **A task's state is code:**
   - It is `waiting` while a dependency is not `done`, and `ready` once every one is.
   - It is `delegated` while its thread works.
   - It is `done` once its thread closes with its work done: a linked pull request merged and none open, or, with no pull request, its last report `done`. A task whose thread closes otherwise is `ready` again, and the supervisor may deliver it again.
   - When every task is `done`, the plan is `done` and its thread closes. All of this happens in the transaction that closes the task's thread.
7. **A consult is a session pocketd starts, read-only by the tools it is offered:**
   - **Who:** the planning session of a plan in `drafting` asks 1 to 3 specialists at once a question of at most 2,000 characters, optionally on one of the plan's asks. An expert is a specialist that takes a delegation and has a home or a workspace. The lead is never one.
   - **The session:** each consult runs from the expert's profile in its home, else its workspace. It gets exactly `read` and `team_finding`: no `write`, `edit` or `bash`, and no other team tool. pocketd writes the question, the session's first message.
   - **Bounds:** at most 3 consults start or run at once per plan, and 8 in all. A call that would pass either bound is refused.
   - **The answer:** `team_finding` files one finding: a stance (`supports`, `objects` or `neutral`) and a summary of at most 2,000 characters. A second is refused.
   - **The end:** a consult with no finding 60 minutes after its call fails. A plan that ends stops its consults. Consults take no place on the desk.
8. **Each job has one owner, and each message goes once:**

   | Job | Owner | Delivered |
   | --- | --- | --- |
   | Start a plan | the supervisor, with `team_delegate` | its call key finds the plan again |
   | The plan's text, graph, readiness and consults | the planning session, with `team_plan` | each call is recorded with its key, and run again answers as it did |
   | A decision for the owner | the planning session's ask | the Inbox shows it once; pocketd places the answer in the planning session once (rule 29) |
   | Whether the plan is settled | `team.Gate` | — |
   | Approve | the owner, from the desktop's Inbox | its request id claimed in the transaction that approves |
   | Telling the supervisor the ready tasks, with each one's agent and repository | pocketd: one note at the approval, one at each change of a task's state, one when an answer clears an approved plan's open questions, and one when a place on the desk frees after the cap held a task back | by its key, placed once in the supervisor's transcript (`Engine.Placed`) |
   | Deliver a task | the supervisor, with `team_delegate` | one thread per call key; the gate in the transaction that opens it |
   | The consult's question | pocketd | by the consult's random owner, placed once |
   | The finding, or the end without one | pocketd, to the planning session | one note per consult, placed once |
   | Done, ready again, the plan's end | pocketd's code, at the close of a thread | — |

   - No one else tells a party the same thing. The supervisor is not told that a plan waits for the owner: only the owner can approve, and the Inbox tells them.
   - The planning session is not told of the approval, since its work is over.
   - The planning brief tells the planning session to report to the supervisor only when it is blocked.
9. **Protocol version 8** adds plans, their tasks and consults, the owner's approval, and plan events. A thread now names the plan and the task it works for.

## Consequences

- **The gate holds without the owner reading prompts.** A delegation that names a plan task waits until the plan is settled and approved, and until the task's dependencies are done. Every refusal names what it waits for.
- **The owner approves a plan from the Inbox.** The card shows the revision, the task graph and the consults. Once the plan is approved, the board shows it with its tasks.
- **A plan cannot change once it is approved.** Without a scope change (below), the owner drops it by closing its thread, and the supervisor starts another.
- **The supervisor still chooses when to deliver,** inside the desk's cap: pocketd tells it which tasks are ready, and delegates nothing itself (the owner's decision in pi-pocket-team).
- **A task's state follows its thread's close alone,** whatever runs inside the thread. A specialist may later run a task's work on cheaper worker sessions without a change to the gate.
- **A consult costs a session, not a thread.** A consult that a plan's end stopped may still finish its run; its finding is refused.
- **A delegation that does not name its plan is not gated.** pocketd cannot tell such a delegation from other work, so the supervisor's instructions say plan work names the plan.
- **Not ported from pi-pocket-team:**
  - the whole-plan verification, and scope changes (`plan revise`) with the carry rule;
  - section comments, reopening an ask, criteria and marks, and `plan amend`;
  - `plan nochange`, the `needs-plan` label, queued consults and the owner's stop of a consult;
  - plan files on disk. The plan lives in the work database, and the desktop shows it.

## Alternatives considered

- **Plan in the supervisor's session:** it carries every other thread, and it could delegate while a decision is open. pi-pocket-team moved planning to a lead's home for the same reasons.
- **A plan file in a folder the agents can read:** the plan's truth would be a file a session could change. The work database is the one writer's store, and the data folder is hidden from agents (ADR-0012).
- **Consults as subagents of the planning session:** a subagent runs on the planning session's model and its tools. A consult runs on the expert's own profile, with tools offered by role.
- **pocketd delivers the tasks itself:** the owner chose, in pi-pocket-team, that the supervisor delegates them inside the cap.
- **A queue for consults over the running bound:** the queue adds a state and a promotion rule. A refusal tells the planning session to wait for a finding.

## References

- Shapes: [design: system architecture](../design/architecture.md#service), [specs: protocol](../specs/protocol.md), [plans: roadmap](../plans/roadmap.md)
- Builds on: [ADR-0008](0008-keep-work-records-in-the-services-own-tables.md), [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0012](0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md), [ADR-0013](0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md), [ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md), [ADR-0016](0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md)
