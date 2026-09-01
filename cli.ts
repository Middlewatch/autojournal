// AutoJournal owner CLI on the in-process TypeScript engine: the same
// verbs and the same --json interface contract as the v1 binary, as thin
// wiring over the engine the extension calls. The --json shapes are the
// Interface-tier contract; every renderer lives here so the surface stays
// reviewable in one file.
//
// Verbs arrive with their engine slices: capture and default are live;
// status, catalog, sync, reseal, search, get, and alias land with the
// index and retrieval slices.

import * as process from "node:process";
import * as fs from "node:fs";
import { pathToFileURL } from "node:url";
import {
  MAX_PAYLOAD_BYTES,
  PAYLOAD_SCHEMA_VERSION,
  EPISODE_SCHEMA,
  parsePayload,
  CaptureError,
  type RawPayload,
} from "./src/contracts.ts";
import { DIGEST_PREFIX } from "./src/identity.ts";
import {
  defaultConfig,
  loadConfig,
  saveCaptureDefaults,
  ConfigError,
  type Config,
} from "./src/config.ts";
import { defaultJournalRoot, defaultIndexPath, MissingHomeError, processEnviron, type Environ } from "./src/paths.ts";
import { capture, type CaptureResult } from "./src/store.ts";
import { statusOf, sync, reseal, catalog, SyncError } from "./src/ops.ts";
import { SNAPSHOT_FORMAT_VERSION, openSnapshot, type Snapshot } from "./src/index.ts";
import { TOKENIZER_VERSION, SCORER_VERSION, CONFIDENCE_POLICY_VERSION } from "./src/retrieval.ts";
import {
  search,
  get,
  DEFAULT_LANES,
  type SearchOutput,
  type CreditMode,
  type Hit,
} from "./src/search.ts";
import { loadAliasMapFile } from "./src/aliases.ts";
import {
  addAlias,
  removeAlias,
  aggregateMisses,
  logSearchMiss,
  AliasError,
} from "./src/ops-alias.ts";
import { missLogPath } from "./src/paths.ts";
import { openExistingRoot, type JournalRoot } from "./src/corpus.ts";
import { rootDigestHex, thesaurusPath } from "./src/paths.ts";
import { isoFromMs } from "./src/render.ts";
import { MAX_QUERY_BYTES, MAX_RESULTS_LIMIT, type Lane } from "./src/contracts.ts";

export const CLI_VERSION = "2.0.1";

const USAGE = `usage: autojournal <command> [options]

commands:
  capture   read one completed-turn JSON payload on stdin and publish it
  search    ranked, bounded recall: autojournal search <query words...>
  get       open one evidence reference exactly
  alias     thesaurus upkeep: list | add <term> <canonical...> |
            remove <term> [canonical] | candidates
  default   show or set the owner default world/scope (--world/--scope)
  status    report journal root, corpus, and index health
  catalog   list discovered worlds and scopes
  sync      rebuild/repair the index snapshot from the Markdown corpus
  reseal    re-attest owner-edited episodes (--preview to only list them)
  version   print version and schema identities

options:
  --config <path>    explicit config file (default: XDG lookup)
  --root <path>      journal root override (bypasses config/default)
  --index <path>     index snapshot override (default: XDG state dir)
  --world <id>       world to search / world value for default
  --scope <token>    restrict search to one scope / scope for default
  --lanes <a,b>      lanes to search (default:
                     conversation,delegated_work,imported_legacy)
  --limit <n>        page size (default from config, cap 100)
  --cursor <c>       continue a previous search page
  --credit-mode <m>  term crediting: substring | word_start | whole_word
  --episode <id>     (get) evidence episode id
  --revision <r>     (get) sha256:<hex> revision the evidence had
  --path <rel>       (get) path hint from a search result
  --lines <a-b>      (get) explicit line bounds
  --json             machine-readable output
`;

export const EXIT_OK = 0;
export const EXIT_FAILURE = 1;
export const EXIT_MALFORMED = 2;
export const EXIT_CONFLICT = 3;

/** The process boundary, so command logic is testable without exec. */
export interface CliIo {
  env: Environ;
  stdin: () => Uint8Array;
  stdout: (text: string) => void;
  stderr: (text: string) => void;
  nowMs: () => number;
}

interface Opts {
  config?: string;
  root?: string;
  defaultRoot?: string;
  index?: string;
  world?: string;
  scope?: string;
  lanes?: string;
  limit?: number;
  cursor?: string;
  episode?: string;
  revision?: string;
  path?: string;
  lines?: string;
  creditMode?: string;
  json: boolean;
  preview: boolean;
  positionals: string[];
}

// clockFromEnv resolves the process clock: AUTOJOURNAL_NOW_MS pinned to a
// decimal millisecond timestamp wins, else the wall clock. The override
// exists because ranking parity runs are only reproducible with the clock
// pinned; an unset, empty, or malformed value is ignored.
export function clockFromEnv(env: Environ): () => number {
  const v = env("AUTOJOURNAL_NOW_MS");
  if (v !== undefined && v !== "" && /^[0-9]+$/.test(v)) {
    const ms = Number(v);
    if (Number.isSafeInteger(ms)) return () => ms;
  }
  return () => Math.max(0, Date.now());
}

export function run(args: string[], io: CliIo): number {
  if (args.length === 0) {
    io.stderr(USAGE);
    return EXIT_MALFORMED;
  }
  const command = args[0];

  const o: Opts = { json: false, preview: false, positionals: [] };
  const rest = args.slice(1);
  const valueSlots: Record<string, (v: string) => void> = {
    "--config": (v) => (o.config = v),
    "--root": (v) => (o.root = v),
    // Undocumented on purpose: pre-1.0 adapters passed a host fallback
    // root here, ranking below owner config and above the XDG default.
    "--default-root": (v) => (o.defaultRoot = v),
    "--index": (v) => (o.index = v),
    "--world": (v) => (o.world = v),
    "--scope": (v) => (o.scope = v),
    "--lanes": (v) => (o.lanes = v),
    "--cursor": (v) => (o.cursor = v),
    "--episode": (v) => (o.episode = v),
    "--revision": (v) => (o.revision = v),
    "--path": (v) => (o.path = v),
    "--lines": (v) => (o.lines = v),
    "--credit-mode": (v) => (o.creditMode = v),
  };
  for (let i = 0; i < rest.length; i++) {
    const arg = rest[i];
    if (arg === "--json") {
      o.json = true;
      continue;
    }
    if (arg === "--preview") {
      o.preview = true;
      continue;
    }
    if (!arg.startsWith("--")) {
      o.positionals.push(arg);
      continue;
    }
    if (arg === "--limit") {
      i++;
      const value = rest[i];
      if (value === undefined || !/^[0-9]+$/.test(value)) {
        io.stderr("--limit must be a positive integer\n");
        return EXIT_MALFORMED;
      }
      o.limit = Number(value);
      continue;
    }
    const slot = valueSlots[arg];
    if (slot === undefined) {
      io.stderr(USAGE);
      return EXIT_MALFORMED;
    }
    i++;
    if (i >= rest.length) {
      io.stderr(arg + " requires a value\n");
      return EXIT_MALFORMED;
    }
    slot(rest[i]);
  }

  if (command === "version") {
    io.stdout(
      `autojournal ${CLI_VERSION} (payload schema v${PAYLOAD_SCHEMA_VERSION}, episode schema ${EPISODE_SCHEMA}, index schema v${SNAPSHOT_FORMAT_VERSION}, ${TOKENIZER_VERSION}, ${SCORER_VERSION}, ${CONFIDENCE_POLICY_VERSION})\n`,
    );
    return EXIT_OK;
  }

  // Configuration is optional because commands can use the host-neutral
  // journal default when neither config nor --root is present.
  const envConfig = io.env("AUTOJOURNAL_CONFIG");
  const explicitConfig = o.config !== undefined || (envConfig !== undefined && envConfig !== "");
  if (o.config === "") {
    // An explicitly named empty path can never load. Refuse it rather than
    // letting the empty string read as "no explicit path" and fall back
    // silently — that would address a corpus the caller did not ask for.
    io.stderr("explicit AutoJournal config was not found\n");
    return EXIT_FAILURE;
  }
  let cfg: Config = defaultConfig();
  try {
    cfg = loadConfig(io.env, o.config ?? "").config;
  } catch (err) {
    if (err instanceof ConfigError && err.code === "not_found") {
      if (explicitConfig) {
        io.stderr("explicit AutoJournal config was not found\n");
        return EXIT_FAILURE;
      }
    } else if (err instanceof ConfigError && err.code === "malformed") {
      io.stderr("config is malformed (see config.json schema)\n");
      return EXIT_FAILURE;
    } else {
      io.stderr("config unavailable\n");
      return EXIT_FAILURE;
    }
  }

  if (command === "default") return defaultCommand(cfg, o, io);
  if (command === "alias") return aliasCommand(cfg, o, io);

  let resolved: ResolvedPaths;
  try {
    resolved = resolveJournalPaths(io.env, cfg, { root: o.root, defaultRoot: o.defaultRoot, index: o.index });
  } catch {
    io.stderr("cannot resolve the journal root or index path (no HOME)\n");
    return EXIT_FAILURE;
  }
  const { rootPath, indexPath, rootSource } = resolved;

  switch (command) {
    case "capture":
      return captureCommand(cfg, rootPath, indexPath, io);
    case "status":
      return statusCommand(rootPath, indexPath, rootSource, sourcePathFor(rootSource, io, o), o.json, io);
    case "catalog": {
      emitJson(io, { pairs: catalog(rootPath, indexPath, cfg.capture) });
      return EXIT_OK;
    }
    case "sync":
      return syncCommand(rootPath, indexPath, o, io);
    case "reseal":
      return resealCommand(rootPath, indexPath, o, io);
    case "search":
      return searchCommand(cfg, rootPath, indexPath, o, io);
    case "get":
      return getCommand(rootPath, indexPath, o, io);
    default:
      io.stderr(USAGE);
      return EXIT_MALFORMED;
  }
}

function aliasCommand(cfg: Config, o: Opts, io: CliIo): number {
  const pos = o.positionals;
  if (pos.length === 0) {
    io.stderr("alias needs a subcommand: list | add <term> <canonical...> | remove <term> [canonical] | candidates\n");
    return EXIT_MALFORMED;
  }
  const sub = pos[0];
  let thesaurus: string;
  try {
    thesaurus = thesaurusPath(io.env, cfg.thesaurusPath);
  } catch {
    io.stderr("cannot resolve the thesaurus path (no HOME)\n");
    return EXIT_FAILURE;
  }

  switch (sub) {
    case "list": {
      const m = loadAliasMapFile(thesaurus);
      if (o.json) {
        emitJson(io, {
          path: thesaurus,
          alias_digest: m.digestHex,
          merged_keys: m.mergedKeys,
          entries: m.entries.map((e) => ({ key: e.key, values: e.values })),
        });
        return EXIT_OK;
      }
      let text = `${m.entries.length} alias(es) in ${thesaurus}\n`;
      for (const entry of m.entries) {
        text += `  ${entry.key} ->` + entry.values.map((v) => ` ${v}`).join("") + "\n";
      }
      text += "edit freely in any text editor; changes apply on the next search\n";
      io.stdout(text);
      return EXIT_OK;
    }

    case "add": {
      if (pos.length < 3) {
        io.stderr("alias add <term> <canonical...>\n");
        return EXIT_MALFORMED;
      }
      try {
        addAlias(thesaurus, pos[1], pos.slice(2));
      } catch (err) {
        if (err instanceof AliasError && err.code === "invalid_term") {
          io.stderr("term must be a searchable word: longer than 2 letters, [a-z0-9_], not a stop word\n");
          return EXIT_MALFORMED;
        }
        if (err instanceof AliasError && err.code === "invalid_value") {
          io.stderr("canonical values must be 2..128 chars of [A-Za-z0-9._:+/@-]\n");
          return EXIT_MALFORMED;
        }
        if (err instanceof AliasError && err.code === "malformed") {
          io.stderr("thesaurus file is not a JSON object; fix it by hand first\n");
          return EXIT_FAILURE;
        }
        io.stderr("cannot write thesaurus file\n");
        return EXIT_FAILURE;
      }
      io.stdout(`added: ${pos[1]} -> ${pos.slice(2).join(" ")} (${thesaurus})\n`);
      return EXIT_OK;
    }

    case "remove": {
      if (pos.length < 2 || pos.length > 3) {
        io.stderr("alias remove <term> [canonical]\n");
        return EXIT_MALFORMED;
      }
      let removed: string;
      try {
        removed = removeAlias(thesaurus, pos[1], pos.length === 3 ? pos[2] : undefined);
      } catch (err) {
        if (err instanceof AliasError && err.code === "not_found") {
          io.stderr("no such alias entry\n");
          return EXIT_FAILURE;
        }
        if (err instanceof AliasError && err.code === "malformed") {
          io.stderr("thesaurus file is not a JSON object; fix it by hand first\n");
          return EXIT_FAILURE;
        }
        io.stderr("cannot write thesaurus file\n");
        return EXIT_FAILURE;
      }
      io.stdout(`removed ${removed}: ${pos[1]}\n`);
      return EXIT_OK;
    }

    case "candidates": {
      let logPath: string;
      try {
        logPath = missLogPath(io.env);
      } catch {
        io.stderr("cannot resolve the miss-log path (no HOME)\n");
        return EXIT_FAILURE;
      }
      let data: string;
      try {
        const raw = fs.readFileSync(logPath);
        if (raw.byteLength > 16 * 1024 * 1024) throw new Error("over budget");
        data = raw.toString("utf8");
      } catch {
        io.stdout(`no candidates yet (${logPath}); enable with "miss_log": true in config.json\n`);
        return EXIT_OK;
      }
      const agg = aggregateMisses(data);
      if (o.json) {
        emitJson(io, {
          path: logPath,
          candidates: agg.map((c) => ({ query: c.query, count: c.count, terms: c.terms })),
        });
        return EXIT_OK;
      }
      const limit = o.limit ?? 20;
      const shown = Math.min(limit, agg.length);
      const plural = agg.length === 1 ? "y" : "ies";
      let text = `${agg.length} distinct weak quer${plural}, most frequent first:\n`;
      for (const cand of agg.slice(0, shown)) {
        text += `  [${cand.count}x] ${cand.query}\n        terms:` + cand.terms.map((t) => ` ${t}`).join("") + "\n";
      }
      text += "for each: if the journal really covers it, promote with\n  autojournal alias add <casual term> <canonical term>\n";
      io.stdout(text);
      return EXIT_OK;
    }
  }
  io.stderr("unknown alias subcommand; use list | add | remove | candidates\n");
  return EXIT_MALFORMED;
}

function parseLanes(text: string): Lane[] | null {
  const lanes: Lane[] = [];
  for (const tag of text.split(",")) {
    const trimmed = tag.trim();
    if (trimmed === "") continue;
    if (
      trimmed !== "conversation" &&
      trimmed !== "delegated_work" &&
      trimmed !== "evaluation" &&
      trimmed !== "imported_legacy"
    ) {
      return null;
    }
    if (lanes.includes(trimmed)) continue;
    if (lanes.length >= 4) return null;
    lanes.push(trimmed);
  }
  return lanes.length === 0 ? null : lanes;
}

function parseLineSpan(text: string): { start: number; end: number } | null {
  const parse = (s: string): number | null => (/^[0-9]{1,9}$/.test(s) ? Number(s) : null);
  const dash = text.indexOf("-");
  if (dash >= 0) {
    const start = parse(text.slice(0, dash));
    const end = parse(text.slice(dash + 1));
    if (start === null || end === null || start === 0 || end < start) return null;
    return { start, end };
  }
  const line = parse(text);
  if (line === null || line === 0) return null;
  return { start: line, end: line };
}

function openForRecall(rootPath: string, indexPath: string, io: CliIo): { root: JournalRoot; snapshot: Snapshot | null } | null {
  let root: JournalRoot;
  try {
    root = openExistingRoot(rootPath);
  } catch {
    io.stderr("journal root missing or unreadable\n");
    return null;
  }
  const opened = openSnapshot(indexPath, rootDigestHex(rootPath));
  if (opened.kind === "foreign") {
    io.stderr("index at this path belongs to a different journal root; run sync to rebuild it\n");
    return null;
  }
  return { root, snapshot: opened.kind === "ok" ? opened.snapshot : null };
}

function outcomeExit(outcome: string): number {
  switch (outcome) {
    case "match":
    case "no_match":
      return EXIT_OK;
    case "malformed":
      return EXIT_MALFORMED;
    case "conflict":
      return EXIT_CONFLICT;
    default:
      return EXIT_FAILURE;
  }
}

function searchCommand(cfg: Config, rootPath: string, indexPath: string, o: Opts, io: CliIo): number {
  if (o.positionals.length === 0) {
    io.stderr("search needs query words: autojournal search <query...>\n");
    return EXIT_MALFORMED;
  }
  const query = o.positionals.join(" ");
  if (Buffer.byteLength(query, "utf8") > MAX_QUERY_BYTES) {
    io.stderr("query exceeds max_query_bytes\n");
    return EXIT_MALFORMED;
  }
  // World fallback mirrors capture: an unconfigured install searches the
  // world capture publishes into.
  let world = cfg.capture.world;
  if (cfg.defaultWorld !== "") world = cfg.defaultWorld;
  if (o.world !== undefined) world = o.world;

  let lanes: Lane[] = [...DEFAULT_LANES];
  if (o.lanes !== undefined) {
    const parsed = parseLanes(o.lanes);
    if (parsed === null) {
      io.stderr("--lanes takes a comma list of: conversation, delegated_work, evaluation, imported_legacy\n");
      return EXIT_MALFORMED;
    }
    lanes = parsed;
  }
  let creditMode: CreditMode = "word_start";
  if (o.creditMode !== undefined) {
    if (o.creditMode !== "substring" && o.creditMode !== "word_start" && o.creditMode !== "whole_word") {
      io.stderr("--credit-mode takes: substring, word_start, whole_word\n");
      return EXIT_MALFORMED;
    }
    creditMode = o.creditMode;
  }
  const recall = openForRecall(rootPath, indexPath, io);
  if (recall === null) return EXIT_FAILURE;
  const aliasMap = loadAliasMapFile(thesaurusPath(io.env, cfg.thesaurusPath));

  // An explicit --limit 0 resolves to the default page size while the
  // config's max_results: 0 stays malformed: a persisted config stating a
  // meaningless page size is an error worth surfacing, and a one-off flag
  // resolves to what the user meant.
  const chosen = o.limit ?? cfg.maxResults;
  const out = search(recall.root, recall.snapshot, aliasMap, {
    query,
    world,
    scope: o.scope,
    lanes,
    creditMode,
    limit: Math.min(chosen, MAX_RESULTS_LIMIT),
    cursor: o.cursor,
    nowMs: io.nowMs(),
    knobs: {
      contextWindow: cfg.contextWindow,
      recencyBoost: cfg.recencyBoost,
      minScore: cfg.minScore,
      confidenceFloor: cfg.confidenceFloor,
    },
  });
  logSearchMiss(io.env, cfg, query, io.nowMs(), out);
  if (o.json) renderSearchJson(world, query, out, io);
  else renderSearchText(query, out, io);
  return outcomeExit(out.outcome);
}

function renderSearchJson(world: string, query: string, out: SearchOutput, io: CliIo): void {
  emitJson(io, {
    outcome: out.outcome,
    query,
    query_terms: out.queryTerms,
    alias_terms: out.aliasTerms,
    folded_terms: out.foldedTerms,
    results: out.hits.map((hit) => ({
      episode_id: hit.episodeId,
      revision: hit.revision,
      path: hit.path,
      world,
      scope: hit.scope,
      lane: hit.lane,
      capture_policy: hit.capturePolicy,
      event_time: isoFromMs(hit.eventTimeMs),
      line: hit.line,
      snippet_start: hit.snippetStart,
      snippet_end: hit.snippetEnd,
      snippet: hit.snippet,
      matched_terms: hit.matchedTerms,
      score: hit.score,
      confidence: hit.confidence,
    })),
    total: out.total,
    cursor: out.nextCursor === "" ? null : out.nextCursor,
    identities: {
      scorer: SCORER_VERSION,
      tokenizer: TOKENIZER_VERSION,
      confidence_policy: CONFIDENCE_POLICY_VERSION,
      alias_digest: out.aliasDigest,
      index_schema: SNAPSHOT_FORMAT_VERSION,
    },
    index: {
      freshness: out.freshness,
      indexed: out.indexed,
      source: out.source,
      edited_excluded: out.editedExcluded,
    },
    detail: out.detail === "" ? null : out.detail,
  });
}

function matchLine(hit: Hit): string {
  if (hit.snippet === "") return "(source changed since indexing)";
  let lineNo = hit.snippetStart;
  for (const line of hit.snippet.split("\n")) {
    if (lineNo === hit.line) return line;
    lineNo++;
  }
  return hit.snippet;
}

function renderSearchText(query: string, out: SearchOutput, io: CliIo): void {
  if (out.outcome === "no_match") {
    let text = `no match for "${query}" (index ${out.freshness}, ${out.indexed} indexed)\n`;
    if (out.editedExcluded > 0) {
      text += `note: ${out.editedExcluded} candidate(s) excluded as edited since indexing; run sync\n`;
    }
    io.stdout(text);
    return;
  }
  if (out.outcome !== "match") {
    let text = `search failed: ${out.outcome}`;
    if (out.detail !== "") text += ` (${out.detail})`;
    io.stdout(text + "\n");
    return;
  }
  let text = `${out.hits.length} of ${out.total} result(s) for "${query}" — index ${out.freshness}\n`;
  if (out.aliasTerms.length > 0) text += "aliases applied: " + out.aliasTerms.join(" ") + "\n";
  out.hits.forEach((hit, i) => {
    text += `${String(i + 1).padStart(2)}. [${hit.score.toFixed(2)} ${hit.confidence}] ${hit.path}:${hit.line} (${isoFromMs(hit.eventTimeMs).slice(0, 10)})\n`;
    text += `    ${matchLine(hit)}\n`;
    text += `    id ${hit.episodeId} rev ${hit.revision}\n`;
  });
  if (out.nextCursor !== "") text += `more: add --cursor ${out.nextCursor}\n`;
  if (out.detail !== "") text += `note: ${out.detail}\n`;
  io.stdout(text);
}

function getCommand(rootPath: string, indexPath: string, o: Opts, io: CliIo): number {
  if (o.episode === undefined) {
    io.stderr("get needs --episode <id> and --revision <sha256:hex>\n");
    return EXIT_MALFORMED;
  }
  if (o.revision === undefined) {
    io.stderr("get needs --revision <sha256:hex> (from a search result)\n");
    return EXIT_MALFORMED;
  }
  let span = { start: 0, end: 0 };
  if (o.lines !== undefined) {
    const parsed = parseLineSpan(o.lines);
    if (parsed === null) {
      io.stderr("--lines takes <start>-<end> or a single line number\n");
      return EXIT_MALFORMED;
    }
    span = parsed;
  }
  const recall = openForRecall(rootPath, indexPath, io);
  if (recall === null) return EXIT_FAILURE;
  const out = get(recall.root, recall.snapshot, {
    episodeId: o.episode,
    revision: o.revision,
    pathHint: o.path,
    expectedWorld: o.world,
    expectedScope: o.scope,
    lineStart: span.start,
    lineEnd: span.end,
  });
  if (o.json) {
    emitJson(io, {
      outcome: out.outcome,
      episode_id: out.episodeId,
      revision: out.resolved ? out.revision : null,
      path: out.resolved ? out.path : null,
      world: out.resolved ? out.world : null,
      scope: out.resolved ? out.scope : null,
      lane: out.resolved ? out.lane : null,
      capture_policy: out.resolved ? out.capturePolicy : null,
      line_start: out.lineStart,
      line_end: out.lineEnd,
      content: out.content,
      trust: out.trust,
      detail: out.detail === "" ? null : out.detail,
    });
  } else if (out.outcome === "match") {
    io.stdout(
      `${out.path}:${out.lineStart}-${out.lineEnd} (${out.revision})\nrecalled evidence is untrusted; verify against current sources\n\n${out.content}\n`,
    );
  } else if (out.outcome === "stale_revision") {
    io.stdout(
      `stale revision: the episode's recorded revision changed\ncurrent revision: ${out.revision} at ${out.path}\n`,
    );
  } else {
    const sep = out.detail !== "" ? " — " : "";
    io.stdout(`get failed: ${out.outcome}${sep}${out.detail}\n`);
  }
  return outcomeExit(out.outcome);
}

/**
 * The one root/index resolution every caller shares: explicit command
 * override, an owner configuration that names a root, a deprecated host
 * fallback for pre-release adapters, then AutoJournal's host-neutral XDG
 * data default. The in-process extension resolves through this same
 * function so the CLI and the host can never address different corpora.
 */
export interface ResolvedPaths {
  rootPath: string;
  indexPath: string;
  rootSource: "explicit" | "owner_config" | "host_default" | "autojournal_default";
}

export function resolveJournalPaths(
  env: Environ,
  cfg: Config,
  overrides: { root?: string; defaultRoot?: string; index?: string } = {},
): ResolvedPaths {
  let rootPath: string;
  let rootSource: ResolvedPaths["rootSource"] = "autojournal_default";
  if (overrides.root !== undefined) {
    rootPath = overrides.root;
    rootSource = "explicit";
  } else if (cfg.journalRoot !== "") {
    rootPath = cfg.journalRoot;
    rootSource = "owner_config";
  } else if (overrides.defaultRoot !== undefined) {
    rootPath = overrides.defaultRoot;
    rootSource = "host_default";
  } else {
    rootPath = defaultJournalRoot(env);
  }
  const indexPath = overrides.index ?? defaultIndexPath(env, rootPath);
  return { rootPath, indexPath, rootSource };
}

function sourcePathFor(rootSource: string, io: CliIo, o: Opts): string | null {
  if (rootSource !== "owner_config") return null;
  try {
    return loadConfig(io.env, o.config ?? "").sourcePath;
  } catch {
    return null;
  }
}

function statusCommand(
  rootPath: string,
  indexPath: string,
  rootSource: string,
  rootSourcePath: string | null,
  asJson: boolean,
  io: CliIo,
): number {
  const report = statusOf(rootPath, indexPath);
  if (asJson) {
    emitJson(io, {
      journal_root: rootPath,
      root_source: rootSource,
      root_source_path: rootSourcePath,
      root_ok: report.rootOk,
      episodes: report.episodes,
      index: {
        freshness: report.freshness,
        indexed: report.indexed,
        truncated: report.truncated,
        path: indexPath,
      },
    });
  } else if (!report.rootOk) {
    io.stdout(`journal_root: ${rootPath} (missing)\nepisodes: 0\nindex: not_built\n`);
  } else {
    io.stdout(
      `journal_root: ${rootPath} (ok)\nepisodes: ${report.episodes}\nindex: ${report.freshness} (${report.indexed} indexed, ${report.truncated} truncated, ${indexPath})\n`,
    );
  }
  if (!report.rootOk || report.freshness === "stale" || report.freshness === "unavailable") return EXIT_FAILURE;
  return EXIT_OK;
}

function syncFailure(err: unknown, io: CliIo): number {
  if (err instanceof SyncError) {
    switch (err.code) {
      case "shared_directory":
        io.stderr(SHARED_DIR_MESSAGE + "\n");
        return EXIT_FAILURE;
      case "root_missing":
        io.stderr("journal root missing; nothing to sync\n");
        return EXIT_FAILURE;
      case "sync_failed":
        io.stderr("sync failed; the previous snapshot stands\n");
        return EXIT_FAILURE;
    }
  }
  io.stderr("cannot open index snapshot\n");
  return EXIT_FAILURE;
}

function syncCommand(rootPath: string, indexPath: string, o: Opts, io: CliIo): number {
  try {
    const report = sync(rootPath, indexPath);
    if (o.json) {
      emitJson(io, {
        indexed: report.indexed,
        unchanged: report.unchanged,
        removed: report.removed,
        skipped_malformed: report.skippedMalformed,
        duplicate_ids: report.duplicateIds,
        digest_mismatch: report.digestMismatch,
        unreadable: report.unreadable,
        truncated: report.truncated,
      });
    } else {
      io.stdout(
        `indexed: ${report.indexed}\nunchanged: ${report.unchanged}\nremoved: ${report.removed}\n` +
          `skipped_malformed: ${report.skippedMalformed}\nduplicate_ids: ${report.duplicateIds}\n` +
          `digest_mismatch: ${report.digestMismatch}\nunreadable: ${report.unreadable}\ntruncated: ${report.truncated}\n`,
      );
    }
    return EXIT_OK;
  } catch (err) {
    return syncFailure(err, io);
  }
}

function resealCommand(rootPath: string, indexPath: string, o: Opts, io: CliIo): number {
  let report;
  try {
    report = reseal(rootPath, indexPath, o.preview);
  } catch (err) {
    return syncFailure(err, io);
  }
  // A write failure is exit 1, but only after the sweep finished and the
  // sync rebaselined what did reseal: the failure exit reports incomplete
  // work, never work undone.
  let exit = EXIT_OK;
  if (report.writeFailures > 0) {
    io.stderr(
      `${report.writeFailures} file(s) could not be rewritten; everything resealed was synced — fix permissions and rerun reseal\n`,
    );
    exit = EXIT_FAILURE;
  }
  if (o.json) {
    emitJson(io, {
      scanned: report.scanned,
      resealed: report.resealed,
      refused: report.refused,
      write_failures: report.writeFailures,
      paths: report.paths,
    });
  } else {
    io.stdout(
      `scanned: ${report.scanned}\nresealed: ${report.resealed}\nrefused: ${report.refused}\nwrite_failures: ${report.writeFailures}\n` +
        report.paths.map((p) => `  ${p}\n`).join(""),
    );
  }
  return exit;
}

interface CaptureReport {
  outcome: string;
  episode_id: string | null;
  payload_digest: string | null;
  path: string | null;
  index: string;
  detail: string | null;
}

function emitJson(io: CliIo, v: unknown): void {
  io.stdout(JSON.stringify(v) + "\n");
}

const SHARED_DIR_MESSAGE =
  "journal root sits under a shared (group- or world-writable) directory; chmod g-w,o-w the parent or configure journal_root to a private location";

function captureCommand(cfg: Config, rootPath: string, indexPath: string, io: CliIo): number {
  const reportMalformed = (detail: string): number => {
    emitJson(io, {
      outcome: "malformed",
      episode_id: null,
      payload_digest: null,
      path: null,
      index: "not_built",
      detail,
    } satisfies CaptureReport);
    return EXIT_MALFORMED;
  };

  const payloadBytes = io.stdin();
  if (payloadBytes.byteLength > MAX_PAYLOAD_BYTES + 1) {
    return reportMalformed("payload exceeds max_payload_bytes");
  }
  let raw: RawPayload;
  try {
    raw = parsePayload(payloadBytes);
  } catch (err) {
    return reportMalformed(err instanceof CaptureError ? err.code : "Malformed");
  }

  // The whole transaction — defaults fill, oversize policy, validation,
  // refusal ordering, publication — is the engine's. This command only
  // reads stdin, parses, and renders.
  const result = capture({
    rootPath,
    indexPath,
    raw,
    defaults: cfg.capture,
    captureTimeMs: io.nowMs(),
  });
  return renderCapture(result, io);
}

function renderCapture(res: CaptureResult, io: CliIo): number {
  let detail: string | null = res.detail === "" ? null : res.detail;
  if (res.sharedDirectory) detail = SHARED_DIR_MESSAGE;
  emitJson(io, {
    outcome: res.outcome,
    episode_id: res.episodeId === "" ? null : res.episodeId,
    payload_digest: res.digestHex === "" ? null : DIGEST_PREFIX + res.digestHex,
    path: res.relPath === "" ? null : res.relPath,
    index: res.indexState,
    detail,
  } satisfies CaptureReport);
  switch (res.outcome) {
    case "conflict":
      return EXIT_CONFLICT;
    case "malformed":
      return EXIT_MALFORMED;
    case "permission_denied":
    case "unavailable":
    case "internal_error":
      return EXIT_FAILURE;
    default:
      return EXIT_OK;
  }
}

// Shows or persists the owner's default world/scope. With neither --world
// nor --scope this prints the effective capture defaults; with either it
// rewrites the owner config atomically, so future sessions start there.
function defaultCommand(cfg: Config, o: Opts, io: CliIo): number {
  const world = o.world ?? cfg.capture.world;
  const scope = o.scope ?? cfg.capture.scope;
  if (o.world === undefined && o.scope === undefined) {
    if (o.json) emitJson(io, { world, scope });
    else io.stdout(`default world: ${world}\ndefault scope: ${scope}\n`);
    return EXIT_OK;
  }
  let written: string;
  try {
    written = saveCaptureDefaults(io.env, o.config ?? "", world, scope);
  } catch (err) {
    if (err instanceof ConfigError && err.code === "malformed") {
      io.stderr("cannot save defaults: invalid world/scope, or the existing config is malformed\n");
      return EXIT_MALFORMED;
    }
    if ((err instanceof ConfigError && err.code === "not_found") || err instanceof MissingHomeError) {
      io.stderr("cannot resolve a config path (no HOME)\n");
      return EXIT_FAILURE;
    }
    io.stderr("cannot write the owner config\n");
    return EXIT_FAILURE;
  }
  if (o.json) emitJson(io, { world, scope, config: written });
  else io.stdout(`default set: ${world} / ${scope}\nconfig: ${written}\n`);
  return EXIT_OK;
}

function readStdinSync(): Uint8Array {
  try {
    return fs.readFileSync(0);
  } catch {
    return new Uint8Array();
  }
}

if (process.argv[1] !== undefined && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const io: CliIo = {
    env: processEnviron,
    stdin: readStdinSync,
    stdout: (t) => process.stdout.write(t),
    stderr: (t) => process.stderr.write(t),
    nowMs: clockFromEnv(processEnviron),
  };
  process.exit(run(process.argv.slice(2), io));
}
