# 0047 — Carry a shift's ci on a proposal to its launch

- **Status:** Accepted
- **Date:** 2026-11-22

## Context

**A shift can wait for CI in code.** `team_delegate`'s `shift` takes `ci` ([ADR-0024](0024-run-a-delegated-thread-in-rounds-as-a-shift-driven-in-code.md), decision 1). With it, done also needs CI green on each pull request the thread links. After the specialist's `STATUS: DONE`, pocketd reads CI itself, at most once every two minutes, with no model turn (decision 3). It ends the shift on green, and on red starts a round that names the failing checks.

**A proposal has no `ci`.** A proposal keeps its repository, title, task, `stop_when`, `max_rounds` and note ([ADR-0033](0033-gather-proposals-from-the-team-in-read-only-sessions-in-code.md), decision 6). `team_propose` and `team_board` take only those fields.

**So a launch never waits for CI.** A launch item takes its proposal's stop condition and rounds ([ADR-0034](0034-launch-the-owners-picks-from-the-supervisors-standing-session-in-code.md), decision 2). Its shift runs without `ci`, whatever its stop condition says.

**The cost showed on the night of 2026-11-21** (frostyard/pocketgo#92). Night-shift proposals named "CI green" in their stop conditions:
- The specialists had no tool to wait for CI. The shift's rules tell them to end their turn rather than wait, sleep or poll.
- So they ended their turns with CI still running, used up their rounds, and ended `max-rounds` or blocked.
- Their pull requests went green after the turns ended (frostyard/frostui#54).

## Decision

1. **A proposal carries `ci`**, false by default, with the same meaning as a shift's `ci`: done also needs CI green on the thread's pull requests.
   - `team_propose` `add` and `edit`, and `team_board` `edit`, take it as a field like the others (`team.ProposalFields.CI`). An add that leaves it out is false. An edit that leaves it out keeps it. An edit may name `ci` alone, and moves the rev as any change of the text does.
   - The work database keeps it on the proposal (`proposals.ci`, work database migration 21). A proposal filed before the migration reads false.
2. **A launch item's shift takes its proposal's `ci`**, with its stop condition and rounds (`store.StartLaunchItem`). The shift's brief then says that done also needs CI green, as a delegated shift's brief does (`team.ShiftRules`).
3. **The tools and the brief say when to set it.**
   - The proposal tools' `ci` field and the gather's brief (`team.ProposalBrief`) explain what `ci` does.
   - The description of `stop_when`, and of `team_delegate`'s `shift.stop`, says to name green CI in the stop condition only with `ci` true. Without `ci`, nothing waits for CI.
4. **Every reader shows it.**
   - `team_propose list` and `team_board list` mark a proposal with `ci` as `needs green CI`. `team_board read` says `ci: true` or `ci: false`.
   - `GET /v1/proposals` and the `proposal` event carry `ci`.
   - The desktop's board adds `needs green CI` to a proposal's caption.
5. **Protocol version 25.** `Proposal.ci` is new ([spec rules 58 and 59](../specs/protocol.md)). A version 24 client would show a proposal without saying that its shift waits for CI.

## Consequences

- A launched shift waits for CI as a delegated one does. Its rounds no longer run out while CI runs.
- A proposal session decides `ci` from the tools' text. A stop condition that names CI while `ci` is false still runs without the CI wait. The skill that asks for proposals should say when to set it.
- Proposals filed before the migration stay without `ci`. The supervisor sets it with `team_board edit` before a launch.
- The desktop and the service must both move to protocol version 25: a client of version 24 cannot reach a version 25 service. The iPad needs a new build too.

## Alternatives considered

- **Infer `ci` from words in `stop_when`:** a model or a pattern would decide a rule. Rules go in code from structured fields ([ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md)).
- **Always launch with `ci`:** work in an agent's home, or a grooming pass that may end with nothing to change, links no pull request. Such a shift could never be done.
- **Let the launch, not the proposal, take `ci`:** the proposing agent knows whether its work ends in a pull request. Each launch would also have to name it for each item.

## References

- Shapes: [design/architecture.md](../design/architecture.md#service), [specs/protocol.md](../specs/protocol.md) (rules 1, 55, 58 and 59)
- Built by: [plans/roadmap.md](../plans/roadmap.md) (item 15, the night shift, slice 3)
- Builds on: [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0024](0024-run-a-delegated-thread-in-rounds-as-a-shift-driven-in-code.md), [ADR-0033](0033-gather-proposals-from-the-team-in-read-only-sessions-in-code.md) (decision 6, a proposal's fields), [ADR-0034](0034-launch-the-owners-picks-from-the-supervisors-standing-session-in-code.md) (decision 2, what a launch item's thread takes)
