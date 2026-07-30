//! AutoJournal core package.
//!
//! One Zig package, two hosts: Evoker imports this module directly for its
//! built-in memory integration; the standalone binary (`src/main.zig`) wraps
//! the same module as helper, owner CLI, and hook target. All product rules
//! live here — hosts translate lifecycle events and register tools, nothing
//! more.

const std = @import("std");

pub const contracts = @import("contracts.zig");
pub const identity = @import("identity.zig");
pub const render = @import("render.zig");
pub const store = @import("store.zig");
pub const config = @import("config.zig");
pub const paths = @import("paths.zig");
pub const db = @import("db.zig");
pub const frontmatter = @import("frontmatter.zig");
pub const index = @import("index.zig");
pub const retrieval = @import("retrieval.zig");
pub const aliases = @import("aliases.zig");
pub const search = @import("search.zig");
pub const ops = @import("ops.zig");

pub const package_version = "0.1.0";

test {
    _ = contracts;
    _ = identity;
    _ = render;
    _ = store;
    _ = config;
    _ = paths;
    _ = db;
    _ = frontmatter;
    _ = index;
    _ = retrieval;
    _ = aliases;
    _ = search;
    _ = ops;
    _ = @import("store_test.zig");
    _ = @import("search_test.zig");
    _ = @import("index_test.zig");
}
