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

## Phase 1 — One durable session, end to end (large)

- [x] **Protocol:** the `proto` module and [specs/protocol.md](../specs/protocol.md) ([architecture: protocol](../design/architecture.md#protocol)).
- [x] **Service:** the `service` module with PiG pinned (v0.7.0), `service/pig/`, device authentication, sessions and the event stream ([architecture: service](../design/architecture.md#service)).
- [x] **Approvals:** tool approval through `BeforeTool`, stored so it survives a restart (`TestAWaitingApprovalSurvivesARestart`).
- [x] **Deployment:** `make deploy` installs `pocketd` as a systemd user service ([architecture: operational notes](../design/architecture.md#operational-notes)).
- [ ] **On the team VM:** ssh access for the deploy, the first deploy, then `pocketd login PROVIDER` and `pocketd device add NAME`, which the owner runs.
- **Desktop:** the `desktop` module, with the data layer on `proto` and the session view ported from frostyard/pocket ([architecture: desktop client](../design/architecture.md#desktop-client)).
- **Repository tooling:**
  - [x] the three modules, `go.work`, and the Go gates in `make ci` and CI
  - [x] `gomod` entries in `.github/dependabot.yml`
  - [x] risk paths for the Go workspace, module files, lint config and `proto/` (L2)
  - [x] risk paths for `service/pig/`, authentication, the store and the approval rule
  - [ ] risk paths for `service/exec/coder/` and the merge gates, as each lands
- [x] [ADR-0011](../adr/0011-put-rules-in-code-not-in-model-judgment.md): rules go in code, not in model judgment.
- **Done when:** on the team VM, the owner starts a session from the desktop on a subscription sign-in, approves a tool call, and restarts `pocketd` mid-run. The run resumes with its transcript intact.

## Phase 2 — Team, work board and inbox

- **Profiles:** seeded from pi-pocket-config, with the new `tools` field.
- **Work:** supervisor delegation, consults, threads, plans and asks.
- **Lessons:** proposal and approval.
- **Model router:** the seam, with the profile's model as its only rule.
- **Inbox and work board:** in the desktop, plus the ported merge gates.
- **Policies enforced in code:** see the [architecture](../design/architecture.md#policies-enforced-in-code).
- **Done when:** the owner asks the supervisor for a change, a specialist opens a pull request in a thread, and the pull request, its asks and the specialist's proposed lessons appear in the inbox.

## Phase 3 — Rounds

- **Round-based jobs:** an objective, a completion condition, a round limit, a deadline, a spend limit and a stop.
- **Pausing:** the job pauses when the owner writes in the session.
- **Done when:** four test jobs each stop for a different reason (completion condition, round limit, deadline, the owner's Stop), and each thread shows why it ended.

## Phase 4 — Coder execution adapter

- **The adapter:** `service/exec/coder/`, a Coder REST client, and a workspace template that includes `pi-env` ([architecture: execution adapters](../design/architecture.md#execution-adapters)).
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
