//! Proven lexical retrieval core: tokenizer, scorer, dedup, confidence,
//! cursor. Pure — no I/O and no SQLite. Candidates come in, a ranked and
//! deduplicated ordering comes out; the search orchestration in
//! `search.zig` owns candidate discovery and snippet extraction.
//!
//! The scoring behavior is settled evidence ported from the deployed
//! TypeScript v1 (`legacy-ts/src/search.ts`): `sum(log(N/df))` rarity with
//! exact-query duplicate term weights, day-quantized recency as a nudge
//! rather than an override, span deduplication, and deterministic
//! tie-breaking. Stage-6 parity gates compare ranked results against that
//! implementation, so behavioral changes here require a version bump and a
//! new parity baseline.

const std = @import("std");
const contracts = @import("contracts.zig");

/// Gates the index: postings from another tokenizer version are disposed.
pub const tokenizer_version = "aj-tok.v1";
/// Stamped on every search result; callers may pin it.
pub const scorer_version = "aj-scorer.v1";
/// Reported separately from score; numeric calibration is deferred, the
/// policy identity is not.
pub const confidence_policy_version = "aj-conf.v1";

// --- Tokenizer ---

/// Verbatim port of the v1 stop-word list, kept diffable against the
/// TypeScript source. The 2-byte entries are live on the index side (its
/// token floor is 2, one below the query side's 3), so they do filter real
/// index tokens.
const stop_words = std.StaticStringMap(void).initComptime(.{
    .{"a"},          .{"an"},        .{"the"},     .{"is"},        .{"are"},
    .{"was"},        .{"were"},      .{"be"},      .{"been"},      .{"being"},
    .{"have"},       .{"has"},       .{"had"},     .{"do"},        .{"does"},
    .{"did"},        .{"will"},      .{"would"},   .{"could"},     .{"should"},
    .{"may"},        .{"might"},     .{"shall"},   .{"can"},       .{"need"},
    .{"dare"},       .{"ought"},     .{"used"},    .{"to"},        .{"of"},
    .{"in"},         .{"for"},       .{"on"},      .{"with"},      .{"at"},
    .{"by"},         .{"from"},      .{"as"},      .{"into"},      .{"through"},
    .{"during"},     .{"before"},    .{"after"},   .{"above"},     .{"below"},
    .{"between"},    .{"out"},       .{"off"},     .{"over"},      .{"under"},
    .{"again"},      .{"further"},   .{"then"},    .{"once"},      .{"here"},
    .{"there"},      .{"when"},      .{"where"},   .{"why"},       .{"how"},
    .{"all"},        .{"both"},      .{"each"},    .{"few"},       .{"more"},
    .{"most"},       .{"other"},     .{"some"},    .{"such"},      .{"no"},
    .{"nor"},        .{"not"},       .{"only"},    .{"own"},       .{"same"},
    .{"so"},         .{"than"},      .{"too"},     .{"very"},      .{"just"},
    .{"because"},    .{"but"},       .{"and"},     .{"or"},        .{"if"},
    .{"while"},      .{"about"},     .{"up"},      .{"down"},      .{"what"},
    .{"which"},      .{"who"},       .{"whom"},    .{"this"},      .{"that"},
    .{"these"},      .{"those"},     .{"i"},       .{"me"},        .{"my"},
    .{"myself"},     .{"we"},        .{"our"},     .{"ours"},      .{"ourselves"},
    .{"you"},        .{"your"},      .{"yours"},   .{"yourself"},  .{"yourselves"},
    .{"he"},         .{"him"},       .{"his"},     .{"himself"},   .{"she"},
    .{"her"},        .{"hers"},      .{"herself"}, .{"it"},        .{"its"},
    .{"itself"},     .{"they"},      .{"them"},    .{"their"},     .{"theirs"},
    .{"themselves"}, .{"also"},      .{"get"},     .{"got"},       .{"like"},
    .{"know"},       .{"think"},     .{"want"},    .{"look"},      .{"use"},
    .{"find"},       .{"give"},      .{"tell"},    .{"say"},       .{"said"},
    .{"take"},       .{"come"},      .{"make"},    .{"go"},        .{"see"},
    .{"thing"},      .{"things"},    .{"really"},  .{"something"}, .{"anything"},
    .{"remember"},   .{"mentioned"}, .{"talked"},
});

pub fn isStopWord(word: []const u8) bool {
    return stop_words.has(word);
}

fn isTokenByte(b: u8) bool {
    return switch (b) {
        'a'...'z', '0'...'9', '_' => true,
        else => false,
    };
}

/// A token is a maximal lowercase `[a-z0-9_]+` run; every other byte
/// separates. Byte-for-byte equivalent to the v1 pipeline (lowercase, strip
/// punctuation to spaces, split on whitespace) for all inputs including
/// UTF-8, because non-ASCII bytes are separators under both.
pub const Terms = struct {
    /// Lowercased copy of the input; `items` slices borrow from it.
    buf: []u8,
    /// Query terms in order, duplicates preserved (a repeated query word
    /// legitimately doubles its weight, exactly as v1 scored it).
    items: []const []const u8,
    /// True when the term cap dropped trailing terms.
    truncated: bool,

    pub fn deinit(self: *const Terms, gpa: std.mem.Allocator) void {
        gpa.free(self.items);
        gpa.free(self.buf);
    }
};

pub fn extractTerms(gpa: std.mem.Allocator, query: []const u8) error{OutOfMemory}!Terms {
    const buf = try gpa.alloc(u8, query.len);
    errdefer gpa.free(buf);
    for (query, 0..) |b, i| buf[i] = std.ascii.toLower(b);

    var items: std.ArrayList([]const u8) = .empty;
    errdefer items.deinit(gpa);
    var truncated = false;
    var i: usize = 0;
    while (i < buf.len) {
        while (i < buf.len and !isTokenByte(buf[i])) i += 1;
        const start = i;
        while (i < buf.len and isTokenByte(buf[i])) i += 1;
        const word = buf[start..i];
        if (word.len <= 2 or isStopWord(word)) continue;
        if (items.items.len >= contracts.max_query_terms) {
            truncated = true;
            break;
        }
        try items.append(gpa, word);
    }
    const owned = try items.toOwnedSlice(gpa);
    return .{ .buf = buf, .items = owned, .truncated = truncated };
}

/// Index-side tokenization: same alphabet and stop-word list as the query
/// side, plus a byte cap that keeps hash blobs out of the vocabulary. The
/// length floor is 2 here, one shorter than the query side: curated alias
/// values may legitimately be two bytes ("q8"), and discovery happens
/// against this vocabulary. Known gap, accepted for parity purposes: a
/// query term whose only occurrence on a line is inside a stop word or an
/// over-cap token is not discoverable, because such tokens are never
/// indexed.
pub const TokenIterator = struct {
    line: []const u8,
    i: usize = 0,
    buf: [contracts.max_token_len]u8 = undefined,

    /// The returned slice points into the iterator and dies on the next
    /// call; consume or copy it immediately.
    pub fn next(self: *TokenIterator) ?[]const u8 {
        while (self.i < self.line.len) {
            while (self.i < self.line.len and !isIndexTokenByte(self.line[self.i])) self.i += 1;
            const start = self.i;
            while (self.i < self.line.len and isIndexTokenByte(self.line[self.i])) self.i += 1;
            const raw = self.line[start..self.i];
            if (raw.len < 2 or raw.len > contracts.max_token_len) continue;
            const word = self.buf[0..raw.len];
            for (raw, 0..) |b, j| word[j] = std.ascii.toLower(b);
            if (isStopWord(word)) continue;
            return word;
        }
        return null;
    }
};

pub fn tokenizeLine(line: []const u8) TokenIterator {
    return .{ .line = line };
}

pub fn isIndexTokenByte(b: u8) bool {
    return switch (b) {
        'a'...'z', 'A'...'Z', '0'...'9', '_' => true,
        else => false,
    };
}

// --- Scorer ---

/// `1 + boost/(days+1)` day-quantized recency. A nudge, not an override;
/// future timestamps get no boost. Day flooring keeps identical queries
/// stable within a day.
pub fn recencyMultiplier(event_time_ms: u64, now_ms: u64, boost: f64) f64 {
    if (event_time_ms > now_ms) return 1.0;
    const days: f64 = @floatFromInt((now_ms - event_time_ms) / std.time.ms_per_day);
    return 1.0 + boost / (days + 1.0);
}

/// `log(N/df)` rarity weight: a term in every episode contributes ~0, a
/// term in one episode of many dominates. `df == 0` (term absent from the
/// candidate corpus) contributes 0, matching v1's `df.get(t) ?? N`.
pub fn idfWeight(corpus_n: u64, df: u64) f64 {
    if (df == 0) return 0.0;
    const n: f64 = @floatFromInt(@max(corpus_n, 1));
    const d: f64 = @floatFromInt(df);
    return @log(n / d);
}

/// One matched body line. `matched_mask` has bit `i` set when query term
/// `i` (by position in the duplicate-preserving term list) occurs in the
/// line as a case-insensitive substring — v1's per-line crediting.
pub const Candidate = struct {
    /// Index into the caller's episode table.
    episode_ord: u32,
    /// 1-based absolute line number in the episode file.
    line_no: u32,
    matched_mask: u64,
};

pub const EpisodeInfo = struct {
    episode_id: []const u8,
    rel_path: []const u8,
    event_time_ms: u64,
};

pub const RankParams = struct {
    now_ms: u64,
    recency_boost: f64 = 1.0,
    /// 0 disables the relevance floor (v1 parity default).
    min_score: f64 = 0.0,
    context_window: u32 = 3,
};

pub const Ranked = struct {
    /// Candidate indices, ranked, deduplicated, and floored. Pagination is
    /// a slice of this ordering.
    order: []const u32,
    /// Score per input candidate (parallel to the candidates array).
    scores: []const f64,

    pub fn deinit(self: *const Ranked, gpa: std.mem.Allocator) void {
        gpa.free(self.order);
        gpa.free(self.scores);
    }
};

/// Scores, sorts, and deduplicates candidates. `idf` is indexed by query
/// term position (duplicate terms carry their weight twice, once per
/// position). Deterministic: ties break on (rel_path, line_no) so the
/// ordering never depends on candidate arrival order.
pub fn rank(
    gpa: std.mem.Allocator,
    candidates: []const Candidate,
    episodes: []const EpisodeInfo,
    idf: []const f64,
    params: RankParams,
) error{OutOfMemory}!Ranked {
    const scores = try gpa.alloc(f64, candidates.len);
    errdefer gpa.free(scores);
    for (candidates, 0..) |c, i| {
        var rarity: f64 = 0.0;
        var mask = c.matched_mask;
        while (mask != 0) {
            const bit: u6 = @intCast(@ctz(mask));
            mask &= mask - 1;
            if (bit < idf.len) rarity += idf[bit];
        }
        const ep = episodes[c.episode_ord];
        scores[i] = rarity * recencyMultiplier(ep.event_time_ms, params.now_ms, params.recency_boost);
    }

    const order = try gpa.alloc(u32, candidates.len);
    errdefer gpa.free(order);
    for (order, 0..) |*slot, i| slot.* = @intCast(i);

    const Ctx = struct {
        candidates: []const Candidate,
        episodes: []const EpisodeInfo,
        scores: []const f64,
        fn lessThan(ctx: @This(), a: u32, b: u32) bool {
            if (ctx.scores[a] != ctx.scores[b]) return ctx.scores[a] > ctx.scores[b];
            const ca = ctx.candidates[a];
            const cb = ctx.candidates[b];
            const pa = ctx.episodes[ca.episode_ord].rel_path;
            const pb = ctx.episodes[cb.episode_ord].rel_path;
            return switch (std.mem.order(u8, pa, pb)) {
                .lt => true,
                .gt => false,
                .eq => ca.line_no < cb.line_no,
            };
        }
    };
    std.mem.sort(u32, order, Ctx{
        .candidates = candidates,
        .episodes = episodes,
        .scores = scores,
    }, Ctx.lessThan);

    // Span dedup after ranking: the best-scoring line in each
    // `context_window * 2` line bucket of an episode survives, so adjacent
    // matches collapse to one result region (v1 semantics).
    const bucket_span: u32 = @max(params.context_window * 2, 1);
    var seen: std.AutoHashMapUnmanaged(u64, void) = .empty;
    defer seen.deinit(gpa);
    var kept: std.ArrayList(u32) = .empty;
    errdefer kept.deinit(gpa);
    for (order) |idx| {
        const c = candidates[idx];
        if (params.min_score > 0 and scores[idx] < params.min_score) continue;
        const key = (@as(u64, c.episode_ord) << 32) | (c.line_no / bucket_span);
        const entry = try seen.getOrPut(gpa, key);
        if (entry.found_existing) continue;
        try kept.append(gpa, idx);
    }
    const kept_owned = try kept.toOwnedSlice(gpa);
    gpa.free(order); // after the last fallible call: its errdefer is now disarmed
    return .{ .order = kept_owned, .scores = scores };
}

// --- Confidence ---

pub const Confidence = enum { low, medium, high };

/// Versioned band policy (`aj-conf.v1`): the floor is the legacy weak-query
/// bar. Reported separately from score; a whole-response `no_match` decision
/// stays with the caller's `min_score` floor.
pub fn confidenceOf(score: f64, floor: f64) Confidence {
    if (floor <= 0) return .high;
    if (score >= 2.0 * floor) return .high;
    if (score >= floor) return .medium;
    return .low;
}

// --- Cursor ---

pub const cursor_prefix = "aj1.";
pub const cursor_guard_hex_len = 8;
pub const cursor_max_len = cursor_prefix.len + 20 + 1 + cursor_guard_hex_len;

pub const CursorInputs = struct {
    query: []const u8,
    world: []const u8,
    scope: []const u8,
    lanes: []const u8,
    alias_digest: []const u8,
};

/// A cursor is only valid against the query/world/scorer state that minted
/// it; the guard makes replay against anything else a typed `malformed`.
pub fn cursorGuardHex(inputs: CursorInputs) [cursor_guard_hex_len]u8 {
    var h = std.crypto.hash.sha2.Sha256.init(.{});
    h.update(scorer_version);
    for ([_][]const u8{
        inputs.query, inputs.world, inputs.scope, inputs.lanes, inputs.alias_digest,
    }) |field| {
        var len_buf: [20]u8 = undefined;
        h.update(std.fmt.bufPrint(&len_buf, "\x00{d}\x00", .{field.len}) catch unreachable);
        h.update(field);
    }
    var sum: [std.crypto.hash.sha2.Sha256.digest_length]u8 = undefined;
    h.final(&sum);
    var out: [cursor_guard_hex_len]u8 = undefined;
    _ = std.fmt.bufPrint(&out, "{x}", .{sum[0 .. cursor_guard_hex_len / 2]}) catch unreachable;
    return out;
}

pub fn cursorEncode(buf: *[cursor_max_len]u8, offset: u64, inputs: CursorInputs) []const u8 {
    const guard = cursorGuardHex(inputs);
    return std.fmt.bufPrint(buf, "{s}{d}.{s}", .{ cursor_prefix, offset, &guard }) catch unreachable;
}

pub fn cursorDecode(cursor: []const u8, inputs: CursorInputs) error{Malformed}!u64 {
    if (!std.mem.startsWith(u8, cursor, cursor_prefix)) return error.Malformed;
    const rest = cursor[cursor_prefix.len..];
    const dot = std.mem.lastIndexOfScalar(u8, rest, '.') orelse return error.Malformed;
    const offset = std.fmt.parseInt(u64, rest[0..dot], 10) catch return error.Malformed;
    const guard = cursorGuardHex(inputs);
    if (!std.mem.eql(u8, rest[dot + 1 ..], &guard)) return error.Malformed;
    return offset;
}

// --- Tests ---

test "tokenizer parity with the v1 lexical contract" {
    const gpa = std.testing.allocator;
    const terms = try extractTerms(gpa, "What did the Cinder routing-mode use?");
    defer terms.deinit(gpa);
    try std.testing.expectEqual(@as(usize, 3), terms.items.len);
    try std.testing.expectEqualStrings("cinder", terms.items[0]);
    try std.testing.expectEqualStrings("routing", terms.items[1]);
    try std.testing.expectEqualStrings("mode", terms.items[2]);
    try std.testing.expect(!terms.truncated);
}

test "tokenizer drops short tokens, stop words, and non-ASCII" {
    const gpa = std.testing.allocator;
    {
        const terms = try extractTerms(gpa, "naïve — ✓ it we is");
        defer terms.deinit(gpa);
        try std.testing.expectEqual(@as(usize, 0), terms.items.len);
    }
    {
        // Underscores join tokens (\w parity); duplicates are preserved.
        const terms = try extractTerms(gpa, "gguf gguf snake_case");
        defer terms.deinit(gpa);
        try std.testing.expectEqual(@as(usize, 3), terms.items.len);
        try std.testing.expectEqualStrings("gguf", terms.items[0]);
        try std.testing.expectEqualStrings("gguf", terms.items[1]);
        try std.testing.expectEqualStrings("snake_case", terms.items[2]);
    }
}

test "index-side token iterator lowercases and filters like the query side" {
    var it = tokenizeLine("The Cinder ROUTING-mode? naïve Q8 x " ++ "a" ** 200);
    try std.testing.expectEqualStrings("cinder", it.next().?);
    try std.testing.expectEqualStrings("routing", it.next().?);
    try std.testing.expectEqualStrings("mode", it.next().?);
    // Two-byte tokens are indexed (alias values like "q8" need them);
    // "naïve" splits at the non-ASCII byte into fragments below the floor;
    // "x" is short; the 200-byte run exceeds the token cap.
    try std.testing.expectEqualStrings("na", it.next().?);
    try std.testing.expectEqualStrings("ve", it.next().?);
    try std.testing.expectEqualStrings("q8", it.next().?);
    try std.testing.expectEqual(@as(?[]const u8, null), it.next());
}

test "term cap truncates with a flag" {
    const gpa = std.testing.allocator;
    var query: std.ArrayList(u8) = .empty;
    defer query.deinit(gpa);
    for (0..contracts.max_query_terms + 5) |i| {
        try query.print(gpa, "term{d:0>3} ", .{i});
    }
    const terms = try extractTerms(gpa, query.items);
    defer terms.deinit(gpa);
    try std.testing.expectEqual(contracts.max_query_terms, terms.items.len);
    try std.testing.expect(terms.truncated);
}

test "recency vectors match v1" {
    const day_ms = std.time.ms_per_day;
    const now: u64 = 1785240000000;
    try std.testing.expectEqual(@as(f64, 2.0), recencyMultiplier(now, now, 1.0));
    try std.testing.expectEqual(@as(f64, 1.5), recencyMultiplier(now - day_ms, now, 1.0));
    // Future timestamps get no boost.
    try std.testing.expectEqual(@as(f64, 1.0), recencyMultiplier(now + day_ms, now, 1.0));
    // Sub-day age floors to zero days.
    try std.testing.expectEqual(@as(f64, 2.0), recencyMultiplier(now - (day_ms - 1), now, 1.0));
}

test "idf weight: absent and ubiquitous terms contribute nothing" {
    try std.testing.expectEqual(@as(f64, 0.0), idfWeight(87, 0));
    try std.testing.expectEqual(@as(f64, 0.0), idfWeight(87, 87));
    try std.testing.expect(idfWeight(87, 1) > 4.4);
}

fn rankFixture(gpa: std.mem.Allocator) !Ranked {
    // Two episodes; episode 0 is older. Term 0 is rare (idf 2.0), term 1
    // common (idf 0.1).
    const episodes = [_]EpisodeInfo{
        .{ .episode_id = "aj1-old", .rel_path = "worlds/w/2026/07/01/aj1-old.md", .event_time_ms = 0 },
        .{ .episode_id = "aj1-new", .rel_path = "worlds/w/2026/07/28/aj1-new.md", .event_time_ms = 1785240000000 },
    };
    const candidates = [_]Candidate{
        .{ .episode_ord = 0, .line_no = 20, .matched_mask = 0b01 }, // rare, old
        .{ .episode_ord = 1, .line_no = 20, .matched_mask = 0b10 }, // common, new
        .{ .episode_ord = 1, .line_no = 22, .matched_mask = 0b11 }, // both, new, same bucket as above
        .{ .episode_ord = 1, .line_no = 40, .matched_mask = 0b10 }, // common, new, distinct bucket
    };
    const idf = [_]f64{ 2.0, 0.1 };
    return rank(gpa, &candidates, &episodes, &idf, .{
        .now_ms = 1785240000000,
        .recency_boost = 1.0,
        .context_window = 3,
    });
}

test "rank orders by rarity times recency and dedups spans" {
    const gpa = std.testing.allocator;
    const ranked = try rankFixture(gpa);
    defer ranked.deinit(gpa);
    // Candidate 2 scores (2.0+0.1)*2.0 = 4.2; candidate 0 scores 2.0*1.0;
    // candidate 1 (0.2) is deduped away by candidate 2 (same 6-line bucket);
    // candidate 3 (0.2) survives in its own bucket.
    try std.testing.expectEqual(@as(usize, 3), ranked.order.len);
    try std.testing.expectEqual(@as(u32, 2), ranked.order[0]);
    try std.testing.expectEqual(@as(u32, 0), ranked.order[1]);
    try std.testing.expectEqual(@as(u32, 3), ranked.order[2]);
    try std.testing.expectEqual(@as(f64, 4.2), ranked.scores[2]);
}

test "rank ties break on path then line, deterministically" {
    const gpa = std.testing.allocator;
    const episodes = [_]EpisodeInfo{
        .{ .episode_id = "aj1-b", .rel_path = "worlds/w/2026/07/02/aj1-b.md", .event_time_ms = 5 },
        .{ .episode_id = "aj1-a", .rel_path = "worlds/w/2026/07/01/aj1-a.md", .event_time_ms = 5 },
    };
    // Arrival order is b-then-a; ranking must not preserve it.
    const candidates = [_]Candidate{
        .{ .episode_ord = 0, .line_no = 30, .matched_mask = 0b1 },
        .{ .episode_ord = 1, .line_no = 30, .matched_mask = 0b1 },
        .{ .episode_ord = 1, .line_no = 90, .matched_mask = 0b1 },
    };
    const idf = [_]f64{1.0};
    const ranked = try rank(gpa, &candidates, &episodes, &idf, .{ .now_ms = 5 });
    defer ranked.deinit(gpa);
    try std.testing.expectEqual(@as(u32, 1), ranked.order[0]); // 07/01 path sorts first
    try std.testing.expectEqual(@as(u32, 2), ranked.order[1]); // same path, higher line
    try std.testing.expectEqual(@as(u32, 0), ranked.order[2]);
}

test "min_score floor drops weak results from order but keeps scores" {
    const gpa = std.testing.allocator;
    const episodes = [_]EpisodeInfo{
        .{ .episode_id = "aj1-x", .rel_path = "worlds/w/e/aj1-x.md", .event_time_ms = 5 },
    };
    const candidates = [_]Candidate{
        .{ .episode_ord = 0, .line_no = 20, .matched_mask = 0b1 },
        .{ .episode_ord = 0, .line_no = 90, .matched_mask = 0b10 },
    };
    const idf = [_]f64{ 3.0, 0.4 };
    // Second candidate scores 0.4 * 2.0 = 0.8 < 1.0 and is floored out; a
    // score exactly at the floor would survive (legacy `>=` semantics).
    const ranked = try rank(gpa, &candidates, &episodes, &idf, .{ .now_ms = 5, .min_score = 1.0 });
    defer ranked.deinit(gpa);
    try std.testing.expectEqual(@as(usize, 1), ranked.order.len);
    try std.testing.expectEqual(@as(u32, 0), ranked.order[0]);
}

test "confidence bands off the floor" {
    try std.testing.expectEqual(Confidence.low, confidenceOf(2.9, 3.0));
    try std.testing.expectEqual(Confidence.medium, confidenceOf(3.0, 3.0));
    try std.testing.expectEqual(Confidence.high, confidenceOf(6.0, 3.0));
    try std.testing.expectEqual(Confidence.high, confidenceOf(0.0, 0.0));
}

test "cursor round trip and guard mismatch" {
    const inputs: CursorInputs = .{
        .query = "cinder routing",
        .world = "willow",
        .scope = "",
        .lanes = "conversation,delegated_work,imported_legacy",
        .alias_digest = "abc123",
    };
    var buf: [cursor_max_len]u8 = undefined;
    const cursor = cursorEncode(&buf, 20, inputs);
    try std.testing.expect(std.mem.startsWith(u8, cursor, "aj1.20."));
    try std.testing.expectEqual(@as(u64, 20), try cursorDecode(cursor, inputs));

    var other = inputs;
    other.query = "different query";
    try std.testing.expectError(error.Malformed, cursorDecode(cursor, other));
    try std.testing.expectError(error.Malformed, cursorDecode("aj1.x.deadbeef", inputs));
    try std.testing.expectError(error.Malformed, cursorDecode("nonsense", inputs));
}

fn rankSweep(gpa: std.mem.Allocator) !void {
    const terms = try extractTerms(gpa, "cinder cinder routing-mode use?");
    defer terms.deinit(gpa);
    const ranked = try rankFixture(gpa);
    ranked.deinit(gpa);
}

test "allocation failures propagate without leaking" {
    try std.testing.checkAllAllocationFailures(std.testing.allocator, rankSweep, .{});
}
