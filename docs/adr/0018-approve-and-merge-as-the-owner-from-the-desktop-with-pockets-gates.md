# 0018 — Approve and merge as the owner from the desktop, with Pocket's gates

- **Status:** Accepted
- **Date:** 2026-11-19

## Context

**The merge is the owner's.** The bot account that agents and pocketd use must never both approve and merge one pull request ([ADR-0017](0017-record-cross-review-verdicts-and-set-ready-for-human-in-code.md)). pocketd writes only `ready-for-human`.

**frostyard/pocket already does it.** It approves and merges as the owner, behind fail-closed gates, through a GitHub App user token ([ADR-0010](0010-port-the-desktop-client-selectively-from-frostyard-pocket.md) ports its `internal/github`):
- **The token:** the GitHub App "Pocket for bketelsen", whose client id is public, signs the owner in with the device flow. The pair is kept in the desktop keyring only, with no file fallback.
- **The gates** read the pull request with one GraphQL query, and the rules of its base. A pull request must be:
  - open and not a draft;
  - not the owner's own;
  - labelled `ready-for-human`;
  - mergeable, with every required check present and green and no review asking for changes;
  - on a base whose rules the owner cannot bypass, or whose every bypassable rule Pocket proves itself.
- **The head is bound:** the confirm click freezes the head. The approval names that head, and the merge or enqueue expects it.
- **Three mutations only:** approve, merge and enqueue, and a test holds that list.
- **`risk:high`:** the diff must be opened first, and the merge holds for that head only.
- **Not needed in pocketgo:**
  - batches;
  - an operation journal that recovers a merge cut off by a crash;
  - receipts posted to pi-pocket's Desk.

  These are the parts that need pi-pocket.

The code is in Go, with only the standard library, and its tests run against fakes.

## Decision

1. **The desktop approves and merges as the owner.** `desktop/internal/github` is frostyard/pocket's `internal/github`, ported whole with its tests. It changes only in naming PocketGo, and in pointing at frostyard/pocket's ADRs for its design:
   - ADR-0008 for the gates and the bound head;
   - ADR-0009 for check runs;
   - ADR-0010 for the App user token;
   - ADR-0011 for bases the owner can bypass.

   A change to the gates is a change there first, or a new ADR here. This ADR makes these changes, in pocketgo's copy:
   - a `risk:high` merge also holds the base commit whose diff the owner opened, at each mutation's read, in the pull request's answer and in the branch read with the rules (`CheckIntent`);
   - a preview of `risk:high` binds the merge to its acknowledged base and head, also once the label went;
   - on a bypassable base, the later read blocks on a review asking for changes, at any commit, and after the approval it must show the owner's approval at the head (`bypassGate`);
   - label names compare in any case, as GitHub's do (`hasLabel`);
   - a pull request that changes between the pages of its checks is no answer (`samePR`);
   - no credential write starts once `TokenSource.Close` began, a write counts as work before the mutex that admitted it goes, and `Close` says whether the work ended;
   - each `pull_request` rule's approvals count on their own: the most any one rule needs, never the largest count plus another rule's extra approval.
2. **The sign-in is the App's device flow,** kept in the desktop keyring under PocketGo's own service (`config.GitHubApp`), behind a lock that lets one PocketGo at a time use it (`config.LockSignIn`). The lock's folder follows the OS's own record of the user, since the keyring entry is the user's: the user database's home on Unix, with no fallback to `$HOME`, and the profile's roaming AppData folder on Windows, not `%AppData%`. The window holds the lock until it closes, and keeps it to the process's end when a credential write is still running then. Approve and merge are on only once the lock and the keyring opened. A sign-out whose keyring deletion failed is tried again, and so is a refreshed pair the keyring did not keep. The App's installations are read at each sign-in and for a kept one, and for any other owner when Check reads its pull request; a merge waits for its owner's. A kept sign-in that acts as another login now signs the window out. The desktop keeps a pair only when GitHub names the owner's login (`github.Login`) and the App's client id. A new pair is checked with `CheckApp` before it is kept. A missing keyring turns Approve + merge off; it never falls back to a file.
3. **Which pull request gets the button is the gates' to say.** The Inbox's card of a pull request that waits for review offers Check to Merge. That is a preview, which reads the gates on GitHub as the owner. The card then shows the refusal and its reason, or Approve + merge with the method and head the gates read. The button needs pocketd's backing at that very head (`state.Backed`, ADR-0017): its newest verdict there is `ready`, it is the head pocketd read, and CI was green there. GitHub's label names the pull request, not a commit, and the gates may pass a base with no required check. The confirmation reads the pull request from pocketd fresh at the click, never from the desktop's copy, which a stream that dropped may have left behind; the read is merged by the desktop's own rule, so an event newer than it wins, and a newer fetch that failed makes it fail. The merge acts on the preview the owner confirmed, and refuses one read again since; it runs only with the token generation the check read with. Open Diff holds the comparison it showed, base and head, and the whole attempt has a five-minute deadline. A `risk:high` pull request needs Open Diff first: the merge holds for the head whose diff was opened. A confirmation names the pull request, its head and the method; then `github.Client.Act` approves at that head and merges or enqueues, reading the gates again before each mutation.
4. **The link to GitHub is built, never taken:** the reference comes from the linked pull request's repository and number, as `state.PullURL` does, never from a URL pocketd sent.
5. **The rules of the flow live in `desktop/internal/state`** ([ADR-0010](0010-port-the-desktop-client-selectively-from-frostyard-pocket.md)): the sign-in's phases, which card may act, the head bound by a confirmation, and a result per pull request. The app only draws them.
6. **Not ported:** batches, the operation journal and the Desk receipts. A merge cut off by a stop leaves GitHub's own record, and the next Check to Merge reads where it stands.

## Consequences

- **Known limit:** the desktop pairs pocketd's verdict with GitHub's preview by repository, number and head commit, not by GitHub's own id of the pull request. pocketd keeps that id, and refuses a pull request that replaced the linked one (ADR-0016), but the protocol does not carry it yet. A repository renamed or replaced, whose new pull request reuses the number at the same head commit, would take the old one's backing. Carrying the id is a protocol change for a later pull request.

- The owner merges from the Inbox, as the owner, only what the gates pass: green, labelled `ready-for-human` by pocketd's recorded cross-review (ADR-0017), at the head shown.
- The bot never approves or merges through pocketgo: the desktop acts only as `github.Login`, and pocketd writes only the label.
- The App must be installed where the owner merges, with the permissions `CheckApp` reads. The desktop names an owner where it is missing.
- One sign-in per desktop user: a second PocketGo window cannot use GitHub while the first holds the lock.
- Without the journal, a crash between the approval and the merge leaves an approved, unmerged pull request, which a new Check to Merge shows.
- `desktop/internal/github`, `desktop/internal/config/github.go` and the merge views are risk paths.

## Alternatives considered

- **pocketd merges with the bot's token:** the bot would approve, through the label, and merge, which is what the rule forbids. The merge also needs the owner's own review.
- **Write new gates:** this discards 6,400 lines of tests that hold the gates fail-closed.
- **A fine-grained personal token per owner,** as frostyard/pocket's ADR-0008 first did: tokens the owner makes by hand, which frostyard/pocket's ADR-0010 replaced with the App.
- **Port batches and the journal now:** they double the port for a flow the owner runs one pull request at a time today.

## References

- Shapes: [design: system architecture](../design/architecture.md#desktop-client), [plans: roadmap](../plans/roadmap.md)
- Builds on: [ADR-0010](0010-port-the-desktop-client-selectively-from-frostyard-pocket.md), [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0016](0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md), [ADR-0017](0017-record-cross-review-verdicts-and-set-ready-for-human-in-code.md)
