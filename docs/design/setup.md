# Setup guide

Living document. Rationale: [ADR-0005](../adr/0005-keep-the-agent-loop-and-model-credentials-in-the-service.md), [ADR-0012](../adr/0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md), [ADR-0013](../adr/0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md), [ADR-0016](../adr/0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md), [ADR-0017](../adr/0017-record-cross-review-verdicts-and-set-ready-for-human-in-code.md), [ADR-0018](../adr/0018-approve-and-merge-as-the-owner-from-the-desktop-with-pockets-gates.md), [ADR-0022](../adr/0022-run-any-session-on-any-sign-in-and-choose-workers-from-the-owners-list.md), [ADR-0023](../adr/0023-add-the-owners-own-model-providers-from-a-file-checked-in-code.md), [ADR-0025](../adr/0025-give-the-owner-a-shell-on-the-service-host-from-the-desktop.md), [ADR-0043](../adr/0043-load-the-global-and-folder-agents-md-and-the-skills-into-every-session.md); each step links the others it follows.
Contracts: [specs/protocol.md](../specs/protocol.md).

## Overview

This guide takes you from a clone of this repository to a supervisor that delegates a thread to a specialist, on a host of your own. It follows the code as it is; the [architecture's operational notes](architecture.md#operational-notes) describe the same parts as a reference, and each step links the decision behind it.

You set up four things:

- **The service host:** a Linux machine or VM where `pocketd` runs as a systemd user service of an account of its own, the *service user*. The agents run there as that user, every command in a bubblewrap sandbox that hides pocketd's own files.
- **The way in:** your devices reach pocketd over your tailnet, by HTTPS through `tailscale serve`.
- **Your configuration on the host:** model sign-ins, devices, the profile folder and a GitHub sign-in for the agents.
- **The desktop:** the `pocket` client on your own machine.

```
your machine                 the service host
pocket (desktop) ──HTTPS──►  tailscale serve :443 ──► pocketd on 127.0.0.1:8788
                                                      ├─ data folder: durable.sqlite, work.sqlite, auth.json
                                                      ├─ profile folder: <id>.md, workers.json, …
                                                      └─ bwrap sandbox ──► agents' commands, git, gh ──► GitHub
```

**The host is the boundary** ([ADR-0012](../adr/0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md)). No tool call waits for approval. An agent's command can do whatever the service user can, the network included, except read pocketd's data folder and change the files pocketd keeps read-only. So give pocketgo a machine or VM of its own, a user of its own, and a GitHub account that reaches only the repositories you want the team to work on.

Placeholders below: `<user>` is the service user, `<host>` the host as ssh names it, `<host>.<tailnet>.ts.net` its name on the tailnet, and `your-org/your-repo` a repository the team works on.

## Design

The setup, step by step, in the order to do it.

### 1. Prepare the service host

Any Linux with systemd, on amd64 or arm64 (`deploy/deploy.sh` builds for those two). As root:

1. Make the service user, for example `useradd -m -s /bin/sh <user>`. pocketd's files live in its home. A deploy over incus needs no login as that user. A deploy over ssh runs its commands through the user's login shell, and `/bin/sh` reads no startup file for a command, which the agents could change (bash may read `~/.bashrc`). For a deploy over ssh, let your key sign in as the user from a file only root can change, never `~<user>/.ssh/authorized_keys`, which the agents could change, since they run as that user: put the key in `/etc/ssh/authorized_keys/<user>` (owned by root, mode 0644), add `Match User <user>` with `AuthorizedKeysFile /etc/ssh/authorized_keys/%u` and `PermitUserRC no` (sshd runs `~/.ssh/rc` at a login otherwise) at the end of `/etc/ssh/sshd_config`, reload sshd, and check it from your machine with `ssh <user>@<host> true`.
2. Install bubblewrap, git and gh from the system's packages (Debian and Ubuntu: `apt install bubblewrap git gh`). pocketd looks for each only in `/usr/bin`, `/bin` and `/usr/local/bin`, never on the PATH, and uses one only if root alone can change it and every folder on the way to it (`trustedBwrap` in `service/pig/sandbox.go`, `trustedGit` and `trustedGH` in `service/pig/team.go`). A copy in the user's home, or under a `/usr/local` the user owns, does not count. git must be 2.45 or later: pocketd makes no cross-review's worktree with a git older than 2.43, which ignores `attr.tree` (`attributesIsolated` in `service/internal/worktree/worktree.go`), and reads no session's changes with one older than 2.45, which lacks `--no-lazy-fetch` (`changes.MinGit`). Debian 12's and Ubuntu 24.04's are older; check with `/usr/bin/git --version`. gh must have `gh auth token`, which pocketd runs before every GitHub read: check with `gh auth token --help`.
3. Let the user's systemd run with nobody logged in: `loginctl enable-linger <user>`.
4. Install Tailscale and join the host to your tailnet (step 3).
5. Install what the agents need to build and test your repositories: compilers, `make`, and so on. An agent's command gets pocketd's `PATH`, not a login shell's (`commandEnv` in `service/pig/env.go`): install toolchains in system folders, or set `PATH` in `~/.config/pocketd.env` (step 2).

bubblewrap needs unprivileged user namespaces. A quick check as the service user, with the options every sandboxed command starts with (`Sandbox.CommandContext`) and the unit's `NoNewPrivileges=yes`: `setpriv --no-new-privs bwrap --dev-bind / / --unshare-pid --proc /proc --die-with-parent true && echo ok`. It prints bwrap's own error, which pocketd's log leaves out. pocketd's own start check decides; [try the host](#the-sandboxs-start-check) before the first deploy.

### 2. Build and deploy pocketd

On your own machine, clone this repository and install the Go release `go.work`'s `go` line names. `make deploy` builds `pocketd` for the host's architecture, copies it there, and installs it as a systemd user service of the service user (`deploy/deploy.sh`):

```sh
make deploy DEPLOY_HOST=<user>@<host>                              # over ssh, as <user>
make deploy DEPLOY_INCUS=<remote>:<instance> DEPLOY_USER=<user>    # over incus exec, as <user>
```

With incus, `DEPLOY_INCUS_PROJECT=<project>` names the project; without it the remote's current project is used. The script installs `~/.local/bin/pocketd` and `~/.config/systemd/user/pocketd.service` from `deploy/systemd/pocketd.service`, then enables and restarts the unit. The unit is `Type=notify`: pocketd reports ready only once its start order completes, so a start that fails fails the deploy, and the log says why ([troubleshooting](#the-sandboxs-start-check)). To upgrade, stop pocketd first, then run the same command again, with the same variables ([upgrades](#upgrades)): its tasks resume at the next start.

The first start makes what is missing, empty: the data folder, the profile folder, the skills folder, the global `AGENTS.md`, the uploads folder and `~/.config/pocketd.env`. It serves with no sign-in and no profile until you add them.

**Your commands on the host.** Run `pocketd login`, `pocketd device`, `pocketd models`, `pocketd profiles`, `gh` and `systemctl --user` as the service user. The agents can change that user's shell startup files ([ADR-0012](../adr/0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md), Consequences), and a login shell as the user (`ssh <user>@<host>`, `su - <user>`) runs them outside the sandbox. So open the user's shell as root (`sudo -i`, or `incus exec <remote>:<instance> -- bash`, with `--project <project>` after `exec` if you deployed with one), without them:

```sh
u=<user>
runuser -u "$u" -- env -i HOME="$(getent passwd "$u" | cut -d: -f6)" PATH=/usr/bin:/bin:/usr/local/bin \
  TERM="$TERM" INPUTRC=/dev/null HISTFILE=/dev/null XDG_RUNTIME_DIR="/run/user/$(id -u "$u")" bash --noprofile --norc
```

Add `XDG_CONFIG_HOME=…` to that `env` when the service runs with one, so gh signs in where the service looks: the value `pocketd.env` sets, else the one `systemctl --user show-environment` prints (most hosts have none). `INPUTRC=/dev/null` and `HISTFILE=/dev/null` keep the user's `~/.inputrc` and `~/.bash_history` out too. Keep `~/.local/bin` off the `PATH`: agents can put programs there. Once agents have run, use gh and git there, and copy files into the user's folders, with pocketd stopped (`systemctl --user stop pocketd`, then `start`): the stop ends every command the agents run, which could otherwise change, as the same user, what those programs read while they work, and their tasks resume at the next start. "As the service user" below means this shell. The program is `~/.local/bin/pocketd`, which agents cannot change. These commands must use the service's folders. The `pocketd serving` line of the service's log names its data folder (`data=…`); when that is not your shell's default, or `~/.config/pocketd.env` sets `POCKETD_DATA`, `POCKETD_PROFILES` or `POCKETD_SKILLS`, pass the same folders with `-data`, `-profiles` and `-skills`, and when it sets `TZ`, run `~/.local/bin/pocketd profiles` with the same `TZ=…` before it, so its report of the schedules uses the service's zone. A device added to another data folder gets a token the service does not know.

```sh
systemctl --user status pocketd          # active (running)
journalctl --user -u pocketd -n 50       # "pocketd serving", or why not
~/.local/bin/pocketd version             # pocketd <commit> protocol <N>
```

On a host whose journal is kept only in memory, the user's `journalctl` may show nothing; as root, `journalctl _SYSTEMD_USER_UNIT=pocketd.service _UID=$(id -u <user>) -n 50` reads the same lines.

**Settings** go in `~/.config/pocketd.env`, which the unit reads as systemd's `EnvironmentFile`, not as a shell script: write one `NAME=value` a line, with literal absolute paths, no quotes and no `$`. `deploy.sh` replaces the unit at every run, but never this file. Change it with pocketd stopped (`systemctl --user stop pocketd`, then `start`): a new file put in its place could be changed by running agents before systemd reads it. Write it from a trusted copy on your machine, or with a here-document in the shell of step 2 (`cat > ~/.config/pocketd.env <<'EOF'` … `EOF`), never with an editor started there: an editor loads the user's own configuration, which agents can change, outside the sandbox.

| Variable | Default | What it sets |
| --- | --- | --- |
| `POCKETD_LISTEN` | `127.0.0.1:8788` | where pocketd listens (step 3) |
| `POCKETD_DATA` | `$XDG_STATE_HOME/pocketgo`, else `~/.local/state/pocketgo` | the data folder (step 4). Changing it moves nothing, and only the new folder is hidden from agents: stop pocketd and move the whole folder there first. Where the file system of `~/.local/share/pocketgo-uploads` makes no unnamed files (`O_TMPFILE`), attachments are staged in the data folder, which must then be on the same file system (`uploads.Folder`) |
| `POCKETD_PROFILES` | `~/.config/pi-pocket-team/agents` | the profile folder (step 7) |
| `POCKETD_SKILLS` | `~/.pi/agent/skills` | the global skills folder (step 7) |
| `POCKETD_INSTRUCTIONS` | `~/.pi/agent/AGENTS.md` | your rules for every session (step 7) |
| `POCKETD_DESK_CAP` | `12` | the most threads on the desk at once: those not closed, but for a plan's own thread (`store.OpenThreads`, [ADR-0014](../adr/0014-delegate-in-threads-with-team-tools-in-code.md)) |
| `TZ` | the zone `/etc/localtime` names | the zone of a schedule's clock time that names none ([ADR-0030](../adr/0030-send-scheduled-messages-from-the-owners-file-and-the-supervisors-tool-in-code.md)) |
| `PATH` | the user manager's | the `PATH` of the agents' commands |

`~/.local/bin/pocketd -h` lists the flags behind these settings. Two have no variable: `-pr-every`, how often open pull requests are read on GitHub again (5 minutes by default), and `-instructions=` with nothing after it, which turns the global instructions off (an empty `POCKETD_INSTRUCTIONS` counts as unset). The unit runs `pocketd serve` with no flags, so give it flags in a drop-in, which `deploy.sh` leaves alone, from the shell of step 2:

```sh
mkdir -p ~/.config/systemd/user/pocketd.service.d
cat > ~/.config/systemd/user/pocketd.service.d/flags.conf <<'EOF'
[Service]
ExecStart=
ExecStart=%h/.local/bin/pocketd serve -pr-every 10m
EOF
systemctl --user daemon-reload && systemctl --user restart pocketd
```

### 3. Reach pocketd from your devices

pocketd listens on `127.0.0.1:8788`. Serve it to your tailnet with HTTPS, as root on the host ([plan: phone and tablet client, Phase 1](../plans/phone-client.md#phase-1--pocketd-over-https-on-the-tailnet-small)):

```sh
tailscale serve --bg --https=443 http://127.0.0.1:8788
tailscale serve status
```

`tailscale serve` needs MagicDNS and HTTPS certificates turned on for your tailnet in Tailscale's admin console. The *service URL* your devices sign in at is the HTTPS address `tailscale serve status` prints, `https://<host>.<tailnet>.ts.net` with the host's MagicDNS name, which may differ from the name ssh uses; nothing else on the tailnet reaches port 8788.

**Without `tailscale serve`:** set `POCKETD_LISTEN=<the host's tailnet address>:8788` in `~/.config/pocketd.env`, with pocketd stopped as Settings in step 2 says, and start it; the service URL is then `http://<host>.<tailnet>.ts.net:8788`, or `http://<the tailnet address>:8788` without MagicDNS. Write an IPv6 address in brackets, in `POCKETD_LISTEN` and in the URL alike (`[fd7a:…]:8788`). That is plain HTTP inside the tailnet, which WireGuard encrypts. The desktop asks before it sends the token over plain HTTP to any host but a loopback address or a `*.ts.net` name, a tailnet address such as `100.x.y.z` included (`state.NeedsHTTPWarning`). Never set an address outside the tailnet: the device token is the key to the service, and a shell on the host ([ADR-0025](../adr/0025-give-the-owner-a-shell-on-the-service-host-from-the-desktop.md)).

**Check it** from a device on the tailnet, with no token: `curl -si -H 'Pocket-Protocol: <N>' <service URL>/v1/hello`, where `<N>` is the protocol `pocketd version` printed. A `401` with code `unauthorized` means pocketd answered; a `400` with code `protocol` means the number is not pocketd's ([protocol: errors](../specs/protocol.md#errors)).

### 4. The data folder

The data folder holds the service's state: `durable.sqlite` (the conversations and their tasks, PiG's), `work.sqlite` (devices, sessions, threads, asks, reports, pull requests and the rest, pocketgo's own), `auth.json` (the model sign-ins), `pocketd.lock` and `upload-staging`. The sandbox hides it from every agent. One `pocketd serve` holds it at a time, through `pocketd.lock`.

The two databases are one state: back them up and restore them together ([backups](#backups)). The files you attach to messages are not in it: they are in `~/.local/share/pocketgo-uploads/<session>/`, read-only to agents, and pocketd never removes them ([ADR-0029](../adr/0029-attach-the-owners-files-to-a-message-stored-outside-the-data-folder.md)).

### 5. Sign in to models

pocketd runs agents on your subscriptions and keys, kept in the data folder's `auth.json`, never Pi's ([ADR-0005](../adr/0005-keep-the-agent-loop-and-model-credentials-in-the-service.md)). As the service user on the host:

```sh
~/.local/bin/pocketd login anthropic        # Claude subscription
~/.local/bin/pocketd login openai-codex     # ChatGPT subscription
~/.local/bin/pocketd models                 # the models the sign-ins can run
```

The examples and their cross-reviews need those two. A Copilot subscription, for profiles that name its models, signs in from the desktop instead, once it is signed in to pocketd: Settings › Sign-ins › Sign In…, a device flow pocketd runs, whose models show at once ([ADR-0062](../adr/0062-sign-pocketd-in-to-github-copilot-from-the-desktop-with-a-device-flow-it-runs.md)); `~/.local/bin/pocketd login github-copilot` does the same from the host.

A subscription signs in by OAuth: the command prints a URL or a device code to open in a browser on any machine, and reads what it asks for, one line at a time. It can run while pocketd serves.

**An API key** is kept for `deepseek` (`pig.APIKeyProviders`) and for a provider of your own with `"key": true` in `providers.json` (step 7). pocketd stores one only while it is stopped, since the running service keeps its own copy of the sign-ins and writes them as it refreshes a subscription's ([ADR-0021](../adr/0021-give-a-threads-specialist-a-worker-on-an-api-key-model-in-code.md), [ADR-0022](../adr/0022-run-any-session-on-any-sign-in-and-choose-workers-from-the-owners-list.md)):

```sh
systemctl --user stop pocketd
~/.local/bin/pocketd login deepseek         # reads the key without showing it
systemctl --user start pocketd
```

Any model with a sign-in runs any session. Two families matter for the team: pocketd starts every cross-review on a model of another family than the author's (`router.ReviewerOf`: a Claude author's change on `openai/gpt-6.1-sol`, which `openai-codex` serves, and a GPT author's on `anthropic/claude-sonnet-5-5`), so sign in to both to get reviews ([ADR-0019](../adr/0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md)). PiG's catalog is fixed at the release `service/go.mod` pins; `pocketd models` prints its date, and `providers.json` adds newer models.

### 6. GitHub for the agents

The agents push branches and open pull requests with the service user's `gh` sign-in, and pocketd reads pull requests and keeps the `ready-for-human` label with the same sign-in, through `gh` in the sandbox ([ADR-0016](../adr/0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md), [ADR-0017](../adr/0017-record-cross-review-verdicts-and-set-ready-for-human-in-code.md)). Every agent can read that token, so use a GitHub account for the team alone, with write access to the repositories it works on.

As the service user on the host:

```sh
d=$(mktemp -d)
GH_CONFIG_DIR="$d" gh auth login --hostname github.com --git-protocol https --scopes workflow --insecure-storage
g="${XDG_CONFIG_HOME:-$HOME/.config}/gh"
mkdir -p "$g" && install -m 0600 "$d/hosts.yml" "$g/hosts.yml" && rm -rf "$d"
gh auth setup-git                                    # git pushes over HTTPS with gh's token
git config --global user.name "<the account's name>"
git config --global user.email "<the account's email>"
```

The login runs with a configuration folder of its own, made just now: gh's settings in the user's `~/.config/gh`, which agents can change, could name a browser or a pager it would start outside the sandbox. Only the sign-in, `hosts.yml`, is installed where the agents' gh and pocketd read it. The temporary folder is one agents could reach while they run, so sign in again the same way with pocketd stopped.

`gh auth login` asks for `repo`, `read:org` and `gist` by default; `workflow` lets an agent push changes to `.github/workflows`. The token must be in gh's own file, `~/.config/gh/hosts.yml`, not a keyring: the sandbox hides `/run/user`, where a keyring's D-Bus socket is, and passes no `GH_TOKEN`. `--insecure-storage` keeps it in the file; a gh whose `gh auth login --help` does not list that flag is old enough to keep it there anyway, so leave the flag out. Check it with the environment pocketd gives a command, which prints nothing of the token: `env -i HOME="$HOME" PATH=/usr/bin:/bin:/usr/local/bin gh auth token --hostname github.com >/dev/null && echo ok`, the `PATH` in the order pocketd looks for gh, with `XDG_CONFIG_HOME=…` added as in the shell.

**The `ready-for-human` label.** pocketd keeps it on a linked pull request that is open and not a draft exactly while a cross-review by another family filed `ready` at its head, base branch and merge base, and CI at the head is green: every check run completed as `success`, `neutral` or `skipped`, every commit status `success`, and at least one of them exists, not counting check runs named `pr-triage` or `pr-triage / pr-triage`, whatever made them (`github.ignored`), all read whole within ten pages of 100 each (`github.CIChecks`; a longer or shifting list counts as pending), so a repository with no CI never gets the label ([ADR-0017](../adr/0017-record-cross-review-verdicts-and-set-ready-for-human-in-code.md)). A merged or closed pull request keeps its labels. pocketd writes nothing else on GitHub. Create the label in each repository first, so it has the colour and description you want: `gh label create ready-for-human --repo your-org/your-repo --color 0E8A16 --description "Cross-reviewed, CI green"`.

### 7. The profile folder

The profile folder holds your agents and your team's configuration ([ADR-0013](../adr/0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md)). By default it is `~/.config/pi-pocket-team/agents` of the service user. Agents' shell commands can read it but never change it, and the file tools, `read` included, refuse it. pocketd reads a profile each time it needs one, so an edit takes effect without a restart; `providers.json` takes effect at the next start. pocketd refuses a folder replaced since it started (a link put at its name, or another folder moved there) until it starts again, so change files inside it, never the folder itself.

| File | What it is | Rules and decision |
| --- | --- | --- |
| `<id>.md` | one agent: YAML frontmatter of settings, then its identity, whose first `# Heading` is its name | `profiles.Parse`; [ADR-0013](../adr/0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md) |
| `workers.json` | the models a thread's worker runs, in order of preference: `{"models": ["provider/id", …]}`, 1 to 8; without the file, `deepseek/deepseek-flash` | `profiles.ParseWorkers`; [ADR-0022](../adr/0022-run-any-session-on-any-sign-in-and-choose-workers-from-the-owners-list.md) |
| `providers.json` | model servers of your own, and newer models of PiG's providers (`builtinModels`) | `profiles.ParseProviders`; [ADR-0023](../adr/0023-add-the-owners-own-model-providers-from-a-file-checked-in-code.md) |
| `schedules.json` | messages, and gathers of proposals, sent to a standing session at set times | `profiles.LoadSchedules`; [ADR-0030](../adr/0030-send-scheduled-messages-from-the-owners-file-and-the-supervisors-tool-in-code.md), [ADR-0033](../adr/0033-gather-proposals-from-the-team-in-read-only-sessions-in-code.md) |
| `prompts/<name>.md` | your saved prompts, which `/` inserts in the composer | `profiles.Folder.Prompts`; [ADR-0051](../adr/0051-insert-the-owners-saved-prompts-into-the-composer-with-a-slash.md) |

Only `<id>.md` files whose id starts with a lowercase letter or digit, followed by lowercase letters, digits, `-` and `_`, are profiles; other files beside them, such as backups, are ignored without a word. Every file must be a regular file with one name: pocketd follows no symbolic link to one and refuses a hard link. A field the code does not know makes a profile, `workers.json` or `providers.json` invalid; in `schedules.json`, an unknown field inside an entry makes only that entry invalid, and the others still go out, but one beside `schedules` sends none; a bad prompt is listed with its problem. `pocketd profiles` lists each profile with the model a session from it runs, or its problem, then the workers, the schedules, the prompts and your providers:

```sh
~/.local/bin/pocketd profiles
```

**A profile's fields** (`Profile.set` in `service/internal/profiles/profiles.go`):

| Field | Value |
| --- | --- |
| `model` | `provider/id`, required; in a list, each entry is checked and the first is the model. `openai/…` runs on `openai-codex` when no sign-in serves `openai` itself, which pocketd keeps no key for (`router.Choose`) |
| `role` | `supervisor`, `clerk` or `agent` (the default). A supervisor delegates; an agent, or a clerk, which the desktop marks with a badge, is a specialist it delegates to |
| `home`, `workspace` | folders, absolute or `~/…`; a session starts in the home, else the workspace |
| `repositories` | a map of `owner/name` to its checkout's folder: a thread on that repository works in a new worktree beside the checkout |
| `worker` | the model of its threads' workers when the supervisor names none, before `workers.json` |
| `session: standing` | the agent keeps one session for good, which New Session opens; it needs a home or workspace that exists ([ADR-0026](../adr/0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md)) |
| `compactAt` | for a standing session, the tokens at which pocketd compacts it, below its model's context window |
| `questionMinutes` | for a supervisor's or a private profile, how long a question to you waits, 1 to 1440 ([ADR-0048](../adr/0048-put-the-supervisors-and-a-private-agents-questions-to-the-owner-inline-in-code.md)) |
| `private` | `true` for an agent of yours alone: no team tools, but the `question` tool and a `schedule` tool of its own for its own sessions ([ADR-0060](../adr/0060-give-a-private-agent-a-schedule-tool-of-its-own-checked-in-code.md)); its sessions start only in its home, though its commands reach what any agent's do, and you can rename them from the desktop ([ADR-0058](../adr/0058-rename-a-private-agents-session-from-the-desktop-checked-in-code.md)) ([ADR-0042](../adr/0042-give-a-private-agent-its-own-preamble-and-no-team-in-its-sessions.md)) |
| `tools` | the coding tools it may call; without it, all of them |
| `mcp` | a list of MCP server names, checked and kept; nothing in pocketd uses it yet |

**Start from the examples** in [`examples/profiles/`](../../examples/profiles/): `supervisor.md`, a standing supervisor on Claude; `builder.md`, a specialist on ChatGPT with one repository; `workers.json`; `schedules.json`; and three prompts. Change `builder.md`'s repository to one of yours in your copy, then copy them into the service's profile folder from your machine. For the default folder:

```sh
scp examples/profiles/*.md examples/profiles/workers.json <user>@<host>:.config/pi-pocket-team/agents/
scp -r examples/profiles/prompts <user>@<host>:.config/pi-pocket-team/agents/
```

Copy `schedules.json` only when you want it: from then on it sends the supervisor's standing session a message every weekday at 8:30, with nobody at the desktop.

With `POCKETD_PROFILES` set, copy to that folder instead. Over incus, make the `prompts` folder first as the service user (`mkdir ~/.config/pi-pocket-team/agents/prompts`), then push each file without `-r`, with `--uid` and `--gid` of the service user so the files stay yours to edit, and `--project <project>` if you deployed with one: `incus file push --uid <uid> --gid <gid> examples/profiles/prompts/*.md <remote>:<instance><home>/.config/pi-pocket-team/agents/prompts/`, and the same for the profiles, where `<home>` is the user's home folder.

**What must exist on the host:** each profile's `home` and `workspace`, and a git checkout of each repository at its folder, with `origin` on GitHub and a parent folder the service user can write. For cross-reviews, the checkout's repository must have no `info/attributes` file and git no system attributes file (`git var GIT_ATTR_SYSTEM` names it), even an empty one; a tracked `.gitattributes` is fine (`attributesIsolated`). A thread works in `<checkout>--<agent>-<slug>`, on the branch `<agent>/<slug>`, started from a fresh fetch of `origin`'s default branch; when that fetch fails, from the checkout's `HEAD`, and the specialist's brief says so (`team.Worktree`, `team.StaleBase`, [ADR-0056](../adr/0056-start-a-new-worktrees-branch-from-a-fresh-fetch-of-the-remotes-default-branch.md)). For the examples:

```sh
mkdir -p ~/team/supervisor ~/team/builder ~/src
GIT_CONFIG_GLOBAL=/dev/null git -c credential.helper='!gh auth git-credential' \
  clone https://github.com/your-org/your-repo ~/src/your-repo
```

Agents can change the user's git configuration, and a credential helper, a filter or a hook it names would run outside the sandbox: `GIT_CONFIG_GLOBAL=/dev/null` reads none of it, and the one helper named is gh, from the shell's `PATH`, which holds no folder of the user's. Add a checkout to a running team with pocketd stopped, so no agent can change the new checkout's `.git` while git fills it.

**`providers.json`** names servers of your own, which speak the OpenAI completions API or Anthropic's messages API. Keep its addresses in your private copy, never in this repository:

```json
{"providers": [{"id": "homelab", "api": "openai-completions", "baseUrl": "http://<server>:<port>/v1",
  "models": [{"id": "<model>", "contextWindow": 131072, "maxTokens": 32768}]}]}
```

`builtinModels` adds a model PiG's catalog lacks to one of PiG's own providers, running as the catalog model it is `like`; a built-in provider's id may not appear under `providers`:

```json
{"builtinModels": [{"provider": "anthropic",
  "models": [{"id": "<new model id>", "name": "<its name>", "like": "claude-sonnet-5-5"}]}]}
```

Restart pocketd after a change, then check `~/.local/bin/pocketd models`, which says why a model was not added.

**Outside the folder,** every session also holds your global instructions, `~/.pi/agent/AGENTS.md`, and lists the skills of `~/.pi/agent/skills/<name>/SKILL.md`; a session in a folder also gets that folder's `AGENTS.md` and its `.pi/skills`, but a cross-review, whose folder is the change under review, gets neither ([ADR-0043](../adr/0043-load-the-global-and-folder-agents-md-and-the-skills-into-every-session.md)). The global file and the skills folder are read-only to agents ([ADR-0035](../adr/0035-make-the-skills-and-uploads-folders-read-only-to-agents.md), [ADR-0045](../adr/0045-make-the-global-instructions-file-read-only-to-agents.md)). Change the global file with pocketd stopped: replacing it by a rename while pocketd runs stops every command running in the sandbox, and a session made during an in-place write keeps what it read. Change the skills inside their folder; removing or replacing the folder itself stops every command too. pocketd reads these files when it makes a session, and for a standing session again at its Start Over, never at a later message: an edit reaches new sessions, and the supervisor's standing session once you start it over. An edit of a profile itself reaches a standing session at its next refresh.

**Keep it in a private repository.** Keep your profiles, `workers.json`, `providers.json`, `schedules.json`, prompts, global `AGENTS.md` and skills in a private repository, and copy them to the host from a checkout on your own machine, with pocketd stopped, so no session starts from a half-copied file and any way of writing will do. While it runs, copy only the profile folder and `skills/`, with `rsync -rt`, which writes each file whole under a temporary name and renames it into place. Copy the global `AGENTS.md` as one file, with pocketd stopped: `~/.pi/agent` also holds Pi's own files. Neither command removes a file you deleted from your copy, so an old schedule or a renamed agent's profile stays live: remove it on the host by hand, or add `--delete` to `rsync` for a folder your copy holds whole (the profile folder, `skills/`), never `~/.pi/agent`, with a trailing `/` on both folders so it changes what is in the folder and never the folder itself. Copy from a checkout agents cannot write: the rest of the service user's home is theirs to change, so a clone there could hand you their edits at the next sync. Never commit real profiles that name hosts or addresses of your network to a public fork.

### 8. Devices

Each device signs in with a token of its own. As the service user on the host:

```sh
~/.local/bin/pocketd device add laptop     # prints the token once
~/.local/bin/pocketd device list           # the devices, and when each was revoked
```

The token starts with `pgd_` and is shown only then. pocketd keeps only its hash. A device token can open a shell on the host from the desktop's Terminal ([ADR-0025](../adr/0025-give-the-owner-a-shell-on-the-service-host-from-the-desktop.md)): revoke a lost device's token at once, with `~/.local/bin/pocketd device revoke <name>`.

### 9. The desktop

The desktop is `desktop/cmd/pocket`, on MyGo and frostui ([ADR-0010](../adr/0010-port-the-desktop-client-selectively-from-frostyard-pocket.md)). On your machine, with the Go release of `go.work`:

```sh
make run      # build and start it from source
make build    # build/pocket and build/pocketd
make app      # the app as people install it, under desktop/build/mygo/<os>-<arch>
```

- **Linux** needs GTK 3 to run it from source. The packages `make app` builds also depend on WebKitGTK (`libwebkit2gtk-4.1-0`), since `desktop/mygo.json` does not set `linux.webview` to false. The view tests also need Pango and cairo, with no display (Debian and Ubuntu: `fonts-dejavu-core libpangocairo-1.0-0`).
- **macOS:** `make app` builds `PocketGo.app` and a disk image. Notifications need the packaged app ([ADR-0049](../adr/0049-notify-the-owner-of-what-waits-once-decided-in-code.md)).
- **The Terminal** draws with Ghostty's emulator, MyGo's terminal plugin, which loads `libghostty-vt`. `make app` puts the library in the app. Run from source, it is downloaded on first use into your cache folder (`mygo/natives/`), checked by its SHA-256; `MYGO_GHOSTTY_VT` names a copy of your own instead.
- **The device token** is kept in the system keyring, or in a private file in the desktop's config folder only when the platform has no keyring (`config.Credentials`). The config folder is `pocketgo` in your user config folder; `POCKETGO_CONFIG_DIR` names another.

**Sign in:** enter the service URL (step 3) and the device's token (step 8), then Connect. The desktop keeps the token, its pins and what you have seen by the service's origin, so a new URL means signing in again. The desktop and pocketd must speak the same protocol version: rebuild the desktop from the same commit after a deploy that changed it.

**The iPad and iPhone** build from the same code on a Mac with Xcode: follow [Try it on a Mac](../plans/phone-client.md#try-it-on-a-mac) in the phone and tablet plan, which also says what does not work on iOS yet.

**Approve and merge from the desktop** is optional: without it, merge on GitHub ([ADR-0018](../adr/0018-approve-and-merge-as-the-owner-from-the-desktop-with-pockets-gates.md)). It acts as you, through a GitHub App's device-flow sign-in, and only for the one login and App compiled in: `Login` in `desktop/internal/github/client.go`, and `AppName`, `AppSlug` and `AppClientID` in `desktop/internal/github/app.go`. These name the repository owner's, so a fork needs an App of its own. The App's sign-in is kept only in the system keyring, with no file fallback (`config.GitHubApp`): without a keyring, Approve and merge stays off.

1. On GitHub, under Settings › Developer settings › GitHub Apps, make a new App. Turn on Enable Device Flow, keep Expire user authorization tokens on (the desktop takes only a pair with a refresh token and both expiries, `OAuth.pair` in `desktop/internal/github/app.go`), and turn off the webhook.
2. Give it these repository permissions (`appPermissions` in `desktop/internal/github/reads.go`): Checks read, Commit statuses read, Contents read and write, Metadata read, Pull requests read and write, Workflows read and write. Merge queues read and write is optional: without it, an enqueue may fail, and the desktop says so.
3. Install it on each account or organization whose pull requests you merge, with All repositories or a selection that holds each repository you merge in: the desktop checks the installation's account and permissions, not its repositories. An App that may be installed only on its own account cannot be installed on an organization: choose Any account when you create it, unless all your repositories are that account's.
4. In your fork, set the four constants to your login and your App's name, slug and client ID, then build the desktop again (`make run` or `make app`): the client ID and the login are compiled in. Some tests in `desktop/internal/github` name the current values.
5. In the rebuilt desktop, choose Sign In to GitHub (the menu, or a pull request's card) and enter the code it shows at GitHub's device page.

A merge then needs pocketd's backing at the pull request's head and base branch: a `ready` verdict and green CI, on an open pull request that is no draft (`state.Backed`). That is a prerequisite: Check to Merge then reads frostyard/pocket's gates on GitHub, which refuse some repositories, classic branch protection among them (`repoGate` in `desktop/internal/github/gates.go`). Merge on GitHub when it refuses. A fork with its own App has not been tried.

### 10. First run: a delegated thread

With the example profiles in place, their folders made, sign-ins for both families, and `~/.local/bin/pocketd profiles` showing a model under RUNS for `supervisor` and `builder`:

1. In the desktop, choose New Session, pick the Supervisor profile, and choose Open Session. It opens the supervisor's standing session.
2. Ask for a small change, for example: "Delegate to builder: in your-org/your-repo, fix a typo in the README and open a pull request."
3. The supervisor calls `team_delegate`. A thread appears on the Board under "With the specialists", and the builder's session starts with pocketd's brief as its first message, in a new worktree beside the checkout when the delegation names a repository.
4. A question the builder needs you for comes to the Inbox as an ask, and the thread moves to "Waits for you". Answer it there.
5. The builder opens the pull request and reports it. pocketd links it to the thread, and the pull request shows in the Inbox at once, open and not a draft. pocketd starts its cross-review on the other family's model ([ADR-0019](../adr/0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md)): a review thread on the Board, whose verdict shows on the pull request's card.
6. Once the verdict is `ready` and CI is green, `ready-for-human` goes on the pull request. A repository with no CI never gets the label ([GitHub for the agents](#6-github-for-the-agents)), and the desktop's merge needs green CI too: merge such a pull request on GitHub.
7. Merge it, on GitHub or from the desktop. pocketd's next read of the pull request sees the merge and closes the thread, which a later message to its specialist reopens ([ADR-0061](../adr/0061-reopen-a-closed-thread-when-a-message-reaches-its-specialist.md)); the reads run in rounds, the next `-pr-every` after a round ends. Once the thread's sessions are idle, pocketd may remove its worktree: only when the tree is clean and its commits are in the pull request, and not always then: a locked worktree, or a tracked file marked `assume-unchanged` or `skip-worktree`, keeps it. Files `.gitignore` matches keep nothing, and go with the worktree. When it keeps one, the thread's report says why. It then deletes the branch with `git branch -d`; when git refuses, as after a squash merge, the branch stays and only pocketd's log says why ([ADR-0054](../adr/0054-remove-a-closed-threads-worktree-in-code-with-no-github-write.md)).

That is a working setup. Everything after builds on it: shifts in rounds ([ADR-0024](../adr/0024-run-a-delegated-thread-in-rounds-as-a-shift-driven-in-code.md)), plans and consults ([ADR-0020](../adr/0020-plan-before-building-with-the-plan-gate-and-consults-in-code.md)), workers ([ADR-0022](../adr/0022-run-any-session-on-any-sign-in-and-choose-workers-from-the-owners-list.md)), schedules and gathers, search, usage, notifications and a session's changes, as the [architecture](architecture.md#service) describes them.

## Operational notes

### Upgrades

Stop pocketd first, from the shell of step 2 (`systemctl --user stop pocketd`): `deploy.sh` stages the new program in the service user's home before it installs it, where running agents could change it, and the stop ends every command they run. Then run your `make deploy` command from step 2 again, with the same variables, from the new commit: running tasks resume at the start, and the work database's migrations run at the start. When `pocketd version` shows a new protocol number, rebuild the desktop from the same commit; a desktop of another version is refused.

### Backups

`durable.sqlite` and `work.sqlite` are one state: the threads, asks and reports of one name the conversations of the other. Back up the data folder as a whole, with pocketd stopped, to a place the agents cannot read, since it holds `auth.json`: off the host, or in a folder of root's. As root, for the default data folder (`-M <user>@` reaches the user's systemd from root, in systemd 248 or later):

```sh
install -d -m 0700 /var/backups/pocketgo
systemctl --user -M <user>@ stop pocketd &&
  cp -a ~<user>/.local/state/pocketgo /var/backups/pocketgo/$(date -u +%Y%m%dT%H%M%SZ) &&
  echo "backup done"
systemctl --user -M <user>@ start pocketd
```

Copy only after the stop succeeded, and trust the backup only when `backup done` printed.

Restore it the same way. Stop pocketd, and while the current data folder is still there, list its devices with `~/.local/bin/pocketd device list` as the service user, and keep its `auth.json`. Move the current folder out of the agents' reach, under the root-only backup folder or off the host, never beside the data folder, which the sandbox does not hide. Put the saved folder back whole, owned by the service user, with the current `auth.json` in place of the saved one: a subscription may have replaced its refresh token since the backup. Before you start pocketd, list the devices again and revoke, with `~/.local/bin/pocketd device revoke <name>`, every restored device that is active and not active with the same ID in the current list: the restored `work.sqlite` holds every token as it was at the backup, a device since revoked or replaced included. Add a device you still use again under a new name. Without the current folder, revoke every restored device that is active, add the ones you use again, set the restored `auth.json` aside where agents cannot read it, and sign in again to the models you still use. pocketd does not reconcile one database restored to an earlier point than the other. The uploads folder, the profile folder and the checkouts are not in the data folder: keep them as you keep the rest of your configuration. An open session's files are in its working folder, which no database holds: a thread's worktree beside its checkout, or, for a thread on no repository, its specialist's home or workspace. To move to another host with work in flight, keep pocketd stopped on the old host from the copy of the data folder on, copy the data folder and every working folder (the checkouts with their worktrees, the agents' homes and workspaces) at the same paths, and start pocketd only on the new host: each host's lock holds only its own data folder, and two would resume the same work.

### Starting over

To wipe the work and keep the sign-ins, first note the open pull requests that carry `ready-for-human`: the new databases link none of them, so pocketd will never take a label off again, and you remove those yourself on GitHub. Then stop pocketd, move `durable.sqlite` and `work.sqlite`, and any `-wal` and `-shm` files beside them, out of the data folder together, to a place the agents cannot read, and start it. pocketd makes both databases anew, seeded by their migrations, and you add each device again. Worktrees, branches and uploads stay where they are. The desktop keeps its pins, seen marks and notified asks by the service URL, and the new databases may give sessions and asks ids the old ones had: for a clean start there too, quit the desktop and move its `settings.json`, in its config folder, aside before you sign in again.

Do not empty tables by hand: move the databases whole. Both seed rows that must stay. `work.sqlite` holds `schema_version` and the one row of `pull_reads`, without which a report that names a pull request fails (`service/internal/store/store.go`). `durable.sqlite` holds PiG's `durable_metadata`, without which PiG's store does not open.

### The sandbox's start check

Before it binds its port, `pocketd serve` makes the sandbox and proves it with trial commands under `bwrap`, or does not start ([architecture: operational notes](architecture.md#operational-notes), [ADR-0012](../adr/0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md), [ADR-0035](../adr/0035-make-the-skills-and-uploads-folders-read-only-to-agents.md)). The reason is in `journalctl --user -u pocketd`. Each message starts with what the sandbox does:

| The error says | Do this |
| --- | --- |
| `bwrap is not installed in /usr/bin/bwrap, /bin/bwrap, /usr/local/bin/bwrap` | install bubblewrap from the system's packages |
| `no bwrap only root can change is installed` | the named file or a folder on its way is writable by a user other than root: install bwrap where only root can change it |
| `the sandbox check failed: process 1 is "…", not bwrap` | the first trial command did not run as it should: most often bwrap could not make its namespaces (turn on unprivileged user namespaces, and check with the probe in step 1); the error after it says more |
| `… is a symbolic link the service user can change: pocketd cannot protect …` | a link on the way to a protected path (the profile folder, the skills folder, `~/.config/systemd`, …) lies in a folder the service user can write, whoever owns the link (`checkedPath`): replace it with the real folder, or name the real folder with `POCKETD_PROFILES` or `POCKETD_SKILLS` |
| `… leads through something the service user can change` | a link inside the systemd folders leads through a folder or to a file the user can change (`linksStayProtected`): point it at a file only root can change, or remove it |
| `… is not empty in the sandbox` or `… is on a mount that is not read-only in the sandbox` | a mount the masks or the read-only binds miss: look at `/proc/self/mountinfo` for a second mount of the named path |
| `no regular file` | the global instructions path holds a folder or another kind of file: make it a regular file, or turn the global instructions off with `-instructions=` in the unit's drop-in (step 2) |

**Try a host before the first deploy:** build `pocketd` for it in `service` (`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/pocketd`, or `GOARCH=arm64` for an arm64 host, as `deploy.sh` picks from `uname -m`), copy it to the host, and run it as the service user with a new, empty data folder: `d=$(mktemp -d) && setpriv --no-new-privs ./pocketd serve -data "$d" -profiles "" -listen 127.0.0.1:0; rm -rf "$d"`, where `setpriv --no-new-privs` matches the unit's `NoNewPrivileges=yes`, so a bwrap that works only as setuid fails here as it would there. `-profiles ""` reads no profile, schedule or provider, so nothing of yours runs. It logs `pocketd serving`, or why the check failed; stop it with Ctrl-C, and the folder goes.

**Other failures at the start:** a port in use fails the bind; another `pocketd serve` on the same data folder holds `pocketd.lock`; a profile folder pocketd cannot look at stops the start, but one whose files it cannot read does not, and neither does an invalid `providers.json`: check `~/.local/bin/pocketd profiles` and the log even when the unit runs. After a start fails, `systemctl --user restart pocketd` tries again.

## References

- Rationale: [ADR-0005](../adr/0005-keep-the-agent-loop-and-model-credentials-in-the-service.md), [ADR-0010](../adr/0010-port-the-desktop-client-selectively-from-frostyard-pocket.md), [ADR-0012](../adr/0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md), [ADR-0013](../adr/0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md), [ADR-0014](../adr/0014-delegate-in-threads-with-team-tools-in-code.md), [ADR-0016](../adr/0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md), [ADR-0017](../adr/0017-record-cross-review-verdicts-and-set-ready-for-human-in-code.md), [ADR-0018](../adr/0018-approve-and-merge-as-the-owner-from-the-desktop-with-pockets-gates.md), [ADR-0019](../adr/0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md), [ADR-0020](../adr/0020-plan-before-building-with-the-plan-gate-and-consults-in-code.md), [ADR-0021](../adr/0021-give-a-threads-specialist-a-worker-on-an-api-key-model-in-code.md), [ADR-0022](../adr/0022-run-any-session-on-any-sign-in-and-choose-workers-from-the-owners-list.md), [ADR-0023](../adr/0023-add-the-owners-own-model-providers-from-a-file-checked-in-code.md), [ADR-0024](../adr/0024-run-a-delegated-thread-in-rounds-as-a-shift-driven-in-code.md), [ADR-0025](../adr/0025-give-the-owner-a-shell-on-the-service-host-from-the-desktop.md), [ADR-0026](../adr/0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md), [ADR-0029](../adr/0029-attach-the-owners-files-to-a-message-stored-outside-the-data-folder.md), [ADR-0030](../adr/0030-send-scheduled-messages-from-the-owners-file-and-the-supervisors-tool-in-code.md), [ADR-0033](../adr/0033-gather-proposals-from-the-team-in-read-only-sessions-in-code.md), [ADR-0035](../adr/0035-make-the-skills-and-uploads-folders-read-only-to-agents.md), [ADR-0042](../adr/0042-give-a-private-agent-its-own-preamble-and-no-team-in-its-sessions.md), [ADR-0043](../adr/0043-load-the-global-and-folder-agents-md-and-the-skills-into-every-session.md), [ADR-0045](../adr/0045-make-the-global-instructions-file-read-only-to-agents.md), [ADR-0048](../adr/0048-put-the-supervisors-and-a-private-agents-questions-to-the-owner-inline-in-code.md), [ADR-0049](../adr/0049-notify-the-owner-of-what-waits-once-decided-in-code.md), [ADR-0051](../adr/0051-insert-the-owners-saved-prompts-into-the-composer-with-a-slash.md), [ADR-0054](../adr/0054-remove-a-closed-threads-worktree-in-code-with-no-github-write.md), [ADR-0056](../adr/0056-start-a-new-worktrees-branch-from-a-fresh-fetch-of-the-remotes-default-branch.md), [ADR-0062](../adr/0062-sign-pocketd-in-to-github-copilot-from-the-desktop-with-a-device-flow-it-runs.md), [ADR-0058](../adr/0058-rename-a-private-agents-session-from-the-desktop-checked-in-code.md), [ADR-0060](../adr/0060-give-a-private-agent-a-schedule-tool-of-its-own-checked-in-code.md), [ADR-0061](../adr/0061-reopen-a-closed-thread-when-a-message-reaches-its-specialist.md)
- Contracts: [specs/protocol.md](../specs/protocol.md)
- Reference: [system architecture, operational notes](architecture.md#operational-notes)
- Built in: [roadmap — Phase 2, item 36](../plans/roadmap.md#phase-2--team-work-board-and-inbox), for [frostyard/pocketgo#45](https://github.com/frostyard/pocketgo/issues/45)
