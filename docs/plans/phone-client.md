# Plan: Phone and tablet client

This plan takes the desktop client to the iPad and the iPhone on MyGo's iOS target, as one app with the desktop. It grows out of the roadmap's [Later / ideas](roadmap.md#later--ideas) item and builds on the data layer that ADR-0010 keeps apart from the views ([architecture: desktop client](../design/architecture.md#desktop-client)). The findings below come from an investigation on 2026-11-21, at main 373babe. [ADR-0040](../adr/0040-build-the-ipad-and-iphone-client-from-the-desktops-code-on-mygos-ios-target.md) records the decision. Phase 2 was built on 2026-11-21, and Phase 3 on 2026-11-21 ([ADR-0044](../adr/0044-navigate-a-compact-window-by-a-bar-of-tabs.md)). Nothing has been linked or run on iOS yet: that needs the owner's Mac ([Try it on a Mac](#try-it-on-a-mac)). [ADR-0071](../adr/0071-make-the-nsgo-client-the-only-client.md) builds the iPad and the iPhone on NSGo's targets in place of MyGo's, and keeps this plan's rules in `state` ([NSGo as the only client](nsgo-only-client.md), Phase 6).

## What the investigation found (2026-11-21)

### What carries over

MyGo runs the same Go app on iPhones and iPads: native UI drawn with Metal, a Swift host, and `mygo build -platform ios`. frostui is plain Go over MyGo's `ui`. These are the non-test lines in `desktop/`:

| Package | Lines | On iOS |
| --- | --- | --- |
| `internal/state` | 10,452 | Builds for `ios/arm64` unchanged |
| `internal/github` | 4,872 | Builds unchanged |
| `internal/config`, `internal/icons` | 638 | Build unchanged. The device token goes in the iOS keychain through `mygo.Secrets`, which the Swift host implements |
| `internal/app` and frostui | 4,851 | Type-check for `ios/arm64` with no error. The layout fits an iPad, not an iPhone |
| `cmd/pocket` | 248 | Links only with cgo and Xcode's clang, on a Mac |

About 76% of the client imports no UI and carries over unchanged, and `proto` is shared too. The rest is the same code, with layout work for the iPhone.

How it was checked:

- In `desktop`, `GOOS=ios GOARCH=arm64 CGO_ENABLED=0 go build ./internal/state ./internal/config ./internal/github ./internal/icons ../proto/...` passes on Linux.
- `internal/app` and `cmd/pocket` cannot build for iOS without cgo, which purego and MyGo's phone glue need. They were type-checked instead, with `golang.org/x/tools/go/packages` under `GOOS=ios GOARCH=arm64`. There was no error in `app`, `state`, `config`, `github`, `proto` or frostui, and `cmd/pocket` failed only on linking.
- The view tests were drawn at 820×1180 (iPad portrait) and at 390×844 (iPhone), with the tester's size changed locally.

### The views at iOS sizes

- **iPad portrait (820×1180):** every view test passes, and the sidebar, the Inbox, the board and the sessions look as they do on the desktop.
- **iPhone (390×844):** a session's transcript and composer read well. Below the desktop's smallest window (760×480), these break:
  - the sidebar folds away, and the Terminal place cannot be reached from it (the terminal's view test cannot find it);
  - the search field covers the sidebar button;
  - cards run past the right edge, and titles and the status bar are cut short;
  - dialogs are wider than the screen, so the Approve and Merge confirmation loses its Cancel button.

### What does not work on iOS as it is

- **The Terminal (ADR-0025):** MyGo's terminal plugin ships libghostty-vt for macOS, Linux and Windows only. Its Go bindings are stubs on iOS: `vt.Supported` is false there (`internal/vt/ffi_other.go`), so `terminal.Load` stops before it looks for a library. The Terminal on iOS needs both the bindings and the library upstream. The shell's connection is in the data layer and needs no change.
  - Found while building Phase 2: MyGo's iOS build bundles the native library each of the app's packages names (`mygo build`'s natives step, MyGo v0.10.0), and stops on libghostty-vt, which has no `ios-arm64` build. So the iOS app must leave the plugin out to build at all.
- **The GitHub sign-in lock:** `config.signInLockDir` puts the lock in Application Support only when `runtime.GOOS` is `darwin`. iOS reports `ios`, so the lock goes under `.config` in the home folder that the user database names, which is outside the app's container. If the lock cannot be taken, the GitHub sign-in does not open, and Approve and Merge stays off even with a working keychain. iOS needs a lock in the app's container, kept for the same GitHub credential.
- **The background:** iOS suspends an app it does not show. The event stream drops, and `state.Link` reconnects when the app comes back. A notification while the app is closed needs pocketd to send it through APNs, with MyGo's `push` plugin on the client. pocketd does not send any.
- **Building:** Go links for iOS through Xcode's clang, so a build needs a Mac with Xcode.
  - The simulator needs no paid account.
  - Devices, TestFlight and push need an Apple developer team and a provisioning profile.
  - A free personal team can sign for the owner's own device, with limits. This was not tried with `mygo build`.
  - Linux CI can still build the UI-free packages for `ios/arm64`.
- **Attachments (ADR-0029):**
  - A pasted image works. A document copied in Files does not paste, since MyGo's phone clipboard reads only text and PNG.
  - The Files picker works for documents: MyGo copies a picked file into the app's temporary folder.
  - Drag-and-drop does not work, not even on an iPad: MyGo's phone window reports no dropped files (`DroppedFiles`).
  - Photos need MyGo's `imagepicker` plugin.

Plain HTTP is not a problem on iOS. pocketd's requests go through Go's own `net/http` (`state.DirectTransport`), not Apple's URL Loading System, so App Transport Security does not apply and needs no exception. The client's own rule, `state.NeedsHTTPWarning`, treats a `*.ts.net` name as the tailnet's on every platform. Phase 1's HTTPS is a deployment choice, not something iOS requires.

## Phase 1 — pocketd over HTTPS on the tailnet (small)

The service host serves HTTPS on port 443 now that pi-pocket is stopped. iOS does not require this (see above). Done on 2026-11-21 ([frostyard/pocketgo#68](https://github.com/frostyard/pocketgo/issues/68)). The proxy took over once the owner's shift had ended, with both databases backed up first, and the owner's desktop showed the Done when.

- [x] `tailscale serve` on the service host takes port 443, which pi-pocket held until it was stopped, and proxies `https://<host>.<tailnet>.ts.net` to pocketd on `127.0.0.1:8788`. That is the unit's default once `POCKETD_LISTEN` leaves `~/.config/pocketd.env` ([architecture: operational notes](../design/architecture.md#operational-notes)).
- [x] The desktop signs in again at the HTTPS address: the owner's did, on 2026-11-21. It keeps the token, the pins and the seen marks by the service's origin ([architecture: desktop client](../design/architecture.md#desktop-client)), so those of the old address stay behind.
- [x] **Done when:** the desktop signs in at `https://<host>.<tailnet>.ts.net` with no plain-HTTP question, its event stream and a terminal run through the proxy, and port 8788 on the tailnet address no longer answers.
  - Seen on 2026-11-21 over the protocol, from the laptop:
    - `hello` answered at protocol 24 through the proxy.
    - The event stream came over HTTP/2.
    - A shell started, ran a command and closed through the proxy.
    - Port 8788 on the tailnet address refused connections.
  - Seen in the owner's desktop on 2026-11-21: it signed in at the HTTPS address, its sessions updated live, and a Terminal tab ran a shell through the proxy.

## Phase 2 — iPad build (medium)

Built on 2026-11-21, as [ADR-0040](../adr/0040-build-the-ipad-and-iphone-client-from-the-desktops-code-on-mygos-ios-target.md) decides: one app with the desktop, from its own code ([architecture: desktop client](../design/architecture.md#desktop-client)). No layout changed.

- [x] An `ios` section in `desktop/mygo.json`: iOS 17.0 or later, the `development` distribution, and both orientations. The team ID stays out of the repository ([Try it on a Mac](#try-it-on-a-mac)).
- [x] The iOS parts of `cmd/pocket`. The keychain through `mygo.Secrets` and the system's picker for Attach files (`mygo.Dialog.Open`) need no code of their own: MyGo's phone backend serves them. The window's size, title bar and close have no say on iOS. What an iPad needed:
  - the menu has no Quit there, since iOS ends apps itself and MyGo's `App.Quit` would leave the window frozen (`state.QuitsItself`);
  - the iOS build leaves MyGo's terminal plugin out, since MyGo's iOS build cannot bundle its library (`terminal_emulator.go`, `terminal_replay.go` and the tests that drive the emulator are built `!ios`; `terminal_emulator_ios.go` holds stubs; `TestTheIOSBuildLeavesTheEmulatorOut` checks that no file built for iOS, tests included, imports the plugin or uses what only those files declare).
- [x] The GitHub sign-in lock in the app's container on iOS, kept for the same credential: `Library/Application Support/org.frostyard.PocketGo` under the home iOS gives the app. Its tests in `desktop/internal/config` (`TestTheSignInLockOnIOSIsInTheAppsContainer`, `TestTheSignInLocksFolderByPlatform`) fail without it.
- [x] The Terminal place hidden on iOS until MyGo's terminal plugin supports it, by a rule in `desktop/internal/state` with its test (`state.HasTerminal`, `TestThePlatformsTerminalAndQuit`). The window takes its platform from `app.Options.Platform`, and `TestTheIPadWindowHasNoTerminalSnapshot` draws the iPad's window at 820×1180: no Terminal in the sidebar, no way to the place, the emulator's library never loaded, and no Quit.
- [x] A portability step that builds the UI-free packages for `ios/arm64` on Linux, so a dependency or a call that iOS lacks fails CI: `make portable`, which `make ci` runs. It builds `proto`, `internal/state`, `internal/config`, `internal/github` and `internal/icons` with CGO off, and fails when the desktop's iOS app, listed as MyGo's build lists it, imports the terminal plugin. `make lint`'s `desktop-state-boundary` rule stays the check on UI imports in `state`.
- [x] Saving as iOS suspends the app ([ADR-0041](../adr/0041-save-the-window-and-hold-the-github-refresh-when-ios-suspends-the-app.md), the owner's choice of 2026-11-21), since iOS may end the app in the background without closing its window. The rule is `state.SavesOnSuspend`, and `cmd/pocket` listens to MyGo's `Power.OnSuspend` and `OnResume` on iOS alone (`suspendHooks`).
  - The window saves what `App.Close` saves (`App.Suspend`, `TestTheIPadKeepsItsWindowWhenSuspended`): the session shown, the panes' layout and the rest of the settings.
  - The GitHub sign-in starts no refresh until the app comes back, and waits up to 3 seconds for one in progress to reach the keychain (`TokenSource.Suspend`, `TestSuspendWaitsForTheRunningRefreshToBeSaved`).
  - A desktop saves as its window closes, as before (`TestTheDesktopSavesNothingWhenSuspended`).
- [ ] **Done when:** on an iPad, or the iPad simulator on the owner's Mac, PocketGo signs in to pocketd over HTTPS (Phase 1, [frostyard/pocketgo#68](https://github.com/frostyard/pocketgo/issues/68)), and the owner answers an ask and sends a message in a session.
  - Not seen yet: it needs a Mac with Xcode. On Linux, `internal/app` and `cmd/pocket` type-check for `ios/arm64` with no error but linking, and every view test passes.

### Try it on a Mac

The iPad simulator needs no Apple account. From a checkout of this repository:

1. Install Xcode from the App Store. Open it once, to accept its license and install the iOS simulator (Xcode › Settings › Components). Install the Go release that `go.work`'s `go` line names.
2. In `desktop`, run `go tool mygo doctor`. It checks Xcode, its command line tools, the iOS SDK and the simulators, and says how to get what is missing.
3. Run `go tool mygo devices` to list the simulators, then `go tool mygo dev -platform ios -device "<an iPad simulator's name>"`. It builds PocketGo for the simulator, boots it, installs the app as "PocketGo Dev" (`org.frostyard.PocketGo.dev`) and launches it, with the app's output in the terminal. A change of the Go code rebuilds and relaunches it.
   - Or build it once: `go tool mygo build -platform ios/simulator` writes `desktop/build/mygo/ios-simulator/PocketGo.app`. With an iPad simulator booted, `xcrun simctl install booted build/mygo/ios-simulator/PocketGo.app` installs it and `xcrun simctl launch booted org.frostyard.PocketGo` starts it.
4. Sign in. The simulator uses the Mac's network, so the Mac must be on the tailnet. The address is `https://<host>.<tailnet>.ts.net` (Phase 1). The token comes from `pocketd device add ipad` on the service host.
5. Answer an ask in the Inbox, and send a message in a session.

**On an iPad itself**, which needs an Apple developer team, a free personal one or the paid program:
- Turn on Developer Mode (Settings › Privacy & Security), and register the iPad with the team.
- Have Xcode make a development provisioning profile for `org.frostyard.PocketGo.dev` (Xcode › Settings › Accounts), or use the team's wildcard profile.
- Run `go tool mygo dev -platform ios -device "<the iPad's name>"`. `go tool mygo build -platform ios` makes a development `.ipa` for `org.frostyard.PocketGo` instead.

**The team ID** never enters the repository. MyGo signs with the installed provisioning profile that best fits the app's identifier, so a Mac with one team's profiles needs none, and the simulator needs no team at all. With profiles of more than one team, add `"team": "<TEAM_ID>"` (Xcode › Settings › Accounts shows it) to the `ios` section of your checkout's `desktop/mygo.json`, and keep it out of commits with `git update-index --skip-worktree desktop/mygo.json`. Undo that with `--no-skip-worktree` before pulling a change to the file.

## Phase 3 — iPhone layout (medium)

Built on 2026-11-21, as [ADR-0044](../adr/0044-navigate-a-compact-window-by-a-bar-of-tabs.md) decides: a compact window navigates by a bar of tabs, the owner's choice of 2026-11-21 between two prototypes ([architecture: desktop client](../design/architecture.md#desktop-client)). Nothing has run on an iPhone yet.

- [x] A compact layout below the desktop's smallest window, chosen by a rule in `desktop/internal/state` with its test (`state.Compact`, `TestCompactIsIOSBelowTheDesktopsSmallestWindow`): iOS, narrower than 760 DIPs at 100%.
  - The places and the sessions are reached from a bar of tabs at the bottom, drawn with MyGo's `ui/mobile`: Inbox with its badge, Board, Sessions and More. A session shows in place of its tab's view, with a back button to it and no tabs. The rules are `state.TabOf`, `state.BackTo`, `state.TabBarShows` and `state.InboxBadge`, with their tests (`TestEachTabHasItsPlace`, `TestBackGoesToTheTabsOwnView`, `TestTheInboxBadge`).
  - The top bar holds the title, the back button and New Session (on Sessions). The filter is the Sessions tab's, so no field covers anything.
  - Cards and dialogs stay within the screen: a smaller gutter, card titles that wrap beside their buttons with the thread's caption under them, and dialogs no wider than the screen, the Approve and Merge confirmation included.
  - The bars go under the status bar and the home indicator, and the toasts show under the top bar.
  - Nothing raises a phone's keyboard unasked: an opened session does not focus its composer, and New Session gives its Cancel the focus. New Session opens as a sheet, which keeps its buttons above the keyboard. The composer's hints name no keys, and its model chip takes a row of its own, so Send stays on screen.
  - Settings is a list in More (Appearance's Zoom), not the desktop's two-pane window.
  - The work board puts a Proposals or Schedules section that holds nothing after the threads, and keeps one that holds something above them (`state.BoardOrder`, the owner's answer of 2026-11-21 to #85's question).
- [x] The same compact layout for an iPad window narrower than 760 under Split View, Slide Over or Stage Manager. The iPad keeps multitasking, with no full-screen requirement (the owner's choice of 2026-11-21). `state.Compact` holds for any iOS window narrower than 760.
- [ ] The titles cut short at iPad portrait (820×1180): the main bar's title and its note, which the search field crowds ("2 tool calls…"), and the cards' titles ("write waits for your ap…"). The owner left them for this phase on 2026-11-21.
  - Not done: Phase 3 keeps the iPad's full window as it is. Its card titles could wrap as a compact window's do.
- [x] View tests and `make snapshot` at 390×844 as well: `TestThePhoneSnapshot` draws the Inbox, Sessions, a session, the work board, the Approve and Merge confirmation and More, with an iPhone's safe areas, in a test process of its own. The safe areas turn MyGo's touch handling on for the whole process. The other phone tests check the tabs, back, the keyboard and the card titles.
- [x] Approve and Merge keeps its gates as the [architecture's desktop client](../design/architecture.md#desktop-client) describes them. Only its confirmation's layout changes, on a compact window.
- [ ] **Done when:** on an iPhone, the owner answers an ask, reads a session, sends a message, and approves and merges a pull request that pocketd backs, with the confirmation whole on screen.
  - Not seen yet: it needs a Mac with Xcode and an iPhone, or its simulator. On Linux, the phone's view tests pass at 390×844.

## Later / ideas

- **Push notifications:** pocketd tells the owner's devices of an ask, a plan or a ready pull request through APNs. This needs the paid developer program and an ADR.
- **The Terminal on iOS:** iOS bindings and an iOS build of libghostty-vt in MyGo's terminal plugin, upstream.
- **Documents by paste or drop:** file formats in MyGo's phone clipboard and file drops in its iPad window, upstream.
- **Photos** as attachments, through MyGo's `imagepicker` plugin.
- **Android**, from the same code, with the NDK.

## Open questions

- **Signing:** a free personal team, or the paid developer program, which the owner may reactivate. This decides whether Phase 2 runs on a device or in the simulator. Decide it by Phase 2.
- **The iPhone's navigation:** resolved on 2026-11-21. The owner chose a bar of tabs over the sidebar as a sheet, from snapshots of both ([ADR-0044](../adr/0044-navigate-a-compact-window-by-a-bar-of-tabs.md)).
- **Push:** whether the owner wants notifications while the app is closed. Decide it after Phase 3.

## References

- Implements: [design: system architecture, desktop client](../design/architecture.md#desktop-client) and [operational notes](../design/architecture.md#operational-notes)
- Contracts: [specs/protocol.md](../specs/protocol.md), unchanged: the phone speaks the desktop's protocol
- Decisions: [ADR-0040](../adr/0040-build-the-ipad-and-iphone-client-from-the-desktops-code-on-mygos-ios-target.md) (one app on MyGo's iOS target, settled by Phase 2), [ADR-0041](../adr/0041-save-the-window-and-hold-the-github-refresh-when-ios-suspends-the-app.md) (saving as iOS suspends the app), [ADR-0044](../adr/0044-navigate-a-compact-window-by-a-bar-of-tabs.md) (a compact window's bar of tabs, settled by Phase 3); ADR-0010 (the data layer apart from the views), ADR-0011 (layout and platform rules in `state`) and ADR-0018 (approve and merge from the client), as the [architecture](../design/architecture.md#desktop-client) links them; a decision this plan settles becomes a new ADR
- From: [roadmap, Later / ideas](roadmap.md#later--ideas)
- Set up by: [design: setup guide](../design/setup.md), for the host, the service URL and a device's token
