// Unit cases ported from the Go engine's suites: the closed payload
// schema, the frozen config acceptance, episode parsing and verification
// edges, and path resolution. The golden and property suites carry the
// corpus-durable pins; these pin the typed failure vocabulary and the
// acceptance edges around them.

import { test } from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";
import * as os from "node:os";

import {
  parsePayload,
  validate,
  CaptureError,
  captureErrorName,
  validToken,
  validScope,
  validWorld,
  validPath,
  type RawPayload,
  type Payload,
} from "../../src/contracts.ts";
import { parseConfig, ConfigError, resolveConfigPath, saveCaptureDefaults, formatPositional, goParseFloat } from "../../src/config.ts";
import { parseEpisode, verifyEpisode, resealDigestHex } from "../../src/episode.ts";
import { render, frontmatterDigestHex, isoFromMs } from "../../src/render.ts";
import { episodeId, payloadDigestHex } from "../../src/identity.ts";
import {
  stateDir,
  defaultJournalRoot,
  thesaurusPath,
  missLogPath,
  resolveJournalRoot,
  rootDigestHex,
  MissingHomeError,
} from "../../src/paths.ts";
import { mapEnviron } from "./helpers.ts";

const enc = new TextEncoder();

function basePayload(overrides: Record<string, unknown> = {}): Uint8Array {
  const doc: Record<string, unknown> = {
    schema_version: 1,
    world: "main",
    scope: "default",
    lane: "conversation",
    harness: "pi",
    adapter_version: "2.0.0",
    session_id: "s-1",
    turn_id: "t-1",
    event_time_ms: 1785240000000,
    capture_policy: "default-v1",
    turn_outcome: "completed",
    user_content: "hello",
    assistant_result: "world",
    ...overrides,
  };
  for (const [k, v] of Object.entries(doc)) {
    if (v === undefined) delete doc[k];
  }
  return enc.encode(JSON.stringify(doc));
}

function validationCode(bytes: Uint8Array): string | null {
  try {
    validate(parsePayload(bytes));
    return null;
  } catch (err) {
    return captureErrorName(err);
  }
}

test("payload parse rejections collapse to Malformed", () => {
  const cases: [string, Uint8Array][] = [
    ["duplicate key", enc.encode('{"schema_version": 1, "schema_version": 1}')],
    ["truncated document", basePayload().slice(0, -1)],
    ["trailing garbage", enc.encode('{"schema_version": 1} extra')],
    ["top-level array", enc.encode("[]")],
    ["unknown field", basePayload({ surprise: 1 })],
    ["missing required field", enc.encode('{"schema_version": 1}')],
    ["string schema_version", basePayload({ schema_version: "1" })],
    ["float schema_version", enc.encode(new TextDecoder().decode(basePayload()).replace('"schema_version":1', '"schema_version":1.0'))],
    ["negative event_time_ms", basePayload({ event_time_ms: -1 })],
    ["null user_content", basePayload({ user_content: null })],
    ["tools with extra key", basePayload({ tools: [{ name: "Bash", extra: 1 }] })],
    ["tools with missing name", basePayload({ tools: [{}] })],
    ["tools with null name", basePayload({ tools: [{ name: null }] })],
    ["tools non-array", basePayload({ tools: "Bash" })],
    ["numeric world", basePayload({ world: 5 })],
  ];
  for (const [name, bytes] of cases) {
    assert.throws(
      () => parsePayload(bytes),
      (err: unknown) => err instanceof CaptureError && err.code === "Malformed",
      name,
    );
  }
});

test("duplicate keys are rejected at any depth", () => {
  const doc = '{"schema_version": 1, "tools": [{"name": "a", "name": "a"}]}';
  assert.throws(() => parsePayload(enc.encode(doc)));
});

test("validation reports the first failure in contract order", () => {
  assert.equal(validationCode(basePayload()), null);
  assert.equal(validationCode(basePayload({ schema_version: 2 })), "UnsupportedSchemaVersion");
  assert.equal(validationCode(basePayload({ world: undefined })), "InvalidWorld", "validate alone demands a world");
  assert.equal(validationCode(basePayload({ world: "Bad World" })), "InvalidWorld");
  assert.equal(validationCode(basePayload({ scope: "has space" })), "InvalidScope");
  assert.equal(validationCode(basePayload({ scope: ".hidden" })), "InvalidScope");
  assert.equal(validationCode(basePayload({ scope: "a/b" })), "InvalidScope");
  assert.equal(validationCode(basePayload({ lane: "unknown" })), "InvalidLane");
  assert.equal(validationCode(basePayload({ event_time_ms: 253402300800000 })), "ImplausibleEventTime");
  assert.equal(validationCode(basePayload({ harness: "bad harness" })), "InvalidHarness");
  assert.equal(validationCode(basePayload({ adapter_version: "" })), "InvalidAdapterVersion");
  assert.equal(validationCode(basePayload({ session_id: "bad session" })), "InvalidSessionId");
  assert.equal(validationCode(basePayload({ turn_id: "bad turn" })), "InvalidTurnId");
  assert.equal(validationCode(basePayload({ capture_policy: "bad policy" })), "InvalidCapturePolicy");
  assert.equal(validationCode(basePayload({ turn_outcome: "bad outcome" })), "InvalidTurnOutcome");
  assert.equal(validationCode(basePayload({ user_content: "" })), "EmptyUserContent");
  assert.equal(validationCode(basePayload({ assistant_result: "" })), "EmptyAssistantResult");
  assert.equal(validationCode(basePayload({ user_content: "x".repeat(2 * 1024 * 1024 + 1) })), "OversizedContent");
  assert.equal(validationCode(basePayload({ tools: Array(257).fill({ name: "t" }) })), "TooManyTools");
  assert.equal(validationCode(basePayload({ tools: [{ name: "bad tool" }] })), "InvalidToolName");
  assert.equal(validationCode(basePayload({ workspace_root: "\t" })), "InvalidWorkspaceRoot");
  assert.equal(validationCode(basePayload({ branch_of: "line\nbreak" })), "InvalidBranchOf");
  assert.equal(validationCode(basePayload({ host: "bad host" })), "InvalidHost");
});

test("multibyte content budgets count bytes, not code units", () => {
  const glyphs = "\u{1F600}".repeat((2 * 1024 * 1024) / 4 + 1);
  assert.equal(validationCode(basePayload({ user_content: glyphs })), "OversizedContent");
});

test("lone surrogates in content are InvalidUtf8 for a direct caller", () => {
  const raw = validate(parsePayload(basePayload()));
  const direct: RawPayload = {
    schemaVersion: 1,
    world: "main",
    scope: "default",
    lane: "conversation",
    harness: raw.harness,
    adapterVersion: raw.adapterVersion,
    sessionId: raw.sessionId,
    turnId: raw.turnId,
    eventTimeMs: 1785240000000n,
    capturePolicy: raw.capturePolicy,
    turnOutcome: raw.turnOutcome,
    userContent: "broken \ud800 surrogate",
    assistantResult: "ok",
    tools: null,
    workspaceRoot: null,
    branchOf: null,
    host: null,
  };
  assert.throws(
    () => validate(direct),
    (err: unknown) => err instanceof CaptureError && err.code === "InvalidUtf8",
  );
});

test("escaped unpaired surrogates decode to U+FFFD, matching the Go corpus", () => {
  const doc = new TextDecoder().decode(basePayload({ user_content: "x" })).replace('"x"', '"\\ud800"');
  const raw = parsePayload(enc.encode(doc));
  assert.equal(raw.userContent, "�");
});

test("identity and digest cover the documented fields", () => {
  const p = validate(parsePayload(basePayload()));
  const id = episodeId(p);
  const otherScope: Payload = { ...p, scope: "other" };
  const otherWorld: Payload = { ...p, world: "other" };
  const otherAdapter: Payload = { ...p, adapterVersion: "9.9.9" };
  assert.equal(episodeId(otherScope), id, "scope does not participate in identity");
  assert.notEqual(episodeId(otherWorld), id);
  const digest = payloadDigestHex(p);
  assert.notEqual(payloadDigestHex(otherScope), digest, "scope participates in the digest");
  assert.equal(payloadDigestHex(otherAdapter), digest, "adapter_version is excluded");
});

test("token charsets", () => {
  assert.ok(validToken("a.b_c-d:e+f/g@h"));
  assert.ok(!validToken(""));
  assert.ok(!validToken("has space"));
  assert.ok(!validToken("ünïcode"));
  assert.ok(validScope("workspace:demo"));
  assert.ok(!validScope("../../escape"));
  assert.ok(!validScope(".hidden"));
  assert.ok(validWorld("w0r1d-x"));
  assert.ok(!validWorld("UPPER"));
  assert.ok(validPath("/home/user/my project"));
  assert.ok(!validPath("a".repeat(513)));
  assert.ok(validPath("ünïcode/påth"));
});

test("closed config schema rejections", () => {
  const cases: [string, string][] = [
    ["unknown key", '{"journal_root": "/j", "surprise": 1}'],
    ["duplicate key", '{"journal_root": "/j", "journal_root": "/k"}'],
    ["relative journal root", '{"journal_root": "relative/path"}'],
    ["relative thesaurus path", '{"journal_root": "/j", "thesaurus_path": "thesaurus.json"}'],
    ["invalid default world", '{"journal_root": "/j", "default_world": "Bad World"}'],
    ["zero context window", '{"journal_root": "/j", "context_window": 0}'],
    ["over-budget context window", '{"journal_root": "/j", "context_window": 11}'],
    ["zero max results", '{"journal_root": "/j", "max_results": 0}'],
    ["invalid capture world", '{"journal_root": "/j", "capture": {"world": "Bad World"}}'],
    ["invalid capture scope", '{"journal_root": "/j", "capture": {"scope": "has space"}}'],
    ["capture null", '{"journal_root": "/j", "capture": null}'],
    ["capture unknown key", '{"journal_root": "/j", "capture": {"world": "main", "zz": 1}}'],
    ["capture world number", '{"capture": {"world": 5}}'],
    ["conflicting roots", '{"journal_root": "/new", "world_root": "/old"}'],
    ["non-object root", "[]"],
    ["number for string field", '{"journal_root": 5}'],
    ["string for bool field", '{"journal_root": "/j", "miss_log": "true"}'],
    ["fractional int", '{"journal_root": "/j", "context_window": 3.5}'],
    ["u32 overflow", '{"journal_root": "/j", "context_window": 4294967296}'],
    ["negative unsigned", '{"journal_root": "/j", "context_window": -1}'],
    ["negative float unsigned", '{"journal_root": "/j", "context_window": -1.0}'],
    ["null for int field", '{"journal_root": "/j", "context_window": null}'],
    ["null capture world", '{"capture": {"world": null}}'],
    ["empty journal root", '{"journal_root": ""}'],
    ["empty world root", '{"world_root": ""}'],
    ["empty thesaurus path", '{"thesaurus_path": ""}'],
    ["empty default world", '{"default_world": ""}'],
    ["empty capture world", '{"capture": {"world": ""}}'],
    ["trailing garbage", '{"journal_root": "/j"} extra'],
    ["negative min score", '{"journal_root": "/j", "min_score": -0.5}'],
    ["non-finite number", '{"recency_boost": 1e999}'],
    ["non-finite string", '{"recency_boost": "inf"}'],
    ["underscored uint string", '{"context_window": "1_0"}'],
    ["-0 int (coerces to 0)", '{"context_window": -0}'],
    ["u64 max+1 float", '{"miss_log_max_bytes": 18446744073709551616.0}'],
  ];
  for (const [name, doc] of cases) {
    assert.throws(
      () => parseConfig(doc),
      (err: unknown) => err instanceof ConfigError && err.code === "malformed",
      name,
    );
  }
  assert.throws(() => parseConfig(Buffer.from([0x7b, 0x22, 0x6a, 0xff, 0x22, 0x3a, 0x31, 0x7d])));
});

test("frozen config numeric coercions", () => {
  assert.equal(parseConfig('{"context_window": "5"}').contextWindow, 5);
  assert.equal(parseConfig('{"context_window": 3.0}').contextWindow, 3);
  assert.equal(parseConfig('{"context_window": 3e0}').contextWindow, 3);
  assert.equal(parseConfig('{"context_window": "3.0"}').contextWindow, 3);
  assert.equal(parseConfig('{"miss_log_max_bytes": "18446744073709551615"}').missLogMaxBytes, 18446744073709551615n);
  assert.equal(parseConfig('{"recency_boost": "1.5"}').recencyBoost, 1.5);
  assert.equal(parseConfig('{"context_window": "010"}').contextWindow, 10);
});

test("config defaults and known keys", () => {
  const cfg = parseConfig('{"journal_root": "/tmp/journals"}');
  assert.equal(cfg.contextWindow, 3);
  assert.equal(cfg.maxResults, 10);
  assert.equal(cfg.missLog, false);
  assert.equal(cfg.missLogMaxBytes, 1024n * 1024n);
  assert.deepEqual(cfg.capture, { world: "main", scope: "default" });
  assert.equal(cfg.recencyBoost, 1.0);
  assert.equal(cfg.minScore, 0.0);
  assert.equal(cfg.confidenceFloor, 3.0);
  const legacy = parseConfig('{"world_root": "/tmp/legacy-journals"}');
  assert.equal(legacy.journalRoot, "/tmp/legacy-journals");
  const captureOnly = parseConfig('{"capture": {"world": "team", "scope": "default"}}');
  assert.equal(captureOnly.journalRoot, "");
  assert.equal(captureOnly.capture.world, "team");
  assert.equal(parseConfig('{"journal_root": null}').journalRoot, "");
});

test("config path resolution order", () => {
  const env = mapEnviron({
    AUTOJOURNAL_CONFIG: "/env/config.json",
    XDG_CONFIG_HOME: "/xdg",
    HOME: "/home/x",
  });
  assert.equal(resolveConfigPath(env, "/explicit/config.json"), "/explicit/config.json");
  assert.equal(resolveConfigPath(env, ""), "/env/config.json");
  assert.equal(resolveConfigPath(mapEnviron({ XDG_CONFIG_HOME: "/xdg", HOME: "/home/x" }), ""), "/xdg/autojournal/config.json");
  assert.equal(resolveConfigPath(mapEnviron({ XDG_CONFIG_HOME: "relative", HOME: "/home/x" }), ""), "/home/x/.config/autojournal/config.json");
  assert.equal(resolveConfigPath(mapEnviron({ HOME: "/home/x" }), ""), "/home/x/.config/autojournal/config.json");
  assert.throws(
    () => resolveConfigPath(mapEnviron({}), ""),
    (err: unknown) => err instanceof ConfigError && err.code === "not_found",
  );
  assert.throws(() => resolveConfigPath(mapEnviron({ HOME: "" }), ""), MissingHomeError);
});

test("save validates the rewritten document and refuses bad defaults", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "aj-save-"));
  try {
    const p = path.join(dir, "config.json");
    assert.throws(() => saveCaptureDefaults(mapEnviron({}), p, "Bad World", "default"));
    assert.ok(!fs.existsSync(p));
    const written = saveCaptureDefaults(mapEnviron({}), p, "team", "default");
    assert.equal(written, p);
    const cfg = parseConfig(fs.readFileSync(p));
    assert.deepEqual(cfg.capture, { world: "team", scope: "default" });
    assert.ok(fs.readFileSync(p, "utf8").endsWith("\n"));
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test("Go float grammar for string-typed config numbers", () => {
  assert.equal(goParseFloat("0x1p-2")?.value, 0.25);
  assert.equal(goParseFloat("1_0.5")?.value, 10.5);
  assert.equal(goParseFloat("+3.0")?.value, 3);
  assert.equal(goParseFloat(".5")?.value, 0.5);
  assert.equal(goParseFloat("5.")?.value, 5);
  assert.equal(goParseFloat("Infinity")?.value, Infinity);
  assert.equal(goParseFloat("0x10"), null);
  assert.equal(goParseFloat("0b101"), null);
  assert.equal(goParseFloat(" 3"), null);
  assert.equal(goParseFloat("3 "), null);
  assert.equal(goParseFloat(""), null);
  assert.equal(goParseFloat("1__0"), null);
  assert.equal(goParseFloat("_10"), null);
  assert.equal(goParseFloat("10_"), null);
  assert.equal(goParseFloat("1e"), null);
  const overflow = goParseFloat("1e999");
  assert.equal(overflow?.value, Infinity);
  assert.equal(overflow?.rangeErr, true);
});

test("positional float formatting never goes scientific", () => {
  assert.equal(formatPositional(1e-10), "0.0000000001");
  assert.equal(formatPositional(1e21), "1" + "0".repeat(21));
  assert.equal(formatPositional(1.5), "1.5");
  assert.equal(formatPositional(-0), "-0");
  assert.equal(formatPositional(3), "3");
  assert.equal(formatPositional(1.2345e-5), "0.000012345");
  assert.equal(formatPositional(-2.5e22), "-25000000000000000000000");
});

test("episode parse and verify edges", () => {
  const p = validate(parsePayload(basePayload()));
  const content = render({ payload: p, episodeId: episodeId(p), digestHex: payloadDigestHex(p), captureTimeMs: 1785240000500 });
  const ep = parseEpisode(content);
  assert.notEqual(ep, null);
  assert.equal(ep!.episodeId, episodeId(p));
  assert.equal(ep!.eventTimeMs, p.eventTimeMs);
  assert.equal(content.split("\n")[ep!.bodyLine - 1], "");
  const v = verifyEpisode(content);
  assert.ok(v.ok && v.episode.userContent === "hello" && v.episode.assistantResult === "world");
  const edited = content.replace("hello", "edited");
  assert.notEqual(parseEpisode(edited), null);
  const failed = verifyEpisode(edited);
  assert.ok(!failed.ok && failed.failure === "digest_mismatch");
  const resealed = resealDigestHex(edited);
  assert.notEqual(resealed, null);
  const reattested = edited.replace(ep!.digestHex, resealed!);
  assert.ok(verifyEpisode(reattested).ok);
  const relabeled = content.replace(ep!.episodeId, "aj1-" + "0".repeat(32));
  const mismatch = verifyEpisode(relabeled);
  assert.ok(!mismatch.ok && mismatch.failure === "digest_mismatch");
  assert.equal(resealDigestHex(relabeled), null);
  assert.equal(parseEpisode(content.slice(0, 40)), null);
  assert.equal(parseEpisode(content.replace("world: main\n", "")), null);
  const broken = verifyEpisode("---\nnot frontmatter");
  assert.ok(!broken.ok && broken.failure === "episode_malformed");
});

test("separator-heavy content verifies: the interpretation cap scales with body size", () => {
  const quoted = "quoting a transcript:" + "\n\n## Assistant\n\n".repeat(500) + "done";
  const p = validate(parsePayload(basePayload({ user_content: quoted })));
  const content = render({ payload: p, episodeId: episodeId(p), digestHex: payloadDigestHex(p), captureTimeMs: 1785240000500 });
  const v = verifyEpisode(content);
  assert.ok(v.ok);
  assert.equal(v.ok && v.episode.userContent, quoted);
});

test("frontmatter digest extraction", () => {
  const p = validate(parsePayload(basePayload()));
  const digest = payloadDigestHex(p);
  const content = render({ payload: p, episodeId: episodeId(p), digestHex: digest, captureTimeMs: 1785240000500 });
  assert.equal(frontmatterDigestHex(content), digest);
  assert.equal(frontmatterDigestHex("no frontmatter"), null);
  assert.equal(frontmatterDigestHex("---\n---\n"), null);
  assert.equal(frontmatterDigestHex("---\npayload_digest: sha256:short\n---\n"), null);
});

test("iso timestamps render at second precision", () => {
  assert.equal(isoFromMs(0), "1970-01-01T00:00:00Z");
  assert.equal(isoFromMs(1785240000999), "2026-07-28T12:00:00Z");
  assert.equal(isoFromMs(253402300799000), "9999-12-31T23:59:59Z");
});

test("journal path derivations", () => {
  const env = mapEnviron({ HOME: "/home/x" });
  assert.equal(stateDir(env), "/home/x/.local/state");
  assert.equal(stateDir(mapEnviron({ HOME: "/home/x", XDG_STATE_HOME: "/xs" })), "/xs");
  assert.equal(stateDir(mapEnviron({ HOME: "/home/x", XDG_STATE_HOME: "relative" })), "/home/x/.local/state");
  assert.equal(defaultJournalRoot(env), "/home/x/.local/share/autojournal/journals");
  assert.equal(defaultJournalRoot(mapEnviron({ XDG_DATA_HOME: "/xd", HOME: "/home/x" })), "/xd/autojournal/journals");
  assert.throws(() => stateDir(mapEnviron({})), MissingHomeError);
  assert.throws(() => stateDir(mapEnviron({ HOME: "" })), MissingHomeError);
  assert.equal(thesaurusPath(env, "/cfg/thesaurus.json"), "/cfg/thesaurus.json");
  assert.equal(thesaurusPath(mapEnviron({ HOME: "/home/x", AUTOJOURNAL_THESAURUS: "/t.json" }), ""), "/t.json");
  assert.equal(thesaurusPath(env, ""), "/home/x/.config/autojournal/thesaurus.json");
  assert.equal(missLogPath(env), "/home/x/.local/state/autojournal/thesaurus-candidates.jsonl");
  assert.equal(missLogPath(mapEnviron({ HOME: "/home/x", AUTOJOURNAL_MISS_LOG: "/m.jsonl" })), "/m.jsonl");
});

test("journal root canonicalization keys one index per root", () => {
  // Separator direction is platform-specific (Go's filepath.Clean also
  // flips to backslashes on Windows); the product property is that every
  // spelling of one root derives one digest.
  const spellings = ["/a/b/", "/a//b", "/a/./b", "/a/b"];
  for (const s of spellings) {
    assert.equal(resolveJournalRoot(s), resolveJournalRoot("/a/b"), s);
    assert.equal(rootDigestHex(s), rootDigestHex("/a/b"), s);
  }
  assert.equal(resolveJournalRoot("/"), path.normalize("/"));
  assert.notEqual(rootDigestHex("/a/b"), rootDigestHex("/a/c"));
  if (process.platform !== "win32") {
    assert.equal(resolveJournalRoot("/a/b/"), "/a/b");
  }
});
