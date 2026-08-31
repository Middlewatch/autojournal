// How the corpus is entered: the sharded layout below the journal root,
// the symlink-refusing owner-only descent, atomic temp-write and directory
// fsync, the containment vocabulary, and the contained readers.
//
// Two requirements drive the mechanics:
//
//   - Publication must never replace an existing episode, because
//     first-write-wins is what makes redelivery idempotent. rename always
//     replaces, so publication uses link(2), which fails atomically with
//     EEXIST when the target exists. A crash between link and unlink can
//     leave an orphan temp file; it is invisible to countEpisodes and to
//     readers, and the next publish retries a fresh temp name.
//   - Descent must never follow a symlink. Node has no openat-confined
//     root handle, so each step lstats first and refuses anything that is
//     not a real directory. The residual check-then-open race requires an
//     attacker who already holds write access inside the owner-only
//     corpus — the same residual the Go engine accepted.

import * as fs from "node:fs";
import * as path from "node:path";
import { CORPUS_WALK_DEPTH, MAX_EPISODE_FILE_BYTES, type Payload } from "./contracts.ts";
import { ID_PREFIX } from "./identity.ts";
import { resolveJournalRoot } from "./paths.ts";

// Enforced on every directory descent / episode file.
export const CORPUS_DIR_MODE = 0o700;
export const CORPUS_FILE_MODE = 0o600;

/**
 * Store failure vocabulary. The CLI maps each code to its contract
 * outcome: containment_violation means a path component inside the corpus
 * is a symlink or not a directory; permission_denied maps to the
 * permission_denied outcome; unavailable is any other I/O failure or sync
 * uncertainty — the caller may retry, idempotency makes redelivery safe.
 */
export type StoreErrorCode = "containment_violation" | "permission_denied" | "unavailable";

export class StoreError extends Error {
  readonly code: StoreErrorCode;
  constructor(code: StoreErrorCode, detail: string) {
    super(`${code}: ${detail}`);
    this.name = "StoreError";
    this.code = code;
  }
}

// corpusError classifies an I/O error into the store's failure vocabulary,
// keeping the OS detail in the message.
function corpusError(context: string, err: unknown): StoreError {
  const code = (err as NodeJS.ErrnoException).code;
  if (code === "EACCES" || code === "EPERM") {
    return new StoreError("permission_denied", `${context}: ${String(err)}`);
  }
  return new StoreError("unavailable", `${context}: ${String(err)}`);
}

/** An opened journal root: the canonicalized absolute path, hardened. */
export interface JournalRoot {
  readonly path: string;
}

/**
 * Opens the journal root for publishing, creating it if absent and
 * enforcing owner-only permissions. Intermediate directories of a freshly
 * created root keep default permissions and only the root itself is
 * hardened: the root is where episodes live, and tightening the owner's
 * unrelated parent directories on the way past would be overreach.
 */
export function openJournalRoot(rootPath: string): JournalRoot {
  const p = resolveJournalRoot(rootPath);
  let st: fs.Stats | null = null;
  try {
    st = fs.statSync(p);
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code !== "ENOENT") throw corpusError("open journal root", err);
  }
  if (st === null) {
    try {
      fs.mkdirSync(p, { recursive: true, mode: 0o755 });
    } catch (err) {
      throw corpusError("create journal root", err);
    }
  }
  try {
    fs.chmodSync(p, CORPUS_DIR_MODE);
  } catch (err) {
    throw corpusError("harden journal root", err);
  }
  return { path: p };
}

/**
 * Opens an existing journal root read-only: never creates it, never
 * changes its permissions. Throws when the root does not exist.
 */
export function openExistingRoot(rootPath: string): JournalRoot {
  const p = resolveJournalRoot(rootPath);
  const st = fs.statSync(p); // throws for the caller to classify
  if (!st.isDirectory()) throw new StoreError("unavailable", `not a directory: ${p}`);
  return { path: p };
}

const pad = (n: number, width: number): string => String(n).padStart(width, "0");

/**
 * The directory components below the journal root: reserved
 * classification directories for non-default world, scope, and lane, then
 * the YYYY/MM/DD shard from the source event time. Validate is where
 * implausible times are refused; this function trusts it.
 */
export function layoutComponents(payload: Pick<Payload, "world" | "scope" | "lane" | "eventTimeMs">): string[] {
  const components: string[] = [];
  if (payload.world !== "main") components.push("worlds", payload.world);
  if (payload.scope !== "default") components.push("scopes", payload.scope);
  if (payload.lane !== "conversation") components.push("lanes", payload.lane);
  const t = new Date(payload.eventTimeMs);
  components.push(pad(t.getUTCFullYear(), 4), pad(t.getUTCMonth() + 1, 2), pad(t.getUTCDate(), 2));
  return components;
}

/**
 * Descends the component list from the root, creating and hardening each
 * level, and returns the final directory's absolute path. A symlink or
 * non-directory component is a containment violation; lstat does not
 * follow links, so a planted link cannot redirect a write out of the
 * corpus. Concurrent creators are tolerated.
 */
export function descendCreating(root: JournalRoot, components: string[]): string {
  let current = root.path;
  for (const component of components) {
    const next = path.join(current, component);
    let st: fs.Stats | null = lstatOrNull(next, "inspect corpus dir " + component);
    if (st === null) {
      try {
        fs.mkdirSync(next, { mode: CORPUS_DIR_MODE });
      } catch (err) {
        if ((err as NodeJS.ErrnoException).code !== "EEXIST") {
          throw corpusError("create corpus dir " + component, err);
        }
      }
      // The new entry must be durable before anything beneath it is
      // reported durable: fsync the parent that carries it. Without this a
      // reported capture success is merely reachable-on-most-filesystems,
      // not durable.
      syncDir(current);
      st = lstatOrNull(next, "inspect corpus dir " + component);
    }
    if (st === null) throw new StoreError("unavailable", "corpus dir vanished: " + component);
    if (!st.isDirectory() || st.isSymbolicLink()) {
      throw new StoreError("containment_violation", "corpus component " + component);
    }
    try {
      fs.chmodSync(next, CORPUS_DIR_MODE);
    } catch (err) {
      throw corpusError("harden corpus dir " + component, err);
    }
    current = next;
  }
  return current;
}

function lstatOrNull(p: string, context: string): fs.Stats | null {
  try {
    return fs.lstatSync(p);
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === "ENOENT") return null;
    throw corpusError(context, err);
  }
}

/** Thrown by writeTemp when the exclusive create collides with an orphan. */
export class TempCollisionError extends Error {
  constructor() {
    super("temp name collision");
    this.name = "TempCollisionError";
  }
}

/**
 * Creates the temp file exclusively with owner-only permissions, writes
 * the content, and fsyncs. A failed write removes the temp it created.
 */
export function writeTemp(dirAbs: string, tmpName: string, content: Uint8Array): void {
  const tmpPath = path.join(dirAbs, tmpName);
  let fd: number;
  try {
    fd = fs.openSync(tmpPath, "wx", CORPUS_FILE_MODE);
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === "EEXIST") throw new TempCollisionError();
    throw corpusError("create temp", err);
  }
  let ok = false;
  try {
    fs.writeFileSync(fd, content);
    fs.fsyncSync(fd);
    ok = true;
  } catch (err) {
    throw corpusError("write temp", err);
  } finally {
    fs.closeSync(fd);
    if (!ok) {
      try {
        fs.rmSync(tmpPath, { force: true });
      } catch {
        // Removal is best-effort; the orphan is invisible to readers.
      }
    }
  }
}

/**
 * Fsyncs a directory, making the entry changes inside it durable. Windows
 * cannot open a directory for fsync; publication there degrades to
 * write-then-rename durability without the directory-entry barrier, which
 * is the documented platform gap rather than a silent one.
 */
export function syncDir(dirAbs: string): void {
  if (process.platform === "win32") return;
  let fd: number;
  try {
    fd = fs.openSync(dirAbs, "r");
  } catch (err) {
    throw corpusError("open dir for sync", err);
  }
  try {
    fs.fsyncSync(fd);
  } catch (err) {
    throw corpusError("sync dir", err);
  } finally {
    fs.closeSync(fd);
  }
}

/**
 * Validates the journal-relative path vocabulary: relative validated
 * components only, no dot components, no Windows separators.
 */
export function containedPath(relPath: string): boolean {
  if (relPath === "" || relPath.startsWith("/")) return false;
  for (const component of relPath.split("/")) {
    if (component === "" || component === "." || component === "..") return false;
    if (component.includes("\\")) return false;
  }
  return true;
}

/** Any failure to read an episode file under containment. */
export class EvidenceUnavailableError extends Error {
  constructor(relPath: string, detail: string) {
    super(`evidence unavailable: ${relPath}: ${detail}`);
    this.name = "EvidenceUnavailableError";
  }
}

/**
 * Reads one episode file under the journal root with containment:
 * relative validated components only, no symlink following, resolution
 * stays beneath the root. Descent lstats each component (refusing
 * symlinks and non-directories) before opening, the same nofollow
 * discipline as the write path.
 */
export function readContained(root: JournalRoot, relPath: string): string {
  const fail = (detail: string): never => {
    throw new EvidenceUnavailableError(relPath, detail);
  };
  if (!containedPath(relPath)) fail("path outside the containment vocabulary");
  const components = relPath.split("/");
  let current = root.path;
  for (let i = 0; i < components.length; i++) {
    const next = path.join(current, components[i]);
    let st: fs.Stats;
    try {
      st = fs.lstatSync(next);
    } catch (err) {
      return fail(String(err));
    }
    if (i < components.length - 1) {
      if (!st.isDirectory()) fail("component is not a directory");
      current = next;
      continue;
    }
    if (!st.isFile()) fail("not a regular file");
    let content: Buffer;
    try {
      content = fs.readFileSync(next);
    } catch (err) {
      return fail(String(err));
    }
    if (content.byteLength > MAX_EPISODE_FILE_BYTES) fail("episode exceeds byte budget");
    return content.toString("utf8");
  }
  return fail("empty path"); // unreachable: containedPath guarantees a component
}

/** What one walkCorpus visit is reporting. */
export type WalkKind =
  /** A regular file whose name is an episode file name. */
  | "episode"
  /**
   * A directory the walk could not read and skipped. Counted as a
   * distinct exclusion so freshness cannot report fresh over content
   * nobody can see.
   */
  | "unreadable_dir"
  /**
   * A visible directory the walk is about to descend into, reported
   * before its entries are read. Sync repairs permissions here; counting
   * callers ignore it.
   */
  | "shard_dir";

export interface WalkEntry {
  relPath: string;
  kind: WalkKind;
  /** Size and mtime, present only for episode entries. */
  sizeBytes?: number;
  mtimeMs?: number;
}

/**
 * The single visibility rule every corpus traversal shares:
 * dot-directories are foreign tooling state and are skipped, symlinks are
 * not followed, descent stops CORPUS_WALK_DEPTH components below the
 * root, and only files named <ID_PREFIX>*.md are episodes. Entries are
 * visited in sorted name order so derived signatures are deterministic.
 * visit returning false stops the walk.
 */
export function walkCorpus(root: JournalRoot, visit: (entry: WalkEntry) => boolean | void): void {
  const walk = (dirAbs: string, relPath: string, depth: number): boolean => {
    let entries: fs.Dirent[];
    try {
      entries = fs.readdirSync(dirAbs, { withFileTypes: true });
    } catch {
      if (relPath === "") throw new StoreError("unavailable", "journal root unreadable");
      return visit({ relPath, kind: "unreadable_dir" }) !== false;
    }
    entries.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
    for (const entry of entries) {
      const name = entry.name;
      const childRel = relPath === "" ? name : relPath + "/" + name;
      if (entry.isDirectory()) {
        if (name === "" || name.startsWith(".")) continue;
        if (depth + 1 > CORPUS_WALK_DEPTH) continue;
        if (visit({ relPath: childRel, kind: "shard_dir" }) === false) return false;
        if (!walk(path.join(dirAbs, name), childRel, depth + 1)) return false;
        continue;
      }
      if (!entry.isFile()) continue; // symlinks and specials are invisible
      if (!name.startsWith(ID_PREFIX) || !name.endsWith(".md")) continue;
      let st: fs.Stats;
      try {
        st = fs.lstatSync(path.join(dirAbs, name));
      } catch {
        continue; // removed between the directory read and the stat
      }
      if (visit({ relPath: childRel, kind: "episode", sizeBytes: st.size, mtimeMs: st.mtimeMs }) === false) {
        return false;
      }
    }
    return true;
  };
  walk(root.path, "", 0);
}

/**
 * A stat-only summary of the corpus: how many episode files are visible
 * and the newest modification time among them. One lstat per entry, no
 * file reads.
 */
export interface CorpusSignature {
  episodes: number;
  maxMtimeMs: number;
}

export function corpusSignatureOf(root: JournalRoot): CorpusSignature {
  const sig: CorpusSignature = { episodes: 0, maxMtimeMs: 0 };
  walkCorpus(root, (entry) => {
    if (entry.kind !== "episode") return;
    sig.episodes++;
    const ms = Math.floor(entry.mtimeMs ?? 0);
    // A pre-1970 mtime contributes nothing to the maximum.
    if (ms > sig.maxMtimeMs) sig.maxMtimeMs = ms;
  });
  return sig;
}

/**
 * Counts authoritative-looking episode files under the journal root.
 * Diagnostics only; malformed candidates are excluded by sync, and an
 * unreadable subtree is invisible to this count by contract — a corpus
 * statistic must never be the thing that breaks recall.
 */
export function countEpisodes(root: JournalRoot): number {
  let total = 0;
  try {
    walkCorpus(root, (entry) => {
      if (entry.kind === "episode") total++;
    });
  } catch {
    return total;
  }
  return total;
}

/**
 * Whether a journal root placed under rootPath would sit in a shared
 * directory, which writing commands must refuse: other users could inject
 * or pre-create paths there, and /tmp-style locations are volatile.
 * Shared means the nearest existing ancestor is group- or world-writable
 * (the sshd StrictModes rule) — the walk stops at the first ancestor that
 * exists, so a not-yet-created root is judged by where it would actually
 * be created. Windows has no POSIX mode bits and ACLs govern instead, so
 * the check is a no-op there.
 */
export function rootInSharedDirectory(rootPath: string): boolean {
  if (process.platform === "win32") return false;
  let candidate = path.dirname(resolveJournalRoot(rootPath));
  for (;;) {
    let st: fs.Stats | null = null;
    try {
      st = fs.statSync(candidate);
    } catch {
      st = null;
    }
    // A non-directory ancestor answers nothing: the question is who else
    // can create entries alongside the root, so the walk continues upward
    // exactly as it does for a path that does not exist.
    if (st === null || !st.isDirectory()) {
      const parent = path.dirname(candidate);
      if (parent === candidate) return false; // filesystem root, no answer
      candidate = parent;
      continue;
    }
    return (st.mode & 0o022) !== 0;
  }
}

/** Reads one confined file with a byte budget; over-budget is an error. */
export function readRootFile(root: JournalRoot, relPath: string, maxBytes: number): Buffer {
  const content = fs.readFileSync(path.join(root.path, relPath));
  if (content.byteLength > maxBytes) throw new Error(`exceeds ${maxBytes} bytes`);
  return content;
}
