#!/usr/bin/env python3
"""Produce a structure-preserving redacted fixture from a real Claude Code
transcript.

The recorded corpus under testdata/transcripts/ pins the transcript shapes
the Stop hook's projection is tested against. Shapes go stale as the harness
evolves, so fixtures are produced by this committed script rather than
hand-maintained: re-run it against a fresh transcript and the fixture
refreshes without anyone re-inventing the redaction rules.

What is preserved: entry kinds and ordering, key structure and nesting,
`type` / `role` / `stop_reason` / block types, harness envelope tags
(<command-name>, <task-notification>, <system-reminder>, ...), tool names,
uuid *shape* (same length and dashes, different hex), and timestamps.

What is replaced: every content string, with deterministic placeholder text
drawn from a fixed sixteen-word alphabet, seeded by a hash of the original so
repeated runs are byte-stable and no original text survives. Newline
structure inside content is preserved because the projection is sensitive to
it.

Encoding: emitted JSON escapes every angle bracket (\u003c / \u003e), so the
fixture files never carry a literal harness-tag byte sequence at rest.
json.loads reconstitutes the exact tags for the tests, but a tool or scanner
reading the raw file sees escaped text, not what looks like live control
markup inside a conversation log. Keep it that way — and keep decoded fixture
content out of terminals and chat contexts; print counts and verdicts, not
entries.

EXPECTED.json is generated here rather than hand-written, for the same
reason: writing it by hand means first reading the fixture bodies, and the
corpus refreshes. `--emit-expected` resolves one declarative rule per fixture
(below) against the fixtures and writes the file, printing only shape counts,
lengths and digests. The rules are authored from the cc-stop.v4 projection
specification and are deliberately independent of the hook — the hook's v3
projection mishandles the slash-command shape, so generating expectations by
running it would pin the very defect the fixtures exist to catch.

Usage:
    redact-transcript.py --self-test
    redact-transcript.py IN.jsonl [--start N] [--end N] > fixture.jsonl
    redact-transcript.py --emit-expected testdata/transcripts
"""

from __future__ import annotations

import argparse
import hashlib
import json
import pathlib
import re
import sys

# The whole placeholder alphabet. test_fixtures_contain_no_unredacted_text
# holds every fixture string to this list (plus preserved structure), so a
# fixture that slipped real text past redaction fails the gate.
PLACEHOLDER_WORDS = (
    "alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel",
    "india", "juliet", "kilo", "lima", "mike", "november", "oscar", "papa",
)

# Envelope tags the hook classifies on. The tags survive redaction; the text
# inside them is placeholder like everything else.
ENVELOPE_TAGS = (
    "task-notification",
    "local-command-caveat",
    "local-command-stdout",
    "command-name",
    "command-message",
    "command-args",
    "system-reminder",
)

# String values preserved verbatim when found under these keys: they are
# harness structure, not owner content.
VERBATIM_KEYS = {
    "type", "role", "stop_reason", "stop_sequence", "subtype", "userType",
    "level", "timestamp", "model", "service_tier", "isSidechain",
    "isMeta", "name",
}

# Keys whose values are identifiers: redacted hex-for-hex so the shape
# (length, dashes, prefixes) survives but the identity does not.
IDENTIFIER_KEYS = {
    "uuid", "parentUuid", "sessionId", "leafUuid", "requestId", "id",
    "parentToolUseID", "toolUseID", "tool_use_id", "agentId",
}

PATH_KEYS = {"cwd", "workspace_root", "transcript_path"}

_TAG_RE = re.compile("(</?(?:%s)>)" % "|".join(ENVELOPE_TAGS))
_HEX_RE = re.compile(r"[0-9a-fA-F]")


def words_for(seed: str, count: int) -> str:
    digest = hashlib.sha256(("aj-redact.v1\0" + seed).encode()).digest()
    picked = []
    for i in range(max(1, count)):
        picked.append(PLACEHOLDER_WORDS[digest[i % len(digest)] % len(PLACEHOLDER_WORDS)])
    return " ".join(picked)


def redact_line(line: str) -> str:
    """Replace one line of content with placeholders of similar word count,
    preserving leading '/' (slash-command names keep their shape) and
    envelope tags if any leak into a single line."""
    if line.strip() == "":
        return line
    prefix = "/" if line.lstrip().startswith("/") else ""
    count = min(len(line.split()), 12)
    return prefix + words_for(line, count)


def redact_text(text: str) -> str:
    """Redact free text: envelope tags survive, everything between them is
    placeholder words, one line at a time so newline structure survives."""
    parts = _TAG_RE.split(text)
    out = []
    for part in parts:
        if _TAG_RE.fullmatch(part):
            out.append(part)
            continue
        out.append("\n".join(redact_line(line) for line in part.split("\n")))
    return "".join(out)


def redact_identifier(value: str) -> str:
    digest = hashlib.sha256(("aj-redact-id.v1\0" + value).encode()).hexdigest()
    it = iter(digest * (1 + len(value) // len(digest)))
    return _HEX_RE.sub(lambda _: next(it), value)


def redact_value(key: str, value: object) -> object:
    if isinstance(value, dict):
        return {k: redact_value(k, v) for k, v in value.items()}
    if isinstance(value, list):
        return [redact_value(key, v) for v in value]
    if not isinstance(value, str):
        return value
    if key in VERBATIM_KEYS:
        return value
    if key in IDENTIFIER_KEYS:
        return redact_identifier(value)
    if key in PATH_KEYS:
        return "/redacted/workspace"
    if key == "gitBranch":
        return "redacted-branch"
    if key == "version":
        return value
    return redact_text(value)


def redact_entry(entry: dict) -> dict:
    return {k: redact_value(k, v) for k, v in entry.items()}


def encode_fixture_line(entry: dict) -> str:
    """One fixture line: compact JSON with angle brackets escaped, so no
    literal tag markup exists in the file's bytes."""
    line = json.dumps(entry, separators=(",", ":"), sort_keys=False)
    return line.replace("<", "\\u003c").replace(">", "\\u003e")


# --------------------------------------------------------------------------
# EXPECTED.json generation
# --------------------------------------------------------------------------

# User entries carrying one of these at the head of their text were injected
# by the harness, not typed by the owner: they never start a turn and never
# end one. `command-name` / `command-args` / `command-message` are absent
# deliberately — a slash-command envelope IS an owner turn, wearing markup.
MACHINE_HEAD_TAGS = (
    "task-notification",
    "local-command-caveat",
    "local-command-stdout",
    "system-reminder",
)

OWNER_TYPED = "owner-typed"
COMMAND_ENVELOPE = "command-envelope"
MACHINE_INJECTED = "machine-injected"

# How a fixture's prompt is extracted once its turn-starting entry is found.
PROMPT_WHOLE_TEXT = "whole-text"
PROMPT_COMMAND_ARGS = "command-args-inner-text"

# One rule per recorded shape. `shape` names what the fixture is and is
# authored from the v4 specification; `expect` is the recorded corpus's shape
# fingerprint, taken once when the fixtures were captured. Generation refuses
# to run when the two disagree, so refreshing the corpus against a different
# transcript fails loudly here instead of quietly emitting an expectation for
# a shape nobody recorded. `tools` counts distinct names, not tool_use blocks.
EXPECTATION_RULES = {
    "plain-turn.jsonl": {
        "shape": "one owner-typed prompt, one terminal text response, no tools",
        "prompt": PROMPT_WHOLE_TEXT,
        "expect": {"owner": 1, "envelope": 0, "machine": 0, "terminals": 1, "tools": 0},
        "skipped": False,
    },
    "slash-command.jsonl": {
        "shape": "slash-command envelope, then the expanded skill body as "
                 "machine-injected continuation; the typed argument is the prompt",
        "prompt": PROMPT_COMMAND_ARGS,
        "expect": {"owner": 0, "envelope": 1, "machine": 6, "terminals": 1, "tools": 3},
        "skipped": False,
    },
    "notification-resume.jsonl": {
        "shape": "owner prompt, a background-agent notification mid-turn, and "
                 "terminal responses on both sides of it",
        "prompt": PROMPT_WHOLE_TEXT,
        "expect": {"owner": 1, "envelope": 0, "machine": 3, "terminals": 2, "tools": 2},
        "skipped": False,
    },
    "thinking-then-text.jsonl": {
        "shape": "a thinking-only end_turn followed by a text end_turn",
        "prompt": PROMPT_WHOLE_TEXT,
        "expect": {"owner": 1, "envelope": 0, "machine": 0, "terminals": 1, "tools": 0},
        "skipped": False,
    },
    "tool-use-turn.jsonl": {
        "shape": "one owner prompt driving tool_use blocks and their tool_result "
                 "carriers, settling on one terminal response",
        "prompt": PROMPT_WHOLE_TEXT,
        "expect": {"owner": 1, "envelope": 0, "machine": 6, "terminals": 1, "tools": 2},
        "skipped": False,
    },
}

EXPECTED_CONTRACT = [
    "Generated by scripts/redact-transcript.py --emit-expected. Do not hand-edit:",
    "regenerate after refreshing testdata/transcripts/*.jsonl.",
    "",
    "One entry per recorded transcript shape, stating what the Claude Code Stop",
    "hook's cc-stop.v4 projection must produce for it. The entry contract is",
    "closed:",
    "  fixture           file name under testdata/transcripts/, always present",
    "  skipped           bool, always present; true when the hook must decline",
    "                    the transcript entirely",
    "  tools             list of tool-name strings, always present, may be empty,",
    "                    first-appearance order, duplicates removed",
    "  user_content      projected prompt; present exactly when skipped is false",
    "  assistant_result  projected body; present exactly when skipped is false",
    "",
    "Angle brackets are escaped throughout, matching the fixture encoding.",
]


def valid_tool_name(value: str) -> bool:
    """The core token rule, restated: a tool name outside it would make the
    whole capture invalid, not just the field."""
    return (
        isinstance(value, str)
        and 0 < len(value.encode("utf-8")) <= 128
        and all(ch.isascii() and (ch.isalnum() or ch in "._-:+/@") for ch in value)
    )


def entry_text(entry: dict) -> str:
    """The text a user entry contributes, string or block content alike."""
    content = entry.get("message", {}).get("content")
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        return "\n".join(
            b.get("text", "")
            for b in content
            if isinstance(b, dict) and b.get("type") == "text"
        )
    return ""


def classify_user(entry: dict) -> str | None:
    """cc-stop.v4 classification of one user entry, or None when the entry is
    not a user entry at all.

    A tool_result carrier, an `isMeta` entry and anything headed by a machine
    tag are all machine-injected: they carry harness output into the
    conversation and never represent something the owner typed."""
    if entry.get("type") != "user":
        return None
    content = entry.get("message", {}).get("content")
    if isinstance(content, list) and any(
        isinstance(b, dict) and b.get("type") == "tool_result" for b in content
    ):
        return MACHINE_INJECTED
    if entry.get("isMeta"):
        return MACHINE_INJECTED
    if entry.get("isSidechain"):
        return MACHINE_INJECTED
    text = entry_text(entry)
    if not text.strip():
        return MACHINE_INJECTED
    head = text.lstrip()
    if any(head.startswith(f"<{tag}>") for tag in MACHINE_HEAD_TAGS):
        return MACHINE_INJECTED
    # Anchored to the head of the text, matching the hook: a real envelope
    # begins with its own markup, while a prompt quoting the tags
    # mid-sentence is owner-typed text about the markup.
    if head.startswith("<command-name>") or head.startswith("<command-message>"):
        return COMMAND_ENVELOPE
    return OWNER_TYPED


def strip_edge_machine_blocks(text: str) -> str:
    """Remove harness-generated <tag>…</tag> blocks from the leading and
    trailing edges of an owner-typed prompt, leaving only what the owner
    typed — the same edge rule the hook applies, so the projected
    expectation and the hook cannot diverge on this shape. An unterminated
    leading tag swallows the rest (machine framing, not prose); interior
    blocks between owner-typed passages are left alone."""

    def strip_leading(s: str) -> str:
        while True:
            s = s.lstrip()
            tag = next((t for t in ENVELOPE_TAGS if s.startswith(f"<{t}>")), None)
            if tag is None:
                return s
            end = s.find(f"</{tag}>")
            if end == -1:
                return ""
            s = s[end + len(tag) + 3:]

    def strip_trailing(s: str) -> str:
        while True:
            s = s.rstrip()
            tag = next((t for t in ENVELOPE_TAGS if s.endswith(f"</{t}>")), None)
            if tag is None:
                return s
            start = s.rfind(f"<{tag}>")
            if start == -1:
                return s
            s = s[:start]

    return strip_trailing(strip_leading(text)).strip()


def command_args_text(text: str) -> str:
    """The inner text of the envelope's <command-args>, which is what the
    owner actually typed after the command name."""
    open_tag, close_tag = "<command-args>", "</command-args>"
    start = text.find(open_tag)
    end = text.find(close_tag, start + len(open_tag)) if start != -1 else -1
    if start == -1 or end == -1:
        return ""
    return text[start + len(open_tag):end].strip()


def command_name_text(text: str) -> str:
    """The inner text of the envelope's <command-name>, e.g. "/plan"."""
    open_tag, close_tag = "<command-name>", "</command-name>"
    start = text.find(open_tag)
    end = text.find(close_tag, start + len(open_tag)) if start != -1 else -1
    if start == -1 or end == -1:
        return ""
    return text[start + len(open_tag):end].strip()


def envelope_prompt(text: str) -> str:
    """The prompt an envelope journals: typed <command-args> when the owner
    supplied any, else the command name itself — the same bare-envelope rule
    the hook applies, so the projected expectation and the hook cannot
    diverge on bare envelopes."""
    args = command_args_text(text)
    if args:
        return args
    return command_name_text(text)


def terminal_text(entry: dict) -> str:
    """The user-visible text of a terminal assistant entry, or "" when the
    entry is not terminal or carries no text (a thinking-only end_turn)."""
    message = entry.get("message", {})
    if message.get("stop_reason") != "end_turn":
        return ""
    content = message.get("content")
    if isinstance(content, str):
        return content.strip()
    if not isinstance(content, list):
        return ""
    return "\n".join(
        b.get("text", "").strip()
        for b in content
        if isinstance(b, dict) and b.get("type") == "text" and b.get("text", "").strip()
    )


def project(entries: list[dict], prompt_rule: str) -> dict:
    """Apply the v4 projection to one fixture's entries.

    The turn starts at the last owner-typed or command-envelope entry.
    Everything after it contributes: machine-injected entries do not break the
    window, so a turn resumed after a notification keeps both halves of its
    output. Terminal responses accumulate in order, joined by one blank line."""
    start = None
    for i, entry in enumerate(entries):
        if classify_user(entry) in (OWNER_TYPED, COMMAND_ENVELOPE):
            start = i
    if start is None:
        return {"skipped": True, "tools": [], "terminals": 0}

    text = entry_text(entries[start])
    if prompt_rule == PROMPT_COMMAND_ARGS:
        prompt = envelope_prompt(text)
    else:
        prompt = strip_edge_machine_blocks(text)

    terminals: list[str] = []
    tools: list[str] = []
    for entry in entries[start + 1:]:
        if entry.get("isSidechain"):
            # A subagent's own exchange contributes nothing to the owner's
            # turn — matching the hook's rule.
            continue
        if entry.get("type") != "assistant":
            continue
        settled = terminal_text(entry)
        if settled:
            terminals.append(settled)
        content = entry.get("message", {}).get("content")
        if isinstance(content, list):
            for block in content:
                if (
                    isinstance(block, dict)
                    and block.get("type") == "tool_use"
                    and isinstance(block.get("name"), str)
                    and block["name"] not in tools
                ):
                    tools.append(block["name"])
    return {
        "skipped": False,
        "user_content": prompt,
        "assistant_result": "\n\n".join(terminals),
        "tools": tools,
        "terminals": len(terminals),
    }


def encode_expected(document: dict) -> str:
    text = json.dumps(document, indent=2, sort_keys=False, ensure_ascii=False)
    return text.replace("<", "\\u003c").replace(">", "\\u003e") + "\n"


def emit_expected(directory: str) -> int:
    """Write EXPECTED.json for the recorded corpus.

    Prints shape counts, lengths and digests only. No fixture content and no
    per-entry rendering reaches stdout — see the module docstring."""
    root = pathlib.Path(directory)
    fixtures = sorted(p.name for p in root.glob("*.jsonl"))
    unruled = [name for name in fixtures if name not in EXPECTATION_RULES]
    unrecorded = [name for name in EXPECTATION_RULES if name not in fixtures]
    if unruled or unrecorded:
        print(f"no rule for: {unruled}; no fixture for: {unrecorded}", file=sys.stderr)
        return 1

    entries_out = []
    for name in fixtures:
        rule = EXPECTATION_RULES[name]
        entries = [
            json.loads(line)
            for line in (root / name).read_text(encoding="utf-8").splitlines()
            if line.strip()
        ]
        counts = {"owner": 0, "envelope": 0, "machine": 0}
        for entry in entries:
            kind = classify_user(entry)
            if kind == OWNER_TYPED:
                counts["owner"] += 1
            elif kind == COMMAND_ENVELOPE:
                counts["envelope"] += 1
            elif kind == MACHINE_INJECTED:
                counts["machine"] += 1

        projected = project(entries, rule["prompt"])
        counts["terminals"] = projected["terminals"]
        counts["tools"] = len(projected["tools"])
        if counts != rule["expect"]:
            print(
                f"{name}: shape changed — declared {rule['expect']}, found {counts}. "
                "Re-read the rule before regenerating.",
                file=sys.stderr,
            )
            return 1
        if projected["skipped"] != rule["skipped"]:
            print(f"{name}: skipped disagrees with the rule", file=sys.stderr)
            return 1

        # The payload contract rejects a capture carrying an out-of-class tool
        # name, so the hook would have to drop one. Rather than silently
        # filtering here and hiding the divergence, fail: a refresh that brings
        # in such a name is a decision, not a regeneration.
        bad = [n for n in projected["tools"] if not valid_tool_name(n)]
        if bad:
            print(f"{name}: {len(bad)} tool name(s) outside the token class", file=sys.stderr)
            return 1

        record = {"fixture": name, "skipped": projected["skipped"], "tools": projected["tools"]}
        if not projected["skipped"]:
            record["user_content"] = projected["user_content"]
            record["assistant_result"] = projected["assistant_result"]
            if not record["user_content"]:
                print(f"{name}: projected an empty prompt", file=sys.stderr)
                return 1
            if not record["assistant_result"]:
                print(f"{name}: projected an empty result", file=sys.stderr)
                return 1
        entries_out.append(record)
        digest = hashlib.sha256(
            json.dumps(record, sort_keys=True).encode()
        ).hexdigest()[:16]
        print(
            f"{name}: owner={counts['owner']} envelope={counts['envelope']} "
            f"machine={counts['machine']} terminals={counts['terminals']} "
            f"tools={counts['tools']} prompt_bytes={len(record.get('user_content', ''))} "
            f"result_bytes={len(record.get('assistant_result', ''))} sha256:{digest}"
        )

    document = {"_contract": EXPECTED_CONTRACT, "expectations": entries_out}
    encoded = encode_expected(document)
    if "<" in encoded or ">" in encoded:
        print("EXPECTED.json: literal tag bytes at rest", file=sys.stderr)
        return 1
    if json.loads(encoded) != document:
        print("EXPECTED.json: encoding does not round-trip", file=sys.stderr)
        return 1
    (root / "EXPECTED.json").write_text(encoded, encoding="utf-8")
    print(f"EXPECTED.json: {len(entries_out)} expectations, {len(encoded)} bytes, "
          f"sha256:{hashlib.sha256(encoded.encode()).hexdigest()[:16]}")
    return 0


def self_test() -> int:
    sample = {
        "type": "user",
        "uuid": "3f2a9b7c-1111-4d00-9abc-001122334455",
        "timestamp": "2026-08-09T12:00:00.000Z",
        "sessionId": "aaaabbbb-cccc-dddd-eeee-ffff00001111",
        "cwd": "/home/someone/secret-project",
        "message": {
            "role": "user",
            "content": "<command-name>/model</command-name>\n"
            "<command-args>the secret argument text</command-args>",
        },
    }
    red1 = redact_entry(sample)
    red2 = redact_entry(sample)
    assert red1 == red2, "redaction is not deterministic"
    assert red1["type"] == "user" and red1["timestamp"] == sample["timestamp"]
    u = red1["uuid"]
    assert len(u) == len(sample["uuid"]) and u.count("-") == 4 and u != sample["uuid"]
    text = red1["message"]["content"]
    assert "<command-args>" in text and "</command-args>" in text
    for leaked in ("secret", "argument", "someone"):
        assert leaked not in json.dumps(red1), f"leaked: {leaked}"
    assert red1["cwd"] == "/redacted/workspace"
    inner = _TAG_RE.split(text)
    for part in inner:
        if _TAG_RE.fullmatch(part) or part.strip() == "":
            continue
        for word in part.replace("/", " ").split():
            assert word in PLACEHOLDER_WORDS, f"non-placeholder word: {word}"

    assistant = {
        "type": "assistant",
        "message": {
            "role": "assistant",
            "stop_reason": "end_turn",
            "content": [
                {"type": "thinking", "thinking": "private reasoning"},
                {"type": "text", "text": "the visible answer"},
                {"type": "tool_use", "id": "toolu_01AbC", "name": "Bash",
                 "input": {"command": "cat /home/someone/private-notes.txt"}},
            ],
        },
    }
    red = redact_entry(assistant)
    blocks = red["message"]["content"]
    assert [b["type"] for b in blocks] == ["thinking", "text", "tool_use"]
    assert red["message"]["stop_reason"] == "end_turn"
    assert blocks[2]["name"] == "Bash"
    assert "private-notes" not in json.dumps(red)

    encoded = encode_fixture_line(red1)
    assert "<" not in encoded and ">" not in encoded, "literal tag bytes at rest"
    assert json.loads(encoded) == red1, "encoding does not round-trip"

    # --- classification and projection, on synthetic entries only -----------
    def user(content, **extra):
        return {"type": "user", "message": {"role": "user", "content": content}, **extra}

    def assistant(blocks, stop="end_turn"):
        return {"type": "assistant",
                "message": {"role": "assistant", "stop_reason": stop, "content": blocks}}

    envelope = user("<command-message>alpha</command-message>\n"
                    "<command-name>/bravo</command-name>\n"
                    "<command-args>charlie delta</command-args>")
    assert classify_user(envelope) == COMMAND_ENVELOPE
    assert command_args_text(entry_text(envelope)) == "charlie delta"
    assert classify_user(user("plain owner text")) == OWNER_TYPED
    # A prompt quoting the envelope tags mid-sentence is owner-typed, not
    # an envelope: classification anchors on the head of the text.
    quoting = user("how does <command-name>/bravo</command-name> pair with "
                   "<command-args>charlie</command-args> in the envelope?")
    assert classify_user(quoting) == OWNER_TYPED
    # A bare envelope journals the command name itself.
    bare = user("<command-name>/bravo</command-name>\n"
                "<command-message>bravo</command-message>\n"
                "<command-args></command-args>")
    assert classify_user(bare) == COMMAND_ENVELOPE
    assert envelope_prompt(entry_text(bare)) == "/bravo"
    assert classify_user(user("<task-notification>echo</task-notification>")) == MACHINE_INJECTED
    assert classify_user(user("<system-reminder>echo</system-reminder>")) == MACHINE_INJECTED
    assert classify_user(user([{"type": "tool_result", "content": "x"}])) == MACHINE_INJECTED
    assert classify_user(user([{"type": "text", "text": "expanded body"}], isMeta=True)) \
        == MACHINE_INJECTED
    assert classify_user(user("   ")) == MACHINE_INJECTED
    # A sidechain user entry is the model prompting a subagent, never the
    # owner: machine-injected wherever it appears.
    assert classify_user(user("delegated prompt", isSidechain=True)) == MACHINE_INJECTED
    assert classify_user({"type": "system", "content": "x"}) is None

    # A thinking-only end_turn contributes nothing; a non-terminal entry with
    # text contributes nothing either.
    assert terminal_text(assistant([{"type": "thinking", "thinking": "x"}])) == ""
    assert terminal_text(assistant([{"type": "text", "text": "answer"}], stop="tool_use")) == ""
    assert terminal_text(assistant([{"type": "text", "text": "answer"}])) == "answer"

    # A turn resumed after a notification keeps both halves of its output, in
    # order, and its tool names in first-appearance order without duplicates.
    resumed = [
        user("the owner prompt"),
        assistant([{"type": "tool_use", "id": "t1", "name": "Bash"}], stop="tool_use"),
        user([{"type": "tool_result", "content": "out"}]),
        assistant([{"type": "text", "text": "first half"}]),
        user("<task-notification>golf</task-notification>"),
        assistant([{"type": "tool_use", "id": "t2", "name": "Bash"},
                   {"type": "tool_use", "id": "t3", "name": "Read"}], stop="tool_use"),
        assistant([{"type": "thinking", "thinking": "x"}]),
        assistant([{"type": "text", "text": "second half"}]),
    ]
    got = project(resumed, PROMPT_WHOLE_TEXT)
    assert got["skipped"] is False
    assert got["user_content"] == "the owner prompt"
    assert got["assistant_result"] == "first half\n\nsecond half"
    assert got["tools"] == ["Bash", "Read"]
    assert got["terminals"] == 2

    # The envelope's expanded body is continuation, not a new prompt.
    slash = [envelope,
             user([{"type": "text", "text": "expanded body"}], isMeta=True),
             assistant([{"type": "text", "text": "answer"}])]
    got = project(slash, PROMPT_COMMAND_ARGS)
    assert got["user_content"] == "charlie delta", got["user_content"]
    assert got["assistant_result"] == "answer"
    assert got["tools"] == []

    # An owner-typed prompt carrying a machine block on its trailing edge
    # journals the typed sentence only — the same edge rule the hook
    # applies, so the two projections cannot diverge on this shape.
    edged = [
        user("the typed question\n<system-reminder>india oscar</system-reminder>"),
        assistant([{"type": "text", "text": "the answer"}]),
    ]
    got = project(edged, PROMPT_WHOLE_TEXT)
    assert got["user_content"] == "the typed question", got["user_content"]

    # Nothing owner-typed anywhere: the hook must decline.
    assert project([user("<system-reminder>hotel</system-reminder>")],
                   PROMPT_WHOLE_TEXT)["skipped"] is True

    assert valid_tool_name("Bash") and not valid_tool_name("bad name")
    doc_encoded = encode_expected({"_contract": EXPECTED_CONTRACT, "expectations": []})
    assert "<" not in doc_encoded and ">" not in doc_encoded

    print("redact-transcript self-test: PASS")
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("transcript", nargs="?", help="source transcript (.jsonl)")
    ap.add_argument("--start", type=int, default=0, help="first entry index")
    ap.add_argument("--end", type=int, default=None, help="one past the last entry index")
    ap.add_argument("--self-test", action="store_true")
    ap.add_argument(
        "--emit-expected",
        metavar="DIR",
        help="regenerate DIR/EXPECTED.json from the fixtures in DIR",
    )
    args = ap.parse_args()

    if args.self_test:
        return self_test()
    if args.emit_expected:
        return emit_expected(args.emit_expected)
    if not args.transcript:
        ap.error("transcript required unless --self-test")

    entries = []
    with open(args.transcript, encoding="utf-8") as fh:
        for line in fh:
            line = line.strip()
            if line:
                entries.append(json.loads(line))
    window = entries[args.start:args.end]
    for entry in window:
        print(encode_fixture_line(redact_entry(entry)))
    return 0


if __name__ == "__main__":
    sys.exit(main())
