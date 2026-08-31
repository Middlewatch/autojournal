// Disposable single-file snapshot projection over the Markdown corpus
// (ADR 0002: stdlib snapshot replaces SQLite).
//
// Contains only rebuildable retrieval state. Deleting the snapshot and
// running sync reproduces it from the authoritative episode files; a
// failed or contended update leaves capture successful and the projection
// visibly stale. Writers serialize on an O_EXCL lock file with stale-lock
// recovery; the snapshot is replaced by atomic rename, so readers only
// ever observe a complete file.
//
// Freshness is a stat-walk signature: the SHA-256 of every visible
// episode's (relpath, size, mtime), in walk order, recorded at the moment
// the projection was built. The projection is fresh exactly while the
// corpus re-derives the same signature — a move, add, remove, or normal
// edit all change it. An edit that forges size and mtime stays invisible
// here and is caught by the per-episode digest verification on the
// serving path.

import * as fs from "node:fs";
import * as path from "node:path";
import { createHash } from "node:crypto";
import { MAX_EPISODE_FILE_BYTES, type IndexFreshness, type Lane } from "./contracts.ts";
import { parseEpisode, verifyEpisode, type Episode } from "./episode.ts";
import { tokenizeLine, TOKENIZER_VERSION } from "./retrieval.ts";
import { walkCorpus, readRootFile, type JournalRoot } from "./corpus.ts";

/**
 * The snapshot's format identity; a snapshot stamped with anything else
 * (or another tokenizer) is disposable state from another build and reads
 * as not built.
 */
export const SNAPSHOT_FORMAT_VERSION = 1;

/** One indexed episode row. */
export interface SnapshotEpisode {
  episodeId: string;
  digestHex: string;
  relPath: string;
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
  bodyLine: number;
  /** SHA-256 of the file bytes when indexed — the incremental skip test. */
  contentSha256: string;
  /** Total oversize-policy bytes dropped (both sides), for reporting. */
  droppedBytes: number;
}

/** term → [episodeOrd, line, line, ...] groups, ords into episodes[]. */
export type Postings = Map<string, number[][]>;

export interface Snapshot {
  rootDigest: string;
  signature: string;
  /** Episode files visible at build time (indexed + excluded). */
  sourceEpisodes: number;
  /** Deliberate exclusions at build time (malformed + duplicates + unreadable). */
  excluded: number;
  episodes: SnapshotEpisode[];
  postings: Postings;
  byId: Map<string, number>;
}

/** How many indexed episodes carry truncation accounting. */
export function truncatedCount(snap: Snapshot): number {
  return snap.episodes.reduce((n, e) => n + (e.droppedBytes > 0 ? 1 : 0), 0);
}

// --- Stat-walk signature -------------------------------------------------

export interface CorpusStat {
  signature: string;
  episodes: number;
  maxMtimeMs: number;
}

/**
 * One stat-only walk: the freshness signature plus the counts reporters
 * show. One lstat per episode file, no reads.
 */
export function corpusStatSignature(root: JournalRoot): CorpusStat {
  const h = createHash("sha256");
  let episodes = 0;
  let maxMtimeMs = 0;
  walkCorpus(root, (entry) => {
    if (entry.kind === "unreadable_dir") {
      h.update(`!${entry.relPath}\n`);
      return;
    }
    if (entry.kind !== "episode") return;
    episodes++;
    const mtime = Math.floor(entry.mtimeMs ?? 0);
    if (mtime > maxMtimeMs) maxMtimeMs = mtime;
    h.update(`${entry.relPath}\u0000${entry.sizeBytes}\u0000${mtime}\n`);
  });
  return { signature: h.digest("hex"), episodes, maxMtimeMs };
}

// --- Snapshot file I/O ---------------------------------------------------

interface SnapshotWire {
  format: number;
  tokenizer: string;
  root_digest: string;
  signature: string;
  source_episodes: number;
  excluded: number;
  episodes: unknown[][];
  postings: Record<string, number[][]>;
}

export type OpenSnapshotResult =
  | { kind: "ok"; snapshot: Snapshot }
  /** Absent, corrupt, or another build's format: rebuildable, never fatal. */
  | { kind: "not_built" }
  /**
   * The snapshot records another root's identity. Never interpreted as
   * empty memory; the caller reports unavailable and advises sync.
   */
  | { kind: "foreign" };

export function openSnapshot(indexPath: string, expectedRootDigest: string | null): OpenSnapshotResult {
  let data: Buffer;
  try {
    data = fs.readFileSync(indexPath);
  } catch {
    return { kind: "not_built" };
  }
  let wire: SnapshotWire;
  try {
    wire = JSON.parse(data.toString("utf8")) as SnapshotWire;
  } catch {
    return { kind: "not_built" };
  }
  if (
    typeof wire !== "object" ||
    wire === null ||
    wire.format !== SNAPSHOT_FORMAT_VERSION ||
    wire.tokenizer !== TOKENIZER_VERSION ||
    !Array.isArray(wire.episodes) ||
    wire.postings === null ||
    typeof wire.postings !== "object"
  ) {
    return { kind: "not_built" };
  }
  if (expectedRootDigest !== null && wire.root_digest !== expectedRootDigest) {
    return { kind: "foreign" };
  }
  // Decode inside a catch: the snapshot is disposable derived state, and a
  // structurally corrupt file that slipped past the shape checks must read
  // as not_built — capture consults this on its way to publishing a turn,
  // and a throw there costs the turn.
  try {
    return decodeSnapshot(wire);
  } catch {
    return { kind: "not_built" };
  }
}

function decodeSnapshot(wire: SnapshotWire): OpenSnapshotResult {
  const episodes: SnapshotEpisode[] = wire.episodes.map((row) => ({
    episodeId: row[0] as string,
    digestHex: row[1] as string,
    relPath: row[2] as string,
    world: row[3] as string,
    scope: row[4] as string,
    lane: row[5] as Lane,
    harness: row[6] as string,
    sessionId: row[7] as string,
    turnId: row[8] as string,
    eventTimeMs: row[9] as number,
    captureTimeMs: row[10] as number,
    capturePolicy: row[11] as string,
    turnOutcome: row[12] as string,
    bodyLine: row[13] as number,
    contentSha256: row[14] as string,
    droppedBytes: row[15] as number,
  }));
  const byId = new Map<string, number>();
  episodes.forEach((e, i) => byId.set(e.episodeId, i));
  return {
    kind: "ok",
    snapshot: {
      rootDigest: wire.root_digest,
      signature: wire.signature,
      sourceEpisodes: wire.source_episodes,
      excluded: wire.excluded,
      episodes,
      postings: new Map(Object.entries(wire.postings)),
      byId,
    },
  };
}

export function writeSnapshot(indexPath: string, snap: Snapshot): void {
  const wire: SnapshotWire = {
    format: SNAPSHOT_FORMAT_VERSION,
    tokenizer: TOKENIZER_VERSION,
    root_digest: snap.rootDigest,
    signature: snap.signature,
    source_episodes: snap.sourceEpisodes,
    excluded: snap.excluded,
    episodes: snap.episodes.map((e) => [
      e.episodeId,
      e.digestHex,
      e.relPath,
      e.world,
      e.scope,
      e.lane,
      e.harness,
      e.sessionId,
      e.turnId,
      e.eventTimeMs,
      e.captureTimeMs,
      e.capturePolicy,
      e.turnOutcome,
      e.bodyLine,
      e.contentSha256,
      e.droppedBytes,
    ]),
    postings: Object.fromEntries(snap.postings),
  };
  const dir = path.dirname(indexPath);
  fs.mkdirSync(dir, { recursive: true, mode: 0o700 });
  const tmpPath = path.join(dir, "." + path.basename(indexPath) + ".tmp");
  const fd = fs.openSync(tmpPath, "w", 0o600);
  try {
    fs.writeFileSync(fd, JSON.stringify(wire));
    fs.fsyncSync(fd);
  } finally {
    fs.closeSync(fd);
  }
  fs.renameSync(tmpPath, indexPath);
  try {
    fs.chmodSync(indexPath, 0o600);
  } catch {
    // Hardening is best-effort on platforms without POSIX modes.
  }
}

// --- Writer lock ---------------------------------------------------------

// Stale-lock bound: a writer holding the lock this long is presumed dead
// even when its pid is unanswerable (another user's process, a recycled
// pid). Sync of the largest observed corpus is under a second, so a
// minute is generous.
const LOCK_STALE_MS = 60_000;

export class IndexLockHeldError extends Error {
  constructor() {
    super("index lock held by another writer");
    this.name = "IndexLockHeldError";
  }
}

/**
 * Serializes snapshot writers: an O_EXCL lock file beside the snapshot,
 * carrying pid and timestamp. A lock whose pid is dead or whose age
 * exceeds the stale bound is broken and retried once. wait=false throws
 * IndexLockHeldError on contention (capture's best-effort update);
 * wait=true retries briefly (sync).
 */
export function withIndexLock<T>(indexPath: string, wait: boolean, fn: () => T): T {
  const lockPath = indexPath + ".lock";
  fs.mkdirSync(path.dirname(indexPath), { recursive: true, mode: 0o700 });
  const deadline = Date.now() + (wait ? 10_000 : 0);
  for (;;) {
    let fd: number | null = null;
    try {
      fd = fs.openSync(lockPath, "wx", 0o600);
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code !== "EEXIST") throw err;
      if (breakStaleLock(lockPath)) continue;
      if (Date.now() >= deadline) throw new IndexLockHeldError();
      // Synchronous sleep: this call path is synchronous end to end, and
      // lock hold times are milliseconds.
      Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 50);
      continue;
    }
    try {
      fs.writeFileSync(fd, JSON.stringify({ pid: process.pid, time_ms: Date.now() }));
    } finally {
      fs.closeSync(fd);
    }
    try {
      return fn();
    } finally {
      try {
        fs.rmSync(lockPath, { force: true });
      } catch {
        // Best-effort: a leftover lock is broken by the stale rule.
      }
    }
  }
}

function breakStaleLock(lockPath: string): boolean {
  let pid = 0;
  let timeMs = 0;
  try {
    const parsed = JSON.parse(fs.readFileSync(lockPath, "utf8")) as { pid?: number; time_ms?: number };
    pid = typeof parsed.pid === "number" ? parsed.pid : 0;
    timeMs = typeof parsed.time_ms === "number" ? parsed.time_ms : 0;
  } catch {
    // Unreadable or torn lock: age it out via mtime below.
  }
  let aged = false;
  try {
    aged = Date.now() - fs.statSync(lockPath).mtimeMs > LOCK_STALE_MS;
  } catch {
    return true; // vanished: retry the exclusive create
  }
  let pidDead = false;
  if (pid > 0) {
    try {
      process.kill(pid, 0);
    } catch (err) {
      pidDead = (err as NodeJS.ErrnoException).code === "ESRCH";
    }
  }
  const stale = pidDead || (Date.now() - Math.max(timeMs, 0) > LOCK_STALE_MS && aged);
  if (!stale) return false;
  try {
    fs.rmSync(lockPath, { force: true });
  } catch {
    return false;
  }
  return true;
}

// --- Build and sync ------------------------------------------------------

export interface SyncReport {
  indexed: number;
  unchanged: number;
  removed: number;
  skippedMalformed: number;
  duplicateIds: number;
  /**
   * Files that parse as episodes but whose recorded digest disagrees with
   * their content. They are indexed and excluded from recall, and reseal
   * is the way back: the projection stays a complete map of the corpus.
   */
  digestMismatch: number;
  /** Subtrees the walk could not read and skipped; they join excluded. */
  unreadable: number;
  /** Indexed episodes carrying oversize-truncation accounting. */
  truncated: number;
}

interface PerEpisodePostings {
  terms: Array<[string, number[]]>;
}

// perEpisodePostings inverts a snapshot's term-major postings once, so an
// incremental sync can carry unchanged episodes' postings over without
// re-tokenizing their files.
function perEpisodePostings(snap: Snapshot): Map<number, PerEpisodePostings> {
  const out = new Map<number, PerEpisodePostings>();
  for (const [term, groups] of snap.postings) {
    for (const group of groups) {
      const ord = group[0];
      let slot = out.get(ord);
      if (slot === undefined) {
        slot = { terms: [] };
        out.set(ord, slot);
      }
      slot.terms.push([term, group.slice(1)]);
    }
  }
  return out;
}

function tokenizeEpisode(content: string, ep: Episode): Array<[string, number[]]> {
  const byTerm = new Map<string, number[]>();
  let lineNo = ep.bodyLine;
  for (const line of content.slice(ep.bodyOffset).split("\n")) {
    for (const token of tokenizeLine(line)) {
      let lines = byTerm.get(token);
      if (lines === undefined) {
        byTerm.set(token, (lines = []));
      }
      // One posting per (term, line): the SQLite projection's primary key
      // deduplicated repeats within a line.
      if (lines.length === 0 || lines[lines.length - 1] !== lineNo) lines.push(lineNo);
    }
    lineNo++;
  }
  return [...byTerm.entries()];
}

function episodeRow(relPath: string, ep: Episode, contentSha256: string): SnapshotEpisode {
  return {
    episodeId: ep.episodeId,
    digestHex: ep.digestHex,
    relPath,
    world: ep.world,
    scope: ep.scope,
    lane: ep.lane,
    harness: ep.harness,
    sessionId: ep.sessionId,
    turnId: ep.turnId,
    eventTimeMs: ep.eventTimeMs,
    captureTimeMs: ep.captureTimeMs,
    capturePolicy: ep.capturePolicy,
    turnOutcome: ep.turnOutcome,
    bodyLine: ep.bodyLine,
    contentSha256,
    droppedBytes: ep.userDroppedBytes + ep.assistantDroppedBytes,
  };
}

/**
 * Rebuilds the projection from the corpus, incrementally against the old
 * snapshot: a byte-identical file (by stored content hash) keeps its row
 * and postings without re-tokenizing; new, edited, and moved files are
 * re-derived; rows whose files are gone simply do not survive into the
 * new snapshot. The whole result replaces the old file atomically under
 * the writer lock, so a torn sync never leaves a half-projection.
 *
 * Content verification runs per file and per run: the digest-mismatch
 * count reads as corpus health, so an edit that predates the previous
 * sync is still reported by this one.
 */
export function syncSnapshot(root: JournalRoot, indexPath: string, rootDigest: string): SyncReport {
  return withIndexLock(indexPath, true, () => {
    const old = openSnapshot(indexPath, null);
    const oldSnap = old.kind === "ok" && old.snapshot.rootDigest === rootDigest ? old.snapshot : null;
    const oldByPath = new Map<string, number>();
    oldSnap?.episodes.forEach((e, i) => oldByPath.set(e.relPath, i));
    const oldPostings = oldSnap === null ? null : perEpisodePostings(oldSnap);

    const report: SyncReport = {
      indexed: 0,
      unchanged: 0,
      removed: 0,
      skippedMalformed: 0,
      duplicateIds: 0,
      digestMismatch: 0,
      unreadable: 0,
      truncated: 0,
    };
    const episodes: SnapshotEpisode[] = [];
    const postings: Postings = new Map();
    const seenIds = new Set<string>();
    const addPostings = (ord: number, terms: Array<[string, number[]]>) => {
      for (const [term, lines] of terms) {
        let groups = postings.get(term);
        if (groups === undefined) {
          postings.set(term, (groups = []));
        }
        groups.push([ord, ...lines]);
      }
    };

    const h = createHash("sha256");
    let sourceEpisodes = 0;
    walkCorpus(root, (entry) => {
      if (entry.kind === "shard_dir") {
        // Permission repair is best effort: a foreign-owned entry that
        // cannot be hardened is still valid memory.
        try {
          fs.chmodSync(path.join(root.path, entry.relPath), 0o700);
        } catch {
          // Self-healing only.
        }
        return;
      }
      if (entry.kind === "unreadable_dir") {
        report.unreadable++;
        h.update(`!${entry.relPath}\n`);
        return;
      }
      sourceEpisodes++;
      try {
        fs.chmodSync(path.join(root.path, entry.relPath), 0o600);
      } catch {
        // Best effort by contract.
      }
      // The signature stats each file *after* the repair chmod, in the
      // same walk that indexes it, so the recorded signature describes
      // exactly the corpus this snapshot was built from.
      let st: fs.Stats;
      try {
        st = fs.lstatSync(path.join(root.path, entry.relPath));
      } catch {
        report.skippedMalformed++;
        return;
      }
      h.update(`${entry.relPath}\u0000${st.size}\u0000${Math.floor(st.mtimeMs)}\n`);

      let content: Buffer;
      try {
        content = readRootFile(root, entry.relPath, MAX_EPISODE_FILE_BYTES);
      } catch {
        report.skippedMalformed++;
        return;
      }
      const text = content.toString("utf8");
      // The change test is the raw bytes, not the frontmatter digest: that
      // digest is written by capture and a hand edit does not update it.
      const verdict = verifyEpisode(text);
      if (!verdict.ok && verdict.failure === "digest_mismatch") report.digestMismatch++;
      const contentSha = createHash("sha256").update(content).digest("hex");

      const oldOrd = oldByPath.get(entry.relPath);
      if (oldSnap !== null && oldOrd !== undefined && oldSnap.episodes[oldOrd].contentSha256 === contentSha) {
        const row = oldSnap.episodes[oldOrd];
        if (seenIds.has(row.episodeId)) {
          report.duplicateIds++;
          return;
        }
        seenIds.add(row.episodeId);
        const ord = episodes.length;
        episodes.push(row);
        addPostings(ord, oldPostings!.get(oldOrd)?.terms ?? []);
        report.unchanged++;
        return;
      }

      const ep = parseEpisode(text);
      if (ep === null) {
        report.skippedMalformed++;
        return;
      }
      // Deduplicate by identity: the first copy encountered stays
      // indexed, later copies are counted and skipped.
      if (seenIds.has(ep.episodeId)) {
        report.duplicateIds++;
        return;
      }
      seenIds.add(ep.episodeId);
      const ord = episodes.length;
      episodes.push(episodeRow(entry.relPath, ep, contentSha));
      addPostings(ord, tokenizeEpisode(text, ep));
      report.indexed++;
    });

    report.removed = oldSnap === null ? 0 : oldSnap.episodes.filter((e) => !seenIds.has(e.episodeId)).length;
    report.truncated = episodes.reduce((n, e) => n + (e.droppedBytes > 0 ? 1 : 0), 0);

    const byId = new Map<string, number>();
    episodes.forEach((e, i) => byId.set(e.episodeId, i));
    writeSnapshot(indexPath, {
      rootDigest,
      signature: h.digest("hex"),
      sourceEpisodes,
      excluded: report.skippedMalformed + report.duplicateIds + report.unreadable,
      episodes,
      postings,
      byId,
    });
    return report;
  });
}

// --- Freshness and lookups ----------------------------------------------

export interface FreshnessResult {
  freshness: IndexFreshness;
  indexed: number;
  source: number;
  excluded: number;
}

/**
 * The one health signal, derived by every reporter from nothing else:
 * the projection is fresh exactly while the corpus re-derives the
 * signature the snapshot was built against.
 */
export function freshnessOf(snap: Snapshot, root: JournalRoot): FreshnessResult {
  const stat = corpusStatSignature(root);
  return {
    freshness: stat.signature === snap.signature ? "fresh" : "stale",
    indexed: snap.episodes.length,
    source: stat.episodes,
    excluded: snap.excluded,
  };
}

/** The projection row for one episode id, or null. */
export function lookupEpisode(snap: Snapshot, episodeId: string): SnapshotEpisode | null {
  const ord = snap.byId.get(episodeId);
  return ord === undefined ? null : snap.episodes[ord];
}

export interface WorldScope {
  world: string;
  scope: string;
}

/** Every distinct world/scope pair the projection knows, in row order. */
export function worldScopePairs(snap: Snapshot): WorldScope[] {
  const seen = new Set<string>();
  const out: WorldScope[] = [];
  for (const e of snap.episodes) {
    const key = e.world + "\u0000" + e.scope;
    if (seen.has(key)) continue;
    seen.add(key);
    out.push({ world: e.world, scope: e.scope });
  }
  return out;
}

/**
 * Capture's best-effort incremental update: under a non-blocking lock,
 * re-derive the published episode's row and postings into the loaded
 * snapshot and re-stamp the signature from a fresh stat walk. Any
 * contention or mismatch throws; the caller downgrades freshness and
 * moves on — sync is always the repair.
 *
 * Returns whether the updated snapshot is fresh. The walk signature is
 * stamped only when the walk's episode count reconciles with the
 * snapshot's rows plus exclusions: a corpus holding files this snapshot
 * never indexed (a concurrent or bypassed capture) must not inherit a
 * signature claiming coverage it does not have.
 */
export function indexEpisodeIncremental(
  root: JournalRoot,
  indexPath: string,
  rootDigest: string,
  relPath: string,
  content: string,
): boolean {
  return withIndexLock(indexPath, false, () => {
    const opened = openSnapshot(indexPath, rootDigest);
    if (opened.kind === "foreign") throw new Error("snapshot foreign");
    // An absent projection is born here, exactly as the v1 engine created
    // its empty database at first capture: without this, the corpus-wide
    // redelivery check would stay blind until the first manual sync, and
    // a cross-date redelivery could store a second copy of an identity.
    const snap: Snapshot =
      opened.kind === "ok"
        ? opened.snapshot
        : {
            rootDigest,
            signature: "",
            sourceEpisodes: 0,
            excluded: 0,
            episodes: [],
            postings: new Map(),
            byId: new Map(),
          };
    const ep = parseEpisode(content);
    if (ep === null) throw new Error("malformed episode content");

    const priorOrd = snap.byId.get(ep.episodeId);
    let rows: SnapshotEpisode[];
    let postings: Postings;
    if (priorOrd === undefined && snap.episodes.every((e) => e.relPath !== relPath)) {
      // The common case: a brand-new episode appends without touching any
      // existing group. Cloning the map (not the groups) keeps it cheap.
      rows = [...snap.episodes];
      postings = new Map(snap.postings);
    } else {
      // Replacement: rebuild both tables without the prior row.
      const per = perEpisodePostings(snap);
      rows = [];
      postings = new Map();
      snap.episodes.forEach((e, ord) => {
        if (ord === priorOrd || e.relPath === relPath) return;
        const newOrd = rows.length;
        rows.push(e);
        for (const [term, lines] of per.get(ord)?.terms ?? []) {
          let groups = postings.get(term);
          if (groups === undefined) postings.set(term, (groups = []));
          groups.push([newOrd, ...lines]);
        }
      });
    }
    const contentSha = createHash("sha256").update(content, "utf8").digest("hex");
    const ord = rows.length;
    rows.push(episodeRow(relPath, ep, contentSha));
    for (const [term, lines] of tokenizeEpisode(content, ep)) {
      const groups = postings.get(term);
      if (groups === undefined) postings.set(term, [[ord, ...lines]]);
      else postings.set(term, [...groups, [ord, ...lines]]);
    }

    const stat = corpusStatSignature(root);
    const covered = stat.episodes === rows.length + snap.excluded;
    const byId = new Map<string, number>();
    rows.forEach((e, i) => byId.set(e.episodeId, i));
    writeSnapshot(indexPath, {
      rootDigest,
      signature: covered ? stat.signature : "",
      sourceEpisodes: stat.episodes,
      excluded: snap.excluded,
      episodes: rows,
      postings,
      byId,
    });
    return covered;
  });
}
