# 0047 — Put the supervisor's and a private agent's questions to the owner inline, in code

- **Status:** Accepted
- **Date:** 2026-11-21

## Context

**The supervisor can only ask in prose.** Today Odrade, the supervisor, ends a turn with "Needs you: a, b or c". The owner then types a free answer, which the agent must parse in its next run. Chani, the owner's private agent ([ADR-0042](0042-give-a-private-agent-its-own-preamble-and-no-team-in-its-sessions.md)), has nothing better. frostyard/pocketgo#83, filed on 2026-11-21 at the owner's request, asks for a `question` tool: the agent puts a question inline in the session the owner talks in, with suggested options, and gets the answer back as the tool's result, in the same run.

**Asks exist, and must stay apart** ([ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md), protocol rules 29 to 30a):
- An ask belongs to a thread. A specialist files it in a report with `team_message`.
- The owner answers it in the Inbox (`GET /v1/asks`), later. The answer reaches the specialist as a message, `The owner answered A<id>…`.
- The issue wants a different noun everywhere: "question" in the tool, the protocol and the UI, never "ask". The desktop already calls asks "questions" in the Inbox's wording.

**An approval already waits for the owner inside a tool call** ([ADR-0012](0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md), protocol rules 12 to 14a):
- Its id derives from the task and the tool call (`store.ApprovalID`). A call that waited when pocketd stopped asks for the same approval when its task runs again, and finds a decision made before.
- An abort denies it once its task ended (`approvals.Manager.RetireEnded`), and so does pocketd's start.
- `approvals.NeedsApproval` selects no tool today.

**Who started a run is in PiG's records:**
- PiG names a session's current run by its first entry (`Engine.RunIdentity`, which an abort uses already).
- The submission that placed that entry carries a request key. The owner's message has a device's key, `u:<device>:<requestId>` (`api.requestKey`).
- pocketd's own messages carry keys of their own: a schedule's (`schedule:…`), a note's (`plan:…`, `launch:…`, `consult:…`), a report's or an answer's, and a supervisor's message (`team-send:…`, [ADR-0032](0032-let-a-supervisor-send-to-a-thread-it-delegated-through-the-outbox.md)).
- A message placed while a run works joins that run; it does not start one.

**The sessions:**
- A supervisor's session gets the team tools by role (`roleTools`). A private agent's session gets no team tool, and every team tool refuses it before it runs (`Server.TeamAllows`, ADR-0042).
- Odrade keeps one standing session, which takes its profile's tools at its idle refresh ([ADR-0026](0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md)).
- Profiles are code-checked config ([ADR-0013](0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md)). Only supervisor and private profiles' sessions would get the tool.

## Decision

1. **The tool.** `question({question, options?, multiple?, placeholder?})`, checked in code (`team.CheckQuestion`):
   - `question`: 1 to 500 characters, trimmed (`proto.MaxQuestion`).
   - `options`: none, or 2 to 6, each 1 to 60 characters, none twice. They are suggestions: an answer in words is always taken.
   - `multiple`: lets the owner pick more than one option; only with options.
   - `placeholder`: at most 120 characters.
   - Its result is JSON the agent reads (`team.QuestionResult`): `{"answer", "option"}`, where `option` is an index, a list of indexes for a multiple choice, or absent for an answer in words alone; or `{"skipped": true, "reason"}`, with the reason `owner`, `timeout` or `aborted`. An answer is 1 to 2,000 characters (`proto.MaxAnswer`). Options picked with no words make the answer the options' text, joined by `; `.

2. **Who gets it, in code** (`team.MayQuestion`): a session of a supervisor's or a private profile that works as no thread's specialist, review, worker, consult or gather session.
   - `service/pig` offers it in an extension of its own, after the team tools (`toolsOf`). A private agent's session gets it with no team tool.
   - The API checks the caller again with the profile as it is now (`Server.Question`, `team.CheckQuestionCaller`): every other session is refused before anything is kept. A thread's specialist is told to use asks.
   - It is no team tool: `TeamAllows` does not run before it.

3. **Nobody watching: only a run the owner's own message started may ask** (`team.CheckWatched`).
   - pocketd reads the request key of the submission that placed the run's first entry (`Engine.RunStarter`). The run may ask only when that key is a device's (`api.ownerMessage`, the `u:` prefix of `requestKey`).
   - A run started by a schedule, a note, a report, an answer or a supervisor's message, or by no submission, is refused at once with the issue's words: "nobody is watching this run; ask in prose, in your summary".
   - It is checked once, when the question is made. A call run again after a restart finds its question and is not checked again.

4. **The wait is built as an approval's** (`approvals.Questions`, beside `approvals.Manager`):
   - The question's id derives from the task and the tool call (`store.QuestionID`). It is kept in the work database, `questions`, migration 21, before the call waits.
   - A call run again after a restart finds its question. It waits again for one still waiting, announcing it again, and answers as it did for one settled.
   - The owner's answer wakes the call; the answer is its result, in the same run.
   - An abort skips it, with the reason `aborted`, once its task ended, after the approvals are retired (`Questions.RetireEnded`). pocketd's start skips one whose task ended while it stopped. So does every end of the session's run (`Server.questionsRunEnded`): an abort that a stop cut off before its task ended is finished by PiG once the tasks resume, with no request and no wait left to skip the question.
   - A wait that ends leaves no wake registered behind it.
   - `approvals.NeedsApproval` still selects no tool. The question is no approval: it carries an answer, not an allow or a deny.

5. **Timeout.** A question with no answer by its deadline is skipped, with the reason `timeout`, by pocketd, and the agent goes on.
   - The deadline is set when the question is made: now plus the profile's `questionMinutes`, else 30 minutes (`team.DefaultQuestionTimeout`, `team.QuestionTimeout`).
   - `questionMinutes` is a whole number of minutes, 1 to 1,440, checked in `service/internal/profiles`. Only a supervisor's or a private profile may name it, since only their sessions get the tool. `GET /v1/profiles` lists it.
   - A deadline that passed while pocketd stopped skips the question as its call runs again. The server's clock and timer are injected, so tests fire it.
   - The timer wakes the wait, which skips the question itself. A skip that fails is tried again, after 1 second, doubling to 30 seconds, until it holds: the run never waits past its deadline for a write that failed once.
   - An answer at or after the deadline is refused (`conflict`, `store.ErrQuestionLate`), also before the timer fired, and skips the question then: a late answer never becomes the result.

6. **One at a time.** A session has at most one waiting question. A second call while one waits is refused, and the work database holds the rule too: a unique index on the session of a waiting question.

7. **The protocol, version 25.**
   - A session carries its waiting question (`Session.question`), and its state is `waiting` while one waits (rule 11).
   - `POST /v1/sessions/{id}/question/{qid}` takes `{requestId, answer?, option?, skipped?}` and answers with the question, as rule 62 says.
   - A session event goes out at each change: asked, answered, skipped.
   - A question is settled once. Any device answers, and the first answer stands: another is `conflict`, naming who settled it. The same `requestId` answers with the question as it is.
   - `GET /v1/asks` never lists a question.

8. **The desktop** (rules in `desktop/internal/state`, drawn in `desktop/internal/app`):
   - **The composer answers the question** (the owner's decision on 2026-11-21, the issue's open question). While a question waits in the open session, the message box's hint says "Answer the question", and what the owner sends there is the free-text answer, not a message. Attaching files is off meanwhile.
   - The card sits in the transcript after the question's call: the question, its options, and Skip, with no text field of its own. An option sends at once; with `multiple`, the options are checkboxes with a Send button.
   - The session's row has a raised hand and the badge "question", in the sidebar and on the phone's Sessions tab.
   - The Inbox never lists a question among its asks. It shows a "waiting for you" line that opens the session, as an approval does.
   - The supervisor's `team_inbox` ([ADR-0046](0046-let-the-supervisor-read-the-owners-inbox-from-pocketds-records-in-code.md)) lists the same sessions, apart from the asks, read in its one snapshot: by session and profile, and when the question is skipped, never by the question's words, since a private agent's session is the owner's alone.
   - The desktop calls the specialists' asks "asks", not "questions", so the two nouns never meet.

9. **Older sessions.** A session of a supervisor's or a private profile made before the tool gets it at start (`UpgradeTeamTools`, `Engine.SetQuestionTool`). A standing one takes it with its profile at its idle refresh, so Odrade's session gets it after a restart, once idle.

## Consequences

- The supervisor and Chani get a structured answer in the same run, and the owner answers with one click or a short line.
- A run started by the owner's message waits while the question does, up to the timeout: the owner must answer, skip or abort, or wait 30 minutes.
- An owner's message that joins a run something else started does not make that run one the owner watches. Its agent puts the question in prose; the owner's next message starts a run that may ask.
- An overnight muster never hangs on a question: its run was started by a schedule, and the call is refused at once.
- Every client must be rebuilt for protocol version 25.
- The question's wait and the approval's are two copies of one pattern, in one package; a change to one rule of the wait may need the other.
- A question's options are fixed when it is made: an answer names them by index, checked against the stored question.
- A deadline is fixed at ask time: a profile's change of `questionMinutes` reaches the next question, not one that waits.
- The work database keeps every question, settled ones included, as it keeps approvals.

## Alternatives considered

- **Disable the composer while a question waits:** the issue left the choice open. The owner chose that the composer answers the question, so a free answer needs no second field.
- **An approval with an answer:** `NeedsApproval` would make every call of the tool wait for an allow or a deny before it runs. A question needs the answer as the result, not leave to run.
- **Asks for the supervisor:** an ask is a thread's, async, and goes through the Inbox. The issue keeps the two apart.
- **A global flag for the timeout (`-question-timeout`):** one value for every agent. The profile already holds each agent's settings in checked config, and only two kinds of profile use it.
- **A timeout the agent names in the call:** the agent would decide how long a run may hang on the owner. Code decides, from the owner's config.
- **"A run with any message of the owner's in it" as watched:** kinder when the owner joins a running muster. The issue and the owner's decision name the run's start; PiG names a run by its first entry, so the rule reads one record.

## References

- Shapes: [specs/protocol.md](../specs/protocol.md) (rules 1, 5, 11, 21, 25 and 60 to 62), [design/architecture.md](../design/architecture.md#service) (the question tool), [design/architecture.md](../design/architecture.md#desktop-client)
- Built by: [plans/roadmap.md](../plans/roadmap.md) (item 22, the question tool)
- Builds on: [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0046](0046-let-the-supervisor-read-the-owners-inbox-from-pocketds-records-in-code.md), [ADR-0012](0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md), [ADR-0013](0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md), [ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md), [ADR-0026](0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md), [ADR-0030](0030-send-scheduled-messages-from-the-owners-file-and-the-supervisors-tool-in-code.md), [ADR-0032](0032-let-a-supervisor-send-to-a-thread-it-delegated-through-the-outbox.md), [ADR-0042](0042-give-a-private-agent-its-own-preamble-and-no-team-in-its-sessions.md), [ADR-0044](0044-navigate-a-compact-window-by-a-bar-of-tabs.md)
