# 0037 — Unblock a thread when the supervisor's message reaches it

- **Status:** Accepted
- **Date:** 2026-11-21

## Context

**A supervisor's message leaves a blocked thread blocked.** [ADR-0032](0032-let-a-supervisor-send-to-a-thread-it-delegated-through-the-outbox.md) lets a supervisor send `team_send` to a thread it delegated. The message's delivery takes a done thread back to open (`store.SendDelivered`), and leaves a blocked thread blocked (its decision 4 and its known limits): the owner had named done alone.

**A blocked thread waits on the owner.** Its specialist reported `blocked` with `team_message`, usually with asks. Each ask waits in the owner's Inbox until the owner answers it, and the answer reaches the specialist as `The owner answered A<id> ("<question>"): <answer>`, once, through the asks' own delivery (protocol rule 29). The supervisor often sends to a blocked thread exactly what the asks wait for: another thread's finding, or the owner's decision told to the supervisor. The thread then stays blocked, and its asks stay in the Inbox for an answer nobody needs.

**The owner's choice, 2026-11-21.** Asked whether a blocked thread should stay blocked when the message reaches it (the recommendation was yes), the owner chose: "Unblock it — Odrade's message withdraws the ask and the thread goes back to open."

**Facts the design must keep:**
- The message's text is kept when `team_send` files it, before pocketd knows what the thread will be when the message reaches the transcript: the specialist may report again, the owner may answer, while it is queued.
- The noting of the message, under `outboxMu` then `workMu`, is the one step that changes the thread, and a report or a close notes the thread's placed messages first (ADR-0032, decision 4). The owner's answer is filed under `workMu`.
- A shift's round waits for what the outbox and the asks owe the specialist: an answer filed and not delivered counts, an open ask does not ([ADR-0024](0024-run-a-delegated-thread-in-rounds-as-a-shift-driven-in-code.md)).
- A plan's gate holds on its thread's open asks and on answers no write named ([ADR-0020](0020-plan-before-building-with-the-plan-gate-and-consults-in-code.md)).

## Decision

This decision supersedes, of [ADR-0032](0032-let-a-supervisor-send-to-a-thread-it-delegated-through-the-outbox.md), the blocked thread that stays blocked in decision 4 and its first known limit. The rest of ADR-0032 stands.

1. **The delivery of a supervisor's message takes a done or a blocked thread back to open**, in the transaction that notes it delivered (`store.SendDelivered`), once: only the noting that changes the message does. An open or a closed thread keeps its state.
2. **The same transaction withdraws each open ask of a blocked thread,** for a message whose text says so (decision 6): one kept before migration 20 says nothing of it, and leaves a blocked thread blocked, as ADR-0032 had it (`reports.unblocks`; Codex review of #62, round 1). The ask's state is `withdrawn`, and it names the message by its outbox key and when (`withdrawn_by`, `withdrawn_at`); the message's row says it unblocked the thread (`unblocked_at`). A withdrawn ask leaves `GET /v1/asks`, owes the specialist nothing, and is never delivered. The asks of an open or a done thread stay, since the owner decided for a blocked thread.
3. **Events go out with the change, under the locks as [ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md) orders them:** the noting holds `outboxMu`, then `workMu`, and publishes each withdrawn ask's `ask` event, then the thread's, which publishes the plan of a plan's thread, in that change (`Server.sendDelivered`). The paths that note a message (the delivery pass, `team_send`'s answer, and a report or a close that notes first) all go through it.
   - **Whatever files a state notes the thread's placed messages first,** so a message is never read as answering a state set after it reached the transcript. Beside the specialist's report and a close (ADR-0032), the owner's stop of a shift, which files pocketd's `blocked` report, does too: under `outboxMu` it withdraws the thread's supervisor's messages still queued, which go again after the stop, and notes those PiG placed, before the report (`Server.beforeStop`). A plan task's close that may end the plan, closing the lead's thread with it, notes the lead's placed messages after it withdraws its queued ones, so a blocked lead's asks are withdrawn before its close. The shift driver files its own reports only when nothing is owed to the specialist, so no message of the thread waits to be noted then (ADR-0024). (Codex review of #62, round 1.)
4. **The owner's answer and the withdrawal race; whichever commits first stands.** `store.AnswerAsk` answers only an open ask. An answer to a withdrawn ask is `store.ErrWithdrawn`, a conflict: `POST /v1/asks/{id}` answers `409 conflict` with the reason (`team.AnswerWithdrawn`), keeps nothing of the answer, and spends no `requestId`. An answer filed first stands, and reaches the specialist as any answer; the withdrawal then withdraws the thread's other open asks, or none, and the thread opens all the same.
5. **The desktop keeps the reason.** A withdrawn ask's event takes it out of the Inbox, as an answered one's does (`state.Data`). An answer refused because its ask no longer waits, answered elsewhere or withdrawn, is kept with the service's reason and the answer as the owner wrote it, before it was trimmed (`state.Store.AnswerRefusals`), and the Inbox shows it after the question's card is gone, until the owner dismisses it. An answer whose reply was lost, to an ask whose withdrawal's event came, is kept the same way, with the desktop's own reason: the event proves pocketd did not take it (Codex review of #62, round 1). Such an answer waits for that event even when a fetch drops its ask first, and the store keeps how an ask left only for an answer that is out, until it settles (round 2).
6. **What the specialist sees is the supervisor's message.** Its text ends with a line written in code (`team.SendText`): if the thread is blocked when the message reaches the specialist, the message unblocks it, its asks that wait for the owner are withdrawn and will not be answered, and the thread is open again. The line states the rule, never the asks, because the text is kept before the thread's state at the placement is known. A report or a close notes a placed message first, so the state the line speaks of is the one the noting reads. No other message is sent for the withdrawal.
7. **A shift's round never waits for a withdrawn ask:** a round waits for an answer filed and not delivered (`store.Owed`), and a withdrawn ask has none.
8. **A withdrawn ask holds no plan's gate** (`team.Gate`): it waits for no answer and has none to write in. A write that names it changes nothing. When it was an approved plan's last open ask, its supervisor gets a note that the ready tasks may go, as after an answer (`team.WithdrawnNote`). `team_plan`'s read lists it as withdrawn.
9. **`team_send`'s answer says what the delivery did.** A delivered message whose own noting unblocked the thread says so, and names each ask it withdrew by number and question: from what that noting committed when the answer itself notes the message, else from the message's row and the asks that name it (`store.AsksWithdrawnBy`), never from another message's noting. The list is bounded, however many asks the thread had: the first 8 by number and question, each cut to 160 characters, the next 100 by number, then a count (`team.MaxWithdrawnQuestions`; Codex review of #62, round 1), and so is its read (round 2). A queued message kept before migration 20 says only that a done thread opens (round 2). A queued message says that a blocked thread's asks are withdrawn when it is delivered. The tool's description and the supervisor's roster say it too.
10. **Work database migration 20** adds `asks.withdrawn_by`, `asks.withdrawn_at`, `reports.unblocked_at` and `reports.unblocks`, which `store.FileSend` sets, with an index of the asks by the message that withdrew them. **Protocol version 24** adds the ask's `withdrawn` state and the conflict of an answer to a withdrawn ask.

## Consequences

- The supervisor unblocks a thread by sending what it waits for; the owner no longer answers a question the supervisor settled, and the board shows the thread with its specialist again.
- The owner may lose an answer in progress: a question withdrawn while the owner types leaves the Inbox, and its draft with it. An answer sent too late is refused and shown with its text, so it can be copied.
- The supervisor's message to an open or a done thread carries a line about blocked threads that does not apply to it.
- The specialist is told the rule, not which asks: one that asked several questions learns from the message that none of them will be answered.
- Protocol version 24 and migration 20: the desktop must be rebuilt, and a restored work database at 19 is upgraded at the next start.
- **Known limits:**
  - An open or a done thread's asks stay when a message reaches it.
  - A withdrawn ask's question is not shown anywhere once it leaves the Inbox, but in the plan's read for a plan's thread, and in `team_send`'s answer for the first 8.
  - A thread blocked by pocketd's own report, at a shift's stop or end, is unblocked by a supervisor's message that reaches it after, with the asks its specialist put while it worked: the specialist's last report did not say blocked.

## Alternatives considered

- **Keep the blocked thread blocked (ADR-0032):** the owner chose to unblock it.
- **Unblock the thread and leave its asks in the Inbox:** the owner would answer questions the supervisor settled, and an open thread would keep waiting on them.
- **Tell the specialist which asks were withdrawn, in a message of its own after the withdrawal:** exact, but one more outbox message, sent after the supervisor's while the specialist acts on it, and a new run when that run ended already; the line in the supervisor's message says the same rule at no cost.
- **Name the asks in the supervisor's message when `team_send` files it:** the asks may change before the message is placed (an answer, a new report), so the text would name asks it does not withdraw.
- **Withdraw the asks when `team_send` is called:** the message may wait in a busy specialist's queue, or be dropped by a close, and the asks would be gone while nothing reached the specialist.

## References

- Shapes: [design/architecture.md](../design/architecture.md#service), [specs/protocol.md](../specs/protocol.md) (rules 1, 29, 30, 30a and 50)
- Built by: [plans/roadmap.md](../plans/roadmap.md) (item 17, the supervisor's message to a thread)
- Supersedes in part: [ADR-0032](0032-let-a-supervisor-send-to-a-thread-it-delegated-through-the-outbox.md) (decision 4's blocked thread that stays blocked, and its first known limit)
- Builds on: [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md), [ADR-0020](0020-plan-before-building-with-the-plan-gate-and-consults-in-code.md), [ADR-0024](0024-run-a-delegated-thread-in-rounds-as-a-shift-driven-in-code.md), [ADR-0032](0032-let-a-supervisor-send-to-a-thread-it-delegated-through-the-outbox.md)
