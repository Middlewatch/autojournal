// Owner-curated thesaurus (alias map). The file is the authority and
// stays a flat, hand-editable JSON object mapping a casual query word to
// the canonical journal terms it should also search:
// {"firmware": ["fwupd", "polkit"]}. Byte-compatible with the deployed v1
// map, loaded fresh on every search invocation (editor changes apply
// immediately), never projected into the index. Its canonical digest is
// stamped on results as the alias identity.
//
// Curation is manual by design: the engine never writes an alias except
// through the owner's confirmed add/remove commands. Duplicate and
// case-variant keys merge on load — one duplicated key never disables the
// whole thesaurus.

import * as fs from "node:fs";
import { createHash } from "node:crypto";
import { MAX_TOKEN_LEN } from "./contracts.ts";
import { parseOrderedJson, type JsonValue } from "./json.ts";

/** Bounds the hand-editable map file. */
export const MAX_THESAURUS_BYTES = 256 * 1024;

/** One casual query word and its canonical terms, lowercased. */
export interface AliasEntry {
  key: string;
  values: string[];
}

/**
 * The loaded thesaurus: entries sorted by key plus the canonical digest
 * that stamps search results and how many keys collapsed during load.
 */
export interface AliasMap {
  entries: AliasEntry[];
  mergedKeys: number;
  digestHex: string;
}

/** The canonical terms for one query term, or null. */
export function aliasGet(m: AliasMap, term: string): string[] | null {
  let lo = 0;
  let hi = m.entries.length;
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    if (m.entries[mid].key < term) lo = mid + 1;
    else hi = mid;
  }
  if (lo < m.entries.length && m.entries[lo].key === term) return m.entries[lo].values;
  return null;
}

function lowerAscii(s: string): string {
  return s.replace(/[A-Z]/g, (c) => String.fromCharCode(c.charCodeAt(0) + 32));
}

// normalizeAliasKey canonicalizes a thesaurus key: surrounding whitespace
// dropped, then full Unicode lowercasing — not the ASCII-only fold values
// use — so "Firmware" and " firmware " are one key wherever they appear.
export function normalizeAliasKey(s: string): string {
  return s.trim().toLowerCase();
}

/**
 * The tolerant load: only object entries whose value is an array become
 * aliases (array items that are not strings are skipped item by item);
 * keys normalize and duplicate or case-variant entries merge rather than
 * disabling the file. Anything unreadable or unparseable is a valid
 * empty configuration — recall degrades but never fails because the
 * thesaurus is malformed.
 */
export function loadAliasMapFromBytes(data: Uint8Array): AliasMap {
  let entries: AliasEntry[] = [];
  let merged = 0;
  // Invalid UTF-8 must not turn into plausible-looking U+FFFD alias keys:
  // a damaged file reads as empty rather than as subtly wrong data.
  let text: string | null = null;
  try {
    text = new TextDecoder("utf-8", { fatal: true }).decode(data);
  } catch {
    text = null;
  }
  if (text !== null) {
    const parsed = parseAliasEntries(text);
    if (parsed !== null) [entries, merged] = mergeAliasEntries(parsed);
  }
  entries.sort((a, b) => (a.key < b.key ? -1 : a.key > b.key ? 1 : 0));
  return { entries, mergedKeys: merged, digestHex: aliasDigest(entries) };
}

// parseAliasEntries walks the top-level object so every occurrence of a
// duplicated key is seen (the ordered parser rejects duplicates, so this
// uses a lenient duplicate-preserving variant): null when the document is
// not one clean JSON object.
function parseAliasEntries(text: string): AliasEntry[] | null {
  // The strict ordered parser rejects duplicate keys, which the tolerant
  // merge wants to see. Wrap each raw entry list by parsing leniently:
  // parse with duplicates allowed by splitting on the strict parser's
  // object model when it succeeds, else fall back to a manual walk.
  const doc = parseOrderedJsonAllowingDuplicates(text);
  if (doc === null) return null;
  const entries: AliasEntry[] = [];
  for (const [key, value] of doc) {
    // Only arrays become aliases; null, scalars, and objects are skipped
    // whole. The alternative is guessing what a scalar meant, and a
    // thesaurus that guesses is worse than one that ignores.
    if (!Array.isArray(value)) continue;
    const entry: AliasEntry = { key: normalizeAliasKey(key), values: [] };
    for (const item of value) {
      // Non-string items — numbers, bools, nested containers — are
      // skipped item by item; the key stays.
      if (typeof item !== "string") continue;
      const byteLen = Buffer.byteLength(item, "utf8");
      if (byteLen === 0 || byteLen > MAX_TOKEN_LEN) continue;
      entry.values.push(lowerAscii(item));
    }
    entries.push(entry);
  }
  return entries;
}

// parseOrderedJsonAllowingDuplicates parses one JSON object into ordered
// [key, value] pairs, tolerating duplicate keys (JSON.parse would keep the
// last silently; this keeps all). Values decode through JSON.parse on the
// whole document — per-key duplicates inside nested values are outside
// this loader's tolerance and reject the document.
function parseOrderedJsonAllowingDuplicates(text: string): Array<[string, unknown]> | null {
  // Fast path: no duplicate keys, the strict parser answers.
  const strict = parseOrderedJson(text);
  if (strict !== null) {
    if (strict.kind !== "object") return null;
    return strict.entries.map((e) => [e.key, plainValue(e.value)]);
  }
  // Duplicate keys (or anything else strict rejects): scan the top level
  // manually. JSON.parse validates overall syntax first, so a genuinely
  // malformed document still reads as null.
  try {
    JSON.parse(text);
  } catch {
    return null;
  }
  const pairs: Array<[string, unknown]> = [];
  let i = skipWs(text, 0);
  if (text[i] !== "{") return null;
  i = skipWs(text, i + 1);
  if (text[i] === "}") return pairs;
  for (;;) {
    const keyEnd = scanJsonValue(text, i);
    if (keyEnd < 0) return null;
    const key = JSON.parse(text.slice(i, keyEnd)) as string;
    i = skipWs(text, keyEnd);
    if (text[i] !== ":") return null;
    i = skipWs(text, i + 1);
    const valueEnd = scanJsonValue(text, i);
    if (valueEnd < 0) return null;
    pairs.push([key, JSON.parse(text.slice(i, valueEnd))]);
    i = skipWs(text, valueEnd);
    if (text[i] === ",") {
      i = skipWs(text, i + 1);
      continue;
    }
    if (text[i] === "}") return pairs;
    return null;
  }
}

function plainValue(v: JsonValue): unknown {
  switch (v.kind) {
    case "null":
      return null;
    case "bool":
      return v.value;
    case "string":
      return v.value;
    case "number":
      return Number(v.literal);
    case "array":
      return v.items.map((item) => plainValue(item));
    case "object": {
      const out: Record<string, unknown> = {};
      for (const e of v.entries) out[e.key] = plainValue(e.value);
      return out;
    }
  }
}

function skipWs(text: string, i: number): number {
  while (i < text.length && (text[i] === " " || text[i] === "\t" || text[i] === "\n" || text[i] === "\r")) i++;
  return i;
}

// scanJsonValue returns the index just past one JSON value starting at i,
// or -1. The document as a whole already passed JSON.parse, so this only
// needs balanced-structure scanning with string awareness.
function scanJsonValue(text: string, i: number): number {
  if (i >= text.length) return -1;
  const c = text[i];
  if (c === '"') return scanString(text, i);
  if (c === "{" || c === "[") {
    // The whole document already passed JSON.parse, so bracket kinds
    // necessarily match; only balanced depth with string awareness is
    // needed here.
    let depth = 0;
    let j = i;
    while (j < text.length) {
      const ch = text[j];
      if (ch === '"') {
        j = scanString(text, j);
        if (j < 0) return -1;
        continue;
      }
      if (ch === "{" || ch === "[") depth++;
      if (ch === "}" || ch === "]") {
        depth--;
        if (depth === 0) return j + 1;
      }
      j++;
    }
    return -1;
  }
  let j = i;
  while (j < text.length && !",}] \t\n\r".includes(text[j])) j++;
  return j;
}

function scanString(text: string, i: number): number {
  let j = i + 1;
  while (j < text.length) {
    if (text[j] === "\\") {
      j += 2;
      continue;
    }
    if (text[j] === '"') return j + 1;
    j++;
  }
  return -1;
}

// mergeAliasEntries collapses entries whose normalized keys coincide,
// preserving first-appearance value order and dropping repeated values.
function mergeAliasEntries(parsed: AliasEntry[]): [AliasEntry[], number] {
  const index = new Map<string, number>();
  const out: AliasEntry[] = [];
  let merged = 0;
  for (const entry of parsed) {
    const at = index.get(entry.key);
    if (at === undefined) {
      index.set(entry.key, out.length);
      out.push({ key: entry.key, values: [...entry.values] });
      continue;
    }
    merged++;
    for (const value of entry.values) {
      if (!out[at].values.includes(value)) out[at].values.push(value);
    }
  }
  return [out, merged];
}

/**
 * Loads the map from disk; a missing, oversized, or unreadable map is a
 * valid empty configuration.
 */
export function loadAliasMapFile(thesaurusPath: string): AliasMap {
  let data: Buffer;
  try {
    data = fs.readFileSync(thesaurusPath);
  } catch {
    return loadAliasMapFromBytes(new TextEncoder().encode("{}"));
  }
  if (data.byteLength > MAX_THESAURUS_BYTES) {
    return loadAliasMapFromBytes(new TextEncoder().encode("{}"));
  }
  return loadAliasMapFromBytes(data);
}

// aliasDigest hashes the canonical form: keys sorted, each entry framed
// as length\x00key then \x00-prefixed length\x00value pairs (values
// sorted and deduped) with a '\n' terminator, so the digest tracks
// meaning rather than file formatting.
function aliasDigest(entries: AliasEntry[]): string {
  const h = createHash("sha256");
  const frame = (s: string) => {
    h.update(String(Buffer.byteLength(s, "utf8")));
    h.update(Buffer.from([0]));
    h.update(s, "utf8");
  };
  for (const entry of entries) {
    const sorted = [...entry.values].sort();
    frame(entry.key);
    let prev = "";
    sorted.forEach((v, i) => {
      if (i > 0 && v === prev) return;
      prev = v;
      h.update(Buffer.from([0]));
      frame(v);
    });
    h.update("\n");
  }
  return h.digest("hex");
}
