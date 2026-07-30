//! Store integration tests: real filesystem via std.testing.tmpDir.

const std = @import("std");
const contracts = @import("contracts.zig");
const identity = @import("identity.zig");
const render = @import("render.zig");
const store = @import("store.zig");

const capture_time_ms: u64 = 1785240000000;

fn testPayloadParsed(gpa: std.mem.Allocator) !std.json.Parsed(contracts.RawPayload) {
    return contracts.parsePayload(gpa, contracts.test_payload_json);
}

test "publish then redeliver: published, duplicate, conflict" {
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    var tmp = std.testing.tmpDir(.{});
    defer tmp.cleanup();

    const parsed = try testPayloadParsed(gpa);
    defer parsed.deinit();
    const payload = try contracts.validate(parsed.value);

    const first = try store.publish(tmp.dir, io, gpa, payload, capture_time_ms);
    defer first.deinit(gpa);
    try std.testing.expect(first.outcome == .published);

    // The published file exists at the reported path, is owner-only, and
    // carries the digest.
    const stat = try tmp.dir.statFile(io, first.rel_path, .{});
    try std.testing.expectEqual(
        @as(u32, 0o600),
        @as(u32, @intCast(@intFromEnum(stat.permissions) & 0o777)),
    );
    const content = try tmp.dir.readFileAlloc(io, first.rel_path, gpa, .limited(1 << 20));
    defer gpa.free(content);
    const on_disk_digest = render.frontmatterDigestHex(content) orelse
        return error.TestUnexpectedResult;
    try std.testing.expectEqualStrings(&first.digest_hex, on_disk_digest);

    // Exact redelivery (different capture time) is a duplicate success.
    const second = try store.publish(tmp.dir, io, gpa, payload, capture_time_ms + 5000);
    defer second.deinit(gpa);
    try std.testing.expect(second.outcome == .duplicate);
    try std.testing.expectEqualStrings(first.rel_path, second.rel_path);

    // Same identity, different body: typed conflict, original untouched.
    var altered = payload;
    altered.assistant_result = "tampered result";
    const third = try store.publish(tmp.dir, io, gpa, altered, capture_time_ms);
    defer third.deinit(gpa);
    try std.testing.expect(third.outcome == .conflict);
    const after = try tmp.dir.readFileAlloc(io, first.rel_path, gpa, .limited(1 << 20));
    defer gpa.free(after);
    try std.testing.expectEqualStrings(content, after);

    try std.testing.expectEqual(@as(u64, 1), store.countEpisodes(tmp.dir, io));
}

test "no temp files remain after duplicate and conflict outcomes" {
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    var tmp = std.testing.tmpDir(.{});
    defer tmp.cleanup();

    const parsed = try testPayloadParsed(gpa);
    defer parsed.deinit();
    const payload = try contracts.validate(parsed.value);

    const first = try store.publish(tmp.dir, io, gpa, payload, capture_time_ms);
    defer first.deinit(gpa);
    const dup = try store.publish(tmp.dir, io, gpa, payload, capture_time_ms + 1);
    defer dup.deinit(gpa);
    var altered = payload;
    altered.user_content = "other user text";
    const conflict = try store.publish(tmp.dir, io, gpa, altered, capture_time_ms + 2);
    defer conflict.deinit(gpa);

    const shard_rel = std.fs.path.dirname(first.rel_path).?;
    var shard_dir = try tmp.dir.openDir(io, shard_rel, .{ .iterate = true });
    defer shard_dir.close(io);
    var entries: u32 = 0;
    var it = shard_dir.iterate();
    while (try it.next(io)) |entry| {
        entries += 1;
        try std.testing.expect(std.mem.endsWith(u8, entry.name, ".md"));
    }
    try std.testing.expectEqual(@as(u32, 1), entries);
}

test "a colliding temp name retries and still classifies the final duplicate" {
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    var tmp = std.testing.tmpDir(.{});
    defer tmp.cleanup();

    const parsed = try testPayloadParsed(gpa);
    defer parsed.deinit();
    const payload = try contracts.validate(parsed.value);
    const first = try store.publish(tmp.dir, io, gpa, payload, capture_time_ms);
    defer first.deinit(gpa);

    const shard_rel = std.fs.path.dirname(first.rel_path).?;
    var shard = try tmp.dir.openDir(io, shard_rel, .{});
    defer shard.close(io);
    const retry_time = capture_time_ms + 1;
    const collision = try std.fmt.allocPrint(
        gpa,
        ".{s}.{d}.0.tmp",
        .{ &first.episode_id, retry_time },
    );
    defer gpa.free(collision);
    try shard.writeFile(io, .{ .sub_path = collision, .data = "other writer" });

    const second = try store.publish(tmp.dir, io, gpa, payload, retry_time);
    defer second.deinit(gpa);
    try std.testing.expect(second.outcome == .duplicate);
    try shard.deleteFile(io, collision);
}

test "different worlds and turns publish distinct episodes" {
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    var tmp = std.testing.tmpDir(.{});
    defer tmp.cleanup();

    const parsed = try testPayloadParsed(gpa);
    defer parsed.deinit();
    const payload = try contracts.validate(parsed.value);

    const a = try store.publish(tmp.dir, io, gpa, payload, capture_time_ms);
    defer a.deinit(gpa);
    var next_turn = payload;
    next_turn.turn_id = "turn-0008";
    const b = try store.publish(tmp.dir, io, gpa, next_turn, capture_time_ms);
    defer b.deinit(gpa);
    var other_world = payload;
    other_world.world = "otherworld";
    const c = try store.publish(tmp.dir, io, gpa, other_world, capture_time_ms);
    defer c.deinit(gpa);

    try std.testing.expect(a.outcome == .published);
    try std.testing.expect(b.outcome == .published);
    try std.testing.expect(c.outcome == .published);
    try std.testing.expectEqual(@as(u64, 3), store.countEpisodes(tmp.dir, io));
}

test "default classifications are elided and optional classifications add directories" {
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    var tmp = std.testing.tmpDir(.{});
    defer tmp.cleanup();

    const parsed = try testPayloadParsed(gpa);
    defer parsed.deinit();
    const base = try contracts.validate(parsed.value);

    var default = base;
    default.world = "main";
    default.scope = "default";
    default.turn_id = "layout-default";
    const a = try store.publish(tmp.dir, io, gpa, default, capture_time_ms);
    defer a.deinit(gpa);
    try std.testing.expectEqualStrings("2026/07/12", std.fs.path.dirname(a.rel_path).?);

    var scoped = default;
    scoped.scope = "project:a";
    scoped.turn_id = "layout-scope";
    const b = try store.publish(tmp.dir, io, gpa, scoped, capture_time_ms);
    defer b.deinit(gpa);
    try std.testing.expect(std.mem.startsWith(u8, b.rel_path, "scopes/project:a/2026/07/12/"));

    var world = default;
    world.world = "isolated-work";
    world.turn_id = "layout-world";
    const c = try store.publish(tmp.dir, io, gpa, world, capture_time_ms);
    defer c.deinit(gpa);
    try std.testing.expect(std.mem.startsWith(u8, c.rel_path, "worlds/isolated-work/2026/07/12/"));

    var lane = default;
    lane.lane = .evaluation;
    lane.turn_id = "layout-lane";
    const d = try store.publish(tmp.dir, io, gpa, lane, capture_time_ms);
    defer d.deinit(gpa);
    try std.testing.expect(std.mem.startsWith(u8, d.rel_path, "lanes/evaluation/2026/07/12/"));

    var combined = default;
    combined.world = "isolated-work";
    combined.scope = "client:a";
    combined.lane = .delegated_work;
    combined.turn_id = "layout-combined";
    const e = try store.publish(tmp.dir, io, gpa, combined, capture_time_ms);
    defer e.deinit(gpa);
    try std.testing.expect(std.mem.startsWith(
        u8,
        e.rel_path,
        "worlds/isolated-work/scopes/client:a/lanes/delegated_work/2026/07/12/",
    ));

    try std.testing.expectEqual(@as(u64, 5), store.countEpisodes(tmp.dir, io));
}

test "symlinked corpus component is a containment violation" {
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    var tmp = std.testing.tmpDir(.{});
    defer tmp.cleanup();

    const parsed = try testPayloadParsed(gpa);
    defer parsed.deinit();
    const payload = try contracts.validate(parsed.value);

    try tmp.dir.createDir(io, "worlds", store.dir_permissions);
    try tmp.dir.createDir(io, "elsewhere", store.dir_permissions);
    try tmp.dir.symLink(io, "../elsewhere", "worlds/testworld", .{});

    try std.testing.expectError(
        error.ContainmentViolation,
        store.publish(tmp.dir, io, gpa, payload, capture_time_ms),
    );
    // Nothing escaped into the link target.
    var elsewhere = try tmp.dir.openDir(io, "elsewhere", .{ .iterate = true });
    defer elsewhere.close(io);
    var it = elsewhere.iterate();
    try std.testing.expectEqual(@as(?std.Io.Dir.Entry, null), try it.next(io));
}

test "allocation failures propagate cleanly through parse and render" {
    try std.testing.checkAllAllocationFailures(std.testing.allocator, struct {
        fn run(gpa: std.mem.Allocator) !void {
            const parsed = contracts.parsePayload(gpa, contracts.test_payload_json) catch |err|
                switch (err) {
                    error.OutOfMemory => return error.OutOfMemory,
                    else => unreachable,
                };
            defer parsed.deinit();
            const p = contracts.validate(parsed.value) catch unreachable;
            const id = identity.episodeId(p);
            const digest = identity.payloadDigestHex(p);
            const content = try render.render(gpa, .{
                .payload = p,
                .episode_id = &id,
                .digest_hex = &digest,
                .capture_time_ms = 1,
            });
            gpa.free(content);
        }
    }.run, .{});
}
