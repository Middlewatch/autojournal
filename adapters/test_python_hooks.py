#!/usr/bin/env python3
"""Tests for the two standalone Python capture hooks.

These hooks are what every harness without a native integration uses, so
they get the same treatment as the Pi adapter: run the real entry point
against a fake `autojournal` binary and assert on the payload it would
have published.

Run directly (`python3 adapters/test_python_hooks.py`) or through
scripts/verify.sh. Standard library only, matching the hooks themselves.
"""

import contextlib
import importlib.util
import json
import os
import pathlib
import shutil
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
repair = load("aj_repair_corpus", "../scripts/repair-corpus.py")


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
        self.assertEqual(published["capture_policy"], "cc-stop.v4")

    def test_quiet_period_accumulates_a_late_terminal_record(self):
        # cc-stop.v4: the body carries every terminal response in order, so a
        # terminal record appended while Stop is settling extends the body
        # rather than replacing it (v3 kept only the last one).
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

        self.assertEqual(
            published["assistant_result"],
            "first terminal draft\n\nsettled final response",
        )

    def test_slash_command_journals_the_typed_argument(self):
        envelope = {
            "type": "user",
            "uuid": "turn-1",
            "timestamp": "2026-08-05T12:00:00Z",
            "message": {
                "content": (
                    "<command-message>review</command-message>\n"
                    "<command-name>/review</command-name>\n"
                    "<command-args>check the retry loop for lost writes</command-args>"
                )
            },
        }
        expanded_body = {
            "type": "user",
            "isMeta": True,
            "message": {
                "content": [
                    {"type": "text", "text": "Full expanded skill body with instructions."}
                ]
            },
        }
        reply = {
            "type": "assistant",
            "message": {
                "stop_reason": "end_turn",
                "content": [{"type": "text", "text": "the loop is fine"}],
            },
        }
        path = self.tmp / "transcript.jsonl"
        path.write_text(
            "".join(json.dumps(e) + "\n" for e in (envelope, expanded_body, reply))
        )
        published = self.run_hook(
            cc,
            {"session_id": "s1", "transcript_path": str(path), "cwd": str(self.tmp)},
        )
        self.assertIsNotNone(published, "slash-command turn was not captured")
        self.assertEqual(published["user_content"], "check the retry loop for lost writes")
        self.assertNotIn("expanded skill body", json.dumps(published))
        self.assertEqual(published["assistant_result"], "the loop is fine")

    def test_a_prompt_quoting_envelope_tags_is_owner_typed(self):
        # Classification anchors on the head of the entry text: a prompt
        # that quotes the envelope tags mid-sentence is a question about
        # the markup, and the whole typed sentence is the prompt — not the
        # quoted example text, and not a skipped turn.
        quoting = {
            "type": "user",
            "uuid": "turn-quote",
            "timestamp": "2026-08-05T12:00:00Z",
            "message": {
                "content": (
                    "How should the projection treat a "
                    "<command-name>/plan</command-name> entry whose "
                    "<command-args>example words</command-args> block is quoted?"
                )
            },
        }
        reply = {
            "type": "assistant",
            "message": {
                "stop_reason": "end_turn",
                "content": [{"type": "text", "text": "as owner-typed text"}],
            },
        }
        path = self.tmp / "transcript.jsonl"
        path.write_text(
            "".join(json.dumps(e) + "\n" for e in (quoting, reply))
        )
        published = self.run_hook(
            cc,
            {"session_id": "s1", "transcript_path": str(path), "cwd": str(self.tmp)},
        )
        self.assertIsNotNone(published, "quoting turn was not captured")
        self.assertEqual(
            published["user_content"],
            quoting["message"]["content"],
        )
        self.assertEqual(published["assistant_result"], "as owner-typed text")

    def test_a_bare_envelope_journals_the_command_name(self):
        # An envelope with empty <command-args> is
        # still an owner-typed instruction, and the command name itself is
        # the prompt the corpus records.
        envelope = {
            "type": "user",
            "uuid": "turn-bare",
            "timestamp": "2026-08-05T12:00:00Z",
            "message": {
                "content": (
                    "<command-message>plan</command-message>\n"
                    "<command-name>/plan</command-name>\n"
                    "<command-args></command-args>"
                )
            },
        }
        reply = {
            "type": "assistant",
            "message": {
                "stop_reason": "end_turn",
                "content": [{"type": "text", "text": "drafted the plan skeleton"}],
            },
        }
        path = self.tmp / "transcript.jsonl"
        path.write_text(
            "".join(json.dumps(e) + "\n" for e in (envelope, reply))
        )
        published = self.run_hook(
            cc,
            {"session_id": "s1", "transcript_path": str(path), "cwd": str(self.tmp)},
        )
        self.assertIsNotNone(published, "bare envelope turn was not captured")
        self.assertEqual(published["user_content"], "/plan")
        self.assertEqual(published["assistant_result"], "drafted the plan skeleton")

    def test_machine_injected_entry_never_starts_a_turn(self):
        entries = [
            {
                "type": "user",
                "uuid": "turn-1",
                "timestamp": "2026-08-05T12:00:00Z",
                "message": {"content": "summarize the failures"},
            },
            {
                "type": "assistant",
                "message": {
                    "stop_reason": "end_turn",
                    "content": [{"type": "text", "text": "two suites failed"}],
                },
            },
            {
                "type": "user",
                "uuid": "notif-1",
                "message": {
                    "content": "<task-notification>background agent finished</task-notification>"
                },
            },
            {
                "type": "assistant",
                "message": {
                    "stop_reason": "end_turn",
                    "content": [{"type": "text", "text": "the third suite passed on retry"}],
                },
            },
        ]
        path = self.tmp / "transcript.jsonl"
        path.write_text("".join(json.dumps(e) + "\n" for e in entries))
        published = self.run_hook(
            cc,
            {"session_id": "s1", "transcript_path": str(path), "cwd": str(self.tmp)},
        )
        self.assertIsNotNone(published, "resumed turn was not captured")
        # The notification neither became the prompt nor broke the window:
        # the turn belongs to the owner's entry and keeps both halves.
        self.assertEqual(published["turn_id"], "turn-1")
        self.assertEqual(published["user_content"], "summarize the failures")
        self.assertEqual(
            published["assistant_result"],
            "two suites failed\n\nthe third suite passed on retry",
        )

    def test_body_accumulates_terminal_responses_in_order(self):
        entries = [
            {
                "type": "user",
                "uuid": "turn-1",
                "timestamp": "2026-08-05T12:00:00Z",
                "message": {"content": "walk me through the release"},
            },
            {
                "type": "assistant",
                "message": {
                    "stop_reason": "end_turn",
                    "content": [{"type": "text", "text": "first: date the changelog"}],
                },
            },
            {
                "type": "user",
                "message": {"content": "<system-reminder>tick</system-reminder>"},
            },
            {
                "type": "assistant",
                "message": {
                    "stop_reason": "end_turn",
                    "content": [{"type": "text", "text": "second: run the gate"}],
                },
            },
            {
                "type": "user",
                "message": {"content": [{"type": "tool_result", "content": "ok"}]},
            },
            {
                "type": "assistant",
                "message": {
                    "stop_reason": "end_turn",
                    "content": [{"type": "text", "text": "third: publish and tag"}],
                },
            },
        ]
        path = self.tmp / "transcript.jsonl"
        path.write_text("".join(json.dumps(e) + "\n" for e in entries))
        published = self.run_hook(
            cc,
            {"session_id": "s1", "transcript_path": str(path), "cwd": str(self.tmp)},
        )
        self.assertIsNotNone(published, "accumulating turn was not captured")
        self.assertEqual(
            published["assistant_result"],
            "first: date the changelog\n\n"
            "second: run the gate\n\n"
            "third: publish and tag",
        )

    def test_progress_prose_and_tool_summaries_stay_excluded(self):
        entries = [
            {
                "type": "user",
                "uuid": "turn-1",
                "timestamp": "2026-08-05T12:00:00Z",
                "message": {"content": "check the lockfile"},
            },
            {
                "type": "assistant",
                "message": {
                    "stop_reason": "tool_use",
                    "content": [
                        {"type": "text", "text": "Let me look at the lockfile now."},
                        {
                            "type": "tool_use",
                            "name": "Read",
                            "input": {"file_path": "/private/lockfile-contents"},
                        },
                    ],
                },
            },
            {
                "type": "user",
                "message": {"content": [{"type": "tool_result", "content": "bytes"}]},
            },
            {
                "type": "assistant",
                "message": {
                    "stop_reason": "end_turn",
                    "content": [{"type": "text", "text": "the lockfile is stale"}],
                },
            },
        ]
        path = self.tmp / "transcript.jsonl"
        path.write_text("".join(json.dumps(e) + "\n" for e in entries))
        published = self.run_hook(
            cc,
            {"session_id": "s1", "transcript_path": str(path), "cwd": str(self.tmp)},
        )
        self.assertIsNotNone(published, "turn was not captured")
        self.assertEqual(published["assistant_result"], "the lockfile is stale")
        self.assertNotIn("look at the lockfile now", json.dumps(published))
        self.assertEqual(published["tools"], [{"name": "Read"}])
        self.assertNotIn("lockfile-contents", json.dumps(published))

    def test_redelivery_extends_rather_than_diverges(self):
        # The append-only join is what makes the store's containment decision
        # mechanical: a redelivery with more output must carry the first
        # delivery's body as a strict prefix, never a rewrite of it.
        path = pathlib.Path(self.transcript("keep going", "the first response"))
        first = self.run_hook(
            cc,
            {"session_id": "s1", "transcript_path": str(path), "cwd": str(self.tmp)},
        )
        self.assertIsNotNone(first, "first delivery was not captured")

        with path.open("a") as transcript:
            transcript.write(
                json.dumps(
                    {
                        "type": "user",
                        "message": {
                            "content": "<task-notification>done</task-notification>"
                        },
                    }
                )
                + "\n"
                + json.dumps(
                    {
                        "type": "assistant",
                        "message": {
                            "stop_reason": "end_turn",
                            "content": [{"type": "text", "text": "the resumed response"}],
                        },
                    }
                )
                + "\n"
            )
        self.captured.unlink()
        second = self.run_hook(
            cc,
            {"session_id": "s1", "transcript_path": str(path), "cwd": str(self.tmp)},
        )
        self.assertIsNotNone(second, "redelivery was not captured")
        self.assertEqual(second["turn_id"], first["turn_id"])
        self.assertTrue(
            second["assistant_result"].startswith(first["assistant_result"]),
            "redelivered body does not extend the first delivery",
        )
        self.assertGreater(
            len(second["assistant_result"]), len(first["assistant_result"])
        )

    def assert_projection_field(self, fixture, field, got, want):
        """Equality without echoing values: a mismatch names the fixture,
        the field and the first divergent byte, never fixture content."""
        if got == want:
            return
        offset = len(os.path.commonprefix([got, want]))
        self.fail(
            f"{fixture}: {field} diverges at byte {offset} "
            f"(projected {len(got)} bytes, expected {len(want)} bytes)"
        )

    def test_replays_every_recorded_transcript_fixture(self):
        corpus = ADAPTERS.parent / "testdata" / "transcripts"
        document = json.loads(
            (corpus / "EXPECTED.json").read_text(encoding="utf-8")
        )
        self.assertTrue(document["expectations"], "empty expectation table")
        with unittest.mock.patch.object(cc, "TRANSCRIPT_QUIET_INTERVAL_S", 0.0):
            for exp in document["expectations"]:
                with self.subTest(fixture=exp["fixture"]):
                    self.captured.unlink(missing_ok=True)
                    published = self.run_hook(
                        cc,
                        {
                            "session_id": "s1",
                            "transcript_path": str(corpus / exp["fixture"]),
                            "cwd": str(self.tmp),
                        },
                    )
                    if exp["skipped"]:
                        self.assertIsNone(
                            published, f"{exp['fixture']}: the hook must decline"
                        )
                        continue
                    self.assertIsNotNone(
                        published, f"{exp['fixture']}: no payload was published"
                    )
                    self.assert_projection_field(
                        exp["fixture"], "user_content",
                        published["user_content"], exp["user_content"],
                    )
                    self.assert_projection_field(
                        exp["fixture"], "assistant_result",
                        published["assistant_result"], exp["assistant_result"],
                    )
                    self.assertEqual(
                        [t["name"] for t in published["tools"]],
                        exp["tools"],
                        f"{exp['fixture']}: tool names disagree",
                    )

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

    def test_sidechain_entry_never_starts_a_turn(self):
        # A sidechain user entry is the model prompting a subagent. The
        # harness does not inline these today; if one ever appears, it must
        # not be adopted as the owner's turn.
        path = pathlib.Path(self.transcript("the owner question", "the owner answer"))
        with path.open("a") as transcript:
            transcript.write(
                json.dumps(
                    {
                        "type": "user",
                        "uuid": "side-1",
                        "isSidechain": True,
                        "message": {"content": "delegated subagent prompt"},
                    }
                )
                + "\n"
                + json.dumps(
                    {
                        "type": "assistant",
                        "isSidechain": True,
                        "message": {
                            "stop_reason": "end_turn",
                            "content": [{"type": "text", "text": "subagent findings"}],
                        },
                    }
                )
                + "\n"
            )
        published = self.run_hook(
            cc,
            {"session_id": "s1", "transcript_path": str(path), "cwd": str(self.tmp)},
        )
        self.assertIsNotNone(published, "owner turn was lost to a sidechain entry")
        self.assertEqual(published["turn_id"], "turn-1")
        self.assertEqual(published["user_content"], "the owner question")
        self.assertEqual(published["assistant_result"], "the owner answer")

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

    def test_pending_file_survives_a_failed_capture(self):
        self.run_hook(
            codex,
            {
                "hook_event_name": "UserPromptSubmit",
                "session_id": "s4",
                "turn_id": "t1",
                "prompt": "retry this capture",
                "cwd": str(self.tmp),
            },
        )
        self.binary.write_text("#!/bin/sh\nexit 7\n")
        self.run_hook(
            codex,
            {
                "hook_event_name": "Stop",
                "session_id": "s4",
                "turn_id": "t1",
                "last_assistant_message": "capture should remain pending",
            },
        )
        pending = codex.pending_path({"session_id": "s4", "turn_id": "t1"})
        self.assertTrue(pending.exists(), "failed capture discarded its pending prompt")

    def test_labels_the_originating_machine(self):
        published = self.stash_and_stop(str(self.tmp))
        self.assertIsNotNone(published, "no payload was published")
        self.assertEqual(published["host"], socket.gethostname().split(".")[0])

    def test_codex_pending_file_is_owner_only(self):
        # The pending file holds the owner's verbatim prompt between hooks;
        # nothing but the owner may read it, whatever the process umask says.
        self.run_hook(
            codex,
            {
                "hook_event_name": "UserPromptSubmit",
                "session_id": "s5",
                "turn_id": "t1",
                "prompt": "a prompt nobody else should read",
                "cwd": str(self.tmp),
            },
        )
        pending = codex.pending_path({"session_id": "s5", "turn_id": "t1"})
        self.assertTrue(pending.exists(), "no pending prompt was stashed")
        self.assertEqual(stat.S_IMODE(pending.parent.stat().st_mode), 0o700)
        self.assertEqual(stat.S_IMODE(pending.stat().st_mode), 0o600)


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


class RecordedTranscriptCorpus(unittest.TestCase):
    """The recorded fixtures under testdata/transcripts pin the transcript
    shapes the Stop hook's projection is tested against. They come from real
    sessions, so two properties have to hold before anything else uses them:
    no original text survived redaction, and no literal harness-tag byte
    exists in the files at rest.

    Failure messages here name keys, counts and file names — never a value.
    Rendering a fixture entry is what these files exist to avoid; see the
    redaction script's module docstring."""

    # The keys whose values are harness structure and survive redaction
    # verbatim. Held here independently of the redaction script so widening
    # that script's allowlist takes a deliberate second edit.
    STRUCTURE_KEYS = {
        "type", "role", "stop_reason", "stop_sequence", "subtype", "userType",
        "level", "timestamp", "model", "service_tier", "isSidechain", "isMeta",
        "name", "version",
    }
    IDENTIFIER_KEYS = {
        "uuid", "parentUuid", "sessionId", "leafUuid", "requestId", "id",
        "parentToolUseID", "toolUseID", "tool_use_id", "agentId",
    }

    @classmethod
    def setUpClass(cls):
        cls.corpus = ADAPTERS.parent / "testdata" / "transcripts"
        cls.redact = load("aj_redact", "../scripts/redact-transcript.py")
        cls.fixtures = sorted(cls.corpus.glob("*.jsonl"))

    def walk(self, value, key=None):
        """Yield (key, string) for every string in a decoded fixture."""
        if isinstance(value, dict):
            for k, v in value.items():
                yield from self.walk(v, k)
        elif isinstance(value, list):
            for v in value:
                yield from self.walk(v, key)
        elif isinstance(value, str):
            yield key, value

    def test_fixtures_contain_no_unredacted_text(self):
        alphabet = set(self.redact.PLACEHOLDER_WORDS)
        tag_re = self.redact._TAG_RE
        self.assertTrue(self.fixtures, "no recorded fixtures")
        self.assertLessEqual(
            set(self.redact.VERBATIM_KEYS), self.STRUCTURE_KEYS,
            "the redaction script preserves a key this test does not vouch for",
        )
        for path in self.fixtures:
            entries = [json.loads(line) for line in
                       path.read_text(encoding="utf-8").splitlines() if line.strip()]
            self.assertTrue(entries, f"{path.name}: empty fixture")
            checked = 0
            for key, text in self.walk(entries):
                if key in self.STRUCTURE_KEYS or key in self.IDENTIFIER_KEYS:
                    # Structure and identifiers are not prose: a leaked
                    # sentence would carry whitespace, and nothing structural
                    # is long.
                    self.assertNotIn(" ", text, f"{path.name}: spaces under key {key!r}")
                    self.assertLessEqual(len(text), 128, f"{path.name}: long value at {key!r}")
                    continue
                if key in self.redact.PATH_KEYS:
                    self.assertEqual(text, "/redacted/workspace", f"{path.name}: {key!r}")
                    continue
                if key == "gitBranch":
                    self.assertEqual(text, "redacted-branch", f"{path.name}: {key!r}")
                    continue
                for part in tag_re.split(text):
                    if tag_re.fullmatch(part):
                        continue
                    for offset, word in enumerate(part.replace("/", " ").split()):
                        checked += 1
                        self.assertIn(
                            word, alphabet,
                            f"{path.name}: word #{offset} under key {key!r} is not "
                            "in the placeholder alphabet",
                        )
            self.assertGreater(checked, 0, f"{path.name}: no content strings checked")

    def test_tag_vocabularies_match_the_hook(self):
        """The redaction script preserves exactly the tags the hook
        classifies on. A tag added to one list but not the other would let
        redaction replace a load-bearing tag with placeholder words, so a
        refreshed fixture no longer pins the shape it was recorded for —
        the one refresh failure that is silent end to end."""
        self.assertEqual(tuple(self.redact.ENVELOPE_TAGS), tuple(cc.SYNTHETIC_TAGS))
        self.assertEqual(
            tuple(self.redact.MACHINE_HEAD_TAGS), tuple(cc.MACHINE_HEAD_TAGS)
        )

    def test_fixtures_carry_no_literal_tag_bytes(self):
        """The at-rest encoding, kept regression-proof: a fixture regenerated
        without the angle-bracket escaping would read as live control markup
        to anything that opens the file."""
        for path in self.fixtures + [self.corpus / "EXPECTED.json"]:
            raw = path.read_bytes()
            self.assertNotIn(b"<", raw, f"{path.name}: literal '<' at rest")
            self.assertNotIn(b">", raw, f"{path.name}: literal '>' at rest")

    def test_expected_json_matches_regeneration(self):
        """EXPECTED.json is generated, not maintained. Regenerating it from
        the fixtures in a scratch copy must reproduce the committed bytes."""
        expected = (self.corpus / "EXPECTED.json").read_bytes()
        with tempfile.TemporaryDirectory() as tmp:
            scratch = pathlib.Path(tmp)
            for path in self.fixtures:
                shutil.copy2(path, scratch / path.name)
            buffer = StringIO()
            with contextlib.redirect_stdout(buffer):
                code = self.redact.emit_expected(str(scratch))
            self.assertEqual(code, 0, "regeneration failed")
            self.assertEqual(
                (scratch / "EXPECTED.json").read_bytes(), expected,
                "EXPECTED.json is stale or hand-edited — regenerate with "
                "scripts/redact-transcript.py --emit-expected",
            )

    def test_expected_json_entry_contract(self):
        document = json.loads((self.corpus / "EXPECTED.json").read_text(encoding="utf-8"))
        self.assertIn("_contract", document)
        entries = document["expectations"]
        self.assertEqual(
            sorted(e["fixture"] for e in entries),
            sorted(p.name for p in self.fixtures),
            "an expectation and a fixture are missing each other",
        )
        for entry in entries:
            keys = set(entry)
            self.assertLessEqual(
                {"fixture", "skipped", "tools"}, keys, f"{entry['fixture']}: missing key",
            )
            self.assertIsInstance(entry["skipped"], bool, entry["fixture"])
            self.assertIsInstance(entry["tools"], list, entry["fixture"])
            for name in entry["tools"]:
                self.assertTrue(cc.valid_token(name), f"{entry['fixture']}: tool name")
            projected = {"user_content", "assistant_result"}
            if entry["skipped"]:
                self.assertFalse(projected & keys, f"{entry['fixture']}: skipped but projected")
            else:
                self.assertLessEqual(projected, keys, f"{entry['fixture']}: missing projection")
                for field in projected:
                    self.assertIsInstance(entry[field], str, entry["fixture"])
                    self.assertNotEqual(entry[field].strip(), "", entry["fixture"])


class RepairCorpus(unittest.TestCase):
    """scripts/repair-corpus.py replays cc-stop.v3 episodes under the v4
    projection. Everything here runs against a synthetic corpus built in a
    temporary directory — a test must not be able to touch real memory."""

    def setUp(self):
        self.tmp = pathlib.Path(tempfile.mkdtemp())
        self.addCleanup(lambda: shutil.rmtree(self.tmp, ignore_errors=True))
        self.root = self.tmp / "journal"
        self.day = self.root / "2026" / "08" / "10"
        self.day.mkdir(parents=True)
        self.transcripts = self.tmp / "transcripts"
        (self.transcripts / "workspace").mkdir(parents=True)

        self.calls = self.tmp / "calls.jsonl"
        self.plan = self.tmp / "plan.json"
        self.plan.write_text("{}")
        self.binary = self.tmp / "fake-autojournal"
        self.binary.write_text(
            "#!/usr/bin/env python3\n"
            "import json, os, sys\n"
            "plan = json.load(open(os.environ['FAKE_AJ_PLAN']))\n"
            "cmd = sys.argv[1]\n"
            "with open(os.environ['FAKE_AJ_CALLS'], 'a') as log:\n"
            "    if cmd == 'capture':\n"
            "        payload = json.load(sys.stdin)\n"
            "        log.write(json.dumps({'cmd': cmd, 'payload': payload}) + '\\n')\n"
            "        entry = plan.get(payload['turn_id'], {})\n"
            "        # The real binary reports a root-relative path on every\n"
            "        # outcome that names an episode, conflict included.\n"
            "        print(json.dumps({'outcome': entry.get('outcome', 'published'),\n"
            "                          'episode_id': None, 'payload_digest': None,\n"
            "                          'path': entry.get('path', '2026/08/10/aj1-new.md'),\n"
            "                          'index': 'fresh', 'detail': None}))\n"
            "    else:\n"
            "        log.write(json.dumps({'cmd': cmd, 'argv': sys.argv[1:]}) + '\\n')\n"
            "        print(json.dumps({'outcome': 'ok'}))\n"
        )
        self.binary.chmod(self.binary.stat().st_mode | stat.S_IEXEC)

        self._env = dict(os.environ)
        self.addCleanup(lambda: (os.environ.clear(), os.environ.update(self._env)))
        os.environ["AUTOJOURNAL_BIN"] = str(self.binary)
        os.environ["FAKE_AJ_PLAN"] = str(self.plan)
        os.environ["FAKE_AJ_CALLS"] = str(self.calls)

    def episode(self, name, policy="cc-stop.v3", session="sess-r1",
                turn="turn-r1", event_ms=1786000000000):
        path = self.day / f"aj1-{name}.md"
        path.write_text(
            "---\n"
            "schema: aj-episode.v1\n"
            f"episode_id: aj1-{name}\n"
            "world: mainworld\n"
            "scope: workspace:demo\n"
            "lane: conversation\n"
            "harness: claude-code\n"
            "adapter_version: cc-stop-hook-1.4.0\n"
            f"session_id: {session}\n"
            f"turn_id: {turn}\n"
            "event_time: 2026-08-05T12:00:00Z\n"
            f"event_time_ms: {event_ms}\n"
            "capture_time: 2026-08-05T12:01:00Z\n"
            "capture_time_ms: 1786000060000\n"
            f"capture_policy: {policy}\n"
            "turn_outcome: completed\n"
            "payload_digest: sha256:0000\n"
            "---\n\n## User\n\nwrongly captured skill body\n"
        )
        return path

    def transcript_for(self, session, turn, args="deploy the falcon"):
        path = self.transcripts / "workspace" / f"{session}.jsonl"
        envelope = (
            "<command-name>/deploy</command-name>\n"
            "<command-message>deploy</command-message>\n"
            f"<command-args>{args}</command-args>"
        )
        path.write_text(
            json.dumps({"type": "user", "uuid": turn,
                        "timestamp": "2026-08-05T12:00:00Z",
                        "message": {"content": envelope}})
            + "\n"
            + json.dumps({"type": "assistant",
                          "message": {"stop_reason": "end_turn",
                                      "content": [{"type": "text",
                                                   "text": "Falcon deployed."}]}})
            + "\n"
        )
        return path

    def run_repair(self, *extra):
        buffer = StringIO()
        with contextlib.redirect_stdout(buffer):
            code = repair.main([
                "--root", str(self.root), "--transcripts", str(self.transcripts),
                *extra,
            ])
        return code, buffer.getvalue()

    def summary(self, output):
        counts = {}
        for line in output.splitlines():
            if line.startswith("  ") and ": " in line:
                key, _, value = line.strip().partition(": ")
                if value.isdigit():
                    counts[key] = int(value)
        return counts

    def capture_calls(self):
        if not self.calls.exists():
            return []
        return [json.loads(line) for line in
                self.calls.read_text().splitlines() if line.strip()]

    def test_repair_report_is_report_only_by_default(self):
        v3 = self.episode("repairable")
        self.transcript_for("sess-r1", "turn-r1")
        before = sorted(p.relative_to(self.tmp) for p in self.tmp.rglob("*") if p.is_file())

        code, output = self.run_repair()

        self.assertEqual(code, 0, output)
        self.assertTrue(v3.exists(), "report mode deleted an episode")
        after = sorted(p.relative_to(self.tmp) for p in self.tmp.rglob("*") if p.is_file())
        self.assertEqual(before, after, "report mode modified the tree")
        self.assertEqual(self.capture_calls(), [], "report mode invoked the binary")
        counts = self.summary(output)
        self.assertEqual(counts["v3_found"], 1)
        self.assertEqual(counts["would_publish"], 1)
        self.assertEqual(counts["would_delete"], 1)
        self.assertIn("WOULD-PUBLISH", output)
        self.assertIn("deploy the falcon", output,
                      "the report does not show the repaired prompt")

    def test_repair_deletes_only_the_vintage_it_replaced(self):
        replaced = self.episode("replaced", session="sess-a", turn="turn-a")
        self.transcript_for("sess-a", "turn-a", args="repair the falcon record")
        conflicted = self.episode("conflicted", session="sess-b", turn="turn-b")
        self.transcript_for("sess-b", "turn-b")
        v4 = self.episode("already-v4", policy="cc-stop.v4",
                          session="sess-c", turn="turn-c")
        self.plan.write_text(json.dumps({
            "turn-a": {"outcome": "published",
                       "path": "2026/08/10/aj1-replacement.md"},
            "turn-b": {"outcome": "conflict",
                       "path": "2026/08/10/aj1-existing.md"},
        }))

        code, output = self.run_repair("--apply")

        self.assertEqual(code, 0, output)
        self.assertFalse(replaced.exists(), "a replaced v3 file survived")
        self.assertTrue(conflicted.exists(),
                        "a conflicted v3 file was deleted even though its "
                        "replacement did not publish")
        self.assertTrue(v4.exists(), "a v4 episode was touched")
        calls = self.capture_calls()
        captured_turns = sorted(
            c["payload"]["turn_id"] for c in calls if c["cmd"] == "capture"
        )
        self.assertEqual(captured_turns, ["turn-a", "turn-b"],
                         "capture ran for the wrong vintage")
        transcript_ms = 1785931200000  # 2026-08-05T12:00:00Z
        for call in calls:
            if call["cmd"] != "capture":
                continue
            payload = call["payload"]
            self.assertEqual(payload["capture_policy"], "cc-stop.v4")
            self.assertEqual(payload["event_time_ms"], transcript_ms,
                             "event time must come from the replayed "
                             "transcript entry, deterministically")
            self.assertEqual(payload["world"], "mainworld")
            self.assertEqual(payload["scope"], "workspace:demo")
        published = next(c["payload"] for c in calls
                         if c["cmd"] == "capture" and c["payload"]["turn_id"] == "turn-a")
        self.assertEqual(published["user_content"], "repair the falcon record",
                         "the replay did not journal the typed argument")
        self.assertEqual(published["assistant_result"], "Falcon deployed.")
        sync_call = calls[-1]
        self.assertEqual(sync_call["cmd"], "sync",
                         "apply did not run sync after publishing")
        self.assertIn("--json", sync_call["argv"],
                      "sync success is judged by parsing its report, which "
                      "only the --json form emits")
        self.assertIn("sync: ok", output)
        counts = self.summary(output)
        self.assertEqual(counts["published"], 1)
        self.assertEqual(counts["deleted"], 1)

    def machine_anchored_transcript(self, session, owner_turn, machine_turn):
        """The shape the repair exists for: v3 anchored its episode on a
        machine-injected entry that follows the owner's real turn."""
        path = self.transcripts / "workspace" / f"{session}.jsonl"
        lines = [
            {"type": "user", "uuid": owner_turn,
             "timestamp": "2026-08-05T12:00:00Z",
             "message": {"content": "please repair the falcon record"}},
            {"type": "assistant",
             "message": {"stop_reason": "end_turn",
                         "content": [{"type": "text", "text": "Working on it."}]}},
            {"type": "user", "uuid": machine_turn,
             "timestamp": "2026-08-05T12:05:00Z",
             "message": {"content": "<system-reminder>machine framing"
                                    "</system-reminder>"}},
            {"type": "assistant",
             "message": {"stop_reason": "end_turn",
                         "content": [{"type": "text", "text": "Falcon repaired."}]}},
        ]
        path.write_text("".join(json.dumps(o) + "\n" for o in lines))
        return path

    def test_repair_reanchors_a_machine_anchored_episode(self):
        # The defect class D12 named first: the v3 episode's recorded turn
        # is an entry v4 classifies as machine-injected. The repair must
        # walk back to the owner's entry, not decline the episode.
        self.episode("machine-anchored", session="sess-m", turn="turn-machine")
        self.machine_anchored_transcript("sess-m", "turn-owner", "turn-machine")

        code, output = self.run_repair("--apply")

        self.assertEqual(code, 0, output)
        calls = [c for c in self.capture_calls() if c["cmd"] == "capture"]
        self.assertEqual(len(calls), 1, "the reanchored episode was not replayed")
        payload = calls[0]["payload"]
        self.assertEqual(payload["turn_id"], "turn-owner",
                         "the replacement must carry the owner turn's identity")
        self.assertEqual(payload["user_content"], "please repair the falcon record")
        self.assertEqual(payload["assistant_result"],
                         "Working on it.\n\nFalcon repaired.",
                         "the owner turn's cumulative body was not projected")
        self.assertEqual(payload["event_time_ms"], 1785931200000,
                         "event time must be the owner entry's timestamp")
        self.assertIn("reanchored to turn turn-owner", output)

    def test_repair_leaves_a_machine_driven_turn_alone(self):
        # Nothing owner-typed precedes the recorded entry: v4 would never
        # have captured this, so there is nothing to replace.
        v3 = self.episode("machine-only", session="sess-md", turn="turn-md")
        path = self.transcripts / "workspace" / "sess-md.jsonl"
        path.write_text(
            json.dumps({"type": "user", "uuid": "turn-md",
                        "timestamp": "2026-08-05T12:00:00Z",
                        "message": {"content": "<system-reminder>x"
                                               "</system-reminder>"}}) + "\n"
        )

        code, output = self.run_repair("--apply")

        self.assertEqual(code, 0, output)
        self.assertTrue(v3.exists(), "a machine-driven episode was removed")
        self.assertEqual(self.capture_calls(), [])
        self.assertEqual(self.summary(output)["not_published_under_v4"], 1)

    def test_repair_never_deletes_the_file_the_replacement_names(self):
        # A publish outcome whose root-relative path resolves to the v3
        # file itself must not delete it, whatever the outcome claimed.
        v3 = self.episode("self-path", session="sess-s", turn="turn-s")
        self.transcript_for("sess-s", "turn-s")
        self.plan.write_text(json.dumps({
            "turn-s": {"outcome": "published",
                       "path": str(v3.relative_to(self.root))},
        }))

        code, output = self.run_repair("--apply")

        self.assertEqual(code, 0, output)
        self.assertTrue(v3.exists(), "the guard against self-deletion failed")
        self.assertIn("NOT-REPLACED", output)
        self.assertEqual(self.summary(output)["deleted"], 0)

    def test_repair_survives_an_odd_transcript_entry(self):
        # One transcript whose shape the projection cannot read costs that
        # episode's repair, never the rest of the run.
        odd = self.episode("odd-shape", session="sess-odd", turn="turn-odd")
        odd_path = self.transcripts / "workspace" / "sess-odd.jsonl"
        odd_path.write_text(
            json.dumps({"type": "user", "uuid": "turn-odd",
                        "message": "just a string, not an object"}) + "\n"
        )
        healthy = self.episode("healthy", session="sess-h", turn="turn-h")
        self.transcript_for("sess-h", "turn-h")

        code, output = self.run_repair("--apply")

        self.assertEqual(code, 0, output)
        self.assertTrue(odd.exists(), "an unreadable transcript cost its episode")
        self.assertFalse(healthy.exists(),
                         "the run did not continue past the odd transcript")
        counts = self.summary(output)
        self.assertEqual(counts["replay_error"], 1)
        self.assertEqual(counts["deleted"], 1)
        self.assertIn("REPLAY-ERROR", output)

    def test_repair_leaves_an_unrepairable_episode_alone(self):
        orphan = self.episode("orphan", session="sess-gone", turn="turn-gone")

        code, output = self.run_repair("--apply")

        self.assertEqual(code, 0, output)
        self.assertTrue(orphan.exists(), "an unrepairable episode was removed")
        self.assertEqual(self.capture_calls(), [],
                         "an unrepairable episode reached the binary")
        counts = self.summary(output)
        self.assertEqual(counts["unrepairable"], 1)
        self.assertEqual(counts["would_publish"], 0)
        self.assertIn("UNREPAIRABLE", output)


if __name__ == "__main__":
    unittest.main(verbosity=2)
