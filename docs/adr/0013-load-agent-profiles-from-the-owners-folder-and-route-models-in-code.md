# 0013 — Load agent profiles from the owner's folder, and route models in code

- **Status:** Accepted
- **Date:** 2026-11-19

## Context

Phase 2 runs a team: a supervisor delegates to named specialists. Each one is defined by a profile: a Markdown file whose frontmatter sets its role, model, repositories, folders, MCP servers and privacy, and whose body is its identity prompt. The owner keeps the 13 profiles in the private pi-pocket-config repository. On the team VM they are installed in `~/.config/pi-pocket-team/agents/`, where pi-pocket-team reads them. That folder also holds the owner's backup copies (`odrade.md.bak-20261118-200400` and the like).

The profiles write folders as `~/…`. Their models name the provider as Pi's catalog first spelled it: `anthropic/claude-opus-5-5`, `openai/gpt-6-astra` and `github-copilot/gpt-6-luna`. pocketd runs only subscription sign-ins (ADR-0005), and there the ChatGPT models come from `openai-codex`, not `openai`.

The architecture already lists the profile fields. Phase 2 adds `tools`: the tools an agent may call. It also asks for one model-router seam whose only rule, for now, is the profile's model.

A profile decides what an agent is told and what it may do. pocketd's agents run as the same user who owns the profile folder (ADR-0012).

## Decision

1. **pocketd reads the profiles where pi-pocket-team reads them.** The default is `~/.config/pi-pocket-team/agents`. `-profiles` or `POCKETD_PROFILES` names another folder. While both systems run there is one copy, synced from pi-pocket-config, and this repository carries example profiles only.
2. **Only `<id>.md` files are profiles.** The id is `[a-z0-9][a-z0-9_-]*`, so backups and other files are ignored. A profile is a regular file with one name: pocketd never follows a symbolic link to one, refuses one with another name (a hard link) that could change it from outside the folder, and never waits on a named pipe. pocketd reads the folder each time it needs a profile, so an edit takes effect without a restart.
3. **Code validates every field.**
   - `role`: `supervisor`, `clerk` or `agent`, default `agent`.
   - `model`: `provider/id`; for a list, the first entry.
   - `repositories`: `owner/name` to a folder.
   - `workspace` and `home`: folders, absolute or `~/…`.
   - `mcp`: a list of server names.
   - `private`: a boolean.
   - `tools`: a list of tool names the agent may call; without it, every tool.
   - Any other key, any value of the wrong shape, and a second YAML document in the frontmatter make the profile invalid. pocketd lists an invalid profile with its problem and starts no session from it.
   - The body is the identity prompt, and its first `# Heading` is the display name.
4. **The profile folder is read-only to agents.** The sandbox binds it read-only, pins the folders above it, and the file tools refuse it, as for pocketd's own start files (ADR-0012). It is not a place a systemd link may lead: pocketd checks a profile's names only when it reads it as a profile. Otherwise an agent could rewrite its own role, or another agent's instructions.
5. **One router chooses every model, in code.** `router.Choose` takes what the work is (a session now; a delegation or a round later) and the models pocketd can run. It returns the model and the rule that chose it:
   - `owner`: the owner named a model.
   - `profile`: the profile's model, when pocketd can run it.
   - `profile-alias`: the profile's model id under the provider that serves it on a subscription. The aliases are a fixed table in code: `openai` to `openai-codex`.

   A profile whose model pocketd cannot run starts nothing, and the error names the model. pocketd records the choice and its rule with the session.
6. **A session can start from a profile.** pocketd applies it as one agent configuration:
   - the routed model
   - the profile's tools
   - instructions made of the profile's identity and pocketd's generated preamble
   - the working directory, by default the profile's `home`

   A private profile works only in its home, in a session the owner starts.

## Consequences

- The owner's existing profiles work unchanged in both systems, and an edit to one reaches pocketd at its next use.
- An agent can no longer change any profile. A profile changes only through pi-pocket-config, or by the owner on the host.
- A profile with a model pocketd cannot run says so, for example the Clerk's `github-copilot` model until pocketd signs in to Copilot.
- The router is where later rules go (ADR-0011): run records and the owner's allowed models. Until then, every choice is recorded with its rule.
- Protocol version 3: `GET /v1/profiles`, and `CreateSession.profile`. A desktop and a pocketd of different versions refuse each other.

## Alternatives considered

- **Copy the profiles into pocketgo's own folder:** two copies drift, and pi-pocket keeps running on the old one.
- **Edit the profiles to name `openai-codex`:** pi-pocket reads the same files, as written.
- **Let a profile name any provider and fail at the first request:** the session would start and fail later, far from the cause.
- **Leave the profile folder writable to agents:** an agent could grant itself the supervisor role, or rewrite another agent's instructions.

## References

- Shapes: [design: system architecture](../design/architecture.md#service), [specs: protocol](../specs/protocol.md), [plans: roadmap](../plans/roadmap.md)
- Builds on: [ADR-0005](0005-keep-the-agent-loop-and-model-credentials-in-the-service.md), [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0012](0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md)
