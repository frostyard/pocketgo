// Guards for the Makefile's Go gates (frostyard/pocketgo#3, Codex review):
// each test fails on the Makefile as it was before the fix it names.
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
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

// The phone's portability step (ADR-0071): proto and the desktop's UI-free
// packages build for ios/arm64 with CGO off.
test("make portable builds proto and the desktop's UI-free packages for ios/arm64 with CGO off", () => {
  const run = make("-n", "portable");
  assert.equal(run.status, 0, run.stderr);
  assert.match(run.stdout, /for target in ios\/arm64; do/);
  assert.match(run.stdout, /cd proto && CGO_ENABLED=0 GOOS=\$goos GOARCH=\$goarch go build -o \/dev\/null \.\/\.\.\./);
  assert.match(
    run.stdout,
    /cd desktop && CGO_ENABLED=0 GOOS=\$goos GOARCH=\$goarch go build -o \/dev\/null \.\/internal\/state \.\/internal\/config \.\/internal\/github\)/,
  );
});

test("make ci runs the portability step after the cross-build", () => {
  const run = make("-n", "ci");
  assert.equal(run.status, 0, run.stderr);
  const cross = run.stdout.indexOf('echo "==> cross:');
  const portable = run.stdout.indexOf('echo "==> portable:');
  assert.ok(cross >= 0 && portable > cross, run.stdout);
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

test("make fmt-check ignores the NSGo compiler cache", () => {
  const module = tempDir();
  mkdirSync(join(module, ".nsgo"));
  writeFileSync(join(module, ".nsgo", "generated.go"), "package generated\nfunc  F( ) {}\n");
  const run = make("fmt-check", `MODULES=${module}`);
  assert.equal(run.status, 0, run.stdout + run.stderr);
});

test("make build honours an absolute BUILD_DIR", () => {
  const out = join(tempDir(), "out");
  const run = make("-n", "build", `BUILD_DIR=${out}`);
  assert.equal(run.status, 0, run.stderr);
  assert.match(run.stdout, new RegExp(`-o "${out}/pocketd"`));
  assert.match(run.stdout, new RegExp(`"${out}/pocket"`));
});

test("make build resolves a relative BUILD_DIR from the repository root", () => {
  const run = make("-n", "build", "BUILD_DIR=dist");
  assert.equal(run.status, 0, run.stderr);
  assert.match(run.stdout, new RegExp(`-o "${join(repo, "dist")}/pocketd"`));
});

test("Linux selects NSGo for build, run and packaging", () => {
  for (const target of ["build", "run", "app"]) {
    const run = make("-n", target, "HOST_OS=Linux", "HOST_ARCH=x86_64");
    assert.equal(run.status, 0, run.stderr);
    assert.match(run.stdout, /go tool nsgo build .*?-target linux_x64/);
  }
});

// The NSGo client is the only client (ADR-0071): on another host make run
// and make app build nothing and say where its build is.
test("another host's make run and make app say the client builds on Linux alone", () => {
  for (const target of ["run", "app"]) {
    const run = make(target, "HOST_OS=Darwin");
    assert.notEqual(run.status, 0, run.stdout);
    assert.match(run.stdout, new RegExp(`make ${target} builds the desktop client on Linux alone`));
    assert.match(run.stdout, /go tool nsgo build -target macos_arm64 in desktop\/cmd\/pocket-nsgo/);
  }
});

test("make check formats before it verifies", () => {
  const run = make("-n", "check");
  assert.equal(run.status, 0, run.stderr);
  const formats = run.stdout.indexOf("gofmt -s -w");
  const verifies = run.stdout.indexOf("go mod tidy -diff");
  assert.ok(formats >= 0 && verifies > formats, run.stdout);
});

// make race gives each package RACE_TIMEOUT instead of go test's 10-minute
// default, which service/internal/api's race run reached on CI with no test
// at fault (frostyard/pocketgo#114). Full local runs keep that larger default;
// CI's quarter-sized shards use a smaller timeout below their own job limit.
function defaultRaceDryRun() {
  // Neither the caller's RACE_TIMEOUT nor outer make overrides reach this
  // dry run (Codex review of #115).
  const env = { ...process.env };
  for (const name of ["RACE_TIMEOUT", "MAKEFLAGS", "MAKEOVERRIDES", "MFLAGS"]) delete env[name];
  const run = spawnSync("make", ["--no-print-directory", "-n", "race", "MODULES=proto"], { cwd: repo, encoding: "utf8", env });
  assert.equal(run.status, 0, run.stderr);
  return run.stdout;
}

function defaultRaceMinutes(output) {
  const flag = /go test -race -short -timeout (\d+)m \.\/\.\.\./.exec(output);
  assert.ok(flag, output);
  return Number(flag[1]);
}

function workflowText() {
  return readFileSync(join(repo, ".github", "workflows", "ci.yml"), "utf8");
}

// Read this workflow's indentation-based surface, not arbitrary YAML. Keep
// jobs and steps separate so a setting in one can never back another.
function workflowJobs(text) {
  const jobs = [];
  let inJobs = false;
  let job;
  let step;
  for (const line of text.split("\n")) {
    if (!line.trim() || line.trimStart().startsWith("#")) continue;
    if (line === "jobs:") { inJobs = true; continue; }
    if (!inJobs) continue;
    if (/^\S/.test(line)) break;
    const name = /^  ([A-Za-z0-9_-]+):\s*$/.exec(line);
    if (name) {
      job = { name: name[1], lines: [], steps: [] };
      jobs.push(job);
      step = undefined;
      continue;
    }
    assert.ok(job, `line outside a job: ${line}`);
    job.lines.push(line);
    if (/^      - /.test(line)) {
      step = [];
      job.steps.push(step);
    } else if (/^    \S/.test(line)) {
      step = undefined;
    }
    if (step) step.push(line);
  }
  assert.ok(jobs.length > 0, "workflow has no jobs");
  return jobs;
}

function field(lines, key, indent) {
  const pattern = new RegExp(`^ {${indent}}${key}:\\s*(.*)$`);
  return lines.map((line) => pattern.exec(line)).find(Boolean)?.[1];
}

function stepField(lines, key) {
  const normalized = lines.map((line) => line.replace(/^      - /, "        "));
  const value = field(normalized, key, 8);
  if (value !== "|") return value;
  const start = normalized.findIndex((line) => line.startsWith(`        ${key}:`));
  const body = [];
  for (const line of normalized.slice(start + 1)) {
    if (!line.startsWith("          ")) break;
    body.push(line.slice(10));
  }
  return body.join("\n");
}

function makeTargets(run = "") {
  return [...run.matchAll(/\bmake\s+([^\n]+)/g)].flatMap((match) =>
    match[1].trim().split(/\s+/).filter((word) => !word.startsWith("-") && !word.includes("=")));
}

function oneJob(jobs, name) {
  const matches = jobs.filter((job) => job.name === name);
  assert.equal(matches.length, 1, `expected exactly one ${name} job`);
  return matches[0];
}

function inlineList(value) {
  assert.match(value ?? "", /^\[.*\]$/);
  return value.slice(1, -1).split(",").map((item) => item.trim());
}

// Mutation checks use copies only: the checked-in workflow is never changed.
function rejectsMutation(t, text, before, after, check) {
  assert.ok(text.includes(before), `missing mutation target: ${before}`);
  assert.throws(() => check(text.replace(before, after)), { name: "AssertionError" });
  t.diagnostic(`guard rejected: ${before} -> ${after}`);
}

test("make race's default timeout is above go test's default", (t) => {
  const check = (output) => assert.ok(defaultRaceMinutes(output) > 10, "full local race timeout must exceed 10m");
  const output = defaultRaceDryRun();
  check(output);
  rejectsMutation(t, output, `-timeout ${defaultRaceMinutes(output)}m ./...`, "-timeout 10m ./...", check);
});

test("each race shard's timeout leaves its job time for the rest", (t) => {
  const defaultMinutes = defaultRaceMinutes(defaultRaceDryRun());
  const check = (text) => {
    const race = oneJob(workflowJobs(text), "race");
    const run = race.steps.map((step) => stepField(step, "run")).find((run) => makeTargets(run).includes("race"));
    const timeout = /\bRACE_TIMEOUT=(\d+)m\b/.exec(run ?? "");
    assert.ok(timeout, "race step must pass its own timeout");
    const minutes = Number(timeout[1]);
    const jobLimit = field(race.lines, "timeout-minutes", 4);
    assert.match(jobLimit ?? "", /^\d+$/);
    assert.ok(minutes + 5 <= Number(jobLimit), "race timeout must leave five minutes");
    assert.ok(minutes < defaultMinutes, "quarter-sized shards need a smaller timeout than full local runs");
  };
  const text = workflowText();
  check(text);
  // A timeout 1 minute too long for the job's limit, and none at all, fail.
  const current = /\bRACE_TIMEOUT=\d+m\b/.exec(text)[0];
  const jobLimit = Number(field(oneJob(workflowJobs(text), "race").lines, "timeout-minutes", 4));
  rejectsMutation(t, text, current, `RACE_TIMEOUT=${jobLimit - 4}m`, check);
  rejectsMutation(t, text, current, "", check);
});

test("every Go cache of CI is keyed on files that exist before any go.sum", (t) => {
  const check = (text) => {
    for (const job of workflowJobs(text)) {
      const caches = job.steps.filter((step) => stepField(step, "uses")?.startsWith("actions/cache@"));
      if (job.steps.some((step) => makeTargets(stepField(step, "run")).length > 0)) {
        assert.ok(caches.length > 0, `${job.name} needs its own cache`);
      }
      for (const step of caches) {
        const key = field(step, "key", 10);
        const hashes = /hashFiles\(([^)]*)\)/.exec(key ?? "");
        assert.ok(hashes, `${job.name} cache key needs hashFiles`);
        assert.ok(hashes[1].split(",").map((word) => word.trim()).includes("'go.work'"));
        assert.ok(hashes[1].split(",").map((word) => word.trim()).includes("'**/go.mod'"));
      }
      for (const step of job.steps.filter((step) => stepField(step, "uses")?.startsWith("actions/setup-go@"))) {
        assert.equal(field(step, "cache", 10), "false", `${job.name} setup-go must not write a second cache`);
      }
    }
  };
  const text = workflowText();
  check(text);
  rejectsMutation(t, text, "hashFiles('go.work', '**/go.mod', '**/go.sum')", "hashFiles('**/go.sum')", check);
  rejectsMutation(t, text, "cache: false", "cache: true", check);
  rejectsMutation(t, text, "actions/cache@", "actions/not-cache@", check);
});

test("CI runs every prerequisite of make ci", (t) => {
  const check = (text) => {
    const jobs = workflowJobs(text);
    const targets = jobs.flatMap((job) => job.steps.flatMap((step) => makeTargets(stepField(step, "run"))));
    for (const target of makeRule("ci").prerequisites) assert.ok(targets.includes(target), `CI does not run make ${target}`);
    const race = oneJob(jobs, "race");
    const step = race.steps.find((step) => makeTargets(stepField(step, "run")).includes("race"));
    const run = stepField(step, "run");
    const shards = /\bRACE_SHARDS=([1-9][0-9]*)\b/.exec(run);
    assert.ok(shards, "race step must name its shard count");
    const matrix = field(race.lines, "shard", 8);
    const actual = inlineList(matrix).map(Number);
    assert.deepEqual(actual, Array.from({ length: Number(shards[1]) }, (_, index) => index + 1));
    assert.match(run, /\bRACE_SHARD="\$SHARD"(?:\s|$)/);
    assert.equal(field(step, "SHARD", 10), "${{ matrix.shard }}");
  };
  const text = workflowText();
  check(text);
  rejectsMutation(t, text, "make cross portable", "make cross", check);
  rejectsMutation(t, text, "shard: [1, 2, 3, 4]", "shard: [1, 2, 3]", check);
  rejectsMutation(t, text, 'RACE_SHARD="$SHARD"', "RACE_SHARD=1", check);
  rejectsMutation(t, text, "SHARD: ${{ matrix.shard }}", "SHARD: 1", check);
});

// The ruleset requires verify by name. A skipped required check counts as
// passed, so verify runs always and checks every other job's results itself.
test("the required check verify passes only when every other job succeeded", (t) => {
  const check = (text) => {
    const jobs = workflowJobs(text);
    const verify = oneJob(jobs, "verify");
    const needs = inlineList(field(verify.lines, "needs", 4));
    assert.deepEqual([...needs].sort(), jobs.filter((job) => job !== verify).map((job) => job.name).sort());
    assert.equal(field(verify.lines, "if", 4), "${{ always() }}");
    assert.ok(verify.steps.some((step) => {
      const run = stepField(step, "run") ?? "";
      return run.includes(`jq -e 'length == ${needs.length} and all(.[]; .result == "success")'`);
    }), "verify must require the right result count and success for every result");
  };
  const text = workflowText();
  check(text);
  rejectsMutation(t, text, "needs: [static, test, race]", "needs: [static, test]", check);
  rejectsMutation(t, text, "if: ${{ always() }}", "if: ${{ success() }}", check);
  rejectsMutation(t, text, "length == 3", "length == 2", check);
  rejectsMutation(t, text, 'all(.[]; .result == "success")', 'all(.[]; .result != "failure")', check);
  rejectsMutation(t, text, "  verify:", "  not-verify:", check);
  rejectsMutation(t, text, "  verify:", "  verify:\n  verify:", check);
});

test("binaries built in a command directory are ignored", () => {
  const ignored = [
    "service/cmd/pocketd/pocketd",
    "desktop/cmd/pocket-nsgo/pocket-nsgo",
    "desktop/cmd/pocket-nsgo/pocket-nsgo.exe",
    "service/pocketd",
    "desktop/pocket.exe",
    ".claude/worktrees/agent-x/README.md",
    "desktop/build/mygo/linux-amd64/pocketgo",
    "desktop/.mygo/bundle",
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
  const run = make("-n", "deploy", "DEPLOY_HOST=", "DEPLOY_INCUS=minideb:team", "DEPLOY_INCUS_PROJECT=team", "DEPLOY_USER=team");
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

// deploySh runs the deploy script with only the given environment, so a
// test never depends on what the caller exported.
function deploySh(env, ...args) {
  return spawnSync("sh", [join(repo, "deploy", "deploy.sh"), ...args], {
    encoding: "utf8",
    env: { PATH: process.env.PATH, HOME: process.env.HOME, ...env },
  });
}

test("the deploy script refuses an ssh host and an incus target together", () => {
  const run = deploySh({ DEPLOY_INCUS: "minideb:team", DEPLOY_USER: "team" }, "team@pocket");
  assert.notEqual(run.status, 0);
  assert.match(run.stderr, /either an ssh host or DEPLOY_INCUS, not both/);
});

test("the deploy script needs a user for incus and a host for ssh", () => {
  const incus = deploySh({ DEPLOY_INCUS: "minideb:team" });
  assert.notEqual(incus.status, 0);
  assert.match(incus.stderr, /DEPLOY_USER names the account/);
  const ssh = deploySh({});
  assert.notEqual(ssh.status, 0);
  assert.match(ssh.stderr, /usage: deploy\/deploy\.sh USER@HOST/);
});

// Exercise the shell recipe rather than merely matching its dry-run text.
function raceShardFixture({ failListing = false } = {}) {
  const dir = tempDir();
  const go = join(dir, "go");
  const log = join(dir, "calls");
  writeFileSync(log, "");
  writeFileSync(go, `#!/bin/sh
printf '%s' "$PWD" >> "$RACE_SHARD_LOG"
for arg do printf '\\t%s' "$arg" >> "$RACE_SHARD_LOG"; done
printf '\\n' >> "$RACE_SHARD_LOG"
for arg do
  if [ "$arg" = -list ]; then
    cat <<'LIST'
TestB
TestA
ExampleC
FuzzD
BenchmarkE
TestA
TestÜber
TestMain prints a line, not a name
ok  \tpkg\t0.1s
?   \tpkg2\t[no test files]
LIST
    exit ${failListing ? 1 : 0}
  fi
done
`, { mode: 0o700 });
  const env = { ...process.env, RACE_SHARD_LOG: log };
  for (const name of ["RACE_TIMEOUT", "RACE_SHARDS", "RACE_SHARD", "MAKEFLAGS", "MAKEOVERRIDES", "MFLAGS"]) delete env[name];
  return {
    run(...args) {
      return spawnSync("make", ["--no-print-directory", "race", `GOCMD=${go}`, "MODULES=proto", ...args], {
        cwd: repo, encoding: "utf8", env,
      });
    },
    calls() {
      return readFileSync(log, "utf8").trim().split("\n").filter(Boolean).map((line) => {
        const [cwd, ...args] = line.split("\t");
        return { cwd, args };
      });
    },
  };
}

function shardNames(call, timeout = "20m") {
  const args = call.args;
  assert.equal(args[0], "test");
  assert.ok(args.includes("-race"));
  assert.ok(args.includes("-short"));
  assert.equal(args[args.indexOf("-timeout") + 1], timeout);
  assert.equal(args.at(-1), "./...");
  const pattern = args[args.indexOf("-run") + 1];
  assert.match(pattern, /^\^\([^\s()^$]+\)\$$/);
  return pattern.slice(2, -2).split("|");
}

// A Go name may hold Unicode letters, so TestÜber must run too.
const raceNames = ["ExampleC", "FuzzD", "TestA", "TestB", "TestÜber"];

test("make race runs each test, example and fuzz target in exactly one shard", () => {
  const names = [];
  for (let shard = 1; shard <= 3; shard++) {
    const fixture = raceShardFixture();
    const run = fixture.run("RACE_SHARDS=3", `RACE_SHARD=${shard}`);
    assert.equal(run.status, 0, run.stdout + run.stderr);
    const calls = fixture.calls();
    const listing = calls.filter((call) => call.args.includes("-list"));
    assert.equal(listing.length, 1);
    assert.deepEqual(listing[0].args, ["test", "-race", "-short", "-list", ".", "./..."]);
    const runs = calls.filter((call) => !call.args.includes("-list"));
    assert.equal(runs.length, 1);
    const selected = shardNames(runs[0]);
    assert.deepEqual(selected, raceNames.filter((_, index) => index % 3 === shard - 1));
    names.push(...selected);
  }
  assert.deepEqual(names.sort(), raceNames);
});

function assertPlainRace(fixture, ...args) {
  const run = fixture.run("MODULES=proto service", ...args);
  assert.equal(run.status, 0, run.stdout + run.stderr);
  const calls = fixture.calls();
  assert.deepEqual(calls.map((call) => call.cwd), [join(repo, "proto"), join(repo, "service")]);
  for (const call of calls) {
    assert.deepEqual(call.args, ["test", "-race", "-short", "-timeout", "20m", "./..."]);
  }
}

test("make race with 1 shard runs every name without listing", () => {
  assertPlainRace(raceShardFixture(), "RACE_SHARDS=1", "RACE_SHARD=1");
});

test("make race defaults to 1 shard without listing", () => {
  assertPlainRace(raceShardFixture());
});

test("make race skips a shard with no tests", () => {
  const fixture = raceShardFixture();
  const run = fixture.run("RACE_SHARDS=6", "RACE_SHARD=6");
  assert.equal(run.status, 0, run.stdout + run.stderr);
  assert.match(run.stdout, /==> race: proto has no tests in this shard/);
  assert.equal(fixture.calls().length, 1);
  assert.ok(fixture.calls()[0].args.includes("-list"));
});

test("make race refuses a shard outside 1..RACE_SHARDS", () => {
  for (const [shard, shards] of [["0", "3"], ["4", "3"], ["x", "3"], ["1", "0"], ["01", "3"],
    ["", "3"], ["-1", "3"], ["2.5", "3"], ["1", ""], ["1", "x"], ["1", "-1"], ["1", "2.5"], ["1", "03"], ["10", "3"]]) {
    const fixture = raceShardFixture();
    const run = fixture.run(`RACE_SHARDS=${shards}`, `RACE_SHARD=${shard}`);
    assert.notEqual(run.status, 0, `accepted ${shard}/${shards}`);
    assert.ok(run.stdout.includes(`race: RACE_SHARD=${shard} RACE_SHARDS=${shards} is not a shard (1 <= RACE_SHARD <= RACE_SHARDS)`), run.stdout);
    assert.deepEqual(fixture.calls(), []);
  }
});

test("make race fails when the listing fails", () => {
  const fixture = raceShardFixture({ failListing: true });
  const run = fixture.run("RACE_SHARDS=2", "RACE_SHARD=1");
  assert.notEqual(run.status, 0);
  assert.match(run.stdout, /TestB/);
  assert.equal(fixture.calls().length, 1);
  assert.ok(fixture.calls()[0].args.includes("-list"));
});

test("make race runs the shard in every module", () => {
  const fixture = raceShardFixture();
  const run = fixture.run("MODULES=proto service", "RACE_SHARDS=2", "RACE_SHARD=1");
  assert.equal(run.status, 0, run.stdout + run.stderr);
  const runs = fixture.calls().filter((call) => !call.args.includes("-list"));
  assert.deepEqual(runs.map((call) => call.cwd), [join(repo, "proto"), join(repo, "service")]);
  for (const call of runs) assert.deepEqual(shardNames(call), raceNames.filter((_, index) => index % 2 === 0));
});

test("make race uses make race's timeout", () => {
  const fixture = raceShardFixture();
  const run = fixture.run("RACE_TIMEOUT=7m", "RACE_SHARDS=2", "RACE_SHARD=1");
  assert.equal(run.status, 0, run.stdout + run.stderr);
  const runs = fixture.calls().filter((call) => !call.args.includes("-list"));
  assert.equal(runs.length, 1);
  assert.deepEqual(shardNames(runs[0], "7m"), raceNames.filter((_, index) => index % 2 === 0));
});

function makeRule(name) {
  const lines = readFileSync(join(repo, "Makefile"), "utf8").split("\n");
  const index = lines.findIndex((line) => line.startsWith(`${name}:`));
  assert.ok(index >= 0, `missing ${name} rule`);
  const prerequisites = lines[index].slice(name.length + 1).trim().split(/\s+/).filter(Boolean);
  const recipe = [];
  for (const line of lines.slice(index + 1)) {
    if (!line.trim() || line.startsWith("#")) continue;
    if (!line.startsWith("\t")) break;
    recipe.push(line);
  }
  return { prerequisites, recipe };
}

test("make ci runs verify's parts", () => {
  const verify = makeRule("verify");
  const called = verify.recipe.map((line) => /^\t\$\(MAKE\) --no-print-directory (\S+)$/.exec(line)).filter(Boolean).map((match) => match[1]);
  assert.ok(verify.prerequisites.length > 0);
  assert.ok(called.length > 0, "verify's recipe must call its test target");
  const ci = makeRule("ci");
  assert.deepEqual(ci.prerequisites.slice(0, verify.prerequisites.length + called.length), [...verify.prerequisites, ...called]);
  assert.deepEqual(ci.recipe, [], "ci has no recipe: CI jobs cover prerequisites only");
});

test("make ci runs each step once, in fail-fast order", () => {
  const env = { ...process.env };
  for (const name of ["MAKEFLAGS", "MAKEOVERRIDES", "MFLAGS"]) delete env[name];
  const run = spawnSync("make", ["--no-print-directory", "-n", "ci", "MODULES=proto", "BASE="], { cwd: repo, encoding: "utf8", env });
  assert.equal(run.status, 0, run.stdout + run.stderr);
  const commands = [
    /go mod tidy -diff/g,
    /go test \.\/\.\.\./g,
    /go test -race -short -timeout \S+ \.\/\.\.\./g,
    /GOOS=\$goos GOARCH=\$goarch go build -o \/dev\/null \.\/\.\.\. &&/g,
    /CGO_ENABLED=0 GOOS=\$goos GOARCH=\$goarch go build -o \/dev\/null \.\/\.\.\./g,
  ];
  let previous = -1;
  for (const command of commands) {
    const matches = [...run.stdout.matchAll(command)];
    assert.equal(matches.length, 1, `${command} must run once:\n${run.stdout}`);
    assert.ok(matches[0].index > previous, `${command} is out of order:\n${run.stdout}`);
    previous = matches[0].index;
  }
  assert.deepEqual(makeRule("ci").prerequisites, ["verify-static", "test", "corrections-base", "race", "cross", "portable"]);
});
