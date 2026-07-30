//! Minimal SQLite binding for the disposable index projection.
//!
//! One connection per handle; the CLI is single-threaded per invocation and
//! cross-process index writers serialize through SQLite itself (WAL plus a
//! bounded busy timeout). A long-lived embedding host must wrap writes in
//! one process-wide mutex before sharing a handle across threads.

const std = @import("std");
pub const c = @cImport({
    @cInclude("sqlite3.h");
});

/// C's `SQLITE_TRANSIENT` — `(sqlite3_destructor_type)-1` — tells SQLite to
/// copy the bound buffer before returning, so bound slices need not outlive
/// the bind call. The macro is a cast expression translate-c cannot import,
/// and the all-ones value cannot be formed as a Zig function pointer on
/// targets that alignment-check fn pointers (aarch64-macos), so the bind is
/// redeclared with a pointer-sized integer destructor: identical ABI, and
/// the sentinel stays an integer end to end.
const sqlite_transient: usize = std.math.maxInt(usize);
const sqlite3_bind_text_transient = struct {
    extern fn sqlite3_bind_text(
        stmt: ?*c.sqlite3_stmt,
        idx: c_int,
        text: [*c]const u8,
        len: c_int,
        destructor: usize,
    ) c_int;
}.sqlite3_bind_text;

pub const Error = error{
    Busy,
    Corrupt,
    ReadOnly,
    CantOpen,
    Misuse,
    SqliteError,
    OutOfMemory,
};

fn mapRc(rc: c_int) Error {
    return switch (rc & 0xff) {
        c.SQLITE_BUSY, c.SQLITE_LOCKED => error.Busy,
        c.SQLITE_CORRUPT, c.SQLITE_NOTADB => error.Corrupt,
        c.SQLITE_READONLY => error.ReadOnly,
        c.SQLITE_CANTOPEN => error.CantOpen,
        c.SQLITE_MISUSE => error.Misuse,
        c.SQLITE_NOMEM => error.OutOfMemory,
        else => error.SqliteError,
    };
}

pub const Db = struct {
    handle: *c.sqlite3,

    /// `path` must be NUL-terminated (`:memory:` works). SQLite creates the
    /// file with its default (umask-derived) permissions; privacy comes from
    /// callers placing the index inside an owner-only state directory.
    pub fn open(path: [:0]const u8) Error!Db {
        var handle: ?*c.sqlite3 = null;
        const rc = c.sqlite3_open_v2(
            path.ptr,
            &handle,
            c.SQLITE_OPEN_READWRITE | c.SQLITE_OPEN_CREATE,
            null,
        );
        if (rc != c.SQLITE_OK) {
            if (handle) |h| _ = c.sqlite3_close(h);
            return mapRc(rc);
        }
        var db: Db = .{ .handle = handle.? };
        errdefer db.close();
        // WAL lets concurrent capture processes serialize without blocking
        // readers; the busy timeout bounds the wait instead of failing
        // instantly. The index is disposable, so NORMAL durability is enough.
        try db.exec("PRAGMA journal_mode=WAL;");
        try db.exec("PRAGMA synchronous=NORMAL;");
        try db.exec("PRAGMA busy_timeout=5000;");
        try db.exec("PRAGMA foreign_keys=ON;");
        return db;
    }

    pub fn close(db: *Db) void {
        _ = c.sqlite3_close(db.handle);
    }

    pub fn exec(db: *Db, sql: [:0]const u8) Error!void {
        const rc = c.sqlite3_exec(db.handle, sql.ptr, null, null, null);
        if (rc != c.SQLITE_OK) return mapRc(rc);
    }

    pub fn prepare(db: *Db, sql: [:0]const u8) Error!Stmt {
        var stmt: ?*c.sqlite3_stmt = null;
        const rc = c.sqlite3_prepare_v2(db.handle, sql.ptr, -1, &stmt, null);
        if (rc != c.SQLITE_OK) return mapRc(rc);
        return .{ .handle = stmt.? };
    }
};

pub const Stmt = struct {
    handle: *c.sqlite3_stmt,

    pub fn finalize(st: *Stmt) void {
        _ = c.sqlite3_finalize(st.handle);
    }

    /// Rewind for re-execution. Bindings survive a reset; rebind every
    /// parameter before the next step, or stale values silently repeat.
    pub fn reset(st: *Stmt) Error!void {
        const rc = c.sqlite3_reset(st.handle);
        if (rc != c.SQLITE_OK) return mapRc(rc);
    }

    /// SQLite copies the text before returning (SQLITE_TRANSIENT), so
    /// `text` only needs to live for this call.
    pub fn bindText(st: *Stmt, idx: c_int, text: []const u8) Error!void {
        const rc = sqlite3_bind_text_transient(st.handle, idx, text.ptr, @intCast(text.len), sqlite_transient);
        if (rc != c.SQLITE_OK) return mapRc(rc);
    }

    pub fn bindInt64(st: *Stmt, idx: c_int, value: i64) Error!void {
        const rc = c.sqlite3_bind_int64(st.handle, idx, value);
        if (rc != c.SQLITE_OK) return mapRc(rc);
    }

    /// True when a row is available; false when done.
    pub fn step(st: *Stmt) Error!bool {
        return switch (c.sqlite3_step(st.handle)) {
            c.SQLITE_ROW => true,
            c.SQLITE_DONE => false,
            else => |rc| mapRc(rc),
        };
    }

    /// Borrowed from statement memory; invalidated by the next step/reset.
    pub fn columnText(st: *Stmt, idx: c_int) []const u8 {
        const ptr = c.sqlite3_column_text(st.handle, idx) orelse return "";
        const len: usize = @intCast(c.sqlite3_column_bytes(st.handle, idx));
        return @as([*]const u8, @ptrCast(ptr))[0..len];
    }

    pub fn columnInt64(st: *Stmt, idx: c_int) i64 {
        return c.sqlite3_column_int64(st.handle, idx);
    }
};

test "in-memory open, version parity with vendored header, round trip" {
    var db = try Db.open(":memory:");
    defer db.close();
    // Runtime library must match the vendored header, or the two files have
    // silently drifted.
    try std.testing.expectEqualStrings(
        c.SQLITE_VERSION,
        std.mem.span(c.sqlite3_libversion()),
    );
    try db.exec("CREATE TABLE t (id TEXT PRIMARY KEY, n INTEGER NOT NULL);");
    var insert = try db.prepare("INSERT OR REPLACE INTO t (id, n) VALUES (?1, ?2);");
    defer insert.finalize();
    try insert.bindText(1, "alpha");
    try insert.bindInt64(2, 41);
    try std.testing.expect(!try insert.step());
    try insert.reset();
    try insert.bindText(1, "alpha");
    try insert.bindInt64(2, 42);
    try std.testing.expect(!try insert.step());

    var query = try db.prepare("SELECT n FROM t WHERE id = ?1;");
    defer query.finalize();
    try query.bindText(1, "alpha");
    try std.testing.expect(try query.step());
    try std.testing.expectEqual(@as(i64, 42), query.columnInt64(0));
    try std.testing.expect(!try query.step());
}
