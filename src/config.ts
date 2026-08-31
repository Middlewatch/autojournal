// Owner configuration. AutoJournal owns its configuration; harness-side
// wiring passes no policy. Resolution order: explicit path,
// `$AUTOJOURNAL_CONFIG`, `$XDG_CONFIG_HOME/autojournal/config.json`,
// `$HOME/.config/autojournal/config.json`.
//
// The config file is a frozen on-disk contract. Two behaviors carry the
// weight of that freeze:
//
//   - parseConfig is a closed schema. Its acceptance is wider than plain
//     JSON typing: integer fields also accept strings and integral floats
//     ("5", 3.0, 3e0), and float fields accept strings ("1.5") under Go's
//     strconv.ParseFloat grammar (including hex floats and digit-separating
//     underscores). That width is frozen because an owner's existing file
//     must keep loading across upgrades — narrowing it is an Interface-tier
//     break. Unknown keys, duplicate keys, and wrong shapes are malformed.
//   - saveCaptureDefaults rewrites the file in place without disturbing
//     anything it was not asked to change: key order preserved, `world_root`
//     migrated to `journal_root`, numbers re-emitted in a stable
//     normalization (1.0 -> 1, 1e-10 -> 0.0000000001), and a minimal
//     escaping table (only control bytes, '"' and '\' escaped) so a
//     hand-maintained file stays human-readable. The bytes on disk are
//     themselves the contract. Golden proof:
//     testdata/golden/config-vectors.json.
//
// Non-finite float knobs ("inf", overflow to infinity) are rejected at
// load alongside the NaN guard: an infinite boost or floor cannot express
// a meaningful value.

import * as fs from "node:fs";
import * as path from "node:path";
import { validWorld, validScope } from "./contracts.ts";
import { type Environ, MissingHomeError } from "./paths.ts";
import {
  parseOrderedJson,
  objectGet,
  objectHas,
  objectSet,
  objectRemove,
  type JsonValue,
  type JsonEntry,
} from "./json.ts";

/** Bounds the owner config file. */
export const MAX_CONFIG_BYTES = 64 * 1024;

/** Completed-turn defaults. */
export interface CaptureDefaults {
  world: string;
  scope: string;
}

/**
 * The resolved owner configuration. Absent keys take the defaults from
 * defaultConfig(). missLogMaxBytes is a bigint because the accepted range
 * is the full uint64 and a number cannot carry it exactly through a
 * rewrite.
 */
export interface Config {
  /**
   * Absolute path to the owner-controlled Markdown journal. Empty when the
   * config names none: the host-neutral XDG data default applies.
   */
  journalRoot: string;
  /** World searched when the caller names none; recall-side convenience only. */
  defaultWorld: string;
  /** Absolute path to the owner-edited alias map. */
  thesaurusPath: string;
  /** Snippet context lines on each side of a matched line. */
  contextWindow: number;
  /** Default memory_search result page size. */
  maxResults: number;
  recencyBoost: number;
  /** Relevance floor; 0 disables it (legacy parity). */
  minScore: number;
  confidenceFloor: number;
  missLog: boolean;
  missLogMaxBytes: bigint;
  capture: CaptureDefaults;
}

export function defaultConfig(): Config {
  return {
    journalRoot: "",
    defaultWorld: "",
    thesaurusPath: "",
    contextWindow: 3,
    maxResults: 10,
    recencyBoost: 1.0,
    minScore: 0.0,
    confidenceFloor: 3.0,
    missLog: false,
    missLogMaxBytes: 1024n * 1024n,
    capture: { world: "main", scope: "default" },
  };
}

/**
 * Config load failures. malformed covers every schema violation; not_found
 * means no config could be resolved or the resolved file is absent;
 * unavailable is an I/O failure (including an over-budget file). The
 * distinction is the owner's remedy: a malformed config needs editing, an
 * oversized one needs a different file.
 */
export type ConfigErrorCode = "not_found" | "malformed" | "unavailable";

export class ConfigError extends Error {
  readonly code: ConfigErrorCode;
  constructor(code: ConfigErrorCode, detail?: string) {
    super(detail === undefined ? `config ${code}` : `config ${code}: ${detail}`);
    this.name = "ConfigError";
    this.code = code;
  }
}

const malformed = () => new ConfigError("malformed");

/**
 * Resolves the owner config path without reading it: explicit path,
 * `$AUTOJOURNAL_CONFIG`, XDG config dir, `$HOME/.config`. An empty
 * explicitPath means "not provided".
 */
export function resolveConfigPath(env: Environ, explicitPath: string): string {
  if (explicitPath !== "") return explicitPath;
  const override = env("AUTOJOURNAL_CONFIG");
  if (override !== undefined && override !== "") return override;
  // Empty or relative XDG values are invalid per the spec and ignored.
  const xdg = env("XDG_CONFIG_HOME");
  if (xdg !== undefined && xdg !== "" && path.isAbsolute(xdg)) {
    return xdg + "/autojournal/config.json";
  }
  const home = env("HOME");
  if (home === "") {
    // A set-but-empty HOME is a broken environment, not a missing config:
    // "" + "/.config/..." would resolve to a root-owned absolute path
    // nobody means.
    throw new MissingHomeError();
  }
  if (home !== undefined) return home + "/.config/autojournal/config.json";
  const profile = env("USERPROFILE");
  if (profile !== undefined && profile !== "") return profile + "/.config/autojournal/config.json";
  throw new ConfigError("not_found");
}

/** A parsed config plus the path it came from. */
export interface LoadedConfig {
  config: Config;
  sourcePath: string;
}

/** Resolves, reads, and parses the owner config. */
export function loadConfig(env: Environ, explicitPath: string): LoadedConfig {
  const sourcePath = resolveConfigPath(env, explicitPath);
  const data = readConfigFile(sourcePath);
  return { config: parseConfig(data), sourcePath };
}

function readConfigFile(configPath: string): Uint8Array {
  let data: Buffer;
  try {
    data = fs.readFileSync(configPath);
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === "ENOENT") throw new ConfigError("not_found");
    throw new ConfigError("unavailable", String(err));
  }
  if (data.byteLength > MAX_CONFIG_BYTES) {
    throw new ConfigError("unavailable", `config exceeds ${MAX_CONFIG_BYTES} bytes`);
  }
  return data;
}

/**
 * Validates config bytes against the closed schema and returns the
 * resolved Config. See the module comment for why the numeric coercions
 * are wider than JSON typing and why that width is frozen.
 */
export function parseConfig(data: Uint8Array | string): Config {
  const cfg = defaultConfig();
  let text: string;
  if (typeof data === "string") {
    text = data;
  } else {
    // The check has to reject invalid UTF-8 outright: letting U+FFFD
    // substitution through would persist the corruption on the next
    // rewrite, turning a transient problem into a permanent one.
    try {
      text = new TextDecoder("utf-8", { fatal: true }).decode(data);
    } catch {
      throw malformed();
    }
  }
  const root = parseOrderedJson(text);
  if (root === null || root.kind !== "object") throw malformed();
  const entries = root.entries;
  const KNOWN = new Set([
    "journal_root",
    "world_root",
    "default_world",
    "thesaurus_path",
    "context_window",
    "max_results",
    "recency_boost",
    "min_score",
    "confidence_floor",
    "miss_log",
    "miss_log_max_bytes",
    "capture",
  ]);
  for (const e of entries) {
    if (!KNOWN.has(e.key)) throw malformed();
  }

  const journalRoot = optStringField(entries, "journal_root");
  const worldRoot = optStringField(entries, "world_root");
  // Compatibility for pre-release owner configurations: world_root names
  // the journal root. Both set to different values is malformed.
  if (journalRoot !== null && worldRoot !== null && journalRoot !== worldRoot) throw malformed();
  const rootSet = journalRoot !== null || worldRoot !== null;
  if (journalRoot !== null) cfg.journalRoot = journalRoot;
  else if (worldRoot !== null) cfg.journalRoot = worldRoot;

  const defaultWorld = optStringField(entries, "default_world");
  if (defaultWorld !== null) cfg.defaultWorld = defaultWorld;
  const thesaurus = optStringField(entries, "thesaurus_path");
  if (thesaurus !== null) cfg.thesaurusPath = thesaurus;

  cfg.contextWindow = Number(optUintField(entries, "context_window", 32, BigInt(cfg.contextWindow)));
  cfg.maxResults = Number(optUintField(entries, "max_results", 32, BigInt(cfg.maxResults)));
  cfg.recencyBoost = optFloatField(entries, "recency_boost", cfg.recencyBoost);
  cfg.minScore = optFloatField(entries, "min_score", cfg.minScore);
  cfg.confidenceFloor = optFloatField(entries, "confidence_floor", cfg.confidenceFloor);
  cfg.missLog = optBoolField(entries, "miss_log", cfg.missLog);
  cfg.missLogMaxBytes = optUintField(entries, "miss_log_max_bytes", 64, cfg.missLogMaxBytes);
  const captureRaw = objectGet(entries, "capture");
  if (captureRaw !== undefined) cfg.capture = parseCapture(captureRaw);

  // The check order is fixed so a config with several problems always
  // reports the same first failure, which keeps the diagnosis
  // reproducible. Presence is tracked separately from the value: an
  // explicit empty string is present, and fails the absolute-path and
  // world-token checks.
  if (rootSet && !path.isAbsolute(cfg.journalRoot)) throw malformed();
  if (thesaurus !== null && !path.isAbsolute(cfg.thesaurusPath)) throw malformed();
  if (defaultWorld !== null && !validWorld(cfg.defaultWorld)) throw malformed();
  // Snippets stay bounded: 10 context lines each side already triples the
  // default and approaches the whole-snippet byte cap.
  if (cfg.contextWindow === 0 || cfg.contextWindow > 10) throw malformed();
  if (cfg.maxResults === 0) throw malformed();
  // The !(x >= 0) shape rejects NaN, which a plain x < 0 would let
  // through. Non-finite values are rejected alongside: an infinite boost
  // or floor cannot express a meaningful value, and would make every score
  // NaN.
  if (!(cfg.recencyBoost >= 0) || !(cfg.minScore >= 0) || !(cfg.confidenceFloor >= 0)) throw malformed();
  if (!Number.isFinite(cfg.recencyBoost) || !Number.isFinite(cfg.minScore) || !Number.isFinite(cfg.confidenceFloor)) {
    throw malformed();
  }
  if (!validWorld(cfg.capture.world)) throw malformed();
  if (!validScope(cfg.capture.scope)) throw malformed();
  return cfg;
}

// parseCapture parses the nested capture object: world/scope strings with
// defaults, closed key set, null and non-strings rejected.
function parseCapture(v: JsonValue): CaptureDefaults {
  const cap: CaptureDefaults = { world: "main", scope: "default" };
  if (v.kind !== "object") throw malformed();
  for (const e of v.entries) {
    if (e.key !== "world" && e.key !== "scope") throw malformed();
    if (e.value.kind !== "string") throw malformed();
    if (e.key === "world") cap.world = e.value.value;
    else cap.scope = e.value.value;
  }
  return cap;
}

// optStringField extracts an optional string field: absent or JSON null
// maps to null (the default applies); anything non-string is malformed.
function optStringField(entries: readonly JsonEntry[], key: string): string | null {
  const v = objectGet(entries, key);
  if (v === undefined || v.kind === "null") return null;
  if (v.kind !== "string") throw malformed();
  return v.value;
}

// optUintField extracts an unsigned integer field under the widened
// numeric acceptance the module comment describes: integer-shaped literals
// parse directly; strings and float-shaped literals are accepted when they
// are exactly integral and in range.
function optUintField(entries: readonly JsonEntry[], key: string, bitSize: 32 | 64, def: bigint): bigint {
  const v = objectGet(entries, key);
  if (v === undefined) return def;
  let lit: string;
  if (v.kind === "number") lit = v.literal;
  else if (v.kind === "string") lit = v.value;
  else throw malformed();
  const n = coerceConfigUint(lit, bitSize);
  if (n === null) throw malformed();
  return n;
}

// optFloatField extracts a float field: number or string literals, Go's
// strconv grammar. Overflow to ±Inf is tolerated here (range error)
// because parseConfig's finiteness guards reject non-finite knob values
// after extraction — the rejection lives with the other value checks so a
// config with several problems reports its first failure in field order.
function optFloatField(entries: readonly JsonEntry[], key: string, def: number): number {
  const v = objectGet(entries, key);
  if (v === undefined) return def;
  let lit: string;
  if (v.kind === "number") lit = v.literal;
  else if (v.kind === "string") lit = v.value;
  else throw malformed();
  const f = goParseFloat(lit);
  if (f === null) throw malformed();
  return f.value;
}

// optBoolField extracts a bool field: JSON true/false only.
function optBoolField(entries: readonly JsonEntry[], key: string, def: boolean): boolean {
  const v = objectGet(entries, key);
  if (v === undefined) return def;
  if (v.kind !== "bool") throw malformed();
  return v.value;
}

// isIntegerShaped reports whether a JSON number literal has no fraction or
// exponent and is not "-0". Integer-shaped literals take the strict
// parsing path; everything else goes through the float coercion below.
function isIntegerShaped(lit: string): boolean {
  return lit !== "-0" && !/[.eE]/.test(lit);
}

// coerceConfigUint resolves a literal to an unsigned value: integer-shaped
// input goes through strict decimal parsing; anything else (strings,
// float-shaped literals) must be finite, integral, in range, and exactly
// the float64 value — the rewrite path re-emits float-shaped literals
// through the float (formatConfigNumber), so a literal accepted beyond
// what the float carries would drift on re-emission and publish a config
// this engine then refuses to load. Acceptance is bounded by emission.
// "inf" and "nan" are out of range for an unsigned target and rejected
// here — unlike the float path, which still accepts them as strings.
function coerceConfigUint(lit: string, bitSize: 32 | 64): bigint | null {
  if (isIntegerShaped(lit)) {
    // Go's ParseUint acceptance: bare decimal digits, leading zeros
    // tolerated, no sign or separators.
    if (!/^[0-9]+$/.test(lit)) return null;
    const n = BigInt(lit);
    return n < 1n << BigInt(bitSize) ? n : null;
  }
  const parsed = goParseFloat(lit);
  if (parsed === null) return null;
  const f = parsed.value;
  if (!Number.isFinite(f) || Number.isNaN(f) || f < 0 || f !== Math.trunc(f)) return null;
  // The literal must BE the float value, not merely round to it: an exact
  // comparison detects the drift a double-rounded acceptance would hide
  // until the rewrite.
  if (!literalExactlyEquals(lit, f)) return null;
  if (f >= 2 ** bitSize) return null;
  return BigInt(f);
}

// --- Go strconv.ParseFloat grammar -------------------------------------
//
// String-typed numeric config values are parsed with Go's acceptance, not
// JavaScript's: Number() would admit "0x10", "0b101", padded whitespace,
// and the empty string, and would miss hex floats ("0x1p-2") and Go's
// digit-separating underscores. The grammar here is the frozen contract.

interface GoFloat {
  value: number;
  /** True when the literal was syntactically valid but overflowed or underflowed. */
  rangeErr: boolean;
}

export function goParseFloat(lit: string): GoFloat | null {
  let s = lit;
  let sign = 1;
  if (s.startsWith("+") || s.startsWith("-")) {
    if (s[0] === "-") sign = -1;
    s = s.slice(1);
  }
  const lower = s.toLowerCase();
  if (lower === "inf" || lower === "infinity") return { value: sign * Infinity, rangeErr: false };
  if (lower === "nan") return { value: NaN, rangeErr: false };
  if (lower.startsWith("0x")) return goParseHexFloat(sign, s.slice(2));

  // Decimal: digits [. digits] [eE sign digits], underscores only between
  // digits, at least one mantissa digit.
  if (!/^(?:[0-9][0-9_]*)?(?:\.(?:[0-9][0-9_]*)?)?(?:[eE][+-]?[0-9][0-9_]*)?$/.test(s)) return null;
  if (!/[0-9]/.test(s.split(/[eE]/)[0])) return null; // mantissa needs a digit
  if (!underscoresBetweenDigits(s, /[0-9]/)) return null;
  const cleaned = s.replace(/_/g, "");
  const value = sign * Number(cleaned);
  if (value === Infinity || value === -Infinity) return { value, rangeErr: true };
  const underflowed = value === 0 && /[1-9]/.test(cleaned.split(/[eE]/)[0]);
  return { value, rangeErr: underflowed };
}

function underscoresBetweenDigits(s: string, digit: RegExp): boolean {
  for (let i = 0; i < s.length; i++) {
    if (s[i] === "_") {
      if (i === 0 || i === s.length - 1) return false;
      if (!digit.test(s[i - 1]) || !digit.test(s[i + 1])) return false;
    }
  }
  return true;
}

function goParseHexFloat(sign: number, s: string): GoFloat | null {
  const m = /^([0-9a-fA-F_]*)(?:\.([0-9a-fA-F_]*))?[pP]([+-]?[0-9][0-9_]*)$/.exec(s);
  if (m === null) return null;
  const intPart = (m[1] ?? "").replace(/_/g, "");
  const fracPart = (m[2] ?? "").replace(/_/g, "");
  if (intPart.length === 0 && fracPart.length === 0) return null;
  if (!underscoresBetweenDigits(s.replace(/[pP][+-]?/, "p"), /[0-9a-fA-F]/)) return null;
  const mantissa = BigInt("0x" + (intPart + fracPart || "0"));
  const exp = Number(m[3].replace(/_/g, "")) - 4 * fracPart.length;
  if (!Number.isFinite(exp)) return null;
  const value = sign * ldexpBig(mantissa, exp);
  if (value === Infinity || value === -Infinity) return { value, rangeErr: true };
  return { value, rangeErr: value === 0 && mantissa !== 0n };
}

// ldexpBig computes mantissa * 2^exp in float64, scaling in bounded chunks
// so intermediate powers stay finite.
function ldexpBig(mantissa: bigint, exp: number): number {
  let x = Number(mantissa); // correctly rounded to float64
  while (exp > 500) {
    x *= 2 ** 500;
    exp -= 500;
    if (!Number.isFinite(x)) return x;
  }
  while (exp < -500) {
    x *= 2 ** -500;
    exp += 500;
    if (x === 0) return x;
  }
  return x * 2 ** exp;
}

// literalExactlyEquals reports whether the numeric literal denotes exactly
// the (integral, non-negative) float f, compared as exact rationals via
// BigInt. lit has already passed goParseFloat, so its shape is trusted.
function literalExactlyEquals(lit: string, f: number): boolean {
  const fInt = BigInt(f); // f is integral and in uint64 range at this call site
  let s = lit.replace(/_/g, "");
  if (s.startsWith("+")) s = s.slice(1);
  if (s.startsWith("-")) return f === 0 && exactDecimalIsZero(s.slice(1));
  if (s.toLowerCase().startsWith("0x")) {
    const m = /^0[xX]([0-9a-fA-F]*)(?:\.([0-9a-fA-F]*))?[pP]([+-]?[0-9]+)$/.exec(s);
    if (m === null) return false;
    const mantissa = BigInt("0x" + ((m[1] ?? "") + (m[2] ?? "") || "0"));
    const exp = Number(m[3]) - 4 * (m[2] ?? "").length;
    return scaledEquals(mantissa, exp, 2n, fInt);
  }
  const m = /^([0-9]*)(?:\.([0-9]*))?(?:[eE]([+-]?[0-9]+))?$/.exec(s);
  if (m === null) return false;
  const digits = (m[1] ?? "") + (m[2] ?? "");
  const exp = (m[3] === undefined ? 0 : Number(m[3])) - (m[2] ?? "").length;
  return scaledEquals(BigInt(digits === "" ? "0" : digits), exp, 10n, fInt);
}

function exactDecimalIsZero(s: string): boolean {
  return !/[1-9]/.test(s.split(/[eE]/)[0]);
}

// scaledEquals reports mantissa × base^exp === target exactly.
function scaledEquals(mantissa: bigint, exp: number, base: bigint, target: bigint): boolean {
  if (exp >= 0) {
    if (exp > 10000) return false; // cannot equal a uint64-range target
    return mantissa * base ** BigInt(exp) === target;
  }
  if (exp < -10000) return mantissa === 0n && target === 0n;
  const scale = base ** BigInt(-exp);
  if (mantissa % scale !== 0n) return false;
  return mantissa / scale === target;
}

// --- Rewrite ------------------------------------------------------------

/**
 * Persists new capture defaults into the owner config with an atomic
 * rewrite that preserves every other key the owner wrote (the pre-release
 * `world_root` key is migrated to `journal_root` on the way). A missing
 * config file is created holding only the capture defaults. A file that
 * fails closed-schema validation is left untouched. Returns the path
 * written.
 *
 * The output bytes are a frozen contract: key order, number normalization,
 * escaping, and indentation are all preserved so that setting one default
 * produces a one-line diff in a file the owner hand-maintains. Pinned by
 * testdata/golden/config-vectors.json.
 */
export function saveCaptureDefaults(env: Environ, explicitPath: string, world: string, scope: string): string {
  if (!validWorld(world) || !validScope(scope)) throw malformed();
  const configPath = resolveConfigPath(env, explicitPath);

  let data: Uint8Array;
  try {
    data = readConfigFile(configPath);
  } catch (err) {
    if (!(err instanceof ConfigError) || err.code !== "not_found") throw err;
    // A missing file is created holding only the capture defaults.
    data = new TextEncoder().encode("{}");
  }
  let text: string;
  try {
    text = new TextDecoder("utf-8", { fatal: true }).decode(data);
  } catch {
    throw malformed();
  }
  const root = parseOrderedJson(text);
  if (root === null || root.kind !== "object") throw malformed();
  const entries = root.entries;

  // Migrate world_root: the value moves to a journal_root key appended at
  // the end (unless one is already present, even as null), and world_root
  // is removed from its old position.
  const legacy = objectGet(entries, "world_root");
  if (legacy !== undefined) {
    if (!objectHas(entries, "journal_root")) objectSet(entries, "journal_root", legacy);
    objectRemove(entries, "world_root");
  }
  let previousWorld = "main";
  const cap = objectGet(entries, "capture");
  if (cap !== undefined && cap.kind === "object") {
    const w = objectGet(cap.entries, "world");
    if (w !== undefined && w.kind === "string") previousWorld = w.value;
  }
  objectSet(entries, "capture", {
    kind: "object",
    entries: [
      { key: "world", value: { kind: "string", value: world } },
      { key: "scope", value: { kind: "string", value: scope } },
    ],
  });
  // `default_world` is the recall-side override; it follows only an actual
  // world change, so a scope-only update touches nothing else.
  if (objectHas(entries, "default_world") && world !== previousWorld) {
    objectSet(entries, "default_world", { kind: "string", value: world });
  }

  const out = writeCanonicalJson(root, 0);
  // Never publish a config this engine would refuse to load.
  parseConfig(out);
  writeAtomicJsonFile(configPath, out);
  return configPath;
}

/**
 * Writes a canonical JSON text plus a trailing newline via a sibling temp
 * file and rename, creating the parent directory if needed. Shared by the
 * config rewrite and the thesaurus curation path, which both promise the
 * owner an atomically replaced, hand-editable file.
 */
export function writeAtomicJsonFile(configPath: string, text: string): void {
  const dir = path.dirname(configPath);
  const tmpPath = path.join(dir, "." + path.basename(configPath) + ".tmp");
  try {
    fs.mkdirSync(dir, { recursive: true, mode: 0o755 });
    const fd = fs.openSync(tmpPath, "w", 0o600);
    try {
      fs.writeFileSync(fd, text + "\n");
      fs.fsyncSync(fd);
    } finally {
      fs.closeSync(fd);
    }
    fs.renameSync(tmpPath, configPath);
  } catch (err) {
    try {
      fs.rmSync(tmpPath, { force: true });
    } catch {
      // The temp file may never have been created.
    }
    throw new ConfigError("unavailable", String(err));
  }
}

/**
 * Serializes an ordered JSON value in the canonical two-space format: two
 * spaces per level, `"key": value`, empty containers as `{}`/`[]`.
 */
export function writeCanonicalJson(v: JsonValue, indent: number): string {
  switch (v.kind) {
    case "null":
      return "null";
    case "bool":
      return v.value ? "true" : "false";
    case "string":
      return writeCanonicalJsonString(v.value);
    case "number":
      return formatConfigNumber(v.literal);
    case "object": {
      if (v.entries.length === 0) return "{}";
      let out = "{";
      v.entries.forEach((e, i) => {
        if (i > 0) out += ",";
        out += lineIndent(indent + 1) + writeCanonicalJsonString(e.key) + ": " + writeCanonicalJson(e.value, indent + 1);
      });
      return out + lineIndent(indent) + "}";
    }
    case "array": {
      if (v.items.length === 0) return "[]";
      let out = "[";
      v.items.forEach((item, i) => {
        if (i > 0) out += ",";
        out += lineIndent(indent + 1) + writeCanonicalJson(item, indent + 1);
      });
      return out + lineIndent(indent) + "]";
    }
  }
}

function lineIndent(indent: number): string {
  return "\n" + "  ".repeat(indent);
}

// writeCanonicalJsonString applies a minimal escaping table: only control
// characters below 0x20, '"', and '\' are escaped (\b \f \n \r \t short
// forms, \u00xx lowercase otherwise); everything else — non-ASCII, DEL,
// and the HTML-significant characters JSON.stringify variants escape —
// passes through raw, so a hand-edited file stays legible after a rewrite
// instead of turning its non-ASCII content into escape sequences.
function writeCanonicalJsonString(s: string): string {
  let out = '"';
  for (const ch of s) {
    switch (ch) {
      case "\\":
        out += "\\\\";
        break;
      case '"':
        out += '\\"';
        break;
      case "\b":
        out += "\\b";
        break;
      case "\f":
        out += "\\f";
        break;
      case "\n":
        out += "\\n";
        break;
      case "\r":
        out += "\\r";
        break;
      case "\t":
        out += "\\t";
        break;
      default: {
        const code = ch.charCodeAt(0);
        if (code < 0x20) out += "\\u" + code.toString(16).padStart(4, "0");
        else out += ch;
      }
    }
  }
  return out + '"';
}

// formatConfigNumber re-emits a JSON number literal in the one canonical
// form the file uses, so an owner's numbers survive a rewrite unchanged:
// integer-shaped literals print verbatim (fitting or not, they are already
// canonical), overflow-to-infinity floats print verbatim, and everything
// else is parsed to a float and printed in full decimal notation —
// shortest round-trip digits placed positionally, never scientific (1e-10
// becomes 0.0000000001, 1e300 becomes 1 followed by 300 zeros).
export function formatConfigNumber(lit: string): string {
  if (isIntegerShaped(lit)) return lit;
  const f = Number(lit); // lit follows the JSON number grammar
  if (!Number.isFinite(f)) return lit; // overflow: kept verbatim
  return formatPositional(f);
}

// formatPositional renders a finite float with its shortest round-trip
// digits in positional notation, matching Go's FormatFloat(f, 'f', -1, 64).
export function formatPositional(f: number): string {
  if (Object.is(f, -0)) return "-0";
  const s = String(f); // shortest round-trip digits
  const m = /^(-?)(\d+)(?:\.(\d+))?e([+-]\d+)$/.exec(s);
  if (m === null) return s;
  const sign = m[1];
  const digits = m[2] + (m[3] ?? "");
  const pointPos = m[2].length + Number(m[4]);
  if (pointPos <= 0) {
    return sign + "0." + "0".repeat(-pointPos) + digits;
  }
  if (pointPos >= digits.length) {
    return sign + digits + "0".repeat(pointPos - digits.length);
  }
  return sign + digits.slice(0, pointPos) + "." + digits.slice(pointPos);
}
