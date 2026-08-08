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

Capture policy cc-stop.v3 — settled terminal-turn projection:
- Harness-generated blocks (task notifications, slash-command echoes,
  system reminders) are stripped from the leading and trailing edges of the
  user prompt before capture; only the owner-typed remainder is journaled.
- A turn whose prompt is nothing but such blocks was machine-driven, not
  owner-driven, and is skipped entirely (its output never enters memory).
- Stop may run before Claude Code has appended its separate terminal-text
  record to the transcript. Capture waits briefly for that record and skips
  the turn if it never settles rather than publishing progress text as the
  result.
- The body contains only the terminal assistant text. Tool names ride in the
  structured tools field; tool arguments and results are never captured.

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
ADAPTER_VERSION = "cc-stop-hook-1.4.0"
CAPTURE_POLICY = "cc-stop.v3"
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


def is_real_user(o: dict) -> bool:
    if o.get("type") != "user":
        return False
    c = o.get("message", {}).get("content")
    if isinstance(c, str):
        return c.strip() != ""
    if isinstance(c, list):
        return not any(isinstance(b, dict) and b.get("type") == "tool_result" for b in c)
    return False


def user_text(o: dict) -> str:
    c = o["message"]["content"]
    if isinstance(c, str):
        return c.strip()
    return "\n".join(
        b.get("text", "") for b in c if isinstance(b, dict) and b.get("type") == "text"
    ).strip()


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

    Claude Code can append a thinking-only ``end_turn`` entry first and the
    user-visible text as a second ``end_turn`` entry shortly afterward. A
    nonempty terminal text record is therefore the settlement condition;
    progress prose and tool calls cannot make a turn publishable."""
    start_idx = next(
        (
            i
            for i, entry in enumerate(entries)
            if turn_identity(entry, i) == turn_id and is_real_user(entry)
        ),
        None,
    )
    if start_idx is None:
        return None

    terminal_text = ""
    tools: list[str] = []
    for o in entries[start_idx + 1:]:
        if is_real_user(o):
            break
        if o.get("type") != "assistant":
            continue
        message = o.get("message", {})
        c = message.get("content")
        if isinstance(c, str):
            if message.get("stop_reason") == "end_turn" and c.strip():
                terminal_text = c.strip()
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
            terminal_text = "\n".join(texts)
        tools.extend(n for n in names if n not in tools)
    if terminal_text == "":
        return None
    return terminal_text, tools


def wait_for_completed_turn(transcript: str, turn_id: str) -> tuple[str, list[str]] | None:
    """Poll for terminal text, then require a short unchanged quiet period.

    The quiet interval lets a second terminal record supersede the first if
    Claude Code is still appending the settled response when Stop starts."""
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

    # latest real user prompt = start of the turn to capture
    last_user_idx = None
    for i in range(len(entries) - 1, -1, -1):
        if is_real_user(entries[i]):
            last_user_idx = i
            break
    if last_user_idx is None:
        return 0

    prompt = strip_synthetic_blocks(user_text(entries[last_user_idx]))
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
    # with grown content is a conflict and the first capture wins.
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
