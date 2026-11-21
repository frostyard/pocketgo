# Plan: Phone and tablet client

This plan takes the desktop client to the iPad and the iPhone on MyGo's iOS target, as one app with the desktop. It grows out of the roadmap's [Later / ideas](roadmap.md#later--ideas) item and builds on the data layer that [ADR-0010](../adr/0010-port-the-desktop-client-selectively-from-frostyard-pocket.md) keeps apart from the views ([architecture: desktop client](../design/architecture.md#desktop-client)). The findings below come from an investigation on 2026-11-21, at main 373babe. Nothing has been built, linked or run on iOS yet.

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

- **The Terminal ([ADR-0025](../adr/0025-give-the-owner-a-shell-on-the-service-host-from-the-desktop.md)):** MyGo's terminal plugin ships libghostty-vt for macOS, Linux and Windows only, so `terminal.Load` finds no library on iOS. The shell's connection is in the data layer and needs no change.
- **Plain HTTP:** iOS's App Transport Security refuses plain HTTP to a tailnet name. pocketd over HTTPS (Phase 1) needs no exception. Otherwise, `ios.infoPlist` in `mygo.json` could carry one.
- **The background:** iOS suspends an app it does not show. The event stream drops, and `state.Link` reconnects when the app comes back. A notification while the app is closed needs pocketd to send it through APNs, with MyGo's `push` plugin on the client. pocketd does not send any.
- **Building:** Go links for iOS through Xcode's clang, so a build needs a Mac with Xcode.
  - The simulator needs no paid account.
  - Devices, TestFlight and push need an Apple developer team and a provisioning profile.
  - A free personal team can sign for the owner's own device, with limits. This was not tried with `mygo build`.
  - Linux CI can still build the UI-free packages for `ios/arm64`.
- **Attachments ([ADR-0029](../adr/0029-attach-the-owners-files-to-a-message-stored-outside-the-data-folder.md)):**
  - Pasting works.
  - The Files picker works: MyGo copies a picked file into the app's temporary folder.
  - Drag-and-drop works only on an iPad.
  - Photos need MyGo's `imagepicker` plugin.

## Phase 1 — pocketd over HTTPS on the tailnet (small)

- `tailscale serve` on the service host takes port 443, which pi-pocket held until it was stopped, and proxies `https://<host>.<tailnet>.ts.net` to pocketd on `127.0.0.1:8788`. That is the unit's default once `POCKETD_LISTEN` leaves `~/.config/pocketd.env` ([architecture: operational notes](../design/architecture.md#operational-notes)).
- The desktop signs in again at the HTTPS address. It keeps the token, the pins and the seen marks by the service's origin ([architecture: desktop client](../design/architecture.md#desktop-client)), so those of the old address stay behind.
- **Done when:** the desktop signs in at `https://<host>.<tailnet>.ts.net` with no plain-HTTP question, its event stream and a terminal run through the proxy, and port 8788 on the tailnet address no longer answers.

## Phase 2 — iPad build (medium)

- An `ios` section in `desktop/mygo.json` (the team, the distribution and the orientations), and the iOS parts of `cmd/pocket`: the keychain through `mygo.Secrets`, and the system's picker for Attach files.
- The Terminal place hidden on iOS until the emulator has an iOS library, by a rule in `desktop/internal/state` with its test.
- A gate step that builds the UI-free packages for `ios/arm64` on Linux, so a UI import or a desktop-only call there fails CI.
- **Done when:** on an iPad, or the iPad simulator on the owner's Mac, PocketGo signs in to pocketd over HTTPS, and the owner answers an ask and sends a message in a session.

## Phase 3 — iPhone layout (medium)

- A compact layout below the desktop's smallest window, chosen by a rule in `desktop/internal/state` with its test:
  - the places and the sessions reached from a bar or a sheet (MyGo's `ui/mobile`);
  - a header in which the search field covers nothing;
  - cards and dialogs within the screen.
- View tests and `make snapshot` at 390×844 as well.
- **Done when:** on an iPhone, the owner answers an ask, reads a session, sends a message, and approves and merges a pull request that pocketd backs, with the confirmation whole on screen.

## Later / ideas

- **Push notifications:** pocketd tells the owner's devices of an ask, a plan or a ready pull request through APNs. This needs the paid developer program and an ADR.
- **The Terminal on iOS:** an iOS build of libghostty-vt in MyGo's terminal plugin, upstream.
- **Photos** as attachments, through MyGo's `imagepicker` plugin.
- **Android**, from the same code, with the NDK.

## Open questions

- **Signing:** a free personal team, or the paid developer program, which the owner may reactivate. This decides whether Phase 2 runs on a device or in the simulator. Decide it by Phase 2.
- **The iPhone's navigation:** tabs for the places, or the sidebar as a sheet. This is the owner's call, shown as snapshots first. Decide it by Phase 3.
- **Push:** whether the owner wants notifications while the app is closed. Decide it after Phase 3.

## References

- Implements: [design: system architecture, desktop client](../design/architecture.md#desktop-client) and [operational notes](../design/architecture.md#operational-notes)
- Contracts: [specs/protocol.md](../specs/protocol.md), unchanged: the phone speaks the desktop's protocol
- Decisions: [ADR-0010](../adr/0010-port-the-desktop-client-selectively-from-frostyard-pocket.md) (the data layer apart from the views), [ADR-0011](../adr/0011-put-rules-in-code-not-in-model-judgment.md) (layout and platform rules in `state`), [ADR-0018](../adr/0018-approve-and-merge-as-the-owner-from-the-desktop-with-pockets-gates.md) (approve and merge from the client)
- From: [roadmap, Later / ideas](roadmap.md#later--ideas)
