// The one-capability-per-module map, held to by a test rather than a
// review note (the equivalent of the Go tree's ownership_test.go): the
// engine's module list is closed, and each load-bearing exported symbol
// lives in exactly the module the architecture map says owns it.

import { test } from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";
import { fileURLToPath } from "node:url";

const SRC = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "..", "src");

const MODULES = [
  "aliases.ts",
  "config.ts",
  "contracts.ts",
  "corpus.ts",
  "episode.ts",
  "identity.ts",
  "index.ts",
  "json.ts",
  "ops-alias.ts",
  "ops.ts",
  "paths.ts",
  "render.ts",
  "retrieval.ts",
  "search.ts",
  "store.ts",
];

// One anchor symbol per capability seam. Not exhaustive: the point is that
// a capability cannot silently migrate or duplicate, not to inventory
// every export.
const OWNERSHIP: Record<string, string[]> = {
  "contracts.ts": ["parsePayload", "validate", "validToken", "validScope", "CaptureError"],
  "json.ts": ["parseOrderedJson"],
  "identity.ts": ["episodeId", "payloadDigestHex"],
  "render.ts": ["render", "isoFromMs", "frontmatterDigestHex"],
  "episode.ts": ["parseEpisode", "verifyEpisode", "resealDigestHex"],
  "paths.ts": ["defaultJournalRoot", "defaultIndexPath", "rootDigestHex", "thesaurusPath", "missLogPath"],
  "corpus.ts": ["openJournalRoot", "walkCorpus", "readContained", "containedPath", "rootInSharedDirectory"],
  "config.ts": ["parseConfig", "saveCaptureDefaults", "resolveConfigPath"],
  "store.ts": ["publish", "capture", "applyOversizePolicy", "checkRedelivery", "findPriorPolicyCapture"],
  "index.ts": ["openSnapshot", "syncSnapshot", "freshnessOf", "withIndexLock", "corpusStatSignature"],
  "retrieval.ts": ["extractTerms", "tokenizeLine", "idfWeight", "rank", "cursorEncode", "cursorDecode"],
  "aliases.ts": ["loadAliasMapFromBytes", "loadAliasMapFile", "aliasGet"],
  "search.ts": ["search", "get", "creditLine"],
  "ops.ts": ["statusOf", "sync", "reseal", "catalog"],
  "ops-alias.ts": ["addAlias", "removeAlias", "aggregateMisses", "logSearchMiss"],
};

test("the engine module list is closed", () => {
  const files = fs
    .readdirSync(SRC)
    .filter((name) => name.endsWith(".ts"))
    .sort();
  assert.deepEqual(files, MODULES, "a new engine module joins the architecture map (and this pin) on purpose");
});

test("each anchor symbol is exported by its owning module and no other", async () => {
  const exportsByModule = new Map<string, Set<string>>();
  for (const name of MODULES) {
    const mod = (await import(path.join(SRC, name))) as Record<string, unknown>;
    exportsByModule.set(name, new Set(Object.keys(mod)));
  }
  for (const [owner, symbols] of Object.entries(OWNERSHIP)) {
    for (const symbol of symbols) {
      assert.ok(exportsByModule.get(owner)?.has(symbol), `${symbol} must live in ${owner}`);
      for (const [other, names] of exportsByModule) {
        if (other === owner) continue;
        assert.ok(!names.has(symbol), `${symbol} is owned by ${owner} but also exported by ${other}`);
      }
    }
  }
});

test("every module opens with a why-it-exists header comment", () => {
  for (const name of MODULES) {
    const first = fs.readFileSync(path.join(SRC, name), "utf8").split("\n")[0];
    assert.ok(first.startsWith("//"), `${name} must open with its capability header`);
  }
});
