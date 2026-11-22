// Keep numbered ADRs, their index and their amendment records in agreement.
import assert from "node:assert/strict";
import { after, test } from "node:test";
import { mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const repo = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const temporary = [];
const adrName = /^\d{4}-[a-z0-9-]+\.md$/;
const statusPrefix = '- **Status:** ';

after(() => {
  for (const dir of temporary) rmSync(dir, { recursive: true, force: true });
});

function records(root) {
  return readdirSync(join(root, "docs", "adr")).filter((name) => adrName.test(name)).sort()
    .map((name) => ({ name, lines: readFileSync(join(root, "docs", "adr", name), "utf8").split(/\r?\n/) }));
}

export function adrIndexProblems(root) {
  const adrs = records(root);
  const index = readFileSync(join(root, "docs", "README.md"), "utf8");
  const problems = [];
  const numbers = new Map();
  for (const { name, lines } of adrs) {
    const number = name.slice(0, 4);
    const names = numbers.get(number) ?? [];
    names.push(name);
    numbers.set(number, names);
    if (!lines[0].startsWith(`# ${number} — `) || lines[0].slice(9).trim() === "") {
      problems.push(`docs/adr/${name}: first line is not "# ${number} — <title>"`);
    }
    const title = lines[0].replace(/^# /, "");
    const links = [...index.matchAll(/\[([^\]\n]+)\]\(adr\/([^)\s]+\.md)\)/g)];
    if (links.filter((link) => link[1] === title && link[2] === name).length !== 1) {
      problems.push(`docs/README.md does not index docs/adr/${name} as "${title}"`);
    }
  }
  for (const [number, names] of numbers) {
    if (names.length > 1) problems.push(`ADR ${number} is used by ${names.join(" and ")}`);
  }
  const highest = Math.max(0, ...[...numbers.keys()].map(Number));
  for (let n = 1; n <= highest; n++) {
    const number = String(n).padStart(4, "0");
    if (!numbers.has(number)) problems.push(`ADR ${number} is missing`);
  }
  const names = new Set(adrs.map(({ name }) => name));
  for (const link of index.matchAll(/\]\(adr\/([^)\s]+\.md)(?:#[^)\s]*)?\)/g)) {
    if (link[1] !== "TEMPLATE.md" && !names.has(link[1])) {
      problems.push(`docs/README.md indexes adr/${link[1]}, which does not exist`);
    }
  }
  return problems;
}

export function adrStatusProblems(root) {
  const problems = [];
  for (const { name, lines } of records(root)) {
    const statuses = lines.filter((line) => line.startsWith(statusPrefix));
    if (statuses.length !== 1) {
      problems.push(`docs/adr/${name}: needs exactly one "${statusPrefix}" line`);
    } else if (!/^(Proposed|Accepted|Superseded by \[)/.test(statuses[0].slice(statusPrefix.length))) {
      problems.push(`docs/adr/${name}: Status must start with Proposed, Accepted or "Superseded by ["`);
    }
  }
  return problems;
}

function amendmentPairs(adrs) {
  const pairs = new Map();
  for (const { name, lines } of adrs) {
    let references = false;
    for (const line of lines) {
      if (line === "## References") {
        references = true;
        continue;
      }
      if (line.startsWith("## ")) references = false;
      if (!references || !line.startsWith("- ")) continue;
      const links = [...line.matchAll(/\[[^\]\n]*\]\((\d{4}-[a-z0-9-]+\.md)(?:#[^)\s]*)?\)/g)];
      const all = /^- (Supersedes|Amends)/.test(line) || /supersedes in part:/i.test(line);
      for (const [i, link] of links.entries()) {
        const after = line.slice(link.index + link[0].length, links[i + 1]?.index ?? line.length);
        if (all || /\bit (amends|supersedes)\b/.test(after)) {
          pairs.set(`${name}:${link[1]}`, [name, link[1]]);
        }
      }
    }
  }
  return [...pairs.values()];
}

export function adrAmendmentProblems(root) {
  const adrs = records(root);
  const byName = new Map(adrs.map((adr) => [adr.name, adr]));
  const problems = [];
  for (const [source, target] of amendmentPairs(adrs)) {
    if (!byName.has(target)) {
      problems.push(`docs/adr/${source}: names ${target}, which does not exist`);
      continue;
    }
    const statuses = byName.get(target).lines.filter((line) => line.startsWith(statusPrefix));
    if (!statuses.some((line) => line.includes(`(${source})`) || line.includes(`(${source}#`))) {
      problems.push(`docs/adr/${target}: Status does not name docs/adr/${source}, which amends or supersedes it`);
    }
  }
  return [...new Set(problems)].sort();
}

for (const [label, check] of [
  ["index", adrIndexProblems], ["status", adrStatusProblems], ["amendment", adrAmendmentProblems],
]) {
  test(`real ADR ${label} records agree`, (t) => {
    const adrs = records(repo);
    t.diagnostic(label === "amendment" ? `${amendmentPairs(adrs).length} amendment pairs checked` : `${adrs.length} ADRs checked`);
    assert.deepEqual(check(repo), []);
  });
}

function document(number, title, status = "Accepted", references = "") {
  return `# ${number} — ${title}\n\n${statusPrefix}${status}\n\n## References\n\n${references}\n`;
}

function fixture(files, index) {
  const root = mkdtempSync(join(tmpdir(), "pocketgo-adr-records-"));
  temporary.push(root);
  mkdirSync(join(root, "docs", "adr"), { recursive: true });
  for (const [name, text] of Object.entries(files)) writeFileSync(join(root, "docs", "adr", name), text, "utf8");
  const automaticIndex = Object.entries(files).filter(([name]) => adrName.test(name))
    .map(([name, text]) => `- [${text.split("\n")[0].replace(/^# /, "")}](adr/${name})`).join("\n");
  writeFileSync(join(root, "docs", "README.md"), index ?? automaticIndex, "utf8");
  return root;
}

const a = document("0001", "A");
const b = document("0002", "B");

test("duplicate ADR numbers are reported in filename order", () => {
  const root = fixture({ "0001-b.md": document("0001", "B"), "0001-a.md": a });
  assert.deepEqual(adrIndexProblems(root), ["ADR 0001 is used by 0001-a.md and 0001-b.md"]);
});

test("a gap in ADR numbers is reported", () => {
  const root = fixture({ "0001-a.md": a, "0003-c.md": document("0003", "C") });
  assert.deepEqual(adrIndexProblems(root), ["ADR 0002 is missing"]);
});

test("a first line with the wrong number is reported", () => {
  const root = fixture({ "0001-a.md": b });
  assert.deepEqual(adrIndexProblems(root), ['docs/adr/0001-a.md: first line is not "# 0001 — <title>"']);
});

test("a first line needs the exact heading format and a title", () => {
  for (const heading of ["0001 — A", "# 0001 - A", "# 0001 — "]) {
    const root = fixture({ "0001-a.md": heading });
    assert.deepEqual(adrIndexProblems(root), ['docs/adr/0001-a.md: first line is not "# 0001 — <title>"']);
  }
});

for (const [label, index] of [
  ["different index title", "- [0001 — Other](adr/0001-a.md)"],
  ["missing index entry", ""],
  ["duplicate index entry", "- [0001 — A](adr/0001-a.md)\n- [0001 — A](adr/0001-a.md)"],
]) {
  test(`an ADR with a ${label} is reported`, () => {
    assert.deepEqual(adrIndexProblems(fixture({ "0001-a.md": a }, index)),
      ['docs/README.md does not index docs/adr/0001-a.md as "0001 — A"']);
  });
}

test("an index link to a missing ADR is reported", () => {
  assert.deepEqual(adrIndexProblems(fixture({}, "- [0001 — A](adr/0001-a.md)")),
    ["docs/README.md indexes adr/0001-a.md, which does not exist"]);
});

test("an unlinked TEMPLATE.md is not an ADR", () => {
  const root = fixture({ "0001-a.md": a, "TEMPLATE.md": "not an ADR" });
  assert.deepEqual(adrIndexProblems(root), []);
  assert.deepEqual(adrStatusProblems(root), []);
  assert.deepEqual(adrAmendmentProblems(root), []);
});

test("a clean two-ADR index has no problems", () => {
  assert.deepEqual(adrIndexProblems(fixture({ "0001-a.md": a, "0002-b.md": b })), []);
});

for (const [label, text] of [
  ["no Status line", "# 0001 — A\n"],
  ["two Status lines", `${a}\n${statusPrefix}Proposed\n`],
]) {
  test(`an ADR with ${label} is reported`, () => {
    assert.deepEqual(adrStatusProblems(fixture({ "0001-a.md": text })),
      ['docs/adr/0001-a.md: needs exactly one "- **Status:** " line']);
  });
}

test("an unknown Status is reported", () => {
  assert.deepEqual(adrStatusProblems(fixture({ "0001-a.md": document("0001", "A", "Done") })),
    ['docs/adr/0001-a.md: Status must start with Proposed, Accepted or "Superseded by ["']);
});

test("Proposed, Accepted with notes and Superseded statuses are valid", () => {
  const root = fixture({
    "0001-a.md": document("0001", "A", "Proposed"),
    "0002-b.md": document("0002", "B", "Accepted; decision 1 superseded by [ADR-0003](0003-c.md)"),
    "0003-c.md": document("0003", "C", "Superseded by [ADR-0002](0002-b.md)"),
  });
  assert.deepEqual(adrStatusProblems(root), []);
});

const amendmentProblem = (target, source = "0002-b.md") =>
  `docs/adr/${target}: Status does not name docs/adr/${source}, which amends or supersedes it`;
const amendsA = "- Builds on: [ADR-0001](0001-a.md), whose decision 2 it amends";

test("a Builds on amendment requires a Status back-link", () => {
  const root = fixture({ "0001-a.md": a, "0002-b.md": document("0002", "B", "Accepted", amendsA) });
  assert.deepEqual(adrAmendmentProblems(root), [amendmentProblem("0001-a.md")]);
});

test("a Status naming its amendment satisfies the rule", () => {
  const root = fixture({
    "0001-a.md": document("0001", "A", "Accepted; amended by [ADR-0002](0002-b.md)"),
    "0002-b.md": document("0002", "B", "Accepted", amendsA),
  });
  assert.deepEqual(adrAmendmentProblems(root), []);
});

test("Supersedes in part targets every ADR link on the line", () => {
  const root = fixture({ "0001-a.md": a, "0002-b.md": b,
    "0003-c.md": document("0003", "C", "Accepted", "- Supersedes in part: [ADR-0001](0001-a.md) (decision 1); amends [ADR-0002](0002-b.md)"),
  });
  assert.deepEqual(adrAmendmentProblems(root), [amendmentProblem("0001-a.md", "0003-c.md"), amendmentProblem("0002-b.md", "0003-c.md")]);
});

test("plain Builds on links require no amendment back-link", () => {
  const root = fixture({ "0001-a.md": a, "0002-b.md": b,
    "0003-c.md": document("0003", "C", "Accepted", "- Builds on: [ADR-0001](0001-a.md), [ADR-0002](0002-b.md)"),
  });
  assert.deepEqual(adrAmendmentProblems(root), []);
});

test("it amends after the second link targets only that link", () => {
  const root = fixture({ "0001-a.md": a, "0002-b.md": b,
    "0003-c.md": document("0003", "C", "Accepted", "- Builds on: [ADR-0001](0001-a.md), [ADR-0002](0002-b.md), whose decision it amends"),
  });
  assert.deepEqual(adrAmendmentProblems(root), [amendmentProblem("0002-b.md", "0003-c.md")]);
});

test("fragment links count as amendment targets and Status back-links", () => {
  const source = document("0002", "B", "Accepted", "- Amends: [ADR-0001](0001-a.md#decision)");
  assert.deepEqual(adrAmendmentProblems(fixture({ "0001-a.md": a, "0002-b.md": source })), [amendmentProblem("0001-a.md")]);
  assert.deepEqual(adrAmendmentProblems(fixture({
    "0001-a.md": document("0001", "A", "Accepted; amended by [ADR-0002](0002-b.md#decision)"), "0002-b.md": source,
  })), []);
});

test("a missing amendment target is reported", () => {
  assert.deepEqual(adrAmendmentProblems(fixture({ "0002-b.md": document("0002", "B", "Accepted", amendsA) })),
    ["docs/adr/0002-b.md: names 0001-a.md, which does not exist"]);
});

test("only References list lines contribute amendment targets", () => {
  const text = `# 0002 — B\n${statusPrefix}Accepted\n${amendsA}\n## References\n${amendsA.slice(2)}\n## Other\n${amendsA}\n`;
  assert.deepEqual(adrAmendmentProblems(fixture({ "0001-a.md": a, "0002-b.md": text })), []);
});

test("amendment problems are sorted and de-duplicated", () => {
  const refs = "- Builds on, and SUPERSEDES IN PART: [B](0002-b.md), [A](0001-a.md), [A again](0001-a.md)";
  const root = fixture({ "0001-a.md": a, "0002-b.md": b, "0003-c.md": document("0003", "C", "Accepted", `${refs}\n${refs}`) });
  assert.deepEqual(adrAmendmentProblems(root), [amendmentProblem("0001-a.md", "0003-c.md"), amendmentProblem("0002-b.md", "0003-c.md")]);
});

test("it supersedes and Supersedes also identify targets", () => {
  for (const refs of ["- Builds on: [A](0001-a.md), whose decision it supersedes", "- Supersedes: [A](0001-a.md)"]) {
    assert.deepEqual(adrAmendmentProblems(fixture({ "0001-a.md": a, "0002-b.md": document("0002", "B", "Accepted", refs) })),
      [amendmentProblem("0001-a.md")]);
  }
});
