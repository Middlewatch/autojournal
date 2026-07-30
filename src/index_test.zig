//! Index projection tests: live capture upsert, sync/rebuild, repair.

const std = @import("std");
const contracts = @import("contracts.zig");
const store = @import("store.zig");
const index = @import("index.zig");

const capture_time_ms: u64 = 1785240000000;

fn publishTwo(
    gpa: std.mem.Allocator,
    io: std.Io,
    root: std.Io.Dir,
    payload: contracts.Payload,
) ![2]store.Published {
    const a = try store.publish(root, io, gpa, payload, capture_time_ms);
    errdefer a.deinit(gpa);
    var next = payload;
    next.turn_id = "turn-0099";
    const b = try store.publish(root, io, gpa, next, capture_time_ms);
    return .{ a, b };
}

test "capture upsert, rebuild from corpus, and gone-file repair agree" {
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    var tmp = std.testing.tmpDir(.{});
    defer tmp.cleanup();

    const parsed = try contracts.parsePayload(gpa, contracts.test_payload_json);
    defer parsed.deinit();
    const payload = try contracts.validate(parsed.value);

    const published = try publishTwo(gpa, io, tmp.dir, payload);
    defer for (&published) |*p| p.deinit(gpa);

    var idx = try index.Index.open(gpa, ":memory:", null);
    defer idx.close();

    // Live-capture path: index one episode from its rendered content.
    try idx.indexEpisode(published[0].rel_path, published[0].content);
    try std.testing.expectEqual(@as(u64, 1), try idx.episodeCount());

    // Sync discovers the second episode and keeps the first (idempotent).
    const first_sync = try idx.syncFromCorpus(tmp.dir, io, gpa);
    try std.testing.expectEqual(@as(u64, 2), first_sync.indexed);
    try std.testing.expectEqual(@as(u64, 0), first_sync.removed);
    try std.testing.expectEqual(@as(u64, 0), first_sync.skipped_malformed);
    try std.testing.expectEqual(@as(u64, 2), try idx.episodeCount());

    // A malformed file is excluded and counted, never merged by filename.
    const shard_rel = std.fs.path.dirname(published[0].rel_path).?;
    var shard = try tmp.dir.openDir(io, shard_rel, .{});
    defer shard.close(io);
    try shard.writeFile(io, .{ .sub_path = "aj1-junk.md", .data = "not an episode" });

    // Deleting a source file removes its row on the next sync.
    try tmp.dir.deleteFile(io, published[1].rel_path);
    const second_sync = try idx.syncFromCorpus(tmp.dir, io, gpa);
    try std.testing.expectEqual(@as(u64, 1), second_sync.indexed);
    try std.testing.expectEqual(@as(u64, 1), second_sync.removed);
    try std.testing.expectEqual(@as(u64, 1), second_sync.skipped_malformed);
    try std.testing.expectEqual(@as(u64, 1), try idx.episodeCount());
}

test "hand-corrupted stored rows are rejected as Corrupt, never misread" {
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    var tmp = std.testing.tmpDir(.{});
    defer tmp.cleanup();

    const parsed = try contracts.parsePayload(gpa, contracts.test_payload_json);
    defer parsed.deinit();
    const payload = try contracts.validate(parsed.value);
    const published = try store.publish(tmp.dir, io, gpa, payload, capture_time_ms);
    defer published.deinit(gpa);

    var idx = try index.Index.open(gpa, ":memory:", null);
    defer idx.close();
    try idx.indexEpisode(published.rel_path, published.content);

    // line_no above the write-side u32 bound: the posting row is Corrupt,
    // not a safe-build panic.
    try idx.handle.exec("UPDATE postings SET line_no = 5000000000;");
    {
        var rows = try idx.postingsForTerm(
            gpa,
            "tests",
            payload.world,
            null,
            &.{.conversation},
        );
        defer rows.deinit();
        try std.testing.expectError(error.Corrupt, rows.next());
    }

    // A lane outside the closed enum: the episode row is Corrupt, not
    // silently defaulted to a real lane.
    try idx.handle.exec("UPDATE episodes SET lane = 'gossip';");
    try std.testing.expectError(
        error.Corrupt,
        idx.lookupEpisode(gpa, &published.episode_id),
    );
}

test "sync keeps one copy of a duplicated episode id and skips dot-directories" {
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    var tmp = std.testing.tmpDir(.{});
    defer tmp.cleanup();

    const parsed = try contracts.parsePayload(gpa, contracts.test_payload_json);
    defer parsed.deinit();
    const payload = try contracts.validate(parsed.value);
    const published = try store.publish(tmp.dir, io, gpa, payload, capture_time_ms);
    defer published.deinit(gpa);

    try tmp.dir.createDir(io, "copies", store.dir_permissions);
    var copies = try tmp.dir.openDir(io, "copies", .{});
    defer copies.close(io);
    const name = std.fs.path.basename(published.rel_path);
    try copies.writeFile(io, .{ .sub_path = name, .data = published.content });

    // Foreign tooling state stays invisible even when it contains an
    // episode-shaped copy.
    try tmp.dir.createDir(io, ".obsidian", store.dir_permissions);
    var hidden = try tmp.dir.openDir(io, ".obsidian", .{});
    defer hidden.close(io);
    try hidden.writeFile(io, .{ .sub_path = name, .data = published.content });

    var idx = try index.Index.open(gpa, ":memory:", null);
    defer idx.close();
    const report = try idx.syncFromCorpus(tmp.dir, io, gpa);
    try std.testing.expectEqual(@as(u64, 1), report.indexed);
    try std.testing.expectEqual(@as(u64, 1), report.duplicate_ids);
    try std.testing.expectEqual(@as(u64, 1), try idx.episodeCount());
    const row = (try idx.lookupEpisode(gpa, &published.episode_id)) orelse
        return error.TestUnexpectedResult;
    defer row.deinit(gpa);
}

fn dfOf(idx: *index.Index, world: []const u8, term: []const u8) !?i64 {
    var st = try idx.handle.prepare("SELECT df FROM term_stats WHERE world = ?1 AND term = ?2;");
    defer st.finalize();
    try st.bindText(1, world);
    try st.bindText(2, term);
    if (!try st.step()) return null;
    return st.columnInt64(0);
}

/// Ordered byte snapshot of all retrieval state; two projections built by
/// different paths must agree byte-for-byte.
fn snapshotRetrieval(idx: *index.Index, gpa: std.mem.Allocator) ![]u8 {
    var out: std.ArrayList(u8) = .empty;
    errdefer out.deinit(gpa);
    const queries = [_][:0]const u8{
        "SELECT group_concat(term || '|' || episode_id || '|' || line_no, ';') FROM (SELECT * FROM postings ORDER BY term, episode_id, line_no);",
        "SELECT group_concat(world || '|' || term || '|' || df || '|' || eval_df, ';') FROM (SELECT * FROM term_stats ORDER BY world, term);",
        "SELECT group_concat(episode_id || '|' || digest_hex || '|' || body_line || '|' || lane, ';') FROM (SELECT * FROM episodes ORDER BY episode_id);",
    };
    for (queries) |q| {
        var st = try idx.handle.prepare(q);
        defer st.finalize();
        if (try st.step()) try out.appendSlice(gpa, st.columnText(0));
        try out.append(gpa, '\n');
    }
    return out.toOwnedSlice(gpa);
}

/// Three-episode corpus with distinct bodies: two conversation lanes and
/// one evaluation lane, so document frequencies are non-degenerate and the
/// evaluation exclusion is observable.
fn publishDistinctCorpus(
    gpa: std.mem.Allocator,
    io: std.Io,
    root: std.Io.Dir,
    base: contracts.Payload,
) ![3]store.Published {
    var p1 = base;
    p1.turn_id = "turn-1001";
    p1.user_content = "the zebra crossed near the quokka enclosure";
    const a = try store.publish(root, io, gpa, p1, capture_time_ms);
    errdefer a.deinit(gpa);
    var p2 = base;
    p2.turn_id = "turn-1002";
    p2.user_content = "quokka feeding schedule and wombat burrows";
    const b = try store.publish(root, io, gpa, p2, capture_time_ms);
    errdefer b.deinit(gpa);
    var p3 = base;
    p3.turn_id = "turn-1003";
    p3.lane = .evaluation;
    p3.user_content = "sealed zebra evaluation phrase";
    const c = try store.publish(root, io, gpa, p3, capture_time_ms);
    return .{ a, b, c };
}

test "postings and df track sync, lane exclusion, and removal" {
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    var tmp = std.testing.tmpDir(.{});
    defer tmp.cleanup();

    const parsed = try contracts.parsePayload(gpa, contracts.test_payload_json);
    defer parsed.deinit();
    const payload = try contracts.validate(parsed.value);
    const published = try publishDistinctCorpus(gpa, io, tmp.dir, payload);
    defer for (&published) |*p| p.deinit(gpa);

    var idx = try index.Index.open(gpa, ":memory:", null);
    defer idx.close();
    const report = try idx.syncFromCorpus(tmp.dir, io, gpa);
    try std.testing.expectEqual(@as(u64, 3), report.indexed);

    // Evaluation lane is excluded from document frequencies and N.
    try std.testing.expectEqual(@as(?i64, 1), try dfOf(&idx, "testworld", "zebra"));
    try std.testing.expectEqual(@as(?i64, 2), try dfOf(&idx, "testworld", "quokka"));
    try std.testing.expectEqual(@as(?i64, 1), try dfOf(&idx, "testworld", "wombat"));
    try std.testing.expectEqual(@as(u64, 2), try idx.statsEpisodeCount("testworld"));
    try std.testing.expectEqual(@as(u64, 3), try idx.episodeCount());

    // Default lanes never see the evaluation posting; asking for the
    // evaluation lane explicitly does.
    {
        var it = try idx.postingsForTerm(gpa, "zebra", "testworld", null, &.{
            .conversation, .delegated_work, .imported_legacy,
        });
        defer it.deinit();
        var count: usize = 0;
        while (try it.next()) |row| : (count += 1) {
            try std.testing.expectEqual(contracts.Lane.conversation, row.lane);
            try std.testing.expect(row.line_no >= row.body_line);
        }
        try std.testing.expectEqual(@as(usize, 1), count);
    }
    {
        var it = try idx.postingsForTerm(gpa, "zebra", "testworld", null, &.{.evaluation});
        defer it.deinit();
        const row = (try it.next()) orelse return error.TestUnexpectedResult;
        try std.testing.expectEqual(contracts.Lane.evaluation, row.lane);
    }

    // Removing an episode file decrements its terms and drops emptied ones.
    try tmp.dir.deleteFile(io, published[1].rel_path);
    const second = try idx.syncFromCorpus(tmp.dir, io, gpa);
    try std.testing.expectEqual(@as(u64, 1), second.removed);
    try std.testing.expectEqual(@as(?i64, 1), try dfOf(&idx, "testworld", "quokka"));
    try std.testing.expectEqual(@as(?i64, null), try dfOf(&idx, "testworld", "wombat"));
}

test "sync-rebuilt projection matches the incrementally built one" {
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    var tmp = std.testing.tmpDir(.{});
    defer tmp.cleanup();

    const parsed = try contracts.parsePayload(gpa, contracts.test_payload_json);
    defer parsed.deinit();
    const payload = try contracts.validate(parsed.value);
    const published = try publishDistinctCorpus(gpa, io, tmp.dir, payload);
    defer for (&published) |*p| p.deinit(gpa);

    var incremental = try index.Index.open(gpa, ":memory:", null);
    defer incremental.close();
    for (&published) |*p| try incremental.indexEpisode(p.rel_path, p.content);

    var rebuilt = try index.Index.open(gpa, ":memory:", null);
    defer rebuilt.close();
    _ = try rebuilt.syncFromCorpus(tmp.dir, io, gpa);

    const a = try snapshotRetrieval(&incremental, gpa);
    defer gpa.free(a);
    const b = try snapshotRetrieval(&rebuilt, gpa);
    defer gpa.free(b);
    try std.testing.expectEqualStrings(a, b);

    // Re-indexing the same content is idempotent, including df.
    try incremental.indexEpisode(published[0].rel_path, published[0].content);
    const c = try snapshotRetrieval(&incremental, gpa);
    defer gpa.free(c);
    try std.testing.expectEqualStrings(a, c);
}

test "version mismatch disposes every table, including unknown leftovers" {
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    var tmp = std.testing.tmpDir(.{});
    defer tmp.cleanup();
    const dir_path = try tmp.dir.realPathFileAlloc(io, ".", gpa);
    defer gpa.free(dir_path);
    const db_path = try std.fmt.allocPrintSentinel(gpa, "{s}/index.sqlite", .{dir_path}, 0);
    defer gpa.free(db_path);

    {
        const db = @import("db.zig");
        var raw = try db.Db.open(db_path);
        defer raw.close();
        try raw.exec(
            \\CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
            \\INSERT INTO meta VALUES ('index_schema_version', '1');
            \\CREATE TABLE episodes (episode_id TEXT PRIMARY KEY);
            \\INSERT INTO episodes VALUES ('aj1-old');
            \\CREATE TABLE leftover_from_the_future (x INTEGER);
        );
    }

    var idx = try index.Index.open(gpa, db_path, null);
    defer idx.close();
    try std.testing.expectEqual(@as(u64, 0), try idx.episodeCount());
    var st = try idx.handle.prepare(
        "SELECT COUNT(*) FROM sqlite_master WHERE name = 'leftover_from_the_future';",
    );
    defer st.finalize();
    try std.testing.expect(try st.step());
    try std.testing.expectEqual(@as(i64, 0), st.columnInt64(0));
}

test "current-version reopen preserves data; foreign root is rejected" {
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    var tmp = std.testing.tmpDir(.{});
    defer tmp.cleanup();
    const dir_path = try tmp.dir.realPathFileAlloc(io, ".", gpa);
    defer gpa.free(dir_path);
    const db_path = try std.fmt.allocPrintSentinel(gpa, "{s}/index.sqlite", .{dir_path}, 0);
    defer gpa.free(db_path);

    const digest_a = "a" ** 64;
    const digest_b = "b" ** 64;
    {
        var idx = try index.Index.open(gpa, db_path, digest_a);
        defer idx.close();
        try idx.upsert(.{
            .episode_id = "aj1-persist",
            .digest_hex = "0" ** 64,
            .rel_path = "worlds/x/2026/07/28/aj1-persist.md",
            .world = "x",
            .scope = "global",
            .lane = .conversation,
            .harness = "h",
            .session_id = "s",
            .turn_id = "t",
            .event_time_ms = 1,
            .capture_time_ms = 2,
            .capture_policy = "p",
            .turn_outcome = "completed",
        });
    }
    {
        // Same root: data survives the reopen (no silent disposal).
        var idx = try index.Index.open(gpa, db_path, digest_a);
        defer idx.close();
        try std.testing.expectEqual(@as(u64, 1), try idx.episodeCount());
    }
    // Another root's digest is a foreign index, never an empty corpus.
    try std.testing.expectError(error.ForeignIndex, index.Index.open(gpa, db_path, digest_b));
}

test "empty corpus sync empties the projection" {
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    var tmp = std.testing.tmpDir(.{});
    defer tmp.cleanup();

    var idx = try index.Index.open(gpa, ":memory:", null);
    defer idx.close();
    try idx.upsert(.{
        .episode_id = "aj1-stale",
        .digest_hex = "0" ** 64,
        .rel_path = "worlds/x/2026/07/28/aj1-stale.md",
        .world = "x",
        .scope = "global",
        .lane = .conversation,
        .harness = "h",
        .session_id = "s",
        .turn_id = "t",
        .event_time_ms = 1,
        .capture_time_ms = 2,
        .capture_policy = "p",
        .turn_outcome = "completed",
    });
    const report = try idx.syncFromCorpus(tmp.dir, io, gpa);
    try std.testing.expectEqual(@as(u64, 0), report.indexed);
    try std.testing.expectEqual(@as(u64, 0), try idx.episodeCount());
}
