// Journal maintenance: the whole operations a host exposes to its owner
// beyond capture and recall. Status and sync carry accounting that is
// easy to get subtly wrong — freshness must fold deliberate exclusions in
// or a corpus with one duplicate reads permanently stale, and sync must
// re-stamp the root identity — so those rules live here where the CLI and
// the extension share them.

import * as fs from "node:fs";
import * as path from "node:path";
import { MAX_EPISODE_FILE_BYTES, type IndexFreshness } from "./contracts.ts";
import { verifyEpisode, resealDigestHex } from "./episode.ts";
import { frontmatterDigestHex } from "./render.ts";
import { DIGEST_PREFIX } from "./identity.ts";
import { rootDigestHex, resolveJournalRoot } from "./paths.ts";
import {
  countEpisodes,
  openExistingRoot,
  rootInSharedDirectory,
  walkCorpus,
  readRootFile,
  writeTemp,
  syncDir,
  TempCollisionError,
  type JournalRoot,
} from "./corpus.ts";
import {
  openSnapshot,
  freshnessOf,
  syncSnapshot,
  worldScopePairs,
  truncatedCount,
  type SyncReport,
  type WorldScope,
} from "./index.ts";
import type { CaptureDefaults } from "./config.ts";

/** One journal health report. */
export interface Status {
  /**
   * False when the journal root does not exist yet. Not an error: a
   * harness that has captured nothing has no root, and reporting zero
   * episodes against a missing root is the honest answer.
   */
  rootOk: boolean;
  /** Episode files found by walking the corpus. */
  episodes: number;
  /** Episode rows the projection holds. */
  indexed: number;
  /** Indexed episodes carrying oversize-truncation accounting. */
  truncated: number;
  freshness: IndexFreshness;
}

/**
 * Read-only: never creates the root, the snapshot, or their parents, so a
 * status check cannot itself change what it reports.
 */
export function statusOf(rootPath: string, indexPath: string): Status {
  let root: JournalRoot;
  try {
    root = openExistingRoot(rootPath);
  } catch {
    return { rootOk: false, episodes: 0, indexed: 0, truncated: 0, freshness: "not_built" };
  }
  const digest = rootDigestHex(rootPath);
  const opened = openSnapshot(indexPath, digest);
  if (opened.kind === "foreign") {
    return { rootOk: true, episodes: countEpisodes(root), indexed: 0, truncated: 0, freshness: "unavailable" };
  }
  if (opened.kind === "not_built") {
    return { rootOk: true, episodes: countEpisodes(root), indexed: 0, truncated: 0, freshness: "not_built" };
  }
  const fresh = freshnessOf(opened.snapshot, root);
  return {
    rootOk: true,
    episodes: fresh.source,
    indexed: fresh.indexed,
    truncated: truncatedCount(opened.snapshot),
    freshness: fresh.freshness,
  };
}

/** Sync failure vocabulary, one code per typed compatibility error. */
export type SyncErrorCode = "shared_directory" | "root_missing" | "index_unavailable" | "sync_failed";

export class SyncError extends Error {
  readonly code: SyncErrorCode;
  constructor(code: SyncErrorCode, detail?: string) {
    super(detail === undefined ? code : `${code}: ${detail}`);
    this.name = "SyncError";
    this.code = code;
  }
}

/**
 * Brings the projection up to date with the corpus and re-stamps its
 * identity. Deliberately without the foreign-root gate: sync replaces
 * whatever snapshot is at indexPath with this root's content, which is
 * the documented way to repoint an index.
 */
export function sync(rootPath: string, indexPath: string): SyncReport {
  if (rootInSharedDirectory(rootPath)) throw new SyncError("shared_directory");
  let root: JournalRoot;
  try {
    root = openExistingRoot(rootPath);
  } catch {
    throw new SyncError("root_missing");
  }
  try {
    fs.chmodSync(root.path, 0o700);
  } catch {
    throw new SyncError("index_unavailable", "cannot harden journal root");
  }
  try {
    return syncSnapshot(root, indexPath, rootDigestHex(rootPath));
  } catch (err) {
    throw new SyncError("sync_failed", String(err));
  }
}

/**
 * Reseal accounting: scanned counts every visible episode file, resealed
 * the digest-stale files re-attested (or, under preview, that would be),
 * refused the files that no longer parse as an episode — or cannot be
 * read, or whose digest line cannot be located — and are left untouched.
 * writeFailures counts files reseal tried to rewrite and could not: the
 * sweep continues past them so one bad shard costs one file, and the
 * terminal sync still runs, but the caller must treat any nonzero count
 * as a failed invocation.
 */
export interface ResealReport {
  scanned: number;
  resealed: number;
  refused: number;
  writeFailures: number;
  paths: string[];
}

/**
 * Re-attests owner-edited episodes: a digest-stale file gets its
 * payload_digest line rewritten to resealDigestHex's chosen reading,
 * through the same owner-only temp-write and atomic rename discipline
 * capture uses, then one sync rebaselines the projection. A file that
 * verifies is skipped; a file that no longer parses is counted and left
 * untouched — reseal re-attests a well-formed edit, never repairs a
 * broken file. Under preview it counts and lists and writes nothing.
 */
export function reseal(rootPath: string, indexPath: string, preview: boolean): ResealReport {
  const report: ResealReport = { scanned: 0, resealed: 0, refused: 0, writeFailures: 0, paths: [] };
  if (rootInSharedDirectory(rootPath)) throw new SyncError("shared_directory");
  let root: JournalRoot;
  try {
    root = openExistingRoot(rootPath);
  } catch {
    throw new SyncError("root_missing");
  }
  walkCorpus(root, (entry) => {
    if (entry.kind !== "episode") return;
    report.scanned++;
    let content: string;
    try {
      content = readRootFile(root, entry.relPath, MAX_EPISODE_FILE_BYTES).toString("utf8");
    } catch {
      report.refused++;
      return;
    }
    const verdict = verifyEpisode(content);
    if (verdict.ok) return;
    if (verdict.failure !== "digest_mismatch") {
      report.refused++;
      return;
    }
    const digest = resealDigestHex(content);
    if (digest === null) {
      report.refused++;
      return;
    }
    const rewritten = rewriteDigestLine(content, digest);
    if (rewritten === null) {
      report.refused++;
      return;
    }
    if (!preview) {
      try {
        resealWrite(root, entry.relPath, rewritten);
      } catch {
        report.writeFailures++;
        return;
      }
    }
    report.resealed++;
    report.paths.push(entry.relPath);
  });
  if (!preview) sync(rootPath, indexPath);
  return report;
}

// rewriteDigestLine replaces the recorded digest hex on the frontmatter's
// payload_digest line, leaving every other byte untouched. Null when the
// content carries no parseable digest line — a state reseal counts as
// refused rather than repairs.
function rewriteDigestLine(content: string, newHex: string): string | null {
  const oldHex = frontmatterDigestHex(content);
  if (oldHex === null) return null;
  const oldLine = "payload_digest: " + DIGEST_PREFIX + oldHex;
  const newLine = "payload_digest: " + DIGEST_PREFIX + newHex;
  if (!content.includes(oldLine)) return null;
  return content.replace(oldLine, newLine);
}

// resealWrite replaces one episode file in place: exclusive owner-only
// temp in the episode's own directory, fsync, rename over the final name,
// directory fsync — so a torn reseal can never leave a half-written
// episode.
function resealWrite(root: JournalRoot, relPath: string, content: string): void {
  const abs = path.join(root.path, relPath);
  const dirAbs = path.dirname(abs);
  const name = path.basename(abs);
  const tmpName = "." + name + ".reseal.tmp";
  // A leftover temp is a crashed reseal's garbage; removing it first
  // keeps one old crash from hard-failing every future reseal.
  try {
    fs.rmSync(path.join(dirAbs, tmpName), { force: true });
  } catch {
    // Nothing to clear.
  }
  try {
    writeTemp(dirAbs, tmpName, Buffer.from(content, "utf8"));
  } catch (err) {
    if (err instanceof TempCollisionError) throw new Error("reseal temp collision");
    throw err;
  }
  try {
    fs.renameSync(path.join(dirAbs, tmpName), abs);
  } finally {
    try {
      fs.rmSync(path.join(dirAbs, tmpName), { force: true });
    } catch {
      // Renamed away already in the success path.
    }
  }
  syncDir(dirAbs);
}

/**
 * The world/scope pairs an owner can select: the configured capture
 * default pair first, then every pair the projection knows, deduplicated
 * in discovery order. A missing or unusable snapshot yields the default
 * pair alone — catalog is a convenience view, never an error path.
 */
export function catalog(rootPath: string, indexPath: string, defaults: CaptureDefaults): WorldScope[] {
  const pairs: WorldScope[] = [{ world: defaults.world, scope: defaults.scope }];
  const opened = openSnapshot(indexPath, rootDigestHex(resolveJournalRoot(rootPath)));
  if (opened.kind !== "ok") return pairs;
  for (const row of worldScopePairs(opened.snapshot)) {
    if (!pairs.some((p) => p.world === row.world && p.scope === row.scope)) pairs.push(row);
  }
  return pairs;
}
