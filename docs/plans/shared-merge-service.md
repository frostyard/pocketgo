# Shared owner merge service

Proposed for PocketGo’s desktop and browser clients. A single owner-operated process holds the GitHub App user credential, evaluates repository rules, and performs confirmed approvals and merges. Other Frostyard applications can use the same versioned HTTP API. The shared UI library can render a preview and confirmation using callbacks. It has no dependency on GitHub, PocketGo, or a running service.

## Service boundary

Run one instance per owner on a trusted host, under a separate OS identity from pocketd and the agents. Serve its own confirmation page and API over HTTPS on the owner’s tailnet. Store the GitHub App access and refresh token in a service-owned credential store with one writer. Use the existing owner identity and installation checks. GitHub attributes a user-token action to the user and limits its permissions to those shared by the user and the app. [GitHub App user authorization](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-user-access-token-for-a-github-app)

Client registration identifies the app, its exact origins or native redirect URI, its allowed repositories and its policy. Authenticate the owner to the service, independently of app registration or tailnet membership. A web sign-in uses a short-lived, secure, HTTP-only session cookie on the service origin. Bind state-changing requests to that origin and the owner session with CSRF protection. Native clients use the system browser for owner authorization and a revocable, audience-bound client token for reads. A pocketd device token continues to authorize only pocketd operations.

**For PocketGo**, register the specific pocketd instance as the source of pull records and cross-reviews. The service itself obtains the current verdict and checks for the exact head and base. Provision a narrowly scoped pull-read credential before enabling the adapter. Today pocketd device tokens also authorize other owner operations, including remote shells. Adding the read grant is a protocol change with corresponding server and client tests. Repository registrations bind the required policies across clients, so switching apps cannot bypass PocketGo’s required review for a managed pull.

## HTTP contract

| Request | Result |
| --- | --- |
| `POST /v1/previews` with repository, pull number and registered source record | An opaque, expiring preview ID and the actual repository, head SHA, base SHA and branch, merge method or queue, and reasons each gate passed or failed. The service resolves the source from registration. |
| `GET /review/{preview}` | The service’s own authenticated confirmation page, with the exact diff and the identity that will approve. Opening the page performs no mutation. An explicit diff acknowledgment is bound to that preview where policy requires one. |
| `POST /v1/operations` from the confirmation page with preview ID and request ID | Recheck fresh PocketGo evidence and GitHub rules, then approve and merge or enqueue under the existing gates. A changed head, base, method, policy or credential requires a fresh preview. |
| `GET /v1/operations/{id}` | Owner-authorized progress and the durable result, including a partial approval, an enqueue, a refusal or an uncertain result. Either UI can reconnect and display it. |

Persist the owner, client, repository, exact decision, request ID and result. Serialize attempts on the same pull. Retrying the same request returns its operation. After a restart, reconcile GitHub’s actual state before retrying a mutation whose response was lost. Redact tokens and device-flow codes from logs. The service constructs GitHub URLs from validated repository identities. It accepts no caller-supplied upstream URL or arbitrary GitHub API request. GitHub’s merge endpoint also supports specifying the expected pull request head. [GitHub pull request merge API](https://docs.github.com/en/rest/pulls/pulls#merge-a-pull-request)

For a batch, show a fixed list of previews under one confirmation. Execute each item in order and re-read its own evidence immediately before its turn. Carry over the existing dependency-bot exception and its base-change recheck. A stop request lets the current operation finish. Disconnecting the requesting client revokes pending confirmations. A completed operation remains queryable by its owner.

## Delivery and acceptance

1. Extract the existing GitHub checks and mutation code behind service-side interfaces. Add the read-only PocketGo evidence adapter. Both clients must obtain identical previews for the same fixture pull, and a registered second app must be unable to relax the policy for that pull. [Existing merge gates](../adr/0018-approve-and-merge-as-the-owner-from-the-desktop-with-pockets-gates.md)
2. Deliver owner sign-in, durable confirmations and reconciliation. A changed head or stale cross-review must produce a refusal in both clients. Repeating a confirmation from two devices must cause at most one approval and merge. Test partial approval and restart after an ambiguous response using simulated upstreams. [Batch and cross-review rules](../adr/0031-count-pocketds-ready-cross-review-as-an-approval-and-merge-all-ready-at-once.md)
3. Enable both PocketGo clients against the same service. Run the same interactive tests for preview, diff, approve, stale preview, cancel, sign-out and batch stop on Linux and WASM. Retire the local signer for service-managed repositories after verifying the cutover. Issue a separate service credential when enrolling it and revoke the previous credential when retiring that signer. [Base-change exception](../adr/0038-check-bot-pull-requests-again-in-the-run-once-a-merge-moves-their-base.md)

The current port uses the existing local signer on desktop and offers a GitHub link in the browser. This proposal introduces a separate deployable component and a new owner authorization boundary. [NSGo client decision](../adr/0068-build-a-shared-nsgo-client-beside-the-desktop-client.md)
