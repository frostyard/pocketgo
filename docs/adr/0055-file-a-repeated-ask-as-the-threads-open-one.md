# 0050 — File a repeated ask as the thread's open one

- **Status:** Accepted
- **Date:** 2026-11-22

## Context

**Only a specialist's `team_message` files an ask** ([ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md)). Each ask is keyed by its report's call and its place, `<report key>:ask:<i>`. The key stops a replay of the same report, and nothing else: a later report with the same question files a new ask.

**A shift's summary round asks for every decision again** ([ADR-0024](0024-run-a-delegated-thread-in-rounds-as-a-shift-driven-in-code.md), decision 4). Its message asked for "each decision you need from the owner as an ask". A specialist that filed an ask in a `working` report, and has no answer yet, files it again in the summary.

**The owner's Inbox got the same question twice** (frostyard/pocketgo#97). On 2026-11-21 and 2026-11-22, two shift threads filed one decision twice, once in a round-1 `working` report and again in the summary report. The owner answered both asks of one, and the second ask of the other waited after its first was answered.

## Decision

1. **A report's ask that repeats an open ask of its thread files no new ask.**
   - Two questions are the same when `team.QuestionKey` makes them equal: their words joined by single spaces, the ends trimmed, in lower case. Only an `open` ask of the same thread counts; an answered or withdrawn one does not, so a question asked again after its answer is a new ask.
   - The report names the open ask in its place: the supervisor's message lists its id with the report's new asks, and the specialist's tool result says it already waited and that no new ask was filed. Its options and session stay those of the open ask.
   - The match runs in the transaction that files the report (`store.FileReport`), against the asks open then, a new ask of the same report included. The link is kept under the repeated ask's key in the requests table, so the report's call run again after a restart returns the same asks. No migration is needed.
   - This holds for every thread: a shift's, a plain one and a plan's. A plan's ask rules ([ADR-0020](0020-plan-before-building-with-the-plan-gate-and-consults-in-code.md), decisions 3 and 4) do not change: a repeat of an open ask leaves the plan with that one open ask, which holds its gate as before.
2. **The summary round's message names the thread's asks.** Read in the transaction that keeps it (`store.SummarizeShift`), it lists:
   - the asks still open, by id and question, with the line that they wait for the owner already and must not be filed again;
   - the asks answered since the shift began, by id, question and answer, with the line to act on the answers.
   - Each list names at most `team.MaxSummaryAsks` asks with their text, cut to `team.MaxSummaryQuestion` and `team.MaxSummaryAnswer` characters, then at most `team.MaxSummaryNumbers` by id, then a count. The message asks for each *new* decision as an ask.

## Consequences

- The owner's Inbox shows a decision once per thread while it waits, whichever report repeats it.
- A reworded question is still a new ask: code compares words, not meaning. The summary's list is what keeps a specialist from rewording one.
- A specialist cannot file a second ask with the same question on purpose, as for new options; it waits for the answer, or words the question differently.
- The requests table holds one more kind of row, the link of a repeated ask.

## Alternatives considered

- **Let the agent name the open ask's id:** the agent's text would decide a rule. The question's own words are enough, and need no new field in `team_message` ([ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md)).
- **Update the open ask's text with the repeat:** the owner may be reading or answering it; a text that changes under the owner's eyes could change the answer's meaning.
- **Fix the summary's text alone:** the agent may still file it again, and a plain thread repeats an ask too. Rules go in code.
- **A column on the asks table for the link:** a migration for a row the generic requests table already keeps by key.

## References

- Shapes: [design/architecture.md](../design/architecture.md#service), [specs/protocol.md](../specs/protocol.md) (rules 27 and 55)
- Builds on: [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0014](0014-delegate-in-threads-with-team-tools-in-code.md), [ADR-0020](0020-plan-before-building-with-the-plan-gate-and-consults-in-code.md), [ADR-0024](0024-run-a-delegated-thread-in-rounds-as-a-shift-driven-in-code.md)
