// Names and literal repository paths in living docs must match the code.
import assert from "node:assert/strict";
import { after, test } from "node:test";
import { existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const repo = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const temporary = [];
const skippedDirectories = new Set([".git", "node_modules", "build"]);
const repoPath = /^(?:proto|service|desktop|deploy|scripts|test|examples|docs|\.github)\/[A-Za-z0-9_.\/-]+$/;

after(() => {
  for (const dir of temporary) rmSync(dir, { recursive: true, force: true });
});

export function goTestNames(root) {
  const names = new Set();
  function walk(folder) {
    if (!existsSync(folder)) return;
    for (const entry of readdirSync(folder, { withFileTypes: true })) {
      const path = join(folder, entry.name);
      if (entry.isDirectory() && !skippedDirectories.has(entry.name)) walk(path);
      if (!entry.isFile() || !entry.name.endsWith("_test.go")) continue;
      const text = readFileSync(path, "utf8");
      for (const match of text.matchAll(/^func ((?:Test|Benchmark|Fuzz)\w*)\(/gm)) names.add(match[1]);
    }
  }
  for (const module of ["proto", "service", "desktop"]) walk(join(root, module));
  return names;
}

function docs(root, plans) {
  const paths = existsSync(join(root, "AGENTS.md")) ? ["AGENTS.md"] : [];
  for (const category of ["design", "specs", ...(plans ? ["plans"] : [])]) {
    const folder = join(root, "docs", category);
    if (!existsSync(folder)) continue;
    for (const entry of readdirSync(folder, { withFileTypes: true })) {
      if (entry.isFile() && entry.name.endsWith(".md") && entry.name !== "TEMPLATE.md") {
        paths.push(`docs/${category}/${entry.name}`);
      }
    }
  }
  return paths.sort();
}

function codeSpans(root, plans) {
  const spans = [];
  for (const doc of docs(root, plans)) {
    const text = readFileSync(join(root, doc), "utf8");
    for (const match of text.matchAll(/`([^`\n]+)`/g)) spans.push({ doc, text: match[1] });
  }
  return spans;
}

function namedTests(root) {
  return codeSpans(root, true).flatMap(({ doc, text }) =>
    [...text.matchAll(/\b(?:Test|Benchmark|Fuzz)[A-Z]\w*/g)].map(([name]) => ({ doc, name })));
}

function namedPaths(root) {
  return codeSpans(root, false).filter(({ text }) => repoPath.test(text));
}

export function missingTestNames(root) {
  const names = goTestNames(root);
  return [...new Set(namedTests(root).filter(({ name }) => !names.has(name))
    .map(({ doc, name }) => `${doc}: names ${name}, which no Go test declares`))].sort();
}

export function missingRepoPaths(root) {
  return [...new Set(namedPaths(root).filter(({ text }) => !existsSync(join(root, text.replace(/\/$/, ""))))
    .map(({ doc, text }) => `${doc}: names ${text}, which does not exist`))].sort();
}

test("test names in the real docs exist in Go test files", (t) => {
  t.diagnostic(`${namedTests(repo).length} test-name references checked against ${goTestNames(repo).size} declared names`);
  assert.deepEqual(missingTestNames(repo), []);
});

test("literal repository paths in the real docs exist", (t) => {
  t.diagnostic(`${namedPaths(repo).length} repository path references checked`);
  assert.deepEqual(missingRepoPaths(repo), []);
});

function fixture(files = {}, folders = []) {
  const root = mkdtempSync(join(tmpdir(), "pocketgo-doc-names-"));
  temporary.push(root);
  for (const folder of folders) mkdirSync(join(root, folder), { recursive: true });
  for (const [name, text] of Object.entries(files)) {
    mkdirSync(dirname(join(root, name)), { recursive: true });
    writeFileSync(join(root, name), text, "utf8");
  }
  return root;
}

const testA = "package x\n\nfunc TestA(t *testing.T) {}\n";
const missingName = (doc, name) => `${doc}: names ${name}, which no Go test declares`;

test("a declared TestA named in AGENTS.md is valid", () => {
  const root = fixture({ "AGENTS.md": "Run `TestA`.", "service/x/a_test.go": testA });
  assert.deepEqual(goTestNames(root), new Set(["TestA"]));
  assert.deepEqual(missingTestNames(root), []);
});

test("a renamed test in the docs is reported", () => {
  const root = fixture({ "AGENTS.md": "Run `TestB`.", "service/x/a_test.go": testA });
  assert.deepEqual(missingTestNames(root), [missingName("AGENTS.md", "TestB")]);
});

test("a test name outside backticks is ignored", () => {
  assert.deepEqual(missingTestNames(fixture({ "AGENTS.md": "Run TestA." })), []);
});

test("test names in separate and longer spans are all found", () => {
  const root = fixture({ "AGENTS.md": "`api`'s `TestA`; run `TestA and TestB`." });
  assert.deepEqual(missingTestNames(root), [missingName("AGENTS.md", "TestA"), missingName("AGENTS.md", "TestB")]);
});

test("Benchmark and Fuzz declarations count", () => {
  const root = fixture({ "AGENTS.md": "`BenchmarkX` and `FuzzY`",
    "proto/x_test.go": "func BenchmarkX(b *testing.B) {}\nfunc FuzzY(f *testing.F) {}\n" });
  assert.deepEqual(goTestNames(root), new Set(["BenchmarkX", "FuzzY"]));
  assert.deepEqual(missingTestNames(root), []);
});

test("a declaration in a non-test Go file does not count", () => {
  const root = fixture({ "AGENTS.md": "`TestC`", "service/a.go": "func TestC(t *testing.T) {}\n" });
  assert.deepEqual(goTestNames(root), new Set());
  assert.deepEqual(missingTestNames(root), [missingName("AGENTS.md", "TestC")]);
});

for (const category of ["plans", "design", "specs"]) {
  test(`test names in docs/${category} are checked`, () => {
    const doc = `docs/${category}/p.md`;
    assert.deepEqual(missingTestNames(fixture({ [doc]: "`TestA`" })), [missingName(doc, "TestA")]);
  });
}

test("TEMPLATE.md and docs outside the named locations are ignored", () => {
  const root = fixture({ "docs/design/TEMPLATE.md": "`TestA` `service/missing.go`",
    "docs/specs/TEMPLATE.md": "`TestB` `proto/missing.go`", "docs/plans/TEMPLATE.md": "`TestC`",
    "docs/adr/0001-a.md": "`TestD` `desktop/missing.go`", "docs/design/nested/a.md": "`TestE`" });
  assert.deepEqual(missingTestNames(root), []);
  assert.deepEqual(missingRepoPaths(root), []);
});

test("all three module trees are walked recursively", () => {
  const root = fixture({ "proto/x/a_test.go": "func TestProto() {}\n", "service/y/b_test.go": "func TestService() {}\n",
    "desktop/z/c_test.go": "func TestDesktop() {}\n" });
  assert.deepEqual(goTestNames(root), new Set(["TestProto", "TestService", "TestDesktop"]));
});

test("excluded directories are not searched for declarations", () => {
  const files = { "AGENTS.md": "`TestHidden`" };
  for (const folder of [".git", "node_modules", "build"]) files[`service/x/${folder}/a_test.go`] = "func TestHidden() {}\n";
  const root = fixture(files);
  assert.deepEqual(goTestNames(root), new Set());
  assert.deepEqual(missingTestNames(root), [missingName("AGENTS.md", "TestHidden")]);
});

test("only matching declaration lines count", () => {
  const root = fixture({ "service/a_test.go": "// func TestComment() {}\n func TestIndented() {}\nfunc TestSpaced () {}\nfunc ExampleA() {}\nfunc TestActual() {}\n" });
  assert.deepEqual(goTestNames(root), new Set(["TestActual"]));
});

test("missing folders and files give empty results", () => {
  const root = fixture();
  assert.deepEqual(goTestNames(root), new Set());
  assert.deepEqual(missingTestNames(root), []);
  assert.deepEqual(missingRepoPaths(root), []);
});

test("an existing literal file path is valid", () => {
  const root = fixture({ "AGENTS.md": "`service/pig/env.go`", "service/pig/env.go": "package pig\n" });
  assert.deepEqual(missingRepoPaths(root), []);
});

test("a missing literal file path is reported", () => {
  assert.deepEqual(missingRepoPaths(fixture({ "AGENTS.md": "`service/pig/env.go`" })),
    ["AGENTS.md: names service/pig/env.go, which does not exist"]);
});

test("an existing folder with a trailing slash is valid", () => {
  assert.deepEqual(missingRepoPaths(fixture({ "AGENTS.md": "`service/pig/`" }, ["service/pig"])), []);
});

test("non-literal and partial paths are not checked", () => {
  const root = fixture({ "AGENTS.md": "`service/<x>/a.go` `service/*.go` `service/{x}/a.go` `service/…/a.go` `service/a.go and more` `other/a.go`" });
  assert.deepEqual(missingRepoPaths(root), []);
});

test("paths in plans are not checked", () => {
  assert.deepEqual(missingRepoPaths(fixture({ "docs/plans/p.md": "`service/future.go`" })), []);
});

for (const category of ["specs", "design"]) {
  test(`missing paths in docs/${category} are reported`, () => {
    const doc = `docs/${category}/s.md`;
    assert.deepEqual(missingRepoPaths(fixture({ [doc]: "`service/missing.go`" })),
      [`${doc}: names service/missing.go, which does not exist`]);
  });
}

test("problems are sorted and de-duplicated", () => {
  const root = fixture({ "AGENTS.md": "`TestZ TestA TestZ` `service/z.go` `service/a.go` `service/z.go`" });
  assert.deepEqual(missingTestNames(root), [missingName("AGENTS.md", "TestA"), missingName("AGENTS.md", "TestZ")]);
  assert.deepEqual(missingRepoPaths(root), ["AGENTS.md: names service/a.go, which does not exist", "AGENTS.md: names service/z.go, which does not exist"]);
});
