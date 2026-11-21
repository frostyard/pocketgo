# 0035 — Make the skills and uploads folders read-only to agents

- **Status:** Accepted
- **Date:** 2026-11-21

## Context

**Two of the owner's folders are writable by every agent.** Agents run as pocketd's own Unix user. The sandbox ([ADR-0012](0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md)) hides pocketd's data folder and the runtime folders, and keeps pocketd's start files and the profile folder read-only. Two folders the owner fills are writable:
- **The skills folder,** `~/.pi/agent/skills` by default (`-skills`, `POCKETD_SKILLS`). A gather reads `<name>/SKILL.md` from it and quotes the text in every asked agent's brief ([ADR-0033](0033-gather-proposals-from-the-team-in-read-only-sessions-in-code.md)). An agent that rewrote a skill would write the brief other agents get; ADR-0033 lists that as a consequence.
- **The uploads folder,** `~/.local/share/pocketgo-uploads`, where pocketd keeps the files the owner attaches ([ADR-0029](0029-attach-the-owners-files-to-a-message-stored-outside-the-data-folder.md)). An agent can change or remove an upload. ADR-0029 left a read-only uploads folder as an open question to the owner.

**The file tools run outside the sandbox.** PiG's read, write and edit run in pocketd's own process, through `guardedEnv`, which refuses the protected paths for every access. A folder the sandbox binds read-only stays writable to them unless `guardedEnv` refuses the change itself.

**The folders may be missing.** A host where no skill was ever synced has no skills folder. A host where the owner never attached a file has no uploads folder until the first upload. `bwrap` cannot bind a path that is not there, and the owner may remove the uploads folder by hand while pocketd runs: uploads stay until the owner removes them (ADR-0029).

**The owner decided on 2026-11-21.** Asked "should these folders be read-only to agents?", the owner answered yes for both.

## Decision

1. **The sandbox shows both folders read-only** (`pig.Sandbox.Shown`).
   - `pocketd serve` names the skills folder and the uploads folder.
   - Every command binds each read-only (`--ro-bind`), under every path a mount shows it at. On the VM, that is both `/home/team/…` and `/var/home/team/…`.
   - The binds come after pocketd's start files and before the masks, so a folder inside a hidden folder stays hidden.
   - Each folder above them that the user could rename is pinned, as the folders above the start files are.
2. **The file tools read them and refuse to change them.** `guardedEnv` reads in them as before. These fail with an error tool result, through a symbolic link too: a write, an append, a truncation, a flush or a new folder in them; a rename or removal of an entry in them; a rename into them; and a rename or removal of a folder that holds one. The refusal says that the folder is the owner's and read-only to agents.
3. **pocketd makes a missing folder, empty.** At its start, pocketd creates each folder that is missing, with mode 0700. So a host without them starts, and no command can create one first and fill it. Before each command, pocketd makes a folder that went missing again (`Sandbox.remakeShown`), so a folder the owner removed by hand stops no command and stays read-only. A folder that cannot be made stops the command. So does a symbolic link that now stands on the way to a folder, as one the owner put in its place while pocketd runs: the bind would follow it to a place the file tools do not guard. Commands run again after a restart, whose start refuses a link the user can change (decision 4).
4. **A link the user can change on the way stops the start**, as it does for pocketd's own protected paths (ADR-0012, decision 4). An agent could point such a link at a folder of its own, which the next start would show as the skills. A link only root can change is followed, and the folder it leads to is the one kept read-only. pocketd does not scan what lies inside a folder: it reads a skill as the sandbox shows it, and never reads an upload through a link (ADR-0029).
5. **The start check proves them read-only.** After process 1 and the hidden paths, the check runs root's `test -w` in the sandbox on every read-only path, pocketd's start files and the profile folder included, and on both folders. pocketd refuses to start unless each answers "not writable" (exit status 1). A path a mask hides is skipped: it is an empty `tmpfs` there, which writes nothing to the host. pocketd made both folders before the check, so a host's first start proves them too.
6. **pocketd itself works with them as before.** It writes and names uploads outside the sandbox (ADR-0029). A gather reads its skill with pocketd's own reader in the sandbox (ADR-0033), which needs no write.

## Consequences

- An agent can no longer change or remove an upload, or change a skill a gather quotes. It reads both as before.
- An agent cannot plant a link or a file in either folder. The uploads code's defenses against what an agent put there (ADR-0029) stay. They now cover what was planted before this change, and what the owner's own hand puts there.
- A skill the owner links from outside the skills folder is only as read-only as the file the link leads to. pocketd follows the link in the sandbox and does not check where it ends.
- The owner still adds, changes and removes skills and uploads from a terminal (ADR-0025, outside the sandbox) or over ssh. A skills sync that an agent's command ran for the owner no longer works.
- A skills folder reached through a link the user can change stops `pocketd serve`, for example `~/.pi/agent/skills` linked into a checkout. The fix is to name the folder itself with `-skills` or `POCKETD_SKILLS`. Either folder must stay a real folder while pocketd runs: the owner who moves one and leaves a link in its place stops the agents' commands until the restart.
- On its first start, pocketd creates `~/.pi/agent/skills` and `~/.local/share/pocketgo-uploads`, empty, when they are missing.
- A command can no longer rename or remove `~/.pi` or `~/.pi/agent`, as it cannot rename `~/.local/share`.
- The start check runs more trial commands: one for each read-only path and each of its mount aliases.
- An agent's command can still type into an open terminal of the owner's, which runs outside the sandbox. The owner accepted that exposure in ADR-0025, and this decision does not change it.
- The file tools check a path and then PiG opens it, so the race ADR-0012 accepts for the protected paths applies here too.

## Alternatives considered

- **Bind a folder only while it exists (`--ro-bind-try`):** a command could create a missing folder and plant a skill that the next gather reads.
- **Refuse to start without the folders:** a host that never synced skills, or never received an upload, would not start. The owner's request was that it does.
- **Hide the folders:** agents must read an upload at the path its message names, and may read the skills.
- **Make all of `~/.pi` read-only:** it covers more than the owner asked for.
- **Follow a skills folder reached through a user's link, and read where it led at the start:** a link changed while pocketd runs would not reach this run, but it would choose the next run's skills.
- **Refuse a skill whose file lies outside the read-only folder:** not asked for. It is a question for the owner.

## References

- Shapes: [design/architecture.md](../design/architecture.md#operational-notes), [specs/protocol.md](../specs/protocol.md) (rules 10a, 14e and 57)
- Built by: [plans/roadmap.md](../plans/roadmap.md) (item 18, read-only folders and upright photos)
- Builds on: [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0012](0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md), [ADR-0025](0025-give-the-owner-a-shell-on-the-service-host-from-the-desktop.md), [ADR-0029](0029-attach-the-owners-files-to-a-message-stored-outside-the-data-folder.md) (its open question of a read-only uploads folder), [ADR-0033](0033-gather-proposals-from-the-team-in-read-only-sessions-in-code.md) (its consequence that an agent could change a skill)
