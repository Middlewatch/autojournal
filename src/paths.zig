//! Where the journal and its projection live.
//!
//! One definition, every host. The owner CLI and an embedding host (Evoker)
//! must derive the journal root and index path identically or they silently
//! address different corpora: the host captures into a journal the CLI never
//! reports, or opens a projection keyed to another root. These derivations
//! were private to the CLI while it was the only caller; they are public
//! now because they are part of the contract an embedding host implements.
//!
//! Every path returned is absolute and owned by the caller.

const std = @import("std");

const Io = std.Io;

pub const Error = error{ OutOfMemory, MissingHome };

/// A usable XDG base directory, or null. Per the XDG Base Directory spec, a
/// value that is empty *or relative* is invalid and must be ignored rather
/// than resolved against the working directory — every path this module
/// hands back is absolute, and a caller that receives a relative one would
/// fault on the first `openDirAbsolute`.
fn xdgBase(environ: *const std.process.Environ.Map, key: []const u8) ?[]const u8 {
    const value = environ.get(key) orelse return null;
    if (value.len == 0) return null;
    if (!std.fs.path.isAbsolute(value)) return null;
    return value;
}

/// `$XDG_STATE_HOME`, else `$HOME/.local/state`. Always allocates, including
/// the environment-supplied case, so callers holding a general-purpose
/// allocator free exactly one way.
pub fn stateDir(
    allocator: std.mem.Allocator,
    environ: *const std.process.Environ.Map,
) Error![]u8 {
    if (xdgBase(environ, "XDG_STATE_HOME")) |xdg| return allocator.dupe(u8, xdg);
    const home = environ.get("HOME") orelse return error.MissingHome;
    return std.fmt.allocPrint(allocator, "{s}/.local/state", .{home});
}

/// The host-neutral journal default: `$XDG_DATA_HOME/autojournal/journals`,
/// else `$HOME/.local/share/autojournal/journals`. It applies when neither a
/// command override nor the owner config names a root, and it is deliberately
/// host-neutral — every harness on the machine lands in one corpus without
/// configuration, which is the whole of "install and forget".
pub fn defaultJournalRoot(
    allocator: std.mem.Allocator,
    environ: *const std.process.Environ.Map,
) Error![]u8 {
    if (xdgBase(environ, "XDG_DATA_HOME")) |xdg| {
        return std.fmt.allocPrint(allocator, "{s}/autojournal/journals", .{xdg});
    }
    const home = environ.get("HOME") orelse return error.MissingHome;
    return std.fmt.allocPrint(allocator, "{s}/.local/share/autojournal/journals", .{home});
}

pub const digest_hex_len = 64;
/// Prefix length used to name the index file. Long enough that distinct
/// roots do not collide in practice, short enough to stay readable in a
/// status line.
pub const digest_name_len = 16;

pub fn rootDigestHex(root_path: []const u8) [digest_hex_len]u8 {
    var sum: [std.crypto.hash.sha2.Sha256.digest_length]u8 = undefined;
    std.crypto.hash.sha2.Sha256.hash(root_path, &sum, .{});
    var out: [digest_hex_len]u8 = undefined;
    _ = std.fmt.bufPrint(&out, "{x}", .{&sum}) catch unreachable;
    return out;
}

/// The index lives outside the journal root (design amendment: the corpus
/// stays a clean git-trackable tree), keyed by a digest of the root path so
/// distinct roots never share a projection. Sentinel-terminated because
/// SQLite takes a C string.
pub fn defaultIndexPath(
    allocator: std.mem.Allocator,
    environ: *const std.process.Environ.Map,
    root_path: []const u8,
) Error![:0]u8 {
    const state = try stateDir(allocator, environ);
    defer allocator.free(state);
    const digest = rootDigestHex(root_path);
    return std.fmt.allocPrintSentinel(allocator, "{s}/autojournal/index-{s}.sqlite", .{
        state, digest[0..digest_name_len],
    }, 0);
}

/// A journal root placed in a shared directory is refused for writing
/// commands: other users could inject or pre-create paths there, and
/// `/tmp`-style locations are volatile. Shared means the nearest existing
/// ancestor is group- or world-writable (the sshd StrictModes rule) — the
/// walk stops at the first ancestor that exists, so a not-yet-created root
/// is judged by where it would actually be created.
pub fn rootInSharedDirectory(io: Io, root_path: []const u8) bool {
    var candidate: ?[]const u8 = std.fs.path.dirname(root_path);
    while (candidate) |dir_path| {
        var dir = Io.Dir.openDirAbsolute(io, dir_path, .{}) catch {
            candidate = std.fs.path.dirname(dir_path);
            continue;
        };
        defer dir.close(io);
        const stat = dir.stat(io) catch return false;
        return stat.permissions.toMode() & 0o022 != 0;
    }
    return false;
}

// --- tests ---

const testing = std.testing;

fn envMap(gpa: std.mem.Allocator, pairs: []const [2][]const u8) !std.process.Environ.Map {
    var map: std.process.Environ.Map = .init(gpa);
    errdefer map.deinit();
    for (pairs) |pair| try map.put(pair[0], pair[1]);
    return map;
}

test "journal root prefers XDG_DATA_HOME and falls back to HOME" {
    const gpa = testing.allocator;
    {
        var env = try envMap(gpa, &.{ .{ "HOME", "/home/x" }, .{ "XDG_DATA_HOME", "/data" } });
        defer env.deinit();
        const root = try defaultJournalRoot(gpa, &env);
        defer gpa.free(root);
        try testing.expectEqualStrings("/data/autojournal/journals", root);
    }
    {
        var env = try envMap(gpa, &.{.{ "HOME", "/home/x" }});
        defer env.deinit();
        const root = try defaultJournalRoot(gpa, &env);
        defer gpa.free(root);
        try testing.expectEqualStrings("/home/x/.local/share/autojournal/journals", root);
    }
    {
        // An empty XDG value is treated as unset, not as the root "/".
        var env = try envMap(gpa, &.{ .{ "HOME", "/home/x" }, .{ "XDG_DATA_HOME", "" } });
        defer env.deinit();
        const root = try defaultJournalRoot(gpa, &env);
        defer gpa.free(root);
        try testing.expectEqualStrings("/home/x/.local/share/autojournal/journals", root);
    }
    {
        // A relative XDG value is invalid per the spec. Resolving it against
        // the working directory would hand back a relative journal root, and
        // every consumer here opens roots as absolute paths.
        var env = try envMap(gpa, &.{ .{ "HOME", "/home/x" }, .{ "XDG_DATA_HOME", "cache/data" } });
        defer env.deinit();
        const root = try defaultJournalRoot(gpa, &env);
        defer gpa.free(root);
        try testing.expectEqualStrings("/home/x/.local/share/autojournal/journals", root);
        try testing.expect(std.fs.path.isAbsolute(root));

        const index_path = try defaultIndexPath(gpa, &env, root);
        defer gpa.free(index_path);
        try testing.expect(std.fs.path.isAbsolute(index_path));
    }
    {
        var env = try envMap(gpa, &.{});
        defer env.deinit();
        try testing.expectError(error.MissingHome, defaultJournalRoot(gpa, &env));
    }
}

test "index path is keyed by the root digest and sits outside the root" {
    const gpa = testing.allocator;
    var env = try envMap(gpa, &.{.{ "HOME", "/home/x" }});
    defer env.deinit();
    const path = try defaultIndexPath(gpa, &env, "/home/x/journals");
    defer gpa.free(path);
    const digest = rootDigestHex("/home/x/journals");
    const want = try std.fmt.allocPrint(gpa, "/home/x/.local/state/autojournal/index-{s}.sqlite", .{
        digest[0..digest_name_len],
    });
    defer gpa.free(want);
    try testing.expectEqualStrings(want, path);
    try testing.expectEqual(@as(u8, 0), path.ptr[path.len]);

    // Distinct roots never share a projection.
    const other = try defaultIndexPath(gpa, &env, "/home/x/other-journals");
    defer gpa.free(other);
    try testing.expect(!std.mem.eql(u8, path, other));
}

test "shared-directory guard judges the nearest existing ancestor" {
    const gpa = testing.allocator;
    const io = testing.io;
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    const base = try tmp.dir.realPathFileAlloc(io, ".", gpa);
    defer gpa.free(base);

    // A private ancestor passes, and a root that does not exist yet is
    // judged by the ancestor it would be created under.
    const private_root = try std.fmt.allocPrint(gpa, "{s}/private/journals", .{base});
    defer gpa.free(private_root);
    try tmp.dir.createDirPath(io, "private");
    try tmp.dir.setFilePermissions(io, "private", @enumFromInt(0o700), .{});
    try testing.expect(!rootInSharedDirectory(io, private_root));

    // A group- or world-writable ancestor is refused.
    const shared_root = try std.fmt.allocPrint(gpa, "{s}/shared/journals", .{base});
    defer gpa.free(shared_root);
    try tmp.dir.createDirPath(io, "shared");
    try tmp.dir.setFilePermissions(io, "shared", @enumFromInt(0o777), .{});
    try testing.expect(rootInSharedDirectory(io, shared_root));
}
