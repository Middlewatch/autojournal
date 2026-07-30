//! Closed, versioned capture contracts and typed outcomes.
//!
//! The wire payload is one JSON object. The schema is closed: unknown fields,
//! duplicate fields, missing fields, and over-budget values are all rejected
//! with typed reasons rather than best-effort acceptance.

const std = @import("std");

pub const payload_schema_version: u32 = 1;
pub const episode_schema = "aj-episode.v1";

pub const max_payload_bytes: usize = 4 * 1024 * 1024;
pub const max_content_bytes: usize = 2 * 1024 * 1024;
/// Read budget for one rendered episode file: the file carries frontmatter
/// and escaping overhead on top of payload content, so it may legitimately
/// exceed `max_payload_bytes`.
pub const max_episode_file_bytes: usize = max_payload_bytes * 2;
/// Directory descent budget below the journal root. The deepest supported
/// layout is worlds/<world>/scopes/<scope>/lanes/<lane>/YYYY/MM/DD/file.
pub const corpus_walk_depth: u8 = 10;
pub const max_world_len: usize = 64;
pub const max_token_len: usize = 128;
pub const max_tools: usize = 256;

pub const Lane = enum {
    conversation,
    delegated_work,
    evaluation,
    imported_legacy,
};

/// Capture result vocabulary reported to adapters. `published` and
/// `duplicate` are success; everything else is a distinct typed failure.
pub const CaptureOutcome = enum {
    published,
    duplicate,
    conflict,
    malformed,
    permission_denied,
    unavailable,
    internal_error,
};

/// Index freshness is reported independently of source publication.
pub const IndexFreshness = enum {
    fresh,
    stale,
    not_built,
    unavailable,
};

/// Retrieval outcome vocabulary shared by `memory_search` and `memory_get`.
/// `no_match` is a valid result, not a failure: empty, stale, unavailable,
/// malformed, and timed-out are all distinct typed outcomes.
pub const Outcome = enum {
    match,
    no_match,
    stale_revision,
    gone,
    index_stale,
    timeout,
    unavailable,
    permission_denied,
    malformed,
    conflict,
    internal_error,
};

// Retrieval bounds. Search returns ranked references and bounded snippets;
// `memory_get` opens one bounded span. Neither ever returns an unbounded
// episode body.
pub const max_query_bytes: usize = 4096;
pub const max_query_terms: usize = 64;
pub const max_snippet_line_bytes: usize = 400;
pub const max_snippet_bytes: usize = 4096;
pub const max_get_lines: u32 = 400;
pub const max_get_bytes: usize = 64 * 1024;
pub const max_results_limit: u32 = 100;

/// Wire shape prior to validation. Field names are the wire contract.
/// `world` and `scope` may be omitted and filled from owner defaults. A
/// first-class host may provide them when transporting an explicit owner
/// session selection. Every other field is a lifecycle fact the adapter
/// knows.
pub const RawPayload = struct {
    schema_version: u32,
    world: ?[]const u8 = null,
    scope: ?[]const u8 = null,
    lane: []const u8,
    harness: []const u8,
    adapter_version: []const u8,
    session_id: []const u8,
    turn_id: []const u8,
    event_time_ms: u64,
    capture_policy: []const u8,
    turn_outcome: []const u8,
    user_content: []const u8,
    assistant_result: []const u8,
    tools: ?[]const Tool = null,

    pub const Tool = struct {
        name: []const u8,
    };
};

/// Validated payload. Slices alias the arena inside `std.json.Parsed`; keep
/// the parse result alive for as long as the payload is used.
pub const Payload = struct {
    world: []const u8,
    scope: []const u8,
    lane: Lane,
    harness: []const u8,
    adapter_version: []const u8,
    session_id: []const u8,
    turn_id: []const u8,
    event_time_ms: u64,
    capture_policy: []const u8,
    turn_outcome: []const u8,
    user_content: []const u8,
    assistant_result: []const u8,
    tools: []const RawPayload.Tool,
};

pub const ValidateError = error{
    UnsupportedSchemaVersion,
    InvalidWorld,
    InvalidScope,
    InvalidLane,
    InvalidHarness,
    InvalidAdapterVersion,
    InvalidSessionId,
    InvalidTurnId,
    InvalidCapturePolicy,
    InvalidTurnOutcome,
    EmptyUserContent,
    EmptyAssistantResult,
    OversizedContent,
    InvalidUtf8,
    TooManyTools,
    InvalidToolName,
};

pub const ParseError = ValidateError || error{ Malformed, OutOfMemory };

/// A world id names a directory component: lowercase alphanumeric plus `-`,
/// bounded, never starting with `.` (enforced by charset).
pub fn validWorld(s: []const u8) bool {
    if (s.len == 0 or s.len > max_world_len) return false;
    for (s) |c| switch (c) {
        'a'...'z', '0'...'9', '-' => {},
        else => return false,
    };
    return true;
}

/// Identity tokens (session, turn, harness, policy, scope, outcome, adapter
/// version): printable, no whitespace/control bytes, so they embed safely in
/// frontmatter lines and canonical digest input.
pub fn validToken(s: []const u8) bool {
    if (s.len == 0 or s.len > max_token_len) return false;
    for (s) |c| switch (c) {
        'a'...'z', 'A'...'Z', '0'...'9', '.', '_', '-', ':', '+', '/', '@' => {},
        else => return false,
    };
    return true;
}

/// Scope names are both frontmatter tokens and directory components. Unlike
/// general identity tokens they cannot contain a path separator or name a
/// traversal component.
pub fn validScope(s: []const u8) bool {
    if (!validToken(s) or std.mem.eql(u8, s, ".") or std.mem.eql(u8, s, ".."))
        return false;
    return std.mem.indexOfScalar(u8, s, '/') == null;
}

pub fn validate(raw: RawPayload) ValidateError!Payload {
    if (raw.schema_version != payload_schema_version) return error.UnsupportedSchemaVersion;
    const world = raw.world orelse return error.InvalidWorld;
    const scope = raw.scope orelse return error.InvalidScope;
    if (!validWorld(world)) return error.InvalidWorld;
    if (!validScope(scope)) return error.InvalidScope;
    const lane = std.meta.stringToEnum(Lane, raw.lane) orelse return error.InvalidLane;
    if (!validToken(raw.harness)) return error.InvalidHarness;
    if (!validToken(raw.adapter_version)) return error.InvalidAdapterVersion;
    if (!validToken(raw.session_id)) return error.InvalidSessionId;
    if (!validToken(raw.turn_id)) return error.InvalidTurnId;
    if (!validToken(raw.capture_policy)) return error.InvalidCapturePolicy;
    if (!validToken(raw.turn_outcome)) return error.InvalidTurnOutcome;
    if (raw.user_content.len == 0) return error.EmptyUserContent;
    if (raw.assistant_result.len == 0) return error.EmptyAssistantResult;
    if (raw.user_content.len > max_content_bytes) return error.OversizedContent;
    if (raw.assistant_result.len > max_content_bytes) return error.OversizedContent;
    if (!std.unicode.utf8ValidateSlice(raw.user_content)) return error.InvalidUtf8;
    if (!std.unicode.utf8ValidateSlice(raw.assistant_result)) return error.InvalidUtf8;
    const tools = raw.tools orelse &[_]RawPayload.Tool{};
    if (tools.len > max_tools) return error.TooManyTools;
    for (tools) |t| if (!validToken(t.name)) return error.InvalidToolName;
    return .{
        .world = world,
        .scope = scope,
        .lane = lane,
        .harness = raw.harness,
        .adapter_version = raw.adapter_version,
        .session_id = raw.session_id,
        .turn_id = raw.turn_id,
        .event_time_ms = raw.event_time_ms,
        .capture_policy = raw.capture_policy,
        .turn_outcome = raw.turn_outcome,
        .user_content = raw.user_content,
        .assistant_result = raw.assistant_result,
        .tools = tools,
    };
}

/// Parses and validates one capture payload. The returned `Parsed` owns every
/// slice in the `Payload`; deinit it when publication is complete.
pub fn parsePayload(
    gpa: std.mem.Allocator,
    bytes: []const u8,
) ParseError!std.json.Parsed(RawPayload) {
    if (bytes.len > max_payload_bytes) return error.Malformed;
    return std.json.parseFromSlice(RawPayload, gpa, bytes, .{
        .allocate = .alloc_always,
    }) catch |err| switch (err) {
        error.OutOfMemory => error.OutOfMemory,
        else => error.Malformed,
    };
}

pub const test_payload_json =
    \\{
    \\  "schema_version": 1,
    \\  "world": "testworld",
    \\  "scope": "workspace:demo",
    \\  "lane": "conversation",
    \\  "harness": "claude-code",
    \\  "adapter_version": "0.1.0",
    \\  "session_id": "sess-01",
    \\  "turn_id": "turn-0007",
    \\  "event_time_ms": 1783862400123,
    \\  "capture_policy": "default-v1",
    \\  "turn_outcome": "completed",
    \\  "user_content": "How do the naïve tests behave? — ✓",
    \\  "assistant_result": "They pass.\n\n```zig\nconst x = 1;\n```",
    \\  "tools": [{"name": "Bash"}, {"name": "Read"}]
    \\}
;

test "valid payload parses and validates" {
    const parsed = try parsePayload(std.testing.allocator, test_payload_json);
    defer parsed.deinit();
    const p = try validate(parsed.value);
    try std.testing.expectEqual(Lane.conversation, p.lane);
    try std.testing.expectEqualStrings("testworld", p.world);
    try std.testing.expectEqual(@as(usize, 2), p.tools.len);
}

test "unknown field is rejected as malformed" {
    const bad =
        \\{"schema_version":1,"world":"w","scope":"s","lane":"conversation",
        \\"harness":"h","adapter_version":"1","session_id":"a","turn_id":"b",
        \\"event_time_ms":1,"capture_policy":"p","turn_outcome":"ok",
        \\"user_content":"u","assistant_result":"a","surprise":true}
    ;
    try std.testing.expectError(error.Malformed, parsePayload(std.testing.allocator, bad));
}

test "missing required field is rejected as malformed" {
    const bad =
        \\{"schema_version":1,"world":"w"}
    ;
    try std.testing.expectError(error.Malformed, parsePayload(std.testing.allocator, bad));
}

test "invalid lane and world are typed validation failures" {
    const parsed = try parsePayload(std.testing.allocator, test_payload_json);
    defer parsed.deinit();
    var raw = parsed.value;
    raw.lane = "gossip";
    try std.testing.expectError(error.InvalidLane, validate(raw));
    raw = parsed.value;
    raw.world = "Bad World";
    try std.testing.expectError(error.InvalidWorld, validate(raw));
    raw = parsed.value;
    raw.assistant_result = "";
    try std.testing.expectError(error.EmptyAssistantResult, validate(raw));
    raw = parsed.value;
    raw.user_content = "\xff\xfe not utf8";
    try std.testing.expectError(error.InvalidUtf8, validate(raw));
}

test "oversized content is a typed validation failure" {
    const gpa = std.testing.allocator;
    const parsed = try parsePayload(gpa, test_payload_json);
    defer parsed.deinit();
    var raw = parsed.value;
    const big = try gpa.alloc(u8, max_content_bytes + 1);
    defer gpa.free(big);
    @memset(big, 'x');
    raw.user_content = big;
    try std.testing.expectError(error.OversizedContent, validate(raw));
}

test "omitted world and scope parse as null and fail validation unfilled" {
    const bare =
        \\{"schema_version":1,"lane":"conversation","harness":"pi",
        \\"adapter_version":"1","session_id":"s","turn_id":"t",
        \\"event_time_ms":1,"capture_policy":"p","turn_outcome":"completed",
        \\"user_content":"u","assistant_result":"a"}
    ;
    const parsed = try parsePayload(std.testing.allocator, bare);
    defer parsed.deinit();
    try std.testing.expect(parsed.value.world == null);
    try std.testing.expect(parsed.value.scope == null);
    try std.testing.expectError(error.InvalidWorld, validate(parsed.value));

    // The capture host's config merge makes the same payload valid.
    var filled = parsed.value;
    filled.world = "main";
    filled.scope = "default";
    const p = try validate(filled);
    try std.testing.expectEqualStrings("main", p.world);
    try std.testing.expectEqualStrings("default", p.scope);
}
