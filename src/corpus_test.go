// Containment and descent mechanics: root opening and hardening,
// symlink-refusing descent, and the contained read path.

package autojournal

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSymlinkedCorpusComponentIsContainmentViolation(t *testing.T) {
	dir := t.TempDir()
	root, err := OpenJournalRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	p := mustValidate(t, testPayloadJSON)

	if err := root.Mkdir("worlds", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := root.Mkdir("elsewhere", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := root.Symlink("../elsewhere", "worlds/testworld"); err != nil {
		t.Fatal(err)
	}

	if _, err := Publish(root, &p, storeCaptureTimeMs); !errors.Is(err, ErrContainmentViolation) {
		t.Fatalf("err = %v, want ErrContainmentViolation", err)
	}
	// Nothing escaped into the link target.
	entries, err := fs.ReadDir(root.FS(), "elsewhere")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("writes escaped into symlink target: %v", entries)
	}
}

func TestOpenJournalRootCreatesAndHardens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh", "journals")
	root, err := OpenJournalRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("root mode = %o", info.Mode().Perm())
	}
	// Reopening an existing root works and keeps it hardened.
	root2, err := OpenJournalRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	root2.Close()
}

// Regression: a single-component path descends zero directories, so the
// handle being read through is still the caller's root — it must survive
// the call, whether the entry is a directory (refused) or a regular file
// (served). Before the fix, the deferred close poisoned the root and every
// later read failed.
func TestReadContainedSingleComponentPathLeavesRootOpen(t *testing.T) {
	fx := setupSearchCorpus(t)

	if _, err := ReadContained(fx.root, "worlds"); err == nil {
		t.Error("reading a directory did not fail")
	}
	if _, err := ReadContained(fx.root, fx.published[0].RelPath); err != nil {
		t.Fatalf("root unusable after single-component directory read: %v", err)
	}

	writeCorpusFile(t, fx.rootPath, "loose.md", "stray but readable")
	if content, err := ReadContained(fx.root, "loose.md"); err != nil || content != "stray but readable" {
		t.Errorf("single-component file read = %q, %v", content, err)
	}
	if _, err := ReadContained(fx.root, fx.published[0].RelPath); err != nil {
		t.Fatalf("root unusable after single-component file read: %v", err)
	}
}

// captureFixtureCorpus publishes every payload in testdata/payloads that
// validates into a fresh sharded journal root — the same matrix
// TestGoldenOpsSamples drives through the CLI. testdata/golden/episodes is a
// flat directory whose file names do not match the episode visibility rule,
// so a walk over it legitimately finds zero episodes; tests that assert
// anything "over the fixture corpus" build a real sharded one here.
func captureFixtureCorpus(t *testing.T) (*os.Root, string) {
	t.Helper()
	rootPath := filepath.Join(t.TempDir(), "root")
	root, err := OpenJournalRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	payloads, err := filepath.Glob(filepath.Join(payloadsDir, "*.json"))
	if err != nil || len(payloads) == 0 {
		t.Fatalf("payload matrix missing: %v", err)
	}
	for _, path := range payloads {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := ParsePayload(b)
		if err != nil {
			continue // the malformed fixtures are part of the matrix
		}
		p, err := validateAsCaptureHost(t, raw)
		if err != nil {
			continue
		}
		if _, err := Publish(root, &p, storeCaptureTimeMs); err != nil {
			t.Fatalf("publish %s: %v", path, err)
		}
	}
	return root, rootPath
}

// TestWalkCorpusVisibilityRules drives one constructed tree through every
// rule WalkCorpus states: dot-directory skip, depth cap, symlink non-follow,
// episode-name filter, unreadable-subtree report, and the unreadable-root
// error — asserting the exact visit sequence.
func TestWalkCorpusVisibilityRules(t *testing.T) {
	base := t.TempDir()
	rootPath := filepath.Join(base, "root")
	root, err := OpenJournalRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	mustMkdir := func(rel string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(rootPath, rel), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite := func(rel string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(rootPath, rel), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Dot directories are foreign tooling state.
	mustMkdir(".git")
	mustWrite(".git/aj1-hidden0000000000000000000000000.md")
	// Episode-name filter: only <IDPrefix>*.md regular files report.
	mustMkdir("aaa")
	mustWrite("aaa/aj1-visible000000000000000000000000.md")
	mustWrite("aaa/notes.md")
	mustWrite("aaa/aj1-wrongsuffix.txt")
	// Depth cap: "depth" is 1 component below the root, c02..c10 reach
	// CorpusWalkDepth (10); c11 is one past it and must be skipped whole.
	chain := "depth"
	for i := 2; i <= 10; i++ {
		chain = filepath.Join(chain, fmt.Sprintf("c%02d", i))
	}
	mustMkdir(chain)
	mustWrite(filepath.Join(chain, "aj1-atdepthcap00000000000000000000.md"))
	mustMkdir(filepath.Join(chain, "c11"))
	mustWrite(filepath.Join(chain, "c11", "aj1-toodeep0000000000000000000000.md"))
	// Symlinks are not followed, wherever they point.
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "aj1-linked00000000000000000000000.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(rootPath, "linked")); err != nil {
		t.Fatal(err)
	}
	// An unreadable subtree is reported as WalkUnreadableDir and skipped.
	mustMkdir("locked")
	mustWrite("locked/aj1-invisible0000000000000000000000.md")
	if err := os.Chmod(filepath.Join(rootPath, "locked"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(rootPath, "locked"), 0o700) })
	mustMkdir("zzz")
	mustWrite("zzz/aj1-last0000000000000000000000000.md")

	type seen struct {
		path    string
		kind    WalkKind
		hasInfo bool
	}
	var visits []seen
	err = WalkCorpus(root, func(relPath string, kind WalkKind, info fs.FileInfo) error {
		visits = append(visits, seen{relPath, kind, info != nil})
		return nil
	})
	if err != nil {
		t.Fatalf("WalkCorpus: %v", err)
	}
	deepEpisode := strings.ReplaceAll(filepath.Join(chain, "aj1-atdepthcap00000000000000000000.md"), string(filepath.Separator), "/")
	want := []seen{
		{"aaa", WalkShardDir, false},
		{"aaa/aj1-visible000000000000000000000000.md", WalkEpisode, true},
		{"depth", WalkShardDir, false},
	}
	// Every level of the depth chain within the cap is descended into;
	// c11 is past the cap and gets no visit at all.
	prefix := "depth"
	for i := 2; i <= 10; i++ {
		prefix = prefix + "/" + fmt.Sprintf("c%02d", i)
		want = append(want, seen{prefix, WalkShardDir, false})
	}
	want = append(want,
		seen{deepEpisode, WalkEpisode, true},
		// An unreadable directory is visited as a shard dir first — that
		// ordering is what lets sync's chmod repair self-heal it — and
		// reported unreadable only when the read still fails.
		seen{"locked", WalkShardDir, false},
		seen{"locked", WalkUnreadableDir, false},
		seen{"zzz", WalkShardDir, false},
		seen{"zzz/aj1-last0000000000000000000000000.md", WalkEpisode, true},
	)
	if len(visits) != len(want) {
		t.Fatalf("visit sequence = %+v, want %+v", visits, want)
	}
	for i := range want {
		if visits[i] != want[i] {
			t.Errorf("visit %d = %+v, want %+v", i, visits[i], want[i])
		}
	}

	// A visit error stops the walk and is returned.
	sentinel := errors.New("stop here")
	calls := 0
	err = WalkCorpus(root, func(relPath string, kind WalkKind, info fs.FileInfo) error {
		calls++
		return sentinel
	})
	if !errors.Is(err, sentinel) || calls != 1 {
		t.Errorf("visit error: err = %v after %d calls, want sentinel after 1", err, calls)
	}

	// An unreadable root is an error, not a silent zero-visit walk.
	lockedRootPath := filepath.Join(base, "lockedroot")
	lockedRoot, err := OpenJournalRoot(lockedRootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lockedRoot.Close()
	if err := os.Chmod(lockedRootPath, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(lockedRootPath, 0o700) })
	if err := WalkCorpus(lockedRoot, func(string, WalkKind, fs.FileInfo) error { return nil }); err == nil {
		t.Error("unreadable root did not return an error")
	}
}

func TestPublishFsyncsNewShardAncestors(t *testing.T) {
	// Every directory created on the way down is made durable by
	// fsyncing the parent that carries its entry, so a reported capture
	// success survives a crash rather than being reachable on most
	// filesystems by luck.
	orig := syncCreatedDir
	defer func() { syncCreatedDir = orig }()
	syncs := 0
	syncCreatedDir = func(dir *os.Root) error {
		syncs++
		return orig(dir)
	}

	root, err := OpenJournalRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	p := mustValidate(t, testPayloadJSON)

	if _, err := Publish(root, &p, 1785240000000); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	want := len(layoutComponents(&p))
	if syncs != want {
		t.Errorf("created-level syncs = %d, want one per created level (%d)", syncs, want)
	}

	// The chain already exists: nothing is created, nothing re-synced.
	syncs = 0
	second := p
	second.TurnID = "turn-0042"
	if _, err := Publish(root, &second, 1785240000000); err != nil {
		t.Fatalf("second Publish: %v", err)
	}
	if syncs != 0 {
		t.Errorf("existing-chain syncs = %d, want 0", syncs)
	}
}
