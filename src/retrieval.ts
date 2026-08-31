// Proven lexical retrieval core, ported from the Go engine (itself the
// settled port of the judged v1 scoring). Pure — no I/O. This slice
// carries the tokenizer the index builder needs; the scorer, confidence
// banding, and cursor land with the retrieval slice.

import { createHash } from "node:crypto";
import { MAX_QUERY_TERMS, MAX_TOKEN_LEN } from "./contracts.ts";

// Version identities. TOKENIZER_VERSION gates the index: postings from
// another tokenizer version are disposed.
export const TOKENIZER_VERSION = "aj-tok.v1";

// The verbatim v1 stop-word list. The 2-byte entries are live on the
// index side (its token floor is 2, one below the query side's 3), so
// they do filter real index tokens.
const STOP_WORDS = new Set([
  "a", "an", "the", "is", "are",
  "was", "were", "be", "been", "being",
  "have", "has", "had", "do", "does",
  "did", "will", "would", "could", "should",
  "may", "might", "shall", "can", "need",
  "dare", "ought", "used", "to", "of",
  "in", "for", "on", "with", "at",
  "by", "from", "as", "into", "through",
  "during", "before", "after", "above", "below",
  "between", "out", "off", "over", "under",
  "again", "further", "then", "once", "here",
  "there", "when", "where", "why", "how",
  "all", "both", "each", "few", "more",
  "most", "other", "some", "such", "no",
  "nor", "not", "only", "own", "same",
  "so", "than", "too", "very", "just",
  "because", "but", "and", "or", "if",
  "while", "about", "up", "down", "what",
  "which", "who", "whom", "this", "that",
  "these", "those", "i", "me", "my",
  "myself", "we", "our", "ours", "ourselves",
  "you", "your", "yours", "yourself", "yourselves",
  "he", "him", "his", "himself", "she",
  "her", "hers", "herself", "it", "its",
  "itself", "they", "them", "their", "theirs",
  "themselves", "also", "get", "got", "like",
  "know", "think", "want", "look", "use",
  "find", "give", "tell", "say", "said",
  "take", "come", "make", "go", "see",
  "thing", "things", "really", "something", "anything",
  "remember", "mentioned", "talked",
]);

/** Whether word (already lowercased) is on the v1 stop-word list. */
export function isStopWord(word: string): boolean {
  return STOP_WORDS.has(word);
}

const isQueryTokenChar = (c: number): boolean =>
  (c >= 0x61 && c <= 0x7a) || (c >= 0x30 && c <= 0x39) || c === 0x5f;

const isIndexTokenChar = (c: number): boolean =>
  (c >= 0x61 && c <= 0x7a) || (c >= 0x41 && c <= 0x5a) || (c >= 0x30 && c <= 0x39) || c === 0x5f;

/**
 * Query-side tokenization: maximal lowercase [a-z0-9_]+ runs, dropping
 * runs of two bytes or fewer and stop words, duplicates preserved (a
 * repeated query word legitimately doubles its weight). Equivalent to the
 * v1 pipeline for all inputs including UTF-8, because non-ASCII code
 * units are separators under both.
 */
export interface Terms {
  items: string[];
  /** True when the term cap dropped trailing terms. */
  truncated: boolean;
}

// ASCII-only lowercasing, matching the Go byte pipeline: Unicode
// lowercasing (İ → i + combining dot) would turn separator characters
// into token characters, drift the vocabulary, and desynchronize
// crediting offsets.
export function asciiLower(s: string): string {
  return s.replace(/[A-Z]/g, (c) => String.fromCharCode(c.charCodeAt(0) + 32));
}

export function extractTerms(query: string): Terms {
  const lower = asciiLower(query);
  const items: string[] = [];
  let truncated = false;
  let i = 0;
  while (i < lower.length) {
    while (i < lower.length && !isQueryTokenChar(lower.charCodeAt(i))) i++;
    const start = i;
    while (i < lower.length && isQueryTokenChar(lower.charCodeAt(i))) i++;
    const word = lower.slice(start, i);
    if (word.length <= 2 || isStopWord(word)) continue;
    if (items.length >= MAX_QUERY_TERMS) {
      truncated = true;
      break;
    }
    items.push(word);
  }
  return { items, truncated };
}

/**
 * Index-side tokenization: same alphabet and stop-word list as the query
 * side, plus a byte cap that keeps hash blobs out of the vocabulary. The
 * length floor is 2 here, one shorter than the query side: curated alias
 * values may legitimately be two bytes, and discovery happens against
 * this vocabulary.
 */
export function tokenizeLine(line: string): string[] {
  const out: string[] = [];
  let i = 0;
  while (i < line.length) {
    while (i < line.length && !isIndexTokenChar(line.charCodeAt(i))) i++;
    const start = i;
    while (i < line.length && isIndexTokenChar(line.charCodeAt(i))) i++;
    const raw = line.slice(start, i);
    if (raw.length < 2 || raw.length > MAX_TOKEN_LEN) continue;
    const word = asciiLower(raw); // runs are pure ASCII by construction
    if (isStopWord(word)) continue;
    out.push(word);
  }
  return out;
}

// --- Scorer ---

// aj-scorer.v4 is v3 with the S0-review derived-tier fix: the rarity
// weight becomes the smoothed idf ln(1 + (N − df + 0.5)/(df + 0.5)),
// replacing ln(N/df) — bounded for df approaching N and stable for tiny
// corpora. Ordering semantics (duplicate term weights, additive folding,
// span dedup, per-episode cap, deterministic tie-breaks) carry over
// unchanged from the judged v3 behavior.
export const SCORER_VERSION = "aj-scorer.v4";
export const CONFIDENCE_POLICY_VERSION = "aj-conf.v2";

const MS_PER_DAY = 24 * 60 * 60 * 1000;

/** The per-episode page cap introduced in aj-scorer.v2. */
export const MAX_PER_EPISODE_DEFAULT = 2;

/**
 * The exponent on term coverage in aj-conf.v2 confidence banding. Linear
 * discount is what the calibration runs measured.
 */
export const CONFIDENCE_COVERAGE_ALPHA = 1.0;

/**
 * 1 + boost/(days+1), where days is elapsed time floored to 24-hour
 * units. A nudge, not an override; future timestamps get no boost.
 */
export function recencyMultiplier(eventTimeMs: number, nowMs: number, boost: number): number {
  if (eventTimeMs > nowMs) return 1.0;
  const days = Math.floor((nowMs - eventTimeMs) / MS_PER_DAY);
  return 1.0 + boost / (days + 1.0);
}

/**
 * The aj-scorer.v4 smoothed rarity weight. df == 0 (term absent from the
 * candidate corpus) contributes 0, preserving the v1 df.get(t) semantics.
 */
export function idfWeight(corpusN: number, df: number): number {
  if (df === 0) return 0.0;
  const n = Math.max(corpusN, 1);
  return Math.log(1 + (n - df + 0.5) / (df + 0.5));
}

/**
 * One matched body line. matchedMask has bit i set when query term i (by
 * position in the duplicate-preserving term list) occurs in the line —
 * per-line crediting.
 */
export interface Candidate {
  /** Indexes the caller's episode table. */
  episodeOrd: number;
  /** 1-based absolute line number in the episode file. */
  lineNo: number;
  matchedMask: bigint;
}

export interface EpisodeInfo {
  episodeId: string;
  relPath: string;
  eventTimeMs: number;
}

export interface RankParams {
  nowMs: number;
  recencyBoost: number;
  /** 0 disables the relevance floor. */
  minScore: number;
  contextWindow: number;
  /**
   * Caps how many result regions one episode contributes to the ordering.
   * 0 disables; search passes MAX_PER_EPISODE_DEFAULT.
   */
  maxPerEpisode: number;
}

/**
 * The scorer's output: order holds candidate indices, ranked,
 * deduplicated, and floored (pagination is a slice of this ordering);
 * scores is parallel to the input candidates array.
 */
export interface Ranked {
  order: number[];
  scores: number[];
}

/**
 * Scores, sorts, and deduplicates candidates. idf is indexed by query
 * term position (duplicate terms carry their weight twice, once per
 * position). Deterministic: ties break on (rel_path, line_no) so the
 * ordering never depends on candidate arrival order.
 */
export function rank(candidates: Candidate[], episodes: EpisodeInfo[], idf: number[], params: RankParams): Ranked {
  const scores = new Array<number>(candidates.length);
  candidates.forEach((c, i) => {
    let rarity = 0;
    let mask = c.matchedMask;
    while (mask !== 0n) {
      const bit = trailingZeros(mask);
      mask &= mask - 1n;
      if (bit < idf.length) rarity += idf[bit];
    }
    const ep = episodes[c.episodeOrd];
    scores[i] = rarity * recencyMultiplier(ep.eventTimeMs, params.nowMs, params.recencyBoost);
  });

  const order = candidates.map((_, i) => i);
  order.sort((a, b) => {
    if (scores[a] !== scores[b]) return scores[b] - scores[a];
    const ca = candidates[a];
    const cb = candidates[b];
    const pa = episodes[ca.episodeOrd].relPath;
    const pb = episodes[cb.episodeOrd].relPath;
    if (pa !== pb) return pa < pb ? -1 : 1;
    return ca.lineNo - cb.lineNo;
  });

  // Span dedup after ranking: the best-scoring line in each
  // context_window*2 line bucket of an episode survives, so adjacent
  // matches collapse to one result region.
  const bucketSpan = Math.max(params.contextWindow * 2, 1);
  const seen = new Set<string>();
  const perEpisode = new Map<number, number>();
  const kept: number[] = [];
  for (const idx of order) {
    const c = candidates[idx];
    if (params.minScore > 0 && scores[idx] < params.minScore) continue;
    const key = c.episodeOrd + ":" + Math.floor(c.lineNo / bucketSpan);
    if (seen.has(key)) continue;
    if (params.maxPerEpisode > 0 && (perEpisode.get(c.episodeOrd) ?? 0) >= params.maxPerEpisode) continue;
    seen.add(key);
    perEpisode.set(c.episodeOrd, (perEpisode.get(c.episodeOrd) ?? 0) + 1);
    kept.push(idx);
  }
  return { order: kept, scores };
}

/** The lowest set bit's index of a nonzero bigint mask. */
export function trailingZeros(mask: bigint): number {
  let n = 0;
  while ((mask & 1n) === 0n) {
    mask >>= 1n;
    n++;
  }
  return n;
}

// --- Confidence ---

/**
 * The stable wire vocabulary shared by every confidence-policy version. A
 * policy version may change classification without changing these values.
 */
export type Confidence = "low" | "medium" | "high";

/**
 * aj-conf.v2 banding: the score is discounted by
 * coverage^CONFIDENCE_COVERAGE_ALPHA before banding, so a hit matching
 * only a fraction of the query's terms needs a proportionally stronger
 * score to earn the same band. Ordering never uses this — display trust
 * only.
 */
export function confidenceWithCoverage(score: number, coverage: number, floor: number): Confidence {
  const c = Math.min(Math.max(coverage, 0), 1);
  return confidenceOf(score * Math.pow(c, CONFIDENCE_COVERAGE_ALPHA), floor);
}

/** Bands a score off the floor; the floor is the legacy weak-query bar. */
export function confidenceOf(score: number, floor: number): Confidence {
  if (floor <= 0) return "high";
  if (score >= 2.0 * floor) return "high";
  if (score >= floor) return "medium";
  return "low";
}

// --- Cursor ---

// aj2 cursors (the S0-review fix): the wire shape is
// "aj2.<offset>.<nowMs>.<8 hex guard>", and the guard additionally binds
// the corpus stat-walk signature and the first page's resolved clock —
// so a corpus change between pages invalidates the cursor (candidate
// ordinals may have shifted), and every page of one search scores
// recency against the same instant.
export const CURSOR_PREFIX = "aj2.";
export const CURSOR_GUARD_HEX_LEN = 8;

export interface CursorInputs {
  query: string;
  world: string;
  scope: string;
  lanes: string;
  aliasDigest: string;
  corpusSignature: string;
}

/** The 8-hex-char guard binding a cursor to its minting state. */
export function cursorGuardHex(inputs: CursorInputs, nowMs: number): string {
  const h = createHash("sha256");
  h.update(SCORER_VERSION);
  for (const field of [
    inputs.query,
    inputs.world,
    inputs.scope,
    inputs.lanes,
    inputs.aliasDigest,
    inputs.corpusSignature,
    String(nowMs),
  ]) {
    h.update(`\x00${Buffer.byteLength(field, "utf8")}\x00`);
    h.update(field, "utf8");
  }
  return h.digest("hex").slice(0, CURSOR_GUARD_HEX_LEN);
}

export function cursorEncode(offset: number, nowMs: number, inputs: CursorInputs): string {
  return `${CURSOR_PREFIX}${offset}.${nowMs}.${cursorGuardHex(inputs, nowMs)}`;
}

export interface DecodedCursor {
  offset: number;
  nowMs: number;
}

/**
 * Validates a cursor against the state that minted it and returns its
 * offset and clock; null for anything malformed. Only the canonical
 * spelling this module mints decodes: "07" parses to the same offset as
 * "7" but was never minted.
 */
export function cursorDecode(cursor: string, inputs: CursorInputs): DecodedCursor | null {
  if (!cursor.startsWith(CURSOR_PREFIX)) return null;
  const rest = cursor.slice(CURSOR_PREFIX.length);
  const parts = rest.split(".");
  if (parts.length !== 3) return null;
  const [offsetText, nowText, guard] = parts;
  if (!/^[0-9]+$/.test(offsetText) || !/^[0-9]+$/.test(nowText)) return null;
  const offset = Number(offsetText);
  const nowMs = Number(nowText);
  if (!Number.isSafeInteger(offset) || String(offset) !== offsetText) return null;
  if (!Number.isSafeInteger(nowMs) || String(nowMs) !== nowText) return null;
  if (guard !== cursorGuardHex(inputs, nowMs)) return null;
  return { offset, nowMs };
}
