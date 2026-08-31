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
} from "./engine/contracts.ts";
import { DIGEST_PREFIX } from "./engine/identity.ts";
import {
  defaultConfig,
  loadConfig,
  saveCaptureDefaults,
  ConfigError,
  type Config,
} from "./engine/config.ts";
import { defaultJournalRoot, defaultIndexPath, MissingHomeError, processEnviron, type Environ } from "./engine/paths.ts";
import { capture, type CaptureResult } from "./engine/store.ts";
import { statusOf, sync, reseal, catalog, SyncError } from "./engine/ops.ts";
import { SNAPSHOT_FORMAT_VERSION } from "./engine/index.ts";
import { TOKENIZER_VERSION } from "./engine/retrieval.ts";

export const CLI_VERSION = "2.0.0";

const USAGE = `usage: autojournal <command> [options]

commands:
  capture   read one completed-turn JSON payload on stdin and publish it
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
  --world <id>       world value for default
  --scope <token>    scope value for default
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
      `autojournal ${CLI_VERSION} (payload schema v${PAYLOAD_SCHEMA_VERSION}, episode schema ${EPISODE_SCHEMA}, snapshot format v${SNAPSHOT_FORMAT_VERSION}, ${TOKENIZER_VERSION})\n`,
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

  // Root resolution: explicit command override, an owner configuration
  // that names a root, a deprecated host fallback for pre-release
  // adapters, then AutoJournal's host-neutral XDG data default.
  let rootPath: string;
  let rootSource = "autojournal_default";
  if (o.root !== undefined) {
    rootPath = o.root;
    rootSource = "explicit";
  } else if (cfg.journalRoot !== "") {
    rootPath = cfg.journalRoot;
    rootSource = "owner_config";
  } else if (o.defaultRoot !== undefined) {
    rootPath = o.defaultRoot;
    rootSource = "host_default";
  } else {
    try {
      rootPath = defaultJournalRoot(io.env);
    } catch {
      io.stderr("cannot resolve the default journal root (no HOME)\n");
      return EXIT_FAILURE;
    }
  }
  let indexPath: string;
  if (o.index !== undefined) {
    indexPath = o.index;
  } else {
    try {
      indexPath = defaultIndexPath(io.env, rootPath);
    } catch {
      io.stderr("cannot resolve the default index path (no HOME)\n");
      return EXIT_FAILURE;
    }
  }

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
    default:
      io.stderr(USAGE);
      return EXIT_MALFORMED;
  }
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

// --- capture ---

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

// --- default ---

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

// --- entry point ---

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
