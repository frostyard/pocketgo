# 0071 — Make the NSGo client the only client

- **Status:** Accepted
- **Date:** 2026-11-24

## Context

PocketGo has 2 clients over one data layer (`desktop/internal/state`, which imports no UI, [ADR-0010](0010-port-the-desktop-client-selectively-from-frostyard-pocket.md)):

- **The MyGo client** (`desktop/internal/app`, `desktop/cmd/pocket`), on MyGo and frostui. It is the client of macOS, the iPad and the iPhone ([ADR-0040](0040-build-the-ipad-and-iphone-client-from-the-desktops-code-on-mygos-ios-target.md)), and a fallback on Linux. Its iOS build has never been linked or run: that needs a Mac ([phone plan](../plans/phone-client.md)).
- **The NSGo client** (`desktop/internal/nsgoclient`, `desktop/cmd/pocket-nsgo`), which [ADR-0068](0068-build-a-shared-nsgo-client-beside-the-desktop-client.md) built beside it. It is the default on Linux and the only client in a browser ([ADR-0070](0070-ship-nsgo-as-the-default-linux-client.md)), in the Frostyard look of the kit it shares with FrostMail and Frostnotes (`github.com/frostyard/frostui/workspaceui`).

Each interaction is written twice. The NSGo client lacks some of what the MyGo client does ([plan](../plans/nsgo-only-client.md#what-the-nsgo-client-lacks)). NSGo has targets for macOS, iOS and Windows, and its terminal view carries libghostty-vt for iOS, which MyGo's terminal plugin does not. PocketGo has built none of them.

PocketGo has no release: its tags are candidates, `v0.1.0-rc.1` to `v0.1.0-rc.4`.

## Decision

The NSGo client becomes PocketGo's only client, on every platform PocketGo has a client for: Linux, a browser, macOS, the iPad and the iPhone.

1. **It first gets what the MyGo client has.** What the plan lists is built in the NSGo client, and its preferences are kept in 1 place, with a move from the other.
2. **The iPad, the iPhone and macOS are built from the same view tree,** on NSGo's targets for them. The rules of a phone and a tablet stay where they are, in `desktop/internal/state`: the compact window's bar of tabs ([ADR-0044](0044-navigate-a-compact-window-by-a-bar-of-tabs.md)), saving when iOS suspends the app ([ADR-0041](0041-save-the-window-and-hold-the-github-refresh-when-ios-suspends-the-app.md)), and no Terminal where the platform has none.
3. **MyGo is removed** once the NSGo client has run on each of those platforms: `desktop/internal/app`, `desktop/cmd/pocket`, `desktop/mygo.json`, and what names them. `desktop/go.mod` then requires neither MyGo nor frostui's root module.
4. **The first release, `v0.1.0`, is of the NSGo client.**

This supersedes ADR-0040's choice of MyGo's iOS target and ends the MyGo fallback and MyGo's other platforms that ADR-0070 kept.

## Consequences

- One client to change, test and draw. The kit's standard for an app (frostui's `docs/specs/workspaceui.md`) and the NSGo client's live suite (`make nsgo-live`) are the product's.
- macOS, the iPad and the iPhone have no client between MyGo's removal and the NSGo client's first run on them, so the removal waits for that run. A build for any of them needs a Mac with Xcode, which CI and the Linux development machines do not have: the owner's Mac runs them.
- Whether NSGo's iOS and macOS targets build and run PocketGo is not known. The first change of the port finds out.
- What the phone plan built for MyGo's iOS target goes with MyGo: its `ios` section of `mygo.json`, the files built `!ios`, `make portable`'s check of MyGo's plugin list, and the phone's view tests of `internal/app`. Their rules in `state`, and those rules' tests, stay.
- The Terminal may come to iOS, since NSGo's terminal view has an iOS library. Until it has run there, `state.HasTerminal` keeps it hidden.
- MyGo's own tools go with it (`go tool mygo vet`, `make snapshot`, frostui's test loop, which 3 tests of `internal/state` use).
- The module requires NSGo, which frostyard does not own.

## Alternatives considered

- **Keep MyGo for macOS and iOS:** every interaction stays written twice, for 3 platforms whose build has never run.
- **Drop the iPad, the iPhone and macOS:** the owner wants PocketGo on them ([ADR-0040](0040-build-the-ipad-and-iphone-client-from-the-desktops-code-on-mygos-ios-target.md)).
- **Remove MyGo before the NSGo client runs on them:** they would have no client, and no proof that one can be built.

## References

- Shapes: [plan: NSGo as the only client](../plans/nsgo-only-client.md), [design: system architecture, desktop client](../design/architecture.md#desktop-client), [plan: phone and tablet client](../plans/phone-client.md)
- Builds on: [ADR-0068](0068-build-a-shared-nsgo-client-beside-the-desktop-client.md), [ADR-0070](0070-ship-nsgo-as-the-default-linux-client.md), [ADR-0041](0041-save-the-window-and-hold-the-github-refresh-when-ios-suspends-the-app.md), [ADR-0044](0044-navigate-a-compact-window-by-a-bar-of-tabs.md)
- Supersedes: [ADR-0040](0040-build-the-ipad-and-iphone-client-from-the-desktops-code-on-mygos-ios-target.md)'s MyGo iOS target
- The same decision in FrostMail: [frostyard/frostmail's ADR-0063](https://github.com/frostyard/frostmail/blob/main/docs/adr/0063-make-the-nsgo-client-the-only-client.md)
