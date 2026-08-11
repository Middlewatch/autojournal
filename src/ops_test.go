package autojournal

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStatusReportsNotBuiltForRootThatDoesNotExist(t *testing.T) {
	base := t.TempDir()
	report := StatusOf(filepath.Join(base, "absent"), filepath.Join(base, "index.sqlite"))
	if report.RootOK {
		t.Error("root_ok for a missing root")
	}
	if report.Healthy() {
		t.Error("healthy for a missing root")
	}
	if report.Freshness != IndexNotBuilt {
		t.Errorf("freshness = %q", report.Freshness)
	}
}

func TestSyncRefusesJournalRootUnderSharedDirectory(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "shared", "journals"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(base, "shared"), 0o777); err != nil {
		t.Fatal(err)
	}
	_, err := Sync(filepath.Join(base, "shared", "journals"), filepath.Join(base, "index.sqlite"))
	if !errors.Is(err, ErrSharedDirectory) {
		t.Errorf("err = %v, want ErrSharedDirectory", err)
	}
}

// The accounting rules the module exists for: sync stamps identity and
// exclusions, status reads them back without contradiction, and the
// redelivery check classifies from the stored file's own frontmatter.
func TestSyncStatusRoundTripAndRedeliveryClassification(t *testing.T) {
	base := t.TempDir()
	rootPath := filepath.Join(base, "journals")
	indexPath := filepath.Join(base, "state", "index.sqlite")

	root, err := OpenJournalRoot(rootPath)
	if err != nil {
		t.Fatalf("OpenJournalRoot: %v", err)
	}
	defer root.Close()
	payload := mustValidate(t, testPayloadJSON)
	pub := mustPublish(t, root, payload)
	second := payload
	second.TurnID = "turn-0099"
	mustPublish(t, root, second)

	report, err := Sync(rootPath, indexPath)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if report.Indexed != 2 {
		t.Errorf("indexed = %d", report.Indexed)
	}

	st := StatusOf(rootPath, indexPath)
	if !st.RootOK || st.Episodes != 2 || st.Indexed != 2 {
		t.Errorf("status = %+v", st)
	}
	if !st.Healthy() || st.Freshness != IndexFresh {
		t.Errorf("freshness = %q", st.Freshness)
	}

	// Equal row/file counts are not enough: an owner edit changes the
	// authoritative bytes and must make status stale until sync reindexes it.
	episodePath := filepath.Join(rootPath, filepath.FromSlash(pub.RelPath))
	content, err := os.ReadFile(episodePath)
	if err != nil {
		t.Fatal(err)
	}
	edited := []byte(strings.Replace(string(content), "naïve tests", "revised tests", 1))
	if bytes.Equal(content, edited) {
		t.Fatal("test edit did not change the episode")
	}
	if err := os.WriteFile(episodePath, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	if editedStatus := StatusOf(rootPath, indexPath); editedStatus.Freshness != IndexStale {
		t.Errorf("edited status freshness = %q, want stale", editedStatus.Freshness)
	}
	if _, err := Sync(rootPath, indexPath); err != nil {
		t.Fatal(err)
	}
	if repairedStatus := StatusOf(rootPath, indexPath); repairedStatus.Freshness != IndexFresh {
		t.Errorf("repaired status freshness = %q, want fresh", repairedStatus.Freshness)
	}

	digest := RootDigestHex(rootPath)
	idx, err := OpenIndex(indexPath, &digest)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer idx.Close()

	// The same payload redelivered anywhere in the corpus is a duplicate.
	dup := CheckRedelivery(root, idx, &payload)
	if dup == nil || dup.Outcome != CaptureDuplicate {
		t.Fatalf("redelivery = %+v, want duplicate", dup)
	}
	if dup.RelPath != pub.RelPath {
		t.Errorf("rel_path = %q, want %q", dup.RelPath, pub.RelPath)
	}

	// The same identity with different content at the very path this
	// payload derives is the supersede candidate: CheckRedelivery defers —
	// nil, proceed to publish — because only Publish's same-path
	// classification can rule on containment.
	altered := payload
	altered.UserContent = "rewritten after the fact"
	if red := CheckRedelivery(root, idx, &altered); red != nil {
		t.Fatalf("redelivery = %+v, want nil (supersede candidate defers to Publish)", red)
	}

	// A differing digest at a *different* path stays a conflict: a
	// changed event time shards the same identity to another date, where
	// publication would succeed instead of colliding.
	sharded := altered
	sharded.EventTimeMs += 24 * 60 * 60 * 1000
	conflict := CheckRedelivery(root, idx, &sharded)
	if conflict == nil || conflict.Outcome != CaptureConflict {
		t.Fatalf("redelivery = %+v, want conflict", conflict)
	}
	if conflict.RelPath != pub.RelPath {
		t.Errorf("conflict rel_path = %q, want %q", conflict.RelPath, pub.RelPath)
	}

	// An unknown identity is not a redelivery: the caller publishes.
	unknown := payload
	unknown.TurnID = "turn-9999"
	if red := CheckRedelivery(root, idx, &unknown); red != nil {
		t.Errorf("redelivery = %+v, want nil", red)
	}
}

func TestStatusTracksChangedAndRemovedExcludedCandidates(t *testing.T) {
	base := t.TempDir()
	rootPath := filepath.Join(base, "journals")
	indexPath := filepath.Join(base, "index.sqlite")
	root, err := OpenJournalRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	published := mustPublish(t, root, mustValidate(t, testPayloadJSON))
	root.Close()

	junkPath := filepath.Join(rootPath, "2026", "07", "28", "aj1-junk.md")
	if err := os.MkdirAll(filepath.Dir(junkPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(junkPath, []byte("not an episode"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Sync(rootPath, indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if report.SkippedMalformed != 1 {
		t.Fatalf("sync report = %+v", report)
	}
	if status := StatusOf(rootPath, indexPath); status.Freshness != IndexFresh {
		t.Fatalf("initial freshness = %q", status.Freshness)
	}

	if err := os.WriteFile(junkPath, []byte("changed malformed episode"), 0o600); err != nil {
		t.Fatal(err)
	}
	if status := StatusOf(rootPath, indexPath); status.Freshness != IndexStale {
		t.Errorf("changed malformed freshness = %q, want stale", status.Freshness)
	}
	if _, err := Sync(rootPath, indexPath); err != nil {
		t.Fatal(err)
	}
	if status := StatusOf(rootPath, indexPath); status.Freshness != IndexFresh {
		t.Errorf("resynced malformed freshness = %q, want fresh", status.Freshness)
	}

	if err := os.Remove(junkPath); err != nil {
		t.Fatal(err)
	}
	if status := StatusOf(rootPath, indexPath); status.Freshness != IndexStale {
		t.Errorf("removed malformed freshness = %q, want stale", status.Freshness)
	}
	if _, err := Sync(rootPath, indexPath); err != nil {
		t.Fatal(err)
	}
	if status := StatusOf(rootPath, indexPath); status.Freshness != IndexFresh {
		t.Errorf("removed malformed resync freshness = %q, want fresh", status.Freshness)
	}

	// A formerly indexed file that becomes readable-but-malformed is a
	// stable exclusion after sync; its content hash must survive row removal.
	episodePath := filepath.Join(rootPath, filepath.FromSlash(published.RelPath))
	if err := os.WriteFile(episodePath, []byte("now malformed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if status := StatusOf(rootPath, indexPath); status.Freshness != IndexStale {
		t.Errorf("valid-to-malformed freshness = %q, want stale", status.Freshness)
	}
	if _, err := Sync(rootPath, indexPath); err != nil {
		t.Fatal(err)
	}
	if status := StatusOf(rootPath, indexPath); status.Freshness != IndexFresh {
		t.Errorf("valid-to-malformed resync freshness = %q, want fresh", status.Freshness)
	}

	// An unreadable or over-budget candidate has no trustworthy byte hash,
	// so even a completed sync must not claim that the projection is fresh.
	oversized := bytes.Repeat([]byte("x"), int(MaxEpisodeFileBytes)+1)
	if err := os.WriteFile(episodePath, oversized, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Sync(rootPath, indexPath); err != nil {
		t.Fatal(err)
	}
	if status := StatusOf(rootPath, indexPath); status.Freshness != IndexStale {
		t.Errorf("over-budget resync freshness = %q, want stale", status.Freshness)
	}

	if err := os.WriteFile(episodePath, published.Content, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Sync(rootPath, indexPath); err != nil {
		t.Fatal(err)
	}
	if status := StatusOf(rootPath, indexPath); status.Freshness != IndexFresh {
		t.Errorf("restored episode freshness = %q, want fresh", status.Freshness)
	}
}

func TestIncrementalIndexMoveReplacesPriorPathHash(t *testing.T) {
	base := t.TempDir()
	rootPath := filepath.Join(base, "journals")
	indexPath := filepath.Join(base, "index.sqlite")
	root, err := OpenJournalRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	published := mustPublish(t, root, mustValidate(t, testPayloadJSON))
	root.Close()
	if _, err := Sync(rootPath, indexPath); err != nil {
		t.Fatal(err)
	}

	newRelPath := "moved/" + filepath.Base(published.RelPath)
	newPath := filepath.Join(rootPath, filepath.FromSlash(newRelPath))
	if err := os.MkdirAll(filepath.Dir(newPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(
		filepath.Join(rootPath, filepath.FromSlash(published.RelPath)), newPath,
	); err != nil {
		t.Fatal(err)
	}

	digest := RootDigestHex(rootPath)
	idx, err := OpenIndex(indexPath, &digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.IndexEpisode(newRelPath, string(published.Content)); err != nil {
		idx.Close()
		t.Fatal(err)
	}
	idx.Close()
	if status := StatusOf(rootPath, indexPath); status.Freshness != IndexFresh {
		t.Errorf("incrementally moved freshness = %q, want fresh", status.Freshness)
	}
}

func TestSyncMetadataFailureRollsBackProjectionChanges(t *testing.T) {
	base := t.TempDir()
	rootPath := filepath.Join(base, "journals")
	indexPath := filepath.Join(base, "index.sqlite")
	root, err := OpenJournalRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	published := mustPublish(t, root, mustValidate(t, testPayloadJSON))
	root.Close()
	if _, err := Sync(rootPath, indexPath); err != nil {
		t.Fatal(err)
	}

	episodePath := filepath.Join(rootPath, filepath.FromSlash(published.RelPath))
	content, err := os.ReadFile(episodePath)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(content), "naïve tests", "rollbackwitness tests", 1)
	if edited == string(content) {
		t.Fatal("test edit did not change the episode")
	}
	if err := os.WriteFile(episodePath, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}

	digest := RootDigestHex(rootPath)
	idx, err := OpenIndex(indexPath, &digest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx.db.Exec(`
		CREATE TRIGGER fail_sync_excluded BEFORE INSERT ON meta
		WHEN NEW.key = 'sync_excluded'
		BEGIN SELECT RAISE(ABORT, 'injected metadata failure'); END;
	`); err != nil {
		idx.Close()
		t.Fatal(err)
	}
	idx.Close()

	if _, err := Sync(rootPath, indexPath); !errors.Is(err, ErrSyncFailed) {
		t.Fatalf("sync error = %v, want ErrSyncFailed", err)
	}
	idx, err = OpenIndex(indexPath, &digest)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := idx.PostingsForTerm(
		"rollbackwitness", "testworld", nil,
		[]Lane{LaneConversation, LaneDelegatedWork, LaneImportedLegacy},
	)
	if err != nil {
		idx.Close()
		t.Fatal(err)
	}
	if len(rows) != 0 {
		idx.Close()
		t.Fatal("failed sync left new postings visible")
	}
	if _, err := idx.db.Exec("DROP TRIGGER fail_sync_excluded;"); err != nil {
		idx.Close()
		t.Fatal(err)
	}
	idx.Close()

	if _, err := Sync(rootPath, indexPath); err != nil {
		t.Fatal(err)
	}
	idx, err = OpenIndex(indexPath, &digest)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	rows, err = idx.PostingsForTerm(
		"rollbackwitness", "testworld", nil,
		[]Lane{LaneConversation, LaneDelegatedWork, LaneImportedLegacy},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("successful retry did not index the edited body")
	}
}

func TestSyncAccountsEarlierSortedDuplicateInOneRun(t *testing.T) {
	base := t.TempDir()
	rootPath := filepath.Join(base, "journals")
	indexPath := filepath.Join(base, "index.sqlite")
	root, err := OpenJournalRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	published := mustPublish(t, root, mustValidate(t, testPayloadJSON))
	root.Close()
	if _, err := Sync(rootPath, indexPath); err != nil {
		t.Fatal(err)
	}

	// "000-copy" sorts before the date-sharded path selected by the first
	// sync, exercising new duplicate first, unchanged stored path second.
	copyPath := filepath.Join(rootPath, "000-copy", filepath.Base(published.RelPath))
	if err := os.MkdirAll(filepath.Dir(copyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copyPath, published.Content, 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Sync(rootPath, indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if report.DuplicateIDs != 1 {
		t.Fatalf("sync report = %+v, want one duplicate", report)
	}
	if status := StatusOf(rootPath, indexPath); status.Freshness != IndexFresh {
		t.Errorf("one-sync duplicate freshness = %q, want fresh", status.Freshness)
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

// TestCountEpisodesUnchangedOverFixtureCorpus holds the WalkCorpus-backed
// CountEpisodes to the pre-move implementation's value over the payload
// matrix: one file per capture vector whose outcome is published. An
// unreadable subtree stays invisible to the count, by the same contract the
// pre-move walker documented.
func TestCountEpisodesUnchangedOverFixtureCorpus(t *testing.T) {
	root, rootPath := captureFixtureCorpus(t)
	var want uint64
	for _, vec := range loadVectors(t) {
		if vec.Outcome == string(CapturePublished) {
			want++
		}
	}
	if want == 0 {
		t.Fatal("no published vectors in the matrix")
	}
	if got := CountEpisodes(root); got != want {
		t.Errorf("CountEpisodes = %d, want %d", got, want)
	}

	locked := filepath.Join(rootPath, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "aj1-invisible0000000000000000000000.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o700) })
	if got := CountEpisodes(root); got != want {
		t.Errorf("CountEpisodes with unreadable subtree = %d, want %d", got, want)
	}
}

// TestCatalogSeedsDefaultsThenIndexPairs holds Catalog to catalogCommand's
// rule: the configured default pair first, then every pair the projection
// knows, deduplicated in discovery order.
func TestCatalogSeedsDefaultsThenIndexPairs(t *testing.T) {
	root, rootPath := captureFixtureCorpus(t)
	_ = root
	indexPath := filepath.Join(filepath.Dir(rootPath), "idx.sqlite")
	if _, err := Sync(rootPath, indexPath); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	defaults := CaptureDefaults{World: "main", Scope: "default"}
	pairs := Catalog(rootPath, indexPath, defaults)
	if len(pairs) < 2 {
		t.Fatalf("pairs = %v, want defaults plus the projection's pairs", pairs)
	}
	if pairs[0] != (WorldScope{World: "main", Scope: "default"}) {
		t.Errorf("first pair = %v, want the configured default", pairs[0])
	}
	seen := map[WorldScope]int{}
	for _, p := range pairs {
		seen[p]++
		if seen[p] > 1 {
			t.Errorf("pair %v appears twice", p)
		}
	}
	// A default pair the projection also knows is not repeated.
	again := Catalog(rootPath, indexPath, CaptureDefaults{World: pairs[1].World, Scope: pairs[1].Scope})
	if again[0] != pairs[1] {
		t.Errorf("first pair = %v, want %v", again[0], pairs[1])
	}
	count := 0
	for _, p := range again {
		if p == pairs[1] {
			count++
		}
	}
	if count != 1 {
		t.Errorf("default pair known to the projection appears %d times", count)
	}
}

// TestCatalogWithoutIndexReturnsDefaultsOnly covers the missing-index and
// unopenable-index branches: both yield the default pair alone.
func TestCatalogWithoutIndexReturnsDefaultsOnly(t *testing.T) {
	base := t.TempDir()
	rootPath := filepath.Join(base, "root")
	defaults := CaptureDefaults{World: "team", Scope: "work"}

	pairs := Catalog(rootPath, filepath.Join(base, "absent.sqlite"), defaults)
	want := []WorldScope{{World: "team", Scope: "work"}}
	if len(pairs) != 1 || pairs[0] != want[0] {
		t.Errorf("missing index: pairs = %v, want %v", pairs, want)
	}

	garbage := filepath.Join(base, "garbage.sqlite")
	if err := os.WriteFile(garbage, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	pairs = Catalog(rootPath, garbage, defaults)
	if len(pairs) != 1 || pairs[0] != want[0] {
		t.Errorf("unopenable index: pairs = %v, want %v", pairs, want)
	}
}

// TestFreshnessAgreesAcrossStatusAndSearch drives one corpus through the
// four health states — fresh, stale, deliberate exclusion, unreadable
// subtree — and holds the two reporters to one answer at every step:
// both derive freshness from (*Index).Freshness and nothing else, so the
// count-based disagreement this phase closes cannot reappear.
func TestFreshnessAgreesAcrossStatusAndSearch(t *testing.T) {
	base := mustValidate(t, testPayloadJSON)
	rootPath, root := testCorpus(t)
	pubs := publishTwo(t, root, base)
	indexPath := filepath.Join(filepath.Dir(rootPath), "index.sqlite")
	digest := RootDigestHex(rootPath)
	noAliases := LoadAliasMapFromBytes([]byte("{}"))

	agree := func(step string, want IndexFreshness) {
		t.Helper()
		st := StatusOf(rootPath, indexPath)
		idx, err := OpenIndex(indexPath, &digest)
		if err != nil {
			t.Fatalf("%s: open index: %v", step, err)
		}
		defer idx.Close()
		out := Search(root, idx, noAliases, SearchRequest{
			Query: "quokka", World: "testworld",
			NowMs: uint64(time.Now().Add(time.Second).UnixMilli()),
		})
		if st.Freshness != out.Freshness {
			t.Errorf("%s: status %q disagrees with search %q", step, st.Freshness, out.Freshness)
		}
		if st.Episodes != out.Source || st.Indexed != out.Indexed {
			t.Errorf("%s: status %d/%d episodes/indexed, search %d/%d", step,
				st.Episodes, st.Indexed, out.Source, out.Indexed)
		}
		if st.Freshness != want {
			t.Errorf("%s: freshness = %q, want %q", step, st.Freshness, want)
		}
	}

	if _, err := Sync(rootPath, indexPath); err != nil {
		t.Fatal(err)
	}
	agree("fresh after sync", IndexFresh)

	// An in-place edit with the mtime moved past the recorded signature.
	victim := filepath.Join(rootPath, filepath.FromSlash(pubs[0].RelPath))
	content, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victim, append(content, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	moved := time.Now().Add(10 * time.Millisecond)
	if err := os.Chtimes(victim, moved, moved); err != nil {
		t.Fatal(err)
	}
	agree("edited episode", IndexStale)

	// A deliberate exclusion is accounted for, not staleness.
	writeCorpusFile(t, rootPath, "junkshard/aj1-junk.md", "not an episode")
	report, err := Sync(rootPath, indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if report.SkippedMalformed != 1 {
		t.Fatalf("sync = %+v, want one malformed exclusion", report)
	}
	agree("deliberate exclusion after sync", IndexFresh)

	// An unreadable subtree that repair cannot fix is not fresh anywhere.
	origRepair := repairShardDir
	repairShardDir = func(*os.Root, string) error { return errors.New("operation not permitted") }
	defer func() { repairShardDir = origRepair }()
	locked := filepath.Join(rootPath, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "aj1-hidden000000000000000000000000.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o700) })
	report, err = Sync(rootPath, indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if report.Unreadable != 1 {
		t.Fatalf("sync = %+v, want one unreadable subtree", report)
	}
	agree("unreadable subtree", IndexStale)
}

// resealFixture publishes n distinct episodes across n date shards and
// returns everything the reseal tests share. Each episode's user content
// carries a searchable marker term.
func resealFixture(t *testing.T, n int) (rootPath, indexPath string, root *os.Root, pubs []*Published) {
	t.Helper()
	base := mustValidate(t, testPayloadJSON)
	rootPath, root = testCorpus(t)
	for i := 0; i < n; i++ {
		p := base
		p.TurnID = fmt.Sprintf("turn-91%02d", i)
		p.EventTimeMs = base.EventTimeMs + uint64(i)*24*60*60*1000
		p.UserContent = fmt.Sprintf("the zorbmarker%d ledger was reconciled", i)
		p.AssistantResult = "Reconciled."
		// No tools section: a tools-bearing body is ambiguous by design
		// and reseal's first-reading choice absorbs it — that case has its
		// own test below; these fixtures re-attest unambiguously.
		p.Tools = nil
		pubs = append(pubs, mustPublish(t, root, p))
	}
	indexPath = filepath.Join(filepath.Dir(rootPath), "index.sqlite")
	if _, err := Sync(rootPath, indexPath); err != nil {
		t.Fatal(err)
	}
	return rootPath, indexPath, root, pubs
}

// editBody flips one body byte on disk, leaving the digest line untouched.
func editBody(t *testing.T, rootPath string, pub *Published, old, new string) string {
	t.Helper()
	full := filepath.Join(rootPath, filepath.FromSlash(pub.RelPath))
	b, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(b), old, new, 1)
	if edited == string(b) {
		t.Fatalf("edit %q did not apply", old)
	}
	if err := os.WriteFile(full, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	return full
}

func TestResealReattestsEditedEpisode(t *testing.T) {
	rootPath, indexPath, root, pubs := resealFixture(t, 1)
	editBody(t, rootPath, pubs[0], "Reconciled.", "Reconciled by hand.")

	report, err := Reseal(rootPath, indexPath, false)
	if err != nil {
		t.Fatalf("Reseal: %v", err)
	}
	if report.Scanned != 1 || report.Resealed != 1 || report.Refused != 0 {
		t.Fatalf("report = %+v", report)
	}
	if len(report.Paths) != 1 || report.Paths[0] != pubs[0].RelPath {
		t.Errorf("paths = %v, want the edited episode", report.Paths)
	}

	content, err := os.ReadFile(filepath.Join(rootPath, filepath.FromSlash(pubs[0].RelPath)))
	if err != nil {
		t.Fatal(err)
	}
	verified, err := VerifyEpisode(string(content))
	if err != nil {
		t.Fatalf("resealed episode does not verify: %v", err)
	}
	if verified.DigestHex == pubs[0].DigestHex {
		t.Error("revision did not change: the edit was not re-attested")
	}
	if verified.AssistantResult != "Reconciled by hand." {
		t.Errorf("re-attested reading = %q, want the edited content", verified.AssistantResult)
	}

	digest := RootDigestHex(rootPath)
	idx, err := OpenIndex(indexPath, &digest)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	out := Search(root, idx, LoadAliasMapFromBytes([]byte("{}")), SearchRequest{
		Query: "zorbmarker0", World: "testworld", NowMs: 1785326400000,
	})
	found := false
	for _, h := range out.Hits {
		if h.EpisodeID == pubs[0].EpisodeID {
			found = true
		}
	}
	if !found || out.EditedExcluded != 0 {
		t.Errorf("resealed episode not served (found=%v edited_excluded=%d)", found, out.EditedExcluded)
	}
}

func TestResealRefusesUnparseableEpisode(t *testing.T) {
	rootPath, indexPath, _, pubs := resealFixture(t, 1)
	full := filepath.Join(rootPath, filepath.FromSlash(pubs[0].RelPath))
	if err := os.WriteFile(full, []byte("no longer an episode at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Reseal(rootPath, indexPath, false)
	if err != nil {
		t.Fatalf("Reseal: %v", err)
	}
	if report.Scanned != 1 || report.Resealed != 0 || report.Refused != 1 {
		t.Fatalf("report = %+v", report)
	}
	b, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "no longer an episode at all" {
		t.Error("refused file was modified")
	}
}

func TestResealPreviewTouchesNothing(t *testing.T) {
	rootPath, indexPath, _, pubs := resealFixture(t, 2)
	full := editBody(t, rootPath, pubs[0], "Reconciled.", "Reconciled twice.")
	before, err := os.Stat(full)
	if err != nil {
		t.Fatal(err)
	}
	beforeBytes, _ := os.ReadFile(full)

	report, err := Reseal(rootPath, indexPath, true)
	if err != nil {
		t.Fatalf("Reseal preview: %v", err)
	}
	if report.Scanned != 2 || report.Resealed != 1 || report.Refused != 0 {
		t.Fatalf("report = %+v", report)
	}
	if len(report.Paths) != 1 || report.Paths[0] != pubs[0].RelPath {
		t.Errorf("paths = %v, want the one candidate", report.Paths)
	}
	after, err := os.Stat(full)
	if err != nil {
		t.Fatal(err)
	}
	afterBytes, _ := os.ReadFile(full)
	if !after.ModTime().Equal(before.ModTime()) || string(afterBytes) != string(beforeBytes) {
		t.Error("preview modified the corpus")
	}
}

func TestResealSweepsWholeCorpus(t *testing.T) {
	rootPath, indexPath, _, pubs := resealFixture(t, 3)
	for i, pub := range pubs {
		editBody(t, rootPath, pub, "Reconciled.", fmt.Sprintf("Reconciled again %d.", i))
	}
	report, err := Reseal(rootPath, indexPath, false)
	if err != nil {
		t.Fatalf("Reseal: %v", err)
	}
	if report.Scanned != 3 || report.Resealed != 3 || report.Refused != 0 {
		t.Fatalf("report = %+v", report)
	}
	for _, pub := range pubs {
		content, err := os.ReadFile(filepath.Join(rootPath, filepath.FromSlash(pub.RelPath)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyEpisode(string(content)); err != nil {
			t.Errorf("%s does not verify after the sweep: %v", pub.RelPath, err)
		}
	}
	if st := StatusOf(rootPath, indexPath); st.Freshness != IndexFresh {
		t.Errorf("post-reseal freshness = %q, want fresh (rebaselined)", st.Freshness)
	}
}

func TestResealChoosesFirstValidReadingOnAmbiguousBody(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(payloadsDir, "body-spoof.json"))
	if err != nil {
		t.Fatal(err)
	}
	rp, err := ParsePayload(raw)
	if err != nil {
		t.Fatal(err)
	}
	p, err := validateAsCaptureHost(t, rp)
	if err != nil {
		t.Fatal(err)
	}
	rootPath, root := testCorpus(t)
	pub := mustPublish(t, root, p)
	indexPath := filepath.Join(filepath.Dir(rootPath), "index.sqlite")
	if _, err := Sync(rootPath, indexPath); err != nil {
		t.Fatal(err)
	}

	full := filepath.Join(rootPath, filepath.FromSlash(pub.RelPath))
	b, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	// Append a body line so the digest goes stale while the tools-shaped
	// tail keeps the reading ambiguous.
	edited := string(b) + "\nappended by the owner\n"
	if err := os.WriteFile(full, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := Reseal(rootPath, indexPath, false)
	if err != nil {
		t.Fatalf("Reseal: %v", err)
	}
	if report.Resealed != 1 || report.Refused != 0 {
		t.Fatalf("report = %+v", report)
	}
	content, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := VerifyEpisode(string(content))
	if err != nil {
		t.Fatalf("resealed ambiguous episode does not verify: %v", err)
	}
	// The reading VerifyEpisode returns afterwards is the one
	// ResealDigestHex chose: recompute the chosen digest from the resealed
	// content and compare.
	chosen, ok := ResealDigestHex(string(content))
	if !ok || verified.DigestHex != chosen {
		t.Errorf("verified digest %s != reseal's chosen %s (ok=%v)", verified.DigestHex, chosen, ok)
	}
}

// TestResealRefusesDuplicatedDigestLine: the duplicated-digest-line case.
// A body edit plus a duplicated payload_digest line (a one-sed editor or
// merge artifact) must be an honest refusal — the file untouched, no
// false resealed count — because with two digest lines the record is
// ambiguous and three readers could each pick a different one.
func TestResealRefusesDuplicatedDigestLine(t *testing.T) {
	rootPath, indexPath, _, pubs := resealFixture(t, 1)
	full := filepath.Join(rootPath, filepath.FromSlash(pubs[0].RelPath))
	b, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	digestLine := "payload_digest: " + DigestPrefix + pubs[0].DigestHex
	mangled := strings.Replace(string(b), digestLine, digestLine+"\n"+digestLine, 1)
	mangled = strings.Replace(mangled, "Reconciled.", "Reconciled by hand.", 1)
	if mangled == string(b) {
		t.Fatal("mangle did not apply")
	}
	if err := os.WriteFile(full, []byte(mangled), 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := Reseal(rootPath, indexPath, false)
	if err != nil {
		t.Fatalf("Reseal: %v", err)
	}
	if report.Resealed != 0 || report.Refused != 1 || report.WriteFailures != 0 {
		t.Fatalf("report = %+v, want one honest refusal", report)
	}
	after, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != mangled {
		t.Error("refused ambiguous file was modified")
	}
	// And a rerun repeats the refusal, not a false success.
	again, err := Reseal(rootPath, indexPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if again.Resealed != 0 || again.Refused != 1 {
		t.Errorf("rerun report = %+v", again)
	}
}

// TestResealSurvivesStaleTemp: a temp file left by a crashed reseal must
// not hard-fail every future reseal on the exclusive create.
func TestResealSurvivesStaleTemp(t *testing.T) {
	rootPath, indexPath, _, pubs := resealFixture(t, 1)
	editBody(t, rootPath, pubs[0], "Reconciled.", "Reconciled anew.")
	full := filepath.Join(rootPath, filepath.FromSlash(pubs[0].RelPath))
	stale := filepath.Join(filepath.Dir(full), "."+filepath.Base(full)+".reseal.tmp")
	if err := os.WriteFile(stale, []byte("crashed reseal leftovers"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Reseal(rootPath, indexPath, false)
	if err != nil {
		t.Fatalf("Reseal over a stale temp: %v", err)
	}
	if report.Resealed != 1 || report.WriteFailures != 0 {
		t.Fatalf("report = %+v", report)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("stale temp survived the reseal")
	}
}

// TestResealContinuesPastWriteFailure: one unwritable shard costs one
// file, the rest of the sweep completes, and the terminal sync still
// rebaselines what did reseal.
func TestResealContinuesPastWriteFailure(t *testing.T) {
	rootPath, indexPath, _, pubs := resealFixture(t, 3)
	for i, pub := range pubs {
		editBody(t, rootPath, pub, "Reconciled.", fmt.Sprintf("Reconciled again %d.", i))
	}
	// A non-empty directory squatting on the temp name makes this one
	// file unrewritable — the stale-temp removal cannot clear it and the
	// exclusive create cannot land — without any owner-repairable
	// permission state the descent would self-heal.
	full := filepath.Join(rootPath, filepath.FromSlash(pubs[1].RelPath))
	squatter := filepath.Join(filepath.Dir(full), "."+filepath.Base(full)+".reseal.tmp")
	if err := os.MkdirAll(filepath.Join(squatter, "occupied"), 0o700); err != nil {
		t.Fatal(err)
	}

	report, err := Reseal(rootPath, indexPath, false)
	if err != nil {
		t.Fatalf("Reseal: %v", err)
	}
	if report.Resealed != 2 || report.WriteFailures != 1 || report.Refused != 0 {
		t.Fatalf("report = %+v, want the sweep to continue past the unwritable shard", report)
	}
	for i, pub := range pubs {
		if i == 1 {
			continue
		}
		content, err := os.ReadFile(filepath.Join(rootPath, filepath.FromSlash(pub.RelPath)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyEpisode(string(content)); err != nil {
			t.Errorf("episode %d not resealed despite the sweep continuing: %v", i, err)
		}
	}
}
