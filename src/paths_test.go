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
