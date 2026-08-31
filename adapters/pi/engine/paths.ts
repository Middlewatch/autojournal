// Where the journal and its projection live.
//
// One definition, every caller. The CLI and the in-process extension must
// derive the journal root and state paths identically or they silently
// address different corpora: a capture into a journal the search never
// reports, or a projection keyed to another root.
//
// Every path returned is absolute. The unset-versus-empty distinction in
// the environment is load-bearing and deliberate: XDG says an empty XDG_*
// value means absent, while a missing HOME is a broken environment that
// must fail loudly rather than resolve to somewhere plausible-looking.

import { createHash } from "node:crypto";
import * as path from "node:path";

/**
 * Looks up one environment variable. undefined means unset; an empty
 * string means set-but-empty. The difference matters: an unset HOME is an
 * error, while a set-but-empty XDG value is merely absent and falls
 * through to its default.
 */
export type Environ = (key: string) => string | undefined;

export const processEnviron: Environ = (key) => process.env[key];

export class MissingHomeError extends Error {
  constructor() {
    super("HOME is not set");
    this.name = "MissingHomeError";
  }
}

// homeDir returns a usable home directory, or throws MissingHomeError for
// unset and empty alike: "" + "/.local/state" would resolve to a
// root-owned absolute path nobody means. USERPROFILE is the Windows
// equivalent and keeps the same rules.
export function homeDir(env: Environ): string {
  const home = env("HOME");
  if (home !== undefined && home !== "") return home;
  const profile = env("USERPROFILE");
  if (profile !== undefined && profile !== "") return profile;
  throw new MissingHomeError();
}

// xdgBase returns a usable XDG base directory, or null. Per the XDG Base
// Directory spec, a value that is empty *or relative* is invalid and must
// be ignored rather than resolved against the working directory — every
// path this module hands back is absolute.
export function xdgBase(env: Environ, key: string): string | null {
  const value = env(key);
  if (value === undefined || value === "" || !path.isAbsolute(value)) return null;
  return value;
}

/** `$XDG_STATE_HOME`, else `$HOME/.local/state`. */
export function stateDir(env: Environ): string {
  const xdg = xdgBase(env, "XDG_STATE_HOME");
  if (xdg !== null) return xdg;
  return homeDir(env) + "/.local/state";
}

/**
 * The host-neutral journal default: `$XDG_DATA_HOME/autojournal/journals`,
 * else `$HOME/.local/share/autojournal/journals`. It applies when neither
 * a command override nor the owner config names a root, and it is
 * deliberately host-neutral — every harness on the machine lands in one
 * corpus without configuration, which is the whole of "install and
 * forget".
 */
export function defaultJournalRoot(env: Environ): string {
  const xdg = xdgBase(env, "XDG_DATA_HOME");
  if (xdg !== null) return xdg + "/autojournal/journals";
  return homeDir(env) + "/.local/share/autojournal/journals";
}

/**
 * The root-digest prefix length used to name index state. Long enough that
 * distinct roots do not collide in practice, short enough to stay readable
 * in a status line.
 */
export const INDEX_DIGEST_NAME_LEN = 16;

/**
 * The full SHA-256 hex of the journal root path. The index projection is
 * keyed by it so distinct roots never share one. The path is canonicalized
 * first, so two spellings of one root — a trailing slash, a doubled
 * separator — derive one digest and therefore one index, whoever the
 * caller is.
 */
export function rootDigestHex(rootPath: string): string {
  return createHash("sha256").update(resolveJournalRoot(rootPath), "utf8").digest("hex");
}

/**
 * Where the index snapshot lives for a given journal root: outside the
 * root (the corpus stays a clean git-trackable tree), in the state
 * directory, keyed by the root digest so distinct roots never share one.
 */
export function defaultIndexPath(env: Environ, rootPath: string): string {
  const digest = rootDigestHex(rootPath);
  return stateDir(env) + "/autojournal/index-" + digest.slice(0, INDEX_DIGEST_NAME_LEN) + ".v2.json";
}

/**
 * The hand-editable thesaurus: owner config first, the legacy environment
 * override second, the XDG default last. A product rule, not a CLI
 * convenience: a caller that resolved this differently would silently read
 * another owner's thesaurus.
 */
export function thesaurusPath(env: Environ, configThesaurusPath: string): string {
  if (configThesaurusPath !== "") return configThesaurusPath;
  const override = env("AUTOJOURNAL_THESAURUS");
  if (override !== undefined && override !== "") return override;
  // xdgBase, not a raw read: an empty or relative XDG_CONFIG_HOME is
  // invalid per the XDG spec and must fall through, or this function would
  // hand back a CWD-dependent path the module header forbids.
  const xdg = xdgBase(env, "XDG_CONFIG_HOME");
  if (xdg !== null) return xdg + "/autojournal/thesaurus.json";
  return homeDir(env) + "/.config/autojournal/thesaurus.json";
}

/**
 * The weak-query miss log: the environment override, else the state
 * directory. The same product-rule reasoning as thesaurusPath applies.
 */
export function missLogPath(env: Environ): string {
  const override = env("AUTOJOURNAL_MISS_LOG");
  if (override !== undefined && override !== "") return override;
  return stateDir(env) + "/autojournal/thesaurus-candidates.jsonl";
}

/**
 * Canonicalizes a root before anything derives from it, so two spellings
 * of one root never get two indexes. Lexical only, deliberately: it must
 * work for a root that does not exist yet, which the first-capture path
 * needs, so symlink resolution is not an option here.
 */
export function resolveJournalRoot(p: string): string {
  const normalized = path.normalize(p === "" ? "." : p);
  // path.normalize keeps one trailing separator; Go's filepath.Clean does
  // not, and the digest keying must not depend on that spelling.
  if (normalized.length > 1 && (normalized.endsWith("/") || normalized.endsWith(path.sep))) {
    const trimmed = normalized.slice(0, -1);
    // Never trim the separator that ends a root like "/" or "C:\".
    if (trimmed !== "" && !trimmed.endsWith(":")) return trimmed;
  }
  return normalized;
}
