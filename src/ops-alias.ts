// Alias maintenance and the weak-query miss log: the thesaurus write path
// and its aggregation, exposed to the owner CLI as `alias add`, `alias
// remove`, and `alias candidates`, and to the Pi menu's search-quality
// section. aliases.ts owns the read path search uses; the operations here
// rewrite the same hand-editable file atomically and never run on a
// recall path.

import * as fs from "node:fs";
import * as path from "node:path";
import { validToken } from "./contracts.ts";
import { isStopWord, asciiLower } from "./retrieval.ts";
import { isoFromMs } from "./render.ts";
import { normalizeAliasKey, MAX_THESAURUS_BYTES } from "./aliases.ts";
import { missLogPath, type Environ } from "./paths.ts";
import { writeCanonicalJson, writeAtomicJsonFile, type Config } from "./config.ts";
import { parseOrderedJson, objectGet, objectSet, objectRemove, type JsonValue, type JsonEntry } from "./json.ts";
import type { SearchOutput } from "./search.ts";

/**
 * Edit failure vocabulary. invalid_term: the key would never fire — it
 * must survive query tokenization (length > 2, [a-z0-9_], not a stop
 * word). invalid_value: a value must be a searchable token or phrase —
 * 2..128 bytes from the identity-token charset. malformed: the file
 * exists but is not a JSON object; refusing to rewrite it protects a
 * hand-edit gone wrong from being clobbered. not_found: no such entry, or
 * no such value in the entry. unavailable: any I/O failure.
 */
export type AliasErrorCode = "invalid_term" | "invalid_value" | "malformed" | "not_found" | "unavailable";

export class AliasError extends Error {
  readonly code: AliasErrorCode;
  constructor(code: AliasErrorCode, detail?: string) {
    super(detail === undefined ? `alias ${code}` : `alias ${code}: ${detail}`);
    this.name = "AliasError";
    this.code = code;
  }
}

export function validAliasKey(key: string): boolean {
  if (key.length <= 2 || key.length > 128) return false;
  if (!/^[a-z0-9_]+$/.test(key)) return false;
  return !isStopWord(key);
}

export function validAliasValue(value: string): boolean {
  if (value.length < 2) return false;
  return validToken(value);
}

/**
 * Adds (or extends) one alias entry and atomically rewrites the file,
 * preserving every entry it does not touch. Values already present are
 * not duplicated.
 */
export function addAlias(thesaurusPath: string, term: string, canonicals: string[]): void {
  const key = normalizeAliasKey(term);
  if (!validAliasKey(key)) throw new AliasError("invalid_term");
  if (canonicals.length === 0) throw new AliasError("invalid_value");
  const entries = readEditableThesaurus(thesaurusPath);
  // An existing case-variant entry is extended rather than shadowed, and
  // the rewrite below persists the collapse.
  canonicalizeAliasKeys(entries);

  let values: JsonValue[] = [];
  const existing = objectGet(entries, key);
  if (existing !== undefined) {
    if (existing.kind !== "array") throw new AliasError("malformed");
    values = existing.items;
  }
  for (const raw of canonicals) {
    const value = asciiLower(raw);
    if (!validAliasValue(value)) throw new AliasError("invalid_value");
    if (!values.some((v) => v.kind === "string" && v.value === value)) {
      values.push({ kind: "string", value });
    }
  }
  objectSet(entries, key, { kind: "array", items: values });
  writeThesaurusAtomic(thesaurusPath, entries);
}

/** Distinguishes a whole-entry removal from a single value. */
export type AliasRemoved = "entry" | "value";

/**
 * Removes a whole entry, or one value from an entry (dropping the entry
 * when its last value goes), and atomically rewrites the file.
 */
export function removeAlias(thesaurusPath: string, term: string, canonical?: string): AliasRemoved {
  const key = normalizeAliasKey(term);
  const entries = readEditableThesaurus(thesaurusPath);
  // Same collapse as addAlias: the entry being removed may exist only as
  // a case variant, and the rewrite persists the normalization.
  canonicalizeAliasKeys(entries);
  const existing = objectGet(entries, key);
  if (existing === undefined) throw new AliasError("not_found");

  let removed: AliasRemoved = "entry";
  if (canonical !== undefined) {
    const value = asciiLower(canonical);
    if (existing.kind !== "array") throw new AliasError("malformed");
    const at = existing.items.findIndex((v) => v.kind === "string" && v.value === value);
    if (at < 0) throw new AliasError("not_found");
    const values = [...existing.items.slice(0, at), ...existing.items.slice(at + 1)];
    if (values.length === 0) {
      objectRemove(entries, key);
    } else {
      objectSet(entries, key, { kind: "array", items: values });
      removed = "value";
    }
  } else {
    objectRemove(entries, key);
  }
  writeThesaurusAtomic(thesaurusPath, entries);
  return removed;
}

// readEditableThesaurus reads the file as a mutable ordered JSON object;
// a missing file starts empty, but an existing non-object file is
// malformed, never overwritten.
function readEditableThesaurus(thesaurusPath: string): JsonEntry[] {
  let data: Buffer;
  try {
    data = fs.readFileSync(thesaurusPath);
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === "ENOENT") return [];
    throw new AliasError("unavailable", String(err));
  }
  if (data.byteLength > MAX_THESAURUS_BYTES) {
    throw new AliasError("unavailable", `exceeds ${MAX_THESAURUS_BYTES} bytes`);
  }
  const root = parseOrderedJson(data.toString("utf8"));
  if (root === null || root.kind !== "object") throw new AliasError("malformed");
  return root.entries;
}

// writeThesaurusAtomic rewrites the file via a sibling temp file and
// rename, in the same two-space canonical format the owner config uses:
// an `alias add` should produce a one-line diff, not reflow the file.
function writeThesaurusAtomic(thesaurusPath: string, entries: JsonEntry[]): void {
  const text = writeCanonicalJson({ kind: "object", entries }, 0);
  try {
    writeAtomicJsonFile(thesaurusPath, text);
  } catch (err) {
    throw new AliasError("unavailable", String(err));
  }
}

// canonicalizeAliasKeys rewrites the editable document's keys to their
// normalized form and merges entries that collapse to one key — value
// order preserved, repeats dropped, first entry's position kept.
function canonicalizeAliasKeys(entries: JsonEntry[]): void {
  const merged: JsonEntry[] = [];
  for (const kv of entries) {
    const key = normalizeAliasKey(kv.key);
    const existing = merged.find((e) => e.key === key);
    if (existing === undefined) {
      merged.push({ key, value: kv.value });
      continue;
    }
    if (existing.value.kind === "array" && kv.value.kind === "array") {
      for (const value of kv.value.items) {
        const dup = existing.value.items.some(
          (have) => have.kind === "string" && value.kind === "string" && have.value === value.value,
        );
        if (!dup) existing.value.items.push(value);
      }
    }
    // A duplicate whose value is not an array keeps the first entry:
    // arrays are the only alias shape, and guessing is worse.
  }
  entries.splice(0, entries.length, ...merged);
}

/** One weak-query record, appended as a JSON line. */
export interface MissRecord {
  ts: string;
  query: string;
  terms: string[];
  best: number;
  top: string | null;
}

/**
 * Appends one record as a JSON line. Best-effort by contract: every
 * failure is swallowed, and the log stops growing at maxBytes.
 */
export function appendMiss(logPath: string, rec: MissRecord, maxBytes: number | bigint): void {
  // The log is self-produced and self-consumed, so it carries no external
  // format obligation; compact JSON is exactly what aggregateMisses reads
  // back.
  const line = JSON.stringify({ ts: rec.ts, query: rec.query, terms: rec.terms, best: rec.best, top: rec.top });
  try {
    const dir = path.dirname(logPath);
    if (dir !== ".") fs.mkdirSync(dir, { recursive: true, mode: 0o700 });
    const fd = fs.openSync(logPath, "a", 0o600);
    try {
      fs.fchmodSync(fd, 0o600);
      if (BigInt(fs.fstatSync(fd).size) >= BigInt(maxBytes)) return;
      fs.writeFileSync(fd, line + "\n");
    } finally {
      fs.closeSync(fd);
    }
  } catch {
    // Recall is never failed by its own diagnostics.
  }
}

/**
 * One reviewed candidate: a distinct weak query, its frequency, and the
 * union of extracted terms across its misses.
 */
export interface MissCandidate {
  query: string;
  count: number;
  terms: string[];
}

/**
 * Aggregates the miss log for review: dedupes by lowercased query, ranks
 * by frequency (ties alphabetical). Malformed lines are skipped — the log
 * is best-effort on the write side too.
 */
export function aggregateMisses(data: string): MissCandidate[] {
  const byQuery = new Map<string, { count: number; terms: Set<string> }>();
  for (const line of data.split("\n")) {
    // Trim exactly space/tab/CR, not the wider Unicode space set: two
    // queries differing only by a non-breaking space are different
    // queries, and merging them would hide a real vocabulary miss.
    const trimmed = line.replace(/^[ \t\r]+|[ \t\r]+$/g, "");
    if (trimmed.length === 0) continue;
    let record: { query?: unknown; terms?: unknown };
    try {
      record = JSON.parse(trimmed) as { query?: unknown; terms?: unknown };
    } catch {
      continue;
    }
    if (typeof record.query !== "string") continue;
    const query = asciiLower(record.query.replace(/^[ \t]+|[ \t]+$/g, ""));
    let slot = byQuery.get(query);
    if (slot === undefined) {
      slot = { count: 0, terms: new Set() };
      byQuery.set(query, slot);
    }
    slot.count++;
    // terms is optional and tolerated item by item: a mistyped or missing
    // terms value never discards the counted query.
    if (Array.isArray(record.terms)) {
      for (const item of record.terms) {
        if (typeof item === "string") slot.terms.add(item);
      }
    }
  }
  const items = [...byQuery.entries()].map(([query, slot]) => ({
    query,
    count: slot.count,
    terms: [...slot.terms].sort(),
  }));
  items.sort((a, b) => (a.count !== b.count ? b.count - a.count : a.query < b.query ? -1 : 1));
  return items;
}

/**
 * Appends one miss-log record when a search was weak enough to be worth
 * reviewing: opt-in (cfg.missLog), only for real recall outcomes (a match
 * or a typed no-match, never an error), and only below the owner's
 * confidence floor. Best-effort by the same contract as appendMiss. This
 * is miss-log policy, owned here beside the log it feeds, so the CLI and
 * the extension cannot disagree about what a weak query is.
 */
export function logSearchMiss(env: Environ, cfg: Config, query: string, nowMs: number, out: SearchOutput): void {
  if (!cfg.missLog) return;
  if (out.outcome !== "match" && out.outcome !== "no_match") return;
  if (out.bestScore >= cfg.confidenceFloor) return;
  let logPath: string;
  try {
    logPath = missLogPath(env);
  } catch {
    return;
  }
  appendMiss(
    logPath,
    {
      ts: isoFromMs(nowMs),
      query,
      terms: out.queryTerms,
      best: out.bestScore,
      top: out.hits.length > 0 ? out.hits[0].episodeId : null,
    },
    cfg.missLogMaxBytes,
  );
}
