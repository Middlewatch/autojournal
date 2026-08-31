// The node CLI over the in-process engine: capture and default with the
// v1 --json interface contract. run() is exercised directly with a fake
// process boundary; no subprocess is spawned.

import { test } from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";
import * as os from "node:os";

import { run, clockFromEnv, EXIT_OK, EXIT_FAILURE, EXIT_MALFORMED, EXIT_CONFLICT, type CliIo } from "../cli.ts";

const PAYLOADS_DIR = path.join(path.dirname(new URL(import.meta.url).pathname), "..", "..", "..", "testdata", "payloads");

interface Captured {
  io: CliIo;
  stdout: () => string;
  stderr: () => string;
}

function fakeIo(env: Record<string, string>, stdin: Uint8Array = new Uint8Array()): Captured {
  let out = "";
  let err = "";
  return {
    io: {
      env: (key) => env[key],
      stdin: () => stdin,
      stdout: (t) => {
        out += t;
      },
      stderr: (t) => {
        err += t;
      },
      nowMs: () => 1785240000500,
    },
    stdout: () => out,
    stderr: () => err,
  };
}

function payloadBytes(name: string): Uint8Array {
  return fs.readFileSync(path.join(PAYLOADS_DIR, name + ".json"));
}

function tempDir(): string {
  return fs.mkdtempSync(path.join(os.tmpdir(), "aj-cli-"));
}

test("capture publishes, dedupes, and conflicts with the wire report", () => {
  const dir = tempDir();
  try {
    const env = { HOME: path.join(dir, "home") };
    const first = fakeIo(env, payloadBytes("basic"));
    const code = run(["capture", "--root", path.join(dir, "journal")], first.io);
    assert.equal(code, EXIT_OK);
    const report = JSON.parse(first.stdout());
    assert.equal(report.outcome, "published");
    assert.equal(report.episode_id, "aj1-2b51a0c261ddfe3de551ddcd9bf03a7d");
    assert.equal(report.payload_digest, "sha256:c3664aa5f523351edd8c571dd5cf8f7be02ae9df77c57d4a98a8dcebe40e3dce");
    assert.equal(report.path, "worlds/testworld/scopes/workspace:demo/2026/07/12/aj1-2b51a0c261ddfe3de551ddcd9bf03a7d.md");
    assert.equal(report.detail, null);
    assert.ok(fs.existsSync(path.join(dir, "journal", report.path)));

    const again = fakeIo(env, payloadBytes("basic"));
    assert.equal(run(["capture", "--root", path.join(dir, "journal")], again.io), EXIT_OK);
    assert.equal(JSON.parse(again.stdout()).outcome, "duplicate");

    const divergentBase = fakeIo(env, payloadBytes("divergent-base"));
    assert.equal(run(["capture", "--root", path.join(dir, "journal")], divergentBase.io), EXIT_OK);
    const divergent = fakeIo(env, payloadBytes("divergent-other"));
    assert.equal(run(["capture", "--root", path.join(dir, "journal")], divergent.io), EXIT_CONFLICT);
    const conflictReport = JSON.parse(divergent.stdout());
    assert.equal(conflictReport.outcome, "conflict");
    assert.equal(conflictReport.payload_digest, "sha256:c2e8159f680f4b7a59d385b61fcabc1768c73618dacfde15cbee4b9d63b6c6dc");
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test("capture reports malformed payloads on stdout with exit 2", () => {
  const dir = tempDir();
  try {
    const bad = fakeIo({ HOME: dir }, new TextEncoder().encode("not json"));
    assert.equal(run(["capture", "--root", path.join(dir, "journal")], bad.io), EXIT_MALFORMED);
    const report = JSON.parse(bad.stdout());
    assert.equal(report.outcome, "malformed");
    assert.equal(report.detail, "Malformed");
    assert.equal(report.episode_id, null);

    const badScope = fakeIo({ HOME: dir }, payloadBytes("bad-scope-slash"));
    assert.equal(run(["capture", "--root", path.join(dir, "journal")], badScope.io), EXIT_MALFORMED);
    assert.equal(JSON.parse(badScope.stdout()).detail, "InvalidScope");
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test("default shows and persists owner capture defaults", () => {
  const dir = tempDir();
  try {
    const env = { HOME: dir };
    const show = fakeIo(env);
    assert.equal(run(["default", "--json"], show.io), EXIT_OK);
    assert.deepEqual(JSON.parse(show.stdout()), { world: "main", scope: "default" });

    const set = fakeIo(env);
    assert.equal(run(["default", "--world", "team", "--json"], set.io), EXIT_OK);
    const setReport = JSON.parse(set.stdout());
    assert.equal(setReport.world, "team");
    assert.equal(setReport.scope, "default");
    assert.ok(fs.existsSync(setReport.config));

    // The persisted default now governs both show and capture's fill.
    const showAfter = fakeIo(env);
    assert.equal(run(["default", "--json"], showAfter.io), EXIT_OK);
    assert.equal(JSON.parse(showAfter.stdout()).world, "team");

    const captured = fakeIo(env, payloadBytes("bare-no-world-scope"));
    assert.equal(run(["capture", "--root", path.join(dir, "journal")], captured.io), EXIT_OK);
    assert.ok(JSON.parse(captured.stdout()).path.startsWith("worlds/team/"));

    const invalid = fakeIo(env);
    assert.equal(run(["default", "--world", "Bad World"], invalid.io), EXIT_MALFORMED);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test("config resolution failures are typed", () => {
  const dir = tempDir();
  try {
    const missing = fakeIo({ HOME: dir }, payloadBytes("basic"));
    assert.equal(run(["capture", "--config", path.join(dir, "nope.json")], missing.io), EXIT_FAILURE);
    assert.match(missing.stderr(), /explicit AutoJournal config was not found/);

    fs.writeFileSync(path.join(dir, "bad.json"), "{broken");
    const malformed = fakeIo({ HOME: dir }, payloadBytes("basic"));
    assert.equal(run(["capture", "--config", path.join(dir, "bad.json")], malformed.io), EXIT_FAILURE);
    assert.match(malformed.stderr(), /config is malformed/);

    // A config naming the journal root routes capture without --root.
    fs.writeFileSync(path.join(dir, "good.json"), JSON.stringify({ journal_root: path.join(dir, "journal") }));
    const routed = fakeIo({ HOME: dir }, payloadBytes("basic"));
    assert.equal(run(["capture", "--config", path.join(dir, "good.json")], routed.io), EXIT_OK);
    assert.ok(fs.existsSync(path.join(dir, "journal")));
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test("usage, version, and the pinned clock", () => {
  const usage = fakeIo({});
  assert.equal(run([], usage.io), EXIT_MALFORMED);
  assert.match(usage.stderr(), /usage: autojournal/);

  const version = fakeIo({});
  assert.equal(run(["version"], version.io), EXIT_OK);
  assert.match(version.stdout(), /^autojournal 2\.0\.0 /);

  assert.equal(clockFromEnv((k) => (k === "AUTOJOURNAL_NOW_MS" ? "12345" : undefined))(), 12345);
  assert.ok(clockFromEnv(() => undefined)() > 0);
});

test("status, sync, catalog, and reseal verbs speak the wire shapes", () => {
  const dir = tempDir();
  try {
    const env = { HOME: path.join(dir, "home") };
    const rootArg = ["--root", path.join(dir, "journal"), "--index", path.join(dir, "index.v2.json")];
    const seed = fakeIo(env, payloadBytes("basic"));
    assert.equal(run(["capture", ...rootArg], seed.io), EXIT_OK);

    // Capture birthed the projection (matching the v1 engine's
    // create-at-first-capture): status is already fresh, and the sync
    // below reports the episode unchanged rather than newly indexed.
    const before = fakeIo(env);
    assert.equal(run(["status", "--json", ...rootArg], before.io), EXIT_OK);
    const beforeReport = JSON.parse(before.stdout());
    assert.equal(beforeReport.root_ok, true);
    assert.equal(beforeReport.episodes, 1);
    assert.equal(beforeReport.index.freshness, "fresh");
    assert.equal(beforeReport.index.path, path.join(dir, "index.v2.json"));
    assert.equal(beforeReport.root_source, "explicit");

    const syncOut = fakeIo(env);
    assert.equal(run(["sync", "--json", ...rootArg], syncOut.io), EXIT_OK);
    assert.deepEqual(JSON.parse(syncOut.stdout()), {
      indexed: 0,
      unchanged: 1,
      removed: 0,
      skipped_malformed: 0,
      duplicate_ids: 0,
      digest_mismatch: 0,
      unreadable: 0,
      truncated: 0,
    });

    const after = fakeIo(env);
    assert.equal(run(["status", "--json", ...rootArg], after.io), EXIT_OK);
    assert.equal(JSON.parse(after.stdout()).index.freshness, "fresh");
    assert.equal(JSON.parse(after.stdout()).index.indexed, 1);

    const cat = fakeIo(env);
    assert.equal(run(["catalog", ...rootArg], cat.io), EXIT_OK);
    const pairs = JSON.parse(cat.stdout()).pairs;
    assert.deepEqual(pairs[0], { world: "main", scope: "default" });
    assert.ok(pairs.some((p: { world: string }) => p.world === "testworld"));

    const resealOut = fakeIo(env);
    assert.equal(run(["reseal", "--json", ...rootArg], resealOut.io), EXIT_OK);
    assert.deepEqual(JSON.parse(resealOut.stdout()), {
      scanned: 1,
      resealed: 0,
      refused: 0,
      write_failures: 0,
      paths: [],
    });

    const missing = fakeIo(env);
    assert.equal(run(["sync", "--root", path.join(dir, "nope"), "--index", path.join(dir, "i.json")], missing.io), EXIT_FAILURE);
    assert.match(missing.stderr(), /journal root missing/);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test("search and get verbs speak the wire shapes", () => {
  const dir = tempDir();
  try {
    const env = { HOME: path.join(dir, "home"), AUTOJOURNAL_NOW_MS: "1785326400000" };
    const rootArg = ["--root", path.join(dir, "journal"), "--index", path.join(dir, "index.v2.json")];
    const seed = fakeIo(env, payloadBytes("basic"));
    assert.equal(run(["capture", ...rootArg], seed.io), EXIT_OK);
    assert.equal(run(["sync", ...rootArg], fakeIo(env).io), EXIT_OK);

    // The basic fixture's world is testworld.
    const found = fakeIo(env);
    assert.equal(run(["search", "--world", "testworld", "--json", ...rootArg, "tests", "behave"], found.io), EXIT_OK);
    const report = JSON.parse(found.stdout());
    assert.equal(report.outcome, "match");
    const hit = report.results[0];
    assert.equal(hit.world, "testworld");
    assert.ok(hit.revision.startsWith("sha256:"));
    assert.equal(report.identities.scorer, "aj-scorer.v4");
    assert.equal(report.identities.tokenizer, "aj-tok.v1");
    assert.equal(report.index.freshness, "fresh");
    const got = fakeIo(env);
    assert.equal(
      run(["get", "--json", "--episode", hit.episode_id, "--revision", hit.revision, "--path", hit.path, ...rootArg], got.io),
      EXIT_OK,
    );
    const gotReport = JSON.parse(got.stdout());
    assert.equal(gotReport.outcome, "match");
    assert.equal(gotReport.trust, "untrusted_evidence");

    const noMatch = fakeIo(env);
    assert.equal(run(["search", "--world", "testworld", "--json", ...rootArg, "xylophone", "zeppelin"], noMatch.io), EXIT_OK);
    assert.equal(JSON.parse(noMatch.stdout()).outcome, "no_match");

    const badLanes = fakeIo(env);
    assert.equal(run(["search", "--lanes", "bogus", ...rootArg, "word"], badLanes.io), EXIT_MALFORMED);
    const noQuery = fakeIo(env);
    assert.equal(run(["search", ...rootArg], noQuery.io), EXIT_MALFORMED);
    const badGet = fakeIo(env);
    assert.equal(run(["get", ...rootArg], badGet.io), EXIT_MALFORMED);
    const staleGet = fakeIo(env);
    assert.equal(
      run(["get", "--json", "--episode", "aj1-" + "0".repeat(32), "--revision", "sha256:" + "0".repeat(64), ...rootArg], staleGet.io),
      EXIT_FAILURE,
    );
    assert.equal(JSON.parse(staleGet.stdout()).outcome, "gone");
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});
