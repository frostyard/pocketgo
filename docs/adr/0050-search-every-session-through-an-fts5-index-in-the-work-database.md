# 0050 — Search every session through an FTS5 index in the work database

- **Status:** Accepted
- **Date:** 2026-11-21

## Context

**Only titles can be found today.** frostyard/pocketgo#99, filed by the owner on 2026-11-21, asks to find a past conversation by what was said in it:
- The sidebar's Filter sessions field matches a session's title and folder alone (`state.Sidebar`), and Recent lists the last 40 sessions (`state.RecentSessions`).
- The team VM holds about 190 sessions. Something an agent said last week can't be found without opening sessions one at a time.

**The issue fixes the shape:** a `GET /v1/search?q=…&limit=…` that searches session titles and the text of the transcripts' entries; case-insensitive matching on the words given; results newest first, naming the session, the entry, its time and a snippet with the match marked; an answer with a size limit that says when hits were left out; read only, with a protocol version change; well under a second at today's size. On the desktop: a Search view (Ctrl/Cmd Shift F, and "Search sessions…" in the command palette), a last row of the Filter sessions field that opens it, hits grouped by session, and a hit that opens its session scrolled to its entry, highlighted for a moment. On the phone, the Sessions tab's field offers the same row, and the hits open as a list. The issue leaves the index to this ADR.

**Where the text is:**
- Transcripts are PiG's, in `durable.sqlite`: each entry a JSON record, an assistant's with its thinking, its tool calls and their arguments, a tool's result with its whole output, a user message with its images inline. Entry ids are one sequence across all sessions, in the order entries are written ([ADR-0004](0004-build-the-service-on-pigs-durable-harness.md)). Only `service/pig` reads them.
- Session titles are in `work.sqlite` (`store.Sessions`, [ADR-0008](0008-keep-work-records-in-the-services-own-tables.md)).
- The two files are backed up and restored together; a key that crosses them is never a row number of one.
- A session's entries reach pocketd through the engine's events: `Engine.Watch` reports each new entry to `Server.PublishEntry`, from where the watch starts (the newest entry at `WatchAll`). An entry written while pocketd stopped, or whose event a failure lost, has no event.

**What the owner wants found** (the issue): the owner's messages, the agents' replies, reports and thread briefs, and the names of tool calls; not tool output. A thread's brief, a report, an answer and a supervisor's message reach their sessions as user entries, which pocketd placed ([ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md), [ADR-0032](0032-let-a-supervisor-send-to-a-thread-it-delegated-through-the-outbox.md)).

**The work database's driver is modernc.org/sqlite**, the pure-Go SQLite PiG uses; it carries FTS5, with the `unicode61` tokenizer.

## Decision

1. **An FTS5 index in the work database, migration 23.**
   - `search_entries` is an FTS5 table: an entry's searched text, its rowid the entry's id in `durable.sqlite`, with its session, its field (`user` or `assistant`) and its time unindexed. It is tokenized by `unicode61` with diacritics kept and the categories `L* N* Co Mn`, since by default `unicode61` drops a non-spacing mark, as a Thai vowel, from a word; the client splits a query the same way (`proto.SearchRune`: letters, numbers, private-use characters and non-spacing marks make words; everything else splits them).
   - `search_cursors` holds each session's cursor: the newest entry read into the index.
   - The index is derived data: it is read from PiG's records alone. The migration makes it empty, so the first start after it reads every transcript in.

2. **Fed from PiG's records, with the events as hints** (`search.Indexer`):
   - `Server.PublishEntry` notes the entry's session; noting never blocks the event.
   - The indexer reads a noted session's entries after its cursor from PiG, oldest first, at most 100 at once (`Engine.EntriesAfter`, `search.Page`), and keeps what it searches of them with the new cursor in one transaction (`store.IndexEntries`). A doc at or before the cursor it finds is kept already, and the cursor never moves back. No entry is skipped or kept twice, whatever events a stop or a failure lost.
   - `StartIndex` starts the indexer after `StartLaunches`; serving does not wait for it. The indexer lists the sessions itself, the most recently updated first (`store.SearchSessions`), and lists them again when that fails. It reads PiG and submits nothing. The first start with search backfills the ~190 sessions this way, in turn, a page each, so a long session holds back no other, and an entry written meanwhile waits only for its session's turn.
   - A session whose read or write fails is read again after 5 seconds, doubling with each failure in a row up to 5 minutes (`search.RetryDelay`); the other sessions go on meanwhile. `Server.Stop` stops the indexer before the engine and the stores close.
   - An answer says `indexing` while the start's pass has not listed every session, or not read one through, as the search read the index: a pass that ends during a search does not clear it.

3. **What is searched** (`search.DocOf`): a user entry's text; an agent's reply, its text, then `Tools: ` and the names of its tool calls. Not thinking, a tool's arguments or output, a system, compaction or reset entry. An entry's text is kept up to 32 KiB (`search.MaxIndexed`), cut at a character. A session's title is searched from the work database at each search.

4. **Matching** (`proto.SearchWords`, `search.Match`, `search.Holds`):
   - A query's words are its runs of letters and digits, lower case, each once, in order. A query has at most 200 bytes, 8 words and 64 characters a word, and at least one word; else `bad_request`.
   - Every word must begin a word of the entry or the title, whatever its case: `"deploy"` finds "Deployment". The index runs `"w1"* AND "w2"*`, built in code from words that hold letters and digits alone; a title is matched in Go by the same rule.

5. **The answer** (`search.Arrange`, `search.Snippet`, protocol rule 63):
   - Hits come newest first: an entry's by its time, a title's by the session's `updatedAt`, a title's before an entry's at one time.
   - At most `limit` hits (default 50, 1 to 200), and at most 5 of one session's entries, so a long session that says the word on every line leaves room for others. The index gives the 20,000 matches last written (`store.SearchScan`), in its own order with no sort, and the service chooses among them newest first by their times: an entry's time is its message's, which a message queued before another was written may have earlier. `cut` says a hit was left out by any of these.
   - A snippet is built in code: spaces run together, at most 160 characters from a word's start at most 40 characters before the first match, cut after a word, with "…" where the text goes on. Its marks are byte ranges, each a word's matched start. A text in which Go finds no word, as one the index folded differently, shows from its start, unmarked.
   - `GET /v1/search` is read only. Protocol version 27.

6. **The desktop** (rules in `desktop/internal/state`, drawn in `desktop/internal/app`):
   - **Open the Search view** with Ctrl Shift F (Cmd Shift F on a Mac), from the command palette's "Search Sessions…", the main menu, or the filter's last row. The filter's row, "Search all sessions for “…”", shows only while the filter holds a word to search (`state.SearchOffer`). It searches on a click or Enter, never as the arrow keys pass over it (`state.Searches`). In a terminal, Ctrl Shift F stays the shell's, as Ctrl N does.
   - **Hits** group by session in the order of each session's newest hit (`Data.SearchGroups`). A snippet draws only the marks it checks: whole, on characters' bounds, in order and apart (`state.SnippetSpans`). A search's answer goes through `Data.BeginSearch` and `Data.SetSearch`, and an older search's answer never undoes a newer one's.
   - **A hit** opens its session scrolled to its entry, once the transcript loaded (`state.Seek`). The target is the first row built from the entry, else from the next entry (`state.TargetRow`, `Row.Entry`). The row shows highlighted, holds for 1.2 seconds, then fades over 0.6 seconds. A title's hit opens the session where it opens.
   - **On the phone**, the Search view shows over the Sessions tab (`state.TabOf`). A hit's session goes back to the hits (`state.BackFrom`), and the hits back to Sessions.

## Consequences

- The owner finds an old session by a word one of its replies said. A search answers in well under a second: over 200,000 entries of about 60 words in 400 sessions, more than twice today's sessions, `store.SearchEntries` took 0.09 ms for a word said once, 36 ms for a word in nearly every entry, and 89 ms for a one-letter prefix (`BenchmarkSearchEntries`, on the developer's machine).
- The work database grows by the searched text and its index, about the text's size twice over. Tool output, the bulk of a transcript, stays out.
- The first start after the upgrade reads every transcript once, in the background; until it ends, an answer says `indexing`, and an old session may be missing.
- A session's text is in two files. A restore of `work.sqlite` older than `durable.sqlite` only lags: the cursors are behind, and the start reads on from them. A `durable.sqlite` restored older than `work.sqlite` leaves cursors past PiG's newest entries, and the index keeps entries PiG no longer has, as the existing rule says: back up and restore both together. Deleting the two search tables' rows makes the next start build the index again.
- Matching is by word start, not substring: "unner" does not find "runner". Diacritics are kept, so "cafe" does not find "café". SQLite folds a few letters' case differently from Go (Turkish İ); such a hit is found by the index and shows unmarked.
- A search that matches more than 20,000 entries says `cut`, and may leave out an older session that matches: more words find it.
- Every client must be rebuilt for protocol version 27.

## Alternatives considered

- **A bounded scan of PiG's records at each search:** no index to keep, but each search would read and decode every record of every session, images and tool output included, through `service/pig`. At 190 sessions that is seconds, and it grows with every run.
- **An FTS5 table in `durable.sqlite`:** next to the text, but that file is PiG's, and PiG stays in `service/pig` ([ADR-0004](0004-build-the-service-on-pigs-durable-harness.md)). pocketd's own tables live in the work database.
- **Index each entry from its event alone:** no read of PiG, but an entry written while pocketd stopped, or whose event was lost, would never be found, and the first start would find no history. The cursor makes the event a hint.
- **The trigram tokenizer, for substring matching:** "unner" would find "runner", but every three-letter run of the text would be indexed, and a query shorter than three characters finds nothing. The issue asks for words.
- **FTS5's `snippet()` and `highlight()`:** SQLite would mark the matches, but the snippet's length, its cut and its marks would be the tokenizer's rules, not code with tests, and the marks would come as characters inserted into the text.
- **Search as the owner types:** the field searches on Enter or Search. A request per key needs a debounce, a timer the desktop's tests would have to drive; the sidebar's filter already narrows by title as one types.

## References

- Shapes: [specs/protocol.md](../specs/protocol.md) (rules 1 and 63), [design/architecture.md](../design/architecture.md#service) (search), [design/architecture.md](../design/architecture.md#desktop-client)
- Built by: [plans/roadmap.md](../plans/roadmap.md) (item 23, search)
- Builds on: [ADR-0004](0004-build-the-service-on-pigs-durable-harness.md), [ADR-0008](0008-keep-work-records-in-the-services-own-tables.md), [ADR-0010](0010-port-the-desktop-client-selectively-from-frostyard-pocket.md), [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md), [ADR-0032](0032-let-a-supervisor-send-to-a-thread-it-delegated-through-the-outbox.md), [ADR-0044](0044-navigate-a-compact-window-by-a-bar-of-tabs.md)
