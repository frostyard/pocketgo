// Guard (frostyard/pocketgo#8, Codex reviews of the desktop: finding 15 of
// the first, 12 of the second): every desktop file that reads, stores,
// sends or gates the device token is an L1 auth risk path, so a change to
// one enforcement point alone never gets the default level.
import assert from "node:assert/strict";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { levelOfFile } from "../scripts/lib/risk-paths.mjs";

const repo = resolve(dirname(fileURLToPath(import.meta.url)), "..");

// The NSGo client is the only client (ADR-0071): its accounts files join
// the keyring to the token storage and sign in with the kept token at start,
// as the MyGo client's command and App.Start did.
const authFiles = [
  "desktop/internal/nsgoclient/accounts*.go",
  "desktop/internal/config/secret.go",
  "desktop/internal/nsgoclient/signin.go",
  "desktop/internal/nsgoclient/transport*.go",
  "desktop/internal/state/store.go",
  "desktop/internal/state/url.go",
  "desktop/internal/state/link.go",
  // The owner's GitHub sign-in (ADR-0018).
  "desktop/internal/github/**",
  "desktop/internal/ownerauth/**",
  "desktop/internal/config/github.go",
  "desktop/internal/config/signin_lock*.go",
  // The GitHub sign-in's hold as iOS suspends the app (ADR-0041), and the
  // rule that installs it (Codex review of #75, round 1).
  "desktop/internal/nsgoclient/compact.go",
  "desktop/internal/state/platform.go",
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
// (frostyard/pocketgo#37, Codex review round 2). The NSGo client's view of
// them is checked below, with its other operator actions.
test("the work board's Close Thread and Stop Shift are destructive-operations risk paths", () => {
  const list = JSON.parse(readFileSync(join(repo, ".github", "risk-paths.json"), "utf8"));
  for (const file of ["desktop/internal/state/shift.go"]) {
    const entry = list.paths.find((p) => p.glob === file);
    assert.ok(entry, `${file} is not in .github/risk-paths.json`);
    assert.ok(["L1", "L2"].includes(entry.level), `${file} is ${entry.level}, want L1 or L2`);
    assert.match(entry.why, /^destructive operations: /, `${file}'s why does not name its category`);
  }
});

// Guard (frostyard/pocketgo#43, Codex review round 1): a standing
// session's Start Over starts the agent's context afresh, in the view
// that confirms it and in the command that sends it, so both files are
// destructive-operations risk paths. The NSGo client's view of it is
// checked below, with its other operator actions.
test("a standing session's Start Over is a destructive-operations risk path", () => {
  const list = JSON.parse(readFileSync(join(repo, ".github", "risk-paths.json"), "utf8"));
  for (const file of ["desktop/internal/state/standing.go"]) {
    const entry = list.paths.find((p) => p.glob === file);
    assert.ok(entry, `${file} is not in .github/risk-paths.json`);
    assert.ok(["L1", "L2"].includes(entry.level), `${file} is ${entry.level}, want L1 or L2`);
    assert.match(entry.why, /^destructive operations: /, `${file}'s why does not name its category`);
  }
});

// The NSGo client's views of the operations above (ADR-0071): a thread's
// close and a shift's stop (board.go), a Start Over (pages.go), the queue
// every confirmation goes through (shell.go), a plan's approval (work.go), a
// drop and a launch (nightshift.go), a provider's sign-out (settings.go) and
// the owner's terminal (terminals.go, terminal_output.go). No change to one
// gets the default level.
test("the NSGo client's operator actions are risk paths", () => {
  const list = JSON.parse(readFileSync(join(repo, ".github", "risk-paths.json"), "utf8"));
  for (const name of ["board.go", "pages.go", "shell.go", "work.go", "nightshift.go", "settings.go", "terminals.go", "terminal_output.go"]) {
    const file = `desktop/internal/nsgoclient/${name}`;
    assert.ok(existsSync(join(repo, file)), `${file} does not exist`);
    const level = levelOfFile(list, file);
    assert.equal(level.source, "glob", `${file} matches no glob of .github/risk-paths.json`);
    assert.ok(["L1", "L2"].includes(level.level), `${file} is ${level.level}, want L1 or L2`);
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
  ]) {
    const entry = list.paths.find((p) => p.glob === file);
    assert.ok(entry, `${file} is not in .github/risk-paths.json`);
    assert.ok(levels.includes(entry.level), `${file} is ${entry.level}, want ${levels.join(" or ")}`);
    assert.match(entry.why, /^auth: /, `${file}'s why does not name its category`);
  }
});
