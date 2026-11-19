# Plan: Roadmap

This plan delivers the [system architecture](../design/architecture.md) in phases. It replaces a setup spread across four repositories: pi-pocket, pi-pocket-team, pi-pocket-config and frostyard/pocket. Together they hold about 170k lines of server and extension code. The plan ports the behaviour this system needs, not every feature.

## What carries over

| Source | Take | Leave |
| --- | --- | --- |
| [TannerMidd/pi-pocket](https://github.com/TannerMidd/pi-pocket) | Behaviour and lessons only (list below the table). | All code, the web UI, Design, the extension gallery, themes, the built-in browser, self-update and the launcher. |
| [bketelsen/pi-pocket-team](https://github.com/bketelsen/pi-pocket-team) | The domain model (list below the table). | The drop-in plumbing, the untyped `run` map, and the `night-shift` names. |
| [frostyard/pocket](https://github.com/frostyard/pocket) | `internal/github`, `internal/config`, the frostui shell, the session views, notification filtering, terminal replay and the test harness ([ADR-0010](../adr/0010-port-the-desktop-client-selectively-from-frostyard-pocket.md)). | `internal/pocket`, Arrakis and the wiki. |
| [bketelsen/pi-pocket-config](https://github.com/bketelsen/pi-pocket-config) | The 13 agent profiles as seed data, from the private repository. The workflow rules in its `AGENTS.md`, several of which become code. | Syncing between the repository and live files. |

**From pi-pocket, carry over these behaviours and lessons:**
- Request ids decide whom a request was made by, and make retries harmless.
- Rules for what may run again after a restart.
- Approval answers are remembered per tool call.
- Waiting out provider rate limits.
- Keeping the prompt cache warm.
- Fencing and defanging inbox text.
- Approving exactly the lesson the owner was shown.
- Only people choose models.

**From pi-pocket-team, port the domain model:**
- The thread, ask, PR link, watch and review-verdict schemas.
- The plan, plan-task and consult schemas.
- The shift stop rules and the limits on lessons.

## Phase 0 — Repository and decisions (done)

- [x] Repository created from bketelsen/agentic-template, with labels, the `verify` ruleset and private vulnerability reporting.
- [x] Template adopted: README, `AGENTS.md`, `SECURITY.md` and the ADR dates.
- [x] ADRs 0004 to 0010, this roadmap and the [architecture](../design/architecture.md).
- **Done when:** `make ci` is green on `main` with the decisions recorded.

## Phase 1 — One durable session, end to end (done)

- [x] **Protocol:** the `proto` module and [specs/protocol.md](../specs/protocol.md) ([architecture: protocol](../design/architecture.md#protocol)).
- [x] **Service:** the `service` module with PiG pinned (v0.7.0), `service/pig/`, device authentication, sessions and the event stream ([architecture: service](../design/architecture.md#service)).
- [x] **Approvals:** tool approval through `BeforeTool`, stored so it survives a restart (`TestAWaitingApprovalSurvivesARestart`).
- [x] **Deployment:** `make deploy` installs `pocketd` as a systemd user service ([architecture: operational notes](../design/architecture.md#operational-notes)).
- [x] **First deploy to the team VM** over incus: `pocketd` runs as `team`'s user service on the tailnet address, port 8788, and the laptop has a device.
- [x] **Subscription sign-in on the team VM:** `pocketd login PROVIDER`, which the owner runs. Both use the service's data folder: the default `~/.local/state/pocketgo`, or, when `~/.config/pocketd.env` sets `POCKETD_DATA`, the same path passed as `-data`. The owner signed in to `anthropic` and `openai-codex` on 2026-11-19.
- **Desktop:** the `desktop` module, with the data layer on `proto` and the session view ported from frostyard/pocket ([architecture: desktop client](../design/architecture.md#desktop-client)).
  - [x] the data layer and state in `desktop/internal/state`, with no UI import: sign-in, the event stream with backoff and `Last-Event-ID`, refetch on reconnect and `resync`, and the merge rules
  - [x] sign-in with the device token in the desktop keyring, and a warning before plain HTTP to a host other than loopback or a `*.ts.net` name, and no redirect to another origin
  - [x] the sessions with their state, the Inbox of pending approvals, and the New Session dialog
  - [x] the session view: transcript, streaming reply, tool calls and results, approval cards (Allow, Deny with a reason), Send and Abort
  - [x] headless view tests against a fake `pocketd`, `make snapshot`, `make app`, and `go tool mygo vet` in `make verify`
  - [x] the Done-when run on the team VM, on 2026-11-19 with `pocketd` ee6f41f:
    - The owner started a session from the desktop on the `openai-codex` sign-in and approved a `write` and a `bash` call.
    - The mid-run restart ran in the desktop, driven by MyGo's automation under Xvfb with the owner's device:
      - a session started from New Session on `anthropic` asked for a `write`, and its approval card showed
      - `pocketd` restarted: the desktop lost the stream, reconnected a second later, took the `resync` and loaded everything again
      - the same approval card came back and was allowed from the desktop
      - the run finished with the `write` done and the reply, and the transcript showed every entry from before the restart
- **Repository tooling:**
  - [x] the three modules, `go.work`, and the Go gates in `make ci` and CI
  - [x] `gomod` entries in `.github/dependabot.yml`
  - [x] risk paths for the Go workspace, module files, lint config and `proto/` (L2)
  - [x] risk paths for `service/pig/`, authentication, the store and the approval rule
  - [x] risk paths for the desktop's token storage and keyring wiring and `desktop/mygo.json` (L1), and its plain-HTTP rule (L2)
- [x] [ADR-0011](../adr/0011-put-rules-in-code-not-in-model-judgment.md): rules go in code, not in model judgment.
- **Done when:** on the team VM, the owner starts a session from the desktop on a subscription sign-in, approves a tool call, and restarts `pocketd` mid-run. The run resumes with its transcript intact.

## Phase 2 — Team, work board and inbox

Built in this order, one pull request each:

1. [x] **Profiles and the router seam** ([ADR-0013](../adr/0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md), [architecture: service](../design/architecture.md#service), [specs: protocol rules 7b and 21 to 23](../specs/protocol.md)).
   - Profiles are read from pi-pocket-team's folder and checked in code, with the new `tools` field, and the folder is read-only to agents.
   - The router chooses every model, with the profile's model as its rule and `openai` aliased to `openai-codex`, and each choice is recorded as the session's `route`.
   - A session can start from a profile, from the desktop's New Session dialog.
2. [x] **Threads, delegation and asks:** the supervisor delegates to a specialist in a thread (`team_delegate` and `team_message` as tools in code), a thread with a repository gets a worktree and one without works in the specialist's home or workspace, and the desk cap and the private-agent rule apply. Asks wait in the Inbox for the owner's answer ([ADR-0014](../adr/0014-delegate-in-threads-with-team-tools-in-code.md), [architecture: delegation](../design/architecture.md#service), [specs: protocol rules 24 to 30](../specs/protocol.md)).
   - Built: the team tools by role, threads and asks in the work database, protocol version 4, and asks in the desktop's Inbox. `TestDelegationEndToEndOverHTTP` drives a delegation, a report with an ask and the owner's answer through the protocol, with a scripted model.
   - Seen on 2026-11-19 through the desktop on the team VM (pocketd 5117e98, protocol 4), merged as #16. An Odrade session on a Claude Haiku 5.5 override delegated thread T1 to Duncan in his home. Duncan's `blocked` report, with ask A1, reached Odrade's session. A1 waited in the desktop's Inbox, the owner answered it there, and the answer reached Duncan's session, which replied.
   - Then T2 went to Duncan with a repository: pocketd made the worktree `<checkout>--duncan-worktree-check-t2-<owner>` on the branch `duncan/worktree-check-t2-<owner>` in the sandbox, Duncan ran `pwd` and `git rev-parse` there, and Odrade closed T2 with `team_close`. The owner closed T1 through the protocol.
   - Seen then, for a later slice: an answer goes only to the session that asked, so the supervisor still thought T1 waited for A1.
3. [x] **Lessons:** `team_memory` proposes a lesson, and the owner approves exactly the text shown. The limits are 300 characters per lesson, 40 approved per agent and 20 pending per writer. Approved lessons join the agent's instructions ([ADR-0015](../adr/0015-keep-each-agents-lessons-approved-by-the-owner-in-code.md), [architecture: lessons](../design/architecture.md#service), [specs: protocol rules 31 to 35](../specs/protocol.md)).
   - Built: lessons in the work database, `team_memory` for every profile's session, protocol version 5, and lessons in the desktop's Inbox. `TestALessonEndToEndOverHTTP` takes a lesson from a `team_memory` call, through the owner's approval over the protocol, into the running session's instructions, with the real engine.
   - Seen on 2026-11-19 through the desktop on the team VM (pocketd d7d6c62, protocol 5), merged as #18. An Odrade session on a Claude Haiku 5.5 override called `team_memory` to add a lesson; it waited in the desktop's Inbox as "A lesson for Odrade waits for your approval", with its exact text, and the owner approved it there.
   - The approval reached the instructions of both Odrade sessions, the one started before protocol 5 included, as `- [L1] <text>` between the section's marker lines; Duncan's sessions got the section with no lesson, and plain sessions none.
   - The owner then forgot L1 from the Inbox's list of lessons in force, and both sessions' sections went back to none.
4. [ ] **Pull requests:** a reported pull request is checked on GitHub and linked to its thread, and its state is kept current.
5. [ ] **Inbox and work board in the desktop:** asks, lessons and pull requests in the Inbox, and threads on the board.
6. [ ] **Merge gates and their risk paths:** cross-review verdicts, the `ready-for-human` rules, and the bot never both approving and merging one pull request.
7. [ ] **Plans and consults:** plans, plan tasks and consults, with the plan gate.

- **Policies enforced in code:** see the [architecture](../design/architecture.md#policies-enforced-in-code).
- [x] **Tools run without approval** in a bubblewrap sandbox that hides pocketd's data folder and keeps its start files read-only ([ADR-0012](../adr/0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md), [architecture: operational notes](../design/architecture.md#operational-notes)). Done 2026-11-19.
- **Done when:** the owner asks the supervisor for a change, a specialist opens a pull request in a thread, and the pull request, its asks and the specialist's proposed lessons appear in the inbox. Slices 1 to 5 reach it.

## Phase 3 — Rounds

- **Round-based jobs:** an objective, a completion condition, a round limit, a deadline, a spend limit and a stop.
- **Pausing:** the job pauses when the owner writes in the session.
- **Done when:** four test jobs each stop for a different reason (completion condition, round limit, deadline, the owner's Stop), and each thread shows why it ended.

## Phase 4 — Coder execution adapter

- **The adapter:** `service/exec/coder/` with its risk path, a Coder REST client, and a workspace template that includes `pi-env` ([architecture: execution adapters](../design/architecture.md#execution-adapters)).
- **Done when:** a thread set to run in Coder runs its commands in a prebuilt workspace, and that workspace holds no model credential.

## Phase 5 — Events, metrics and follow-ups

- **Events:** GitHub webhooks, and the NATS events bridge.
- **Records:** run records, the metrics views and scheduled follow-ups.
- **Done when:** a signed GitHub webhook wakes the responsible agent, and the metrics view compares outcomes by model and by way of working.

## Later / ideas

- **History migration from pi-pocket.** PiG can probably open `pocket.sqlite`, but pi-pocket's own document kinds need an importer. A fresh start with the profiles and lessons imported is acceptable.
- **A phone client** on MyGo's iOS target, reusing the client's data layer. Revisit push notifications with it.

## Out of scope

- Multiplayer: invites, roles, side chat, taking turns.
- A web UI or PWA.
- Design, the extension gallery, themes, the built-in browser.
- Self-update, the launcher and tunnels. Use systemd and Tailscale instead.
- Relays to Discord, Slack, Telegram, Matrix or RSS.
- Pi TypeScript extensions.
- PiP, which the supervisor replaces.
- Arrakis and the homewiki.

## Open questions

- **How the model router decides:** decided from the Phase 5 run records. It is recorded as an ADR once there's a conclusion.

## References

- Implements: [design: system architecture](../design/architecture.md)
- Decisions: [ADR-0004](../adr/0004-build-the-service-on-pigs-durable-harness.md) to [ADR-0011](../adr/0011-put-rules-in-code-not-in-model-judgment.md)
