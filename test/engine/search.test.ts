// Retrieval behavior: the ranking fixture replay (the public witness of
// the term-weighting invariant), discovery and crediting semantics,
// cursor binding, edited-evidence exclusion, and bounded get.

import { test } from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";
import * as os from "node:os";

import { parsePayload, type RawPayload } from "../../src/contracts.ts";
import { openExistingRoot } from "../../src/corpus.ts";
import { capture } from "../../src/store.ts";
import { openSnapshot, type Snapshot } from "../../src/index.ts";
import { sync, reseal } from "../../src/ops.ts";
import { rootDigestHex } from "../../src/paths.ts";
import { search, get, creditLine } from "../../src/search.ts";
import { idfWeight, recencyMultiplier, confidenceWithCoverage, cursorEncode, cursorDecode } from "../../src/retrieval.ts";
import { loadAliasMapFromBytes } from "../../src/aliases.ts";
import { PAYLOADS_DIR, REPO_ROOT } from "./helpers.ts";

const enc = new TextEncoder();
const EMPTY_ALIASES = loadAliasMapFromBytes(enc.encode("{}"));

function scratch() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "aj-search-"));
  return {
    dir,
    rootPath: path.join(dir, "journal"),
    indexPath: path.join(dir, "index.v2.json"),
    drop: () => fs.rmSync(dir, { recursive: true, force: true }),
  };
}

let turnCounter = 0;

function turnPayload(user: string, assistant: string, overrides: Partial<RawPayload> = {}): RawPayload {
  turnCounter++;
  return {
    schemaVersion: 1,
    world: "main",
    scope: "default",
    lane: "conversation",
    harness: "search-test",
    adapterVersion: "0.0.0",
    sessionId: "s-search",
    turnId: "t-" + turnCounter,
    eventTimeMs: 1785240000000n,
    capturePolicy: "default-v1",
    turnOutcome: "completed",
    userContent: user,
    assistantResult: assistant,
    tools: null,
    workspaceRoot: null,
    branchOf: null,
    host: null,
    ...overrides,
  };
}

function publishTurn(s: ReturnType<typeof scratch>, raw: RawPayload): void {
  const result = capture({
    rootPath: s.rootPath,
    indexPath: "",
    raw,
    defaults: { world: "main", scope: "default" },
    captureTimeMs: 1785240001000,
  });
  assert.equal(result.outcome, "published");
}

function openSynced(s: ReturnType<typeof scratch>): { root: ReturnType<typeof openExistingRoot>; snapshot: Snapshot } {
  sync(s.rootPath, s.indexPath);
  const opened = openSnapshot(s.indexPath, rootDigestHex(s.rootPath));
  assert.equal(opened.kind, "ok");
  return { root: openExistingRoot(s.rootPath), snapshot: (opened as { kind: "ok"; snapshot: Snapshot }).snapshot };
}

const NOW = 1785326400000;

test("ranking fixture replays with the pinned ordering", () => {
  const fixture = JSON.parse(
    fs.readFileSync(path.join(REPO_ROOT, "testdata", "ranking", "case-duplicate-weight.json"), "utf8"),
  ) as {
    query: string;
    world: string;
    now_ms: number;
    thesaurus: unknown;
    payloads: unknown[];
    expected: Array<{ episode_id: string; line: number }>;
  };
  assert.ok(fixture.expected.length > 0, "fixture pins nothing");
  const s = scratch();
  try {
    for (const pb of fixture.payloads) {
      publishTurn(s, parsePayload(enc.encode(JSON.stringify(pb))));
    }
    const { root, snapshot } = openSynced(s);
    const aliasMap = loadAliasMapFromBytes(enc.encode(JSON.stringify(fixture.thesaurus)));
    const out = search(root, snapshot, aliasMap, {
      query: fixture.query,
      world: fixture.world,
      nowMs: fixture.now_ms,
    });
    const got = out.hits.map((h) => `${h.episodeId}:${h.line}`);
    const want = fixture.expected.map((e) => `${e.episode_id}:${e.line}`);
    assert.deepEqual(got, want, "ordered ranking diverged from the pinned fixture");
  } finally {
    s.drop();
  }
});

test("discovery, crediting, aliases, and folding", () => {
  const s = scratch();
  try {
    publishTurn(s, turnPayload("we fixed the fwupd daemon today", "Firmware daemon repaired."));
    publishTurn(s, turnPayload("the strange changes were reverted", "Reverted cleanly."));
    publishTurn(s, turnPayload("quota policies were rewritten", "Policies rewritten."));
    const { root, snapshot } = openSynced(s);

    const hang = search(root, snapshot, EMPTY_ALIASES, { query: "hang daemon", world: "main", nowMs: NOW });
    assert.equal(hang.outcome, "match");
    assert.ok(hang.hits.every((h) => !h.matchedTerms.includes("hang")));

    const infix = search(root, snapshot, EMPTY_ALIASES, {
      query: "hang",
      world: "main",
      nowMs: NOW,
      creditMode: "substring",
    });
    assert.equal(infix.outcome, "match");

    const aliases = loadAliasMapFromBytes(enc.encode('{"firmware": ["fwupd"]}'));
    const aliased = search(root, snapshot, aliases, { query: "firmware", world: "main", nowMs: NOW });
    assert.equal(aliased.outcome, "match");
    assert.deepEqual(aliased.aliasTerms, ["fwupd"]);
    assert.ok(aliased.hits.some((h) => h.matchedTerms.includes("fwupd")));

    const folded = search(root, snapshot, EMPTY_ALIASES, { query: "policies", world: "main", nowMs: NOW });
    assert.ok(folded.foldedTerms.includes("policy"));

    const none = search(root, snapshot, EMPTY_ALIASES, { query: "zebra xylophone", world: "main", nowMs: NOW });
    assert.equal(none.outcome, "no_match");
    const stops = search(root, snapshot, EMPTY_ALIASES, { query: "the of and", world: "main", nowMs: NOW });
    assert.equal(stops.outcome, "no_match");
    assert.deepEqual(stops.queryTerms, []);

    const unbuilt = search(root, null, EMPTY_ALIASES, { query: "daemon", world: "main", nowMs: NOW });
    assert.equal(unbuilt.outcome, "index_stale");
    assert.equal(unbuilt.freshness, "not_built");
  } finally {
    s.drop();
  }
});

test("scope and lane filters bound recall", () => {
  const s = scratch();
  try {
    publishTurn(s, turnPayload("marker text alpha", "In default scope."));
    publishTurn(s, turnPayload("marker text beta", "In project scope.", { scope: "project:x" }));
    publishTurn(s, turnPayload("marker text gamma", "In evaluation lane.", { lane: "evaluation" }));
    const { root, snapshot } = openSynced(s);

    const all = search(root, snapshot, EMPTY_ALIASES, { query: "marker", world: "main", nowMs: NOW });
    assert.equal(all.total, 2);
    const scoped = search(root, snapshot, EMPTY_ALIASES, { query: "marker", world: "main", scope: "project:x", nowMs: NOW });
    assert.equal(scoped.total, 1);
    assert.equal(scoped.hits[0].scope, "project:x");
    const evalLane = search(root, snapshot, EMPTY_ALIASES, {
      query: "marker",
      world: "main",
      lanes: ["evaluation"],
      nowMs: NOW,
    });
    assert.equal(evalLane.total, 1);
    assert.equal(evalLane.hits[0].lane, "evaluation");
    const otherWorld = search(root, snapshot, EMPTY_ALIASES, { query: "marker", world: "elsewhere", nowMs: NOW });
    assert.equal(otherWorld.outcome, "no_match");
  } finally {
    s.drop();
  }
});

test("cursors page deterministically and bind to their minting state", () => {
  const s = scratch();
  try {
    for (let i = 0; i < 5; i++) {
      publishTurn(s, turnPayload(`pagination marker item ${i}`, `Recorded item ${i}.`));
    }
    const { root, snapshot } = openSynced(s);
    const page1 = search(root, snapshot, EMPTY_ALIASES, { query: "pagination marker", world: "main", nowMs: NOW, limit: 2 });
    assert.equal(page1.outcome, "match");
    assert.equal(page1.hits.length, 2);
    assert.ok(page1.nextCursor.startsWith("aj2."));
    const page2 = search(root, snapshot, EMPTY_ALIASES, {
      query: "pagination marker",
      world: "main",
      nowMs: NOW + 999_999,
      limit: 2,
      cursor: page1.nextCursor,
    });
    assert.equal(page2.outcome, "match");
    const ids1 = page1.hits.map((h) => `${h.episodeId}:${h.line}`);
    const ids2 = page2.hits.map((h) => `${h.episodeId}:${h.line}`);
    assert.equal(new Set([...ids1, ...ids2]).size, ids1.length + ids2.length, "pages overlap");

    const wrongQuery = search(root, snapshot, EMPTY_ALIASES, {
      query: "pagination markers",
      world: "main",
      nowMs: NOW,
      cursor: page1.nextCursor,
    });
    assert.equal(wrongQuery.outcome, "malformed");

    publishTurn(s, turnPayload("pagination marker item late", "Recorded late."));
    const afterChange = search(root, openSynced(s).snapshot, EMPTY_ALIASES, {
      query: "pagination marker",
      world: "main",
      nowMs: NOW,
      cursor: page1.nextCursor,
    });
    assert.equal(afterChange.outcome, "malformed");
    assert.equal(afterChange.detail, "cursor does not match this query");
  } finally {
    s.drop();
  }
});

test("edited evidence is excluded from search and stale for get", () => {
  const s = scratch();
  try {
    publishTurn(s, turnPayload("the unique wombat incident report", "Wombat contained."));
    const { root, snapshot } = openSynced(s);
    const found = search(root, snapshot, EMPTY_ALIASES, { query: "wombat incident", world: "main", nowMs: NOW });
    assert.equal(found.outcome, "match");
    const hit = found.hits[0];

    const gotten = get(root, snapshot, { episodeId: hit.episodeId, revision: hit.revision, pathHint: hit.path });
    assert.equal(gotten.outcome, "match");
    assert.ok(gotten.content.includes("wombat incident"));
    assert.equal(gotten.trust, "untrusted_evidence");
    assert.ok(gotten.lineStart >= 1 && gotten.lineEnd >= gotten.lineStart);

    const abs = path.join(s.rootPath, hit.path);
    fs.writeFileSync(abs, fs.readFileSync(abs, "utf8").replace("Wombat contained.", "Wombat escaped."));
    const excluded = search(root, snapshot, EMPTY_ALIASES, { query: "wombat incident", world: "main", nowMs: NOW });
    assert.equal(excluded.outcome, "no_match");
    assert.equal(excluded.editedExcluded, 1);
    const stale = get(root, snapshot, { episodeId: hit.episodeId, revision: hit.revision, pathHint: hit.path });
    assert.equal(stale.outcome, "stale_revision");
    assert.equal(stale.revision, "", "an unverifiable file has no honest current revision");

    reseal(s.rootPath, s.indexPath, false);
    const resealed = openSynced(s);
    const again = search(resealed.root, resealed.snapshot, EMPTY_ALIASES, { query: "wombat incident", world: "main", nowMs: NOW });
    assert.equal(again.outcome, "match");
    const newHit = again.hits[0];
    assert.notEqual(newHit.revision, hit.revision);
    assert.equal(get(resealed.root, resealed.snapshot, { episodeId: newHit.episodeId, revision: newHit.revision }).outcome, "match");
    const oldRev = get(resealed.root, resealed.snapshot, { episodeId: hit.episodeId, revision: hit.revision });
    assert.equal(oldRev.outcome, "stale_revision");
    assert.equal(oldRev.revision, newHit.revision, "stale_revision carries the replacement reference");
  } finally {
    s.drop();
  }
});

test("get validates identity, revision shape, and bounds", () => {
  const s = scratch();
  try {
    publishTurn(s, turnPayload("alpha\nbravo\ncharlie\ndelta", "Multi-line body."));
    const { root, snapshot } = openSynced(s);
    const hit = search(root, snapshot, EMPTY_ALIASES, { query: "bravo charlie", world: "main", nowMs: NOW }).hits[0];

    assert.equal(get(root, snapshot, { episodeId: "nonsense", revision: hit.revision }).outcome, "malformed");
    assert.equal(get(root, snapshot, { episodeId: hit.episodeId, revision: "sha256:short" }).outcome, "malformed");
    assert.equal(
      get(root, snapshot, { episodeId: hit.episodeId, revision: hit.revision, lineStart: 9, lineEnd: 3 }).outcome,
      "malformed",
    );
    assert.equal(
      get(root, snapshot, { episodeId: "aj1-" + "0".repeat(32), revision: hit.revision }).outcome,
      "gone",
    );
    assert.equal(
      get(root, snapshot, { episodeId: hit.episodeId, revision: hit.revision, expectedWorld: "other" }).outcome,
      "gone",
    );
    const hinted = get(root, snapshot, { episodeId: hit.episodeId, revision: hit.revision, pathHint: "2020/01/01/aj1-nope.md" });
    assert.equal(hinted.outcome, "match");
    const span = get(root, snapshot, {
      episodeId: hit.episodeId,
      revision: hit.revision,
      lineStart: hit.line,
      lineEnd: hit.line,
    });
    assert.equal(span.lineStart, hit.line);
    assert.equal(span.lineEnd, hit.line);
    assert.equal(span.content.split("\n").length, 1);
  } finally {
    s.drop();
  }
});

test("scorer primitives: smoothed idf, recency, confidence, crediting", () => {
  // aj-scorer.v4 smoothed idf: ln(1 + (N − df + 0.5)/(df + 0.5)).
  assert.equal(idfWeight(10, 0), 0);
  assert.ok(Math.abs(idfWeight(10, 1) - Math.log(1 + 9.5 / 1.5)) < 1e-12);
  assert.ok(Math.abs(idfWeight(10, 10) - Math.log(1 + 0.5 / 10.5)) < 1e-12);
  assert.ok(idfWeight(10, 1) > idfWeight(10, 5), "rarer terms weigh more");
  assert.ok(idfWeight(2, 2) > 0, "saturated terms stay positive under smoothing");

  assert.equal(recencyMultiplier(NOW, NOW, 1.0), 2.0);
  assert.equal(recencyMultiplier(NOW - 24 * 60 * 60 * 1000, NOW, 1.0), 1.5);
  assert.equal(recencyMultiplier(NOW + 1, NOW, 1.0), 1.0, "future timestamps get no boost");

  assert.equal(confidenceWithCoverage(10, 1, 3), "high");
  assert.equal(confidenceWithCoverage(10, 0.35, 3), "medium");
  assert.equal(confidenceWithCoverage(10, 0.1, 3), "low");
  assert.equal(confidenceWithCoverage(1, 1, 0), "high", "floor 0 disables banding");

  assert.ok(creditLine("the hanging lamp", "hang", "word_start"));
  assert.ok(!creditLine("the changed file", "hang", "word_start"));
  assert.ok(creditLine("the changed file", "hang", "substring"));
  assert.ok(!creditLine("the hanging lamp", "hang", "whole_word"));
  assert.ok(creditLine("let it hang there", "hang", "whole_word"));
  assert.ok(creditLine("uses llama.cpp today", "llama.cpp", "whole_word"));

  const inputs = { query: "q", world: "main", scope: "", lanes: "conversation", aliasDigest: "d", corpusSignature: "sig", rankingTag: "t" };
  const cursor = cursorEncode(7, NOW, inputs);
  assert.deepEqual(cursorDecode(cursor, inputs), { offset: 7, nowMs: NOW });
  assert.equal(cursorDecode(cursor, { ...inputs, corpusSignature: "other" }), null);
  assert.equal(cursorDecode("aj2.07." + NOW + cursor.slice(cursor.lastIndexOf(".")), inputs), null, "only the canonical spelling decodes");
  assert.equal(cursorDecode("aj1.7.deadbeef", inputs), null, "v1 cursors are foreign");
});
