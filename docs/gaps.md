# Migration gaps

A survey, on 2026-11-24, of what the setup pocketgo replaces did that pocketgo does not, and of pocketgo's own plans that are not finished. It is a list for the owner to sort: strike what should go, keep what should stay, then set an order. It changes no decision. An item that stays becomes a roadmap item, an issue or an ADR, and leaves this list.

**Compared:** pocketgo at `b4e8967`, against [bketelsen/pi-pocket-team](https://github.com/bketelsen/pi-pocket-team) at `96c8417`, [TannerMidd/pi-pocket](https://github.com/TannerMidd/pi-pocket) at `5d009fa4`, frostyard/pocket at `9af56b3`, and the owner's private configuration repository at `34f5419`. The [roadmap](plans/roadmap.md)'s "What carries over" table says what each was to give; its "Out of scope" list and the ADRs say what was left out on purpose. Those are not gaps, and the [last section](#left-out-on-purpose-not-gaps) lists them for reference.

**How each item reads:**
- **Was:** what the old setup did, and where.
- **Now:** what pocketgo has, or what shows it is absent.
- **Docs:** what pocketgo's docs say of it.

**Status**, after each title:
- **Missing:** pocketgo has none of it, and no pocketgo doc mentions it.
- **Partial:** pocketgo has part of it.
- **Deferred:** a pocketgo doc says it comes later, with no phase.
- **Planned:** a roadmap phase or an open issue holds it.
- **Unverified:** built, but its "Still to see" was never seen.
- **Confirm:** probably left out on purpose; the owner confirms or keeps it.

The numbers stay with their items when others are struck, so refer to an item by its number.

## Promised by the roadmap, not built

The roadmap's "What carries over" names each of these.

- [ ] **G1. Wait out provider rate limits.** Missing.
  - **Was:** pi-pocket's `src/server/limit-gate.ts` and `src/server/limits.ts`. A run that hits a provider's usage limit waits until the reset, up to 6 hours by default, sends one test request, and the wait survives a restart.
  - **Now:** nothing in `service/` handles a model call's 429, rate limit or `retry-after` (only `service/internal/signins` does, for GitHub's device flow). Runs take PiG v0.7.0's `DefaultRetryPolicy`: 3 retries, 2 s apart at first, 60 s at most. A subscription's usage window, hours long, likely ends the run with an error.
  - **Docs:** the roadmap lists "Waiting out provider rate limits" under pi-pocket's behaviours. No slice or ADR builds it.

- [ ] **G2. Keep the prompt cache warm.** Missing.
  - **Was:** pi-pocket's `src/server/cache-keeper.ts`.
  - **Now:** nothing in `service/` keeps a cache warm or sets a cache retention.
  - **Docs:** the roadmap lists "Keeping the prompt cache warm". No slice builds it.
  - The standing supervisor session ([ADR-0026](adr/0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md)) re-reads the longest context after an idle gap, so it gains most.

- [ ] **G3. Fence and defang outside text.** Missing.
  - **Was:** pi-pocket's `src/server/fence.ts`: `defang`, `attribute`, `quoted`, and its class of invisible characters.
  - **Now:** text from outside reaches agents unfenced:
    - a specialist's report (`team.ReportText`, [team.go](../service/internal/team/team.go))
    - a worker's reply (`team.WorkerReplyText`, [workers.go](../service/internal/team/workers.go))
    - a supervisor's message (`team.SendText`, [reopen.go](../service/internal/team/reopen.go))
    - `team_github`'s answers, which hold issue and pull request text
    - an MCP tool's result, which pocketd redacts for secrets but does not fence
    - `team_inbox` quotes some fields with `%q`, which is no fence.
  - **Docs:** the roadmap lists "Fencing and defanging inbox text". The [architecture](design/architecture.md)'s "External events" says, as intent, "Outside text is fenced and defanged before an agent sees it", next to Phase 5's webhooks. GitHub and MCP text arrive today, before Phase 5.

- [ ] **G4. Watches.** Missing.
  - **Was:** pi-pocket-team's `team_delegate` `watch` (`check_on`, `check`, `on`, `condition`), the desk's `watch` and `result`, and the desk page's Watching section (Watch, Clear, Pass, Fail). A watched thread slept outside the desk cap until a date, an event, or a condition checked by machine: a pull request merged, checks green, a run succeeded. pi-pocket had `watch_pr` too.
  - **Now:** no watch table, tool parameter or protocol rule. `team_delegate` takes `agent`, `task`, `title`, `repository`, `plan`, `shift`, `worker` and `follows` ([team.go](../service/pig/team.go)). `team_schedule`'s reminders cover a dated check, with no condition.
  - **Docs:** the roadmap's "From pi-pocket-team, port the domain model" names "The thread, ask, PR link, watch and review-verdict schemas". The architecture's overview says the owner can "schedule a follow-up" from a thread, and Phase 5 names "scheduled follow-ups"; either may mean this. The night shift's design put the supervisor's "merge watches" in a later slice.

- [ ] **G5. Wait for CI outside a shift.** Missing.
  - **Was:** pi-pocket-team's `team_wait_for_ci` (`team.ts`): any session waited for a pull request's checks. pi-pocket's `watch_pr` did the same.
  - **Now:** pocketd reads CI in code only for a shift with `ci: true` ([ADR-0024](adr/0024-run-a-delegated-thread-in-rounds-as-a-shift-driven-in-code.md), [ADR-0047](adr/0047-carry-a-shifts-ci-on-a-proposal-to-its-launch.md)). A thread that is no shift reports its pull request and cannot wait for its checks.
  - **Docs:** ADR-0047's context says "the specialists had no tool to wait for CI" and fixes it for shifts and launches alone.

## pi-pocket-team: the team

- [ ] **G6. An agent starts its own linked session in a fresh worktree.** Missing.
  - **Was:** `team_work` (`team.ts`): an agent in its home opened a session of its own on a new worktree of one of its repositories, linked to its own.
  - **Now:** no such tool; `roleTools` ([team.go](../service/pig/team.go)) offers none. The owner's New Session on a repository ([ADR-0027](adr/0027-start-the-owners-session-on-a-repository-in-a-new-worktree-named-in-code.md)) is the nearest, and only the owner starts it.
  - **Docs:** none.

- [ ] **G7. The home guard.** Missing.
  - **Was:** a hook (`homeGuard`, `team.ts`) that refused `edit` and `write` into any repository of the roster, or a sibling worktree, from a session in a home.
  - **Now:** `guardedEnv` ([env.go](../service/pig/env.go)) and the sandbox protect pocketd's files, not one agent's repositories from another's session.
  - **Docs:** none. Its reason holds: [ADR-0043](adr/0043-load-the-global-and-folder-agents-md-and-the-skills-into-every-session.md) loads the `AGENTS.md` of the session's own folder, so a session in a home never reads a repository's rules.

- [ ] **G8. The home section of an agent's instructions.** Missing.
  - **Was:** `placesText` (`team.ts`): "Your home", each repository's folder and its `AGENTS.md` path, and four rules of the home.
  - **Now:** a specialist gets its identity, with no list of its places (`sessionInstructions`, [engine.go](../service/pig/engine.go)).
  - **Docs:** none.

- [ ] **G9. A clock line on every message.** Missing.
  - **Was:** `withMessageTime` (`_tool-time.ts`): a "Current time" line with the local date and time on each brief and team message.
  - **Now:** no brief (`team.Brief`), report, answer or scheduled message carries one, and PiG adds none that the survey found. Shift deadlines show in UTC alone (`team/shift.go`).
  - **Docs:** none. pi-pocket-team's beacon ran a trial of the clock line, due for review on 2026-11-25.

- [ ] **G10. The supervisor lists and reads its threads.** Missing.
  - **Was:** `team_sessions` (`team.ts`), and the desk's `list` and `read` (`desk.ts`): open threads with who moves next and stale marks, a thread in full or in brief, one report.
  - **Now:** the supervisor's tools are `team_delegate`, `team_close`, `team_schedule`, `team_send`, `team_gather`, `team_board`, `team_launch` and `team_inbox`, beside `team_memory`. `team_inbox` ([ADR-0046](adr/0046-let-the-supervisor-read-the-owners-inbox-from-pocketds-records-in-code.md)) lists what waits for the owner, not the supervisor's threads.
  - **Docs:** none. It may be part of why the supervisor reminds the owner of settled work (the reason for ADR-0046).

- [ ] **G11. The supervisor is told when the owner answers an ask.** Partial.
  - **Was:** the desk told the supervisor of the answer with the thread's next step.
  - **Now:** the answer reaches the asking session alone. The supervisor learns of it only by calling `team_inbox`.
  - **Docs:** roadmap Phase 2, slice 2: "Seen then, for a later slice: an answer goes only to the session that asked, so the supervisor still thought T1 waited for A1."

- [ ] **G12. The owner adds a lesson that holds at once.** Missing.
  - **Was:** the owner wrote a lesson for an agent from the Team panel, in force with no approval step (`team.ts`).
  - **Now:** the protocol has `GET /v1/lessons` and `POST /v1/lessons/{id}`, which decides an agent's proposal. Nothing adds one.
  - **Docs:** [ADR-0015](adr/0015-keep-each-agents-lessons-approved-by-the-owner-in-code.md) rejects the owner editing a lesson's text, not adding one.

- [ ] **G13. Change a session's agent after it starts.** Missing.
  - **Was:** `/team-agent <id|clear>`, "Use X in this session", and an identity taken from the home folder a session opened in (`team.ts`).
  - **Now:** a session takes its profile when it is made (`CreateSession.profile`, [ADR-0013](adr/0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md)), and keeps it.
  - **Docs:** none.

- [ ] **G14. The supervisor's roster in full.** Partial.
  - **Was:** each agent's `## Responsibilities`, home, folders and model.
  - **Now:** `team.Roster` ([team.go](../service/internal/team/team.go)) gives each agent's id, name, and repositories or "works in its home".
  - **Docs:** none.

- [ ] **G15. What `team_message` reports.** Partial.
  - **Was:** a report could go `to` another session than the supervisor's; `prs` took up to 100 pull requests "created, changed, reviewed or labelled"; a `review` field filed an `owner-review`.
  - **Now:** a report goes to the thread's supervisor alone; `pull_requests` takes up to 10 opened for the thread, each checked by `team.CheckPull`, so a pull request the agent only reviewed or labelled links nowhere; only a cross-review files a verdict ([ADR-0016](adr/0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md), [ADR-0019](adr/0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md)).
  - **Docs:** ADR-0016 and ADR-0019 decide the new rules; neither names what was dropped.

- [ ] **G16. The owner starts a thread from the desktop.** Partial.
  - **Was:** the Team panel's "Start linked task": a task on an agent's repository that reports to the supervisor.
  - **Now:** the owner opens a profile's session on a repository worktree (ADR-0027), which is no thread and reports to nobody. Only a supervisor's `team_delegate` opens a thread.
  - **Docs:** none.

- [ ] **G17. Seed an agent's home.** Missing.
  - **Was:** `homes/seed.mjs` made each home a git repository with a charter `AGENTS.md` and `notes/`, `plans/` and `reports/`.
  - **Now:** pocketd's commands are `serve`, `device`, `login`, `models`, `profiles`, `mcp` and `version`; none makes a home. The [setup guide](design/setup.md) says the homes must exist.
  - **Docs:** none. It matters for a new agent or a new host, not for the homes that exist.

- [ ] **G18. The Clerk and its digests.** Deferred.
  - **Was:** a Clerk agent on a cheap model took routine desk news off the supervisor, in digests, with `desk escalate` for what the supervisor must see (pi-pocket-team's plans `clerk.md` and `desk-digest.md`, both done and running).
  - **Now:** `role: clerk` is accepted ([profiles.go](../service/internal/profiles/profiles.go)) and shown as a badge, and does nothing else.
  - **Docs:** roadmap slice 11: "The Clerk and its digests come later." ADR-0026 says the same. No phase.

## pi-pocket-team: the desk

- [ ] **G19. A thread's history.** Missing.
  - **Was:** the desk kept a thread's last 10 events and every report, and marked what was new since the owner's last look.
  - **Now:** `proto.Thread` holds the last `Report` alone, and there is no `GET /v1/threads/{id}` or events table.
  - **Docs:** none.

- [ ] **G20. Stale threads, and Keep, Park and Wake.** Missing.
  - **Was:** the desk marked a thread stale after a quiet spell; the owner or supervisor kept it, parked it outside the cap, or woke it (`desk.ts`).
  - **Now:** a thread is open, done, blocked or closed; the board groups it by `Data.NextMover`, with no stale mark.
  - **Docs:** none.

- [ ] **G21. Allow one over the cap.** Missing.
  - **Was:** the page's "Allow 1", or "desk ok" in chat, let one delegation past the cap.
  - **Now:** `team_delegate` over the cap is refused with "close a finished one with team_close" ([team.go](../service/internal/team/team.go)); only `-desk-cap` at start changes the cap.
  - **Docs:** none.

- [ ] **G22. Found threads.** Missing.
  - **Was:** the desk's `own`, and its sweep: a linked session with no thread, or a folder with changes not committed or not pushed, became a thread F1, F2, …
  - **Now:** nothing looks for work outside threads.
  - **Docs:** none.

- [ ] **G23. Attach and detach a pull request by hand.** Missing.
  - **Was:** `desk attach` and `detach`, with a list of what was dropped, and pull requests found from what a session named, its branch, or a time window.
  - **Now:** a thread links only the pull requests a report names ([ADR-0016](adr/0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md)).
  - **Docs:** none.

- [ ] **G24. Other authors' open pull requests.** Missing.
  - **Was:** the page's "Other PRs" section, for Dependabot's and others' pull requests in the team's repositories.
  - **Now:** a pull request no thread reported is not in pocketd's records (ADR-0046 says so), so Merge All Ready sees only pull requests pocketd backs.
  - **Docs:** ADR-0046 states the fact, not a decision to leave it out.

- [ ] **G25. Wake the agent to repair its pull request.** Missing.
  - **Was:** a thread's agent was told when its pull request hit a conflict or failing checks.
  - **Now:** only a shift's round sees failing checks (`team/shift.go`).
  - **Docs:** none.

- [ ] **G26. Wake the agent to close out after a merge.** Missing.
  - **Was:** after a merge, the agent was told to verify the release or deploy and clean up.
  - **Now:** pocketd closes the thread and removes its worktree ([ADR-0054](adr/0054-remove-a-closed-threads-worktree-in-code-with-no-github-write.md)), and tells no agent.
  - **Docs:** none.

- [ ] **G27. Archive a closed thread's sessions.** Missing.
  - **Was:** an automatic pass, and `desk archive`.
  - **Now:** sessions have no archived state (see also G67).
  - **Docs:** none.

- [ ] **G28. The desk's git export.** Missing.
  - **Was:** `threads.md`, `others.md`, `board.md` and a `T*.md` for each thread, committed by the `exportCommit` setting.
  - **Now:** none.
  - **Docs:** none.

- [ ] **G29. The issue queue.** Missing.
  - **Was:** pi-pocket-team's plan `issue-queue.md`, implemented in `desk.ts`: the owner queued GitHub issues with the `agent-queue` and `agent-urgent` labels or by assignment; `desk issues`, the page's section, and an issue still open after its pull request merged.
  - **Now:** none. A gather with an issue-triage skill and `team_github` ([ADR-0036](adr/0036-let-proposal-sessions-consults-and-plan-leads-read-github-with-get-calls-built-in-code.md)) covers part.
  - **Docs:** the roadmap lists only the `needs-plan` label as not ported.

- [ ] **G30. Tell the supervisor what waits on it.** Missing.
  - **Was:** "the supervisor's queue" and the failed-run prompt told the supervisor of threads that waited on it: a silent session, a failed run, a blocked report.
  - **Now:** pocketd sends the supervisor each report once, and nothing for a silent session or a failed run, outside shifts and workers.
  - **Docs:** none.

- [ ] **G31. The rules of asks.** Partial.
  - **Was:** a reminder when a report's prose asked a question but filed no ask; a refusal of an ask for a plain merge; `rewrite`; and the supervisor's `withdraw`, with a reason or into another ask.
  - **Now:** a repeated question files as the thread's open ask ([ADR-0055](adr/0055-file-a-repeated-ask-as-the-threads-open-one.md)); an ask is withdrawn only when the supervisor's `team_send` unblocks its thread ([ADR-0037](adr/0037-unblock-a-thread-when-the-supervisors-message-reaches-it.md)).
  - **Docs:** none for the rest.

- [ ] **G32. How a thread ended.** Missing.
  - **Was:** a closed thread's outcome: completed, abandoned, verified or frozen.
  - **Now:** a closed thread has its last report alone.
  - **Docs:** none. Run records (G61) would want it.

- [ ] **G33. How the desk drew things.** Missing.
  - **Was:**
    - pull requests ready to merge sorted by their `level:L1` to `level:L4` labels
    - an ask's text as safe Markdown, with `artifact:` and desk path links
    - a warning when a GitHub read failed or a pull request went unread, and a `MISSING` state
  - **Now:** asks draw as plain text ([inbox.go](../desktop/internal/app/inbox.go), [asks.go](../desktop/internal/nsgoclient/asks.go)); no level sort; `team_inbox` and the cards say when pocketd last read a pull request.
  - **Docs:** none.

- [ ] **G34. Sync on demand, and the log of what changed.** Partial.
  - **Was:** `desk sync`, and `x.desk.observed`, the log of what the desk saw change, which fed the daily audit.
  - **Now:** pocketd reads open pull requests every 5 minutes, and `GET /v1/pulls/{id}` reads one at once for a merge. No log of changes.
  - **Docs:** none.

- [ ] **G35. When a thread closes and reopens.** Partial.
  - **Was:** a done thread closed itself; a busy session or a new pull request reopened a closed one.
  - **Now:** a thread closes when its pull requests merge or by `team_close` or the owner, so a done thread with no pull request stays open until someone closes it; it reopens only when a message reaches its specialist ([ADR-0061](adr/0061-reopen-a-closed-thread-when-a-message-reaches-its-specialist.md)).
  - **Docs:** ADR-0061 decides reopening; nothing decides closing a done thread with no pull request.

- [ ] **G36. The usage meter.** Partial.
  - **Was:** the desk meter (`_desk-meter.ts`, `_desk-usage.ts`): each provider's window percentages, resets and a burn projection; a ledger at list prices, so a subscription's use had a cost; cost by agent; a digest footer; a launch held while the meter was high; the `desk usage` tool.
  - **Now:** `GET /v1/usage` by day and model, and each session's and thread's usage ([ADR-0052](adr/0052-show-each-sessions-context-fill-and-usage-and-sum-the-days-usage-in-code.md)). A subscription's cost is 0.
  - **Docs:** none for the rest.

- [ ] **G37. The goal board's settings.** Partial.
  - **Was:** the goal board's limits were settings; an agent could propose at any time; a row could have a start time; the meter could hold a launch.
  - **Now:** gathers, proposals and launches ([ADR-0033](adr/0033-gather-proposals-from-the-team-in-read-only-sessions-in-code.md), [ADR-0034](adr/0034-launch-the-owners-picks-from-the-supervisors-standing-session-in-code.md)) with constants (`team.LaunchAtOnce` 3, `team.LaunchReserve` 2, `team.LaunchRounds` 200); agents propose only inside a gather; no start time; no hold.
  - **Docs:** the ADRs decide the constants; none names the rest.

- [ ] **G38. The owner's plan actions.** Missing.
  - **Was:** New Plan (frostyard/pocket's `new_plan.go`), Request Changes, and Stop and Revise.
  - **Now:** a client approves a plan and nothing else ([ADR-0020](adr/0020-plan-before-building-with-the-plan-gate-and-consults-in-code.md)); a plan starts only from a supervisor's `team_delegate` with `plan`.
  - **Docs:** roadmap slice 7 lists other plan features as not ported, not these.

- [ ] **G39. An ask with no thread.** Partial.
  - **Was:** `ask new`: any agent asked the owner, with no thread; pi-pocket's `ask` tool did the same in any session.
  - **Now:** the `question` tool, for a supervisor's and a private profile's sessions alone, and only in a run the owner's own message started ([ADR-0048](adr/0048-put-the-supervisors-and-a-private-agents-questions-to-the-owner-inline-in-code.md)).
  - **Docs:** ADR-0048 decides who asks; it does not name `ask new`.

- [ ] **G40. The owner's review and the plan's review.** Partial.
  - **Was:** verdict kinds `owner-review` and `plan-review` beside the cross-review.
  - **Now:** a cross-review's verdict alone ([ADR-0017](adr/0017-record-cross-review-verdicts-and-set-ready-for-human-in-code.md)).
  - **Docs:** roadmap slice 7 leaves out "the whole-plan verification", which was `plan-review`; nothing names `owner-review`.

## pi-pocket-team: shifts

pocketgo's shifts ([ADR-0024](adr/0024-run-a-delegated-thread-in-rounds-as-a-shift-driven-in-code.md)) take `stop`, `max_rounds`, `hours` and `ci` (`team.Shift`, [shift.go](../service/internal/team/shift.go)). pi-pocket-team's (`night-shift.ts`, its module doc `shift.md`) took more.

- [ ] **G41. Start a shift at a clock time.** Missing.
  - **Was:** `start 06:00`: round 1 waits for the time.
  - **Now:** a shift's round 1 starts at once. A schedule that messages the supervisor at the time is the workaround.
  - **Docs:** none.

- [ ] **G42. Wait out DeepSeek's peak hours.** Missing.
  - **Was:** a `deepseek/…` worker held round 1 during 01:00 to 04:00 and 06:00 to 10:00 UTC.
  - **Now:** none.
  - **Docs:** none.

- [ ] **G43. A deadline at a clock time.** Partial.
  - **Was:** `until 07:00`.
  - **Now:** `hours`, 24 at most. A launch takes a clock time ([ADR-0064](adr/0064-end-a-launch-at-the-next-06-45-by-default.md)); a delegated shift does not.
  - **Docs:** none.

- [ ] **G44. The CI check's options.** Partial.
  - **Was:** `pr_check` with `checks: required` and its fallback, `checks: none` with a `label` that done waits for, and a `pr` and `repo` named.
  - **Now:** `ci: true` reads every check of the thread's linked pull requests but `IgnoredChecks` ([ci.go](../service/internal/github/ci.go)).
  - **Docs:** none.

- [ ] **G45. Keep the failing jobs.** Partial.
  - **Was:** `stopChecks`: the repository, run and job ids of up to 20 failing jobs, kept with the stop.
  - **Now:** the check names alone, in the stop's reason; `proto.Shift` has no list.
  - **Docs:** none.

- [ ] **G46. Name the worker in a shift's end report.** Partial.
  - **Was:** "Rounds: N, worker: X".
  - **Now:** `team.ShiftEndedReport` names the rounds, not the worker.
  - **Docs:** none.

- [ ] **G47. A worker's thinking level.** Missing.
  - **Was:** `workerThinking`.
  - **Now:** `workers.json` names models alone ([workers.go](../service/internal/profiles/workers.go)).
  - **Docs:** none.

- [ ] **G48. The owner starts a shift in any session.** Deferred.
  - **Was:** `/team-shift`, in any session, the owner's own included.
  - **Now:** only a supervisor's `team_delegate` with `shift`, or a launch.
  - **Docs:** ADR-0024: "a desktop start comes later". No phase.

- [ ] **G49. A free `check` command.** Deferred.
  - **Was:** a command of up to 2,000 characters, run after each round for 10 minutes at most, its last 3,000 characters of output kept.
  - **Now:** none.
  - **Docs:** ADR-0024: it "comes with the desktop start" (G48). No phase.

- [ ] **G50. A spend limit.** Deferred.
  - **Was:** a shift stopped once its spend passed its limit (`heldBack`). pi-pocket limited a session's or a person's spend (`src/server/spend.ts`).
  - **Now:** none.
  - **Docs:** roadmap Phase 3: "The spend limit waits: the owner chose rounds and the deadline first." ADR-0024 says the same. No phase.

## pi-pocket-team: the model router and its bench

- [ ] **G51. The router's modes.** Missing.
  - **Was:** `model-router.ts`: modes `off`, `shadow` (record the pick, change nothing) and `assign` (assign a shift's worker on the tiers the owner made live, `liveTiers`); rule v1's bands, easy, medium and hard (`router-rule-v1.json`); an A/B arm from a SHA-256 of the thread id; eligibility (a bench repository, no risk path); the route's line on the thread.
  - **Now:** `router.Choose` routes by fixed rules: the owner's model, the profile's, an alias, the cross-review's and the worker's ([router](../service/internal/router)).
  - **Docs:** the roadmap's open question: how the router decides, "decided from the Phase 5 run records".

- [ ] **G52. Record the brief's features.** Planned.
  - **Was:** each delegation's brief classified and recorded, with features v1 (kind, complexity, brief length, paths named, repositories named, language, risk path named, tests beside) and a hash of the brief, masked before any outside call.
  - **Now:** a session records its route alone.
  - **Docs:** the architecture's "Model router" intends to record "the choice, the features of the brief and the outcome"; Phase 5's run records.

- [ ] **G53. Skip a model that cannot serve now.** Missing.
  - **Was:** the router skipped DeepSeek in its peak hours, and a model with a problem now.
  - **Now:** pocketd skips only a model with no sign-in.
  - **Docs:** none.

- [ ] **G54. The router bench.** Missing.
  - **Was:** `bench/` and the plan `router-bench.md`: merged tasks replayed in bwrap, scored with hidden tests and two calibrated judge models, behind a pool gate and a budget guard. Stage 1 froze rule v1; stage 2 waits for more samples, with monthly rounds from 2026-12-01.
  - **Now:** none.
  - **Docs:** the open question suggests run records take its place, but nothing says the bench is dropped.

## pi-pocket-team: audit, journal and experiments

- [ ] **G55. The daily audit.** Missing.
  - **Was:** `audit/` and the plan `daily-audit.md`, done and running: a read-only daily report of the team's work, with checks of acceptance, review rounds, the blocker-to-guard rate, stops after 3 same failures and flaky CI, a comparison with the day before (new, repeating, gone, maybe fixed), at most 5 proposals checked in code at $0.50 a day, findings due after 7 days with owners assigned in code, and a check of the homes.
  - **Now:** none. The old script reads `pocket.sqlite` and cannot see pocketd's databases.
  - **Docs:** [ADR-0008](adr/0008-keep-work-records-in-the-services-own-tables.md) says "metrics and audits are SQL queries"; no phase names an audit.

- [ ] **G56. The daily journal.** Missing.
  - **Was:** the plan `pocket-journal.md`, implemented: a daily note of the threads finished, written to the owner's notes through MCP.
  - **Now:** MCP servers connect ([ADR-0063](adr/0063-connect-a-profiles-mcp-servers-to-its-sessions-with-credentials-pocketd-keeps.md)), but nothing writes a journal. A schedule that asks an agent to write one would serve.
  - **Docs:** none.

- [ ] **G57. The beacon's experiments.** Missing.
  - **Was:** pi-pocket-team's `docs/beacon.md`: experiments registered before they ran (fresh linked task sessions, a brief prompt to read the desk, the clock trial of G9), and a loop to collect, analyse and act.
  - **Now:** none.
  - **Docs:** none, beyond Phase 5's run records.

## Phase 5: events and records

Phase 5 of the roadmap holds these. Each lists what pi-pocket-team's version did, so the port can be checked against it.

- [ ] **G58. GitHub webhooks.** Planned.
  - **Was:** `github-hooks.ts`: a listener for signed deliveries that kept no GitHub text and only woke the desk to sync early; an `allowFrom` list of peers; a secret rotated through a `.next` file; duplicates dropped by delivery id and body digest (72 hours, 20,000 at most); alarms when deliveries missed changes the sweep saw; a `/github-hooks status` card; a log of 2,000 deliveries and an audit report.
  - **Now:** none. pocketd reads pull requests every 5 minutes.
  - **Docs:** roadmap Phase 5. The architecture's design differs: a webhook "becomes a submission … delivered to the responsible agent", with fenced text (G3), not a nudge with no text.

- [ ] **G59. The events bridge.** Planned.
  - **Was:** `events-bridge.ts` and pi-pocket-team's ADRs 0004 and 0005:
    - each agent's own lodge OAuth client, scope `events`
    - four `events_*` tools
    - `now` events in batches: 60 s, 6 an hour, 40 events or 7,000 characters
    - retries on 409 and 410
    - an 08:00 digest, 1,000 events kept
    - events held until lodge and the agent's identity are confirmed
    - the agent's own events dropped, and waiting events deleted on a revoke
    - `repeat=maybe`, NATS pulls over WebSocket, and a prompt section that counts the digest
    - its own `<Agent> events` session
  - **Now:** none.
  - **Docs:** roadmap Phase 5 and [frostyard/pocketgo#82](https://github.com/frostyard/pocketgo/issues/82), which sends events to the profile's standing session instead of a session of their own. It waited on the MCP credential store, built since ([ADR-0063](adr/0063-connect-a-profiles-mcp-servers-to-its-sessions-with-credentials-pocketd-keeps.md)). The bridge's files on disk need reading or moving.

- [ ] **G60. Scheduled follow-ups.** Planned.
  - **Was:** watches (G4) and a thread's dated checks.
  - **Now:** `team_schedule` reminders, with no link to a thread.
  - **Docs:** roadmap Phase 5; the architecture's overview: "schedule a follow-up" from a thread.

- [ ] **G61. Run records and the metrics views.** Planned.
  - **Was:** the desk's dashboard and metrics (`_desk-dashboard.ts`, `_desk-metrics.ts`, `docs/desk-dashboard.md`), and frostyard/pocket's Numbers place (`numbers.go`, `agent_cost.go`).
  - **Now:** usage by day (ADR-0052). No run record: the model and thinking level, the profile or brief version, tokens and cost, review rounds and verdicts, the outcome.
  - **Docs:** roadmap Phase 5, whose "Done when" compares outcomes by model and by way of working; the architecture's "Run records" and its desktop place "Run records and metrics", both intent.

## pi-pocket: session features

These were pi-pocket's, drawn by frostyard/pocket's client, and are on no list of what was left out.

- [ ] **G62. Fork a session.** Missing.
  - **Was:** pi-pocket's `src/server/branches.ts`, frostyard/pocket's Fork.
  - **Now:** none in the [protocol](specs/protocol.md).
  - **Docs:** none.

- [ ] **G63. Export a session.** Missing.
  - **Was:** pi-pocket's `src/server/export.ts`, frostyard/pocket's Export.
  - **Now:** none.
  - **Docs:** none.

- [ ] **G64. A new context, or a compaction, for any session.** Missing.
  - **Was:** New Context and Compact on any session.
  - **Now:** a standing session compacts by itself and starts over behind a confirmation (ADR-0026); any other session does neither.
  - **Docs:** none.

- [ ] **G65. Steer a run, and edit the queue.** Missing.
  - **Was:** Ctrl+Enter sent a message into the run under way; a queued message could be edited or withdrawn; Stop could hold the queue; Up-arrow brought back earlier messages; `@agent task` in the composer.
  - **Now:** a message sent while a run works waits for its end. `proto.SendMessage` holds `requestId`, `text` and `attachments`.
  - **Docs:** none.

- [ ] **G66. Background commands, and Running Now.** Missing.
  - **Was:** pi-pocket's `background` and `running` tools (`src/server/extensions/background.ts`), the composer's pill and reader of background commands, and frostyard/pocket's Running Now (`running.go`), with Stop.
  - **Now:** the tools are `read`, `write`, `edit` and `bash`, beside the team's. The owner's global `AGENTS.md` still tells agents to use `background` (G85).
  - **Docs:** none.

- [ ] **G67. Archive a session.** Missing.
  - **Was:** pi-pocket's archive, and frostyard/pocket's Archive in the sidebar.
  - **Now:** sessions are never archived; Recent holds them all.
  - **Docs:** none.

- [ ] **G68. Plan mode.** Missing.
  - **Was:** pi-pocket's `mode` tool (`src/server/extensions/codemode.ts`) and frostyard/pocket's Plan Mode.
  - **Now:** none outside a plan thread ([ADR-0020](adr/0020-plan-before-building-with-the-plan-gate-and-consults-in-code.md)).
  - **Docs:** none.

- [ ] **G69. Subagents for the owner's and the supervisor's sessions.** Missing.
  - **Was:** pi-pocket's `subagent`, with frostyard/pocket's subagents bar.
  - **Now:** a thread's specialist has `team_worker` ([ADR-0021](adr/0021-give-a-threads-specialist-a-worker-on-an-api-key-model-in-code.md)); no other session has a helper.
  - **Docs:** ADR-0019 and ADR-0021 put workers where subagents were for a thread's specialist; nothing says the rest is dropped.

- [ ] **G70. A project memory tool.** Partial.
  - **Was:** pi-pocket's `memory` tool (`src/server/extensions/memory.ts`), and frostyard/pocket's core memory in the Inbox (`core_memory.go`).
  - **Now:** `team_memory` proposes an agent's lessons, for team profiles alone ([ADR-0015](adr/0015-keep-each-agents-lessons-approved-by-the-owner-in-code.md)); a private agent and the owner's plain session have none ([ADR-0042](adr/0042-give-a-private-agent-its-own-preamble-and-no-team-in-its-sessions.md)).
  - **Docs:** ADR-0042 decides the private agent's; none decides the plain session's.

- [ ] **G71. Artifacts.** Missing.
  - **Was:** pi-pocket's `artifact` tool (`src/server/extensions/artifacts.ts`), and frostyard/pocket's Artifacts place (`artifacts.go`).
  - **Now:** none. The file viewer ([ADR-0066](adr/0066-let-the-owner-read-the-agents-homes-in-a-read-only-file-viewer.md)) reads files in the homes.
  - **Docs:** not on the out-of-scope list.

- [ ] **G72. Push notifications.** Deferred.
  - **Was:** pi-pocket's Web Push (`src/server/push.ts`), to the owner's devices while the app was closed.
  - **Now:** the desktop notifies while it runs ([ADR-0049](adr/0049-notify-the-owner-of-what-waits-once-decided-in-code.md)); iOS notifies nothing.
  - **Docs:** the [phone plan](plans/phone-client.md)'s "Later / ideas" (APNs, which needs the paid developer program) and its open question "Push".

## frostyard/pocket: the desktop client

- [ ] **G73. Notification filtering.** Partial.
  - **Was:** frostyard/pocket's `notify.go`: Do Not Disturb, snooze, quiet hours, bursts gathered into one notification, approval tags that expire.
  - **Now:** a notification shows when its kind is on and the window is not focused (`state.ShowNotice`, [notify.go](../desktop/internal/state/notify.go)).
  - **Docs:** [ADR-0010](adr/0010-port-the-desktop-client-selectively-from-frostyard-pocket.md) lists "notification filtering" among what was ported.

- [ ] **G74. Commands and keys.** Missing.
  - **Was:** Find in Session, Next Session That Needs You, J and K and 1 to 6 in the Inbox, Allow and Deny on ⌥Y and ⌥N, and a Keyboard Shortcuts sheet (frostyard/pocket's `frame.go`).
  - **Now:** none of these in the command palette or the keys.
  - **Docs:** none.

- [ ] **G75. The Team place.** Missing.
  - **Was:** frostyard/pocket's team page (`teampage.go`): the roster, an agent's page, a hover card, each agent's lessons.
  - **Now:** the sidebar groups sessions by agent; the Inbox lists lessons.
  - **Docs:** the architecture's desktop places list "The team." as intent.

- [ ] **G76. The session's side pane.** Partial.
  - **Was:** spend, speed and cache in the side pane, the subagents bar (G69) and the background pill (G66).
  - **Now:** the context mark and the usage ([ADR-0067](adr/0067-slim-the-composer-copy-a-reply-and-show-the-context-mark-by-the-composer.md), ADR-0052).
  - **Docs:** none.

## The owner's configuration

What moving the owner's private configuration repository to pocketgo needs. Nothing here names a host.

- [ ] **G77. The TrueNAS MCP server.** Missing.
  - **Was:** Moneo's and Bellonda's profiles name `mcp: [truenas]`, a server the global `mcp.json` runs as a stdio `command`.
  - **Now:** pocketd refuses a stdio server ([profiles/mcp.go](../service/internal/profiles/mcp.go), ADR-0063) and logs a warning, so both profiles lose its tools.
  - **Docs:** ADR-0063 refuses stdio, since its command would run outside the sandbox. A choice for the owner: serve it over HTTP, or a new decision for stdio servers run in the sandbox.

- [ ] **G78. The MCP servers' new home.** Missing.
  - **Was:** the servers in the global `mcp.json`, their secrets as `!command` values.
  - **Now:** they go in the profile folder's `mcp.json`, each header `"stored"`, set by `pocketd mcp set-header` or `pocketd mcp login`.
  - **Docs:** ADR-0063 and the setup guide. A task for the owner, unless done.

- [ ] **G79. The desk cap.** Missing.
  - **Was:** the desk's `cap` is 16 in the owner's `desk.json`.
  - **Now:** pocketd's default is 12 (`team.DefaultDeskCap`), unless `-desk-cap` or `POCKETD_DESK_CAP` in `~/.config/pocketd.env` says otherwise. The survey did not read the team VM's file.
  - **Docs:** the architecture names the flag.

- [ ] **G80. Rules still in prompts.** Partial.
  - **Was:** the owner's global `AGENTS.md` holds workflow rules. [ADR-0011](adr/0011-put-rules-in-code-not-in-model-judgment.md) puts a rule that can be stated in code; several are:
    - no commit, push or force-push unless asked
    - approve or merge, never both: agents can still run `gh` as the machine account, since the sandbox hides only the data and runtime folders ([sandbox.go](../service/pig/sandbox.go))
    - a private agent's data stays private, yet every agent's home is visible in the one sandbox
    - `/tmp` is shared between agents
  - **Now:** ADR-0043 loads the file into every session, so the rules reach agents as text.
  - **Docs:** none.

- [ ] **G81. The global `AGENTS.md` names tools pocketgo lacks.** Missing.
  - **Was:** it tells agents to use `team_wait_for_ci`, `watch_pr`, `subagent`, `background`, plan mode, `ask` and the desk's `reports/` export, and the Clerk's and the supervisor's profiles use `desk read`.
  - **Now:** none of them exists (G4, G5, G10, G18, G28, G39, G66, G68, G69), and ADR-0043 puts the file in every session.
  - **Docs:** none. Either the tools come back or the file changes, in the owner's private repository.

## pocketgo's own unfinished plans

- [ ] **G82. Phase 4: the Coder execution adapter.** Planned.
  - **Now:** not started: there is no `service/exec/coder/`, Coder REST client or workspace template.
  - **Docs:** roadmap Phase 4, [ADR-0009](adr/0009-run-agents-in-coder-workspaces-through-pi-env-and-the-rest-api.md), and the architecture's "Execution adapters". Phase 5 is G58 to G61.

- [ ] **G83. Live checks never seen.** Unverified.
  - **Now:** each of these slices of roadmap Phase 2 is built and merged, and waits for its "Still to see" on the team VM:
    - 10, the owner's model providers (a model added under `builtinModels`)
    - 11, the supervisor's standing session
    - 12, New Session on a repository
    - 13, the model selector
    - 14, attachments
    - 15, the night shift: schedules, gathers and proposals, the board and its launch, a launched proposal's CI
    - 16, Merge All Ready, with the bot exceptions
    - 17, the supervisor's message to a thread, and unblocking
    - 18, the read-only skills and uploads folders, and upright photos
    - 19 and 20, the global and folder `AGENTS.md` and skills, and the global file read-only
    - 21, `team_inbox`
    - 22, the question tool
    - 23, desktop notifications and the badge
    - 24, search
    - 25, usage
    - 26, saved prompts
    - 27, the Changes view
    - 28, a worktree from a fresh fetch
    - 29, a closed thread's worktree removed
    - 30, a cross-review reading pinned code
    - 31, rename
    - 32, the extra approval counted as GitHub counts it
    - 33, a private agent's schedules
    - 34, the Copilot sign-in
    - 35, a closed thread reopened by a message
    - 36, the setup guide followed by someone else
    - 37, MCP servers
    - 38, the service's messages folded
    - 39, the file viewer
    - 40, the slim composer
  - Also: Phase 2's "Done when" has not seen a specialist propose a lesson in a thread, and Phase 3 has not seen a shift paused by the owner's message and resumed, a done that waits on CI, or the same failing checks three times.
  - **Docs:** the [roadmap](plans/roadmap.md), item by item.

- [ ] **G84. The phone and tablet client.** Planned.
  - **Now:** the [phone plan](plans/phone-client.md)'s Phase 2 (iPad) and Phase 3 (iPhone) are built; neither "Done when" is seen, and the titles cut short at iPad portrait (820×1180) are open.
  - **Docs:** the phone plan; its "Later / ideas" also holds the Terminal on iOS, documents by paste or drop, photos as attachments, and Android.

- [ ] **G85. Approve and merge in the browser.** Planned.
  - **Now:** the NSGo client ([ADR-0068](adr/0068-build-a-shared-nsgo-client-beside-the-desktop-client.md)) in a browser offers a link to GitHub, with no approval or merge, since GitHub's sign-in needs a keyring and the sign-in lock.
  - **Docs:** the [shared owner merge service](plans/shared-merge-service.md), proposed, which says so in its last paragraph.

- [ ] **G86. Desktop builds and packages.** Planned.
  - **Now:** open issues [frostyard/pocketgo#72](https://github.com/frostyard/pocketgo/issues/72) (tracking), [#69](https://github.com/frostyard/pocketgo/issues/69) (macOS), [#70](https://github.com/frostyard/pocketgo/issues/70) (Windows) and [#71](https://github.com/frostyard/pocketgo/issues/71) (Linux). [ADR-0070](adr/0070-ship-nsgo-as-the-default-linux-client.md) cuts a Flatpak and a web archive.
  - **Docs:** the issues.

- [ ] **G87. The shared browser.** Planned.
  - **Now:** [frostyard/pocketgo#152](https://github.com/frostyard/pocketgo/issues/152): an agent's browser tool and a desktop panel that watch and drive one page. The roadmap's out-of-scope list holds "the built-in browser", so it needs a decision.
  - **Docs:** the issue.

- [ ] **G88. History from pi-pocket.** Deferred.
  - **Now:** no importer. Sessions start fresh.
  - **Docs:** the roadmap's "Later / ideas": "A fresh start with the profiles and lessons imported is acceptable." ADR-0015 rejects importing pi-pocket-team's lessons, so the two disagree on lessons.

- [ ] **G89. The terminal's later asks.** Missing.
  - **Was:** asks the owner named and left out when the terminal was built on 2026-11-20: open a terminal at a session's folder, Ctrl+Shift+T, a key to go to the terminal, and a Reconnect button.
  - **Now:** none.
  - **Docs:** none; [ADR-0025](adr/0025-give-the-owner-a-shell-on-the-service-host-from-the-desktop.md) builds the terminal.

## Probably left out on purpose

Each of these looks deliberate from an ADR or the owner's choice, but no doc says it in so many words. Confirm, and strike it.

- [ ] **G90. MAI-Code as a worker only.** Confirm.
  - **Was:** pi-pocket-team ran `github-copilot/mai-code-*` only as a worker (`WORKER_ONLY_MODEL`, `team.ts`).
  - **Now:** any model with a sign-in runs any session.
  - **Docs:** [ADR-0021](adr/0021-give-a-threads-specialist-a-worker-on-an-api-key-model-in-code.md) put the rule off until a Copilot sign-in; [ADR-0022](adr/0022-run-any-session-on-any-sign-in-and-choose-workers-from-the-owners-list.md) removed every worker-only rule at the owner's request, and the Copilot sign-in has since landed ([ADR-0062](adr/0062-sign-pocketd-in-to-github-copilot-from-the-desktop-with-a-device-flow-it-runs.md)).

- [ ] **G91. The router's tiers.** Confirm.
  - **Was:** tiers quick, standard, high and premium in `router.json`, with a floor for each kind of work, such as high or above for a plan or a review.
  - **Now:** the owner's ordered `workers.json`, and the worker the supervisor asks for ([ADR-0022](adr/0022-run-any-session-on-any-sign-in-and-choose-workers-from-the-owners-list.md)).

- [ ] **G92. Schedules edited from the client.** Confirm.
  - **Was:** frostyard/pocket made, edited and paused schedules (`schedules.go`).
  - **Now:** the board's Schedules section reads them; the owner edits `schedules.json` ([ADR-0030](adr/0030-send-scheduled-messages-from-the-owners-file-and-the-supervisors-tool-in-code.md)).

- [ ] **G93. Revert in Changes, and file downloads.** Confirm.
  - **Was:** frostyard/pocket reverted a file from its diff, and downloaded a file.
  - **Now:** the Changes view and the file viewer are read-only ([ADR-0053](adr/0053-show-a-sessions-changes-read-with-isolated-git-in-code.md), [ADR-0066](adr/0066-let-the-owner-read-the-agents-homes-in-a-read-only-file-viewer.md)).

- [ ] **G94. Agents that drive other sessions.** Confirm.
  - **Was:** pi-pocket's PiP tools, `session_start`, `session_send`, `session_read`, `session_follow`, `session_stop`, `session_archive`, `session_schedule`, `session_watch_pr` and `repo_watch` (`src/server/extensions/pip.ts`).
  - **Now:** the supervisor's team tools.
  - **Docs:** the roadmap's out-of-scope list: "PiP, which the supervisor replaces." Its watches are G4 and G5.

## Left out on purpose (not gaps)

For reference, with where each is decided:

- **The roadmap's out-of-scope list:** multiplayer (invites, roles, side chat, turns, and so pi-pocket's `ask_someone`); a web UI or PWA (and so pi-pocket-team's web Desk, Team and Shift pages); Design, the extension gallery, themes and the built-in browser; self-update, the launcher and tunnels; relays to Discord, Slack, Telegram, Matrix or RSS; Pi TypeScript extensions; PiP; Arrakis and the homewiki.
- **The roadmap's "What carries over":** pi-pocket's code; pi-pocket-team's drop-in plumbing, untyped `run` map and `night-shift` names; frostyard/pocket's `internal/pocket`, Arrakis and the wiki; syncing the configuration repository with the live files.
- **Roadmap slice 6b:** frostyard/pocket's batches, operation journal and Desk receipts.
- **Roadmap slice 7:** whole-plan verification, scope changes, section comments, reopening an ask, `plan amend`, `plan nochange`, the `needs-plan` label and queued consults.
- **ADR-0015:** the owner editing a lesson's text.
- **ADR-0042:** lessons for a private agent.
- **ADR-0013:** a fallback to the supervisor's model when a profile's cannot run.
- **ADR-0014:** a fallback to the main checkout when a worktree cannot be made.
- **ADR-0063:** MCP for the owner's plain session, stdio servers, a change before restart, and MCP for reviews, consults, gathers, plan leads and workers.
- **ADR-0024:** the 60-minute cap on a round's wait for subagents.
- **[ADR-0012](adr/0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md):** pi-pocket's Lancet guard; the sandbox replaces it.
- **ADR-0040:** the terminal on iOS, and with it the phone's key bar.
- **ADR-0017:** an agent setting `ready-for-human`; pocketd alone sets it.
- **pi-pocket-team's previews** (`preview.ts`, `preview-desk.ts`) are replaced by the view tests and `make snapshot`.
