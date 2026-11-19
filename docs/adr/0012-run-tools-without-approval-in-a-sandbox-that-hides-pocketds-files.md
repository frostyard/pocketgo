# 0012 — Run tools without approval, in a sandbox that hides pocketd's files

- **Status:** Accepted
- **Date:** 2026-11-19

## Context

Phase 1 made every `bash`, `write` and `edit` call wait for the owner (`approvals.NeedsApproval`). The owner runs agents to get work done without watching them. In the pi-pocket installation this replaces, the Lancet guard was never switched on, so no call ever waited. The owner treats the team VM as the hard boundary: it is isolated, and it holds no GitHub token of the owner's.

Some things on the VM are worth protecting from the agents themselves:
- **pocketd's data folder.** Its `auth.json` holds the owner's subscription sign-ins, and its databases hold every transcript.
- **The user's runtime folder.** Its D-Bus socket reaches the user's keyring and systemd manager.
- **The files that decide how pocketd starts.** These are its program, its systemd unit, its environment file and the user's other systemd configuration.

The file tools already refuse the data folder (`guardedEnv`). But a command runs as the same user as pocketd, and the user owns the folders above all of these paths. Without approvals, a prompt injection from a repository or a web page could:
- copy the sign-ins off the VM
- change what pocketd runs at its next start
- reach the keyring
- rename a folder such as `~/.local` to move a protected path out from under any rule written against its old path

On the VM, `/home` is a bind mount of `/var/home`, so every file in the service user's home is reachable under two paths.

The VM has bubblewrap (`bwrap`) 0.12 and unprivileged user namespaces. Without root, `bwrap` can replace a folder with an empty `tmpfs`, bind a path read-only, bind a folder onto itself, and give a command its own process namespace. The Linux kernel refuses to rename or remove a mount point, in every namespace.

The owner also considered specpi-jev-guard. It settles obvious commands with local rules and asks TypeSafe's Jev classifier, a hosted early-access API, about the rest: high scores block, and the middle band asks the owner.

## Decision

1. **No tool call waits for the owner.** `approvals.NeedsApproval` selects no tool. The approval machinery stays, because the merge gates (Phase 2) and later policies use it: the stored decision, the Inbox, the cards, and approvals surviving a restart. A call that asked for an approval under an earlier rule still waits for it, and a decision made for it applies. A future rule changes `NeedsApproval`, in code, with its test (ADR-0011).
2. **Every command a tool runs goes through `bwrap`**, from `service/pig`'s guarded environment:
   - the data folder is an empty `tmpfs`, and so is `/run/user`, which holds every user's runtime folder. A runtime folder that a login creates while a command runs stays hidden from it too. An `XDG_RUNTIME_DIR` outside `/run/user` is hidden as well.
   - each protected path is masked under every path a mount shows it at, read from `/proc/self/mountinfo`: on the VM, both `/var/home/team/…` and `/home/team/…`
   - pocketd's program, `~/.config/pocketd.env`, and the user's systemd configuration are read-only. The systemd configuration is `systemd` and `environment.d` under the default and XDG configuration folders, and `systemd` under the data folders. pocketd creates any of these that are missing, empty, so a command cannot create one first.
   - each folder above those paths that the user could rename is bound onto itself. A mount point cannot be renamed or removed, so the paths stay where the masks cover them, from one command to the next.
   - the command sees only its own processes (`--unshare-pid`, a new `/proc`), and dies with pocketd
   - everything else stays as it is, so agents keep their tools, repositories, caches and network
3. **The file tools refuse the same paths.** This covers every mount alias, and a symbolic link that points to a protected path that does not exist yet, which creating the file would follow. A read, write or edit of a hidden or read-only path fails, and a rename or removal refuses any folder that holds one.
4. **pocketd refuses a protected name that it cannot pin down.** If a symbolic link the service user can change lies on the way to the data folder, the environment file or a systemd folder, pocketd does not start. The user could point that link elsewhere.
5. **Only a trusted `bwrap` runs.** pocketd looks for `bwrap` in the system's folders, never on the `PATH`. It follows each link to the program and uses it only if root alone can change every folder along the way, every link and the program itself.
6. **pocketd proves the sandbox before it starts.** Right after taking its lock, before it binds the port or opens a task, `pocketd serve` runs `bwrap` with no shell and the commands' own environment. The sandbox must show `bwrap` as process 1 and every hidden path empty. A fake `bwrap`, or a mount the masks missed, stops the start. `pig.Open` refuses to run without a sandbox unless a test asks for that.
7. **Sessions lose the old promise.** A session created while approvals waited carries instructions that promised them. When pocketd starts watching it, generated text that is still unchanged becomes the current text.

## Consequences

- Agents run without stopping for the owner. The Inbox stays empty until a gate uses it.
- A command cannot read the sign-ins or the transcripts, reach the user's D-Bus, keyring or systemd manager, or see or signal pocketd. It cannot change pocketd's program, unit, settings or the user's systemd units, and it cannot move them aside.
- The user's `~/.config`, `~/.local`, `~/.local/share` and `~/.local/state` can no longer be renamed or removed from a command. Their contents stay writable, except the protected paths.
- The host needs bubblewrap installed by the system and unprivileged user namespaces. pocketd is Linux-only; `pocketd serve` fails elsewhere.
- Everything else the user `team` owns is still writable: repositories, the rest of its configuration, and its home folder. A command can also reach the network. That is the accepted risk of treating the VM as the boundary.
- The file tools check a path, then PiG opens it. A command that swaps a symbolic link between the two could still lead a file tool into a protected path. The check runs on every call, and the swap needs a second process racing it. Closing this for good means opening the file first and checking what was opened, which PiG's file methods do not offer yet.
- The same user still runs pocketd and the commands. A file outside the protected list that some later process runs as `team` could still carry a change from an agent into that process, outside the sandbox: for example, a shell startup file that an interactive login as `team` sources. `cron` and `at` are not installed on the VM. A separate user for commands would close this, at the cost of root setup on the host.
- Tests that need a sandbox skip only on a host where `bwrap` cannot make one, such as CI runners whose AppArmor forbids user namespaces. Where it can, a failing sandbox fails the test. The sandbox's command line and paths are pure functions, with tests that run everywhere.

## Alternatives considered

- **Keep approvals for `bash`, `write` and `edit`:** the owner would approve nearly every step, and the agents would wait on the owner, which defeats running them.
- **A Jev score through specpi-jev-guard:** it needs a separate key and the network, and it fails closed, so an outage stops every agent. Its middle band still asks the owner, and the announcement post publishes no accuracy figures. A score with thresholds in code would fit ADR-0011, so it can come later behind `NeedsApproval`, in a mode that only blocks.
- **Approvals off with no sandbox:** a single injected command could copy the subscription sign-ins.
- **A read-only filesystem with explicit writable folders:** it protects more, but every tool that writes outside the working directory breaks: caches, package managers, toolchains. The pins keep the paths fixed without that cost.
- **Move pocketd to root-owned paths, as a system service:** this needs no pins, but it means a data migration, a different deploy and different commands for the owner. The pins give the same guarantee for the paths in the user's home.
- **A separate user for commands:** closes the same-user paths above, but needs root on the host and changes file ownership in every repository. Revisit if the residual paths matter.

## References

- Shapes: [design: system architecture](../design/architecture.md#service), [operational notes](../design/architecture.md#operational-notes), [specs: protocol](../specs/protocol.md), [plans: roadmap](../plans/roadmap.md)
- Builds on: [ADR-0004](0004-build-the-service-on-pigs-durable-harness.md), [ADR-0005](0005-keep-the-agent-loop-and-model-credentials-in-the-service.md), [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md)
