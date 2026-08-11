package autojournal

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mapEnviron builds an Environ fixture from key/value pairs.
func mapEnviron(pairs ...string) Environ {
	m := map[string]string{}
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i]] = pairs[i+1]
	}
	return func(key string) (string, bool) {
		v, ok := m[key]
		return v, ok
	}
}

func TestJournalRootPrefersXDGAndFallsBackToHome(t *testing.T) {
	root, err := DefaultJournalRoot(mapEnviron("HOME", "/home/x", "XDG_DATA_HOME", "/data"))
	if err != nil {
		t.Fatal(err)
	}
	if root != "/data/autojournal/journals" {
		t.Errorf("root = %q", root)
	}

	root, err = DefaultJournalRoot(mapEnviron("HOME", "/home/x"))
	if err != nil {
		t.Fatal(err)
	}
	if root != "/home/x/.local/share/autojournal/journals" {
		t.Errorf("root = %q", root)
	}

	// An empty XDG value is treated as unset, not as the root "/".
	root, err = DefaultJournalRoot(mapEnviron("HOME", "/home/x", "XDG_DATA_HOME", ""))
	if err != nil {
		t.Fatal(err)
	}
	if root != "/home/x/.local/share/autojournal/journals" {
		t.Errorf("empty XDG: root = %q", root)
	}

	// A relative XDG value is invalid per the spec. Resolving it against
	// the working directory would hand back a relative journal root, and
	// every consumer here opens roots as absolute paths.
	env := mapEnviron("HOME", "/home/x", "XDG_DATA_HOME", "cache/data")
	root, err = DefaultJournalRoot(env)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(root) {
		t.Errorf("relative XDG produced relative root %q", root)
	}
	indexPath, err := DefaultIndexPath(env, root)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(indexPath) {
		t.Errorf("relative XDG produced relative index path %q", indexPath)
	}

	if _, err := DefaultJournalRoot(mapEnviron()); !errors.Is(err, ErrMissingHome) {
		t.Errorf("err = %v, want ErrMissingHome", err)
	}
}

func TestIndexPathIsKeyedByRootDigestAndSitsOutsideRoot(t *testing.T) {
	env := mapEnviron("HOME", "/home/x")
	path, err := DefaultIndexPath(env, "/home/x/journals")
	if err != nil {
		t.Fatal(err)
	}
	digest := RootDigestHex("/home/x/journals")
	want := "/home/x/.local/state/autojournal/index-" + digest[:IndexDigestNameLen] + ".sqlite"
	if path != want {
		t.Errorf("index path = %q, want %q", path, want)
	}

	// Distinct roots never share a projection.
	other, err := DefaultIndexPath(env, "/home/x/other-journals")
	if err != nil {
		t.Fatal(err)
	}
	if path == other {
		t.Error("distinct roots share an index path")
	}
}

func TestSharedDirectoryGuardJudgesNearestExistingAncestor(t *testing.T) {
	base := t.TempDir()

	// A private ancestor passes, and a root that does not exist yet is
	// judged by the ancestor it would be created under.
	privateDir := filepath.Join(base, "private")
	if err := os.Mkdir(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if RootInSharedDirectory(filepath.Join(privateDir, "journals")) {
		t.Error("private ancestor judged shared")
	}

	// A group- or world-writable ancestor is refused.
	sharedDir := filepath.Join(base, "shared")
	if err := os.Mkdir(sharedDir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sharedDir, 0o777); err != nil { // defeat umask
		t.Fatal(err)
	}
	if !RootInSharedDirectory(filepath.Join(sharedDir, "journals")) {
		t.Error("world-writable ancestor judged private")
	}

	// A non-directory ancestor answers nothing: the walk continues to the
	// nearest existing *directory*.
	filePath := filepath.Join(base, "afile")
	if err := os.WriteFile(filePath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if RootInSharedDirectory(filepath.Join(filePath, "journals")) {
		t.Error("walk stopped at a file ancestor")
	}
}

func TestStateDirFallsBackToHome(t *testing.T) {
	state, err := StateDir(mapEnviron("HOME", "/home/x"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(state, "/.local/state") {
		t.Errorf("state = %q", state)
	}
	if _, err := StateDir(mapEnviron()); !errors.Is(err, ErrMissingHome) {
		t.Errorf("err = %v, want ErrMissingHome", err)
	}
}

// TestThesaurusPathPrecedence covers every branch of the resolution rule:
// owner config, the legacy environment override, XDG, HOME, and no HOME.
func TestThesaurusPathPrecedence(t *testing.T) {
	cfgWithPath := Config{ThesaurusPath: "/explicit/thesaurus.json"}

	// Owner config wins over everything.
	got, err := ThesaurusPath(mapEnviron("AUTOJOURNAL_THESAURUS", "/env/t.json", "XDG_CONFIG_HOME", "/xdg", "HOME", "/home/x"), cfgWithPath)
	if err != nil || got != "/explicit/thesaurus.json" {
		t.Errorf("config path: %q, %v", got, err)
	}
	// The environment override beats XDG and HOME.
	got, err = ThesaurusPath(mapEnviron("AUTOJOURNAL_THESAURUS", "/env/t.json", "XDG_CONFIG_HOME", "/xdg", "HOME", "/home/x"), Config{})
	if err != nil || got != "/env/t.json" {
		t.Errorf("env override: %q, %v", got, err)
	}
	// An empty override falls through.
	got, err = ThesaurusPath(mapEnviron("AUTOJOURNAL_THESAURUS", "", "XDG_CONFIG_HOME", "/xdg", "HOME", "/home/x"), Config{})
	if err != nil || got != "/xdg/autojournal/thesaurus.json" {
		t.Errorf("empty override: %q, %v", got, err)
	}
	// XDG beats the HOME default; an empty XDG falls through to HOME.
	got, err = ThesaurusPath(mapEnviron("XDG_CONFIG_HOME", "", "HOME", "/home/x"), Config{})
	if err != nil || got != "/home/x/.config/autojournal/thesaurus.json" {
		t.Errorf("home default: %q, %v", got, err)
	}
	// A relative XDG value is invalid per the spec and falls through too:
	// every path this file returns is absolute, never CWD-dependent.
	got, err = ThesaurusPath(mapEnviron("XDG_CONFIG_HOME", "relative/dir", "HOME", "/home/x"), Config{})
	if err != nil || got != "/home/x/.config/autojournal/thesaurus.json" {
		t.Errorf("relative XDG: %q, %v", got, err)
	}
	// No HOME and nothing above it is the typed error.
	if _, err := ThesaurusPath(mapEnviron(), Config{}); !errors.Is(err, ErrMissingHome) {
		t.Errorf("no HOME: err = %v, want ErrMissingHome", err)
	}
}

// TestMissLogPathPrecedence covers the override, the state-dir derivation,
// and the no-HOME error.
func TestMissLogPathPrecedence(t *testing.T) {
	got, err := MissLogPath(mapEnviron("AUTOJOURNAL_MISS_LOG", "/env/miss.jsonl", "HOME", "/home/x"))
	if err != nil || got != "/env/miss.jsonl" {
		t.Errorf("env override: %q, %v", got, err)
	}
	// An empty override falls through to the state dir.
	got, err = MissLogPath(mapEnviron("AUTOJOURNAL_MISS_LOG", "", "XDG_STATE_HOME", "/state", "HOME", "/home/x"))
	if err != nil || got != "/state/autojournal/thesaurus-candidates.jsonl" {
		t.Errorf("xdg state: %q, %v", got, err)
	}
	got, err = MissLogPath(mapEnviron("HOME", "/home/x"))
	if err != nil || got != "/home/x/.local/state/autojournal/thesaurus-candidates.jsonl" {
		t.Errorf("home state: %q, %v", got, err)
	}
	if _, err := MissLogPath(mapEnviron()); !errors.Is(err, ErrMissingHome) {
		t.Errorf("no HOME: err = %v, want ErrMissingHome", err)
	}
}

func TestEmptyHomeIsMissingHome(t *testing.T) {
	// A set-but-empty HOME is the same broken environment as an unset
	// one: "" + "/.local/..." would resolve to a root-owned absolute
	// path nobody means.
	env := func(key string) (string, bool) {
		if key == "HOME" {
			return "", true
		}
		return "", false
	}
	if _, err := StateDir(env); !errors.Is(err, ErrMissingHome) {
		t.Errorf("StateDir: err = %v, want ErrMissingHome", err)
	}
	if _, err := DefaultJournalRoot(env); !errors.Is(err, ErrMissingHome) {
		t.Errorf("DefaultJournalRoot: err = %v, want ErrMissingHome", err)
	}
	if _, err := ThesaurusPath(env, Config{}); !errors.Is(err, ErrMissingHome) {
		t.Errorf("ThesaurusPath: err = %v, want ErrMissingHome", err)
	}
	if _, err := ResolvePath(env, ""); !errors.Is(err, ErrMissingHome) {
		t.Errorf("ResolvePath: err = %v, want ErrMissingHome", err)
	}
}

func TestRootDigestIgnoresTrailingSlash(t *testing.T) {
	canonical := RootDigestHex("/home/x/journals")
	for _, spelling := range []string{
		"/home/x/journals/",
		"/home/x//journals",
		"/home/x/./journals",
		"/home/x/other/../journals",
	} {
		if got := RootDigestHex(spelling); got != canonical {
			t.Errorf("RootDigestHex(%q) differs from the canonical spelling", spelling)
		}
	}
	if RootDigestHex("/home/x/other-journals") == canonical {
		t.Error("distinct roots share a digest")
	}
	// The index filename derives from the digest, so both spellings name
	// one index file.
	env := func(key string) (string, bool) {
		if key == "XDG_STATE_HOME" {
			return "/state", true
		}
		return "", false
	}
	a, err := DefaultIndexPath(env, "/home/x/journals")
	if err != nil {
		t.Fatal(err)
	}
	b, err := DefaultIndexPath(env, "/home/x/journals/")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("index paths differ: %q vs %q", a, b)
	}
}
