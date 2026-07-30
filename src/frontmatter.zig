//! Re-parses rendered episode frontmatter for index sync and rebuild.
//!
//! Stored data is untrusted at the read boundary: a hand-edited or corrupt
//! file yields null and is excluded with visible diagnostics, never a crash
//! and never a merged-by-filename guess.

const std = @import("std");
const contracts = @import("contracts.zig");
const identity = @import("identity.zig");

/// All slices borrow from the parsed content buffer.
pub const Episode = struct {
    episode_id: []const u8,
    world: []const u8,
    scope: []const u8,
    lane: contracts.Lane,
    harness: []const u8,
    session_id: []const u8,
    turn_id: []const u8,
    event_time_ms: u64,
    capture_time_ms: u64,
    capture_policy: []const u8,
    turn_outcome: []const u8,
    digest_hex: []const u8,
    /// 1-based line number of the first line after the closing `---`.
    /// Frontmatter is metadata, not memory: indexing and snippet clamping
    /// start here.
    body_line: u32,
    /// Byte offset of that same first body line.
    body_offset: usize,
};

pub fn parse(content: []const u8) ?Episode {
    if (!std.mem.startsWith(u8, content, "---\n")) return null;
    var schema: ?[]const u8 = null;
    var episode_id: ?[]const u8 = null;
    var world: ?[]const u8 = null;
    var scope: ?[]const u8 = null;
    var lane: ?contracts.Lane = null;
    var harness: ?[]const u8 = null;
    var session_id: ?[]const u8 = null;
    var turn_id: ?[]const u8 = null;
    var event_time_ms: ?u64 = null;
    var capture_time_ms: ?u64 = null;
    var capture_policy: ?[]const u8 = null;
    var turn_outcome: ?[]const u8 = null;
    var digest_hex: ?[]const u8 = null;

    var rest = content["---\n".len..];
    var offset: usize = "---\n".len;
    var line_no: u32 = 1; // the opening `---` is line 1
    var closed = false;
    while (rest.len > 0) {
        const line_end = std.mem.indexOfScalar(u8, rest, '\n') orelse break;
        const line = rest[0..line_end];
        rest = rest[line_end + 1 ..];
        offset += line_end + 1;
        line_no += 1;
        if (std.mem.eql(u8, line, "---")) {
            closed = true;
            break;
        }
        const sep = std.mem.indexOf(u8, line, ": ") orelse return null;
        const key = line[0..sep];
        const value = line[sep + 2 ..];
        if (std.mem.eql(u8, key, "schema")) {
            schema = value;
        } else if (std.mem.eql(u8, key, "episode_id")) {
            episode_id = value;
        } else if (std.mem.eql(u8, key, "world")) {
            world = value;
        } else if (std.mem.eql(u8, key, "scope")) {
            scope = value;
        } else if (std.mem.eql(u8, key, "lane")) {
            lane = std.meta.stringToEnum(contracts.Lane, value) orelse return null;
        } else if (std.mem.eql(u8, key, "harness")) {
            harness = value;
        } else if (std.mem.eql(u8, key, "session_id")) {
            session_id = value;
        } else if (std.mem.eql(u8, key, "turn_id")) {
            turn_id = value;
        } else if (std.mem.eql(u8, key, "event_time_ms")) {
            event_time_ms = std.fmt.parseInt(u64, value, 10) catch return null;
        } else if (std.mem.eql(u8, key, "capture_time_ms")) {
            capture_time_ms = std.fmt.parseInt(u64, value, 10) catch return null;
        } else if (std.mem.eql(u8, key, "capture_policy")) {
            capture_policy = value;
        } else if (std.mem.eql(u8, key, "turn_outcome")) {
            turn_outcome = value;
        } else if (std.mem.eql(u8, key, "payload_digest")) {
            if (!std.mem.startsWith(u8, value, identity.digest_prefix)) return null;
            const hex = value[identity.digest_prefix.len..];
            if (hex.len != identity.digest_hex_len) return null;
            digest_hex = hex;
        }
        // Unknown keys are tolerated on read: a newer writer may add fields.
    }
    if (!closed) return null;
    if (!std.mem.eql(u8, schema orelse return null, contracts.episode_schema)) return null;

    const ep: Episode = .{
        .episode_id = episode_id orelse return null,
        .world = world orelse return null,
        .scope = scope orelse return null,
        .lane = lane orelse return null,
        .harness = harness orelse return null,
        .session_id = session_id orelse return null,
        .turn_id = turn_id orelse return null,
        .event_time_ms = event_time_ms orelse return null,
        .capture_time_ms = capture_time_ms orelse return null,
        .capture_policy = capture_policy orelse return null,
        .turn_outcome = turn_outcome orelse return null,
        .digest_hex = digest_hex orelse return null,
        .body_line = line_no + 1,
        .body_offset = offset,
    };
    if (!contracts.validWorld(ep.world)) return null;
    if (!contracts.validToken(ep.scope)) return null;
    if (!contracts.validToken(ep.harness)) return null;
    if (!contracts.validToken(ep.session_id)) return null;
    if (!contracts.validToken(ep.turn_id)) return null;
    if (!contracts.validToken(ep.capture_policy)) return null;
    if (!contracts.validToken(ep.turn_outcome)) return null;
    return ep;
}

test "round trip: rendered episode parses back" {
    const gpa = std.testing.allocator;
    const render = @import("render.zig");
    const parsed = try contracts.parsePayload(gpa, contracts.test_payload_json);
    defer parsed.deinit();
    const p = try contracts.validate(parsed.value);
    const id = identity.episodeId(p);
    const digest = identity.payloadDigestHex(p);
    const content = try render.render(gpa, .{
        .payload = p,
        .episode_id = &id,
        .digest_hex = &digest,
        .capture_time_ms = 1785240000000,
    });
    defer gpa.free(content);

    const ep = parse(content) orelse return error.TestUnexpectedResult;
    try std.testing.expectEqualStrings(&id, ep.episode_id);
    try std.testing.expectEqualStrings(p.world, ep.world);
    try std.testing.expectEqual(p.lane, ep.lane);
    try std.testing.expectEqual(p.event_time_ms, ep.event_time_ms);
    try std.testing.expectEqualStrings(&digest, ep.digest_hex);
    // Rendered frontmatter is `---` + 16 keys + `---`, so the body starts
    // on line 19, at the blank line before `## User`.
    try std.testing.expectEqual(@as(u32, 19), ep.body_line);
    try std.testing.expect(std.mem.startsWith(u8, content[ep.body_offset..], "\n## User\n"));
    // A body line that mimics frontmatter must not shift the body start.
    var spoofed = p;
    spoofed.user_content = "---\nworld: fake\n---";
    const spoof_id = identity.episodeId(spoofed);
    const spoof_digest = identity.payloadDigestHex(spoofed);
    const spoof_content = try render.render(gpa, .{
        .payload = spoofed,
        .episode_id = &spoof_id,
        .digest_hex = &spoof_digest,
        .capture_time_ms = 1785240000000,
    });
    defer gpa.free(spoof_content);
    const spoof_ep = parse(spoof_content) orelse return error.TestUnexpectedResult;
    try std.testing.expectEqual(@as(u32, 19), spoof_ep.body_line);
}

test "corrupt frontmatter yields null, not a crash" {
    try std.testing.expectEqual(@as(?Episode, null), parse("not an episode"));
    try std.testing.expectEqual(@as(?Episode, null), parse("---\nschema: aj-episode.v1\n"));
    try std.testing.expectEqual(@as(?Episode, null), parse("---\nschema: wrong.v9\n---\n"));
    try std.testing.expectEqual(
        @as(?Episode, null),
        parse("---\nschema: aj-episode.v1\nepisode_id: x\n---\n"),
    );
}
