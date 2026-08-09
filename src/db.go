// SQLite substrate for the disposable index projection.
//
// The Zig reference wrapped the C sqlite3 API directly; in Go the
// database/sql package with the pure-Go modernc.org/sqlite driver is that
// layer (the only direct dependency: no cgo, static binaries keep
// working). This file owns the two things that must not drift between call
// sites: the connection configuration (WAL, bounded busy wait, immediate
// transactions) and the driver-error → sentinel mapping the rest of the
// package classifies with errors.Is.
//
// One connection per handle (SetMaxOpenConns(1)): the CLI is
// single-threaded per invocation, and cross-process writers serialize
// through SQLite itself (WAL plus the busy timeout), exactly the Zig
// compatibility contract. An embedding host that shares an *Index across
// goroutines gets correct serialization from database/sql; write latency
// under contention is bounded by the busy timeout.

package autojournal

import (
	"database/sql"
	"errors"
	"fmt"

	"modernc.org/sqlite"
)

// Failure vocabulary for the SQLite layer, mirroring the Zig oracle's
// db.zig error set. Search maps ErrSQLiteBusy to the timeout outcome and
// every other database failure to unavailable.
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
// every database/sql transaction BEGIN IMMEDIATE, matching the Zig
// oracle's explicit transaction statements.
//
// The DSN is plain concatenation: index paths are derived by paths.go
// under the owner's state directory and never contain '?' or '#'.
func openSQLite(path string) (*sql.DB, error) {
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(5000)" +
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
