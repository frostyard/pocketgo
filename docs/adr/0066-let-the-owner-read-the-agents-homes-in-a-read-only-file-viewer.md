# 0066 — Let the owner read the agents' homes in a read-only file viewer

- **Status:** Accepted
- **Date:** 2026-11-23

## Context

**The team points the owner to files in the agents' homes.** A supervisor's report names a file in its home, such as `reports/<dir>/README.md`, for the owner to read. Every profile names its agent's home (`home`, [ADR-0013](0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md)), a folder on the service host. The owner reads such a file in a synced copy of one home on the desktop computer, or in the Terminal ([ADR-0025](0025-give-the-owner-a-shell-on-the-service-host-from-the-desktop.md)), which the iPad and the iPhone do not offer ([ADR-0040](0040-build-the-ipad-and-iphone-client-from-the-desktops-code-on-mygos-ios-target.md)). Away from the desktop computer, the owner cannot open them.

**What pocketd reads today:**
- A session's changes: git and pocketd's own read helper run in the sandbox, which hides pocketd's data folder, and a new file is read below the checkout's top through no link ([ADR-0053](0053-show-a-sessions-changes-read-with-isolated-git-in-code.md), [ADR-0012](0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md)).
- The owner's saved prompts, through the pinned profile folder ([ADR-0051](0051-insert-the-owners-saved-prompts-into-the-composer-with-a-slash.md)).
- An image a model gets is fitted by `uploads.PrepareImage` ([ADR-0029](0029-attach-the-owners-files-to-a-message-stored-outside-the-data-folder.md)).

**The agents' homes hold more than reports.** They hold git checkouts, tool caches, dot folders (`.git`, `.stfolder`) and files an agent wrote, which may hold credentials. A private agent's home holds the owner's personal notes ([ADR-0042](0042-give-a-private-agent-its-own-preamble-and-no-team-in-its-sessions.md)). Homes hold symbolic links: a checkout's `CLAUDE.md -> AGENTS.md`, and links to folders outside the home.

**The desktop's Markdown view links code.** MyGo's `markdown.Options.CodeLink` makes a piece of code within text a link, and `OnLink` takes a click on it.

## Decision

1. **The roots are the profiles' homes** (`files.Roots`, protocol rule 70). Each valid profile with a `home` gives one root, by the profile's id, a private profile's included. A home that is, holds or lies inside pocketd's data folder, the profile folder, the skills folder, the uploads folder or the owner's global instructions file, compared as written and with links resolved, is no root. No other folder is read.
2. **Three read-only endpoints** (protocol rules 70 to 72, version 35): `GET /v1/files` lists the roots, with the service user's home folder for `~/`; `GET /v1/files/list?root=&path=` lists a folder; `GET /v1/files/read?root=&path=` reads a file. They write nothing, take no `requestId` and send no event. They sit behind the device token, as every route does; no tool of an agent's session calls them, and pocketd adds none that does.
3. **A path is checked in code first** (`proto.CheckFilePath`, shared by the desktop): at most 4,096 bytes of UTF-8 with no NUL; never absolute; cleaned with `path.Clean`; refused when the cleaned path is `..` or starts with `../`. A refused path is `bad_request`.
4. **Hidden and withheld names:**
   - A name that starts with a dot is hidden: never listed, never read, and a path that names one is refused.
   - A name that looks like it holds a secret is withheld (`proto.WithheldName`): listed with `withheld`, never opened, read or followed. In any case, it holds `token`, `secret`, `password`, `passwd`, `credential`, `apikey`, `api_key` or `api-key`; ends in `.key`, `.pem`, `.p12`, `.pfx`, `.jks`, `.keystore`, `.kdbx`, `.gpg` or `.env`; starts with `id_rsa`, `id_dsa`, `id_ecdsa` or `id_ed25519`; or is `auth.json`, `netrc` or `_netrc`.
5. **The walk runs in the sandbox, by descriptor** (`files.List`, `files.Read`, run by pocketd's hidden command `pig.ReadFilesHelper` through `Engine.ListFiles` and `Engine.ReadFile`):
   - from the root, each folder opened from the one above it, with no symbolic link followed by the kernel;
   - a link is read and its target followed only when the target is relative, at most 40 links a path (`files.MaxLinks`); a `..` steps back to a folder the walk holds open, and one above the root is refused, so the real path never leaves the root;
   - every name of the real path follows rule 4, so a link inside the home to a hidden or withheld name is refused;
   - a pipe, socket or device is never opened; a file is opened without waiting for a writer, and checked to be a regular file on the descriptor read;
   - a link out of the home, a loop, a dangling link, a real path through a hidden or withheld name, the wrong kind and nothing there are `not_found`, with why (`files.ErrOutside`, `files.ErrHidden`, `files.ErrNotFound`, `files.ErrNotFolder`, `files.ErrNotFile`);
   - the sandbox hides pocketd's data folder and `/run/user` from the helper, as from every command.
6. **Limits and content** (`proto` constants):
   - a listing gives at most 1,000 entries (`MaxFolderEntries`) by name in any case, then byte by byte, with `more` counting the rest; at most 10,000 names of one folder are read (`MaxFolderNames`), and `partial` says a folder held more;
   - a file over 5,242,880 bytes (`MaxFileImage`) is not read; a text file over 1,048,576 bytes (`MaxFileText`) is not sent: both are `large`;
   - the kind is decided from the real name and the bytes: `image` for `.png`, `.jpg`, `.jpeg`, `.gif` and `.webp`, fitted by `uploads.PrepareImage`, `binary` when it cannot be decoded or fitted, or for bytes that are not UTF-8 or hold a NUL; `markdown` for text named `.md` or `.markdown`; `text` for other text;
   - the answer is JSON (`application/json`): text as a string, an image's bytes as base64 with its `mimeType`, and a `note` for what is not shown;
   - a read runs for at most 15 seconds, and 4 run at once (`MaxFileReads`).
7. **The desktop's Files place** (rules in `desktop/internal/state`, drawn in `desktop/internal/app`, on the desktop, the iPad and the iPhone):
   - the sidebar's Files row and the command Browse Files… open it; on a compact window the More tab opens it;
   - it lists the roots, then a folder's entries, folders first, with breadcrumbs back up; a withheld entry and an entry of kind `other` do not open;
   - Markdown draws with the transcript's renderer, and a relative link in it opens the path from the file's folder in the same root; text draws in monospace; an image draws decoded; the other kinds show the name, the size, the time and the note;
   - a window wide enough shows the folder beside the file; a compact one shows the file in the folder's place, with a way back.
8. **A path in a message opens the viewer** (`state.FileLink`): code within a message's text links when it names a path below a root, either absolute or after `~/`, or relative in a session of a profile with a root whose home's top folder lists the path's first name; a link that `proto.CheckFilePath` refuses is none.

## Consequences

- The owner reads the files the team points to from any device signed in to pocketd, the iPad and the iPhone included, and opens one from a message with a click.
- A private agent's home is readable from every signed-in device. A lost device's token reads the homes until it is revoked.
- Withholding is by name: a secret in a file with an ordinary name shows, and a file with an ordinary word such as `tokenizer.md` is withheld. A hard link to a file outside the home is read as a file inside it.
- An absolute link inside the home is refused even when it points back inside it; a home with such links shows them as `other`.
- Dot files, such as `.gitignore` or a dot folder's notes, cannot be read in the viewer.
- A file over 1 MiB of text, or over 5 MiB, shows its name, size and time alone; the Terminal or a synced copy reads the rest.
- A relative path in a message links only once the session's root top folder is listed, and only to that root: a report naming a path in another agent's checkout does not link.
- pocketd decodes an agent's image in its own process, bounded as an upload's is.
- The change needs a pocketd deploy and a restart, and a rebuild of the desktop and iPad apps; a client of version 34 is refused.

## Alternatives considered

- **List every folder below `~/agents`:** a folder name in code that the public repository and a fresh host do not share; a folder with no profile is no agent's.
- **Follow every link and check the real path's prefix afterwards:** a folder swapped between the check and the read would lead elsewhere; the walk by descriptor never leaves the root.
- **Use `openat2` with `RESOLVE_BENEATH`:** Linux only, and the names of the real path would still need reading back through `/proc` to apply rule 4.
- **List hidden files too:** a home's dot folders hold git objects, caches and credentials; the files the team points to are not hidden.
- **Refuse withheld files without listing them:** a file that vanishes from its folder reads as a fault; a listed, withheld one says why it does not open.
- **Push the files as events:** the viewer reads what the owner opens, and a home changes far more often than the owner looks.

## References

- Shapes: [specs/protocol.md](../specs/protocol.md) (rules 70 to 72), [design/architecture.md](../design/architecture.md#service), [design/architecture.md](../design/architecture.md#desktop-client)
- Built by: [plans/roadmap.md](../plans/roadmap.md)
- Builds on: [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0012](0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md), [ADR-0013](0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md), [ADR-0029](0029-attach-the-owners-files-to-a-message-stored-outside-the-data-folder.md), [ADR-0040](0040-build-the-ipad-and-iphone-client-from-the-desktops-code-on-mygos-ios-target.md), [ADR-0042](0042-give-a-private-agent-its-own-preamble-and-no-team-in-its-sessions.md), [ADR-0044](0044-navigate-a-compact-window-by-a-bar-of-tabs.md), [ADR-0053](0053-show-a-sessions-changes-read-with-isolated-git-in-code.md)
