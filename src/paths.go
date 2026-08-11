// Where the journal and its projection live.
//
// One definition, every host. The owner CLI and an embedding host must derive
// the journal root and index path identically or they silently
// address different corpora: the host captures into a journal the CLI never
// reports, or opens a projection keyed to another root.
//
// Every path returned is absolute. The unset-versus-empty distinction in the
// environment is load-bearing and deliberate: XDG says an empty XDG_* value
// means absent, while a missing HOME is a broken environment that must fail
// loudly rather than resolve to somewhere plausible-looking.

package autojournal

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
)

// Environ looks up one environment variable, shaped like os.LookupEnv:
// the CLI passes os.LookupEnv directly and tests pass fixtures. The bool
// matters: an unset HOME is an error, while a set-but-empty XDG value is
// merely absent and falls through to its default. Collapsing "empty" and
// "unset" into one signal would lose that difference.
type Environ func(key string) (string, bool)

// ErrMissingHome is returned when a derivation needs $HOME and it is not
// set — or is set but empty, which is the same broken environment wearing
// a different shell idiom. The check matches xdgBase's treatment of
// an empty XDG value.
var ErrMissingHome = errors.New("HOME is not set")

// homeDir returns a usable $HOME, or ErrMissingHome for unset and empty
// alike: "" + "/.local/state" would resolve to a root-owned absolute path
// nobody means.
func homeDir(env Environ) (string, error) {
	home, ok := env("HOME")
	if !ok || home == "" {
		return "", ErrMissingHome
	}
	return home, nil
}

// xdgBase returns a usable XDG base directory, or false. Per the XDG Base
// Directory spec, a value that is empty *or relative* is invalid and must
// be ignored rather than resolved against the working directory — every
// path this file hands back is absolute.
func xdgBase(env Environ, key string) (string, bool) {
	value, ok := env(key)
	if !ok || value == "" || !filepath.IsAbs(value) {
		return "", false
	}
	return value, true
}

// StateDir returns `$XDG_STATE_HOME`, else `$HOME/.local/state`.
func StateDir(env Environ) (string, error) {
	if xdg, ok := xdgBase(env, "XDG_STATE_HOME"); ok {
		return xdg, nil
	}
	home, err := homeDir(env)
	if err != nil {
		return "", err
	}
	return home + "/.local/state", nil
}

// DefaultJournalRoot returns the host-neutral journal default:
// `$XDG_DATA_HOME/autojournal/journals`, else
// `$HOME/.local/share/autojournal/journals`. It applies when neither a
// command override nor the owner config names a root, and it is
// deliberately host-neutral — every harness on the machine lands in one
// corpus without configuration, which is the whole of "install and
// forget".
func DefaultJournalRoot(env Environ) (string, error) {
	if xdg, ok := xdgBase(env, "XDG_DATA_HOME"); ok {
		return xdg + "/autojournal/journals", nil
	}
	home, err := homeDir(env)
	if err != nil {
		return "", err
	}
	return home + "/.local/share/autojournal/journals", nil
}

// IndexDigestNameLen is the root-digest prefix length used to name the
// index file. Long enough that distinct roots do not collide in practice,
// short enough to stay readable in a status line.
const IndexDigestNameLen = 16

// RootDigestHex is the full SHA-256 hex of the journal root path. The
// index projection is keyed by it so distinct roots never share one. The
// path is canonicalized first, so two spellings of one root — a
// trailing slash, a doubled separator — derive one digest and therefore
// one index, whoever the caller is.
func RootDigestHex(rootPath string) string {
	sum := sha256.Sum256([]byte(ResolveJournalRoot(rootPath)))
	return hex.EncodeToString(sum[:])
}

// DefaultIndexPath returns where the index lives for a given journal
// root: outside the root (the corpus stays a clean git-trackable tree),
// keyed by the root digest.
func DefaultIndexPath(env Environ, rootPath string) (string, error) {
	state, err := StateDir(env)
	if err != nil {
		return "", err
	}
	digest := RootDigestHex(rootPath)
	return state + "/autojournal/index-" + digest[:IndexDigestNameLen] + ".sqlite", nil
}

// RootInSharedDirectory reports whether a journal root placed under
// rootPath would sit in a shared directory, which writing commands must
// refuse: other users could inject or pre-create paths there, and
// /tmp-style locations are volatile. Shared means the nearest existing
// ancestor is group- or world-writable (the sshd StrictModes rule) — the
// walk stops at the first ancestor that exists, so a not-yet-created root
// is judged by where it would actually be created.
func RootInSharedDirectory(rootPath string) bool {
	candidate := filepath.Dir(rootPath)
	for {
		info, err := os.Stat(candidate)
		// A non-directory ancestor answers nothing: the question is who else
		// can create entries alongside the root, and a plain file in the way
		// has no answer, so the walk continues upward exactly as it does for
		// a path that does not exist.
		if err != nil || !info.IsDir() {
			parent := filepath.Dir(candidate)
			if parent == candidate {
				return false // reached the filesystem root without an answer
			}
			candidate = parent
			continue
		}
		return info.Mode().Perm()&0o022 != 0
	}
}

// ThesaurusPath resolves the hand-editable thesaurus: owner config first,
// the legacy environment override second, the XDG default last. A product
// rule, not a CLI convenience: an embedding host that resolved this
// differently would silently read another owner's thesaurus.
func ThesaurusPath(env Environ, cfg Config) (string, error) {
	if cfg.ThesaurusPath != "" {
		return cfg.ThesaurusPath, nil
	}
	if p, ok := env("AUTOJOURNAL_THESAURUS"); ok && p != "" {
		return p, nil
	}
	// xdgBase, not a raw read: an empty or relative XDG_CONFIG_HOME is
	// invalid per the XDG spec and must fall through, or this function
	// would hand back a CWD-dependent path the file header forbids.
	if xdg, ok := xdgBase(env, "XDG_CONFIG_HOME"); ok {
		return xdg + "/autojournal/thesaurus.json", nil
	}
	home, err := homeDir(env)
	if err != nil {
		return "", err
	}
	return home + "/.config/autojournal/thesaurus.json", nil
}

// MissLogPath resolves the weak-query miss log: the environment override,
// else the state directory. The same product-rule reasoning as
// ThesaurusPath applies.
func MissLogPath(env Environ) (string, error) {
	if p, ok := env("AUTOJOURNAL_MISS_LOG"); ok && p != "" {
		return p, nil
	}
	state, err := StateDir(env)
	if err != nil {
		return "", err
	}
	return state + "/autojournal/thesaurus-candidates.jsonl", nil
}

// ResolveJournalRoot applies filepath.Clean to a root before anything
// derives from it, so two spellings of one root never get two indexes.
// Lexical only, deliberately: it must work for a root that does not
// exist yet, which the first-capture path needs, so EvalSymlinks is not an
// option here.
func ResolveJournalRoot(path string) string {
	return filepath.Clean(path)
}
