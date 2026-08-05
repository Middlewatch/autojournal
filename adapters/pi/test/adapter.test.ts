import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import {
  default as autojournalExtension,
  buildPayload,
  captureFromEntries,
  DEFAULT_SELECTION,
  extractText,
  formatMenuTitle,
  legacyPiJournalRoot,
  originHost,
  renderGetResult,
  renderSearchResults,
  resolveBinary,
  runBinary,
  sanitizeToken,
  selectionFromEntries,
  SESSION_POLICY_ENTRY,
  stableTurnId,
  summarizeRun,
  validScope,
  validWorld,
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

test("summarizeRun keeps last assistant text and dedups tool names", () => {
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
  assert.equal(summary.assistantText, "final answer");
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

test("buildPayload carries selected world/scope and sanitizes identities", () => {
  const payload = buildPayload({
    summary: { userText: "u", assistantText: "a", toolNames: ["weird tool!"] },
    sessionId: "2026-07-29 bad id",
    turnId: "t/1",
    eventTimeMs: 123,
    selection: { world: "isolated-work", scope: "client:a" },
  });
  assert.equal(payload.world, "isolated-work");
  assert.equal(payload.scope, "client:a");
  assert.equal(payload.lane, "conversation");
  assert.equal(payload.session_id, "2026-07-29-bad-id");
  assert.equal(payload.turn_id, "t/1");
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

test("buildPayload labels the originating machine and omits it when unknown", () => {
  const base = {
    summary: { userText: "u", assistantText: "a", toolNames: [] },
    sessionId: "s",
    turnId: "t",
    eventTimeMs: 1,
    selection: DEFAULT_SELECTION,
  };
  assert.equal(buildPayload({ ...base, host: "stealth" }).host, "stealth");
  assert.ok(!("host" in buildPayload({ ...base, host: null })));
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
  });
  assert.match(text, /1 of 9 matching result\(s\):/);
  assert.match(text, /aj1-x\.md:12/);
  assert.match(text, /> the fwupd refresh worked/);
  assert.match(text, /memory_get/);

  const stale = renderSearchResults({
    outcome: "match",
    total: 1,
    results: [hit],
    index: { freshness: "stale", indexed: 8, source: 9, edited_excluded: 0 },
  });
  assert.match(stale, /\(index stale\)/);
  assert.doesNotMatch(stale, /object Object/);
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
    /Capture: OFF — this session's turns are not being journaled/,
  );
});

test(
  "/autojournal menu creates and persists a session world",
  { skip: e2eBinary === null },
  async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "aj-menu-"));
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
            if (selects === 3) return "Save as default for new sessions";
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
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  },
);

test(
  "capture skips headless/subagent modes and publishes interactive turns",
  { skip: e2eBinary === null },
  async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "aj-mode-"));
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
      fs.rmSync(tmp, { recursive: true, force: true });
    }
  },
);

test(
  "menu capture toggle suppresses publishing for the session and persists as policy",
  { skip: e2eBinary === null },
  async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "aj-toggle-"));
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

test("resolveBinary honors AUTOJOURNAL_BIN and rejects missing paths", () => {
  // A dead override is never returned; resolution falls through to the
  // bundled platform binary when one is present, else null.
  const fallback = resolveBinary({ AUTOJOURNAL_BIN: "/nonexistent/bin", PATH: "" });
  assert.notEqual(fallback, "/nonexistent/bin");
  if (fallback !== null) assert.ok(fs.existsSync(fallback));
  const self = process.execPath; // any existing file works for the override
  assert.equal(resolveBinary({ AUTOJOURNAL_BIN: self, PATH: "" }), self);
});

// End-to-end against the real binary: capture with an active selection, then
// search and get through the same CLI surface the extension uses. Skipped
// when no binary is available (e.g. a consumer running tests before install).
test("end-to-end capture -> search -> get through the binary", { skip: e2eBinary === null }, async () => {
  const bin = e2eBinary as string;
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "aj-adapter-"));
  const index = path.join(tmp, "index.sqlite");
  const thesaurus = path.join(tmp, "thesaurus.json");
  fs.writeFileSync(thesaurus, "{}");
  // Isolate from any real owner config/thesaurus on the host.
  const env = {
    AUTOJOURNAL_THESAURUS: thesaurus,
    AUTOJOURNAL_MISS_LOG: path.join(tmp, "misses.jsonl"),
    XDG_CONFIG_HOME: path.join(tmp, "config"),
    XDG_DATA_HOME: path.join(tmp, "data"),
  };
  try {
    const payload = buildPayload({
      summary: {
        userText: "how did the quokka enclosure fare",
        assistantText: "the quokka enclosure needed reindexing today",
        toolNames: ["bash"],
      },
      sessionId: "e2e-session",
      turnId: "e2e-turn-1",
      eventTimeMs: Date.now(),
      selection: DEFAULT_SELECTION,
    });
    // Capture resolves the root the way every fresh host does: no config and
    // the host-neutral XDG data default.
    const captured = await runBinary(bin, ["capture", "--index", index], {
      stdin: JSON.stringify(payload),
      env,
    });
    const report = JSON.parse(captured.stdout);
    assert.equal(report.outcome, "published");
    assert.match(report.path, /^\d{4}\/\d{2}\/\d{2}\//);
    assert.equal(fs.existsSync(path.join(env.XDG_DATA_HOME, "autojournal", "journals", report.path)), true);

    for (const [harness, turn, sentinel] of [
      ["claude-code", "claude-turn", "claudecrossharness"],
      ["codex", "codex-turn", "codexcrossharness"],
    ] as const) {
      const crossHarness = {
        ...payload,
        harness,
        session_id: `${harness}-session`,
        turn_id: turn,
        user_content: `remember ${sentinel}`,
        assistant_result: `${sentinel} was captured through the shared core`,
      };
      const crossCapture = await runBinary(bin, ["capture", "--index", index], {
        stdin: JSON.stringify(crossHarness),
        env,
      });
      assert.equal(JSON.parse(crossCapture.stdout).outcome, "published");
      const crossSearch = await runBinary(
        bin,
        [
          "search", sentinel, "--world", "main", "--scope", "default",
          "--index", index, "--json",
        ],
        { env },
      );
      assert.equal(JSON.parse(crossSearch.stdout).outcome, "match");
    }

    // No --world: an unconfigured search falls back to the capture world.
    const searched = await runBinary(bin, [
      "search", "quokka", "--index", index, "--json",
    ], { timeoutMs: 15000, env });
    const result = JSON.parse(searched.stdout);
    assert.equal(result.outcome, "match");
    const hit = result.results[0];
    const rendered = renderSearchResults(result);
    assert.match(rendered, /quokka/);
    // Guard the wire contract, not just the snippet text: the header must
    // render the binary's index object without leaking a stringified value.
    assert.match(rendered, /^\d+ of \d+ matching result\(s\):/);
    assert.doesNotMatch(rendered, /object Object/);

    const isolatedPayload = buildPayload({
      summary: {
        userText: "record the platypus release checklist",
        assistantText: "the platypus release remains isolated",
        toolNames: [],
      },
      sessionId: "e2e-session",
      turnId: "e2e-turn-2",
      eventTimeMs: Date.now(),
      selection: { world: "isolated-work", scope: "client:a" },
    });
    const isolatedCapture = await runBinary(bin, ["capture", "--index", index], {
      stdin: JSON.stringify(isolatedPayload),
      env,
    });
    assert.match(
      JSON.parse(isolatedCapture.stdout).path,
      /^worlds\/isolated-work\/scopes\/client:a\/\d{4}\/\d{2}\/\d{2}\//,
    );
    const hiddenFromDefault = await runBinary(
      bin,
      ["search", "platypus", "--world", "main", "--scope", "default", "--index", index, "--json"],
      { env },
    );
    assert.equal(JSON.parse(hiddenFromDefault.stdout).outcome, "no_match");
    const visibleInSelection = await runBinary(
      bin,
      [
        "search", "platypus", "--world", "isolated-work", "--scope", "client:a",
        "--index", index, "--json",
      ],
      { env },
    );
    assert.equal(JSON.parse(visibleInSelection.stdout).outcome, "match");
    const catalog = await runBinary(bin, ["catalog", "--index", index, "--json"], { env });
    assert.deepEqual(JSON.parse(catalog.stdout).pairs, [
      { world: "main", scope: "default" },
      { world: "isolated-work", scope: "client:a" },
    ]);

    const got = await runBinary(bin, [
      "get", "--episode", hit.episode_id, "--revision", hit.revision,
      "--index", index, "--json",
    ], { env });
    const opened = JSON.parse(got.stdout);
    assert.equal(opened.outcome, "match");
    assert.match(renderGetResult(opened), /quokka/);
    const isolatedGet = await runBinary(bin, [
      "get", "--episode", hit.episode_id, "--revision", hit.revision,
      "--world", "isolated-work", "--scope", "client:a",
      "--index", index, "--json",
    ], { env });
    assert.equal(JSON.parse(isolatedGet.stdout).outcome, "gone");
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true });
  }
});
