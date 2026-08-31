// Store behavior: golden publish replay (the corpus-durable path pins),
// v2 redelivery classification (supersede removed: equal recorded digest
// is duplicate, anything else at an occupied path is conflict), the
// containment discipline, the corpus walk, and the oversize truncation
// policy.

import { test } from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";
import * as os from "node:os";

import { parsePayload, validate, MAX_CONTENT_BYTES, type Payload, type RawPayload } from "../../src/contracts.ts";
import { parseEpisode, verifyEpisode } from "../../src/episode.ts";
import {
  openJournalRoot,
  walkCorpus,
  readContained,
  containedPath,
  corpusSignatureOf,
  countEpisodes,
  rootInSharedDirectory,
  StoreError,
  EvidenceUnavailableError,
  type WalkEntry,
} from "../../src/corpus.ts";
import { publish, capture, applyOversizePolicy } from "../../src/store.ts";
import { GOLDEN_DIR, PAYLOADS_DIR } from "./helpers.ts";

const onWindows = process.platform === "win32";

function tempDir(): string {
  return fs.mkdtempSync(path.join(os.tmpdir(), "aj-store-"));
}

function loadPayload(name: string): Payload {
  const raw = parsePayload(fs.readFileSync(path.join(PAYLOADS_DIR, name + ".json")));
  if (raw.world === null) raw.world = "main";
  if (raw.scope === null) raw.scope = "default";
  return validate(raw);
}

interface CaptureVector {
  outcome: string;
  episode_id: string | null;
  payload_digest: string | null;
  path: string | null;
}

const vectors: Record<string, CaptureVector> = JSON.parse(
  fs.readFileSync(path.join(GOLDEN_DIR, "capture-vectors.json"), "utf8"),
);

// Replays every pinned capture through publish and demands the same
// journal-relative path and episode bytes the fixtures pin. The path is an
// index and evidence-reference key, so a layout drift here would silently
// fork the corpus.
test("golden publish replay", async (t) => {
  for (const [name, vec] of Object.entries(vectors)) {
    if (vec.outcome !== "published") continue;
    await t.test(name, () => {
      const dir = tempDir();
      try {
        const golden = fs.readFileSync(path.join(GOLDEN_DIR, "episodes", name + ".md"));
        const ep = parseEpisode(golden.toString("utf8"));
        assert.notEqual(ep, null);
        const p = loadPayload(name);
        const root = openJournalRoot(dir);
        const pub = publish(root, p, ep!.captureTimeMs);
        assert.equal(pub.outcome, "published");
        assert.equal(pub.relPath, vec.path);
        assert.ok(Buffer.from(pub.content, "utf8").equals(golden), "published bytes differ from the pinned episode");
        const st = fs.lstatSync(path.join(dir, pub.relPath));
        if (!onWindows) assert.equal(st.mode & 0o777, 0o600);
        // Redelivery against the pinned corpus shape dedupes.
        const again = publish(root, p, ep!.captureTimeMs + 1);
        assert.equal(again.outcome, "duplicate");
        assert.equal(again.relPath, pub.relPath);
      } finally {
        fs.rmSync(dir, { recursive: true, force: true });
      }
    });
  }
});

// v2 redelivery classification: the v1 supersede vector (a proven strict
// extension of the stored turn) now classifies as conflict — capture fires
// once per settled turn, so a same-identity redelivery with different
// bytes only ever means divergence. First-write-wins: the stored bytes are
// untouched.
test("same-identity redelivery with different bytes is a conflict", () => {
  for (const [baseName, otherName] of [
    ["supersede-base", "supersede-extended"],
    ["divergent-base", "divergent-other"],
  ]) {
    const dir = tempDir();
    try {
      const root = openJournalRoot(dir);
      const base = loadPayload(baseName);
      const first = publish(root, base, 1785240000000);
      assert.equal(first.outcome, "published");
      const storedBytes = fs.readFileSync(path.join(dir, first.relPath));
      const other = loadPayload(otherName);
      const second = publish(root, other, 1785240000001);
      assert.equal(second.outcome, "conflict", otherName);
      assert.equal(second.relPath, first.relPath);
      assert.ok(fs.readFileSync(path.join(dir, first.relPath)).equals(storedBytes), "conflict modified stored bytes");
    } finally {
      fs.rmSync(dir, { recursive: true, force: true });
    }
  }
});

test("a planted symlink at the final episode path is refused, not followed", { skip: onWindows }, () => {
  const dir = tempDir();
  const outside = tempDir();
  try {
    const root = openJournalRoot(dir);
    const p = loadPayload("bare-no-world-scope"); // shards to 1970/01/01
    const victim = path.join(outside, "victim.md");
    fs.writeFileSync(victim, "outside bytes", { mode: 0o644 });
    fs.mkdirSync(path.join(dir, "1970", "01", "01"), { recursive: true, mode: 0o700 });
    // Plant the link at the exact final name publish will derive (the
    // pinned id from testdata/golden/capture-vectors.json).
    const finalName = "aj1-b42df6da3736e3f73459dd31930678b4.md";
    fs.symlinkSync(victim, path.join(dir, "1970", "01", "01", finalName));
    assert.throws(
      () => publish(root, p, 1785240000000),
      (err: unknown) => err instanceof StoreError && err.code === "containment_violation",
    );
    assert.equal(fs.readFileSync(victim, "utf8"), "outside bytes", "the outside target was read or modified");
    assert.equal(fs.lstatSync(victim).mode & 0o777, 0o644, "the outside target's mode was changed");
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
    fs.rmSync(outside, { recursive: true, force: true });
  }
});

test("a symlinked shard component is a containment violation", { skip: onWindows }, () => {
  const dir = tempDir();
  const outside = tempDir();
  try {
    const root = openJournalRoot(dir);
    const p = loadPayload("basic"); // worlds/testworld/...
    fs.symlinkSync(outside, path.join(dir, "worlds"));
    assert.throws(
      () => publish(root, p, 1785240000000),
      (err: unknown) => err instanceof StoreError && err.code === "containment_violation",
    );
    assert.equal(fs.readdirSync(outside).length, 0, "the write escaped the corpus");
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
    fs.rmSync(outside, { recursive: true, force: true });
  }
});

test("corpus walk visibility rule", () => {
  const dir = tempDir();
  try {
    const root = openJournalRoot(dir);
    const p = loadPayload("basic");
    const pub = publish(root, p, 1785240000000);
    // Foreign tooling state and non-episode files are invisible.
    fs.mkdirSync(path.join(dir, ".git"), { recursive: true });
    fs.writeFileSync(path.join(dir, ".git", "aj1-fake.md"), "x");
    fs.writeFileSync(path.join(dir, "notes.md"), "x");
    fs.writeFileSync(path.join(dir, "2026"), "a file where a shard could be");
    const seen: WalkEntry[] = [];
    walkCorpus(root, (e) => {
      seen.push(e);
    });
    const episodes = seen.filter((e) => e.kind === "episode");
    assert.equal(episodes.length, 1);
    assert.equal(episodes[0].relPath, pub.relPath);
    assert.ok(episodes[0].mtimeMs !== undefined && episodes[0].sizeBytes !== undefined);
    assert.equal(countEpisodes(root), 1);
    const sig = corpusSignatureOf(root);
    assert.equal(sig.episodes, 1);
    assert.ok(sig.maxMtimeMs > 0);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test("contained path vocabulary", () => {
  assert.ok(containedPath("2026/07/28/aj1-x.md"));
  assert.ok(containedPath("worlds/w/scopes/s/2026/07/28/aj1-x.md"));
  assert.ok(!containedPath(""));
  assert.ok(!containedPath("/abs/path.md"));
  assert.ok(!containedPath("a//b.md"));
  assert.ok(!containedPath("a/./b.md"));
  assert.ok(!containedPath("a/../b.md"));
  assert.ok(!containedPath("a\\b.md"));
});

test("contained read refuses traversal and symlinks", { skip: onWindows }, () => {
  const dir = tempDir();
  const outside = tempDir();
  try {
    fs.writeFileSync(path.join(outside, "secret.md"), "outside");
    const root = openJournalRoot(dir);
    const p = loadPayload("bare-no-world-scope");
    const pub = publish(root, p, 1785240000000);
    assert.ok(readContained(root, pub.relPath).includes("## User"));
    fs.symlinkSync(outside, path.join(dir, "link"));
    for (const bad of ["../secret.md", "link/secret.md", "missing/nope.md", pub.relPath + "/deeper.md"]) {
      assert.throws(() => readContained(root, bad), EvidenceUnavailableError, bad);
    }
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
    fs.rmSync(outside, { recursive: true, force: true });
  }
});

test("oversize policy truncates at a code-point boundary and records drops", () => {
  const base = parsePayload(fs.readFileSync(path.join(PAYLOADS_DIR, "basic.json")));
  // A 4-byte glyph straddling the cut must not yield a torn code point.
  const glyph = "\u{1F600}";
  const over: RawPayload = {
    ...base,
    userContent: "u".repeat(MAX_CONTENT_BYTES - 2) + glyph,
    assistantResult: "a".repeat(MAX_CONTENT_BYTES + 7),
  };
  const { raw, drops } = applyOversizePolicy(over);
  assert.equal(Buffer.byteLength(raw.userContent, "utf8"), MAX_CONTENT_BYTES - 2);
  assert.equal(drops.user, 4); // the whole 4-byte glyph: 2 bytes over budget, 2 more to reach a boundary
  assert.equal(Buffer.byteLength(raw.assistantResult, "utf8"), MAX_CONTENT_BYTES);
  assert.equal(drops.assistant, 7);
  assert.ok(raw.userContent.isWellFormed());
  // Under-budget content passes through untouched.
  const clean = applyOversizePolicy(base);
  assert.equal(clean.raw, base);
  assert.deepEqual(clean.drops, { user: 0, assistant: 0 });
});

test("an oversized turn captures with visible accounting and verifies", () => {
  const dir = tempDir();
  try {
    const base = parsePayload(fs.readFileSync(path.join(PAYLOADS_DIR, "basic.json")));
    const over: RawPayload = { ...base, assistantResult: "a".repeat(MAX_CONTENT_BYTES + 100) };
    const result = capture({
      rootPath: path.join(dir, "journal"),
      indexPath: "",
      raw: over,
      defaults: { world: "main", scope: "default" },
      captureTimeMs: 1785240000000,
    });
    assert.equal(result.outcome, "published");
    const content = fs.readFileSync(path.join(dir, "journal", result.relPath), "utf8");
    assert.ok(content.includes("\nassistant_dropped_bytes: 100\n"));
    assert.ok(!content.includes("\nuser_dropped_bytes:"));
    const ep = parseEpisode(content);
    assert.equal(ep!.assistantDroppedBytes, 100);
    assert.equal(ep!.userDroppedBytes, 0);
    assert.ok(verifyEpisode(content).ok, "truncated episode must verify against its own digest");
    // Determinism: redelivering the same oversized turn dedupes.
    const again = capture({
      rootPath: path.join(dir, "journal"),
      indexPath: "",
      raw: over,
      defaults: { world: "main", scope: "default" },
      captureTimeMs: 1785240000999,
    });
    assert.equal(again.outcome, "duplicate");
    assert.equal(again.relPath, result.relPath);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test("capture refuses a root under a shared directory", { skip: onWindows }, () => {
  const dir = tempDir();
  try {
    fs.chmodSync(dir, 0o777);
    const base = parsePayload(fs.readFileSync(path.join(PAYLOADS_DIR, "basic.json")));
    const result = capture({
      rootPath: path.join(dir, "journal"),
      indexPath: "",
      raw: base,
      defaults: { world: "main", scope: "default" },
      captureTimeMs: 1785240000000,
    });
    assert.equal(result.outcome, "permission_denied");
    assert.ok(result.sharedDirectory);
    assert.ok(!fs.existsSync(path.join(dir, "journal")), "a refused root must never be created");
    assert.ok(rootInSharedDirectory(path.join(dir, "journal")));
    fs.chmodSync(dir, 0o755);
    assert.ok(!rootInSharedDirectory(path.join(dir, "journal")));
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test("capture reports validation failures with their typed detail", () => {
  const dir = tempDir();
  try {
    const base = parsePayload(fs.readFileSync(path.join(PAYLOADS_DIR, "basic.json")));
    const bad: RawPayload = { ...base, lane: "unknown" };
    const result = capture({
      rootPath: dir,
      indexPath: "",
      raw: bad,
      defaults: { world: "main", scope: "default" },
      captureTimeMs: 1785240000000,
    });
    assert.equal(result.outcome, "malformed");
    assert.equal(result.detail, "InvalidLane");
    assert.equal(countEpisodes(openJournalRoot(dir)), 0);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test("journal root is hardened owner-only on open", { skip: onWindows }, () => {
  const dir = tempDir();
  try {
    const rootPath = path.join(dir, "journal");
    openJournalRoot(rootPath);
    assert.equal(fs.statSync(rootPath).mode & 0o777, 0o700);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});
