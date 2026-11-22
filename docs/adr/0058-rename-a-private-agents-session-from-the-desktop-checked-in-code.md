# 0058 — Rename a private agent's session from the desktop, checked in code

- **Status:** Accepted
- **Date:** 2026-11-22

## Context

**A session's title is set once.** `CreateSession.title` is at most 200 bytes, trimmed, and an empty one becomes `Session <id>` (protocol rule 9). Nothing changes it later. Many sessions keep the default or a long first line. A delegated thread's session takes the `team_delegate` title, a review's `ReviewTitle`, a standing session the agent's name ([ADR-0026](0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md)), and a worktree session's folder and branch are named from its title and request id when it starts ([ADR-0027](0027-start-the-owners-session-on-a-repository-in-a-new-worktree-named-in-code.md)).

**The owner asked to rename a private agent's session** from the session's header and from the sidebar ([frostyard/pocketgo#78](https://github.com/frostyard/pocketgo/issues/78)). On 2026-11-21 the owner decided: only sessions of a profile with `private: true` may be renamed. Team, thread, review and supervisor sessions may not, and neither may the owner's own sessions. The service checks this in code, against the profile as it is loaded now. Today one profile is private: the owner's personal agent, outside the team ([ADR-0042](0042-give-a-private-agent-its-own-preamble-and-no-team-in-its-sessions.md)).

**Facts the design must keep:**
- pocketd reads the profile folder at each request, so a profile may become private, or stop being private, while its sessions live ([ADR-0042](0042-give-a-private-agent-its-own-preamble-and-no-team-in-its-sessions.md)).
- Sessions pocketd starts for the team's work are recorded with `created_by` `team` (a thread's specialist, its worker, a review, a consult) or `gather` (a gather's ask). The owner's own sessions carry the device that made them, and a standing session pocketd opened for a schedule carries `schedule`.
- The sidebar lists sessions most recently updated first, and a session moves up with the `session` event after each entry (rule 16b). Search finds a session by its title as the work database has it, at the session's `updatedAt` ([ADR-0050](0050-search-every-session-through-an-fts5-index-in-the-work-database.md)).
- frostui's `SourceList` renames a row in place: F2 on the chosen row, or `StartRename`, shows a field of its title; Enter or the focus leaving keeps the name, Escape gives up, and it calls back only with a trimmed name that is neither empty nor the title.
- The issue left one question open: whether a rename counts as activity and moves the session up in Recent.

## Decision

1. **`POST /v1/sessions/{id}/title` renames a session** (`RenameSession`: `requestId` and `title`; protocol version 31, rule 68). It answers 200 with the session.
2. **Who may be renamed is code: `team.CheckRename`.** pocketd reads the session's record and its profile as loaded now (`Server.renameFacts`), and the rule refuses, in this order:
   - a session started from no profile: the owner's own;
   - a session pocketd started for the team's work (`created_by` `team` or `gather`): a thread's, a worker's, a review's, a consult's or a gather's, whatever its profile says now;
   - a session whose profile is gone or invalid now, or any session while pocketd runs without profiles;
   - a session whose profile is not private now;
   - a session whose private profile has the role `supervisor`, since the owner named supervisor sessions among those that may not be renamed.

   A refusal is `bad_request` with the rule's reason, and nothing changes. A profile pocketd cannot read now is `internal`, and a later rename tries again. A private profile's standing session may be renamed, whoever opened it, a schedule included (`created_by` `schedule`): it is the profile's own session, not the team's work. It keeps the new title, and New Session on its profile opens it with that title.
3. **The title follows the creation rule.** At most 200 bytes as sent (`proto.MaxTitle`), then trimmed. An empty title gives the default, `Session <id>` (`proto.DefaultTitle`). The body must name the title as a string: one without it, or with `null`, is `bad_request`, so a malformed request never resets a title.
4. **A rename changes the title alone.** `store.RenameSession` writes the title and records the request in one transaction. The session's id, folder, model and route stay, and so do its worktree and branch (named at creation, ADR-0027), its pins (kept by id), its thread link, its pull requests and its PiG conversation, whose creation record keeps the first title for a create's retry. pocketd publishes the session after the commit, so every signed-in device shows the new title (rule 16b).
5. **A rename is not activity.** The session's `updatedAt` stays as it was. Its `session` event carries the same `updatedAt`, so the sidebar and `GET /v1/sessions` keep the session where it was, and search finds the new title at the session's old time. This settles the issue's open question: a rename names a session, and Recent orders sessions by what their agents did.
6. **A retry does nothing twice.** A rename with a `requestId` already recorded for it answers the session as it is now and renames nothing again. The same id with another title, or for another session, is `conflict`. Since the title and the record commit together, a failed rename leaves no record, and its id is free again.
7. **`Session.renamable` tells a client where to offer it.** pocketd sets it on every copy of a session it sends (the list, the detail, its events and every answer that carries a session) when `team.CheckRename` takes the session as its profile is loaded then. A profile that read badly sets none. The desktop offers a rename only on a session that carries it, and pocketd checks the rule again at the rename.
8. **The desktop renames through `state`.** `Data.Renamable` says where the controls show, and `Store.MayRename` whether an edit may start: not while a rename of the session is on its way, whose end would drop a second title. `TitleEdit.Check` ends an edit, sending nothing, once a session event says the session may no longer be renamed. `TitleEdit` holds the header's edit: Enter or the focus leaving saves, Escape cancels, an unchanged title sends nothing, and an empty one asks for the default. `Store.Rename` checks the length (`ValidateTitle`), takes its id from `requestIDs.For("title:<id>", title)`, applies the answer through `Data.Renamed` with the fetch token it began, and keeps a failed rename's id until the session shows another title (`settleRenames`), as a model change's (ADR-0028).
   - On a desktop, a renamable session's header has a Rename button, and a double click on its title edits it in place. Its sidebar row's menu adds Rename…, which shows the session as choosing the row would, and F2 on the chosen row renames it in place (`SourceList.Rename`).
   - On a compact window ([ADR-0044](0044-navigate-a-compact-window-by-a-bar-of-tabs.md)), the top bar has the Rename button, and the field shows under it. A session row's Rename… opens the session with the field.
   - A failed rename leaves the title as pocketd last sent it and shows the error above the composer. Other sessions show no control and no greyed-out item.

## Consequences

- The owner names a private agent's sessions from the header or the sidebar, and every device shows the name at once.
- The rule is one function, so the desktop's controls and pocketd's refusal follow the same code. Code also keeps pocketd's own records of the team's work from being renamed.
- A session keeps its place in Recent when renamed. A session's worktree folder and branch keep the name its first title gave them.
- Every copy of a session costs a read of its profile. The list reads the folder once for all its sessions.
- **Known limits:**
  - A profile that becomes private, or stops being private, while pocketd runs changes `renamable` only in the next copy of its sessions a client gets. A rename from a stale copy is refused by pocketd's check, with the reason.
  - The sidebar's field cannot give the default title back, since `SourceList` drops an empty name. The header's field can.
  - PiG's record of the conversation keeps the first title. Only a retry of the session's own creation reads it.

## Alternatives considered

- **Let every session be renamed:** the owner decided on 2026-11-21 that only a private agent's may be. A thread's or a review's title names pocketd's own record of the team's work.
- **Hide the controls in the desktop alone:** a client is not a rule. A second device, or a stale copy, could still rename a team session.
- **Tell the desktop through `GET /v1/profiles`:** the desktop would repeat the rule, the team's own sessions and the supervisor's included, and could drift from pocketd's.
- **Count a rename as activity:** Recent would reorder as names change, though no agent did anything. The issue suggested not, and nothing asks for it.
- **Rename the worktree and branch too:** the agent works in that folder, and a pull request names that branch. Moving either under a running session would break both.
- **A new `forbidden` error code:** one endpoint would carry it. Start Over refuses a session that is no standing one with `bad_request` too.

## References

- Shapes: [specs: protocol rules 1, 5, 9 and 68](../specs/protocol.md), [design: system architecture](../design/architecture.md#service), [design: desktop client](../design/architecture.md#desktop-client), [plans: roadmap](../plans/roadmap.md) (item 31)
- Builds on: [ADR-0010](0010-port-the-desktop-client-selectively-from-frostyard-pocket.md), [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0013](0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md), [ADR-0026](0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md), [ADR-0027](0027-start-the-owners-session-on-a-repository-in-a-new-worktree-named-in-code.md), [ADR-0028](0028-let-the-owner-change-a-sessions-model-from-the-composer.md), [ADR-0042](0042-give-a-private-agent-its-own-preamble-and-no-team-in-its-sessions.md), [ADR-0044](0044-navigate-a-compact-window-by-a-bar-of-tabs.md), [ADR-0050](0050-search-every-session-through-an-fts5-index-in-the-work-database.md)
- Issue: [frostyard/pocketgo#78](https://github.com/frostyard/pocketgo/issues/78)
