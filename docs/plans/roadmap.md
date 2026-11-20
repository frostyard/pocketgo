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
4. [x] **Pull requests:** a reported pull request is checked on GitHub and linked to its thread, and its state is kept current ([ADR-0016](../adr/0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md), [architecture: pull requests](../design/architecture.md#service), [specs: protocol rules 36 to 40](../specs/protocol.md)).
   - Built: `pull_requests` on `team_message`, read with `gh` in the sandbox and checked in code, linked in the work database, kept current every 5 minutes, protocol version 6, and threads closed once their pull requests merged.
   - Seen on 2026-11-19 on the team VM over the protocol (pocketd a67ab9b, protocol 6, merged as #20; the desktop shows pull requests from slice 5). An Odrade session on a Claude Haiku 5.5 override delegated T3 to Duncan in his home, and Duncan reported frostyard/hotedge#1 with `team_message`.
   - pocketd read it on GitHub with the trusted `gh`, in the sandbox, with the empty configuration folder and the bot account's token: it linked GitHub's pull request 4639470725, "Document hotedge fork divergence from upstream", as merged, and closed T3 with the report `Closed: its pull requests merged (frostyard/hotedge#1).`. The supervisor's message named `Pull requests: frostyard/hotedge#1 (merged)`.
   - Then seen on 2026-11-19 in the owner's "Done when" run (pocketd a67ab9b): T6 linked bketelsen/pi-pocket-config#80, open, in the owner's private configuration repository. The owner merged it on GitHub; the next round of `-pr-every` read it as merged and closed T6 at 21:37 UTC with `Closed: its pull requests merged (bketelsen/pi-pocket-config#80).`.
5. [x] **Inbox and work board in the desktop:** asks, lessons and pull requests in the Inbox, and threads on the board ([architecture: desktop client](../design/architecture.md#desktop-client), [ADR-0014](../adr/0014-delegate-in-threads-with-team-tools-in-code.md), [ADR-0016](../adr/0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md), [specs: protocol rules 24 to 40](../specs/protocol.md)).
   - Built: the work board, with the threads grouped by who moves them next (`state.Data.NextMover`), their reports, pull requests and sessions, and Close Thread; pull requests that wait for review in the Inbox, opened on GitHub at a link the desktop builds (`state.PullURL`). No protocol change: protocol version 6.
   - Seen on 2026-11-19 through the desktop, built from this change and run under Xvfb, against the team VM's pocketd (a67ab9b, protocol 6). An Odrade session on a Claude Haiku 5.5 override delegated T4 to Duncan, who asked A2. The work board showed T4 under "Waits for you" with "A2 waits for your answer.", and the Inbox's card for A2 named "T4 · Slice 5 board check".
   - The owner answered A2 in the Inbox, and T4 moved to "With the supervisors"; Odrade closed it with `team_close` once Duncan reported it done. T5, reported `working`, showed under "With the specialists", and the owner closed it with the board's Close Thread after confirming.
   - Then seen on 2026-11-19 in the owner's "Done when" run, through the same desktop: the Inbox said "1 question and 1 pull request wait for you", with A3 and "bketelsen/pi-pocket-config#80 waits for your review", each naming T6, and the board showed T6 under "Waits for you" with both.
6. [x] **Merge gates and their risk paths:** cross-review verdicts, the `ready-for-human` rules, and the bot never both approving and merging one pull request. Two pull requests:
   - [x] **6a, cross-review in code** ([ADR-0017](../adr/0017-record-cross-review-verdicts-and-set-ready-for-human-in-code.md), [architecture: cross-review](../design/architecture.md#service), [specs: protocol rules 41 to 45](../specs/protocol.md)). Built: `team_verdict` for a thread's specialist, the family rule (`router.Family`), verdicts bound to the head pocketd reads, `ready-for-human` kept in step with them and with CI by pocketd, protocol version 7, and the verdict on the desktop's cards.
     - Seen on 2026-11-20 in 6c's run below: the reviewer, on a model of the OpenAI family, filed a `ready` verdict on a Claude author's pull request at the head pocketd read, and pocketd put `ready-for-human` on it once CI at that head was green.
   - [x] **6b, approve and merge as the owner** ([ADR-0018](../adr/0018-approve-and-merge-as-the-owner-from-the-desktop-with-pockets-gates.md), [ADR-0010](../adr/0010-port-the-desktop-client-selectively-from-frostyard-pocket.md), [architecture: desktop client](../design/architecture.md#desktop-client)). Built: frostyard/pocket's `internal/github` ported whole with its tests, the GitHub App sign-in by the device flow in the keyring behind a lock, and on the Inbox's pull request cards Check to Merge, Open Diff for `risk:high`, and Approve + Merge after a confirmation, as the owner at the head the gates read. Not ported: batches, the operation journal and Desk receipts.
     - Seen on 2026-11-20 in 6c's run below: the owner approved frostyard/nsl#124 at 05:07:48 UTC and merged it at 05:07:55 UTC from a desktop built from main (protocol 8), as bketelsen, through the GitHub App sign-in, as ADR-0018 has the owner do both. pocketd's account only put on `ready-for-human`: it neither approved nor merged the pull request.
   - [x] **6c, pocketd starts each cross-review** ([ADR-0019](../adr/0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md), [architecture: cross-review](../design/architecture.md#service), [specs: protocol rules 25, 41, 42 and 46](../specs/protocol.md)). Built: one review thread a head and base branch, started in code when a report links a pull request and at every round for the pull requests it read, with the author's own agent on the reviewer model of another family, in a worktree detached at the head, with read-only tools and pocketd's brief; the owner's budget of 4 reviews; only those threads file `team_verdict`, and a verdict closes its review and tells the author once; a verdict backs its pull request only at the head, base branch and merge base it was filed on (protocol 8). Seen as needed on 2026-11-19: bketelsen/pi-pocket-config#81 got no review, since its author followed pi-pocket's skill, which starts a subagent pocketgo does not have. Merged as #26 after 12 Codex rounds; the twelfth round's P2 points are [frostyard/pocketgo#27](https://github.com/frostyard/pocketgo/issues/27).
     - Seen on 2026-11-20 on the team VM (pocketd 2e92843, protocol 8). Odrade delegated frostyard/nsl#121 to Duncan as T8, and Duncan opened frostyard/nsl#124 and reported it with `team_message`. No agent asked for a review: pocketd linked the pull request and started T9, "Cross-review 1 of 4: frostyard/nsl#124 at d1626ba0f699", with Duncan's agent on `openai-codex/gpt-6.1-sol`, route `cross-review`, in a worktree detached at the head.
     - T9 filed `ready` at d1626ba. pocketd recorded it, closed T9 with `Verdict R1: frostyard/nsl#124 at d1626ba0f699 is ready.`, and T8's session got the verdict once. About a minute after CI at that head went green, pocketd's account put `ready-for-human` on the pull request, and the owner merged it from the desktop (6b).
7. [x] **Plans and consults:** plans, plan tasks and consults, with the plan gate ([ADR-0020](../adr/0020-plan-before-building-with-the-plan-gate-and-consults-in-code.md), [architecture: plans and consults](../design/architecture.md#service), [specs: protocol rules 48 to 54](../specs/protocol.md)).
   - Built: a plan thread for each plan, led by a specialist in its home and off the desk; `team_plan` (read, write from the revision read, tasks, ready, consult) and `team_finding`; the plan gate in code, checked where each act commits; the owner's approval of an exact revision and task graph from the desktop's Inbox; task delivery through `team_delegate` with `plan`; tasks done at their thread's close; read-only consults of 60 minutes; one keyed note per message pocketd owes; protocol version 10; plans in the Inbox and on the work board. `TestAPlanEndToEndOverHTTP` drives a plan from the owner's request through a consult, an ask, the approval and a delivered task to the plan's end, over the protocol, with a scripted model.
   - Not ported from pi-pocket-team: the whole-plan verification, scope changes, section comments, reopening an ask, `plan amend`, `plan nochange`, the `needs-plan` label, and queued consults.
   - Seen on 2026-11-20 on the team VM (pocketd 8138940, protocol 10), with the owner's approval from the desktop. An Odrade session started plan P1, "Plan live check", with Duncan as its lead in thread T11. Duncan wrote the plan, asked Teg in consult K1 (`openai-codex/gpt-6-astra`), whose finding (`objects`) came back to him and which he answered in the plan, drafted one task, t1 for Sheeana in her home, and readied the plan with its gate empty.
   - The owner approved P1 at rev 373deee with tasks version 2 from the desktop's Inbox (`approvedBy` the device `laptop`). Odrade got `[P1 · approved]` once and delivered t1 with `team_delegate`, which opened T12. Sheeana made the file and reported done, and Odrade closed T12: in that close t1 became `done`, P1 `done`, and T11 closed with `Closed: every task of plan P1 is done.`. Odrade got `[P1 · done]` once.
   - Two gaps the run showed, both fixed since: the consult got only the lead's question, not the plan, so Teg objected that the change was unspecified, and its question now quotes the plan's text and `rev` as the consult starts (`team.ConsultQuestion`); and a task with no repository was still told to open a pull request, and is now told it opens none (`team.PlanTaskText`).
8. [x] **Workers:** a thread's specialist gives the work to a worker on a cheaper model, the DeepSeek API by default, and checks its reply ([ADR-0021](../adr/0021-give-a-threads-specialist-a-worker-on-an-api-key-model-in-code.md), [architecture: workers](../design/architecture.md#service), [specs: protocol rules 14d, 22, 25 and 47](../specs/protocol.md)). Built ahead of slice 7, by the owner's order of 2026-11-19: `team_worker`, one worker a thread, the router's worker model (the profile's `worker`, else `deepseek/deepseek-flash`), API-key sign-ins that serve workers alone, the worker's reply once through the outbox, protocol version 9.
   - Seen on 2026-11-20 on the team VM over the protocol (pocketd 8138940, protocol 10). The owner stored the DeepSeek key with `pocketd login deepseek` while pocketd was stopped, and `pocketd models` then listed `deepseek/deepseek-flash` and `deepseek/deepseek-v4-pro` as "(workers only)".
   - Odrade delegated T10, "Worker live check", to Duncan in his home. Duncan's `team_worker` started session 736, `T10 · worker`, on `deepseek/deepseek-flash`, route `worker-default`, in the thread's folder, with Duncan's brief as its first message.
   - The worker made the file and replied. Duncan's session got `[T10 · worker] Created …` once, read the file itself, and reported the thread done with the worker's reply quoted. The worker's run cost $0.0005; Duncan's, on his own model, $0.037.
   - The owner then asked for less: DeepSeek may power any session, and the workers' models are a list of their preference, which item 9 builds.
9. [x] **The owner's worker list, and any session on any sign-in** ([ADR-0022](../adr/0022-run-any-session-on-any-sign-in-and-choose-workers-from-the-owners-list.md), [architecture: workers](../design/architecture.md#service), [specs: protocol rules 14d, 22, 26 and 47](../specs/protocol.md)). Asked for by the owner on 2026-11-20.
   - Built: an API key's models, DeepSeek's, run any session and show in the desktop's New Session. The worker-only guards of ADR-0021 are gone.
   - Built: a worker runs the model the supervisor asked for, else its profile's `worker`, else the first model pocketd runs of the owner's ordered list, `workers.json` in the profile folder.
   - Built: the supervisor passes the owner's choice, as "use deepseek", as `team_delegate`'s `worker`, which names an entry of that list and is kept on the thread (work database migration 11). `pocketd profiles` reports the list and the model a worker runs now. Protocol version 11, for the new worker routes: the desktop must be rebuilt.
   - Seen on 2026-11-20 on the team VM (pocketd ea7b7fe, protocol 11). `pocketd models` listed DeepSeek's models for any session. An Odrade session on Claude Sonnet 5.5 was told "Use deepseek for the workers of this work"; it passed `worker` with `team_delegate`, and thread T13 kept `deepseek/deepseek-flash` as its asked worker model. Duncan's worker, session 954, ran on it with the route `worker-asked`, and its reply reached Duncan once; Odrade closed T13 on his report.
   - Then seen on 2026-11-20 (pocketd 1bfb973, protocol 12): session 1295, "DeepSeek session check", started from the desktop's New Session with no profile on `deepseek/deepseek-flash` (route `owner`), answered "deepseek session ok".
10. [ ] **The owner's own model providers** ([ADR-0023](../adr/0023-add-the-owners-own-model-providers-from-a-file-checked-in-code.md), [architecture: workers](../design/architecture.md#service), [specs: protocol rules 14d and 47](../specs/protocol.md)). Built: `providers.json` in the profile folder, checked in code, each provider on PiG's OpenAI-completions or Anthropic-Messages adapter with its base URL and models, an optional key through `pocketd login`, and its models for any session and the worker list; and newer models added to PiG's own providers (`builtinModels`), since PiG's catalog is fixed in its release, which the owner found stale on 2026-11-20. No protocol change.
   - Seen on 2026-11-20 on the team VM (pocketd ea7b7fe). At the owner's request, `providers.json` in the VM's profile folder names `halogen`, their halogen-flash-server on the home network, on its OpenAI chat-completions API with no key. After a restart, `pocketd models` listed `halogen/halogen-qwen3.8-flash-next`. A session on it (905) answered, and it ran a `bash` tool call in the sandbox and gave back its output. The file went to the owner's private configuration repository as a pull request.
   - Still to see: a model added to a built-in provider under `builtinModels` answering, which waits for the owner to name the models they miss.
11. [ ] **The supervisor's standing session** ([ADR-0026](../adr/0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md), [architecture: standing sessions](../design/architecture.md#service), [specs: protocol rules 7a, 7c, 7d and 16c](../specs/protocol.md)). Asked for by the owner on 2026-11-20.
   - Built: `session: standing` and `compactAt` on a profile; New Session opens the profile's one session; pocketd compacts it at its `compactAt` at a run's end, and it follows its profile's identity, roster, tools and model when idle; Start Over resets it in the same session, behind a confirmation; every model's summary has room (`summaryReserve`). Work database migration 13 and protocol version 14: the desktop must be rebuilt.
   - Still to see: on the team VM, Odrade's profile with `session: standing` and `compactAt: 300000`, a compaction row in her session's transcript, and a Start Over after which her threads still report to her session. The Clerk and its digests come later.
12. [ ] **New Session on a repository, in a new worktree** ([ADR-0027](../adr/0027-start-the-owners-session-on-a-repository-in-a-new-worktree-named-in-code.md), [architecture: sessions on a repository](../design/architecture.md#service), [specs: protocol rules 5, 6a, 7c and 21](../specs/protocol.md)). Asked for by the owner on 2026-11-20.
   - Built: a profile's repositories with their folders; New Session's Repository choice under Profile, which fills in the checkout, or with No profile every profile's repositories; "In a new worktree", which shows the folder and branch pocketd makes from the request, `<checkout>--<agent>-<slug>` on `<agent>/<slug>` from `HEAD`, in the sandbox; the path bound to its request (work database migration 14). Protocol version 16.
   - Still to see: through the desktop on the team VM, a Duncan session on frostyard/chairlift in a new worktree, its folder as the form showed it, and git there on the branch it named.
   - Open: at the desktop's smallest window height, the dialog with a Repository row does not show its buttons; the dialog does not scroll.
   - Open: how far a session's worktree defends against its folders changing under it as it is made. A folder swapped for another between the `mkdir` that makes it and the git that fills it cannot be told by path-based git, and threads' worktrees do not defend against it either.

- **Policies enforced in code:** see the [architecture](../design/architecture.md#policies-enforced-in-code).
- [x] **Tools run without approval** in a bubblewrap sandbox that hides pocketd's data folder and keeps its start files read-only ([ADR-0012](../adr/0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md), [architecture: operational notes](../design/architecture.md#operational-notes)). Done 2026-11-19.
- **Done when:** the owner asks the supervisor for a change, a specialist opens a pull request in a thread, and the pull request, its asks and the specialist's proposed lessons appear in the inbox. Slices 1 to 5 reach it.
  - [ ] Seen on 2026-11-19, but for the lessons (pocketd a67ab9b, the desktop of slice 5). The owner asked the supervisor in a session for a change to the team's configuration. The supervisor delegated T6 to Sheeana in a worktree of bketelsen/pi-pocket-config, and Sheeana opened bketelsen/pi-pocket-config#80 and reported it with `team_message`, with ask A3. The pull request and A3 waited in the desktop's Inbox, and T6 on the board. Sheeana proposed no lesson in T6, so a specialist's proposed lesson in this run is still to see; slice 3 showed one in the Inbox on its own.
  - Before T6, a pull request opened in a session the owner started from a profile, outside any thread, was not linked: only a thread's report links one (ADR-0016).

## Phase 3 — Rounds (done)

The owner's shifts from pi-pocket-team, started by the supervisor first, with no spend limit yet (the owner, 2026-11-20). Built in this order, one pull request each:

1. [x] **Shifts in the service:** `team_delegate`'s `shift`, the rounds driven in code, the stops (done with an optional CI check, blocked, the same failing checks three times, the round limit, the deadline), the summary round's one report, the owner's pause, stop and resume, protocol version 12 ([ADR-0024](../adr/0024-run-a-delegated-thread-in-rounds-as-a-shift-driven-in-code.md), [architecture: shifts](../design/architecture.md#service), [specs: protocol rule 55](../specs/protocol.md)). Merged as #36 and deployed on 2026-11-20 (pocketd 1bfb973).
   - Still to see live: a shift paused by the owner's message and resumed, a done that waits on CI, and the same failing checks three times.
2. [x] **Shifts in the desktop:** each shift on the work board, its round and why it ended, with Stop and Resume ([ADR-0024](../adr/0024-run-a-delegated-thread-in-rounds-as-a-shift-driven-in-code.md), [architecture: desktop client](../design/architecture.md#desktop-client)). Merged as #37 on 2026-11-20.

- **Round-based jobs:** an objective, a completion condition, a round limit, a deadline, a spend limit and a stop. The spend limit waits: the owner chose rounds and the deadline first.
- **Pausing:** the job pauses when the owner writes in the session.
- **Done when:** four test jobs each stop for a different reason (completion condition, round limit, deadline, the owner's Stop), and each thread shows why it ended.
  - [x] Seen on 2026-11-20 on the team VM (pocketd 1bfb973, protocol 12), through the desktop of #37. An Odrade session on Claude Sonnet 5.5 delegated each shift to Duncan in his home with `team_delegate`'s `shift`, and each thread's card on the work board said why its shift ended:
    - T14, a file to write: "Shift ended at round 1 of 3: the stop condition holds." Its round said `DONE`, and Duncan's summary report was `done`.
    - T15, one line a round toward 100 lines: "Shift ended at round 2 of 2: it reached its 2 rounds."
    - T18, the same with a 3-minute deadline: "Shift ended at round 8 of 100: it reached its deadline." It ran eight `CONTINUE` rounds first.
    - T16, the same with 30 rounds: the owner's Stop Shift on its card, after the confirmation, ended it in round 2: "Shift ended at round 2 of 30: the owner stopped it." Duncan's turn under way went on to its end, and pocketd filed the shift's report.
    - Odrade got each thread's last report once.
  - A first deadline test, T17, ended `blocked` in round 8 instead. Its round ended past the deadline, and Duncan reported `BLOCKED`, since one line a round could not reach 100 lines in its 50 rounds. pocketd takes a round's verdict before its own deadline check. T18 repeated the test with a reachable goal and asked Duncan never to report `BLOCKED`.
  - Duncan asked the owner A6 to A9 in T15 to T17: whether to lower the unreachable goals.

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
