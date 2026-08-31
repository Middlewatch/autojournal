// The remaining two parse-boundary properties: the alias-map loader and
// the cursor codec (the payload, config, and episode properties live in
// properties.test.ts beside the shared harness).

import { test } from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";

import { loadAliasMapFromBytes } from "../../src/aliases.ts";
import { validAliasValue } from "../../src/ops-alias.ts";
import { cursorEncode, cursorDecode, cursorGuardHex, type CursorInputs } from "../../src/retrieval.ts";
import { FUZZ_SEED_DIR, decodeGoFuzzCorpus, Prng, mutate, propertyIterations } from "./helpers.ts";

function aliasSeeds(): Buffer[] {
  const seeds: Buffer[] = [
    Buffer.from('{"portal": ["gateway"], "refresh": ["fwupd"]}'),
    Buffer.from("{}"),
    Buffer.from("not json"),
  ];
  const dir = path.join(FUZZ_SEED_DIR, "FuzzLoadAliasMapFromBytes");
  for (const name of fs.readdirSync(dir).sort()) {
    seeds.push(decodeGoFuzzCorpus(path.join(dir, name)));
  }
  return seeds;
}

// The totality oracle under-approximates: only entries that are
// unambiguously valid under an independent parse — object of string
// arrays, valid normalized key, every value already canonical — carry an
// expectation, so the oracle never flakes and never guesses.
function expectedAliasKeys(data: Buffer): Set<string> | null {
  let text: string;
  try {
    text = new TextDecoder("utf-8", { fatal: true }).decode(data);
  } catch {
    return null;
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    return null;
  }
  if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) return null;
  const keys = new Set<string>();
  for (const [k, vs] of Object.entries(parsed as Record<string, unknown>)) {
    const nk = k.trim().toLowerCase();
    if (nk.length <= 2 || nk.length > 128 || !/^[a-z0-9_]+$/.test(nk)) continue;
    if (!Array.isArray(vs) || vs.length === 0) continue;
    if (vs.every((v) => typeof v === "string" && !/[A-Z]/.test(v) && v.isWellFormed() && validAliasValue(v))) {
      keys.add(nk);
    }
  }
  return keys;
}

// Loading never fails — a broken thesaurus degrades recall, never search
// itself — entries the tolerance rules give no grounds to drop survive
// loading, and merging is idempotent: re-serializing the loaded entries
// and loading again yields the same digest with nothing left to merge.
test("alias map load property", () => {
  const seeds = aliasSeeds();
  const check = (data: Buffer) => {
    const m = loadAliasMapFromBytes(data);
    assert.notEqual(m, null);
    const expected = expectedAliasKeys(data);
    if (expected !== null) {
      const loaded = new Set(m.entries.map((e) => e.key));
      for (const k of expected) {
        assert.ok(loaded.has(k), `valid alias entry ${JSON.stringify(k)} vanished on load`);
      }
    }
    const reserialized = JSON.stringify(Object.fromEntries(m.entries.map((e) => [e.key, e.values])));
    const again = loadAliasMapFromBytes(Buffer.from(reserialized));
    assert.equal(again.digestHex, m.digestHex, "reload of the re-serialized map changed the digest");
    assert.equal(again.mergedKeys, 0, "the collapse is not idempotent");
  };
  for (const seed of seeds) check(seed);
  const prng = new Prng(0xa11a5n);
  const iters = propertyIterations(300);
  for (let i = 0; i < iters; i++) {
    const data = mutate(prng, seeds);
    try {
      check(data);
    } catch (err) {
      throw new Error(`property failed on mutated input ${JSON.stringify(data.toString("latin1"))}`, { cause: err });
    }
  }
});

// A cursor decodes only against the inputs it was minted with: a minted
// cursor round-trips its offset and clock, an arbitrary token either
// fails or re-mints byte-identically, and every guard field is binding
// (conditional on the 8-hex guards actually differing — the guard is 32
// bits by design, so a hunted collision is accepted behavior).
test("cursor decode property", () => {
  const prng = new Prng(0xc0ffeen);
  const randomText = (): string => {
    const len = prng.int(12);
    let s = "";
    for (let i = 0; i < len; i++) s += String.fromCharCode(0x20 + prng.int(90));
    return s;
  };
  const iters = propertyIterations(300);
  for (let i = 0; i < iters; i++) {
    const inputs: CursorInputs = {
      query: randomText(),
      world: randomText(),
      scope: randomText(),
      lanes: randomText(),
      aliasDigest: randomText(),
      corpusSignature: randomText(),
      rankingTag: randomText(),
    };
    const offset = prng.int(1_000_000);
    const nowMs = prng.int(2_000_000_000_000);
    const minted = cursorEncode(offset, nowMs, inputs);
    assert.deepEqual(cursorDecode(minted, inputs), { offset, nowMs }, "minted cursor did not round-trip");

    const arbitrary = `aj2.${prng.int(100)}.${prng.int(100)}.${randomText()}`;
    const decoded = cursorDecode(arbitrary, inputs);
    if (decoded !== null) {
      assert.equal(
        cursorEncode(decoded.offset, decoded.nowMs, inputs),
        arbitrary,
        "decoded token does not re-mint identically",
      );
    }

    for (const mutateField of [
      (x: CursorInputs) => (x.query += " m"),
      (x: CursorInputs) => (x.world += " m"),
      (x: CursorInputs) => (x.scope += " m"),
      (x: CursorInputs) => (x.lanes += " m"),
      (x: CursorInputs) => (x.aliasDigest += " m"),
      (x: CursorInputs) => (x.corpusSignature += " m"),
      (x: CursorInputs) => (x.rankingTag += " m"),
    ]) {
      const other = { ...inputs };
      mutateField(other);
      if (cursorGuardHex(other, nowMs) === cursorGuardHex(inputs, nowMs)) continue;
      assert.equal(cursorDecode(minted, other), null, "cursor decoded against inputs it was not minted with");
    }
  }
});
