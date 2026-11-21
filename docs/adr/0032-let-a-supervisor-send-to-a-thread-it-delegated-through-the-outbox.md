# 0032 — Let a supervisor send to a thread it delegated, through the outbox

- **Status:** Accepted
- **Date:** 2026-11-21

## Context

**A supervisor cannot reach a thread it delegated.** [ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md) gives a supervisor `team_delegate` and `team_close`, and `team_message` goes the other way, from the specialist. Only the owner messages a specialist's session, from the desktop. When new information for a thread reaches the supervisor (another thread's review, a CI result, the owner's decision), it has two bad ways on (frostyard/pocketgo#54):
- **Relay through the owner:** it writes the message for the owner to paste. That costs the owner's attention, and nothing confirms the message went.
- **Close and delegate again:** the specialist's context is lost, and a new worktree thread cannot report the old thread's pull request ([ADR-0016](0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md)).

**Both happened on 2026-11-21.** T72 had to renumber its ADR after core#281 merged the same number, and the owner passed that on. For T77, the supervisor wrote a blocking point from T80 for the owner to paste. It never reached T77, which reported done without the fix, with `ready-for-human` set on a pull request the owner had blocked. In both cases the supervisor had the exact text to send.

**pocketgo has the parts:**
- The outbox, the `reports` table, places each message pocketd owes a session once, by its request key, until `Engine.Placed` finds it in the transcript, across restarts (ADR-0014, item 5). It carries a worker's replies to its specialist already ([ADR-0021](0021-give-a-threads-specialist-a-worker-on-an-api-key-model-in-code.md)).
- A shift's round waits for everything owed to the specialist's session in that outbox, and reads its verdict from the last run after its own input ([ADR-0024](0024-run-a-delegated-thread-in-rounds-as-a-shift-driven-in-code.md)).
- A close withdraws a thread's queued messages before it commits, and drops the rest after it.

**The owner's choices, 2026-11-20 and 2026-11-21:**
- Build the issue's proposal: a supervisor-only `team_send(thread, text)`, for a thread that is not closed. A done thread goes back to open once the message is delivered, so its next report is expected.
- The message goes through the same outbox as the owner's answers, up to 6,000 characters like a report, and the tool says whether it was delivered or queued.
- The desktop shows it in the thread, marked as the supervisor's.
- **Shifts:** a supervisor's message is a round input, and does not pause the shift, as the owner's message does.
- **No owner control:** no switch per thread turns supervisor messages off.
- **Plans** were left open: a plan's thread is to be treated like any thread, unless the plan gate makes that unsafe.
- The owner dislikes refusals nobody asked for: a rule refuses only what correctness needs.

## Decision

1. **`team_send` is a supervisor's team tool**, offered by role (`roleTools`, after `team_schedule`) and checked again in `service/internal/api`. It takes `thread` and `text`, and refuses, in code (`team.CheckSend`, `team.CheckSendTarget`):
   - a caller that is no supervisor;
   - no thread, and a text that is empty or over 6,000 characters (`team.MaxSend`);
   - a thread a session of another profile delegated, as `team_close` refuses one, and a cross-review, which no supervisor delegated ([ADR-0019](0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md));
   - a closed thread, and one still starting, whose specialist has no session to take a message yet.

   An open, a done and a blocked thread take one.
2. **The message is a row of the outbox** to the thread's specialist session, kept once by the call's key (`team.SendKey`: `team-send:<supervisor session>:` and the `api.TeamCall` key), only while the thread is open, done or blocked (`store.FileSend`). A call run again after a restart finds its row, whatever the profiles and the thread say by then. Its text is pocketd's: `[T<thread> · <supervisor>, the supervisor] <text>`, then a line asking for a report.
3. **The outbox places it as it places the owner's answers:** sent once, followed until PiG's record of its key shows its entry in the transcript, tried again until it goes and at the next start. A busy specialist's queue holds it for the run's next turn. It goes to a watched session, and only once the thread's brief is in the transcript, as the owner's message does (protocol rule 10). A close withdraws it while it is queued and drops it after the close commits: it goes to no one, as a worker's reply does. A plan task's close that may end its plan, closing the lead's thread with it, withdraws the lead's queued messages first too; they go again if the lead's thread stays open.
4. **Its delivery takes a done thread back to open**, in the transaction that notes it placed (`store.SendDelivered`), with the thread's event, under `outboxMu` and `workMu`. Only that noting reopens the thread, once; a blocked thread stays blocked. A report the specialist files notes first, under `outboxMu` held through the filing, each of the thread's messages PiG already placed: the report came after them, so its state stands, and a message placed after the report reopens the thread once noted. A close notes them first too, after it withdraws what is queued, so it reads the state they leave: a plan task a message followed after its done report is ready again at its close, not done.
5. **The answer says where the message is:** delivered once PiG's record shows it in the transcript, with the thread's state now, never which message moved it; else queued. A message kept whose place pocketd cannot read is answered as kept, with "do not send it again", never as a failure the supervisor would send again under another key. One a close dropped is an error: it went to no one. The answer names where the specialist's reports go: to the caller when it delegated the thread, else to the delegating session.
6. **A shift takes it as a round input.** It pauses nothing: only the owner's message, claimed as it goes, pauses a shift. The round waits for it, as for anything owed to the specialist, and its verdict is the `STATUS` line of the last run after its own input, the message's run included.
7. **A plan's thread is a thread like any.** Its lead takes a message. The plan gate is code at each of its acts, and a message changes none of them: a write the lead makes after it moves a plan in review back to drafting, as any write does, and only the owner approves ([ADR-0020](0020-plan-before-building-with-the-plan-gate-and-consults-in-code.md)).
8. **pocketd marks the message as the supervisor's**, from its own record and never from the text. The request key of each submission that carries one names the supervisor's session (`team.SentBy`), and no other message has a key of that prefix: the owner's keys are their device's. `service/pig` gives each user entry and each queued message its `sender` from the key of the submission that placed or carries it. A queued message whose submission it cannot read is left out of the queue, never shown as the owner's, and the watch reads it again without waiting for a commit.
9. **Protocol version 20** adds `Entry.sender` and `QueuedMessage.sender`, `{"kind": "supervisor", "session": <id>}`. The desktop draws such a row with the supervisor's name and a `supervisor` pill, queued or placed (`state.Row.FromSupervisor`), and the owner's own row as before, whatever its text.
10. **An older supervisor's session gets `team_send`** at start, after its `team_schedule`, as it gets `team_schedule` ([ADR-0030](0030-send-scheduled-messages-from-the-owners-file-and-the-supervisors-tool-in-code.md); `Engine.SetSendTool`). A standing one takes it at its idle refresh ([ADR-0026](0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md)).

## Consequences

- The supervisor passes on what it learns for a thread itself, and is told whether it arrived; the owner no longer relays.
- A done thread a message reopens is with its specialist on the work board until the next report. A plan task's thread closed in between counts as not done, and its task is ready again ([ADR-0020](0020-plan-before-building-with-the-plan-gate-and-consults-in-code.md), point 6), since its last report did not follow the message.
- The supervisor's messages show in the specialist's transcript as the supervisor's. pocketd's other messages there, the brief, the answers and a shift's rounds, still show as the owner's, as before.
- Protocol version 20: the desktop must be rebuilt.
- **Known limits:**
  - A blocked thread stays blocked when a message reaches it: the owner named done alone. The specialist's next report sets its state.
  - The owner cannot turn a thread's supervisor messages off, by the owner's choice.

## Alternatives considered

- **Relay through the owner, or close and delegate again:** the two ways the issue shows fail; see Context.
- **A note, as pocketd's own messages to a supervisor are:** a shift's round counts only what the outbox and the asks owe the specialist, so a note would let a round end before the message's run.
- **Mark the message by its text on the desktop:** the owner may type any text, so the desktop would show an owner's message as the supervisor's. pocketd marks it from the key only its outbox writes.
- **Pause the shift, as the owner's message does:** the owner chose a round input. The supervisor's message is work for the round, not the owner taking over.
- **A switch per thread for the owner:** the owner declined it.

## References

- Shapes: [design/architecture.md](../design/architecture.md#service), [specs/protocol.md](../specs/protocol.md) (rules 1, 16a, 25, 30a and 55)
- Built by: [plans/roadmap.md](../plans/roadmap.md) (item 17, the supervisor's message to a thread)
- Builds on: [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md), [ADR-0019](0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md), [ADR-0020](0020-plan-before-building-with-the-plan-gate-and-consults-in-code.md), [ADR-0021](0021-give-a-threads-specialist-a-worker-on-an-api-key-model-in-code.md), [ADR-0024](0024-run-a-delegated-thread-in-rounds-as-a-shift-driven-in-code.md), [ADR-0026](0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md), [ADR-0030](0030-send-scheduled-messages-from-the-owners-file-and-the-supervisors-tool-in-code.md)
