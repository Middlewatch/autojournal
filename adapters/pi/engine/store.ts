// Atomic episode publication under a contained journal root, and the
// whole capture transaction.
//
// Default layout: YYYY/MM/DD/<episode-id>.md. Non-default classifications
// add reserved worlds/, scopes/, and lanes/ components before the date.
// Each episode is one immutable file per completed turn. Publication is:
// exclusive owner-only temp file in the target directory → write → fsync →
// atomic no-replace hard-link into place → temp unlink → parent directory
// fsync. An existing target is classified by recorded digest: exact
// duplicate is success, anything else is a typed conflict. (v1's
// supersede path — in-place replacement of a proven extension — was
// removed in v2: capture fires once per settled turn, so a same-identity
// redelivery with different bytes only ever means divergence.)
//
// The descent, temp-write, and fsync mechanics live in corpus.ts with the
// rest of the containment discipline; this file owns the publication
// decision itself.

import * as fs from "node:fs";
import * as path from "node:path";
import {
  MAX_CONTENT_BYTES,
  MAX_EPISODE_FILE_BYTES,
  validate,
  CaptureError,
  captureErrorName,
  type CaptureOutcome,
  type IndexFreshness,
  type Payload,
  type RawPayload,
  type CaptureErrorCode,
} from "./contracts.ts";
import { episodeId, payloadDigestHex } from "./identity.ts";
import { render } from "./render.ts";
import { frontmatterDigestHex } from "./render.ts";
import type { CaptureDefaults } from "./config.ts";
import {
  descendCreating,
  layoutComponents,
  openJournalRoot,
  rootInSharedDirectory,
  syncDir,
  writeTemp,
  StoreError,
  TempCollisionError,
  type JournalRoot,
} from "./corpus.ts";
import { resolveJournalRoot } from "./paths.ts";

/** The result of one publish call. */
export interface Published {
  outcome: Extract<CaptureOutcome, "published" | "duplicate" | "conflict">;
  episodeId: string;
  digestHex: string;
  /**
   * The episode path relative to the journal root, slash-joined (the path
   * vocabulary of evidence references).
   */
  relPath: string;
  /**
   * The rendered episode content, so the capture path can index without
   * re-reading the file it just wrote.
   */
  content: string;
}

/** Oversize-policy accounting carried alongside a publish. */
export interface DroppedBytes {
  user: number;
  assistant: number;
}

const NO_DROPS: DroppedBytes = { user: 0, assistant: 0 };

/**
 * Publishes one validated payload into the journal root. The world
 * subtree is created on demand with owner-only permissions.
 */
export function publish(
  root: JournalRoot,
  payload: Payload,
  captureTimeMs: number,
  drops: DroppedBytes = NO_DROPS,
): Published {
  const id = episodeId(payload);
  const digestHex = payloadDigestHex(payload);
  const content = render({
    payload,
    episodeId: id,
    digestHex,
    captureTimeMs,
    userDroppedBytes: drops.user,
    assistantDroppedBytes: drops.assistant,
  });
  const contentBytes = Buffer.from(content, "utf8");

  const components = layoutComponents(payload);
  const episodeDir = descendCreating(root, components);

  const finalName = id + ".md";
  // The temp name embeds the capture time and an attempt counter; a
  // collision (orphan from a crashed writer) retries a fresh name.
  let tmpName = "";
  let written = false;
  for (let attempt = 0; attempt < 64 && !written; attempt++) {
    tmpName = `.${id}.${captureTimeMs}.${attempt}.tmp`;
    try {
      writeTemp(episodeDir, tmpName, contentBytes);
      written = true;
    } catch (err) {
      if (err instanceof TempCollisionError) continue;
      throw err;
    }
  }
  if (!written) throw new StoreError("unavailable", "temp name: 64 collisions");

  let outcome: Published["outcome"] = "published";
  try {
    try {
      fs.linkSync(path.join(episodeDir, tmpName), path.join(episodeDir, finalName));
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code !== "EEXIST") {
        throw classifyIo("link episode", err);
      }
      outcome = classifyExisting(episodeDir, finalName, digestHex);
    }
  } finally {
    // Whatever happened above, the temp file does not outlive this call.
    try {
      fs.rmSync(path.join(episodeDir, tmpName), { force: true });
    } catch {
      // Best-effort: an orphan is invisible to readers.
    }
  }

  // Make the directory entry durable before reporting success.
  syncDir(episodeDir);

  return {
    outcome,
    episodeId: id,
    digestHex,
    relPath: [...components, finalName].join("/"),
    content,
  };
}

function classifyIo(context: string, err: unknown): StoreError {
  const code = (err as NodeJS.ErrnoException).code;
  if (code === "EACCES" || code === "EPERM") return new StoreError("permission_denied", `${context}: ${String(err)}`);
  return new StoreError("unavailable", `${context}: ${String(err)}`);
}

// classifyExisting decides duplicate or conflict for a target that already
// exists at the derived path: the stored file's *recorded* digest equal to
// the incoming digest is a duplicate — deliberately the recorded line, not
// a re-verification, so an exact redelivery of an episode the owner has
// since hand-edited stays a duplicate, which is the answer that keeps
// redelivery idempotent. Anything else — a differing digest, a file with
// no readable digest line — is a conflict.
//
// The existing file's permissions are repaired to owner-only on the way:
// owner-only is a standing invariant, and a redelivery is a free
// opportunity to fix a file that lost it.
function classifyExisting(dirAbs: string, finalName: string, digestHex: string): "duplicate" | "conflict" {
  const finalPath = path.join(dirAbs, finalName);
  let existing: Buffer;
  try {
    fs.chmodSync(finalPath, 0o600);
    existing = fs.readFileSync(finalPath);
  } catch (err) {
    throw classifyIo("read existing episode", err);
  }
  if (existing.byteLength > MAX_EPISODE_FILE_BYTES) {
    throw new StoreError("unavailable", `read existing episode: exceeds ${MAX_EPISODE_FILE_BYTES} bytes`);
  }
  const recorded = frontmatterDigestHex(existing.toString("utf8"));
  if (recorded !== null && recorded === digestHex) return "duplicate";
  return "conflict";
}

/**
 * The oversize policy (owner ruling 2026-08-31): a side over the content
 * budget is deterministically tail-truncated to the largest code-point
 * boundary within the budget instead of rejecting the turn, and the
 * dropped byte count is recorded in frontmatter rather than vanishing.
 * Returns the payload to validate plus the per-side accounting.
 */
export function applyOversizePolicy(raw: RawPayload): { raw: RawPayload; drops: DroppedBytes } {
  const [userContent, user] = truncateTail(raw.userContent);
  const [assistantResult, assistant] = truncateTail(raw.assistantResult);
  if (user === 0 && assistant === 0) return { raw, drops: NO_DROPS };
  return { raw: { ...raw, userContent, assistantResult }, drops: { user, assistant } };
}

// truncateTail cuts a string to MAX_CONTENT_BYTES of UTF-8, backing off to
// a code-point boundary, and reports the dropped byte count. Lone
// surrogates would make the byte accounting ill-defined; they are left for
// validate to refuse as InvalidUtf8.
function truncateTail(s: string): [string, number] {
  const total = Buffer.byteLength(s, "utf8");
  if (total <= MAX_CONTENT_BYTES || !s.isWellFormed()) return [s, 0];
  const bytes = Buffer.from(s, "utf8");
  let cut = MAX_CONTENT_BYTES;
  // Back off past UTF-8 continuation bytes (10xxxxxx) to a boundary.
  while (cut > 0 && (bytes[cut] & 0b1100_0000) === 0b1000_0000) cut--;
  const kept = bytes.subarray(0, cut).toString("utf8");
  return [kept, total - cut];
}

/** One whole capture transaction's input. */
export interface CaptureInput {
  rootPath: string;
  raw: RawPayload;
  /** Owner capture defaults, for world/scope fill. */
  defaults: CaptureDefaults;
  captureTimeMs: number;
}

/**
 * The transaction's typed outcome. detail carries the capture error code
 * for failure outcomes and is empty for every success.
 */
export interface CaptureResult {
  outcome: CaptureOutcome;
  episodeId: string;
  digestHex: string;
  relPath: string;
  indexState: IndexFreshness;
  detail: CaptureErrorCode | "";
  /**
   * True when the refusal was the shared-directory rule specifically, so
   * a renderer can keep its long-standing remediation wording. Wording is
   * rendering; this flag is the typed sentinel.
   */
  sharedDirectory: boolean;
}

/**
 * Composes the whole capture transaction so the extension and the CLI run
 * the same code rather than the same intent: defaults fill, the oversize
 * policy, validate, root canonicalization, shared-directory refusal,
 * atomic publication. The order is part of the contract: shared-directory
 * refusal is decided before the root is opened (a refused root is never
 * created), and validate before either.
 *
 * The index projection joins this composition at the snapshot-index slice;
 * until then successful publication honestly reports a stale projection.
 */
export function capture(input: CaptureInput): CaptureResult {
  const failure = (outcome: CaptureOutcome, detail: CaptureErrorCode, sharedDirectory = false): CaptureResult => ({
    outcome,
    episodeId: "",
    digestHex: "",
    relPath: "",
    indexState: "not_built",
    detail,
    sharedDirectory,
  });

  // Owner-default world/scope fill: a host provides explicit values only
  // when transporting an owner session choice.
  let raw = input.raw;
  if (raw.world === null) raw = { ...raw, world: input.defaults.world };
  if (raw.scope === null) raw = { ...raw, scope: input.defaults.scope };
  const sized = applyOversizePolicy(raw);

  let payload: Payload;
  try {
    payload = validate(sized.raw);
  } catch (err) {
    return failure("malformed", captureErrorName(err));
  }

  const rootPath = resolveJournalRoot(input.rootPath);
  if (rootInSharedDirectory(rootPath)) {
    return failure("permission_denied", "PermissionDenied", true);
  }

  let root: JournalRoot;
  try {
    root = openJournalRoot(rootPath);
  } catch (err) {
    return failure("unavailable", storeErrorCode(err));
  }

  let published: Published;
  try {
    published = publish(root, payload, input.captureTimeMs, sized.drops);
  } catch (err) {
    if (err instanceof StoreError) {
      switch (err.code) {
        case "containment_violation":
          return failure("internal_error", "ContainmentViolation");
        case "permission_denied":
          return failure("permission_denied", "PermissionDenied");
        default:
          return failure("unavailable", "Unavailable");
      }
    }
    return failure("unavailable", "Unavailable");
  }

  return {
    outcome: published.outcome,
    episodeId: published.episodeId,
    digestHex: published.digestHex,
    relPath: published.relPath,
    indexState: "stale",
    detail: "",
    sharedDirectory: false,
  };
}

function storeErrorCode(err: unknown): CaptureErrorCode {
  if (err instanceof StoreError && err.code === "permission_denied") return "PermissionDenied";
  if (err instanceof CaptureError) return err.code;
  return "Unavailable";
}

