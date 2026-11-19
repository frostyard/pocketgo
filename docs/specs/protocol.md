# Spec: pocketgo protocol, version 3

This spec defines the HTTP protocol between `pocketd` and its clients: the desktop client now, and a phone client later. The Go types in `proto/` are its machine form. `service/internal/api` serves it, and `proto.Client` calls it. A change to this spec lands in the same pull request as those three.

## Interface

### Every request

| Header | Required | Value |
| --- | --- | --- |
| `Pocket-Protocol` | yes | `3` (`proto.Version`) |
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
| `GET /v1/events` | — | 200 | `text/event-stream` of `Event` |

### Errors

Every non-2xx answer has an `Error` body: `{"code": "...", "message": "..."}`.

| Status | `code` | When |
| --- | --- | --- |
| 400 | `protocol` | `Pocket-Protocol` is missing or is not `3` |
| 401 | `unauthorized` | the token is missing, malformed, unknown or revoked; a failure to check it is 500 |
| 400 | `bad_request` | the body or a path value breaks a rule below |
| 404 | `not_found` | no session or approval with that id, or no route for the path |
| 405 | `bad_request` | the path exists, but not for that method |
| 409 | `conflict` | the approval is already decided, or the `requestId` was already used for a different request |
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
| `resync` | — | the events after the client's `Last-Event-ID` are gone |

```
id: 1795057648000001
event: approval
data: {"seq":1795057648000001,"type":"approval","approval":{"id":"a3f0…","sessionId":2,"callId":"w1","tool":"write","arguments":{"path":"hello.txt"},"state":"pending","requestedAt":"2026-11-18T22:00:00Z"}}

```

## Rules

**Authentication**
1. The service refuses a request without `Pocket-Protocol: 3` before it checks the token. Version 2 added the stream's `hello` (rule 18), a `session` event after each entry (rule 16b), `submissionId` on entries and the session's queue (rule 16a), and models' `thinkingLevels` (rule 7a). Version 3 added the agents' profiles (rules 21 and 22), sessions started from one (rule 7b), and a session's `profile` and `route` (rule 23). A client of one version cannot work with a service of another, so each side refuses the other's version.
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

   A `requestId` already used for another endpoint or another target is `conflict`. A request that failed leaves no record, so its `requestId` is free again. A session's conversation records the request that created it, so a retry after a failure between the two stores finds that conversation instead of creating another.

**Creating a session**
6. `CreateSession.cwd` is an absolute path to an existing directory on the service host. The service cleans it, for example by removing a trailing `/`. With a `profile`, an empty `cwd` is the profile's `home`, else its `workspace`.
7. `CreateSession.model` is one of the models `GET /v1/models` lists. With a `profile`, a zero `model` is the router's choice for it (rule 22).
7b. `CreateSession.profile` names a profile `GET /v1/profiles` lists without a `problem`. The session's agent gets the profile's identity after the service's instructions, and exactly its `tools`. A `private` profile works only in its `home`. An unknown or invalid profile, a profile naming a tool the service does not have, and a model the router cannot choose are `bad_request` ([ADR-0013](../adr/0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md)).
7a. Each `Model` lists its `thinkingLevels`, lowest first, out of `off`, `minimal`, `low`, `medium`, `high`, `xhigh` and `max`, as PiG reads them from the model's catalog entry. A model that does not think lists only `off`. `reasoning` is true when the list has a level other than `off`.
8. `thinking` is empty, for the model's default, or one of the model's `thinkingLevels`. A provider would quietly run another level in place of one the model lacks, so the service refuses it.
9. `title` is at most 200 bytes. An empty title becomes `Session <id>`.

**Messages**
10. `SendMessage.text` is 1 to 262,144 bytes, and not only white space.

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

21. `GET /v1/profiles` lists every `<id>.md` file of the profile folder, by id; other files, such as backups, are not profiles. A valid profile lists its fields, and `runs`: the model a session from it runs now, as the router chooses it. A profile that breaks a rule, or that no sign-in serves, or that names a tool the service does not have, lists a `problem`. A file that is not a valid profile lists only its `id` and `problem`. The service reads the folder at each request.
22. The router chooses every model in code. The owner's `model` comes first (route `owner`). Otherwise the profile's model, as written (route `profile`), when the service runs it. Otherwise the same model id under a provider that serves the profile's provider on a subscription, from a fixed table: `openai` to `openai-codex` (route `profile-alias`).
23. A session started from a profile carries its `profile`. Every session carries the `route` that chose its model.

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
| `service/pig` | Rules 7a, 13, 14b, 14e, 15, 16 and 16a; `TestAWaitingApprovalSurvivesARestart` checks rule 13, `TestPathToolsCannotReachTheDataFolder` checks rule 14b, `TestUserEntriesCarryTheirSubmission`, `TestAQueuedMessageStaysQueuedAfterAFailedRun`, `TestALiveStateListsABoundedQueue` and `TestAFailedReadIsTriedAgainWithoutAnotherCommit` check rule 16a, and `TestModelsListTheirThinkingLevels` checks rule 7a; `TestCommandsCannotReachPocketdsFiles`, `TestFileToolsRefuseWhatTheSandboxProtects`, `TestABashCallRunsSandboxedWithoutWaiting` and `pocketd`'s `TestServeRefusesToStartWithoutTheSandbox` check rule 14e, `TestEndToEndWithoutApprovalsOverHTTP` checks rule 12 over the protocol, and `TestAnApprovalAskedBeforeAnUpgradeStillApplies` checks that a recorded approval outlives a change of rule 12 |

## References

- Rationale: [ADR-0006](../adr/0006-share-the-wire-protocol-as-go-types-in-one-module.md), [ADR-0011](../adr/0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0012](../adr/0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md), [ADR-0013](../adr/0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md)
- Context: [design: system architecture](../design/architecture.md#protocol)
