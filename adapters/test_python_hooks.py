#!/usr/bin/env python3
"""Tests for the two standalone Python capture hooks.

These hooks are what every harness without a native integration uses, so
they get the same treatment as the Pi adapter: run the real entry point
against a fake `autojournal` binary and assert on the payload it would
have published.

Run directly (`python3 adapters/test_python_hooks.py`) or through
scripts/verify.sh. Standard library only, matching the hooks themselves.
"""

import importlib.util
import json
import os
import pathlib
import socket
import stat
import sys
import tempfile
import threading
import unittest
import unittest.mock
from io import StringIO

ADAPTERS = pathlib.Path(__file__).resolve().parent


def load(name: str, relative: str):
    spec = importlib.util.spec_from_file_location(name, ADAPTERS / relative)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


cc = load("aj_cc_hook", "claude-code/autojournal_capture.py")
codex = load("aj_codex_hook", "codex/autojournal_capture.py")


class HookHarness(unittest.TestCase):
    """Gives each test a scratch dir, a fake binary that records the
    payload it was handed, and clean hook-related environment."""

    def setUp(self):
        self.tmp = pathlib.Path(tempfile.mkdtemp())
        self.addCleanup(lambda: __import__("shutil").rmtree(self.tmp, ignore_errors=True))
        self.captured = self.tmp / "captured.json"
        self.binary = self.tmp / "fake-autojournal"
        self.binary.write_text(
            "#!/usr/bin/env python3\n"
            "import os, sys\n"
            "open(os.environ['FAKE_AJ_OUT'], 'w').write(sys.stdin.read())\n"
        )
        self.binary.chmod(self.binary.stat().st_mode | stat.S_IEXEC)

        self._env = dict(os.environ)
        self.addCleanup(lambda: (os.environ.clear(), os.environ.update(self._env)))
        os.environ["AUTOJOURNAL_BIN"] = str(self.binary)
        os.environ["FAKE_AJ_OUT"] = str(self.captured)
        # Belt and braces: if binary resolution ever fell through to a real
        # install, these keep it writing into scratch instead of the owner's
        # journal. A test must not be able to publish real memory.
        os.environ["AUTOJOURNAL_HOOK_ROOT"] = str(self.tmp / "journal")
        os.environ["AUTOJOURNAL_HOOK_INDEX"] = str(self.tmp / "index.sqlite")
        os.environ["XDG_STATE_HOME"] = str(self.tmp / "state")

    def run_hook(self, module, payload):
        stdin = sys.stdin
        sys.stdin = StringIO(json.dumps(payload))
        try:
            self.assertEqual(module.main(), 0)
        finally:
            sys.stdin = stdin
        if not self.captured.exists():
            return None
        return json.loads(self.captured.read_text())

    def transcript(self, prompt, reply):
        path = self.tmp / "transcript.jsonl"
        path.write_text(
            json.dumps(
                {
                    "type": "user",
                    "uuid": "turn-1",
                    "timestamp": "2026-08-05T12:00:00Z",
                    "message": {"content": prompt},
                }
            )
            + "\n"
            + json.dumps(
                {
                    "type": "assistant",
                    "message": {
                        "stop_reason": "end_turn",
                        "content": [{"type": "text", "text": reply}],
                    },
                }
            )
            + "\n"
        )
        return str(path)


class ClaudeCodeHook(HookHarness):
    def test_captures_a_session_outside_the_home_directory(self):
        # Regression: capture was gated on the cwd living under one
        # hard-coded directory, so every other install silently never
        # captured anything.
        published = self.run_hook(
            cc,
            {
                "session_id": "s1",
                "transcript_path": self.transcript("what broke the build?", "a stale lockfile"),
                "cwd": "/srv/someone-elses-checkout",
            },
        )
        self.assertIsNotNone(published, "no payload was published")
        self.assertEqual(published["user_content"], "what broke the build?")
        self.assertEqual(published["assistant_result"], "a stale lockfile")
        self.assertEqual(published["harness"], "claude-code")
        self.assertEqual(published["workspace_root"], "/srv/someone-elses-checkout")

    def test_waits_for_terminal_text_and_excludes_tool_arguments(self):
        path = pathlib.Path(self.transcript("explain the result", "placeholder"))
        lines = path.read_text().splitlines()
        path.write_text(
            lines[0]
            + "\n"
            + json.dumps(
                {
                    "type": "assistant",
                    "message": {
                        "stop_reason": "tool_use",
                        "content": [
                            {"type": "text", "text": "I will inspect it."},
                            {
                                "type": "tool_use",
                                "name": "Bash",
                                "input": {"command": "echo secret-tool-input"},
                            },
                        ],
                    },
                }
            )
            + "\n"
            + json.dumps(
                {
                    "type": "assistant",
                    "message": {
                        "stop_reason": "end_turn",
                        "content": [{"type": "thinking", "thinking": ""}],
                    },
                }
            )
            + "\n"
        )

        def append_terminal():
            with path.open("a") as transcript:
                transcript.write(
                    json.dumps(
                        {
                            "type": "assistant",
                            "message": {
                                "stop_reason": "end_turn",
                                "content": [
                                    {"type": "text", "text": "The final answer."}
                                ],
                            },
                        }
                    )
                    + "\n"
                )

        timer = threading.Timer(0.05, append_terminal)
        timer.start()
        self.addCleanup(timer.cancel)
        published = self.run_hook(
            cc,
            {
                "session_id": "s1",
                "transcript_path": str(path),
                "cwd": str(self.tmp),
            },
        )
        timer.join()

        self.assertIsNotNone(published, "settled turn was not published")
        self.assertEqual(published["assistant_result"], "The final answer.")
        self.assertEqual(published["tools"], [{"name": "Bash"}])
        self.assertNotIn("secret-tool-input", json.dumps(published))
        self.assertEqual(published["capture_policy"], "cc-stop.v3")

    def test_quiet_period_selects_the_last_terminal_record(self):
        path = pathlib.Path(self.transcript("settle this", "first terminal draft"))

        def append_settled_terminal():
            with path.open("a") as transcript:
                transcript.write(
                    json.dumps(
                        {
                            "type": "assistant",
                            "message": {
                                "stop_reason": "end_turn",
                                "content": [
                                    {"type": "text", "text": "settled final response"}
                                ],
                            },
                        }
                    )
                    + "\n"
                )

        timer = threading.Timer(0.05, append_settled_terminal)
        timer.start()
        self.addCleanup(timer.cancel)
        published = self.run_hook(
            cc,
            {
                "session_id": "s1",
                "transcript_path": str(path),
                "cwd": str(self.tmp),
            },
        )
        timer.join()

        self.assertEqual(published["assistant_result"], "settled final response")

    def test_user_without_uuid_uses_the_stable_index_fallback(self):
        path = pathlib.Path(self.transcript("uuid-free turn", "complete response"))
        entries = [json.loads(line) for line in path.read_text().splitlines()]
        del entries[0]["uuid"]
        path.write_text("\n".join(json.dumps(entry) for entry in entries) + "\n")

        published = self.run_hook(
            cc,
            {
                "session_id": "s1",
                "transcript_path": str(path),
                "cwd": str(self.tmp),
            },
        )

        self.assertEqual(published["turn_id"], "idx0")
        self.assertEqual(published["assistant_result"], "complete response")

    def test_invalid_tool_name_is_dropped_without_losing_the_turn(self):
        path = pathlib.Path(self.transcript("use a tool", "complete response"))
        entries = [json.loads(line) for line in path.read_text().splitlines()]
        entries.insert(
            1,
            {
                "type": "assistant",
                "message": {
                    "stop_reason": "tool_use",
                    "content": [
                        {"type": "tool_use", "name": "bad tool name", "input": {}}
                    ],
                },
            },
        )
        path.write_text("\n".join(json.dumps(entry) for entry in entries) + "\n")

        published = self.run_hook(
            cc,
            {
                "session_id": "s1",
                "transcript_path": str(path),
                "cwd": str(self.tmp),
            },
        )

        self.assertEqual(published["assistant_result"], "complete response")
        self.assertEqual(published["tools"], [])

    def test_skips_a_turn_that_never_gains_terminal_text(self):
        path = pathlib.Path(self.transcript("unfinished", "placeholder"))
        entries = [json.loads(line) for line in path.read_text().splitlines()]
        entries[1]["message"] = {
            "stop_reason": "tool_use",
            "content": [{"type": "tool_use", "name": "Read", "input": {}}],
        }
        path.write_text("\n".join(json.dumps(entry) for entry in entries) + "\n")

        with unittest.mock.patch.object(cc, "TRANSCRIPT_SETTLE_TIMEOUT_S", 0):
            self.assertIsNone(
                self.run_hook(
                    cc,
                    {
                        "session_id": "s1",
                        "transcript_path": str(path),
                        "cwd": str(self.tmp),
                    },
                )
            )

    def test_skips_a_machine_driven_turn(self):
        prompt = "<system-reminder>budget warning</system-reminder>"
        self.assertIsNone(
            self.run_hook(
                cc,
                {
                    "session_id": "s1",
                    "transcript_path": self.transcript(prompt, "noted"),
                    "cwd": str(self.tmp),
                },
            )
        )

    def test_no_binary_anywhere_is_a_silent_no_op(self):
        os.environ["AUTOJOURNAL_BIN"] = str(self.tmp / "does-not-exist")
        self.assertIsNone(
            self.run_hook(
                cc,
                {
                    "session_id": "s1",
                    "transcript_path": self.transcript("hello there", "hi"),
                    "cwd": str(self.tmp),
                },
            )
        )

    def test_omits_an_unusable_workspace_root(self):
        published = self.run_hook(
            cc,
            {
                "session_id": "s1",
                "transcript_path": self.transcript("still capture this", "sure"),
                "cwd": "/bad\x01path",
            },
        )
        self.assertIsNotNone(published, "a bad cwd must not lose the turn")
        self.assertNotIn("workspace_root", published)

    def test_labels_the_originating_machine(self):
        published = self.run_hook(
            cc,
            {
                "session_id": "s1",
                "transcript_path": self.transcript("which box was this?", "this one"),
                "cwd": str(self.tmp),
            },
        )
        self.assertIsNotNone(published, "no payload was published")
        self.assertEqual(published["host"], socket.gethostname().split(".")[0])


class CodexHook(HookHarness):
    def stash_and_stop(self, cwd):
        self.run_hook(
            codex,
            {
                "hook_event_name": "UserPromptSubmit",
                "session_id": "s2",
                "turn_id": "t1",
                "prompt": "where did the deploy fail?",
                "cwd": cwd,
            },
        )
        return self.run_hook(
            codex,
            {
                "hook_event_name": "Stop",
                "session_id": "s2",
                "turn_id": "t1",
                "last_assistant_message": "the arm64 job",
            },
        )

    def test_captures_a_session_outside_the_home_directory(self):
        published = self.stash_and_stop("/srv/someone-elses-checkout")
        self.assertIsNotNone(published, "no payload was published")
        self.assertEqual(published["user_content"], "where did the deploy fail?")
        self.assertEqual(published["assistant_result"], "the arm64 job")
        self.assertEqual(published["harness"], "codex")
        self.assertEqual(published["workspace_root"], "/srv/someone-elses-checkout")

    def test_pending_file_is_cleared_after_publication(self):
        self.stash_and_stop(str(self.tmp))
        pending = codex.pending_path({"session_id": "s2", "turn_id": "t1"})
        self.assertFalse(pending.exists(), "pending prompt outlived its capture")

    def test_pending_file_survives_a_missing_binary(self):
        # The turn is recoverable on a later run rather than dropped.
        self.run_hook(
            codex,
            {
                "hook_event_name": "UserPromptSubmit",
                "session_id": "s3",
                "turn_id": "t1",
                "prompt": "keep this prompt safe",
                "cwd": str(self.tmp),
            },
        )
        os.environ["AUTOJOURNAL_BIN"] = str(self.tmp / "does-not-exist")
        self.run_hook(
            codex,
            {
                "hook_event_name": "Stop",
                "session_id": "s3",
                "turn_id": "t1",
                "last_assistant_message": "ok",
            },
        )
        pending = codex.pending_path({"session_id": "s3", "turn_id": "t1"})
        self.assertTrue(pending.exists(), "pending prompt was discarded unpublished")

    def test_labels_the_originating_machine(self):
        published = self.stash_and_stop(str(self.tmp))
        self.assertIsNotNone(published, "no payload was published")
        self.assertEqual(published["host"], socket.gethostname().split(".")[0])


class BinaryResolution(HookHarness):
    def test_explicit_override_wins(self):
        self.assertEqual(cc.resolve_binary(), str(self.binary))
        self.assertEqual(codex.resolve_binary(), str(self.binary))

    def test_non_executable_override_resolves_to_nothing(self):
        plain = self.tmp / "not-executable"
        plain.write_text("")
        os.environ["AUTOJOURNAL_BIN"] = str(plain)
        self.assertIsNone(cc.resolve_binary())

    def test_falls_back_to_path(self):
        del os.environ["AUTOJOURNAL_BIN"]
        on_path = self.tmp / "autojournal"
        on_path.write_text("#!/bin/sh\n")
        on_path.chmod(on_path.stat().st_mode | stat.S_IEXEC)
        os.environ["PATH"] = str(self.tmp)
        # Only meaningful when the conventional install is absent, which is
        # the situation a fresh non-owner install is actually in.
        if not os.access(pathlib.Path.home() / ".local" / "bin" / "autojournal", os.X_OK):
            self.assertEqual(cc.resolve_binary(), str(on_path))


class WorkspaceRootContract(unittest.TestCase):
    def test_matches_the_payload_path_rule(self):
        for module in (cc, codex):
            self.assertEqual(module.workspace_root("/home/x/proj"), "/home/x/proj")
            self.assertIsNone(module.workspace_root(""))
            self.assertIsNone(module.workspace_root("/bad\x00path"))
            self.assertIsNone(module.workspace_root("/x" + "y" * 512))


class OriginHostContract(unittest.TestCase):
    """The host label is provenance, so a hostname the payload contract
    would reject must cost the field, never the turn."""

    def test_reports_the_short_hostname(self):
        for module in (cc, codex):
            with unittest.mock.patch.object(
                module.socket, "gethostname", return_value="stealth.tail8255b9.ts.net"
            ):
                self.assertEqual(module.origin_host(), "stealth")

    def test_omits_a_hostname_the_contract_would_reject(self):
        for module in (cc, codex):
            for bad in ("", "   ", "two words", "bad\x01name", "x" * 129):
                with unittest.mock.patch.object(
                    module.socket, "gethostname", return_value=bad
                ):
                    self.assertIsNone(module.origin_host(), f"accepted {bad!r}")

    def test_survives_an_unreadable_hostname(self):
        for module in (cc, codex):
            with unittest.mock.patch.object(
                module.socket, "gethostname", side_effect=OSError("no hostname")
            ):
                self.assertIsNone(module.origin_host())


if __name__ == "__main__":
    unittest.main(verbosity=2)
