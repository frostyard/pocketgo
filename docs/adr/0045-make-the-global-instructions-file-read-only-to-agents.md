# 0045 — Make the global instructions file read-only to agents

- **Status:** Accepted
- **Date:** 2026-11-21

## Context

**Agents can write the file every new session's instructions hold.** pocketd loads the owner's global instructions file into every new session, right after its preamble ([ADR-0043](0043-load-the-global-and-folder-agents-md-and-the-skills-into-every-session.md)). The `-instructions` flag names it; without the flag, `$POCKETD_INSTRUCTIONS` names it, else `~/.pi/agent/AGENTS.md`. The sandbox ([ADR-0012](0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md)) keeps only the skills folder of `~/.pi/agent` read-only ([ADR-0035](0035-make-the-skills-and-uploads-folders-read-only-to-agents.md)). An agent that rewrote the file, with a command or with `write` or `edit`, would write the instructions of every later session, every agent's. ADR-0043 lists this as a known limit (frostyard/pocketgo#87).

**ADR-0035 closed the same hole for the skills and uploads folders** (`Sandbox.Shown`):
- the sandbox binds each read-only under every path a mount shows it at, and pins the folders above it;
- pocketd makes a missing one at its start, and keeps each where it found it while it runs (`Sandbox.keepShown`, `Sandbox.watchShown`), held open;
- the gate in the sandbox and the start check prove each on a read-only mount;
- the file tools read each and refuse every change, by identity too.

The code holds a single file a mount shows, but every folder pocketd names must be a folder.

**What writes under `~/.pi/agent` today** (seen on the team VM on 2026-11-21):
- The owner's install script of the private configuration writes `AGENTS.md`, a copy `AGENTS.md.bak-<time>` of each file it changes, the skills and `bin/`. The owner runs it from a terminal, outside the sandbox.
- Pi's own programs wrote `auth.json` on 2026-11-20, and `settings.json`, `mcp-auth.json`, `models-store.json` and `sessions/` on 2026-11-19. Pi's service is stopped, but an agent's command can still run a Pi program, which writes there.
- The lease tool, `~/.pi/agent/bin/lease`, runs from there and keeps its leases in `~/.local/state/leases`.
- pocketd keeps its own sign-ins in its data folder, not there.

**How the install script writes the file:** when the file differs, it saves a copy with `cp -p`, then runs `cp` onto the file. `cp` onto an existing file writes the same file, its inode, in place. Only a missing file is made with `install -m`.

**What a bind of a file shows.** `bwrap --ro-bind` binds the file found at the path when the command starts: its inode, not the path. Checked with bwrap on the team VM on 2026-11-21:
- A write in place on the host shows at once in every sandbox, read-only there.
- A rename over the path, or a removal, on the host takes the bind of every command running then with it: the kernel detaches a mount whose point is unlinked in another mount namespace. Those commands then find the new file at the path, writable.

**The owner put #87 first** on 2026-11-21, before the other work on pocketgo.

## Decision

1. **Scope: the file alone.** The sandbox keeps read-only the one file the instructions name, not `~/.pi/agent`.
   - Agents' commands still write under `~/.pi/agent`: Pi's programs write their sign-ins and settings there, and a sign-in that cannot be refreshed breaks the command that runs one.
   - Hiding or keeping Pi's files read-only is a decision of its own: it is not about what a session's instructions hold.
2. **The sandbox keeps the file as it keeps a shown folder.** The file is a shown root that is a single file (`Sandbox.Shown`, `Sandbox.files`). ADR-0035's decisions 1, 3, 4 and 6 apply to it as they do to a folder:
   - every command binds it read-only (`--ro-bind`), under every path a mount shows it at, after pocketd's start files and before the masks;
   - each folder above it that the user could rename is pinned, so a command cannot move a folder on its way, or put a link in its place;
   - pocketd looks at it before each command and every quarter of a second, held open, with the pinned folders above it;
   - the gate refuses a command unless every path a mount shows the file at is read-only in the sandbox;
   - the start check proves it on a read-only mount;
   - a symbolic link the user can change on the way stops the start.
   pocketd reads the file as the sandbox keeps it, links resolved: `newSandbox` rewrites the path `-instructions` named, and every session reads that path (ADR-0043, decision 7).
3. **A missing file is made, empty.** At its start, pocketd makes the file when it is missing: an empty regular file, mode 0600, and each missing folder above it, mode 0700. It walks from the root without following a link, and it never truncates a file that is there. So no command can make the file first and fill it. The same happens when the owner removes the file while pocketd runs: once the commands are stopped (decision 4), the file is made again, empty. An empty file is no part of the instructions (ADR-0043, decision 2).
   - The path must hold a regular file. A folder or any other kind of file there stops the start. One put there while pocketd runs refuses every command until a regular file is there again, or a restart.
4. **The owner replaces the file as before.**
   - A write in place, as the install script's `cp`, keeps the same file. pocketd stops nothing. Running commands see the new text, read-only, and the next new session reads it.
   - A replacement by rename, or a removal, is a replaced shown root (ADR-0035, decision 3). pocketd first stops every command running in the sandbox, whose binds went with the old file. Only then does it keep the new file, held open. Every command after that, and the next new session, read the new file.
   - A standing session takes the new text at its next Start Over, as ADR-0043 decided.
5. **The file tools read the file and refuse to change it.** `guardedEnv` reads it as before. These are error tool results, through a link too:
   - a write, an append, a truncation or a flush of the file, which covers `write` and `edit`;
   - a rename onto it, and a removal or rename of it;
   - a rename or removal of a folder that holds it.
   A change through another name of the file (a hard link) is refused by its identity. The refusal says that the file holds the owner's rules for every session and is read-only to agents (`errReadOnlyFile`).
6. **A hard link made before is not taken on trust.** An agent could have linked the file under another name before this decision, and change it through that name. A global file with more than one name is left out of a session's instructions, with a note, as a global skill with more than one name is (`team.GlobalSection`, ADR-0043). A command cannot make a new hard link to it, since a link across mounts fails.
7. **The rule follows the flag.** The file kept read-only is the one the instructions name: `-instructions FILE`, else `$POCKETD_INSTRUCTIONS`, else `~/.pi/agent/AGENTS.md`. `-instructions ""` names none, and the sandbox keeps no file for it. A path inside a folder the sandbox hides is masked there, as before: agents see nothing at it, and no session holds it.
8. **No wire change.** No type, path or event changes, so the protocol version stays.

## Consequences

- An agent can no longer change the global instructions file, through a command or the file tools. It still reads the file.
- The owner's install script, run from a terminal (ADR-0025) or over ssh, still updates the file in place, and the next new session holds the new text. Run from an agent's command, it can no longer write the file, as it can no longer write the skills folder or the profile folder.
- An editor or a script that replaces the file by rename while pocketd runs kills every command running in the sandbox, the agents' and pocketd's own git and `gh` alike. Writing the file in place keeps them alive.
- The file must stay a regular file. A link the user can change on the way stops `pocketd serve`, for example `~/.pi/agent/AGENTS.md` linked into a checkout: name the file itself with `-instructions`. A link put there while pocketd runs stops every command until a restart.
- On its first start, pocketd creates `~/.pi/agent/AGENTS.md`, empty, when it is missing.
- What a command running at the moment of a rename writes in the new file, in the quarter of a second before pocketd sees the change, stays (ADR-0035).
- A global file with another name is left out of every new session until the owner removes that name, or copies the file anew.
- The rest of `~/.pi/agent` stays writable to agents, Pi's `auth.json` included, and is not hidden.

## Alternatives considered

- **Keep all of `~/.pi/agent` read-only:** Pi's programs that agents' commands run write their sign-ins and settings there. It also covers more than the issue asks for.
- **Make the file a read-only path, as `~/.config/pocketd.env` is:** the file tools would refuse to read it. Nothing would watch it, so a replacement by rename would leave the commands running then a writable file.
- **Bind the file only while it exists (`--ro-bind-try`):** a command could make the missing file first, and write the next session's instructions.
- **Refuse to start without the file:** a host with no global file would not start, though ADR-0043 reads a missing file as nothing.
- **Bind an empty placeholder while the file is missing:** the file the owner makes later would show at the path of every command running then, writable. pocketd would also have to change its binds while it runs. Making the file empty does what ADR-0035 does for a folder.

## References

- Shapes: [design/architecture.md](../design/architecture.md#operational-notes), [specs/protocol.md](../specs/protocol.md) (rule 14e)
- Built by: [plans/roadmap.md](../plans/roadmap.md) (Phase 2, item 20)
- Builds on: [ADR-0012](0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md), [ADR-0025](0025-give-the-owner-a-shell-on-the-service-host-from-the-desktop.md), [ADR-0035](0035-make-the-skills-and-uploads-folders-read-only-to-agents.md), [ADR-0043](0043-load-the-global-and-folder-agents-md-and-the-skills-into-every-session.md) (its known limit)
