# Spec: pocketgo protocol, version 5

This spec defines the HTTP protocol between `pocketd` and its clients: the desktop client now, and a phone client later. The Go types in `proto/` are its machine form. `service/internal/api` serves it, and `proto.Client` calls it. A change to this spec lands in the same pull request as those three.

## Interface

### Every request

| Header | Required | Value |
| --- | --- | --- |
| `Pocket-Protocol` | yes | `5` (`proto.Version`) |
| `Authorization` | yes | `Bearer <device token>` (`pocketd device add NAME` prints one) |
| `Content-Type` | with a body | `application/json` |

Bodies are JSON objects of the `proto` type named below. Times are RFC 3339 in UTC.

### Endpoints

| Method and path | Body | Answer | `proto` types |
| --- | --- | --- | --- |
| `GET /v1/hello` | — | 200 | `Hello` |
| `GET /v1/models` | — | 200 | `[]Model` |
| `GET /v1/profiles` | — | 200 | `[]Profile`, by id |
| `GET /v1/sessions` | — | 200 | `[]Session`, most recently updated first |
| `POST /v1/sessions` | `CreateSession` | 201 new, 200 retry | `Session` |
| `GET /v1/sessions/{id}` | — | 200 | `SessionDetail` |
| `POST /v1/sessions/{id}/messages` | `SendMessage` | 200 | `SentMessage` |
| `POST /v1/sessions/{id}/abort` | `Abort` | 200 | `{}` |
| `GET /v1/approvals` | — | 200 | `[]Approval`, pending only, oldest first |
| `POST /v1/approvals/{id}` | `Decide` | 200 | `Approval` |
| `GET /v1/threads` | — | 200 | `[]Thread`, most recently changed first |
| `POST /v1/threads/{id}/close` | `CloseThread` | 200 | `Thread` |
| `GET /v1/asks` | — | 200 | `[]Ask`, open only, oldest first |
| `POST /v1/asks/{id}` | `AnswerAsk` | 200 | `Ask` |
| `GET /v1/lessons` | — | 200 | `[]Lesson`, pending and in force, oldest first |
| `POST /v1/lessons/{id}` | `DecideLesson` | 200 | `Lesson` |
| `GET /v1/events` | — | 200 | `text/event-stream` of `Event` |

### Errors

Every non-2xx answer has an `Error` body: `{"code": "...", "message": "..."}`.

| Status | `code` | When |
| --- | --- | --- |
| 400 | `protocol` | `Pocket-Protocol` is missing or is not `4` |
| 401 | `unauthorized` | the token is missing, malformed, unknown or revoked; a failure to check it is 500 |
| 400 | `bad_request` | the body or a path value breaks a rule below |
| 404 | `not_found` | no session, approval, thread or ask with that id, or no route for the path |
| 405 | `bad_request` | the path exists, but not for that method |
| 409 | `conflict` | the approval is already decided, the ask is already answered, or the `requestId` was already used for a different request |
| 500 | `internal` | the service failed |

### Events

`GET /v1/events` streams Server-Sent Events. Each event has:
- `id:` the event's `seq`
- `event:` its `type`
- `data:` the JSON of `proto.Event`

| `type` | Payload field | Sent when |
| --- | --- | --- |
| `hello` | — | the stream opens; always its first event (rule 18) |
| `session` | `session` | a session is created, its `state` changes, or an entry of it is written |
| `entry` | `entry` | a transcript entry is written |
| `live` | `live` | a session's live state changes, its queue of messages included; `live` replaces the previous one whole |
| `approval` | `approval` | an approval is requested or decided |
| `thread` | `thread` | a thread is delegated, starts, gets a report, or closes |
| `ask` | `ask` | an ask is put to the owner or answered |
| `lesson` | `lesson` | a lesson is written, changed by its agent, or decided by the owner |
| `resync` | — | the events after the client's `Last-Event-ID` are gone |

```
id: 1795057648000001
event: approval
data: {"seq":1795057648000001,"type":"approval","approval":{"id":"a3f0…","sessionId":2,"callId":"w1","tool":"write","arguments":{"path":"hello.txt"},"state":"pending","requestedAt":"2026-11-18T22:00:00Z"}}

```

## Rules

**Authentication**
1. The service refuses a request without `Pocket-Protocol: 5` before it checks the token. Version 2 added the stream's `hello` (rule 18), a `session` event after each entry (rule 16b), `submissionId` on entries and the session's queue (rule 16a), and models' `thinkingLevels` (rule 7a). Version 3 added the agents' profiles (rules 21 and 22), sessions started from one (rule 7b), and a session's `profile` and `route` (rule 23). Version 4 added threads and asks (rules 24 to 30). Version 5 added each agent's lessons (rules 31 to 35). A client of one version cannot work with a service of another, so each side refuses the other's version.
2. A device token is `pgd_` followed by 64 hexadecimal characters. The service stores only its SHA-256, and a revoked token gets 401.

**Request bodies**
3. A request body is one JSON object. Unknown fields, trailing data and bodies over 1 MiB are `bad_request`.

**Request ids**
4. Every `POST` carries a `requestId` of 1 to 128 bytes. The service scopes it to the device: the same id from two devices is two requests.
5. A retried `POST` with the same `requestId` from the same device does nothing twice:
   - `CreateSession` answers 200 with the session the first call created, even if its `cwd` or model has gone away since.
   - `SendMessage` answers with the same `submissionId`.
   - `Abort` records the run it targets (by the run's first entry) before it acts, then waits up to 15 seconds for the run to settle. A retry finishes that abort and never stops a run started after the first call. A settled abort answers 200 on every retry.
   - `Decide` answers with the decided approval.
   - `AnswerAsk` answers with the answered ask.
   - `CloseThread` answers with the thread. Its `requestId` is reserved before the close withdraws a queued brief: a close that fails with no withdrawal taken effect frees it, and one that fails after a withdrawal keeps it, so the client retries the same close with the same `requestId`, never another request. A close whose freeing of its `requestId` fails says so (`internal`, with a message that the `requestId` could not be freed yet), and the service frees it later while it runs, never one a close completed since. A restart before then leaves the `requestId` held: the same close with it still works, and another request with it is `conflict` (a known limit; the desktop never reuses a `requestId` for another request).

   A `requestId` already used for another endpoint or another target is `conflict`. Requests of one `requestId` run one at a time, on every endpoint, so of two that share it one takes effect. A close checks its `requestId` before its thread, so one used for another request is `conflict` even for a thread that does not exist. A request that failed leaves no record, so its `requestId` is free again. A session's conversation records the request that created it, so a retry after a failure between the two stores finds that conversation instead of creating another.

**Creating a session**
6. `CreateSession.cwd` is an absolute path to an existing directory on the service host. The service cleans it, for example by removing a trailing `/`. With a `profile`, an empty `cwd` is the profile's `home`, else its `workspace`.
7. `CreateSession.model` is one of the models `GET /v1/models` lists. With a `profile`, a zero `model` is the router's choice for it (rule 22).
7b. `CreateSession.profile` names a profile `GET /v1/profiles` lists without a `problem`. The session's agent gets the profile's identity after the service's instructions, and exactly its `tools`: absent for every tool, `[]` for none. A profile whose own model the router cannot run starts nothing, whatever `model` the owner names. A `private` profile works only in its `home`. An unknown or invalid profile, a profile naming a tool the service does not have, and a model the router cannot choose are `bad_request` ([ADR-0013](../adr/0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md)).
7a. Each `Model` lists its `thinkingLevels`, lowest first, out of `off`, `minimal`, `low`, `medium`, `high`, `xhigh` and `max`, as PiG reads them from the model's catalog entry. A model that does not think lists only `off`. `reasoning` is true when the list has a level other than `off`.
8. `thinking` is empty, for the model's default, or one of the model's `thinkingLevels`. A provider would quietly run another level in place of one the model lacks, so the service refuses it.
9. `title` is at most 200 bytes. An empty title becomes `Session <id>`.

**Messages**
10. `SendMessage.text` is 1 to 262,144 bytes, and not only white space. A thread's specialist session takes no message while its thread is `starting`, or `open` with its brief not yet in the specialist's transcript (`conflict`): it gets its brief first, so an abort that withdraws a queued brief never leaves the owner's message ahead of it. A message goes to a watched session: the service starts the watch first, so the message's entry reaches the event stream.

**Session state**
11. A session's `state` is:
   - `waiting` while an approval of the session is pending
   - else `running` while its live state is busy
   - else `idle`

**Approvals**
12. No tool call waits for an approval: `NeedsApproval` selects no tool ([ADR-0012](../adr/0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md)). A call of a tool it selects waits for an approval, as rules 13 to 15 describe, and so does a call that asked for one under an earlier rule.
13. An approval's `id` is derived from the task and the tool call. A call that waited when the service stopped asks for the same approval when its task runs again, and a decision made before the restart applies.
14. An approval is decided once. Deciding it again with a new `requestId` is `conflict`. `Decide.allow` is required: a body without a boolean `allow` is `bad_request`, never a denial.
14a. Once an aborted run settles, its pending approvals whose tasks ended are denied, with `decidedBy` `pocketd`; a run started since keeps its own. At start, the service also denies each pending approval whose task has ended. Approval events leave in the order the approvals changed.
14b. No file access by a tool reaches the service's data folder, which holds the sign-ins. The service checks the path the environment will open, after its own normalization (relative paths, `~`, `file://` URLs) and after resolving symbolic links, at the moment of access. The refusal is an error tool result.
14c. A tool's command gets only an allowlist of the service's environment variables (`PATH`, `HOME`, `USER`, locale, temporary and XDG folders, and Go's `GOPATH`, `GOCACHE`, `GOMODCACHE` and `GOFLAGS`), plus what the tool sets. Provider keys never reach it.
14d. `GET /v1/models` lists only the models of providers with a stored sign-in. An API key in the service's environment neither lists a model nor runs a session.
14e. Every command a tool runs goes through the sandbox. In it, the service's data folder and `/run/user` are empty, under every path a mount shows them at. The service's program and the files that decide its next start are read-only, the folders above them cannot be renamed or removed, and the command sees only its own processes. The file tools refuse the same paths, through links too, and a rename or removal refuses a folder that holds one. The service refuses to start unless a trial in the sandbox shows `bwrap` as process 1 and every hidden path empty.
15. A denied call's tool result is an error that carries the owner's reason.

**Transcripts**
16. Entry ids grow in the order entries are written. `SessionDetail.entries` lists every entry, by ascending id.
16a. A `user` entry carries the `submissionId` of the message that placed it, the one `SendMessage` answered with, both in `SessionDetail.entries` and in its `entry` event. A message sent while the session runs waits for a run: `Live.queued` lists it, with its `submissionId` and text, oldest first. It leaves the queue when a run reaches it and writes its entry, whose `entry` event comes before the `live` event without it.
   - An abort of a running session withdraws its queued messages, and they get no entry. A run that fails keeps them queued, with the session idle; an abort then does nothing, and the next message's run takes them first.
   - `Live.queued` lists at most 100 messages (`proto.MaxQueuedListed`), each with its first 4,096 bytes of text cut at a character (`MaxQueuedText`, with `cut` set), and `queuedMore` counts the newer ones it leaves out. The message's entry has all of its text.
   - `SessionDetail` reads its `live` and its `entries` at one revision: the entries through the newest one the live state saw, so a queued message is in one of them.
   - When the service fails to read a watched session, it sends none of the new entries and not the live state that follows them, and reads again within seconds.
16b. Each `entry` event is followed by a `session` event of the entry's session, with the `updatedAt` the entry set and the `usage` it added. Session events leave in the order the service read the sessions, so the last one is current. A session event the service fails to read, the usage included, is read again within seconds until it goes out; a summary with no usage never goes out in its place. So is the write of the entry's time: the entry goes out and its time is noted at once, the summary waits for the write, and never goes out with the time before the entry. A retried write sets the time the entry had, never a later one, and never moves a session back. A new session is watched and its creation event goes out even when reading the answer to `CreateSession` fails; a retry of the request answers it.

**Event stream**
17. Event `seq` numbers grow across service restarts, even when the clock moves back: the service reserves them in the work database in blocks, and never sends a number outside a reservation. When a reservation fails, the event is dropped and every stream ends. Each client resyncs, also one whose `Last-Event-ID` is the event before the dropped one. The service keeps no events across a restart: each process starts at numbers of its own, inside its first reservation, and a `Last-Event-ID` from before them resyncs. Some numbers go to no event (a process's start, a `hello`'s); they are reserved too.
18. Every stream starts with one `hello` event. A client sends the `seq` of the last event it applied, the `hello`'s included, as `Last-Event-ID` when it reconnects.
   - Without `Last-Event-ID`, `hello` carries the newest event's `seq`. A client that reconnects with it misses nothing, even when no other event came first.
   - On reconnect, a client sends `Last-Event-ID: <seq>`. `hello` carries that same `seq`. The service then sends every kept event after it.
   - If those events are gone, or `seq` is in the future, a single `resync` follows `hello`, and the client refetches what it shows. This `hello` carries a `seq` that resyncs for as long as the service runs, from before its first event or a lost one: a client that loses the stream before `resync` comes back with it and gets `resync` again. The client's own `seq` would not do when it is in the future, since the service may have given it out by then. The `resync` carries the newest `seq`, always past that boundary: right after a lost event it is a fresh number, so a client that applied it reconnects without another resync.
   - A client that comes back fetches again what had not loaded when the stream dropped, a refetch still in flight included: it is past the events that would bring what it missed.
19. The service sends `: keepalive` comment lines every 10 seconds while the stream is idle. At each one, it checks the stream's token: a revoked device's stream ends within 10 seconds. A service that stops ends every stream first. Each write has a 10-second deadline, so a client that stops reading cannot hold its stream open.
20. The service drops a client that falls 256 events behind; the client reconnects with `Last-Event-ID`.

**Profiles**

21. `GET /v1/profiles` lists every `<id>.md` file of the profile folder, by id; other files, such as backups, are not profiles. The service reads a profile only as a regular file in that folder with no other name, never through a symbolic link or a hard link, and never waits on a named pipe. The frontmatter is one YAML document. A valid profile lists its fields; `cwd`, the folder a session from it starts in by default (its `home`, else its workspace); and `runs`, the model a session from it runs now, as the router chooses it. A profile that breaks a rule, or that no sign-in serves, or that names a tool the service does not have, or that is private with a home that is not a folder, lists a `problem`. A file that is not a valid profile lists only its `id` and `problem`. The service reads the folder at each request.
22. The router chooses every model in code. The owner's `model` comes first (route `owner`). Otherwise the profile's model, as written (route `profile`), when the service runs it. Otherwise the same model id under a provider that serves the profile's provider on a subscription, from a fixed table: `openai` to `openai-codex` (route `profile-alias`).
23. A session started from a profile carries its `profile`. Every session carries the `route` that chose its model; one created before profiles carries `owner`. A retry of a create whose session was made but not recorded records the session as it was made, not as a new routing would choose (rule 5).

**Threads and asks** ([ADR-0014](../adr/0014-delegate-in-threads-with-team-tools-in-code.md))

24. A thread is a piece of work a supervisor's session delegated to a specialist with `team_delegate`. Its `state` is `starting` while the service sets it up (with its `session` set once the specialist's session exists), `open` once the specialist has its brief, `done` or `blocked` after a report says so, and `closed`. A thread that could not start is `closed`, with why in its `report`, at most 2,000 characters.
25. The team tools go by role, in code. A session started from a `supervisor` profile gets `team_delegate` and `team_close`, with its other tools; a thread's specialist session gets `team_message`; no other session gets one of these. Every session started from a profile gets `team_memory` (rule 32). Each tool refuses a caller of another role, too. A call is one tool task: its steps are keyed by the session, the task and the call's id, so a run of the task again after a restart finds what it did, and a later call that reuses an id is a call of its own.
26. `team_delegate` refuses: a caller that is not a supervisor; a target that is unknown, private, a supervisor, or cannot start (rule 7b); a repository the target's profile does not name, and no repository for a target with no home or workspace; a task that is empty or over 6,000 characters, or a title over 120; and a full desk, where the desk is the threads not `closed` and its cap is `-desk-cap` (else `POCKETD_DESK_CAP`), 12 by default; a cap that is set but empty, not a number, or below 1, in the flag or the environment, stops `pocketd serve`. Otherwise:
   - The specialist's new session starts only from a profile that still passes the target rules when it starts, also after a restart, read again once its worktree is made. A session made already is finished without its profile; if its brief has not gone, its worktree is checked again as it is now, from the checkout its name was made from. The target rules check privacy and role before startability, which may look at the profile's folders.
   - With a repository, the specialist works in a git worktree at `<checkout>--<agent>-<slug>` on the branch `<agent>/<slug>`, from the checkout's `HEAD`, or on that branch when git lists it already (a lookup that could not tell makes nothing); `<slug>` is the title's letters and digits, then `-t<thread>-` and the thread's whole random owner (16 lowercase base32 characters), so no two threads, of any data folder, share a name, and a folder or branch of the name is one an earlier try of the same thread made. The checkout must be the top of its repository. Every worktree the specialist gets is checked, also a new one after the checkout's hooks ran, with its checkout checked again as the top of its repository: a folder at its path, not a link to one, of the repository, on its branch, which names a commit, registered by git at its own path (its git folder in the repository's registry, its backlink, absolute or relative, naming `.git` in the worktree's own folder (that folder, as written, compared in the sandbox, never cleaned as a string, so a copy whose `.git` is a hard link is another folder), and its `.git` a file, not a link), and not locked, with the lock file of that worktree's git folder. Every path is looked at and compared by a command in the sandbox, bound by the call's deadline, never by the service itself. A locked one, as git leaves one a stopped try did not finish, is refused; the service never removes one. The sandbox's checks (git, existence, a short read) get a tool command's environment (rule 14c), and one whose sandbox fails is an error, never an answer: a check that could not run (a probe that failed, git that did not start, a deadline that passed) says so with its cause (git's exit status comes back apart from the sandbox's, so a sandbox that failed around git, a shell that could not start git, or a signal that ended git, is never git's answer), never that the worktree is wrong, and a worktree that is wrong says so with git's word for it. git runs in the sandbox (rule 14e), the service keeps at most 4 KiB of its errors, output past 64 KiB is an error, never a cut answer, and it ends with the call's abort or after 5 minutes. Without a repository, the specialist works in its profile's `home`, else its `workspace`, which must be a folder as a command in the sandbox sees it.
   - The specialist's session starts from its profile with the router's model, titled `T<thread> · <title>`, keyed by the thread's random owner, and only while the thread, read again, is not closed, the call is not aborted, and the profile and the models, read again last, after the worktree's check and right before the session starts, still passes the target rules with the same folder; the worktree is checked again then too (of its repository, on its branch, not locked, with a git deadline of 30 seconds), and a home must still be a folder. The session and its thread's attachment commit together, before either is published, and the session is watched before its brief goes, so the brief's entry reaches the event stream. The thread takes the session, the specialist gets the brief, which the service writes, and the thread opens, in one step that no close falls within. A closed thread never gets a brief, also when its call runs again. A thread whose brief went, or may have gone (its submission found by key, or not known), never closes because a later step failed: it stays `starting`, and its first report moves it on, or a delivery pass opens it once its delegating call's task ended, without the brief again. A session an earlier run made, whose brief has not gone, has its folder checked as it is now before the brief goes: a worktree as a new one's, a home as a folder. A thread still `starting` whose delegating call's task has ended will not start: a delivery pass closes it, after a restart too, unless its specialist may have its brief. The brief is kept with the thread and counts as delivered only once its entry is in the specialist's transcript: one an abort withdrew is sent again, once its specialist's folder is checked again as it is now (an abort of the call that sends it stops the check): a folder no longer as the brief names it gets no brief, and its thread, starting or open, closes with why; a check that could not tell sends nothing and is tried again. A known limit: the sandbox's shell cannot tell a lookup that failed (an I/O error) from an entry that is not there, so a home that such an error hides reads as gone. An aborted call starts no session, or sends no brief (its first submission takes the call's context), and closes the thread; a call the service's stop cut off leaves the thread starting, and finishes when it runs again.
   - A supervisor's session names, in its instructions, the agents it can delegate to now (those the target rules accept, with somewhere to work) and the desk's cap.
27. `team_message` files a report on the caller's thread: a `status` of `working`, `done` or `blocked`; a `message` of at most 6,000 characters; and at most 4 asks, each a `question` of at most 500 characters with no `options` or 2 to 6 distinct ones of at most 40 characters. The report and its asks are filed in one transaction, once per call: the thread takes the status's state (`working` keeps it `open`) and the message as its `report`, and a run of the call again never writes over a newer report. The supervisor's session gets the message `[T<thread> · <agent name> · <status>] <message>`, followed by the ids of the asks. The message is kept with the report, and sending it is the service's: it counts as sent only once its entry is in the supervisor's transcript, a thread's reports go in the order they were filed, one that cannot go now is tried again every second until it goes, and at the next start, and the specialist is told the report is filed. A closed thread takes no report, and keeps none of its asks.
28. Closing a thread first withdraws every attempt of its brief that is still queued, so a closed thread's specialist never starts its work; a close whose withdrawal fails does not happen (`internal`), and a retry closes it. The close reserves its request id before any withdrawal, so a close refused for its request id touches no queue; a close that fails with no withdrawal taken effect frees it, and a `team_close` reserved and cut off before it committed finishes when its call runs again. `team_close` closes a thread that a session of the caller's own profile delegated, recorded with the call's key: the call run again answers as the first did, with a text that depends only on the thread. The owner closes any thread with `POST /v1/threads/{id}/close`; closing a closed thread changes nothing.
29. `AnswerAsk.answer` is 1 to 2,000 characters once trimmed (`proto.MaxAnswer`); the options are suggestions, and any answer is taken. An ask is answered once: answering it again with a new `requestId` is `conflict`. The answer reaches the asking session as the message `The owner answered A<id> ("<question>"): <answer>`, once. It is keyed by the ask's own key and a digest of the answer, never the ask's number, and counts as delivered only once the durable store's record of its key shows its entry in the transcript: a busy session's queue holds it; one an idle session holds after a failed run (PiG keeps such a queue until the next input) is withdrawn while still queued and sent again as a new input, whose run starts from its own entry; and one an abort withdrew, or the durable store does not have, is sent again under a key of its own, unless an earlier attempt still lives. One message is placed at a time, so no two callers send it twice, and a brief is sent again only under a fresh read of its thread that finds it not closed. A thread's brief whose fate the durable store cannot tell is neither sent nor its thread closed until it can. pocketd's stop ends the delivery passes before the engine closes. Sending it is the service's: an answer that cannot go now still answers 200, and the service tries again every second until it goes, and at its next start, after the tasks resume. Each report and answer is tried, so one that cannot go holds back no other; one pass runs at a time, and serving does not wait for the first.
30. Thread and ask events go out with the change that made them, in the order the rows changed. `GET /v1/asks` lists the open asks; an answered one leaves it with its `ask` event.

**Lessons** ([ADR-0015](../adr/0015-keep-each-agents-lessons-approved-by-the-owner-in-code.md))

31. A lesson is something an agent keeps across its sessions. Its `state` is `pending` while it waits for the owner, `active` once it is in force, and `discarded` or `forgotten` once it is gone. A `change` on a lesson in force is new text, or forgetting it, that waits for the owner; the lesson stays in force as it is until then. A lesson's `text` is one line: white space becomes one space, and invisible characters (controls, format characters, private use, unassigned code points, variation selectors, blank letters) go. It is 1 to 300 characters once flattened (`proto.MaxLesson`).
32. Every session started from a profile gets `team_memory`, by its role, whatever the profile's `tools`. `add` saves a `pending` lesson for the session's agent. At most 20 items wait from one writer (a profile) for one agent: its new lessons and its changes to lessons in force; replacing its own change takes no more room. `update` and `remove` act on the agent's own lessons: a `pending` one changes or is `discarded` at once, and one in force gets a `change`. `list` shows the agent's lessons with what waits. A supervisor may `add` or `list` for another agent of the team. A call is keyed by the session, its tool task and its call id (rule 25), and what it did is recorded with it: run again, it answers as it did and changes nothing, also once the profiles changed.
33. `DecideLesson.decision` is `approve`, `discard` or `forget`. An approval names what the owner saw waiting: `text`, the new lesson's or the change's, compared exactly as sent, or `forget: true`, and only an approval names either. One whose lesson no longer waits as named is `conflict`, as is a decision with nothing to decide, on a lesson that is gone, or that would put a 41st lesson of one agent in force. `approve` puts a new lesson in force, gives a lesson its new text, or forgets it; `discard` drops what waits, and a lesson in force stays; `forget` removes a lesson in force at once. A lesson is decided once per `requestId` (rule 5).
34. The lessons in force of an agent are the end of the instructions of every session started from its profile: a section that starts with the line `## Lessons pocketd keeps for you` and lists each as `- [L<id>] <text>`, oldest first. The service writes the section when a session starts, again in every session of the agent once its lessons in force change, and for every such session when the service starts; it changes nothing before the section. A profile whose identity holds that line is invalid (rule 21).
35. Lesson events go out with the change that made them, in the order the rows changed. `GET /v1/lessons` lists the lessons that are not gone; one is waiting when it is `pending`, or `active` with a `change`.

## Derived artifacts

| Artifact | Derivation |
| --- | --- |
| `proto/*.go` | The types, paths and constants of this spec |
| `proto/client.go` | Calls each endpoint, and parses the event stream |
| `service/internal/api` | Serves each endpoint and enforces the rules; its tests check rules 1 to 11, 14, 14a, 16b and 17 to 19, and `TestASessionEventThatFailsIsTriedAgain` the retry of rule 16b; `TestProfilesListsEveryProfileWithItsModel` and `TestCreateSessionFromAProfile` check rules 7b and 21 to 23 |
| `service/internal/events` | Numbers and keeps events, and gives each stream its start and its resync (rules 17, 18 and 20) |
| `service/internal/approvals` | Rule 12 (`NeedsApproval`; `TestPolicy` checks it) and the waiting side of rule 13 |
| `service/internal/profiles` | Reads and checks the profiles of rules 7b and 21 (`TestParseRefusesWhatBreaksARule`, `TestLoadReadsOnlyProfiles`) |
| `service/internal/router` | Rule 22 (`TestChoose`, `TestChooseRefuses`) |
| `service/internal/team` | The rules of rules 26, 27, 31 and 32, the lessons section of rule 34, and the texts of rules 26, 27, 29 and 32 (`TestLessonTextIsOneVisibleLine`, `TestTheLessonLimits`, `TestCheckMemoryKnowsEachAction`, `TestTheLessonsSection`, `TestCheckDelegation`, `TestCheckReport`, `TestTitleSlugAndWorktree`, `TestBrief`, `TestRoster`, `TestTexts`, `TestTheStaticRulesComeFirst`) |
| `service/internal/worktree` | The worktree of rule 26 (`TestAddMakesAndFindsTheWorktree`, `TestAddRefusesWhatItCannotUse`, `TestAHalfMadeWorktreeIsRefused`, `TestAWorktreeWithANewlineInItsPathIsFound`, `TestANewWorktreeIsCheckedAfterItsHooks`, `TestAWorktreeBesideATagOfItsNameIsFound`, `TestAWorktreeWithoutACommitIsRefused`, `TestACopiedWorktreeIsRefused`, `TestARelativeBacklinkIsRegistered`, `TestALinkedCopyIsRefused`, `TestAnOrphanedWorktreeIsRefused`) |
| `service/internal/api`, threads and asks | Rules 24 to 30: `TestDelegateStartsAThreadOnce`, `TestDelegateRefuses`, `TestADelegationThatCannotStartLeavesTheDesk`, `TestAReusedCallIDIsANewCall`, `TestAThreadStartsOnlyFromAProfileThatStillTakesIt`, `TestAStartedSpecialistNeedsNoProfile`, `TestAThreadClosedWhileItStartsGetsNoBrief`, `TestAClosedThreadNeverGetsItsBrief`, `TestABriefThatFailsLeavesTheDesk`, `TestAnAbortedDelegationStartsNoSpecialist`, `TestADelegationCutOffByAStopFinishesAfterIt`, `TestAWorktreeFromAnotherDataFolderIsNotTaken`, `TestAThreadWhoseBriefWentNeverCloses`, `TestAProfileChangedWhileTheWorktreeIsMadeStartsNothing`, `TestACloseRunAgainAnswersAsBefore`, `TestReportsReachTheSupervisorInOrder`, `TestDeliveryPassesRunOneAtATime`, `TestAnAnswerCountsOnlyOncePlaced`, `TestAStartWhoseBriefWentNeverCloses`, `TestAThreadWhoseCloseFailedClosesLater`, `TestABriefIsFollowedUntilPlaced`, `TestMessagesArePlacedUnderTheOutboxLock`, `TestAStrandedMessageIsSentAgain`, `TestAnAbortDuringTheBriefSendsNone`, `TestClosingAThreadWithdrawsItsQueuedBrief`, `TestAThreadClosesOnlyOnceItsBriefIsWithdrawn`, `TestTheSpecialistIsCheckedOnceTheLocksAreHeld`, `TestAnAnswerKeyNamesItsContent`, `TestARefusedCloseLeavesTheQueue`, `TestACloseReservesItsRequestBeforeTheQueue`, `TestAReservedTeamCloseFinishes`, `TestAFailedCloseFreesItsRequestID`, `TestRequestsOfOneKeyRunOneAtATime`, `TestAStrandedEarlierAttemptGivesWay`, `TestACloseThatChangedNothingFreesItsID`, `TestACloseThatWithdrewKeepsItsID`, `TestAWorktreeChangedBeforeTheSessionStartsNothing`, `TestTheWorktreeCheckUnderTheLocksHasADeadline`, `TestASpecialistSessionGoesOutWithItsThread`, `TestAProfileChangedDuringTheWorktreeCheckStartsNothing`, `TestStopWaitsForARunningPass`, `TestTheOutboxIsFreeBetweenReports`, `TestNoOwnerMessageBeforeTheBrief`, `TestARecoveredSessionsWorktreeIsCheckedBeforeItsBrief`, `TestTheSpecialistIsWatchedBeforeItsBrief`, `TestAReusedCloseIDIsRefusedBeforeNotFound`, `TestAnAbsentAttemptChecksTheEarlierOnes`, `TestASpecialistSessionIsKeyedByItsThreadsOwner`, `TestABriefResentAfterACloseNeverGoes`, `TestAnInconclusiveBriefLookupWaits`, `TestStopEndsTheDeliveryPasses`, `TestAHomeChangedWhileStartingStartsNothing`, `TestReportAsksAndAnswers`, `TestAFiledReportReachesTheSupervisorLater`, `TestAnAnswerThatWasNotSentIsSentLater`, `TestOneAnswerThatCannotGoHoldsNoOther`, `TestThreadAndAskEventsLeaveWithTheirChange`, `TestCloseThreads` |
| `service/internal/api`, lessons | Rules 31 to 35: `TestTeamMemoryKeepsTheAgentsLessons`, `TestTeamMemoryChecksWhoKeepsWhat`, `TestTheOwnerApprovesWhatTheyWereShown`, `TestChangesToALessonInForceWaitForTheOwner`, `TestADecisionNamesWhatItDecides`, `TestLessonsReachEverySessionInTheEnd` and `TestALessonApprovedWhileASessionStartsReachesIt`; `service/internal/store`'s `TestDecideLessonHoldsOnlyForWhatTheOwnerSaw` and `TestDiscardForgetAndRoom`; `service/pig`'s `TestSetLessonsReplacesOnlyTheLessons`, `TestTeamMemoryCallsTheTeam` and `TestALessonEndToEndOverHTTP` |
| `service/pig` | Rules 7a, 13, 14b, 14e, 15, 16 and 16a; `TestAWaitingApprovalSurvivesARestart` checks rule 13, `TestPathToolsCannotReachTheDataFolder` checks rule 14b, `TestUserEntriesCarryTheirSubmission`, `TestAQueuedMessageStaysQueuedAfterAFailedRun`, `TestALiveStateListsABoundedQueue` and `TestAFailedReadIsTriedAgainWithoutAnotherCommit` check rule 16a, and `TestModelsListTheirThinkingLevels` checks rule 7a; `TestCommandsCannotReachPocketdsFiles`, `TestFileToolsRefuseWhatTheSandboxProtects`, `TestABashCallRunsSandboxedWithoutWaiting` and `pocketd`'s `TestServeRefusesToStartWithoutTheSandbox` check rule 14e, `TestEndToEndWithoutApprovalsOverHTTP` checks rule 12 over the protocol, and `TestAnApprovalAskedBeforeAnUpgradeStillApplies` checks that a recorded approval outlives a change of rule 12; `TestTeamToolsGoByRole` checks rule 25, `TestPlacedFollowsASubmission` and `TestAStrandedMessageIsWithdrawnAndSentAgain` the placement of rules 27 and 29, `TestGitRunsInTheSandbox`, `TestAWorktreeIsMadeInTheSandbox`, `TestGitKeepsBoundedOutput`, `TestTheSandboxChecksGetNoServiceEnvironment` and `TestABrokenSandboxIsNoAnswer` the git and checks of rule 26, and `TestDelegationEndToEndOverHTTP` rules 24 to 30 over the protocol |

## References

- Rationale: [ADR-0006](../adr/0006-share-the-wire-protocol-as-go-types-in-one-module.md), [ADR-0011](../adr/0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0012](../adr/0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md), [ADR-0013](../adr/0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md), [ADR-0014](../adr/0014-delegate-in-threads-with-team-tools-in-code.md), [ADR-0015](../adr/0015-keep-each-agents-lessons-approved-by-the-owner-in-code.md)
- Context: [design: system architecture](../design/architecture.md#protocol)
