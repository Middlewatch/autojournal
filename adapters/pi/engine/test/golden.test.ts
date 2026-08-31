// Golden fixture harness — the enforcement of the corpus-durable tier.
//
// testdata/payloads is the capture contract matrix. testdata/golden/
// capture-vectors.json pins the required outcome, episode id, payload
// digest, and published path for every payload in it, and
// testdata/golden/episodes holds the exact episode bytes each must
// produce. These fixtures are the authority: the corpus the Go engine
// wrote must stay readable and addressable by this engine, and that
// promise is only as good as this matrix.
//
// A diff here is a contract break, never a fixture to update. If a change
// makes one of these fail, the change is wrong — or the contract is
// genuinely moving, which is a major version and an owner decision, not a
// test edit.

import { test } from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";
import * as os from "node:os";

import { parsePayload, validate, type RawPayload, type Payload } from "../contracts.ts";
import { episodeId, payloadDigestHex, DIGEST_PREFIX } from "../identity.ts";
import { render } from "../render.ts";
import { parseEpisode } from "../episode.ts";
import { layoutComponents } from "../corpus.ts";
import { saveCaptureDefaults, ConfigError } from "../config.ts";
import { GOLDEN_DIR, PAYLOADS_DIR, mapEnviron } from "./helpers.ts";

interface CaptureVector {
  outcome: string;
  episode_id: string | null;
  payload_digest: string | null;
  path: string | null;
}

const vectors: Record<string, CaptureVector> = JSON.parse(
  fs.readFileSync(path.join(GOLDEN_DIR, "capture-vectors.json"), "utf8"),
);

// Mirrors the capture host's config merge: omitted world/scope are filled
// from owner defaults (main/default here) before validation.
function validateAsCaptureHost(raw: RawPayload): Payload {
  if (raw.world === null) raw.world = "main";
  if (raw.scope === null) raw.scope = "default";
  return validate(raw);
}

test("golden capture vectors", async (t) => {
  for (const [name, vec] of Object.entries(vectors)) {
    await t.test(name, () => {
      const bytes = fs.readFileSync(path.join(PAYLOADS_DIR, name + ".json"));
      if (vec.outcome === "malformed") {
        assert.throws(() => validateAsCaptureHost(parsePayload(bytes)));
        return;
      }
      // published, superseded (a v1 outcome; v2 classifies the same
      // delivery as conflict), and conflict vectors all pin what the
      // *incoming* delivery derives, independent of corpus state.
      assert.ok(
        ["published", "superseded", "conflict"].includes(vec.outcome),
        `unhandled vector outcome ${vec.outcome}`,
      );
      const p = validateAsCaptureHost(parsePayload(bytes));
      assert.equal(episodeId(p), vec.episode_id);
      assert.equal(DIGEST_PREFIX + payloadDigestHex(p), vec.payload_digest);
    });
  }
});

test("golden episode bytes re-render byte-identically", async (t) => {
  for (const [name, vec] of Object.entries(vectors)) {
    if (vec.outcome !== "published") continue;
    await t.test(name, () => {
      const golden = fs.readFileSync(path.join(GOLDEN_DIR, "episodes", name + ".md"));
      const ep = parseEpisode(golden.toString("utf8"));
      assert.notEqual(ep, null, "parseEpisode rejected a pinned episode");
      const p = validateAsCaptureHost(parsePayload(fs.readFileSync(path.join(PAYLOADS_DIR, name + ".json"))));
      const rendered = render({
        payload: p,
        episodeId: ep!.episodeId,
        digestHex: ep!.digestHex,
        captureTimeMs: ep!.captureTimeMs,
      });
      assert.ok(Buffer.from(rendered, "utf8").equals(golden), "rendered episode differs from golden");
      // The pinned frontmatter facts must agree with the vectors.
      assert.equal(ep!.episodeId, vec.episode_id);
      assert.equal(DIGEST_PREFIX + ep!.digestHex, vec.payload_digest);
    });
  }
});

// The published path is an index and evidence-reference key, so a layout
// drift here would silently fork the corpus. Publication itself lands in
// the store slice; the layout derivation is pinned from the start.
test("golden publish paths derive from the layout", async (t) => {
  for (const [name, vec] of Object.entries(vectors)) {
    if (vec.outcome !== "published") continue;
    await t.test(name, () => {
      const p = validateAsCaptureHost(parsePayload(fs.readFileSync(path.join(PAYLOADS_DIR, name + ".json"))));
      const rel = [...layoutComponents(p), episodeId(p) + ".md"].join("/");
      assert.equal(rel, vec.path);
    });
  }
});

interface ConfigVector {
  world: string;
  scope: string;
  before: boolean;
  outcome: string;
}

// Replays saveCaptureDefaults against the byte fixtures in
// testdata/golden/config/. The rewritten config file is a frozen contract:
// an owner's hand-maintained file must survive a rewrite with its key
// order, number normalization, escaping, and indentation intact, so that
// the only thing a default-command run changes is the value it was asked
// to change.
test("golden config vectors", async (t) => {
  const configVectors: Record<string, ConfigVector> = JSON.parse(
    fs.readFileSync(path.join(GOLDEN_DIR, "config-vectors.json"), "utf8"),
  );
  for (const [name, vec] of Object.entries(configVectors)) {
    await t.test(name, () => {
      const dir = fs.mkdtempSync(path.join(os.tmpdir(), "aj-config-"));
      try {
        const configPath = path.join(dir, "config.json");
        let before: Buffer | null = null;
        if (vec.before) {
          before = fs.readFileSync(path.join(GOLDEN_DIR, "config", name + ".before.json"));
          fs.writeFileSync(configPath, before, { mode: 0o600 });
        }
        const env = mapEnviron({});
        if (vec.outcome === "ok") {
          saveCaptureDefaults(env, configPath, vec.world, vec.scope);
          const want = fs.readFileSync(path.join(GOLDEN_DIR, "config", name + ".after.json"));
          const got = fs.readFileSync(configPath);
          assert.equal(got.toString("utf8"), want.toString("utf8"));
        } else {
          assert.equal(vec.outcome, "malformed", `unhandled vector outcome ${vec.outcome}`);
          assert.throws(
            () => saveCaptureDefaults(env, configPath, vec.world, vec.scope),
            (err: unknown) => err instanceof ConfigError && err.code === "malformed",
          );
          const got = fs.readFileSync(configPath);
          assert.ok(before !== null && got.equals(before), "malformed config was modified");
        }
      } finally {
        fs.rmSync(dir, { recursive: true, force: true });
      }
    });
  }
});
