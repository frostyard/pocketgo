# Plan: NSGo as the only client

This plan makes the NSGo client PocketGo's only client, as [ADR-0071](../adr/0071-make-the-nsgo-client-the-only-client.md) decides: it gets what the MyGo client has, it is built for the iPad, the iPhone and macOS, MyGo is removed, and `v0.1.0` is released. It follows the [phone and tablet plan](phone-client.md), whose rules it keeps and whose MyGo build it replaces. It is PocketGo's part of the same work in FrostMail and Frostnotes ([frostyard/frostmail's plan 0017](https://github.com/frostyard/frostmail/blob/main/docs/plans/0017-nsgo-as-the-only-client.md)).

## What the NSGo client lacks

Read from `desktop/internal/app` (the MyGo client: 8,540 lines in 26 files, and 9,666 lines of tests) and `desktop/internal/nsgoclient` (about 5,000 lines with its tests) at `6d2ec7e`, on 2026-11-24, by a read-only agent. Nothing was built or run for this list. A phase checks its items again when it starts: this list orders the work, and is no record of a check. Both clients call the same rules in `desktop/internal/state`, so most items are a view over a rule that exists and has its test.

**Missing: the MyGo client shows it and the NSGo client shows nothing in its place.**

1. **Failures and loading.** A list that failed to load says so with "Try Again", and says what may be missing: the sessions, the profiles, the threads, the proposals, the schedules, the lessons, the terminals, a transcript ("This transcript may be out of date"), a search. The NSGo client reads none of `ThreadsStatus`, `ProposalsStatus`, `SchedulesStatus`, `TerminalsStatus`, and shows a transcript's error as a caption with no retry.
2. **A run's failures above the composer:** "The run failed", "Compacting the context failed", "Starting over failed" (`Live.Failure`, `Live.CompactionFailure`, `Store.StartingOver`).
3. **Lessons in force:** the Inbox's folded list of each agent's lessons, with Forget (`Data.LessonsInForce`, `proto.LessonForget`). The NSGo client shows only a lesson that waits.
4. **What waits and runs, at a glance.** The status bar's counts (sessions, asks, lessons and pull requests that wait; sessions that run), usage today and for 7 days with a tooltip by provider, pocketd's version and the device's name (`Store.Hello`), and "GitHub sign-in not saved" with Save Again from any view. The NSGo client has the Inbox's count, per-agent badges, and usage and Save Again inside Settings.
5. **New session:** the worktree's folder and branch before the request goes (`state.WorktreePreview`, sent with `Store.NextCreateID`'s id), a standing profile's note, the profiles' problems, the models' loading and its failure with Reload Models, and the refusal of a profile pocketd no longer offers.
6. **The work board's sentences:** why a thread is in its place (`threadWhy`: "A3 waits for your answer.", "X reported it done; Y reads the report."), the agent's name and where it works, the changes' count, the task line of a plan, a plan's state, revision, waits and tasks on its thread's card, the "reopened" pill, the cross-review line and the "ready-for-human" pill on a pull request, and the busy lines of a shift and a close. The confirmations name less: "Close thread?" for "Close T12 and drop P3 (title)?".
7. **Proposals and schedules:** the end chooser ("Until" a time, or "In 4 hours") with "Ends HH:MM" or the error (`state.UntilOf`); a blank field means 4 hours in the NSGo client, unsaid. "None of your schedules file goes out: …" (`Schedules().Problem`), and the empty notes.
8. **The sidebar:** the counts on Work Board (`Data.OnTheDesk`) and Terminal (`Data.LiveTerminals`), a group's role, hint and problem, a session's tag, its dimming when old and seen, "finished, not read", the filter's offer by its words (`state.SearchOffer`), and a row's menu with Pinned and Rename.
9. **A session:** the cue of a question that is skipped and "Checking whether pocketd took your answer…", the Changes view with Refresh, the workers' strip above the composer, a rename from the title, the header's usage line and state, a note and an error drawn apart from a caption, the supervisor's name on its messages, and Start Over's own body text.
10. **The Terminal place:** titles (`state.TerminalTitle`), the note of a shell that ended and its Close, reconnecting, the copy and paste hint, the refused-link toast, and the loading and failed states.
11. **Approve and merge:** why it is off (`OwnerProblem`, `MergeNote`), Check Again, the result's texts, Merge All's progress and its confirmation's exclusions, and a rebase's wait.
12. **pocketd's sign-ins and the sign-in screen:** a row's caption, the countdown, the address for another computer, the field that is missing, the reason pocketd signed the device out, and the note when the keyring would not keep the token.
13. **Search:** the hits' marked words, their field and time, the opened entry's highlight, and the empty and failed states.
14. **Zoom that fits:** `state.ZoomFit` and "Shown at N%: the window is too narrow for more"; zoom keys on the sign-in screen.
15. **A notification clicked before the kept sign-in connected** opens once connected. The NSGo client drops it.
16. **Commands:** Insert Prompt, Show or Hide the Sessions, Sign Out, Sign In to and Sign Out of GitHub, and Quit have no command; Search Sessions has no key (Ctrl+Shift+F), nor the palette's second key (Ctrl+Shift+P), nor Activity's (Ctrl+J). Stop run is listed only while a run works, where MyGo lists it dimmed with its reason. About shows no version.
17. **The Activity console:** levels, the source, the time and Copy. The NSGo client joins the lines on a page.

**Different by design, for the owner to sort** ([Open questions](#open-questions)). The NSGo client does these another way, and nothing says which is wanted:

- **The board's lanes.** MyGo groups threads by who moves them next (`Data.Board`: "Waits for you", "With the specialists", "With the supervisors", "Closed"). NSGo draws lanes by the thread's state (Starting, In progress, Blocked, Done) with a badge for who moves.
- **New session** is a dialog in MyGo and a page in NSGo. **Rename and pin** are on the row and the title in MyGo, and in the session's inspector in NSGo. **A report** folds at 4 lines in MyGo and is a collapsible of rendered Markdown in NSGo.
- **The palette** lists commands in MyGo, and commands and every session in NSGo. NSGo has an Open terminal command.
- **Plans** are on their threads' cards in MyGo and a section of their own in NSGo.

**Preferences are in 2 places, with no move.** MyGo keeps `settings.json` in the config folder (`desktop/internal/config`); NSGo keeps keys in its own storage (`storage.json` under `$XDG_DATA_HOME/org.frostyard.PocketGo`, `localStorage` in a browser). The device token and the GitHub sign-in are shared on a desktop: both clients use the same keyring entries. Nothing else is. By the code, a person who goes from the MyGo client to the NSGo one meets the sign-in form with an empty address, and the pins, the seen marks, the notification switches and the zoom start afresh; this was not run.

**Rules of a phone and a tablet.** `state` holds them with their tests: `Compact`, `TabOf`, `BackTo`, `TabBarShows`, `InboxBadge`, `HasTerminal`, `QuitsItself`, `SavesOnSuspend`, `BoardOrder`, `MinTarget`, `Notifies`, `BadgesIcon`. The NSGo client reads `InboxBadge` alone. Its narrow layout is by the window's width (the kit's shell, compact under 720), with no bar of tabs, no suspend hook and no touch size. `desktop/cmd/pocket-nsgo/nsgo.yaml` builds `linux_x64` and `web`.

**The terminal's clipboard.** The NSGo terminal view sets neither `AllowClipboardRead` nor `AllowClipboardWrite`, which NSGo denies by default, and `remoteOutput.Read` drops OSC 52 before the emulator. No test of the NSGo client holds either. MyGo's has 2 (`TestTerminalOptionsKeepTheServerOffTheDesktop`, `TestServerOutputCannotWriteTheClipboard`).

**On the web** there is no owner sign-in to GitHub and no merge, since they need a keyring: a pull request's card links to GitHub. The [shared owner merge service](shared-merge-service.md) is the proposal for it, and is not part of this plan.

**What a removal must change first.** No non-test file outside `internal/app`, `cmd/pocket` and `internal/icons` imports MyGo or frostui's root module. 3 tests of `internal/state` use frostui's test loop (`frostuitest.Loop`: `helpers_test.go`, `merge_test.go`, `mergeall_test.go`). `desktop/go.mod` lists MyGo's tool, and the Makefile, CI, the lint rules and 2 script tests name MyGo.

## Phase 1 — The rules that have no test (small)

- A test of the NSGo terminal's clipboard: output with OSC 52 from pocketd's test server leaves the clipboard as it was, as `TestServerOutputCannotWriteTheClipboard` shows for MyGo.
- The preferences move: at a start with no NSGo preferences, the NSGo client reads `settings.json` once and takes the service's address, the pins, the seen marks, the notification switches, the notified keys and the zoom. It does not change the file.
- A notification clicked before the sign-in connected opens once connected (item 15).
- **Done when:** each has its test in `desktop/internal/nsgoclient`, and a start on a MyGo client's `settings.json` signs in with no form.

## Phase 2 — What may be missing is said (medium)

- Items 1 and 2 of the list: every list's failed and loading states with Try Again, a transcript that may be out of date, and a run's failures.
- **Done when:** with pocketd's test server failing each list in turn, the NSGo client names the list and offers Try Again, and no view shows an empty state for a list it could not load.

## Phase 3 — The Inbox, the counts and New session (medium)

- Items 3, 4 and 5: lessons in force with Forget, the counts and usage and the service's version where the window always shows them, Save Again from any view, and New session's worktree preview and notes.
- **Done when:** the NSGo client's tests hold what `TestTheOwnerForgetsALessonInForce`, `TestSessionsAndInboxSnapshot`'s counts, `TestTheUsageMeterSnapshot`, `TestAnUnsavedSignInShowsInTheStatusBar` and `TestNewSessionOnARepositoryInAWorktreeSnapshot` hold for MyGo.

## Phase 4 — The board, the sidebar and a session (large)

- Items 6 to 9, in the layout the owner chooses for each difference above.
- **Done when:** each sentence of `threadWhy` shows for its case in a test, the proposals' end is stated before a launch, and a session's question, changes, workers and rename have the tests their MyGo views have.

## Phase 5 — The rest of the desktop (large)

- Items 10 to 14, 16 and 17: the Terminal place, approve and merge's texts, the sign-ins, search, zoom, the commands and keys, and the Activity console.
- The 3 tests of `internal/state` get a test loop of the repository's own in place of frostui's.
- **Done when:** this plan's list has no item left that is not built or struck by the owner, and the NSGo client's live suite passes (`make nsgo-live`).

## Phase 6 — The iPad, the iPhone and macOS (large)

- The compact window on the NSGo client, by `state`'s rules: the bar of tabs, back to a tab's view, the board's order, the touch size, no Terminal where `state.HasTerminal` says none, no Quit, and saving and the held GitHub refresh when iOS suspends the app. Tests run headless with a phone's and a tablet's environment (`nsgotest.Idiom`).
- NSGo's `macos`, `ios` and `ios_simulator` targets in `nsgo.yaml`. A build needs a Mac with Xcode: the owner's Mac runs it, as the phone plan's [Try it on a Mac](phone-client.md#try-it-on-a-mac) does for MyGo.
- **Done when:** on the owner's Mac, the NSGo client runs as a macOS app and in the iPad and iPhone simulators, signs in over HTTPS, and the owner answers an ask and sends a message in each.

## Phase 7 — MyGo is removed, and the first release (medium)

- `desktop/internal/app`, `desktop/cmd/pocket`, `desktop/internal/icons`, `desktop/mygo.json`, MyGo's tool in `desktop/go.mod`, and what names them in the Makefile, CI, the lint rules, the script tests, `AGENTS.md` and the design docs. The phone plan's MyGo parts are struck.
- **Done when:** `desktop/go.mod` names neither `github.com/egoist/mygo` nor frostui's root module, `make ci` passes, and the owner tags and promotes `v0.1.0`.

## Later / ideas

- Approve and merge in a browser: the [shared owner merge service](shared-merge-service.md).
- The Terminal on iOS: NSGo's terminal view has an iOS library. `state.HasTerminal` changes when it has run there.
- A Windows build: NSGo has the target, and PocketGo ships none.

## Open questions

- **The board's lanes:** by who moves next, as MyGo, or by the thread's state, as NSGo? The owner, by Phase 4.
- **New session, rename and pin, a report's fold, the palette's sessions, the plans' place:** MyGo's way or NSGo's, for each? The owner, by Phase 4.
- **Which of the 17 items are not wanted?** The owner strikes them; an item not struck is built.
- **NSGo's iOS and macOS targets:** whether they build and run PocketGo is not known. Phase 6's first change finds out, on the owner's Mac.

## References

- Implements: [design: system architecture, desktop client](../design/architecture.md#desktop-client)
- Decisions: [ADR-0071](../adr/0071-make-the-nsgo-client-the-only-client.md), which names the ADRs it builds on and amends (ADR-0068 and ADR-0070, the NSGo client; ADR-0041 and ADR-0044, a phone's rules; ADR-0010 and ADR-0011, the rules in `state`)
- Beside: [phone and tablet client](phone-client.md), [shared owner merge service](shared-merge-service.md), [migration gaps](../gaps.md) (G74 to G76 and G84 to G86 touch the clients)
