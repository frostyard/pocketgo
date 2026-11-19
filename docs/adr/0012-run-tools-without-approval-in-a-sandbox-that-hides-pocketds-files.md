# 0012 — Run tools without approval, in a sandbox that hides pocketd's files

- **Status:** Accepted
- **Date:** 2026-11-19

## Context

Phase 1 made every `bash`, `write` and `edit` call wait for the owner (`approvals.NeedsApproval`). The owner runs agents to get work done without watching them. In the pi-pocket installation this replaces, the Lancet guard was never switched on, so no call ever waited. The owner treats the team VM as the hard boundary: it is isolated, and it holds no GitHub token of the owner's.

One thing on the VM is worth protecting from the agents themselves: pocketd's data folder. Its `auth.json` holds the owner's subscription sign-ins, and its databases hold every transcript. The file tools refuse the folder (`guardedEnv`), but a command runs as the same user as pocketd. Only the approval card stood between `bash` and `cat auth.json`. Without approvals, a prompt injection from a repository or a web page could copy the sign-ins off the VM.

Commands could also change what pocketd runs at its next start, through files the same user owns: its program, its systemd unit, its environment file, or the user's systemd manager. They could stop pocketd through the user's D-Bus.

The VM has bubblewrap (`bwrap`) 0.12 and unprivileged user namespaces. `bwrap` can replace a folder with an empty `tmpfs`, bind a path read-only, and give a command its own process namespace, without root.

The owner also considered specpi-jev-guard. It settles obvious commands with local rules and asks TypeSafe's Jev classifier, a hosted early-access API, about the rest: high scores block, the middle band asks the owner.

## Decision

1. **No tool call waits for the owner.** `approvals.NeedsApproval` selects no tool. The approval machinery stays, because the merge gates (Phase 2) and later policies use it: the stored decision, the Inbox, the cards, and approvals surviving a restart. A future rule changes `NeedsApproval`, in code, with its test (ADR-0011).
2. **Every command a tool runs goes through `bwrap`** (`service/pig`'s guarded environment):
   - pocketd's data folder and the user's runtime folder (`/run/user/<uid>`, which holds the D-Bus and systemd sockets) are empty `tmpfs` mounts
   - pocketd's program, the user's systemd configuration (`~/.config/systemd`, `~/.local/share/systemd`, `~/.config/environment.d`) and `~/.config/pocketd.env` are read-only
   - the command sees only its own processes (`--unshare-pid`, a new `/proc`), and dies with pocketd
   - everything else stays as it is, so agents keep their tools, repositories, caches and network
3. **pocketd refuses to start without a working sandbox.** `pocketd serve` runs `bwrap` once, before it binds the port or opens a task. If that fails, pocketd exits, and no command ever runs unsandboxed.

## Consequences

- Agents run without stopping for the owner. The Inbox stays empty until a gate uses it.
- A command cannot read the sign-ins or the transcripts, cannot see or signal pocketd, and cannot change pocketd's program, unit or settings.
- The host needs bubblewrap and unprivileged user namespaces. pocketd is Linux-only; `pocketd serve` fails elsewhere.
- Everything else the user `team` owns is still writable: repositories, the user's other configuration, and the rest of its home folder. A command can also reach the network. That is the accepted risk of treating the VM as the boundary. The VM holds no credential of the owner's beyond the sign-ins this sandbox hides.
- The same user still runs pocketd and the commands. A path that pocketd or the owner runs as `team`, which is not in the read-only list, could still carry a change from an agent into a later start: for example, a shell startup file the owner sources in an interactive shell. `cron` and `at` are not installed on the VM. A separate user for commands would close this, at the cost of root setup on the host.
- Tests that need a sandbox skip on a host where `bwrap` cannot run, such as CI runners whose AppArmor forbids user namespaces. The sandbox's arguments are a pure function, with a test that runs everywhere.

## Alternatives considered

- **Keep approvals for `bash`, `write` and `edit`:** the owner would approve nearly every step, and the agents would wait on the owner, which defeats running them.
- **A Jev score through specpi-jev-guard:** it needs a separate key and the network, and it fails closed, so an outage stops every agent. Its middle band still asks the owner, and the announcement post publishes no accuracy figures. A score with thresholds in code would fit ADR-0011, so it can come later behind `NeedsApproval`, in a mode that only blocks.
- **Approvals off with no sandbox:** a single injected command could copy the subscription sign-ins.
- **A separate user for commands:** closes the same-user paths above, but needs root on the host and changes file ownership in every repository. Revisit if the residual paths matter.

## References

- Shapes: [design: system architecture](../design/architecture.md#service), [operational notes](../design/architecture.md#operational-notes), [specs: protocol](../specs/protocol.md), [plans: roadmap](../plans/roadmap.md)
- Builds on: [ADR-0004](0004-build-the-service-on-pigs-durable-harness.md), [ADR-0005](0005-keep-the-agent-loop-and-model-credentials-in-the-service.md), [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md)
