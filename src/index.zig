//! Disposable SQLite projection over the Markdown corpus.
//!
//! Contains only rebuildable retrieval state. Deleting the database and
//! running `sync` reproduces it from the authoritative episode files; a
//! failed or busy index update leaves capture successful and the projection
//! visibly stale.
//!
//! Schema v2 adds retrieval state: per-line term postings, a per-world
//! vocabulary with episode document frequencies, the body start line, and
//! identity rows (schema, tokenizer, root digest) that gate reuse — a
//! wrong-schema, wrong-tokenizer, or wrong-root database is disposed or
//! rejected, never misread as an empty memory corpus.

const std = @import("std");
const Io = std.Io;
const db = @import("db.zig");
const contracts = @import("contracts.zig");
const frontmatter = @import("frontmatter.zig");
const retrieval = @import("retrieval.zig");

pub const index_schema_version = 2;

const create_sql =
    \\CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
    \\CREATE TABLE IF NOT EXISTS episodes (
    \\  episode_id TEXT PRIMARY KEY,
    \\  digest_hex TEXT NOT NULL,
    \\  rel_path TEXT NOT NULL,
    \\  world TEXT NOT NULL,
    \\  scope TEXT NOT NULL,
    \\  lane TEXT NOT NULL,
    \\  harness TEXT NOT NULL,
    \\  session_id TEXT NOT NULL,
    \\  turn_id TEXT NOT NULL,
    \\  event_time_ms INTEGER NOT NULL,
    \\  capture_time_ms INTEGER NOT NULL,
    \\  capture_policy TEXT NOT NULL,
    \\  turn_outcome TEXT NOT NULL,
    \\  body_line INTEGER NOT NULL DEFAULT 0
    \\);
    \\CREATE INDEX IF NOT EXISTS idx_episodes_world_time
    \\  ON episodes(world, event_time_ms);
    \\CREATE INDEX IF NOT EXISTS idx_episodes_world_lane
    \\  ON episodes(world, lane);
    \\CREATE TABLE IF NOT EXISTS postings (
    \\  term TEXT NOT NULL,
    \\  episode_id TEXT NOT NULL,
    \\  line_no INTEGER NOT NULL,
    \\  PRIMARY KEY (term, episode_id, line_no)
    \\) WITHOUT ROWID;
    \\CREATE INDEX IF NOT EXISTS idx_postings_episode ON postings(episode_id);
    \\CREATE TABLE IF NOT EXISTS term_stats (
    \\  world TEXT NOT NULL,
    \\  term TEXT NOT NULL,
    \\  df INTEGER NOT NULL,
    \\  eval_df INTEGER NOT NULL DEFAULT 0,
    \\  PRIMARY KEY (world, term)
    \\) WITHOUT ROWID;
;

pub const OpenError = db.Error || error{
    /// The database carries another root's identity. Never interpreted as
    /// an empty corpus; the caller reports `unavailable` and advises
    /// `sync`/`rebuild` against the right root.
    ForeignIndex,
};

pub const Index = struct {
    handle: db.Db,

    /// Opens (creating if needed) the projection at `path`. A projection
    /// whose schema or tokenizer version does not match is disposable state
    /// from another build: every table is dropped and recreated. A
    /// projection recording a different `root_digest` is foreign and is
    /// rejected instead — disposal would silently destroy another root's
    /// index.
    pub fn open(
        gpa: std.mem.Allocator,
        path: [:0]const u8,
        expected_root_digest: ?[]const u8,
    ) OpenError!Index {
        var d = try db.Db.open(path);
        errdefer d.close();
        var idx: Index = .{ .handle = d };

        // Identity is read before any DDL: running `create_sql` against an
        // unknown older schema could itself fail. A missing meta table
        // (fresh database) reads as no identity and takes the dispose-and-
        // create path, which drops nothing.
        var buf: [meta_value_max]u8 = undefined;
        const version = idx.metaGetInt("index_schema_version") catch |err| switch (err) {
            error.SqliteError => null,
            else => return err,
        };
        const tokenizer_ok = blk: {
            const stored = idx.metaGetBuf("tokenizer_version", &buf) catch |err| switch (err) {
                error.SqliteError => null,
                else => return err,
            };
            break :blk if (stored) |v| std.mem.eql(u8, v, retrieval.tokenizer_version) else false;
        };
        if (version != index_schema_version or !tokenizer_ok) {
            try idx.disposeAllTables(gpa);
            try d.exec(create_sql);
            try idx.writeIdentity();
        }

        if (expected_root_digest) |expected| {
            if (try idx.metaGetBuf("root_digest", &buf)) |stored| {
                if (!std.mem.eql(u8, stored, expected)) return error.ForeignIndex;
            } else {
                try idx.metaSet("root_digest", expected);
            }
        }
        return idx;
    }

    pub fn close(idx: *Index) void {
        idx.handle.close();
    }

    /// `open`, plus the filesystem hygiene every host owes the projection:
    /// the parent directory is created owner-only, and the database and its
    /// SQLite sidecars are narrowed to 0600. The index holds episode text,
    /// so a default-umask 0644 database would publish the owner's captured
    /// conversations to every account on the machine.
    ///
    /// Hosts call this rather than `open` unless they are deliberately
    /// opening a projection they did not create.
    pub fn openHardened(
        gpa: std.mem.Allocator,
        io: Io,
        index_path: []const u8,
        expected_root_digest: ?[]const u8,
    ) OpenError!Index {
        const path_z = try gpa.dupeZ(u8, index_path);
        defer gpa.free(path_z);

        const parent = std.fs.path.dirname(index_path) orelse return error.CantOpen;
        Io.Dir.cwd().createDirPath(io, parent) catch return error.CantOpen;
        var parent_dir = Io.Dir.openDirAbsolute(io, parent, .{ .iterate = true }) catch
            return error.CantOpen;
        defer parent_dir.close(io);
        parent_dir.setPermissions(io, @enumFromInt(0o700)) catch return error.CantOpen;

        var idx = try Index.open(gpa, path_z, expected_root_digest);
        errdefer idx.close();
        try hardenFiles(gpa, io, index_path);
        return idx;
    }

    /// Narrow the database and any SQLite sidecar to owner-only. Called on
    /// open and again after a write, because the `-wal` and `-shm` files can
    /// appear at any point in a transaction.
    pub fn hardenFiles(
        gpa: std.mem.Allocator,
        io: Io,
        index_path: []const u8,
    ) OpenError!void {
        var index_file = Io.Dir.cwd().openFile(io, index_path, .{ .mode = .read_write }) catch
            return error.CantOpen;
        defer index_file.close(io);
        index_file.setPermissions(io, @enumFromInt(0o600)) catch return error.CantOpen;
        for ([_][]const u8{ "-wal", "-shm", "-journal" }) |suffix| {
            const sidecar = try std.fmt.allocPrint(gpa, "{s}{s}", .{ index_path, suffix });
            defer gpa.free(sidecar);
            Io.Dir.cwd().setFilePermissions(
                io,
                sidecar,
                @enumFromInt(0o600),
                .{ .follow_symlinks = false },
            ) catch |err| switch (err) {
                error.FileNotFound => {},
                else => return error.CantOpen,
            };
        }
    }

    const meta_value_max = 128;

    fn metaGetInt(idx: *Index, key: []const u8) db.Error!?i64 {
        var buf: [meta_value_max]u8 = undefined;
        const text = (try idx.metaGetBuf(key, &buf)) orelse return null;
        return std.fmt.parseInt(i64, text, 10) catch null;
    }

    /// Copies the value into `buf`; null when absent or oversized.
    /// Files the last sync deliberately excluded (duplicate ids, malformed).
    /// Freshness checks add this to the indexed count so deliberate
    /// exclusions never read as staleness.
    pub fn excludedCount(idx: *Index) u64 {
        var buf: [20]u8 = undefined;
        const text = (idx.metaGetBuf("sync_excluded", &buf) catch return 0) orelse return 0;
        return std.fmt.parseInt(u64, text, 10) catch 0;
    }

    pub fn metaGetBuf(idx: *Index, key: []const u8, buf: []u8) db.Error!?[]u8 {
        var st = try idx.handle.prepare("SELECT value FROM meta WHERE key = ?1;");
        defer st.finalize();
        try st.bindText(1, key);
        if (!try st.step()) return null;
        const value = st.columnText(0);
        if (value.len > buf.len) return null;
        @memcpy(buf[0..value.len], value);
        return buf[0..value.len];
    }

    pub fn metaSet(idx: *Index, key: []const u8, value: []const u8) db.Error!void {
        var st = try idx.handle.prepare(
            "INSERT OR REPLACE INTO meta (key, value) VALUES (?1, ?2);",
        );
        defer st.finalize();
        try st.bindText(1, key);
        try st.bindText(2, value);
        _ = try st.step();
    }

    fn writeIdentity(idx: *Index) db.Error!void {
        var buf: [20]u8 = undefined;
        try idx.metaSet(
            "index_schema_version",
            std.fmt.bufPrint(&buf, "{d}", .{index_schema_version}) catch unreachable,
        );
        try idx.metaSet("tokenizer_version", retrieval.tokenizer_version);
        try idx.metaSet("scorer_version", retrieval.scorer_version);
        try idx.metaSet("confidence_policy", retrieval.confidence_policy_version);
    }

    /// Drops every non-internal table, whatever schema left it behind, so a
    /// version bump can never strand stale tables from an unknown build.
    fn disposeAllTables(idx: *Index, gpa: std.mem.Allocator) db.Error!void {
        var names: std.ArrayList([]u8) = .empty;
        defer {
            for (names.items) |n| gpa.free(n);
            names.deinit(gpa);
        }
        {
            var st = try idx.handle.prepare(
                "SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%';",
            );
            defer st.finalize();
            while (try st.step()) {
                const name = st.columnText(0);
                if (std.mem.indexOfScalar(u8, name, '"') != null) continue;
                const copy = try gpa.dupe(u8, name);
                errdefer gpa.free(copy);
                try names.append(gpa, copy);
            }
        }
        for (names.items) |name| {
            const sql = std.fmt.allocPrintSentinel(gpa, "DROP TABLE IF EXISTS \"{s}\";", .{name}, 0) catch
                return error.OutOfMemory;
            defer gpa.free(sql);
            try idx.handle.exec(sql);
        }
    }

    pub const Row = struct {
        episode_id: []const u8,
        digest_hex: []const u8,
        rel_path: []const u8,
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
        body_line: u32 = 0,
    };

    /// Episodes-row-only upsert: postings and term stats are NOT touched.
    /// `indexEpisode` is the paved path; this survives for repair tooling
    /// and tests that need a bare row.
    pub fn upsert(idx: *Index, row: Row) db.Error!void {
        var st = try idx.handle.prepare(
            \\INSERT OR REPLACE INTO episodes (
            \\  episode_id, digest_hex, rel_path, world, scope, lane, harness,
            \\  session_id, turn_id, event_time_ms, capture_time_ms,
            \\  capture_policy, turn_outcome, body_line
            \\) VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11,?12,?13,?14);
        );
        defer st.finalize();
        try st.bindText(1, row.episode_id);
        try st.bindText(2, row.digest_hex);
        try st.bindText(3, row.rel_path);
        try st.bindText(4, row.world);
        try st.bindText(5, row.scope);
        try st.bindText(6, @tagName(row.lane));
        try st.bindText(7, row.harness);
        try st.bindText(8, row.session_id);
        try st.bindText(9, row.turn_id);
        try st.bindInt64(10, @intCast(@min(row.event_time_ms, std.math.maxInt(i64))));
        try st.bindInt64(11, @intCast(@min(row.capture_time_ms, std.math.maxInt(i64))));
        try st.bindText(12, row.capture_policy);
        try st.bindText(13, row.turn_outcome);
        try st.bindInt64(14, row.body_line);
        _ = try st.step();
    }

    pub const IndexEpisodeError = db.Error || error{Malformed};

    /// Fully indexes one episode from its rendered content: episode row,
    /// per-line postings from the body (frontmatter is metadata, not
    /// memory), and per-world document frequencies (evaluation lane is
    /// excluded from stats). One transaction; idempotent per content.
    pub fn indexEpisode(idx: *Index, rel_path: []const u8, content: []const u8) IndexEpisodeError!void {
        try idx.handle.exec("BEGIN IMMEDIATE;");
        errdefer idx.handle.exec("ROLLBACK;") catch {};
        try idx.indexEpisodeInTx(rel_path, content);
        try idx.handle.exec("COMMIT;");
    }

    /// Caller holds the transaction (`sync` wraps the whole corpus walk in
    /// one); never call outside BEGIN..COMMIT or a torn write becomes
    /// visible.
    fn indexEpisodeInTx(idx: *Index, rel_path: []const u8, content: []const u8) IndexEpisodeError!void {
        const ep = frontmatter.parse(content) orelse return error.Malformed;
        try idx.deindexEpisodeInTx(ep.episode_id);
        try idx.upsert(.{
            .episode_id = ep.episode_id,
            .digest_hex = ep.digest_hex,
            .rel_path = rel_path,
            .world = ep.world,
            .scope = ep.scope,
            .lane = ep.lane,
            .harness = ep.harness,
            .session_id = ep.session_id,
            .turn_id = ep.turn_id,
            .event_time_ms = ep.event_time_ms,
            .capture_time_ms = ep.capture_time_ms,
            .capture_policy = ep.capture_policy,
            .turn_outcome = ep.turn_outcome,
            .body_line = ep.body_line,
        });

        {
            var st = try idx.handle.prepare(
                "INSERT OR IGNORE INTO postings (term, episode_id, line_no) VALUES (?1, ?2, ?3);",
            );
            defer st.finalize();
            var line_no: u32 = ep.body_line;
            var lines = std.mem.splitScalar(u8, content[ep.body_offset..], '\n');
            while (lines.next()) |line| : (line_no += 1) {
                var tokens = retrieval.tokenizeLine(line);
                while (tokens.next()) |token| {
                    // Bindings survive reset: rebind every parameter.
                    try st.reset();
                    try st.bindText(1, token);
                    try st.bindText(2, ep.episode_id);
                    try st.bindInt64(3, line_no);
                    _ = try st.step();
                }
            }
        }

        // Every lane's terms enter the vocabulary (or explicit evaluation
        // queries could never discover anything), but only non-evaluation
        // lanes count toward `df` corpus statistics. The no-op `WHERE true`
        // is required: SQLite refuses `INSERT ... SELECT ... ON CONFLICT`
        // without a WHERE clause on the SELECT (documented upsert parsing
        // ambiguity), so removing it is a syntax error.
        {
            var st = try idx.handle.prepare(if (ep.lane == .evaluation)
                \\INSERT INTO term_stats (world, term, df, eval_df)
                \\  SELECT ?1, term, 0, 1
                \\  FROM (SELECT DISTINCT term FROM postings WHERE episode_id = ?2)
                \\  WHERE true
                \\  ON CONFLICT(world, term) DO UPDATE SET eval_df = eval_df + 1;
            else
                \\INSERT INTO term_stats (world, term, df, eval_df)
                \\  SELECT ?1, term, 1, 0
                \\  FROM (SELECT DISTINCT term FROM postings WHERE episode_id = ?2)
                \\  WHERE true
                \\  ON CONFLICT(world, term) DO UPDATE SET df = df + 1;
            );
            defer st.finalize();
            try st.bindText(1, ep.world);
            try st.bindText(2, ep.episode_id);
            _ = try st.step();
        }
    }

    /// Removes an episode's postings and decrements its document
    /// frequencies (when it counted toward them). The episodes row itself
    /// is left to the caller: upsert replaces it, deletion removes it.
    fn deindexEpisodeInTx(idx: *Index, episode_id: []const u8) db.Error!void {
        var world_buf: [contracts.max_world_len]u8 = undefined;
        var old_world: ?[]u8 = null;
        var old_counted = false;
        {
            var st = try idx.handle.prepare(
                "SELECT world, lane FROM episodes WHERE episode_id = ?1;",
            );
            defer st.finalize();
            try st.bindText(1, episode_id);
            if (try st.step()) {
                const world = st.columnText(0);
                // An over-bound world is a corrupt row; `old_world` stays
                // null, postings and stats are left in place, and `sync`
                // rebuilds them rather than guessing here.
                if (world.len <= world_buf.len) {
                    @memcpy(world_buf[0..world.len], world);
                    old_world = world_buf[0..world.len];
                }
                old_counted = !std.mem.eql(u8, st.columnText(1), @tagName(contracts.Lane.evaluation));
            }
        }
        if (old_world) |world| {
            {
                var st = try idx.handle.prepare(if (old_counted)
                    \\UPDATE term_stats SET df = df - 1
                    \\  WHERE world = ?1 AND term IN
                    \\    (SELECT DISTINCT term FROM postings WHERE episode_id = ?2);
                else
                    \\UPDATE term_stats SET eval_df = eval_df - 1
                    \\  WHERE world = ?1 AND term IN
                    \\    (SELECT DISTINCT term FROM postings WHERE episode_id = ?2);
                );
                defer st.finalize();
                try st.bindText(1, world);
                try st.bindText(2, episode_id);
                _ = try st.step();
                var cleanup = try idx.handle.prepare(
                    "DELETE FROM term_stats WHERE world = ?1 AND df <= 0 AND eval_df <= 0;",
                );
                defer cleanup.finalize();
                try cleanup.bindText(1, world);
                _ = try cleanup.step();
            }
            var st = try idx.handle.prepare("DELETE FROM postings WHERE episode_id = ?1;");
            defer st.finalize();
            try st.bindText(1, episode_id);
            _ = try st.step();
        }
    }

    pub fn episodeCount(idx: *Index) db.Error!u64 {
        var st = try idx.handle.prepare("SELECT COUNT(*) FROM episodes;");
        defer st.finalize();
        if (!try st.step()) return 0;
        return @intCast(@max(0, st.columnInt64(0)));
    }

    /// Corpus size `N` for IDF: episodes in the world, evaluation lane
    /// excluded from corpus statistics.
    pub fn statsEpisodeCount(idx: *Index, world: []const u8) db.Error!u64 {
        var st = try idx.handle.prepare(
            "SELECT COUNT(*) FROM episodes WHERE world = ?1 AND lane <> 'evaluation';",
        );
        defer st.finalize();
        try st.bindText(1, world);
        if (!try st.step()) return 0;
        return @intCast(@max(0, st.columnInt64(0)));
    }

    /// Iterates the world's vocabulary for substring discovery. Slices
    /// borrow statement memory and die on the next call.
    pub const TermIterator = struct {
        st: db.Stmt,

        pub fn next(self: *TermIterator) db.Error!?[]const u8 {
            if (!try self.st.step()) return null;
            return self.st.columnText(0);
        }

        pub fn deinit(self: *TermIterator) void {
            self.st.finalize();
        }
    };

    pub fn vocabIterator(idx: *Index, world: []const u8) db.Error!TermIterator {
        var st = try idx.handle.prepare(
            "SELECT term FROM term_stats WHERE world = ?1;",
        );
        errdefer st.finalize();
        try st.bindText(1, world);
        return .{ .st = st };
    }

    /// One posting joined with its episode metadata. Slices borrow
    /// statement memory and die on the next call.
    pub const PostingRow = struct {
        episode_id: []const u8,
        digest_hex: []const u8,
        rel_path: []const u8,
        scope: []const u8,
        lane: contracts.Lane,
        capture_policy: []const u8,
        event_time_ms: u64,
        body_line: u32,
        line_no: u32,
    };

    pub const PostingIterator = struct {
        st: db.Stmt,

        pub fn next(self: *PostingIterator) db.Error!?PostingRow {
            if (!try self.st.step()) return null;
            // Stored values outside their write-side bounds mean the
            // database was tampered with or damaged: reject it as Corrupt
            // rather than misreading (or panicking on) the row.
            return .{
                .episode_id = self.st.columnText(0),
                .line_no = std.math.cast(u32, self.st.columnInt64(1)) orelse return error.Corrupt,
                .digest_hex = self.st.columnText(2),
                .rel_path = self.st.columnText(3),
                .scope = self.st.columnText(4),
                .lane = std.meta.stringToEnum(contracts.Lane, self.st.columnText(5)) orelse return error.Corrupt,
                .capture_policy = self.st.columnText(6),
                .event_time_ms = @intCast(@max(0, self.st.columnInt64(7))),
                .body_line = std.math.cast(u32, self.st.columnInt64(8)) orelse return error.Corrupt,
            };
        }

        pub fn deinit(self: *PostingIterator) void {
            self.st.finalize();
        }
    };

    /// All postings for one vocabulary token, filtered by world, optional
    /// scope, and an explicit lane set. Lane tags come from the closed enum,
    /// so baking them into the SQL text is injection-safe.
    pub fn postingsForTerm(
        idx: *Index,
        gpa: std.mem.Allocator,
        term: []const u8,
        world: []const u8,
        scope: ?[]const u8,
        lanes: []const contracts.Lane,
    ) (db.Error || error{OutOfMemory})!PostingIterator {
        var sql: std.ArrayList(u8) = .empty;
        defer sql.deinit(gpa);
        try sql.appendSlice(gpa,
            \\SELECT p.episode_id, p.line_no, e.digest_hex, e.rel_path, e.scope,
            \\       e.lane, e.capture_policy, e.event_time_ms, e.body_line
            \\FROM postings p JOIN episodes e ON e.episode_id = p.episode_id
            \\WHERE p.term = ?1 AND e.world = ?2
        );
        if (scope != null) try sql.appendSlice(gpa, " AND e.scope = ?3");
        try sql.appendSlice(gpa, " AND e.lane IN (");
        for (lanes, 0..) |lane, i| {
            if (i > 0) try sql.appendSlice(gpa, ",");
            try sql.print(gpa, "'{s}'", .{@tagName(lane)});
        }
        try sql.appendSlice(gpa, ");");
        try sql.append(gpa, 0);

        var st = try idx.handle.prepare(sql.items[0 .. sql.items.len - 1 :0]);
        errdefer st.finalize();
        try st.bindText(1, term);
        try st.bindText(2, world);
        if (scope) |s| try st.bindText(3, s);
        return .{ .st = st };
    }

    /// Looks up one episode row by ID; all slices are duped with `gpa` and
    /// owned by the caller.
    pub const EpisodeRow = struct {
        digest_hex: []u8,
        rel_path: []u8,
        world: []u8,
        scope: []u8,
        lane: contracts.Lane,
        capture_policy: []u8,
        event_time_ms: u64,
        body_line: u32,

        pub fn deinit(self: *const EpisodeRow, gpa: std.mem.Allocator) void {
            gpa.free(self.digest_hex);
            gpa.free(self.rel_path);
            gpa.free(self.world);
            gpa.free(self.scope);
            gpa.free(self.capture_policy);
        }
    };

    pub fn lookupEpisode(
        idx: *Index,
        gpa: std.mem.Allocator,
        episode_id: []const u8,
    ) (db.Error || error{OutOfMemory})!?EpisodeRow {
        var st = try idx.handle.prepare(
            \\SELECT digest_hex, rel_path, world, scope, lane, capture_policy,
            \\       event_time_ms, body_line
            \\FROM episodes WHERE episode_id = ?1;
        );
        defer st.finalize();
        try st.bindText(1, episode_id);
        if (!try st.step()) return null;
        const digest_hex = try gpa.dupe(u8, st.columnText(0));
        errdefer gpa.free(digest_hex);
        const rel_path = try gpa.dupe(u8, st.columnText(1));
        errdefer gpa.free(rel_path);
        const world = try gpa.dupe(u8, st.columnText(2));
        errdefer gpa.free(world);
        const scope = try gpa.dupe(u8, st.columnText(3));
        errdefer gpa.free(scope);
        // Same contract as `PostingIterator.next`: out-of-bound stored
        // values reject the row as Corrupt instead of being misread.
        const lane = std.meta.stringToEnum(contracts.Lane, st.columnText(4)) orelse return error.Corrupt;
        const capture_policy = try gpa.dupe(u8, st.columnText(5));
        errdefer gpa.free(capture_policy);
        return .{
            .digest_hex = digest_hex,
            .rel_path = rel_path,
            .world = world,
            .scope = scope,
            .lane = lane,
            .capture_policy = capture_policy,
            .event_time_ms = @intCast(@max(0, st.columnInt64(6))),
            .body_line = std.math.cast(u32, st.columnInt64(7)) orelse return error.Corrupt,
        };
    }

    pub const SyncReport = struct {
        indexed: u64 = 0,
        removed: u64 = 0,
        skipped_malformed: u64 = 0,
        duplicate_ids: u64 = 0,
    };

    /// Rebuilds the projection to match the corpus: every parseable episode
    /// file is fully reindexed (row, postings, stats), rows whose files are
    /// gone are removed, malformed files are counted and excluded. One
    /// transaction; a torn sync never leaves a half-projection.
    pub fn syncFromCorpus(
        idx: *Index,
        root: Io.Dir,
        io: Io,
        gpa: std.mem.Allocator,
    ) (db.Error || error{Unavailable})!SyncReport {
        var arena_state = std.heap.ArenaAllocator.init(gpa);
        defer arena_state.deinit();
        const arena = arena_state.allocator();

        var report: SyncReport = .{};
        var seen: std.StringHashMapUnmanaged(void) = .empty;

        try idx.handle.exec("BEGIN IMMEDIATE;");
        errdefer idx.handle.exec("ROLLBACK;") catch {};

        var corpus = root.openDir(io, ".", .{
            .follow_symlinks = false,
            .iterate = true,
        }) catch |err| switch (err) {
            error.FileNotFound => {
                // Empty corpus: the whole projection becomes empty too.
                try idx.handle.exec("DELETE FROM postings;");
                try idx.handle.exec("DELETE FROM term_stats;");
                try idx.handle.exec("DELETE FROM episodes;");
                try idx.handle.exec("COMMIT;");
                return report;
            },
            else => return error.Unavailable,
        };
        defer corpus.close(io);

        var path: std.ArrayList(u8) = .empty;
        defer path.deinit(gpa);
        try idx.walkDir(
            corpus,
            io,
            gpa,
            arena,
            &path,
            contracts.corpus_walk_depth,
            &seen,
            &report,
        );

        // Remove rows (and their postings/stats) whose source files are gone.
        var to_remove: std.ArrayList([]const u8) = .empty;
        defer to_remove.deinit(gpa);
        {
            var st = try idx.handle.prepare("SELECT episode_id FROM episodes;");
            defer st.finalize();
            while (try st.step()) {
                const id = st.columnText(0);
                if (!seen.contains(id)) {
                    try to_remove.append(gpa, try arena.dupe(u8, id));
                }
            }
        }
        {
            var st = try idx.handle.prepare("DELETE FROM episodes WHERE episode_id = ?1;");
            defer st.finalize();
            for (to_remove.items) |id| {
                try idx.deindexEpisodeInTx(id);
                try st.reset();
                try st.bindText(1, id);
                _ = try st.step();
                report.removed += 1;
            }
        }

        try idx.handle.exec("COMMIT;");
        return report;
    }

    fn walkDir(
        idx: *Index,
        dir: Io.Dir,
        io: Io,
        gpa: std.mem.Allocator,
        arena: std.mem.Allocator,
        path: *std.ArrayList(u8),
        depth_left: u8,
        seen: *std.StringHashMapUnmanaged(void),
        report: *SyncReport,
    ) (db.Error || error{Unavailable})!void {
        var it = dir.iterate();
        while (it.next(io) catch return error.Unavailable) |entry| {
            switch (entry.kind) {
                .file => {
                    if (!std.mem.startsWith(u8, entry.name, "aj1-") or
                        !std.mem.endsWith(u8, entry.name, ".md")) continue;
                    // Permission repair is best effort: a foreign-owned file
                    // that cannot be hardened is still valid memory.
                    dir.setFilePermissions(
                        io,
                        entry.name,
                        @enumFromInt(0o600),
                        .{ .follow_symlinks = false },
                    ) catch {};
                    const content = dir.readFileAlloc(
                        io,
                        entry.name,
                        gpa,
                        .limited(contracts.max_episode_file_bytes),
                    ) catch {
                        report.skipped_malformed += 1;
                        continue;
                    };
                    defer gpa.free(content);
                    const ep = frontmatter.parse(content) orelse {
                        report.skipped_malformed += 1;
                        continue;
                    };
                    // Deduplicate by identity: the first copy encountered
                    // stays indexed, later copies are counted and skipped.
                    // After manual corpus surgery, `sync` rebaselines.
                    if (seen.contains(ep.episode_id)) {
                        report.duplicate_ids += 1;
                        continue;
                    }
                    const rel_path = if (path.items.len == 0)
                        try arena.dupe(u8, entry.name)
                    else
                        try std.fmt.allocPrint(arena, "{s}/{s}", .{ path.items, entry.name });
                    idx.indexEpisodeInTx(rel_path, content) catch |err| switch (err) {
                        error.Malformed => {
                            report.skipped_malformed += 1;
                            continue;
                        },
                        else => |db_err| return db_err,
                    };
                    const id_key = try arena.dupe(u8, ep.episode_id);
                    try seen.put(arena, id_key, {});
                    report.indexed += 1;
                },
                .directory => {
                    // Dot-directories (`.git`, `.obsidian`, …) are foreign
                    // tooling state, never episode shards: skip them.
                    if (entry.name.len == 0 or entry.name[0] == '.') continue;
                    if (depth_left == 0) continue;
                    var child = dir.openDir(io, entry.name, .{
                        .follow_symlinks = false,
                        .iterate = true,
                    }) catch continue;
                    defer child.close(io);
                    child.setPermissions(io, @enumFromInt(0o700)) catch {};
                    const prev_len = path.items.len;
                    if (path.items.len > 0) try path.append(gpa, '/');
                    try path.appendSlice(gpa, entry.name);
                    defer path.items.len = prev_len;
                    try idx.walkDir(
                        child,
                        io,
                        gpa,
                        arena,
                        path,
                        depth_left - 1,
                        seen,
                        report,
                    );
                },
                else => {},
            }
        }
    }
};
