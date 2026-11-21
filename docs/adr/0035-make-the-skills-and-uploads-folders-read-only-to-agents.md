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
   - `pocketd serve` names the skills folder and the uploads folder. From then on, the gathers and the uploads use each folder as the sandbox keeps it, links resolved, so a link retargeted later leads none of them elsewhere.
   - Every command binds each read-only (`--ro-bind`), under every path a mount shows it at. On the VM, that is both `/home/team/…` and `/var/home/team/…`. Those paths come from the mount a folder is on as the kernel walks to it, never from a mount another covers, and each is a path where the mount that shows the folder is the one seen. A path that shows a single file of a folder is bound and held as that file. A mount table that shows no mount a protected path is on, as one read in a chroot that is no mount point, stops the start.
   - The binds come after pocketd's start files and before the masks, so a folder inside a hidden folder stays hidden.
   - Each folder above them that the user could rename is pinned, as the folders above the start files are. So is each folder above another path a mount shows a protected tree at, for the hidden and the read-only paths too: renamed, it would take that alias out from under its mask.
2. **The file tools read them and refuse to change them.** `guardedEnv` reads in them as before. These fail with an error tool result, through a symbolic link too: a write, an append, a truncation, a flush or a new folder in them; a rename or removal of an entry in them; a rename into them; and a rename or removal of a folder that holds one. The refusal says that the folder is the owner's and read-only to agents. The tools watch the paths exactly as the sandbox checked them, never resolved again. While decision 3 stops commands, the tools change nothing either. While a link leads a folder's path elsewhere, as one the owner left in the place of a folder they moved while pocketd runs, the file tools change nothing at all until a restart, as no command runs then (decision 3): the folder the link leads to may show under other paths a mount gives it, which only the start's mount table named.
3. **pocketd keeps each folder where it found it** (`Sandbox.keepShown`).
   - At its start, pocketd creates each folder that is missing, with mode 0700. So a host without them starts, and no command can create one first and fill it.
   - A folder removed or renamed outside the sandbox takes the read-only binds of the commands running then with it: the kernel detaches a mount whose point goes, and a new folder at that path is writable to those commands. So pocketd looks at both folders before each command and every quarter of a second (`Sandbox.watchShown`). When one is missing, is another folder than the one it kept, or has a link on the way to it, pocketd first stops every command running in the sandbox, by killing each `bwrap` it started, which takes the command's whole process namespace with it.
   - pocketd signals a `bwrap` only through a pidfd it opened before it checked the process again, and only a `bwrap` that carries this sandbox's random mark (`--setenv POCKETD_SANDBOX`). So a `bwrap` the owner starts from a terminal is never stopped. It holds a pidfd of the first process of each sandbox's namespace too, and kills it alongside the `bwrap`. The stop is done only once all of them have ended, at most 10 seconds later. When that first process has ended, the kernel has already ended every other process of its namespace.
   - A kernel that opens no pidfd (before Linux 5.3) gets no signal, and the stop fails. So does a stop where pocketd cannot look at a process again, or where one has not ended in time. The next stop waits for that one again.
   - Besides each folder, pocketd keeps the pinned folders above it. If one of those is replaced, with the shown folder moved into the new one, the running commands' binds went with the old one, so it is handled as a replaced folder.
   - A change that asks for a stop gets it before anything else is reported, a mount table that cannot be read included. A stop that failed is done again at every look until one succeeds, even once what asked for it is gone. A sandbox is held whole, its `bwrap` and its first process, or not at all, so a sandbox left running is found again.
   - pocketd holds each path with `O_PATH`, for its identity alone, so a folder it may pass through but not read stops nothing.
   - Every command starts through pocketd's own gate inside the sandbox (`pig.GateHelper`). Before it runs the command, the gate reads the sandbox's mount table and refuses (exit status 125) unless every path a mount shows a folder, or a part of one, at is read-only there. So a command admitted just before a new mount, and started just after, does not run with binds that miss it.
   - Each kept folder is held open, so its inode cannot be used again while it is kept: a folder made at its path after it went is always told apart. pocketd opens it without following a link and without waiting on a named pipe.
   - Only once the stop succeeded does pocketd make a missing folder again, empty, or keep another folder from then on. A stop that failed is tried again at the next look. So a folder the owner removed by hand stops no later command and stays read-only.
   - At every look, pocketd reads the mount table again. Commands and the file tools' changes stop, until that mount goes or a restart binds it too, if a mount shows a folder, or a part of one, somewhere the sandbox does not keep read-only. That covers a replacement bind-mounted elsewhere before the owner moved it in, and a mounted folder the owner moved into a shown one. They also stop if the table shows no mount a folder is on.
   - pocketd makes a missing folder it named before it looks at the paths a mount showed that folder at. Those paths come back with the folder, as `/home/team/…` does once `/var/home/team/…` is made again.
   - A folder that cannot be made stops the command. So does a link on the way to a folder, which the bind would follow to a place the file tools did not guard before decision 2. Commands run again after a restart, whose start refuses a link the user can change (decision 4).
4. **A link the user can change on the way stops the start**, as it does for pocketd's own protected paths (ADR-0012, decision 4). An agent could point such a link at a folder of its own, which the next start would show as the skills. A link only root can change is followed, and the folder it leads to is the one kept read-only. pocketd does not scan what lies inside a folder: it reads a skill as the sandbox shows it, and never reads an upload through a link (ADR-0029).
5. **What agents could change before is not taken on trust.** A hard link an agent made before this decision, outside the read-only folder, could still change the file, and an agent could change a file before the folder was read-only. No command can make a new hard link into either folder: a link across mounts fails.
   - An upload goes to a model only as the bytes it was uploaded with: pocketd checks them against the SHA-256 it kept when it staged the file (`uploads.ReadUpload`). Otherwise the line notes that the file changed since it was uploaded.
   - A gather refuses a skill whose `SKILL.md` has more than one name, as pocketd refuses such a profile (ADR-0013). pocketd's reader in the sandbox answers `linked` for it (`api.ErrManyNames`), and `pocketd profiles` says so too.
   - A skill has no record of what it was, so pocketd cannot tell one an agent changed before this decision. The owner syncs the skills folder again once this is deployed, from the configuration they keep, which restores trust in the skills.
6. **The start check proves them read-only.** After process 1 and the hidden paths, the check reads the sandbox's own mount table (`/proc/self/mountinfo`, with root's `cat`). It refuses to start unless every read-only path, pocketd's start files and the profile folder included, and both folders lie on a mount whose own options are `ro`, found as the kernel walks to the path. A permission that refuses a write proves nothing of the files below, and a probe that did not run proves nothing at all. A path a mask hides is skipped: it is an empty `tmpfs` there, which writes nothing to the host. pocketd made both folders before the check, so a host's first start proves them too.
7. **pocketd itself works with them as before.** It writes and names uploads outside the sandbox (ADR-0029). A gather reads its skill with pocketd's own reader in the sandbox (ADR-0033), which needs no write.

## Consequences

- An agent can no longer change or remove an upload, or change a skill a gather quotes. It reads both as before.
- An agent cannot plant a link or a file in either folder. The uploads code's defenses against what an agent put there (ADR-0029) stay. They now cover what was planted before this change, and what the owner's own hand puts there.
- A skill the owner links from outside the skills folder is only as read-only as the file the link leads to. pocketd follows the link in the sandbox and does not check where it ends.
- The owner still adds, changes and removes skills and uploads from a terminal (ADR-0025, outside the sandbox) or over ssh. A skills sync that an agent's command ran for the owner no longer works.
- A skills folder reached through a link the user can change stops `pocketd serve`, for example `~/.pi/agent/skills` linked into a checkout. The fix is to name the folder itself with `-skills` or `POCKETD_SKILLS`. Either folder must stay a real folder while pocketd runs: the owner who moves one and leaves a link in its place stops the agents' commands until the restart.
- On its first start, pocketd creates `~/.pi/agent/skills` and `~/.local/share/pocketgo-uploads`, empty, when they are missing.
- A command can no longer rename or remove `~/.pi` or `~/.pi/agent`, as it cannot rename `~/.local/share`.
- When the owner removes or replaces either folder while pocketd runs, every command running in the sandbox is killed, the agents' and pocketd's own git and `gh` alike, and its tool call fails. Change the folders' contents in place, or stop pocketd first, to keep running commands alive.
- What a command running at that moment puts in the new folder, in the quarter of a second before pocketd sees the change, stays there. Without root, pocketd cannot keep a folder the owner replaces read-only to a sandbox that started before the change. A command must already be running, and race the owner's change.
- A skill that has another name stops working until the owner removes that name or copies the file into the folder anew. An upload whose bytes changed is named in its message, but its image goes to no model.
- After deploying this, the owner syncs the skills folder once again (decision 5).
- The start check reads one more file in the sandbox, its mount table.
- An agent's command can still type into an open terminal of the owner's, which runs outside the sandbox. The owner accepted that exposure in ADR-0025, and this decision does not change it.
- The file tools check a path and then PiG opens it, so the race ADR-0012 accepts for the protected paths applies here too.

## Alternatives considered

- **Bind a folder only while it exists (`--ro-bind-try`):** a command could create a missing folder and plant a skill that the next gather reads.
- **Refuse to start without the folders:** a host that never synced skills, or never received an upload, would not start. The owner's request was that it does.
- **Hide the folders:** agents must read an upload at the path its message names, and may read the skills.
- **Make all of `~/.pi` read-only:** it covers more than the owner asked for.
- **Follow a skills folder reached through a user's link, and read where it led at the start:** a link changed while pocketd runs would not reach this run, but it would choose the next run's skills.
- **Refuse a skill whose file lies outside the read-only folder:** not asked for. It is a question for the owner.
- **Prove a folder read-only with `test -w`:** a folder of mode 0555 on a writable mount passes, though its files can change, and `bwrap` failing to start exits 1 as `test -w` does for "not writable".
- **Stop only the commands started before a change:** pocketd does not know when the owner's change came, so it stops every command that runs when it sees one.

## References

- Shapes: [design/architecture.md](../design/architecture.md#operational-notes), [specs/protocol.md](../specs/protocol.md) (rules 10a, 14e and 57)
- Built by: [plans/roadmap.md](../plans/roadmap.md) (item 18, read-only folders and upright photos)
- Builds on: [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0012](0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md), [ADR-0013](0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md) (a profile with another name is no profile), [ADR-0025](0025-give-the-owner-a-shell-on-the-service-host-from-the-desktop.md), [ADR-0029](0029-attach-the-owners-files-to-a-message-stored-outside-the-data-folder.md) (its open question of a read-only uploads folder), [ADR-0033](0033-gather-proposals-from-the-team-in-read-only-sessions-in-code.md) (its consequence that an agent could change a skill)
