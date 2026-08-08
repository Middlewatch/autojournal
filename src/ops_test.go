package autojournal

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

	// The same identity with different content is a conflict.
	altered := payload
	altered.UserContent = "rewritten after the fact"
	conflict := CheckRedelivery(root, idx, &altered)
	if conflict == nil || conflict.Outcome != CaptureConflict {
		t.Fatalf("redelivery = %+v, want conflict", conflict)
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
