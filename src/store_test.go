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

const storeCaptureTimeMs = 1785240000000

func publishTestPayload(t *testing.T, root *os.Root, p *Payload, captureTimeMs uint64) *Published {
	t.Helper()
	pub, err := Publish(root, p, captureTimeMs)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	return pub
}

func TestPublishThenRedeliver(t *testing.T) {
	root, err := OpenJournalRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	p := mustValidate(t, testPayloadJSON)

	first := publishTestPayload(t, root, &p, storeCaptureTimeMs)
	if first.Outcome != CapturePublished {
		t.Fatalf("outcome = %q", first.Outcome)
	}

	// The published file exists at the reported path, is owner-only, and
	// carries the digest.
	info, err := root.Lstat(first.RelPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("episode mode = %o", info.Mode().Perm())
	}
	onDisk, err := root.ReadFile(first.RelPath)
	if err != nil {
		t.Fatal(err)
	}
	onDiskDigest, ok := FrontmatterDigestHex(string(onDisk))
	if !ok || onDiskDigest != first.DigestHex {
		t.Errorf("on-disk digest = %q, %v", onDiskDigest, ok)
	}
	if string(onDisk) != string(first.Content) {
		t.Error("on-disk bytes differ from returned content")
	}
	// Every directory created on the way down is owner-only.
	shardInfo, err := root.Lstat(filepath.Dir(first.RelPath))
	if err != nil {
		t.Fatal(err)
	}
	if shardInfo.Mode().Perm() != 0o700 {
		t.Errorf("shard dir mode = %o", shardInfo.Mode().Perm())
	}

	// Exact redelivery (different capture time) is a duplicate success.
	second := publishTestPayload(t, root, &p, storeCaptureTimeMs+5000)
	if second.Outcome != CaptureDuplicate {
		t.Errorf("redelivery outcome = %q", second.Outcome)
	}
	if second.RelPath != first.RelPath {
		t.Errorf("redelivery path = %q, want %q", second.RelPath, first.RelPath)
	}

	// Same identity, different body: typed conflict, original untouched.
	altered := p
	altered.AssistantResult = "tampered result"
	third := publishTestPayload(t, root, &altered, storeCaptureTimeMs)
	if third.Outcome != CaptureConflict {
		t.Errorf("altered outcome = %q", third.Outcome)
	}
	after, err := root.ReadFile(first.RelPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(onDisk) {
		t.Error("conflict redelivery modified the original episode")
	}

	if got := CountEpisodes(root); got != 1 {
		t.Errorf("CountEpisodes = %d, want 1", got)
	}
}

func TestNoTempFilesRemainAfterDuplicateAndConflict(t *testing.T) {
	root, err := OpenJournalRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	p := mustValidate(t, testPayloadJSON)

	first := publishTestPayload(t, root, &p, storeCaptureTimeMs)
	publishTestPayload(t, root, &p, storeCaptureTimeMs+1)
	altered := p
	altered.UserContent = "other user text"
	publishTestPayload(t, root, &altered, storeCaptureTimeMs+2)

	shardDir, err := root.OpenRoot(filepath.Dir(first.RelPath))
	if err != nil {
		t.Fatal(err)
	}
	defer shardDir.Close()
	entries, err := fs.ReadDir(shardDir.FS(), ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("shard entries = %d, want 1", len(entries))
	}
	if !strings.HasSuffix(entries[0].Name(), ".md") {
		t.Errorf("unexpected entry %q", entries[0].Name())
	}
}

func TestCollidingTempNameRetriesAndStillClassifiesDuplicate(t *testing.T) {
	root, err := OpenJournalRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	p := mustValidate(t, testPayloadJSON)

	first := publishTestPayload(t, root, &p, storeCaptureTimeMs)
	shardDir, err := root.OpenRoot(filepath.Dir(first.RelPath))
	if err != nil {
		t.Fatal(err)
	}
	defer shardDir.Close()

	retryTime := uint64(storeCaptureTimeMs + 1)
	collision := fmt.Sprintf(".%s.%d.0.tmp", first.EpisodeID, retryTime)
	if err := shardDir.WriteFile(collision, []byte("other writer"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer shardDir.Remove(collision)

	second := publishTestPayload(t, root, &p, retryTime)
	if second.Outcome != CaptureDuplicate {
		t.Errorf("outcome = %q, want duplicate", second.Outcome)
	}
}

func TestDifferentWorldsAndTurnsPublishDistinctEpisodes(t *testing.T) {
	root, err := OpenJournalRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	p := mustValidate(t, testPayloadJSON)

	a := publishTestPayload(t, root, &p, storeCaptureTimeMs)
	nextTurn := p
	nextTurn.TurnID = "turn-0008"
	b := publishTestPayload(t, root, &nextTurn, storeCaptureTimeMs)
	otherWorld := p
	otherWorld.World = "otherworld"
	c := publishTestPayload(t, root, &otherWorld, storeCaptureTimeMs)

	for _, pub := range []*Published{a, b, c} {
		if pub.Outcome != CapturePublished {
			t.Errorf("outcome = %q", pub.Outcome)
		}
	}
	if got := CountEpisodes(root); got != 3 {
		t.Errorf("CountEpisodes = %d, want 3", got)
	}
}

func TestLayoutElidesDefaultsAndAddsClassificationDirs(t *testing.T) {
	root, err := OpenJournalRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	base := mustValidate(t, testPayloadJSON)

	def := base
	def.World = "main"
	def.Scope = "default"
	def.TurnID = "layout-default"
	a := publishTestPayload(t, root, &def, storeCaptureTimeMs)
	if filepath.Dir(a.RelPath) != "2026/07/12" {
		t.Errorf("default layout dir = %q", filepath.Dir(a.RelPath))
	}

	scoped := def
	scoped.Scope = "project:a"
	scoped.TurnID = "layout-scope"
	b := publishTestPayload(t, root, &scoped, storeCaptureTimeMs)
	if !strings.HasPrefix(b.RelPath, "scopes/project:a/2026/07/12/") {
		t.Errorf("scoped path = %q", b.RelPath)
	}

	world := def
	world.World = "isolated-work"
	world.TurnID = "layout-world"
	c := publishTestPayload(t, root, &world, storeCaptureTimeMs)
	if !strings.HasPrefix(c.RelPath, "worlds/isolated-work/2026/07/12/") {
		t.Errorf("world path = %q", c.RelPath)
	}

	lane := def
	lane.Lane = LaneEvaluation
	lane.TurnID = "layout-lane"
	d := publishTestPayload(t, root, &lane, storeCaptureTimeMs)
	if !strings.HasPrefix(d.RelPath, "lanes/evaluation/2026/07/12/") {
		t.Errorf("lane path = %q", d.RelPath)
	}

	combined := def
	combined.World = "isolated-work"
	combined.Scope = "client:a"
	combined.Lane = LaneDelegatedWork
	combined.TurnID = "layout-combined"
	e := publishTestPayload(t, root, &combined, storeCaptureTimeMs)
	wantPrefix := "worlds/isolated-work/scopes/client:a/lanes/delegated_work/2026/07/12/"
	if !strings.HasPrefix(e.RelPath, wantPrefix) {
		t.Errorf("combined path = %q", e.RelPath)
	}

	if got := CountEpisodes(root); got != 5 {
		t.Errorf("CountEpisodes = %d, want 5", got)
	}
}

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

func TestCountEpisodesSkipsDotDirsAndStrays(t *testing.T) {
	root, err := OpenJournalRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	p := mustValidate(t, testPayloadJSON)
	publishTestPayload(t, root, &p, storeCaptureTimeMs)

	// Foreign tooling state and non-episode files are invisible.
	if err := root.Mkdir(".git", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(".git/aj1-fake00000000000000000000000000.md", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile("notes.md", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := CountEpisodes(root); got != 1 {
		t.Errorf("CountEpisodes = %d, want 1", got)
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

func TestPublishHardensPreexistingShardDirs(t *testing.T) {
	root, err := OpenJournalRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	// A pre-existing, group-readable corpus dir is hardened on descent,
	// matching the reference's setPermissions on every open.
	if err := root.Mkdir("worlds", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := root.Chmod("worlds", 0o755); err != nil { // defeat umask
		t.Fatal(err)
	}
	p := mustValidate(t, testPayloadJSON)
	pub := publishTestPayload(t, root, &p, storeCaptureTimeMs)
	info, err := root.Lstat("worlds")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("worlds mode after publish = %o", info.Mode().Perm())
	}
	info, err = root.Lstat(filepath.Dir(pub.RelPath))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("shard mode after publish = %o", info.Mode().Perm())
	}
}
