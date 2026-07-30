//! Owner configuration. AutoJournal owns its configuration; harness-side
//! wiring passes no policy. Resolution order: explicit `--config` path,
//! `$AUTOJOURNAL_CONFIG`, `$XDG_CONFIG_HOME/autojournal/config.json`,
//! `$HOME/.config/autojournal/config.json`.

const std = @import("std");
const Io = std.Io;
const contracts = @import("contracts.zig");

pub const max_config_bytes: usize = 64 * 1024;

pub const Config = struct {
    /// Absolute path to the owner-controlled Markdown journal. Absent when
    /// the config names none: the host-neutral XDG data default applies, so
    /// a config may hold only capture defaults or retrieval knobs.
    journal_root: ?[]const u8 = null,
    /// World searched when the caller names none. Capture always names its
    /// world explicitly; this is a recall-side owner convenience only.
    default_world: ?[]const u8 = null,
    /// Absolute path to the owner-edited alias map. Default is the XDG
    /// config-dir `thesaurus.json` next to this file.
    thesaurus_path: ?[]const u8 = null,
    /// Snippet context lines on each side of a matched line.
    context_window: u32 = 3,
    /// Default `memory_search` result page size.
    max_results: u32 = 10,
    /// Recency nudge weight in `1 + boost/(days+1)`.
    recency_boost: f64 = 1.0,
    /// Relevance floor; 0 disables it (legacy parity).
    min_score: f64 = 0.0,
    /// Score at which a result counts as confident; also the weak-query bar
    /// for miss logging.
    confidence_floor: f64 = 3.0,
    /// Weak-query logging is opt-in and owner-private.
    miss_log: bool = false,
    /// Appends stop once the miss log reaches this size.
    miss_log_max_bytes: u64 = 1024 * 1024,
    /// Completed-turn defaults. A host adapter may override world/scope only
    /// when transporting an explicit per-session owner selection.
    capture: Capture = .{},
};

pub const Capture = struct {
    /// World that completed-turn capture publishes into.
    world: []const u8 = "main",
    /// Scope token recorded on captured episodes.
    scope: []const u8 = "default",
};

pub const LoadError = error{
    NotFound,
    Malformed,
    OutOfMemory,
    Unavailable,
};

const RawConfig = struct {
    journal_root: ?[]const u8 = null,
    // Compatibility for pre-release owner configurations. New docs and
    // generated configuration use `journal_root`.
    world_root: ?[]const u8 = null,
    default_world: ?[]const u8 = null,
    thesaurus_path: ?[]const u8 = null,
    context_window: u32 = 3,
    max_results: u32 = 10,
    recency_boost: f64 = 1.0,
    min_score: f64 = 0.0,
    confidence_floor: f64 = 3.0,
    miss_log: bool = false,
    miss_log_max_bytes: u64 = 1024 * 1024,
    capture: Capture = .{},
};

pub const ParsedConfig = struct {
    parsed: std.json.Parsed(RawConfig),
    config: Config,

    pub fn deinit(self: *const ParsedConfig) void {
        self.parsed.deinit();
    }
};

pub const Loaded = struct {
    parsed: ParsedConfig,
    source_path: []u8,
    gpa: std.mem.Allocator,

    pub fn value(self: *const Loaded) Config {
        return self.parsed.config;
    }

    pub fn deinit(self: *const Loaded) void {
        self.gpa.free(self.source_path);
        self.parsed.deinit();
    }
};

/// Resolves the owner config path without reading it: explicit `--config`,
/// `$AUTOJOURNAL_CONFIG`, XDG config dir, `$HOME/.config`. Caller owns the
/// returned path.
pub fn resolvePath(
    gpa: std.mem.Allocator,
    environ: *const std.process.Environ.Map,
    explicit_path: ?[]const u8,
) LoadError![]u8 {
    if (explicit_path) |p| return gpa.dupe(u8, p);
    if (environ.get("AUTOJOURNAL_CONFIG")) |p| if (p.len > 0) {
        return gpa.dupe(u8, p);
    };
    // Empty or relative XDG values are invalid per the spec and ignored.
    if (environ.get("XDG_CONFIG_HOME")) |xdg| if (xdg.len > 0 and std.fs.path.isAbsolute(xdg)) {
        return std.fmt.allocPrint(gpa, "{s}/autojournal/config.json", .{xdg});
    };
    const home = environ.get("HOME") orelse return error.NotFound;
    return std.fmt.allocPrint(gpa, "{s}/.config/autojournal/config.json", .{home});
}

pub fn load(
    gpa: std.mem.Allocator,
    io: Io,
    environ: *const std.process.Environ.Map,
    explicit_path: ?[]const u8,
) LoadError!Loaded {
    const path = try resolvePath(gpa, environ, explicit_path);
    errdefer gpa.free(path);

    const bytes = Io.Dir.cwd().readFileAlloc(io, path, gpa, .limited(max_config_bytes)) catch |err|
        switch (err) {
            error.FileNotFound => return error.NotFound,
            error.OutOfMemory => return error.OutOfMemory,
            else => return error.Unavailable,
        };
    defer gpa.free(bytes);

    const parsed = try parseConfig(gpa, bytes);
    return .{ .parsed = parsed, .source_path = path, .gpa = gpa };
}

/// Closed schema: unknown keys are malformed, missing optional keys take
/// their defaults. Paths must be absolute so behavior never depends on the
/// invoking process's working directory.
pub fn parseConfig(
    gpa: std.mem.Allocator,
    bytes: []const u8,
) LoadError!ParsedConfig {
    const parsed = std.json.parseFromSlice(RawConfig, gpa, bytes, .{
        .allocate = .alloc_always,
    }) catch |err| switch (err) {
        error.OutOfMemory => return error.OutOfMemory,
        else => return error.Malformed,
    };
    errdefer parsed.deinit();
    const raw = parsed.value;
    if (raw.journal_root != null and raw.world_root != null and
        !std.mem.eql(u8, raw.journal_root.?, raw.world_root.?)) return error.Malformed;
    const cfg: Config = .{
        .journal_root = raw.journal_root orelse raw.world_root,
        .default_world = raw.default_world,
        .thesaurus_path = raw.thesaurus_path,
        .context_window = raw.context_window,
        .max_results = raw.max_results,
        .recency_boost = raw.recency_boost,
        .min_score = raw.min_score,
        .confidence_floor = raw.confidence_floor,
        .miss_log = raw.miss_log,
        .miss_log_max_bytes = raw.miss_log_max_bytes,
        .capture = raw.capture,
    };
    if (cfg.journal_root) |r| if (!std.fs.path.isAbsolute(r)) return error.Malformed;
    if (cfg.thesaurus_path) |p| if (!std.fs.path.isAbsolute(p)) return error.Malformed;
    if (cfg.default_world) |w| {
        if (!contracts.validWorld(w)) return error.Malformed;
    }
    // Snippets stay bounded: 10 context lines each side already triples the
    // default and approaches the whole-snippet byte cap.
    if (cfg.context_window == 0 or cfg.context_window > 10) return error.Malformed;
    if (cfg.max_results == 0) return error.Malformed;
    if (!(cfg.recency_boost >= 0) or !(cfg.min_score >= 0) or !(cfg.confidence_floor >= 0))
        return error.Malformed;
    if (!contracts.validWorld(cfg.capture.world)) return error.Malformed;
    if (!contracts.validScope(cfg.capture.scope)) return error.Malformed;
    return .{ .parsed = parsed, .config = cfg };
}

pub const SaveError = error{
    NotFound,
    Malformed,
    OutOfMemory,
    Unavailable,
};

/// Persists new capture defaults into the owner config with an atomic
/// rewrite that preserves every other key the owner wrote (the pre-release
/// `world_root` key is migrated to `journal_root` on the way). A missing
/// config file is created holding only the capture defaults — the journal
/// root may stay implicit now that the host-neutral default exists. A file
/// that fails the closed-schema validation is left untouched. Returns the
/// path written, owned by `gpa`.
pub fn saveCaptureDefaults(
    gpa: std.mem.Allocator,
    io: Io,
    environ: *const std.process.Environ.Map,
    explicit_path: ?[]const u8,
    world: []const u8,
    scope: []const u8,
) SaveError![]u8 {
    if (!contracts.validWorld(world) or !contracts.validScope(scope))
        return error.Malformed;

    var arena_owner = std.heap.ArenaAllocator.init(gpa);
    defer arena_owner.deinit();
    const arena = arena_owner.allocator();

    const path = try resolvePath(gpa, environ, explicit_path);
    errdefer gpa.free(path);

    const bytes: []const u8 = Io.Dir.cwd().readFileAlloc(io, path, arena, .limited(max_config_bytes)) catch |err|
        switch (err) {
            error.FileNotFound => "{}",
            error.OutOfMemory => return error.OutOfMemory,
            else => return error.Unavailable,
        };
    var root = std.json.parseFromSliceLeaky(std.json.Value, arena, bytes, .{}) catch |err|
        switch (err) {
            error.OutOfMemory => return error.OutOfMemory,
            else => return error.Malformed,
        };
    if (root != .object) return error.Malformed;

    if (root.object.get("world_root")) |legacy| {
        if (root.object.get("journal_root") == null) {
            try root.object.put(arena, "journal_root", legacy);
        }
        _ = root.object.orderedRemove("world_root");
    }
    const previous_world: []const u8 = blk: {
        const cap = root.object.get("capture") orelse break :blk "main";
        if (cap != .object) break :blk "main";
        const w = cap.object.get("world") orelse break :blk "main";
        break :blk if (w == .string) w.string else "main";
    };
    var capture: std.json.ObjectMap = .empty;
    try capture.put(arena, "world", .{ .string = world });
    try capture.put(arena, "scope", .{ .string = scope });
    try root.object.put(arena, "capture", .{ .object = capture });
    // `default_world` is the recall-side override; it follows only an actual
    // world change, so a scope-only update touches nothing else.
    if (root.object.get("default_world") != null and
        !std.mem.eql(u8, world, previous_world))
    {
        try root.object.put(arena, "default_world", .{ .string = world });
    }

    const text = std.json.Stringify.valueAlloc(arena, root, .{
        .whitespace = .indent_2,
    }) catch return error.OutOfMemory;
    // Never publish a config this module would refuse to load.
    const check = parseConfig(gpa, text) catch |err| switch (err) {
        error.OutOfMemory => return error.OutOfMemory,
        else => return error.Malformed,
    };
    check.deinit();

    try writeAtomic(arena, io, path, text);
    return path;
}

fn writeAtomic(
    arena: std.mem.Allocator,
    io: Io,
    path: []const u8,
    text: []const u8,
) SaveError!void {
    const with_newline = std.fmt.allocPrint(arena, "{s}\n", .{text}) catch
        return error.OutOfMemory;
    const dir_path = std.fs.path.dirname(path) orelse return error.Unavailable;
    const base = std.fs.path.basename(path);
    Io.Dir.cwd().createDirPath(io, dir_path) catch return error.Unavailable;
    var dir = Io.Dir.openDirAbsolute(io, dir_path, .{}) catch return error.Unavailable;
    defer dir.close(io);

    const tmp_name = std.fmt.allocPrint(arena, ".{s}.tmp", .{base}) catch
        return error.OutOfMemory;
    var file = dir.createFile(io, tmp_name, .{
        .permissions = @enumFromInt(0o600),
        .truncate = true,
    }) catch return error.Unavailable;
    var file_open = true;
    errdefer if (file_open) {
        file.close(io);
        dir.deleteFile(io, tmp_name) catch {};
    };
    file.writeStreamingAll(io, with_newline) catch return error.Unavailable;
    file.sync(io) catch return error.Unavailable;
    file.close(io);
    file_open = false;
    dir.rename(tmp_name, dir, base, io) catch {
        dir.deleteFile(io, tmp_name) catch {};
        return error.Unavailable;
    };
}

test "minimal config parses with retrieval defaults" {
    const parsed = try parseConfig(std.testing.allocator,
        \\{"journal_root": "/tmp/journals"}
    );
    defer parsed.deinit();
    try std.testing.expectEqual(@as(u32, 3), parsed.config.context_window);
    try std.testing.expectEqual(@as(u32, 10), parsed.config.max_results);
    try std.testing.expectEqual(false, parsed.config.miss_log);
    try std.testing.expectEqual(@as(?[]const u8, null), parsed.config.default_world);
    try std.testing.expectEqualStrings("main", parsed.config.capture.world);
    try std.testing.expectEqualStrings("default", parsed.config.capture.scope);
}

test "unknown keys and relative paths are malformed" {
    try std.testing.expectError(error.Malformed, parseConfig(std.testing.allocator,
        \\{"journal_root": "/j", "surprise": 1}
    ));
    try std.testing.expectError(error.Malformed, parseConfig(std.testing.allocator,
        \\{"journal_root": "relative/path"}
    ));
    try std.testing.expectError(error.Malformed, parseConfig(std.testing.allocator,
        \\{"journal_root": "/j", "thesaurus_path": "thesaurus.json"}
    ));
    try std.testing.expectError(error.Malformed, parseConfig(std.testing.allocator,
        \\{"journal_root": "/j", "default_world": "Bad World"}
    ));
    try std.testing.expectError(error.Malformed, parseConfig(std.testing.allocator,
        \\{"journal_root": "/j", "context_window": 0}
    ));
    try std.testing.expectError(error.Malformed, parseConfig(std.testing.allocator,
        \\{"journal_root": "/j", "capture": {"world": "Bad World"}}
    ));
    try std.testing.expectError(error.Malformed, parseConfig(std.testing.allocator,
        \\{"journal_root": "/j", "capture": {"scope": "has space"}}
    ));
}

test "retrieval knobs are honored" {
    const parsed = try parseConfig(std.testing.allocator,
        \\{"journal_root": "/j", "default_world": "willow", "confidence_floor": 2.5,
        \\ "miss_log": true, "thesaurus_path": "/home/x/thesaurus.json"}
    );
    defer parsed.deinit();
    try std.testing.expectEqualStrings("willow", parsed.config.default_world.?);
    try std.testing.expectEqual(@as(f64, 2.5), parsed.config.confidence_floor);
    try std.testing.expectEqual(true, parsed.config.miss_log);
}

test "legacy world_root is accepted but conflicting roots are malformed" {
    const parsed = try parseConfig(std.testing.allocator,
        \\{"world_root": "/tmp/legacy-journals"}
    );
    defer parsed.deinit();
    try std.testing.expectEqualStrings("/tmp/legacy-journals", parsed.config.journal_root.?);
    try std.testing.expectError(error.Malformed, parseConfig(std.testing.allocator,
        \\{"journal_root": "/new", "world_root": "/old"}
    ));
}

test "a config without journal_root is valid and names no root" {
    const parsed = try parseConfig(std.testing.allocator,
        \\{"capture": {"world": "team", "scope": "default"}}
    );
    defer parsed.deinit();
    try std.testing.expectEqual(@as(?[]const u8, null), parsed.config.journal_root);
    try std.testing.expectEqualStrings("team", parsed.config.capture.world);
}

test "saveCaptureDefaults creates, preserves, migrates, and refuses" {
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    var tmp = std.testing.tmpDir(.{});
    defer tmp.cleanup();
    const dir_path = try tmp.dir.realPathFileAlloc(io, ".", gpa);
    defer gpa.free(dir_path);
    const path = try std.fmt.allocPrint(gpa, "{s}/config.json", .{dir_path});
    defer gpa.free(path);
    var environ: std.process.Environ.Map = .{
        .array_hash_map = .empty,
        .allocator = gpa,
    };

    // A missing file is created with only the capture defaults.
    {
        const written = try saveCaptureDefaults(gpa, io, &environ, path, "team", "default");
        defer gpa.free(written);
        const loaded = try load(gpa, io, &environ, path);
        defer loaded.deinit();
        try std.testing.expectEqual(@as(?[]const u8, null), loaded.value().journal_root);
        try std.testing.expectEqualStrings("team", loaded.value().capture.world);
    }

    // A legacy config keeps its other keys, migrates the root key, and the
    // recall-side override follows the new default.
    try tmp.dir.writeFile(io, .{
        .sub_path = "config.json",
        .data =
        \\{"world_root": "/j", "default_world": "old", "miss_log": true}
        ,
    });
    {
        const written = try saveCaptureDefaults(gpa, io, &environ, path, "willow", "global");
        defer gpa.free(written);
        const loaded = try load(gpa, io, &environ, path);
        defer loaded.deinit();
        try std.testing.expectEqualStrings("/j", loaded.value().journal_root.?);
        try std.testing.expectEqualStrings("willow", loaded.value().default_world.?);
        try std.testing.expectEqualStrings("willow", loaded.value().capture.world);
        try std.testing.expectEqualStrings("global", loaded.value().capture.scope);
        try std.testing.expectEqual(true, loaded.value().miss_log);
        const raw = try tmp.dir.readFileAlloc(io, "config.json", gpa, .limited(4096));
        defer gpa.free(raw);
        try std.testing.expect(std.mem.indexOf(u8, raw, "world_root") == null);
    }

    // A scope-only update leaves a diverged recall-side override untouched.
    try tmp.dir.writeFile(io, .{
        .sub_path = "config.json",
        .data =
        \\{"default_world": "willow", "capture": {"world": "main", "scope": "default"}}
        ,
    });
    {
        const written = try saveCaptureDefaults(gpa, io, &environ, path, "main", "work");
        defer gpa.free(written);
        const loaded = try load(gpa, io, &environ, path);
        defer loaded.deinit();
        try std.testing.expectEqualStrings("willow", loaded.value().default_world.?);
        try std.testing.expectEqualStrings("work", loaded.value().capture.scope);
    }

    // Invalid identities and a malformed existing file are refused, and the
    // file is left untouched.
    try std.testing.expectError(
        error.Malformed,
        saveCaptureDefaults(gpa, io, &environ, path, "Bad World", "default"),
    );
    try tmp.dir.writeFile(io, .{
        .sub_path = "config.json",
        .data =
        \\{"surprise": 1}
        ,
    });
    try std.testing.expectError(
        error.Malformed,
        saveCaptureDefaults(gpa, io, &environ, path, "team", "default"),
    );
    const raw = try tmp.dir.readFileAlloc(io, "config.json", gpa, .limited(4096));
    defer gpa.free(raw);
    try std.testing.expectEqualStrings("{\"surprise\": 1}", raw);
}
