# 0043 — Load the global and folder AGENTS.md and the skills into every session

- **Status:** Accepted
- **Date:** 2026-11-21

## Context

**Pi loaded three things into every session, and pocketd loads none of them** (frostyard/pocketgo#80):
- The owner's global instructions, `~/.pi/agent/AGENTS.md`: "Brian's rules for every session", about 190 lines (18 KB on 2026-11-21). They hold the general rules, plan-then-execute on a worker model, and cross-review before a pull request is ready. Under pocketd no agent gets them. The rules reach an agent only where a profile or a brief repeats them.
- The `AGENTS.md` of the session's folder: an agent's home, a workspace or a repository's worktree. Chani's memory system is her home's `AGENTS.md`.
- A list of skills, by the name and description in each `SKILL.md`'s frontmatter, from the global skills folder `~/.pi/agent/skills` and from the folder's `.pi/skills`. The agent read a skill's body with its read tool. Chani has 4 skills in her home's `.pi/skills`. For now her profile tells her to read them herself (pi-pocket-config#93).

The owner decided on 2026-11-21 (#80, option a): pocketd loads the global file into every session, before the profile's own instructions.

**Facts the design must keep:**
- `sessionInstructions` (`service/pig`) builds a session's instructions: pocketd's preamble, then the profile's identity, a supervisor's roster, and the lessons section. A private profile gets its own preamble, with no roster and no lessons section ([ADR-0042](0042-give-a-private-agent-its-own-preamble-and-no-team-in-its-sessions.md)).
- pocketd owns the instructions from the last lessons marker on (`team.WithoutLessons`, [ADR-0015](0015-keep-each-agents-lessons-approved-by-the-owner-in-code.md)). `Engine.SetLessons` replaces only that section, and only at the end. A profile whose identity holds a marker line is invalid.
- pocketd never rewrites an ordinary session's identity. A standing session takes its profile at each refresh and at Start Over, keeping its lessons section ([ADR-0026](0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md)).
- pocketd names the global skills folder with `-skills` ([ADR-0033](0033-gather-proposals-from-the-team-in-read-only-sessions-in-code.md)), and agents cannot change it ([ADR-0035](0035-make-the-skills-and-uploads-folders-read-only-to-agents.md)). A gather reads its skill as the agents' sandbox shows it (`Engine.ReadUpTo`), never with pocketd's own rights.
- Agents can write their folders, and most of the service user's home, `~/.pi/agent` included. The sandbox hides pocketd's data folder and `/run/user` ([ADR-0012](0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md)).

## Decision

1. **The global file.** `pocketd serve -instructions FILE` names it. When the flag is not given, `$POCKETD_INSTRUCTIONS` names it, else the service user's `~/.pi/agent/AGENTS.md`; `-instructions ""` names none. Every session holds it, in every role, right after pocketd's preamble: before the identity, as the owner's rules for every session, which the profile then makes specific. It is bounded at 64 KiB (`team.MaxInstructionsFile`).
2. **The order of a session's instructions** (`sessionInstructions`, `team.Instructions`):
   1. pocketd's preamble;
   2. the global file;
   3. the identity: the team's "You are <Name>, the team's <role>" and the profile, or a private agent's `team.PrivateIntro` and the profile, or a worker's instructions;
   4. a supervisor's roster;
   5. the folder's `AGENTS.md`;
   6. the skills list;
   7. the lessons section, last.

   A part with nothing in it is left out. The folder's file comes after the identity and the roster, because it is about the place, not the agent. Each file is introduced by where it was read: "The owner's rules for every session follow, from <path>." and "The instructions of your folder follow, from <path>.", and each ends with a line that says so.
3. **The folder's file.** pocketd loads `<session folder>/AGENTS.md` for every session, as Pi did: a home, a workspace and a repository worktree. A repository's rules are what a thread's specialist and its worker need most, and a session only in a home would miss them. Only the folder itself is read, not the folders above it, which for a worktree are other checkouts. The bound is the same, 64 KiB: pocketgo's own `AGENTS.md` is 38 KB.
   - **A worker** gets everything: the global file, its own worker instructions, the folder's file and the skills, in the order of decision 2. The worker writes the code in the thread's folder, under that folder's rules.
   - **A cross-review** gets the global file and the global skills, but nothing of its folder. Its folder is the change under review, and its author could write the reviewer's instructions there. The reviewer reads the repository's `AGENTS.md` with its read tool, as a part of what it reviews.
   - Every other session gets everything: a session the owner starts, from a profile or not, a thread's specialist, a consult, a plan's lead and a gather's proposal session.
4. **The skills list.** The skills are read from the global skills folder (`-skills`) and from `<session folder>/.pi/skills`, in that order. When both name the same folder, it is read once.
   - Each folder holding a `SKILL.md` is a skill. A file, or a folder whose name starts with a dot or has no `SKILL.md`, is no skill and gets no note. At most 100 skills are listed per folder (`team.MaxSkillsListed`), the first by name, and a note says when more are there.
   - pocketd reads the first 8 KiB of each `SKILL.md` (`profiles.MaxSkillHead`). Its frontmatter must end within them, and must be one YAML map. Its `name` must be its folder's name (1 to 64 letters, digits, `-` and `_`). Its `description` must be text that is not empty, at most 1,024 characters once its spaces and line breaks are made single spaces (`profiles.ParseSkillHead`). Other keys are the skill's own.
   - Each skill is listed as `- <name>: <description> (<path of its SKILL.md>)`, after one line that tells the agent to read a skill's file with its read tool when a task matches. pocketd never puts a skill's body in the instructions.
   - A skill whose frontmatter breaks a rule is left out with a note that names its file and the reason. It never fails the session. A folder whose name is no skill's is noted with its name quoted.
5. **When pocketd reads the files.** It reads them when a session is made (`Engine.CreateSession`), and when a standing session starts over (`Engine.StartOver`, with the profile it applies), and at no other time.
   - It keeps what it wrote of them with the session, as a PiG document (`pocketgo.loaded`), in the commit that writes the instructions. A standing session's refresh (`Engine.Reconfigure`) writes its instructions again with the parts it kept, and reads no file. So a refresh never changes the parts, and the model's cache of the instructions holds while the agent edits its own `AGENTS.md`. A Start Over whose profile cannot be applied changes no instructions, and reads nothing.
   - **Existing sessions** (no migration, no protocol version):
     - A session made before this decision keeps its instructions. It has no kept parts, so a refresh writes none in.
     - A standing session gets the files at its next Start Over. An ordinary session never gets them, since pocketd never rewrites an ordinary session's identity; a new session gets them.
     - A file changed later reaches every session made after the change, and a standing session at its next Start Over.
   - **The lessons stay last, once.** The lessons section still ends the instructions, and `SetLessons` and `team.WithoutLessons` do not change. A file that holds the lessons marker or the lessons end as a line of its own is left out with a note, as a profile with one is invalid: pocketd would take the rest of the instructions for its own section, and cut them there at a refresh. A skill's description is one line after `- <name>: `, so it can hold no such line. A private session, which keeps no section, keeps the files whole at its refresh.
6. **A private profile** ([ADR-0042](0042-give-a-private-agent-its-own-preamble-and-no-team-in-its-sessions.md)) gets the global file, its `team.PrivateIntro` and profile, its home's `AGENTS.md` and its skills: the global ones and its home's `.pi/skills`. It gets no roster and no lessons section, as before. Chani's profile can then drop the note that tells her to read her `AGENTS.md` and skills herself.
7. **Security.**
   - **pocketd reads the files as the agents' sandbox shows them, never with its own rights.** Every file and skills folder of a session is read in one run of pocketd's own `pig.ContextHelper` in the sandbox (`Engine.loadContext`), as a gather's skill is read (ADR-0033).
   - **A link is followed as the sandbox shows it.** That is safe: a link leads nowhere the agent's own read tool could not read already. A link into pocketd's data folder, or anywhere else the sandbox hides, reads as nothing. So pocketd follows links, and needs no rule that keeps them inside the folder.
   - **No file holds the read up.** The helper opens each file without waiting for a writer, and checks on the descriptor it read that it is a regular file. A named pipe, a socket or a link loop is no file.
   - **Every read has a bound.** A file is read up to its bound and one byte more: 64 KiB for an `AGENTS.md` and 8 KiB for a `SKILL.md`. pocketd lists at most 100 skills a folder, and it takes at most 4 MiB of the helper's answer, checked entry by entry.
   - **A file pocketd leaves out is a note.** That covers a file over its bound, one that is not UTF-8 and one that holds a lessons line. The note tells the agent the path, the reason, and that it may read the file itself.
   - **A file that is not there is nothing,** with no note.
   - **A `SKILL.md` in the global skills folder with more than one name (a hard link) is left out,** as a gather leaves it out: agents cannot change that folder, but they could change such a file through its other name. A skill in the session's own folder is listed all the same, since agents can change that folder anyway.
   - **A read the sandbox cannot make is one note,** which names the files and folders left out (`team.UnreadNote`). The session starts all the same.
8. **No wire change.** No type, path or event changes, so the protocol version stays.

## Consequences

- Every agent gets the owner's rules for every session, its folder's rules and its skills, as under Pi. No profile or brief has to repeat them, and Chani's memory system and skills reach her sessions with no prompt telling her to read them.
- A session's instructions grow by the files: the global file is about 18 KB today, and a repository's `AGENTS.md` can be 40 KB more. Every session pays for them in tokens, at least once in each new context: a cache holds them across turns.
- A standing session takes changed files only at Start Over, not at its refresh. The owner who changes the global file starts a standing session over to give it the change.
- Making a session now runs one command in the sandbox, as a gather does.
- **Known limits:**
  - **Agents can write the global file.** `~/.pi/agent` is not read-only to agents: only the skills folder in it is (ADR-0035). An agent that rewrote `~/.pi/agent/AGENTS.md` would write every later session's instructions, as one that rewrites another agent's home `AGENTS.md` writes that agent's. Making the file read-only, as ADR-0035 made the skills folder, is a change of the sandbox and a decision of its own: [ADR-0044](0044-make-the-global-instructions-file-read-only-to-agents.md).
  - **An ordinary session made before this decision never gets the files.** Rewriting it would change an identity pocketd never rewrites.
  - **A cross-review gets no repository rules in its instructions.** It reads them as part of the change.

## Alternatives considered

- **Load the folder's `AGENTS.md` only for a profile's home:** a thread's specialist and its worker in a repository would miss the repository's rules, which Pi gave them.
- **Read the files with pocketd's own rights, refusing a link out of the folder (`os.Root`):** a link inside a folder can still lead into the data folder when the folder holds it: a session in the service user's home. The sandbox already says what an agent may read, and reading through it keeps one rule for gathers and sessions.
- **Read the files again at every refresh of a standing session:** an agent that edits its own `AGENTS.md`, as Chani's memory system does, would change its instructions, and lose the model's cache, at every run's end. Pi read them when a session started.
- **Truncate a file over its bound:** a rule cut in the middle reads as a different rule. A note sends the agent to the whole file.
- **Fail the session when the sandbox cannot read the files:** a thread or a review would not start because of a file, which Pi never did. A note tells the agent what is missing.
- **Put a skill's whole text in the instructions:** 10 skills of up to 64 KiB each would fill the context. Pi listed them, and the agent reads the one it needs.

## References

- Shapes: [design/architecture.md](../design/architecture.md#service)
- Built by: [plans/roadmap.md](../plans/roadmap.md) (Phase 2, item 19)
- Builds on: [ADR-0012](0012-run-tools-without-approval-in-a-sandbox-that-hides-pocketds-files.md), [ADR-0015](0015-keep-each-agents-lessons-approved-by-the-owner-in-code.md), [ADR-0021](0021-give-a-threads-specialist-a-worker-on-an-api-key-model-in-code.md), [ADR-0026](0026-keep-the-supervisor-in-one-standing-session-compacted-early-in-code.md), [ADR-0033](0033-gather-proposals-from-the-team-in-read-only-sessions-in-code.md), [ADR-0035](0035-make-the-skills-and-uploads-folders-read-only-to-agents.md), [ADR-0042](0042-give-a-private-agent-its-own-preamble-and-no-team-in-its-sessions.md)
