#!/usr/bin/env python3
"""Claude Code 'Stop' hook — AutoJournal 1.0 capture.

Publishes each completed turn through the `autojournal capture` binary into
the owner-configured journal root. The binary owns identity, dedupe, atomic
publish, and index freshness; the hook only extracts the turn from the
transcript and ships one payload.

Wired as a Stop hook in ~/.claude/settings.json. Reads the hook payload on
stdin ({session_id, transcript_path, cwd, ...}). Non-blocking: any error →
exit 0, no capture.

Every session is captured, wherever it runs. Narrowing what enters memory
is the journal's job, not the hook's: set capture defaults with
`autojournal default --world <w> --scope <s>`, or point `journal_root`
somewhere else in ~/.config/autojournal/config.json. The turn's working
directory rides along as `workspace_root` provenance, and the machine it
ran on as `host`.

Capture policy cc-stop.v4 — classification-projected turns:
- Every user transcript entry is classified: owner-typed, command-envelope
  (a slash command's <command-name>/<command-args> wrapper), or
  machine-injected (tool_result carriers, isMeta entries, blank entries, and
  entries headed by a harness tag such as <task-notification>). The turn is
  assembled from the classification, not from transcript position.
- The turn starts at the most recent owner-typed or command-envelope entry.
  A machine-injected entry never starts a turn and never closes the window,
  so a turn resumed after a background-agent notification keeps the output
  on both sides of it. A transcript with nothing owner-typed was
  machine-driven and is skipped entirely.
- An envelope is recognized by its head: the entry text begins with
  <command-name> or <command-message>. A prompt that merely quotes those
  tags mid-sentence is owner-typed text about the markup, not an envelope.
- A command envelope journals its <command-args> inner text as the prompt —
  what the owner actually typed. A bare envelope (empty <command-args>)
  journals the command name itself. The expanded command body that follows
  is continuation context, not a new prompt.
- Machine-generated blocks are stripped from the leading and trailing edges
  of an owner-typed prompt; only the owner-typed remainder is journaled.
- The body carries every terminal assistant response since the prompt, in
  order, joined by one blank line. Progress prose and tool-call summaries
  stay excluded; tool names ride in the structured tools field and tool
  arguments and results are never captured.
- Stop may run before Claude Code has appended its separate terminal-text
  record to the transcript. Capture waits briefly for the terminal set to
  settle and skips the turn if no terminal text ever lands, rather than
  publishing progress text as the result.

Testing override: AUTOJOURNAL_HOOK_ROOT / AUTOJOURNAL_HOOK_INDEX pass
--root/--index to the binary instead of the owner config.
"""

import json
import os
import pathlib
import shutil
import socket
import subprocess
import sys
import time
from datetime import datetime

HARNESS = "claude-code"
ADAPTER_VERSION = "cc-stop-hook-1.6.0"
CAPTURE_POLICY = "cc-stop.v4"
TRANSCRIPT_SETTLE_TIMEOUT_S = 2.0
TRANSCRIPT_POLL_INTERVAL_S = 0.05
TRANSCRIPT_QUIET_INTERVAL_S = 0.15

# Closed list of harness-generated block tags. A prompt edge wrapped in one
# of these was produced by the machine, not typed by the owner.
SYNTHETIC_TAGS = (
    "task-notification",
    "local-command-caveat",
    "local-command-stdout",
    "command-name",
    "command-message",
    "command-args",
    "system-reminder",
)


def strip_synthetic_blocks(text: str) -> str:
    """Deterministically remove harness-generated <tag>…</tag> blocks from
    the leading and trailing edges of a prompt. An unterminated leading tag
    swallows the rest of the text (it is machine framing, not prose).
    Interior blocks between owner-typed passages are left alone."""

    def strip_leading(s: str) -> str:
        while True:
            s = s.lstrip()
            tag = next((t for t in SYNTHETIC_TAGS if s.startswith(f"<{t}>")), None)
            if tag is None:
                return s
            end = s.find(f"</{tag}>")
            if end == -1:
                return ""
            s = s[end + len(tag) + 3:]

    def strip_trailing(s: str) -> str:
        while True:
            s = s.rstrip()
            tag = next((t for t in SYNTHETIC_TAGS if s.endswith(f"</{t}>")), None)
            if tag is None:
                return s
            start = s.rfind(f"<{tag}>")
            if start == -1:
                return s
            s = s[:start]

    return strip_trailing(strip_leading(text)).strip()


def resolve_binary() -> str | None:
    """AUTOJOURNAL_BIN, then the conventional user install, then PATH —
    the same order the Pi adapter resolves. Returns None when no
    executable is found, which makes the hook a no-op instead of an
    error."""
    override = os.environ.get("AUTOJOURNAL_BIN", "").strip()
    if override:
        return override if os.access(override, os.X_OK) else None
    local = pathlib.Path.home() / ".local" / "bin" / "autojournal"
    if os.access(local, os.X_OK):
        return str(local)
    return shutil.which("autojournal")


def workspace_root(cwd: str) -> str | None:
    """The turn's working directory, as optional episode provenance. Sent
    only when it satisfies the payload contract's path rule (non-empty,
    <=512 bytes, no control characters), because an invalid value would
    reject the whole capture rather than just this field."""
    if not cwd or len(cwd.encode("utf-8")) > 512:
        return None
    if any(ord(ch) < 0x20 or ord(ch) == 0x7F for ch in cwd):
        return None
    return cwd


def origin_host() -> str | None:
    """The machine the turn ran on, as optional episode provenance. One
    journal root can be fed by several machines — a laptop syncing into a
    server's corpus, say — and without this the episodes are
    indistinguishable. Only the short name is sent, and only when it
    satisfies the payload contract's token rule, because an invalid value
    would reject the whole capture rather than just this field."""
    try:
        name = socket.gethostname().split(".")[0].strip()
    except Exception:
        return None
    if not valid_token(name):
        return None
    return name


def valid_token(value: object) -> bool:
    """Match the core token rule used by tool names and identity fields."""
    return (
        isinstance(value, str)
        and 0 < len(value.encode("utf-8")) <= 128
        and all(
            ch.isascii() and (ch.isalnum() or ch in "._-:+/@")
            for ch in value
        )
    )


# Entry classification (cc-stop.v4). Three kinds cover every user transcript
# entry, and the turn is assembled from the classification rather than from
# transcript position, so the next unfamiliar transcript shape becomes a new
# classification case instead of a new bug.
OWNER_TYPED = "owner-typed"
COMMAND_ENVELOPE = "command-envelope"
MACHINE_INJECTED = "machine-injected"

# A user entry whose text is headed by one of these tags was injected by the
# harness. The command-* tags are deliberately absent: a slash-command
# envelope is an owner turn wearing markup, classified separately.
MACHINE_HEAD_TAGS = (
    "task-notification",
    "local-command-caveat",
    "local-command-stdout",
    "system-reminder",
)


def entry_text(o: dict) -> str:
    """The text a user entry contributes, whether the harness recorded it
    as a plain string or as content blocks."""
    c = o.get("message", {}).get("content")
    if isinstance(c, str):
        return c
    if isinstance(c, list):
        return "\n".join(
            b.get("text", "")
            for b in c
            if isinstance(b, dict) and b.get("type") == "text"
        )
    return ""


def classify_user_entry(o: dict) -> str | None:
    """Classify one user transcript entry, or None for a non-user entry.

    A tool_result carrier, an ``isMeta`` entry (the expanded body of a slash
    command among them), a blank entry, and an entry headed by a machine tag
    all carry harness output into the conversation: none of them is
    something the owner typed, so none of them ever starts a turn."""
    if o.get("type") != "user":
        return None
    c = o.get("message", {}).get("content")
    if isinstance(c, list) and any(
        isinstance(b, dict) and b.get("type") == "tool_result" for b in c
    ):
        return MACHINE_INJECTED
    if o.get("isMeta"):
        return MACHINE_INJECTED
    if o.get("isSidechain"):
        return MACHINE_INJECTED
    text = entry_text(o)
    if not text.strip():
        return MACHINE_INJECTED
    head = text.lstrip()
    if any(head.startswith(f"<{tag}>") for tag in MACHINE_HEAD_TAGS):
        return MACHINE_INJECTED
    # Anchored to the head of the text, not substring containment: a real
    # envelope always begins with its own markup (command-name or
    # command-message, both shapes recorded in the fixture corpus), while
    # an owner-typed prompt *quoting* the tags mid-sentence is a prompt
    # about the markup, not an instance of it — substring matching
    # journaled the quoted example text, or skipped the turn entirely.
    if head.startswith("<command-name>") or head.startswith("<command-message>"):
        return COMMAND_ENVELOPE
    return OWNER_TYPED


def command_args_text(text: str) -> str:
    """The envelope's <command-args> inner text — what the owner actually
    typed after the command name, which is the part worth journaling."""
    open_tag, close_tag = "<command-args>", "</command-args>"
    start = text.find(open_tag)
    if start == -1:
        return ""
    end = text.find(close_tag, start + len(open_tag))
    if end == -1:
        return ""
    return text[start + len(open_tag):end].strip()


def command_name_text(text: str) -> str:
    """The envelope's <command-name> inner text, e.g. "/plan"."""
    open_tag, close_tag = "<command-name>", "</command-name>"
    start = text.find(open_tag)
    if start == -1:
        return ""
    end = text.find(close_tag, start + len(open_tag))
    if end == -1:
        return ""
    return text[start + len(open_tag):end].strip()


def envelope_prompt(text: str) -> str:
    """The prompt a command envelope journals: the typed <command-args>
    when the owner supplied any, else the command name itself — a bare `/plan` is still an owner-typed
    instruction, and a corpus that cannot record it cannot explain the
    turn that follows."""
    args = command_args_text(text)
    if args:
        return args
    return command_name_text(text)


def read_entries(transcript: str) -> list[dict] | None:
    """Read every complete JSON object currently durable in a transcript.
    A final line being appended concurrently is ignored on this pass and
    reconsidered by the settle loop."""
    try:
        entries = []
        with open(transcript, encoding="utf-8") as fh:
            for line in fh:
                line = line.strip()
                if line:
                    try:
                        entries.append(json.loads(line))
                    except Exception:
                        continue
        return entries
    except Exception:
        return None


def turn_identity(entry: dict, index: int) -> str:
    """Return Claude's durable user UUID or a transcript-position fallback."""
    value = entry.get("uuid")
    return value if isinstance(value, str) and value else f"idx{index}"


def completed_turn(entries: list, turn_id: str) -> tuple[str, list[str]] | None:
    """Project one transcript turn only after terminal text is present.

    The window runs from the turn-starting entry to the end of the
    transcript: machine-injected entries never close it, so a turn resumed
    after a background-agent notification keeps the responses on both sides
    of it. Every terminal response accumulates in order, joined by one blank
    line — Claude Code can append a thinking-only ``end_turn`` entry first
    and the user-visible text as a second ``end_turn`` entry shortly
    afterward, and only entries carrying terminal *text* contribute.
    Progress prose and tool calls cannot make a turn publishable."""
    start_idx = next(
        (
            i
            for i, entry in enumerate(entries)
            if turn_identity(entry, i) == turn_id
            and classify_user_entry(entry) in (OWNER_TYPED, COMMAND_ENVELOPE)
        ),
        None,
    )
    if start_idx is None:
        return None

    terminals: list[str] = []
    tools: list[str] = []
    for o in entries[start_idx + 1:]:
        if classify_user_entry(o) in (OWNER_TYPED, COMMAND_ENVELOPE):
            break
        if o.get("isSidechain"):
            # A subagent's own exchange, not the assistant answering the
            # owner: it neither ends the window nor contributes output.
            continue
        if o.get("type") != "assistant":
            continue
        message = o.get("message", {})
        c = message.get("content")
        if isinstance(c, str):
            if message.get("stop_reason") == "end_turn" and c.strip():
                terminals.append(c.strip())
            continue
        if not isinstance(c, list):
            continue
        texts = [
            b.get("text", "").strip()
            for b in c
            if isinstance(b, dict)
            and b.get("type") == "text"
            and b.get("text", "").strip()
        ]
        names = [
            b.get("name")
            for b in c
            if isinstance(b, dict)
            and b.get("type") == "tool_use"
            and valid_token(b.get("name"))
        ]
        if message.get("stop_reason") == "end_turn" and texts:
            terminals.append("\n".join(texts))
        tools.extend(n for n in names if n not in tools)
    if not terminals:
        return None
    return "\n\n".join(terminals), tools


def wait_for_completed_turn(transcript: str, turn_id: str) -> tuple[str, list[str]] | None:
    """Poll for terminal text, then require a short unchanged quiet period.

    The quiet interval lets the terminal set finish growing if Claude Code
    is still appending settled responses when Stop starts."""
    deadline = time.monotonic() + TRANSCRIPT_SETTLE_TIMEOUT_S
    candidate = None
    candidate_since = 0.0
    while True:
        entries = read_entries(transcript)
        if entries is None:
            return None
        completed = completed_turn(entries, turn_id)
        now = time.monotonic()
        if completed is None:
            candidate = None
        elif completed != candidate:
            candidate = completed
            candidate_since = now
        elif now - candidate_since >= TRANSCRIPT_QUIET_INTERVAL_S:
            return completed
        remaining = deadline - now
        if remaining <= 0:
            return None
        time.sleep(min(TRANSCRIPT_POLL_INTERVAL_S, remaining))


def main() -> int:
    try:
        payload = json.load(sys.stdin)
    except Exception:
        return 0

    sid = payload.get("session_id") or ""
    transcript = payload.get("transcript_path") or ""
    cwd = payload.get("cwd") or os.getcwd()
    if not sid or not transcript:
        return 0
    binary = resolve_binary()
    if binary is None:
        return 0

    entries = read_entries(transcript)
    if entries is None:
        return 0

    # The most recent owner-typed or command-envelope entry starts the turn
    # to capture; machine-injected entries never do. Nothing owner-typed
    # anywhere means the turn was machine-driven, and is skipped.
    last_user_idx = None
    kind = None
    for i in range(len(entries) - 1, -1, -1):
        kind = classify_user_entry(entries[i])
        if kind in (OWNER_TYPED, COMMAND_ENVELOPE):
            last_user_idx = i
            break
    if last_user_idx is None:
        return 0

    text = entry_text(entries[last_user_idx])
    if kind == COMMAND_ENVELOPE:
        prompt = envelope_prompt(text)
    else:
        prompt = strip_synthetic_blocks(text)
    if len(prompt) < 3:
        return 0
    turn_id = turn_identity(entries[last_user_idx], last_user_idx)

    # The transcript's own timestamp keeps the payload deterministic, so a
    # repeated Stop for the same turn dedupes instead of conflicting.
    event_time_ms = int(time.time() * 1000)
    try:
        stamp = entries[last_user_idx].get("timestamp")
        if stamp:
            event_time_ms = int(
                datetime.fromisoformat(stamp.replace("Z", "+00:00")).timestamp() * 1000
            )
    except Exception:
        pass

    completed = wait_for_completed_turn(transcript, turn_id)
    if completed is None:
        return 0
    agent, tools = completed

    # Identity (session, turn, policy) is deterministic, so a repeated Stop
    # for the same turn is a duplicate the binary absorbs; a re-delivery
    # whose assistant result strictly extends the stored one supersedes it
    # in place, and anything divergent is a conflict the first capture wins.
    capture = {
        "schema_version": 1,
        "lane": "conversation",
        "harness": HARNESS,
        "adapter_version": ADAPTER_VERSION,
        "session_id": sid,
        "turn_id": turn_id,
        "event_time_ms": event_time_ms,
        "capture_policy": CAPTURE_POLICY,
        "turn_outcome": "completed",
        "user_content": prompt,
        "assistant_result": agent,
        "tools": [{"name": n} for n in tools[:256]],
    }
    ws = workspace_root(cwd)
    if ws is not None:
        capture["workspace_root"] = ws
    host = origin_host()
    if host is not None:
        capture["host"] = host

    cmd = [binary, "capture"]
    root = os.environ.get("AUTOJOURNAL_HOOK_ROOT")
    index = os.environ.get("AUTOJOURNAL_HOOK_INDEX")
    if root:
        cmd += ["--root", root]
    if index:
        cmd += ["--index", index]
    try:
        subprocess.run(
            cmd,
            input=json.dumps(capture).encode(),
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            timeout=30,
        )
    except Exception:
        pass
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception:
        sys.exit(0)  # never block the session
