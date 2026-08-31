// Proven lexical retrieval core, ported from the Go engine (itself the
// settled port of the judged v1 scoring). Pure — no I/O. This slice
// carries the tokenizer the index builder needs; the scorer, confidence
// banding, and cursor land with the retrieval slice.

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
// into token characters and drift the vocabulary.
function asciiLower(s: string): string {
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
