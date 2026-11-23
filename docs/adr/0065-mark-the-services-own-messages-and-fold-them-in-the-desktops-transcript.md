# 0065 — Mark the service's own messages, and fold them in the desktop's transcript

- **Status:** Accepted
- **Date:** 2026-11-22

## Context

**pocketd places many messages in sessions itself.** Its outbox (`Server.place`) brings a thread's report to the session that delegated it (`[T300 · Murbella · done] …`, `team.ReportText`), notes to a supervisor (a plan's, a consult's, a gather's, a launch's, a shift's), a cross-review's verdict to the author's session, a scheduled message (`[scheduled] …`), a thread's brief, the owner's answer to an ask, a shift's rounds and a worker's messages and replies. Each goes as a submission whose request key pocketd writes. A device's message has a key of its own form, `u:<device>:<request id>` (`requestKey`), which nothing else writes.

**The desktop draws all of them as the owner's.** Only a supervisor's message carries a `sender` ([ADR-0032](0032-let-a-supervisor-send-to-a-thread-it-delegated-through-the-outbox.md), decision 9). Every other user entry shows as "You", with its whole text as Markdown. In a supervisor's session a report takes a screen or more, and a busy night brings dozens: the reading pane is mostly reports the Inbox and the work board already show, and the conversation between the owner and the supervisor is hard to follow.

**The desktop cannot tell them apart today without reading their text.** ADR-0032 refused a mark read from the text: the owner may type any text.

## Decision

1. **pocketd marks the service's own messages.** `service/pig` gives a user entry and a queued message the `sender` `{"kind": "service"}` (`proto.SenderService`, with no `session`) when the request key of the submission that placed or carries it is neither a supervisor's (`team.SentBy`) nor a device's (`team.IsOwnerKey`, the prefix `team.OwnerKeyPrefix`, `u:`, which `requestKey` writes). A supervisor's message keeps its own kind, and the owner's message, and a submission with no key, carry none, whatever their text. The mark comes from pocketd's record of the submission, never from the text, as ADR-0032's does.
2. **Protocol version 34** adds the kind. A version 33 client would show such a message as the owner's, whole.
3. **The desktop folds a message the service placed to one line, by default** (`state.Row.FromService`). The line is a button with a chevron: the message's bracketed header (`[T300 · Murbella · done]`, `[scheduled]`), when it starts with one, then the start of its first line of words as plain text, cut with "…" (`state.FoldOf`, `state.FoldedRunes`). Activated by a click, Enter or Space, and named with its state for a screen reader as the transcript's other folds are, it opens to the whole message as Markdown, and folds it again (`state.Unfolded`). The header is read from the text only to draw the line; whether a message folds is pocketd's mark alone.
4. **The choice is the window's**, per message, for as long as the client runs: no setting, no service state. The iPad and the phone draw the same row, from the same code.
5. **Every message the service places folds**, since they come by the one delivery path: reports, notes, verdicts, scheduled messages, briefs, answers, rounds and workers' messages. The owner's own messages and a supervisor's message do not fold: their words were written for the session. The agents' replies show as before.

## Consequences

- A supervisor's session reads as the conversation between the owner and the supervisor, with each report one line that opens in place.
- A scheduled message and the owner's answer to an ask fold too, though their words are the owner's: they come through the outbox, with pocketd's header or lead-in, and their line says what they are.
- A thread's brief folds in the specialist's session: its line names the supervisor, the thread and the title.
- Protocol version 34: pocketd needs a deploy and a restart, and the desktop and the iPad app a rebuild, together, since each side refuses the other's version.
- The service's messages no longer show as "You": their folded line names no sender, and their header says where they come from.
- **Known limit:** a row's key changes when a queued message is placed (`q<submission>` to `e<entry>`), so a queued message the owner opened shows folded once placed.

## Alternatives considered

- **Match the header's text on the desktop:** no protocol change and no deploy, but the owner's message that begins with such a header would fold, and a change of pocketd's wording would leave its messages whole with no test across the two modules to catch it. pocketd knows which messages it placed.
- **A kind for each sort of message (report, note, schedule, brief, answer):** the desktop folds them all alike, and each sort would need its own reading of the keys, which the outbox does not write in one form.
- **Fold only reports:** the notes, verdicts and scheduled messages break the reading pane in the same way, and come by the same path.
- **Keep the open state in pocketd, or in the settings file:** a message opened once is read, so the window's memory is enough, and a write per click would be a service request or a settings save for a view choice.

## References

- Shapes: [specs/protocol.md](../specs/protocol.md) (rules 1 and 16a), [design/architecture.md](../design/architecture.md#desktop-client)
- Built by: [plans/roadmap.md](../plans/roadmap.md) (item 38, the service's messages folded)
- Builds on: [ADR-0032](0032-let-a-supervisor-send-to-a-thread-it-delegated-through-the-outbox.md), whose decision 9 it amends (the kinds of `sender`), [ADR-0010](0010-port-the-desktop-client-selectively-from-frostyard-pocket.md), [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0030](0030-send-scheduled-messages-from-the-owners-file-and-the-supervisors-tool-in-code.md), [ADR-0040](0040-build-the-ipad-and-iphone-client-from-the-desktops-code-on-mygos-ios-target.md)
