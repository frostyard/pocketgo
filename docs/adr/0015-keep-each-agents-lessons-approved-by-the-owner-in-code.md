# 0015 — Keep each agent's lessons, approved by the owner, in code

- **Status:** Accepted
- **Date:** 2026-11-19

## Context

Phase 2's Done-when has the specialist's proposed lessons appear in the owner's Inbox. pi-pocket-team keeps lessons for each named agent: what the agent learned about its role, the owner's preferences for its work, or anything that spans its repositories. They are kept with `team_memory`:
- An agent adds, changes or forgets a lesson, and each of these waits for the owner's approval. A change waits as a proposal, and the lesson stays in force until the owner approves it.
- An approval holds only for the text the owner saw.
- The limits are 300 characters per lesson, 40 lessons in force per agent, and 20 waiting per writer for one agent.
- The lessons in force join the agent's instructions, in every session of that agent.

pi-pocket-team keeps these rules in TypeScript, and the lessons in a Durable document. pocketd keeps work records in its own tables (ADR-0008), puts every rule it can state in code (ADR-0011), and offers the team tools by role (ADR-0014). The owner is the only person; every other writer is an agent's session.

## Decision

1. **A lesson is a row in the work database** (ADR-0008), with:
   - the agent it is for (a profile's id), its text and its state
   - the profile and session that wrote it, and the call that wrote it
   - a change that waits: new text, or forgetting it

   Its states: `pending` while it waits for the owner, `active` once it is in force, and `discarded` or `forgotten` once it is gone. A lesson that is gone keeps its row, so a call run again finds what it did.
2. **The text is checked in code** (`service/internal/team`):
   - It is one line: white space becomes one space, and invisible characters (controls, format characters, private use, unassigned code points, variation selectors, blank letters) go, so the owner approves all of it.
   - It is 1 to 300 characters once flattened.
3. **`team_memory` is a Go tool,** offered to every session started from a profile:
   - `add` saves a lesson for the session's own agent. It waits for the owner, and at most 20 wait from one writer for one agent.
   - `update` and `remove` act on the agent's own lessons. A waiting lesson changes or goes at once. A change to a lesson in force waits as a proposal, and the lesson stays in force until the owner approves it.
   - `list` shows the agent's lessons, in force and waiting.
   - A supervisor may also `add` or `list` for another agent of the team.
   - A call is keyed by the calling session, its tool task and the call's id (ADR-0014): a call run again after a restart finds the lesson it made.
4. **The owner decides over the protocol,** from the Inbox:
   - **Approve** holds only when the request names what the owner saw: the waiting text, or that the lesson is to be forgotten. A lesson that changed since is `conflict`.
   - Approving a new lesson puts it in force, unless the agent has 40 in force already.
   - **Discard** drops a waiting lesson, or a waiting change, and the lesson in force stays.
   - **Forget** removes a lesson in force at once; the Inbox lists the lessons in force, folded, after what waits.
5. **The lessons in force join the agent's instructions,** as a section pocketd writes at the end of them:
   - in a new session of the agent
   - in every session of the agent, once a lesson in force changes, and in every session started from a profile when pocketd starts

   A delivery pass brings them, and tries again until every session has them. The section starts and ends with marker lines, lists each lesson as `[L<id>] <text>`, oldest first, and says how to keep lessons with `team_memory`. pocketd replaces only a section it wrote, one that ends the instructions, and changes nothing before it: text of an older identity that looks like a marker stays. A profile whose identity holds a marker is invalid.

## Consequences

- An agent's lessons follow it into every session, as in pi-pocket-team, and the owner sees and approves each word.
- Protocol version 5: lessons, with their events. A desktop and a pocketd of different versions refuse each other.
- A session made before this change has no `team_memory`: the owner starts a new one. Its instructions still get the section, since pocketd refreshes them.
- A change to the lessons in force changes the instructions of the agent's running sessions, which ends the model's cache of them once.
- The Inbox shows waiting lessons next to asks. The owner cannot edit a lesson's text: they discard it, and ask the agent again.
- pi-pocket-team's lessons stay in its Durable document: pocketd does not import them.

## Alternatives considered

- **Lessons as prompt text the agent keeps in a file:** an agent could change its own instructions without the owner, and nothing would hold an approval to the text shown.
- **Instructions fixed when a session starts:** a lesson approved later would reach only new sessions, and a long session would keep a forgotten lesson.
- **The owner edits a lesson's text:** the edit would be an approval of text the agent never proposed; discarding and asking again keeps one writer per text.

## References

- Shapes: [design: system architecture](../design/architecture.md#service), [specs: protocol](../specs/protocol.md), [plans: roadmap](../plans/roadmap.md)
- Builds on: [ADR-0008](0008-keep-work-records-in-the-services-own-tables.md), [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0013](0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md), [ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md)
