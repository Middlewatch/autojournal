// Episode file rendering: closed frontmatter plus Markdown body.
//
// The rendered file is the authoritative artifact. Frontmatter values are
// restricted to validated charsets — identity fields to token charsets,
// provenance paths to control-free single-line text — so no quoting or
// escaping layer is needed; body content is arbitrary validated text and is
// never inspected by the frontmatter parser, which stops at the closing
// delimiter.
//
// The byte layout below is corpus-durable: frozen and verified
// byte-for-byte against testdata/golden/episodes. Changing it makes every
// existing episode unreadable to a future build, which is a major version.

import { EPISODE_SCHEMA, type Payload } from "./contracts.ts";
import { DIGEST_PREFIX, DIGEST_HEX_LEN } from "./identity.ts";

/** Everything render needs beyond the payload itself. */
export interface RenderInput {
  payload: Payload;
  episodeId: string;
  digestHex: string;
  captureTimeMs: number;
  /**
   * Oversize-policy accounting (v2): bytes deterministically dropped from
   * the tail of each side before validation. Rendered as optional
   * frontmatter only when nonzero, so episodes from turns that fit stay
   * byte-identical to the v1 rendering.
   */
  userDroppedBytes?: number;
  assistantDroppedBytes?: number;
}

/**
 * Produces the complete episode file content. It cannot fail: every input
 * was already validated into frontmatter-safe charsets.
 */
export function render(input: RenderInput): string {
  const p = input.payload;
  let out =
    `---\n` +
    `schema: ${EPISODE_SCHEMA}\n` +
    `episode_id: ${input.episodeId}\n` +
    `world: ${p.world}\n` +
    `scope: ${p.scope}\n` +
    `lane: ${p.lane}\n` +
    `harness: ${p.harness}\n` +
    `adapter_version: ${p.adapterVersion}\n` +
    `session_id: ${p.sessionId}\n` +
    `turn_id: ${p.turnId}\n` +
    `event_time: ${isoFromMs(p.eventTimeMs)}\n` +
    `event_time_ms: ${p.eventTimeMs}\n` +
    `capture_time: ${isoFromMs(input.captureTimeMs)}\n` +
    `capture_time_ms: ${input.captureTimeMs}\n` +
    `capture_policy: ${p.capturePolicy}\n` +
    `turn_outcome: ${p.turnOutcome}\n`;
  // Truncation accounting renders before provenance: like capture_time it
  // describes this capture run, and like provenance it is absent from
  // the digest, so a faithful redelivery still dedupes.
  if ((input.userDroppedBytes ?? 0) > 0) out += `user_dropped_bytes: ${input.userDroppedBytes}\n`;
  if ((input.assistantDroppedBytes ?? 0) > 0) out += `assistant_dropped_bytes: ${input.assistantDroppedBytes}\n`;
  // Optional provenance keys render only when the payload carried them, so
  // episodes from adapters that do not know them stay byte-identical to the
  // pre-provenance rendering.
  if (p.workspaceRoot !== null) out += `workspace_root: ${p.workspaceRoot}\n`;
  if (p.branchOf !== null) out += `branch_of: ${p.branchOf}\n`;
  if (p.host !== null) out += `host: ${p.host}\n`;
  out +=
    `payload_digest: ${DIGEST_PREFIX}${input.digestHex}\n` +
    `---\n\n## User\n\n${p.userContent}\n\n## Assistant\n\n${p.assistantResult}\n`;
  if (p.tools.length > 0) {
    out += "\n## Tools\n\n";
    for (const t of p.tools) {
      out += `- ${t.name}\n`;
    }
  }
  return out;
}

/**
 * Renders epoch milliseconds as UTC ISO-8601 to second precision. Inputs
 * beyond the validated event-time window are refused upstream; this
 * function does not defend itself.
 */
export function isoFromMs(ms: number): string {
  return new Date(Math.floor(ms / 1000) * 1000).toISOString().slice(0, 19) + "Z";
}

/**
 * Extracts the digest hex from a rendered episode's frontmatter, for the
 * duplicate-vs-conflict decision on redelivery. Returns null when the file
 * has no parseable digest line in its leading frontmatter block.
 */
export function frontmatterDigestHex(content: string): string | null {
  const key = "payload_digest: " + DIGEST_PREFIX;
  if (!content.startsWith("---\n")) return null;
  let rest = content.slice(4);
  while (rest.length > 0) {
    let lineEnd = rest.indexOf("\n");
    if (lineEnd < 0) lineEnd = rest.length;
    const line = rest.slice(0, lineEnd);
    if (line === "---") return null;
    if (line.startsWith(key)) {
      const hexPart = line.slice(key.length);
      return hexPart.length === DIGEST_HEX_LEN ? hexPart : null;
    }
    if (lineEnd === rest.length) return null;
    rest = rest.slice(lineEnd + 1);
  }
  return null;
}
