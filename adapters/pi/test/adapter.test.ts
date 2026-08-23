import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import {
  adapterStatePath,
  default as autojournalExtension,
  buildPayload,
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
  resolveBinary,
  runBinary,
  sanitizeToken,
  selectionFromEntries,
  sessionHeaderIsSubagent,
  SESSION_POLICY_ENTRY,
  stableTurnId,
  summarizeRun,
  syncResultBody,
  writeSubagentCapture,
  QUERY_TIMEOUT_MS,
  SYNC_TIMEOUT_MS,
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

test("sync runs on a maintenance budget, not the query budget", () => {
  // Regression: a 4k-episode rebuild takes ~36s; under the 15s query budget
  // the adapter SIGKILLed it and reported "(sync produced no output)".
  assert.ok(SYNC_TIMEOUT_MS > QUERY_TIMEOUT_MS);
});

test("syncResultBody names the timeout instead of blaming the binary", () => {
  const timedOut = { code: null, stdout: "", stderr: "", timedOut: true };
  assert.match(syncResultBody(timedOut), /timed out after 600s/);
  assert.match(syncResultBody(timedOut), /rolled back unchanged/);
  const ok = { code: 0, stdout: "indexed: 3\n", stderr: "", timedOut: false };
  assert.equal(syncResultBody(ok), "indexed: 3");
  const stderrOnly = { code: 1, stdout: "", stderr: "boom\n", timedOut: false };
  assert.equal(syncResultBody(stderrOnly), "boom");
  const silent = { code: 0, stdout: "", stderr: "", timedOut: false };
  assert.equal(syncResultBody(silent), "(sync produced no output)");
});

test(
  "/autojournal sync owns the footer status for its duration, then clears it",
  { skip: e2eBinary === null },
  async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "aj-syncstatus-"));
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
  const previousBin = process.env.AUTOJOURNAL_BIN;
  const previousLog = process.env.AUTOJOURNAL_TEST_LOG;
  const script = path.join(tmp, "fake-autojournal");
  const log = path.join(tmp, "args.log");
  const episode = "aj1-0123456789abcdef0123456789abcdef";
  const revision = `sha256:${"a".repeat(64)}`;
  fs.writeFileSync(
    script,
    `#!/bin/sh\n` +
      `printf '%s\\n' "$*" >> "$AUTOJOURNAL_TEST_LOG"\n` +
      `case "$1" in\n` +
      `  search) [ "$2" = "slow" ] && sleep 0.1; printf '%s\\n' '${JSON.stringify({
        outcome: "match",
        total: 1,
        results: [{
          episode_id: episode,
          revision,
          path: `2026/08/12/${episode}.md`,
          line: 7,
          snippet: "reference sentinel",
          score: 1,
        }],
        index: { freshness: "fresh" },
      })}' ;;\n` +
      `  get) printf '%s\\n' '${JSON.stringify({
        outcome: "match",
        episode_id: episode,
        revision,
        path: `2026/08/12/${episode}.md`,
        line_start: 7,
        line_end: 8,
        content: "reference sentinel",
      })}' ;;\n` +
      `  *) printf '%s\\n' '{"pairs":[{"world":"main","scope":"default"}]}' ;;\n` +
      `esac\n`,
  );
  fs.chmodSync(script, 0o755);
  process.env.AUTOJOURNAL_BIN = script;
  process.env.AUTOJOURNAL_TEST_LOG = log;

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
    autojournalExtension(fakePi as never);
    const search = tools.get("memory_search") as ToolSpec;
    const get = tools.get("memory_get") as ToolSpec;
    assert.deepEqual(get.parameters.required, ["reference"]);
    assert.ok(get.parameters.properties?.reference);
    assert.equal(get.parameters.properties?.episode_id, undefined);
    assert.equal(get.parameters.properties?.revision, undefined);

    const searched = await search.execute("search-1", { query: "reference sentinel" } as never);
    assert.match(searched.content[0].text, /\[reference 1\]/);
    const searchDetails = searched.details as { evidence_references: Array<{ reference: number }> };
    assert.equal(searchDetails.evidence_references[0].reference, 1);

    const opened = await get.execute("get-1", { reference: 1, lines: "7-8" } as never);
    assert.match(opened.content[0].text, /reference sentinel/);
    assert.match(
      fs.readFileSync(log, "utf8"),
      new RegExp(`get --episode ${episode} --revision ${revision} --json --world main --scope default --lines 7-8`),
    );

    // A resumed branch restores references from durable tool-result details.
    // The reference retains the world/scope where search found it even when
    // the branch's active selection later changes.
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
    await get.execute("get-resumed", { reference: 1 } as never);
    const resumedCall = fs.readFileSync(log, "utf8").trim().split("\n").at(-1);
    assert.match(resumedCall ?? "", /--world main --scope default/);

    const delayedSearch = search.execute("search-slow", { query: "slow" } as never);
    await new Promise((resolve) => setTimeout(resolve, 20));
    await sessionTree({}, resumedContext);
    const abandoned = await delayedSearch;
    assert.match(abandoned.content[0].text, /branch changed while search was running/);
    const beforeAbandonedGet = fs.readFileSync(log, "utf8");
    const abandonedGet = await get.execute("get-abandoned", { reference: 2 } as never);
    assert.match(abandonedGet.content[0].text, /run memory_search again/);
    assert.equal(fs.readFileSync(log, "utf8"), beforeAbandonedGet);

    const beforeUnknown = fs.readFileSync(log, "utf8");
    const unknown = await get.execute("get-2", { reference: 999 } as never);
    assert.match(unknown.content[0].text, /run memory_search again/);
    assert.equal(fs.readFileSync(log, "utf8"), beforeUnknown, "unknown references must not invoke the core");

    const prepared = get.prepareArguments?.({
      episode_id: episode,
      revision,
      lines: "9-10",
    });
    assert.ok(prepared);
    assert.equal(typeof prepared.reference, "number");
    await get.execute("get-legacy", prepared as never);
    assert.match(fs.readFileSync(log, "utf8"), /--lines 9-10/);
  } finally {
    if (previousBin === undefined) delete process.env.AUTOJOURNAL_BIN;
    else process.env.AUTOJOURNAL_BIN = previousBin;
    if (previousLog === undefined) delete process.env.AUTOJOURNAL_TEST_LOG;
    else process.env.AUTOJOURNAL_TEST_LOG = previousLog;
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
  "test_pi_menu_offers_reseal",
  { skip: e2eBinary === null },
  async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "aj-reseal-menu-"));
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

// One helper for the outcome-tolerance pair: a fake binary that reports a
// chosen capture outcome, a driven extension, and the notifications plus the
// /autojournal status line it produced.
async function driveCaptureWithOutcome(outcome: string): Promise<{
  notifications: string[];
  statusLine: string;
}> {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "aj-outcome-"));
  const previousBin = process.env.AUTOJOURNAL_BIN;
  const script = path.join(tmp, "fake-autojournal");
  fs.writeFileSync(
    script,
    `#!/bin/sh\necho '{"outcome":"${outcome}","index":"fresh"}'\n`,
  );
  fs.chmodSync(script, 0o755);
  process.env.AUTOJOURNAL_BIN = script;
  try {
    const handlers = new Map<string, (event: unknown, ctx: unknown) => Promise<void>>();
    let commandHandler: ((args: string, ctx: unknown) => Promise<void>) | null = null;
    const fakePi = {
      on(name: string, handler: (event: unknown, ctx: unknown) => Promise<void>) {
        handlers.set(name, handler);
      },
      registerTool() {},
      registerCommand(_name: string, spec: { handler(args: string, ctx: unknown): Promise<void> }) {
        commandHandler = spec.handler;
      },
      appendEntry() {},
    };
    autojournalExtension(fakePi as never);

    const notifications: string[] = [];
    const ctx = {
      mode: "tui",
      hasUI: false,
      sessionManager: {
        getLeafId: () => "leaf-outcome",
        getBranch: () => [1],
        getEntries: () => [],
      },
      ui: {
        notify(message: string) {
          notifications.push(message);
        },
      },
    };
    await handlers.get("agent_end")!(
      {
        messages: [
          { role: "user", content: "outcome tolerance sentinel" },
          { role: "assistant", content: [{ type: "text", text: "done" }] },
        ],
      },
      ctx,
    );
    await handlers.get("agent_settled")!({}, ctx);

    const before = notifications.length;
    await commandHandler!("status", ctx);
    const statusLine = notifications.slice(before).join("\n");
    return { notifications: notifications.slice(0, before), statusLine };
  } finally {
    if (previousBin === undefined) delete process.env.AUTOJOURNAL_BIN;
    else process.env.AUTOJOURNAL_BIN = previousBin;
    fs.rmSync(tmp, { recursive: true, force: true });
  }
}

test("test_pi_adapter_counts_superseded_as_success", async () => {
  const { notifications, statusLine } = await driveCaptureWithOutcome("superseded");
  for (const message of notifications) {
    assert.ok(!message.includes("capture failing"), `failure notification: ${message}`);
  }
  assert.match(statusLine, /1 superseded/);
  assert.match(statusLine, /0 failed/);
});

test("test_pi_adapter_does_not_fail_on_an_unknown_outcome", async () => {
  const { notifications, statusLine } = await driveCaptureWithOutcome("archived_v9");
  for (const message of notifications) {
    assert.ok(!message.includes("capture failing"), `failure notification: ${message}`);
  }
  assert.match(statusLine, /0 failed/);
  assert.match(statusLine, /1 with an outcome this adapter does not know/);
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
  { skip: e2eBinary === null },
  async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "aj-lever-menu-"));
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
  "subagent sessions publish only when the lever is on",
  { skip: e2eBinary === null },
  async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "aj-subgate-"));
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
  "headless owner runs stay skipped even with the lever on",
  { skip: e2eBinary === null },
  async () => {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "aj-headless-"));
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
