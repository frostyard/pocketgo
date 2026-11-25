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

1. Make the service user, for example `useradd -m -s /bin/sh <user>`. pocketd's files live in its home.
   - Whatever the deploy path, a host that runs sshd must not trust `~<user>/.ssh/authorized_keys`, which the agents could change, since they run as that user.
   - Deploying over incus, deny the user ssh logins (`DenyUsers <user>` in `/etc/ssh/sshd_config`), check the file with `sshd -t`, reload sshd, and check what sshd applies to a login of the user, from your machine for example, which a refused login does not prove for an account with no key: `sshd -T -C user=<user>,host=<your machine's name>,addr=<your machine's address, as the host sees it> | grep -i denyusers` lists the user.
   - A deploy over ssh runs its commands through the user's login shell, and `/bin/sh` reads no startup file for a command, which the agents could change (bash may read `~/.bashrc`).
   - For a deploy over ssh, also let your key sign in as the user from a file only root can change: put the public key, the line of your `.pub` file (the private key never leaves your machine), in `/etc/ssh/authorized_keys/<user>` (owned by root, mode 0644), add `Match User <user>` with `AuthorizedKeysFile /etc/ssh/authorized_keys/%u`, `PermitUserRC no` (sshd runs `~/.ssh/rc` at a login otherwise) and `AllowAgentForwarding no` (a forwarded agent's socket in `/tmp` would sign for any process of the user while the deploy's session lasts) at the end of `/etc/ssh/sshd_config`, keep `PermitUserEnvironment` at its default, `no`, outside any `Match` block (sshd reads `~/.ssh/environment` otherwise, and refuses the setting inside one), and drop `user_readenv=1` from any `pam_env.so` line of `/etc/pam.d/sshd` (Debian's has one), with which PAM reads `~/.pam_environment`; where sshd runs with `UsePAM no`, deploy over incus instead: sshd then refuses the locked account `useradd` makes before it looks at a key, and gives an ssh command no `XDG_RUNTIME_DIR`, without which `deploy.sh`'s `systemctl --user` finds no manager; then check it with `sshd -t`, reload sshd, check what sshd applies to a login of the user from your machine with `sshd -T -C user=<user>,host=<your machine's name>,addr=<your machine's address, as the host sees it> | grep -i -e authorizedkeysfile -e permituserrc -e permituserenvironment -e allowagentforwarding`, which must show `/etc/ssh/authorized_keys/%u` and `no` for the other three, and check, from your machine, over the connection `make deploy` uses, that the user's systemd answers: `ssh <user>@<host> systemctl --user is-system-running` prints `running` or `degraded` (after step 3 below, which starts the user's manager).
2. Install bubblewrap, git and gh from the system's packages (Debian and Ubuntu: `apt install bubblewrap git gh`). pocketd looks for each only in `/usr/bin`, `/bin` and `/usr/local/bin`, never on the PATH, and uses one only if root alone can change it and every folder on the way to it (`trustedBwrap` in `service/pig/sandbox.go`, `trustedGit` and `trustedGH` in `service/pig/team.go`). A copy in the user's home, or under a `/usr/local` the user owns, does not count. git must be 2.45 or later: pocketd makes no cross-review's worktree with a git older than 2.43, which ignores `attr.tree` (`attributesIsolated` in `service/internal/worktree/worktree.go`), and reads no session's changes with one older than 2.45, which lacks `--no-lazy-fetch` (`changes.MinGit`, [ADR-0053](../adr/0053-show-a-sessions-changes-read-with-isolated-git-in-code.md)). Debian 12's and Ubuntu 24.04's are older; check with `/usr/bin/git --version`. gh must have `gh auth token`, which pocketd runs before every GitHub read: check with `gh auth token --help`.
3. Let the user's systemd run with nobody logged in: `loginctl enable-linger <user>`. Let the user run nothing outside pocketd's unit: where cron or at is installed, keep the user out of them (`/etc/cron.allow` and `/etc/at.allow` without it, or the user in `cron.deny` and `at.deny`). Its systemd folders are read-only to agents already, so they can add no unit or timer.
4. Install Tailscale and join the host to your tailnet (step 3).
5. Install what the agents need to build and test your repositories: compilers, `make`, and so on. An agent's command gets pocketd's `PATH`, not a login shell's (`commandEnv` in `service/pig/env.go`): install toolchains in system folders, or set `PATH` in `~/.config/pocketd.env` (step 2).

bubblewrap needs unprivileged user namespaces. A quick check as the service user, with the options every sandboxed command starts with (`Sandbox.CommandContext`) and the unit's `NoNewPrivileges=yes`: `setpriv --no-new-privs bwrap --dev-bind / / --unshare-pid --proc /proc --die-with-parent true && echo ok`. It prints bwrap's own error, which pocketd's log leaves out. pocketd's own start check decides; [try the host](#the-sandboxs-start-check) before the first deploy.

### 2. Build and deploy pocketd

On your own machine, clone this repository and install the Go release `go.work`'s `go` line names. `make deploy` builds `pocketd` for the host's architecture, copies it there, and installs it as a systemd user service of the service user (`deploy/deploy.sh`):

```sh
make deploy DEPLOY_HOST=<user>@<host>                              # over ssh, as <user>
make deploy DEPLOY_INCUS=<remote>:<instance> DEPLOY_USER=<user>    # over incus exec, as <user>
```

With incus, `DEPLOY_INCUS_PROJECT=<project>` names the project; without it the remote's current project is used. The script installs `~/.local/bin/pocketd` and `~/.config/systemd/user/pocketd.service` from `deploy/systemd/pocketd.service`, then enables and restarts the unit. The user's systemd must look for units there, as it does unless its own `XDG_CONFIG_HOME` points elsewhere: check that `systemctl --user show -p UnitPath --value`, as the service user, which asks the running manager, lists `~/.config/systemd/user`. The unit is `Type=notify`: pocketd reports ready only once its start order completes, so a start that fails fails the deploy, and the log says why ([troubleshooting](#the-sandboxs-start-check)). To upgrade, stop pocketd first, then run the same command again, with the same variables ([upgrades](#upgrades)): its tasks resume at the next start.

The first start makes what is missing, empty: the data folder, the profile folder, the skills folder, the global `AGENTS.md`, the uploads folder and `~/.config/pocketd.env`. It serves with no sign-in and no profile until you add them.

**Your commands on the host.** Run `pocketd login`, `pocketd device`, `pocketd models`, `pocketd profiles`, `gh` and `systemctl --user` as the service user. The agents can change that user's shell startup files ([ADR-0012](../adr/0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md), Consequences), and a login shell as the user (`ssh <user>@<host>`, `su - <user>`) runs them outside the sandbox. So open the user's shell as root (`sudo -i`, or `incus exec <remote>:<instance> -- bash`, with `--project <project>` after `exec` if you deployed with one), without them:

```sh
u=<user>
runuser -u "$u" -- env -i HOME="$(getent passwd "$u" | cut -d: -f6)" PATH=/usr/bin:/bin:/usr/local/bin \
  TERM="$TERM" INPUTRC=/dev/null HISTFILE=/dev/null PAGER=cat SYSTEMD_PAGER=cat GIT_PAGER=cat GH_PAGER=cat XDG_RUNTIME_DIR="/run/user/$(id -u "$u")" bash --noprofile --norc
```

Add `XDG_CONFIG_HOME=…` to that `env` when the service runs with one, so gh signs in where the service looks: the value `pocketd.env` sets, else the one `systemctl --user show-environment` prints (most hosts have none). `INPUTRC=/dev/null` and `HISTFILE=/dev/null` keep the user's `~/.inputrc` and `~/.bash_history` out too, and the `*PAGER=cat` settings keep systemctl, journalctl, git and gh from starting `less`, which would read the user's own configuration. Keep `~/.local/bin` off the `PATH`: agents can put programs there. Once agents have run, use gh and git there, and copy files into the user's folders, with pocketd stopped (`systemctl --user stop pocketd`, then `start`): the stop ends every command the agents run, which could otherwise change, as the same user, what those programs read while they work, and their tasks resume at the next start. "As the service user" below means this shell. The program is `~/.local/bin/pocketd`, which agents cannot change. These commands must use the folders and zone the service runs with, which may come from `pocketd.env`, from the user manager's environment (`systemctl --user show-environment`) or from flags in a drop-in (below). The `pocketd serving` line of the service's log names its data folder (`data=…`); pass the service's folders with `-data`, `-profiles` and `-skills` whenever they are not your shell's defaults, and run `~/.local/bin/pocketd profiles` with the service's `TZ=…` before it when the service has one, so its report of the schedules uses the service's zone. A device added to another data folder gets a token the service does not know.

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
| `POCKETD_DATA` | `$XDG_STATE_HOME/pocketgo`, else `~/.local/state/pocketgo` | the data folder (step 4). Changing it moves nothing, and only the new folder is hidden from agents: stop pocketd and move the whole folder there first. Where the file system of `~/.local/share/pocketgo-uploads` makes no unnamed files (`O_TMPFILE`), attachments are staged in the data folder, which must then be on the same mount, not only the same file system (`uploads.Folder`) |
| `POCKETD_PROFILES` | `~/.config/pi-pocket-team/agents` | the profile folder (step 7) |
| `POCKETD_SKILLS` | `~/.pi/agent/skills` | the global skills folder (step 7) |
| `POCKETD_INSTRUCTIONS` | `~/.pi/agent/AGENTS.md` | your rules for every session (step 7) |
| `POCKETD_DESK_CAP` | `12` | how many threads the desk holds before a new delegation is refused: those not closed, but for a plan's own thread (`store.OpenThreads`, `team.CheckDelegation`); a closed thread a message reopens may take the desk past it ([ADR-0014](../adr/0014-delegate-in-threads-with-team-tools-in-code.md)) |
| `TZ` | the zone `/etc/localtime` names | the zone of a schedule's clock time that names none ([ADR-0030](../adr/0030-send-scheduled-messages-from-the-owners-file-and-the-supervisors-tool-in-code.md)) |
| `PATH` | the user manager's | the `PATH` of the agents' commands |

`~/.local/bin/pocketd -h` lists the flags behind these settings. Some have no variable: `-pr-every`, how often open pull requests are read on GitHub again (5 minutes by default); `-cache-keeper=false`, which stops pocketd keeping a run's prompt cache warm while the run waits on a tool call, a warm costing a cache read of the prompt each time, of your Anthropic subscription's usage, on a Claude model signed in with `pocketd login anthropic` alone ([ADR-0074](../adr/0074-keep-the-prompt-cache-warm-between-a-runs-requests-in-code.md)); and the empty flags `-profiles=`, `-skills=` and `-instructions=`, which turn the profile folder, the skills folder or the global instructions off; an empty `POCKETD_PROFILES`, `POCKETD_SKILLS` or `POCKETD_INSTRUCTIONS` counts as unset, and keeps the default. The unit runs `pocketd serve` with no flags, so give it flags in a drop-in, which `deploy.sh` leaves alone, from the shell of step 2:

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

The examples and their cross-reviews need those two. A Copilot subscription, for profiles that name its models, signs in from the desktop instead, once it is signed in to pocketd: Settings › Sign-ins › Sign In…, a device flow pocketd runs, whose models show at once ([ADR-0062](../adr/0062-sign-pocketd-in-to-github-copilot-from-the-desktop-with-a-device-flow-it-runs.md)); `~/.local/bin/pocketd login github-copilot` signs in from the host instead, and also turns on, where it can, each model whose account policy is unset; after the desktop's sign-in, a listed model's first call may be refused until you enable it on GitHub.

A subscription signs in by OAuth: the command prints a URL or a device code to open in a browser on any machine, and reads what it asks for, one line at a time. It can run while pocketd serves.

**An API key** is kept for `deepseek` (`pig.APIKeyProviders`) and for a provider of your own with `"key": true` in `providers.json` (step 7). pocketd stores one only while it is stopped, since the running service keeps its own copy of the sign-ins and writes them as it refreshes a subscription's ([ADR-0021](../adr/0021-give-a-threads-specialist-a-worker-on-an-api-key-model-in-code.md), [ADR-0022](../adr/0022-run-any-session-on-any-sign-in-and-choose-workers-from-the-owners-list.md)):

```sh
systemctl --user stop pocketd
~/.local/bin/pocketd login deepseek         # reads the key without showing it
systemctl --user start pocketd
```

Any model with a sign-in runs any session. Two families matter for the team: pocketd starts every cross-review on a model of another family than the author's (`router.ReviewerOf`: a Claude author's change on `openai/gpt-6.1-sol`, which `openai-codex` serves, and a GPT author's on `anthropic/claude-sonnet-5-5`), so sign in to both to get reviews ([ADR-0019](../adr/0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md)). PiG's catalog is fixed at the release `service/go.mod` pins; `pocketd models` prints its date, and `providers.json` adds newer models.

### 6. GitHub for the agents

The agents push branches and open pull requests with the service user's `gh` sign-in, and pocketd reads pull requests and keeps the `ready-for-human` label with the same sign-in, through `gh` in the sandbox ([ADR-0016](../adr/0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md), [ADR-0017](../adr/0017-record-cross-review-verdicts-and-set-ready-for-human-in-code.md)). Every agent can read that token, so use a GitHub account for the team alone, with write access to the repositories it works on. That access lets the agents push to any branch and merge with gh as that account: pocketd's label and the desktop's gates bind neither, so set branch rules on GitHub (a required review and status checks on the default branch) when merges must wait for you.

As the service user on the host:

```sh
d=$(mktemp -d) &&
  s=$(GH_CONFIG_DIR="$d" GH_PAGER=cat gh auth login --help | grep -o -- --insecure-storage | head -n 1) &&
  GH_CONFIG_DIR="$d" GH_BROWSER=true gh auth login --hostname github.com --git-protocol https --scopes workflow $s &&
  g="${XDG_CONFIG_HOME:-$HOME/.config}/gh" && mkdir -p "$g" && install -m 0600 "$d/hosts.yml" "$g/hosts.yml" &&
  gh auth setup-git &&                               # git pushes over HTTPS with gh's token
  git config --global user.name "<the account's name>" &&
  git config --global user.email "<the account's email>" &&
  echo "signed in"; [ -n "$d" ] && rm -rf "$d"
```

Go on only when `signed in` printed: each step runs only after the one before it worked, so gh never runs with an empty `GH_CONFIG_DIR`, which would read the user's own settings. Open the address and enter the code it prints on your own machine: `GH_BROWSER=true` starts no browser on the host, which would read the user's configuration. The login runs with a configuration folder of its own, made just now: gh's settings in the user's `~/.config/gh`, which agents can change, could name a browser or a pager it would start outside the sandbox. Only the sign-in, `hosts.yml`, is installed where the agents' gh and pocketd read it. The temporary folder is one agents could reach while they run, so sign in again the same way with pocketd stopped.

`gh auth login` asks for `repo`, `read:org` and `gist` by default; `workflow` lets an agent push changes to `.github/workflows`. The token must be in gh's own file, `~/.config/gh/hosts.yml`, not a keyring: the sandbox hides `/run/user`, where a keyring's D-Bus socket is, and passes no `GH_TOKEN`. `--insecure-storage` keeps it in the file; the block passes it only when `gh auth login --help` lists it, since a gh without the flag is old enough to keep the token there anyway. Check it with the environment pocketd gives a command, which prints nothing of the token: `bwrap --dev-bind / / --tmpfs /run/user env -i HOME="$HOME" PATH=/usr/bin:/bin:/usr/local/bin gh auth token --hostname github.com >/dev/null && echo ok`, with `/run/user` hidden as the sandbox hides it, so a keyring cannot answer, and the `PATH` in the order pocketd looks for gh, with `XDG_CONFIG_HOME=…` added as in the shell.

**The `ready-for-human` label.** pocketd keeps it on a linked pull request that is open and not a draft exactly while a cross-review by another family filed `ready` at its head, base branch and merge base, and CI at the head is green: every check run completed as `success`, `neutral` or `skipped`, every commit status `success`, and at least one of them exists, not counting check runs named `pr-triage` or `pr-triage / pr-triage`, whatever made them (`github.ignored`), all read whole within ten pages of 100 each (`github.CIChecks`; a longer or shifting list counts as pending), so a repository with no CI never gets the label ([ADR-0017](../adr/0017-record-cross-review-verdicts-and-set-ready-for-human-in-code.md)). A merged or closed pull request keeps its labels. pocketd writes nothing else on GitHub. Create the label in each repository first, so it has the colour and description you want: `gh label create ready-for-human --repo your-org/your-repo --color 0E8A16 --description "Cross-reviewed, CI green"`.

### 7. The profile folder

The profile folder holds your agents and your team's configuration ([ADR-0013](../adr/0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md)). By default it is `~/.config/pi-pocket-team/agents` of the service user. Agents' shell commands can read it but never change it, and the file tools, `read` included, refuse it. pocketd reads a profile each time it needs one, so an edit takes effect without a restart; `providers.json`, `mcp.json` and the profiles' `mcp` lists take effect at the next start. pocketd refuses a folder replaced since it started (a link put at its name, or another folder moved there) until it starts again, so change files inside it, never the folder itself.

| File | What it is | Rules and decision |
| --- | --- | --- |
| `<id>.md` | one agent: YAML frontmatter of settings, then its identity, whose first `# Heading` is its name | `profiles.Parse`; [ADR-0013](../adr/0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md) |
| `workers.json` | the models a thread's worker runs, in order of preference: `{"models": ["provider/id", …]}`, 1 to 8; without the file, `deepseek/deepseek-flash` | `profiles.ParseWorkers`; [ADR-0022](../adr/0022-run-any-session-on-any-sign-in-and-choose-workers-from-the-owners-list.md) |
| `providers.json` | model servers of your own, and newer models of PiG's providers (`builtinModels`) | `profiles.ParseProviders`; [ADR-0023](../adr/0023-add-the-owners-own-model-providers-from-a-file-checked-in-code.md) |
| `mcp.json` | the MCP servers your profiles' `mcp` lists name, by name, each with its `url` and the `headers` or the `oauth` it signs in with, and no secret: a header's value is `"stored"` | `profiles.ParseMCP`; [ADR-0063](../adr/0063-connect-a-profiles-mcp-servers-to-its-sessions-with-credentials-pocketd-keeps.md) |
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
| `compactAt` | for a standing session, the tokens past which pocketd compacts it once it is idle and has an older part to summarize (a run can go past it), below its model's context window |
| `questionMinutes` | for a supervisor's or a private profile, how long a question to you waits, 1 to 1440 ([ADR-0048](../adr/0048-put-the-supervisors-and-a-private-agents-questions-to-the-owner-inline-in-code.md)) |
| `private` | `true` for an agent of yours alone: no team tools, but the `question` tool and a `schedule` tool of its own for its own sessions ([ADR-0060](../adr/0060-give-a-private-agent-a-schedule-tool-of-its-own-checked-in-code.md)); its sessions start only in its home, though its commands reach what any agent's do, and, for a private profile that is no supervisor, you can rename its own sessions from the desktop ([ADR-0058](../adr/0058-rename-a-private-agents-session-from-the-desktop-checked-in-code.md)) ([ADR-0042](../adr/0042-give-a-private-agent-its-own-preamble-and-no-team-in-its-sessions.md)) |
| `tools` | the coding tools it may call; without it, all of them |
| `mcp` | the servers of `mcp.json` its sessions reach, as `mcp__<server>__<tool>` tools: the sessions you talk in and a building thread's specialist, never a review, a consult, a plan's lead or a worker ([ADR-0063](../adr/0063-connect-a-profiles-mcp-servers-to-its-sessions-with-credentials-pocketd-keeps.md)) |

**Start from the examples** in [`examples/profiles/`](../../examples/profiles/): `supervisor.md`, a standing supervisor on Claude; `builder.md`, a specialist on ChatGPT with one repository; `workers.json`; `schedules.json`; `mcp.json`, whose servers are placeholders to replace with yours; and three prompts. Change `builder.md`'s repository to one of yours in your copy, then copy them into the service's profile folder from your machine. For the default folder:

```sh
scp examples/profiles/*.md examples/profiles/workers.json <user>@<host>:.config/pi-pocket-team/agents/
scp -r examples/profiles/prompts <user>@<host>:.config/pi-pocket-team/agents/
```

Copy `schedules.json` only when you want it, and last, once the folders below exist and `~/.local/bin/pocketd profiles` shows a model under RUNS for both profiles: from then on it sends the supervisor's standing session a message every weekday at 8:30, with nobody at the desktop.

With `POCKETD_PROFILES` set, copy to that folder instead. Over incus, make the `prompts` folder first as the service user (`mkdir ~/.config/pi-pocket-team/agents/prompts`), then push each file without `-r`, with `--uid` and `--gid` of the service user so the files stay yours to edit, and `--project <project>` if you deployed with one: `incus file push --uid <uid> --gid <gid> examples/profiles/prompts/*.md <remote>:<instance><home>/.config/pi-pocket-team/agents/prompts/`, and the same for the profiles, where `<home>` is the user's home folder.

**What must exist on the host:** each profile's `home` and `workspace`, and a git checkout of each repository at its folder, with `origin` on GitHub and a parent folder the service user can write. For cross-reviews and for a session's changes in the desktop, the checkout's repository must have no `info/attributes` file and git no system attributes file (`git var GIT_ATTR_SYSTEM` names it), even an empty one; a tracked `.gitattributes` is fine (`attributesIsolated`). A thread works in `<checkout>--<agent>-<slug>`, on the branch `<agent>/<slug>`, started from a fresh fetch of `origin`'s default branch; when that fetch fails, from the checkout's `HEAD`, and the specialist's brief says so (`team.Worktree`, `team.StaleBase`, [ADR-0056](../adr/0056-start-a-new-worktrees-branch-from-a-fresh-fetch-of-the-remotes-default-branch.md)). For the examples:

```sh
mkdir -p ~/team/supervisor ~/team/builder ~/src
GIT_CONFIG_GLOBAL=/dev/null git -c credential.helper= -c credential.helper='!gh auth git-credential' \
  clone https://github.com/your-org/your-repo ~/src/your-repo
```

Agents can change the user's git configuration, and a credential helper, a filter or a hook it names would run outside the sandbox: `GIT_CONFIG_GLOBAL=/dev/null` reads none of it, the empty `credential.helper` drops any helper the system's configuration names, and the one helper left is gh, from the shell's `PATH`, which holds no folder of the user's. Add a checkout to a running team with pocketd stopped, so no agent can change the new checkout's `.git` while git fills it.

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

**`mcp.json`** names your MCP servers, which pocketd connects itself, outside the sandbox, over HTTPS, or plain HTTP on a loopback or tailnet (`*.ts.net`) host; a command server is refused. Keep their addresses in your private copy. pocketd keeps their secrets in the data folder, never in the profile folder, which agents can read: with pocketd stopped, store a header's value with `~/.local/bin/pocketd mcp set-header NAME HEADER` (the value on standard input), or sign in to an `oauth` server with `~/.local/bin/pocketd mcp login NAME`, which prints a page to approve pocketd in any browser and asks for the address the browser lands on; then `~/.local/bin/pocketd mcp list` shows what is kept and the tools each server listed. A sign-in the server revokes shows as a tool error that says to run `pocketd mcp login NAME` again ([ADR-0063](../adr/0063-connect-a-profiles-mcp-servers-to-its-sessions-with-credentials-pocketd-keeps.md)).

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
make app      # package the host client (NSGo on Linux, MyGo on macOS)
```

- **Linux** uses NSGo and GTK 4. `make run-mygo` and `make app-mygo` select the GTK 3 fallback. See [release candidates](../../packaging/README.md) for Flatpak installation and web serving.
- **macOS:** `make app` builds `PocketGo.app` and a disk image. Notifications need the packaged app ([ADR-0049](../adr/0049-notify-the-owner-of-what-waits-once-decided-in-code.md)).
- **The MyGo Terminal** draws with Ghostty's emulator, MyGo's terminal plugin, which loads `libghostty-vt`. `make app` puts the library in the app. Run from source, it is downloaded on first use into your cache folder (`mygo/natives/`), checked by its SHA-256; `MYGO_GHOSTTY_VT` names a copy of your own instead.
- **The device token** is kept in the system keyring, or in a private file in the desktop's config folder only when the platform has no keyring (`config.Credentials`). The config folder is `pocketgo` in your user config folder; `POCKETGO_CONFIG_DIR` names another.

**Sign in:** enter the service URL (step 3) and the device's token (step 8), then Connect. The desktop keeps the token, its pins and what you have seen by the service's origin, so a new URL means signing in again. The desktop and pocketd must speak the same protocol version: rebuild the desktop from the same commit after a deploy that changed it.

**The iPad and iPhone** build from the same code on a Mac with Xcode: follow [Try it on a Mac](../plans/phone-client.md#try-it-on-a-mac) in the phone and tablet plan, which also says what does not work on iOS yet.

**Approve and merge from the desktop** is optional: without it, merge on GitHub ([ADR-0018](../adr/0018-approve-and-merge-as-the-owner-from-the-desktop-with-pockets-gates.md)). It acts as you, through a GitHub App's device-flow sign-in, and only for the one login and App compiled in: `Login` in `desktop/internal/github/client.go`, and `AppName`, `AppSlug` and `AppClientID` in `desktop/internal/github/app.go`. These name the repository owner's, so a fork needs an App of its own. The App's sign-in is kept only in the system keyring, with no file fallback (`config.GitHubApp`): without a keyring, Approve and merge stays off.

1. On GitHub, under Settings › Developer settings › GitHub Apps, make a new App. Turn on Enable Device Flow, keep Expire user authorization tokens on (the desktop takes only a pair with a refresh token and both expiries, `OAuth.pair` in `desktop/internal/github/app.go`), and turn off the webhook.
2. Give it these repository permissions (`appPermissions` in `desktop/internal/github/reads.go`): Checks read, Commit statuses read, Contents read and write, Metadata read, Pull requests read and write, Workflows read and write. Merge queues read and write is optional: without it, an enqueue may fail, and the desktop says so.
3. Install it on each account or organization whose pull requests you merge, with All repositories or a selection that holds each repository you merge in: the desktop checks the installation's account and permissions, not its repositories. An App that may be installed only on its own account installs on no other account: choose Any account when you create it, unless every repository you merge in belongs to the account that owns the App, organization or not.
4. In your fork, set the four constants to your login and your App's name, slug and client ID, then build the desktop again (`make run` or `make app`): the client ID and the login are compiled in. Tests in `desktop/internal/github`, `desktop/internal/config`, `desktop/internal/state` and `desktop/internal/app` name the current login; change them too, or `make verify` fails.
5. In the rebuilt desktop, choose Sign In to GitHub (the menu, or a pull request's card) and enter the code it shows at GitHub's device page.

A merge then needs pocketd's backing at the pull request's head and base branch: a `ready` verdict and green CI, on an open pull request that is no draft (`state.Backed`). That is a prerequisite: Check to Merge then reads frostyard/pocket's gates on GitHub, which refuse some repositories, classic branch protection among them (`repoGate` in `desktop/internal/github/gates.go`). Merge on GitHub when it refuses. A fork with its own App has not been tried.

### 10. First run: a delegated thread

With the example profiles in place, their folders made, sign-ins for both families, and `~/.local/bin/pocketd profiles` showing a model under RUNS for `supervisor` and `builder`:

1. In the desktop, choose New Session, pick the Supervisor profile, and choose Open Session. It opens the supervisor's standing session.
2. Ask for a small change, for example: "Delegate to builder: in your-org/your-repo, fix a typo in the README and open a pull request." For a thread on a repository, as this one, pocketd links a pull request only from the thread's branch to the profile's repository itself (`team.CheckPull`): to try this on a fork, name the fork in `repositories` and let the pull request target the fork.
3. The supervisor calls `team_delegate`. A thread appears on the Board under "With the specialists", and the builder's session starts with pocketd's brief as its first message, in a new worktree beside the checkout when the delegation names a repository.
4. A question the builder needs you for comes to the Inbox as an ask, and the thread moves to "Waits for you". Answer it there.
5. The builder opens the pull request and reports it. pocketd links it to the thread, and the pull request shows in the Inbox at once, open and not a draft. pocketd starts its cross-review on the other family's model ([ADR-0019](../adr/0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md)): a review thread on the Board, whose verdict shows on the pull request's card.
6. Once the verdict is `ready` and CI is green, `ready-for-human` goes on the pull request. A repository with no CI never gets the label ([GitHub for the agents](#6-github-for-the-agents)), and the desktop's merge needs green CI too: merge such a pull request on GitHub.
7. Merge it, on GitHub or from the desktop. pocketd's next read of the pull request sees the merge and closes the thread, which a later message to its specialist, from you or the supervisor's `team_send`, reopens only while its worktree is still there; once pocketd removed it, delegate a new thread (`team.FolderGone`, [ADR-0061](../adr/0061-reopen-a-closed-thread-when-a-message-reaches-its-specialist.md)); the reads run in rounds, the next `-pr-every` after a round ends. Once the thread's sessions are idle, pocketd may remove its worktree: only when the tree is clean and its commits are in the pull request, and not always then: a locked worktree, or a tracked file marked `assume-unchanged` or `skip-worktree`, keeps it. Untracked files that `.gitignore` matches keep nothing, and go with the worktree; a tracked file with changes keeps it, whatever `.gitignore` says. When it keeps one, the thread's report says why. It then deletes the branch with `git branch -d`; when git refuses, as after a squash merge, the branch stays and only pocketd's log says why ([ADR-0054](../adr/0054-remove-a-closed-threads-worktree-in-code-with-no-github-write.md)).

That is a working setup. Everything after builds on it: shifts in rounds ([ADR-0024](../adr/0024-run-a-delegated-thread-in-rounds-as-a-shift-driven-in-code.md)), plans and consults ([ADR-0020](../adr/0020-plan-before-building-with-the-plan-gate-and-consults-in-code.md)), workers ([ADR-0022](../adr/0022-run-any-session-on-any-sign-in-and-choose-workers-from-the-owners-list.md)), schedules and gathers, search, usage, notifications and a session's changes, as the [architecture](architecture.md#service) describes them.

## Operational notes

### Upgrades

Stop pocketd first, from the shell of step 2 (`systemctl --user stop pocketd`): `deploy.sh` stages the new program in the service user's home before it installs it, where running agents could change it, and the stop ends every command they run. Then run your `make deploy` command from step 2 again, with the same variables, from the new commit: running tasks resume at the start, and the work database's migrations run at the start. When `pocketd version` shows a new protocol number, rebuild the desktop from the same commit; a desktop of another version is refused.

### Backups

`durable.sqlite` and `work.sqlite` are one state: the threads, asks and reports of one name the conversations of the other. Back up the data folder as a whole, with pocketd stopped, to a place the agents cannot read, since it holds `auth.json`: off the host, or in a folder of root's. As root, for the default data folder (`-M <user>@` reaches the user's systemd from root, in systemd 248 or later):

The block below copies the whole data folder, sign-ins included, but not the files of open sessions, which live in their working folders (the checkouts with their worktrees, the agents' homes and workspaces), nor the attachments in `~/.local/share/pocketgo-uploads`, whose paths the messages name. So take it when no thread is open and no session runs: a restore then resumes nothing whose files it lacks.

```sh
install -d -m 0700 /var/backups/pocketgo
b=/var/backups/pocketgo/$(date -u +%Y%m%dT%H%M%SZ)
systemctl --user -M <user>@ stop pocketd && install -d -m 0700 "$b" &&
  cp -a ~<user>/.local/state/pocketgo "$b/data" &&
  echo "backup done"
systemctl --user -M <user>@ start pocketd
```

Every copy runs only after the stop succeeded, and the backup is whole only when `backup done` printed. A backup of work in flight is beyond this guide: it must copy, in the same stop, every open session's working folder as its session names it, links and where they lead, a linked checkout's common git folder, and the uploads.

To restore, with pocketd stopped, in this order:

1. While the current data folder is still there, note the open pull requests that carry `ready-for-human`, as for a wipe: the restored databases may not link them all, and pocketd takes no label off a pull request it does not link, so you remove those yourself on GitHub after the restore. List its devices with `~/.local/bin/pocketd device list` as the service user, and copy its `auth.json` aside, out of the agents' reach.
2. Move the current data folder out of the agents' reach, under the root-only backup folder or off the host, never beside the data folder, which the sandbox does not hide.
3. A restored task resumes in the working folder it had, which may since have gone, and its messages name their attachments by path: restore a backup taken with no thread open, or, if you saved working folders and the uploads, put them back at their paths now, owned by the service user, leaving the data folder out of them even when a profile's home holds it.
4. Put the saved data folder back whole, owned by the service user, then the current `auth.json` in place of the saved one: a subscription may have replaced its refresh token since the backup.
5. List the devices again and revoke, with `~/.local/bin/pocketd device revoke <name>`, every restored device that is active and not active with the same ID in the current list: the restored `work.sqlite` holds every token as it was at the backup, a device since revoked or replaced included. Add a device you still use again under a new name.
6. Reset the desktop's settings as for a wipe, below: the restored databases may give ids the desktop saw for other things since. Then start pocketd.

Without the current folder, revoke every restored device that is active, add the ones you use again, set the restored `auth.json` aside where agents cannot read it, and sign in again to the models you still use. pocketd does not reconcile one database restored to an earlier point than the other. The uploads folder, the profile folder and the checkouts are not in the data folder: keep them as you keep the rest of your configuration. An open session's files are in its working folder, which no database holds: a thread's worktree beside its checkout, or, for a thread on no repository, its specialist's home or workspace. To move to another host, close or finish the open threads first, for the same reason, then stop pocketd on the old host and keep it from starting again, even at a reboot, with `systemctl --user disable --now pocketd`, before you copy the data folder, copy the data folder, every working folder (the checkouts with their worktrees, the agents' homes and workspaces) and the uploads folder at the same paths, and start pocketd only on the new host: each host's lock holds only its own data folder, and two would resume the same work. On the new host, give the service user the same UID and GID as on the old one (`groupadd -g <gid> <user>`, then `useradd -m -s /bin/sh -u <uid> -g <gid> <user>`), or give the copied files to the new user with `chown -R` before pocketd starts: pocketd refuses an uploads folder another UID owns, and git a checkout. Set everything up before pocketd first starts with the moved state, since its start resumes work and runs the schedules at once: `pocketd.env` and any drop-in as on the old host, so the data folder is where you copy it, but with this host's own tailnet address in `POCKETD_LISTEN` if you set one (step 3), and its service URL on your devices; the GitHub sign-in (step 6); the profile folder with `schedules.json`, the global `AGENTS.md` and the skills (step 7), since a missing `schedules.json` drops the schedules' records at the start. `make deploy` starts the unit: stop it at once, copy the data folder, the working folders and the uploads, leaving out what `make deploy` installed (`~/.local/bin/pocketd`, `~/.config/systemd/user`) even when a home holds it, since the old host's program may be built for another architecture, then start it.

### Starting over

To wipe the work and keep the sign-ins, stop pocketd first, so it puts no label on while you look, then note on GitHub the open pull requests that carry `ready-for-human`: the new databases link none of them, so pocketd will never take a label off again, and you remove those yourself. Then move `durable.sqlite` and `work.sqlite`, and any `-wal` and `-shm` files beside them, out of the data folder together, to a place the agents cannot read, and start it. pocketd makes both databases anew, seeded by their migrations, and you add each device again. Worktrees, branches and uploads stay where they are. The desktop keeps its pins, seen marks and notified asks by the service URL, and the new databases may give sessions and asks ids the old ones had: for a clean start there too, quit the desktop and move its `settings.json`, in its config folder, aside before you sign in again.

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

**Try a host before the first deploy:** build `pocketd` for it in `service` (`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/pocketd`, or `GOARCH=arm64` for an arm64 host, as `deploy.sh` picks from `uname -m`), copy it into the service user's home as `~/pocketd-trial`, executable, since `/tmp` may be mounted `noexec`, and run it in the shell of step 2 with a new, empty data folder: `d=$(mktemp -d) && setpriv --no-new-privs ~/pocketd-trial serve -data "$d" -profiles "" -listen 127.0.0.1:0; rm -rf "$d"`, where `setpriv --no-new-privs` matches the unit's `NoNewPrivileges=yes`, so a bwrap that works only as setuid fails here as it would there. `-profiles ""` reads no profile, schedule or provider, so nothing of yours runs. It logs `pocketd serving`, or why the check failed; stop it with Ctrl-C, and the folder goes. Remove `~/pocketd-trial` after.

**Other failures at the start:** a port in use fails the bind; another `pocketd serve` on the same data folder holds `pocketd.lock`; a profile folder pocketd cannot look at stops the start, but one whose files it cannot read does not, and neither does an invalid `providers.json`: check `~/.local/bin/pocketd profiles` and the log even when the unit runs. After a start fails, `systemctl --user restart pocketd` tries again.

## References

- Rationale: [ADR-0005](../adr/0005-keep-the-agent-loop-and-model-credentials-in-the-service.md), [ADR-0010](../adr/0010-port-the-desktop-client-selectively-from-frostyard-pocket.md), [ADR-0012](../adr/0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md), [ADR-0013](../adr/0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md), [ADR-0014](../adr/0014-delegate-in-threads-with-team-tools-in-code.md), [ADR-0016](../adr/0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md), [ADR-0017](../adr/0017-record-cross-review-verdicts-and-set-ready-for-human-in-code.md), [ADR-0018](../adr/0018-approve-and-merge-as-the-owner-from-the-desktop-with-pockets-gates.md), [ADR-0019](../adr/0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md), [ADR-0020](../adr/0020-plan-before-building-with-the-plan-gate-and-consults-in-code.md), [ADR-0021](../adr/0021-give-a-threads-specialist-a-worker-on-an-api-key-model-in-code.md), [ADR-0022](../adr/0022-run-any-session-on-any-sign-in-and-choose-workers-from-the-owners-list.md), [ADR-0023](../adr/0023-add-the-owners-own-model-providers-from-a-file-checked-in-code.md), [ADR-0024](../adr/0024-run-a-delegated-thread-in-rounds-as-a-shift-driven-in-code.md), [ADR-0025](../adr/0025-give-the-owner-a-shell-on-the-service-host-from-the-desktop.md), [ADR-0026](../adr/0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md), [ADR-0029](../adr/0029-attach-the-owners-files-to-a-message-stored-outside-the-data-folder.md), [ADR-0030](../adr/0030-send-scheduled-messages-from-the-owners-file-and-the-supervisors-tool-in-code.md), [ADR-0033](../adr/0033-gather-proposals-from-the-team-in-read-only-sessions-in-code.md), [ADR-0035](../adr/0035-make-the-skills-and-uploads-folders-read-only-to-agents.md), [ADR-0042](../adr/0042-give-a-private-agent-its-own-preamble-and-no-team-in-its-sessions.md), [ADR-0043](../adr/0043-load-the-global-and-folder-agents-md-and-the-skills-into-every-session.md), [ADR-0045](../adr/0045-make-the-global-instructions-file-read-only-to-agents.md), [ADR-0048](../adr/0048-put-the-supervisors-and-a-private-agents-questions-to-the-owner-inline-in-code.md), [ADR-0049](../adr/0049-notify-the-owner-of-what-waits-once-decided-in-code.md), [ADR-0051](../adr/0051-insert-the-owners-saved-prompts-into-the-composer-with-a-slash.md), [ADR-0054](../adr/0054-remove-a-closed-threads-worktree-in-code-with-no-github-write.md), [ADR-0056](../adr/0056-start-a-new-worktrees-branch-from-a-fresh-fetch-of-the-remotes-default-branch.md), [ADR-0062](../adr/0062-sign-pocketd-in-to-github-copilot-from-the-desktop-with-a-device-flow-it-runs.md), [ADR-0063](../adr/0063-connect-a-profiles-mcp-servers-to-its-sessions-with-credentials-pocketd-keeps.md), [ADR-0058](../adr/0058-rename-a-private-agents-session-from-the-desktop-checked-in-code.md), [ADR-0060](../adr/0060-give-a-private-agent-a-schedule-tool-of-its-own-checked-in-code.md), [ADR-0061](../adr/0061-reopen-a-closed-thread-when-a-message-reaches-its-specialist.md), [ADR-0053](../adr/0053-show-a-sessions-changes-read-with-isolated-git-in-code.md), [ADR-0074](../adr/0074-keep-the-prompt-cache-warm-between-a-runs-requests-in-code.md)
- Contracts: [specs/protocol.md](../specs/protocol.md)
- Reference: [system architecture, operational notes](architecture.md#operational-notes)
- Built in: [roadmap — Phase 2, item 36](../plans/roadmap.md#phase-2--team-work-board-and-inbox), for [frostyard/pocketgo#45](https://github.com/frostyard/pocketgo/issues/45)
