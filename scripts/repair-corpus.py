#!/usr/bin/env python3
"""Repair wrongly-captured cc-stop.v3 episodes by replaying them under v4.

The v3 Claude Code Stop hook could anchor a turn on machine input — a slash
command's expanded body, a task notification, a tool-result carrier — so the
episode records what the harness injected rather than what the owner typed.
The v4 projection classifies every transcript entry before assembling the
turn. This script finds each episode captured under `cc-stop.v3`, locates its
source transcript, replays the recorded turn under the v4 projection —
reanchoring to the owner-typed entry the turn actually belongs to when v3
anchored on machine input — and reports what it would publish and what it
would delete.

Report-only by default: nothing is modified. With --apply it publishes each
replacement through `autojournal capture` (world, scope, and lane from the
v3 episode; turn identity and event time from the replayed transcript entry,
so repeated runs stay deterministic and sibling episodes wrongly split from
one turn dedupe into one replacement), deletes only the v3 file whose
replacement published successfully, and runs `autojournal sync` at the end.
An episode whose transcript no longer exists is reported as unrepairable and
left alone; a turn with nothing owner-typed is left alone too. The script
never deletes a file whose replacement did not publish.
"""

from __future__ import annotations

import argparse
import importlib.util
import json
import os
import pathlib
import re
import shutil
import subprocess
import sys

REPO = pathlib.Path(__file__).resolve().parent.parent
ADAPTER_VERSION = "repair-corpus-1.0.0"
FM_LINE = re.compile(r"^([a-z_]+): (.*)$")

# The publish outcomes under which the v4 content is durable in the corpus.
# `duplicate` and `superseded` mean an identical or extending replay already
# landed; `conflict` and every error outcome mean it did not.
REPLACED_OUTCOMES = frozenset({"published", "duplicate", "superseded"})


def load_v4_projection():
    """The v4 classification and projection live in the Claude Code hook;
    replaying through the same module keeps this script an operator of the
    shipped rules rather than a second implementation of them."""
    path = REPO / "adapters" / "claude-code" / "autojournal_capture.py"
    spec = importlib.util.spec_from_file_location("aj_cc_hook_v4", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def parse_frontmatter(path: pathlib.Path) -> dict | None:
    try:
        lines = path.read_text(encoding="utf-8", errors="replace").split("\n")
    except OSError:
        return None
    if not lines or lines[0] != "---":
        return None
    fm = {}
    for line in lines[1:]:
        if line == "---":
            return fm
        m = FM_LINE.match(line)
        if m:
            fm[m.group(1)] = m.group(2)
    return None


def find_transcript(transcripts: pathlib.Path, session_id: str) -> pathlib.Path | None:
    """Transcripts live one directory per workspace under the harness state
    dir, named by session id — the id is globally unique, so a glob avoids
    re-deriving the workspace-to-directory name munging."""
    if not session_id or not transcripts.is_dir():
        return None
    hits = sorted(transcripts.glob(f"*/{session_id}.jsonl"))
    return hits[0] if hits else None


def entry_time_ms(entry: dict) -> int | None:
    """The entry's transcript timestamp in epoch milliseconds, or None."""
    stamp = entry.get("timestamp")
    if not stamp:
        return None
    try:
        from datetime import datetime
        parsed = datetime.fromisoformat(str(stamp).replace("Z", "+00:00"))
        return int(parsed.timestamp() * 1000)
    except Exception:
        return None


def replay_turn(v4, transcript: pathlib.Path, turn_id: str) -> dict | None:
    """Project the turn a v3 episode recorded, under cc-stop.v4.

    The v3 defect this repairs is anchoring: v3 could start a turn on an
    entry v4 classifies as machine-injected — a slash command's expanded
    body, a tool-result carrier, a harness notification. Under v4 that
    entry belongs to the turn of the nearest owner-typed (or
    command-envelope) entry before it, so when the recorded anchor is
    machine-injected the replay walks back to that entry and projects its
    turn. The replacement carries the reanchored entry's identity and
    transcript timestamp: several wrongly-anchored v3 episodes from one
    owner turn then replay to byte-identical payloads, and the capture
    binary's dedupe collapses them into one replacement instead of
    reporting conflicts between siblings.

    None means v4 would not publish anything here — nothing owner-typed
    precedes the recorded entry (the turn was machine-driven), the prompt
    strips to nothing, or no terminal text ever landed."""
    entries = v4.read_entries(str(transcript))
    if not entries:
        return None
    anchor_idx = next(
        (i for i, e in enumerate(entries) if v4.turn_identity(e, i) == turn_id),
        None,
    )
    if anchor_idx is None:
        return None
    idx, kind = None, None
    for i in range(anchor_idx, -1, -1):
        kind = v4.classify_user_entry(entries[i])
        if kind in (v4.OWNER_TYPED, v4.COMMAND_ENVELOPE):
            idx = i
            break
    if idx is None:
        return None
    entry = entries[idx]
    text = v4.entry_text(entry)
    if kind == v4.COMMAND_ENVELOPE:
        prompt = v4.envelope_prompt(text)
    else:
        prompt = v4.strip_synthetic_blocks(text)
    if len(prompt) < 3:
        return None
    effective_id = v4.turn_identity(entry, idx)
    completed = v4.completed_turn(entries, effective_id)
    if completed is None:
        return None
    body, tools = completed
    return {
        "prompt": prompt,
        "body": body,
        "tools": tools,
        "turn_id": effective_id,
        "reanchored": effective_id != turn_id,
        "event_time_ms": entry_time_ms(entry),
    }


def build_payload(fm: dict, projected: dict) -> dict:
    """The replacement capture payload. Placement fields (world, scope,
    lane) come from the v3 episode so the replay lands beside what it
    repairs. Turn identity and event time come from the replayed turn's own
    transcript entry — for a correctly-anchored episode they equal the v3
    frontmatter, and for a reanchored one they are the identity of the turn
    v4 says the content belongs to; the transcript timestamp keeps the
    replay deterministic across runs. A transcript entry without a
    timestamp falls back to the v3 episode's recorded event time."""
    event_time_ms = projected["event_time_ms"]
    if event_time_ms is None:
        event_time_ms = int(fm["event_time_ms"])
    payload = {
        "schema_version": 1,
        "lane": fm.get("lane", "conversation"),
        "harness": fm.get("harness", "claude-code"),
        "adapter_version": ADAPTER_VERSION,
        "session_id": fm["session_id"],
        "turn_id": projected["turn_id"],
        "event_time_ms": event_time_ms,
        "capture_policy": "cc-stop.v4",
        "turn_outcome": "completed",
        "user_content": projected["prompt"],
        "assistant_result": projected["body"],
        "tools": [{"name": n} for n in projected["tools"][:256]],
    }
    for key in ("world", "scope", "workspace_root", "host"):
        if fm.get(key):
            payload[key] = fm[key]
    return payload


def resolve_binary() -> str | None:
    override = os.environ.get("AUTOJOURNAL_BIN", "").strip()
    if override:
        return override if os.access(override, os.X_OK) else None
    local = pathlib.Path.home() / ".local" / "bin" / "autojournal"
    if os.access(local, os.X_OK):
        return str(local)
    return shutil.which("autojournal")


def run_binary(binary: str, args: list[str], root: str, index: str | None,
               payload: dict | None = None) -> dict | None:
    """Run one autojournal command and parse its JSON report; None when the
    command failed to produce one."""
    cmd = [binary] + args + ["--root", root]
    if index:
        cmd += ["--index", index]
    stdin = json.dumps(payload).encode() if payload is not None else None
    try:
        proc = subprocess.run(
            cmd, input=stdin, capture_output=True, timeout=60,
        )
        return json.loads(proc.stdout.decode())
    except Exception:
        return None


def publish_replacement(binary: str, payload: dict, root: str,
                        index: str | None) -> tuple[bool, str, str | None]:
    """Publish one replacement; (replaced, outcome, replacement_path)."""
    report = run_binary(binary, ["capture"], root, index, payload=payload)
    if report is None:
        return False, "capture-failed", None
    outcome = report.get("outcome", "capture-failed")
    return outcome in REPLACED_OUTCOMES, outcome, report.get("path")


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--root", required=True, help="journal root to repair")
    ap.add_argument("--index", default=None,
                    help="index path passed through to capture and sync")
    ap.add_argument("--transcripts",
                    default=str(pathlib.Path.home() / ".claude" / "projects"),
                    help="directory holding per-workspace transcript dirs")
    ap.add_argument("--apply", action="store_true",
                    help="publish replacements and delete replaced v3 files "
                         "(default is a report that modifies nothing)")
    args = ap.parse_args(argv)

    root = pathlib.Path(args.root).expanduser()
    if not root.is_dir():
        print(f"{root} is not a directory", file=sys.stderr)
        return 2
    transcripts = pathlib.Path(args.transcripts).expanduser()
    binary = resolve_binary() if args.apply else None
    if args.apply and binary is None:
        print("no autojournal binary found (set AUTOJOURNAL_BIN)", file=sys.stderr)
        return 2

    v4 = load_v4_projection()
    counts = {
        "v3_found": 0, "transcript_located": 0, "replayed": 0,
        "reanchored": 0, "would_publish": 0, "would_delete": 0,
        "published": 0, "deleted": 0, "not_published_under_v4": 0,
        "replay_error": 0, "unrepairable": 0,
    }

    for path in sorted(root.rglob("aj1-*.md")):
        fm = parse_frontmatter(path)
        if fm is None or fm.get("capture_policy") != "cc-stop.v3":
            continue
        counts["v3_found"] += 1
        rel = path.relative_to(root)
        if not (fm.get("session_id") and fm.get("turn_id")
                and fm.get("event_time_ms", "").isdigit()):
            counts["unrepairable"] += 1
            print(f"UNREPAIRABLE {rel} — frontmatter incomplete, left alone")
            continue

        transcript = find_transcript(transcripts, fm["session_id"])
        if transcript is None:
            counts["unrepairable"] += 1
            print(f"UNREPAIRABLE {rel} — transcript not found, left alone")
            continue
        counts["transcript_located"] += 1

        try:
            projected = replay_turn(v4, transcript, fm["turn_id"])
        except Exception as exc:
            # One transcript with a shape the projection cannot read must
            # cost that episode's repair, never the rest of the run.
            counts["replay_error"] += 1
            print(f"REPLAY-ERROR {rel} — {type(exc).__name__}, left alone")
            continue
        counts["replayed"] += 1
        if projected is None:
            counts["not_published_under_v4"] += 1
            print(f"NO-TURN      {rel} — nothing owner-typed starts this "
                  "turn under v4, left alone")
            continue

        counts["would_publish"] += 1
        counts["would_delete"] += 1
        if projected["reanchored"]:
            counts["reanchored"] += 1
        anchor_note = (
            f" (reanchored to turn {projected['turn_id']})"
            if projected["reanchored"] else ""
        )
        preview = projected["prompt"].replace("\n", " ")[:80]
        if not args.apply:
            print(f"WOULD-PUBLISH {rel}{anchor_note} — prompt: {preview!r}")
            print(f"WOULD-DELETE  {rel}")
            continue

        payload = build_payload(fm, projected)
        replaced, outcome, new_path = publish_replacement(
            binary, payload, str(root), args.index,
        )
        # The report's path is root-relative; resolve it before the guard,
        # because deleting the file the replacement itself lives in can
        # never be right, whatever the outcome claimed.
        replacement = None
        if new_path:
            replacement = pathlib.Path(new_path)
            if not replacement.is_absolute():
                replacement = root / replacement
        if replaced and replacement is not None \
                and replacement.resolve() != path.resolve():
            counts["published"] += 1
            path.unlink()
            counts["deleted"] += 1
            print(f"REPLACED     {rel}{anchor_note} — {outcome} → {new_path}")
        else:
            print(f"NOT-REPLACED {rel} — outcome {outcome}, v3 file kept")

    if args.apply and counts["published"] > 0:
        # --json, because success is judged by parsing the report: the text
        # form parses as nothing and would report a successful sync as failed.
        report = run_binary(binary, ["sync", "--json"], str(root), args.index)
        synced = "ok" if report is not None else "FAILED — run `autojournal sync` manually"
        print(f"sync: {synced}")

    print()
    print("summary:")
    for key, value in counts.items():
        print(f"  {key}: {value}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
