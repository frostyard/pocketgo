# Documentation

Docs are split by the question they answer:

| Directory | Question | Contents |
| --- | --- | --- |
| [adr/](adr/) | **Why** did we choose this? | Architecture Decision Records — immutable once accepted; superseded, never edited |
| [design/](design/) | **How** does it fit together? | Living documents describing the current architecture |
| [specs/](specs/) | **What exactly** is the contract? | Precise, testable interface definitions |
| [plans/](plans/) | **When/in what order** do we build? | Roadmaps and phase plans; updated as work lands |

## Index

### Decisions (ADRs)

- [0001 — Record architecture decisions](adr/0001-record-architecture-decisions.md)
- [0002 — Agent-portable instruction surface](adr/0002-agent-portable-instruction-surface.md)
- [0003 — Verify gate, risk paths and corrections](adr/0003-verify-gate-risk-paths-and-corrections.md)
- [0004 — Build the service on PiG's durable harness](adr/0004-build-the-service-on-pigs-durable-harness.md)
- [0005 — Keep the agent loop and model credentials in the service](adr/0005-keep-the-agent-loop-and-model-credentials-in-the-service.md)
- [0006 — Share the wire protocol as Go types in one module](adr/0006-share-the-wire-protocol-as-go-types-in-one-module.md)
- [0007 — Split the repository into three Go modules](adr/0007-split-the-repository-into-three-go-modules.md)
- [0008 — Keep work records in the service's own tables](adr/0008-keep-work-records-in-the-services-own-tables.md)
- [0009 — Run agents in Coder workspaces through pi-env and the REST API](adr/0009-run-agents-in-coder-workspaces-through-pi-env-and-the-rest-api.md)
- [0010 — Port the desktop client selectively from frostyard/pocket](adr/0010-port-the-desktop-client-selectively-from-frostyard-pocket.md)
- [0011 — Put rules in code, not in model judgment](adr/0011-put-rules-in-code-not-in-model-judgment.md)
- [0012 — Run tools without approval, in a sandbox that hides pocketd's files](adr/0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md)
- [0013 — Load agent profiles from the owner's folder, and route models in code](adr/0013-load-agent-profiles-from-the-owners-folder-and-route-models-in-code.md)
- [0014 — Delegate in threads, with team tools in code](adr/0014-delegate-in-threads-with-team-tools-in-code.md)
- [0015 — Keep each agent's lessons, approved by the owner, in code](adr/0015-keep-each-agents-lessons-approved-by-the-owner-in-code.md)
- [0016 — Link reported pull requests to their threads, checked on GitHub in code](adr/0016-link-reported-pull-requests-to-threads-checked-on-github-in-code.md)
- [0017 — Record cross-review verdicts and set ready-for-human in code](adr/0017-record-cross-review-verdicts-and-set-ready-for-human-in-code.md)
- [0018 — Approve and merge as the owner from the desktop, with Pocket's gates](adr/0018-approve-and-merge-as-the-owner-from-the-desktop-with-pockets-gates.md)
- [0019 — Start each cross-review in code, with the author's agent on the reviewer model](adr/0019-start-each-cross-review-in-code-with-the-authors-agent-on-the-reviewer-model.md)
- [0020 — Plan before building, with the plan gate and consults in code](adr/0020-plan-before-building-with-the-plan-gate-and-consults-in-code.md)
- [0021 — Give a thread's specialist a worker on an API-key model, in code](adr/0021-give-a-threads-specialist-a-worker-on-an-api-key-model-in-code.md)
- [0022 — Run any session on any sign-in, and choose workers from the owner's list](adr/0022-run-any-session-on-any-sign-in-and-choose-workers-from-the-owners-list.md)
- [0023 — Add the owner's own model providers from a file checked in code](adr/0023-add-the-owners-own-model-providers-from-a-file-checked-in-code.md)
- [0024 — Run a delegated thread in rounds as a shift, driven in code](adr/0024-run-a-delegated-thread-in-rounds-as-a-shift-driven-in-code.md)
- [0025 — Give the owner a shell on the service host from the desktop](adr/0025-give-the-owner-a-shell-on-the-service-host-from-the-desktop.md)
- [0026 — Keep the supervisor in one standing session, compacted early in code](adr/0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md)
- [0027 — Start the owner's session on a repository, in a new worktree named in code](adr/0027-start-the-owners-session-on-a-repository-in-a-new-worktree-named-in-code.md)
- [0028 — Let the owner change a session's model from the composer](adr/0028-let-the-owner-change-a-sessions-model-from-the-composer.md)
- [0029 — Attach the owner's files to a message, stored outside the data folder](adr/0029-attach-the-owners-files-to-a-message-stored-outside-the-data-folder.md)
- [0030 — Send scheduled messages from the owner's file and the supervisor's tool, in code](adr/0030-send-scheduled-messages-from-the-owners-file-and-the-supervisors-tool-in-code.md)
- [0031 — Count pocketd's ready cross-review as an approval, and merge all ready pull requests at once](adr/0031-count-pocketds-ready-cross-review-as-an-approval-and-merge-all-ready-at-once.md)
- [0032 — Let a supervisor send to a thread it delegated, through the outbox](adr/0032-let-a-supervisor-send-to-a-thread-it-delegated-through-the-outbox.md)
- [0033 — Gather proposals from the team in read-only sessions, in code](adr/0033-gather-proposals-from-the-team-in-read-only-sessions-in-code.md)
- [0034 — Launch the owner's picks from the supervisor's standing session, in code](adr/0034-launch-the-owners-picks-from-the-supervisors-standing-session-in-code.md)
- [0035 — Make the skills and uploads folders read-only to agents](adr/0035-make-the-skills-and-uploads-folders-read-only-to-agents.md)
- [0036 — Let proposal sessions, consults and plan leads read GitHub with GET calls built in code](adr/0036-let-proposal-sessions-consults-and-plan-leads-read-github-with-get-calls-built-in-code.md)
- [0037 — Unblock a thread when the supervisor's message reaches it](adr/0037-unblock-a-thread-when-the-supervisors-message-reaches-it.md)
- [0038 — Check bot pull requests again in the run once a merge moves their base](adr/0038-check-bot-pull-requests-again-in-the-run-once-a-merge-moves-their-base.md)
- [0039 — Count a bot's pull request the run left in conflict as waiting for its rebase](adr/0039-count-a-bots-pull-request-the-run-left-in-conflict-as-waiting-for-its-rebase.md)
- [0040 — Build the iPad and iPhone client from the desktop's code on MyGo's iOS target](adr/0040-build-the-ipad-and-iphone-client-from-the-desktops-code-on-mygos-ios-target.md)
- [0041 — Save the window and hold the GitHub refresh when iOS suspends the app](adr/0041-save-the-window-and-hold-the-github-refresh-when-ios-suspends-the-app.md)
- [0042 — Navigate a compact window by a bar of tabs](adr/0042-navigate-a-compact-window-by-a-bar-of-tabs.md)

### Org-wide decisions

- [Org-wide decisions (frostyard/core ADRs)](org-adrs.md)

### Guides

- [After you create a repository from this template](new-repository.md)

### Design

- [Agent-ready repository](design/agent-ready-repository.md)
- [System architecture](design/architecture.md)

### Specs

- [Protocol, version 1](specs/protocol.md)

### Plans

- [Roadmap](plans/roadmap.md)
- [Phone and tablet client](plans/phone-client.md)

## Conventions

- **New docs start from their category's `TEMPLATE.md`** (in each directory).
- New decision → new ADR with the next number; if it reverses an old one, mark
  the old one `Superseded by NNNN` rather than editing it.
- Design docs are updated in place to always reflect reality.
- Specs change only alongside the code that implements them.
- Cross-links between categories are mandatory in both directions — see the
  documentation rules in [AGENTS.md](../AGENTS.md) (CLAUDE.md/GEMINI.md are
  symlinks to it, ADR-0002).
- Adding a doc means adding it to the index above.
