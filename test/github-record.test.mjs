// Guards on the recorder of the GitHub answers
// (desktop/internal/github/testdata/record.sh). Each test runs a copy of the
// recorder against a fake gh and a fake jq, with no network.
//   - frostyard/pocketgo#24, Codex review round 9: the recorder takes a base
//     branch whose name holds a slash. It sends the name encoded as a path
//     segment, and writes its files under the encoded name.
//   - Round 11: it reads every page of a base's rules, as Client.Rules does,
//     records them in 1 list, and reads the rulesets of every page.
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { chmodSync, copyFileSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

const repo = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const recorder = join(repo, "desktop", "internal", "github", "testdata", "record.sh");

// fakeGH logs each call's arguments, one line a call, and answers the REST
// reads a base needs: page N of its rules from $PAGES/N.json (a read without
// a page is page 1, as on GitHub; a page with no file fails), a ruleset by
// its id, and the branch.
const fakeGH = `#!/usr/bin/env bash
printf '%s\\n' "$*" >>"$GH_LOG"
case "$2" in
  *rules/branches/*'&page='*) cat "$PAGES/\${2##*&page=}.json" ;;
  *rules/branches/*) cat "$PAGES/1.json" ;;
  *rulesets/*) printf '{"id":%s}\\n' "\${2##*/}" ;;
  *branches/*) echo '{"name":"release/1.x","commit":{"sha":"abc"},"protected":true,"protection":{},"protection_url":"x"}' ;;
  *) echo "unexpected gh call: $*" >&2; exit 2 ;;
esac
`;

// fakeJQ stands in for jq, which the gate's tools do not include (Codex
// review of #24, round 10). It reads 1 JSON value from each file named after
// the filter, or from stdin; -s makes them 1 list. It runs the filters that
// count, join and list the rules, and passes its input through for any other.
const fakeJQ = `#!/usr/bin/env node
const { readFileSync } = require("node:fs");
const args = process.argv.slice(2);
const [filter, ...files] = args.filter((a) => !a.startsWith("-"));
const values = (files.length ? files : [0]).map((f) => JSON.parse(readFileSync(f, "utf8")));
const input = args.includes("-s") ? values : values[0];
const filters = {
  "arrays | length": (v) => (Array.isArray(v) ? [v.length] : []),
  add: (pages) => [pages.flat()],
  "[.[].ruleset_id] | unique | .[]": (rules) => [...new Set(rules.map((r) => r.ruleset_id))].sort((a, b) => a - b),
};
const out = (filters[filter] ?? ((v) => [v]))(input);
for (const v of out) console.log(typeof v === "string" ? v : JSON.stringify(v));
process.exit(args.includes("-e") && out.length === 0 ? 4 : 0);
`;

// record runs a copy of the recorder on frostyard/nsl's base release/1.x,
// whose rules GitHub answers in the given pages. It returns gh's calls and
// the folder the recorder wrote.
function record(t, pages) {
  const dir = mkdtempSync(join(tmpdir(), "pocketgo-record-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const bin = join(dir, "bin");
  const pagesDir = join(dir, "pages");
  const out = join(dir, "testdata");
  for (const d of [bin, pagesDir, out]) mkdirSync(d);
  for (const [name, text] of [["gh", fakeGH], ["jq", fakeJQ]]) {
    writeFileSync(join(bin, name), text);
    chmodSync(join(bin, name), 0o755);
  }
  pages.forEach((page, i) => writeFileSync(join(pagesDir, `${i + 1}.json`), JSON.stringify(page)));
  copyFileSync(recorder, join(out, "record.sh"));
  const log = join(dir, "gh.log");
  execFileSync("bash", [join(out, "record.sh"), "frostyard/nsl", "release/1.x"], {
    env: {
      ...process.env,
      PATH: `${bin}:${dirname(process.execPath)}:${process.env.PATH}`,
      GH_LOG: log,
      PAGES: pagesDir,
    },
    stdio: "pipe",
  });
  return { calls: readFileSync(log, "utf8").trim().split("\n"), written: join(out, "frostyard-nsl") };
}

test("the recorder encodes a base branch name that holds a slash", (t) => {
  const { calls, written } = record(t, [[]]);
  assert.deepEqual(calls, [
    "api repos/frostyard/nsl/rules/branches/release%2F1.x?per_page=100&page=1",
    "api repos/frostyard/nsl/branches/release%2F1.x",
  ]);
  for (const file of ["rules-branches-release%2F1.x.json", "branches-release%2F1.x.json"]) {
    assert.ok(existsSync(join(written, file)), `${file} was not written`);
  }
});

test("the recorder records every page of a base's rules", (t) => {
  // 100 rules fill page 1, so page 2 is read; its 1 rule ends the rules.
  const rule = (id) => ({ type: "deletion", ruleset_id: id });
  const pages = [Array.from({ length: 100 }, () => rule(1)), [rule(2)]];
  const { calls, written } = record(t, pages);
  const rules = JSON.parse(readFileSync(join(written, "rules-branches-release%2F1.x.json"), "utf8"));
  assert.equal(rules.length, 101, "the recorded rules are not every page's");
  assert.deepEqual(rules, pages.flat());
  assert.deepEqual(calls, [
    "api repos/frostyard/nsl/rules/branches/release%2F1.x?per_page=100&page=1",
    "api repos/frostyard/nsl/rules/branches/release%2F1.x?per_page=100&page=2",
    "api repos/frostyard/nsl/rulesets/1",
    "api repos/frostyard/nsl/rulesets/2",
    "api repos/frostyard/nsl/branches/release%2F1.x",
  ]);
  assert.ok(existsSync(join(written, "ruleset-2.json")), "the ruleset of page 2 was not recorded");
});
