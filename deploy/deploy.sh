#!/usr/bin/env bash
# deploy/deploy.sh USER@HOST: build pocketd for the host's architecture and
# install it there as a systemd user service of USER (deploy/systemd/
# pocketd.service). Needs ssh access as USER, and lingering for USER
# (loginctl enable-linger USER) so the service runs with nobody logged in.
# Re-running it upgrades in place: pocketd stops on SIGTERM and its running
# tasks resume at the next start.
set -euo pipefail

host=${1:?usage: deploy/deploy.sh USER@HOST}
root=$(cd "$(dirname "$0")/.." && pwd)

case "$(ssh "$host" uname -m)" in
x86_64) goarch=amd64 ;;
aarch64 | arm64) goarch=arm64 ;;
*) echo "deploy: unsupported architecture" >&2; exit 1 ;;
esac

out=$(mktemp -d)
cleanup() { rm -f "$out/pocketd"; rmdir "$out"; }
trap cleanup EXIT
(cd "$root/service" && CGO_ENABLED=0 GOOS=linux GOARCH=$goarch go build -trimpath -o "$out/pocketd" ./cmd/pocketd)

ssh "$host" 'mkdir -p ~/.local/bin ~/.config/systemd/user'
scp -q "$out/pocketd" "$host:.local/bin/pocketd.new"
scp -q "$root/deploy/systemd/pocketd.service" "$host:.config/systemd/user/pocketd.service"
ssh "$host" 'set -e
chmod 0755 ~/.local/bin/pocketd.new
mv ~/.local/bin/pocketd.new ~/.local/bin/pocketd
systemctl --user daemon-reload
systemctl --user enable pocketd.service >/dev/null
systemctl --user restart pocketd.service
systemctl --user is-active pocketd.service
~/.local/bin/pocketd version'
