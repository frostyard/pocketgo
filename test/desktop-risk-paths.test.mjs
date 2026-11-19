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
// diff.
test("the work board's Close Thread is a destructive-operations risk path", () => {
  const list = JSON.parse(readFileSync(join(repo, ".github", "risk-paths.json"), "utf8"));
  const file = "desktop/internal/app/board.go";
  const entry = list.paths.find((p) => p.glob === file);
  assert.ok(entry, `${file} is not in .github/risk-paths.json`);
  assert.ok(["L1", "L2"].includes(entry.level), `${file} is ${entry.level}, want L1 or L2`);
  assert.match(entry.why, /^destructive operations: /, `${file}'s why does not name its category`);
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
