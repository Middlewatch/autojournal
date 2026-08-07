package autojournal

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

	// Sync discovers the second episode and keeps the first (idempotent).
	first, err := idx.SyncFromCorpus(root)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if first.Indexed != 2 || first.Removed != 0 || first.SkippedMalformed != 0 {
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

	// The lean discovery path (PostingPairs + SearchEpisodes membership)
	// must agree with the joined per-term query it replaced in Search.
	pairs, err := idx.PostingPairs([]string{"zebra", "quokka"})
	if err != nil {
		t.Fatal(err)
	}
	eligible, err := idx.SearchEpisodes("testworld", nil,
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
