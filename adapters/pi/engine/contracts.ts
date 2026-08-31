// Closed, versioned capture contracts and typed outcomes.
//
// The wire payload is one JSON object. The schema is closed: unknown fields,
// duplicate fields, missing fields, and over-budget values are all rejected
// with typed reasons rather than best-effort acceptance. Ported from the Go
// engine's contracts.go; the acceptance is a frozen Interface-tier contract.

import { parseOrderedJson, objectGet, type JsonValue, type JsonEntry } from "./json.ts";

/** The only capture wire version accepted. */
export const PAYLOAD_SCHEMA_VERSION = 1;

/** Stamps every rendered episode's frontmatter. */
export const EPISODE_SCHEMA = "aj-episode.v1";

// Size and shape budgets.
export const MAX_PAYLOAD_BYTES = 4 * 1024 * 1024;
export const MAX_CONTENT_BYTES = 2 * 1024 * 1024;
// The read budget for one rendered episode file: the file carries
// frontmatter and separator overhead on top of payload content, so it may
// legitimately exceed MAX_PAYLOAD_BYTES.
export const MAX_EPISODE_FILE_BYTES = MAX_PAYLOAD_BYTES * 2;
// Bounds directory descent below the journal root. The deepest supported
// layout is worlds/<world>/scopes/<scope>/lanes/<lane>/YYYY/MM/DD/file.
export const CORPUS_WALK_DEPTH = 10;
export const MAX_WORLD_LEN = 64;
export const MAX_TOKEN_LEN = 128;
export const MAX_TOOLS = 256;
// Bounds the optional provenance paths (workspace root, branch-of).
export const MAX_PATH_LEN = 512;

// Retrieval bounds. Search returns ranked references and bounded snippets;
// get opens one bounded span. Neither ever returns an unbounded episode body.
export const MAX_QUERY_BYTES = 4096;
export const MAX_QUERY_TERMS = 64;
export const MAX_SNIPPET_LINE_BYTES = 400;
export const MAX_SNIPPET_BYTES = 4096;
export const MAX_GET_LINES = 400;
export const MAX_GET_BYTES = 64 * 1024;
export const MAX_RESULTS_LIMIT = 100;

/**
 * Lane distinguishes normal conversation, delegated work, evaluation, and
 * explicit imported legacy source. A system record type, never a user
 * folder choice.
 */
export type Lane = "conversation" | "delegated_work" | "evaluation" | "imported_legacy";

export const LANES: readonly Lane[] = ["conversation", "delegated_work", "evaluation", "imported_legacy"];

export function validLane(l: string): l is Lane {
  return (LANES as readonly string[]).includes(l);
}

// Event times outside this window are refused at validate with
// ImplausibleEventTime: a wrapped or garbage timestamp would otherwise
// shard the episode into a nonsense date directory. The bounds are
// deliberately wide — the epoch through 9999-12-31T23:59:59Z — because the
// contract's job is refusing nonsense, not judging clocks.
export const MIN_EVENT_TIME_MS = 0n;
export const MAX_EVENT_TIME_MS = 253402300799000n;

/**
 * The capture result vocabulary reported to adapters. published and
 * duplicate are success; everything else is a distinct typed failure.
 * Consumers must tolerate values they do not know: the vocabulary is an
 * interface-tier contract. (v2 removed `superseded`: a redelivery either
 * matches the recorded digest — duplicate — or conflicts.)
 */
export type CaptureOutcome =
  | "published"
  | "duplicate"
  | "conflict"
  | "malformed"
  | "permission_denied"
  | "unavailable"
  | "internal_error";

/** Index freshness, reported independently of source publication. */
export type IndexFreshness = "fresh" | "stale" | "not_built" | "unavailable";

/**
 * The retrieval vocabulary shared by memory_search and memory_get.
 * no_match is a valid result, not a failure.
 */
export type Outcome =
  | "match"
  | "no_match"
  | "stale_revision"
  | "gone"
  | "index_stale"
  | "timeout"
  | "unavailable"
  | "permission_denied"
  | "malformed"
  | "conflict"
  | "internal_error";

/**
 * Validation failure vocabulary, carried in a capture report's `detail`.
 * These CamelCase codes are an Interface-tier contract adapters match on.
 * parsePayload throws only Malformed; validate throws one specific code so
 * the CLI maps each to its contract outcome without string matching.
 */
export type CaptureErrorCode =
  | "Malformed"
  | "UnsupportedSchemaVersion"
  | "InvalidWorld"
  | "InvalidScope"
  | "InvalidLane"
  | "InvalidHarness"
  | "InvalidAdapterVersion"
  | "InvalidSessionId"
  | "InvalidTurnId"
  | "InvalidCapturePolicy"
  | "InvalidTurnOutcome"
  | "ImplausibleEventTime"
  | "EmptyUserContent"
  | "EmptyAssistantResult"
  | "OversizedContent"
  | "InvalidUtf8"
  | "TooManyTools"
  | "InvalidToolName"
  | "InvalidWorkspaceRoot"
  | "InvalidBranchOf"
  | "InvalidHost"
  | "ContainmentViolation"
  | "PermissionDenied"
  | "Unavailable";

export class CaptureError extends Error {
  readonly code: CaptureErrorCode;
  constructor(code: CaptureErrorCode) {
    super(code);
    this.name = "CaptureError";
    this.code = code;
  }
}

/** Maps any thrown value to the capture error vocabulary. */
export function captureErrorName(err: unknown): CaptureErrorCode {
  return err instanceof CaptureError ? err.code : "Unavailable";
}

/** The allowlisted safe metadata for one tool call: its name only. */
export interface Tool {
  readonly name: string;
}

/**
 * The wire shape prior to validation. world and scope may be omitted and
 * filled from owner defaults. eventTimeMs stays a bigint here so an
 * out-of-range uint64 is carried exactly to the implausibility check.
 */
export interface RawPayload {
  schemaVersion: number;
  world: string | null;
  scope: string | null;
  lane: string;
  harness: string;
  adapterVersion: string;
  sessionId: string;
  turnId: string;
  eventTimeMs: bigint;
  capturePolicy: string;
  turnOutcome: string;
  userContent: string;
  assistantResult: string;
  tools: Tool[] | null; // null when the wire omitted the field
  // Optional session provenance: where the turn happened. Excluded from the
  // payload digest like other capture-source metadata, so a faithful
  // re-delivery still dedupes.
  workspaceRoot: string | null;
  branchOf: string | null;
  host: string | null;
}

/** A validated capture payload. */
export interface Payload {
  world: string;
  scope: string;
  lane: Lane;
  harness: string;
  adapterVersion: string;
  sessionId: string;
  turnId: string;
  eventTimeMs: number;
  capturePolicy: string;
  turnOutcome: string;
  userContent: string;
  assistantResult: string;
  tools: Tool[];
  workspaceRoot: string | null;
  branchOf: string | null;
  host: string | null;
}

const utf8ByteLength = (s: string): number => Buffer.byteLength(s, "utf8");

/**
 * Whether s names a directory component: lowercase alphanumeric plus '-',
 * bounded, never starting with '.' (enforced by charset).
 */
export function validWorld(s: string): boolean {
  if (s.length === 0 || s.length > MAX_WORLD_LEN) return false;
  return /^[a-z0-9-]+$/.test(s);
}

/**
 * Whether s is a safe identity token (session, turn, harness, policy,
 * scope, outcome, adapter version): printable, no whitespace or control
 * bytes, so it embeds safely in frontmatter lines and canonical digest
 * input. The charset is ASCII, so code-unit length equals byte length.
 */
export function validToken(s: string): boolean {
  if (s.length === 0 || utf8ByteLength(s) > MAX_TOKEN_LEN) return false;
  return /^[A-Za-z0-9._\-:+/@]+$/.test(s);
}

/**
 * Whether s is safe as a frontmatter line value. Provenance paths are never
 * directory components or digest input, so the rule is line safety, not a
 * charset: bounded, well-formed, and free of control bytes. Spaces and
 * non-ASCII are legitimate in real filesystem paths and are allowed.
 */
export function validPath(s: string): boolean {
  if (s.length === 0 || utf8ByteLength(s) > MAX_PATH_LEN) return false;
  if (!s.isWellFormed()) return false;
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    if (c < 0x20 || c === 0x7f) return false;
  }
  return true;
}

/**
 * Whether s is a usable scope name. Scopes are both frontmatter tokens and
 * directory components; unlike general identity tokens they cannot contain
 * a path separator, name a traversal component, or start with '.' — the
 * corpus walk skips dot-directories as foreign tooling state, so a dot-led
 * scope would publish episodes the walk could never see.
 */
export function validScope(s: string): boolean {
  if (!validToken(s) || s.startsWith(".")) return false;
  return !s.includes("/");
}

// The closed wire object.
const REQUIRED_KEYS = [
  "schema_version",
  "lane",
  "harness",
  "adapter_version",
  "session_id",
  "turn_id",
  "event_time_ms",
  "capture_policy",
  "turn_outcome",
  "user_content",
  "assistant_result",
] as const;

const OPTIONAL_KEYS = new Set(["world", "scope", "tools", "workspace_root", "branch_of", "host"]);

const malformed = () => new CaptureError("Malformed");

/**
 * Parses the wire bytes into a RawPayload. Every parse-level problem — over
 * budget, invalid JSON, duplicate or unknown fields, missing required
 * fields, wrong value types — collapses to Malformed. One typed outcome is
 * the honest answer because every one of them has the same remedy for the
 * adapter that sent it: fix the payload.
 */
export function parsePayload(bytes: Uint8Array): RawPayload {
  if (bytes.byteLength > MAX_PAYLOAD_BYTES) throw malformed();
  // Invalid UTF-8 decodes to U+FFFD inside strings (Go parity) and to a
  // syntax error anywhere else, because U+FFFD is not valid JSON syntax.
  const text = new TextDecoder("utf-8").decode(bytes);
  const root = parseOrderedJson(text);
  if (root === null || root.kind !== "object") throw malformed();
  const entries = root.entries;
  for (const key of REQUIRED_KEYS) {
    if (objectGet(entries, key) === undefined) throw malformed();
  }
  for (const e of entries) {
    if (!OPTIONAL_KEYS.has(e.key) && !(REQUIRED_KEYS as readonly string[]).includes(e.key)) {
      throw malformed();
    }
  }
  const schemaVersion = reqUint(entries, "schema_version", 32);
  const eventTimeMs = reqUint(entries, "event_time_ms", 64);
  return {
    schemaVersion: Number(schemaVersion),
    eventTimeMs,
    lane: reqString(entries, "lane"),
    harness: reqString(entries, "harness"),
    adapterVersion: reqString(entries, "adapter_version"),
    sessionId: reqString(entries, "session_id"),
    turnId: reqString(entries, "turn_id"),
    capturePolicy: reqString(entries, "capture_policy"),
    turnOutcome: reqString(entries, "turn_outcome"),
    userContent: reqString(entries, "user_content"),
    assistantResult: reqString(entries, "assistant_result"),
    world: optString(entries, "world"),
    scope: optString(entries, "scope"),
    workspaceRoot: optString(entries, "workspace_root"),
    branchOf: optString(entries, "branch_of"),
    host: optString(entries, "host"),
    tools: optTools(entries),
  };
}

// reqString extracts a required string field, rejecting JSON null and
// non-strings.
function reqString(entries: readonly JsonEntry[], key: string): string {
  const v = objectGet(entries, key);
  if (v === undefined || v.kind !== "string") throw malformed();
  return v.value;
}

// optString extracts an optional string field: absent or JSON null maps to
// null, anything else must be a string.
function optString(entries: readonly JsonEntry[], key: string): string | null {
  const v = objectGet(entries, key);
  if (v === undefined || v.kind === "null") return null;
  if (v.kind !== "string") throw malformed();
  return v.value;
}

// reqUint extracts a required unsigned integer field. Only bare decimal
// digits are accepted — no sign, fraction, or exponent. These fields feed
// identity and digest derivation, so each value must have exactly one
// textual form; accepting 1.0e3 for 1000 would make identity depend on
// formatting.
function reqUint(entries: readonly JsonEntry[], key: string, bitSize: 32 | 64): bigint {
  const v = objectGet(entries, key);
  if (v === undefined || v.kind !== "number") throw malformed();
  if (!/^[0-9]+$/.test(v.literal)) throw malformed();
  const n = BigInt(v.literal);
  if (n >= 1n << BigInt(bitSize)) throw malformed();
  return n;
}

// optTools extracts the optional tools array. Each element must be an
// object carrying exactly one "name" string — the closed schema does not
// admit future tool metadata without a schema version bump.
function optTools(entries: readonly JsonEntry[]): Tool[] | null {
  const v = objectGet(entries, "tools");
  if (v === undefined || v.kind === "null") return null;
  if (v.kind !== "array") throw malformed();
  const tools: Tool[] = [];
  for (const e of v.items) {
    if (e.kind !== "object" || e.entries.length !== 1) throw malformed();
    const nameV = objectGet(e.entries, "name");
    if (nameV === undefined || nameV.kind !== "string") throw malformed();
    tools.push({ name: nameV.value });
  }
  return tools;
}

/**
 * Checks a parsed payload against the closed contract and returns the
 * capture-ready Payload. The check order is fixed so that a payload with
 * several problems always reports the same first failure, which is what
 * lets an adapter test against a stable error code.
 */
export function validate(raw: RawPayload): Payload {
  const fail = (code: CaptureErrorCode): never => {
    throw new CaptureError(code);
  };
  if (raw.schemaVersion !== PAYLOAD_SCHEMA_VERSION) fail("UnsupportedSchemaVersion");
  if (raw.world === null) fail("InvalidWorld");
  if (raw.scope === null) fail("InvalidScope");
  if (!validWorld(raw.world!)) fail("InvalidWorld");
  if (!validScope(raw.scope!)) fail("InvalidScope");
  if (!validLane(raw.lane)) fail("InvalidLane");
  if (raw.eventTimeMs < MIN_EVENT_TIME_MS || raw.eventTimeMs > MAX_EVENT_TIME_MS) {
    fail("ImplausibleEventTime");
  }
  if (!validToken(raw.harness)) fail("InvalidHarness");
  if (!validToken(raw.adapterVersion)) fail("InvalidAdapterVersion");
  if (!validToken(raw.sessionId)) fail("InvalidSessionId");
  if (!validToken(raw.turnId)) fail("InvalidTurnId");
  if (!validToken(raw.capturePolicy)) fail("InvalidCapturePolicy");
  if (!validToken(raw.turnOutcome)) fail("InvalidTurnOutcome");
  if (raw.userContent.length === 0) fail("EmptyUserContent");
  if (raw.assistantResult.length === 0) fail("EmptyAssistantResult");
  if (utf8ByteLength(raw.userContent) > MAX_CONTENT_BYTES || utf8ByteLength(raw.assistantResult) > MAX_CONTENT_BYTES) {
    fail("OversizedContent");
  }
  if (!raw.userContent.isWellFormed() || !raw.assistantResult.isWellFormed()) fail("InvalidUtf8");
  const tools = raw.tools ?? [];
  if (tools.length > MAX_TOOLS) fail("TooManyTools");
  for (const t of tools) {
    if (!validToken(t.name)) fail("InvalidToolName");
  }
  if (raw.workspaceRoot !== null && !validPath(raw.workspaceRoot)) fail("InvalidWorkspaceRoot");
  if (raw.branchOf !== null && !validPath(raw.branchOf)) fail("InvalidBranchOf");
  if (raw.host !== null && !validToken(raw.host)) fail("InvalidHost");
  return {
    world: raw.world!,
    scope: raw.scope!,
    lane: raw.lane as Lane,
    harness: raw.harness,
    adapterVersion: raw.adapterVersion,
    sessionId: raw.sessionId,
    turnId: raw.turnId,
    eventTimeMs: Number(raw.eventTimeMs),
    capturePolicy: raw.capturePolicy,
    turnOutcome: raw.turnOutcome,
    userContent: raw.userContent,
    assistantResult: raw.assistantResult,
    tools,
    workspaceRoot: raw.workspaceRoot,
    branchOf: raw.branchOf,
    host: raw.host,
  };
}
