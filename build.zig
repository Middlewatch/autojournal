const std = @import("std");

pub fn build(b: *std.Build) void {
    const target = b.standardTargetOptions(.{});
    const optimize = b.standardOptimizeOption(.{});
    const strip = b.option(bool, "strip", "Omit debug info from the binary") orelse false;

    const root_mod = b.addModule("autojournal", .{
        .root_source_file = b.path("src/root.zig"),
        .target = target,
        .optimize = optimize,
        .link_libc = true,
    });
    root_mod.addIncludePath(b.path("vendor/sqlite"));
    root_mod.addCSourceFile(.{
        .file = b.path("vendor/sqlite/sqlite3.c"),
        .flags = &.{
            "-DSQLITE_THREADSAFE=1",
            "-DSQLITE_ENABLE_FTS5",
            "-DSQLITE_OMIT_LOAD_EXTENSION",
            "-DSQLITE_OMIT_DEPRECATED",
        },
    });

    const exe_mod = b.createModule(.{
        .root_source_file = b.path("src/main.zig"),
        .target = target,
        .optimize = optimize,
        .strip = strip,
        .imports = &.{
            .{ .name = "autojournal", .module = root_mod },
        },
    });
    const exe = b.addExecutable(.{
        .name = "autojournal",
        .root_module = exe_mod,
    });
    b.installArtifact(exe);

    const unit_tests = b.addTest(.{ .root_module = root_mod });
    const run_unit_tests = b.addRunArtifact(unit_tests);
    const cli_tests = b.addTest(.{ .root_module = exe_mod });
    const run_cli_tests = b.addRunArtifact(cli_tests);
    const test_step = b.step("test", "Run AutoJournal unit tests");
    test_step.dependOn(&run_unit_tests.step);
    test_step.dependOn(&run_cli_tests.step);
}
