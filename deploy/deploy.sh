#!/bin/sh
# The staging name is chosen here and expanded on purpose before it is sent.
# shellcheck disable=SC2029
#
# deploy/deploy.sh: build pocketd for the target's architecture and install
# it there as a systemd user service (deploy/systemd/pocketd.service).
#
#   deploy/deploy.sh USER@HOST          over ssh, as USER
#   DEPLOY_INCUS=REMOTE:INSTANCE [DEPLOY_INCUS_PROJECT=PROJECT] \
#   DEPLOY_USER=USER deploy/deploy.sh   over incus exec, as USER
#
# The user needs lingering (loginctl enable-linger USER) so the service runs
# with nobody logged in. Re-running it upgrades in place: pocketd stops on
# SIGTERM and its running tasks resume at the next start. GOCMD picks the go
# command (default go). The unit is Type=notify: pocketd reports ready only
# after its start order completes, so a failed start fails the restart, and
# this script with it.
set -eu

gocmd=${GOCMD:-go}
root=$(cd "$(dirname "$0")/.." && pwd)

if [ -n "${DEPLOY_INCUS:-}" ]; then
	instance=$DEPLOY_INCUS
	project=${DEPLOY_INCUS_PROJECT:-default}
	user=${DEPLOY_USER:?DEPLOY_USER names the account that runs pocketd}
	uid=$(incus exec "$instance" --project "$project" -- id -u "$user")
	home=$(incus exec "$instance" --project "$project" -- sh -c "getent passwd $user | cut -d: -f6")
	# remote runs a shell command as the user, with the user's systemd.
	remote() {
		incus exec "$instance" --project "$project" --cwd "$home" -- \
			runuser -u "$user" -- env HOME="$home" XDG_RUNTIME_DIR="/run/user/$uid" \
			DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$uid/bus" sh -c "$1"
	}
	# put copies a local file to a path under the user's home.
	put() {
		incus file push --project "$project" --uid "$uid" --gid "$(incus exec "$instance" --project "$project" -- id -g "$user")" \
			--mode 0600 "$1" "$instance$home/$2"
	}
else
	host=${1:?usage: deploy/deploy.sh USER@HOST, or set DEPLOY_INCUS and DEPLOY_USER}
	remote() { ssh "$host" "$1"; }
	put() { scp -q "$1" "$host:$2"; }
fi

case "$(remote 'uname -m')" in
x86_64) goarch=amd64 ;;
aarch64 | arm64) goarch=arm64 ;;
*) echo "deploy: unsupported architecture" >&2; exit 1 ;;
esac

out=$(mktemp -d)
cleanup() { rm -f "$out/pocketd"; rmdir "$out"; }
trap cleanup EXIT
(cd "$root/service" && CGO_ENABLED=0 GOOS=linux GOARCH=$goarch "$gocmd" build -trimpath -o "$out/pocketd" ./cmd/pocketd)

# Each run stages under its own name, and the install runs under a lock
# (mkdir is atomic), so two deploys to one target never swap a half-written
# file into place.
stage=".pocketd-deploy.$(date +%s).$$"
remote "mkdir -p ~/.local/bin ~/.config/systemd/user ~/$stage"
put "$out/pocketd" "$stage/pocketd"
put "$root/deploy/systemd/pocketd.service" "$stage/pocketd.service"
remote "set -e
mkdir ~/.pocketd-deploy.lock 2>/dev/null || { echo 'deploy: another deploy holds ~/.pocketd-deploy.lock' >&2; exit 1; }
trap 'rmdir ~/.pocketd-deploy.lock' EXIT
chmod 0755 ~/$stage/pocketd
chmod 0644 ~/$stage/pocketd.service
mv ~/$stage/pocketd ~/.local/bin/pocketd
mv ~/$stage/pocketd.service ~/.config/systemd/user/pocketd.service
rmdir ~/$stage
systemctl --user daemon-reload
systemctl --user enable pocketd.service >/dev/null
systemctl --user restart pocketd.service
systemctl --user is-active pocketd.service
~/.local/bin/pocketd version"
