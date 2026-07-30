//! Standalone AutoJournal binary: framed-stdio helper, owner CLI, and hook
//! target in one executable. This slice ships capture, status, catalog, sync,
//! search, get, alias, and version; the framed protocol follows.

const std = @import("std");
const Io = std.Io;
const aj = @import("autojournal");

const usage_text =
    \\usage: autojournal <command> [options]
    \\
    \\commands:
    \\  capture   read one completed-turn JSON payload on stdin and publish it
    \\  search    ranked, bounded recall: autojournal search <query words...>
    \\  get       open one evidence reference exactly
    \\  alias     thesaurus upkeep: list | add <term> <canonical...> |
    \\            remove <term> [canonical] | candidates
    \\  default   show or set the owner default world/scope (--world/--scope)
    \\  status    report journal root, corpus, and index health
    \\  catalog   list discovered worlds and scopes
    \\  sync      rebuild/repair the index projection from the Markdown corpus
    \\  version   print version and schema identities
    \\
    \\options:
    \\  --config <path>    explicit config file (default: XDG lookup)
    \\  --root <path>      journal root override (bypasses config/default)
    \\  --default-root <p> deprecated host fallback retained for adapters
    \\  --index <path>     index database override (default: XDG state dir)
    \\  --world <id>       world to search (default: config default_world,
    \\                     else the capture world)
    \\  --scope <token>    restrict search to one scope
    \\  --lanes <a,b>      lanes to search (default:
    \\                     conversation,delegated_work,imported_legacy)
    \\  --limit <n>        page size (default from config, cap 100)
    \\  --cursor <c>       continue a previous search page
    \\  --credit-mode <m>  term crediting: substring | word_start | whole_word
    \\                     (default word_start; see docs/SEARCH_TUNING.md)
    \\  --episode <id>     (get) evidence episode id
    \\  --revision <r>     (get) sha256:<hex> revision the evidence had
    \\  --path <rel>       (get) path hint from a search result
    \\  --lines <a-b>      (get) explicit line bounds
    \\  --json             machine-readable output
    \\
;

const Exit = enum(u8) {
    ok = 0,
    failure = 1,
    malformed = 2,
    conflict = 3,
};

const RootSource = enum {
    explicit,
    owner_config,
    host_default,
    autojournal_default,
};

fn outcomeExit(outcome: aj.contracts.Outcome) Exit {
    return switch (outcome) {
        // A typed empty result is a successful answer, not an error.
        .match, .no_match => .ok,
        .malformed => .malformed,
        .conflict => .conflict,
        else => .failure,
    };
}

const Opts = struct {
    config: ?[]const u8 = null,
    root: ?[]const u8 = null,
    default_root: ?[]const u8 = null,
    index: ?[]const u8 = null,
    world: ?[]const u8 = null,
    scope: ?[]const u8 = null,
    lanes: ?[]const u8 = null,
    limit: ?u32 = null,
    cursor: ?[]const u8 = null,
    episode: ?[]const u8 = null,
    revision: ?[]const u8 = null,
    path: ?[]const u8 = null,
    lines: ?[]const u8 = null,
    credit_mode: ?[]const u8 = null,
    json: bool = false,
    positionals: std.ArrayList([]const u8) = .empty,
};

pub fn main(init: std.process.Init) !void {
    const gpa = init.gpa;
    const io = init.io;
    const arena = init.arena.allocator();

    var args = std.process.Args.Iterator.init(init.minimal.args);
    _ = args.skip();
    const command = args.next() orelse return fail(io, .malformed, usage_text);

    var opts: Opts = .{};
    while (args.next()) |arg| {
        if (std.mem.eql(u8, arg, "--json")) {
            opts.json = true;
        } else if (std.mem.startsWith(u8, arg, "--")) {
            const value = args.next() orelse
                return failFmt(io, gpa, .malformed, "{s} requires a value\n", .{arg});
            const slot: ?*?[]const u8 = if (std.mem.eql(u8, arg, "--config"))
                &opts.config
            else if (std.mem.eql(u8, arg, "--root"))
                &opts.root
            else if (std.mem.eql(u8, arg, "--default-root"))
                &opts.default_root
            else if (std.mem.eql(u8, arg, "--index"))
                &opts.index
            else if (std.mem.eql(u8, arg, "--world"))
                &opts.world
            else if (std.mem.eql(u8, arg, "--scope"))
                &opts.scope
            else if (std.mem.eql(u8, arg, "--lanes"))
                &opts.lanes
            else if (std.mem.eql(u8, arg, "--cursor"))
                &opts.cursor
            else if (std.mem.eql(u8, arg, "--episode"))
                &opts.episode
            else if (std.mem.eql(u8, arg, "--revision"))
                &opts.revision
            else if (std.mem.eql(u8, arg, "--path"))
                &opts.path
            else if (std.mem.eql(u8, arg, "--lines"))
                &opts.lines
            else if (std.mem.eql(u8, arg, "--credit-mode"))
                &opts.credit_mode
            else
                null;
            if (slot) |s| {
                s.* = value;
            } else if (std.mem.eql(u8, arg, "--limit")) {
                opts.limit = std.fmt.parseInt(u32, value, 10) catch
                    return fail(io, .malformed, "--limit must be a positive integer\n");
            } else {
                return fail(io, .malformed, usage_text);
            }
        } else {
            try opts.positionals.append(arena, arg);
        }
    }

    if (std.mem.eql(u8, command, "version")) {
        return printOut(io, "autojournal {s} (payload schema v{d}, episode schema {s}, index schema v{d}, {s}, {s}, {s})\n", .{
            aj.package_version,                     aj.contracts.payload_schema_version,
            aj.contracts.episode_schema,            aj.index.index_schema_version,
            aj.retrieval.tokenizer_version,         aj.retrieval.scorer_version,
            aj.retrieval.confidence_policy_version,
        });
    }

    // Configuration is optional because commands can use the host-neutral
    // journal default when neither config nor --root is present.
    const explicit_config = opts.config != null or
        if (init.environ_map.get("AUTOJOURNAL_CONFIG")) |p| p.len > 0 else false;
    const loaded: ?aj.config.Loaded = aj.config.load(gpa, io, init.environ_map, opts.config) catch |err| switch (err) {
        error.NotFound => if (explicit_config)
            return fail(io, .failure, "explicit AutoJournal config was not found\n")
        else
            null,
        error.Malformed => return fail(io, .failure, "config is malformed (see config.json schema)\n"),
        error.OutOfMemory => return error.OutOfMemory,
        error.Unavailable => return fail(io, .failure, "config unavailable\n"),
    };
    // `cfg` slices borrow from `loaded`; freed here after the command
    // returns, which also keeps the Debug allocator's exit leak check clean.
    defer if (loaded) |l| l.deinit();
    const cfg: aj.config.Config = if (loaded) |l| l.value() else .{};

    if (std.mem.eql(u8, command, "alias")) {
        return aliasCommand(gpa, io, arena, init.environ_map, cfg, &opts);
    }
    if (std.mem.eql(u8, command, "default")) {
        return defaultCommand(gpa, io, init.environ_map, cfg, &opts);
    }

    // Root resolution: explicit command override, an owner configuration
    // that names a root, a deprecated host fallback for pre-release
    // adapters, then AutoJournal's host-neutral XDG data default.
    const root_source: RootSource = if (opts.root != null)
        .explicit
    else if (cfg.journal_root != null)
        .owner_config
    else if (opts.default_root != null)
        .host_default
    else
        .autojournal_default;
    const root_path = opts.root orelse cfg.journal_root orelse opts.default_root orelse
        try defaultJournalRoot(arena, init.environ_map);
    const index_path = opts.index orelse
        try defaultIndexPath(arena, init.environ_map, root_path);

    if (std.mem.eql(u8, command, "capture")) return captureCommand(gpa, io, cfg, root_path, index_path);
    if (std.mem.eql(u8, command, "status")) return statusCommand(
        gpa,
        io,
        root_path,
        index_path,
        root_source,
        if (root_source == .owner_config)
            (if (loaded) |l| l.source_path else null)
        else
            null,
        opts.json,
    );
    if (std.mem.eql(u8, command, "catalog")) return catalogCommand(gpa, io, arena, cfg, root_path, index_path);
    if (std.mem.eql(u8, command, "sync")) return syncCommand(gpa, io, root_path, index_path);
    if (std.mem.eql(u8, command, "search")) {
        return searchCommand(gpa, io, arena, init.environ_map, cfg, root_path, index_path, &opts);
    }
    if (std.mem.eql(u8, command, "get")) {
        return getCommand(gpa, io, root_path, index_path, &opts);
    }
    return fail(io, .malformed, usage_text);
}

// --- Paths ---
//
// The derivations themselves live in `aj.paths` so this CLI and an embedding
// host cannot drift apart on where the journal and its index are.

const defaultJournalRoot = aj.paths.defaultJournalRoot;
const rootInSharedDirectory = aj.paths.rootInSharedDirectory;
const rootDigestHex = aj.paths.rootDigestHex;
const defaultIndexPath = aj.paths.defaultIndexPath;
const stateDir = aj.paths.stateDir;

/// Thesaurus resolution: owner config first, the legacy environment
/// override second, the XDG default last. The file is hand-editable and
/// hot-loads on every invocation.
fn thesaurusPath(
    arena: std.mem.Allocator,
    environ: *const std.process.Environ.Map,
    cfg: aj.config.Config,
) ![]const u8 {
    if (cfg.thesaurus_path) |p| return p;
    if (environ.get("AUTOJOURNAL_THESAURUS")) |p| {
        if (p.len > 0) return p;
    }
    if (environ.get("XDG_CONFIG_HOME")) |xdg| if (xdg.len > 0) {
        return std.fmt.allocPrint(arena, "{s}/autojournal/thesaurus.json", .{xdg});
    };
    const home = environ.get("HOME") orelse return error.MissingHome;
    return std.fmt.allocPrint(arena, "{s}/.config/autojournal/thesaurus.json", .{home});
}

fn missLogPath(
    arena: std.mem.Allocator,
    environ: *const std.process.Environ.Map,
) ![]const u8 {
    if (environ.get("AUTOJOURNAL_MISS_LOG")) |p| {
        if (p.len > 0) return p;
    }
    return std.fmt.allocPrint(arena, "{s}/autojournal/thesaurus-candidates.jsonl", .{
        try stateDir(arena, environ),
    });
}

/// Opens the projection, creating its parent directory owner-only on the
/// first run. Never touches the journal root. When `expected_root` is set,
/// an index recording another root's identity is rejected as
/// `error.ForeignIndex` instead of being misread as empty memory; `sync`
/// passes null because it rebuilds and re-stamps the identity itself.
fn openIndex(
    gpa: std.mem.Allocator,
    io: Io,
    index_path: []const u8,
    expected_root: ?[]const u8,
) !aj.index.Index {
    const digest: ?[aj.paths.digest_hex_len]u8 = if (expected_root) |root| rootDigestHex(root) else null;
    const digest_slice: ?[]const u8 = if (digest) |*d| d else null;
    return aj.index.Index.openHardened(gpa, io, index_path, digest_slice);
}

const hardenIndexFiles = aj.index.Index.hardenFiles;

fn nowMs(io: Io) u64 {
    return @intCast(@max(0, Io.Clock.now(.real, io).toMilliseconds()));
}

// --- search ---

fn parseLanes(text: []const u8, buf: *[4]aj.contracts.Lane) ?[]const aj.contracts.Lane {
    var count: usize = 0;
    var it = std.mem.splitScalar(u8, text, ',');
    while (it.next()) |tag| {
        const trimmed = std.mem.trim(u8, tag, " ");
        if (trimmed.len == 0) continue;
        const lane = std.meta.stringToEnum(aj.contracts.Lane, trimmed) orelse return null;
        const dup = for (buf[0..count]) |seen| {
            if (seen == lane) break true;
        } else false;
        if (dup) continue;
        if (count >= buf.len) return null;
        buf[count] = lane;
        count += 1;
    }
    if (count == 0) return null;
    return buf[0..count];
}

fn searchCommand(
    gpa: std.mem.Allocator,
    io: Io,
    arena: std.mem.Allocator,
    environ: *const std.process.Environ.Map,
    cfg: aj.config.Config,
    root_path: []const u8,
    index_path: []const u8,
    opts: *const Opts,
) !void {
    if (opts.positionals.items.len == 0) {
        return fail(io, .malformed, "search needs query words: autojournal search <query...>\n");
    }
    const query = try std.mem.join(arena, " ", opts.positionals.items);
    if (query.len > aj.contracts.max_query_bytes) {
        return fail(io, .malformed, "query exceeds max_query_bytes\n");
    }
    // World fallback mirrors capture: an unconfigured install searches the
    // world capture publishes into ("main" unless the config says otherwise).
    const world = opts.world orelse cfg.default_world orelse cfg.capture.world;

    var lanes_buf: [4]aj.contracts.Lane = undefined;
    const lanes: []const aj.contracts.Lane = if (opts.lanes) |text|
        parseLanes(text, &lanes_buf) orelse
            return fail(io, .malformed, "--lanes takes a comma list of: conversation, delegated_work, evaluation, imported_legacy\n")
    else
        &aj.search.default_lanes;

    var root = Io.Dir.openDirAbsolute(io, root_path, .{ .iterate = true }) catch {
        return fail(io, .failure, "journal root missing or unreadable\n");
    };
    defer root.close(io);
    var idx = openIndex(gpa, io, index_path, root_path) catch |err| switch (err) {
        error.ForeignIndex => return fail(io, .failure, "index at this path belongs to a different journal root; run sync to rebuild it\n"),
        else => return fail(io, .failure, "cannot open index database\n"),
    };
    defer idx.close();

    const thesaurus = try thesaurusPath(arena, environ, cfg);
    const alias_map = try aj.aliases.loadFromFile(gpa, io, thesaurus);
    defer alias_map.deinit();

    const credit_mode: aj.search.CreditMode = if (opts.credit_mode) |text|
        std.meta.stringToEnum(aj.search.CreditMode, text) orelse
            return fail(io, .malformed, "--credit-mode takes: substring, word_start, whole_word\n")
    else
        .word_start;

    const out = try aj.search.search(gpa, io, root, &idx, &alias_map, .{
        .query = query,
        .world = world,
        .scope = opts.scope,
        .lanes = lanes,
        .credit_mode = credit_mode,
        .limit = @min(opts.limit orelse cfg.max_results, aj.contracts.max_results_limit),
        .cursor = opts.cursor,
        .now_ms = nowMs(io),
        .knobs = .{
            .context_window = cfg.context_window,
            .recency_boost = cfg.recency_boost,
            .min_score = cfg.min_score,
            .confidence_floor = cfg.confidence_floor,
        },
    });
    defer out.deinit();

    // Weak-query miss logging: opt-in, bounded, best-effort, and only for
    // real (non-error) recall outcomes.
    if (cfg.miss_log and (out.outcome == .match or out.outcome == .no_match) and
        out.best_score < cfg.confidence_floor)
    {
        var iso: [20]u8 = undefined;
        aj.aliases.appendMiss(gpa, io, try missLogPath(arena, environ), .{
            .ts = aj.render.isoFromMs(nowMs(io), &iso),
            .query = query,
            .terms = out.query_terms,
            .best = out.best_score,
            .top = if (out.hits.len > 0) out.hits[0].episode_id else null,
        }, cfg.miss_log_max_bytes);
    }

    if (opts.json) {
        try renderSearchJson(gpa, io, world, query, &out);
    } else {
        try renderSearchText(gpa, io, query, &out);
    }
    exitOutcome(out.outcome);
}

fn exitOutcome(outcome: aj.contracts.Outcome) void {
    const exit = outcomeExit(outcome);
    if (exit != .ok) std.process.exit(@intFromEnum(exit));
}

const HitJson = struct {
    episode_id: []const u8,
    revision: []const u8,
    path: []const u8,
    world: []const u8,
    scope: []const u8,
    lane: []const u8,
    capture_policy: []const u8,
    event_time: []const u8,
    line: u32,
    snippet_start: u32,
    snippet_end: u32,
    snippet: []const u8,
    matched_terms: []const []const u8,
    score: f64,
    confidence: []const u8,
};

fn renderSearchJson(
    gpa: std.mem.Allocator,
    io: Io,
    world: []const u8,
    query: []const u8,
    out: *const aj.search.SearchOutput,
) !void {
    var arena_owner = std.heap.ArenaAllocator.init(gpa);
    defer arena_owner.deinit();
    const arena = arena_owner.allocator();

    const results = try arena.alloc(HitJson, out.hits.len);
    for (results, out.hits) |*r, hit| {
        var iso: [20]u8 = undefined;
        r.* = .{
            .episode_id = hit.episode_id,
            .revision = hit.revision,
            .path = hit.path,
            .world = world,
            .scope = hit.scope,
            .lane = @tagName(hit.lane),
            .capture_policy = hit.capture_policy,
            .event_time = try arena.dupe(u8, aj.render.isoFromMs(hit.event_time_ms, &iso)),
            .line = hit.line,
            .snippet_start = hit.snippet_start,
            .snippet_end = hit.snippet_end,
            .snippet = hit.snippet,
            .matched_terms = hit.matched_terms,
            .score = hit.score,
            .confidence = @tagName(hit.confidence),
        };
    }
    const report = .{
        .outcome = @tagName(out.outcome),
        .query = query,
        .query_terms = out.query_terms,
        .alias_terms = out.alias_terms,
        .results = results,
        .total = out.total,
        .cursor = out.next_cursor,
        .identities = .{
            .scorer = aj.retrieval.scorer_version,
            .tokenizer = aj.retrieval.tokenizer_version,
            .confidence_policy = aj.retrieval.confidence_policy_version,
            .alias_digest = &out.alias_digest,
            .index_schema = aj.index.index_schema_version,
        },
        .index = .{
            .freshness = @tagName(out.freshness),
            .indexed = out.indexed,
            .source = out.source,
            .edited_excluded = out.edited_excluded,
        },
        .detail = out.detail,
    };
    const text = try std.json.Stringify.valueAlloc(gpa, report, .{});
    defer gpa.free(text);
    try printOut(io, "{s}\n", .{text});
}

fn renderSearchText(
    gpa: std.mem.Allocator,
    io: Io,
    query: []const u8,
    out: *const aj.search.SearchOutput,
) !void {
    var buf: std.ArrayList(u8) = .empty;
    defer buf.deinit(gpa);

    switch (out.outcome) {
        .match => {}, // fall through to results
        .no_match => {
            try buf.print(gpa, "no match for \"{s}\" (index {s}, {d} indexed)\n", .{
                query, @tagName(out.freshness), out.indexed,
            });
            if (out.edited_excluded > 0) {
                try buf.print(gpa, "note: {d} candidate(s) excluded as edited since indexing; run sync\n", .{out.edited_excluded});
            }
            return printOut(io, "{s}", .{buf.items});
        },
        else => {
            try buf.print(gpa, "search failed: {s}", .{@tagName(out.outcome)});
            if (out.detail) |d| try buf.print(gpa, " ({s})", .{d});
            try buf.append(gpa, '\n');
            return printOut(io, "{s}", .{buf.items});
        },
    }

    try buf.print(gpa, "{d} of {d} result(s) for \"{s}\" — index {s}\n", .{
        out.hits.len, out.total, query, @tagName(out.freshness),
    });
    if (out.alias_terms.len > 0) {
        try buf.appendSlice(gpa, "aliases applied:");
        for (out.alias_terms) |t| try buf.print(gpa, " {s}", .{t});
        try buf.append(gpa, '\n');
    }
    for (out.hits, 1..) |hit, rank| {
        var iso: [20]u8 = undefined;
        try buf.print(gpa, "{d:>2}. [{d:.2} {s}] {s}:{d} ({s})\n", .{
            rank,     hit.score,                                           @tagName(hit.confidence), hit.path,
            hit.line, aj.render.isoFromMs(hit.event_time_ms, &iso)[0..10],
        });
        try buf.print(gpa, "    {s}\n", .{matchLine(hit)});
        try buf.print(gpa, "    id {s} rev {s}\n", .{ hit.episode_id, hit.revision });
    }
    if (out.next_cursor) |cursor| {
        try buf.print(gpa, "more: add --cursor {s}\n", .{cursor});
    }
    if (out.detail) |d| try buf.print(gpa, "note: {s}\n", .{d});
    try printOut(io, "{s}", .{buf.items});
}

/// The matched line extracted from a hit's snippet (the snippet spans
/// context lines; the hit's own line is the evidence).
fn matchLine(hit: aj.search.Hit) []const u8 {
    if (hit.snippet.len == 0) return "(source changed since indexing)";
    var lines = std.mem.splitScalar(u8, hit.snippet, '\n');
    var line_no = hit.snippet_start;
    while (lines.next()) |line| : (line_no += 1) {
        if (line_no == hit.line) return line;
    }
    return hit.snippet;
}

// --- get ---

const LineSpan = struct { start: u32, end: u32 };

fn parseLineSpan(text: []const u8) ?LineSpan {
    if (std.mem.indexOfScalar(u8, text, '-')) |dash| {
        const start = std.fmt.parseInt(u32, text[0..dash], 10) catch return null;
        const end = std.fmt.parseInt(u32, text[dash + 1 ..], 10) catch return null;
        if (start == 0 or end < start) return null;
        return .{ .start = start, .end = end };
    }
    const line = std.fmt.parseInt(u32, text, 10) catch return null;
    if (line == 0) return null;
    return .{ .start = line, .end = line };
}

fn getCommand(
    gpa: std.mem.Allocator,
    io: Io,
    root_path: []const u8,
    index_path: []const u8,
    opts: *const Opts,
) !void {
    const episode_id = opts.episode orelse
        return fail(io, .malformed, "get needs --episode <id> and --revision <sha256:hex>\n");
    const revision = opts.revision orelse
        return fail(io, .malformed, "get needs --revision <sha256:hex> (from a search result)\n");
    var span: LineSpan = .{ .start = 0, .end = 0 };
    if (opts.lines) |text| {
        span = parseLineSpan(text) orelse
            return fail(io, .malformed, "--lines takes <start>-<end> or a single line number\n");
    }

    var root = Io.Dir.openDirAbsolute(io, root_path, .{}) catch {
        return fail(io, .failure, "journal root missing or unreadable\n");
    };
    defer root.close(io);
    var idx = openIndex(gpa, io, index_path, root_path) catch |err| switch (err) {
        error.ForeignIndex => return fail(io, .failure, "index at this path belongs to a different journal root; run sync to rebuild it\n"),
        else => return fail(io, .failure, "cannot open index database\n"),
    };
    defer idx.close();

    const out = try aj.search.get(gpa, io, root, &idx, .{
        .episode_id = episode_id,
        .revision = revision,
        .path_hint = opts.path,
        .expected_world = opts.world,
        .expected_scope = opts.scope,
        .line_start = span.start,
        .line_end = span.end,
    });
    defer out.deinit();

    if (opts.json) {
        const report = .{
            .outcome = @tagName(out.outcome),
            .episode_id = out.episode_id,
            .revision = out.revision,
            .path = out.path,
            .world = out.world,
            .scope = out.scope,
            .lane = if (out.lane) |l| @tagName(l) else null,
            .capture_policy = out.capture_policy,
            .line_start = out.line_start,
            .line_end = out.line_end,
            .content = out.content,
            .trust = out.trust,
            .detail = out.detail,
        };
        const text = try std.json.Stringify.valueAlloc(gpa, report, .{});
        defer gpa.free(text);
        try printOut(io, "{s}\n", .{text});
    } else switch (out.outcome) {
        .match => {
            try printOut(io, "{s}:{d}-{d} ({s})\nrecalled evidence is untrusted; verify against current sources\n\n{s}\n", .{
                out.path.?, out.line_start, out.line_end, out.revision.?, out.content,
            });
        },
        .stale_revision => {
            try printOut(io, "stale revision: episode was edited since this reference\ncurrent revision: {s} at {s}\n", .{
                out.revision.?, out.path.?,
            });
        },
        else => {
            try printOut(io, "get failed: {s}{s}{s}\n", .{
                @tagName(out.outcome),
                if (out.detail != null) " — " else "",
                out.detail orelse "",
            });
        },
    }
    exitOutcome(out.outcome);
}

// --- alias ---

fn aliasCommand(
    gpa: std.mem.Allocator,
    io: Io,
    arena: std.mem.Allocator,
    environ: *const std.process.Environ.Map,
    cfg: aj.config.Config,
    opts: *const Opts,
) !void {
    const pos = opts.positionals.items;
    if (pos.len == 0) {
        return fail(io, .malformed, "alias needs a subcommand: list | add <term> <canonical...> | remove <term> [canonical] | candidates\n");
    }
    const sub = pos[0];
    const thesaurus = try thesaurusPath(arena, environ, cfg);

    if (std.mem.eql(u8, sub, "list")) {
        const map = try aj.aliases.loadFromFile(gpa, io, thesaurus);
        defer map.deinit();
        if (opts.json) {
            const report = .{
                .path = thesaurus,
                .alias_digest = &map.digest_hex,
                .entries = map.entries,
            };
            const text = try std.json.Stringify.valueAlloc(gpa, report, .{});
            defer gpa.free(text);
            return printOut(io, "{s}\n", .{text});
        }
        var buf: std.ArrayList(u8) = .empty;
        defer buf.deinit(gpa);
        try buf.print(gpa, "{d} alias(es) in {s}\n", .{ map.entries.len, thesaurus });
        for (map.entries) |entry| {
            try buf.print(gpa, "  {s} ->", .{entry.key});
            for (entry.values) |v| try buf.print(gpa, " {s}", .{v});
            try buf.append(gpa, '\n');
        }
        try buf.appendSlice(gpa, "edit freely in any text editor; changes apply on the next search\n");
        return printOut(io, "{s}", .{buf.items});
    }

    if (std.mem.eql(u8, sub, "add")) {
        if (pos.len < 3) {
            return fail(io, .malformed, "alias add <term> <canonical...>\n");
        }
        aj.aliases.addAlias(gpa, io, thesaurus, pos[1], pos[2..]) catch |err| switch (err) {
            error.InvalidTerm => return fail(io, .malformed, "term must be a searchable word: longer than 2 letters, [a-z0-9_], not a stop word\n"),
            error.InvalidValue => return fail(io, .malformed, "canonical values must be 2..128 chars of [A-Za-z0-9._:+/@-]\n"),
            error.Malformed => return fail(io, .failure, "thesaurus file is not a JSON object; fix it by hand first\n"),
            error.NotFound => unreachable,
            error.OutOfMemory => return error.OutOfMemory,
            error.Unavailable => return fail(io, .failure, "cannot write thesaurus file\n"),
        };
        return printOut(io, "added: {s} -> {s} ({s})\n", .{
            pos[1], try std.mem.join(arena, " ", pos[2..]), thesaurus,
        });
    }

    if (std.mem.eql(u8, sub, "remove")) {
        if (pos.len < 2 or pos.len > 3) {
            return fail(io, .malformed, "alias remove <term> [canonical]\n");
        }
        const removed = aj.aliases.removeAlias(gpa, io, thesaurus, pos[1], if (pos.len == 3) pos[2] else null) catch |err| switch (err) {
            error.NotFound => return fail(io, .failure, "no such alias entry\n"),
            error.Malformed => return fail(io, .failure, "thesaurus file is not a JSON object; fix it by hand first\n"),
            error.InvalidTerm, error.InvalidValue => unreachable,
            error.OutOfMemory => return error.OutOfMemory,
            error.Unavailable => return fail(io, .failure, "cannot write thesaurus file\n"),
        };
        return printOut(io, "removed {s}: {s}\n", .{
            switch (removed) {
                .entry => "entry",
                .value => "value",
            },
            pos[1],
        });
    }

    if (std.mem.eql(u8, sub, "candidates")) {
        const log_path = try missLogPath(arena, environ);
        const bytes = Io.Dir.cwd().readFileAlloc(io, log_path, gpa, .limited(16 * 1024 * 1024)) catch |err| switch (err) {
            error.OutOfMemory => return error.OutOfMemory,
            else => return printOut(io, "no candidates yet ({s}); enable with \"miss_log\": true in config.json\n", .{log_path}),
        };
        defer gpa.free(bytes);
        const agg = try aj.aliases.aggregateMisses(gpa, bytes);
        defer agg.deinit();
        const limit: usize = @min(agg.items.len, opts.limit orelse 20);
        var buf: std.ArrayList(u8) = .empty;
        defer buf.deinit(gpa);
        try buf.print(gpa, "{d} distinct weak quer{s}, most frequent first:\n", .{
            agg.items.len, if (agg.items.len == 1) @as([]const u8, "y") else "ies",
        });
        for (agg.items[0..limit]) |cand| {
            try buf.print(gpa, "  [{d}x] {s}\n        terms:", .{ cand.count, cand.query });
            for (cand.terms) |t| try buf.print(gpa, " {s}", .{t});
            try buf.append(gpa, '\n');
        }
        try buf.appendSlice(gpa, "for each: if the journal really covers it, promote with\n  autojournal alias add <casual term> <canonical term>\n");
        return printOut(io, "{s}", .{buf.items});
    }

    return fail(io, .malformed, "unknown alias subcommand; use list | add | remove | candidates\n");
}

// --- capture / status / sync (write slice) ---

const CaptureReport = struct {
    outcome: []const u8,
    episode_id: ?[]const u8 = null,
    payload_digest: ?[]const u8 = null,
    path: ?[]const u8 = null,
    index: []const u8 = @tagName(aj.contracts.IndexFreshness.not_built),
    detail: ?[]const u8 = null,
};

fn captureCommand(gpa: std.mem.Allocator, io: Io, cfg: aj.config.Config, root_path: []const u8, index_path: []const u8) !void {
    var stdin_buf: [4096]u8 = undefined;
    var stdin_reader = Io.File.stdin().reader(io, &stdin_buf);
    const payload_bytes = stdin_reader.interface.allocRemaining(
        gpa,
        .limited(aj.contracts.max_payload_bytes + 1),
    ) catch |err| switch (err) {
        error.StreamTooLong => return reportCapture(gpa, io, .malformed, .{
            .outcome = "malformed",
            .detail = "payload exceeds max_payload_bytes",
        }),
        else => return fail(io, .failure, "failed reading stdin\n"),
    };
    defer gpa.free(payload_bytes);

    const parsed = aj.contracts.parsePayload(gpa, payload_bytes) catch |err| switch (err) {
        error.OutOfMemory => return error.OutOfMemory,
        else => return reportCapture(gpa, io, .malformed, .{
            .outcome = "malformed",
            .detail = @errorName(err),
        }),
    };
    defer parsed.deinit();
    // An omitted world/scope falls back to owner capture defaults. A host may
    // provide explicit values only when transporting an owner session choice.
    var raw = parsed.value;
    if (raw.world == null) raw.world = cfg.capture.world;
    if (raw.scope == null) raw.scope = cfg.capture.scope;
    const payload = aj.contracts.validate(raw) catch |err| {
        return reportCapture(gpa, io, .malformed, .{
            .outcome = "malformed",
            .detail = @errorName(err),
        });
    };

    if (rootInSharedDirectory(io, root_path)) {
        return reportCapture(gpa, io, .failure, .{
            .outcome = "permission_denied",
            .detail = "journal root sits under a shared (group- or world-writable) directory; chmod g-w,o-w the parent or configure journal_root to a private location",
        });
    }
    var root = openOrCreateRoot(io, root_path) catch {
        return reportCapture(gpa, io, .failure, .{
            .outcome = "unavailable",
            .detail = "cannot open journal root",
        });
    };
    defer root.close(io);

    // Identity is corpus-wide, but the store's duplicate detection is
    // path-local; consult the index first so a redelivery whose event time
    // shards to another date is still recognized. Best-effort: an absent or
    // unhelpful index falls through to publication as before.
    if (openIndex(gpa, io, index_path, root_path)) |idx_const| {
        var idx = idx_const;
        defer idx.close();
        if (try aj.ops.checkRedelivery(gpa, io, root, &idx, payload)) |existing| {
            defer existing.deinit(gpa);
            const episode_id = aj.identity.episodeId(payload);
            const digest_hex = aj.identity.payloadDigestHex(payload);
            var pre_digest_buf: [aj.identity.digest_prefix.len + aj.identity.digest_hex_len]u8 = undefined;
            return reportCapture(
                gpa,
                io,
                if (existing.outcome == .duplicate) .ok else .conflict,
                .{
                    .outcome = @tagName(existing.outcome),
                    .episode_id = &episode_id,
                    .payload_digest = std.fmt.bufPrint(&pre_digest_buf, "{s}{s}", .{
                        aj.identity.digest_prefix, &digest_hex,
                    }) catch unreachable,
                    .path = existing.rel_path,
                    .index = if (existing.outcome == .duplicate) "fresh" else "stale",
                },
            );
        }
    } else |_| {}

    const capture_time_ms = nowMs(io);
    const published = aj.store.publish(root, io, gpa, payload, capture_time_ms) catch |err| {
        const outcome: aj.contracts.CaptureOutcome = switch (err) {
            error.ContainmentViolation => .internal_error,
            error.PermissionDenied => .permission_denied,
            error.Unavailable => .unavailable,
            error.OutOfMemory => return error.OutOfMemory,
        };
        return reportCapture(gpa, io, .failure, .{
            .outcome = @tagName(outcome),
            .detail = @errorName(err),
        });
    };
    defer published.deinit(gpa);

    // Source publication is already durable; the index is best-effort here
    // and repairable via `sync`, so its failure downgrades freshness only.
    const index_state: aj.contracts.IndexFreshness = switch (published.outcome) {
        .conflict => .stale,
        .published, .duplicate => blk: {
            var idx = openIndex(gpa, io, index_path, root_path) catch |err| switch (err) {
                error.ForeignIndex => break :blk .unavailable,
                else => break :blk .stale,
            };
            defer idx.close();
            idx.indexEpisode(published.rel_path, published.content) catch break :blk .stale;
            hardenIndexFiles(gpa, io, index_path) catch break :blk .stale;
            break :blk .fresh;
        },
    };

    const exit: Exit = switch (published.outcome) {
        .published, .duplicate => .ok,
        .conflict => .conflict,
    };
    var digest_buf: [aj.identity.digest_prefix.len + aj.identity.digest_hex_len]u8 = undefined;
    return reportCapture(gpa, io, exit, .{
        .outcome = @tagName(published.outcome),
        .episode_id = &published.episode_id,
        .payload_digest = std.fmt.bufPrint(&digest_buf, "{s}{s}", .{
            aj.identity.digest_prefix, &published.digest_hex,
        }) catch unreachable,
        .path = published.rel_path,
        .index = @tagName(index_state),
    });
}

const StatusReport = struct {
    journal_root: []const u8,
    root_source: []const u8,
    root_source_path: ?[]const u8 = null,
    root_ok: bool,
    episodes: u64,
    index: struct {
        freshness: []const u8,
        indexed: u64,
        path: []const u8,
    },
};

fn statusCommand(
    gpa: std.mem.Allocator,
    io: Io,
    root_path: []const u8,
    index_path: []const u8,
    root_source: RootSource,
    root_source_path: ?[]const u8,
    json: bool,
) !void {
    const report = aj.ops.status(gpa, io, root_path, index_path);
    if (json) {
        const wire: StatusReport = .{
            .journal_root = root_path,
            .root_source = @tagName(root_source),
            .root_source_path = root_source_path,
            .root_ok = report.root_ok,
            .episodes = report.episodes,
            .index = .{
                .freshness = @tagName(report.freshness),
                .indexed = report.indexed,
                .path = index_path,
            },
        };
        const text = try std.json.Stringify.valueAlloc(gpa, wire, .{});
        defer gpa.free(text);
        try printOut(io, "{s}\n", .{text});
    } else if (!report.root_ok) {
        try printOut(io, "journal_root: {s} (missing)\nepisodes: 0\nindex: not_built\n", .{root_path});
    } else {
        try printOut(io,
            \\journal_root: {s} (ok)
            \\episodes: {d}
            \\index: {s} ({d} indexed, {s})
            \\
        , .{ root_path, report.episodes, @tagName(report.freshness), report.indexed, index_path });
    }
    if (!report.root_ok or report.freshness == .stale or report.freshness == .unavailable) {
        std.process.exit(@intFromEnum(Exit.failure));
    }
}

const CatalogPair = struct {
    world: []const u8,
    scope: []const u8,
};

fn catalogCommand(
    gpa: std.mem.Allocator,
    io: Io,
    arena: std.mem.Allocator,
    cfg: aj.config.Config,
    root_path: []const u8,
    index_path: []const u8,
) !void {
    var pairs: std.ArrayList(CatalogPair) = .empty;
    defer pairs.deinit(gpa);
    try pairs.append(gpa, .{ .world = cfg.capture.world, .scope = cfg.capture.scope });

    if (Io.Dir.cwd().statFile(io, index_path, .{})) |_| {
        if (openIndex(gpa, io, index_path, root_path)) |idx_const| {
            var idx = idx_const;
            defer idx.close();
            var st = idx.handle.prepare(
                "SELECT DISTINCT world, scope FROM episodes ORDER BY world, scope;",
            ) catch null;
            if (st) |*statement| {
                defer statement.finalize();
                while (statement.step() catch false) {
                    const world = try arena.dupe(u8, statement.columnText(0));
                    const scope = try arena.dupe(u8, statement.columnText(1));
                    var exists = false;
                    for (pairs.items) |pair| {
                        if (std.mem.eql(u8, pair.world, world) and
                            std.mem.eql(u8, pair.scope, scope))
                        {
                            exists = true;
                            break;
                        }
                    }
                    if (!exists) try pairs.append(gpa, .{ .world = world, .scope = scope });
                }
            }
        } else |_| {}
    } else |_| {}

    const text = try std.json.Stringify.valueAlloc(gpa, .{ .pairs = pairs.items }, .{});
    defer gpa.free(text);
    try printOut(io, "{s}\n", .{text});
}

/// Shows or persists the owner's default world/scope. With neither --world
/// nor --scope this prints the effective capture defaults; with either it
/// rewrites the owner config atomically, so future sessions in every
/// conforming harness start there.
fn defaultCommand(
    gpa: std.mem.Allocator,
    io: Io,
    environ: *const std.process.Environ.Map,
    cfg: aj.config.Config,
    opts: *const Opts,
) !void {
    const world = opts.world orelse cfg.capture.world;
    const scope = opts.scope orelse cfg.capture.scope;
    if (opts.world != null or opts.scope != null) {
        const written = aj.config.saveCaptureDefaults(
            gpa,
            io,
            environ,
            opts.config,
            world,
            scope,
        ) catch |err| switch (err) {
            error.Malformed => return fail(io, .malformed, "cannot save defaults: invalid world/scope, or the existing config is malformed\n"),
            error.NotFound => return fail(io, .failure, "cannot resolve a config path (no HOME)\n"),
            error.OutOfMemory => return error.OutOfMemory,
            error.Unavailable => return fail(io, .failure, "cannot write the owner config\n"),
        };
        defer gpa.free(written);
        if (opts.json) {
            const text = try std.json.Stringify.valueAlloc(gpa, .{
                .world = world,
                .scope = scope,
                .config = written,
            }, .{});
            defer gpa.free(text);
            try printOut(io, "{s}\n", .{text});
        } else {
            try printOut(io, "default set: {s} / {s}\nconfig: {s}\n", .{ world, scope, written });
        }
        return;
    }
    if (opts.json) {
        const text = try std.json.Stringify.valueAlloc(gpa, .{
            .world = world,
            .scope = scope,
        }, .{});
        defer gpa.free(text);
        try printOut(io, "{s}\n", .{text});
    } else {
        try printOut(io, "default world: {s}\ndefault scope: {s}\n", .{ world, scope });
    }
}

fn syncCommand(gpa: std.mem.Allocator, io: Io, root_path: []const u8, index_path: []const u8) !void {
    const report = aj.ops.sync(gpa, io, root_path, index_path) catch |err| switch (err) {
        error.SharedDirectory => return fail(io, .failure, "journal root sits under a shared (group- or world-writable) directory; chmod g-w,o-w the parent or configure journal_root to a private location\n"),
        error.RootMissing => return fail(io, .failure, "journal root missing; nothing to sync\n"),
        error.IndexUnavailable => return fail(io, .failure, "cannot open index database\n"),
        error.SyncFailed => return fail(io, .failure, "sync failed; projection rolled back\n"),
        error.OutOfMemory => return error.OutOfMemory,
    };
    try printOut(io, "indexed: {d}\nremoved: {d}\nskipped_malformed: {d}\nduplicate_ids: {d}\n", .{
        report.indexed, report.removed, report.skipped_malformed, report.duplicate_ids,
    });
}

fn openOrCreateRoot(io: Io, root_path: []const u8) !Io.Dir {
    var probe: ?Io.Dir = Io.Dir.openDirAbsolute(io, root_path, .{}) catch |err| switch (err) {
        error.FileNotFound => {
            try Io.Dir.cwd().createDirPath(io, root_path);
            return openOrCreateRoot(io, root_path);
        },
        else => return err,
    };
    if (probe) |*opened| opened.close(io);
    var hardened = try Io.Dir.openDirAbsolute(io, root_path, .{ .iterate = true });
    defer hardened.close(io);
    try hardened.setPermissions(io, @enumFromInt(0o700));
    return Io.Dir.openDirAbsolute(io, root_path, .{});
}

fn reportCapture(gpa: std.mem.Allocator, io: Io, exit: Exit, report: CaptureReport) !void {
    const text = try std.json.Stringify.valueAlloc(gpa, report, .{});
    defer gpa.free(text);
    try printOut(io, "{s}\n", .{text});
    if (exit != .ok) std.process.exit(@intFromEnum(exit));
}

fn printOut(io: Io, comptime fmt: []const u8, args: anytype) !void {
    var buf: [4096]u8 = undefined;
    var writer = Io.File.stdout().writer(io, &buf);
    try writer.interface.print(fmt, args);
    try writer.interface.flush();
}

fn fail(io: Io, exit: Exit, message: []const u8) !void {
    var buf: [1024]u8 = undefined;
    var writer = Io.File.stderr().writer(io, &buf);
    writer.interface.writeAll(message) catch {};
    writer.interface.flush() catch {};
    std.process.exit(@intFromEnum(exit));
}

fn failFmt(io: Io, gpa: std.mem.Allocator, exit: Exit, comptime fmt: []const u8, args: anytype) !void {
    const message = try std.fmt.allocPrint(gpa, fmt, args);
    defer gpa.free(message);
    return fail(io, exit, message);
}

// --- Tests (pure parsing helpers) ---

test "lane list parsing" {
    var buf: [4]aj.contracts.Lane = undefined;
    const lanes = parseLanes("conversation, evaluation", &buf).?;
    try std.testing.expectEqual(@as(usize, 2), lanes.len);
    try std.testing.expectEqual(aj.contracts.Lane.evaluation, lanes[1]);
    const deduped = parseLanes("conversation,conversation", &buf).?;
    try std.testing.expectEqual(@as(usize, 1), deduped.len);
    try std.testing.expectEqual(@as(?[]const aj.contracts.Lane, null), parseLanes("gossip", &buf));
    try std.testing.expectEqual(@as(?[]const aj.contracts.Lane, null), parseLanes("", &buf));
}

test "line span parsing" {
    const span = parseLineSpan("19-40").?;
    try std.testing.expectEqual(@as(u32, 19), span.start);
    try std.testing.expectEqual(@as(u32, 40), span.end);
    const single = parseLineSpan("21").?;
    try std.testing.expectEqual(@as(u32, 21), single.start);
    try std.testing.expectEqual(@as(u32, 21), single.end);
    try std.testing.expectEqual(@as(?@TypeOf(span), null), parseLineSpan("40-19"));
    try std.testing.expectEqual(@as(?@TypeOf(span), null), parseLineSpan("0"));
    try std.testing.expectEqual(@as(?@TypeOf(span), null), parseLineSpan("abc"));
}
