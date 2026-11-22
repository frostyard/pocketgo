# 0040 — Build the iPad and iPhone client from the desktop's code on MyGo's iOS target

- **Status:** Accepted; the iPhone's compact layout its consequences call for decided by [ADR-0044](0044-navigate-a-compact-window-by-a-bar-of-tabs.md), a bar of tabs; its consequence that `App.Close` may not run on iOS amended by [ADR-0041](0041-save-the-window-and-hold-the-github-refresh-when-ios-suspends-the-app.md), which saves the window as iOS suspends the app
- **Date:** 2026-11-21

## Context

**The owner wants PocketGo on an iPad, and later on an iPhone.** The [phone and tablet client plan](../plans/phone-client.md) grew out of the roadmap's Later item. Its investigation of 2026-11-21 found:
- MyGo, which the desktop is built on, runs the same Go app on iOS: native UI drawn with Metal, a Swift host, and `mygo build -platform ios`. frostui is plain Go over MyGo's `ui`.
- The desktop's UI-free packages (`internal/state`, `internal/config`, `internal/github`, `internal/icons`) and `proto` build for `ios/arm64` unchanged. They are about 76% of the client. `internal/app` type-checks for iOS, and every view test passes at iPad size (820×1180).
- Go links for iOS only with cgo and Xcode's clang, on a Mac.
- Plain HTTP is no problem on iOS. pocketd's requests go through Go's own `net/http` (`state.DirectTransport`), not Apple's URL Loading System, so App Transport Security does not apply. `state.NeedsHTTPWarning` treats a `*.ts.net` name as the tailnet's on every platform.

**What does not work on iOS as it is:**
- **The Terminal** ([ADR-0025](0025-give-the-owner-a-shell-on-the-service-host-from-the-desktop.md)). MyGo's terminal plugin ships libghostty-vt for macOS, Linux and Windows only. Its bindings are stubs on iOS (`vt.Supported` is false), so `terminal.Load` fails there. MyGo's iOS build also bundles the native library each of the app's packages names, and stops on one with no iOS build (MyGo v0.10.0, `mygo build`'s natives step). An app that imports the plugin does not build for iOS at all.
- **The GitHub sign-in lock** ([ADR-0018](0018-approve-and-merge-as-the-owner-from-the-desktop-with-pockets-gates.md)). `config.signInLockDir` puts the lock in Application Support only when `runtime.GOOS` is `darwin`. iOS reports `ios`, so the lock goes under `.config` in the home folder that the user database names. That folder is outside the app's container, and the sandbox does not let the app write there. Without the lock the GitHub sign-in does not open, and Approve and Merge stays off even with a working keychain.
- **Quit.** iOS ends apps itself. MyGo's `App.Quit` on a phone stops the app's loop and leaves the process to the system, so the window stops answering.

## Decision

1. **One app.** The iPad client, and later the iPhone's, is the desktop client itself. It is built from the `desktop` module on MyGo's iOS target (`go tool mygo build -platform ios` or `ios/simulator`), not as a separate SwiftUI app. Its app ID stays `org.frostyard.PocketGo`.
   - Its iOS settings are `desktop/mygo.json`'s `ios` section: iOS 17.0 or later, the `development` distribution, and both orientations.
   - No Apple team ID enters the repository. MyGo signs with the installed provisioning profile that best fits the identifier. An owner whose Mac holds profiles of more than one team sets `ios.team` in their own checkout only ([plan: Try it on a Mac](../plans/phone-client.md#try-it-on-a-mac)).
2. **HTTPS is not an iOS requirement.** The iPad signs in as the desktop does: over HTTPS, or over plain HTTP to a `*.ts.net` name, with the same token, warning and redirect rules. Phase 1's HTTPS through `tailscale serve` is a deployment choice (frostyard/pocketgo#68).
3. **The platform rules are code in `desktop/internal/state`** ([ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md)). They take `runtime.GOOS`, which the program gives the window as `app.Options.Platform`, so a view test can draw another platform's window.
   - `state.HasTerminal`: iOS has no Terminal until MyGo's terminal plugin has an iOS library. There the sidebar leaves the place out, the window never goes to it, and the emulator's library never loads. The shells' connection and its rules stay in the data layer, unchanged.
   - The iOS build leaves the plugin out altogether: its two files in `internal/app`, and the tests that drive the emulator, are built `!ios`, and `terminal_emulator_ios.go` stands in for them with stubs. A test checks that no file built for iOS, a test included, imports the plugin or uses what only those files declare. So MyGo's build names no library it cannot bundle, and `make portable` checks that the plugin stays out of the iOS app.
   - `state.QuitsItself`: the app quits itself everywhere but on iOS. On iOS the menu has no Quit, as when the program gives none.
4. **The GitHub sign-in lock on iOS is in the app's container:** `Library/Application Support/org.frostyard.PocketGo/github-signin.lock` under the home folder iOS gives the app, which is its container (`$HOME`).
   - This amends ADR-0018's lock folder on iOS alone. On iOS the keychain entry is the app's own, in its access group, not the user's. The system sets the app's environment, not the user, and runs one process of the app. So one lock in the container is one lock per GitHub credential.
   - The rest of ADR-0018 holds: `config.LockSignIn`, the sign-in in the keychain only (`config.GitHubApp` over `mygo.Secrets`), no file fallback, and Approve and Merge only once the lock and the keychain opened.
   - The folder is chosen by `signInLockDirOn`, which takes the platform and both homes, so a test on Linux checks the iOS folder.
5. **The keychain and the picker need no code of their own.** `mygo.Secrets` keeps the device token and the GitHub sign-in in the iOS Keychain, through MyGo's Swift host. `mygo.Dialog.Open` shows the system's document picker for Attach files and gives the app copies of the files chosen ([ADR-0029](0029-attach-the-owners-files-to-a-message-stored-outside-the-data-folder.md)).
6. **CI builds the UI-free packages for `ios/arm64` on Linux.** `make portable`, which `make ci` runs, builds `proto` and the desktop's `internal/state`, `internal/config`, `internal/github` and `internal/icons` with CGO off. A dependency or a call that iOS lacks then fails CI. It does not vet them or their tests for iOS, since `go vet` for iOS refuses to run with CGO off. It also fails when the desktop's iOS app, listed as MyGo's build lists it, imports MyGo's terminal plugin, or when the list cannot load a package. `make lint`'s `desktop-state-boundary` rule stays the check on UI imports in `state`.

## Consequences

- The iPad gets each fix and feature of the desktop with no second code base, and the rules in `state` are written and tested once.
- The views work at iPad size as they are. The iPhone needs a compact layout below the desktop's smallest window (Phase 3), chosen by a rule in `state`.
- Building, signing and running on iOS need a Mac with Xcode. The simulator needs no Apple account; a device needs a developer team and a provisioning profile. `internal/app`, `cmd/pocket` and `terminal_emulator_ios.go` are compiled for iOS only there: CI on Linux does not build them for iOS. The iOS file holds stubs only.
- The Terminal comes back on iOS only once MyGo's terminal plugin has iOS bindings and an iOS library upstream. Then `state.HasTerminal` and the build constraint change together.
- iOS suspends an app it does not show. The event stream drops, and `state.Link` connects again when the app comes back. A notification while the app is closed needs pocketd to send it through APNs, which is not built.
- iOS ends a suspended app without closing its window, so `App.Close` may not run there. The pane widths and the session shown are then not kept for the next launch. The zoom, the seen marks and the pins are saved as they change.
- A document copied in Files does not paste, and nothing drops on the window, since MyGo's phone clipboard reads only text and PNG and its window reports no dropped files. The picker works.

## Alternatives considered

- **A separate SwiftUI app:** a second client in another language, with its own data layer, rules and tests to keep in step with the desktop's. The rules in `state` would be written twice.
- **Wait for the Terminal on iOS:** the Inbox, the work board and the sessions are what the owner needs away from the desk. The Terminal follows once MyGo's plugin supports iOS.
- **Keep the lock in the user database's home on iOS:** the sandbox does not let the app write outside its container, so Approve and Merge would stay off.
- **No lock on iOS:** that is a rule for one platform in `config.LockSignIn`. A lock in the container costs nothing, since an iOS app runs one process.
- **Keep the terminal plugin in the iOS build:** MyGo's iOS build stops on its library, which has no iOS build.

## References

- Shapes: [design/architecture.md](../design/architecture.md#desktop-client), [plans/phone-client.md](../plans/phone-client.md)
- Built by: [plans/phone-client.md](../plans/phone-client.md#phase-2--ipad-build-medium) (Phase 2)
- Builds on: [ADR-0010](0010-port-the-desktop-client-selectively-from-frostyard-pocket.md) (the data layer apart from the views), [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0018](0018-approve-and-merge-as-the-owner-from-the-desktop-with-pockets-gates.md) (whose lock folder it amends on iOS), [ADR-0025](0025-give-the-owner-a-shell-on-the-service-host-from-the-desktop.md) (whose Terminal it leaves out on iOS), [ADR-0029](0029-attach-the-owners-files-to-a-message-stored-outside-the-data-folder.md)
