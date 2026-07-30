//! Owner-curated thesaurus (alias map) and the weak-query miss log.
//!
//! The thesaurus file is the authority and stays a flat, hand-editable JSON
//! object mapping a casual query word to the canonical journal terms it
//! should also search: `{"firmware": ["fwupd", "polkit"]}`. It is
//! byte-compatible with the deployed v1 map, loaded fresh on every search
//! invocation (editor changes apply immediately, no cache to invalidate),
//! and never projected into SQLite — only its canonical digest is recorded
//! and stamped on results as the alias identity.
//!
//! Curation is manual by design: the engine never writes an alias itself.
//! The miss log is the raw material for growing the map from real recall
//! misses; it is opt-in, owner-private, bounded, and best-effort.

const std = @import("std");
const Io = std.Io;
const contracts = @import("contracts.zig");
const retrieval = @import("retrieval.zig");
const store = @import("store.zig");

pub const max_thesaurus_bytes: usize = 256 * 1024;

pub const AliasMap = struct {
    state: std.heap.ArenaAllocator.State,
    gpa: std.mem.Allocator,
    /// Sorted by key; keys and values are lowercased.
    entries: []const Entry,
    /// SHA-256 hex of the canonical form (sorted keys, sorted deduped
    /// values). Independent of file formatting and key order.
    digest_hex: [64]u8,

    pub const Entry = struct {
        key: []const u8,
        values: []const []const u8,
    };

    pub fn deinit(self: *const AliasMap) void {
        self.state.promote(self.gpa).deinit();
    }

    pub fn get(self: *const AliasMap, term: []const u8) ?[]const []const u8 {
        var lo: usize = 0;
        var hi: usize = self.entries.len;
        while (lo < hi) {
            const mid = lo + (hi - lo) / 2;
            switch (std.mem.order(u8, self.entries[mid].key, term)) {
                .lt => lo = mid + 1,
                .gt => hi = mid,
                .eq => return self.entries[mid].values,
            }
        }
        return null;
    }
};

/// Tolerant load, v1 parity: only object entries whose value is an array
/// become aliases (array items that are not strings are skipped); keys and
/// values are lowercased. Anything unreadable or unparseable is a valid
/// empty configuration — recall never fails because the thesaurus does.
pub fn loadFromBytes(gpa: std.mem.Allocator, bytes: []const u8) error{OutOfMemory}!AliasMap {
    var arena_owner = std.heap.ArenaAllocator.init(gpa);
    errdefer arena_owner.deinit();
    const arena = arena_owner.allocator();

    var list: std.ArrayList(AliasMap.Entry) = .empty;
    build: {
        const parsed = std.json.parseFromSliceLeaky(std.json.Value, arena, bytes, .{}) catch |err|
            switch (err) {
                error.OutOfMemory => return error.OutOfMemory,
                else => break :build,
            };
        const object = switch (parsed) {
            .object => |o| o,
            else => break :build,
        };
        var it = object.iterator();
        while (it.next()) |kv| {
            const values = switch (kv.value_ptr.*) {
                .array => |a| a.items,
                else => continue,
            };
            var out_values: std.ArrayList([]const u8) = .empty;
            for (values) |v| {
                const text = switch (v) {
                    .string => |s| s,
                    else => continue,
                };
                if (text.len == 0 or text.len > contracts.max_token_len) continue;
                try out_values.append(arena, try lowerDupe(arena, text));
            }
            try list.append(arena, .{
                .key = try lowerDupe(arena, kv.key_ptr.*),
                .values = out_values.items,
            });
        }
    }
    std.mem.sort(AliasMap.Entry, list.items, {}, entryLessThan);

    // Populate before the result literal reads `arena_owner.state`.
    const entries = list.items;
    const digest = try canonicalDigest(arena, entries);
    return .{
        .state = arena_owner.state,
        .gpa = gpa,
        .entries = entries,
        .digest_hex = digest,
    };
}

pub fn loadFromFile(gpa: std.mem.Allocator, io: Io, path: []const u8) error{OutOfMemory}!AliasMap {
    const bytes = Io.Dir.cwd().readFileAlloc(io, path, gpa, .limited(max_thesaurus_bytes)) catch |err|
        switch (err) {
            error.OutOfMemory => return error.OutOfMemory,
            // Missing or unreadable map is a valid empty configuration.
            else => return loadFromBytes(gpa, "{}"),
        };
    defer gpa.free(bytes);
    return loadFromBytes(gpa, bytes);
}

fn lowerDupe(arena: std.mem.Allocator, text: []const u8) error{OutOfMemory}![]u8 {
    const copy = try arena.dupe(u8, text);
    for (copy) |*b| b.* = std.ascii.toLower(b.*);
    return copy;
}

fn entryLessThan(_: void, a: AliasMap.Entry, b: AliasMap.Entry) bool {
    return std.mem.order(u8, a.key, b.key) == .lt;
}

/// Canonical form: keys sorted, each line `key\x00value\x00value…\n` with
/// values sorted and deduped, so the digest tracks meaning rather than file
/// formatting. Every value participates — a digest that ignored any of
/// them could call two different maps identical.
fn canonicalDigest(
    arena: std.mem.Allocator,
    entries: []const AliasMap.Entry,
) error{OutOfMemory}![64]u8 {
    var h = std.crypto.hash.sha2.Sha256.init(.{});
    for (entries) |entry| {
        h.update(entry.key);
        const sorted = try arena.dupe([]const u8, entry.values);
        std.mem.sort([]const u8, sorted, {}, strLessThan);
        var prev: ?[]const u8 = null;
        for (sorted) |v| {
            if (prev != null and std.mem.eql(u8, prev.?, v)) continue;
            prev = v;
            h.update("\x00");
            h.update(v);
        }
        h.update("\n");
    }
    var sum: [std.crypto.hash.sha2.Sha256.digest_length]u8 = undefined;
    h.final(&sum);
    var out: [64]u8 = undefined;
    _ = std.fmt.bufPrint(&out, "{x}", .{&sum}) catch unreachable;
    return out;
}

fn strLessThan(_: void, a: []const u8, b: []const u8) bool {
    return std.mem.order(u8, a, b) == .lt;
}

// --- Owner edits (CLI `alias add` / `alias remove`) ---

pub const EditError = error{
    /// The key would never fire: it must survive query tokenization
    /// (length > 2, `[a-z0-9_]`, not a stop word).
    InvalidTerm,
    /// A value must be a searchable token or phrase: 2..128 bytes from the
    /// identity-token charset.
    InvalidValue,
    /// The file exists but is not a JSON object; refusing to rewrite it
    /// protects a hand-edit gone wrong from being clobbered.
    Malformed,
    NotFound,
    OutOfMemory,
    Unavailable,
};

/// Adds (or extends) one alias entry and atomically rewrites the file,
/// preserving every entry it does not touch. Values already present are
/// not duplicated.
pub fn addAlias(
    gpa: std.mem.Allocator,
    io: Io,
    path: []const u8,
    term: []const u8,
    canonicals: []const []const u8,
) EditError!void {
    var arena_owner = std.heap.ArenaAllocator.init(gpa);
    defer arena_owner.deinit();
    const arena = arena_owner.allocator();

    const key = try lowerDupe(arena, term);
    if (!validAliasKey(key)) return error.InvalidTerm;
    if (canonicals.len == 0) return error.InvalidValue;

    var root = try readEditable(arena, io, path);
    const existing = root.object.getPtr(key);
    var values: std.json.Array = if (existing) |v| switch (v.*) {
        .array => |a| a,
        else => return error.Malformed,
    } else .init(arena);

    for (canonicals) |raw| {
        const value = try lowerDupe(arena, raw);
        if (!validAliasValue(value)) return error.InvalidValue;
        const already = for (values.items) |v| {
            if (v == .string and std.mem.eql(u8, v.string, value)) break true;
        } else false;
        if (!already) try values.append(.{ .string = value });
    }
    try root.object.put(arena, key, .{ .array = values });
    try writeAtomic(arena, io, path, root);
}

pub const Removed = enum { entry, value };

/// Removes a whole entry, or one value from an entry (dropping the entry
/// when its last value goes), and atomically rewrites the file.
pub fn removeAlias(
    gpa: std.mem.Allocator,
    io: Io,
    path: []const u8,
    term: []const u8,
    canonical: ?[]const u8,
) EditError!Removed {
    var arena_owner = std.heap.ArenaAllocator.init(gpa);
    defer arena_owner.deinit();
    const arena = arena_owner.allocator();

    const key = try lowerDupe(arena, term);
    var root = try readEditable(arena, io, path);
    const existing = root.object.getPtr(key) orelse return error.NotFound;

    var removed: Removed = .entry;
    if (canonical) |raw| {
        const value = try lowerDupe(arena, raw);
        var values = switch (existing.*) {
            .array => |a| a,
            else => return error.Malformed,
        };
        const at = for (values.items, 0..) |v, i| {
            if (v == .string and std.mem.eql(u8, v.string, value)) break i;
        } else return error.NotFound;
        _ = values.orderedRemove(at);
        if (values.items.len == 0) {
            _ = root.object.orderedRemove(key);
        } else {
            try root.object.put(arena, key, .{ .array = values });
            removed = .value;
        }
    } else {
        _ = root.object.orderedRemove(key);
    }
    try writeAtomic(arena, io, path, root);
    return removed;
}

fn validAliasKey(key: []const u8) bool {
    if (key.len <= 2 or key.len > contracts.max_token_len) return false;
    for (key) |b| switch (b) {
        'a'...'z', '0'...'9', '_' => {},
        else => return false,
    };
    return !retrieval.isStopWord(key);
}

fn validAliasValue(value: []const u8) bool {
    if (value.len < 2) return false;
    return contracts.validToken(value);
}

/// Reads the file as a mutable JSON object; a missing file starts empty,
/// but an existing non-object file is `Malformed`, never overwritten.
fn readEditable(arena: std.mem.Allocator, io: Io, path: []const u8) EditError!std.json.Value {
    const bytes = Io.Dir.cwd().readFileAlloc(io, path, arena, .limited(max_thesaurus_bytes)) catch |err|
        switch (err) {
            error.OutOfMemory => return error.OutOfMemory,
            error.FileNotFound => return .{ .object = .empty },
            else => return error.Unavailable,
        };
    const parsed = std.json.parseFromSliceLeaky(std.json.Value, arena, bytes, .{}) catch |err|
        switch (err) {
            error.OutOfMemory => return error.OutOfMemory,
            else => return error.Malformed,
        };
    if (parsed != .object) return error.Malformed;
    return parsed;
}

fn writeAtomic(arena: std.mem.Allocator, io: Io, path: []const u8, root: std.json.Value) EditError!void {
    const text = std.json.Stringify.valueAlloc(arena, root, .{
        .whitespace = .indent_2,
    }) catch return error.OutOfMemory;
    const with_newline = std.fmt.allocPrint(arena, "{s}\n", .{text}) catch return error.OutOfMemory;

    const dir_path = std.fs.path.dirname(path) orelse return error.Unavailable;
    const base = std.fs.path.basename(path);
    Io.Dir.cwd().createDirPath(io, dir_path) catch return error.Unavailable;
    var dir = Io.Dir.openDirAbsolute(io, dir_path, .{}) catch return error.Unavailable;
    defer dir.close(io);

    const tmp_name = std.fmt.allocPrint(arena, ".{s}.tmp", .{base}) catch return error.OutOfMemory;
    var file = dir.createFile(io, tmp_name, .{
        .permissions = store.file_permissions,
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

// --- Miss log ---

pub const MissRecord = struct {
    ts: []const u8,
    query: []const u8,
    terms: []const []const u8,
    best: f64,
    top: ?[]const u8,
};

/// Appends one weak-query record as a JSON line. Best-effort by contract:
/// every failure is swallowed, and the log stops growing at `max_bytes`.
pub fn appendMiss(
    gpa: std.mem.Allocator,
    io: Io,
    path: []const u8,
    record: MissRecord,
    max_bytes: u64,
) void {
    const line = std.json.Stringify.valueAlloc(gpa, record, .{}) catch return;
    defer gpa.free(line);
    const dir_path = std.fs.path.dirname(path) orelse return;
    Io.Dir.cwd().createDirPath(io, dir_path) catch return;

    var file = Io.Dir.cwd().openFile(io, path, .{ .mode = .write_only }) catch |err| switch (err) {
        error.FileNotFound => Io.Dir.cwd().createFile(io, path, .{
            .permissions = store.file_permissions,
            .truncate = false,
        }) catch return,
        else => return,
    };
    defer file.close(io);
    file.setPermissions(io, store.file_permissions) catch return;
    const stat = file.stat(io) catch return;
    if (stat.size >= max_bytes) return;
    var buf: std.ArrayList(u8) = .empty;
    defer buf.deinit(gpa);
    buf.print(gpa, "{s}\n", .{line}) catch return;
    file.writePositionalAll(io, buf.items, stat.size) catch return;
}

/// One reviewed candidate: a distinct weak query, its frequency, and the
/// union of extracted terms across its misses.
pub const Candidate = struct {
    query: []const u8,
    count: u64,
    terms: []const []const u8,
};

pub const Candidates = struct {
    state: std.heap.ArenaAllocator.State,
    gpa: std.mem.Allocator,
    items: []const Candidate,

    pub fn deinit(self: *const Candidates) void {
        self.state.promote(self.gpa).deinit();
    }
};

/// Aggregates the miss log for review: dedupes by lowercased query, ranks
/// by frequency (ties alphabetical). Malformed lines are skipped — the log
/// is best-effort on the write side too.
pub fn aggregateMisses(gpa: std.mem.Allocator, bytes: []const u8) error{OutOfMemory}!Candidates {
    var arena_owner = std.heap.ArenaAllocator.init(gpa);
    errdefer arena_owner.deinit();
    const arena = arena_owner.allocator();

    const Agg = struct {
        count: u64,
        terms: std.StringArrayHashMapUnmanaged(void),
    };
    var by_query: std.StringArrayHashMapUnmanaged(Agg) = .empty;

    var lines = std.mem.splitScalar(u8, bytes, '\n');
    while (lines.next()) |line| {
        const trimmed = std.mem.trim(u8, line, " \t\r");
        if (trimmed.len == 0) continue;
        const parsed = std.json.parseFromSliceLeaky(std.json.Value, arena, trimmed, .{}) catch |err|
            switch (err) {
                error.OutOfMemory => return error.OutOfMemory,
                else => continue,
            };
        if (parsed != .object) continue;
        const query_value = parsed.object.get("query") orelse continue;
        if (query_value != .string) continue;
        const query = try lowerDupe(arena, std.mem.trim(u8, query_value.string, " \t"));

        const slot = try by_query.getOrPut(arena, query);
        if (!slot.found_existing) slot.value_ptr.* = .{ .count = 0, .terms = .empty };
        slot.value_ptr.count += 1;
        if (parsed.object.get("terms")) |terms_value| {
            if (terms_value == .array) for (terms_value.array.items) |t| {
                if (t == .string) _ = try slot.value_ptr.terms.getOrPut(arena, t.string);
            };
        }
    }

    var items = try arena.alloc(Candidate, by_query.count());
    var it = by_query.iterator();
    var i: usize = 0;
    while (it.next()) |kv| : (i += 1) {
        const terms = kv.value_ptr.terms.keys();
        std.mem.sort([]const u8, terms, {}, strLessThan);
        items[i] = .{
            .query = kv.key_ptr.*,
            .count = kv.value_ptr.count,
            .terms = terms,
        };
    }
    std.mem.sort(Candidate, items, {}, candidateMoreFrequent);

    const owned_items = items;
    return .{
        .state = arena_owner.state,
        .gpa = gpa,
        .items = owned_items,
    };
}

fn candidateMoreFrequent(_: void, a: Candidate, b: Candidate) bool {
    if (a.count != b.count) return a.count > b.count;
    return std.mem.order(u8, a.query, b.query) == .lt;
}

// --- Tests ---

test "load tolerates junk and lowercases; digest ignores formatting and order" {
    const gpa = std.testing.allocator;
    const map = try loadFromBytes(gpa,
        \\{"Firmware": ["FWUPD", "polkit"], "quant": ["gguf", "q8"],
        \\ "junk": "not-an-array", "numbers": [1, 2]}
    );
    defer map.deinit();
    const fw = map.get("firmware") orelse return error.TestUnexpectedResult;
    try std.testing.expectEqual(@as(usize, 2), fw.len);
    try std.testing.expectEqualStrings("fwupd", fw[0]);
    try std.testing.expectEqual(@as(?[]const []const u8, null), map.get("junk"));
    // "numbers" keeps its key with no valid values.
    try std.testing.expectEqual(@as(usize, 0), map.get("numbers").?.len);

    const reordered = try loadFromBytes(gpa,
        \\{
        \\  "numbers": [],
        \\  "quant": ["q8", "gguf", "q8"],
        \\  "firmware": ["polkit", "fwupd"]
        \\}
    );
    defer reordered.deinit();
    try std.testing.expectEqualStrings(&map.digest_hex, &reordered.digest_hex);

    const different = try loadFromBytes(gpa,
        \\{"firmware": ["fwupd"]}
    );
    defer different.deinit();
    try std.testing.expect(!std.mem.eql(u8, &map.digest_hex, &different.digest_hex));
}

test "digest covers every value, including past the 64th" {
    const gpa = std.testing.allocator;
    var a_json: std.ArrayList(u8) = .empty;
    defer a_json.deinit(gpa);
    var b_json: std.ArrayList(u8) = .empty;
    defer b_json.deinit(gpa);
    try a_json.appendSlice(gpa, "{\"term\": [");
    try b_json.appendSlice(gpa, "{\"term\": [");
    // The two maps agree on the 64 lexically-smallest values and differ
    // only in the last one, so any digest that caps the sorted value list
    // would wrongly call them identical.
    for (0..65) |i| {
        if (i > 0) {
            try a_json.append(gpa, ',');
            try b_json.append(gpa, ',');
        }
        try a_json.print(gpa, "\"v{d:0>3}\"", .{i});
        if (i == 64) {
            try b_json.appendSlice(gpa, "\"v999\"");
        } else {
            try b_json.print(gpa, "\"v{d:0>3}\"", .{i});
        }
    }
    try a_json.appendSlice(gpa, "]}");
    try b_json.appendSlice(gpa, "]}");
    const a = try loadFromBytes(gpa, a_json.items);
    defer a.deinit();
    const b = try loadFromBytes(gpa, b_json.items);
    defer b.deinit();
    try std.testing.expect(!std.mem.eql(u8, &a.digest_hex, &b.digest_hex));
}

test "corrupt or missing thesaurus is an empty map, never an error" {
    const gpa = std.testing.allocator;
    const corrupt = try loadFromBytes(gpa, "not json at all {{{");
    defer corrupt.deinit();
    try std.testing.expectEqual(@as(usize, 0), corrupt.entries.len);
    const empty = try loadFromBytes(gpa, "{}");
    defer empty.deinit();
    try std.testing.expectEqualStrings(&empty.digest_hex, &corrupt.digest_hex);

    const missing = try loadFromFile(gpa, std.testing.io, "/nonexistent/thesaurus.json");
    defer missing.deinit();
    try std.testing.expectEqual(@as(usize, 0), missing.entries.len);
}

test "add and remove rewrite the file atomically and preserve foreign entries" {
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    var tmp = std.testing.tmpDir(.{});
    defer tmp.cleanup();
    const dir_path = try tmp.dir.realPathFileAlloc(io, ".", gpa);
    defer gpa.free(dir_path);
    const path = try std.fmt.allocPrint(gpa, "{s}/thesaurus.json", .{dir_path});
    defer gpa.free(path);

    // Foreign key shapes survive edits untouched.
    try tmp.dir.writeFile(io, .{
        .sub_path = "thesaurus.json",
        .data =
        \\{"keep-me": {"nested": true}, "firmware": ["fwupd"]}
        ,
    });

    try addAlias(gpa, io, path, "Firmware", &.{ "polkit", "FWUPD" });
    try addAlias(gpa, io, path, "planner", &.{"plannotator"});
    {
        const map = try loadFromFile(gpa, io, path);
        defer map.deinit();
        const fw = map.get("firmware").?;
        try std.testing.expectEqual(@as(usize, 2), fw.len); // fwupd deduped
        try std.testing.expectEqualStrings("plannotator", map.get("planner").?[0]);
    }
    {
        const raw = try tmp.dir.readFileAlloc(io, "thesaurus.json", gpa, .limited(4096));
        defer gpa.free(raw);
        try std.testing.expect(std.mem.indexOf(u8, raw, "keep-me") != null);
        try std.testing.expect(std.mem.indexOf(u8, raw, "nested") != null);
    }

    try std.testing.expectEqual(Removed.value, try removeAlias(gpa, io, path, "firmware", "polkit"));
    try std.testing.expectEqual(Removed.entry, try removeAlias(gpa, io, path, "firmware", "fwupd"));
    try std.testing.expectError(error.NotFound, removeAlias(gpa, io, path, "firmware", null));
    try std.testing.expectEqual(Removed.entry, try removeAlias(gpa, io, path, "planner", null));

    // Guardrails: keys must be able to fire; values must be searchable.
    try std.testing.expectError(error.InvalidTerm, addAlias(gpa, io, path, "the", &.{"fwupd"}));
    try std.testing.expectError(error.InvalidTerm, addAlias(gpa, io, path, "no spaces", &.{"x2"}));
    try std.testing.expectError(error.InvalidValue, addAlias(gpa, io, path, "weight", &.{"q"}));
}

test "editing refuses to clobber a non-object file" {
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    var tmp = std.testing.tmpDir(.{});
    defer tmp.cleanup();
    const dir_path = try tmp.dir.realPathFileAlloc(io, ".", gpa);
    defer gpa.free(dir_path);
    const path = try std.fmt.allocPrint(gpa, "{s}/thesaurus.json", .{dir_path});
    defer gpa.free(path);
    try tmp.dir.writeFile(io, .{ .sub_path = "thesaurus.json", .data = "[1, 2, 3]" });
    try std.testing.expectError(error.Malformed, addAlias(gpa, io, path, "weight", &.{"gguf"}));
    const raw = try tmp.dir.readFileAlloc(io, "thesaurus.json", gpa, .limited(4096));
    defer gpa.free(raw);
    try std.testing.expectEqualStrings("[1, 2, 3]", raw);
}

test "miss log appends, bounds, and aggregates" {
    const gpa = std.testing.allocator;
    const io = std.testing.io;
    var tmp = std.testing.tmpDir(.{});
    defer tmp.cleanup();
    const dir_path = try tmp.dir.realPathFileAlloc(io, ".", gpa);
    defer gpa.free(dir_path);
    const path = try std.fmt.allocPrint(gpa, "{s}/state/misses.jsonl", .{dir_path});
    defer gpa.free(path);

    const rec: MissRecord = .{
        .ts = "2026-07-28T12:00:00Z",
        .query = "Weight Format",
        .terms = &.{ "weight", "format" },
        .best = 1.75,
        .top = null,
    };
    appendMiss(gpa, io, path, rec, 1024 * 1024);
    appendMiss(gpa, io, path, rec, 1024 * 1024);
    var other = rec;
    other.query = "vpn setup";
    other.terms = &.{ "vpn", "setup" };
    appendMiss(gpa, io, path, other, 1024 * 1024);
    // A full log stops growing but never errors.
    appendMiss(gpa, io, path, other, 1);

    const bytes = try Io.Dir.cwd().readFileAlloc(io, path, gpa, .limited(65536));
    defer gpa.free(bytes);
    const agg = try aggregateMisses(gpa, bytes);
    defer agg.deinit();
    try std.testing.expectEqual(@as(usize, 2), agg.items.len);
    try std.testing.expectEqualStrings("weight format", agg.items[0].query);
    try std.testing.expectEqual(@as(u64, 2), agg.items[0].count);
    try std.testing.expectEqualStrings("vpn setup", agg.items[1].query);
    try std.testing.expectEqual(@as(usize, 2), agg.items[0].terms.len);
}
