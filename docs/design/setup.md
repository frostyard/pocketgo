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

1. Make the service user, for example `useradd -m <user>`. pocketd's files live in its home.
2. Install bubblewrap, git and gh from the system's packages (Debian and Ubuntu: `apt install bubblewrap git gh`). pocketd looks for each only in `/usr/bin`, `/bin` and `/usr/local/bin`, never on the PATH, and uses one only if root alone can change it and every folder on the way to it (`trustedBwrap` in `service/pig/sandbox.go`, `trustedGit` and `trustedGH` in `service/pig/team.go`). A copy in the user's home, or under a `/usr/local` the user owns, does not count. git must be 2.43 or later for cross-reviews: pocketd makes no review worktree with an older git, which ignores `attr.tree` (`attributesIsolated` in `service/internal/worktree/worktree.go`). Debian 12's is older; check with `/usr/bin/git --version`.
3. Let the user's systemd run with nobody logged in: `loginctl enable-linger <user>`.
4. Install Tailscale and join the host to your tailnet (step 3).
5. Install what the agents need to build and test your repositories: compilers, `make`, and so on. An agent's command gets pocketd's `PATH`, not a login shell's (`commandEnv` in `service/pig/env.go`): install toolchains in system folders, or set `PATH` in `~/.config/pocketd.env` (step 2).

bubblewrap needs unprivileged user namespaces. Check them as the service user: `bwrap --dev-bind / / --unshare-all true && echo ok`. If that fails, see [the sandbox's start check](#the-sandboxs-start-check).

### 2. Build and deploy pocketd

On your own machine, clone this repository and install the Go release `go.work`'s `go` line names. `make deploy` builds `pocketd` for the host's architecture, copies it there, and installs it as a systemd user service of the service user (`deploy/deploy.sh`):

```sh
make deploy DEPLOY_HOST=<user>@<host>                              # over ssh, as <user>
make deploy DEPLOY_INCUS=<remote>:<instance> DEPLOY_USER=<user>    # over incus exec, as <user>
```

With incus, `DEPLOY_INCUS_PROJECT=<project>` names the project; without it the remote's current project is used. The script installs `~/.local/bin/pocketd` and `~/.config/systemd/user/pocketd.service` from `deploy/systemd/pocketd.service`, then enables and restarts the unit. The unit is `Type=notify`: pocketd reports ready only once its start order completes, so a start that fails fails the deploy, and the log says why ([troubleshooting](#the-sandboxs-start-check)). Run the same command again, with the same variables, to upgrade: running tasks stop on SIGTERM and resume at the next start.

The first start makes what is missing, empty: the data folder, the profile folder, the skills folder, the global `AGENTS.md`, the uploads folder and `~/.config/pocketd.env`. It serves with no sign-in and no profile until you add them.

**Your commands on the host.** Run `pocketd login`, `pocketd device`, `pocketd models` and `pocketd profiles` as the service user, in a shell on the host: `ssh <user>@<host>`, or `incus exec <remote>:<instance> -- su - <user>` (add `--project <project>` after `exec` when you deployed with `DEPLOY_INCUS_PROJECT`). The program is `~/.local/bin/pocketd`. These commands must use the service's folders: when `~/.config/pocketd.env` sets `POCKETD_DATA`, `POCKETD_PROFILES` or `POCKETD_SKILLS`, pass the same folders with `-data`, `-profiles` and `-skills`. If `systemctl --user` cannot reach the bus in that shell, `export XDG_RUNTIME_DIR=/run/user/$(id -u)` first.

```sh
systemctl --user status pocketd          # active (running)
journalctl --user -u pocketd -n 50       # "pocketd serving", or why not
~/.local/bin/pocketd version             # pocketd <commit> protocol <N>
```

**Settings** go in `~/.config/pocketd.env`, which the unit reads as systemd's `EnvironmentFile`, not as a shell script: write one `NAME=value` a line, with literal absolute paths, no quotes and no `$`. `deploy.sh` replaces the unit at every run, but never this file. Restart after a change: `systemctl --user restart pocketd`.

| Variable | Default | What it sets |
| --- | --- | --- |
| `POCKETD_LISTEN` | `127.0.0.1:8788` | where pocketd listens (step 3) |
| `POCKETD_DATA` | `$XDG_STATE_HOME/pocketgo`, else `~/.local/state/pocketgo` | the data folder (step 4) |
| `POCKETD_PROFILES` | `~/.config/pi-pocket-team/agents` | the profile folder (step 7) |
| `POCKETD_SKILLS` | `~/.pi/agent/skills` | the global skills folder (step 7) |
| `POCKETD_INSTRUCTIONS` | `~/.pi/agent/AGENTS.md` | your rules for every session (step 7) |
| `POCKETD_DESK_CAP` | `12` | the most threads on the desk at once: those not closed, but for a plan's own thread (`store.OpenThreads`, [ADR-0014](../adr/0014-delegate-in-threads-with-team-tools-in-code.md)) |
| `TZ` | the zone `/etc/localtime` names | the zone of a schedule's clock time that names none ([ADR-0030](../adr/0030-send-scheduled-messages-from-the-owners-file-and-the-supervisors-tool-in-code.md)) |
| `PATH` | the user manager's | the `PATH` of the agents' commands |

`pocketd -h` lists the flags behind these settings. One has no variable: `-pr-every`, how often open pull requests are read on GitHub again, 5 minutes by default.

### 3. Reach pocketd from your devices

pocketd listens on `127.0.0.1:8788`. Serve it to your tailnet with HTTPS, as root on the host ([plan: phone and tablet client, Phase 1](../plans/phone-client.md#phase-1--pocketd-over-https-on-the-tailnet-small)):

```sh
tailscale serve --bg --https=443 http://127.0.0.1:8788
tailscale serve status
```

`tailscale serve` needs MagicDNS and HTTPS certificates turned on for your tailnet in Tailscale's admin console. The *service URL* your devices sign in at is then `https://<host>.<tailnet>.ts.net`, and nothing else on the tailnet reaches port 8788.

**Without `tailscale serve`:** set `POCKETD_LISTEN=<the host's tailnet address>:8788` in `~/.config/pocketd.env` and restart; the service URL is then `http://<host>.<tailnet>.ts.net:8788`. That is plain HTTP inside the tailnet, which WireGuard encrypts. The desktop asks before it sends the token over plain HTTP to any host but a loopback address or a `*.ts.net` name, a tailnet address such as `100.x.y.z` included (`state.NeedsHTTPWarning`). Never set an address outside the tailnet: the device token is the key to the service, and a shell on the host ([ADR-0025](../adr/0025-give-the-owner-a-shell-on-the-service-host-from-the-desktop.md)).

**Check it** from a device on the tailnet, with no token: `curl -si -H 'Pocket-Protocol: <N>' <service URL>/v1/hello`, where `<N>` is the protocol `pocketd version` printed. A `401` with code `unauthorized` means pocketd answered; a `400` with code `protocol` means the number is not pocketd's ([protocol: errors](../specs/protocol.md#errors)).

### 4. The data folder

The data folder holds the service's state: `durable.sqlite` (the conversations and their tasks, PiG's), `work.sqlite` (devices, sessions, threads, asks, reports, pull requests and the rest, pocketgo's own), `auth.json` (the model sign-ins), `pocketd.lock` and `upload-staging`. The sandbox hides it from every agent. One `pocketd serve` holds it at a time, through `pocketd.lock`.

The two databases are one state: back them up and restore them together ([backups](#backups)). The files you attach to messages are not in it: they are in `~/.local/share/pocketgo-uploads/<session>/`, read-only to agents, and pocketd never removes them ([ADR-0029](../adr/0029-attach-the-owners-files-to-a-message-stored-outside-the-data-folder.md)).

### 5. Sign in to models

pocketd runs agents on your subscriptions and keys, kept in the data folder's `auth.json`, never Pi's ([ADR-0005](../adr/0005-keep-the-agent-loop-and-model-credentials-in-the-service.md)). As the service user on the host:

```sh
~/.local/bin/pocketd login anthropic        # Claude subscription
~/.local/bin/pocketd login openai-codex     # ChatGPT subscription
~/.local/bin/pocketd login github-copilot   # Copilot subscription
~/.local/bin/pocketd models                 # the models the sign-ins can run
```

A subscription signs in by OAuth: the command prints a URL or a device code to open in a browser on any machine, and reads what it asks for, one line at a time. It can run while pocketd serves.

**An API key** is kept for `deepseek` (`pig.APIKeyProviders`) and for a provider of your own with `"key": true` in `providers.json` (step 7). pocketd stores one only while it is stopped, since the running service keeps its own copy of the sign-ins and writes them as it refreshes a subscription's ([ADR-0021](../adr/0021-give-a-threads-specialist-a-worker-on-an-api-key-model-in-code.md), [ADR-0022](../adr/0022-run-any-session-on-any-sign-in-and-choose-workers-from-the-owners-list.md)):

```sh
systemctl --user stop pocketd
~/.local/bin/pocketd login deepseek         # reads the key without showing it
systemctl --user start pocketd
```

Any model with a sign-in runs any session. Two families matter for the team: pocketd starts every cross-review on a model of another family than the author's (`router.ReviewerOf`: a Claude author's change on `openai/gpt-6.1-sol`, served by `openai-codex`, and a GPT author's on `anthropic/claude-sonnet-5-5`), so sign in to both to get reviews ([ADR-0019](../adr/0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md)). PiG's catalog is fixed at the release `service/go.mod` pins; `pocketd models` prints its date, and `providers.json` adds newer models.

### 6. GitHub for the agents

The agents push branches and open pull requests with the service user's `gh` sign-in, and pocketd reads pull requests and keeps the `ready-for-human` label with the same sign-in, through `gh` in the sandbox ([ADR-0016](../adr/0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md), [ADR-0017](../adr/0017-record-cross-review-verdicts-and-set-ready-for-human-in-code.md)). Every agent can read that token, so use a GitHub account for the team alone, with write access to the repositories it works on.

As the service user on the host:

```sh
gh auth login --hostname github.com --git-protocol https --scopes workflow --insecure-storage
gh auth setup-git                                    # git pushes over HTTPS with gh's token
git config --global user.name "<the account's name>"
git config --global user.email "<the account's email>"
```

`gh auth login` asks for `repo`, `read:org` and `gist` by default; `workflow` lets an agent push changes to `.github/workflows`. The token must be in gh's own file, `~/.config/gh/hosts.yml`, not a keyring: the sandbox hides `/run/user`, where a keyring's D-Bus socket is, and passes no `GH_TOKEN`. `--insecure-storage` keeps it in the file; a gh whose `gh auth login --help` does not list that flag is old enough to keep it there anyway, so leave the flag out. Check it with the environment pocketd gives a command, which prints nothing of the token: `env -i HOME="$HOME" PATH=/usr/bin:/bin gh auth token --hostname github.com >/dev/null && echo ok`.

**The `ready-for-human` label.** pocketd keeps it on a linked pull request that is open and not a draft exactly while a cross-review by another family filed `ready` at its head, base branch and merge base, and CI at the head is green: every check run and status there passed, and at least one exists, so a repository with no CI never gets the label ([ADR-0017](../adr/0017-record-cross-review-verdicts-and-set-ready-for-human-in-code.md)). A merged or closed pull request keeps its labels. pocketd writes nothing else on GitHub. Create the label in each repository first, so it has the colour and description you want: `gh label create ready-for-human --repo your-org/your-repo --description "Cross-reviewed, CI green"`.

### 7. The profile folder

The profile folder holds your agents and your team's configuration ([ADR-0013](../adr/0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md)). By default it is `~/.config/pi-pocket-team/agents` of the service user. Agents can read it but never change it. pocketd reads a profile each time it needs one, so an edit takes effect without a restart; `providers.json` takes effect at the next start. pocketd refuses a folder replaced since it started (a link put at its name, or another folder moved there) until it starts again, so change files inside it, never the folder itself.

| File | What it is | Rules and decision |
| --- | --- | --- |
| `<id>.md` | one agent: YAML frontmatter of settings, then its identity, whose first `# Heading` is its name | `profiles.Parse`; [ADR-0013](../adr/0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md) |
| `workers.json` | the models a thread's worker runs, in order of preference: `{"models": ["provider/id", …]}`, 1 to 8; without the file, `deepseek/deepseek-flash` | `profiles.ParseWorkers`; [ADR-0022](../adr/0022-run-any-session-on-any-sign-in-and-choose-workers-from-the-owners-list.md) |
| `providers.json` | model servers of your own, and newer models of PiG's providers (`builtinModels`) | `profiles.ParseProviders`; [ADR-0023](../adr/0023-add-the-owners-own-model-providers-from-a-file-checked-in-code.md) |
| `schedules.json` | messages, and gathers of proposals, sent to a standing session at set times | `profiles.LoadSchedules`; [ADR-0030](../adr/0030-send-scheduled-messages-from-the-owners-file-and-the-supervisors-tool-in-code.md), [ADR-0033](../adr/0033-gather-proposals-from-the-team-in-read-only-sessions-in-code.md) |
| `prompts/<name>.md` | your saved prompts, which `/` inserts in the composer | `profiles.Folder.Prompts`; [ADR-0051](../adr/0051-insert-the-owners-saved-prompts-into-the-composer-with-a-slash.md) |

Only `<id>.md` files with an id of lowercase letters, digits, `-` and `_` are profiles, so backups beside them are ignored. Every file must be a regular file with one name: pocketd follows no symbolic link to one and refuses a hard link. A field the code does not know makes the file invalid. `pocketd profiles` lists each profile with the model a session from it runs, or its problem, then the workers, the schedules, the prompts and your providers:

```sh
~/.local/bin/pocketd profiles
```

**A profile's fields** (`Profile.set` in `service/internal/profiles/profiles.go`):

| Field | Value |
| --- | --- |
| `model` | `provider/id`, required; in a list, each entry is checked and the first is the model. `openai/…` runs as `openai-codex/…` |
| `role` | `supervisor`, `clerk` or `agent` (the default). A supervisor delegates; an agent, or a clerk, which the desktop marks with a badge, is a specialist it delegates to |
| `home`, `workspace` | folders, absolute or `~/…`; a session starts in the home, else the workspace |
| `repositories` | a map of `owner/name` to its checkout's folder: a thread on that repository works in a new worktree beside the checkout |
| `worker` | the model of its threads' workers when the supervisor names none, before `workers.json` |
| `session: standing` | the agent keeps one session for good, which New Session opens; it needs a home or workspace that exists ([ADR-0026](../adr/0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md)) |
| `compactAt` | for a standing session, the tokens at which pocketd compacts it, below its model's context window |
| `questionMinutes` | for a supervisor's or a private profile, how long a question to you waits, 1 to 1440 ([ADR-0048](../adr/0048-put-the-supervisors-and-a-private-agents-questions-to-the-owner-inline-in-code.md)) |
| `private` | `true` for an agent of yours alone: no team, works only in its home ([ADR-0042](../adr/0042-give-a-private-agent-its-own-preamble-and-no-team-in-its-sessions.md)) |
| `tools` | the coding tools it may call; without it, all of them |
| `mcp` | a list of MCP server names, checked and kept; nothing in pocketd uses it yet |

**Start from the examples** in [`examples/profiles/`](../../examples/profiles/): `supervisor.md`, a standing supervisor on Claude; `builder.md`, a specialist on ChatGPT with one repository; `workers.json`; `schedules.json`; and three prompts. Copy them into the service's profile folder from your machine, then change `builder.md`'s repository to one of yours. For the default folder:

```sh
scp examples/profiles/*.md examples/profiles/*.json <user>@<host>:.config/pi-pocket-team/agents/
scp -r examples/profiles/prompts <user>@<host>:.config/pi-pocket-team/agents/
```

With `POCKETD_PROFILES` set, copy to that folder instead. Over incus, `incus file push` does the same (with `--project <project>` if you deployed with one), with `--uid` and `--gid` of the service user so the files stay yours to edit.

**What must exist on the host:** each profile's `home` and `workspace`, and a git checkout of each repository at its folder, with `origin` on GitHub and a parent folder the service user can write. A thread works in `<checkout>--<agent>-<slug>`, on the branch `<agent>/<slug>`, started from a fresh fetch of `origin`'s default branch; when that fetch fails, from the checkout's `HEAD`, and the specialist's brief says so (`team.Worktree`, `team.StaleBase`, [ADR-0056](../adr/0056-start-a-new-worktrees-branch-from-a-fresh-fetch-of-the-remotes-default-branch.md)). For the examples:

```sh
mkdir -p ~/team/supervisor ~/team/builder ~/src
git clone https://github.com/your-org/your-repo ~/src/your-repo
```

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

Restart pocketd after a change, then check `pocketd models`, which says why a model was not added.

**Outside the folder,** every session also holds your global instructions, `~/.pi/agent/AGENTS.md`, and lists the skills of `~/.pi/agent/skills/<name>/SKILL.md`; a session in a folder also gets that folder's `AGENTS.md` and its `.pi/skills`, but a cross-review, whose folder is the change under review, gets neither ([ADR-0043](../adr/0043-load-the-global-and-folder-agents-md-and-the-skills-into-every-session.md)). The global file and the skills folder are read-only to agents ([ADR-0035](../adr/0035-make-the-skills-and-uploads-folders-read-only-to-agents.md), [ADR-0045](../adr/0045-make-the-global-instructions-file-read-only-to-agents.md)). Change them in place, as `cp` writes a file: replacing the file by a rename, or removing or replacing the skills folder, stops every command running in the sandbox.

**Keep it in a private repository.** Keep your profiles, `workers.json`, `providers.json`, `schedules.json`, prompts, global `AGENTS.md` and skills in a private repository, and copy them to the host from a checkout on your own machine: with `scp` as above, or `rsync -rt --inplace` into the folders, which writes each file in place. Copy from a checkout agents cannot write: the rest of the service user's home is theirs to change, so a clone there could hand you their edits at the next sync. Never commit real profiles that name hosts or addresses of your network to a public fork.

### 8. Devices

Each device signs in with a token of its own. As the service user on the host:

```sh
~/.local/bin/pocketd device add laptop     # prints the token once
~/.local/bin/pocketd device list           # the devices, and when each was revoked
```

The token starts with `pgd_` and is shown only then. pocketd keeps only its hash. A device token can open a shell on the host from the desktop's Terminal ([ADR-0025](../adr/0025-give-the-owner-a-shell-on-the-service-host-from-the-desktop.md)): revoke a lost device's token at once, with `pocketd device revoke <name>`.

### 9. The desktop

The desktop is `desktop/cmd/pocket`, on MyGo and frostui ([ADR-0010](../adr/0010-port-the-desktop-client-selectively-from-frostyard-pocket.md)). On your machine, with the Go release of `go.work`:

```sh
make run      # build and start it from source
make build    # build/pocket and build/pocketd
make app      # the app as people install it, under desktop/build/mygo/<os>-<arch>
```

- **Linux** needs GTK 3 to run it. The view tests also need Pango and cairo, with no display (Debian and Ubuntu: `fonts-dejavu-core libpangocairo-1.0-0`).
- **macOS:** `make app` builds `PocketGo.app` and a disk image. Notifications need the packaged app ([ADR-0049](../adr/0049-notify-the-owner-of-what-waits-once-decided-in-code.md)).
- **The Terminal** draws with Ghostty's emulator, MyGo's terminal plugin, which loads `libghostty-vt`. `make app` puts the library in the app. Run from source, it is downloaded on first use into your cache folder (`mygo/natives/`), checked by its SHA-256; `MYGO_GHOSTTY_VT` names a copy of your own instead.
- **The device token** is kept in the system keyring, or in a private file in the desktop's config folder only when the platform has no keyring (`config.Credentials`). The config folder is `pocketgo` in your user config folder; `POCKETGO_CONFIG_DIR` names another.

**Sign in:** enter the service URL (step 3) and the device's token (step 8), then Connect. The desktop keeps the token, its pins and what you have seen by the service's origin, so a new URL means signing in again. The desktop and pocketd must speak the same protocol version: rebuild the desktop from the same commit after a deploy that changed it.

**The iPad and iPhone** build from the same code on a Mac with Xcode: follow [Try it on a Mac](../plans/phone-client.md#try-it-on-a-mac) in the phone and tablet plan, which also says what does not work on iOS yet.

**Approve and merge from the desktop** is optional: without it, merge on GitHub ([ADR-0018](../adr/0018-approve-and-merge-as-the-owner-from-the-desktop-with-pockets-gates.md)). It acts as you, through a GitHub App's device-flow sign-in, and only for the one login and App compiled in: `Login` in `desktop/internal/github/client.go`, and `AppName`, `AppSlug` and `AppClientID` in `desktop/internal/github/app.go`. These name the repository owner's, so a fork needs an App of its own. The App's sign-in is kept only in the system keyring, with no file fallback (`config.GitHubApp`): without a keyring, Approve and merge stays off.

1. On GitHub, under Settings › Developer settings › GitHub Apps, make a new App. Turn on Enable Device Flow, keep Expire user authorization tokens on (the desktop takes only a pair with a refresh token and both expiries, `OAuth.pair` in `desktop/internal/github/app.go`), and turn off the webhook.
2. Give it these repository permissions (`appPermissions` in `desktop/internal/github/reads.go`): Checks read, Commit statuses read, Contents read and write, Metadata read, Pull requests read and write, Workflows read and write. Merge queues read and write is optional: without it, an enqueue may fail, and the desktop says so.
3. Install it on each account or organization whose pull requests you merge.
4. In your fork, set the four constants to your login and your App's name, slug and client ID, then build the desktop again (`make run` or `make app`): the client ID and the login are compiled in. Some tests in `desktop/internal/github` name the current values.
5. In the rebuilt desktop, choose Sign In to GitHub (the menu, or a pull request's card) and enter the code it shows at GitHub's device page.

A merge then needs pocketd's backing at the pull request's head and base branch: a `ready` verdict and green CI, on an open pull request that is no draft (`state.Backed`). A fork with its own App has not been tried.

### 10. First run: a delegated thread

With the example profiles in place, their folders made, sign-ins for both families, and `pocketd profiles` showing a model under RUNS for `supervisor` and `builder`:

1. In the desktop, choose New Session and the Supervisor profile. It opens the supervisor's standing session.
2. Ask for a small change, for example: "Delegate to builder: in your-org/your-repo, fix a typo in the README and open a pull request."
3. The supervisor calls `team_delegate`. A thread appears on the Board under "With the specialists", and the builder's session starts with pocketd's brief as its first message, in a new worktree beside the checkout when the delegation names a repository.
4. A question the builder needs you for comes to the Inbox as an ask, and the thread moves to "Waits for you". Answer it there.
5. The builder opens the pull request and reports it. pocketd links it to the thread, and the pull request shows in the Inbox at once, open and not a draft. pocketd starts its cross-review on the other family's model ([ADR-0019](../adr/0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md)): a review thread on the Board, whose verdict shows on the pull request's card.
6. Once the verdict is `ready` and CI is green, `ready-for-human` goes on the pull request. A repository with no CI never gets the label ([GitHub for the agents](#6-github-for-the-agents)), and the desktop's merge needs green CI too: merge such a pull request on GitHub.
7. Merge it, on GitHub or from the desktop. pocketd's next read of the pull request, within `-pr-every`, sees the merge and closes the thread. It then removes the thread's worktree and branch once its sessions are idle, when the tree is clean and its commits are in the pull request; otherwise it keeps them and says why in the thread's report ([ADR-0054](../adr/0054-remove-a-closed-threads-worktree-in-code-with-no-github-write.md)).

That is a working setup. Everything after builds on it: shifts in rounds ([ADR-0024](../adr/0024-run-a-delegated-thread-in-rounds-as-a-shift-driven-in-code.md)), plans and consults ([ADR-0020](../adr/0020-plan-before-building-with-the-plan-gate-and-consults-in-code.md)), workers ([ADR-0022](../adr/0022-run-any-session-on-any-sign-in-and-choose-workers-from-the-owners-list.md)), schedules and gathers, search, usage, notifications and a session's changes, as the [architecture](architecture.md#service) describes them.

## Operational notes

### Upgrades

Run your `make deploy` command from step 2 again, with the same variables, from the new commit: pocketd stops, running tasks resume after the restart, and the work database's migrations run at the start. When `pocketd version` shows a new protocol number, rebuild the desktop from the same commit; a desktop of another version is refused.

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

Restore it the same way: stop pocketd, put the saved folder back whole, owned by the service user, and start it. pocketd does not reconcile one database restored to an earlier point than the other. The uploads folder, the profile folder and the checkouts are not in the data folder: keep them as you keep the rest of your configuration.

### Starting over

To wipe the work and keep the sign-ins: stop pocketd, move `durable.sqlite` and `work.sqlite`, and any `-wal` and `-shm` files beside them, out of the data folder together, to a place the agents cannot read, and start it. pocketd makes both databases anew, seeded by their migrations, and you add each device again. Worktrees, branches and uploads stay where they are.

Do not empty tables by hand: move the databases whole. Both seed rows that must stay. `work.sqlite` holds `schema_version` and the one row of `pull_reads`, without which a report that names a pull request fails (`service/internal/store/store.go`). `durable.sqlite` holds PiG's `durable_metadata`, without which PiG's store does not open.

### The sandbox's start check

Before it binds its port, `pocketd serve` makes the sandbox and proves it with trial commands under `bwrap`, or does not start ([architecture: operational notes](architecture.md#operational-notes), [ADR-0012](../adr/0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md), [ADR-0035](../adr/0035-make-the-skills-and-uploads-folders-read-only-to-agents.md)). The reason is in `journalctl --user -u pocketd`. Each message starts with what the sandbox does:

| The error says | Do this |
| --- | --- |
| `bwrap is not installed in /usr/bin/bwrap, /bin/bwrap, /usr/local/bin/bwrap` | install bubblewrap from the system's packages |
| `no bwrap only root can change is installed` | the named file or a folder on its way is writable by a user other than root: install bwrap where only root can change it |
| `the sandbox check failed: process 1 is "…", not bwrap` | bwrap could not make its namespaces: turn on unprivileged user namespaces, and run `bwrap --dev-bind / / --unshare-all true` as the service user until it works |
| `… is a symbolic link the service user can change: pocketd cannot protect …` | a protected path, such as the profile folder, the skills folder or `~/.config/systemd`, is a link the user owns: make it a real folder, or name the real folder with `-profiles`, `-skills` or their variables |
| `… leads through something the service user can change` | the same, for a folder on the way to a protected path, or a link in the systemd folders to a file the user can change |
| `… is not empty in the sandbox` or `… is on a mount that is not read-only in the sandbox` | a mount the masks or the read-only binds miss: look at `/proc/self/mountinfo` for a second mount of the named path |
| `no regular file` | the global instructions path holds a folder or another kind of file: make it a regular file, or set `-instructions ""` for none |

**Try a host before the first deploy:** build `pocketd` for it in `service` (`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/pocketd`, or `GOARCH=arm64` for an arm64 host, as `deploy.sh` picks from `uname -m`), copy it to the host, and run it as the service user with a data folder of its own: `./pocketd serve -data ~/pocketd-check -listen 127.0.0.1:0`. It logs `pocketd serving`, or why the check failed; stop it with Ctrl-C and remove the folder.

**Other failures at the start:** a port in use fails the bind; another `pocketd serve` on the same data folder holds `pocketd.lock`; a profile folder that cannot be read stops the start. After a start fails, `systemctl --user restart pocketd` tries again.

## References

- Rationale: [ADR-0005](../adr/0005-keep-the-agent-loop-and-model-credentials-in-the-service.md), [ADR-0010](../adr/0010-port-the-desktop-client-selectively-from-frostyard-pocket.md), [ADR-0012](../adr/0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md), [ADR-0013](../adr/0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md), [ADR-0014](../adr/0014-delegate-in-threads-with-team-tools-in-code.md), [ADR-0016](../adr/0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md), [ADR-0017](../adr/0017-record-cross-review-verdicts-and-set-ready-for-human-in-code.md), [ADR-0018](../adr/0018-approve-and-merge-as-the-owner-from-the-desktop-with-pockets-gates.md), [ADR-0019](../adr/0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md), [ADR-0020](../adr/0020-plan-before-building-with-the-plan-gate-and-consults-in-code.md), [ADR-0021](../adr/0021-give-a-threads-specialist-a-worker-on-an-api-key-model-in-code.md), [ADR-0022](../adr/0022-run-any-session-on-any-sign-in-and-choose-workers-from-the-owners-list.md), [ADR-0023](../adr/0023-add-the-owners-own-model-providers-from-a-file-checked-in-code.md), [ADR-0024](../adr/0024-run-a-delegated-thread-in-rounds-as-a-shift-driven-in-code.md), [ADR-0025](../adr/0025-give-the-owner-a-shell-on-the-service-host-from-the-desktop.md), [ADR-0026](../adr/0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md), [ADR-0029](../adr/0029-attach-the-owners-files-to-a-message-stored-outside-the-data-folder.md), [ADR-0030](../adr/0030-send-scheduled-messages-from-the-owners-file-and-the-supervisors-tool-in-code.md), [ADR-0033](../adr/0033-gather-proposals-from-the-team-in-read-only-sessions-in-code.md), [ADR-0035](../adr/0035-make-the-skills-and-uploads-folders-read-only-to-agents.md), [ADR-0042](../adr/0042-give-a-private-agent-its-own-preamble-and-no-team-in-its-sessions.md), [ADR-0043](../adr/0043-load-the-global-and-folder-agents-md-and-the-skills-into-every-session.md), [ADR-0045](../adr/0045-make-the-global-instructions-file-read-only-to-agents.md), [ADR-0048](../adr/0048-put-the-supervisors-and-a-private-agents-questions-to-the-owner-inline-in-code.md), [ADR-0049](../adr/0049-notify-the-owner-of-what-waits-once-decided-in-code.md), [ADR-0051](../adr/0051-insert-the-owners-saved-prompts-into-the-composer-with-a-slash.md), [ADR-0054](../adr/0054-remove-a-closed-threads-worktree-in-code-with-no-github-write.md), [ADR-0056](../adr/0056-start-a-new-worktrees-branch-from-a-fresh-fetch-of-the-remotes-default-branch.md)
- Contracts: [specs/protocol.md](../specs/protocol.md)
- Reference: [system architecture, operational notes](architecture.md#operational-notes)
- Built in: [roadmap — Phase 2, item 32](../plans/roadmap.md#phase-2--team-work-board-and-inbox), for [frostyard/pocketgo#45](https://github.com/frostyard/pocketgo/issues/45)
