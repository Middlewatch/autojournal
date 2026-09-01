// End-to-end retrieval smoke through the installed node bin, portable
// across every platform CI runs: capture two episodes, search (exact,
// boundary-credited, and alias-rescued), open evidence, then prove
// stale_revision and typed no_match. Isolated root/index/thesaurus.
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const smoke = fs.mkdtempSync(path.join(os.tmpdir(), "aj-smoke-"));
const journal = path.join(smoke, "root");
const index = path.join(smoke, "index.v2.json");

const env = {
  ...process.env,
  AUTOJOURNAL_THESAURUS: path.join(smoke, "thesaurus.json"),
  AUTOJOURNAL_MISS_LOG: path.join(smoke, "misses.jsonl"),
};
const baseArgs = ["--root", journal, "--index", index, "--world", "smokeworld"];

function aj(args, stdin) {
  const result = spawnSync(process.execPath, [path.join(root, "bin", "autojournal"), ...args], {
    input: stdin,
    encoding: "utf8",
    env,
  });
  return { code: result.status, stdout: result.stdout ?? "", stderr: result.stderr ?? "" };
}

function expect(condition, label, run) {
  if (!condition) {
    console.error(`smoke FAIL: ${label}\nstdout: ${run?.stdout}\nstderr: ${run?.stderr}`);
    process.exitCode = 1;
    throw new Error(label);
  }
}

function payload(session, turn, user, assistant) {
  return JSON.stringify({
    schema_version: 1,
    world: "smokeworld",
    scope: "global",
    lane: "conversation",
    harness: "verify",
    adapter_version: "0.0.0",
    session_id: session,
    turn_id: turn,
    event_time_ms: 1785240000000,
    capture_policy: "default-v1",
    turn_outcome: "completed",
    user_content: user,
    assistant_result: assistant,
  });
}

try {
  let run = aj(["capture", "--root", journal, "--index", index], payload("s1", "t1", "the zephyr firmware needed a fwupd refresh", "Refreshed."));
  expect(run.stdout.includes('"outcome":"published"'), "capture t1 publishes", run);
  run = aj(["capture", "--root", journal, "--index", index], payload("s2", "t2", "reindexing the corpus took four seconds", "Done."));
  expect(run.stdout.includes('"outcome":"published"'), "capture t2 publishes", run);

  run = aj(["search", "fwupd", ...baseArgs, "--json"]);
  expect(run.stdout.includes('"outcome":"match"'), "exact search matches", run);
  const episode = run.stdout.match(/"episode_id":"([^"]+)"/)?.[1];
  const revision = run.stdout.match(/"revision":"([^"]+)"/)?.[1];
  expect(episode !== undefined && revision !== undefined, "search reports identity", run);

  run = aj(["search", "index", ...baseArgs, "--json"]);
  expect(run.stdout.includes('"outcome":"no_match"'), "word-start refuses infix", run);
  run = aj(["search", "index", ...baseArgs, "--credit-mode", "substring", "--json"]);
  expect(run.stdout.includes('"outcome":"match"'), "substring mode credits infix", run);
  run = aj(["search", "reindex", ...baseArgs, "--json"]);
  expect(run.stdout.includes('"outcome":"match"'), "word-start prefix credits", run);

  run = aj(["search", "hardware", ...baseArgs, "--json"]);
  expect(run.stdout.includes('"outcome":"no_match"'), "unaliased casual term misses", run);
  run = aj(["alias", "add", "hardware", "fwupd"]);
  expect(run.code === 0, "alias add succeeds", run);
  run = aj(["search", "hardware", ...baseArgs, "--json"]);
  expect(run.stdout.includes('"alias_terms":["fwupd"]'), "alias rescues the search", run);
  run = aj(["alias", "remove", "hardware"]);
  expect(run.code === 0, "alias remove succeeds", run);

  run = aj(["get", "--episode", episode, "--revision", revision, ...baseArgs, "--json"]);
  expect(run.stdout.includes('"outcome":"match"'), "get serves the requested revision", run);
  let episodeFile = null;
  (function find(dir) {
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
      const p = path.join(dir, entry.name);
      if (entry.isDirectory()) find(p);
      else if (entry.name === `${episode}.md`) episodeFile = p;
    }
  })(journal);
  expect(episodeFile !== null, "published episode file exists");
  fs.writeFileSync(episodeFile, fs.readFileSync(episodeFile, "utf8").replace("Refreshed.", "Refreshed twice."));
  run = aj(["get", "--episode", episode, "--revision", revision, ...baseArgs, "--json"]);
  expect(run.stdout.includes('"outcome":"stale_revision"'), "an edit reports stale_revision", run);

  run = aj(["search", "zeppelin", ...baseArgs, "--json"]);
  expect(run.stdout.includes('"outcome":"no_match"') && run.code === 0, "typed no_match exits 0", run);

  console.log("e2e smoke: PASS");
} finally {
  fs.rmSync(smoke, { recursive: true, force: true });
}
