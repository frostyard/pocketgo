#!/bin/sh
# The staging name is chosen here and expanded on purpose before it is sent.
# shellcheck disable=SC2029
#
# deploy/deploy.sh: build pocketd for the target's architecture and install
# it there as a systemd user service (deploy/systemd/pocketd.service).
#
#   deploy/deploy.sh USER@HOST          over ssh, as USER
#   DEPLOY_INCUS=REMOTE:INSTANCE [DEPLOY_INCUS_PROJECT=PROJECT] \
#   DEPLOY_USER=USER deploy/deploy.sh   over incus exec, as USER (without
#                                       DEPLOY_INCUS_PROJECT, the remote's
#                                       current project)
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

die() { echo "deploy: $*" >&2; exit 1; }

if [ -n "${DEPLOY_INCUS:-}" ]; then
	[ $# -eq 0 ] || die "set either an ssh host or DEPLOY_INCUS, not both"
	instance=$DEPLOY_INCUS
	project=${DEPLOY_INCUS_PROJECT:-}
	user=${DEPLOY_USER:-}
	[ -n "$user" ] || die "DEPLOY_USER names the account that runs pocketd"
	# on runs incus exec on the instance, in the explicit project if one is
	# set, else the remote's current project.
	on() {
		if [ -n "$project" ]; then
			incus exec "$instance" --project "$project" -- "$@"
		else
			incus exec "$instance" -- "$@"
		fi
	}
	uid=$(on id -u "$user")
	home=$(on getent passwd "$user" | cut -d: -f6)
	case "$home" in
	/*) ;;
	*) die "no home folder for $user" ;;
	esac
	# remote runs a shell command as the user, with the user's systemd.
	remote() {
		on runuser -u "$user" -- env HOME="$home" XDG_RUNTIME_DIR="/run/user/$uid" \
			DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$uid/bus" sh -c "cd && $1"
	}
	# put streams a local file to a path under the user's home, written by
	# the user, so the guest's own permissions decide where it may go.
	# shellcheck disable=SC2016 # $1 is the remote shell's argument
	put() { on runuser -u "$user" -- sh -c 'umask 077 && cat > "$1"' sh "$home/$2" <"$1"; }
else
	[ $# -eq 1 ] || die "usage: deploy/deploy.sh USER@HOST, or set DEPLOY_INCUS and DEPLOY_USER"
	host=$1
	remote() { ssh "$host" "$1"; }
	put() { scp -q "$1" "$host:$2"; }
fi

case "$(remote 'uname -m')" in
x86_64) goarch=amd64 ;;
aarch64 | arm64) goarch=arm64 ;;
*) die "unsupported architecture" ;;
esac

out=$(mktemp -d)
cleanup() { rm -f "$out/pocketd"; rmdir "$out"; }
trap cleanup EXIT
(cd "$root/service" && CGO_ENABLED=0 GOOS=linux GOARCH=$goarch "$gocmd" build -trimpath -o "$out/pocketd" ./cmd/pocketd)

# Each run stages under its own name, and the install runs under a lock, so
# two deploys to one target never swap a half-written file into place. The
# lock is a directory (mkdir is atomic; no flock needed) holding the boot id
# and pid of its holder: a lock whose holder is gone (a reboot, a killed
# shell) is stale and taken over.
stage=".pocketd-deploy.$(date +%s).$$"
remote "mkdir -p ~/.local/bin ~/.config/systemd/user ~/$stage"
put "$out/pocketd" "$stage/pocketd"
put "$root/deploy/systemd/pocketd.service" "$stage/pocketd.service"
remote "set -e
lock=~/.pocketd-deploy.lockdir
boot=\$(cat /proc/sys/kernel/random/boot_id)
if ! mkdir \"\$lock\" 2>/dev/null; then
	read -r held_boot held_pid <\"\$lock/owner\" 2>/dev/null || true
	if [ \"\${held_boot:-}\" = \"\$boot\" ] && kill -0 \"\${held_pid:-0}\" 2>/dev/null; then
		echo 'deploy: another deploy is running' >&2
		exit 1
	fi
	rm -f \"\$lock/owner\"
	rmdir \"\$lock\"
	mkdir \"\$lock\"
fi
echo \"\$boot \$\$\" >\"\$lock/owner\"
trap 'rm -f \"\$lock/owner\"; rmdir \"\$lock\"' EXIT
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
