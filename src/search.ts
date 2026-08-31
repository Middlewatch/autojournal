// memory_search and memory_get orchestration.
//
// Search is not evidence opening: search returns ranked, stable evidence
// references with bounded snippets; get opens one reference with explicit
// line bounds and validates identity and revision against the file on
// disk. Failures are typed outcomes, never exceptions — recall degrading
// is a normal result the caller renders.
//
// Discovery pipeline: query terms → additive alias expansion → additive
// singular folding of plural terms → vocabulary substring scan over the
// snapshot's sorted term list → postings under world/scope/lane filters →
// per-line crediting against the verified source text → the pure scorer
// in retrieval.ts → span dedup, per-episode cap, floor, page.

import {
  MAX_QUERY_TERMS,
  MAX_RESULTS_LIMIT,
  MAX_SNIPPET_LINE_BYTES,
  MAX_SNIPPET_BYTES,
  MAX_GET_LINES,
  MAX_GET_BYTES,
  type IndexFreshness,
  type Lane,
  type Outcome,
} from "./contracts.ts";
import { DIGEST_PREFIX, ID_PREFIX, EPISODE_ID_LEN, DIGEST_HEX_LEN } from "./identity.ts";
import { parseEpisode, verifyEpisode } from "./episode.ts";
import { readContained, type JournalRoot } from "./corpus.ts";
import {
  corpusStatSignature,
  lookupEpisode,
  type Snapshot,
  type SnapshotEpisode,
} from "./index.ts";
import {
  extractTerms,
  tokenizeLine,
  isStopWord,
  asciiLower,
  idfWeight,
  rank,
  trailingZeros,
  confidenceWithCoverage,
  cursorEncode,
  cursorDecode,
  MAX_PER_EPISODE_DEFAULT,
  type Candidate,
  type EpisodeInfo,
  type Confidence,
  type CursorInputs,
} from "./retrieval.ts";
import { aliasGet, type AliasMap } from "./aliases.ts";

/** The recall lane set when the caller does not restrict it. */
export const DEFAULT_LANES: readonly Lane[] = ["conversation", "delegated_work", "imported_legacy"];

/** The page size when the caller does not set one. */
export const DEFAULT_RESULTS_LIMIT = 10;

/**
 * Vocabulary terms one query's discovery may match; beyond it discovery
 * is truncated and reported. The surviving matches are a stable prefix of
 * the sorted vocabulary.
 */
export const MAX_VOCAB_MATCHES = 1024;

/**
 * Needles shorter than this are excluded from the vocabulary scan when
 * longer needles exist — a 2-byte needle substring-matches a huge share
 * of the vocabulary and floods MAX_VOCAB_MATCHES. A query whose tokens
 * are all short still scans with them, so curated short alias values
 * keep working on their own.
 */
export const MIN_NEEDLE_LEN = 3;

/**
 * How a term is credited against a matched line's text. substring: any
 * occurrence counts. word_start: the occurrence must begin at a token
 * boundary — "hang" credits "hanging" but not "change". whole_word: both
 * edges must be token boundaries.
 */
export type CreditMode = "substring" | "word_start" | "whole_word";

/** Scoring knobs, resolved from owner config by the caller. */
export interface Knobs {
  contextWindow: number;
  recencyBoost: number;
  minScore: number;
  confidenceFloor: number;
}

export const DEFAULT_KNOBS: Knobs = {
  contextWindow: 3,
  recencyBoost: 1.0,
  minScore: 0.0,
  confidenceFloor: 3.0,
};

export interface SearchRequest {
  query: string;
  world: string;
  scope?: string;
  /** Undefined means DEFAULT_LANES. */
  lanes?: Lane[];
  /** 0 resolves to DEFAULT_RESULTS_LIMIT; above MAX_RESULTS_LIMIT clamps down. */
  limit?: number;
  /** Pages a previous identical request. */
  cursor?: string;
  /** Injectable clock (epoch ms) for deterministic recency; 0 = wall clock. */
  nowMs?: number;
  knobs?: Knobs;
  creditMode?: CreditMode;
}

/** One ranked evidence reference. */
export interface Hit {
  episodeId: string;
  /** sha256:<hex> revision this evidence was ranked against. */
  revision: string;
  path: string;
  scope: string;
  lane: Lane;
  capturePolicy: string;
  eventTimeMs: number;
  /** 1-based matched line in the source file. */
  line: number;
  snippetStart: number;
  snippetEnd: number;
  /** Bounded context, rendered from the same verified content the crediting pass read. */
  snippet: string;
  matchedTerms: string[];
  score: number;
  confidence: Confidence;
}

export interface SearchOutput {
  outcome: Outcome;
  queryTerms: string[];
  aliasTerms: string[];
  /** Additive singular variants that joined the term list. */
  foldedTerms: string[];
  hits: Hit[];
  /** True post-dedup, post-floor result count. */
  total: number;
  nextCursor: string;
  bestScore: number;
  aliasDigest: string;
  freshness: IndexFreshness;
  indexed: number;
  source: number;
  /** Candidates dropped because their source no longer matches the indexed revision. */
  editedExcluded: number;
  detail: string;
}

// singularVariants returns additive singular candidates for one query
// term: "quotas"→"quota", "boxes"→"box"/"boxe", "policies"→"policy".
// Purely additive recall closing the word-form gap word-start crediting
// cannot; a variant that never credits merely contributes df 0.
function singularVariants(term: string): string[] {
  const out: string[] = [];
  const add = (v: string) => {
    if (v.length > 2 && !isStopWord(v)) out.push(v);
  };
  if (term.endsWith("ies") && term.length > 4) {
    add(term.slice(0, -3) + "y");
  } else if (term.endsWith("es") && term.length > 4) {
    add(term.slice(0, -1));
    add(term.slice(0, -2));
  } else if (term.endsWith("s") && !term.endsWith("ss") && term.length > 3) {
    add(term.slice(0, -1));
  }
  return out;
}

/**
 * Runs one memory_search against the snapshot. snapshot null means the
 * projection is not built: reported honestly as index_stale over a
 * nonempty corpus rather than no_match.
 */
export function search(root: JournalRoot, snapshot: Snapshot | null, aliasMap: AliasMap, req: SearchRequest): SearchOutput {
  const knobs = req.knobs ?? DEFAULT_KNOBS;
  const creditMode = req.creditMode ?? "word_start";
  const limit = Math.max(Math.min(req.limit === undefined || req.limit === 0 ? DEFAULT_RESULTS_LIMIT : req.limit, MAX_RESULTS_LIMIT), 1);
  const out: SearchOutput = {
    outcome: "internal_error",
    queryTerms: [],
    aliasTerms: [],
    foldedTerms: [],
    hits: [],
    total: 0,
    nextCursor: "",
    bestScore: 0,
    aliasDigest: aliasMap.digestHex,
    freshness: "unavailable",
    indexed: 0,
    source: 0,
    editedExcluded: 0,
    detail: "",
  };

  // Index health first: an empty projection over a nonempty corpus is
  // index_stale, never no_match. The stat walk also yields the corpus
  // signature the cursor binds.
  const stat = corpusStatSignature(root);
  out.source = stat.episodes;
  if (snapshot === null) {
    out.freshness = "not_built";
    out.outcome = stat.episodes > 0 ? "index_stale" : "no_match";
    return out;
  }
  out.indexed = snapshot.episodes.length;
  out.freshness = stat.signature === snapshot.signature ? "fresh" : "stale";

  // --- Terms and alias expansion ---
  const base = extractTerms(req.query);
  let termsTruncated = base.truncated;
  out.queryTerms = base.items;
  if (base.items.length === 0) {
    out.outcome = "no_match";
    return out;
  }

  // Duplicate term weights are unconditional: the query's own list keeps
  // its repetitions, and alias values and folded singular variants are
  // appended to it — deduplicated against the terms already present,
  // never replacing the list.
  let finalTerms = [...base.items];
  const have = new Set(base.items);
  for (const t of base.items) {
    for (const v of aliasGet(aliasMap, t) ?? []) {
      if (have.has(v)) continue;
      have.add(v);
      out.aliasTerms.push(v);
      finalTerms.push(v);
    }
  }
  for (const t of base.items) {
    for (const v of singularVariants(t)) {
      if (have.has(v)) continue;
      have.add(v);
      out.foldedTerms.push(v);
      finalTerms.push(v);
    }
  }
  if (finalTerms.length > MAX_QUERY_TERMS) {
    finalTerms = finalTerms.slice(0, MAX_QUERY_TERMS);
    termsTruncated = true;
  }

  // --- Cursor (decoded before scoring: its clock pins recency) ---
  const lanes = req.lanes ?? [...DEFAULT_LANES];
  const cursorInputs: CursorInputs = {
    query: req.query,
    world: req.world,
    scope: req.scope ?? "",
    lanes: lanes.join(","),
    aliasDigest: out.aliasDigest,
    corpusSignature: stat.signature,
    rankingTag: `${creditMode};${knobs.contextWindow};${knobs.recencyBoost};${knobs.minScore};${knobs.confidenceFloor}`,
  };
  let nowMs = req.nowMs === undefined || req.nowMs === 0 ? Date.now() : req.nowMs;
  let offset = 0;
  if (req.cursor !== undefined && req.cursor !== "") {
    const decoded = cursorDecode(req.cursor, cursorInputs);
    if (decoded === null) {
      out.outcome = "malformed";
      out.detail = "cursor does not match this query";
      return out;
    }
    offset = decoded.offset;
    nowMs = decoded.nowMs;
  }

  // --- Discovery: vocabulary substring scan over sorted terms ---
  // Needles are the index-token components of each term, so a phrase
  // value like "llama.cpp" discovers via "llama"/"cpp" and is credited by
  // full-substring match on the line text below. The fallback is per
  // query: any long needle makes short ones ride along ignored; only a
  // wholly-short query scans with its short needles, preserving curated
  // short-alias reachability. Both paths iterate the vocabulary in sorted
  // term order, so the cap truncates a stable prefix.
  const needles = new Set<string>();
  const shortNeedles = new Set<string>();
  for (const t of finalTerms) {
    for (const needle of tokenizeLine(t)) {
      if (needle.length >= MIN_NEEDLE_LEN) needles.add(needle);
      else shortNeedles.add(needle);
    }
  }
  const scanNeedles = needles.size > 0 ? [...needles] : [...shortNeedles];
  const vocab = [...snapshot.postings.keys()].sort();
  const vocabMatches: string[] = [];
  let vocabTruncated = false;
  scan: for (const token of vocab) {
    for (const needle of scanNeedles) {
      if (token.includes(needle)) {
        if (vocabMatches.length >= MAX_VOCAB_MATCHES) {
          vocabTruncated = true;
          break scan;
        }
        vocabMatches.push(token);
        continue scan;
      }
    }
  }

  // --- Candidate accumulation from postings ---
  interface EpisodeAccum {
    row: SnapshotEpisode;
    lines: number[];
    unionMask: bigint;
    content: string;
  }
  const laneSet = new Set(lanes);
  const eligible = (row: SnapshotEpisode): boolean =>
    row.world === req.world && (req.scope === undefined || row.scope === req.scope) && laneSet.has(row.lane);
  const episodeOrds = new Map<number, number>(); // snapshot ord -> accum ord
  const episodes: EpisodeAccum[] = [];
  for (const term of vocabMatches) {
    for (const group of snapshot.postings.get(term) ?? []) {
      const snapOrd = group[0];
      const row = snapshot.episodes[snapOrd];
      if (row === undefined || !eligible(row)) continue;
      let ord = episodeOrds.get(snapOrd);
      if (ord === undefined) {
        ord = episodes.length;
        episodes.push({ row, lines: [], unionMask: 0n, content: "" });
        episodeOrds.set(snapOrd, ord);
      }
      const accum = episodes[ord];
      for (let i = 1; i < group.length; i++) {
        accum.lines.push(group[i]);
      }
    }
  }

  const truncationDetail = () => {
    if (vocabTruncated || termsTruncated) out.detail = "discovery_truncated";
  };
  if (episodes.length === 0) {
    out.outcome = "no_match";
    if (out.indexed === 0 && out.source > 0 && out.freshness === "stale") out.outcome = "index_stale";
    truncationDetail();
    return out;
  }

  // --- Per-line crediting against source text ---
  const candidates: Candidate[] = [];
  const df = new Array<number>(finalTerms.length).fill(0);
  for (const [accumOrd, ep] of episodes.entries()) {
    ep.lines = [...new Set(ep.lines)].sort((a, b) => a - b);
    let content: string;
    try {
      content = readContained(root, ep.row.relPath);
    } catch {
      out.editedExcluded++;
      continue;
    }
    // Evidence is verified against content, not against the recorded
    // digest line. A file that verifies against a different digest than
    // the projection holds is an absorbed edit awaiting sync — excluded
    // the same way, never served under a stale reference.
    const verified = verifyEpisode(content);
    if (!verified.ok || verified.episode.digestHex !== ep.row.digestHex) {
      out.editedExcluded++;
      continue;
    }
    ep.content = content;

    const textLines = content.split("\n");
    for (const lineNo of ep.lines) {
      const line = textLines[lineNo - 1];
      if (line === undefined) continue;
      let mask = 0n;
      finalTerms.forEach((term, i) => {
        if (creditLine(line, term, creditMode)) mask |= 1n << BigInt(i);
      });
      if (mask === 0n) continue;
      candidates.push({ episodeOrd: accumOrd, lineNo, matchedMask: mask });
      ep.unionMask |= mask;
    }
    let mask = ep.unionMask;
    while (mask !== 0n) {
      const bit = trailingZeros(mask);
      mask &= mask - 1n;
      if (bit < df.length) df[bit]++;
    }
  }

  if (candidates.length === 0) {
    out.outcome = "no_match";
    truncationDetail();
    return out;
  }

  // --- Score and rank ---
  let creditedEpisodes = 0;
  const episodeInfos: EpisodeInfo[] = episodes.map((ep) => {
    if (ep.unionMask !== 0n) creditedEpisodes++;
    return { episodeId: ep.row.episodeId, relPath: ep.row.relPath, eventTimeMs: ep.row.eventTimeMs };
  });
  // N is the world's non-evaluation episode population; floored at the
  // credited-episode count (and 1) so df can never exceed it.
  const statsN = snapshot.episodes.reduce(
    (n, row) => n + (row.world === req.world && row.lane !== "evaluation" ? 1 : 0),
    0,
  );
  const n = Math.max(statsN, creditedEpisodes, 1);
  const idf = df.map((d) => idfWeight(n, d));

  const ranked = rank(candidates, episodeInfos, idf, {
    nowMs,
    recencyBoost: knobs.recencyBoost,
    minScore: knobs.minScore,
    contextWindow: knobs.contextWindow,
    maxPerEpisode: MAX_PER_EPISODE_DEFAULT,
  });
  out.total = ranked.order.length;
  if (ranked.order.length > 0) out.bestScore = ranked.scores[ranked.order[0]];
  if (ranked.order.length === 0) {
    out.outcome = "no_match";
    truncationDetail();
    return out;
  }

  // --- Page ---
  const start = Math.min(offset, ranked.order.length);
  const end = Math.min(start + limit, ranked.order.length);
  if (end < ranked.order.length) out.nextCursor = cursorEncode(end, nowMs, cursorInputs);

  // --- Render page hits with bounded snippets ---
  out.hits = ranked.order.slice(start, end).map((candIdx) => {
    const cand = candidates[candIdx];
    const ep = episodes[cand.episodeOrd];
    const matched: string[] = [];
    let matchedPositions = 0;
    let mask = cand.matchedMask;
    while (mask !== 0n) {
      const bit = trailingZeros(mask);
      mask &= mask - 1n;
      if (bit >= finalTerms.length) continue;
      matchedPositions++;
      const term = finalTerms[bit];
      if (!matched.includes(term)) matched.push(term);
    }
    const coverage = matchedPositions / finalTerms.length;
    const snip = renderSnippet(ep.content, {
      line: cand.lineNo,
      bodyLine: ep.row.bodyLine,
      contextWindow: knobs.contextWindow,
    });
    return {
      episodeId: ep.row.episodeId,
      revision: DIGEST_PREFIX + ep.row.digestHex,
      path: ep.row.relPath,
      scope: ep.row.scope,
      lane: ep.row.lane,
      capturePolicy: ep.row.capturePolicy,
      eventTimeMs: ep.row.eventTimeMs,
      line: cand.lineNo,
      snippetStart: snip.start,
      snippetEnd: snip.end,
      snippet: snip.text,
      matchedTerms: matched,
      score: ranked.scores[candIdx],
      confidence: confidenceWithCoverage(ranked.scores[candIdx], coverage, knobs.confidenceFloor),
    };
  });
  out.outcome = "match";
  truncationDetail();
  return out;
}

const isIndexTokenByte = (c: number): boolean =>
  (c >= 0x61 && c <= 0x7a) || (c >= 0x41 && c <= 0x5a) || (c >= 0x30 && c <= 0x39) || c === 0x5f;

/**
 * Whether term occurs in line under mode's boundary rule
 * (case-insensitive). Boundaries use the index-token alphabet, so a
 * phrase term ("out of memory", "llama.cpp") is checked at the edges of
 * the whole occurrence and its interior punctuation needs no special
 * handling.
 */
export function creditLine(line: string, term: string, mode: CreditMode): boolean {
  // ASCII-only folding keeps offsets identical between the folded and
  // original text; boundary checks index the folded string directly.
  const lowerLine = asciiLower(line);
  const lowerTerm = asciiLower(term);
  for (let from = 0; from + lowerTerm.length <= lowerLine.length; ) {
    const pos = lowerLine.indexOf(lowerTerm, from);
    if (pos < 0) return false;
    from = pos + 1;
    if (mode === "substring") return true;
    if (pos > 0 && isIndexTokenByte(lowerLine.charCodeAt(pos - 1))) continue;
    if (mode === "word_start") return true;
    const end = pos + lowerTerm.length;
    if (end >= lowerLine.length || !isIndexTokenByte(lowerLine.charCodeAt(end))) return true;
  }
  return false;
}

interface SnippetSpec {
  line: number;
  bodyLine: number;
  contextWindow: number;
}

interface Snippet {
  text: string;
  start: number;
  end: number;
}

// renderSnippet renders ±context_window lines from the content the
// crediting pass already read and verified — one read per episode per
// query, and a snippet always shows the revision that was credited.
// Rendering runs after rank and feeds no scoring input. Lines are clamped
// to the body, each capped at a codepoint boundary, the whole snippet
// capped at MAX_SNIPPET_BYTES.
function renderSnippet(content: string, spec: SnippetSpec): Snippet {
  const first = Math.max(spec.bodyLine, spec.line - spec.contextWindow);
  const last = spec.line + spec.contextWindow;
  let text = "";
  let start = 0;
  let end = 0;
  let any = false;
  const lines = content.split("\n");
  for (let lineNo = 0; lineNo < lines.length; lineNo++) {
    const no = lineNo + 1;
    if (no < first) continue;
    if (no > last) break;
    const capped = capAtCodepoint(lines[lineNo], MAX_SNIPPET_LINE_BYTES);
    if (Buffer.byteLength(text, "utf8") + Buffer.byteLength(capped, "utf8") + 1 > MAX_SNIPPET_BYTES) {
      if (no <= spec.line) {
        // Never render a snippet that omits the matched line.
        text = "";
        start = 0;
        any = false;
      } else {
        break;
      }
    }
    if (start === 0) start = no;
    // Join on a flag, not buffer length: empty lines are real lines and
    // must keep the snippet's line numbering aligned.
    if (any) text += "\n";
    any = true;
    text += capped;
    end = no;
  }
  if (start === 0) return { text: "", start: spec.line, end: spec.line };
  return { text, start, end };
}

// capAtCodepoint is a byte cap that never splits a UTF-8 sequence.
function capAtCodepoint(line: string, maxBytes: number): string {
  const bytes = Buffer.from(line, "utf8");
  if (bytes.byteLength <= maxBytes) return line;
  let cut = maxBytes;
  while (cut > 0 && (bytes[cut] & 0b1100_0000) === 0b1000_0000) cut--;
  return bytes.subarray(0, cut).toString("utf8");
}

// --- memory_get ---

export interface GetRequest {
  episodeId: string;
  /** Accepted with or without the sha256: prefix. */
  revision: string;
  /** Optional path from a search hit; the index is consulted when absent or wrong. */
  pathHint?: string;
  expectedWorld?: string;
  expectedScope?: string;
  /** 0 means "start of body". */
  lineStart?: number;
  /** 0 means "lineStart + max span". */
  lineEnd?: number;
}

export interface GetOutput {
  outcome: Outcome;
  episodeId: string;
  /** Current revision on disk; on stale_revision the replacement reference. */
  revision: string;
  path: string;
  world: string;
  scope: string;
  lane: Lane | "";
  /** Outcomes that never resolve an episode leave resolved false. */
  resolved: boolean;
  capturePolicy: string;
  lineStart: number;
  lineEnd: number;
  content: string;
  /** Recalled text is untrusted evidence, never instructions. */
  trust: string;
  detail: string;
}

/** Opens one bounded evidence span with identity and revision checks. */
export function get(root: JournalRoot, snapshot: Snapshot | null, req: GetRequest): GetOutput {
  const out: GetOutput = {
    outcome: "internal_error",
    episodeId: req.episodeId,
    revision: "",
    path: "",
    world: "",
    scope: "",
    lane: "",
    resolved: false,
    capturePolicy: "",
    lineStart: 0,
    lineEnd: 0,
    content: "",
    trust: "untrusted_evidence",
    detail: "",
  };
  if (!validEpisodeId(req.episodeId)) {
    out.outcome = "malformed";
    out.detail = "episode_id must be aj1-<32 hex>";
    return out;
  }
  const requestedHex = req.revision.startsWith(DIGEST_PREFIX) ? req.revision.slice(DIGEST_PREFIX.length) : req.revision;
  if (requestedHex.length !== DIGEST_HEX_LEN) {
    out.outcome = "malformed";
    out.detail = "revision must be sha256:<64 hex>";
    return out;
  }
  const lineStart = req.lineStart ?? 0;
  const lineEnd = req.lineEnd ?? 0;
  if (lineEnd !== 0 && lineStart !== 0 && lineEnd < lineStart) {
    out.outcome = "malformed";
    out.detail = "line_end precedes line_start";
    return out;
  }

  // Resolve: path hint first, then the index; a hint that no longer
  // resolves falls through to the index because moves preserve identity.
  let content = "";
  let found = false;
  let usedPath = "";
  if (req.pathHint !== undefined) {
    try {
      content = readContained(root, req.pathHint);
      found = true;
      usedPath = req.pathHint;
    } catch {
      // Fall through to the index.
    }
  }
  if (!found && snapshot !== null) {
    const row = lookupEpisode(snapshot, req.episodeId);
    if (row !== null) {
      try {
        content = readContained(root, row.relPath);
        found = true;
        usedPath = row.relPath;
      } catch {
        // Gone below.
      }
    }
  }
  if (!found) {
    out.outcome = "gone";
    out.detail = "no source file for this episode (index may be stale; try sync)";
    return out;
  }

  const ep = parseEpisode(content);
  if (ep === null) {
    out.outcome = "gone";
    out.detail = "source file is no longer a parseable episode";
    return out;
  }
  if (ep.episodeId !== req.episodeId) {
    out.outcome = "gone";
    out.detail = "file at the resolved path carries another episode identity";
    return out;
  }
  if (req.expectedWorld !== undefined && ep.world !== req.expectedWorld) {
    out.outcome = "gone";
    out.detail = "episode is outside the active world";
    return out;
  }
  if (req.expectedScope !== undefined && ep.scope !== req.expectedScope) {
    out.outcome = "gone";
    out.detail = "episode is outside the active scope";
    return out;
  }
  out.resolved = true;
  out.path = usedPath;
  out.world = ep.world;
  out.scope = ep.scope;
  out.lane = ep.lane;
  out.capturePolicy = ep.capturePolicy;

  // Two distinct edit states report differently. A file that does not
  // verify at all — the digest-stale state — has no honest current
  // revision to offer until the owner reseals it. A file that verifies
  // against a different digest than requested is an absorbed edit, and
  // the current verified revision is the replacement reference.
  const verified = verifyEpisode(content);
  if (!verified.ok) {
    out.outcome = "stale_revision";
    out.detail = "episode was edited after capture; run reseal to re-attest it";
    return out;
  }
  out.revision = DIGEST_PREFIX + verified.episode.digestHex;
  if (verified.episode.digestHex !== requestedHex) {
    // Edited evidence is never silently served as the old revision.
    out.outcome = "stale_revision";
    out.detail = "episode was edited; re-search or request the current revision";
    return out;
  }

  // Bounded body span.
  let start = ep.bodyLine;
  if (lineStart !== 0 && lineStart > start) start = lineStart;
  let requestedEnd = start + MAX_GET_LINES - 1;
  if (lineEnd !== 0 && lineEnd < requestedEnd) requestedEnd = lineEnd;

  let text = "";
  let servedStart = 0;
  let servedEnd = 0;
  let any = false;
  const lines = content.split("\n");
  for (let lineNo = 0; lineNo < lines.length; lineNo++) {
    const no = lineNo + 1;
    if (no < start) continue;
    if (no > requestedEnd) break;
    const line = lines[lineNo];
    if (Buffer.byteLength(text, "utf8") + Buffer.byteLength(line, "utf8") + 1 > MAX_GET_BYTES) break;
    if (servedStart === 0) servedStart = no;
    if (any) text += "\n";
    any = true;
    text += line;
    servedEnd = no;
  }
  out.lineStart = servedStart;
  out.lineEnd = servedEnd;
  out.content = text;
  out.outcome = "match";
  return out;
}

function validEpisodeId(id: string): boolean {
  if (id.length !== EPISODE_ID_LEN || !id.startsWith(ID_PREFIX)) return false;
  return /^[0-9a-f]+$/.test(id.slice(ID_PREFIX.length));
}
