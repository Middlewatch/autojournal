// SQLite substrate for the disposable index projection.
//
// database/sql with the pure-Go modernc.org/sqlite driver is that layer — the
// package's only direct dependency, chosen because no cgo means the binary
// stays static and dependency-free, which DESIGN.md treats as a product
// property rather than a build detail. This file owns the two things that must not drift between call
// sites: the connection configuration (WAL, bounded busy wait, immediate
// transactions) and the driver-error → sentinel mapping the rest of the
// package classifies with errors.Is.
//
// One connection per handle (SetMaxOpenConns(1)): the CLI is
// single-threaded per invocation, and cross-process writers serialize
// through SQLite itself (WAL plus the busy timeout), which is what keeps a
// separate advisory-lock layer out of the design. An embedding host that shares an *Index across
// goroutines gets correct serialization from database/sql; write latency
// under contention is bounded by the busy timeout.

package autojournal

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"modernc.org/sqlite"
)

// Failure vocabulary for the SQLite layer. The set exists so callers can
// classify with errors.Is instead of matching driver strings: Search maps
// ErrSQLiteBusy to the timeout outcome and every other database failure to
// unavailable, and those two need to stay distinguishable because they mean
// different things to an owner — one is contention, the other is damage.
var (
	ErrSQLiteBusy     = errors.New("sqlite: busy or locked")
	ErrSQLiteCorrupt  = errors.New("sqlite: corrupt or not a database")
	ErrSQLiteReadOnly = errors.New("sqlite: read-only database")
	ErrSQLiteCantOpen = errors.New("sqlite: cannot open")
	ErrSQLiteMisuse   = errors.New("sqlite: misuse")
	ErrSQLiteNoMemory = errors.New("sqlite: out of memory")
	// ErrSQLite is any other driver failure; the wrapped message keeps
	// the driver detail.
	ErrSQLite = errors.New("sqlite error")
)

// Primary result codes (extended code & 0xff), from sqlite3.h.
const (
	sqliteBusy     = 5
	sqliteLocked   = 6
	sqliteNoMemory = 7
	sqliteReadOnly = 8
	sqliteCantOpen = 14
	sqliteCorrupt  = 11
	sqliteNotADB   = 26
	sqliteMisuse   = 21
)

// mapDBError classifies a driver error into the package vocabulary. The
// sentinel wraps the driver message so logs keep the detail while callers
// match with errors.Is.
func mapDBError(err error) error {
	if err == nil {
		return nil
	}
	var sqlErr *sqlite.Error
	if !errors.As(err, &sqlErr) {
		return fmt.Errorf("%w: %v", ErrSQLite, err)
	}
	var sentinel error
	switch sqlErr.Code() & 0xff {
	case sqliteBusy, sqliteLocked:
		sentinel = ErrSQLiteBusy
	case sqliteCorrupt, sqliteNotADB:
		sentinel = ErrSQLiteCorrupt
	case sqliteReadOnly:
		sentinel = ErrSQLiteReadOnly
	case sqliteCantOpen:
		sentinel = ErrSQLiteCantOpen
	case sqliteMisuse:
		sentinel = ErrSQLiteMisuse
	case sqliteNoMemory:
		sentinel = ErrSQLiteNoMemory
	default:
		sentinel = ErrSQLite
	}
	return fmt.Errorf("%w: %v", sentinel, err)
}

// openSQLite opens (creating if needed) the database at path with the
// projection's fixed configuration. ":memory:" works for tests. WAL lets
// concurrent capture processes serialize without blocking readers; the
// busy timeout bounds the wait instead of failing instantly; the index is
// disposable, so NORMAL durability is enough. _txlock=immediate makes
// every database/sql transaction BEGIN IMMEDIATE, taking the write lock up
// front rather than upgrading mid-transaction, which is where a deferred
// transaction would deadlock into SQLITE_BUSY after already doing work.
//
// The path rides in a SQLite URI, so sqliteURIPath escapes the characters
// that would terminate it early: --index is a documented CLI override and
// XDG_STATE_HOME is owner-supplied, and without the escaping a path
// containing '?' would truncate the DSN and silently create a database at
// a different path with none of the pragmas above.
// busyTimeoutMs is the connection's bounded busy wait, priced for writes
// that must succeed. One caller opts out per operation: the freshness memo
// stamp zeroes it for its transaction and restores it before the
// connection returns to the pool, because a cache write must never make a
// search wait behind a writer.
const busyTimeoutMs = 5000

func openSQLite(path string) (*sql.DB, error) {
	dsn := "file:" + sqliteURIPath(path) +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(" + strconv.Itoa(busyTimeoutMs) + ")" +
		"&_pragma=foreign_keys(1)" +
		"&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, mapDBError(err)
	}
	// One connection serializes all use of this handle. With more than
	// one, a transaction on conn A could not see uncommitted state on
	// conn B, and concurrent in-process writers would busy-wait on each
	// other instead of queueing.
	db.SetMaxOpenConns(1)
	// sql.Open is lazy; Ping forces the pragma list to execute so a
	// malformed database fails here, at open, not on first use.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, mapDBError(err)
	}
	return db, nil
}

// sqliteURIPath escapes the characters that end a SQLite URI filename:
// '%' first since it introduces escapes, then '?' (query string) and '#'
// (fragment). Everything else passes through — SQLite percent-decodes the
// whole path component, so escaped and literal spellings name one file.
func sqliteURIPath(path string) string {
	return strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
}
