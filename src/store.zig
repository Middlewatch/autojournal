//! Atomic episode publication under a contained journal root.
//!
//! Default layout: YYYY/MM/DD/<episode-id>.md. Non-default classifications
//! add reserved worlds/, scopes/, and lanes/ components before the date.
//! Each episode is one
//! immutable file per completed turn. Publication is: exclusive owner-only
//! temp file in the target directory → write → fsync → atomic no-replace
//! rename → parent directory fsync. An existing target is validated by
//! digest: exact duplicate is success, mismatch is a typed conflict.
//!
//! Containment: every component below the root is either a validated token
//! or generated here, and every descent refuses to follow symlinks, so a
//! link planted inside the corpus cannot redirect writes outside it.

const std = @import("std");
const Io = std.Io;
const contracts = @import("contracts.zig");
const identity = @import("identity.zig");
const render = @import("render.zig");

pub const dir_permissions: Io.File.Permissions = @enumFromInt(0o700);
pub const file_permissions: Io.File.Permissions = @enumFromInt(0o600);

pub const PublishError = error{
    /// A path component inside the corpus is a symlink or not a directory.
    ContainmentViolation,
    PermissionDenied,
    /// I/O failure or sync uncertainty; the caller may retry — idempotency
    /// makes redelivery safe.
    Unavailable,
    OutOfMemory,
};

pub const Published = struct {
    outcome: enum { published, duplicate, conflict },
    episode_id: [identity.episode_id_len]u8,
    digest_hex: [identity.digest_hex_len]u8,
    /// Owned by the caller's allocator: path relative to the journal root.
    rel_path: []u8,
    /// Owned by the caller's allocator: the rendered episode bytes, so the
    /// capture path can index without re-reading the file it just wrote.
    content: []u8,

    pub fn deinit(self: *const Published, gpa: std.mem.Allocator) void {
        gpa.free(self.rel_path);
        gpa.free(self.content);
    }
};

/// Publishes one validated payload into the journal root. The root `Dir` is
/// the configured journals root (already opened); the world subtree is
/// created on demand with owner-only permissions.
pub fn publish(
    root: Io.Dir,
    io: Io,
    gpa: std.mem.Allocator,
    payload: contracts.Payload,
    capture_time_ms: u64,
) PublishError!Published {
    const episode_id = identity.episodeId(payload);
    const digest_hex = identity.payloadDigestHex(payload);

    const content = render.render(gpa, .{
        .payload = payload,
        .episode_id = &episode_id,
        .digest_hex = &digest_hex,
        .capture_time_ms = capture_time_ms,
    }) catch return error.OutOfMemory;
    errdefer gpa.free(content);

    var shard: ShardPath = .init(payload.event_time_ms);
    var component_buf: [9][]const u8 = undefined;
    const components = layoutComponents(payload, &shard, &component_buf);
    var episode_dir = try openComponents(root, io, components);
    defer episode_dir.close(io);

    var final_name_buf: [identity.episode_id_len + 3]u8 = undefined;
    const final_name = std.fmt.bufPrint(&final_name_buf, "{s}.md", .{&episode_id}) catch unreachable;

    // `.` + id + `.` + up to 20 decimal ms digits + `.tmp` = id + 26; 48
    // leaves slack so the `catch unreachable` below stays trivially safe.
    var tmp_name_buf: [identity.episode_id_len + 56]u8 = undefined;
    var tmp_name: []const u8 = "";
    for (0..64) |attempt| {
        tmp_name = std.fmt.bufPrint(&tmp_name_buf, ".{s}.{d}.{d}.tmp", .{
            &episode_id, capture_time_ms, attempt,
        }) catch unreachable;
        writeTemp(episode_dir, io, tmp_name, content) catch |err| switch (err) {
            error.TempCollision => continue,
            error.ContainmentViolation => return error.ContainmentViolation,
            error.OutOfMemory => return error.OutOfMemory,
            error.PermissionDenied => return error.PermissionDenied,
            error.Unavailable => return error.Unavailable,
        };
        break;
    } else return error.Unavailable;
    var tmp_live = true;
    defer if (tmp_live) episode_dir.deleteFile(io, tmp_name) catch {};

    var outcome: @FieldType(Published, "outcome") = .published;
    episode_dir.renamePreserve(tmp_name, episode_dir, final_name, io) catch |err| switch (err) {
        error.PathAlreadyExists => {
            outcome = try classifyExisting(episode_dir, io, gpa, final_name, &digest_hex);
        },
        error.AccessDenied, error.PermissionDenied => return error.PermissionDenied,
        else => return error.Unavailable,
    };
    if (outcome == .published) tmp_live = false;

    // Make the directory entry durable before reporting success.
    syncDir(episode_dir, io) catch return error.Unavailable;

    var path_buf: [10][]const u8 = undefined;
    @memcpy(path_buf[0..components.len], components);
    path_buf[components.len] = final_name;
    const rel_path = std.mem.join(gpa, "/", path_buf[0 .. components.len + 1]) catch
        return error.OutOfMemory;

    return .{
        .outcome = outcome,
        .episode_id = episode_id,
        .digest_hex = digest_hex,
        .rel_path = rel_path,
        .content = content,
    };
}

/// `YYYY/MM/DD` derived from the source event time.
pub const ShardPath = struct {
    buf: [10]u8,

    pub fn init(event_time_ms: u64) ShardPath {
        const secs = std.time.epoch.EpochSeconds{ .secs = event_time_ms / 1000 };
        const year_day = secs.getEpochDay().calculateYearDay();
        const month_day = year_day.calculateMonthDay();
        var self: ShardPath = .{ .buf = undefined };
        _ = std.fmt.bufPrint(&self.buf, "{d:0>4}/{d:0>2}/{d:0>2}", .{
            @as(u16, year_day.year),
            @as(u8, month_day.month.numeric()),
            @as(u8, month_day.day_index) + 1,
        }) catch unreachable;
        return self;
    }

    pub fn slice(self: *const ShardPath) []const u8 {
        return &self.buf;
    }
};

fn layoutComponents(
    payload: contracts.Payload,
    shard: *const ShardPath,
    buf: *[9][]const u8,
) []const []const u8 {
    var len: usize = 0;
    if (!std.mem.eql(u8, payload.world, "main")) {
        buf[len] = "worlds";
        len += 1;
        buf[len] = payload.world;
        len += 1;
    }
    if (!std.mem.eql(u8, payload.scope, "default")) {
        buf[len] = "scopes";
        len += 1;
        buf[len] = payload.scope;
        len += 1;
    }
    if (payload.lane != .conversation) {
        buf[len] = "lanes";
        len += 1;
        buf[len] = @tagName(payload.lane);
        len += 1;
    }
    buf[len] = shard.buf[0..4];
    len += 1;
    buf[len] = shard.buf[5..7];
    len += 1;
    buf[len] = shard.buf[8..10];
    len += 1;
    return buf[0..len];
}

fn openComponents(root: Io.Dir, io: Io, components: []const []const u8) PublishError!Io.Dir {
    var current = root;
    var owns_current = false;
    errdefer if (owns_current) current.close(io);
    for (components) |component| {
        const next = try openOrCreateChild(current, io, component);
        if (owns_current) current.close(io);
        current = next;
        owns_current = true;
    }
    return current;
}

/// Opens a direct child directory without following symlinks, creating it
/// owner-only if absent. Concurrent creators are tolerated.
fn openOrCreateChild(parent: Io.Dir, io: Io, name: []const u8) PublishError!Io.Dir {
    const open_options: Io.Dir.OpenOptions = .{
        .follow_symlinks = false,
        .iterate = true,
    };
    var child = parent.openDir(io, name, open_options) catch |err| switch (err) {
        error.FileNotFound => blk: {
            parent.createDir(io, name, dir_permissions) catch |create_err| switch (create_err) {
                error.PathAlreadyExists => {},
                error.AccessDenied, error.PermissionDenied => return error.PermissionDenied,
                else => return error.Unavailable,
            };
            break :blk parent.openDir(io, name, open_options) catch |reopen_err|
                switch (reopen_err) {
                    error.NotDir, error.SymLinkLoop => return error.ContainmentViolation,
                    error.AccessDenied, error.PermissionDenied => return error.PermissionDenied,
                    else => return error.Unavailable,
                };
        },
        error.NotDir, error.SymLinkLoop => return error.ContainmentViolation,
        error.AccessDenied, error.PermissionDenied => return error.PermissionDenied,
        else => return error.Unavailable,
    };
    child.setPermissions(io, dir_permissions) catch |err| {
        child.close(io);
        return switch (err) {
            error.AccessDenied, error.PermissionDenied => error.PermissionDenied,
            else => error.Unavailable,
        };
    };
    return child;
}

const WriteTempError = PublishError || error{TempCollision};

fn writeTemp(dir: Io.Dir, io: Io, tmp_name: []const u8, content: []const u8) WriteTempError!void {
    var file = dir.createFile(io, tmp_name, .{
        .exclusive = true,
        .permissions = file_permissions,
        .resolve_beneath = true,
    }) catch |err| switch (err) {
        error.PathAlreadyExists => return error.TempCollision,
        error.AccessDenied, error.PermissionDenied => return error.PermissionDenied,
        else => return error.Unavailable,
    };
    // A failed write must remove the temp it created: the name embeds
    // `capture_time_ms`, so an orphan would make an identical redelivery
    // fail its exclusive create forever.
    errdefer {
        file.close(io);
        dir.deleteFile(io, tmp_name) catch {};
    }
    file.writeStreamingAll(io, content) catch return error.Unavailable;
    file.sync(io) catch return error.Unavailable;
    file.close(io);
}

fn classifyExisting(
    dir: Io.Dir,
    io: Io,
    gpa: std.mem.Allocator,
    final_name: []const u8,
    digest_hex: *const [identity.digest_hex_len]u8,
) PublishError!@FieldType(Published, "outcome") {
    var file = dir.openFile(io, final_name, .{}) catch |err| switch (err) {
        error.AccessDenied, error.PermissionDenied => return error.PermissionDenied,
        else => return error.Unavailable,
    };
    defer file.close(io);
    file.setPermissions(io, file_permissions) catch |err| switch (err) {
        error.AccessDenied, error.PermissionDenied => return error.PermissionDenied,
        else => return error.Unavailable,
    };
    const existing = dir.readFileAlloc(io, final_name, gpa, .limited(contracts.max_episode_file_bytes)) catch |err|
        switch (err) {
            error.OutOfMemory => return error.OutOfMemory,
            error.AccessDenied, error.PermissionDenied => return error.PermissionDenied,
            else => return error.Unavailable,
        };
    defer gpa.free(existing);
    const existing_digest = render.frontmatterDigestHex(existing) orelse return .conflict;
    return if (std.mem.eql(u8, existing_digest, digest_hex)) .duplicate else .conflict;
}

fn syncDir(dir: Io.Dir, io: Io) !void {
    var dir_file = try dir.openFile(io, ".", .{});
    defer dir_file.close(io);
    try dir_file.sync(io);
}

/// Counts authoritative-looking episode files under the journal root.
/// Diagnostics only; malformed candidates are excluded by sync.
pub fn countEpisodes(root: Io.Dir, io: Io) u64 {
    var corpus = root.openDir(io, ".", .{
        .follow_symlinks = false,
        .iterate = true,
    }) catch return 0;
    defer corpus.close(io);
    return countIn(corpus, io, contracts.corpus_walk_depth);
}

fn countIn(dir: Io.Dir, io: Io, depth_left: u8) u64 {
    var total: u64 = 0;
    var it = dir.iterate();
    while (it.next(io) catch return total) |entry| {
        switch (entry.kind) {
            .file => if (std.mem.startsWith(u8, entry.name, "aj1-") and
                std.mem.endsWith(u8, entry.name, ".md"))
            {
                total += 1;
            },
            .directory => if (depth_left > 0) {
                // Same visibility rule as the index walk: dot-directories
                // are foreign tooling state, never episode shards.
                if (entry.name.len == 0 or entry.name[0] == '.') continue;
                var child = dir.openDir(io, entry.name, .{
                    .follow_symlinks = false,
                    .iterate = true,
                }) catch continue;
                defer child.close(io);
                total += countIn(child, io, depth_left - 1);
            },
            else => {},
        }
    }
    return total;
}
