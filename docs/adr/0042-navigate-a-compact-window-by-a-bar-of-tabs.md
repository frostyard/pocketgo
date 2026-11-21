# 0042 — Navigate a compact window by a bar of tabs

- **Status:** Accepted
- **Date:** 2026-11-21

## Context

**The iPhone runs the desktop's code** ([ADR-0040](0040-build-the-ipad-and-iphone-client-from-the-desktops-code-on-mygos-ios-target.md)). Its views reflow at 390×844, but its window is narrower than the desktop's smallest (760×480), and the desktop's layout breaks there ([plan: phone and tablet client](../plans/phone-client.md#the-views-at-ios-sizes)):
- the sidebar folds away behind a toggle that the search field half covers;
- the main pane keeps its least width of 420 DIPs, so cards run past the right edge;
- titles and the status bar are cut short;
- dialogs are wider than the screen, and the Approve and Merge confirmation loses its Cancel button.

**The plan left the navigation open:** tabs for the places, or the sidebar as a sheet. Both were built as prototypes on 2026-11-21, with the app's real views and data, and drawn as snapshots at 390×844:
- **Tabs:** a bar at the bottom with the Inbox, the work board, the sessions and More.
- **A sheet:** one view at a time, under a top bar whose button opens the desktop's sidebar as a sheet over it.

**The owner chose tabs on 2026-11-21.**

**What MyGo v0.10.0 gives:**
- `ui/mobile` draws a phone's bars, sheets and lists on `ui`'s public API: `TopBar`, `BottomBar` with a badge on a button, `Sheet`, `Section` and `ListRow`, in iOS's look on iOS.
- On a phone the window's root keeps its content out from under the status bar and the home indicator. `Context.SafeArea` says how far they reach, and `BottomBar` goes down under the home indicator itself.
- `mygo.NewTabs` shows the system's own tab bar, with one window for each tab. PocketGo has one window, whose views route within it.

## Decision

1. **Which window is compact is code** ([ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md)). `state.Compact` holds on iOS for a window narrower than the desktop's smallest, 760 DIPs at 100%. That covers an iPhone, and an iPad in Split View, Slide Over or Stage Manager. An iPad's full window and every desktop window keep the desktop's layout.
2. **A compact window navigates by a bar of tabs at the bottom.** The tabs are:
   - **Inbox**, with a badge that counts what waits there;
   - **Board**, the work board;
   - **Sessions**, the sidebar's pinned, team and recent sessions, with its filter and New Session;
   - **More**, Settings' Zoom and what a desktop's main menu and status bar hold.

   Each tab shows its view at full width, under a compact top bar with its title and the desktop's note line.
3. **A session shows in place of its tab's view, with a back button and no tab bar.** Its composer takes the bottom of the screen, as in a phone's messaging apps. Back goes to the own view of the tab the session was opened from, never to an earlier session. Going to a place by any way (its tab, a key, a card's button) chooses that place's tab.
4. **These rules live in `desktop/internal/state`, each with its test:**
   - `TabOf`: which tab a place belongs to;
   - `BackTo`: where back goes;
   - `TabBarShows`: when the bar shows;
   - `InboxBadge`: the Inbox's count, up to "99+", or "!" when nothing is counted and a list failed to load.

   `desktop/internal/app` draws them with `ui/mobile`'s `TopBar`, `BottomBar`, `Section` and `ListRow`.
5. **A compact window fits the screen.** On a compact window only:
   - The bars' colors go under the status bar and the home indicator, and the toasts show under the top bar.
   - Dialogs are no wider than the screen. The confirmations go through the app's own queue, which feeds frostui's dialog on a desktop window.
   - Cards keep a smaller gutter. A card's title wraps beside its buttons, and its thread's caption goes under it.
   - Nothing raises the keyboard unasked: an opened session does not focus its composer, and New Session does not focus a field.
   - The composer's hints name no keys.

## Consequences

- The Inbox is one tap away from every tab's view, and its count shows on the bar under all of them.
- A compact window is drawn by PocketGo with `ui/mobile`, not by the system's tab bar (`mygo.NewTabs`). It does not take on iOS's newer bar looks, and it is tested headless like the rest of the views.
- A session hides the tabs, so the way from one session to another goes through its tab.
- Settings is a list in More on a compact window, not the desktop's two-pane window. The window's zoom still never goes past what fits the session list beside the session (`ZoomFit`), so a zoom above 100% shows at 100% on an iPhone.
- The phone's snapshot test runs as a test process of its own. The safe areas a test gives the window turn MyGo's touch handling on for the whole process (`inPhoneProcess`).
- The views are still the desktop's. A view that wants a phone's own layout, such as the work board's sections, is a further change.

## Alternatives considered

- **The sidebar as a sheet:** one view at a time, with the desktop's sidebar (places, pinned, team, recent and More's items) in a sheet that a top-bar button opens. The owner turned it down. Tabs keep the Inbox one tap away, with its count always visible, while the sheet puts every place two taps away and its count on a button. The sheet also takes the focus as it opens, so its filter had to wait behind a button of its own to keep a phone's keyboard down.
- **The system's tab bar (`mygo.NewTabs`):** it shows each tab as a window of its own, and pushes a session as another window. The window's state, the stream and the dialogs would have to be shared among windows, and headless tests could not draw it.
- **Keep the desktop's layout and only fix the widths:** the sidebar would still fold behind a toggle, with the Inbox hidden behind it.

## References

- Shapes: [design/architecture.md](../design/architecture.md#desktop-client), [plans/phone-client.md](../plans/phone-client.md#phase-3--iphone-layout-medium)
- Built by: [plans/phone-client.md](../plans/phone-client.md#phase-3--iphone-layout-medium) (Phase 3)
- Builds on: [ADR-0040](0040-build-the-ipad-and-iphone-client-from-the-desktops-code-on-mygos-ios-target.md), [ADR-0010](0010-port-the-desktop-client-selectively-from-frostyard-pocket.md), [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md)
