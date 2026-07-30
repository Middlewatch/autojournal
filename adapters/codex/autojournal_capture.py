#!/usr/bin/env python3
"""Codex hooks bridge — AutoJournal 1.0 capture.

Publishes each completed Codex turn through the deployed `autojournal
capture` binary (~/.local/bin/autojournal) into the owner-configured
journal root. The binary owns identity, dedupe, atomic publish, and index
freshness; the hook only assembles the turn and ships one payload.

Replaces legacy-ts/codex-hook.mjs, which appended markdown to the legacy
~/memory/journal directly. Wired in ~/.codex/hooks.json for three events,
all reading one JSON payload on stdin:

- UserPromptSubmit stashes {prompt, cwd, time} in a pending file keyed by
  (session_id, turn_id), because Codex's Stop payload carries only the
  last assistant message.
- Stop joins the pending prompt with last_assistant_message and publishes
  one completed turn, then removes the pending file.
- SessionStart emits additionalContext pointing the agent at the
  `autojournal` CLI for recall.

Capture policy codex-stop.v1: the prompt is the owner-typed text Codex
delivered verbatim (no transcript reconstruction, no tool summaries), and
a turn publishes only when both sides are non-trivial. The stashed prompt
time is the episode's event time, so a repeated Stop for the same turn
dedupes instead of conflicting.

Non-blocking: any error → exit 0, no capture, Codex is never disrupted.

Testing override: AUTOJOURNAL_HOOK_ROOT / AUTOJOURNAL_HOOK_INDEX pass
--root/--index to the binary instead of the owner config.
"""

import json
import os
import pathlib
import re
import subprocess
import sys
import time

BINARY = pathlib.Path.home() / ".local" / "bin" / "autojournal"
HARNESS = "codex"
ADAPTER_VERSION = "codex-stop-hook-1.0.0"
CAPTURE_POLICY = "codex-stop.v1"

# A pending prompt whose Stop never arrived (crash, abandoned turn) is
# meaningless after this long and is swept opportunistically.
PENDING_MAX_AGE_S = 48 * 3600


def pending_dir() -> pathlib.Path:
    state = os.environ.get("XDG_STATE_HOME", "").strip() or str(
        pathlib.Path.home() / ".local" / "state"
    )
    return pathlib.Path(state) / "autojournal" / "codex-pending"


def safe_id(value: str) -> str:
    return re.sub(r"[^A-Za-z0-9_.-]", "_", value)[:64]


def pending_path(payload: dict) -> pathlib.Path:
    sid = safe_id(str(payload.get("session_id") or ""))
    turn = safe_id(str(payload.get("turn_id") or ""))
    return pending_dir() / f"{sid}-{turn}.json"


def is_under_willow(cwd: str) -> bool:
    try:
        real = os.path.realpath(cwd)
        root = os.path.realpath("/willow")
    except Exception:
        return False
    return real == root or real.startswith(root + os.sep)


def sweep_stale(now_s: float) -> None:
    try:
        for entry in pending_dir().iterdir():
            try:
                if now_s - entry.stat().st_mtime > PENDING_MAX_AGE_S:
                    entry.unlink()
            except OSError:
                continue
    except OSError:
        pass


def stash_prompt(payload: dict) -> None:
    prompt = payload.get("prompt")
    cwd = payload.get("cwd") or os.getcwd()
    if not isinstance(prompt, str) or len(prompt.strip()) < 3:
        return
    if not is_under_willow(cwd):
        return
    target = pending_path(payload)
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(
        json.dumps(
            {"prompt": prompt, "cwd": cwd, "event_time_ms": int(time.time() * 1000)}
        ),
        encoding="utf-8",
    )
    sweep_stale(time.time())


def capture_stop(payload: dict) -> None:
    assistant = payload.get("last_assistant_message")
    if not isinstance(assistant, str) or assistant.strip() == "":
        return
    source = pending_path(payload)
    if not source.exists():
        return
    try:
        pending = json.loads(source.read_text(encoding="utf-8"))
    except Exception:
        source.unlink(missing_ok=True)
        return
    prompt = pending.get("prompt")
    if not isinstance(prompt, str) or len(prompt.strip()) < 3:
        source.unlink(missing_ok=True)
        return
    if not is_under_willow(str(pending.get("cwd") or "")):
        source.unlink(missing_ok=True)
        return

    event_time_ms = pending.get("event_time_ms")
    if not isinstance(event_time_ms, int):
        event_time_ms = int(time.time() * 1000)

    capture = {
        "schema_version": 1,
        "lane": "conversation",
        "harness": HARNESS,
        "adapter_version": ADAPTER_VERSION,
        "session_id": str(payload.get("session_id") or ""),
        "turn_id": str(payload.get("turn_id") or ""),
        "event_time_ms": event_time_ms,
        "capture_policy": CAPTURE_POLICY,
        "turn_outcome": "completed",
        "user_content": prompt,
        "assistant_result": assistant,
    }

    cmd = [str(BINARY), "capture"]
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
        return
    source.unlink(missing_ok=True)


def session_start(payload: dict) -> None:
    if not is_under_willow(str(payload.get("cwd") or os.getcwd())):
        return
    context = (
        "Persistent memory of past agent sessions is available through the "
        "`autojournal` CLI. Search it with `autojournal search <words> --json` "
        "(curated aliases expand the query) and open exact evidence with "
        "`autojournal get --episode <id> --revision <sha256:...>`. Use it "
        "before re-deriving decisions earlier sessions likely settled. This "
        "session is captured there automatically after each completed turn."
    )
    sys.stdout.write(
        json.dumps(
            {
                "hookSpecificOutput": {
                    "hookEventName": "SessionStart",
                    "additionalContext": context,
                }
            }
        )
    )


def main() -> int:
    try:
        payload = json.load(sys.stdin)
    except Exception:
        return 0
    event = payload.get("hook_event_name")
    if event == "UserPromptSubmit":
        stash_prompt(payload)
    elif event == "Stop":
        capture_stop(payload)
    elif event == "SessionStart":
        session_start(payload)
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception:
        sys.exit(0)  # journaling must never disrupt Codex
