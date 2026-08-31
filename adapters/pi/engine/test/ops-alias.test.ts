// Alias curation and the weak-query miss log: atomic thesaurus rewrites
// that never clobber a hand-edit gone wrong, case-variant collapse, and
// the opt-in, bounded, best-effort miss accounting.

import { test } from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";
import * as os from "node:os";

import { addAlias, removeAlias, AliasError, aggregateMisses, appendMiss, logSearchMiss } from "../ops-alias.ts";
import { loadAliasMapFile, loadAliasMapFromBytes, aliasGet } from "../aliases.ts";
import { defaultConfig } from "../config.ts";
import type { SearchOutput } from "../search.ts";

function tempFile(name: string): { dir: string; file: string; drop: () => void } {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "aj-alias-"));
  return { dir, file: path.join(dir, name), drop: () => fs.rmSync(dir, { recursive: true, force: true }) };
}

test("alias add and remove rewrite the thesaurus atomically and canonically", () => {
  const t = tempFile("thesaurus.json");
  try {
    addAlias(t.file, "firmware", ["fwupd", "Polkit"]);
    let m = loadAliasMapFile(t.file);
    assert.deepEqual(aliasGet(m, "firmware"), ["fwupd", "polkit"]);
    // Values already present are not duplicated; new ones extend.
    addAlias(t.file, "Firmware", ["fwupd", "lvfs"]);
    m = loadAliasMapFile(t.file);
    assert.deepEqual(aliasGet(m, "firmware"), ["fwupd", "polkit", "lvfs"]);
    assert.equal(m.entries.length, 1, "case-variant keys collapse");

    assert.equal(removeAlias(t.file, "firmware", "polkit"), "value");
    m = loadAliasMapFile(t.file);
    assert.deepEqual(aliasGet(m, "firmware"), ["fwupd", "lvfs"]);
    assert.equal(removeAlias(t.file, "firmware"), "entry");
    assert.equal(loadAliasMapFile(t.file).entries.length, 0);
    // The rewrite is the canonical two-space form with a trailing newline.
    assert.ok(fs.readFileSync(t.file, "utf8").endsWith("\n"));
  } finally {
    t.drop();
  }
});

test("alias edits preserve untouched entries and hand formatting survives as canonical form", () => {
  const t = tempFile("thesaurus.json");
  try {
    fs.writeFileSync(t.file, '{"portal": ["gateway"], "quota": ["limit"]}');
    addAlias(t.file, "firmware", ["fwupd"]);
    const m = loadAliasMapFile(t.file);
    assert.deepEqual(aliasGet(m, "portal"), ["gateway"]);
    assert.deepEqual(aliasGet(m, "quota"), ["limit"]);
    assert.deepEqual(aliasGet(m, "firmware"), ["fwupd"]);
  } finally {
    t.drop();
  }
});

test("alias edit refusals are typed and leave the file untouched", () => {
  const t = tempFile("thesaurus.json");
  try {
    const fail = (fn: () => unknown, code: string) =>
      assert.throws(fn, (err: unknown) => err instanceof AliasError && err.code === code);
    fail(() => addAlias(t.file, "an", ["fwupd"]), "invalid_term"); // too short
    fail(() => addAlias(t.file, "the", ["fwupd"]), "invalid_term"); // stop word
    fail(() => addAlias(t.file, "bad-key", ["fwupd"]), "invalid_term"); // outside [a-z0-9_]
    fail(() => addAlias(t.file, "firmware", []), "invalid_value");
    fail(() => addAlias(t.file, "firmware", ["x"]), "invalid_value"); // 1 byte
    fail(() => removeAlias(t.file, "missing"), "not_found");

    // A non-object file is malformed and never rewritten.
    fs.writeFileSync(t.file, "[1, 2]");
    fail(() => addAlias(t.file, "firmware", ["fwupd"]), "malformed");
    assert.equal(fs.readFileSync(t.file, "utf8"), "[1, 2]");
  } finally {
    t.drop();
  }
});

test("miss log aggregation dedupes, ranks, and tolerates junk", () => {
  const lines = [
    JSON.stringify({ ts: "t", query: "Quokka Fence", terms: ["quokka", "fence"], best: 0.5, top: null }),
    JSON.stringify({ ts: "t", query: "quokka fence", terms: ["quokka"], best: 0.4, top: "aj1-x" }),
    JSON.stringify({ ts: "t", query: "rare topic", terms: ["rare", "topic"], best: 0, top: null }),
    "not json at all",
    JSON.stringify({ ts: "t", terms: ["orphan"] }), // no query: skipped
    JSON.stringify({ ts: "t", query: "rare topic", terms: "mistyped" }), // terms tolerated
  ].join("\n");
  const agg = aggregateMisses(lines);
  assert.equal(agg.length, 2);
  assert.deepEqual(agg[0], { query: "quokka fence", count: 2, terms: ["fence", "quokka"] });
  assert.deepEqual(agg[1], { query: "rare topic", count: 2, terms: ["rare", "topic"] });
});

test("appendMiss is bounded and best-effort", () => {
  const t = tempFile("misses.jsonl");
  try {
    const rec = { ts: "2026-08-31T00:00:00Z", query: "q", terms: ["q"], best: 0, top: null };
    appendMiss(t.file, rec, 1024);
    appendMiss(t.file, rec, 1024);
    assert.equal(fs.readFileSync(t.file, "utf8").trim().split("\n").length, 2);
    // At the cap the log stops growing rather than rotating or failing.
    appendMiss(t.file, rec, 1);
    assert.equal(fs.readFileSync(t.file, "utf8").trim().split("\n").length, 2);
  } finally {
    t.drop();
  }
});

test("logSearchMiss gates on opt-in, outcome, and the confidence floor", () => {
  const t = tempFile("misses.jsonl");
  try {
    const env = (key: string) => (key === "AUTOJOURNAL_MISS_LOG" ? t.file : undefined);
    const out = (over: Partial<SearchOutput>): SearchOutput =>
      ({
        outcome: "no_match",
        queryTerms: ["quokka"],
        aliasTerms: [],
        foldedTerms: [],
        hits: [],
        total: 0,
        nextCursor: "",
        bestScore: 0,
        aliasDigest: "",
        freshness: "fresh",
        indexed: 0,
        source: 0,
        editedExcluded: 0,
        detail: "",
        ...over,
      }) as SearchOutput;
    const off = defaultConfig();
    logSearchMiss(env, off, "quokka", 0, out({}));
    assert.ok(!fs.existsSync(t.file), "opt-in: default config logs nothing");

    const on = { ...defaultConfig(), missLog: true };
    logSearchMiss(env, on, "quokka", 0, out({ outcome: "unavailable" }));
    assert.ok(!fs.existsSync(t.file), "errors are never logged as misses");
    logSearchMiss(env, on, "quokka", 0, out({ bestScore: 99 }));
    assert.ok(!fs.existsSync(t.file), "a confident hit is not a miss");
    logSearchMiss(env, on, "quokka", 0, out({}));
    assert.ok(fs.existsSync(t.file));
    const record = JSON.parse(fs.readFileSync(t.file, "utf8").trim());
    assert.equal(record.query, "quokka");
    assert.deepEqual(record.terms, ["quokka"]);
  } finally {
    t.drop();
  }
});

test("alias load digest is stable across formatting and key order", () => {
  const a = loadAliasMapFromBytes(new TextEncoder().encode('{"b": ["y", "x"], "a": ["z"]}'));
  const b = loadAliasMapFromBytes(new TextEncoder().encode('{\n  "a": ["z"],\n  "b": ["x", "y"]\n}'));
  assert.equal(a.digestHex, b.digestHex);
  const c = loadAliasMapFromBytes(new TextEncoder().encode('{"a": ["z"], "b": ["x"]}'));
  assert.notEqual(a.digestHex, c.digestHex);
});
