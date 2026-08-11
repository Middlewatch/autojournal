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

func TestPublishHardensPreexistingShardDirs(t *testing.T) {
	root, err := OpenJournalRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	// A pre-existing, group-readable corpus dir is hardened on descent,
	// every open re-hardens permissions, so a loosened mode is repaired rather
	// than trusted.
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

// TestCaptureComposesTheTransaction drives a payload with omitted world and
// scope through Capture alone and holds the result to the pinned golden
// vector: same outcome, identity, digest, path, and byte-identical episode
// content on disk.
func TestCaptureComposesTheTransaction(t *testing.T) {
	const name = "bare-no-world-scope"
	vec, ok := loadVectors(t)[name]
	if !ok || vec.Outcome != string(CapturePublished) {
		t.Fatalf("vector %s missing or not published", name)
	}
	golden, err := os.ReadFile(filepath.Join(goldenDir, "episodes", name+".md"))
	if err != nil {
		t.Fatal(err)
	}
	ep := ParseEpisode(string(golden))
	if ep == nil {
		t.Fatal("golden episode does not parse")
	}
	payloadBytes, err := os.ReadFile(filepath.Join(payloadsDir, name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := ParsePayload(payloadBytes)
	if err != nil {
		t.Fatal(err)
	}
	if raw.World != nil || raw.Scope != nil {
		t.Fatal("fixture no longer omits world/scope; the defaults-fill claim would be vacuous")
	}

	base := t.TempDir()
	res := Capture(CaptureRequest{
		RootPath:      filepath.Join(base, "root"),
		IndexPath:     filepath.Join(base, "idx.sqlite"),
		Raw:           raw,
		Defaults:      CaptureDefaults{World: "main", Scope: "default"},
		CaptureTimeMs: ep.CaptureTimeMs,
	})
	if res.Err != nil || string(res.Outcome) != vec.Outcome {
		t.Fatalf("outcome = %q (err %v), want %q", res.Outcome, res.Err, vec.Outcome)
	}
	if res.EpisodeID != *vec.EpisodeID {
		t.Errorf("episode id = %q, golden %q", res.EpisodeID, *vec.EpisodeID)
	}
	if DigestPrefix+res.DigestHex != *vec.PayloadDigest {
		t.Errorf("digest = %q, golden %q", res.DigestHex, *vec.PayloadDigest)
	}
	if res.RelPath != *vec.Path {
		t.Errorf("path = %q, golden %q", res.RelPath, *vec.Path)
	}
	if res.IndexState != IndexFresh {
		t.Errorf("index state = %q, want fresh", res.IndexState)
	}
	stored, err := os.ReadFile(filepath.Join(base, "root", filepath.FromSlash(res.RelPath)))
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != string(golden) {
		t.Error("stored episode bytes differ from the golden fixture")
	}
}

// TestCaptureFailureOrdering pins the transaction's failure order: Validate
// is reported before any filesystem consequence, and the shared-directory
// refusal is decided before the root is opened — a refused root is never
// created.
func TestCaptureFailureOrdering(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "shared"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(base, "shared"), 0o777); err != nil {
		t.Fatal(err)
	}
	sharedRoot := filepath.Join(base, "shared", "journals")

	// An invalid payload under a shared directory reports malformed:
	// Validate runs before the refusal.
	raw, err := ParsePayload([]byte(testPayloadJSON))
	if err != nil {
		t.Fatal(err)
	}
	raw.Lane = "not_a_lane"
	res := Capture(CaptureRequest{
		RootPath:      sharedRoot,
		IndexPath:     filepath.Join(base, "idx.sqlite"),
		Raw:           raw,
		CaptureTimeMs: storeCaptureTimeMs,
	})
	if res.Outcome != CaptureMalformed {
		t.Errorf("invalid payload in shared dir: outcome = %q, want malformed", res.Outcome)
	}

	// A valid payload under a shared directory is refused before the root
	// is opened: the root directory must not exist afterwards.
	raw, err = ParsePayload([]byte(testPayloadJSON))
	if err != nil {
		t.Fatal(err)
	}
	res = Capture(CaptureRequest{
		RootPath:      sharedRoot,
		IndexPath:     filepath.Join(base, "idx.sqlite"),
		Raw:           raw,
		CaptureTimeMs: storeCaptureTimeMs,
	})
	if res.Outcome != CapturePermissionDenied {
		t.Errorf("shared dir: outcome = %q, want permission_denied", res.Outcome)
	}
	if !errors.Is(res.Err, ErrSharedDirectory) {
		t.Errorf("shared dir: err = %v, want ErrSharedDirectory in the chain", res.Err)
	}
	if _, err := os.Stat(sharedRoot); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("refused root was created anyway: stat err = %v", err)
	}
}

// TestCaptureReportsIndexStaleWhenIndexingFails: an unwritable index path
// yields a success outcome with IndexState downgraded and the episode on
// disk — indexing failure never fails a capture.
func TestCaptureReportsIndexStaleWhenIndexingFails(t *testing.T) {
	base := t.TempDir()
	blocked := filepath.Join(base, "blocked")
	if err := os.Mkdir(blocked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(blocked, 0o700) })
	raw, err := ParsePayload([]byte(testPayloadJSON))
	if err != nil {
		t.Fatal(err)
	}
	res := Capture(CaptureRequest{
		RootPath:      filepath.Join(base, "root"),
		IndexPath:     filepath.Join(blocked, "sub", "idx.sqlite"),
		Raw:           raw,
		CaptureTimeMs: storeCaptureTimeMs,
	})
	if res.Outcome != CapturePublished || res.Err != nil {
		t.Fatalf("outcome = %q (err %v), want published", res.Outcome, res.Err)
	}
	if res.IndexState != IndexStale {
		t.Errorf("index state = %q, want stale", res.IndexState)
	}
	if _, err := os.Stat(filepath.Join(base, "root", filepath.FromSlash(res.RelPath))); err != nil {
		t.Errorf("published file missing: %v", err)
	}
}

// TestCaptureRefusesSharedDirectory: a group-writable parent yields
// permission_denied and writes nothing at all.
func TestCaptureRefusesSharedDirectory(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "shared"), 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(base, "shared"), 0o770); err != nil {
		t.Fatal(err)
	}
	raw, err := ParsePayload([]byte(testPayloadJSON))
	if err != nil {
		t.Fatal(err)
	}
	res := Capture(CaptureRequest{
		RootPath:      filepath.Join(base, "shared", "journals"),
		IndexPath:     filepath.Join(base, "idx.sqlite"),
		Raw:           raw,
		CaptureTimeMs: storeCaptureTimeMs,
	})
	if res.Outcome != CapturePermissionDenied {
		t.Errorf("outcome = %q, want permission_denied", res.Outcome)
	}
	if res.Detail != "PermissionDenied" {
		t.Errorf("detail = %q, want PermissionDenied", res.Detail)
	}
	entries, err := os.ReadDir(filepath.Join(base, "shared"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("refusal wrote into the shared directory: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(base, "idx.sqlite")); !errors.Is(err, fs.ErrNotExist) {
		t.Error("refusal created an index")
	}
}

// TestCaptureReportsIndexUnavailableOnForeignIndex: a projection stamped
// with another root's identity downgrades to unavailable — never stale,
// which would promise that a sync could clear it, and never a failure.
func TestCaptureReportsIndexUnavailableOnForeignIndex(t *testing.T) {
	base := t.TempDir()
	indexPath := filepath.Join(base, "idx.sqlite")
	foreign := RootDigestHex(filepath.Join(base, "some-other-root"))
	idx, err := OpenIndexHardened(indexPath, &foreign)
	if err != nil {
		t.Fatal(err)
	}
	idx.Close()

	raw, err := ParsePayload([]byte(testPayloadJSON))
	if err != nil {
		t.Fatal(err)
	}
	res := Capture(CaptureRequest{
		RootPath:      filepath.Join(base, "root"),
		IndexPath:     indexPath,
		Raw:           raw,
		CaptureTimeMs: storeCaptureTimeMs,
	})
	if res.Outcome != CapturePublished || res.Err != nil {
		t.Fatalf("outcome = %q (err %v), want published", res.Outcome, res.Err)
	}
	if res.IndexState != IndexUnavailable {
		t.Errorf("index state = %q, want unavailable", res.IndexState)
	}
}

// --- Supersede on proven containment ---

// supersedeExtensionRaw returns the shared test payload extended the way a
// settled redelivery extends it: the assistant result grown by an appended
// terminal response and the tool list grown by one name — a strict
// containment of the original on both axes.
func supersedeExtensionRaw(t *testing.T) RawPayload {
	t.Helper()
	raw, err := ParsePayload([]byte(testPayloadJSON))
	if err != nil {
		t.Fatal(err)
	}
	raw.AssistantResult += "\n\nAfter the notification settled, the migration completed."
	raw.Tools = append(raw.Tools, Tool{Name: "Write"})
	return raw
}

func captureShared(t *testing.T, base string, raw RawPayload) CaptureResult {
	t.Helper()
	return Capture(CaptureRequest{
		RootPath:      filepath.Join(base, "root"),
		IndexPath:     filepath.Join(base, "idx.sqlite"),
		Raw:           raw,
		CaptureTimeMs: storeCaptureTimeMs,
	})
}

func countEpisodeFiles(t *testing.T, rootPath string) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(rootPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".md") {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return count
}

func TestCaptureSupersedesOnStrictExtension(t *testing.T) {
	base := t.TempDir()
	raw, err := ParsePayload([]byte(testPayloadJSON))
	if err != nil {
		t.Fatal(err)
	}
	first := captureShared(t, base, raw)
	if first.Outcome != CapturePublished || first.Err != nil {
		t.Fatalf("first outcome = %q (err %v)", first.Outcome, first.Err)
	}

	res := captureShared(t, base, supersedeExtensionRaw(t))
	if res.Outcome != CaptureSuperseded || res.Err != nil {
		t.Fatalf("outcome = %q (err %v), want superseded", res.Outcome, res.Err)
	}
	if res.RelPath != first.RelPath || res.EpisodeID != first.EpisodeID {
		t.Errorf("superseded path/id moved: %q %q", res.RelPath, res.EpisodeID)
	}
	if res.DigestHex == first.DigestHex {
		t.Error("superseded digest did not change")
	}
	if res.IndexState != IndexFresh {
		t.Errorf("index state = %q, want fresh", res.IndexState)
	}

	// The file on disk carries the fuller content under the new digest;
	// the old revision's bytes exist nowhere else in the corpus.
	stored, err := os.ReadFile(filepath.Join(base, "root", filepath.FromSlash(res.RelPath)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stored), "the migration completed") {
		t.Error("stored episode does not carry the extension")
	}
	if got, ok := FrontmatterDigestHex(string(stored)); !ok || got != res.DigestHex {
		t.Errorf("stored digest = %q (%v), want %q", got, ok, res.DigestHex)
	}
	if n := countEpisodeFiles(t, filepath.Join(base, "root")); n != 1 {
		t.Errorf("episode files = %d, want exactly 1", n)
	}
}

func TestCaptureConflictsOnDivergence(t *testing.T) {
	base := t.TempDir()
	raw, err := ParsePayload([]byte(testPayloadJSON))
	if err != nil {
		t.Fatal(err)
	}
	first := captureShared(t, base, raw)
	if first.Outcome != CapturePublished {
		t.Fatalf("first outcome = %q", first.Outcome)
	}
	before, err := os.ReadFile(filepath.Join(base, "root", filepath.FromSlash(first.RelPath)))
	if err != nil {
		t.Fatal(err)
	}

	// Divergence, not extension: existing text changed.
	diverged, err := ParsePayload([]byte(testPayloadJSON))
	if err != nil {
		t.Fatal(err)
	}
	diverged.AssistantResult = strings.Replace(diverged.AssistantResult, "They pass.", "They fail.", 1)
	res := captureShared(t, base, diverged)
	if res.Outcome != CaptureConflict {
		t.Fatalf("outcome = %q, want conflict", res.Outcome)
	}

	// The first publication survives byte for byte.
	after, err := os.ReadFile(filepath.Join(base, "root", filepath.FromSlash(first.RelPath)))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("conflict changed the stored episode")
	}
}

func TestCaptureConflictsWhenStoredFileDoesNotVerify(t *testing.T) {
	base := t.TempDir()
	raw, err := ParsePayload([]byte(testPayloadJSON))
	if err != nil {
		t.Fatal(err)
	}
	first := captureShared(t, base, raw)
	if first.Outcome != CapturePublished {
		t.Fatalf("first outcome = %q", first.Outcome)
	}

	// Hand-edit the stored body with the digest line untouched: the file
	// no longer verifies, and a stored file that does not verify is never
	// superseded on any path.
	episodePath := filepath.Join(base, "root", filepath.FromSlash(first.RelPath))
	content, err := os.ReadFile(episodePath)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(content), "They pass.", "They sometimes pass.", 1)
	if edited == string(content) {
		t.Fatal("test edit did not change the episode")
	}
	if err := os.WriteFile(episodePath, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}

	res := captureShared(t, base, supersedeExtensionRaw(t))
	if res.Outcome != CaptureConflict {
		t.Fatalf("outcome = %q, want conflict", res.Outcome)
	}
	after, err := os.ReadFile(episodePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != edited {
		t.Error("conflict changed the hand-edited file")
	}
}

func TestCaptureSupersedeSurvivesTheRedeliveryCheck(t *testing.T) {
	// The reachability CheckRedelivery's supersede carve-out exists for: with a
	// healthy index that already knows the stored episode, the extension
	// must still reach Publish's same-path classification instead of
	// short-circuiting to conflict on the corpus-wide digest mismatch.
	base := t.TempDir()
	raw, err := ParsePayload([]byte(testPayloadJSON))
	if err != nil {
		t.Fatal(err)
	}
	first := captureShared(t, base, raw)
	if first.Outcome != CapturePublished || first.IndexState != IndexFresh {
		t.Fatalf("first = %q/%q, want published/fresh", first.Outcome, first.IndexState)
	}

	rootPath := filepath.Join(base, "root")
	digest := RootDigestHex(rootPath)
	idx, err := OpenIndex(filepath.Join(base, "idx.sqlite"), &digest)
	if err != nil {
		t.Fatal(err)
	}
	row, err := idx.LookupEpisode(first.EpisodeID)
	idx.Close()
	if err != nil || row == nil {
		t.Fatalf("index does not know the stored episode (row %v, err %v)", row, err)
	}

	res := captureShared(t, base, supersedeExtensionRaw(t))
	if res.Outcome != CaptureSuperseded {
		t.Fatalf("outcome = %q, want superseded", res.Outcome)
	}
}

func TestCaptureSupersedesWithNoIndex(t *testing.T) {
	// The independence claim: correcting a settled turn never depends on
	// projection health.
	base := t.TempDir()
	raw, err := ParsePayload([]byte(testPayloadJSON))
	if err != nil {
		t.Fatal(err)
	}
	first := captureShared(t, base, raw)
	if first.Outcome != CapturePublished {
		t.Fatalf("first outcome = %q", first.Outcome)
	}
	for _, sidecar := range []string{"", "-wal", "-shm", "-journal"} {
		os.Remove(filepath.Join(base, "idx.sqlite"+sidecar))
	}

	res := captureShared(t, base, supersedeExtensionRaw(t))
	if res.Outcome != CaptureSuperseded {
		t.Fatalf("outcome = %q, want superseded", res.Outcome)
	}
}

func TestCaptureConflictsWhenRedeliveryShardsElsewhere(t *testing.T) {
	base := t.TempDir()
	raw, err := ParsePayload([]byte(testPayloadJSON))
	if err != nil {
		t.Fatal(err)
	}
	first := captureShared(t, base, raw)
	if first.Outcome != CapturePublished || first.IndexState != IndexFresh {
		t.Fatalf("first = %q/%q, want published/fresh", first.Outcome, first.IndexState)
	}

	// Otherwise a strict extension — but a different event time derives a
	// different date shard, where publication would succeed rather than
	// collide: conflict, and the corpus keeps exactly one file for
	// the episode id.
	sharded := supersedeExtensionRaw(t)
	sharded.EventTimeMs += 24 * 60 * 60 * 1000
	res := captureShared(t, base, sharded)
	if res.Outcome != CaptureConflict {
		t.Fatalf("outcome = %q, want conflict", res.Outcome)
	}
	if n := countEpisodeFiles(t, filepath.Join(base, "root")); n != 1 {
		t.Errorf("episode files = %d, want exactly 1", n)
	}
}

func TestSupersedeLeavesStaleReferenceHonest(t *testing.T) {
	base := t.TempDir()
	raw, err := ParsePayload([]byte(testPayloadJSON))
	if err != nil {
		t.Fatal(err)
	}
	first := captureShared(t, base, raw)
	if first.Outcome != CapturePublished {
		t.Fatalf("first outcome = %q", first.Outcome)
	}
	res := captureShared(t, base, supersedeExtensionRaw(t))
	if res.Outcome != CaptureSuperseded {
		t.Fatalf("outcome = %q, want superseded", res.Outcome)
	}

	rootPath := filepath.Join(base, "root")
	root, err := OpenJournalRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	digest := RootDigestHex(rootPath)
	idx, err := OpenIndex(filepath.Join(base, "idx.sqlite"), &digest)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	// Evidence held against the pre-supersede revision is answered
	// stale_revision, with the current revision offered for re-request.
	stale := Get(root, idx, GetRequest{
		EpisodeID: first.EpisodeID,
		Revision:  DigestPrefix + first.DigestHex,
	})
	if stale.Outcome != OutcomeStaleRevision {
		t.Fatalf("get outcome = %q, want stale_revision", stale.Outcome)
	}
	if stale.Revision != DigestPrefix+res.DigestHex {
		t.Errorf("replacement revision = %q, want %q", stale.Revision, DigestPrefix+res.DigestHex)
	}

	current := Get(root, idx, GetRequest{
		EpisodeID: first.EpisodeID,
		Revision:  DigestPrefix + res.DigestHex,
	})
	if current.Outcome != OutcomeMatch {
		t.Errorf("current revision outcome = %q, want match", current.Outcome)
	}
}

func TestSupersedeIsAsDurableAsPublish(t *testing.T) {
	// The durability property is witnessed nowhere else outside the code:
	// the rename path must perform the same temp fsync and directory fsync
	// the link path does. Publish's two durability points are observed
	// through the package-level indirections, restored on exit.
	origWrite, origSync, origRename := publishWriteTemp, publishSyncDir, publishRename
	defer func() {
		publishWriteTemp, publishSyncDir, publishRename = origWrite, origSync, origRename
	}()

	var events []string
	publishWriteTemp = func(dir *os.Root, tmpName string, content []byte) error {
		events = append(events, "temp-write+fsync")
		return origWrite(dir, tmpName, content)
	}
	publishSyncDir = func(dir *os.Root) error {
		events = append(events, "dir-fsync")
		return origSync(dir)
	}
	publishRename = func(dir *os.Root, oldname, newname string) error {
		events = append(events, "rename")
		return origRename(dir, oldname, newname)
	}

	root, err := OpenJournalRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	p := mustValidate(t, testPayloadJSON)
	first := publishTestPayload(t, root, &p, storeCaptureTimeMs)
	if first.Outcome != CapturePublished {
		t.Fatalf("first outcome = %q", first.Outcome)
	}
	linkPath := strings.Join(events, ",")

	events = nil
	extended := p
	extended.AssistantResult += "\n\nAfter the notification settled, the migration completed."
	extended.Tools = append(append([]Tool{}, p.Tools...), Tool{Name: "Write"})
	second := publishTestPayload(t, root, &extended, storeCaptureTimeMs)
	if second.Outcome != CaptureSuperseded {
		t.Fatalf("second outcome = %q, want superseded", second.Outcome)
	}
	renamePath := strings.Join(events, ",")

	if linkPath != "temp-write+fsync,dir-fsync" {
		t.Fatalf("link path durability points = %q", linkPath)
	}
	// Ordering is the property, not just parity: the directory fsync must
	// land after the rename it makes durable, or a crash after a
	// `superseded` exit 0 can revert the episode to its pre-supersede bytes.
	if renamePath != "temp-write+fsync,rename,dir-fsync" {
		t.Errorf("rename path durability points = %q, want temp-write+fsync,rename,dir-fsync", renamePath)
	}
}

func TestCaptureConflictsOnEverySamePathAxis(t *testing.T) {
	// The axes whose divergence is reachable at the collision point — same
	// episode id, same layout path — are exactly UserContent,
	// AssistantResult, TurnOutcome and Tools. The first two are pinned by
	// the golden matrix and TestCaptureConflictsOnDivergence; these rows pin
	// the other two, so no comparison in `supersedes` can be deleted with
	// the gates staying green.
	cases := []struct {
		name    string
		perturb func(raw *RawPayload)
	}{
		{"turn_outcome_changed", func(raw *RawPayload) {
			raw.TurnOutcome = "aborted"
		}},
		{"stored_tool_renamed", func(raw *RawPayload) {
			raw.Tools = []Tool{{Name: "Bash"}, {Name: "Grep"}, {Name: "Write"}}
		}},
		{"stored_tools_reordered", func(raw *RawPayload) {
			raw.Tools = []Tool{{Name: "Read"}, {Name: "Bash"}, {Name: "Write"}}
		}},
		{"incoming_tools_shorter_than_stored", func(raw *RawPayload) {
			raw.Tools = []Tool{{Name: "Bash"}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			raw, err := ParsePayload([]byte(testPayloadJSON))
			if err != nil {
				t.Fatal(err)
			}
			if first := captureShared(t, base, raw); first.Outcome != CapturePublished {
				t.Fatalf("first outcome = %q", first.Outcome)
			}
			// Start from the strict extension, which supersedes, then
			// perturb exactly one axis: the answer must fall to conflict.
			ext := supersedeExtensionRaw(t)
			tc.perturb(&ext)
			res := captureShared(t, base, ext)
			if res.Outcome != CaptureConflict {
				t.Fatalf("outcome = %q, want conflict", res.Outcome)
			}
		})
	}
}
