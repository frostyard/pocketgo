// Guards for the Makefile's Go gates (frostyard/pocketgo#3, Codex review):
// each test fails on the Makefile as it was before the fix it names.
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { after, test } from "node:test";
import { fileURLToPath } from "node:url";

const repo = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const temporary = [];
after(() => {
  for (const directory of temporary) rmSync(directory, { recursive: true, force: true });
});

// make in the repository root, without printing directory changes.
function make(...args) {
  return spawnSync("make", ["--no-print-directory", ...args], { cwd: repo, encoding: "utf8" });
}

function tempDir() {
  const directory = mkdtempSync(join(tmpdir(), "pocketgo-makefile-"));
  temporary.push(directory);
  return directory;
}

test("a bare make verifies and does not format", () => {
  const run = make("-n");
  assert.equal(run.status, 0, run.stderr);
  assert.match(run.stdout, /go mod tidy -diff/);
  assert.doesNotMatch(run.stdout, /gofmt -s -w/);
});

test("make cross reads each module's own CROSS_<module> list", () => {
  const run = make("-n", "cross", "MODULES=proto", "CROSS_proto=plan9/amd64");
  assert.equal(run.status, 0, run.stderr);
  assert.match(run.stdout, /plan9\/amd64/);
});

test("make cross fails for a module without a CROSS_<module> list", () => {
  const run = make("-n", "cross", "MODULES=proto extra");
  assert.notEqual(run.status, 0);
  assert.match(run.stderr, /CROSS_extra is not set/);
});

test("make fmt-check fails when gofmt cannot parse a file", () => {
  const module = tempDir();
  writeFileSync(join(module, "broken.go"), "//go:build ignore\n\npackage main\n\nfunc {\n");
  const run = make("fmt-check", `MODULES=${module}`);
  assert.notEqual(run.status, 0, run.stdout);
  assert.match(run.stdout + run.stderr, /gofmt failed/);
});

test("make fmt-check names an unformatted file and fails", () => {
  const module = tempDir();
  writeFileSync(join(module, "ugly.go"), "package ugly\nfunc  F( ) {}\n");
  const run = make("fmt-check", `MODULES=${module}`);
  assert.notEqual(run.status, 0);
  assert.match(run.stdout, /ugly\.go/);
});

test("make fmt-check passes on formatted code", () => {
  const module = tempDir();
  writeFileSync(join(module, "fine.go"), "package fine\n\nfunc F() {}\n");
  const run = make("fmt-check", `MODULES=${module}`);
  assert.equal(run.status, 0, run.stdout + run.stderr);
});

test("make build honours an absolute BUILD_DIR", () => {
  const out = join(tempDir(), "out");
  const run = make("-n", "build", `BUILD_DIR=${out}`);
  assert.equal(run.status, 0, run.stderr);
  assert.match(run.stdout, new RegExp(`-o "${out}/pocketd"`));
  assert.match(run.stdout, new RegExp(`-o "${out}/pocket"`));
});

test("make build resolves a relative BUILD_DIR from the repository root", () => {
  const run = make("-n", "build", "BUILD_DIR=dist");
  assert.equal(run.status, 0, run.stderr);
  assert.match(run.stdout, new RegExp(`-o "${join(repo, "dist")}/pocketd"`));
});

test("make check formats before it verifies", () => {
  const run = make("-n", "check");
  assert.equal(run.status, 0, run.stderr);
  const formats = run.stdout.indexOf("gofmt -s -w");
  const verifies = run.stdout.indexOf("go mod tidy -diff");
  assert.ok(formats >= 0 && verifies > formats, run.stdout);
});

test("the CI Go cache is keyed on files that exist before any go.sum", () => {
  const workflow = readFileSync(join(repo, ".github", "workflows", "ci.yml"), "utf8");
  const key = /cache-dependency-path: \|\n((?: {12}\S.*\n)+)/.exec(workflow);
  assert.ok(key, "cache-dependency-path is not a block list");
  const paths = key[1].split("\n").map((line) => line.trim()).filter(Boolean);
  assert.ok(paths.includes("go.work"), paths.join(", "));
  assert.ok(paths.includes("**/go.mod"), paths.join(", "));
});

test("binaries built in a command directory are ignored", () => {
  const ignored = [
    "service/cmd/pocketd/pocketd",
    "desktop/cmd/pocket/pocket",
    "desktop/cmd/pocket/pocket.exe",
    "service/pocketd",
    "desktop/pocket.exe",
    ".claude/worktrees/agent-x/README.md",
  ];
  const run = spawnSync("git", ["check-ignore", "--no-index", ...ignored], { cwd: repo, encoding: "utf8" });
  assert.equal(run.status, 0, run.stderr);
  assert.deepEqual(run.stdout.trim().split("\n").sort(), [...ignored].sort());
  const source = spawnSync("git", ["check-ignore", "--no-index", "service/cmd/pocketd/main.go"], { cwd: repo });
  assert.equal(source.status, 1, "a command's source must not be ignored");
});


test("make deploy needs a target", () => {
  const run = make("deploy", "DEPLOY_HOST=", "DEPLOY_INCUS=");
  assert.notEqual(run.status, 0);
  assert.match(run.stdout, /usage: make deploy DEPLOY_HOST=USER@HOST, or DEPLOY_INCUS=REMOTE:INSTANCE/);
});

test("make deploy runs the deploy script with the host", () => {
  const run = make("-n", "deploy", "DEPLOY_HOST=team@pocket");
  assert.equal(run.status, 0, run.stderr);
  assert.match(run.stdout, /deploy\/deploy\.sh team@pocket$/m);
});

test("make deploy passes the incus target to the deploy script", () => {
  const run = make("-n", "deploy", "DEPLOY_INCUS=minideb:team", "DEPLOY_INCUS_PROJECT=team", "DEPLOY_USER=team");
  assert.equal(run.status, 0, run.stderr);
  assert.match(run.stdout, /DEPLOY_INCUS="minideb:team" DEPLOY_INCUS_PROJECT="team" DEPLOY_USER="team" deploy\/deploy\.sh $/m);
});

test("make deploy builds with the Makefile's go command", () => {
  const run = make("-n", "deploy", "DEPLOY_HOST=team@pocket", "GOCMD=/opt/go/bin/go");
  assert.equal(run.status, 0, run.stderr);
  assert.match(run.stdout, /GOCMD="\/opt\/go\/bin\/go" .*deploy\/deploy\.sh/);
});

test("the deploy script parses as POSIX sh", () => {
  const run = spawnSync("sh", ["-n", join(repo, "deploy", "deploy.sh")], { encoding: "utf8" });
  assert.equal(run.status, 0, run.stderr);
});
