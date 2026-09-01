// Snapshot index behavior: build and incremental sync accounting,
// stat-walk freshness, the writer lock, capture's projection integration,
// and the maintenance ops (status, reseal, catalog).

import { test } from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";
import * as os from "node:os";

import { parsePayload, type RawPayload } from "../../src/contracts.ts";
import { verifyEpisode } from "../../src/episode.ts";
import { openJournalRoot, openExistingRoot } from "../../src/corpus.ts";
import { capture } from "../../src/store.ts";
import {
  openSnapshot,
  freshnessOf,
  lookupEpisode,
  withIndexLock,
  IndexLockHeldError,
  type Snapshot,
} from "../../src/index.ts";
import { statusOf, sync, reseal, catalog, SyncError } from "../../src/ops.ts";
import { rootDigestHex } from "../../src/paths.ts";
import { PAYLOADS_DIR } from "./helpers.ts";

function scratch(): { dir: string; rootPath: string; indexPath: string; drop: () => void } {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "aj-index-"));
  return {
    dir,
    rootPath: path.join(dir, "journal"),
    indexPath: path.join(dir, "state", "index.v2.json"),
    drop: () => fs.rmSync(dir, { recursive: true, force: true }),
  };
}

// NTFS cannot represent the fixtures' colon scope; the same logic runs
// under a portable name on Windows.
const FIXTURE_SCOPE = process.platform === "win32" ? "workspace-demo" : "workspace:demo";

function rawPayload(name: string, overrides: Partial<RawPayload> = {}): RawPayload {
  const p = { ...parsePayload(fs.readFileSync(path.join(PAYLOADS_DIR, name + ".json"))), ...overrides };
  if (process.platform === "win32" && typeof p.scope === "string") p.scope = p.scope.replace(/:/g, "-");
  return p;
}

const DEFAULTS = { world: "main", scope: "default" };

function mustOpen(indexPath: string, rootPath: string): Snapshot {
  const opened = openSnapshot(indexPath, rootDigestHex(rootPath));
  assert.equal(opened.kind, "ok");
  return (opened as { kind: "ok"; snapshot: Snapshot }).snapshot;
}

function captureInto(rootPath: string, indexPath: string, raw: RawPayload, timeMs = 1785240000000) {
  return capture({ rootPath, indexPath, raw, defaults: DEFAULTS, captureTimeMs: timeMs });
}

test("sync builds, resyncs unchanged, and accounts for corpus surgery", () => {
  const s = scratch();
  try {
    for (const name of ["basic", "delegated", "evaluation", "bare-no-world-scope"]) {
      assert.equal(captureInto(s.rootPath, "", rawPayload(name)).outcome, "published");
    }
    const first = sync(s.rootPath, s.indexPath);
    assert.deepEqual(
      [first.indexed, first.unchanged, first.removed, first.skippedMalformed, first.duplicateIds, first.digestMismatch],
      [4, 0, 0, 0, 0, 0],
    );
    const again = sync(s.rootPath, s.indexPath);
    assert.deepEqual([again.indexed, again.unchanged, again.removed], [0, 4, 0]);

    const snap = mustOpen(s.indexPath, s.rootPath);
    const [a, b, c, d] = snap.episodes.map((e) => e.relPath);
    fs.copyFileSync(path.join(s.rootPath, a), path.join(s.rootPath, path.dirname(a), "aj1-copy.md"));
    fs.writeFileSync(path.join(s.rootPath, path.dirname(b), "aj1-garbage.md"), "not an episode");
    const edited = fs.readFileSync(path.join(s.rootPath, c), "utf8").replace("## Assistant\n\n", "## Assistant\n\nEDIT ");
    fs.writeFileSync(path.join(s.rootPath, c), edited);
    fs.rmSync(path.join(s.rootPath, d));
    const surgery = sync(s.rootPath, s.indexPath);
    assert.equal(surgery.duplicateIds, 1);
    assert.equal(surgery.skippedMalformed, 1);
    assert.equal(surgery.digestMismatch, 1);
    assert.equal(surgery.removed, 1);
    assert.equal(surgery.indexed, 1);
    assert.equal(surgery.unchanged, 2);
  } finally {
    s.drop();
  }
});

test("freshness follows the stat-walk signature", () => {
  const s = scratch();
  try {
    captureInto(s.rootPath, "", rawPayload("basic"));
    sync(s.rootPath, s.indexPath);
    const root = openExistingRoot(s.rootPath);
    assert.equal(freshnessOf(mustOpen(s.indexPath, s.rootPath), root).freshness, "fresh");

    captureInto(s.rootPath, "", rawPayload("delegated"));
    assert.equal(freshnessOf(mustOpen(s.indexPath, s.rootPath), root).freshness, "stale");
    const result = captureInto(s.rootPath, s.indexPath, rawPayload("evaluation"));
    assert.equal(result.indexState, "stale", "projection missing the bypassed capture stays stale");
    sync(s.rootPath, s.indexPath);
    const after = captureInto(s.rootPath, s.indexPath, rawPayload("imported-legacy"));
    assert.equal(after.outcome, "published");
    assert.equal(after.indexState, "fresh");
    const snap = mustOpen(s.indexPath, s.rootPath);
    assert.equal(freshnessOf(snap, root).freshness, "fresh");
    assert.notEqual(lookupEpisode(snap, after.episodeId), null);
  } finally {
    s.drop();
  }
});

test("capture classifies redelivery corpus-wide through the projection", () => {
  const s = scratch();
  try {
    const first = captureInto(s.rootPath, s.indexPath, rawPayload("basic"));
    assert.equal(first.outcome, "published");
    sync(s.rootPath, s.indexPath);

    const exact = captureInto(s.rootPath, s.indexPath, rawPayload("basic"), 1785240099999);
    assert.equal(exact.outcome, "duplicate");
    assert.equal(exact.indexState, "fresh");
    assert.equal(exact.relPath, first.relPath);

    const moved = captureInto(
      s.rootPath,
      s.indexPath,
      rawPayload("basic", { eventTimeMs: rawPayload("basic").eventTimeMs + 86_400_000n }),
    );
    assert.equal(moved.outcome, "conflict");
    assert.equal(moved.relPath, first.relPath);
    assert.equal(moved.indexState, "stale");
  } finally {
    s.drop();
  }
});

test("a foreign snapshot is refused, never misread as empty memory", () => {
  const s = scratch();
  try {
    captureInto(s.rootPath, "", rawPayload("basic"));
    sync(s.rootPath, s.indexPath);
    const otherRoot = path.join(s.dir, "other-journal");
    captureInto(otherRoot, "", rawPayload("delegated"));
    assert.equal(openSnapshot(s.indexPath, rootDigestHex(otherRoot)).kind, "foreign");
    assert.equal(statusOf(otherRoot, s.indexPath).freshness, "unavailable");
    sync(otherRoot, s.indexPath);
    assert.equal(openSnapshot(s.indexPath, rootDigestHex(otherRoot)).kind, "ok");
  } finally {
    s.drop();
  }
});

test("a structurally corrupt snapshot reads as not_built, never throws", () => {
  const s = scratch();
  try {
    captureInto(s.rootPath, "", rawPayload("basic"));
    sync(s.rootPath, s.indexPath);
    const wire = JSON.parse(fs.readFileSync(s.indexPath, "utf8"));
    wire.postings = null;
    fs.writeFileSync(s.indexPath, JSON.stringify(wire));
    assert.equal(openSnapshot(s.indexPath, null).kind, "not_built");
    wire.postings = {};
    wire.episodes = [null];
    fs.writeFileSync(s.indexPath, JSON.stringify(wire));
    assert.equal(openSnapshot(s.indexPath, null).kind, "not_built");
    const result = captureInto(s.rootPath, s.indexPath, rawPayload("delegated"));
    assert.equal(result.outcome, "published");
  } finally {
    s.drop();
  }
});

test("the writer lock serializes and recovers from stale holders", () => {
  const s = scratch();
  try {
    captureInto(s.rootPath, "", rawPayload("basic"));
    sync(s.rootPath, s.indexPath);
    fs.writeFileSync(s.indexPath + ".lock", JSON.stringify({ pid: process.pid, time_ms: Date.now() }));
    const result = captureInto(s.rootPath, s.indexPath, rawPayload("delegated"));
    assert.equal(result.outcome, "published");
    assert.equal(result.indexState, "stale");
    fs.rmSync(s.indexPath + ".lock");
    fs.writeFileSync(s.indexPath + ".lock", JSON.stringify({ pid: 999999999, time_ms: Date.now() }));
    assert.equal(
      withIndexLock(s.indexPath, false, () => "ran"),
      "ran",
    );
    assert.ok(!fs.existsSync(s.indexPath + ".lock"));
    withIndexLock(s.indexPath, false, () => {
      assert.throws(() => withIndexLock(s.indexPath, false, () => "never"), IndexLockHeldError);
    });
  } finally {
    s.drop();
  }
});

test("status reports health and reseal re-attests edits", () => {
  const s = scratch();
  try {
    assert.equal(statusOf(s.rootPath, s.indexPath).rootOk, false);
    captureInto(s.rootPath, "", rawPayload("basic"));
    let st = statusOf(s.rootPath, s.indexPath);
    assert.deepEqual([st.rootOk, st.episodes, st.freshness], [true, 1, "not_built"]);
    sync(s.rootPath, s.indexPath);
    st = statusOf(s.rootPath, s.indexPath);
    assert.deepEqual([st.rootOk, st.episodes, st.indexed, st.freshness], [true, 1, 1, "fresh"]);

    const relPath = mustOpen(s.indexPath, s.rootPath).episodes[0].relPath;
    const abs = path.join(s.rootPath, relPath);
    fs.writeFileSync(abs, fs.readFileSync(abs, "utf8").replace("## Assistant\n\n", "## Assistant\n\nEDIT "));
    assert.equal(statusOf(s.rootPath, s.indexPath).freshness, "stale");

    const previewReport = reseal(s.rootPath, s.indexPath, true);
    assert.deepEqual(
      [previewReport.scanned, previewReport.resealed, previewReport.paths],
      [1, 1, [relPath]],
    );
    assert.ok(!verifyEpisode(fs.readFileSync(abs, "utf8")).ok, "preview must write nothing");

    const report = reseal(s.rootPath, s.indexPath, false);
    assert.equal(report.resealed, 1);
    assert.ok(verifyEpisode(fs.readFileSync(abs, "utf8")).ok);
    assert.equal(statusOf(s.rootPath, s.indexPath).freshness, "fresh");
  } finally {
    s.drop();
  }
});

test("status surfaces truncation accounting", () => {
  const s = scratch();
  try {
    const over = rawPayload("basic", { assistantResult: "a".repeat(2 * 1024 * 1024 + 5) });
    assert.equal(captureInto(s.rootPath, "", over).outcome, "published");
    const report = sync(s.rootPath, s.indexPath);
    assert.equal(report.truncated, 1);
    assert.equal(statusOf(s.rootPath, s.indexPath).truncated, 1);
  } finally {
    s.drop();
  }
});

test("catalog lists the default pair first, then discovered pairs", () => {
  const s = scratch();
  try {
    assert.deepEqual(catalog(s.rootPath, s.indexPath, DEFAULTS), [{ world: "main", scope: "default" }]);
    captureInto(s.rootPath, "", rawPayload("basic"));
    captureInto(s.rootPath, "", rawPayload("bare-no-world-scope"));
    sync(s.rootPath, s.indexPath);
    const pairs = catalog(s.rootPath, s.indexPath, DEFAULTS);
    assert.deepEqual(pairs[0], { world: "main", scope: "default" });
    assert.ok(pairs.some((p) => p.world === "testworld" && p.scope === FIXTURE_SCOPE));
    assert.equal(pairs.length, 2);
  } finally {
    s.drop();
  }
});

test("sync typed failures", () => {
  const s = scratch();
  try {
    assert.throws(
      () => sync(path.join(s.dir, "missing"), s.indexPath),
      (err: unknown) => err instanceof SyncError && err.code === "root_missing",
    );
  } finally {
    s.drop();
  }
});
