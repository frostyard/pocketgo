# PocketGo on NSGo

PocketGo’s Linux and web clients render the same Go component tree. The chat uses NSGo’s floating composer, Markdown messages, collapsible work summaries, tool rows and message actions. The shell changes to a navigation button on narrow windows. Appearance follows the system or a live choice in Settings. Compare the same conversation on [Linux](../../internal/nsgoclient/comparison/linux/conversation.png) and [Chromium](../../internal/nsgoclient/comparison/web/conversation.png).

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

Enter the printed service URL and fixture device token in each client. The default **busy-team** scene includes two running agents, five open asks, all four active board lanes and a 64-turn supervisor session. Open **Release supervisor** to expand a completed turn, or follow **Work board → View ask A43 → Open session** and use Back twice. The browser’s Back and Forward buttons follow the same visits. Use `-scene checklist` for the smaller fixture. Open **Release checklist** on both clients to try messaging. Send a message from one and watch the other receive it. Try **Saved prompts** in the composer, switch to **Files**, or change the appearance in **Settings**. The fixture keeps data in memory and discards it when stopped. The terminal integration tests provide a simulated shell. The native build uses GTK 4 and the adjacent Ghostty library.

## Use with pocketd

Sign in with a service URL and a device token issued by the existing pocketd. For web, serve the generated web directory and route `/v1/` to pocketd on the same HTTPS origin. Pass the authorization header through and disable response buffering for the event stream. A browser on another origin needs an explicitly configured CORS policy on the API. The browser keeps the device token in memory for the page session. On desktop, the port uses PocketGo’s credential store and the existing owner sign-in lock. Both desktop clients share that account identity.

Open **Files** to choose an agent home, expand directories and preview a file. In a conversation, the **Session files** toolbar button opens the same explorer beside the draft. Drag the dividers to size the tree and the rail. On a narrow window, **File tree** returns from a preview to its directory. An existing pocketd can serve sessions within advertised agent homes. Deploy the service with [session folder support](../../../docs/adr/0069-browse-a-sessions-working-folder.md) to browse other permitted session directories and worktrees.

The native client can check and approve a GitHub merge through the existing owner flow. The web client currently provides a pull-request link. The [shared merge service proposal](../../../docs/plans/shared-merge-service.md) covers owner sign-in, revision-bound confirmation and PocketGo cross-review for both clients and future Frostyard apps.

## Code and checks

[workspaceui](../../workspaceui) contains reusable shell, layout and chat components. Its public API accepts content and callbacks. [nsgoclient](../../internal/nsgoclient) binds these to PocketGo’s state store, network protocol and platform services. The Linux client uses the application ID `org.frostyard.PocketGo`. See the [release candidate instructions](../../../packaging/README.md) for the Flatpak and deployable web archive.

Compare the busy-team scene on [Linux](../../internal/nsgoclient/comparison/linux/busy-team-conversation.png) and [Chromium](../../internal/nsgoclient/comparison/web/busy-team-conversation.png).

The real-target tests run the same interactions in a Linux window and a browser. From `desktop/internal/nsgoclient` run:

```sh
go tool nsgo test -target linux
go tool nsgo test -target web
```

Use Xvfb for a Linux host without a display. Set `NSGO_BROWSER` to a Chromium executable if the tool cannot find one. The headless Go suite also exercises clipboard attachment injection. The repository’s ordinary checks run with `make verify`.

## Workspace UI task list

- [x] Group conversation work by turn, keeping decisions visible and history bounded.
- [x] Restore the originating session, ask or board card on Back and Forward.
- [x] Offer model, thinking level and context usage at the composer.
- [x] Keep inspector fields stable and remember both pane widths.
- [x] Provide a repeatable busy-team preview for native and web.
- [x] Let applications supply keyed conversation entries, reveal targets and decision footers through workspaceui.
