# 0030 — Send scheduled messages from the owner's file and the supervisor's tool, in code

- **Status:** Accepted
- **Date:** 2026-11-20

## Context

**The night shift starts with a scheduled message.** In the owner's old system (pi-pocket with pi-pocket-team), a schedule sent the supervisor's session a message at 19:00. The supervisor then asked every agent for proposals, the owner picked some, and each pick ran as a shift. On 2026-11-20 the owner chose to bring the night shift to pocketgo in three slices: schedules, then gathers and proposals, then the board and its launch. They run 3 or 4 of these "night shifts" a day, not one at a fixed hour.

**pi-pocket's schedules are the behaviour to keep** (`src/server/schedules.ts`, `when.ts`):
- The grammar is the words people write: `in 30m`, `7:00`, `tomorrow 9am`, `fri 17:30`, `every 2h`, `every day 19:00`, `every weekday 8:00`, `every mon,thu 9:00`.
- Clock times are wall-clock times in a time zone. A time that daylight saving skips is the moment after the gap, and a time that comes twice is the first.
- Each time sends `[scheduled] <text>` as a follow-up, under a request key of its own, so a restart never sends it twice.
- A repeat whose last message still waits in the session's queue skips its time, and counts the skip. After a stop, a missed time goes out once, with no backfill.
- The limits: 50 schedules a session, 4,000 characters a message, and repeats at least 10 minutes apart. Pi's `schedule` tool adds, lists and cancels its own.

**The old system's lessons:**
- The rotation of gather kinds, and their limits, lived in prompt text.
- Changing a schedule meant creating it again.
- Schedules rode on durable tasks of their own: their state lived beside the conversation.

**pocketgo has the parts:**
- The supervisor keeps one standing session for good ([ADR-0026](0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md)).
- Every message pocketd owes a session is a note, filed in the transaction of its cause and placed once ([ADR-0020](0020-plan-before-building-with-the-plan-gate-and-consults-in-code.md)). It counts as sent only once `Engine.Placed` finds it in the transcript.
- The owner's own configuration is a file in the profile folder, checked in code: `workers.json` ([ADR-0022](0022-run-any-session-on-any-sign-in-and-choose-workers-from-the-owners-list.md)) and `providers.json` ([ADR-0023](0023-add-the-owners-own-model-providers-from-a-file-checked-in-code.md)).
- Timers are kept in the work database and woken in process: the shifts' wakes and the consults' deadlines.

**The owner's choices, 2026-11-20:**
- Schedules are a `schedules.json` in the profile folder, next to `workers.json` and `providers.json`, checked in code.
- The supervisor also gets a schedule tool for her own one-off reminders and repeats, as in pi-pocket.
- A schedule names a target standing session by its profile, and an action. In this slice the action is a message. The next slice adds a gather, which pocketd runs itself.
- The owner dislikes refusals nobody asked for: a rule refuses only what correctness needs.

## Decision

1. **The owner's schedules are `schedules.json` in the profile folder**, read by `profiles.LoadSchedules` through the folder pocketd pinned at start:

   ```json
   {"schedules": [
     {"id": "evening-mon", "when": "every mon 19:00", "zone": "Europe/Berlin",
      "profile": "supervisor", "message": "Run the evening gather."}
   ]}
   ```

   - Each key is read as written: another case, a key named twice or an unknown key is refused.
   - A file that is not this object goes out not at all. An entry that breaks a rule has its problem and goes out never, and the others go out. It keeps its record by its one `id` key, whatever else breaks it. The rules: its `id` (1 to 64 letters, digits, `.`, `-`, `_`, the same in any case as no other entry's), its `when`, its `zone` (an IANA zone, not Go's `Local`, or none for the service's), its `profile` (a profile id) and exactly one action.
   - The action is `message`, 1 to 4,000 characters once trimmed. A later slice adds `gather` as a second action, so an entry names one of the two.
   - An entry goes to its profile's standing session. A profile that is gone, invalid or keeps no standing session, or whose standing session nobody opened yet, misses the time, and the record says why. A profile pocketd cannot read now holds the time for a later pass.
   - A message to a standing session waits until the session takes what its profile says now, as the owner's own message does ([ADR-0026](0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md)): while the profile cannot be applied, or the session is at work while its profile's change waits.
2. **The grammar and its limits are code**, in `service/internal/schedule`: pi-pocket's `when.ts` in Go, with its tests.
   - Repeats are at least `schedule.MinRepeat` (10 minutes) apart. A time that goes out once is at most 366 days ahead. Words that are no part of a time are refused.
   - Clock times are wall-clock times in the entry's zone, else the service's: `TZ` (a zone's file read as the zone's name), else the zone `/etc/localtime` names, else Go's local zone. A record of the service's zone whose name no zone database has is read in the service's zone. A skipped time is the moment after the gap, and a time that comes twice is the first. A time on a weekday goes out on that date's night, also when a gap at midnight moves it into the next date.
   - pocketd carries Go's own zone database (`time/tzdata`), so a zone name resolves on a minimal host.
3. **pocketd sends each time once, through the outbox.** A schedules pass runs one at a time: at start, at the soonest time a schedule goes out, every minute, and after each `team_schedule` call.
   - The pass reads the owner's file and files each time that is due in one transaction (`store.FireSchedule`). That transaction checks the time is still the schedule's, files the note `[scheduled] <message>` keyed `schedule:<owner>:<n>` (the schedule's random owner, never a row number), and moves the schedule on.
   - The delivery pass places the note as every note is placed, under `outboxMu`.
   - A repeat whose last note is not placed yet skips its time, counts the skip, and keeps its count, so its next time looks at the same note. Before it skips, it looks up that note's place in the durable store under `outboxMu`, so a message placed before a stop is never taken for one that waits. A time that goes out once is never skipped.
   - A time missed while pocketd stopped goes out once, and the next is the first after now. A repeat after a span keeps its beat from the time due.
4. **The supervisor keeps her own schedules with `team_schedule`.**
   - The tool is offered to a supervisor's session by role (`roleTools`), and `service/internal/api` checks the role again. At start, a supervisor's session started before the tool gets it (`Engine.SetScheduleTool`), but a standing one, which takes it at its idle refresh; a profile that cannot be read then is tried again at each pass.
   - `add` takes a `when` and a `message`, and the schedule goes to the calling session. An add is recorded once per team call (`api.TeamCall`): run again, it answers as it did, whatever the rules say by then.
   - `list` shows the session's own schedules and, for a standing session, the owner's that go to it. `cancel` ends one of the session's own; its answer depends only on the schedule.
   - A session keeps at most 50 schedules that still go out, counted in the transaction that adds one. The owner's schedules count apart, and change only in their file.
5. **The owner's records follow their file**, read at every pass:
   - A new entry goes out first at its first time after the pass. An entry whose `when` or zone changed starts again from then and keeps its count, so its notes' keys go on. A `when` is compared as its words in lowercase: a change of case or spaces alone is none. A new message or profile is taken for the next time.
   - An entry that breaks a rule keeps its record, and goes out never while it does. A file that cannot be read keeps every record and sends none. An entry gone from the file loses its record.
6. **The owner sees them.**
   - Protocol version 19 adds `GET /v1/schedules` and the `schedules` event ([spec rule 57](../specs/protocol.md)). Each lists the owner's schedules in their file's order, then the supervisors' own: when each goes next, how often it went out and skipped, what its last time did, and why one does not go out. A list longer than 1 MiB goes as an event with no list, and the client fetches it.
   - `pocketd profiles` reports the file, entry by entry.
   - The supervisor's transcript shows each `[scheduled]` message.
   - The desktop shows no schedules yet: where they go is the owner's choice.
7. **The start order** adds `Server.StartSchedules` after `StartPulls`, after `engine.Resume`, since a pass files notes that a delivery pass submits. Serving does not wait for it. Work database migration 16 adds the `schedules` table.

## Consequences

- The owner changes a schedule by editing a file in their private configuration, and pocketd follows within a minute, with no restart and no prompt text.
- When a message goes out is code. The model only reads it, and its own reminders are bounded in code.
- A schedule to a profile whose standing session nobody opened yet misses its times until the owner opens it with New Session.
- On a host with no zone set, clock times are UTC. The owner sets `TZ` in `~/.config/pocketd.env`, or a zone on each entry.
- pocketd grows by Go's zone database, about 450 KB.
- A file that cannot be read stops all of the owner's schedules at once, and says so in `GET /v1/schedules` and `pocketd profiles`. An entry that breaks a rule stops only itself.
- The work database holds every schedule's state. It is backed up with `durable.sqlite`, as always: a note's key crosses into the durable store only as the schedule's random owner.

## Alternatives considered

- **A PiG durable task per schedule, as pi-pocket did:** each schedule's state would live in `durable.sqlite`, apart from the work database that holds the owner's records. Following the owner's file needs pocketd's own pass anyway, and pocketgo's other timers are already kept in the work database.
- **Refusing the whole file for one bad entry, as `workers.json` does:** one typo would stop every evening gather. A per-entry problem stops only that entry, and the listing names it.
- **Schedules made from the desktop:** the owner chose a file, kept with the profiles in their private configuration repository.
- **The rotation of gather kinds in the supervisor's prompt, as before:** that is model judgment over a rule ([ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md)). Each schedule names its own action.

## References

- Shapes: [design/architecture.md](../design/architecture.md#service), [specs/protocol.md](../specs/protocol.md) (rules 1 and 57)
- Built by: [plans/roadmap.md](../plans/roadmap.md) (item 15, the night shift, slice 1)
- Builds on: [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md), [ADR-0020](0020-plan-before-building-with-the-plan-gate-and-consults-in-code.md), [ADR-0022](0022-run-any-session-on-any-sign-in-and-choose-workers-from-the-owners-list.md), [ADR-0023](0023-add-the-owners-own-model-providers-from-a-file-checked-in-code.md), [ADR-0026](0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md)
