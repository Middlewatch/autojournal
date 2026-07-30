//! Journal maintenance: the two whole operations a host exposes to its owner
//! beyond capture and recall.
//!
//! `status` and `sync` are more than thin wrappers over `store` and `index` —
//! each carries accounting that is easy to get subtly wrong. Freshness must
//! add deliberate exclusions to the indexed count or a corpus with one
//! duplicate reads as permanently stale; sync must re-stamp the root identity
//! and record its exclusion count or the next status contradicts it. Those
//! rules live here so the owner CLI and an embedding host cannot disagree
//! about whether memory is healthy.

const std = @import("std");

const contracts = @import("contracts.zig");
const index_mod = @import("index.zig");
const paths = @import("paths.zig");
const store = @import("store.zig");

const Io = std.Io;

pub const Status = struct {
    /// False when the journal root does not exist yet. Not an error: a
    /// harness that has captured nothing has no root, and reporting zero
    /// episodes against a missing root is the honest answer.
    root_ok: bool,
    /// Episode files found by walking the corpus.
    episodes: u64,
    /// Episode rows the projection holds.
    indexed: u64,
    freshness: contracts.IndexFreshness,

    /// True when the projection can answer recall completely. `stale` and
    /// `unavailable` both mean a `sync` is owed.
    pub fn healthy(self: Status) bool {
        return self.freshness == .fresh;
    }
};

/// Read-only: never creates the root, the index, or their parents, so a
/// status check cannot itself change what it reports.
pub fn status(
    gpa: std.mem.Allocator,
    io: Io,
    root_path: []const u8,
    index_path: []const u8,
) Status {
    var root = Io.Dir.openDirAbsolute(io, root_path, .{}) catch return .{
        .root_ok = false,
        .episodes = 0,
        .indexed = 0,
        .freshness = .not_built,
    };
    defer root.close(io);
    const episodes = store.countEpisodes(root, io);

    // A missing database is `not_built`, never mistaken for empty memory.
    _ = Io.Dir.cwd().statFile(io, index_path, .{}) catch return .{
        .root_ok = true,
        .episodes = episodes,
        .indexed = 0,
        .freshness = .not_built,
    };
    const unavailable: Status = .{
        .root_ok = true,
        .episodes = episodes,
        .indexed = 0,
        .freshness = .unavailable,
    };
    const path_z = gpa.dupeZ(u8, index_path) catch return unavailable;
    defer gpa.free(path_z);
    const digest = paths.rootDigestHex(root_path);
    var idx = index_mod.Index.open(gpa, path_z, digest[0..]) catch return unavailable;
    defer idx.close();
    const indexed = idx.episodeCount() catch 0;
    return .{
        .root_ok = true,
        .episodes = episodes,
        .indexed = indexed,
        // Files the last sync deliberately excluded (duplicate ids,
        // malformed) count as accounted for, or they read as staleness
        // that no sync can ever clear.
        .freshness = if (indexed + idx.excludedCount() == episodes) .fresh else .stale,
    };
}

pub const SyncError = error{
    /// The journal root sits under a group- or world-writable directory.
    SharedDirectory,
    /// The root does not exist; there is nothing to sync.
    RootMissing,
    /// The projection could not be opened or its permissions not narrowed.
    IndexUnavailable,
    /// The rebuild failed and was rolled back; the projection is unchanged.
    SyncFailed,
    OutOfMemory,
};

/// Rebuilds the projection from the corpus and re-stamps its identity.
///
/// Opened without the foreign-root gate on purpose: sync replaces whatever
/// projection is at `index_path` with this root's content, which is the
/// documented way to repoint an index.
pub fn sync(
    gpa: std.mem.Allocator,
    io: Io,
    root_path: []const u8,
    index_path: []const u8,
) SyncError!index_mod.Index.SyncReport {
    if (paths.rootInSharedDirectory(io, root_path)) return error.SharedDirectory;
    var root = Io.Dir.openDirAbsolute(io, root_path, .{ .iterate = true }) catch
        return error.RootMissing;
    defer root.close(io);
    root.setPermissions(io, @enumFromInt(0o700)) catch return error.IndexUnavailable;

    var idx = index_mod.Index.openHardened(gpa, io, index_path, null) catch
        return error.IndexUnavailable;
    defer idx.close();

    const report = idx.syncFromCorpus(root, io, gpa) catch return error.SyncFailed;
    const digest = paths.rootDigestHex(root_path);
    idx.metaSet("root_digest", &digest) catch {};
    var excluded_buf: [20]u8 = undefined;
    const excluded = std.fmt.bufPrint(&excluded_buf, "{d}", .{
        report.duplicate_ids + report.skipped_malformed,
    }) catch unreachable;
    idx.metaSet("sync_excluded", excluded) catch {};
    index_mod.Index.hardenFiles(gpa, io, index_path) catch return error.IndexUnavailable;
    return report;
}

// --- tests ---

const testing = std.testing;

test "status reports not_built for a root that does not exist" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    const gpa = testing.allocator;
    const io = testing.io;
    const base = try tmp.dir.realPathFileAlloc(io, ".", gpa);
    defer gpa.free(base);
    const root_path = try std.fmt.allocPrint(gpa, "{s}/absent", .{base});
    defer gpa.free(root_path);
    const index_path = try std.fmt.allocPrint(gpa, "{s}/index.sqlite", .{base});
    defer gpa.free(index_path);

    const report = status(gpa, io, root_path, index_path);
    try testing.expect(!report.root_ok);
    try testing.expect(!report.healthy());
    try testing.expectEqual(contracts.IndexFreshness.not_built, report.freshness);
}

test "sync refuses a journal root under a shared directory" {
    var tmp = testing.tmpDir(.{});
    defer tmp.cleanup();
    const gpa = testing.allocator;
    const io = testing.io;
    const base = try tmp.dir.realPathFileAlloc(io, ".", gpa);
    defer gpa.free(base);
    try tmp.dir.createDirPath(io, "shared/journals");
    try tmp.dir.setFilePermissions(io, "shared", @enumFromInt(0o777), .{});
    const root_path = try std.fmt.allocPrint(gpa, "{s}/shared/journals", .{base});
    defer gpa.free(root_path);
    const index_path = try std.fmt.allocPrint(gpa, "{s}/index.sqlite", .{base});
    defer gpa.free(index_path);

    try testing.expectError(error.SharedDirectory, sync(gpa, io, root_path, index_path));
}
