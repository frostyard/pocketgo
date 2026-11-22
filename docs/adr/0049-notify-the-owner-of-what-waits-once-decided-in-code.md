# 0049 — Notify the owner of what waits, once, decided in code

- **Status:** Accepted
- **Date:** 2026-11-21

## Context

**The owner has to keep looking at the window.** frostyard/pocketgo#98, filed on 2026-11-21: nothing tells the owner when something begins to wait for them while PocketGo is in the background. What waits shows only inside the window: the sidebar's counts, the status bar and the phone's Inbox tab badge (`state.InboxBadge`, [ADR-0044](0044-navigate-a-compact-window-by-a-bar-of-tabs.md)). The only notify hook is the activity console's.

**What waits for the owner** is what the Inbox lists:
- an ask from a thread ([ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md));
- a question from the supervisor or a private agent ([ADR-0048](0048-put-the-supervisors-and-a-private-agents-questions-to-the-owner-inline-in-code.md));
- a tool call to approve;
- a pull request to review (`state.ToReview`);
- a plan to approve (`state.ToApprove`);
- a lesson to decide ([ADR-0015](0015-keep-each-agents-lessons-approved-by-the-owner-in-code.md)).

The issue also asks for a notification when a run ends in a session where the owner sent the last message, so the owner knows the answer is ready.

**The owner has seen duplicate notifications from an earlier system.** Each thing must notify once, through a reconnect, a refetch, a second copy of an event and a restart of the app.

**The desktop already gets what it needs, with no protocol change:**
- Every list's fetch, and every event, of every session, entries included ([protocol rule 16b](../specs/protocol.md)).
- `SendMessage`'s answer names the owner's message by its `submissionId`, and so does its user entry (rule 16a).
- pocketd marks a supervisor's message with its `sender` (rule 30a, [ADR-0032](0032-let-a-supervisor-send-to-a-thread-it-delegated-through-the-outbox.md)), but no other message it places: a report or an answer looks like the owner's. Only the desktop that sent a message knows it is the owner's.
- A session's `running` state cannot tell a run's end: pocketd's own code notes that "a short run's busy state may never reach `PublishLive`". pocketd's rule for a run's end is "a final answer ends its run": an assistant entry whose `stopReason` is not `toolUse`.

**What MyGo v0.10.0 gives:**
- `NewNotification` with an id of the app's own, and `App.OnNotificationClick`, which gets that id, also for a click that launched the app (macOS) when it is registered before `Run`. On macOS, the first `Show` asks the owner to allow notifications and waits for the answer.
- `App.SetBadgeCount`: the Dock's badge on macOS, the launcher entry's count on Linux. On Windows, this release keeps the count in memory and draws nothing; on iOS, its mobile backend ignores it.
- `Window.OnFocus`, `Window.OnBlur` and `Window.IsFocused`.

## Decision

1. **The rules live in `desktop/internal/state`, each with its test** ([ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0010](0010-port-the-desktop-client-selectively-from-frostyard-pocket.md)). The `Store` decides each notice as it applies an event or an answer, and hands it to `Options.Notify`. State also decides:
   - each notice's key, words and target (`Data.AskNotice` and the rest);
   - whether it shows (`ShowNotice`);
   - the id a click hands back (`NoticeID`, `ParseNoticeID`);
   - the badge's count (`InboxCount`).

   `desktop/internal/app` only sends what state decides, and `desktop/cmd/pocket` joins it to MyGo.
2. **Each thing's key is the thing itself:**

   | What waits | Key | A click opens |
   | --- | --- | --- |
   | an ask | `ask:<id>` | the Inbox at its card |
   | a question | `question:<id>` | its session |
   | a tool call to approve | `approval:<id>` | the Inbox at its card |
   | a pull request to review, at its head | `pull:<id>:<headSha>` | the Inbox at its card |
   | a plan to approve, at its revision | `plan:<id>:<rev>` | the Inbox at its card |
   | a lesson, at what waits on it (`LessonSeen`) | `lesson:<id>:<digest>` | the Inbox at its card |
   | a run that answered the owner's message | `run:<session>:<submission>` | its session |

   A push to a pull request, a new revision of a plan and another proposal on a lesson notify again. A change that keeps the key, such as a new title, does not.
3. **Each key notifies once.** The store keeps the keys it has seen, and the app writes them to `settings.json` by service URL (`config.Settings.Notified`), with the other per-device marks. They are read again as a sign-in to that service begins. A service keeps at most 500 keys (`NoticeKeysKept`); past them, the oldest keys of things that no longer wait go first, and a key of a thing that still waits is never dropped.
4. **What waits at a list's first load notifies nothing.** A list notifies nothing until its first successful load after a sign-in begins, at the app's start or at the sign-in form, and it takes that load as seen; the badge counts what it holds. A first load that fails waits for the next one. A refetch after pocketd's `resync` notifies what began to wait meanwhile, and nothing it notified already.
5. **A finished run notifies for the owner's message from this desktop:**
   - The store follows a session's entries from the moment the owner sends it a message, until every message it sent there settles, and keeps only the first line of an answer.
   - `Store.Send`'s answer names the message's submission. Every such message counts until it settles, so a message the session queues while the run before it works does not take that run's place.
   - Its user entry starts its turn. A user entry of another submission ends the turn: the last message is someone else's then, and an answer after it notifies nothing.
   - The turn's final answer, by pocketd's own rule, notifies. Its body is the answer's first line, or the error the run stopped with, or "It was stopped."
   - After pocketd's `resync`, the stream's entries in between are lost, so the store reads each followed session's transcript again, and takes the entries the stream brings meanwhile after it when they are newer.
6. **A notice shows only while the window is not focused**, with notifications on and its kind's switch on (`ShowNotice`). One that does not show is spent: it never shows later.
7. **Settings has a Notifications page** for the service signed in to, kept in `config.Settings.Notify` by service URL. Everything is on by default, and the page has a switch for all notifications and one for each kind:
   - asks and questions;
   - tool approvals;
   - pull requests, plans and lessons;
   - finished runs.

   A lesson joins the pull requests and plans: like them, it is a thing a thread or an agent proposes for the owner to decide.
8. **A click opens what the notification is about.** Its id names the service and the target. A click on one of another service opens nothing. One clicked before the kept sign-in connected opens once it has, in place of the session shown last. The window comes up, out of the Dock or the taskbar, and the Inbox scrolls its card into view.
9. **The app's icon shows the Inbox's count** (`InboxCount`), the number the phone's Inbox tab shows up to 99. Signed out, or with the window closed, it shows none. MyGo v0.10.0 shows it on macOS and Linux alone (`BadgesIcon`); on Windows it keeps the count in memory, so the Notifications page promises the icon's count only where it shows.
10. **iOS shows no notification and has no Notifications page** (`Notifies`). A suspended app needs push to notify, which is out of scope. The badge is set there too, for when MyGo supports it.

## Consequences

- The owner hears of each thing once, in the background, and a click takes them to it.
- A finished run notifies only for a message sent from this desktop. One sent from the phone, a scheduled message or pocketd's own message does not, since pocketd marks none of them as the owner's.
- A run that writes no assistant entry, such as one that fails before any answer, notifies nothing. Neither does a run that ends after the app started again, since the wait for it lives in memory, nor one whose transcript could not be read after a resync.
- A thing that began to wait while the app was closed does not notify. The badge counts it.
- Keys are pocketd's ids. If the two databases are restored from an older backup, a reused id may count as seen: such a thing then goes unnotified, which is better than notified twice.
- `settings.json` is written each time a new thing waits.
- The system keeps a notification after its thing was handled elsewhere: nothing withdraws it.
- Nothing has shown a real notification on macOS or Linux yet. On macOS, notifications need the packaged app (`make app`).

## Alternatives considered

- **The session's change from running to idle as the run's end:** a short run may never show as running, so the notification would come before the answer, or not at all.
- **Keys with the thing's creation time, against reused ids:** an event and a list may carry the same time at another precision, which would notify twice.
- **Keys in memory alone:** a question missing from the first load while pocketd restarts its task, then back with the same id (rule 61), would notify again after the app started again.
- **Notify what waits at the first load:** each start would bring a burst of old notifications.
- **Notifications sent by pocketd, through a push service:** a protocol change and a service of its own, out of the issue's scope.
- **Switches for the whole device:** the per-device marks of the sidebar, its seen marks and its pins, are kept by service URL, and so are these.

## References

- Shapes: [design/architecture.md](../design/architecture.md#desktop-client), [plans/roadmap.md](../plans/roadmap.md#phase-2--team-work-board-and-inbox) (item 23)
- Builds on: [ADR-0010](0010-port-the-desktop-client-selectively-from-frostyard-pocket.md), [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0032](0032-let-a-supervisor-send-to-a-thread-it-delegated-through-the-outbox.md), [ADR-0040](0040-build-the-ipad-and-iphone-client-from-the-desktops-code-on-mygos-ios-target.md), [ADR-0044](0044-navigate-a-compact-window-by-a-bar-of-tabs.md), [ADR-0048](0048-put-the-supervisors-and-a-private-agents-questions-to-the-owner-inline-in-code.md)
