# 0052 — Show each session's context fill and usage, and sum the days' usage, in code

- **Status:** Accepted
- **Date:** 2026-11-21

## Context

**The data is there, and nothing shows it** (frostyard/pocketgo#101):
- A session carries `usage`, its tokens and cost summed over the session from PiG's ledger ([protocol rule 16b](../specs/protocol.md)).
- Each answer, an `assistant` entry, carries its own `usage`, its `model` and its time.
- Each model carries its `contextWindow` (rule 7a), and a standing profile its `compactAt` ([ADR-0026](0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md)).
- The desktop shows none of it. The owner cannot tell when a session nears compaction, or what a thread or a day cost.

**The owner runs two kinds of models side by side** ([ADR-0022](0022-run-any-session-on-any-sign-in-and-choose-workers-from-the-owners-list.md)): subscription models, signed in by OAuth and paid by the month, and API-key models such as DeepSeek, which cost per token. PiG prices every answer at its catalog's rates, a subscription's too (`ai.CalculateCost`), so a subscription's `cost` is an estimate no one pays.

**PiG compacts on one measure of the context.** Its estimate is the newest answer's total tokens, else the sum of its input, output, cache read and cache write, plus an estimate of the messages after it (`harness.EstimateContext`, `ai.CalculateContextTokens`). `team.ShouldCompact` compares that estimate with a standing session's `compactAt`.

**The issue's measure, input plus cache read, leaves context out.**
- A provider that caches (Anthropic) counts the newly cached part of a prompt as cache write. A session's first request writes its whole prompt to the cache, so input plus cache read would read near 0%.
- The answer's own output is in the next request's context too.

**There is no record of usage by day.** PiG's ledger sums a session's spend by model, with no time. The work database keeps nothing of it, and a key that crosses the two databases may not be a row number of one (`AGENTS.md`). The answers themselves carry their times.

## Decision

1. **The service sums the days' usage: `GET /v1/usage?since=<day>`** ([protocol rule 63](../specs/protocol.md)).
   - It sums the `usage` of the sessions' answers, the `assistant` entries with a usage, by day in the service's time zone and by model. That zone is the one schedules read their clock times in ([ADR-0030](0030-send-scheduled-messages-from-the-owners-file-and-the-supervisors-tool-in-code.md)). An answer that names no model counts under its session's model as it is when the report is made.
   - A day begins at its first instant in that zone, after midnight on a day whose clocks skip it.
   - A usage's tokens are its total, or its parts when they are more (`proto.Usage.Tokens`): PiG's ledger sums a model's answers, so its total misses an answer whose provider reported none. A sum counts as its total each usage's tokens (`proto.Usage.Add`).
   - `since` is a day, `YYYY-MM-DD`, at most 91 days before today (`proto.MaxUsageDays`). Any other since is `bad_request`.
   - The answer, a `UsageReport`, carries the zone, `today` and `todayEnds`. It lists the days from `since` through today that have usage, oldest first, each with its models' usage by provider, then by model id.
   - It is read only, and keeps nothing in the work database. It reads PiG's transcripts through `Engine.UsageEntries`, newest first, and stops at the first answer before `since`, since a session's answers are written in the order of their times. It reads every session it keeps sums of, and every one updated since the oldest day a `since` may name: the transcript, not the session's time, says whether it has an answer since `since`, since a stop between an answer's write and the write of its session's time leaves the time behind.
   - It keeps in memory the days' sums of each session it read, through the newest entry it read. The next report reads each session on from there, with no view of its newest records, so each answer is read once while pocketd runs. A report from an earlier day reads the session again from that day. Each report drops the days older than a report can ask for, and the sessions left with none.
   - A request whose client went away reads no further session.
   - A compaction's summary counts in its session's `usage` but in no day: it is no entry.
2. **A cost is spend only from a metered provider** (`pig.meteredBy`). A provider signed in with an API key, or one of the owner's own, is charged per token. A subscription, signed in by OAuth, is not: the service sends its models' usage with a `cost` of 0 everywhere, a session's, an entry's, a live reply's and a day's. A provider with no sign-in kept now is a subscription when PiG signs in to it by OAuth, as `pocketd login` does. The days' sums keep each answer's cost as PiG priced it, and a report prices it by the provider of the model it counts under, as that provider's sign-in is then.
3. **Protocol version 27.** `GET /v1/usage` and its types are new. The service's change touches no other endpoint, but a version 26 client cannot reach a version 27 service, so the desktop must be rebuilt.
4. **The context fill is code** (`state.Fill`, `state.ContextTokens`).
   - It is the context the newest answer measured: the reply being written first, then the entries newest first. An answer measures it when its usage has tokens.
   - The measure is PiG's: the answer's total, or its input, output, cache read and cache write summed when they are more. It departs from the issue's input plus cache read, for the reasons above. The meter then reads what the compaction decision reads, but for PiG's estimate of the messages after the answer.
   - A compaction or a reset after that answer changed the context whole. The fill is then unknown until the next answer, and says `compacted` or `started over`.
   - It is set against the session's model's `contextWindow`, from `GET /v1/models`. For a profile's standing session, its `compactAt` counts too, when it is below a known window, as pocketd compacts at it only then (`team.ShouldCompact`).
5. **The meter's thresholds are code.**
   - Its limit is the standing session's `compactAt` when it has one, else the window.
   - It is marked near from 80% of the limit (`state.NearPercent`), and full at the limit or at the window. Below that, and with no limit known, it is calm.
   - The percent is of the window, rounded down. With no window known, the meter shows the tokens.
6. **Numbers are formatted in code.**
   - Tokens: as they are below 1,000, then in k, M and B, one decimal below 10 of the unit (`1.2k`, `3.4M`), none from 10 on (`12k`), rounded half up (`state.FormatTokens`).
   - Cost: in cents below $100 (`$0.42`), `<$0.01` above zero, whole dollars from $100 (`state.FormatCost`).
   - A cost shows only when it is more than zero (`state.ShowsCost`), so a subscription's usage, whose cost the service sends as 0, shows tokens alone.
7. **Where it shows.** Each view shows nothing until there is something to show:
   - **The session's header:** the meter, a short bar and its percent, colored by its level, with the exact numbers in its tooltip, and the session's tokens and cost under it. It follows the transcript's entries and the live reply, so it updates as the run goes. On a phone, the note under the title says `context 61%`.
   - **The board:** each thread card's caption adds what the thread used (`Data.ThreadUsage`), its specialist's session and its worker's: its cost, else its tokens.
   - **The desktop's status bar:** today's total and the last 7 days', each by provider in its tooltip.
   - **More on a phone:** a Usage section with the same totals, each with a line for each provider.
8. **The client fetches the report in code.**
   - It asks from 8 days before its own today, so the report covers the service's week whichever day it is there (`state.UsageSince`).
   - It fetches the report on sign-in and refresh, and on a reconnect when the last fetch failed or the report's day ended by its clock.
   - It fetches it again at a `session` event whose `usage` changed, or that follows an answer's entry that used something, whatever summary a list brought meanwhile. The session event follows the entry (rule 16b), so the entry and the summary it changed ask for one fetch.
   - It fetches it again when a session's model changed, whatever brought the change first, an event, a list, a detail or an action's answer: an answer that names no model counts under its session's model now.
   - Changes that come while a fetch runs share one more fetch after it. The answer of a fetch that a change came after is not shown; the next one is.
   - The totals are computed from the report as of the client's clock (`state.Totals`). A report whose day ended is still the newest only when nothing was used since. Today then has no usage, and the week moves on by the days that ended. So no timer waits for midnight.

## Consequences

- The owner sees, without leaving the app, how near a session is to compaction, what each session and thread used, and what today and the week cost by provider.
- The first report after a start reads every session updated in the last 92 days, newest first, down to its first answer before `since`. Later reports read only what is new, but the sums stay in memory, a map of days and models for each session read.
- The days count answers only. A compaction's summarizing request, which PiG puts in the session's ledger, is in no day, so a standing session's day can read lower than its `usage` grew.
- A clock that moves back can put an answer before an older one. The newest-first read then stops early, and a few answers count nowhere.
- A report the client keeps across a midnight moves its week on by 24-hour days, so on a daylight-saving day it can be an hour off until the next fetch. A client clock ahead of the service's shows today empty for the minutes between the two midnights.
- The week is the last 7 days, not the calendar week: on a Monday, a calendar week would read nearly as today does.
- Every client must be rebuilt for protocol version 27.

## Alternatives considered

- **A usage table in the work database, written as entries are watched:** it would key on PiG's entry ids, a row number of the other database. It would also start empty, with no days before it.
- **Show a subscription's estimated cost:** it would read as spend, and add to the API keys' real spend in the day's total.
- **The issue's measure, input plus cache read:** it leaves out what a caching provider writes to its cache and the answer's output. A session's first request on Anthropic would read near 0%.
- **The desktop sums the days from the `session` events:** it would miss what happened while it was away, and count twice what a fetch in flight also counted.
- **A timer in the desktop for the day's end:** the stream's backoff already waits on the store's clock, which the tests drive one wait at a time. Computing the totals from the clock needs no wait.

## References

- Shapes: [design/architecture.md](../design/architecture.md#desktop-client), [specs/protocol.md](../specs/protocol.md) (rules 1 and 63)
- Built by: [plans/roadmap.md](../plans/roadmap.md#phase-5--events-metrics-and-follow-ups) (Usage at a glance)
- Builds on: [ADR-0006](0006-share-the-wire-protocol-as-go-types-in-one-module.md), [ADR-0010](0010-port-the-desktop-client-selectively-from-frostyard-pocket.md), [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0022](0022-run-any-session-on-any-sign-in-and-choose-workers-from-the-owners-list.md), [ADR-0026](0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md) (`compactAt` and the window), [ADR-0030](0030-send-scheduled-messages-from-the-owners-file-and-the-supervisors-tool-in-code.md) (the service's time zone), [ADR-0044](0044-navigate-a-compact-window-by-a-bar-of-tabs.md) (More)
