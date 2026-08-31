// Episode identity and canonical payload digests.
//
// The episode ID is the idempotency identity: source harness, session,
// turn, world, and capture policy version. Re-delivering the same identity
// with the same payload digest is a duplicate (success); the same identity
// with a different digest is a conflict.
//
// The payload digest covers canonical identity metadata plus the body and
// excludes capture-run metadata (capture time, adapter version, provenance)
// so that a faithful re-delivery hashes identically regardless of when or
// by which adapter build it arrives.
//
// Both derivations are corpus-durable contracts, pinned in both directions
// by testdata/golden. Changing either re-identifies every existing episode
// and invalidates every outstanding evidence reference, which is a major
// version.

import { createHash } from "node:crypto";
import type { Tool, Lane } from "./contracts.ts";

/** Starts every episode ID. */
export const ID_PREFIX = "aj1-";

/** Prefix plus 32 hex chars (128 bits of the identity hash). */
export const EPISODE_ID_LEN = ID_PREFIX.length + 32;

/** Starts every rendered digest value. */
export const DIGEST_PREFIX = "sha256:";

/** Hex length of a full SHA-256 sum. */
export const DIGEST_HEX_LEN = 64;

/** The fields episode identity derives from. */
export interface IdentityFields {
  harness: string;
  sessionId: string;
  turnId: string;
  world: string;
  capturePolicy: string;
}

/** The fields the payload digest covers. */
export interface DigestFields extends IdentityFields {
  scope: string;
  lane: Lane;
  eventTimeMs: number;
  turnOutcome: string;
  userContent: string;
  assistantResult: string;
  tools: Tool[];
}

const ZERO = Buffer.from([0]);

/**
 * Derives the collision-resistant idempotency identity: SHA-256 over a
 * version tag followed by 0x00-separated identity fields, truncated to the
 * first 16 bytes of the sum and hex-encoded.
 */
export function episodeId(p: IdentityFields): string {
  const h = createHash("sha256");
  h.update("autojournal-episode-id.v1");
  for (const field of [p.harness, p.sessionId, p.turnId, p.world, p.capturePolicy]) {
    h.update(ZERO);
    h.update(field, "utf8");
  }
  return ID_PREFIX + h.digest("hex").slice(0, 32);
}

/**
 * Derives the canonical payload digest — the revision identity used by
 * evidence references. The input is length-prefix framed so no content
 * bytes can be confused with framing. Every field that participates is
 * versioned under the leading tag; changing the set of fields requires a
 * new tag.
 */
export function payloadDigestHex(p: DigestFields): string {
  const h = createHash("sha256");
  h.update("autojournal-digest.v1");
  const field = (s: string) => {
    // The framed length is the UTF-8 byte length, not the code-unit count:
    // framing counts the bytes actually hashed.
    const b = Buffer.from(s, "utf8");
    h.update(ZERO);
    h.update(String(b.byteLength));
    h.update(ZERO);
    h.update(b);
  };
  field(p.world);
  field(p.scope);
  field(p.lane);
  field(p.harness);
  field(p.sessionId);
  field(p.turnId);
  field(String(p.eventTimeMs));
  field(p.capturePolicy);
  field(p.turnOutcome);
  field(p.userContent);
  field(p.assistantResult);
  field(String(p.tools.length));
  for (const t of p.tools) {
    field(t.name);
  }
  return h.digest("hex");
}
