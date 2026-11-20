// Guard (frostyard/pocketgo#24, Codex review round 9): the recorder of the
// GitHub answers (desktop/internal/github/testdata/record.sh) takes a base
// branch whose name holds a slash. It sends the name encoded as a path
// segment, and writes its files under the encoded name. The test runs a
// copy of the recorder against a fake gh, with no network.
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { chmodSync, copyFileSync, existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

const repo = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const recorder = join(repo, "desktop", "internal", "github", "testdata", "record.sh");

// fakeGH logs each call's arguments, one line a call, and answers the two
// REST reads a base needs: no rules, and the branch.
const fakeGH = `#!/usr/bin/env bash
printf '%s\\n' "$*" >>"$GH_LOG"
case "$*" in
  *rules/branches/*) echo '[]' ;;
  *branches/*) echo '{"name":"release/1.x","commit":{"sha":"abc"},"protected":true,"protection":{},"protection_url":"x"}' ;;
  *) echo "unexpected gh call: $*" >&2; exit 2 ;;
esac
`;

test("the recorder encodes a base branch name that holds a slash", (t) => {
  const dir = mkdtempSync(join(tmpdir(), "pocketgo-record-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const bin = join(dir, "bin");
  const out = join(dir, "testdata");
  execFileSync("mkdir", ["-p", bin, out]);
  writeFileSync(join(bin, "gh"), fakeGH);
  chmodSync(join(bin, "gh"), 0o755);
  copyFileSync(recorder, join(out, "record.sh"));
  const log = join(dir, "gh.log");
  execFileSync("bash", [join(out, "record.sh"), "frostyard/nsl", "release/1.x"], {
    env: { ...process.env, PATH: `${bin}:${process.env.PATH}`, GH_LOG: log },
    stdio: "pipe",
  });
  const calls = readFileSync(log, "utf8").trim().split("\n");
  assert.deepEqual(calls, [
    "api repos/frostyard/nsl/rules/branches/release%2F1.x?per_page=100",
    "api repos/frostyard/nsl/branches/release%2F1.x",
  ]);
  for (const file of ["rules-branches-release%2F1.x.json", "branches-release%2F1.x.json"]) {
    assert.ok(existsSync(join(out, "frostyard-nsl", file)), `${file} was not written`);
  }
});
