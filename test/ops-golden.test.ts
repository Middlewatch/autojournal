// Golden CLI --json ops samples: the Interface-tier pins for capture
// redelivery, status, catalog, sync, reseal, and version, replayed against
// a corpus built deterministically from the golden payload fixtures.
// Absolute paths are normalized to $ROOT/$INDEX placeholders so the pinned
// bytes are machine-independent.
//
// Re-minting (an interface change made on purpose, never an accident):
// AUTOJOURNAL_MINT_OPS_SAMPLES=1 npm test -- test/ops-golden.test.ts

import { test } from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";
import { fileURLToPath } from "node:url";
import * as os from "node:os";

import { run, type CliIo } from "../cli.ts";
import { parseEpisode } from "../src/episode.ts";

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const GOLDEN_DIR = path.join(REPO_ROOT, "testdata", "golden");
const SAMPLES_DIR = path.join(GOLDEN_DIR, "ops-samples");
const MINT = process.env.AUTOJOURNAL_MINT_OPS_SAMPLES === "1";

interface CaptureVector {
  outcome: string;
}

function cliRun(env: Record<string, string>, nowMs: number, args: string[], stdin?: Uint8Array) {
  let stdout = "";
  let stderr = "";
  const io: CliIo = {
    env: (key) => env[key],
    stdin: () => stdin ?? new Uint8Array(),
    stdout: (t) => {
      stdout += t;
    },
    stderr: (t) => {
      stderr += t;
    },
    nowMs: () => nowMs,
  };
  const code = run(args, io);
  return { code, stdout, stderr };
}

function checkSample(name: string, got: string): void {
  const file = path.join(SAMPLES_DIR, name);
  if (MINT) {
    fs.writeFileSync(file, got);
    return;
  }
  assert.equal(got, fs.readFileSync(file, "utf8"), `ops sample ${name} diverged — a --json interface change must be re-minted on purpose`);
}

test("golden CLI ops samples", {
  // The pinned samples replay colon-scoped captures NTFS cannot represent.
  skip: process.platform === "win32" && "pinned colon-scope samples not representable on NTFS",
}, () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "aj-ops-golden-"));
  try {
    const rootPath = path.join(dir, "root");
    const indexPath = path.join(dir, "index.v2.json");
    const env = { HOME: path.join(dir, "home") };
    const rootArgs = ["--root", rootPath, "--index", indexPath];
    const normalize = (text: string): string =>
      text.split(rootPath).join("$ROOT").split(indexPath).join("$INDEX");

    const vectors: Record<string, CaptureVector> = JSON.parse(
      fs.readFileSync(path.join(GOLDEN_DIR, "capture-vectors.json"), "utf8"),
    );
    for (const [name, vec] of Object.entries(vectors)) {
      if (vec.outcome !== "published") continue;
      const ep = parseEpisode(fs.readFileSync(path.join(GOLDEN_DIR, "episodes", name + ".md"), "utf8"));
      assert.notEqual(ep, null);
      const payload = fs.readFileSync(path.join(REPO_ROOT, "testdata", "payloads", name + ".json"));
      const captured = cliRun(env, ep!.captureTimeMs, ["capture", ...rootArgs], payload);
      assert.equal(JSON.parse(captured.stdout).outcome, "published", name);
    }

    const basic = JSON.parse(fs.readFileSync(path.join(REPO_ROOT, "testdata", "payloads", "basic.json"), "utf8"));
    const enc = new TextEncoder();
    const dup = cliRun(env, 1785240001000, ["capture", ...rootArgs], enc.encode(JSON.stringify(basic)));
    checkSample("capture-duplicate.json", dup.stdout);
    const divergent = { ...basic, assistant_result: "They fail." };
    const conflict = cliRun(env, 1785240002000, ["capture", ...rootArgs], enc.encode(JSON.stringify(divergent)));
    assert.equal(conflict.code, 3);
    checkSample("capture-conflict.json", conflict.stdout);

    checkSample("sync.json", cliRun(env, 1785240003000, ["sync", "--json", ...rootArgs]).stdout);
    checkSample("status.json", normalize(cliRun(env, 1785240004000, ["status", "--json", ...rootArgs]).stdout));
    checkSample("catalog.json", cliRun(env, 1785240005000, ["catalog", ...rootArgs]).stdout);
    checkSample("reseal.json", cliRun(env, 1785240006000, ["reseal", "--json", ...rootArgs]).stdout);
    checkSample("version.txt", cliRun(env, 0, ["version"]).stdout);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});
