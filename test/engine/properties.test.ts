// Parse-boundary properties: the functions that turn bytes this engine did
// not produce into structured values, asserted against round-trip and
// containment invariants rather than crash-freedom — every defect found at
// these boundaries in the Go engine parsed cleanly and produced a wrong
// value. Seeds are the fixture corpus plus the named regression seeds
// minted per defect (testdata/fuzz); each also runs under a bounded
// deterministic mutation loop, and the weekly long job raises the budget
// through AUTOJOURNAL_PROPERTY_ITERS.

import { test } from "node:test";
import assert from "node:assert/strict";

import { parsePayload, validate, type RawPayload } from "../../src/contracts.ts";
import { episodeId, ID_PREFIX } from "../../src/identity.ts";
import { layoutComponents } from "../../src/corpus.ts";
import { parseEpisode, verifyEpisode, REQUIRED_EPISODE_KEYS } from "../../src/episode.ts";
import { parseConfig, writeCanonicalJson, type Config } from "../../src/config.ts";
import { parseOrderedJson } from "../../src/json.ts";
import { validWorld, validScope } from "../../src/contracts.ts";
import { fuzzSeeds, mutate, propertyIterations, Prng, PAYLOADS_DIR, GOLDEN_DIR } from "./helpers.ts";
import * as path from "node:path";

function runProperty(name: string, seeds: Buffer[], check: (data: Buffer) => void): void {
  test(name, () => {
    for (const seed of seeds) check(seed);
    const prng = new Prng(0x6a1n);
    const iters = propertyIterations(300);
    for (let i = 0; i < iters; i++) {
      const data = mutate(prng, seeds);
      try {
        check(data);
      } catch (err) {
        throw new Error(`property failed on mutated input ${JSON.stringify(data.toString("latin1"))}`, {
          cause: err,
        });
      }
    }
  });
}

// A payload that validates derives a prefixed episode id and layout
// components that stay inside the corpus root — and the date shard is
// exactly four, two and two digits naming a year in 0001-9999. The digit
// bound is the invariant, not decoration: containment alone holds over a
// wrapped timestamp, so only the shape assertion can catch a reverted
// event_time_ms bound.
runProperty("payload parse property", fuzzSeeds("FuzzParsePayload", PAYLOADS_DIR, ".json"), (data) => {
  let raw: RawPayload;
  try {
    raw = parsePayload(data);
  } catch {
    return;
  }
  if (raw.world === null) raw.world = "main";
  if (raw.scope === null) raw.scope = "default";
  let p;
  try {
    p = validate(raw);
  } catch {
    return;
  }
  assert.ok(episodeId(p).startsWith(ID_PREFIX));
  const comps = layoutComponents(p);
  assert.ok(comps.length >= 3, "layout too shallow to carry a date shard");
  for (const c of comps) {
    assert.ok(c !== "" && c !== "." && c !== ".." && !/[/\\\x00]/.test(c), "layout component escapes containment");
  }
  const date = comps.slice(-3);
  const widths = [4, 2, 2];
  date.forEach((part, i) => {
    assert.ok(new RegExp(`^[0-9]{${widths[i]}}$`).test(part), `date shard component ${part} is not ${widths[i]} digits`);
  });
  assert.notEqual(date[0], "0000", "date shard names year zero");
});

function configEquals(a: Config, b: Config): boolean {
  return (
    a.journalRoot === b.journalRoot &&
    a.defaultWorld === b.defaultWorld &&
    a.thesaurusPath === b.thesaurusPath &&
    a.contextWindow === b.contextWindow &&
    a.maxResults === b.maxResults &&
    Object.is(a.recencyBoost, b.recencyBoost) &&
    Object.is(a.minScore, b.minScore) &&
    Object.is(a.confidenceFloor, b.confidenceFloor) &&
    a.missLog === b.missLog &&
    a.missLogMaxBytes === b.missLogMaxBytes &&
    a.capture.world === b.capture.world &&
    a.capture.scope === b.capture.scope
  );
}

// A config that parses re-emits stably through the rewrite path — the
// canonical emission parses, re-emitting it is byte-identical, and the
// emission carries the same typed values, so an owner rewrite can never
// publish a config this engine refuses or one that keeps churning. An
// accepted config is finite, stated independently of the parser's internal
// rejection path so the config_non_finite regression seed can fire if that
// path is ever narrowed away.
runProperty("config parse property", fuzzSeeds("FuzzParseConfig", path.join(GOLDEN_DIR, "config"), ".json"), (data) => {
  let cfg: Config;
  try {
    cfg = parseConfig(data);
  } catch {
    return;
  }
  const root = parseOrderedJson(data.toString("utf8"));
  assert.ok(root !== null && root.kind === "object", "config parsed but the ordered reader refuses it");
  const first = writeCanonicalJson(root, 0);
  const cfg2 = parseConfig(first);
  const root2 = parseOrderedJson(first);
  assert.ok(root2 !== null, "rewrite emitted unreadable JSON");
  assert.equal(writeCanonicalJson(root2, 0), first, "rewrite is not byte-stable");
  assert.ok(configEquals(cfg, cfg2), "rewrite changed typed values");
  for (const [name, v] of Object.entries({
    recency_boost: cfg.recencyBoost,
    min_score: cfg.minScore,
    confidence_floor: cfg.confidenceFloor,
  })) {
    assert.ok(Number.isFinite(v), `accepted config carries non-finite ${name}`);
  }
});

// An episode that parses carries only contract-clean identity fields (the
// read boundary revalidates what capture enforced), binds each required
// key exactly once, and one that verifies re-renders its body to identical
// text.
runProperty(
  "episode parse property",
  fuzzSeeds("FuzzParseEpisode", path.join(GOLDEN_DIR, "episodes"), ".md"),
  (data) => {
    const content = data.toString("utf8");
    const ep = parseEpisode(content);
    if (ep === null) return;
    assert.ok(validWorld(ep.world) && validScope(ep.scope), "parsed episode carries contract-violating identity");
    const fm = content.slice(0, ep.bodyOffset);
    for (const key of REQUIRED_EPISODE_KEYS) {
      const n = fm.split("\n" + key + ": ").length - 1;
      assert.equal(n, 1, `parsed episode carries ${n} ${key} lines, want exactly 1`);
    }
    const v = verifyEpisode(content);
    if (!v.ok) return;
    let body = "\n## User\n\n" + v.episode.userContent + "\n\n## Assistant\n\n" + v.episode.assistantResult + "\n";
    if (v.episode.tools.length > 0) {
      body += "\n## Tools\n\n";
      for (const tool of v.episode.tools) body += "- " + tool.name + "\n";
    }
    assert.equal(body, content.slice(v.episode.bodyOffset), "verified body does not re-render");
  },
);
