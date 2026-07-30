//! Episode identity and canonical payload digests.
//!
//! The episode ID is the idempotency identity: source harness, session,
//! turn, world, and capture policy version. Re-delivering the same identity
//! with the same payload digest is a duplicate (success); the same identity
//! with a different digest is a conflict.
//!
//! The payload digest covers canonical identity metadata plus the body and
//! excludes capture-run metadata (capture time, adapter version) so that a
//! faithful re-delivery hashes identically regardless of when or by which
//! adapter build it arrives.

const std = @import("std");
const contracts = @import("contracts.zig");
const Sha256 = std.crypto.hash.sha2.Sha256;

pub const id_prefix = "aj1-";
/// Prefix + 32 hex chars (128 bits of the identity hash).
pub const episode_id_len = id_prefix.len + 32;
pub const digest_prefix = "sha256:";
pub const digest_hex_len = 64;

pub fn episodeId(p: contracts.Payload) [episode_id_len]u8 {
    var h = Sha256.init(.{});
    h.update("autojournal-episode-id.v1");
    for ([_][]const u8{ p.harness, p.session_id, p.turn_id, p.world, p.capture_policy }) |field| {
        h.update(&.{0});
        h.update(field);
    }
    var sum: [Sha256.digest_length]u8 = undefined;
    h.final(&sum);
    var out: [episode_id_len]u8 = undefined;
    @memcpy(out[0..id_prefix.len], id_prefix);
    _ = std.fmt.bufPrint(out[id_prefix.len..], "{x}", .{sum[0..16]}) catch unreachable;
    return out;
}

/// Canonical digest input is length-prefix framed so no content bytes can be
/// confused with framing. Every field that participates is versioned under
/// the leading tag; changing the set of fields requires a new tag.
pub fn payloadDigestHex(p: contracts.Payload) [digest_hex_len]u8 {
    var h = Sha256.init(.{});
    h.update("autojournal-digest.v1");
    hashField(&h, p.world);
    hashField(&h, p.scope);
    hashField(&h, @tagName(p.lane));
    hashField(&h, p.harness);
    hashField(&h, p.session_id);
    hashField(&h, p.turn_id);
    var num_buf: [20]u8 = undefined;
    hashField(&h, std.fmt.bufPrint(&num_buf, "{d}", .{p.event_time_ms}) catch unreachable);
    hashField(&h, p.capture_policy);
    hashField(&h, p.turn_outcome);
    hashField(&h, p.user_content);
    hashField(&h, p.assistant_result);
    var count_buf: [20]u8 = undefined;
    hashField(&h, std.fmt.bufPrint(&count_buf, "{d}", .{p.tools.len}) catch unreachable);
    for (p.tools) |t| hashField(&h, t.name);
    var sum: [Sha256.digest_length]u8 = undefined;
    h.final(&sum);
    var out: [digest_hex_len]u8 = undefined;
    _ = std.fmt.bufPrint(&out, "{x}", .{&sum}) catch unreachable;
    return out;
}

fn hashField(h: *Sha256, bytes: []const u8) void {
    var len_buf: [20]u8 = undefined;
    const len_str = std.fmt.bufPrint(&len_buf, "{d}", .{bytes.len}) catch unreachable;
    h.update(&.{0});
    h.update(len_str);
    h.update(&.{0});
    h.update(bytes);
}

fn testPayload() !struct { parsed: std.json.Parsed(contracts.RawPayload), p: contracts.Payload } {
    const parsed = try contracts.parsePayload(std.testing.allocator, contracts.test_payload_json);
    errdefer parsed.deinit();
    return .{ .parsed = parsed, .p = try contracts.validate(parsed.value) };
}

test "episode id is stable and prefixed" {
    const t = try testPayload();
    defer t.parsed.deinit();
    const a = episodeId(t.p);
    const b = episodeId(t.p);
    try std.testing.expectEqualStrings(&a, &b);
    try std.testing.expect(std.mem.startsWith(u8, &a, id_prefix));
}

test "identity fields change the id; body does not" {
    const t = try testPayload();
    defer t.parsed.deinit();
    const base = episodeId(t.p);
    var other = t.p;
    other.turn_id = "turn-0008";
    try std.testing.expect(!std.mem.eql(u8, &base, &episodeId(other)));
    other = t.p;
    other.assistant_result = "different body";
    try std.testing.expectEqualStrings(&base, &episodeId(other));
}

test "digest tracks body and identity but not capture-run metadata" {
    const t = try testPayload();
    defer t.parsed.deinit();
    const base = payloadDigestHex(t.p);
    var other = t.p;
    other.assistant_result = "changed";
    try std.testing.expect(!std.mem.eql(u8, &base, &payloadDigestHex(other)));
    other = t.p;
    other.adapter_version = "9.9.9";
    try std.testing.expectEqualStrings(&base, &payloadDigestHex(other));
}

test "field-boundary shifts change the digest" {
    const t = try testPayload();
    defer t.parsed.deinit();
    var a = t.p;
    a.user_content = "ab";
    a.assistant_result = "c";
    var b = t.p;
    b.user_content = "a";
    b.assistant_result = "bc";
    try std.testing.expect(!std.mem.eql(u8, &payloadDigestHex(a), &payloadDigestHex(b)));
}
