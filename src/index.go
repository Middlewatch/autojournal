// Disposable SQLite projection over the Markdown corpus.
//
// Contains only rebuildable retrieval state. Deleting the database and
// running sync reproduces it from the authoritative episode files; a
// failed or busy index update leaves capture successful and the
// projection visibly stale.
//
// Schema v2 adds retrieval state: per-line term postings, a per-world
// vocabulary with episode document frequencies, the body start line, and
// identity rows (schema, tokenizer, root digest) that gate reuse — a
// wrong-schema, wrong-tokenizer, or wrong-root database is disposed or
// rejected, never misread as an empty memory corpus.

package autojournal

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// syncHashKeyPrefix namespaces the per-file content hashes sync stores
// in meta. Keys (not columns) keep the frozen index schema untouched;
// the SQL literals below hard-code the prefix's 12-byte length.
const syncHashKeyPrefix = "sync_sha256:"

// MaxVocabMatches caps the vocabulary terms one query's discovery may
// match; beyond it discovery is truncated and the caller reports it. The
// cap lives beside the vocabulary lookup primitives that enforce it;
// which needles to build, when the short-query fallback applies, and what
// a truncated discovery means for outcome and confidence stay discovery
// policy in the search capability. VocabTerms and VocabCandidates iterate
// in ORDER BY term order, so the surviving matches are a stable prefix of
// the sorted vocabulary — which 1024 terms a capped query keeps is
// defined, not a scan-order accident.
const MaxVocabMatches = 1024

// IndexSchemaVersion is the projection's schema identity; a database
// stamped with anything else is disposed and recreated.
const IndexSchemaVersion = 3

const createIndexSQL = `
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS episodes (
  episode_id TEXT PRIMARY KEY,
  digest_hex TEXT NOT NULL,
  rel_path TEXT NOT NULL,
  world TEXT NOT NULL,
  scope TEXT NOT NULL,
  lane TEXT NOT NULL,
  harness TEXT NOT NULL,
  session_id TEXT NOT NULL,
  turn_id TEXT NOT NULL,
  event_time_ms INTEGER NOT NULL,
  capture_time_ms INTEGER NOT NULL,
  capture_policy TEXT NOT NULL,
  turn_outcome TEXT NOT NULL,
  body_line INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_episodes_world_time
  ON episodes(world, event_time_ms);
CREATE INDEX IF NOT EXISTS idx_episodes_world_lane
  ON episodes(world, lane);
CREATE TABLE IF NOT EXISTS postings (
  term TEXT NOT NULL,
  episode_id TEXT NOT NULL,
  line_no INTEGER NOT NULL,
  PRIMARY KEY (term, episode_id, line_no)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS idx_postings_episode ON postings(episode_id);
CREATE TABLE IF NOT EXISTS term_stats (
  world TEXT NOT NULL,
  term TEXT NOT NULL,
  df INTEGER NOT NULL,
  eval_df INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (world, term)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS term_trigrams (
  world TEXT NOT NULL,
  trigram TEXT NOT NULL,
  term TEXT NOT NULL,
  PRIMARY KEY (world, trigram, term)
) WITHOUT ROWID;
`

// ErrForeignIndex means the database carries another root's identity.
// Never interpreted as an empty corpus; the caller reports unavailable
// and advises sync/rebuild against the right root.
var ErrForeignIndex = errors.New("index belongs to a different journal root")

// ErrIndexMalformed means content handed to IndexEpisode is not a
// parseable episode.
var ErrIndexMalformed = errors.New("malformed episode content")

// Index is the projection handle. It is safe for concurrent use
// (database/sql serializes through the single connection); transactions
// hold that connection until they end, so callers must not start a second
// operation while one is open — the CLI never does.
type Index struct {
	db *sql.DB
}

// sqlQuerier is satisfied by both *sql.DB and *sql.Tx, so helpers run
// inside or outside an explicit transaction without duplication.
type sqlQuerier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// OpenIndex opens (creating if needed) the projection at path. A
// projection whose schema or tokenizer version does not match is
// disposable state from another build: every table is dropped and
// recreated. A projection recording a different rootDigest is foreign and
// is rejected instead — disposal would silently destroy another root's
// index. A nil rootDigest skips the identity check (sync's deliberate
// repoint path).
func OpenIndex(path string, rootDigest *string) (*Index, error) {
	db, err := openSQLite(path)
	if err != nil {
		return nil, err
	}
	idx := &Index{db: db}
	ok := false
	defer func() {
		if !ok {
			db.Close()
		}
	}()

	// Identity is read before any DDL: running the create statements
	// against an unknown older schema could itself fail. A missing meta
	// table (fresh database) reads as no identity and takes the
	// dispose-and-create path, which drops nothing.
	version, err := idx.metaGetInt("index_schema_version")
	if err != nil {
		return nil, err
	}
	tokenizer, err := idx.metaGet("tokenizer_version")
	if err != nil {
		return nil, err
	}
	// The foreign-root gate runs before any disposal decision: the meta
	// table (and its root_digest key) exists in every schema this product
	// has shipped, so a database recording another root's identity is
	// rejected whatever its schema version — disposing it first would
	// silently destroy another root's index, the exact event this check
	// exists to prevent. A nil rootDigest (sync's deliberate repoint path)
	// skips the gate and may dispose freely.
	stored := ""
	if rootDigest != nil {
		stored, err = idx.metaGet("root_digest")
		if err != nil {
			return nil, err
		}
		if stored != "" && stored != *rootDigest {
			return nil, ErrForeignIndex
		}
	}
	if version != IndexSchemaVersion || tokenizer != TokenizerVersion {
		if err := idx.disposeAllTables(); err != nil {
			return nil, err
		}
		if _, err := db.Exec(createIndexSQL); err != nil {
			return nil, mapDBError(err)
		}
		if err := idx.writeIdentity(); err != nil {
			return nil, err
		}
		stored = "" // disposal dropped meta; re-stamp below
	}

	if rootDigest != nil && stored == "" {
		if err := idx.metaSet("root_digest", *rootDigest); err != nil {
			return nil, err
		}
	}
	ok = true
	return idx, nil
}

// Close releases the database handle.
func (idx *Index) Close() error {
	return idx.db.Close()
}

// OpenIndexHardened is OpenIndex plus the filesystem hygiene every host
// owes the projection: the parent directory is created owner-only, and
// the database and its SQLite sidecars are narrowed to 0600. The index
// holds episode text, so a default-umask 0644 database would publish the
// owner's captured conversations to every account on the machine.
//
// Hosts call this rather than OpenIndex unless they are deliberately
// opening a projection they did not create.
func OpenIndexHardened(path string, rootDigest *string) (*Index, error) {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSQLiteCantOpen, err)
	}
	if err := os.Chmod(parent, 0o700); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSQLiteCantOpen, err)
	}
	idx, err := OpenIndex(path, rootDigest)
	if err != nil {
		return nil, err
	}
	if err := HardenIndexFiles(path); err != nil {
		idx.Close()
		return nil, err
	}
	return idx, nil
}

// HardenIndexFiles narrows the database and any SQLite sidecar to
// owner-only. Called on open and again after a write, because the -wal
// and -shm files can appear at any point in a transaction.
func HardenIndexFiles(path string) error {
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("%w: %v", ErrSQLiteCantOpen, err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		err := os.Chmod(path+suffix, 0o600)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %v", ErrSQLiteCantOpen, err)
		}
	}
	return nil
}

// metaGet reads one meta value; "" when absent. A missing meta table
// (fresh or foreign database) reads as absence too — the generic driver
// error for "no such table" — while busy/corrupt failures still propagate.
// Reading absence routes to dispose-and-rebuild, which is safe by doctrine:
// the projection is disposable and sync rebuilds it from Markdown. Note the
// swallow is wider than that reasoning strictly licenses — an unmapped driver
// error also reads as absence.
func (idx *Index) metaGet(key string) (string, error) {
	var value string
	err := idx.db.QueryRow("SELECT value FROM meta WHERE key = ?1;", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		mapped := mapDBError(err)
		if errors.Is(mapped, ErrSQLite) {
			return "", nil
		}
		return "", mapped
	}
	return value, nil
}

func (idx *Index) metaGetInt(key string) (int64, error) {
	text, err := idx.metaGet(key)
	if err != nil || text == "" {
		return 0, err
	}
	// An unparseable stored version reads as "no identity" (0 never
	// matches the current schema version), forcing disposal.
	n, _ := strconv.ParseInt(text, 10, 64)
	return n, nil
}

// ExcludedCount is the number of files the last sync deliberately
// excluded (duplicate ids, malformed). Freshness checks add this to the
// indexed count so deliberate exclusions never read as staleness.
func (idx *Index) ExcludedCount() uint64 {
	n, _ := idx.excludedCount()
	return n
}

func (idx *Index) excludedCount() (uint64, error) {
	text, err := idx.metaGet("sync_excluded")
	if err != nil {
		return 0, err
	}
	n, _ := strconv.ParseUint(text, 10, 64)
	return n, nil
}

// CorpusMatches reports the visible episode count and whether every current
// candidate has the same path and bytes recorded by capture or the last sync.
// It is read-only; a missing hash means stale so older indexes self-repair on
// their next sync rather than claiming freshness from row counts alone.
func (idx *Index) CorpusMatches(root *os.Root) (uint64, bool, error) {
	stored := map[string]string{}
	rows, err := idx.db.Query(
		"SELECT key, value FROM meta WHERE substr(key, 1, 12) = 'sync_sha256:';")
	if err != nil {
		return 0, false, mapDBError(err)
	}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			rows.Close()
			return 0, false, mapDBError(err)
		}
		stored[key[len(syncHashKeyPrefix):]] = value
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, false, mapDBError(err)
	}
	rows.Close()

	// The walk is WalkCorpus's visibility rule; this visitor only hashes.
	// An unreadable root is returned as the walk error; an unreadable
	// subtree is skipped without a verdict here, as before the conversion.
	var total uint64
	current := true
	seen := map[string]struct{}{}
	walkErr := WalkCorpus(root, func(path string, kind WalkKind, info fs.FileInfo) error {
		if kind != WalkEpisode {
			return nil
		}
		total++
		seen[path] = struct{}{}
		content, err := readRootFile(root, path, MaxEpisodeFileBytes)
		if err != nil {
			current = false
			return nil
		}
		sum := sha256.Sum256(content)
		value := hex.EncodeToString(sum[:])
		if stored[path] != value {
			current = false
		}
		return nil
	})
	if walkErr != nil {
		return total, false, walkErr
	}
	if len(seen) != len(stored) {
		current = false
	}
	return total, current, nil
}

func (idx *Index) metaSet(key, value string) error {
	_, err := idx.db.Exec(
		"INSERT OR REPLACE INTO meta (key, value) VALUES (?1, ?2);", key, value)
	return mapDBError(err)
}

func (idx *Index) writeIdentity() error {
	for _, kv := range [][2]string{
		{"index_schema_version", strconv.Itoa(IndexSchemaVersion)},
		{"tokenizer_version", TokenizerVersion},
		{"scorer_version", ScorerVersion},
		{"confidence_policy", ConfidencePolicyVersion},
	} {
		if err := idx.metaSet(kv[0], kv[1]); err != nil {
			return err
		}
	}
	return nil
}

// disposeAllTables drops every non-internal table, whatever schema left
// it behind, so a version bump can never strand stale tables from an
// unknown build.
func (idx *Index) disposeAllTables() error {
	rows, err := idx.db.Query(
		"SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%';")
	if err != nil {
		return mapDBError(err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return mapDBError(err)
		}
		if strings.ContainsRune(name, '"') {
			continue
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return mapDBError(err)
	}
	rows.Close()
	for _, name := range names {
		if _, err := idx.db.Exec(`DROP TABLE IF EXISTS "` + name + `";`); err != nil {
			return mapDBError(err)
		}
	}
	return nil
}

// EpisodeRow is one episodes-table row.
type EpisodeRow struct {
	EpisodeID     string
	DigestHex     string
	RelPath       string
	World         string
	Scope         string
	Lane          Lane
	Harness       string
	SessionID     string
	TurnID        string
	EventTimeMs   uint64
	CaptureTimeMs uint64
	CapturePolicy string
	TurnOutcome   string
	BodyLine      uint32
}

// clampMillis bounds a millisecond timestamp into the projection's signed
// column: anything above int64 saturates rather than wrapping to a negative
// instant. Saturation is deliberate — a wrapped timestamp would sort a turn
// before every real one and corrupt recency ordering, whereas a saturated one
// is merely wrong in a visible direction. Values this large are rejected at
// Validate; this is the projection's own guard, not a substitute for it.
func clampMillis(v uint64) int64 {
	return int64(min(v, math.MaxInt64))
}

func nonNeg(v int64) uint64 {
	return uint64(max(v, 0))
}

// Upsert is the episodes-row-only upsert: postings and term stats are NOT
// touched. IndexEpisode is the paved path; this survives for repair
// tooling and tests that need a bare row.
func (idx *Index) Upsert(row EpisodeRow) error {
	return idx.upsert(idx.db, row)
}

func (idx *Index) upsert(q sqlQuerier, row EpisodeRow) error {
	_, err := q.ExecContext(context.Background(),
		`INSERT OR REPLACE INTO episodes (
		  episode_id, digest_hex, rel_path, world, scope, lane, harness,
		  session_id, turn_id, event_time_ms, capture_time_ms,
		  capture_policy, turn_outcome, body_line
		) VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11,?12,?13,?14);`,
		row.EpisodeID, row.DigestHex, row.RelPath, row.World, row.Scope,
		string(row.Lane), row.Harness, row.SessionID, row.TurnID,
		clampMillis(row.EventTimeMs), clampMillis(row.CaptureTimeMs),
		row.CapturePolicy, row.TurnOutcome, int64(row.BodyLine))
	return mapDBError(err)
}

// IndexEpisode fully indexes one episode from its rendered content:
// episode row, per-line postings from the body (frontmatter is metadata,
// not memory), and per-world document frequencies (evaluation lane is
// excluded from stats). One transaction; idempotent per content.
func (idx *Index) IndexEpisode(relPath, content string) error {
	tx, err := idx.db.BeginTx(context.Background(), nil)
	if err != nil {
		return mapDBError(err)
	}
	defer tx.Rollback() // no-op after Commit; undoes the partial write otherwise
	parsed := ParseEpisode(content)
	if parsed == nil {
		return ErrIndexMalformed
	}
	var priorPath string
	err = tx.QueryRowContext(context.Background(),
		"SELECT rel_path FROM episodes WHERE episode_id = ?1;", parsed.EpisodeID).Scan(&priorPath)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return mapDBError(err)
	}
	if err := idx.indexEpisodeInTx(tx, relPath, content); err != nil {
		return err
	}
	if priorPath != "" && priorPath != relPath {
		if _, err := tx.ExecContext(context.Background(),
			"DELETE FROM meta WHERE key = ?1;", syncHashKeyPrefix+priorPath); err != nil {
			return mapDBError(err)
		}
	}
	sum := sha256.Sum256([]byte(content))
	if _, err := tx.ExecContext(context.Background(),
		"INSERT OR REPLACE INTO meta (key, value) VALUES (?1, ?2);",
		syncHashKeyPrefix+relPath, hex.EncodeToString(sum[:])); err != nil {
		return mapDBError(err)
	}
	return mapDBError(tx.Commit())
}

// indexEpisodeInTx runs inside the caller's transaction (sync wraps the
// whole corpus walk in one); never call outside a transaction or a torn
// write becomes visible.
func (idx *Index) indexEpisodeInTx(tx *sql.Tx, relPath, content string) error {
	ep := ParseEpisode(content)
	if ep == nil {
		return ErrIndexMalformed
	}
	if err := idx.deindexEpisodeInTx(tx, ep.EpisodeID); err != nil {
		return err
	}
	if err := idx.upsert(tx, EpisodeRow{
		EpisodeID:     ep.EpisodeID,
		DigestHex:     ep.DigestHex,
		RelPath:       relPath,
		World:         ep.World,
		Scope:         ep.Scope,
		Lane:          ep.Lane,
		Harness:       ep.Harness,
		SessionID:     ep.SessionID,
		TurnID:        ep.TurnID,
		EventTimeMs:   ep.EventTimeMs,
		CaptureTimeMs: ep.CaptureTimeMs,
		CapturePolicy: ep.CapturePolicy,
		TurnOutcome:   ep.TurnOutcome,
		BodyLine:      ep.BodyLine,
	}); err != nil {
		return err
	}

	lineNo := ep.BodyLine
	// One prepare per episode instead of one implicit prepare per token:
	// a body yields hundreds of postings, and modernc.org/sqlite
	// re-parses an unprepared statement on every Exec.
	postingStmt, err := tx.PrepareContext(context.Background(),
		"INSERT OR IGNORE INTO postings (term, episode_id, line_no) VALUES (?1, ?2, ?3);")
	if err != nil {
		return mapDBError(err)
	}
	defer postingStmt.Close()
	for _, line := range strings.Split(content[ep.BodyOffset:], "\n") {
		for _, token := range TokenizeLine(line) {
			_, err := postingStmt.ExecContext(context.Background(), token, ep.EpisodeID, int64(lineNo))
			if err != nil {
				return mapDBError(err)
			}
		}
		lineNo++
	}

	// Every lane's terms enter the vocabulary (or explicit evaluation
	// queries could never discover anything), but only non-evaluation
	// lanes count toward df corpus statistics. The no-op WHERE true is
	// required: SQLite refuses INSERT ... SELECT ... ON CONFLICT without
	// a WHERE clause on the SELECT (documented upsert parsing ambiguity),
	// so removing it is a syntax error.
	// Terms this world has not seen before, read BEFORE the stats upsert:
	// any term already in term_stats carries its trigram rows from the
	// write that introduced it, so only genuinely new vocabulary pays the
	// trigram expansion.
	newTermRows, err := tx.QueryContext(context.Background(),
		`SELECT DISTINCT p.term FROM postings p WHERE p.episode_id = ?2
		   AND NOT EXISTS (SELECT 1 FROM term_stats ts
		                   WHERE ts.world = ?1 AND ts.term = p.term);`,
		ep.World, ep.EpisodeID)
	if err != nil {
		return mapDBError(err)
	}
	var newTerms []string
	for newTermRows.Next() {
		var term string
		if err := newTermRows.Scan(&term); err != nil {
			newTermRows.Close()
			return mapDBError(err)
		}
		newTerms = append(newTerms, term)
	}
	if err := newTermRows.Err(); err != nil {
		newTermRows.Close()
		return mapDBError(err)
	}
	newTermRows.Close()

	statsSQL := `INSERT INTO term_stats (world, term, df, eval_df)
	  SELECT ?1, term, 1, 0
	  FROM (SELECT DISTINCT term FROM postings WHERE episode_id = ?2)
	  WHERE true
	  ON CONFLICT(world, term) DO UPDATE SET df = df + 1;`
	if ep.Lane == LaneEvaluation {
		statsSQL = `INSERT INTO term_stats (world, term, df, eval_df)
		  SELECT ?1, term, 0, 1
		  FROM (SELECT DISTINCT term FROM postings WHERE episode_id = ?2)
		  WHERE true
		  ON CONFLICT(world, term) DO UPDATE SET eval_df = eval_df + 1;`
	}
	if _, err := tx.ExecContext(context.Background(), statsSQL, ep.World, ep.EpisodeID); err != nil {
		return mapDBError(err)
	}

	if len(newTerms) == 0 {
		return nil
	}
	trigramStmt, err := tx.PrepareContext(context.Background(),
		"INSERT OR IGNORE INTO term_trigrams (world, trigram, term) VALUES (?1, ?2, ?3);")
	if err != nil {
		return mapDBError(err)
	}
	defer trigramStmt.Close()
	for _, term := range newTerms {
		for _, tri := range trigramsOf(term) {
			if _, err := trigramStmt.ExecContext(context.Background(), ep.World, tri, term); err != nil {
				return mapDBError(err)
			}
		}
	}
	return nil
}

// trigramsOf returns the distinct three-byte substrings of term — the
// unit term_trigrams posts. Bytes, not runes: terms and needles come out
// of the same tokenizer, so byte-level agreement is exact, and a term
// under three bytes simply has no trigram (it stays reachable through
// the wholly-short linear-scan fallback).
func trigramsOf(term string) []string {
	if len(term) < 3 {
		return nil
	}
	seen := make(map[string]struct{}, len(term)-2)
	out := make([]string, 0, len(term)-2)
	for i := 0; i+3 <= len(term); i++ {
		tri := term[i : i+3]
		if _, dup := seen[tri]; dup {
			continue
		}
		seen[tri] = struct{}{}
		out = append(out, tri)
	}
	return out
}

// deindexEpisodeInTx removes an episode's postings and decrements its
// document frequencies (when it counted toward them). The episodes row
// itself is left to the caller: upsert replaces it, deletion removes it.
func (idx *Index) deindexEpisodeInTx(tx *sql.Tx, episodeID string) error {
	var oldWorld, oldLane string
	err := tx.QueryRowContext(context.Background(),
		"SELECT world, lane FROM episodes WHERE episode_id = ?1;", episodeID).
		Scan(&oldWorld, &oldLane)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return mapDBError(err)
	}
	// An over-bound world is a corrupt row; postings and stats are left
	// in place and sync rebuilds them rather than guessing here.
	if errors.Is(err, sql.ErrNoRows) || len(oldWorld) > MaxWorldLen {
		return nil
	}

	statsSQL := `UPDATE term_stats SET df = df - 1
	  WHERE world = ?1 AND term IN
	    (SELECT DISTINCT term FROM postings WHERE episode_id = ?2);`
	if oldLane == string(LaneEvaluation) {
		statsSQL = `UPDATE term_stats SET eval_df = eval_df - 1
		  WHERE world = ?1 AND term IN
		    (SELECT DISTINCT term FROM postings WHERE episode_id = ?2);`
	}
	if _, err := tx.ExecContext(context.Background(), statsSQL, oldWorld, episodeID); err != nil {
		return mapDBError(err)
	}
	// Terms about to leave the vocabulary take their trigram rows with
	// them, read before the stats delete so the list still exists. The
	// per-trigram delete walks the (world, trigram, term) primary key;
	// no second index is needed for a path this rare.
	dyingRows, err := tx.QueryContext(context.Background(),
		"SELECT term FROM term_stats WHERE world = ?1 AND df <= 0 AND eval_df <= 0;",
		oldWorld)
	if err != nil {
		return mapDBError(err)
	}
	var dying []string
	for dyingRows.Next() {
		var term string
		if err := dyingRows.Scan(&term); err != nil {
			dyingRows.Close()
			return mapDBError(err)
		}
		dying = append(dying, term)
	}
	if err := dyingRows.Err(); err != nil {
		dyingRows.Close()
		return mapDBError(err)
	}
	dyingRows.Close()
	for _, term := range dying {
		for _, tri := range trigramsOf(term) {
			if _, err := tx.ExecContext(context.Background(),
				"DELETE FROM term_trigrams WHERE world = ?1 AND trigram = ?2 AND term = ?3;",
				oldWorld, tri, term); err != nil {
				return mapDBError(err)
			}
		}
	}
	if _, err := tx.ExecContext(context.Background(),
		"DELETE FROM term_stats WHERE world = ?1 AND df <= 0 AND eval_df <= 0;",
		oldWorld); err != nil {
		return mapDBError(err)
	}
	_, err = tx.ExecContext(context.Background(),
		"DELETE FROM postings WHERE episode_id = ?1;", episodeID)
	return mapDBError(err)
}

// EpisodeCount is the number of rows the projection holds.
func (idx *Index) EpisodeCount() (uint64, error) {
	var n int64
	err := idx.db.QueryRow("SELECT COUNT(*) FROM episodes;").Scan(&n)
	return nonNeg(n), mapDBError(err)
}

// StatsEpisodeCount is the corpus size N for IDF: episodes in the world,
// evaluation lane excluded from corpus statistics.
func (idx *Index) StatsEpisodeCount(world string) (uint64, error) {
	var n int64
	err := idx.db.QueryRow(
		"SELECT COUNT(*) FROM episodes WHERE world = ?1 AND lane <> 'evaluation';",
		world).Scan(&n)
	return nonNeg(n), mapDBError(err)
}

// VocabTerms iterates the world's vocabulary for substring discovery, in
// ORDER BY term order. The order is a contract, not tidiness: discovery
// caps matches at MaxVocabMatches, and a stated order makes the cap
// truncate a stable prefix. Without the ORDER BY the scan happens to walk
// the (world, term) primary key of this WITHOUT ROWID table in the same
// order today — but that is a query-planner accident SQL semantics do not
// promise, and ranking must not rest on it.
func (idx *Index) VocabTerms(world string) ([]string, error) {
	rows, err := idx.db.Query("SELECT term FROM term_stats WHERE world = ?1 ORDER BY term;", world)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer rows.Close()
	var terms []string
	for rows.Next() {
		var term string
		if err := rows.Scan(&term); err != nil {
			return nil, mapDBError(err)
		}
		terms = append(terms, term)
	}
	return terms, mapDBError(rows.Err())
}

// VocabCandidates is the index's vocabulary lookup primitive for
// trigram-eligible queries: the terms containing any needle, in ORDER BY
// term order, capped at MaxVocabMatches, with the truncation flag
// alongside. Trigram postings narrow the candidates (a term containing a
// needle necessarily posts every one of the needle's trigrams), a
// strings.Contains verification then makes the survivor set exactly the
// linear scan's, and the join against term_stats keeps any orphaned
// trigram row invisible. Trigrams are over vocabulary terms only, never
// episode bodies. A needle under three bytes has no trigram and matches
// nothing here; the caller routes wholly-short queries through the
// VocabTerms linear scan instead.
func (idx *Index) VocabCandidates(world string, needles []string) ([]string, bool, error) {
	matched := map[string]struct{}{}
	for _, needle := range needles {
		tris := trigramsOf(needle)
		if len(tris) == 0 {
			continue
		}
		placeholders := make([]string, len(tris))
		args := make([]any, 0, len(tris)+2)
		args = append(args, world)
		for i, tri := range tris {
			placeholders[i] = fmt.Sprintf("?%d", i+2)
			args = append(args, tri)
		}
		args = append(args, len(tris))
		rows, err := idx.db.Query(
			`SELECT tg.term FROM term_trigrams tg
			   JOIN term_stats ts ON ts.world = tg.world AND ts.term = tg.term
			   WHERE tg.world = ?1 AND tg.trigram IN (`+strings.Join(placeholders, ", ")+`)
			   GROUP BY tg.term HAVING COUNT(*) = ?`+strconv.Itoa(len(tris)+2)+`;`,
			args...)
		if err != nil {
			return nil, false, mapDBError(err)
		}
		for rows.Next() {
			var term string
			if err := rows.Scan(&term); err != nil {
				rows.Close()
				return nil, false, mapDBError(err)
			}
			if strings.Contains(term, needle) {
				matched[term] = struct{}{}
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, false, mapDBError(err)
		}
		rows.Close()
	}
	candidates := make([]string, 0, len(matched))
	for term := range matched {
		candidates = append(candidates, term)
	}
	slices.Sort(candidates)
	if len(candidates) > MaxVocabMatches {
		return candidates[:MaxVocabMatches], true, nil
	}
	return candidates, false, nil
}

// PostingRow is one posting joined with its episode metadata.
type PostingRow struct {
	EpisodeID     string
	DigestHex     string
	RelPath       string
	Scope         string
	Lane          Lane
	CapturePolicy string
	EventTimeMs   uint64
	BodyLine      uint32
	LineNo        uint32
}

// postingsTermChunk caps the terms bound into one IN clause, well under
// SQLite's bound-parameter limit while keeping a MaxVocabMatches-sized
// discovery to at most a handful of queries.
const postingsTermChunk = 500

// PostingPair is one (episode, line) coordinate from the postings table,
// without episode metadata.
type PostingPair struct {
	EpisodeID string
	LineNo    uint32
}

// PostingPairs returns the (episode, line) coordinates for a set of
// vocabulary tokens, in chunked IN-clause queries against the postings
// primary key alone. No episode join: profiling showed the per-row B-tree
// probe into episodes dominating broad searches, so Search loads episode
// metadata once via SearchEpisodes and joins in memory. The same
// (episode, line) pair recurs once per matching term; callers dedup.
func (idx *Index) PostingPairs(terms []string) ([]PostingPair, error) {
	var out []PostingPair
	for start := 0; start < len(terms); start += postingsTermChunk {
		chunk := terms[start:min(start+postingsTermChunk, len(terms))]
		if err := idx.postingPairsChunk(chunk, &out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (idx *Index) postingPairsChunk(terms []string, out *[]PostingPair) error {
	var sqlText strings.Builder
	sqlText.WriteString("SELECT episode_id, line_no FROM postings WHERE term IN (")
	args := make([]any, 0, len(terms))
	for i, term := range terms {
		if i > 0 {
			sqlText.WriteByte(',')
		}
		sqlText.WriteByte('?')
		args = append(args, term)
	}
	sqlText.WriteString(");")
	rows, err := idx.db.Query(sqlText.String(), args...)
	if err != nil {
		return mapDBError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			pair   PostingPair
			lineNo int64
		)
		if err := rows.Scan(&pair.EpisodeID, &lineNo); err != nil {
			return mapDBError(err)
		}
		if lineNo < 0 || lineNo > math.MaxUint32 {
			return fmt.Errorf("posting line out of range: %w", ErrSQLiteCorrupt)
		}
		pair.LineNo = uint32(lineNo)
		*out = append(*out, pair)
	}
	return mapDBError(rows.Err())
}

// EpisodeMetadata returns the metadata Search needs for exactly the
// referenced episode ids — the in-memory side of the join PostingPairs
// avoids, at a cost proportional to the match set rather than the world
// (this replaces the whole-world SearchEpisodes load). The world, scope,
// and lane filters ride in the query, so a posting outside them simply
// misses the result. Chunked IN clauses follow PostingPairs' pattern;
// lane tags come from the closed enum, so baking them into the SQL text
// is injection-safe.
func (idx *Index) EpisodeMetadata(ids []string, world string, scope *string, lanes []Lane) ([]PostingRow, error) {
	var out []PostingRow
	for start := 0; start < len(ids); start += postingsTermChunk {
		chunk := ids[start:min(start+postingsTermChunk, len(ids))]
		if err := idx.episodeMetadataChunk(chunk, world, scope, lanes, &out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (idx *Index) episodeMetadataChunk(ids []string, world string, scope *string, lanes []Lane, out *[]PostingRow) error {
	var sqlText strings.Builder
	sqlText.WriteString(
		`SELECT episode_id, digest_hex, rel_path, scope, lane, capture_policy,
		       event_time_ms, body_line
		FROM episodes WHERE world = ?`)
	args := []any{world}
	if scope != nil {
		sqlText.WriteString(" AND scope = ?")
		args = append(args, *scope)
	}
	sqlText.WriteString(" AND lane IN (")
	for i, lane := range lanes {
		if i > 0 {
			sqlText.WriteByte(',')
		}
		fmt.Fprintf(&sqlText, "'%s'", string(lane))
	}
	sqlText.WriteString(") AND episode_id IN (")
	for i, id := range ids {
		if i > 0 {
			sqlText.WriteByte(',')
		}
		sqlText.WriteByte('?')
		args = append(args, id)
	}
	sqlText.WriteString(");")
	rows, err := idx.db.Query(sqlText.String(), args...)
	if err != nil {
		return mapDBError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			row      PostingRow
			bodyLine int64
			lane     string
			eventMs  int64
		)
		if err := rows.Scan(&row.EpisodeID, &row.DigestHex, &row.RelPath,
			&row.Scope, &lane, &row.CapturePolicy, &eventMs, &bodyLine); err != nil {
			return mapDBError(err)
		}
		if bodyLine < 0 || bodyLine > math.MaxUint32 {
			return fmt.Errorf("body line out of range: %w", ErrSQLiteCorrupt)
		}
		row.BodyLine = uint32(bodyLine)
		row.Lane = Lane(lane)
		switch row.Lane {
		case LaneConversation, LaneDelegatedWork, LaneEvaluation, LaneImportedLegacy:
		default:
			return fmt.Errorf("episode lane %q: %w", lane, ErrSQLiteCorrupt)
		}
		row.EventTimeMs = nonNeg(eventMs)
		*out = append(*out, row)
	}
	return mapDBError(rows.Err())
}

// PostingsForTerm returns all postings for one vocabulary token, filtered
// by world, optional scope, and an explicit lane set. Lane tags come from
// the closed enum, so baking them into the SQL text is injection-safe.
func (idx *Index) PostingsForTerm(term, world string, scope *string, lanes []Lane) ([]PostingRow, error) {
	var sqlText strings.Builder
	sqlText.WriteString(
		`SELECT p.episode_id, p.line_no, e.digest_hex, e.rel_path, e.scope,
		       e.lane, e.capture_policy, e.event_time_ms, e.body_line
		FROM postings p JOIN episodes e ON e.episode_id = p.episode_id
		WHERE p.term = ?1 AND e.world = ?2`)
	args := []any{term, world}
	if scope != nil {
		sqlText.WriteString(" AND e.scope = ?3")
		args = append(args, *scope)
	}
	sqlText.WriteString(" AND e.lane IN (")
	for i, lane := range lanes {
		if i > 0 {
			sqlText.WriteByte(',')
		}
		fmt.Fprintf(&sqlText, "'%s'", string(lane))
	}
	sqlText.WriteString(");")

	rows, err := idx.db.Query(sqlText.String(), args...)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer rows.Close()
	var out []PostingRow
	for rows.Next() {
		var (
			row              PostingRow
			lineNo, bodyLine int64
			lane             string
			eventMs          int64
		)
		if err := rows.Scan(&row.EpisodeID, &lineNo, &row.DigestHex, &row.RelPath,
			&row.Scope, &lane, &row.CapturePolicy, &eventMs, &bodyLine); err != nil {
			return nil, mapDBError(err)
		}
		// Stored values outside their write-side bounds mean the database
		// was tampered with or damaged: reject it as Corrupt rather than
		// misreading the row.
		if lineNo < 0 || lineNo > math.MaxUint32 || bodyLine < 0 || bodyLine > math.MaxUint32 {
			return nil, fmt.Errorf("posting line out of range: %w", ErrSQLiteCorrupt)
		}
		row.LineNo = uint32(lineNo)
		row.BodyLine = uint32(bodyLine)
		row.Lane = Lane(lane)
		switch row.Lane {
		case LaneConversation, LaneDelegatedWork, LaneEvaluation, LaneImportedLegacy:
		default:
			return nil, fmt.Errorf("posting lane %q: %w", lane, ErrSQLiteCorrupt)
		}
		row.EventTimeMs = nonNeg(eventMs)
		out = append(out, row)
	}
	return out, mapDBError(rows.Err())
}

// LookupEpisode looks up one episode row by ID.
func (idx *Index) LookupEpisode(episodeID string) (*EpisodeRow, error) {
	var (
		row              EpisodeRow
		lane             string
		eventMs, bodyRaw int64
	)
	err := idx.db.QueryRow(
		`SELECT digest_hex, rel_path, world, scope, lane, capture_policy,
		       event_time_ms, body_line
		FROM episodes WHERE episode_id = ?1;`, episodeID).
		Scan(&row.DigestHex, &row.RelPath, &row.World, &row.Scope, &lane,
			&row.CapturePolicy, &eventMs, &bodyRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	// Same contract as PostingsForTerm: out-of-bound stored values reject
	// the row as Corrupt instead of being misread.
	row.Lane = Lane(lane)
	switch row.Lane {
	case LaneConversation, LaneDelegatedWork, LaneEvaluation, LaneImportedLegacy:
	default:
		return nil, fmt.Errorf("episode lane %q: %w", lane, ErrSQLiteCorrupt)
	}
	if bodyRaw < 0 || bodyRaw > math.MaxUint32 {
		return nil, fmt.Errorf("body line out of range: %w", ErrSQLiteCorrupt)
	}
	row.EpisodeID = episodeID
	row.EventTimeMs = nonNeg(eventMs)
	row.BodyLine = uint32(bodyRaw)
	return &row, nil
}

// WorldScope pairs for catalog. The owner's configured default pair is
// prepended by the caller, which owns config.
type WorldScope struct {
	World string
	Scope string
}

// WorldScopePairs lists the distinct (world, scope) pairs the projection
// knows, sorted.
func (idx *Index) WorldScopePairs() ([]WorldScope, error) {
	rows, err := idx.db.Query("SELECT DISTINCT world, scope FROM episodes ORDER BY world, scope;")
	if err != nil {
		return nil, mapDBError(err)
	}
	defer rows.Close()
	var pairs []WorldScope
	for rows.Next() {
		var p WorldScope
		if err := rows.Scan(&p.World, &p.Scope); err != nil {
			return nil, mapDBError(err)
		}
		pairs = append(pairs, p)
	}
	return pairs, mapDBError(rows.Err())
}

// SyncReport is the sync accounting. Indexed counts episodes whose
// projection rows were (re)written this run; Unchanged counts episodes
// whose file still matches the stored row and were skipped wholesale.
type SyncReport struct {
	Indexed          uint64
	Unchanged        uint64
	Removed          uint64
	SkippedMalformed uint64
	DuplicateIDs     uint64
	// DigestMismatch counts files that parse as episodes but whose recorded
	// digest disagrees with their content. They are indexed and excluded
	// from recall, and reseal is the way back: the projection stays a
	// complete map of the corpus, so the freshness arithmetic is untouched.
	DigestMismatch uint64
	// Unreadable counts subtrees the walk could not read and skipped. Sync
	// still succeeds — one foreign-owned directory must not make sync
	// unusable — but the count joins the deliberate-exclusion total so
	// freshness cannot report fresh over content nobody can see.
	Unreadable uint64
}

// SyncFromCorpus brings the projection up to date with the corpus.
// Each file's SHA-256 is compared against the hash stored under its
// meta key when it was last indexed; a byte-identical file with its row
// still present is skipped without parsing or writing, because
// re-deriving its rows would rewrite exactly them. New, edited, and
// moved files are fully reindexed (row, postings, stats), rows whose
// files are gone are removed, malformed files are counted and excluded.
// One transaction; a torn sync never leaves a half-projection or
// half-updated hash map.
func (idx *Index) SyncFromCorpus(root *os.Root) (SyncReport, error) {
	return idx.syncFromCorpus(root, "")
}

// Freshness memo meta keys: the verdict beside the exact stat-only
// signature that produced it, plus the counts the verdict was computed with,
// so a reused verdict reports the same arithmetic an authoritative run would.
const (
	metaFreshnessVerdict  = "freshness_verdict"
	metaFreshnessEpisodes = "freshness_episodes"
	metaFreshnessMaxMtime = "freshness_max_mtime_ms"
	metaFreshnessIndexed  = "freshness_indexed"
	metaFreshnessExcluded = "freshness_excluded"
)

// FreshnessResult is the one health signal. Every reporter derives its
// freshness from this and nothing else.
type FreshnessResult struct {
	Freshness IndexFreshness
	Indexed   uint64
	Source    uint64
	Excluded  uint64
}

// Freshness answers the one health question: does the projection cover the
// corpus? The authoritative check reads and hashes every episode file, which
// is too expensive to repeat per query, so the verdict is memoized in index
// meta beside the signature that produced it. A call takes the stat-only
// signature; when it matches the stored one the stored verdict is reused,
// and otherwise the authoritative check runs and re-stamps both.
//
// Memoizing in the projection rather than in process is what makes status
// and search agree: the binary runs once per operation, so two invocations
// share nothing else. The residual risk is any change that preserves both
// halves of the signature — episode count and newest mtime — which a plain
// mv of an episode between shard directories reachably does, not only the
// mtime-forging edit the narrower reading suggests. Such a corpus serves a
// memoized fresh until the next capture or sync moves the signature —
// bounded, because the per-episode digest verification on the search path
// is independent of this and excludes edited evidence whatever freshness
// says.
//
// nowMs is the settled-corpus guard. A memo is reused or written only while
// the newest episode mtime is strictly older than the current millisecond:
// a write landing in the same millisecond as the signature's newest mtime
// would leave the signature unchanged, so a verdict stamped against a
// still-hot corpus could outlive a change it never saw. While the corpus is
// hot the call degrades to the authoritative check, never to a wrong reuse.
//
// The memo write is best-effort: a read-only or busy projection computes
// without memoizing rather than failing the caller. Every projection write
// elsewhere either changes the corpus signature (capture and supersede both
// touch episode files) or clears the memo in its own transaction (sync), so
// a stored verdict can never describe a projection state that no longer
// exists.
func (idx *Index) Freshness(root *os.Root, nowMs uint64) (FreshnessResult, error) {
	sig, err := CorpusSignatureOf(root)
	if err != nil {
		return FreshnessResult{}, err
	}
	settled := sig.MaxMtimeMs < nowMs
	if settled {
		if res, ok := idx.freshnessMemo(sig); ok {
			return res, nil
		}
	}
	episodes, matches, err := idx.CorpusMatches(root)
	if err != nil {
		return FreshnessResult{}, err
	}
	indexed, err := idx.EpisodeCount()
	if err != nil {
		return FreshnessResult{}, err
	}
	excluded, err := idx.excludedCount()
	if err != nil {
		return FreshnessResult{}, err
	}
	verdict := IndexStale
	if matches && indexed+excluded == episodes {
		verdict = IndexFresh
	}
	res := FreshnessResult{Freshness: verdict, Indexed: indexed, Source: episodes, Excluded: excluded}
	if settled {
		_ = idx.stampFreshness(sig, res)
	}
	return res, nil
}

// freshnessMemo returns the stored verdict when every memo key is present,
// well-formed, and stamped against exactly this signature. Anything less —
// missing keys, an unknown verdict, a signature mismatch — reads as no
// memo, which degrades to the authoritative check.
func (idx *Index) freshnessMemo(sig CorpusSignature) (FreshnessResult, bool) {
	verdict, err := idx.metaGet(metaFreshnessVerdict)
	if err != nil || (verdict != string(IndexFresh) && verdict != string(IndexStale)) {
		return FreshnessResult{}, false
	}
	nums := map[string]uint64{}
	for _, key := range []string{
		metaFreshnessEpisodes, metaFreshnessMaxMtime,
		metaFreshnessIndexed, metaFreshnessExcluded,
	} {
		text, err := idx.metaGet(key)
		if err != nil {
			return FreshnessResult{}, false
		}
		n, err := strconv.ParseUint(text, 10, 64)
		if err != nil {
			return FreshnessResult{}, false
		}
		nums[key] = n
	}
	if nums[metaFreshnessEpisodes] != sig.Episodes || nums[metaFreshnessMaxMtime] != sig.MaxMtimeMs {
		return FreshnessResult{}, false
	}
	return FreshnessResult{
		Freshness: IndexFreshness(verdict),
		Indexed:   nums[metaFreshnessIndexed],
		Source:    sig.Episodes,
		Excluded:  nums[metaFreshnessExcluded],
	}, true
}

// stampFreshness writes verdict, signature, and counts in one transaction,
// so a torn stamp can never pair a verdict with a signature it was not
// computed against.
//
// The stamp opts out of the connection's busy wait: it is a cache write on
// the read path, and stalling a search behind a concurrent writer for the
// full busy_timeout inverts the memo's purpose — a busy projection costs an
// immediate skip, and losing one stamp costs exactly one recompute. The
// zero timeout must be set before Begin, because the DSN's
// txlock=immediate takes the write lock (and so waits) at BEGIN itself —
// hence the pinned connection, with the timeout restored before it
// returns to the pool.
func (idx *Index) stampFreshness(sig CorpusSignature, res FreshnessResult) error {
	ctx := context.Background()
	conn, err := idx.db.Conn(ctx)
	if err != nil {
		return mapDBError(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "PRAGMA busy_timeout(0);"); err != nil {
		return mapDBError(err)
	}
	defer conn.ExecContext(ctx, "PRAGMA busy_timeout("+strconv.Itoa(busyTimeoutMs)+");")
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return mapDBError(err)
	}
	defer tx.Rollback()
	for _, kv := range [][2]string{
		{metaFreshnessVerdict, string(res.Freshness)},
		{metaFreshnessEpisodes, strconv.FormatUint(sig.Episodes, 10)},
		{metaFreshnessMaxMtime, strconv.FormatUint(sig.MaxMtimeMs, 10)},
		{metaFreshnessIndexed, strconv.FormatUint(res.Indexed, 10)},
		{metaFreshnessExcluded, strconv.FormatUint(res.Excluded, 10)},
	} {
		if _, err := tx.ExecContext(ctx,
			"INSERT OR REPLACE INTO meta (key, value) VALUES (?1, ?2);", kv[0], kv[1]); err != nil {
			return mapDBError(err)
		}
	}
	return mapDBError(tx.Commit())
}

// repairShardDir is sync's per-directory permission repair, indirected so a
// test can simulate a foreign-owned directory whose chmod does not take
// without needing a second uid; owner-owned directories self-heal through it.
var repairShardDir = func(root *os.Root, path string) error {
	return root.Chmod(path, 0o700)
}

// syncFromCorpus optionally stamps the owner root identity in the same
// transaction as every projection and freshness update. Package-level Sync
// supplies it; direct index tests and embeddings may leave it empty.
func (idx *Index) syncFromCorpus(root *os.Root, rootDigest string) (SyncReport, error) {
	var report SyncReport
	tx, err := idx.db.BeginTx(context.Background(), nil)
	if err != nil {
		return report, mapDBError(err)
	}
	defer tx.Rollback()

	// The skip decision needs two maps up front, both consistent with
	// the transaction's writes: path -> episode identity for the seen
	// bookkeeping, and path -> content hash for the change test. The
	// hashes live in meta keys (not a column) so the frozen index schema
	// is untouched; losing them degrades to a full rebuild, never to a
	// wrong projection — a hash miss always reindexes.
	stored := map[string]string{}
	storedRows, err := tx.QueryContext(context.Background(), "SELECT rel_path, episode_id FROM episodes;")
	if err != nil {
		return report, mapDBError(err)
	}
	for storedRows.Next() {
		var relPath, id string
		if err := storedRows.Scan(&relPath, &id); err != nil {
			storedRows.Close()
			return report, mapDBError(err)
		}
		stored[relPath] = id
	}
	if err := storedRows.Err(); err != nil {
		storedRows.Close()
		return report, mapDBError(err)
	}
	storedRows.Close()

	syncedHashes := map[string]string{}
	hashRows, err := tx.QueryContext(context.Background(),
		"SELECT key, value FROM meta WHERE substr(key, 1, 12) = 'sync_sha256:';")
	if err != nil {
		return report, mapDBError(err)
	}
	for hashRows.Next() {
		var key, value string
		if err := hashRows.Scan(&key, &value); err != nil {
			hashRows.Close()
			return report, mapDBError(err)
		}
		syncedHashes[key[len(syncHashKeyPrefix):]] = value
	}
	if err := hashRows.Err(); err != nil {
		hashRows.Close()
		return report, mapDBError(err)
	}
	hashRows.Close()

	// Sync changes the projection without touching the corpus, which is the
	// one write the freshness signature cannot see: a memoized verdict left
	// standing here would describe a projection state that no longer exists
	// (a repaired index still reading stale, forever). Clearing it in the
	// same transaction forces the next Freshness call to the authoritative
	// check.
	if _, err := tx.ExecContext(context.Background(),
		"DELETE FROM meta WHERE substr(key, 1, 10) = 'freshness_';"); err != nil {
		return report, mapDBError(err)
	}

	// A vanished corpus (deleted between root open and sync) is an empty
	// corpus: the whole projection becomes empty too.
	if _, err := fs.Stat(root.FS(), "."); errors.Is(err, fs.ErrNotExist) {
		for _, table := range []string{"postings", "term_stats", "term_trigrams", "episodes"} {
			if _, err := tx.ExecContext(context.Background(), "DELETE FROM "+table+";"); err != nil {
				return report, mapDBError(err)
			}
		}
		if _, err := tx.ExecContext(context.Background(),
			"DELETE FROM meta WHERE substr(key, 1, 12) = 'sync_sha256:';"); err != nil {
			return report, mapDBError(err)
		}
		if _, err := tx.ExecContext(context.Background(),
			"INSERT OR REPLACE INTO meta (key, value) VALUES ('sync_excluded', '0');"); err != nil {
			return report, mapDBError(err)
		}
		if rootDigest != "" {
			if _, err := tx.ExecContext(context.Background(),
				"INSERT OR REPLACE INTO meta (key, value) VALUES ('root_digest', ?1);",
				rootDigest); err != nil {
				return report, mapDBError(err)
			}
		}
		return report, mapDBError(tx.Commit())
	}

	// The walk is WalkCorpus's visibility rule; this visitor owns chmod
	// repair and indexing. An unreadable corpus root aborts the sync and
	// rolls back (the walk error), because indexing nothing is not the same
	// answer as an empty corpus. An unreadable subdirectory is skipped
	// instead — the WalkShardDir visit chmods each directory before its
	// read, so an owner-owned one self-heals, and only a foreign-owned tree
	// stays WalkUnreadableDir.
	seen := map[string]struct{}{}
	seenPaths := map[string]struct{}{}
	walkErr := WalkCorpus(root, func(path string, kind WalkKind, info fs.FileInfo) error {
		switch kind {
		case WalkShardDir:
			// Permission repair is best effort: a foreign-owned entry
			// that cannot be hardened is still valid memory.
			_ = repairShardDir(root, path)
			return nil
		case WalkUnreadableDir:
			report.Unreadable++
			return nil
		}
		seenPaths[path] = struct{}{}
		_ = root.Chmod(path, 0o600) // best effort by contract
		content, err := readRootFile(root, path, MaxEpisodeFileBytes)
		if err != nil {
			if _, err := tx.ExecContext(context.Background(),
				"DELETE FROM meta WHERE key = ?1;", syncHashKeyPrefix+path); err != nil {
				return mapDBError(err)
			}
			report.SkippedMalformed++
			return nil
		}
		// The change test is the raw bytes, not the frontmatter's
		// payload_digest: that digest is written by capture and a hand
		// edit does not update it, so trusting it would leave edited
		// postings stale. A byte hash catches every edit; requiring the
		// episode row too keeps a hand-mangled database self-healing
		// (the row is missing -> no skip -> full reindex).
		// Content verification is per file and per run, not only for
		// changed files: the count reads as corpus health, so an edit that
		// predates the previous sync must still be reported by this one.
		if _, verr := VerifyEpisode(string(content)); errors.Is(verr, ErrDigestMismatch) {
			report.DigestMismatch++
		}
		sum := sha256.Sum256(content)
		hexSum := hex.EncodeToString(sum[:])
		if syncedHashes[path] != hexSum {
			if _, err := tx.ExecContext(context.Background(),
				"INSERT OR REPLACE INTO meta (key, value) VALUES (?1, ?2);",
				syncHashKeyPrefix+path, hexSum); err != nil {
				return mapDBError(err)
			}
		}
		if syncedHashes[path] == hexSum {
			if id, ok := stored[path]; ok {
				if _, dup := seen[id]; dup {
					report.DuplicateIDs++
					return nil
				}
				seen[id] = struct{}{}
				report.Unchanged++
				return nil
			}
		}
		ep := ParseEpisode(string(content))
		if ep == nil {
			report.SkippedMalformed++
			return nil
		}
		// Deduplicate by identity: the first copy encountered stays
		// indexed, later copies are counted and skipped. After manual
		// corpus surgery, sync rebaselines.
		if _, dup := seen[ep.EpisodeID]; dup {
			report.DuplicateIDs++
			return nil
		}
		if err := idx.indexEpisodeInTx(tx, path, string(content)); err != nil {
			if errors.Is(err, ErrIndexMalformed) {
				report.SkippedMalformed++
				return nil
			}
			return err
		}
		seen[ep.EpisodeID] = struct{}{}
		report.Indexed++
		return nil
	})
	if walkErr != nil {
		return report, walkErr
	}

	// Remove rows (and their postings/stats) whose source files are gone.
	rows, err := tx.QueryContext(context.Background(), "SELECT episode_id, rel_path FROM episodes;")
	if err != nil {
		return report, mapDBError(err)
	}
	var toRemove []struct{ id, relPath string }
	for rows.Next() {
		var id, relPath string
		if err := rows.Scan(&id, &relPath); err != nil {
			rows.Close()
			return report, mapDBError(err)
		}
		if _, ok := seen[id]; !ok {
			toRemove = append(toRemove, struct{ id, relPath string }{id, relPath})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return report, mapDBError(err)
	}
	rows.Close()
	for _, victim := range toRemove {
		if err := idx.deindexEpisodeInTx(tx, victim.id); err != nil {
			return report, err
		}
		if _, err := tx.ExecContext(context.Background(),
			"DELETE FROM episodes WHERE episode_id = ?1;", victim.id); err != nil {
			return report, mapDBError(err)
		}
		if _, present := seenPaths[victim.relPath]; !present {
			if _, err := tx.ExecContext(context.Background(),
				"DELETE FROM meta WHERE key = ?1;", syncHashKeyPrefix+victim.relPath); err != nil {
				return report, mapDBError(err)
			}
		}
		report.Removed++
	}
	// Excluded candidates have no episode row, so their stale hash keys
	// need path-based cleanup in addition to the row-based removal above.
	for path := range syncedHashes {
		if _, ok := seenPaths[path]; ok {
			continue
		}
		if _, err := tx.ExecContext(context.Background(),
			"DELETE FROM meta WHERE key = ?1;", syncHashKeyPrefix+path); err != nil {
			return report, mapDBError(err)
		}
	}
	excluded := report.DuplicateIDs + report.SkippedMalformed + report.Unreadable
	if _, err := tx.ExecContext(context.Background(),
		"INSERT OR REPLACE INTO meta (key, value) VALUES ('sync_excluded', ?1);",
		strconv.FormatUint(excluded, 10)); err != nil {
		return report, mapDBError(err)
	}
	if rootDigest != "" {
		if _, err := tx.ExecContext(context.Background(),
			"INSERT OR REPLACE INTO meta (key, value) VALUES ('root_digest', ?1);",
			rootDigest); err != nil {
			return report, mapDBError(err)
		}
	}

	return report, mapDBError(tx.Commit())
}
