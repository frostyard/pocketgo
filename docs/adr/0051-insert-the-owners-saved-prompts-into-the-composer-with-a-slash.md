# 0051 — Insert the owner's saved prompts into the composer with /

- **Status:** Accepted
- **Date:** 2026-11-21

## Context

**The owner types the same requests again and again.** "Review this PR and fix what you find", "ship it: rebase, run make verify, open the PR" and the night shift's muster are typed by hand each time. frostyard/pocketgo#100, filed by the owner, asks for saved prompts the composer inserts with `/`.

**Agents have skills; the owner has nothing of the kind.** pocketd lists the skills of the owner's skills folder in every session's instructions ([ADR-0043](0043-load-the-global-and-folder-agents-md-and-the-skills-into-every-session.md)). Those are the agents' instructions, not text the owner sends.

**The profile folder already holds the owner's own files, checked in code.** `workers.json`, `providers.json` and `schedules.json` sit beside the profiles. `service/internal/profiles` reads each through the folder pocketd pinned at start, checks every field, and refuses a symbolic link, a hard link and a named pipe ([ADR-0013](0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md), [ADR-0022](0022-run-any-session-on-any-sign-in-and-choose-workers-from-the-owners-list.md), [ADR-0030](0030-send-scheduled-messages-from-the-owners-file-and-the-supervisors-tool-in-code.md)). The agents' sandbox shows the folder read-only ([ADR-0012](0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md)). Real profiles stay in the owner's private configuration repository; this repository carries examples only.

**The composer is the desktop's, and so are its rules.** `desktop/internal/state` holds the client's rules with no UI import, and `desktop/internal/app` draws them ([ADR-0010](0010-port-the-desktop-client-selectively-from-frostyard-pocket.md)). The phone draws the same composer in a compact window ([ADR-0044](0044-navigate-a-compact-window-by-a-bar-of-tabs.md)). While a session's question waits, the composer answers it ([ADR-0048](0048-put-the-supervisors-and-a-private-agents-questions-to-the-owner-inline-in-code.md)).

## Decision

1. **A saved prompt is a Markdown file in the profile folder: `prompts/<name>.md`.**
   - The name is the file's name without `.md`: 1 to 64 lowercase letters, digits and dashes, starting with a letter or a digit.
   - The file may start with front matter between `---` lines: one YAML map whose only fields are `description` (at most 300 characters) and `argument-hint`, what may follow the name (at most 100). Each is text, read on one line, never named twice. Any other field makes the file invalid.
   - The text is the rest of the file, without the white space around it. It is not empty, holds no control character but tabs and line breaks, and the file is at most 16 KiB of UTF-8.
   - `profiles.Folder.Prompts` reads the folder at each call, so a change shows without a restart. It reads through the pinned folder, as a profile is read: `prompts` is a folder of its own, never a link, and each file a regular file of one name, never read through a link nor waited on as a named pipe.
   - Files that end in `.md`, but those whose name starts with a dot, are prompts, by name (without `.md`), at most 100. A file that breaks a rule is listed with its problem, and the others still work. A folder that cannot be read lists none, and says why. No folder is no prompts.
   - `pocketd profiles` reports them too.
2. **`GET /v1/prompts` lists them** (protocol version 27, rule 63): each prompt's `name`, `description`, `argumentHint` and `text`, or its `name` and `problem`; and the folder's `problem`. It is read only, and has no event: the desktop fetches it.
3. **The composer inserts a prompt; nothing is sent on its own.** The rules are `state`'s, each with its test:
   - **When the picker opens:** the message's first line starts with `/` at the very start, then letters, digits and dashes, the name typed so far, then the end of the line or any white space, a pasted no-break space included, before the arguments (`ParseSlashHead`). A path such as `/etc/hosts`, or text before the `/`, opens nothing. An answer to a question takes no prompt, as it takes no file.
   - **Filtering:** the valid prompts whose name starts with what was typed come first, then those that hold it elsewhere, each by name, in any case (`PromptMatches`). Files with a problem show below them, with why, and cannot be chosen.
   - **Keys:** Up and Down move the highlight, around at the ends. The prompt shown highlighted is the one Enter takes: kept by name while the name typed stays, as a list that comes later changes, and the best match of a new name. One that a newer list no longer offers is never swapped for another under the key: Enter and Tab take nothing until the next frame shows what takes its place. Tab or Enter puts the highlighted prompt in. With nothing to put in, Enter does nothing, so a call typed before its prompt shows is never sent as a message; Tab moves the focus. The Enter that put a prompt in, in the field or on a row the keyboard chose, takes its own repeats until it is let go, so holding Enter never sends what it put in; a press of its own, seen as the key comes, sends. Escape closes the picker and leaves the text as typed. It stays closed while the message's first line starts with the call's line as it was then, so typing on keeps it closed; erasing into the name, or typing over the message, opens it again.
   - **The text that ends up in the composer** (`InsertPrompt`): the call's line becomes the prompt's text, every `$ARGUMENTS` in it replaced by what followed the name on that line, and the lines below the call stay. A prompt without `$ARGUMENTS` keeps those words below its text, after a blank line, so nothing typed is lost. The caret goes after the prompt's text, the field takes the keys, and one Undo takes the prompt back out. Text typed before the prompt went in follows it. A draft with no call, when the phone's sheet inserts a prompt, stays below it. A text past what a message takes, 262,144 bytes, is counted before it is built and never goes in: the draft stays, and the picker says why.
   - **Freshness:** the store loads the prompts when it connects, and the picker asks again each time it opens or shows again, as when its session is opened again, so a prompt added to the folder shows the next time the owner types `/`.
4. **The desktop draws it in three places.** The picker sits over the composer's field, with each prompt's name, argument hint and description. The command palette gets Insert Prompt…, which puts a `/` of its own above the draft and opens the picker. On the phone, a button beside Attach opens the same list as a sheet, which closes when the composer turns to answering a question.
5. **The repository carries three example prompts** in `examples/profiles/prompts/`, which a test reads as `pocketd` does. The owner's real prompts stay in their private configuration repository.

## Consequences

- The owner keeps recurring requests as files they edit and version with the rest of their configuration. A change shows at the next `/`.
- A prompt never leaves without the owner: it lands in the composer, where it is edited and sent like typed text. A slip of Enter on an unfinished call sends nothing.
- Agents can read the prompts, as they read the rest of the profile folder, but cannot change them. The prompts are not secret.
- A file with a typo shows in the picker and in `pocketd profiles` with its problem, instead of vanishing.
- The protocol goes to version 27. An older desktop and a newer pocketd refuse each other, as always.
- The picker loads the list again at each opening: one small read of the folder each time. 100 prompts of 16 KiB bound the answer to about 1.6 MiB.

## Alternatives considered

- **Prompts on the desktop alone:** they would be lost to the phone and to another desktop. The profile folder is already the owner's one place for configuration.
- **Send the prompt at once on Enter:** the owner asked that nothing is ever sent on its own, so a prompt is always text to edit first.
- **More placeholders (`$1`, named arguments):** `$ARGUMENTS` is what the owner asked for. More can come later, with a new ADR.
- **An event when the folder changes:** pocketd does not watch the folder for profiles either. Asking again when the picker opens shows a change at the moment it matters.

## References

- Shapes: [specs/protocol.md](../specs/protocol.md) (rules 1 and 63), [design/architecture.md](../design/architecture.md#service), [design/architecture.md](../design/architecture.md#desktop-client)
- Built by: [plans/roadmap.md](../plans/roadmap.md) (item 23, the owner's saved prompts)
- Builds on: [ADR-0010](0010-port-the-desktop-client-selectively-from-frostyard-pocket.md), [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0012](0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md), [ADR-0013](0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md), [ADR-0022](0022-run-any-session-on-any-sign-in-and-choose-workers-from-the-owners-list.md), [ADR-0030](0030-send-scheduled-messages-from-the-owners-file-and-the-supervisors-tool-in-code.md), [ADR-0043](0043-load-the-global-and-folder-agents-md-and-the-skills-into-every-session.md), [ADR-0044](0044-navigate-a-compact-window-by-a-bar-of-tabs.md), [ADR-0048](0048-put-the-supervisors-and-a-private-agents-questions-to-the-owner-inline-in-code.md)
