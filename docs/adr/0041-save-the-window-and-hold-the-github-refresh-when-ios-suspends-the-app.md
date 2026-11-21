# 0041 — Save the window and hold the GitHub refresh when iOS suspends the app

- **Status:** Accepted
- **Date:** 2026-11-21

## Context

**iOS ends apps in the background without closing their windows.** [ADR-0040](0040-build-the-ipad-and-iphone-client-from-the-desktops-code-on-mygos-ios-target.md) builds the iPad client from the desktop's code. Its consequences named two losses:
- `App.Close` may not run on iOS, so the pane widths and the session shown are not kept for the next launch.
- A GitHub refresh in progress can be cut short. Once GitHub answers a refresh, the old refresh token is spent. If the app stops before the new pair is in the keychain, the GitHub sign-in is lost, and the owner must sign in again.

**The owner chose on 2026-11-21.** "Save on suspend: build it." When the app goes to the background on iOS, it saves what `App.Close` saves today, and a GitHub token refresh is not lost to the suspension. The desktop stays as it is unless saving there on the same event is clearly harmless.

**What MyGo v0.10.0 gives:**
- The iOS host asks Go as the app's last scene goes to the background (`sceneDidEnterBackground`), and waits for the answer. MyGo then calls the `Power.OnSuspend` listeners on the app's thread, one after another. Coming back calls the `Power.OnResume` listeners.
- While the host waits, it runs the calls of Go's other goroutines, keychain writes among them.
- iOS gives an app about 5 seconds to leave for the background. MyGo has no way to ask for more (`Power.KeepRunning` does nothing on phones).
- On a desktop, `Power.OnSuspend` is the computer about to sleep, and a first listener starts MyGo's watch of the system's power events. The window stays open across the sleep.

## Decision

1. **When to save is code** ([ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md)): `state.SavesOnSuspend` holds on iOS alone. `cmd/pocket` registers its suspend and resume listeners only then (`suspendHooks`).
2. **The window saves as it goes to the background** (`App.Suspend`). It saves what `App.Close` saves: the session shown, marked seen as it is, the panes' widths and folds, and the rest of the settings. Unlike Close it stops nothing: no stream, no shell, no store. The app goes on as it comes back.
3. **Then the GitHub sign-in holds its refresh** (`github.TokenSource.Suspend`).
   - No refresh starts until `Resume`. A lease that needs one waits for it.
   - A lease of a pair that needs no refresh goes on.
   - Suspend waits for a refresh or a credential write in progress, counted as `TokenSource` work under its mutex as `Close` counts it, at most 3 seconds. So a pair GitHub has refreshed reaches the keychain before the app stops.
   - The sign-in lock stays held. The process keeps it until it ends (`config.LockSignIn`).
   - `Resume`, as the app comes back, lets a refresh start.
4. **The desktops stay as they are.** They save as the window closes and listen to no power event. Saving on a desktop's sleep would start MyGo's power watch for nothing, since the window stays open.
5. **`TokenSource.Suspend` and `Resume` are pocketgo's own**, in the port of frostyard/pocket's `internal/github` ([ADR-0018](0018-approve-and-merge-as-the-owner-from-the-desktop-with-pockets-gates.md)). No gate changes.

This amends ADR-0040's consequence about `App.Close` on iOS. The rest of ADR-0040 holds.

## Consequences

- The iPad comes back to the session it showed, with its layout, after iOS ended it in the background.
- A refresh that GitHub answers within 3 seconds reaches the keychain before the app stops.
- A refresh that takes longer can still be cut short. iOS then stops the app with GitHub's answer outstanding, and the next refresh may find the sign-in ended. The owner then signs in to GitHub again.
- A GitHub job begun while the app is suspended, whose pair needs a refresh, waits for the app to come back.
- The suspend can hold the app's thread up to 3 seconds. Nothing shows on screen then.
- `desktop/internal/github` is no longer frostyard/pocket's file for file: `tokensource.go` carries the two methods.

## Alternatives considered

- **Save when the app resigns active (`App.OnDidResignActive`):** iOS sends that for the app switcher or a pulled-down notification, with the app still on screen. The suspend is when iOS may end the app.
- **Save each change of the layout and of the session shown as it happens:** many more writes, since a pane's width changes as it is dragged, and a refresh in progress would still be cut short.
- **Save on the desktop's sleep too:** the window stays open across a sleep and saves as it closes. A power listener would start MyGo's power watch there for nothing.
- **Wait longer for the refresh:** iOS ends an app that has not left within about 5 seconds, and MyGo writes its own state in the same event.
- **MyGo's `App.OnSaveState`:** it keeps a small blob that the next launch reads with `RestoredState`. The settings already live in their own file.

## References

- Shapes: [design/architecture.md](../design/architecture.md#desktop-client), [plans/phone-client.md](../plans/phone-client.md)
- Built by: [plans/phone-client.md](../plans/phone-client.md#phase-2--ipad-build-medium) (Phase 2)
- Builds on: [ADR-0040](0040-build-the-ipad-and-iphone-client-from-the-desktops-code-on-mygos-ios-target.md), whose consequence it amends, [ADR-0018](0018-approve-and-merge-as-the-owner-from-the-desktop-with-pockets-gates.md), [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md)
