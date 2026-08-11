package autojournal

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const indexTestCaptureMs = 1785240000000

// testCorpus is one journal root plus its path on disk.
func testCorpus(t *testing.T) (rootPath string, root *os.Root) {
	t.Helper()
	rootPath = filepath.Join(t.TempDir(), "root")
	root, err := OpenJournalRoot(rootPath)
	if err != nil {
		t.Fatalf("OpenJournalRoot: %v", err)
	}
	t.Cleanup(func() { root.Close() })
	return rootPath, root
}

func mustPublish(t *testing.T, root *os.Root, p Payload) *Published {
	t.Helper()
	pub, err := Publish(root, &p, indexTestCaptureMs)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if pub.Outcome != CapturePublished {
		t.Fatalf("outcome = %q", pub.Outcome)
	}
	return pub
}

// publishTwo publishes the base payload and a second-turn variant.
func publishTwo(t *testing.T, root *os.Root, base Payload) [2]*Published {
	t.Helper()
	a := mustPublish(t, root, base)
	next := base
	next.TurnID = "turn-0099"
	b := mustPublish(t, root, next)
	return [2]*Published{a, b}
}

// publishDistinctCorpus publishes three episodes with distinct bodies:
// two conversation lanes and one evaluation lane, so document frequencies
// are non-degenerate and the evaluation exclusion is observable.
func publishDistinctCorpus(t *testing.T, root *os.Root, base Payload) [3]*Published {
	t.Helper()
	p1 := base
	p1.TurnID = "turn-1001"
	p1.UserContent = "the zebra crossed near the quokka enclosure"
	a := mustPublish(t, root, p1)
	p2 := base
	p2.TurnID = "turn-1002"
	p2.UserContent = "quokka feeding schedule and wombat burrows"
	b := mustPublish(t, root, p2)
	p3 := base
	p3.TurnID = "turn-1003"
	p3.Lane = LaneEvaluation
	p3.UserContent = "sealed zebra evaluation phrase"
	c := mustPublish(t, root, p3)
	return [3]*Published{a, b, c}
}

func openMemoryIndex(t *testing.T) *Index {
	t.Helper()
	idx, err := OpenIndex(":memory:", nil)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	t.Cleanup(func() { idx.Close() })
	return idx
}

func writeCorpusFile(t *testing.T, rootPath, rel, data string) {
	t.Helper()
	abs := filepath.Join(rootPath, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func dfOf(t *testing.T, idx *Index, world, term string) (int64, bool) {
	t.Helper()
	rows, err := idx.db.Query(
		"SELECT df FROM term_stats WHERE world = ?1 AND term = ?2;", world, term)
	if err != nil {
		t.Fatalf("dfOf: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return 0, false
	}
	var df int64
	if err := rows.Scan(&df); err != nil {
		t.Fatal(err)
	}
	return df, true
}

// snapshotRetrieval is an ordered byte snapshot of all retrieval state;
// two projections built by different paths must agree byte-for-byte.
func snapshotRetrieval(t *testing.T, idx *Index) string {
	t.Helper()
	queries := []string{
		"SELECT group_concat(term || '|' || episode_id || '|' || line_no, ';') FROM (SELECT * FROM postings ORDER BY term, episode_id, line_no);",
		"SELECT group_concat(world || '|' || term || '|' || df || '|' || eval_df, ';') FROM (SELECT * FROM term_stats ORDER BY world, term);",
		"SELECT group_concat(episode_id || '|' || digest_hex || '|' || body_line || '|' || lane, ';') FROM (SELECT * FROM episodes ORDER BY episode_id);",
	}
	var out strings.Builder
	for _, q := range queries {
		var s sql.NullString
		if err := idx.db.QueryRow(q).Scan(&s); err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		out.WriteString(s.String)
		out.WriteByte('\n')
	}
	return out.String()
}

func TestCaptureUpsertRebuildAndGoneFileRepairAgree(t *testing.T) {
	base := mustValidate(t, testPayloadJSON)
	rootPath, root := testCorpus(t)
	published := publishTwo(t, root, base)
	idx := openMemoryIndex(t)

	// Live-capture path: index one episode from its rendered content.
	if err := idx.IndexEpisode(published[0].RelPath, string(published[0].Content)); err != nil {
		t.Fatalf("IndexEpisode: %v", err)
	}
	if n, _ := idx.EpisodeCount(); n != 1 {
		t.Fatalf("count = %d, want 1", n)
	}

	// Sync discovers the second episode and skips the byte-identical first,
	// whose live-capture hash already proves its projection current.
	first, err := idx.SyncFromCorpus(root)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if first.Indexed != 1 || first.Unchanged != 1 || first.Removed != 0 || first.SkippedMalformed != 0 {
		t.Errorf("first sync = %+v", first)
	}
	if n, _ := idx.EpisodeCount(); n != 2 {
		t.Fatalf("count = %d, want 2", n)
	}

	// A malformed file is excluded and counted, never merged by filename.
	shard := filepath.Dir(published[0].RelPath)
	writeCorpusFile(t, rootPath, shard+"/aj1-junk.md", "not an episode")

	// Deleting a source file removes its row on the next sync. The one
	// surviving episode is digest-matched and skipped: Indexed counts
	// only rows this run (re)wrote, Unchanged the skips.
	if err := os.Remove(filepath.Join(rootPath, filepath.FromSlash(published[1].RelPath))); err != nil {
		t.Fatal(err)
	}
	second, err := idx.SyncFromCorpus(root)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if second.Indexed != 0 || second.Unchanged != 1 || second.Removed != 1 || second.SkippedMalformed != 1 {
		t.Errorf("second sync = %+v", second)
	}
	if n, _ := idx.EpisodeCount(); n != 1 {
		t.Fatalf("count = %d, want 1", n)
	}
}

func TestHandCorruptedStoredRowsRejectedAsCorrupt(t *testing.T) {
	base := mustValidate(t, testPayloadJSON)
	_, root := testCorpus(t)
	published := mustPublish(t, root, base)
	idx := openMemoryIndex(t)
	if err := idx.IndexEpisode(published.RelPath, string(published.Content)); err != nil {
		t.Fatal(err)
	}

	// line_no above the write-side u32 bound: the posting row is Corrupt,
	// not a panic or a silent truncation.
	if _, err := idx.db.Exec("UPDATE postings SET line_no = 5000000000;"); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.PostingsForTerm("tests", base.World, nil, []Lane{LaneConversation}); !errors.Is(err, ErrSQLiteCorrupt) {
		t.Errorf("postings err = %v, want ErrSQLiteCorrupt", err)
	}

	// A lane outside the closed enum: the episode row is Corrupt, not
	// silently defaulted to a real lane.
	if _, err := idx.db.Exec("UPDATE episodes SET lane = 'gossip';"); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.LookupEpisode(published.EpisodeID); !errors.Is(err, ErrSQLiteCorrupt) {
		t.Errorf("lookup err = %v, want ErrSQLiteCorrupt", err)
	}
}

func TestSyncKeepsOneCopyOfDuplicatedIDAndSkipsDotDirs(t *testing.T) {
	base := mustValidate(t, testPayloadJSON)
	rootPath, root := testCorpus(t)
	published := mustPublish(t, root, base)

	name := filepath.Base(published.RelPath)
	writeCorpusFile(t, rootPath, "copies/"+name, string(published.Content))
	// Foreign tooling state stays invisible even when it contains an
	// episode-shaped copy.
	writeCorpusFile(t, rootPath, ".obsidian/"+name, string(published.Content))

	idx := openMemoryIndex(t)
	report, err := idx.SyncFromCorpus(root)
	if err != nil {
		t.Fatal(err)
	}
	if report.Indexed != 1 || report.DuplicateIDs != 1 {
		t.Errorf("report = %+v", report)
	}
	if n, _ := idx.EpisodeCount(); n != 1 {
		t.Errorf("count = %d, want 1", n)
	}
	row, err := idx.LookupEpisode(published.EpisodeID)
	if err != nil || row == nil {
		t.Errorf("lookup = %v, %v", row, err)
	}
}

func TestPostingsAndDfTrackSyncLaneExclusionAndRemoval(t *testing.T) {
	base := mustValidate(t, testPayloadJSON)
	rootPath, root := testCorpus(t)
	published := publishDistinctCorpus(t, root, base)

	idx := openMemoryIndex(t)
	report, err := idx.SyncFromCorpus(root)
	if err != nil {
		t.Fatal(err)
	}
	if report.Indexed != 3 {
		t.Fatalf("indexed = %d, want 3", report.Indexed)
	}

	// Evaluation lane is excluded from document frequencies and N.
	for term, want := range map[string]int64{"zebra": 1, "quokka": 2, "wombat": 1} {
		if df, ok := dfOf(t, idx, "testworld", term); !ok || df != want {
			t.Errorf("df(%s) = %d, %v; want %d", term, df, ok, want)
		}
	}
	if n, _ := idx.StatsEpisodeCount("testworld"); n != 2 {
		t.Errorf("stats N = %d, want 2", n)
	}
	if n, _ := idx.EpisodeCount(); n != 3 {
		t.Errorf("count = %d, want 3", n)
	}

	// Default lanes never see the evaluation posting; asking for the
	// evaluation lane explicitly does.
	rows, err := idx.PostingsForTerm("zebra", "testworld", nil,
		[]Lane{LaneConversation, LaneDelegatedWork, LaneImportedLegacy})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("default-lane zebra postings = %d, want 1", len(rows))
	}
	if rows[0].Lane != LaneConversation {
		t.Errorf("lane = %q", rows[0].Lane)
	}
	if rows[0].LineNo < rows[0].BodyLine {
		t.Errorf("line %d precedes body %d", rows[0].LineNo, rows[0].BodyLine)
	}
	rows, err = idx.PostingsForTerm("zebra", "testworld", nil, []Lane{LaneEvaluation})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Lane != LaneEvaluation {
		t.Errorf("eval-lane zebra postings = %+v", rows)
	}

	// The lean discovery path (PostingPairs + EpisodeMetadata membership)
	// must agree with the joined per-term query it replaced in Search.
	pairs, err := idx.PostingPairs([]string{"zebra", "quokka"})
	if err != nil {
		t.Fatal(err)
	}
	var referenced []string
	seenIDs := map[string]struct{}{}
	for _, pair := range pairs {
		if _, dup := seenIDs[pair.EpisodeID]; !dup {
			seenIDs[pair.EpisodeID] = struct{}{}
			referenced = append(referenced, pair.EpisodeID)
		}
	}
	eligible, err := idx.EpisodeMetadata(referenced, "testworld", nil,
		[]Lane{LaneConversation, LaneDelegatedWork, LaneImportedLegacy})
	if err != nil {
		t.Fatal(err)
	}
	metaByID := map[string]PostingRow{}
	for _, ep := range eligible {
		metaByID[ep.EpisodeID] = ep
	}
	joined := map[string]int{}
	for _, term := range []string{"zebra", "quokka"} {
		termRows, err := idx.PostingsForTerm(term, "testworld", nil,
			[]Lane{LaneConversation, LaneDelegatedWork, LaneImportedLegacy})
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range termRows {
			joined[fmt.Sprintf("%s:%d", row.EpisodeID, row.LineNo)]++
		}
	}
	lean := map[string]int{}
	for _, pair := range pairs {
		meta, ok := metaByID[pair.EpisodeID]
		if !ok {
			continue // evaluation-lane posting, filtered by membership
		}
		if meta.EpisodeID != pair.EpisodeID {
			t.Fatalf("metadata identity mismatch for %s", pair.EpisodeID)
		}
		lean[fmt.Sprintf("%s:%d", pair.EpisodeID, pair.LineNo)]++
	}
	if len(lean) != len(joined) {
		t.Errorf("lean path found %d coordinates, joined path %d", len(lean), len(joined))
	}
	for key := range joined {
		if lean[key] == 0 {
			t.Errorf("joined coordinate %s missing from lean path", key)
		}
	}

	// Removing an episode file decrements its terms and drops emptied ones.
	if err := os.Remove(filepath.Join(rootPath, filepath.FromSlash(published[1].RelPath))); err != nil {
		t.Fatal(err)
	}
	second, err := idx.SyncFromCorpus(root)
	if err != nil {
		t.Fatal(err)
	}
	if second.Removed != 1 {
		t.Errorf("removed = %d, want 1", second.Removed)
	}
	if df, ok := dfOf(t, idx, "testworld", "quokka"); !ok || df != 1 {
		t.Errorf("df(quokka) = %d, %v; want 1", df, ok)
	}
	if _, ok := dfOf(t, idx, "testworld", "wombat"); ok {
		t.Error("df(wombat) survived its last episode")
	}
}

func TestSyncRebuiltProjectionMatchesIncrementallyBuilt(t *testing.T) {
	base := mustValidate(t, testPayloadJSON)
	_, root := testCorpus(t)
	published := publishDistinctCorpus(t, root, base)

	incremental := openMemoryIndex(t)
	for _, p := range published {
		if err := incremental.IndexEpisode(p.RelPath, string(p.Content)); err != nil {
			t.Fatal(err)
		}
	}
	rebuilt := openMemoryIndex(t)
	if _, err := rebuilt.SyncFromCorpus(root); err != nil {
		t.Fatal(err)
	}

	a := snapshotRetrieval(t, incremental)
	if b := snapshotRetrieval(t, rebuilt); a != b {
		t.Fatalf("rebuilt projection differs\n--- incremental ---\n%s\n--- rebuilt ---\n%s", a, b)
	}

	// Re-indexing the same content is idempotent, including df.
	if err := incremental.IndexEpisode(published[0].RelPath, string(published[0].Content)); err != nil {
		t.Fatal(err)
	}
	if c := snapshotRetrieval(t, incremental); a != c {
		t.Fatal("re-indexing the same content changed the projection")
	}
}

func TestVersionMismatchDisposesEveryTable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "index.sqlite")
	{
		raw, err := openSQLite(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		_, err = raw.Exec(`
			CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
			INSERT INTO meta VALUES ('index_schema_version', '1');
			CREATE TABLE episodes (episode_id TEXT PRIMARY KEY);
			INSERT INTO episodes VALUES ('aj1-old');
			CREATE TABLE leftover_from_the_future (x INTEGER);
		`)
		raw.Close()
		if err != nil {
			t.Fatal(err)
		}
	}

	idx, err := OpenIndex(dbPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	if n, _ := idx.EpisodeCount(); n != 0 {
		t.Errorf("count = %d, want 0", n)
	}
	var leftover int64
	if err := idx.db.QueryRow(
		"SELECT COUNT(*) FROM sqlite_master WHERE name = 'leftover_from_the_future';",
	).Scan(&leftover); err != nil {
		t.Fatal(err)
	}
	if leftover != 0 {
		t.Error("unknown leftover table survived disposal")
	}
}

func TestCurrentVersionReopenPreservesDataForeignRootRejected(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "index.sqlite")
	digestA := strings.Repeat("a", 64)
	digestB := strings.Repeat("b", 64)
	{
		idx, err := OpenIndex(dbPath, &digestA)
		if err != nil {
			t.Fatal(err)
		}
		err = idx.Upsert(EpisodeRow{
			EpisodeID: "aj1-persist", DigestHex: strings.Repeat("0", 64),
			RelPath: "worlds/x/2026/07/28/aj1-persist.md",
			World:   "x", Scope: "global", Lane: LaneConversation,
			Harness: "h", SessionID: "s", TurnID: "t",
			EventTimeMs: 1, CaptureTimeMs: 2,
			CapturePolicy: "p", TurnOutcome: "completed",
		})
		idx.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	{
		// Same root: data survives the reopen (no silent disposal).
		idx, err := OpenIndex(dbPath, &digestA)
		if err != nil {
			t.Fatal(err)
		}
		if n, _ := idx.EpisodeCount(); n != 1 {
			t.Errorf("count = %d, want 1", n)
		}
		idx.Close()
	}
	// Another root's digest is a foreign index, never an empty corpus.
	if _, err := OpenIndex(dbPath, &digestB); !errors.Is(err, ErrForeignIndex) {
		t.Errorf("err = %v, want ErrForeignIndex", err)
	}
}

func TestSyncSkipsUnchangedFilesAndTracksByteEdits(t *testing.T) {
	base := mustValidate(t, testPayloadJSON)
	rootPath, root := testCorpus(t)
	published := publishDistinctCorpus(t, root, base)

	idx := openMemoryIndex(t)
	first, err := idx.SyncFromCorpus(root)
	if err != nil {
		t.Fatal(err)
	}
	if first.Indexed != 3 || first.Unchanged != 0 {
		t.Fatalf("first sync = %+v, want 3 indexed, 0 unchanged", first)
	}

	// A no-change re-sync is a walk: every file byte-matches its stored
	// hash, so nothing is parsed or rewritten.
	second, err := idx.SyncFromCorpus(root)
	if err != nil {
		t.Fatal(err)
	}
	if second.Indexed != 0 || second.Unchanged != 3 || second.Removed != 0 {
		t.Fatalf("second sync = %+v, want 0 indexed, 3 unchanged", second)
	}

	// A body-only hand edit leaves the frontmatter payload_digest stale,
	// but the byte hash still forces reindexing — the README's
	// hand-editing promise depends on sync noticing any byte change.
	edited := bytes.Replace(published[0].Content, []byte("zebra"), []byte("aardvark"), 1)
	if bytes.Equal(edited, published[0].Content) {
		t.Fatal("edit did not change the file")
	}
	writeCorpusFile(t, rootPath, published[0].RelPath, string(edited))
	third, err := idx.SyncFromCorpus(root)
	if err != nil {
		t.Fatal(err)
	}
	if third.Indexed != 1 || third.Unchanged != 2 {
		t.Fatalf("third sync = %+v, want 1 indexed, 2 unchanged", third)
	}

	// The incrementally maintained projection equals a from-scratch
	// rebuild of the same corpus, df bookkeeping included.
	fresh := openMemoryIndex(t)
	if _, err := fresh.SyncFromCorpus(root); err != nil {
		t.Fatal(err)
	}
	if a, b := snapshotRetrieval(t, idx), snapshotRetrieval(t, fresh); a != b {
		t.Fatalf("incremental projection differs from fresh rebuild\n--- incremental ---\n%s\n--- fresh ---\n%s", a, b)
	}
}

func TestEmptyCorpusSyncEmptiesProjection(t *testing.T) {
	_, root := testCorpus(t)
	idx := openMemoryIndex(t)
	if err := idx.Upsert(EpisodeRow{
		EpisodeID: "aj1-stale", DigestHex: strings.Repeat("0", 64),
		RelPath: "worlds/x/2026/07/28/aj1-stale.md",
		World:   "x", Scope: "global", Lane: LaneConversation,
		Harness: "h", SessionID: "s", TurnID: "t",
		EventTimeMs: 1, CaptureTimeMs: 2,
		CapturePolicy: "p", TurnOutcome: "completed",
	}); err != nil {
		t.Fatal(err)
	}
	report, err := idx.SyncFromCorpus(root)
	if err != nil {
		t.Fatal(err)
	}
	if report.Indexed != 0 {
		t.Errorf("indexed = %d, want 0", report.Indexed)
	}
	if n, _ := idx.EpisodeCount(); n != 0 {
		t.Errorf("count = %d, want 0", n)
	}
}

// TestSyncCountsDigestMismatches: a hand-edited episode (body changed,
// digest line untouched) is counted digest_mismatch and stays indexed —
// the projection remains a complete map of the corpus.
func TestSyncCountsDigestMismatches(t *testing.T) {
	rootPath, root := testCorpus(t)
	p := mustValidate(t, testPayloadJSON)
	pub := mustPublish(t, root, p)

	full := filepath.Join(rootPath, filepath.FromSlash(pub.RelPath))
	b, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(b), "## Assistant\n\n", "## Assistant\n\nedited: ", 1)
	if edited == string(b) {
		t.Fatal("edit did not apply")
	}
	if err := os.WriteFile(full, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}

	idx := openMemoryIndex(t)
	report, err := idx.SyncFromCorpus(root)
	if err != nil {
		t.Fatal(err)
	}
	if report.DigestMismatch != 1 {
		t.Errorf("digest_mismatch = %d, want 1", report.DigestMismatch)
	}
	row, err := idx.LookupEpisode(pub.EpisodeID)
	if err != nil || row == nil {
		t.Errorf("digest-mismatched episode not indexed: row=%v err=%v", row, err)
	}

	// The count is corpus health, not per-run novelty: a second sync over
	// the unchanged corpus reports the same mismatch.
	report, err = idx.SyncFromCorpus(root)
	if err != nil {
		t.Fatal(err)
	}
	if report.DigestMismatch != 1 {
		t.Errorf("second sync digest_mismatch = %d, want 1", report.DigestMismatch)
	}
}

// TestSyncUnchangedOverFixtureCorpus holds the WalkCorpus-backed sync to the
// pre-conversion walker's accounting over the payload matrix, field for
// field: a first sync indexes exactly the published vectors, a second sync
// over the untouched corpus skips exactly the same files as unchanged, and
// no other counter moves in either run.
func TestSyncUnchangedOverFixtureCorpus(t *testing.T) {
	root, _ := captureFixtureCorpus(t)
	var published uint64
	for _, vec := range loadVectors(t) {
		if vec.Outcome == string(CapturePublished) {
			published++
		}
	}
	if published == 0 {
		t.Fatal("no published vectors in the matrix")
	}

	idx := openMemoryIndex(t)
	first, err := idx.SyncFromCorpus(root)
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if want := (SyncReport{Indexed: published}); first != want {
		t.Errorf("first sync = %+v, want %+v", first, want)
	}
	second, err := idx.SyncFromCorpus(root)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if want := (SyncReport{Unchanged: published}); second != want {
		t.Errorf("second sync = %+v, want %+v", second, want)
	}
}

// TestSyncCountsUnreadableSubtree: a directory sync cannot read and cannot
// repair is counted, sync still succeeds, and the count joins the exclusion
// arithmetic so freshness never reports fresh over content nobody can see.
// The repair seam stands in for a foreign-owned directory, which a
// single-uid test cannot create for real; the last section shows the
// owner-owned case self-healing instead of counting.
func TestSyncCountsUnreadableSubtree(t *testing.T) {
	base := mustValidate(t, testPayloadJSON)
	rootPath, root := testCorpus(t)
	publishTwo(t, root, base)
	indexPath := filepath.Join(filepath.Dir(rootPath), "index.sqlite")

	if _, err := Sync(rootPath, indexPath); err != nil {
		t.Fatalf("baseline sync: %v", err)
	}
	if st := StatusOf(rootPath, indexPath); st.Freshness != IndexFresh {
		t.Fatalf("baseline freshness = %q, want fresh", st.Freshness)
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

	// A foreign-owned directory cannot be hardened: the repair does not take.
	origRepair := repairShardDir
	repairShardDir = func(*os.Root, string) error { return errors.New("operation not permitted") }
	report, err := Sync(rootPath, indexPath)
	repairShardDir = origRepair
	if err != nil {
		t.Fatalf("sync over unreadable subtree failed: %v", err)
	}
	if report.Unreadable != 1 {
		t.Errorf("unreadable = %d, want 1 (report %+v)", report.Unreadable, report)
	}
	if report.Unchanged != 2 || report.Indexed != 0 || report.Removed != 0 {
		t.Errorf("visible accounting moved: %+v", report)
	}
	if st := StatusOf(rootPath, indexPath); st.Freshness == IndexFresh {
		t.Errorf("freshness = fresh over an unreadable subtree")
	}

	// Owner-owned: the real repair self-heals the directory, nothing is
	// unreadable, and the revealed junk file is excluded as malformed.
	healed, err := Sync(rootPath, indexPath)
	if err != nil {
		t.Fatalf("healing sync: %v", err)
	}
	if healed.Unreadable != 0 || healed.SkippedMalformed != 1 {
		t.Errorf("healing sync = %+v, want unreadable 0, skipped_malformed 1", healed)
	}
}

// freshnessFixture builds a synced two-episode corpus with a file-backed
// index and returns everything the freshness tests share, including a nowMs
// safely past every episode mtime so the corpus reads as settled.
func freshnessFixture(t *testing.T) (rootPath, indexPath string, root *os.Root, nowMs uint64) {
	t.Helper()
	base := mustValidate(t, testPayloadJSON)
	rootPath, root = testCorpus(t)
	publishTwo(t, root, base)
	indexPath = filepath.Join(filepath.Dir(rootPath), "index.sqlite")
	if _, err := Sync(rootPath, indexPath); err != nil {
		t.Fatalf("baseline sync: %v", err)
	}
	sig, err := CorpusSignatureOf(root)
	if err != nil {
		t.Fatal(err)
	}
	return rootPath, indexPath, root, sig.MaxMtimeMs + 1000
}

// lockEpisodeFiles makes every episode file unreadable without moving its
// mtime (chmod touches ctime only), so a stat-only signature is unchanged
// while any attempt to read content for the authoritative check would flip
// the verdict to stale. Asserting fresh afterwards proves no file was read.
func lockEpisodeFiles(t *testing.T, rootPath string, root *os.Root) {
	t.Helper()
	var locked []string
	err := WalkCorpus(root, func(path string, kind WalkKind, info fs.FileInfo) error {
		if kind == WalkEpisode {
			locked = append(locked, filepath.Join(rootPath, filepath.FromSlash(path)))
		}
		return nil
	})
	if err != nil || len(locked) == 0 {
		t.Fatalf("no episode files to lock (err %v)", err)
	}
	for _, p := range locked {
		if err := os.Chmod(p, 0); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, p := range locked {
			os.Chmod(p, 0o600)
		}
	})
}

func TestFreshnessReusesVerdictWhenSignatureUnchanged(t *testing.T) {
	rootPath, indexPath, root, nowMs := freshnessFixture(t)
	digest := RootDigestHex(rootPath)
	idx, err := OpenIndex(indexPath, &digest)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	first, err := idx.Freshness(root, nowMs)
	if err != nil {
		t.Fatalf("first Freshness: %v", err)
	}
	want := FreshnessResult{Freshness: IndexFresh, Indexed: 2, Source: 2, Excluded: 0}
	if first != want {
		t.Fatalf("first = %+v, want %+v", first, want)
	}
	if v, _ := idx.metaGet(metaFreshnessVerdict); v != string(IndexFresh) {
		t.Fatalf("memo verdict = %q, want stamped fresh", v)
	}

	lockEpisodeFiles(t, rootPath, root)
	second, err := idx.Freshness(root, nowMs+1000)
	if err != nil {
		t.Fatalf("second Freshness: %v", err)
	}
	if second != want {
		t.Errorf("second = %+v, want the reused %+v (a recompute would have read the now-unreadable files and reported stale)", second, want)
	}
}

func TestFreshnessRecomputesWhenSignatureChanges(t *testing.T) {
	rootPath, indexPath, root, nowMs := freshnessFixture(t)
	digest := RootDigestHex(rootPath)
	idx, err := OpenIndex(indexPath, &digest)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	if res, err := idx.Freshness(root, nowMs); err != nil || res.Freshness != IndexFresh {
		t.Fatalf("baseline = %+v, %v; want fresh", res, err)
	}

	// A new episode changes the count half of the signature.
	third := mustValidate(t, testPayloadJSON)
	third.TurnID = "turn-7301"
	mustPublish(t, root, third)
	res, err := idx.Freshness(root, uint64(time.Now().UnixMilli())+1000)
	if err != nil {
		t.Fatal(err)
	}
	if res.Freshness != IndexStale || res.Source != 3 || res.Indexed != 2 {
		t.Errorf("after new episode = %+v, want stale over 3 source / 2 indexed", res)
	}

	// Repair, then an in-place edit that moves only the mtime half.
	if _, err := Sync(rootPath, indexPath); err != nil {
		t.Fatal(err)
	}
	nowMs = uint64(time.Now().UnixMilli()) + 1000
	if res, err := idx.Freshness(root, nowMs); err != nil || res.Freshness != IndexFresh {
		t.Fatalf("post-sync = %+v, %v; want fresh", res, err)
	}
	var victim string
	err = WalkCorpus(root, func(path string, kind WalkKind, info fs.FileInfo) error {
		if kind == WalkEpisode && victim == "" {
			victim = filepath.Join(rootPath, filepath.FromSlash(path))
		}
		return nil
	})
	if err != nil || victim == "" {
		t.Fatalf("no episode to edit (err %v)", err)
	}
	content, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victim, append(content, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	moved := time.UnixMilli(int64(nowMs) + 500)
	if err := os.Chtimes(victim, moved, moved); err != nil {
		t.Fatal(err)
	}
	res, err = idx.Freshness(root, uint64(moved.UnixMilli())+1000)
	if err != nil {
		t.Fatal(err)
	}
	if res.Freshness != IndexStale {
		t.Errorf("after in-place edit = %+v, want stale (authoritative hash check)", res)
	}
}

func TestFreshnessVerdictSurvivesProcessBoundary(t *testing.T) {
	rootPath, indexPath, root, nowMs := freshnessFixture(t)
	digest := RootDigestHex(rootPath)

	idx1, err := OpenIndex(indexPath, &digest)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := idx1.Freshness(root, nowMs); err != nil || res.Freshness != IndexFresh {
		idx1.Close()
		t.Fatalf("stamping call = %+v, %v; want fresh", res, err)
	}
	idx1.Close()

	// A second handle stands in for the next process: the only place the
	// verdict can travel is the index file. The locked files prove the
	// second handle reused it rather than recomputing.
	lockEpisodeFiles(t, rootPath, root)
	idx2, err := OpenIndex(indexPath, &digest)
	if err != nil {
		t.Fatal(err)
	}
	defer idx2.Close()
	res, err := idx2.Freshness(root, nowMs+1000)
	if err != nil {
		t.Fatal(err)
	}
	if res.Freshness != IndexFresh || res.Indexed != 2 {
		t.Errorf("second handle = %+v, want the stamped fresh verdict", res)
	}
}

func TestFreshnessSurvivesUnwritableIndex(t *testing.T) {
	rootPath, indexPath, root, nowMs := freshnessFixture(t)
	digest := RootDigestHex(rootPath)

	// No memo exists yet; the projection goes read-only before the first
	// Freshness call, so the memo write must fail and be swallowed.
	if err := os.Chmod(indexPath, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(indexPath, 0o600) })
	idx, err := OpenIndex(indexPath, &digest)
	if err != nil {
		t.Skipf("read-only projection cannot be opened at all on this driver: %v", err)
	}
	defer idx.Close()

	res, err := idx.Freshness(root, nowMs)
	if err != nil {
		t.Fatalf("Freshness over a read-only projection: %v", err)
	}
	if res.Freshness != IndexFresh || res.Indexed != 2 || res.Source != 2 {
		t.Errorf("read-only answer = %+v, want the computed fresh verdict", res)
	}
	if v, _ := idx.metaGet(metaFreshnessVerdict); v != "" {
		t.Errorf("memo verdict = %q, want none on a read-only projection", v)
	}
}

// TestFreshnessDoesNotMemoizeHotCorpus pins the settled-corpus guard: while
// the newest episode mtime is not strictly older than nowMs, a write landing
// in that same millisecond would leave the signature unchanged, so the
// verdict is computed but neither written nor reused.
func TestFreshnessDoesNotMemoizeHotCorpus(t *testing.T) {
	rootPath, indexPath, root, _ := freshnessFixture(t)
	digest := RootDigestHex(rootPath)
	idx, err := OpenIndex(indexPath, &digest)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	sig, err := CorpusSignatureOf(root)
	if err != nil {
		t.Fatal(err)
	}

	res, err := idx.Freshness(root, sig.MaxMtimeMs)
	if err != nil {
		t.Fatal(err)
	}
	if res.Freshness != IndexFresh {
		t.Fatalf("hot-corpus verdict = %+v, want computed fresh", res)
	}
	if v, _ := idx.metaGet(metaFreshnessVerdict); v != "" {
		t.Errorf("memo verdict = %q, want none while the corpus is hot", v)
	}

	// A memo somebody stamped is likewise not trusted while hot: lock the
	// files so a recompute is detectable, stamp a fresh verdict, and watch
	// the hot call recompute (stale) instead of reusing it.
	if err := idx.stampFreshness(sig, res); err != nil {
		t.Fatal(err)
	}
	lockEpisodeFiles(t, rootPath, root)
	hot, err := idx.Freshness(root, sig.MaxMtimeMs)
	if err != nil {
		t.Fatal(err)
	}
	if hot.Freshness != IndexStale {
		t.Errorf("hot call = %+v, want the authoritative (stale) answer, not the stamped memo", hot)
	}
}

// TestFreshnessStampSkipsBusyProjection: a concurrent index writer must
// cost the caller at most the stamp budget, not the connection's
// five-second busy timeout. The verdict is still served; the memo is
// simply not written, and one later recompute is the whole price.
func TestFreshnessStampSkipsBusyProjection(t *testing.T) {
	rootPath, indexPath, root, nowMs := freshnessFixture(t)
	digest := RootDigestHex(rootPath)
	idx, err := OpenIndex(indexPath, &digest)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	blocker, err := OpenIndex(indexPath, &digest)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	btx, err := blocker.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := btx.Exec(
		"INSERT OR REPLACE INTO meta (key, value) VALUES ('blocker', 'held');"); err != nil {
		t.Fatal(err)
	}
	defer btx.Rollback()

	start := time.Now()
	res, err := idx.Freshness(root, nowMs)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Freshness under a held writer: %v", err)
	}
	if res.Freshness != IndexFresh {
		t.Errorf("verdict under contention = %+v, want computed fresh", res)
	}
	// Well under busy_timeout (5s): the stamp budget plus the compute. A
	// generous bound keeps slow CI honest without making the test flaky.
	if elapsed > 2500*time.Millisecond {
		t.Errorf("Freshness stalled %v behind a writer; the stamp must skip, not wait", elapsed)
	}
	if err := btx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if v, _ := idx.metaGet(metaFreshnessVerdict); v != "" {
		t.Errorf("memo verdict = %q, want none after a skipped stamp", v)
	}
}

// TestVocabOrderIsDeterministic pins the discovery-order contract: the
// vocabulary iterates in ORDER BY term order, so the MaxVocabMatches cap
// truncates a stable, defined prefix of the sorted vocabulary. The
// vocabulary here exceeds the cap so the prefix the cap would keep is
// exercised. Note the storage accident this test must outlive: term_stats
// is WITHOUT ROWID keyed (world, term), so even an unordered SELECT walks
// sorted today — the ORDER BY turns that accident into a contract, and
// this test kills any mutation of the stated order (e.g. DESC).
func TestVocabOrderIsDeterministic(t *testing.T) {
	const n = MaxVocabMatches + 50
	idx := openMemoryIndex(t)
	want := make([]string, n)
	for i := range want {
		want[i] = fmt.Sprintf("vocab%04d", i)
	}
	// Insert in reverse so agreement with `want` cannot come from
	// insertion order alone.
	for i := n - 1; i >= 0; i-- {
		if _, err := idx.db.Exec(
			"INSERT INTO term_stats (world, term, df, eval_df) VALUES ('w', ?1, 1, 0);",
			want[i]); err != nil {
			t.Fatal(err)
		}
	}
	for run := 0; run < 2; run++ {
		got, err := idx.VocabTerms("w")
		if err != nil {
			t.Fatalf("VocabTerms run %d: %v", run, err)
		}
		if len(got) != n {
			t.Fatalf("run %d: %d terms, want %d", run, len(got), n)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("run %d: position %d = %q, want %q (sorted order)",
					run, i, got[i], want[i])
			}
		}
	}
}

// TestVocabCandidatesMatchLinearScan property-tests the trigram lookup
// against the linear scan it replaces. The vocabulary enters through the
// real write path (Publish + SyncFromCorpus), so trigram population is
// part of what is under test; the reference is the exact scan rule the
// wholly-short fallback still runs — any-needle containment over the
// sorted vocabulary, capped at MaxVocabMatches. One needle stem
// ("zzz", 1100 terms) overflows the cap so the truncated prefix and flag
// are compared, boundary (three-byte) needles are included, and an
// orphaned trigram row is planted to pin the term_stats join.
func TestVocabCandidatesMatchLinearScan(t *testing.T) {
	base := mustValidate(t, testPayloadJSON)
	_, root := testCorpus(t)

	rng := rand.New(rand.NewSource(83))
	seen := map[string]struct{}{}
	var words []string
	add := func(w string) {
		if _, dup := seen[w]; !dup {
			seen[w] = struct{}{}
			words = append(words, w)
		}
	}
	for i := 0; i < 1100; i++ {
		add(fmt.Sprintf("zzz%04d", i))
	}
	for len(words) < 1500 {
		n := 3 + rng.Intn(8)
		b := make([]byte, n)
		for j := range b {
			b[j] = "abcz"[rng.Intn(4)]
		}
		add(string(b))
	}
	const perEpisode = 150
	for i := 0; i*perEpisode < len(words); i++ {
		chunk := words[i*perEpisode : min((i+1)*perEpisode, len(words))]
		var body strings.Builder
		for j, w := range chunk {
			if j > 0 {
				if j%10 == 0 {
					body.WriteByte('\n')
				} else {
					body.WriteByte(' ')
				}
			}
			body.WriteString(w)
		}
		p := base
		p.TurnID = fmt.Sprintf("turn-vc%03d", i)
		p.UserContent = body.String()
		mustPublish(t, root, p)
	}

	idx := openMemoryIndex(t)
	if _, err := idx.SyncFromCorpus(root); err != nil {
		t.Fatalf("SyncFromCorpus: %v", err)
	}
	world := base.World

	// A ghost row: the term posts a trigram but has no vocabulary entry.
	// Without the term_stats join it would surface for needle "abc" —
	// planted on an uncapped needle deliberately, because a ghost behind
	// the "zzz" overflow would sort past the cap and truncate away
	// unseen.
	if _, err := idx.db.Exec(
		"INSERT INTO term_trigrams (world, trigram, term) VALUES (?1, 'abc', 'abcghost');",
		world); err != nil {
		t.Fatal(err)
	}

	linear := func(needles []string) ([]string, bool) {
		vocab, err := idx.VocabTerms(world)
		if err != nil {
			t.Fatalf("VocabTerms: %v", err)
		}
		var matches []string
	scan:
		for _, token := range vocab {
			for _, needle := range needles {
				if strings.Contains(token, needle) {
					if len(matches) >= MaxVocabMatches {
						return matches, true
					}
					matches = append(matches, token)
					continue scan
				}
			}
		}
		return matches, false
	}

	needleSets := [][]string{
		{"zzz"},         // overflows the cap: prefix + flag equality
		{"abc"},         // ordinary three-byte needle
		{"zzz0007"},     // needle equal to a full term
		{"caz", "abz"},  // multi-needle union
		{"qqq"},         // matches nothing
		{"czz", "zza"},  // boundary needles sharing the stem
		{"zzz0", "cab"}, // stem prefix plus unrelated needle
	}
	for i := 0; i < 30; i++ {
		n := 3 + rng.Intn(4)
		b := make([]byte, n)
		for j := range b {
			b[j] = "abcz"[rng.Intn(4)]
		}
		needleSets = append(needleSets, []string{string(b)})
	}
	for _, set := range needleSets {
		want, wantTruncated := linear(set)
		got, gotTruncated, err := idx.VocabCandidates(world, set)
		if err != nil {
			t.Fatalf("VocabCandidates(%q): %v", set, err)
		}
		if !slices.Equal(got, want) {
			t.Errorf("needles %q: %d candidates != linear scan's %d (first diff at %d)",
				set, len(got), len(want), firstDiff(got, want))
		}
		if gotTruncated != wantTruncated {
			t.Errorf("needles %q: truncated = %v, linear scan says %v", set, gotTruncated, wantTruncated)
		}
	}

	// A sub-trigram needle cannot be witnessed here; the discovery policy
	// routes wholly-short queries through the linear scan instead.
	got, truncated, err := idx.VocabCandidates(world, []string{"ab"})
	if err != nil || len(got) != 0 || truncated {
		t.Errorf("sub-trigram needle: got %d candidates, truncated %v, err %v", len(got), truncated, err)
	}
}

func firstDiff(a, b []string) int {
	for i := 0; i < min(len(a), len(b)); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

// TestEpisodeMetadataBoundedByMatches pins the match-proportional
// metadata contract: the rows fetched are exactly the referenced,
// filter-eligible episode ids — never the world. The corpus holds three
// episodes; asking for one id returns one row, an evaluation-lane id is
// filtered by the query rather than surfaced, and an id list crossing
// the chunk boundary (mostly ids the index has never seen) still returns
// only the real rows, proving the chunk loop and the IN filter rather
// than an accidental whole-world scan.
func TestEpisodeMetadataBoundedByMatches(t *testing.T) {
	base := mustValidate(t, testPayloadJSON)
	_, root := testCorpus(t)
	published := publishDistinctCorpus(t, root, base)

	idx := openMemoryIndex(t)
	if _, err := idx.SyncFromCorpus(root); err != nil {
		t.Fatalf("SyncFromCorpus: %v", err)
	}
	world := base.World
	searchLanes := []Lane{LaneConversation, LaneDelegatedWork, LaneImportedLegacy}

	one, err := idx.EpisodeMetadata([]string{published[0].EpisodeID}, world, nil, searchLanes)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].EpisodeID != published[0].EpisodeID {
		t.Fatalf("one id: %d rows", len(one))
	}

	// published[2] is the evaluation-lane episode: referenced but
	// filtered inside the query, not by the caller.
	filtered, err := idx.EpisodeMetadata([]string{published[2].EpisodeID}, world, nil, searchLanes)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 0 {
		t.Fatalf("evaluation-lane id surfaced %d rows", len(filtered))
	}

	ids := []string{published[0].EpisodeID, published[1].EpisodeID}
	for i := 0; i < postingsTermChunk+50; i++ {
		ids = append(ids, fmt.Sprintf("aj1-%032d", i))
	}
	many, err := idx.EpisodeMetadata(ids, world, nil, searchLanes)
	if err != nil {
		t.Fatal(err)
	}
	if len(many) != 2 {
		t.Fatalf("chunked lookup: %d rows, want exactly the 2 real ids", len(many))
	}
}

// A wrong-schema database belonging to another root is rejected as foreign
// before any disposal decision runs: the meta table and its root identity
// exist in every shipped schema, so the dispose-and-recreate path must
// never get the chance to destroy another root's index.
func TestForeignRootRejectedBeforeSchemaDisposal(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "index.sqlite")
	digestA := strings.Repeat("a", 64)
	digestB := strings.Repeat("b", 64)
	{
		idx, err := OpenIndex(dbPath, &digestA)
		if err != nil {
			t.Fatal(err)
		}
		if err := idx.Upsert(EpisodeRow{
			EpisodeID: "aj1-victim", DigestHex: strings.Repeat("0", 64),
			RelPath: "2026/07/28/aj1-victim.md",
			World:   "x", Scope: "global", Lane: LaneConversation,
			Harness: "h", SessionID: "s", TurnID: "t",
			EventTimeMs: 1, CaptureTimeMs: 2,
			CapturePolicy: "p", TurnOutcome: "completed",
		}); err != nil {
			idx.Close()
			t.Fatal(err)
		}
		// Simulate an older build's projection: only the version stamp
		// moves; identity metadata is present in every shipped schema.
		if err := idx.metaSet("index_schema_version", "2"); err != nil {
			idx.Close()
			t.Fatal(err)
		}
		idx.Close()
	}
	if _, err := OpenIndex(dbPath, &digestB); !errors.Is(err, ErrForeignIndex) {
		t.Fatalf("err = %v, want ErrForeignIndex", err)
	}
	// The refused open disposed nothing: root A's row and identity stand.
	raw, err := openSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var episodes int64
	if err := raw.QueryRow("SELECT COUNT(*) FROM episodes;").Scan(&episodes); err != nil {
		t.Fatal(err)
	}
	if episodes != 1 {
		t.Errorf("episodes = %d, want 1 (foreign open must not dispose)", episodes)
	}
	var storedDigest string
	if err := raw.QueryRow(
		"SELECT value FROM meta WHERE key = 'root_digest';").Scan(&storedDigest); err != nil {
		t.Fatal(err)
	}
	if storedDigest != digestA {
		t.Errorf("root_digest = %q, want root A's", storedDigest)
	}
}
