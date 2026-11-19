#!/bin/sh
# The staging name is chosen here and expanded on purpose before ssh runs.
# shellcheck disable=SC2029
# deploy/deploy.sh USER@HOST: build pocketd for the host's architecture and
# install it there as a systemd user service of USER (deploy/systemd/
# pocketd.service). Needs ssh access as USER, and lingering for USER
# (loginctl enable-linger USER) so the service runs with nobody logged in.
# Re-running it upgrades in place: pocketd stops on SIGTERM and its running
# tasks resume at the next start. GOCMD picks the go command (default go).
#
# The unit is Type=notify: pocketd reports ready only after its start order
# completes, so a failed start fails the restart, and this script with it.
set -eu

host=${1:?usage: deploy/deploy.sh USER@HOST}
gocmd=${GOCMD:-go}
root=$(cd "$(dirname "$0")/.." && pwd)

case "$(ssh "$host" uname -m)" in
x86_64) goarch=amd64 ;;
aarch64 | arm64) goarch=arm64 ;;
*) echo "deploy: unsupported architecture" >&2; exit 1 ;;
esac

out=$(mktemp -d)
cleanup() { rm -f "$out/pocketd"; rmdir "$out"; }
trap cleanup EXIT
(cd "$root/service" && CGO_ENABLED=0 GOOS=linux GOARCH=$goarch "$gocmd" build -trimpath -o "$out/pocketd" ./cmd/pocketd)

# Each run stages under its own name; the install runs under a lock, so two
# deploys to one host never swap a half-written file into place.
stage=".pocketd-deploy.$(date +%s).$$"
ssh "$host" "mkdir -p ~/.local/bin ~/.config/systemd/user ~/$stage"
scp -q "$out/pocketd" "$host:$stage/pocketd"
scp -q "$root/deploy/systemd/pocketd.service" "$host:$stage/pocketd.service"
ssh "$host" "set -e
exec 9>~/.pocketd-deploy.lock
flock 9
chmod 0755 ~/$stage/pocketd
mv ~/$stage/pocketd ~/.local/bin/pocketd
mv ~/$stage/pocketd.service ~/.config/systemd/user/pocketd.service
rmdir ~/$stage
systemctl --user daemon-reload
systemctl --user enable pocketd.service >/dev/null
systemctl --user restart pocketd.service
systemctl --user is-active pocketd.service
~/.local/bin/pocketd version"
