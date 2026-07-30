//! `memory_search` and `memory_get` orchestration.
//!
//! Search is not evidence opening: `search` returns ranked, stable evidence
//! references with bounded snippets; `get` opens one reference with
//! explicit line bounds and validates identity and revision against the
//! file on disk. Failures are typed outcomes, never Zig errors — recall
//! degrading is a normal result the caller renders, not an exception.
//!
//! Discovery pipeline: query terms → additive alias expansion →
//! vocabulary substring scan → postings fetch under world/scope/lane
//! filters → per-line crediting against the source text (word-start
//! boundary by default, so `hang` credits `hanging` but not `changed`;
//! `.substring` preserves v1 parity's infix recall) → pure scorer in
//! `retrieval.zig` → span dedup, floor, page.

const std = @import("std");
const Io = std.Io;
const contracts = @import("contracts.zig");
const db = @import("db.zig");
const index_mod = @import("index.zig");
const retrieval = @import("retrieval.zig");
const aliases = @import("aliases.zig");
const frontmatter = @import("frontmatter.zig");
const render = @import("render.zig");
const store = @import("store.zig");
const identity = @import("identity.zig");

pub const default_lanes = [_]contracts.Lane{
    .conversation, .delegated_work, .imported_legacy,
};

/// Cap on vocabulary tokens matched by one query's substring scan; beyond
/// it discovery is truncated and flagged in `detail`.
pub const max_vocab_matches: usize = 1024;

/// Needles shorter than this are excluded from the vocabulary scan when
/// longer needles exist: a 2-byte needle ("pi") substring-matches a huge
/// share of the vocabulary, floods `max_vocab_matches`, and the scan's
/// early break then silently drops discovery for the query's remaining
/// terms. A query whose tokens are all short still scans with them, so
/// curated short alias values ("q8") keep working on their own.
pub const min_needle_len: usize = 3;

/// How a term is credited against a matched line's text.
///
/// `substring`: v1 parity — any occurrence counts, so "hang" credits
/// lines containing "change". `word_start`: the occurrence must begin at
/// a token boundary — "hang" credits "hanging" but not "change", and
/// "config" still credits "configuration". `whole_word`: both edges must
/// be token boundaries — "hang" credits only the exact word.
pub const CreditMode = enum { substring, word_start, whole_word };

/// Scoring knobs, resolved from owner config by the caller.
pub const Knobs = struct {
    context_window: u32 = 3,
    recency_boost: f64 = 1.0,
    min_score: f64 = 0.0,
    confidence_floor: f64 = 3.0,
};

pub const SearchRequest = struct {
    query: []const u8,
    world: []const u8,
    scope: ?[]const u8 = null,
    lanes: []const contracts.Lane = &default_lanes,
    limit: u32 = 10,
    cursor: ?[]const u8 = null,
    /// Injectable clock (epoch ms) for deterministic recency.
    now_ms: u64,
    knobs: Knobs = .{},
    credit_mode: CreditMode = .word_start,
};

pub const Hit = struct {
    episode_id: []const u8,
    /// `sha256:<hex>` revision this evidence was ranked against.
    revision: []const u8,
    path: []const u8,
    scope: []const u8,
    lane: contracts.Lane,
    capture_policy: []const u8,
    event_time_ms: u64,
    /// 1-based matched line in the source file.
    line: u32,
    snippet_start: u32,
    snippet_end: u32,
    /// Bounded context lines; empty when the source changed between
    /// ranking and rendering.
    snippet: []const u8,
    matched_terms: []const []const u8,
    score: f64,
    confidence: retrieval.Confidence,
};

pub const SearchOutput = struct {
    state: std.heap.ArenaAllocator.State,
    gpa: std.mem.Allocator,

    outcome: contracts.Outcome,
    query_terms: []const []const u8 = &.{},
    alias_terms: []const []const u8 = &.{},
    hits: []const Hit = &.{},
    /// True post-dedup, post-floor result count (not the raw match count).
    total: u64 = 0,
    next_cursor: ?[]const u8 = null,
    best_score: f64 = 0.0,
    alias_digest: [64]u8 = @splat('0'),
    freshness: contracts.IndexFreshness = .unavailable,
    indexed: u64 = 0,
    source: u64 = 0,
    /// Candidates dropped because their source file no longer matches the
    /// indexed revision (edited or vanished since indexing).
    edited_excluded: u64 = 0,
    detail: ?[]const u8 = null,

    pub fn deinit(self: *const SearchOutput) void {
        self.state.promote(self.gpa).deinit();
    }
};

pub fn search(
    gpa: std.mem.Allocator,
    io: Io,
    root: Io.Dir,
    idx: *index_mod.Index,
    alias_map: *const aliases.AliasMap,
    req: SearchRequest,
) error{OutOfMemory}!SearchOutput {
    var arena_owner = std.heap.ArenaAllocator.init(gpa);
    errdefer arena_owner.deinit();

    var out: SearchOutput = .{
        .state = undefined,
        .gpa = gpa,
        .outcome = .internal_error,
        .alias_digest = alias_map.digest_hex,
    };
    searchInner(arena_owner.allocator(), gpa, io, root, idx, alias_map, req, &out) catch |err| {
        switch (err) {
            error.OutOfMemory => return error.OutOfMemory,
            error.Busy => out.outcome = .timeout,
            else => out.outcome = .unavailable,
        }
        out.detail = @errorName(err);
    };
    out.state = arena_owner.state;
    return out;
}

fn searchInner(
    arena: std.mem.Allocator,
    gpa: std.mem.Allocator,
    io: Io,
    root: Io.Dir,
    idx: *index_mod.Index,
    alias_map: *const aliases.AliasMap,
    req: SearchRequest,
    out: *SearchOutput,
) (db.Error || error{OutOfMemory})!void {
    // Index health first: an empty projection over a nonempty corpus is
    // `index_stale`, never `no_match`. Files the last sync deliberately
    // excluded count as accounted for, matching `status`.
    out.indexed = try idx.episodeCount();
    out.source = store.countEpisodes(root, io);
    out.freshness = if (out.indexed + idx.excludedCount() == out.source) .fresh else .stale;

    // --- Terms and alias expansion ---
    const base = try retrieval.extractTerms(arena, req.query);
    var terms_truncated = base.truncated;
    out.query_terms = base.items;
    if (base.items.len == 0) {
        out.outcome = .no_match;
        return;
    }

    var have: std.StringArrayHashMapUnmanaged(void) = .empty;
    for (base.items) |t| _ = try have.getOrPut(arena, t);
    var alias_list: std.ArrayList([]const u8) = .empty;
    for (base.items) |t| {
        const values = alias_map.get(t) orelse continue;
        // Dupe into the search arena so `SearchOutput` (alias_terms,
        // matched_terms) is self-owned and never borrows the caller's
        // AliasMap, which may be freed before the output is rendered.
        for (values) |v| {
            if (have.contains(v)) continue;
            const copy = try arena.dupe(u8, v);
            try have.put(arena, copy, {});
            try alias_list.append(arena, copy);
        }
    }
    out.alias_terms = alias_list.items;

    // v1 parity quirk, preserved deliberately: without aliases the raw
    // duplicate-preserving list scores (duplicate query words weigh twice);
    // once aliases fire, the deduplicated union is used instead.
    var final_terms: []const []const u8 = undefined;
    if (alias_list.items.len == 0) {
        final_terms = base.items;
    } else {
        final_terms = have.keys();
    }
    if (final_terms.len > contracts.max_query_terms) {
        final_terms = final_terms[0..contracts.max_query_terms];
        terms_truncated = true;
    }

    // --- Discovery: vocabulary substring scan ---
    // Needles are the index-token components of each term, so a phrase
    // value like "llama.cpp" discovers via "llama"/"cpp" and is credited
    // by full-substring match on the line text below.
    var needles: std.StringArrayHashMapUnmanaged(void) = .empty;
    var short_needles: std.StringArrayHashMapUnmanaged(void) = .empty;
    for (final_terms) |t| {
        var it = retrieval.tokenizeLine(t);
        while (it.next()) |needle| {
            const bucket = if (needle.len >= min_needle_len) &needles else &short_needles;
            if (bucket.contains(needle)) continue;
            try bucket.put(arena, try arena.dupe(u8, needle), {});
        }
    }
    const needle_keys = if (needles.count() > 0) needles.keys() else short_needles.keys();

    var vocab_matches: std.ArrayList([]const u8) = .empty;
    var vocab_truncated = false;
    {
        var it = try idx.vocabIterator(req.world);
        defer it.deinit();
        scan: while (try it.next()) |token| {
            for (needle_keys) |needle| {
                if (std.mem.indexOf(u8, token, needle) != null) {
                    if (vocab_matches.items.len >= max_vocab_matches) {
                        vocab_truncated = true;
                        break :scan;
                    }
                    try vocab_matches.append(arena, try arena.dupe(u8, token));
                    continue :scan;
                }
            }
        }
    }

    // --- Candidate accumulation from postings ---
    const EpisodeAccum = struct {
        meta: retrieval.EpisodeInfo,
        digest_hex: []const u8,
        scope: []const u8,
        lane: contracts.Lane,
        capture_policy: []const u8,
        body_line: u32,
        lines: std.ArrayList(u32) = .empty,
        union_mask: u64 = 0,
    };
    var episode_ords: std.StringArrayHashMapUnmanaged(u32) = .empty;
    var episodes: std.ArrayList(EpisodeAccum) = .empty;
    var seen_lines: std.AutoArrayHashMapUnmanaged(u64, void) = .empty;

    for (vocab_matches.items) |token| {
        var rows = try idx.postingsForTerm(gpa, token, req.world, req.scope, req.lanes);
        defer rows.deinit();
        while (try rows.next()) |row| {
            const ord: u32 = episode_ords.get(row.episode_id) orelse blk: {
                const id = try arena.dupe(u8, row.episode_id);
                const new_ord: u32 = @intCast(episodes.items.len);
                try episodes.append(arena, .{
                    .meta = .{
                        .episode_id = id,
                        .rel_path = try arena.dupe(u8, row.rel_path),
                        .event_time_ms = row.event_time_ms,
                    },
                    .digest_hex = try arena.dupe(u8, row.digest_hex),
                    .scope = try arena.dupe(u8, row.scope),
                    .lane = row.lane,
                    .capture_policy = try arena.dupe(u8, row.capture_policy),
                    .body_line = row.body_line,
                });
                try episode_ords.put(arena, id, new_ord);
                break :blk new_ord;
            };
            const key = (@as(u64, ord) << 32) | row.line_no;
            const gop = try seen_lines.getOrPut(arena, key);
            if (gop.found_existing) continue;
            try episodes.items[ord].lines.append(arena, row.line_no);
        }
    }

    if (episodes.items.len == 0) {
        out.outcome = if (out.indexed == 0 and out.source > 0 and out.freshness == .stale)
            .index_stale
        else
            .no_match;
        if (vocab_truncated or terms_truncated) out.detail = "discovery_truncated";
        return;
    }

    // --- Per-line crediting against source text ---
    var candidates: std.ArrayList(retrieval.Candidate) = .empty;
    const df = try arena.alloc(u64, final_terms.len);
    @memset(df, 0);

    for (episodes.items, 0..) |*ep, ord| {
        std.mem.sort(u32, ep.lines.items, {}, std.sort.asc(u32));
        const content = readContained(gpa, io, root, ep.meta.rel_path) catch |err| switch (err) {
            error.OutOfMemory => return error.OutOfMemory,
            else => {
                out.edited_excluded += 1;
                continue;
            },
        };
        defer gpa.free(content);
        const current = render.frontmatterDigestHex(content) orelse "";
        if (!std.mem.eql(u8, current, ep.digest_hex)) {
            out.edited_excluded += 1;
            continue;
        }

        var lines = std.mem.splitScalar(u8, content, '\n');
        var line_no: u32 = 1;
        var want: usize = 0;
        while (lines.next()) |line| : (line_no += 1) {
            if (want >= ep.lines.items.len) break;
            if (line_no != ep.lines.items[want]) continue;
            want += 1;
            var mask: u64 = 0;
            for (final_terms, 0..) |term, i| {
                if (creditLine(line, term, req.credit_mode)) {
                    mask |= @as(u64, 1) << @intCast(i);
                }
            }
            if (mask == 0) continue;
            try candidates.append(arena, .{
                .episode_ord = @intCast(ord),
                .line_no = line_no,
                .matched_mask = mask,
            });
            ep.union_mask |= mask;
        }
        var mask = ep.union_mask;
        while (mask != 0) {
            const bit: u6 = @intCast(@ctz(mask));
            mask &= mask - 1;
            if (bit < df.len) df[bit] += 1;
        }
    }

    if (candidates.items.len == 0) {
        out.outcome = .no_match;
        if (vocab_truncated or terms_truncated) out.detail = "discovery_truncated";
        return;
    }

    // --- Score and rank ---
    var credited_episodes: u64 = 0;
    for (episodes.items) |ep| {
        if (ep.union_mask != 0) credited_episodes += 1;
    }
    // Floor N at the credited-episode count (and 1): stats can lag the
    // postings after partial damage, and df > N would flip an IDF weight
    // negative.
    const stats_n = try idx.statsEpisodeCount(req.world);
    const n = @max(@max(stats_n, credited_episodes), 1);
    const idf = try arena.alloc(f64, final_terms.len);
    for (idf, df) |*w, d| w.* = retrieval.idfWeight(n, d);

    const episode_infos = try arena.alloc(retrieval.EpisodeInfo, episodes.items.len);
    for (episode_infos, episodes.items) |*info, ep| info.* = ep.meta;

    const ranked = try retrieval.rank(arena, candidates.items, episode_infos, idf, .{
        .now_ms = req.now_ms,
        .recency_boost = req.knobs.recency_boost,
        .min_score = req.knobs.min_score,
        .context_window = req.knobs.context_window,
    });
    out.total = ranked.order.len;
    if (ranked.order.len > 0) out.best_score = ranked.scores[ranked.order[0]];

    // --- Cursor and page ---
    const cursor_inputs: retrieval.CursorInputs = .{
        .query = req.query,
        .world = req.world,
        .scope = req.scope orelse "",
        .lanes = try lanesTag(arena, req.lanes),
        .alias_digest = &out.alias_digest,
    };
    var offset: u64 = 0;
    if (req.cursor) |cursor| {
        offset = retrieval.cursorDecode(cursor, cursor_inputs) catch {
            out.outcome = .malformed;
            out.detail = "cursor does not match this query";
            return;
        };
    }
    if (ranked.order.len == 0) {
        out.outcome = .no_match;
        if (vocab_truncated or terms_truncated) out.detail = "discovery_truncated";
        return;
    }
    const start: usize = @min(offset, ranked.order.len);
    const limit: usize = @max(@min(req.limit, contracts.max_results_limit), 1);
    const end = @min(start + limit, ranked.order.len);
    if (end < ranked.order.len) {
        var buf: [retrieval.cursor_max_len]u8 = undefined;
        out.next_cursor = try arena.dupe(u8, retrieval.cursorEncode(&buf, end, cursor_inputs));
    }

    // --- Render page hits with bounded snippets ---
    var hits = try arena.alloc(Hit, end - start);
    for (ranked.order[start..end], 0..) |cand_idx, hit_i| {
        const cand = candidates.items[cand_idx];
        const ep = episodes.items[cand.episode_ord];

        var matched: std.ArrayList([]const u8) = .empty;
        var mask = cand.matched_mask;
        while (mask != 0) {
            const bit: u6 = @intCast(@ctz(mask));
            mask &= mask - 1;
            if (bit >= final_terms.len) continue;
            const term = final_terms[bit];
            const dup = for (matched.items) |m| {
                if (std.mem.eql(u8, m, term)) break true;
            } else false;
            if (!dup) try matched.append(arena, term);
        }

        const snippet = try renderSnippet(arena, gpa, io, root, ep.digest_hex, ep.meta.rel_path, .{
            .line = cand.line_no,
            .body_line = ep.body_line,
            .context_window = req.knobs.context_window,
        });
        hits[hit_i] = .{
            .episode_id = ep.meta.episode_id,
            .revision = try std.fmt.allocPrint(arena, "{s}{s}", .{
                identity.digest_prefix, ep.digest_hex,
            }),
            .path = ep.meta.rel_path,
            .scope = ep.scope,
            .lane = ep.lane,
            .capture_policy = ep.capture_policy,
            .event_time_ms = ep.meta.event_time_ms,
            .line = cand.line_no,
            .snippet_start = snippet.start,
            .snippet_end = snippet.end,
            .snippet = snippet.text,
            .matched_terms = matched.items,
            .score = ranked.scores[cand_idx],
            .confidence = retrieval.confidenceOf(ranked.scores[cand_idx], req.knobs.confidence_floor),
        };
    }
    out.hits = hits;
    out.outcome = .match;
    if (vocab_truncated or terms_truncated) out.detail = "discovery_truncated";
}

/// True when `term` occurs in `line` under `mode`'s boundary rule
/// (case-insensitive). Boundaries use the index-token alphabet, so a
/// phrase term ("out of memory", "llama.cpp") is checked at the edges of
/// the whole occurrence and its interior punctuation needs no special
/// handling.
pub fn creditLine(line: []const u8, term: []const u8, mode: CreditMode) bool {
    var from: usize = 0;
    while (std.ascii.indexOfIgnoreCasePos(line, from, term)) |pos| : (from = pos + 1) {
        if (mode == .substring) return true;
        const start_ok = pos == 0 or !retrieval.isIndexTokenByte(line[pos - 1]);
        if (!start_ok) continue;
        if (mode == .word_start) return true;
        const end = pos + term.len;
        if (end >= line.len or !retrieval.isIndexTokenByte(line[end])) return true;
    }
    return false;
}

fn lanesTag(arena: std.mem.Allocator, lanes: []const contracts.Lane) error{OutOfMemory}![]const u8 {
    var buf: std.ArrayList(u8) = .empty;
    for (lanes, 0..) |lane, i| {
        if (i > 0) try buf.append(arena, ',');
        try buf.appendSlice(arena, @tagName(lane));
    }
    return buf.items;
}

const Snippet = struct {
    text: []const u8,
    start: u32,
    end: u32,
};

const SnippetSpec = struct {
    line: u32,
    body_line: u32,
    context_window: u32,
};

/// Reads the source again and renders `±context_window` lines clamped to
/// the body, each line capped at a codepoint boundary, the whole snippet
/// capped at `max_snippet_bytes`. A file whose revision changed since
/// ranking renders an empty snippet — the reference stays valid for
/// `memory_get`, which will report `stale_revision` honestly.
fn renderSnippet(
    arena: std.mem.Allocator,
    gpa: std.mem.Allocator,
    io: Io,
    root: Io.Dir,
    expected_digest: []const u8,
    rel_path: []const u8,
    spec: SnippetSpec,
) error{OutOfMemory}!Snippet {
    const empty: Snippet = .{ .text = "", .start = spec.line, .end = spec.line };
    const content = readContained(gpa, io, root, rel_path) catch |err| switch (err) {
        error.OutOfMemory => return error.OutOfMemory,
        else => return empty,
    };
    defer gpa.free(content);
    const current = render.frontmatterDigestHex(content) orelse "";
    if (!std.mem.eql(u8, current, expected_digest)) return empty;

    const first = @max(spec.body_line, spec.line -| spec.context_window);
    const last = spec.line + spec.context_window;

    var text: std.ArrayList(u8) = .empty;
    var start: u32 = 0;
    var end: u32 = 0;
    var any = false;
    var lines = std.mem.splitScalar(u8, content, '\n');
    var line_no: u32 = 1;
    while (lines.next()) |line| : (line_no += 1) {
        if (line_no < first) continue;
        if (line_no > last) break;
        const capped = capAtCodepoint(line, contracts.max_snippet_line_bytes);
        if (text.items.len + capped.len + 1 > contracts.max_snippet_bytes) {
            if (line_no <= spec.line) {
                // Never render a snippet that omits the matched line.
                text.clearRetainingCapacity();
                start = 0;
                any = false;
            } else break;
        }
        if (start == 0) start = line_no;
        // Join on a flag, not buffer length: empty lines are real lines and
        // must keep the snippet's line numbering aligned.
        if (any) try text.append(arena, '\n');
        any = true;
        try text.appendSlice(arena, capped);
        end = line_no;
    }
    if (start == 0) return empty;
    return .{ .text = text.items, .start = start, .end = end };
}

/// Byte cap that never splits a UTF-8 sequence.
fn capAtCodepoint(line: []const u8, max: usize) []const u8 {
    if (line.len <= max) return line;
    var cut = max;
    while (cut > 0 and (line[cut] & 0b1100_0000) == 0b1000_0000) cut -= 1;
    return line[0..cut];
}

const ReadContainedError = error{ OutOfMemory, Unavailable };

/// Reads one episode file under the journal root with containment: relative
/// validated components only, no symlink following, resolution stays
/// beneath the root.
fn readContained(
    gpa: std.mem.Allocator,
    io: Io,
    root: Io.Dir,
    rel_path: []const u8,
) ReadContainedError![]u8 {
    if (!containedPath(rel_path)) return error.Unavailable;
    var file = root.openFile(io, rel_path, .{
        .follow_symlinks = false,
        .resolve_beneath = true,
    }) catch return error.Unavailable;
    defer file.close(io);
    var reader = file.reader(io, &.{});
    return reader.interface.allocRemaining(
        gpa,
        .limited(contracts.max_episode_file_bytes),
    ) catch |err| switch (err) {
        error.OutOfMemory => return error.OutOfMemory,
        else => return error.Unavailable,
    };
}

pub fn containedPath(rel_path: []const u8) bool {
    if (rel_path.len == 0 or rel_path[0] == '/') return false;
    var components = std.mem.splitScalar(u8, rel_path, '/');
    while (components.next()) |component| {
        if (component.len == 0) return false;
        if (std.mem.eql(u8, component, ".") or std.mem.eql(u8, component, "..")) return false;
        // Windows path separator: never a valid byte in a stored rel_path.
        if (std.mem.indexOfScalar(u8, component, '\\') != null) return false;
    }
    return true;
}

// --- memory_get ---

pub const GetRequest = struct {
    episode_id: []const u8,
    /// Accepted with or without the `sha256:` prefix.
    revision: []const u8,
    /// Optional path hint from a search hit; the index is consulted when
    /// absent or wrong (moves preserve identity after sync).
    path_hint: ?[]const u8 = null,
    expected_world: ?[]const u8 = null,
    expected_scope: ?[]const u8 = null,
    /// 0 means "start of body".
    line_start: u32 = 0,
    /// 0 means "line_start + max span".
    line_end: u32 = 0,
};

pub const GetOutput = struct {
    state: std.heap.ArenaAllocator.State,
    gpa: std.mem.Allocator,

    outcome: contracts.Outcome,
    episode_id: []const u8 = "",
    /// Current revision on disk (`sha256:<hex>`), which on
    /// `stale_revision` is the replacement reference.
    revision: ?[]const u8 = null,
    path: ?[]const u8 = null,
    world: ?[]const u8 = null,
    scope: ?[]const u8 = null,
    lane: ?contracts.Lane = null,
    capture_policy: ?[]const u8 = null,
    line_start: u32 = 0,
    line_end: u32 = 0,
    content: []const u8 = "",
    /// Recalled text is untrusted evidence, never instructions.
    trust: []const u8 = "untrusted_evidence",
    detail: ?[]const u8 = null,

    pub fn deinit(self: *const GetOutput) void {
        self.state.promote(self.gpa).deinit();
    }
};

pub fn get(
    gpa: std.mem.Allocator,
    io: Io,
    root: Io.Dir,
    idx: *index_mod.Index,
    req: GetRequest,
) error{OutOfMemory}!GetOutput {
    var arena_owner = std.heap.ArenaAllocator.init(gpa);
    errdefer arena_owner.deinit();

    var out: GetOutput = .{
        .state = undefined,
        .gpa = gpa,
        .outcome = .internal_error,
    };
    getInner(arena_owner.allocator(), gpa, io, root, idx, req, &out) catch |err| {
        switch (err) {
            error.OutOfMemory => return error.OutOfMemory,
            error.Busy => out.outcome = .timeout,
            else => out.outcome = .unavailable,
        }
        out.detail = @errorName(err);
    };
    out.state = arena_owner.state;
    return out;
}

fn getInner(
    arena: std.mem.Allocator,
    gpa: std.mem.Allocator,
    io: Io,
    root: Io.Dir,
    idx: *index_mod.Index,
    req: GetRequest,
    out: *GetOutput,
) (db.Error || error{OutOfMemory})!void {
    out.episode_id = try arena.dupe(u8, req.episode_id);
    if (!validEpisodeId(req.episode_id)) {
        out.outcome = .malformed;
        out.detail = "episode_id must be aj1-<32 hex>";
        return;
    }
    const requested_hex = stripDigestPrefix(req.revision);
    if (requested_hex.len != identity.digest_hex_len) {
        out.outcome = .malformed;
        out.detail = "revision must be sha256:<64 hex>";
        return;
    }
    if (req.line_end != 0 and req.line_start != 0 and req.line_end < req.line_start) {
        out.outcome = .malformed;
        out.detail = "line_end precedes line_start";
        return;
    }

    // Resolve: path hint first, then the index; a hint that no longer
    // resolves falls through to the index because moves preserve identity.
    var content: ?[]u8 = null;
    defer if (content) |c| gpa.free(c);
    var used_path: []const u8 = "";
    if (req.path_hint) |hint| {
        if (readContained(gpa, io, root, hint)) |bytes| {
            content = bytes;
            used_path = try arena.dupe(u8, hint);
        } else |err| switch (err) {
            error.OutOfMemory => return error.OutOfMemory,
            error.Unavailable => {},
        }
    }
    if (content == null) {
        if (try idx.lookupEpisode(gpa, req.episode_id)) |row| {
            defer row.deinit(gpa);
            if (readContained(gpa, io, root, row.rel_path)) |bytes| {
                content = bytes;
                used_path = try arena.dupe(u8, row.rel_path);
            } else |err| switch (err) {
                error.OutOfMemory => return error.OutOfMemory,
                error.Unavailable => {},
            }
        }
    }
    const bytes = content orelse {
        out.outcome = .gone;
        out.detail = "no source file for this episode (index may be stale; try sync)";
        return;
    };

    const ep = frontmatter.parse(bytes) orelse {
        out.outcome = .gone;
        out.detail = "source file is no longer a parseable episode";
        return;
    };
    if (!std.mem.eql(u8, ep.episode_id, req.episode_id)) {
        out.outcome = .gone;
        out.detail = "file at the resolved path carries another episode identity";
        return;
    }
    if (req.expected_world) |world| {
        if (!std.mem.eql(u8, ep.world, world)) {
            out.outcome = .gone;
            out.detail = "episode is outside the active world";
            return;
        }
    }
    if (req.expected_scope) |scope| {
        if (!std.mem.eql(u8, ep.scope, scope)) {
            out.outcome = .gone;
            out.detail = "episode is outside the active scope";
            return;
        }
    }
    out.path = used_path;
    out.revision = try std.fmt.allocPrint(arena, "{s}{s}", .{
        identity.digest_prefix, ep.digest_hex,
    });
    out.world = try arena.dupe(u8, ep.world);
    out.scope = try arena.dupe(u8, ep.scope);
    out.lane = ep.lane;
    out.capture_policy = try arena.dupe(u8, ep.capture_policy);

    if (!std.mem.eql(u8, ep.digest_hex, requested_hex)) {
        // Edited evidence is never silently served as the old revision.
        out.outcome = .stale_revision;
        out.detail = "episode was edited; re-search or request the current revision";
        return;
    }

    // Bounded body span.
    const start: u32 = if (req.line_start == 0) ep.body_line else @max(req.line_start, ep.body_line);
    const requested_end: u32 = if (req.line_end == 0)
        start +| (contracts.max_get_lines - 1)
    else
        @min(req.line_end, start +| (contracts.max_get_lines - 1));

    var text: std.ArrayList(u8) = .empty;
    var served_start: u32 = 0;
    var served_end: u32 = 0;
    var any = false;
    var lines = std.mem.splitScalar(u8, bytes, '\n');
    var line_no: u32 = 1;
    while (lines.next()) |line| : (line_no += 1) {
        if (line_no < start) continue;
        if (line_no > requested_end) break;
        if (text.items.len + line.len + 1 > contracts.max_get_bytes) break;
        if (served_start == 0) served_start = line_no;
        if (any) try text.append(arena, '\n');
        any = true;
        try text.appendSlice(arena, line);
        served_end = line_no;
    }
    out.line_start = served_start;
    out.line_end = served_end;
    out.content = text.items;
    out.outcome = .match;
}

fn validEpisodeId(id: []const u8) bool {
    if (id.len != identity.episode_id_len) return false;
    if (!std.mem.startsWith(u8, id, identity.id_prefix)) return false;
    for (id[identity.id_prefix.len..]) |b| switch (b) {
        '0'...'9', 'a'...'f' => {},
        else => return false,
    };
    return true;
}

fn stripDigestPrefix(revision: []const u8) []const u8 {
    if (std.mem.startsWith(u8, revision, identity.digest_prefix)) {
        return revision[identity.digest_prefix.len..];
    }
    return revision;
}
