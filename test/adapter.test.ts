import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import {
  adapterStatePath,
  default as autojournalExtension,
  buildRawPayload,
  captureFromEntries,
  DEFAULT_SELECTION,
  EvidenceReferenceStore,
  extractText,
  formatMenuTitle,
  legacyPiJournalRoot,
  originHost,
  readSubagentCapture,
  renderGetResult,
  renderSearchResults,
  runCli,
  sanitizeToken,
  selectionFromEntries,
  sessionHeaderIsSubagent,
  SESSION_POLICY_ENTRY,
  stableTurnId,
  summarizeRun,
  syncResultBody,
  writeSubagentCapture,
  validScope,
  validWorld,
} from "../index.ts";

test("syncResultBody prefers stdout, then stderr, then a placeholder", () => {
  assert.equal(syncResultBody({ code: 0, stdout: "indexed: 3\n", stderr: "" }), "indexed: 3");
  assert.equal(syncResultBody({ code: 1, stdout: "", stderr: "root missing" }), "root missing");
  assert.equal(syncResultBody({ code: 0, stdout: "", stderr: "" }), "(sync produced no output)");
});

test(
  "/autojournal sync owns the footer status for its duration, then clears it",
  async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "aj-syncstatus-"));
    const previous = {
      config: process.env.AUTOJOURNAL_CONFIG,
      xdgConfig: process.env.XDG_CONFIG_HOME,
      data: process.env.XDG_DATA_HOME,
      state: process.env.XDG_STATE_HOME,
    };
    delete process.env.AUTOJOURNAL_CONFIG;
    process.env.XDG_CONFIG_HOME = path.join(tmp, "config");
    process.env.XDG_DATA_HOME = path.join(tmp, "data");
    process.env.XDG_STATE_HOME = path.join(tmp, "state");
    try {
      // An existing-but-empty root: sync exits 0 with a report rather
      // than the missing-root failure.
      fs.mkdirSync(path.join(tmp, "data", "autojournal", "journals"), { recursive: true });
      let command: ((args: string, ctx: unknown) => Promise<void>) | undefined;
      const fakePi = {
        on() {},
        registerTool() {},
        registerCommand(_name: string, spec: { handler: typeof command }) {
          command = spec.handler;
        },
        appendEntry() {},
      };
      autojournalExtension(fakePi as never);
      assert.ok(command);
      const statuses: Array<string | undefined> = [];
      const notices: Array<{ msg: string; type: string }> = [];
      await command("sync", {
        hasUI: true,
        ui: {
          notify(msg: string, type: string) { notices.push({ msg, type }); },
          setStatus(_key: string, text: string | undefined) { statuses.push(text); },
          async select() { return "Close"; },
          async input() { return ""; },
        },
      });
      assert.ok(statuses.length >= 2, `status lifecycle too short: ${JSON.stringify(statuses)}`);
      assert.match(statuses[0] as string, /autojournal: syncing index/);
      assert.equal(statuses[statuses.length - 1], undefined, "status must be cleared at the end");
      assert.ok(notices.some((n) => n.type === "info" && n.msg.includes("indexed:")));
    } finally {
      if (previous.config === undefined) delete process.env.AUTOJOURNAL_CONFIG;
      else process.env.AUTOJOURNAL_CONFIG = previous.config;
      if (previous.xdgConfig === undefined) delete process.env.XDG_CONFIG_HOME;
      else process.env.XDG_CONFIG_HOME = previous.xdgConfig;
      if (previous.data === undefined) delete process.env.XDG_DATA_HOME;
      else process.env.XDG_DATA_HOME = previous.data;
      if (previous.state === undefined) delete process.env.XDG_STATE_HOME;
      else process.env.XDG_STATE_HOME = previous.state;
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  },
);

test("extractText handles strings, blocks, and junk", () => {
  assert.equal(extractText("plain"), "plain");
  assert.equal(extractText("   "), "");
  assert.equal(
    extractText([
      { type: "text", text: "a" },
      { type: "thinking", text: "hidden" },
      { type: "text", text: "b" },
    ]),
    "a\nb",
  );
  assert.equal(extractText(undefined), "");
  assert.equal(extractText(42), "");
});

test("summarizeRun keeps every visible assistant segment and dedups tool names", () => {
  const summary = summarizeRun([
    { role: "user", content: "do the thing" },
    {
      role: "assistant",
      content: [
        { type: "text", text: "working" },
        { type: "toolCall", name: "bash" },
        { type: "toolCall", name: "read" },
      ],
    },
    { role: "toolResult", content: "raw tool output must not leak" },
    {
      role: "assistant",
      content: [
        { type: "toolCall", name: "bash" },
        { type: "text", text: "final answer" },
      ],
    },
  ]);
  assert.equal(summary.userText, "do the thing");
  assert.equal(summary.assistantText, "working\n\nfinal answer");
  assert.deepEqual(summary.toolNames, ["bash", "read"]);
});

test("stableTurnId uses Pi's durable leaf id and deterministic fallback", () => {
  const summary = {
    userText: "same user turn",
    assistantText: "same settled answer",
    toolNames: ["read"],
  };
  assert.equal(stableTurnId("session-a", "entry-123", 10, summary), "entry-123");
  assert.equal(
    stableTurnId("session-a", null, 10, summary),
    stableTurnId("session-a", null, 10, summary),
  );
  assert.notEqual(
    stableTurnId("session-a", null, 10, summary),
    stableTurnId("session-b", null, 10, summary),
  );
  assert.notEqual(
    stableTurnId("session-a", null, 10, summary),
    stableTurnId("session-a", null, 12, summary),
  );
});

test("buildRawPayload carries selected world/scope and sanitizes identities", () => {
  const payload = buildRawPayload({
    summary: { userText: "u", assistantText: "a", toolNames: ["weird tool!"] },
    sessionId: "2026-07-29 bad id",
    turnId: "t/1",
    eventTimeMs: 123,
    selection: { world: "isolated-work", scope: "client:a" },
  });
  assert.equal(payload.world, "isolated-work");
  assert.equal(payload.scope, "client:a");
  assert.equal(payload.lane, "conversation");
  assert.equal(payload.sessionId, "2026-07-29-bad-id");
  assert.equal(payload.turnId, "t/1");
  assert.equal(payload.eventTimeMs, 123n);
  assert.equal(payload.capturePolicy, "pi-visible-v2");
  assert.deepEqual(payload.tools, [{ name: "weird-tool-" }]);
});

test("sanitizeToken falls back when nothing survives", () => {
  assert.equal(sanitizeToken("???", "fb"), "---");
  assert.equal(sanitizeToken("", "fb"), "fb");
});

test("originHost reports the short machine name, or nothing it cannot label", () => {
  assert.equal(originHost("stealth.tail8255b9.ts.net"), "stealth");
  assert.equal(originHost("battlestation"), "battlestation");
  assert.equal(originHost(""), null);
  assert.equal(originHost("   "), null);
  // Provenance names a real machine, so an unusable name costs the field
  // rather than being sanitized into a host that does not exist.
  assert.equal(originHost("two words"), null);
  assert.equal(originHost("x".repeat(129)), null);
});

test("buildRawPayload labels the originating machine and omits it when unknown", () => {
  const base = {
    summary: { userText: "u", assistantText: "a", toolNames: [] },
    sessionId: "s",
    turnId: "t",
    eventTimeMs: 1,
    selection: DEFAULT_SELECTION,
  };
  assert.equal(buildRawPayload({ ...base, host: "stealth" }).host, "stealth");
  assert.equal(buildRawPayload({ ...base, host: null }).host, null);
});

test("renderSearchResults covers match, no_match, and typed failures", () => {
  assert.match(renderSearchResults({ outcome: "no_match", alias_terms: ["fwupd"] }), /No matching memory.*fwupd/);
  assert.match(renderSearchResults({ outcome: "index_stale" }), /index_stale/);
  const hit = {
    episode_id: "aj1-x",
    revision: "sha256:abc",
    path: "worlds/w/2026/07/29/aj1-x.md",
    line: 12,
    snippet: "the fwupd refresh worked",
    score: 3.2,
  };
  const text = renderSearchResults({
    outcome: "match",
    total: 9,
    results: [hit],
    // The binary reports index health as an object; a fresh index adds
    // nothing to the header.
    index: { freshness: "fresh", indexed: 9, source: 9, edited_excluded: 0 },
  }, [{
    reference: 17,
    episode_id: hit.episode_id,
    revision: hit.revision,
    world: "main",
    scope: "default",
  }]);
  assert.match(text, /1 of 9 matching result\(s\):/);
  assert.match(text, /\[reference 17\]/);
  assert.match(text, /aj1-x\.md:12/);
  assert.match(text, /> the fwupd refresh worked/);
  assert.match(text, /memory_get\(reference, lines\)/);

  const stale = renderSearchResults({
    outcome: "match",
    total: 1,
    results: [hit],
    index: { freshness: "stale", indexed: 8, source: 9, edited_excluded: 0 },
  });
  assert.match(stale, /\(index stale\)/);
  assert.doesNotMatch(stale, /object Object/);
});

test("evidence references are bounded and restore from search tool details", () => {
  const store = new EvidenceReferenceStore(2);
  const remembered = store.remember([
    { episode_id: "aj1-a", revision: "sha256:a", world: "main", scope: "default" },
    { episode_id: "aj1-b", revision: "sha256:b", world: "main", scope: "default" },
    { episode_id: "aj1-c", revision: "sha256:c", world: "main", scope: "default" },
  ]);
  assert.equal(store.resolve(remembered[0].reference), undefined);
  assert.deepEqual(store.resolve(remembered[1].reference), {
    episode_id: "aj1-b",
    revision: "sha256:b",
    world: "main",
    scope: "default",
  });

  const restored = new EvidenceReferenceStore(2);
  restored.restoreFromEntries([{
    type: "message",
    message: {
      role: "toolResult",
      toolName: "memory_search",
      details: { evidence_references: remembered.slice(1) },
    },
  }]);
  assert.deepEqual(restored.resolve(remembered[2].reference), {
    episode_id: "aj1-c",
    revision: "sha256:c",
    world: "main",
    scope: "default",
  });
  const [next] = restored.remember([{
    episode_id: "aj1-d",
    revision: "sha256:d",
    world: "main",
    scope: "default",
  }]);
  assert.equal(next.reference, remembered[2].reference + 1);
});

test("memory_get resolves short references and keeps legacy calls compatible", async () => {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "aj-evidence-ref-"));
  const previous = {
    config: process.env.AUTOJOURNAL_CONFIG,
    xdgConfig: process.env.XDG_CONFIG_HOME,
    data: process.env.XDG_DATA_HOME,
    state: process.env.XDG_STATE_HOME,
  };
  delete process.env.AUTOJOURNAL_CONFIG;
  process.env.XDG_CONFIG_HOME = path.join(tmp, "config");
  process.env.XDG_DATA_HOME = path.join(tmp, "data");
  process.env.XDG_STATE_HOME = path.join(tmp, "state");

  type ToolResult = {
    content: Array<{ type: string; text: string }>;
    details?: unknown;
  };
  type ToolSpec = {
    parameters: { required?: string[]; properties?: Record<string, unknown> };
    prepareArguments?(args: unknown): { reference: number; lines?: string };
    execute(id: string, params: never): Promise<ToolResult>;
  };
  const tools = new Map<string, ToolSpec>();
  const events = new Map<string, (event: unknown, ctx: unknown) => Promise<void>>();
  const fakePi = {
    on(name: string, handler: (event: unknown, ctx: unknown) => Promise<void>) {
      events.set(name, handler);
    },
    registerTool(spec: ToolSpec & { name: string }) { tools.set(spec.name, spec); },
    registerCommand() {},
    appendEntry() {},
  };

  try {
    // A real episode in a real corpus: the tools run the engine in-process.
    const captured = runCli(["capture"], JSON.stringify({
      schema_version: 1,
      world: "main",
      scope: "default",
      lane: "conversation",
      harness: "pi",
      adapter_version: "2.0.0",
      session_id: "ref-session",
      turn_id: "ref-turn-1",
      event_time_ms: Date.parse("2026-08-12T10:00:00.000Z"),
      capture_policy: "pi-visible-v2",
      turn_outcome: "completed",
      user_content: "where is the reference sentinel",
      assistant_result: "the reference sentinel lives here",
    }));
    const report = JSON.parse(captured.stdout) as { outcome: string; episode_id: string };
    assert.equal(report.outcome, "published");

    autojournalExtension(fakePi as never);
    const search = tools.get("memory_search") as ToolSpec;
    const get = tools.get("memory_get") as ToolSpec;
    assert.deepEqual(get.parameters.required, ["reference"]);
    assert.ok(get.parameters.properties?.reference);
    assert.equal(get.parameters.properties?.episode_id, undefined);
    assert.equal(get.parameters.properties?.revision, undefined);

    const searched = await search.execute("search-1", { query: "reference sentinel" } as never);
    assert.match(searched.content[0].text, /\[reference 1\]/);
    const searchDetails = searched.details as {
      evidence_references: Array<{ reference: number; episode_id: string; revision: string }>;
    };
    assert.equal(searchDetails.evidence_references[0].reference, 1);
    assert.equal(searchDetails.evidence_references[0].episode_id, report.episode_id);

    const opened = await get.execute("get-1", { reference: 1 } as never);
    assert.match(opened.content[0].text, /reference sentinel/);
    assert.match(opened.content[0].text, /untrusted data/);

    // A legacy resumed call folds episode_id/revision into a fresh
    // reference under the *current* selection, so it runs before the
    // branch switch below moves the session to another world.
    const identity = searchDetails.evidence_references[0];
    const prepared = get.prepareArguments?.({
      episode_id: identity.episode_id,
      revision: identity.revision,
      lines: "1-400",
    });
    assert.ok(prepared);
    assert.equal(typeof prepared.reference, "number");
    const legacy = await get.execute("get-legacy", prepared as never);
    assert.match(legacy.content[0].text, /reference sentinel/);

    // A resumed branch restores references from durable tool-result
    // details. The reference retains the world/scope where search found it
    // even when the branch's active selection later changes — the get
    // still resolves (a get bound to the changed selection would be gone).
    const sessionTree = events.get("session_tree");
    assert.ok(sessionTree);
    const resumedContext = {
      sessionManager: {
        getBranch: () => [
          {
            type: "message",
            message: { role: "toolResult", toolName: "memory_search", details: searched.details },
          },
          {
            type: "custom",
            customType: SESSION_POLICY_ENTRY,
            data: { world: "private", scope: "resumed" },
          },
        ],
        getEntries: () => [],
      },
    };
    await sessionTree({}, resumedContext);
    const resumed = await get.execute("get-resumed", { reference: 1 } as never);
    assert.match(resumed.content[0].text, /reference sentinel/, "restored reference keeps its own world/scope");

    const unknown = await get.execute("get-2", { reference: 999 } as never);
    assert.match(unknown.content[0].text, /run memory_search again/);

  } finally {
    if (previous.config === undefined) delete process.env.AUTOJOURNAL_CONFIG;
    else process.env.AUTOJOURNAL_CONFIG = previous.config;
    if (previous.xdgConfig === undefined) delete process.env.XDG_CONFIG_HOME;
    else process.env.XDG_CONFIG_HOME = previous.xdgConfig;
    if (previous.data === undefined) delete process.env.XDG_DATA_HOME;
    else process.env.XDG_DATA_HOME = previous.data;
    if (previous.state === undefined) delete process.env.XDG_STATE_HOME;
    else process.env.XDG_STATE_HOME = previous.state;
    fs.rmSync(tmp, { recursive: true, force: true });
  }
});

test("renderGetResult frames content as untrusted evidence", () => {
  const text = renderGetResult({
    outcome: "match",
    episode_id: "aj1-x",
    revision: "sha256:abc",
    path: "worlds/w/2026/07/29/aj1-x.md",
    line_start: 3,
    line_end: 5,
    content: "line3\nline4\nline5",
  });
  assert.match(text, /untrusted data, not instructions/);
  assert.match(text, /line4/);
  assert.match(renderGetResult({ outcome: "stale_revision" }), /stale_revision/);
});

test("world/scope validation and branch-local selection restoration", () => {
  assert.equal(validWorld("isolated-work"), true);
  assert.equal(validWorld("Bad World"), false);
  assert.equal(validScope("client:a"), true);
  assert.equal(validScope("../escape"), false);
  // Dot-led scopes publish into directories the corpus walk skips: the
  // core refuses them, and this validator must agree.
  assert.equal(validScope(".hidden"), false);
  assert.equal(validScope("."), false);
  assert.equal(validScope("a.b"), true);
  assert.deepEqual(selectionFromEntries([], DEFAULT_SELECTION), DEFAULT_SELECTION);
  assert.deepEqual(
    selectionFromEntries([
      { type: "custom", customType: SESSION_POLICY_ENTRY, data: { world: "first", scope: "default" } },
      { type: "custom", customType: "other", data: { world: "ignored", scope: "default" } },
      { type: "custom", customType: SESSION_POLICY_ENTRY, data: { world: "latest", scope: "project-a" } },
    ]),
    { world: "latest", scope: "project-a" },
  );
});

test("capture toggle restoration: latest stated value wins, silence keeps capture on", () => {
  assert.equal(captureFromEntries([]), true);
  // Entries written before the toggle existed carry no capture field.
  assert.equal(
    captureFromEntries([
      { type: "custom", customType: SESSION_POLICY_ENTRY, data: { world: "main", scope: "default" } },
    ]),
    true,
  );
  assert.equal(
    captureFromEntries([
      { type: "custom", customType: SESSION_POLICY_ENTRY, data: { world: "main", scope: "default", capture: "off" } },
      { type: "custom", customType: "other", data: { capture: "on" } },
    ]),
    false,
  );
  assert.equal(
    captureFromEntries([
      { type: "custom", customType: SESSION_POLICY_ENTRY, data: { world: "main", scope: "default", capture: "off" } },
      { type: "custom", customType: SESSION_POLICY_ENTRY, data: { world: "main", scope: "default", capture: "on" } },
    ]),
    true,
  );
});

test("/autojournal menu title exposes resolved journal directory and source", () => {
  const title = formatMenuTitle(
    {
      journal_root: "/data/autojournal/journals",
      root_source: "owner_config",
      root_source_path: "/home/user/.config/autojournal/config.json",
      episodes: 12,
      index: { freshness: "fresh", indexed: 12, path: "/state/index.sqlite" },
    },
    { world: "isolated-work", scope: "client:a" },
  );
  assert.match(title, /Journal: \/data\/autojournal\/journals/);
  assert.match(title, /owner config: \/home\/user\/\.config\/autojournal\/config\.json/);
  assert.match(title, /Active: isolated-work \/ client:a \/ conversation/);
  assert.doesNotMatch(title, /Capture: OFF/);
  assert.match(
    formatMenuTitle(null, DEFAULT_SELECTION, false),
    /Capture: OFF \(this session's turns are not being journaled\)/,
  );
});

test(
  "/autojournal menu creates and persists a session world",
  async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "aj-menu-"));
    const previous = {
      config: process.env.AUTOJOURNAL_CONFIG,
      xdgConfig: process.env.XDG_CONFIG_HOME,
      data: process.env.XDG_DATA_HOME,
      state: process.env.XDG_STATE_HOME,
    };
    delete process.env.AUTOJOURNAL_CONFIG;
    process.env.XDG_CONFIG_HOME = path.join(tmp, "config");
    process.env.XDG_DATA_HOME = path.join(tmp, "data");
    process.env.XDG_STATE_HOME = path.join(tmp, "state");
    try {
      let command: ((args: string, ctx: unknown) => Promise<void>) | undefined;
      const appended: Array<{ type: string; data: unknown }> = [];
      const fakePi = {
        on() {},
        registerTool() {},
        registerCommand(_name: string, spec: { handler: typeof command }) {
          command = spec.handler;
        },
        appendEntry(type: string, data: unknown) {
          appended.push({ type, data });
        },
      };
      autojournalExtension(fakePi as never);
      assert.ok(command);
      let selects = 0;
      const titles: string[] = [];
      await command("", {
        hasUI: true,
        ui: {
          notify() {},
          async select(title: string, options: string[]) {
            titles.push(title);
            selects += 1;
            if (selects === 1) return options.find((option) => option.startsWith("World:"));
            if (selects === 2) return "New world…";
            if (selects === 3) return "Save world/scope as default for new sessions";
            return "Close";
          },
          async input() {
            return "isolated-work";
          },
        },
      });
      assert.match(titles[0], new RegExp(`Journal: ${path.join(tmp, "data", "autojournal", "journals")}`));
      assert.deepEqual(appended, [
        {
          type: SESSION_POLICY_ENTRY,
          data: { version: 1, world: "isolated-work", scope: "default", capture: "on" },
        },
      ]);
      const savedConfig = JSON.parse(
        fs.readFileSync(path.join(tmp, "config", "autojournal", "config.json"), "utf8"),
      );
      assert.deepEqual(savedConfig.capture, { world: "isolated-work", scope: "default" });
    } finally {
      if (previous.config === undefined) delete process.env.AUTOJOURNAL_CONFIG;
      else process.env.AUTOJOURNAL_CONFIG = previous.config;
      if (previous.xdgConfig === undefined) delete process.env.XDG_CONFIG_HOME;
      else process.env.XDG_CONFIG_HOME = previous.xdgConfig;
      if (previous.data === undefined) delete process.env.XDG_DATA_HOME;
      else process.env.XDG_DATA_HOME = previous.data;
      if (previous.state === undefined) delete process.env.XDG_STATE_HOME;
      else process.env.XDG_STATE_HOME = previous.state;
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  },
);

test(
  "test_pi_menu_offers_reseal",
  async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "aj-reseal-menu-"));
    const previous = {
      config: process.env.AUTOJOURNAL_CONFIG,
      xdgConfig: process.env.XDG_CONFIG_HOME,
      data: process.env.XDG_DATA_HOME,
      state: process.env.XDG_STATE_HOME,
    };
    delete process.env.AUTOJOURNAL_CONFIG;
    process.env.XDG_CONFIG_HOME = path.join(tmp, "config");
    process.env.XDG_DATA_HOME = path.join(tmp, "data");
    process.env.XDG_STATE_HOME = path.join(tmp, "state");
    // An existing (empty) journal root: reseal over it reports its scan
    // rather than the missing-root refusal.
    fs.mkdirSync(path.join(tmp, "data", "autojournal", "journals"), { recursive: true, mode: 0o700 });
    try {
      let command: ((args: string, ctx: unknown) => Promise<void>) | undefined;
      const fakePi = {
        on() {},
        registerTool() {},
        registerCommand(_name: string, spec: { handler: typeof command }) {
          command = spec.handler;
        },
        appendEntry() {},
      };
      autojournalExtension(fakePi as never);
      assert.ok(command);
      const offered: string[][] = [];
      const notices: string[] = [];
      let selects = 0;
      await command("", {
        hasUI: true,
        ui: {
          notify(body: string) {
            notices.push(body);
          },
          async select(_title: string, options: string[]) {
            offered.push(options);
            selects += 1;
            if (selects === 1) return "Reseal edited episodes";
            return "Close";
          },
          async input() {
            return "";
          },
        },
      });
      assert.ok(
        offered[0].includes("Reseal edited episodes"),
        `menu is missing the reseal entry: ${offered[0].join(", ")}`,
      );
      // Selecting it shells the real binary over the fresh empty journal
      // and renders its report.
      assert.ok(
        notices.some((body) => /scanned: 0/.test(body) && /resealed: 0/.test(body)),
        `no reseal report was rendered: ${JSON.stringify(notices)}`,
      );
    } finally {
      if (previous.config === undefined) delete process.env.AUTOJOURNAL_CONFIG;
      else process.env.AUTOJOURNAL_CONFIG = previous.config;
      if (previous.xdgConfig === undefined) delete process.env.XDG_CONFIG_HOME;
      else process.env.XDG_CONFIG_HOME = previous.xdgConfig;
      if (previous.data === undefined) delete process.env.XDG_DATA_HOME;
      else process.env.XDG_DATA_HOME = previous.data;
      if (previous.state === undefined) delete process.env.XDG_STATE_HOME;
      else process.env.XDG_STATE_HOME = previous.state;
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  },
);

test(
  "capture skips headless/subagent modes and publishes interactive turns",
  async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "aj-mode-"));
    const previous = {
      config: process.env.AUTOJOURNAL_CONFIG,
      xdgConfig: process.env.XDG_CONFIG_HOME,
      data: process.env.XDG_DATA_HOME,
      state: process.env.XDG_STATE_HOME,
    };
    delete process.env.AUTOJOURNAL_CONFIG;
    process.env.XDG_CONFIG_HOME = path.join(tmp, "config");
    process.env.XDG_DATA_HOME = path.join(tmp, "data");
    process.env.XDG_STATE_HOME = path.join(tmp, "state");
    try {
      const handlers = new Map<string, (event: unknown, ctx: unknown) => Promise<void>>();
      const fakePi = {
        on(name: string, handler: (event: unknown, ctx: unknown) => Promise<void>) {
          handlers.set(name, handler);
        },
        registerTool() {},
        registerCommand() {},
        appendEntry() {},
      };
      autojournalExtension(fakePi as never);
      const messages = [
        { role: "user", content: "mode gating sentinel" },
        { role: "assistant", content: [{ type: "text", text: "done" }] },
      ];
      const ctx = {
        sessionManager: {
          getLeafId: () => "leaf-mode-gate",
          getBranch: () => [1],
          getEntries: () => [],
        },
        ui: { notify() {} },
      };
      const journals = path.join(tmp, "data", "autojournal", "journals");

      await handlers.get("agent_end")!({ messages }, ctx);
      await handlers.get("agent_settled")!({}, { ...ctx, mode: "print" });
      assert.equal(fs.existsSync(journals), false);

      await handlers.get("agent_end")!({ messages }, ctx);
      await handlers.get("agent_settled")!({}, { ...ctx, mode: "tui" });
      assert.equal(fs.existsSync(journals), true);
    } finally {
      if (previous.config === undefined) delete process.env.AUTOJOURNAL_CONFIG;
      else process.env.AUTOJOURNAL_CONFIG = previous.config;
      if (previous.xdgConfig === undefined) delete process.env.XDG_CONFIG_HOME;
      else process.env.XDG_CONFIG_HOME = previous.xdgConfig;
      if (previous.data === undefined) delete process.env.XDG_DATA_HOME;
      else process.env.XDG_DATA_HOME = previous.data;
      if (previous.state === undefined) delete process.env.XDG_STATE_HOME;
      else process.env.XDG_STATE_HOME = previous.state;
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  },
);

test(
  "menu capture toggle suppresses publishing for the session and persists as policy",
  async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "aj-toggle-"));
    const previous = {
      config: process.env.AUTOJOURNAL_CONFIG,
      xdgConfig: process.env.XDG_CONFIG_HOME,
      data: process.env.XDG_DATA_HOME,
      state: process.env.XDG_STATE_HOME,
    };
    delete process.env.AUTOJOURNAL_CONFIG;
    process.env.XDG_CONFIG_HOME = path.join(tmp, "config");
    process.env.XDG_DATA_HOME = path.join(tmp, "data");
    process.env.XDG_STATE_HOME = path.join(tmp, "state");
    try {
      let command: ((args: string, ctx: unknown) => Promise<void>) | undefined;
      const handlers = new Map<string, (event: unknown, ctx: unknown) => Promise<void>>();
      const appended: Array<{ type: string; data: unknown }> = [];
      const fakePi = {
        on(name: string, handler: (event: unknown, ctx: unknown) => Promise<void>) {
          handlers.set(name, handler);
        },
        registerTool() {},
        registerCommand(_name: string, spec: { handler: typeof command }) {
          command = spec.handler;
        },
        appendEntry(type: string, data: unknown) {
          appended.push({ type, data });
        },
      };
      autojournalExtension(fakePi as never);
      assert.ok(command);

      const toggleOnce = async () => {
        let selects = 0;
        await command!("", {
          hasUI: true,
          ui: {
            notify() {},
            async select(_title: string, options: string[]) {
              selects += 1;
              if (selects === 1) return options.find((option) => option.startsWith("Capture:"));
              return "Close";
            },
            async input() {
              return undefined;
            },
          },
        });
      };
      const messages = [
        { role: "user", content: "capture toggle sentinel" },
        { role: "assistant", content: [{ type: "text", text: "done" }] },
      ];
      const ctx = {
        sessionManager: {
          getLeafId: () => "leaf-capture-toggle",
          getBranch: () => [1],
          getEntries: () => [],
        },
        ui: { notify() {} },
      };
      const journals = path.join(tmp, "data", "autojournal", "journals");

      await toggleOnce(); // capture off
      assert.deepEqual(appended.at(-1), {
        type: SESSION_POLICY_ENTRY,
        data: { version: 1, world: "main", scope: "default", capture: "off" },
      });
      await handlers.get("agent_end")!({ messages }, ctx);
      await handlers.get("agent_settled")!({}, { ...ctx, mode: "tui" });
      assert.equal(fs.existsSync(journals), false);

      await toggleOnce(); // capture back on
      assert.deepEqual(appended.at(-1), {
        type: SESSION_POLICY_ENTRY,
        data: { version: 1, world: "main", scope: "default", capture: "on" },
      });
      await handlers.get("agent_end")!({ messages }, ctx);
      await handlers.get("agent_settled")!({}, { ...ctx, mode: "tui" });
      assert.equal(fs.existsSync(journals), true);
    } finally {
      if (previous.config === undefined) delete process.env.AUTOJOURNAL_CONFIG;
      else process.env.AUTOJOURNAL_CONFIG = previous.config;
      if (previous.xdgConfig === undefined) delete process.env.XDG_CONFIG_HOME;
      else process.env.XDG_CONFIG_HOME = previous.xdgConfig;
      if (previous.data === undefined) delete process.env.XDG_DATA_HOME;
      else process.env.XDG_DATA_HOME = previous.data;
      if (previous.state === undefined) delete process.env.XDG_STATE_HOME;
      else process.env.XDG_STATE_HOME = previous.state;
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  },
);

test("legacyPiJournalRoot follows Pi's agent directory override", () => {
  assert.equal(
    legacyPiJournalRoot({ PI_CODING_AGENT_DIR: "/custom/agent" }),
    path.join("/custom/agent", "journals"),
  );
  assert.equal(
    legacyPiJournalRoot({}),
    path.join(os.homedir(), ".pi", "agent", "journals"),
  );
});

// End-to-end against the real binary: capture with an active selection, then
// search and get through the same CLI surface the extension uses. Skipped
// when no binary is available (e.g. a consumer running tests before install).
test("end-to-end capture -> search -> get through the in-process engine", async () => {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "aj-adapter-"));
  const index = path.join(tmp, "index.v2.json");
  const thesaurus = path.join(tmp, "thesaurus.json");
  fs.writeFileSync(thesaurus, "{}");
  // Isolate from any real owner config/thesaurus on the host.
  const previous = {
    thesaurus: process.env.AUTOJOURNAL_THESAURUS,
    missLog: process.env.AUTOJOURNAL_MISS_LOG,
    config: process.env.AUTOJOURNAL_CONFIG,
    xdgConfig: process.env.XDG_CONFIG_HOME,
    data: process.env.XDG_DATA_HOME,
  };
  process.env.AUTOJOURNAL_THESAURUS = thesaurus;
  process.env.AUTOJOURNAL_MISS_LOG = path.join(tmp, "misses.jsonl");
  delete process.env.AUTOJOURNAL_CONFIG;
  process.env.XDG_CONFIG_HOME = path.join(tmp, "config");
  process.env.XDG_DATA_HOME = path.join(tmp, "data");
  try {
    const wire = (over: Record<string, unknown>): string =>
      JSON.stringify({
        schema_version: 1,
        world: "main",
        scope: "default",
        lane: "conversation",
        harness: "pi",
        adapter_version: "2.0.0",
        session_id: "e2e-session",
        turn_id: "e2e-turn-1",
        event_time_ms: Date.now(),
        capture_policy: "pi-visible-v2",
        turn_outcome: "completed",
        user_content: "how did the quokka enclosure fare",
        assistant_result: "the quokka enclosure needed reindexing today",
        tools: [{ name: "bash" }],
        ...over,
      });
    // Capture resolves the root the way every fresh host does: no config
    // and the host-neutral XDG data default.
    const captured = runCli(["capture", "--index", index], wire({}));
    const report = JSON.parse(captured.stdout);
    assert.equal(report.outcome, "published");
    assert.match(report.path, /^\d{4}\/\d{2}\/\d{2}\//);
    assert.equal(
      fs.existsSync(path.join(process.env.XDG_DATA_HOME as string, "autojournal", "journals", report.path)),
      true,
    );

    for (const [harness, turn, sentinel] of [
      ["claude-code", "claude-turn", "claudecrossharness"],
      ["codex", "codex-turn", "codexcrossharness"],
    ] as const) {
      const crossCapture = runCli(
        ["capture", "--index", index],
        wire({
          harness,
          session_id: `${harness}-session`,
          turn_id: turn,
          user_content: `remember ${sentinel}`,
          assistant_result: `${sentinel} was captured through the shared engine`,
        }),
      );
      assert.equal(JSON.parse(crossCapture.stdout).outcome, "published");
      const crossSearch = runCli([
        "search", sentinel, "--world", "main", "--scope", "default",
        "--index", index, "--json",
      ]);
      assert.equal(JSON.parse(crossSearch.stdout).outcome, "match");
    }

    // No --world: an unconfigured search falls back to the capture world.
    const searched = runCli(["search", "quokka", "--index", index, "--json"]);
    const result = JSON.parse(searched.stdout);
    assert.equal(result.outcome, "match");
    const hit = result.results[0];
    const rendered = renderSearchResults(result);
    assert.match(rendered, /quokka/);
    // Guard the wire contract, not just the snippet text: the header must
    // render the engine's index object without leaking a stringified value.
    assert.match(rendered, /^\d+ of \d+ matching result\(s\):/);
    assert.doesNotMatch(rendered, /object Object/);

    const isolatedCapture = runCli(
      ["capture", "--index", index],
      wire({
        world: "isolated-work",
        scope: "client:a",
        turn_id: "e2e-turn-2",
        user_content: "record the platypus release checklist",
        assistant_result: "the platypus release remains isolated",
        tools: [],
      }),
    );
    assert.match(
      JSON.parse(isolatedCapture.stdout).path,
      /^worlds\/isolated-work\/scopes\/client:a\/\d{4}\/\d{2}\/\d{2}\//,
    );
    const hiddenFromDefault = runCli(
      ["search", "platypus", "--world", "main", "--scope", "default", "--index", index, "--json"],
    );
    assert.equal(JSON.parse(hiddenFromDefault.stdout).outcome, "no_match");
    const visibleInSelection = runCli([
      "search", "platypus", "--world", "isolated-work", "--scope", "client:a",
      "--index", index, "--json",
    ]);
    assert.equal(JSON.parse(visibleInSelection.stdout).outcome, "match");
    const catalogRun = runCli(["catalog", "--index", index, "--json"]);
    assert.deepEqual(JSON.parse(catalogRun.stdout).pairs, [
      { world: "main", scope: "default" },
      { world: "isolated-work", scope: "client:a" },
    ]);

    const got = runCli([
      "get", "--episode", hit.episode_id, "--revision", hit.revision,
      "--index", index, "--json",
    ]);
    const opened = JSON.parse(got.stdout);
    assert.equal(opened.outcome, "match");
    assert.match(renderGetResult(opened), /quokka/);
    const isolatedGet = runCli([
      "get", "--episode", hit.episode_id, "--revision", hit.revision,
      "--world", "isolated-work", "--scope", "client:a",
      "--index", index, "--json",
    ]);
    assert.equal(JSON.parse(isolatedGet.stdout).outcome, "gone");
  } finally {
    if (previous.thesaurus === undefined) delete process.env.AUTOJOURNAL_THESAURUS;
    else process.env.AUTOJOURNAL_THESAURUS = previous.thesaurus;
    if (previous.missLog === undefined) delete process.env.AUTOJOURNAL_MISS_LOG;
    else process.env.AUTOJOURNAL_MISS_LOG = previous.missLog;
    if (previous.config === undefined) delete process.env.AUTOJOURNAL_CONFIG;
    else process.env.AUTOJOURNAL_CONFIG = previous.config;
    if (previous.xdgConfig === undefined) delete process.env.XDG_CONFIG_HOME;
    else process.env.XDG_CONFIG_HOME = previous.xdgConfig;
    if (previous.data === undefined) delete process.env.XDG_DATA_HOME;
    else process.env.XDG_DATA_HOME = previous.data;
    fs.rmSync(tmp, { recursive: true, force: true });
  }
});

test("adapterStatePath resolves beside the resolved owner config", () => {
  assert.equal(
    adapterStatePath({ AUTOJOURNAL_CONFIG: "/elsewhere/my-config.json" }),
    path.join("/elsewhere", "pi-adapter.json"),
  );
  assert.equal(
    adapterStatePath({ XDG_CONFIG_HOME: "/xdg" }),
    path.join("/xdg", "autojournal", "pi-adapter.json"),
  );
  assert.equal(
    adapterStatePath({}),
    path.join(os.homedir(), ".config", "autojournal", "pi-adapter.json"),
  );
});

test("subagent capture lever round-trips; missing, malformed, or mistyped state means off", () => {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "aj-lever-"));
  try {
    const file = path.join(tmp, "autojournal", "pi-adapter.json");
    assert.equal(readSubagentCapture(file), false, "missing file");
    writeSubagentCapture(file, true);
    assert.equal(readSubagentCapture(file), true);
    writeSubagentCapture(file, false);
    assert.equal(readSubagentCapture(file), false);
    fs.writeFileSync(file, "not json");
    assert.equal(readSubagentCapture(file), false, "malformed file");
    fs.writeFileSync(file, JSON.stringify({ capture_subagent_sessions: "yes" }));
    assert.equal(readSubagentCapture(file), false, "non-boolean value");
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true });
  }
});

test("sessionHeaderIsSubagent reads the header's parentSession field", () => {
  assert.equal(sessionHeaderIsSubagent(JSON.stringify({ type: "session", parentSession: "/p.jsonl" })), true);
  assert.equal(sessionHeaderIsSubagent(JSON.stringify({ type: "session" })), false);
  assert.equal(sessionHeaderIsSubagent("not json"), false);
  assert.equal(sessionHeaderIsSubagent(null), false);
});

test("menu title announces subagent capture only while the lever is on", () => {
  assert.doesNotMatch(formatMenuTitle(null, DEFAULT_SELECTION), /Subagent capture/);
  assert.doesNotMatch(formatMenuTitle(null, DEFAULT_SELECTION, true, false), /Subagent capture/);
  assert.match(
    formatMenuTitle(null, DEFAULT_SELECTION, true, true),
    /Subagent capture: ON \(subagent sessions are being journaled\)/,
  );
});

test(
  "menu subagent toggle writes the adapter state file and the next title shows it",
  async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "aj-lever-menu-"));
    const previous = {
      config: process.env.AUTOJOURNAL_CONFIG,
      xdgConfig: process.env.XDG_CONFIG_HOME,
      data: process.env.XDG_DATA_HOME,
      state: process.env.XDG_STATE_HOME,
    };
    delete process.env.AUTOJOURNAL_CONFIG;
    process.env.XDG_CONFIG_HOME = path.join(tmp, "config");
    process.env.XDG_DATA_HOME = path.join(tmp, "data");
    process.env.XDG_STATE_HOME = path.join(tmp, "state");
    try {
      let command: ((args: string, ctx: unknown) => Promise<void>) | undefined;
      const fakePi = {
        on() {},
        registerTool() {},
        registerCommand(_name: string, spec: { handler: typeof command }) {
          command = spec.handler;
        },
        appendEntry() {},
      };
      autojournalExtension(fakePi as never);
      assert.ok(command);
      let selects = 0;
      const titles: string[] = [];
      await command!("", {
        hasUI: true,
        ui: {
          notify() {},
          async select(title: string, options: string[]) {
            titles.push(title);
            selects += 1;
            if (selects === 1) return options.find((option) => option.startsWith("Subagent capture:"));
            return "Close";
          },
          async input() {
            return undefined;
          },
        },
      });
      const stateFile = path.join(tmp, "config", "autojournal", "pi-adapter.json");
      assert.equal(readSubagentCapture(stateFile), true, "toggle wrote the lever");
      assert.match(titles[1], /Subagent capture: ON/, "next menu render reflects the lever");
    } finally {
      if (previous.config === undefined) delete process.env.AUTOJOURNAL_CONFIG;
      else process.env.AUTOJOURNAL_CONFIG = previous.config;
      if (previous.xdgConfig === undefined) delete process.env.XDG_CONFIG_HOME;
      else process.env.XDG_CONFIG_HOME = previous.xdgConfig;
      if (previous.data === undefined) delete process.env.XDG_DATA_HOME;
      else process.env.XDG_DATA_HOME = previous.data;
      if (previous.state === undefined) delete process.env.XDG_STATE_HOME;
      else process.env.XDG_STATE_HOME = previous.state;
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  },
);

test(
  "subagent sessions publish only when the lever is on",
  async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "aj-subgate-"));
    const previous = {
      config: process.env.AUTOJOURNAL_CONFIG,
      xdgConfig: process.env.XDG_CONFIG_HOME,
      data: process.env.XDG_DATA_HOME,
      state: process.env.XDG_STATE_HOME,
    };
    delete process.env.AUTOJOURNAL_CONFIG;
    process.env.XDG_CONFIG_HOME = path.join(tmp, "config");
    process.env.XDG_DATA_HOME = path.join(tmp, "data");
    process.env.XDG_STATE_HOME = path.join(tmp, "state");
    try {
      const handlers = new Map<string, (event: unknown, ctx: unknown) => Promise<void>>();
      const fakePi = {
        on(name: string, handler: (event: unknown, ctx: unknown) => Promise<void>) {
          handlers.set(name, handler);
        },
        registerTool() {},
        registerCommand() {},
        appendEntry() {},
      };
      autojournalExtension(fakePi as never);

      // A session whose log header carries parentSession is a subagent
      // session, however it was spawned.
      const subagentFile = path.join(tmp, "subagent-session.jsonl");
      fs.writeFileSync(
        subagentFile,
        `${JSON.stringify({ type: "session", id: "sub-1", parentSession: "/tmp/parent.jsonl" })}\n`,
      );
      const ctx = {
        sessionManager: {
          getSessionFile: () => subagentFile,
          getLeafId: () => "leaf-subagent-gate",
          getBranch: () => [1],
          getEntries: () => [],
        },
        ui: { notify() {} },
      };
      await handlers.get("session_start")!({}, ctx);

      const messages = [
        { role: "user", content: "subagent gating sentinel" },
        { role: "assistant", content: [{ type: "text", text: "done" }] },
      ];
      const journals = path.join(tmp, "data", "autojournal", "journals");
      const stateFile = path.join(tmp, "config", "autojournal", "pi-adapter.json");

      // Lever off: a subagent session settling in print mode is skipped.
      await handlers.get("agent_end")!({ messages }, ctx);
      await handlers.get("agent_settled")!({}, { ...ctx, mode: "print" });
      assert.equal(fs.existsSync(journals), false);

      // Lever on: the same session publishes.
      writeSubagentCapture(stateFile, true);
      await handlers.get("agent_end")!({ messages }, ctx);
      await handlers.get("agent_settled")!({}, { ...ctx, mode: "print" });
      assert.equal(fs.existsSync(journals), true);
    } finally {
      if (previous.config === undefined) delete process.env.AUTOJOURNAL_CONFIG;
      else process.env.AUTOJOURNAL_CONFIG = previous.config;
      if (previous.xdgConfig === undefined) delete process.env.XDG_CONFIG_HOME;
      else process.env.XDG_CONFIG_HOME = previous.xdgConfig;
      if (previous.data === undefined) delete process.env.XDG_DATA_HOME;
      else process.env.XDG_DATA_HOME = previous.data;
      if (previous.state === undefined) delete process.env.XDG_STATE_HOME;
      else process.env.XDG_STATE_HOME = previous.state;
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  },
);

test(
  "headless owner runs stay skipped even with the lever on",
  async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "aj-headless-"));
    const previous = {
      config: process.env.AUTOJOURNAL_CONFIG,
      xdgConfig: process.env.XDG_CONFIG_HOME,
      data: process.env.XDG_DATA_HOME,
      state: process.env.XDG_STATE_HOME,
    };
    delete process.env.AUTOJOURNAL_CONFIG;
    process.env.XDG_CONFIG_HOME = path.join(tmp, "config");
    process.env.XDG_DATA_HOME = path.join(tmp, "data");
    process.env.XDG_STATE_HOME = path.join(tmp, "state");
    try {
      const handlers = new Map<string, (event: unknown, ctx: unknown) => Promise<void>>();
      const fakePi = {
        on(name: string, handler: (event: unknown, ctx: unknown) => Promise<void>) {
          handlers.set(name, handler);
        },
        registerTool() {},
        registerCommand() {},
        appendEntry() {},
      };
      autojournalExtension(fakePi as never);

      // No parentSession in the header: a headless owner run, not a
      // subagent session.
      const headlessFile = path.join(tmp, "headless-session.jsonl");
      fs.writeFileSync(headlessFile, `${JSON.stringify({ type: "session", id: "headless-1" })}\n`);
      const ctx = {
        sessionManager: {
          getSessionFile: () => headlessFile,
          getLeafId: () => "leaf-headless-gate",
          getBranch: () => [1],
          getEntries: () => [],
        },
        ui: { notify() {} },
      };
      await handlers.get("session_start")!({}, ctx);

      writeSubagentCapture(path.join(tmp, "config", "autojournal", "pi-adapter.json"), true);
      const messages = [
        { role: "user", content: "headless gating sentinel" },
        { role: "assistant", content: [{ type: "text", text: "done" }] },
      ];
      const journals = path.join(tmp, "data", "autojournal", "journals");
      await handlers.get("agent_end")!({ messages }, ctx);
      await handlers.get("agent_settled")!({}, { ...ctx, mode: "print" });
      assert.equal(fs.existsSync(journals), false);
    } finally {
      if (previous.config === undefined) delete process.env.AUTOJOURNAL_CONFIG;
      else process.env.AUTOJOURNAL_CONFIG = previous.config;
      if (previous.xdgConfig === undefined) delete process.env.XDG_CONFIG_HOME;
      else process.env.XDG_CONFIG_HOME = previous.xdgConfig;
      if (previous.data === undefined) delete process.env.XDG_DATA_HOME;
      else process.env.XDG_DATA_HOME = previous.data;
      if (previous.state === undefined) delete process.env.XDG_STATE_HOME;
      else process.env.XDG_STATE_HOME = previous.state;
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  },
);

test("menu search-quality section promotes a weak query into an alias behind a confirm", async () => {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "aj-quality-"));
  const previous = {
    config: process.env.AUTOJOURNAL_CONFIG,
    thesaurus: process.env.AUTOJOURNAL_THESAURUS,
    missLog: process.env.AUTOJOURNAL_MISS_LOG,
    xdgConfig: process.env.XDG_CONFIG_HOME,
    data: process.env.XDG_DATA_HOME,
    state: process.env.XDG_STATE_HOME,
  };
  delete process.env.AUTOJOURNAL_CONFIG;
  process.env.AUTOJOURNAL_THESAURUS = path.join(tmp, "thesaurus.json");
  process.env.AUTOJOURNAL_MISS_LOG = path.join(tmp, "misses.jsonl");
  process.env.XDG_CONFIG_HOME = path.join(tmp, "config");
  process.env.XDG_DATA_HOME = path.join(tmp, "data");
  process.env.XDG_STATE_HOME = path.join(tmp, "state");
  try {
    fs.writeFileSync(
      path.join(tmp, "misses.jsonl"),
      JSON.stringify({ ts: "t", query: "weak firmware query", terms: ["weak", "firmware", "query"], best: 0, top: null }) + "\n",
    );
    let command: ((args: string, ctx: unknown) => Promise<void>) | undefined;
    const fakePi = {
      on() {},
      registerTool() {},
      registerCommand(_name: string, spec: { handler: typeof command }) {
        command = spec.handler;
      },
      appendEntry() {},
    };
    autojournalExtension(fakePi as never);
    assert.ok(command);
    const titles: string[] = [];
    let selects = 0;
    await command("", {
      hasUI: true,
      ui: {
        notify() {},
        async select(title: string, options: string[]) {
          titles.push(title);
          selects += 1;
          if (selects === 1) return "Search quality";
          if (selects === 2) {
            assert.ok(options.some((o) => o.startsWith('Promote: "weak firmware query" (1x)')), options.join("|"));
            return options.find((o) => o.startsWith("Promote:"));
          }
          if (selects === 3) return "Add it"; // the confirm step
          if (selects === 4) return "Back";
          return "Close";
        },
        async input(title: string) {
          return title.startsWith("Casual term") ? "firmware" : "fwupd lvfs";
        },
      },
    });
    assert.match(titles[1], /Miss log: off/);
    assert.match(titles[1], /Weak queries: 1/);
    const written = JSON.parse(fs.readFileSync(path.join(tmp, "thesaurus.json"), "utf8"));
    assert.deepEqual(written, { firmware: ["fwupd", "lvfs"] });
    // The refreshed section (rendered again before "Back") now lists the
    // alias for confirmed removal.
    assert.match(titles[3] ?? "", /Aliases: 1/);
  } finally {
    if (previous.config === undefined) delete process.env.AUTOJOURNAL_CONFIG;
    else process.env.AUTOJOURNAL_CONFIG = previous.config;
    if (previous.thesaurus === undefined) delete process.env.AUTOJOURNAL_THESAURUS;
    else process.env.AUTOJOURNAL_THESAURUS = previous.thesaurus;
    if (previous.missLog === undefined) delete process.env.AUTOJOURNAL_MISS_LOG;
    else process.env.AUTOJOURNAL_MISS_LOG = previous.missLog;
    if (previous.xdgConfig === undefined) delete process.env.XDG_CONFIG_HOME;
    else process.env.XDG_CONFIG_HOME = previous.xdgConfig;
    if (previous.data === undefined) delete process.env.XDG_DATA_HOME;
    else process.env.XDG_DATA_HOME = previous.data;
    if (previous.state === undefined) delete process.env.XDG_STATE_HOME;
    else process.env.XDG_STATE_HOME = previous.state;
    fs.rmSync(tmp, { recursive: true, force: true });
  }
});
