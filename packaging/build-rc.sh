#!/usr/bin/env bash
# Build both clients from the same committed tree, even in a dirty checkout.
set -euo pipefail
root=$(git rev-parse --show-toplevel)
revision=$(git -C "$root" rev-parse HEAD)
id=org.frostyard.PocketGo
version=0.1.0-rc.1
name=pocketgo-$version-${revision:0:12}
out=$(realpath -m "${1:?supply an output directory}")
if [[ $(uname -sm) != 'Linux x86_64' ]]; then
    echo 'The Flatpak RC requires a Linux x86-64 builder.' >&2
    exit 1
fi
git -C "$root" diff --quiet HEAD -- packaging/build-rc.sh || {
    echo 'Commit the release script before building a candidate.' >&2
    exit 1
}
mkdir -p "$out"
stage=$(mktemp -d "$out/.stage.XXXXXXXX")
trap 'rm -rf "$stage"' EXIT
mkdir "$stage/src" "$stage/result"
git -C "$root" archive "$revision" | tar -x -C "$stage/src"

# NSGo stamps the stable application ID and includes the native terminal
# companion. Its release directory is private to this invocation.
# Resolve the selected Go toolchain in the caller’s checkout. This also
# lets NSGo invoke its child Go builds without reselecting a version.
go_root=$("${GOCMD:-go}" env GOROOT)
export PATH="$go_root/bin:$PATH"
cd "$stage/src/desktop/cmd/pocket-nsgo"
export GOWORK="$stage/src/go.work"
"$go_root/bin/go" tool nsgo build -target linux_x64,web -installers=false -channel none -version "$version" -output "$stage/dist"

cd "$stage"
cp src/packaging/flatpak/* .
mv "dist/PocketGo-$version-linux_x64" native
web="$stage/dist/PocketGo-$version-web"
printf '%s\n' "$revision" > result/revision.txt
cp result/revision.txt native/
cp result/revision.txt "$web/"
cp src/LICENSE native/
python3 -m zipfile -c "result/$name-web.zip" "$web"/*

if command -v flatpak-builder >/dev/null; then
    builder=(flatpak-builder)
else
    builder=(flatpak run "--filesystem=$stage" org.flatpak.Builder)
fi
"${builder[@]}" --disable-download --repo=repo app "$id.json"
flatpak build-bundle --runtime-repo=https://flathub.org/repo/flathub.flatpakrepo repo "result/$name-x86_64.flatpak" "$id" rc

(cd result && sha256sum -- *.flatpak *.zip revision.txt > SHA256SUMS)
# Publish a complete local set. Keep concurrently built candidates separate.
if [[ -e "$out/$name" ]]; then
    echo "Candidate already exists at $out/$name. Choose another BUILD_DIR to rebuild." >&2
    exit 1
fi
mv -T result "$out/$name"
printf 'Candidate: %s\nSource: %s\n' "$out/$name" "$revision"
