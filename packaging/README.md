# PocketGo release candidates

On Linux x86-64 install Flatpak, flatpak-builder (or the org.flatpak.Builder app), Python 3 and the Go version in go.work. Install the runtime and SDK with `flatpak install flathub org.gnome.Platform//51 org.gnome.Sdk//51`. From the repository root run `make rc`. The script builds a Git archive of HEAD. The output in `build/rc/pocketgo-*/` contains an installable Flatpak, a static web ZIP, their SHA-256 sums and the source revision.

Run `sha256sum -c SHA256SUMS` inside the candidate directory. Install with `flatpak install --user --bundle ./pocketgo-*-x86_64.flatpak`, then launch with `flatpak run org.frostyard.PocketGo//rc`. Read the packaged revision with `flatpak run --command=cat org.frostyard.PocketGo//rc /app/share/pocketgo/revision.txt`. The Flatpak is the client for an existing pocketd. It has access to the host’s Secret Service and the PocketGo sign-in lock directory. The RC uses the production app ID. On first launch, supply your service URL and device token. Client preferences are per installation.

Extract the web ZIP into its own web root and serve it over HTTPS. Route `/v1/` to pocketd on the same origin, forward Authorization and stream responses without buffering. Serve WASM as `application/wasm` and use matching Content-Encoding when serving precompressed assets. The web root’s `revision.txt` identifies the source. Refresh the entry page on an update and stage all its hashed assets before switching releases.

## Frostyard remote

The **PocketGo release candidate** workflow builds and uploads both files in one run. A dispatch on main with `publish_rc` selected also exports that same Flatpak to public GHCR as OCI, attests the image digest, then advances its `rc` tag. To list it, register repository `frostyard/pocketgo`, tag `rc`, and workflow `.github/workflows/flatpak.yml` in [the remote’s apps.json](https://github.com/frostyard/flatpak-index/blob/main/apps.json). After its catalogue refresh, install with `flatpak install frostyard org.frostyard.PocketGo//rc`. Web hosting is independent of the Flatpak index.
