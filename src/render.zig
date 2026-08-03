//! Episode file rendering: closed frontmatter plus Markdown body.
//!
//! The rendered file is the authoritative artifact. Frontmatter values are
//! restricted to validated charsets — identity fields to token charsets,
//! provenance paths to control-free single-line text — so no quoting or
//! escaping layer is needed; body content is arbitrary validated UTF-8 and
//! is never inspected by the frontmatter parser, which stops at the closing
//! delimiter.

const std = @import("std");
const contracts = @import("contracts.zig");
const identity = @import("identity.zig");

pub const RenderInput = struct {
    payload: contracts.Payload,
    episode_id: []const u8,
    digest_hex: []const u8,
    capture_time_ms: u64,
};

pub fn render(gpa: std.mem.Allocator, in: RenderInput) std.mem.Allocator.Error![]u8 {
    var out: std.ArrayList(u8) = .empty;
    errdefer out.deinit(gpa);
    const p = in.payload;
    var event_iso: [20]u8 = undefined;
    var capture_iso: [20]u8 = undefined;
    try out.print(gpa,
        \\---
        \\schema: {s}
        \\episode_id: {s}
        \\world: {s}
        \\scope: {s}
        \\lane: {s}
        \\harness: {s}
        \\adapter_version: {s}
        \\session_id: {s}
        \\turn_id: {s}
        \\event_time: {s}
        \\event_time_ms: {d}
        \\capture_time: {s}
        \\capture_time_ms: {d}
        \\capture_policy: {s}
        \\turn_outcome: {s}
        \\
    , .{
        contracts.episode_schema,
        in.episode_id,
        p.world,
        p.scope,
        @tagName(p.lane),
        p.harness,
        p.adapter_version,
        p.session_id,
        p.turn_id,
        isoFromMs(p.event_time_ms, &event_iso),
        p.event_time_ms,
        isoFromMs(in.capture_time_ms, &capture_iso),
        in.capture_time_ms,
        p.capture_policy,
        p.turn_outcome,
    });
    // Optional provenance keys render only when the payload carried them,
    // so episodes from adapters that do not know them stay byte-identical
    // to the pre-provenance rendering.
    if (p.workspace_root) |root| try out.print(gpa, "workspace_root: {s}\n", .{root});
    if (p.branch_of) |branch| try out.print(gpa, "branch_of: {s}\n", .{branch});
    if (p.host) |host| try out.print(gpa, "host: {s}\n", .{host});
    try out.print(gpa,
        \\payload_digest: {s}{s}
        \\---
        \\
        \\## User
        \\
        \\{s}
        \\
        \\## Assistant
        \\
        \\{s}
        \\
    , .{
        identity.digest_prefix,
        in.digest_hex,
        p.user_content,
        p.assistant_result,
    });
    if (p.tools.len > 0) {
        try out.print(gpa, "\n## Tools\n\n", .{});
        for (p.tools) |t| try out.print(gpa, "- {s}\n", .{t.name});
    }
    return out.toOwnedSlice(gpa);
}

/// UTC ISO-8601 to second precision. Epoch-ms input is unsigned, so
/// pre-epoch times cannot occur by construction.
pub fn isoFromMs(ms: u64, out: *[20]u8) []const u8 {
    const secs = std.time.epoch.EpochSeconds{ .secs = ms / 1000 };
    const year_day = secs.getEpochDay().calculateYearDay();
    const month_day = year_day.calculateMonthDay();
    const day_secs = secs.getDaySeconds();
    return std.fmt.bufPrint(out, "{d:0>4}-{d:0>2}-{d:0>2}T{d:0>2}:{d:0>2}:{d:0>2}Z", .{
        @as(u16, year_day.year),
        @as(u8, month_day.month.numeric()),
        @as(u8, month_day.day_index) + 1,
        @as(u8, day_secs.getHoursIntoDay()),
        @as(u8, day_secs.getMinutesIntoHour()),
        @as(u8, day_secs.getSecondsIntoMinute()),
    }) catch unreachable;
}

/// Extracts the digest hex from a rendered episode's frontmatter, for the
/// duplicate-vs-conflict decision on redelivery. Returns null when the file
/// has no parseable digest line in its leading frontmatter block.
pub fn frontmatterDigestHex(content: []const u8) ?[]const u8 {
    const digest_key = "payload_digest: " ++ identity.digest_prefix;
    if (!std.mem.startsWith(u8, content, "---\n")) return null;
    var rest = content["---\n".len..];
    while (rest.len > 0) {
        const line_end = std.mem.indexOfScalar(u8, rest, '\n') orelse rest.len;
        const line = rest[0..line_end];
        if (std.mem.eql(u8, line, "---")) return null;
        if (std.mem.startsWith(u8, line, digest_key)) {
            const hex = line[digest_key.len..];
            if (hex.len != identity.digest_hex_len) return null;
            return hex;
        }
        if (line_end == rest.len) return null;
        rest = rest[line_end + 1 ..];
    }
    return null;
}

test "iso rendering" {
    var buf: [20]u8 = undefined;
    // 2026-07-28T12:00:00Z
    try std.testing.expectEqualStrings("2026-07-28T12:00:00Z", isoFromMs(1785240000000, &buf));
    try std.testing.expectEqualStrings("1970-01-01T00:00:00Z", isoFromMs(0, &buf));
}

test "render produces parseable frontmatter with recoverable digest" {
    const gpa = std.testing.allocator;
    const parsed = try contracts.parsePayload(gpa, contracts.test_payload_json);
    defer parsed.deinit();
    const p = try contracts.validate(parsed.value);
    const id = identity.episodeId(p);
    const digest = identity.payloadDigestHex(p);
    const content = try render(gpa, .{
        .payload = p,
        .episode_id = &id,
        .digest_hex = &digest,
        .capture_time_ms = 1785240000000,
    });
    defer gpa.free(content);
    try std.testing.expect(std.mem.startsWith(u8, content, "---\nschema: aj-episode.v1\n"));
    const recovered = frontmatterDigestHex(content) orelse return error.TestUnexpectedResult;
    try std.testing.expectEqualStrings(&digest, recovered);
    try std.testing.expect(std.mem.indexOf(u8, content, "## Assistant") != null);
    try std.testing.expect(std.mem.indexOf(u8, content, "- Bash\n") != null);
}

test "digest recovery rejects bodies that mimic the digest line" {
    const gpa = std.testing.allocator;
    const parsed = try contracts.parsePayload(gpa, contracts.test_payload_json);
    defer parsed.deinit();
    var raw = parsed.value;
    raw.user_content = "---\npayload_digest: sha256:" ++ ("ab" ** 32);
    const p = try contracts.validate(raw);
    const id = identity.episodeId(p);
    const digest = identity.payloadDigestHex(p);
    const content = try render(gpa, .{
        .payload = p,
        .episode_id = &id,
        .digest_hex = &digest,
        .capture_time_ms = 1,
    });
    defer gpa.free(content);
    const recovered = frontmatterDigestHex(content) orelse return error.TestUnexpectedResult;
    // The frontmatter digest, not the body imitation, must win.
    try std.testing.expectEqualStrings(&digest, recovered);
}

test "provenance fields render as frontmatter keys and stay out of the digest" {
    const gpa = std.testing.allocator;
    const parsed = try contracts.parsePayload(gpa, contracts.test_payload_json);
    defer parsed.deinit();
    const bare = try contracts.validate(parsed.value);
    var raw = parsed.value;
    raw.workspace_root = "/home/user/projects/demo";
    raw.branch_of = "/home/user/sessions/parent.jsonl";
    raw.host = "buildbox-01";
    const p = try contracts.validate(raw);
    const id = identity.episodeId(p);
    const digest = identity.payloadDigestHex(p);
    // Same identity and digest as the provenance-free payload: provenance
    // is capture-source metadata, so a faithful re-delivery still dedupes.
    try std.testing.expectEqualStrings(&identity.episodeId(bare), &id);
    try std.testing.expectEqualStrings(&identity.payloadDigestHex(bare), &digest);
    const content = try render(gpa, .{
        .payload = p,
        .episode_id = &id,
        .digest_hex = &digest,
        .capture_time_ms = 1785240000000,
    });
    defer gpa.free(content);
    try std.testing.expect(std.mem.indexOf(u8, content, "\nworkspace_root: /home/user/projects/demo\n") != null);
    try std.testing.expect(std.mem.indexOf(u8, content, "\nbranch_of: /home/user/sessions/parent.jsonl\n") != null);
    try std.testing.expect(std.mem.indexOf(u8, content, "\nhost: buildbox-01\n") != null);
    const recovered = frontmatterDigestHex(content) orelse return error.TestUnexpectedResult;
    try std.testing.expectEqualStrings(&digest, recovered);
    // The frontmatter reader tolerates the new keys (older-reader posture).
    const ep = @import("frontmatter.zig").parse(content) orelse return error.TestUnexpectedResult;
    try std.testing.expectEqualStrings(&id, ep.episode_id);
}
