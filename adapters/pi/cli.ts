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
import { defaultJournalRoot, MissingHomeError, processEnviron, type Environ } from "./engine/paths.ts";
import { capture, type CaptureResult } from "./engine/store.ts";

export const CLI_VERSION = "2.0.0";

const USAGE = `usage: autojournal <command> [options]

commands:
  capture   read one completed-turn JSON payload on stdin and publish it
  default   show or set the owner default world/scope (--world/--scope)
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

  const o: Opts = { json: false, positionals: [] };
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
    io.stdout(`autojournal ${CLI_VERSION} (payload schema v${PAYLOAD_SCHEMA_VERSION}, episode schema ${EPISODE_SCHEMA})\n`);
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
  if (o.root !== undefined) rootPath = o.root;
  else if (cfg.journalRoot !== "") rootPath = cfg.journalRoot;
  else if (o.defaultRoot !== undefined) rootPath = o.defaultRoot;
  else {
    try {
      rootPath = defaultJournalRoot(io.env);
    } catch {
      io.stderr("cannot resolve the default journal root (no HOME)\n");
      return EXIT_FAILURE;
    }
  }

  switch (command) {
    case "capture":
      return captureCommand(cfg, rootPath, io);
    default:
      io.stderr(USAGE);
      return EXIT_MALFORMED;
  }
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

function captureCommand(cfg: Config, rootPath: string, io: CliIo): number {
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
