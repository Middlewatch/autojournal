#!/usr/bin/env python3
"""One-shot legacy corpus importer for AutoJournal 1.0.

Reads the pre-AutoJournal session Markdown corpus (the legacy v1 journal
written by the Pi extension, the Codex hook, and the Claude Code hook) and
publishes each User/Agent turn pair through `autojournal capture` into the
`imported_legacy` lane. The legacy source is never modified.

Identity contract (docs/AUTOJOURNAL_1_0_DESIGN.md, "Legacy import"): every
turn receives a deterministic synthesized identity — the legacy session id,
a 1-based turn ordinal (`legacy-NNNN`), and the fixed capture policy
`legacy-import.v1` — so re-running the importer yields `duplicate`, never a
second copy. Imported episodes stay distinguishable from native capture
forever via their lane.

All three legacy writers stamped turn blocks with machine-local wall-clock
times; `--tz` names that writing zone so event times convert to true epoch
milliseconds regardless of the host's current timezone.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import subprocess
import sys
from collections import Counter
from datetime import datetime
from pathlib import Path
from zoneinfo import ZoneInfo

MARKER = re.compile(r"^\[(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})\] \[(User|Agent)\]$")
FM_LINE = re.compile(r"^([A-Za-z_]+):\s*(.*)$")

CAPTURE_POLICY = "legacy-import.v1"
ADAPTER_VERSION = "legacy-import-0.1.0"

# Legacy record type -> synthesized turn_outcome token. The lane is always
# imported_legacy; the outcome token preserves what kind of record it was.
RECORD_TYPES = {
    "conversation": "imported-conversation",
    "conversation_journal": "imported-conversation",
    "delegated_work": "imported-delegated-work",
}


class SkipFile(Exception):
    """File is not an importable legacy session; carries the reason."""


def parse_frontmatter(lines: list[str], name: str) -> tuple[dict[str, str], int]:
    if not lines or lines[0] != "---":
        raise SkipFile("no frontmatter")
    fm: dict[str, str] = {}
    for i in range(1, len(lines)):
        if lines[i] == "---":
            return fm, i + 1
        m = FM_LINE.match(lines[i])
        if m:
            fm[m.group(1)] = m.group(2).strip().strip('"')
    raise SkipFile("unterminated frontmatter")


def unquote_agent(text: str) -> str:
    """Exact inverse of the writers' blockquote step: blank lines were
    stored as `>`, everything else as `> ` + line."""
    out = []
    for line in text.split("\n"):
        if line == ">":
            out.append("")
        elif line.startswith("> "):
            out.append(line[2:])
        else:
            out.append(line)
    return "\n".join(out)


def parse_file(path: Path, tz: ZoneInfo) -> tuple[dict[str, str], list[dict]]:
    """Returns (frontmatter, payload fields per turn pair) or raises SkipFile."""
    text = path.read_text(encoding="utf-8")
    lines = text.split("\n")
    fm, body_start = parse_frontmatter(lines, path.name)

    record_type = fm.get("type") or fm.get("kind") or ""
    if record_type not in RECORD_TYPES:
        raise SkipFile(f"record type {record_type!r} is not an importable session")
    session_id = fm.get("session_id", "")
    harness = fm.get("harness", "")
    if not session_id or not harness:
        raise SkipFile("missing session_id or harness")

    markers = []  # (role, timestamp string, line index)
    for i in range(body_start, len(lines)):
        m = MARKER.match(lines[i])
        if m:
            markers.append((m.group(2), m.group(1), i))
    if not markers:
        raise SkipFile("no turn markers")

    roles = "".join("U" if r == "User" else "A" for r, _, _ in markers)
    if re.fullmatch(r"(UA)+", roles) is None:
        raise SkipFile(f"role sequence {roles!r} is not strict User/Agent pairs")

    turns = []
    for pair in range(len(markers) // 2):
        u_role, u_ts, u_idx = markers[2 * pair]
        a_role, a_ts, a_idx = markers[2 * pair + 1]
        seg_end = markers[2 * pair + 2][2] if 2 * pair + 2 < len(markers) else len(lines)
        user_text = "\n".join(lines[u_idx + 1 : a_idx]).strip()
        agent_text = unquote_agent("\n".join(lines[a_idx + 1 : seg_end])).strip()
        if not user_text or not agent_text:
            raise SkipFile(f"empty user or agent segment in pair {pair + 1}")
        local = datetime.strptime(u_ts, "%Y-%m-%d %H:%M:%S").replace(tzinfo=tz)
        turns.append(
            {
                "schema_version": 1,
                "world": "main",
                "scope": "default",
                "lane": "imported_legacy",
                "harness": harness,
                "adapter_version": ADAPTER_VERSION,
                "session_id": session_id,
                "turn_id": f"legacy-{pair + 1:04d}",
                "event_time_ms": int(local.timestamp() * 1000),
                "capture_policy": CAPTURE_POLICY,
                "turn_outcome": RECORD_TYPES[record_type],
                "user_content": user_text,
                "assistant_result": agent_text,
            }
        )
    return fm, turns


def discover(source: Path) -> list[Path]:
    files = sorted(p for p in source.glob("*.md") if p.is_file())
    delegated = source / "delegated"
    if delegated.is_dir():
        files += sorted(p for p in delegated.glob("*.md") if p.is_file())
    return files


def capture(binary: str, payload: dict, root: str | None, index: str | None) -> dict:
    cmd = [binary, "capture"]
    if root:
        cmd += ["--root", root]
    if index:
        cmd += ["--index", index]
    proc = subprocess.run(
        cmd,
        input=json.dumps(payload).encode(),
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    out = proc.stdout.decode(errors="replace").strip()
    try:
        report = json.loads(out)
    except json.JSONDecodeError:
        report = {"outcome": f"unparseable (exit {proc.returncode})", "raw": out[:200]}
    report["exit"] = proc.returncode
    if proc.stderr:
        report["stderr"] = proc.stderr.decode(errors="replace")[:200]
    return report


def resolve_binary(value: str) -> str | None:
    candidate = Path(value).expanduser()
    if candidate.is_file() and os.access(candidate, os.X_OK):
        return str(candidate)
    return shutil.which(value)


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--source", required=True, help="legacy journal directory")
    ap.add_argument("--binary", default="autojournal",
                    help="autojournal executable or path (default: resolve from PATH)")
    ap.add_argument("--tz", required=True,
                    help="timezone the legacy writers stamped local times in")
    ap.add_argument("--root", default=None, help="journal root override (rehearsal); omit to use owner config")
    ap.add_argument("--index", default=None, help="index override (rehearsal)")
    ap.add_argument("--dry-run", action="store_true", help="parse and validate everything, publish nothing")
    ap.add_argument("--limit", type=int, default=None, help="import at most N files (smoke)")
    args = ap.parse_args()

    source = Path(args.source).expanduser()
    binary = resolve_binary(args.binary)
    tz = ZoneInfo(args.tz)
    if not source.is_dir():
        print(f"source {source} is not a directory", file=sys.stderr)
        return 2
    if not args.dry_run and binary is None:
        print(f"binary {args.binary!r} not found or not executable", file=sys.stderr)
        return 2

    files = discover(source)
    if args.limit:
        files = files[: args.limit]

    outcomes: Counter[str] = Counter()
    skipped: list[tuple[str, str]] = []
    failures: list[tuple[str, str, dict]] = []
    files_imported = 0
    turns_seen = 0

    for path in files:
        rel = str(path.relative_to(source))
        try:
            _, turns = parse_file(path, tz)
        except SkipFile as e:
            skipped.append((rel, str(e)))
            continue
        turns_seen += len(turns)
        files_imported += 1
        if args.dry_run:
            continue
        assert binary is not None
        for payload in turns:
            report = capture(binary, payload, args.root, args.index)
            outcome = report.get("outcome", "missing-outcome")
            outcomes[outcome] += 1
            if outcome not in ("published", "duplicate"):
                failures.append((rel, payload["turn_id"], report))

    mode = "dry-run" if args.dry_run else "import"
    print(f"{mode}: {files_imported} session files, {turns_seen} turn pairs"
          f" ({len(files)} files examined)")
    if outcomes:
        print("capture outcomes:", dict(outcomes))
    if skipped:
        print(f"skipped {len(skipped)} file(s):")
        for rel, reason in skipped:
            print(f"  {rel}: {reason}")
    if failures:
        print(f"FAILED captures: {len(failures)}")
        for rel, turn, report in failures[:20]:
            print(f"  {rel} {turn}: {report}")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
