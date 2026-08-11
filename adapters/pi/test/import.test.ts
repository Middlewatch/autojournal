import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import {
  buildPayload,
  eventTimeFromEntries,
  formatImportSummary,
  importableSessionHeader,
  importPiHistory,
  listPiSessionFiles,
  parsePiSession,
  piSessionsRoot,
  readFirstLine,
  resolveBinary,
  runBinary,
  sessionIdFromFile,
  SESSION_POLICY_ENTRY,
} from "../index.ts";

const e2eBinary = resolveBinary({
  AUTOJOURNAL_BIN: path.join(
    import.meta.dirname,
    "..",
    "bin",
    `${process.platform}-${process.arch}`,
    "autojournal",
  ),
  PATH: process.env.PATH,
});

function jsonl(entries: unknown[]): string {
  return entries.map((e) => JSON.stringify(e)).join("\n") + "\n";
}

function header(extra: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    type: "session",
    version: 3,
    id: "0192aaaa-0000-7000-8000-000000000001",
    timestamp: "2026-07-01T10:00:00.000Z",
    cwd: "/home/user/project",
    ...extra,
  };
}

function userMsg(id: string, ts: string, text: string): Record<string, unknown> {
  return {
    type: "message",
    id,
    timestamp: ts,
    message: { role: "user", content: [{ type: "text", text }] },
  };
}

function assistantMsg(id: string, ts: string, text: string, tools: string[] = []): Record<string, unknown> {
  return {
    type: "message",
    id,
    timestamp: ts,
    message: {
      role: "assistant",
      content: [
        { type: "text", text },
        ...tools.map((name) => ({ type: "toolCall", name })),
      ],
    },
  };
}

function toolResult(id: string, ts: string): Record<string, unknown> {
  return {
    type: "message",
    id,
    timestamp: ts,
    message: { role: "toolResult", content: [{ type: "text", text: "raw output must not leak" }] },
  };
}

test("piSessionsRoot honors PI_CODING_AGENT_DIR and defaults under home", () => {
  assert.equal(piSessionsRoot({ PI_CODING_AGENT_DIR: "/x/agent" }), path.join("/x/agent", "sessions"));
  assert.equal(piSessionsRoot({}), path.join(os.homedir(), ".pi", "agent", "sessions"));
});

test("parsePiSession pairs turns and pins identity at the final assistant entry", () => {
  const parsed = parsePiSession(
    jsonl([
      header(),
      { type: "model_change", id: "m1", timestamp: "2026-07-01T10:00:01.000Z" },
      userMsg("u1", "2026-07-01T10:01:00.000Z", "first question"),
      assistantMsg("a1", "2026-07-01T10:01:05.000Z", "working", ["bash"]),
      toolResult("t1", "2026-07-01T10:01:06.000Z"),
      assistantMsg("a2", "2026-07-01T10:01:10.000Z", "first answer", ["read"]),
      userMsg("u2", "2026-07-01T10:02:00.000Z", "second question"),
      assistantMsg("a3", "2026-07-01T10:02:05.000Z", "second answer"),
    ]),
  );
  assert.equal(parsed.skip, undefined);
  assert.equal(parsed.skippedTurns, 0);
  assert.equal(parsed.turns.length, 2);

  const [first, second] = parsed.turns;
  assert.equal(first.turnId, "a2");
  assert.equal(first.eventTimeMs, Date.parse("2026-07-01T10:01:10.000Z"));
  assert.equal(first.summary.userText, "first question");
  assert.equal(first.summary.assistantText, "first answer");
  assert.deepEqual(first.summary.toolNames, ["bash", "read"]);
  assert.ok(!first.summary.assistantText.includes("raw output"));

  assert.equal(second.turnId, "a3");
  assert.equal(second.summary.assistantText, "second answer");
});

test("parsePiSession skips subagent sessions, junk, and empty files", () => {
  assert.equal(
    parsePiSession(jsonl([header({ parentSession: "/somewhere/parent.jsonl" }), userMsg("u1", "2026-07-01T10:01:00.000Z", "hi")])).skip,
    "subagent session",
  );
  assert.equal(parsePiSession(jsonl([userMsg("u1", "2026-07-01T10:01:00.000Z", "no header")])).skip, "missing session header");
  assert.equal(parsePiSession("not json\n").skip, "missing session header");
  assert.equal(parsePiSession("").skip, "empty file");
});

test("parsePiSession counts incomplete turns as skipped", () => {
  // A user prompt with no assistant reply (crash, abort) and an
  // assistant-only preamble both fail the completed-turn requirement.
  const parsed = parsePiSession(
    jsonl([
      header(),
      userMsg("u1", "2026-07-01T10:01:00.000Z", "answered"),
      assistantMsg("a1", "2026-07-01T10:01:05.000Z", "answer"),
      userMsg("u2", "2026-07-01T10:02:00.000Z", "never answered"),
    ]),
  );
  assert.equal(parsed.turns.length, 1);
  assert.equal(parsed.skippedTurns, 1);
});

test("parsePiSession honors capture policy at each turn's leaf, not retroactively", () => {
  const off = { type: "custom", id: "c1", customType: SESSION_POLICY_ENTRY, data: { capture: "off" } };
  const on = { type: "custom", id: "c2", customType: SESSION_POLICY_ENTRY, data: { capture: "on" } };
  const parsed = parsePiSession(
    jsonl([
      header(),
      userMsg("u1", "2026-07-01T10:01:00.000Z", "captured"),
      assistantMsg("a1", "2026-07-01T10:01:05.000Z", "kept"),
      // Toggle lands after u1/a1 settled: that turn stays importable even
      // though it only finalizes when u2 arrives below.
      off,
      userMsg("u2", "2026-07-01T10:02:00.000Z", "private"),
      assistantMsg("a2", "2026-07-01T10:02:05.000Z", "dropped"),
      on,
      userMsg("u3", "2026-07-01T10:03:00.000Z", "captured again"),
      assistantMsg("a3", "2026-07-01T10:03:05.000Z", "kept again"),
    ]),
  );
  assert.deepEqual(
    parsed.turns.map((t) => t.turnId),
    ["a1", "a3"],
  );
  assert.equal(parsed.skippedTurns, 1);
});

test("session file discovery walks project dirs and probes headers cheaply", () => {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "aj-import-list-"));
  try {
    const projectDir = path.join(tmp, "--home-user-project--");
    fs.mkdirSync(projectDir);
    fs.writeFileSync(path.join(projectDir, "b.jsonl"), jsonl([header()]));
    fs.writeFileSync(path.join(projectDir, "a.jsonl"), jsonl([header({ parentSession: "/p.jsonl" })]));
    fs.writeFileSync(path.join(projectDir, "notes.txt"), "not a session");
    fs.writeFileSync(path.join(tmp, "stray.jsonl"), jsonl([header()]));

    const files = listPiSessionFiles(tmp);
    assert.deepEqual(
      files.map((f) => path.basename(f)),
      ["a.jsonl", "b.jsonl"],
    );
    assert.equal(importableSessionHeader(readFirstLine(files[1])), true);
    assert.equal(importableSessionHeader(readFirstLine(files[0])), false);
    assert.equal(importableSessionHeader(readFirstLine(path.join(tmp, "missing.jsonl"))), false);
    assert.deepEqual(listPiSessionFiles(path.join(tmp, "nonexistent")), [], "missing root yields empty");
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true });
  }
});

test("eventTimeFromEntries reads the leaf entry timestamp in either form", () => {
  const ms = Date.parse("2026-07-01T10:02:10.000Z");
  assert.equal(eventTimeFromEntries([{ timestamp: "2026-07-01T10:02:10.000Z" }]), ms);
  assert.equal(eventTimeFromEntries([{}, { timestamp: ms }]), ms);
  assert.equal(eventTimeFromEntries([{ timestamp: "garbage" }]), null);
  assert.equal(eventTimeFromEntries([]), null);
});

test("sessionIdFromFile matches live capture's session id derivation", () => {
  assert.equal(
    sessionIdFromFile("/root/sessions/--proj--/2026-07-01T10-00-00-000Z_0192aaaa.jsonl"),
    "2026-07-01T10-00-00-000Z_0192aaaa",
  );
});

test("formatImportSummary reports failure detail only when something failed", () => {
  const base = {
    files: 3,
    skippedFiles: 1,
    published: 5,
    existing: 2,
    skippedTurns: 1,
    unrecognized: 0,
    failed: 0,
    firstFailure: null,
  };
  assert.ok(!formatImportSummary(base).includes("failed"));
  const failing = { ...base, failed: 2, firstFailure: "malformed" };
  assert.ok(formatImportSummary(failing).includes("2 failed (first: malformed)"));
  const tolerant = { ...base, unrecognized: 1 };
  assert.ok(
    formatImportSummary(tolerant).includes("1 stored with an outcome this adapter does not know"),
  );
});

test(
  "e2e: import publishes once, re-import dedups, live-captured turns are not doubled",
  { skip: e2eBinary === null },
  async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "aj-import-e2e-"));
    const previous = {
      bin: process.env.AUTOJOURNAL_BIN,
      config: process.env.AUTOJOURNAL_CONFIG,
      xdgConfig: process.env.XDG_CONFIG_HOME,
      data: process.env.XDG_DATA_HOME,
      state: process.env.XDG_STATE_HOME,
    };
    process.env.AUTOJOURNAL_BIN = e2eBinary as string;
    delete process.env.AUTOJOURNAL_CONFIG;
    process.env.XDG_CONFIG_HOME = path.join(tmp, "config");
    process.env.XDG_DATA_HOME = path.join(tmp, "data");
    process.env.XDG_STATE_HOME = path.join(tmp, "state");
    try {
      const sessionsDir = path.join(tmp, "sessions", "--home-user-project--");
      fs.mkdirSync(sessionsDir, { recursive: true });
      const sessionFile = path.join(sessionsDir, "2026-07-01T10-00-00-000Z_0192aaaa.jsonl");
      fs.writeFileSync(
        sessionFile,
        jsonl([
          header(),
          userMsg("u1", "2026-07-01T10:01:00.000Z", "imported question"),
          assistantMsg("a1", "2026-07-01T10:01:10.000Z", "imported answer", ["bash"]),
          userMsg("u2", "2026-07-01T10:02:00.000Z", "live question"),
          assistantMsg("a2", "2026-07-01T10:02:10.000Z", "live answer"),
        ]),
      );
      fs.writeFileSync(
        path.join(sessionsDir, "subagent.jsonl"),
        jsonl([header({ parentSession: "/p.jsonl" }), userMsg("u1", "2026-07-01T10:01:00.000Z", "synthetic")]),
      );

      const selection = { world: "main", scope: "default" };
      const binary = e2eBinary as string;

      // Simulate the second turn having been captured live before the
      // import: same session/turn identity and the same leaf-entry event
      // time live capture derives, so the import's re-delivery must resolve
      // as already present, not a fresh publish.
      const livePayload = buildPayload({
        summary: { userText: "live question", assistantText: "live answer", toolNames: [] },
        sessionId: sessionIdFromFile(sessionFile),
        turnId: "a2",
        eventTimeMs: Date.parse("2026-07-01T10:02:10.000Z"),
        selection,
      });
      const liveRun = await runBinary(binary, ["capture"], { stdin: JSON.stringify(livePayload) });
      assert.equal((JSON.parse(liveRun.stdout) as { outcome: string }).outcome, "published");

      const files = listPiSessionFiles(path.join(tmp, "sessions"));
      const first = await importPiHistory({ binary, selection, files });
      assert.equal(first.files, 1);
      assert.equal(first.skippedFiles, 1, "subagent session file is not importable");
      assert.equal(first.published, 1, "only the turn not captured live publishes");
      assert.equal(first.existing, 1, "live-captured turn resolves as already present");
      assert.equal(first.failed, 0);

      const again = await importPiHistory({ binary, selection, files });
      assert.equal(again.published, 0, "re-import publishes nothing");
      assert.equal(again.existing, 2);
      assert.equal(again.failed, 0);

      // Cross-date redelivery: same identity with an event time on another
      // day shards to a different path, so only the core's corpus-wide
      // identity check stands between this and a silent second copy.
      const crossDate = buildPayload({
        summary: { userText: "live question", assistantText: "live answer", toolNames: [] },
        sessionId: sessionIdFromFile(sessionFile),
        turnId: "a2",
        eventTimeMs: Date.parse("2026-07-02T10:02:10.000Z"),
        selection,
      });
      const crossRun = await runBinary(binary, ["capture"], { stdin: JSON.stringify(crossDate) });
      assert.equal(
        (JSON.parse(crossRun.stdout) as { outcome: string }).outcome,
        "conflict",
        "cross-date redelivery of an existing identity must not publish",
      );
    } finally {
      if (previous.bin === undefined) delete process.env.AUTOJOURNAL_BIN;
      else process.env.AUTOJOURNAL_BIN = previous.bin;
      if (previous.config === undefined) delete process.env.AUTOJOURNAL_CONFIG;
      else process.env.AUTOJOURNAL_CONFIG = previous.config;
      if (previous.xdgConfig === undefined) delete process.env.XDG_CONFIG_HOME;
      else process.env.XDG_CONFIG_HOME = previous.xdgConfig;
      if (previous.data === undefined) delete process.env.XDG_DATA_HOME;
      else process.env.XDG_DATA_HOME = previous.data;
      if (previous.state === undefined) delete process.env.XDG_STATE_HOME;
      else process.env.XDG_STATE_HOME = previous.state;
    }
  },
);
