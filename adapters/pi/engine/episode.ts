// Parses stored episodes at the read boundary: frontmatter for index sync
// and rebuild, and — because evidence is verified against content, not
// against a recorded claim — the body, re-deriving the canonical digest
// from what the file actually says before that content is served.
//
// Stored data is untrusted here: a hand-edited or corrupt file yields a
// null parse or a typed verification failure and is excluded with visible
// diagnostics, never a crash and never a merged-by-filename guess.

import {
  EPISODE_SCHEMA,
  validWorld,
  validScope,
  validToken,
  type Lane,
  type Tool,
} from "./contracts.ts";
import { episodeId, payloadDigestHex, DIGEST_PREFIX, DIGEST_HEX_LEN, type DigestFields } from "./identity.ts";

/** The parsed frontmatter view of one stored episode file. */
export interface Episode {
  episodeId: string;
  world: string;
  scope: string;
  lane: Lane;
  harness: string;
  sessionId: string;
  turnId: string;
  eventTimeMs: number;
  captureTimeMs: number;
  capturePolicy: string;
  turnOutcome: string;
  digestHex: string;
  /**
   * 1-based line number of the first line after the closing `---`.
   * Frontmatter is metadata, not memory: indexing and snippet clamping
   * start here.
   */
  bodyLine: number;
  /** Offset (in string code units) of that same first body line. */
  bodyOffset: number;
}

// The frontmatter keys every episode must carry exactly once: absence and
// duplication are both refused, since a duplicated required key leaves
// readers free to disagree about which line binds.
export const REQUIRED_EPISODE_KEYS = [
  "episode_id",
  "world",
  "scope",
  "lane",
  "harness",
  "session_id",
  "turn_id",
  "event_time_ms",
  "capture_time_ms",
  "capture_policy",
  "turn_outcome",
  "payload_digest",
] as const;

/**
 * Re-reads the leading frontmatter block of a stored episode. Unknown keys
 * are tolerated on read (a newer writer may add fields); any missing,
 * duplicated, malformed, or contract-violating required value yields null.
 */
export function parseEpisode(content: string): Episode | null {
  if (!content.startsWith("---\n")) return null;
  let schema = "";
  const fields: Record<string, string> = {};
  let eventTimeMs = 0;
  let captureTimeMs = 0;
  let digestHex = "";
  const seen = new Set<string>();
  const required = new Set<string>(REQUIRED_EPISODE_KEYS);

  let rest = content.slice(4);
  let offset = 4;
  let lineNo = 1; // the opening `---` is line 1
  let closed = false;
  while (rest.length > 0) {
    const lineEnd = rest.indexOf("\n");
    if (lineEnd < 0) break;
    const line = rest.slice(0, lineEnd);
    rest = rest.slice(lineEnd + 1);
    offset += lineEnd + 1;
    lineNo++;
    if (line === "---") {
      closed = true;
      break;
    }
    const sep = line.indexOf(": ");
    if (sep < 0) return null;
    const key = line.slice(0, sep);
    const value = line.slice(sep + 2);
    switch (key) {
      case "schema":
        schema = value;
        break;
      case "lane":
        if (value !== "conversation" && value !== "delegated_work" && value !== "evaluation" && value !== "imported_legacy") {
          return null;
        }
        fields.lane = value;
        break;
      case "event_time_ms": {
        const n = parseFrontmatterUint(value);
        if (n === null) return null;
        eventTimeMs = n;
        break;
      }
      case "capture_time_ms": {
        const n = parseFrontmatterUint(value);
        if (n === null) return null;
        captureTimeMs = n;
        break;
      }
      case "payload_digest": {
        if (!value.startsWith(DIGEST_PREFIX)) return null;
        const hexPart = value.slice(DIGEST_PREFIX.length);
        if (hexPart.length !== DIGEST_HEX_LEN) return null;
        digestHex = hexPart;
        break;
      }
      case "episode_id":
      case "world":
      case "scope":
      case "harness":
      case "session_id":
      case "turn_id":
      case "capture_policy":
      case "turn_outcome":
        fields[key] = value;
        break;
      default:
        break; // unknown keys are tolerated on read
    }
    // A duplicated required key makes the record ambiguous — readers could
    // disagree about which line wins, and for payload_digest that
    // disagreement turns reseal's success report into a lie. No file this
    // product wrote has one; refuse rather than pick. Duplicated unknown
    // keys stay tolerated: they bind nothing.
    if (required.has(key) && seen.has(key)) return null;
    seen.add(key);
  }
  if (!closed) return null;
  if (schema !== EPISODE_SCHEMA) return null;
  for (const k of REQUIRED_EPISODE_KEYS) {
    if (!seen.has(k)) return null;
  }
  const ep: Episode = {
    episodeId: fields.episode_id,
    world: fields.world,
    scope: fields.scope,
    lane: fields.lane as Lane,
    harness: fields.harness,
    sessionId: fields.session_id,
    turnId: fields.turn_id,
    eventTimeMs,
    captureTimeMs,
    capturePolicy: fields.capture_policy,
    turnOutcome: fields.turn_outcome,
    digestHex,
    bodyLine: lineNo + 1,
    bodyOffset: offset,
  };
  // The read boundary revalidates exactly the charsets capture enforced:
  // scope through validScope, matching the write boundary. No episode this
  // product wrote can fail it, so a file that does is a visible, located
  // problem — skipped_malformed — not a tolerated one.
  if (!validWorld(ep.world)) return null;
  if (!validScope(ep.scope)) return null;
  for (const s of [ep.harness, ep.sessionId, ep.turnId, ep.capturePolicy, ep.turnOutcome]) {
    if (!validToken(s)) return null;
  }
  return ep;
}

// parseFrontmatterUint parses a frontmatter integer value: bare decimal
// digits only. Rendered files never carry signs or whitespace; a
// hand-edited value that does is treated as corruption, not interpreted.
// Values beyond 2^53-1 are refused here (a deviation from the Go engine,
// which parsed the full uint64 range): the product never writes one — the
// event-time contract caps at year 9999 — and a number type cannot carry
// it exactly through digest re-derivation.
function parseFrontmatterUint(s: string): number | null {
  if (s.length === 0 || !/^[0-9]+$/.test(s)) return null;
  const n = Number(s);
  return Number.isSafeInteger(n) ? n : null;
}

// --- Body parsing and digest verification ---

// Body separators, exactly as render emits them. They are ordinary text an
// owner's content may also contain, which is why parsing enumerates
// candidate splits instead of trusting the first occurrence.
const BODY_USER_HEADER = "\n## User\n\n";
const BODY_ASSISTANT_SEP = "\n\n## Assistant\n\n";
const BODY_TOOLS_SEP = "\n\n## Tools\n\n";
const BODY_TOOL_LINE_PREFIX = "- ";

// Caps the candidate readings of one body. A rendered body is not
// injectively decodable: the "## Assistant" and "## Tools" separators are
// ordinary text that owner content may also contain, so one byte sequence
// can be the rendering of several distinct payloads. The cap bounds the
// search; a body exceeding it is reported as unverifiable rather than
// guessed at.
//
// The unit is evaluated candidate *pairs*, not occurrences of either
// separator: the candidate space is the cross product of
// assistant-separator positions and tools-separator positions plus the
// no-tools reading. Enumeration is lazy in render order and stops at the
// cap.
export const MAX_BODY_INTERPRETATIONS = 64;

/**
 * A parsed episode together with the body reading whose recomputed digest
 * equals the digest the file records about itself.
 */
export interface VerifiedEpisode extends Episode {
  userContent: string;
  assistantResult: string;
  tools: Tool[];
}

export type VerifyFailure =
  /** The content is not a parseable episode. */
  | "episode_malformed"
  /**
   * The content parses, but no reading of its body recomputes to the
   * digest the file records — or the recorded episode id disagrees with
   * the identity its own fields derive, which would otherwise let an
   * edited id line shadow another episode's recall. This is the
   * edited-episode state: excluded from search, stale_revision for get,
   * counted by sync.
   */
  | "digest_mismatch";

export type VerifyResult =
  | { ok: true; episode: VerifiedEpisode }
  | { ok: false; failure: VerifyFailure };

// recordedIdentityAgrees re-derives the episode id from the five identity
// fields the frontmatter carries and compares it to the recorded id line.
// The payload digest does not cover the id itself — it covers the fields
// the id derives from — so without this check an edited id line would
// verify clean and serve one episode's content under another's identity.
function recordedIdentityAgrees(ep: Episode): boolean {
  return (
    episodeId({
      world: ep.world,
      harness: ep.harness,
      sessionId: ep.sessionId,
      turnId: ep.turnId,
      capturePolicy: ep.capturePolicy,
    }) === ep.episodeId
  );
}

// One candidate decomposition of a body region.
interface BodyReading {
  userContent: string;
  assistantResult: string;
  tools: Tool[];
}

// digestFields assembles the fields the digest derivation covers from the
// parsed frontmatter plus one candidate reading.
function digestFields(ep: Episode, r: BodyReading): DigestFields {
  return {
    world: ep.world,
    scope: ep.scope,
    lane: ep.lane,
    harness: ep.harness,
    sessionId: ep.sessionId,
    turnId: ep.turnId,
    eventTimeMs: ep.eventTimeMs,
    capturePolicy: ep.capturePolicy,
    turnOutcome: ep.turnOutcome,
    userContent: r.userContent,
    assistantResult: r.assistantResult,
    tools: r.tools,
  };
}

// parseToolsSection reads a candidate tools section: one or more lines,
// each exactly "- <name>\n" with a name satisfying the tool-name rule.
// Render never emits an empty section, so zero lines is not a reading.
function parseToolsSection(section: string): Tool[] | null {
  if (section === "") return null;
  const tools: Tool[] = [];
  let rest = section;
  while (rest.length > 0) {
    const lineEnd = rest.indexOf("\n");
    if (lineEnd < 0) return null;
    const line = rest.slice(0, lineEnd);
    rest = rest.slice(lineEnd + 1);
    if (!line.startsWith(BODY_TOOL_LINE_PREFIX)) return null;
    const name = line.slice(BODY_TOOL_LINE_PREFIX.length);
    if (!validToken(name)) return null;
    tools.push({ name });
  }
  return tools;
}

// enumerateReadings walks every candidate decomposition of the body region
// in render order — earliest assistant separator first, and within one, the
// no-tools reading before any tools reading — calling visit for each
// structurally valid candidate until visit returns true (stop, found) or
// the enumeration ends. Returns [candidates visited, found].
function enumerateReadings(body: string, visit: (r: BodyReading) => boolean): [number, boolean] {
  let visited = 0;
  if (!body.startsWith(BODY_USER_HEADER)) return [0, false];
  const region = body.slice(BODY_USER_HEADER.length);
  // Splits examined are bounded too, valid or not: a corrupt body with many
  // assistant separators and no valid reading must return promptly, not
  // scan quadratically. The bound cannot strand a legitimate file — every
  // rendered region ends in a newline, so each earlier split yields a
  // countable no-tools candidate and the pair cap fires first.
  let splits = 0;
  for (let from = 0; ; ) {
    const i = region.indexOf(BODY_ASSISTANT_SEP, from);
    if (i < 0) return [visited, false];
    splits++;
    if (splits > MAX_BODY_INTERPRETATIONS) return [visited, false];
    const split = i;
    const userContent = region.slice(0, split);
    const rest = region.slice(split + BODY_ASSISTANT_SEP.length);

    // The no-tools reading: everything up to a final newline.
    if (rest.endsWith("\n")) {
      visited++;
      if (visit({ userContent, assistantResult: rest.slice(0, -1), tools: [] })) {
        return [visited, true];
      }
      if (visited >= MAX_BODY_INTERPRETATIONS) return [visited, false];
    }
    // Tools readings, earliest separator first.
    for (let tfrom = 0; ; ) {
      const j = rest.indexOf(BODY_TOOLS_SEP, tfrom);
      if (j < 0) break;
      const tools = parseToolsSection(rest.slice(j + BODY_TOOLS_SEP.length));
      if (tools !== null) {
        visited++;
        if (visit({ userContent, assistantResult: rest.slice(0, j), tools })) {
          return [visited, true];
        }
        if (visited >= MAX_BODY_INTERPRETATIONS) return [visited, false];
      }
      tfrom = j + 1;
    }
    from = split + 1;
  }
}

/**
 * Parses content and returns the reading of its body that agrees with the
 * recorded digest. Existence, not uniqueness, is the test: an unedited
 * file's true reading is always among the candidates, and an edited file
 * has no agreeing candidate short of a SHA-256 collision. The no-tools
 * reading is a candidate in its own right, not a fallback — a body ending
 * in something tools-shaped may be an assistant result that happens to
 * look like one.
 */
export function verifyEpisode(content: string): VerifyResult {
  const ep = parseEpisode(content);
  if (ep === null) return { ok: false, failure: "episode_malformed" };
  if (!recordedIdentityAgrees(ep)) return { ok: false, failure: "digest_mismatch" };
  const body = content.slice(ep.bodyOffset);
  let found: BodyReading | null = null;
  const [visited, ok] = enumerateReadings(body, (r) => {
    if (payloadDigestHex(digestFields(ep, r)) === ep.digestHex) {
      found = { userContent: r.userContent, assistantResult: r.assistantResult, tools: r.tools };
      return true;
    }
    return false;
  });
  if (ok && found !== null) {
    return { ok: true, episode: { ...ep, ...(found as BodyReading) } };
  }
  if (visited === 0) {
    // No structurally valid reading at all: this body is not a rendering of
    // anything, which is a different state than an edit the digest catches
    // — reseal refuses it rather than re-attesting it.
    return { ok: false, failure: "episode_malformed" };
  }
  return { ok: false, failure: "digest_mismatch" };
}

/**
 * Returns the digest of the first structurally valid candidate reading in
 * render order — earliest assistant separator, then the no-tools reading
 * before any tools reading — for reseal to write back. Returns null when
 * the content does not parse as an episode at all: reseal re-attests a
 * well-formed edit and never repairs a broken file.
 *
 * On an ambiguous body the chosen reading may not be the decomposition the
 * owner had in mind, and after reseal that reading is what verifyEpisode
 * returns to get. The alternative — refusing to reseal an ambiguous body —
 * would strand exactly the episodes whose content made the ambiguity,
 * which is the wrong side to fail on.
 */
export function resealDigestHex(content: string): string | null {
  const ep = parseEpisode(content);
  if (ep === null) return null;
  if (!recordedIdentityAgrees(ep)) {
    // A lying identity line is not re-attestable: identity is
    // corpus-durable and reseal rewrites only the digest line.
    return null;
  }
  let digest = "";
  const [, found] = enumerateReadings(content.slice(ep.bodyOffset), (r) => {
    digest = payloadDigestHex(digestFields(ep, r));
    return true;
  });
  return found ? digest : null;
}
