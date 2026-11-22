# 0062 — Sign pocketd in to GitHub Copilot from the desktop, with a device flow it runs

- **Status:** Accepted
- **Date:** 2026-11-22

## Context

**Signing in needs a shell on the service host.** pocketd signs in to a subscription only with `pocketd login github-copilot` (also `anthropic` and `openai-codex`), which runs PiG's sign-in and writes the data folder's `auth.json` in Pi's format ([ADR-0005](0005-keep-the-agent-loop-and-model-credentials-in-the-service.md)). The owner asked to sign in from the desktop instead: a Sign-ins page in Settings, and a sheet that runs GitHub's device flow ([frostyard/pocketgo#73](https://github.com/frostyard/pocketgo/issues/73)).

**Facts the design must keep:**
- The credentials are stored and refreshed on the service host, in one place ([ADR-0005](0005-keep-the-agent-loop-and-model-credentials-in-the-service.md)). The running engine reads them through one PiG `AuthStorage` of `auth.json`, which its models' credential store (`keptCredentials`) wraps. PiG refreshes a subscription's token through that store's `Modify`, under its process lock and the file's lock, and writes the file whole. Two processes refreshing one OAuth refresh token can sign each other out. That is why `loginKey` refuses to store an API key while pocketd serves.
- `GET /v1/models` lists only the models with a sign-in ([ADR-0022](0022-run-any-session-on-any-sign-in-and-choose-workers-from-the-owners-list.md)). The engine reads the store at each call, so a sign-in written through it lists its models at once.
- PiG's Copilot sign-in (`LoginGitHubCopilot`) runs the whole flow itself, on the network's real clock: it polls with a sleep no caller can replace. Its token exchange is public as the provider's OAuth refresh, which gets Copilot's token and the account's models for a GitHub token of Copilot's OAuth app.
- frostui has no sheet component. The desktop draws its dialogs with MyGo's modal, as the GitHub App's device flow already does ([ADR-0018](0018-approve-and-merge-as-the-owner-from-the-desktop-with-pockets-gates.md)).
- The desktop opens a pull request only at a link it builds, never at a URL pocketd sent.

## Decision

1. **pocketd runs the device flow, and the desktop never sees a token.** `POST /v1/signins/{id}`, `{id}` the provider's id, starts GitHub's device flow, for Copilot's OAuth app with the `read:user` scope, and answers the flow: its id, the user code, the verification page, `expiresIn` and `interval` (protocol version 32, rule 69). pocketd polls GitHub itself (`signins.Flows`): after each interval, never past the code's expiry, a `slow_down` widening the wait by 5 seconds or to the interval GitHub names, a denial or an expiry ending it, three polls in a row that do not reach GitHub failing it. Every field of GitHub's answers is checked in code (`signins.GitHub`), on GitHub's origin alone, with no proxy and no redirect. The clock and GitHub are injected, so tests run every state against a fake GitHub with no network and no wall clock.
2. **The sign-in lands through the engine's one credential writer.** On approval, pocketd reads the account's login from GitHub, exchanges the GitHub token for Copilot's as PiG does (the provider's OAuth refresh), and writes the sign-in with `Modify` on the engine's own `AuthStorage`, the one PiG refreshes through (`Engine.KeepSignIn`). It opens no second store of `auth.json`, so a sign-in, a sign-out and a refresh take the same locks in turn. `keptCredentials` and `loginKey` are unchanged: the first still wraps that one store, for the models' reads and PiG's refresh, and the second still refuses a key while pocketd serves.
3. **The models follow at once.** The engine reads the store at each call, so `GET /v1/models` lists the Copilot models the account offers from the approval on. A `signin` event tells clients, and the desktop fetches the models again when a provider's state changes, so New Session offers them with no restart.
4. **A sign-out goes the same way.** `DELETE /v1/signins/{id}` cancels the provider's waiting flow and deletes the sign-in from the same store, ordered with any approved flow's keep (`Flows.SignOut`). A session on a Copilot model keeps its model; its next model call fails for want of a sign-in, and its run ends as a provider's failure does.
5. **What a client sees is code.** `GET /v1/signins` lists the subscriptions pocketd signs in to (`signins.Providers`), Copilot first, each `signed_in`, `expired` (an access token past its expiry with no refresh token) or `none` (`signins.State`), and the provider's newest flow. The account shows only beside the credential it was kept with: pocketd keeps the login in the work database with a digest of that credential (`signin_accounts`, migration 24), never the credential. No answer and no event carries a token or the device code.
6. **One flow a provider, cancelable, gone with pocketd.** A start while a flow waits answers it. `POST /v1/signins/flows/{id}/cancel` ends a waiting flow; `GET /v1/signins/flows/{id}` reads one. A flow lives in memory: a restart ends it, and the owner starts another.
7. **The desktop's Sign-ins page and sheet.** Settings has a Sign-ins page: one row a provider, its state and account, Sign In… or Sign Out for Copilot, and for the others the `pocketd login` command that signs them in. The sheet shows the code, large, with Copy; Open github.com/login/device, a page the desktop builds (`state.DevicePage`), and the page as text for another computer; a spinner, "Waiting for approval…" and the time left; then "Signed in as @user", or why it failed, with Try Again. Cancel at any step cancels the flow on pocketd. Sign Out asks first. A compact window's More lists the same rows, and opens the same sheet. The rules are `desktop/internal/state`'s; the sheet is drawn with MyGo's modal, as the GitHub App's sign-in is.
8. **Copilot alone.** `anthropic` and `openai-codex` sign in with a browser redirect and a pasted code; the endpoint takes a provider, so a later decision can add them. API-key providers stay out: pocketd still stores a key only while it does not serve.

## Consequences

- **No shell for Copilot.** The owner signs pocketd in to Copilot, and out, from the desktop; its models show in New Session at once.
- **One writer, the engine.** A desktop sign-in never writes `auth.json` beside the engine. `pocketd login` for a subscription still writes it through PiG's own store, under PiG's file lock, as before; this decision adds no refusal there.
- **Not quite PiG's sign-in.** PiG's own Copilot sign-in also enables, best effort, each model whose account policy is unconfigured. The exchange alone does not: such a model lists, and its first call may be refused until the owner enables it on GitHub. A Copilot account used before has its policies set.
- **pocketd holds a GitHub token briefly.** The token lives in pocketd's memory from the poll that gets it to the store that keeps it; errors are scrubbed of it.
- **A restart drops a flow.** A code that waits when pocketd stops is lost, and the owner starts another.
- **A revoked sign-in shows signed in.** pocketd cannot tell a refresh token GitHub revoked from a good one until it uses it.
- **A protocol bump:** version 32. The desktop must be rebuilt.
- **frostui gains no sheet yet.** The sheet is the app's own modal; a reusable sheet for ChairLift, Frostmail and Frostnotes belongs to frostyard/frostui.

## Alternatives considered

- **Run PiG's `LoginGitHubCopilot` in the engine:** it writes through the same store and enables policies, but it polls on PiG's own sleep, so no test can run a `slow_down` or an expiry without the wall clock, and its device code and cancel stay out of reach.
- **Run `pocketd login` from the desktop's terminal:** a second process writes `auth.json` beside the serving engine, which ADR-0005 avoids, and the desktop could not say when the models are there.
- **Let the desktop run the flow and send pocketd the token:** a GitHub token would cross the network and sit in the desktop; pocketd keeps every credential.
- **Keep the account in `auth.json`:** PiG's refresh writes Copilot's credential whole and would drop it.
- **Persist a running flow:** the code expires in 15 minutes, and a restart is rarer than starting another.

## References

- Shapes: [specs: protocol rules 1, 5 and 69](../specs/protocol.md#rules), [design: system architecture](../design/architecture.md#operational-notes), [design: desktop client](../design/architecture.md#desktop-client), [plans: roadmap](../plans/roadmap.md) (item 34)
- Set up by: [design: setup guide](../design/setup.md)
- Builds on: [ADR-0004](0004-build-the-service-on-pigs-durable-harness.md), [ADR-0005](0005-keep-the-agent-loop-and-model-credentials-in-the-service.md), [ADR-0008](0008-keep-work-records-in-the-services-own-tables.md), [ADR-0011](0011-put-rules-in-code-not-in-model-judgment.md), [ADR-0018](0018-approve-and-merge-as-the-owner-from-the-desktop-with-pockets-gates.md), [ADR-0021](0021-give-a-threads-specialist-a-worker-on-an-api-key-model-in-code.md), [ADR-0022](0022-run-any-session-on-any-sign-in-and-choose-workers-from-the-owners-list.md), [ADR-0049](0049-notify-the-owner-of-what-waits-once-decided-in-code.md)
- Issue: [frostyard/pocketgo#73](https://github.com/frostyard/pocketgo/issues/73)
