// Cross-links in both directions (AGENTS.md, "Cross-linking is mandatory"):
// each ADR that a design doc or a plan links must link that doc back.
import assert from "node:assert/strict";
import { after, test } from "node:test";
import { existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { basename, dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const repo = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const temporary = [];

after(() => {
  for (const directory of temporary) rmSync(directory, { recursive: true, force: true });
});

const ADR_LINK = /\]\(\.\.\/adr\/([^)#\s]+\.md)(?:#[^)\s]*)?\)/g;

// True when text holds a complete Markdown link to ../design/<name>: the
// exact path, an optional fragment, an optional title, then ")".
export function linksDesign(text, name) {
  return linksDoc(text, "design", name);
}

// True when text holds a complete Markdown link to ../<category>/<name>.
export function linksDoc(text, category, name) {
  const path = `../${category}/${name}`.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  return new RegExp(`\\]\\(${path}(?:#[^)\\s]*)?(?:\\s+"[^"]*")?\\)`).test(text);
}

// Returns { pairs, missing }: pairs is the number of design-to-ADR links
// checked, missing lists each "design -> ADR" link with no back-link.
export function designBacklinks(root) {
  return backlinks(root, "design");
}

// Returns { pairs, missing } as designBacklinks does, for the plans: a
// plan's phase links its ADR, and the ADR links the plan back (Codex
// review of frostyard/pocketgo#48, round 1).
export function planBacklinks(root) {
  return backlinks(root, "plans");
}

function backlinks(root, category) {
  const docDir = join(root, "docs", category);
  const adrDir = join(root, "docs", "adr");
  const result = { pairs: 0, missing: [] };
  if (!existsSync(docDir)) return result;
  for (const name of readdirSync(docDir).sort()) {
    if (!name.endsWith(".md") || name === "TEMPLATE.md") continue;
    const text = readFileSync(join(docDir, name), "utf8");
    const adrs = new Set([...text.matchAll(ADR_LINK)].map((m) => m[1]));
    for (const adr of adrs) {
      result.pairs++;
      const adrPath = join(adrDir, adr);
      const back = existsSync(adrPath) && linksDoc(readFileSync(adrPath, "utf8"), category, name);
      if (!back) result.missing.push(`docs/${category}/${name} -> docs/adr/${basename(adr)}`);
    }
  }
  return result;
}

test("every ADR that a design doc links links the design doc back", (t) => {
  const { pairs, missing } = designBacklinks(repo);
  t.diagnostic(`${pairs} design-to-ADR links checked`);
  assert.deepEqual(missing, []);
});

test("every ADR that a plan links links the plan back", (t) => {
  const { pairs, missing } = planBacklinks(repo);
  t.diagnostic(`${pairs} plan-to-ADR links checked`);
  assert.deepEqual(missing, []);
});

test("a plan whose ADR has no back-link is reported", () => {
  const root = mkdtempSync(join(tmpdir(), "agentic-template-backlinks-"));
  temporary.push(root);
  mkdirSync(join(root, "docs", "plans"), { recursive: true });
  mkdirSync(join(root, "docs", "adr"), { recursive: true });
  writeFileSync(join(root, "docs", "plans", "roadmap.md"),
    "1. [ ] **A** ([ADR-0001](../adr/0001-x.md), [ADR-0002](../adr/0002-y.md)).\n", "utf8");
  writeFileSync(join(root, "docs", "plans", "TEMPLATE.md"), "[ADR-NNNN](../adr/NNNN-x.md)\n", "utf8");
  writeFileSync(join(root, "docs", "adr", "0001-x.md"), "- Built by: [roadmap](../plans/roadmap.md#phase-1)\n", "utf8");
  writeFileSync(join(root, "docs", "adr", "0002-y.md"), "- Shapes: [design](../design/roadmap.md)\n", "utf8");
  assert.deepEqual(planBacklinks(root), { pairs: 2, missing: ["docs/plans/roadmap.md -> docs/adr/0002-y.md"] });
});

test("a design doc whose ADR has no back-link is reported", () => {
  const root = mkdtempSync(join(tmpdir(), "agentic-template-backlinks-"));
  temporary.push(root);
  mkdirSync(join(root, "docs", "design"), { recursive: true });
  mkdirSync(join(root, "docs", "adr"), { recursive: true });
  writeFileSync(join(root, "docs", "design", "a.md"),
    "Rationale: [ADR-0001](../adr/0001-x.md), [ADR-0002](../adr/0002-y.md#context).\n", "utf8");
  writeFileSync(join(root, "docs", "adr", "0001-x.md"), "- Shapes: [design: a](../design/a.md)\n", "utf8");
  writeFileSync(join(root, "docs", "adr", "0002-y.md"), "- Shapes: `AGENTS.md`\n", "utf8");
  assert.deepEqual(designBacklinks(root), { pairs: 2, missing: ["docs/design/a.md -> docs/adr/0002-y.md"] });
});

test("the design TEMPLATE.md is not checked", () => {
  const root = mkdtempSync(join(tmpdir(), "agentic-template-backlinks-"));
  temporary.push(root);
  mkdirSync(join(root, "docs", "design"), { recursive: true });
  writeFileSync(join(root, "docs", "design", "TEMPLATE.md"), "[ADR-NNNN](../adr/NNNN-x.md)\n", "utf8");
  assert.deepEqual(designBacklinks(root), { pairs: 0, missing: [] });
});

test("only a complete link to the exact design path counts as a back-link", () => {
  assert.equal(linksDesign("[d](../design/a.md)", "a.md"), true);
  assert.equal(linksDesign("[d](../design/a.md#overview)", "a.md"), true);
  assert.equal(linksDesign('[d](../design/a.md "A")', "a.md"), true);
  assert.equal(linksDesign("[d](../design/a.md-other)", "a.md"), false);
  assert.equal(linksDesign("[d](../design/a.md-other.md)", "a.md"), false);
  assert.equal(linksDesign("[d](../design/a.md", "a.md"), false);
  assert.equal(linksDesign("[d](../design/a.md\n", "a.md"), false);
  assert.equal(linksDesign("[d](../design/aXmd)", "a.md"), false);
  assert.equal(linksDesign("[d](../design/ba.md)", "a.md"), false);
});

test("an ADR with only a longer or an incomplete design link is reported", () => {
  const root = mkdtempSync(join(tmpdir(), "agentic-template-backlinks-"));
  temporary.push(root);
  mkdirSync(join(root, "docs", "design"), { recursive: true });
  mkdirSync(join(root, "docs", "adr"), { recursive: true });
  writeFileSync(join(root, "docs", "design", "a.md"),
    "Rationale: [ADR-0001](../adr/0001-x.md), [ADR-0002](../adr/0002-y.md).\n", "utf8");
  writeFileSync(join(root, "docs", "adr", "0001-x.md"), "- Shapes: [design](../design/a.md-other)\n", "utf8");
  writeFileSync(join(root, "docs", "adr", "0002-y.md"), "- Shapes: [design](../design/a.md\n", "utf8");
  assert.deepEqual(designBacklinks(root).missing,
    ["docs/design/a.md -> docs/adr/0001-x.md", "docs/design/a.md -> docs/adr/0002-y.md"]);
});
