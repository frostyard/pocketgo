# 0070 — Ship NSGo as the default Linux client

- **Status:** Accepted
- **Date:** 2026-11-23

## Context

The Linux desktop and browser can run the same PocketGo views. The existing desktop client also serves macOS and iOS.

## Decision

The Linux build, run and packaging targets select NSGo under the existing application ID `org.frostyard.PocketGo`. MyGo remains an explicit Linux fallback and keeps its other platforms. Cut an x86-64 Flatpak on the `rc` branch and a static web archive from one committed source tree. Record the full revision inside each artifact. Publishing the Flatpak to the Frostyard remote requires an attested OCI image and registration in flatpak-index.

## Consequences

The two clients can be tested against the same deployed pocketd. The Flatpak includes the terminal runtime and shares the host directory for PocketGo’s exclusive GitHub sign-in lock. Linux users can return to MyGo during the cutover.

## Alternatives considered

Independent builds from dirty working directories make it difficult to identify the code behind a reported difference.

## References

- [Shared NSGo client](0068-build-a-shared-nsgo-client-beside-the-desktop-client.md)
- [Architecture](../design/architecture.md)
- [Setup](../design/setup.md)
