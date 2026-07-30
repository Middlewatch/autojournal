//! End-to-end retrieval tests: corpus on disk, SQLite projection, search
//! orchestration, evidence opening. Frozen clock throughout.

const std = @import("std");
const contracts = @import("contracts.zig");
const store = @import("store.zig");
const index = @import("index.zig");
const search = @import("search.zig");
const aliases = @import("aliases.zig");
const retrieval = @import("retrieval.zig");
const render = @import("render.zig");
const identity = @import("identity.zig");

const now_ms: u64 = 1785240000000;

const Fixture = struct {
    tmp: std.testing.TmpDir,
    idx: index.Index,
    published: [4]store.Published,
    empty_map: aliases.AliasMap,

    fn deinit(self: *Fixture, gpa: std.mem.Allocator) void {
        self.empty_map.deinit();
        for (&self.published) |*p| p.deinit(gpa);
        self.idx.close();
        self.tmp.cleanup();
    }

    fn request(self: *const Fixture, query: []const u8) search.SearchRequest {
        _ = self;
        return .{ .query = query, .world = "testworld", .now_ms = now_ms };
    }
};

/// Four episodes: two conversation, one delegated, one evaluation, with
/// distinct multi-line bodies so ranking, context windows, and lane
/// filters are all observable.
fn setup(gpa: std.mem.Allocator, io: std.Io) !Fixture {
    var tmp = std.testing.tmpDir(.{});
    errdefer tmp.cleanup();

    const parsed = try contracts.parsePayload(gpa, contracts.test_payload_json);
    defer parsed.deinit();
    const base = try contracts.validate(parsed.value);

    var p1 = base;
    p1.turn_id = "turn-2001";
    p1.user_content = "context before\nthe quokka enclosure needed reindexing today\ncontext after";
    p1.assistant_result = "Noted the fwupd firmware refresh too.";
    const a = try store.publish(tmp.dir, io, gpa, p1, now_ms);
    errdefer a.deinit(gpa);

    var p2 = base;
    p2.turn_id = "turn-2002";
    p2.user_content = "a quokka appeared in the pi-web-access logs";
    p2.assistant_result = "Filed it.";
    const b = try store.publish(tmp.dir, io, gpa, p2, now_ms);
    errdefer b.deinit(gpa);

    var p3 = base;
    p3.turn_id = "turn-2003";
    p3.lane = .delegated_work;
    p3.user_content = "delegated wombat census";
    p3.assistant_result = "Census complete.";
    const c = try store.publish(tmp.dir, io, gpa, p3, now_ms);
    errdefer c.deinit(gpa);

    var p4 = base;
    p4.turn_id = "turn-2004";
    p4.lane = .evaluation;
    p4.user_content = "sealed evaluation phrase quokka";
    p4.assistant_result = "Sealed.";
    const d = try store.publish(tmp.dir, io, gpa, p4, now_ms);
    errdefer d.deinit(gpa);

    var idx = try index.Index.open(gpa, ":memory:", null);
    errdefer idx.close();
    _ = try idx.syncFromCorpus(tmp.dir, io, gpa);

    const empty_map = try aliases.loadFromBytes(gpa, "{}");
    return .{
        .tmp = tmp,
        .idx = idx,
        .published = .{ a, b, c, d },
        .empty_map = empty_map,
    };
}

test "search ranks multi-term hits first with full provenance and snippets" {
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    var fx = try setup(gpa, io);
    defer fx.deinit(gpa);

    const out = try search.search(gpa, io, fx.tmp.dir, &fx.idx, &fx.empty_map, fx.request("quokka enclosure"));
    defer out.deinit();
    try std.testing.expectEqual(contracts.Outcome.match, out.outcome);
    try std.testing.expectEqual(@as(u64, 2), out.total);
    try std.testing.expectEqual(@as(usize, 2), out.hits.len);

    const top = out.hits[0];
    try std.testing.expectEqualStrings(&fx.published[0].episode_id, top.episode_id);
    try std.testing.expect(std.mem.startsWith(u8, top.revision, "sha256:"));
    try std.testing.expect(std.mem.startsWith(u8, top.path, "worlds/testworld/"));
    try std.testing.expectEqual(contracts.Lane.conversation, top.lane);
    try std.testing.expectEqualStrings("workspace:demo", top.scope);
    try std.testing.expect(top.score > out.hits[1].score);
    try std.testing.expectEqual(@as(usize, 2), top.matched_terms.len);
    // The snippet carries the matched line plus context, never frontmatter.
    try std.testing.expect(std.mem.indexOf(u8, top.snippet, "quokka enclosure") != null);
    try std.testing.expect(std.mem.indexOf(u8, top.snippet, "context before") != null);
    try std.testing.expect(std.mem.indexOf(u8, top.snippet, "payload_digest") == null);
    try std.testing.expect(top.snippet_start >= 19); // body starts after frontmatter
    try std.testing.expect(top.line >= top.snippet_start and top.line <= top.snippet_end);
    // Line numbering stays aligned through blank lines: walking the snippet
    // to the hit's line index lands on the matched text.
    {
        var lines = std.mem.splitScalar(u8, top.snippet, '\n');
        var line_no = top.snippet_start;
        const match_line = while (lines.next()) |line| : (line_no += 1) {
            if (line_no == top.line) break line;
        } else return error.TestUnexpectedResult;
        try std.testing.expect(std.mem.indexOf(u8, match_line, "quokka enclosure") != null);
    }
    try std.testing.expectEqual(contracts.IndexFreshness.fresh, out.freshness);

    // Determinism: an identical call returns identical results.
    const again = try search.search(gpa, io, fx.tmp.dir, &fx.idx, &fx.empty_map, fx.request("quokka enclosure"));
    defer again.deinit();
    try std.testing.expectEqualStrings(top.episode_id, again.hits[0].episode_id);
    try std.testing.expectEqual(top.line, again.hits[0].line);
    try std.testing.expectEqual(top.score, again.hits[0].score);
}

test "infix recall is substring-mode only; word_start default drops it" {
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    var fx = try setup(gpa, io);
    defer fx.deinit(gpa);

    // "index" appears only inside "reindexing". v1-parity substring
    // crediting still surfaces it.
    var parity_req = fx.request("index");
    parity_req.credit_mode = .substring;
    const parity = try search.search(gpa, io, fx.tmp.dir, &fx.idx, &fx.empty_map, parity_req);
    defer parity.deinit();
    try std.testing.expectEqual(contracts.Outcome.match, parity.outcome);
    try std.testing.expectEqualStrings(&fx.published[0].episode_id, parity.hits[0].episode_id);
    try std.testing.expectEqualStrings("index", parity.hits[0].matched_terms[0]);

    // The word_start default refuses the infix credit ("index" mid-word),
    // trading it away to stop "hang"-in-"changed" false credits; curated
    // aliases ("index" -> "reindex") recover wanted infix families.
    const strict = try search.search(gpa, io, fx.tmp.dir, &fx.idx, &fx.empty_map, fx.request("index"));
    defer strict.deinit();
    try std.testing.expectEqual(contracts.Outcome.no_match, strict.outcome);
}

test "evaluation lane is invisible until explicitly requested" {
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    var fx = try setup(gpa, io);
    defer fx.deinit(gpa);

    const hidden = try search.search(gpa, io, fx.tmp.dir, &fx.idx, &fx.empty_map, fx.request("sealed"));
    defer hidden.deinit();
    try std.testing.expectEqual(contracts.Outcome.no_match, hidden.outcome);

    var req = fx.request("sealed");
    req.lanes = &.{.evaluation};
    const shown = try search.search(gpa, io, fx.tmp.dir, &fx.idx, &fx.empty_map, req);
    defer shown.deinit();
    try std.testing.expectEqual(contracts.Outcome.match, shown.outcome);
    try std.testing.expectEqual(contracts.Lane.evaluation, shown.hits[0].lane);
}

test "aliases rescue vocabulary-mismatch queries, including phrase values" {
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    var fx = try setup(gpa, io);
    defer fx.deinit(gpa);

    // Without the alias the casual word misses.
    const miss = try search.search(gpa, io, fx.tmp.dir, &fx.idx, &fx.empty_map, fx.request("portal"));
    defer miss.deinit();
    try std.testing.expectEqual(contracts.Outcome.no_match, miss.outcome);

    const map = try aliases.loadFromBytes(gpa,
        \\{"portal": ["pi-web-access"], "refresh": ["fwupd"]}
    );
    defer map.deinit();
    const hit = try search.search(gpa, io, fx.tmp.dir, &fx.idx, &map, fx.request("portal"));
    defer hit.deinit();
    try std.testing.expectEqual(contracts.Outcome.match, hit.outcome);
    try std.testing.expectEqualStrings(&fx.published[1].episode_id, hit.hits[0].episode_id);
    try std.testing.expectEqual(@as(usize, 1), hit.alias_terms.len);
    try std.testing.expectEqualStrings("pi-web-access", hit.hits[0].matched_terms[0]);
    // Alias identity differs from the empty map and stamps the output.
    try std.testing.expect(!std.mem.eql(u8, &hit.alias_digest, &miss.alias_digest));
}

test "noise queries produce typed no_match, not weak results" {
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    var fx = try setup(gpa, io);
    defer fx.deinit(gpa);

    const noise = try search.search(gpa, io, fx.tmp.dir, &fx.idx, &fx.empty_map, fx.request("xyzzyplugh frobnicate"));
    defer noise.deinit();
    try std.testing.expectEqual(contracts.Outcome.no_match, noise.outcome);
    try std.testing.expectEqual(@as(u64, 0), noise.total);

    const stops = try search.search(gpa, io, fx.tmp.dir, &fx.idx, &fx.empty_map, fx.request("the of and it"));
    defer stops.deinit();
    try std.testing.expectEqual(contracts.Outcome.no_match, stops.outcome);
}

test "pagination pages deterministically with a guarded cursor" {
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    var fx = try setup(gpa, io);
    defer fx.deinit(gpa);

    var req = fx.request("quokka");
    req.limit = 1;
    const page1 = try search.search(gpa, io, fx.tmp.dir, &fx.idx, &fx.empty_map, req);
    defer page1.deinit();
    try std.testing.expectEqual(@as(u64, 2), page1.total);
    try std.testing.expectEqual(@as(usize, 1), page1.hits.len);
    const cursor = page1.next_cursor orelse return error.TestUnexpectedResult;

    var req2 = req;
    req2.cursor = cursor;
    const page2 = try search.search(gpa, io, fx.tmp.dir, &fx.idx, &fx.empty_map, req2);
    defer page2.deinit();
    try std.testing.expectEqual(@as(usize, 1), page2.hits.len);
    try std.testing.expectEqual(@as(?[]const u8, null), page2.next_cursor);
    try std.testing.expect(!std.mem.eql(u8, page1.hits[0].episode_id, page2.hits[0].episode_id));

    // A cursor replayed against a different query is malformed.
    var req3 = fx.request("enclosure");
    req3.cursor = cursor;
    const replay = try search.search(gpa, io, fx.tmp.dir, &fx.idx, &fx.empty_map, req3);
    defer replay.deinit();
    try std.testing.expectEqual(contracts.Outcome.malformed, replay.outcome);
}

test "empty projection over a nonempty corpus is index_stale, not no_match" {
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    var fx = try setup(gpa, io);
    defer fx.deinit(gpa);

    var fresh_idx = try index.Index.open(gpa, ":memory:", null);
    defer fresh_idx.close();
    const out = try search.search(gpa, io, fx.tmp.dir, &fresh_idx, &fx.empty_map, fx.request("quokka"));
    defer out.deinit();
    try std.testing.expectEqual(contracts.Outcome.index_stale, out.outcome);
    try std.testing.expectEqual(contracts.IndexFreshness.stale, out.freshness);
}

test "an episode edited after indexing is excluded from evidence" {
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    var fx = try setup(gpa, io);
    defer fx.deinit(gpa);

    // Replace episode 2's file with a re-rendered revision (new body, new
    // digest) without re-syncing: the projection still holds the old
    // revision, so the candidate must be dropped, not served.
    const parsed = try contracts.parsePayload(gpa, contracts.test_payload_json);
    defer parsed.deinit();
    var p2 = try contracts.validate(parsed.value);
    p2.turn_id = "turn-2002";
    p2.user_content = "the logs were rotated away";
    p2.assistant_result = "Filed it.";
    const id = identity.episodeId(p2);
    const digest = identity.payloadDigestHex(p2);
    const content = try render.render(gpa, .{
        .payload = p2,
        .episode_id = &id,
        .digest_hex = &digest,
        .capture_time_ms = now_ms,
    });
    defer gpa.free(content);
    try fx.tmp.dir.deleteFile(io, fx.published[1].rel_path);
    try fx.tmp.dir.writeFile(io, .{ .sub_path = fx.published[1].rel_path, .data = content });

    const req = fx.request("quokka");
    const out = try search.search(gpa, io, fx.tmp.dir, &fx.idx, &fx.empty_map, req);
    defer out.deinit();
    try std.testing.expectEqual(contracts.Outcome.match, out.outcome);
    try std.testing.expectEqual(@as(u64, 1), out.total);
    try std.testing.expectEqual(@as(u64, 1), out.edited_excluded);
    try std.testing.expectEqualStrings(&fx.published[0].episode_id, out.hits[0].episode_id);
}

test "memory_get opens exact bounded evidence and tracks revisions" {
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    var fx = try setup(gpa, io);
    defer fx.deinit(gpa);

    const found = try search.search(gpa, io, fx.tmp.dir, &fx.idx, &fx.empty_map, fx.request("wombat"));
    defer found.deinit();
    const hit = found.hits[0];

    // Happy path: hint + matching revision.
    const got = try search.get(gpa, io, fx.tmp.dir, &fx.idx, .{
        .episode_id = hit.episode_id,
        .revision = hit.revision,
        .path_hint = hit.path,
    });
    defer got.deinit();
    try std.testing.expectEqual(contracts.Outcome.match, got.outcome);
    try std.testing.expect(std.mem.indexOf(u8, got.content, "delegated wombat census") != null);
    try std.testing.expect(std.mem.indexOf(u8, got.content, "payload_digest") == null);
    try std.testing.expect(got.line_start >= 19);
    try std.testing.expectEqualStrings("untrusted_evidence", got.trust);
    try std.testing.expectEqual(contracts.Lane.delegated_work, got.lane.?);

    // Without the hint, the index resolves the path.
    const no_hint = try search.get(gpa, io, fx.tmp.dir, &fx.idx, .{
        .episode_id = hit.episode_id,
        .revision = hit.revision,
    });
    defer no_hint.deinit();
    try std.testing.expectEqual(contracts.Outcome.match, no_hint.outcome);

    // A revision that no longer matches the file is stale, with the
    // current revision reported for re-request.
    const stale = try search.get(gpa, io, fx.tmp.dir, &fx.idx, .{
        .episode_id = hit.episode_id,
        .revision = "sha256:" ++ ("ab" ** 32),
        .path_hint = hit.path,
    });
    defer stale.deinit();
    try std.testing.expectEqual(contracts.Outcome.stale_revision, stale.outcome);
    try std.testing.expectEqualStrings(hit.revision, stale.revision.?);

    // Malformed references are typed, not crashes.
    const bad_id = try search.get(gpa, io, fx.tmp.dir, &fx.idx, .{
        .episode_id = "not-an-id",
        .revision = hit.revision,
    });
    defer bad_id.deinit();
    try std.testing.expectEqual(contracts.Outcome.malformed, bad_id.outcome);
    const bad_rev = try search.get(gpa, io, fx.tmp.dir, &fx.idx, .{
        .episode_id = hit.episode_id,
        .revision = "sha256:short",
    });
    defer bad_rev.deinit();
    try std.testing.expectEqual(contracts.Outcome.malformed, bad_rev.outcome);

    // Deleted evidence is gone.
    try fx.tmp.dir.deleteFile(io, hit.path);
    const gone = try search.get(gpa, io, fx.tmp.dir, &fx.idx, .{
        .episode_id = hit.episode_id,
        .revision = hit.revision,
        .path_hint = hit.path,
    });
    defer gone.deinit();
    try std.testing.expectEqual(contracts.Outcome.gone, gone.outcome);

    // Escaping path hints are rejected by containment, not resolved.
    try std.testing.expect(!search.containedPath("../outside.md"));
    try std.testing.expect(!search.containedPath("/etc/passwd"));
    try std.testing.expect(search.containedPath("worlds/w/2026/07/28/aj1-x.md"));
}

test "line bounds clamp to the body and honor explicit spans" {
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    var fx = try setup(gpa, io);
    defer fx.deinit(gpa);

    const found = try search.search(gpa, io, fx.tmp.dir, &fx.idx, &fx.empty_map, fx.request("enclosure"));
    defer found.deinit();
    const hit = found.hits[0];

    // Requesting frontmatter lines clamps to the body start.
    const clamped = try search.get(gpa, io, fx.tmp.dir, &fx.idx, .{
        .episode_id = hit.episode_id,
        .revision = hit.revision,
        .path_hint = hit.path,
        .line_start = 1,
        .line_end = 100,
    });
    defer clamped.deinit();
    try std.testing.expectEqual(contracts.Outcome.match, clamped.outcome);
    try std.testing.expect(clamped.line_start >= 19);

    // A single-line span serves exactly that line.
    const single = try search.get(gpa, io, fx.tmp.dir, &fx.idx, .{
        .episode_id = hit.episode_id,
        .revision = hit.revision,
        .path_hint = hit.path,
        .line_start = hit.line,
        .line_end = hit.line,
    });
    defer single.deinit();
    try std.testing.expectEqual(hit.line, single.line_start);
    try std.testing.expectEqual(hit.line, single.line_end);
    try std.testing.expect(std.mem.indexOf(u8, single.content, "quokka enclosure") != null);
    try std.testing.expect(std.mem.indexOfScalar(u8, single.content, '\n') == null);
}

test "creditLine boundary rules per mode" {
    const cl = search.creditLine;

    // The motivating false positive: "hang" inside "changed".
    try std.testing.expect(cl("we changed the config", "hang", .substring));
    try std.testing.expect(!cl("we changed the config", "hang", .word_start));
    try std.testing.expect(!cl("we changed the config", "hang", .whole_word));

    // Prefix inflections survive word_start but not whole_word.
    try std.testing.expect(cl("the server was hanging", "hang", .word_start));
    try std.testing.expect(!cl("the server was hanging", "hang", .whole_word));
    try std.testing.expect(cl("config", "config", .whole_word));
    try std.testing.expect(cl("configuration drift", "config", .word_start));
    try std.testing.expect(!cl("configuration drift", "config", .whole_word));

    // Infix recall exists only under substring parity.
    try std.testing.expect(cl("reindexing finished", "index", .substring));
    try std.testing.expect(!cl("reindexing finished", "index", .word_start));

    // Case-insensitive in every mode.
    try std.testing.expect(cl("Hang detected", "hang", .whole_word));

    // A later occurrence can credit after an earlier bounded one fails.
    try std.testing.expect(cl("changed, then a hang", "hang", .whole_word));

    // Phrase terms: boundaries apply at the edges of the whole occurrence;
    // interior punctuation and spaces need no special handling.
    try std.testing.expect(cl("ran out of memory today", "out of memory", .whole_word));
    try std.testing.expect(!cl("timeout of memory", "out of memory", .word_start));
    try std.testing.expect(cl("built llama.cpp again", "llama.cpp", .whole_word));

    // Term at line edges has an implicit boundary.
    try std.testing.expect(cl("hang", "hang", .whole_word));
    try std.testing.expect(!cl("hangs", "hang", .whole_word));
    try std.testing.expect(cl("hangs", "hang", .word_start));

    // Underscore is a token byte: "foo_hang" does not word_start-credit "hang".
    try std.testing.expect(!cl("foo_hang", "hang", .word_start));
    try std.testing.expect(!cl("hang_foo", "hang", .whole_word));
    try std.testing.expect(cl("hang_foo", "hang", .word_start));
}
