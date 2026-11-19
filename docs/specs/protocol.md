# Spec: pocketgo protocol, version 1

This spec defines the HTTP protocol between `pocketd` and its clients: the desktop client now, and a phone client later. The Go types in `proto/` are its machine form. `service/internal/api` serves it, and `proto.Client` calls it. A change to this spec lands in the same pull request as those three.

## Interface

### Every request

| Header | Required | Value |
| --- | --- | --- |
| `Pocket-Protocol` | yes | `1` (`proto.Version`) |
| `Authorization` | yes | `Bearer <device token>` (`pocketd device add NAME` prints one) |
| `Content-Type` | with a body | `application/json` |

Bodies are JSON objects of the `proto` type named below. Times are RFC 3339 in UTC.

### Endpoints

| Method and path | Body | Answer | `proto` types |
| --- | --- | --- | --- |
| `GET /v1/hello` | — | 200 | `Hello` |
| `GET /v1/models` | — | 200 | `[]Model` |
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
| 400 | `protocol` | `Pocket-Protocol` is missing or is not `1` |
| 401 | `unauthorized` | the token is missing, malformed, unknown or revoked |
| 400 | `bad_request` | the body or a path value breaks a rule below |
| 404 | `not_found` | no session or approval with that id |
| 409 | `conflict` | the approval is already decided, or the `requestId` was already used for a different request |
| 500 | `internal` | the service failed |

### Events

`GET /v1/events` streams Server-Sent Events. Each event has:
- `id:` the event's `seq`
- `event:` its `type`
- `data:` the JSON of `proto.Event`

| `type` | Payload field | Sent when |
| --- | --- | --- |
| `session` | `session` | a session is created, or its `state` changes |
| `entry` | `entry` | a transcript entry is written |
| `live` | `live` | a session's live state changes; `live` replaces the previous one whole |
| `approval` | `approval` | an approval is requested or decided |
| `resync` | — | the events after the client's `Last-Event-ID` are gone |

```
id: 1795057648000001
event: approval
data: {"seq":1795057648000001,"type":"approval","approval":{"id":"a3f0…","sessionId":2,"callId":"w1","tool":"write","arguments":{"path":"hello.txt"},"state":"pending","requestedAt":"2026-11-18T22:00:00Z"}}

```

## Rules

**Authentication**
1. The service refuses a request without `Pocket-Protocol: 1` before it checks the token.
2. A device token is `pgd_` followed by 64 hexadecimal characters. The service stores only its SHA-256, and a revoked token gets 401.

**Request bodies**
3. A request body is one JSON object. Unknown fields, trailing data and bodies over 1 MiB are `bad_request`.

**Request ids**
4. Every `POST` carries a `requestId` of 1 to 128 bytes. The service scopes it to the device: the same id from two devices is two requests.
5. A retried `POST` with the same `requestId` from the same device does nothing twice:
   - `CreateSession` answers 200 with the session the first call created, even if its `cwd` or model has gone away since.
   - `SendMessage` answers with the same `submissionId`.
   - `Abort` answers 200 without aborting again, so a retry never stops a run started after the first call.
   - `Decide` answers with the decided approval.

   A `requestId` already used for another endpoint or another target is `conflict`. The record of a request is written in the same transaction as its effect, so a request that failed leaves none.

**Creating a session**
6. `CreateSession.cwd` is an absolute path to an existing directory on the service host. The service cleans it, for example by removing a trailing `/`.
7. `CreateSession.model` is one of the models `GET /v1/models` lists.
8. `thinking` is empty, `minimal`, `low`, `medium`, `high` or `xhigh`.
9. `title` is at most 200 bytes. An empty title becomes `Session <id>`.

**Messages**
10. `SendMessage.text` is 1 to 262,144 bytes, and not only white space.

**Session state**
11. A session's `state` is:
   - `waiting` while an approval of the session is pending
   - else `running` while its live state is busy
   - else `idle`

**Approvals**
12. A call of `bash`, `write` or `edit` waits for an approval. Calls of other tools run without one.
13. An approval's `id` is derived from the task and the tool call. A call that waited when the service stopped asks for the same approval when its task runs again, and a decision made before the restart applies.
14. An approval is decided once. Deciding it again with a new `requestId` is `conflict`. `Decide.allow` is required: a body without a boolean `allow` is `bad_request`, never a denial.
14a. Aborting a session denies its pending approvals, with `decidedBy` `pocketd`. At start, the service also denies each pending approval whose task has ended.
14b. `read`, `write` and `edit` refuse a `path` inside the service's data folder, which holds the sign-ins, whatever the session's `cwd`, and through symbolic links. The refusal is an error tool result.
15. A denied call's tool result is an error that carries the owner's reason.

**Transcripts**
16. Entry ids grow in the order entries are written. `SessionDetail.entries` lists every entry, by ascending id.

**Event stream**
17. Event `seq` numbers grow across service restarts, even when the clock moves back: the service reserves them in the work database in blocks.
18. On reconnect, a client sends `Last-Event-ID: <seq>`:
   - The service sends every kept event after it.
   - If those events are gone, or `seq` is in the future, it sends a single `resync`, and the client refetches what it shows.
19. The service sends `: keepalive` comment lines every 10 seconds while the stream is idle. At each one, it checks the stream's token: a revoked device's stream ends within 10 seconds. A service that stops ends every stream first.
20. The service drops a client that falls 256 events behind; the client reconnects with `Last-Event-ID`.

## Derived artifacts

| Artifact | Derivation |
| --- | --- |
| `proto/*.go` | The types, paths and constants of this spec |
| `proto/client.go` | Calls each endpoint, and parses the event stream |
| `service/internal/api` | Serves each endpoint and enforces the rules; its tests check rules 1 to 11, 14, 14a and 17 to 19 |
| `service/internal/approvals` | Rule 12 (`NeedsApproval`) and the waiting side of rule 13 |
| `service/pig` | Rules 13, 14b, 15 and 16; `TestAWaitingApprovalSurvivesARestart` checks rule 13, and `TestPathToolsCannotReachTheDataFolder` checks rule 14b |

## References

- Rationale: [ADR-0006](../adr/0006-share-the-wire-protocol-as-go-types-in-one-module.md), [ADR-0011](../adr/0011-put-rules-in-code-not-in-model-judgment.md)
- Context: [design: system architecture](../design/architecture.md#protocol)
