# 0025 — Give the owner a shell on the service host from the desktop

- **Status:** Accepted
- **Date:** 2026-11-20

## Context

**The owner uses a terminal most.** pi-pocket-team's terminal module ran login shells of the service user for the owner, and frostyard/pocket's Terminal place showed them. The team VM is an incus instance on a remote machine, in a project that is not the default, so the owner reaches it most easily through Pocket. pocketgo replaces pi-pocket-team and has no terminal.

**What there is to port:**
- **pi-pocket-team's module** (`terminal.ts`) runs each shell on a pseudo-terminal and keeps the last 256 K characters of its output. The output comes by a long poll of at most 25 seconds that names how much the client has, and a shell runs only as fast as its slowest client draws. Keys come numbered by their client, and a repeat is written once. A shell ends on `DELETE`, when it exits, 60 seconds after its last poll, or 60 minutes after its last input.
- **frostyard/pocket's client and place** (`internal/pocket/terminal.go`, `internal/app/terminals.go`, `internal/app/terminal_replay.go`) draw each shell with Ghostty's emulator (MyGo's terminal plugin). A shell's output may not write the desktop's clipboard, and only web and mail links open from it. The history is drawn off-screen first, so its old queries send no answer to the live shell.

**The agents' commands run in a sandbox** that hides pocketd's files ([ADR-0012](0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md)). The owner's shell is not an agent's tool.

**The owner's words, 2026-11-20:** "it's already practically impossible to get to pocket if you're not me, given the tokens, keys, protocols, tailscale. Don't over-engineer security on this. I just want a terminal."

## Decision

1. **pocketd runs the owner's terminals** (`service/internal/terminal`), pi-pocket-team's module in Go:
   - Each is a login shell of the service user (its shell in `/etc/passwd`), in its own session on a pseudo-terminal opened with `golang.org/x/sys/unix`, with no cgo.
   - It runs outside the agents' sandbox, with only the variables of pocketd's environment that pi-pocket-team's allowlist kept.
   - It starts in the home folder, a session's folder, or a path.
   - The module's limits are kept: 4 shells at once, the last 256 KiB of output, a poll of 25 seconds at most, inputs of 32 KiB, back pressure, and the module's end rules. A shell that exited stays listed for 60 seconds. pocketd's stop ends every shell.
2. **The device token is the only gate.** No other check, confirmation or warning stands before a shell. The agents' sandbox and `guardedEnv` stay as they are for the agents' tools.
3. **Protocol version 13 carries it** ([protocol rule 56](../specs/protocol.md)):
   - `/v1/terminals` for the list and a start;
   - `DELETE /v1/terminals/{id}`;
   - `GET /v1/terminals/{id}/output`, the long poll;
   - `POST` of `input` and `resize`;
   - a `terminals` event with the whole list.

   The output is bytes counted from the shell's start, not JavaScript's UTF-16 units. A start takes a request id (rule 5). Keys carry their client's `tab` and `seq` instead: a request id per key would put each keystroke in the work database.
4. **The desktop is frostyard/pocket's** ([ADR-0010](0010-port-the-desktop-client-selectively-from-frostyard-pocket.md)):
   - `desktop/internal/state` holds the client (`TermConn`), the list's merge rule and the rules of which shell shows and which links open.
   - `desktop/internal/app` holds the Terminal place and the replay. The sidebar's Terminal entry counts the shells that run. Each shell has a tab, titled as its shell sets it, + starts one, and Ctrl Shift C and Ctrl Shift V copy and paste.
   - The window's Ctrl keys that shells use stand aside while a terminal has the keys.

## Consequences

- **A device token is now a shell.** Whoever holds one runs commands as the service user: the data folder, the sign-ins and the agents' profiles included. A lost device needs `pocketd device revoke` at once.
- **Shells live with pocketd's process.** A restart or a deploy ends every shell, and none comes back. They run in pocketd's systemd unit, with its `UMask=0077` and `NoNewPrivileges=yes`: `sudo` does not work in them.
- **Linux only.** pocketd on another system answers a start with `internal`; it runs on the team VM.
- **Nothing logs a key or the output**, as in pi-pocket-team.
- **An agent can reach an open shell's pseudo-terminal.** Agents' commands run as the same user, and the agents' sandbox shows the host's `/dev/pts`. While a shell runs, one can open its pseudo-terminal, read what the owner types and print on the owner's screen. Hiding `/dev/pts` in the agents' sandbox would close that. The owner accepted the exposure on 2026-11-20 and chose to leave the agents' sandbox as it is (Codex review of #41, round 3).

## Alternatives considered

- **SSH, or `incus exec`, from the desktop:** the owner asked for the terminal in the app, as Pocket had it, and the VM sits behind a remote incus project.
- **A WebSocket:** frostyard/pocket's client and pi-pocket-team's contract are long polls and small POSTs, which the ported client and its tests keep, through the same HTTP client and token rules as every other request.
- **The shell in the agents' sandbox:** it is the owner's own shell, and the owner said not to fence it.
- **A request id per input:** it would write each keystroke's id to the work database; the client's sequence numbers do the same job in memory.

## References

- Shapes: [design: system architecture](../design/architecture.md), [specs: protocol](../specs/protocol.md)
- Set up by: [design: setup guide](../design/setup.md)
- Builds on: [ADR-0006](0006-share-the-wire-protocol-as-go-types-in-one-module.md), [ADR-0010](0010-port-the-desktop-client-selectively-from-frostyard-pocket.md), [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0012](0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md)
