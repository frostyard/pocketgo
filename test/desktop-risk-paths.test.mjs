// Guard (frostyard/pocketgo#8, Codex reviews of the desktop: finding 15 of
// the first, 12 of the second): every desktop file that reads, stores,
// sends or gates the device token is an L1 auth risk path, so a change to
// one enforcement point alone never gets the default level.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

const repo = resolve(dirname(fileURLToPath(import.meta.url)), "..");

const authFiles = [
  "desktop/cmd/pocket/main.go",
  "desktop/internal/config/secret.go",
  "desktop/internal/app/app.go",
  "desktop/internal/app/signin.go",
  "desktop/internal/state/store.go",
  "desktop/internal/state/url.go",
  "desktop/internal/state/link.go",
  // The owner's GitHub sign-in (ADR-0018).
  "desktop/internal/github/**",
  "desktop/internal/config/github.go",
  "desktop/internal/config/signin_lock*.go",
  "desktop/cmd/pocket/github.go",
  "desktop/internal/state/merge.go",
];

test("the desktop's token storage, sending and sign-in are L1 auth risk paths", () => {
  const list = JSON.parse(readFileSync(join(repo, ".github", "risk-paths.json"), "utf8"));
  for (const file of authFiles) {
    const entry = list.paths.find((p) => p.glob === file);
    assert.ok(entry, `${file} is not in .github/risk-paths.json`);
    assert.equal(entry.level, "L1", `${file} is ${entry.level}, want L1`);
    assert.match(entry.why, /^auth: /, `${file}'s why does not name its category`);
  }
});

// Guard (frostyard/pocketgo#22, Codex review round 1): the work board's
// Close Thread and its confirmation withdraw a brief not started yet, so
// the file is a destructive-operations risk path read at least by its
// diff. So is the owner's Stop of a shift, which withdraws its queued
// round and ends it, in the view and in the command it sends
// (frostyard/pocketgo#37, Codex review round 2).
test("the work board's Close Thread and Stop Shift are destructive-operations risk paths", () => {
  const list = JSON.parse(readFileSync(join(repo, ".github", "risk-paths.json"), "utf8"));
  for (const file of ["desktop/internal/app/board.go", "desktop/internal/state/shift.go"]) {
    const entry = list.paths.find((p) => p.glob === file);
    assert.ok(entry, `${file} is not in .github/risk-paths.json`);
    assert.ok(["L1", "L2"].includes(entry.level), `${file} is ${entry.level}, want L1 or L2`);
    assert.match(entry.why, /^destructive operations: /, `${file}'s why does not name its category`);
  }
});

// Guard (frostyard/pocketgo#24, Codex review round 8): the check that
// screens the recorded GitHub answers for credentials is a secrets risk
// path, as the answers it screens are, so weakening it alone never gets
// the default level.
test("the GitHub fixtures' credential screen is an L1 secrets risk path", () => {
  const list = JSON.parse(readFileSync(join(repo, ".github", "risk-paths.json"), "utf8"));
  const file = "test/github-testdata.test.mjs";
  const entry = list.paths.find((p) => p.glob === file);
  assert.ok(entry, `${file} is not in .github/risk-paths.json`);
  assert.equal(entry.level, "L1", `${file} is ${entry.level}, want L1`);
  assert.match(entry.why, /^secrets: /, `${file}'s why does not name its category`);
});

// The owner's terminal (ADR-0025): a shell as the service user, outside
// the agents' sandbox, behind the device token alone. Its service, its
// client and what its output may do on the desktop are auth risk paths,
// so a change to one never gets the default level.
test("the owner's terminal is an auth risk path", () => {
  const list = JSON.parse(readFileSync(join(repo, ".github", "risk-paths.json"), "utf8"));
  for (const [file, levels] of [
    ["service/internal/terminal/**", ["L1"]],
    ["service/internal/api/terminals.go", ["L1"]],
    ["desktop/internal/state/terminal.go", ["L1", "L2"]],
    ["desktop/internal/app/terminal*.go", ["L1", "L2"]],
  ]) {
    const entry = list.paths.find((p) => p.glob === file);
    assert.ok(entry, `${file} is not in .github/risk-paths.json`);
    assert.ok(levels.includes(entry.level), `${file} is ${entry.level}, want ${levels.join(" or ")}`);
    assert.match(entry.why, /^auth: /, `${file}'s why does not name its category`);
  }
});
