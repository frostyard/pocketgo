# PocketGo on NSGo

PocketGo’s Linux and web clients render the same Go component tree. The chat uses NSGo’s floating composer, Markdown messages, collapsible work summaries, tool rows and message actions. The shell changes to a navigation button on narrow windows. Both draw in the shared Frostyard themes, a dark and a light one, with the bundled Inter font. The **Theme** setting follows the system or shows the light or the dark theme at once, and the choice is kept on the device. Compare the same conversation on [Linux](../../internal/nsgoclient/comparison/linux/conversation.png) and [Chromium](../../internal/nsgoclient/comparison/web/conversation.png).

## Try both against disposable data

From the repository root, build the two targets and start the fixture with these commands.

```sh
cd desktop/cmd/pocket-nsgo
go tool nsgo build
cd ../..
go run ./internal/nsgoclient/testdata/preview -scene busy-team
```

Open the printed loopback URL in a browser. In another terminal, start the native app from the repository root.

```sh
desktop/cmd/pocket-nsgo/dist/PocketGo-0.1.0-linux_x64/PocketGo
```

Enter the printed service URL and fixture device token in each client. The default **busy-team** scene includes two running agents, five open asks, a thread in each of the board's three open groups and a 64-turn supervisor session. Open **Release supervisor** to expand a completed turn, or follow **Work board → View ask A43 → Open session** and use Back twice. The browser’s Back and Forward buttons follow the same visits. Use `-scene checklist` for the smaller fixture. Open **Release checklist** on both clients to try messaging. Send a message from one and watch the other receive it. Try **Saved prompts** in the composer, switch to **Files**, or change the appearance in **Settings**. The fixture keeps data in memory and discards it when stopped. The terminal integration tests provide a simulated shell. The native build uses GTK 4 and the adjacent Ghostty library.

## Use with pocketd

Sign in with a service URL and a device token issued by the existing pocketd. For web, serve the generated web directory and route `/v1/` to pocketd on the same HTTPS origin. Pass the authorization header through and disable response buffering for the event stream. A browser on another origin needs an explicitly configured CORS policy on the API. The browser keeps the device token in memory for the page session. On desktop, the port uses PocketGo’s credential store and the existing owner sign-in lock. Both desktop clients share that account identity.

Open **Files** to choose an agent home, expand directories and preview a file. In a conversation, the **Session files** toolbar button opens the same explorer beside the draft. Drag the dividers to size the tree and the rail. On a narrow window, **File tree** returns from a preview to its directory. An existing pocketd can serve sessions within advertised agent homes. Deploy the service with [session folder support](../../../docs/adr/0069-browse-a-sessions-working-folder.md) to browse other permitted session directories and worktrees.

The native client can check and approve a GitHub merge through the existing owner flow. The web client currently provides a pull-request link. The [shared merge service proposal](../../../docs/plans/shared-merge-service.md) covers owner sign-in, revision-bound confirmation and PocketGo cross-review for both clients and future Frostyard apps.

## Code and checks

[workspaceui](https://github.com/frostyard/frostui/tree/workspaceui/v0.5.1/workspaceui), the module `github.com/frostyard/frostui/workspaceui` that `desktop/go.mod` pins at v0.5.1, contains reusable shell, layout and chat components, and the Frostyard look ([its theme spec](https://github.com/frostyard/frostui/blob/workspaceui/v0.5.1/docs/specs/frostyard-theme.md)). Its public API accepts content and callbacks. The client calls `workspaceui.Apply` once at its root, which sets the themes and registers Inter, and puts its views in `workspaceui.Frame`, which paints the ground and sets the text's color and font. The view inside the frame applies the Zoom setting. The Theme setting is the module's `AppearanceChoice`, a segmented control of System, Light and Dark. The client keeps the choice in its storage under `appearance`, as `System`, `Light` or `Dark`, and shows a saved choice at its start. [nsgoclient](../../internal/nsgoclient) binds these to PocketGo’s state store, network protocol and platform services. The Linux client uses the application ID `org.frostyard.PocketGo`. See the [release candidate instructions](../../../packaging/README.md) for the Flatpak and deployable web archive.

Compare the busy-team scene on [Linux](../../internal/nsgoclient/comparison/linux/busy-team-conversation.png) and [Chromium](../../internal/nsgoclient/comparison/web/busy-team-conversation.png).

The real-target tests run the same interactions in a Linux window and a browser. From the repository root, `make nsgo-live` runs the suite in a Linux window. It needs `xvfb-run` and is no part of `make verify` or `make ci`.

```sh
make nsgo-live
```

The target runs `go tool nsgo test -target linux` in `desktop/internal/nsgoclient` under Xvfb, on a screen of 1600 by 1200, and sets `GDK_BACKEND=x11`. Xvfb alone does not keep the windows off a Wayland desktop: GTK tries Wayland first and finds the desktop's compositor even with `WAYLAND_DISPLAY` unset. A picture shorter than the window its test asked for was taken on the desktop, under a title bar. The target also gives GTK the settings file in `testdata/xdg`, which stops the text caret's blinking. It writes its pictures to `desktop/internal/nsgoclient/.nsgo/test/screenshots/linux`, which git ignores.

For the browser, run the suite from `desktop/internal/nsgoclient`. Set `NSGO_BROWSER` to a Chromium executable if the tool cannot find one.

```sh
go tool nsgo test -target web
```

The kept pictures in `comparison/linux` and `comparison/web` come from the same tests. Take them again after a change to a view, and look at them:

```sh
make nsgo-pictures
cd desktop/internal/nsgoclient && go tool nsgo test -target web -screenshots comparison/web
```

Each folder keeps 13 pictures. Seven show the dark theme, which their tests choose in Settings: `agent-sessions`, `folder-error-toast`, `inbox-asks`, `inbox-pull-request`, `inbox-review-cards`, `settings` and `work-board`. The other six show the light theme. `busy-team-conversation` can differ between two runs of the same tree, since it holds an animated icon.

The headless Go suite also exercises clipboard attachment injection. The repository’s ordinary checks run with `make verify`.

## Workspace UI task list

- [x] Group conversation work by turn, keeping decisions visible and history bounded.
- [x] Restore the originating session, ask or board card on Back and Forward.
- [x] Offer model, thinking level and context usage at the composer.
- [x] Keep inspector fields stable and remember both pane widths.
- [x] Provide a repeatable busy-team preview for native and web.
- [x] Let applications supply keyed conversation entries, reveal targets and decision footers through workspaceui.
