# 0064 — End a launch at the next 06:45 by default

- **Status:** Accepted
- **Date:** 2026-11-23

## Context

**A launch runs until one end** ([ADR-0034](0034-launch-the-owners-picks-from-the-supervisors-standing-session-in-code.md), decisions 1 and 9). The owner's `POST /v1/launches` sends an absolute end, which the desktop's end chooser computes. The supervisor's `team_launch` takes `hours` or a clock time `until`.

**The default was 4 hours.** `team_launch` with neither ended 4 hours after the call (`team.DefaultLaunchHours`). The desktop's end chooser opened on "In 4 hours".

**Launches are night shifts.** The owner launches before bed, and the work is meant to run until the morning. A launch made in the evening with the default ended in the middle of the night, so its waiting items were skipped and its shifts stopped at their deadline before morning.

## Decision

1. **The default end is the next 06:45.** The clock time is one constant, `proto.DefaultLaunchUntil` (`"06:45"`), which `team.DefaultLaunchUntil` names.
2. **`team_launch` with neither `hours` nor `until` ends at the next 06:45 in the service's zone** (`team.LaunchEnd`): today's when the call comes before 06:45, tomorrow's from 06:45 on. It is read by the one daylight saving policy of an explicit `until` (`proto.NextClockTime`). When that end is more than `team.MaxShiftHours` away, which happens only when the clock falls back between a call made from 06:45 to 07:45 and the next 06:45, the end is `team.MaxShiftHours` after the call, so the default is never refused. Explicit `hours` and `until` are unchanged, and an explicit `until` is never shortened.
3. **The tool says so.** `team_launch`'s description of `until` names the default from the same constant.
4. **The desktop's end chooser opens on "Until", with `proto.DefaultLaunchUntil` filled in**, so it shows "Ends tomorrow 06:45" in the evening. Its second segment is a number of hours, labelled from `state.LaunchChoiceHours` (4). The end is computed in the desktop's zone, as before (`state.LaunchEnd`).
5. **No wire change.** The protocol version stays: `POST /v1/launches` still takes an absolute end, and `team_launch`'s parameters are the same.

## Consequences

- A launch made in the evening runs until the morning without the owner or the supervisor naming an end.
- A launch made in the morning (after 06:45) by default runs almost a day. The supervisor names `hours` or `until` for a shorter one, and the owner picks the hours segment or types another time.
- The service's default reads the service's zone and the desktop's the desktop's. With the two in different zones, the same default names different instants, as an explicit clock time always did.
- Once a year the default ends an hour before 06:45, when the clock falls back and the call came between 06:45 and 07:45.
- The default needs a pocketd deploy and a desktop rebuild; a desktop not rebuilt still opens on 4 hours.

## Alternatives considered

- **Keep 4 hours and let each launch name its end:** every night's launch would need the same end typed or named, and one that forgot it stopped before morning.
- **A longer number of hours (for example 10):** the end would still move with the launch's start time; the morning is a clock time.
- **Read the default from the owner's profile folder:** a setting for one value that has not varied; a change of code is enough while it holds.
- **Refuse the default when it is more than a day away:** the default would fail for an hour once a year, with no one to name another end.

## References

- Shapes: [specs/protocol.md](../specs/protocol.md) (rule 59), [design/architecture.md](../design/architecture.md#service)
- Built by: [plans/roadmap.md](../plans/roadmap.md) (item 15, the night shift, slice 3)
- Builds on: [ADR-0034](0034-launch-the-owners-picks-from-the-supervisors-standing-session-in-code.md), whose decisions 1 and 9 it amends (the default end), [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md)
