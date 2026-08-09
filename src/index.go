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
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// syncHashKeyPrefix namespaces the per-file content hashes sync stores
// in meta. Keys (not columns) keep the frozen index schema untouched;
// the SQL literals below hard-code the prefix's 12-byte length.
const syncHashKeyPrefix = "sync_sha256:"

// IndexSchemaVersion is the projection's schema identity; a database
// stamped with anything else is disposed and recreated.
const IndexSchemaVersion = 2

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
	}

	if rootDigest != nil {
		stored, err := idx.metaGet("root_digest")
		if err != nil {
			return nil, err
		}
		if stored == "" {
			if err := idx.metaSet("root_digest", *rootDigest); err != nil {
				return nil, err
			}
		} else if stored != *rootDigest {
			return nil, ErrForeignIndex
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
// error for "no such table" — while busy/corrupt failures still
// propagate, matching the Zig oracle's error split.
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

	var total uint64
	current := true
	seen := map[string]struct{}{}
	walkErr := fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == "." {
				return err
			}
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if path == "." {
				return nil
			}
			name := d.Name()
			if name == "" || name[0] == '.' {
				return fs.SkipDir
			}
			if strings.Count(path, "/")+1 > CorpusWalkDepth {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		name := d.Name()
		if !strings.HasPrefix(name, IDPrefix) || !strings.HasSuffix(name, ".md") {
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

// clampMillis bounds a stored millisecond timestamp the way the Zig oracle
// does: negative reads as 0, above int64 on write clamps to int64.
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
	_, err = tx.ExecContext(context.Background(), statsSQL, ep.World, ep.EpisodeID)
	return mapDBError(err)
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

// VocabTerms iterates the world's vocabulary for substring discovery.
func (idx *Index) VocabTerms(world string) ([]string, error) {
	rows, err := idx.db.Query("SELECT term FROM term_stats WHERE world = ?1;", world)
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

// SearchEpisodes returns the metadata Search needs for every episode in
// the world under the scope/lane filters — the in-memory side of the
// join PostingPairs avoids. Lane tags come from the closed enum, so
// baking them into the SQL text is injection-safe.
func (idx *Index) SearchEpisodes(world string, scope *string, lanes []Lane) ([]PostingRow, error) {
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
	sqlText.WriteString(");")
	rows, err := idx.db.Query(sqlText.String(), args...)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer rows.Close()
	var out []PostingRow
	for rows.Next() {
		var (
			row      PostingRow
			bodyLine int64
			lane     string
			eventMs  int64
		)
		if err := rows.Scan(&row.EpisodeID, &row.DigestHex, &row.RelPath,
			&row.Scope, &lane, &row.CapturePolicy, &eventMs, &bodyLine); err != nil {
			return nil, mapDBError(err)
		}
		if bodyLine < 0 || bodyLine > math.MaxUint32 {
			return nil, fmt.Errorf("body line out of range: %w", ErrSQLiteCorrupt)
		}
		row.BodyLine = uint32(bodyLine)
		row.Lane = Lane(lane)
		switch row.Lane {
		case LaneConversation, LaneDelegatedWork, LaneEvaluation, LaneImportedLegacy:
		default:
			return nil, fmt.Errorf("episode lane %q: %w", lane, ErrSQLiteCorrupt)
		}
		row.EventTimeMs = nonNeg(eventMs)
		out = append(out, row)
	}
	return out, mapDBError(rows.Err())
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

	// A vanished corpus (deleted between root open and sync) is an empty
	// corpus: the whole projection becomes empty too.
	if _, err := fs.Stat(root.FS(), "."); errors.Is(err, fs.ErrNotExist) {
		for _, table := range []string{"postings", "term_stats", "episodes"} {
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

	seen := map[string]struct{}{}
	seenPaths := map[string]struct{}{}
	walkErr := fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// The Zig oracle aborts the sync (rolled back) when the corpus
			// root itself cannot be read; an unreadable subdirectory is
			// just skipped, like its openDir-failure continue.
			if path == "." {
				return err
			}
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if path == "." {
				return nil
			}
			name := d.Name()
			// Dot-directories (.git, .obsidian, …) are foreign tooling
			// state, never episode shards: skip them.
			if name == "" || name[0] == '.' {
				return fs.SkipDir
			}
			if strings.Count(path, "/")+1 > CorpusWalkDepth {
				return fs.SkipDir
			}
			// Permission repair is best effort: a foreign-owned entry
			// that cannot be hardened is still valid memory.
			_ = root.Chmod(path, 0o700)
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		name := d.Name()
		if !strings.HasPrefix(name, IDPrefix) || !strings.HasSuffix(name, ".md") {
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
	excluded := report.DuplicateIDs + report.SkippedMalformed
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

// readRootFile reads one confined file with a byte budget; over-budget is
// an error, never a truncation.
func readRootFile(root *os.Root, path string, maxBytes int64) ([]byte, error) {
	f, err := root.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	content, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > maxBytes {
		return nil, fmt.Errorf("exceeds %d bytes", maxBytes)
	}
	return content, nil
}
