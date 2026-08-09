// Where the journal and its projection live.
//
// One definition, every host. The owner CLI and an embedding host must derive
// the journal root and index path identically or they silently
// address different corpora: the host captures into a journal the CLI never
// reports, or opens a projection keyed to another root.
//
// Every path returned is absolute. Behavior is byte-parity with the Zig
// reference (src/paths.zig at tag zig-final), including its unset-vs-empty
// environment distinctions.

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
// matters — the Zig reference treats an unset HOME as an error but a
// set-but-empty XDG value as absent, so "empty" and "unset" must stay
// distinguishable.
type Environ func(key string) (string, bool)

// ErrMissingHome is returned when a derivation needs $HOME and it is not
// set. A set-but-empty HOME is used as-is, matching the Zig oracle.
var ErrMissingHome = errors.New("HOME is not set")

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
	home, ok := env("HOME")
	if !ok {
		return "", ErrMissingHome
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
	home, ok := env("HOME")
	if !ok {
		return "", ErrMissingHome
	}
	return home + "/.local/share/autojournal/journals", nil
}

// IndexDigestNameLen is the root-digest prefix length used to name the
// index file. Long enough that distinct roots do not collide in practice,
// short enough to stay readable in a status line.
const IndexDigestNameLen = 16

// RootDigestHex is the full SHA-256 hex of the journal root path. The
// index projection is keyed by it so distinct roots never share one.
func RootDigestHex(rootPath string) string {
	sum := sha256.Sum256([]byte(rootPath))
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
		// A non-directory ancestor answers nothing either: the Zig oracle
		// opens each candidate as a directory, so a file in the way sends
		// the walk upward just like a missing path does.
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
