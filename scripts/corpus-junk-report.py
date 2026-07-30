#!/usr/bin/env python3
"""Recall-hygiene report: rank sessions by synthetic/junk content mass.

Scans rendered episodes in an AutoJournal corpus and scores each session by
how much of its content is machine-shaped rather than conversational: tool
call captures, shell transcripts, structured blobs (JSON/hex/base64/paths),
and repeated boilerplate. Sessions that are mostly such material contribute
index mass that can crowd real context out of recall.

Report-only: nothing is modified. The ranked output is meant for owner
review; removal (delete episode files, then `autojournal sync`) and import
exclusion are deliberate follow-up steps.
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from collections import defaultdict
from pathlib import Path

FM_LINE = re.compile(r"^([a-z_]+): (.*)$")

TOOLCALL = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*\(\{")          # Bash({"command": …
SHELL = re.compile(r"^\$ |^> \$ |^\+ |^(read|write|edit|bash) /")  # transcript lines
BLOBBY = re.compile(r"[{}\[\]<>|\\]|::|—>|sha256:|0x[0-9a-f]{6,}")
LONG_TOKEN = re.compile(r"\S{48,}")
WORD = re.compile(r"[A-Za-z]{2,}")


def classify_line(line: str) -> str:
    s = line.strip()
    if not s:
        return "blank"
    if TOOLCALL.match(s) or SHELL.match(s):
        return "tool"
    if LONG_TOKEN.search(s):
        return "blob"
    words = WORD.findall(s)
    alpha = sum(len(w) for w in words)
    if alpha / max(len(s), 1) < 0.5 or len(words) < 3:
        return "blob" if BLOBBY.search(s) else "other"
    return "prose"


def scan_episode(path: Path) -> dict | None:
    text = path.read_text(encoding="utf-8", errors="replace")
    lines = text.split("\n")
    if not lines or lines[0] != "---":
        return None
    fm = {}
    body_start = 0
    for i in range(1, len(lines)):
        if lines[i] == "---":
            body_start = i + 1
            break
        m = FM_LINE.match(lines[i])
        if m:
            fm[m.group(1)] = m.group(2)
    counts = defaultdict(int)
    body = [l for l in lines[body_start:] if not l.startswith("## ")]
    for line in body:
        counts[classify_line(line)] += 1
    content = [l for l in body if l.strip()]

    # A turn whose User section opens with a harness-generated block was not
    # typed by the owner: the session advanced on machine input.
    synthetic_user = False
    for i in range(body_start, len(lines)):
        if lines[i] == "## User":
            for l in lines[i + 1:]:
                if l.strip():
                    synthetic_user = l.lstrip().startswith(
                        ("<task-notification>", "<local-command", "<command-name>", "<system-")
                    )
                    break
            break
    return {
        "fm": fm,
        "counts": dict(counts),
        "bytes": len(text.encode()),
        "lines": content,
        "synthetic_user": synthetic_user,
    }


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--root", default="~/memory/journals", help="journal root to scan")
    ap.add_argument("--top", type=int, default=25, help="sessions to show")
    ap.add_argument("--min-bytes", type=int, default=4096,
                    help="ignore sessions smaller than this (too small to poison)")
    ap.add_argument("--json", default=None, help="write full per-session detail to this file")
    args = ap.parse_args()

    root = Path(args.root).expanduser()
    if not root.is_dir():
        print(f"{root} is not a directory", file=sys.stderr)
        return 2

    sessions: dict[str, dict] = {}
    n_episodes = 0
    for path in sorted(root.rglob("aj1-*.md")):
        ep = scan_episode(path)
        if ep is None:
            continue
        n_episodes += 1
        sid = ep["fm"].get("session_id", "?")
        s = sessions.setdefault(sid, {
            "harness": ep["fm"].get("harness", "?"),
            "lane": ep["fm"].get("lane", "?"),
            "outcome": ep["fm"].get("turn_outcome", "?"),
            "dates": set(),
            "episodes": 0,
            "bytes": 0,
            "counts": defaultdict(int),
            "lines": [],
            "paths": [],
        })
        s["episodes"] += 1
        s["synthetic_turns"] = s.get("synthetic_turns", 0) + (1 if ep["synthetic_user"] else 0)
        s["bytes"] += ep["bytes"]
        s["dates"].add(ep["fm"].get("event_time", "")[:10])
        for k, v in ep["counts"].items():
            s["counts"][k] += v
        s["lines"].extend(ep["lines"])
        s["paths"].append(str(path.relative_to(root)))

    rows = []
    for sid, s in sessions.items():
        if s["bytes"] < args.min_bytes:
            continue
        c = s["counts"]
        content = c.get("prose", 0) + c.get("tool", 0) + c.get("blob", 0) + c.get("other", 0)
        if content == 0:
            continue
        prose = c.get("prose", 0) / content
        tool = c.get("tool", 0) / content
        blob = c.get("blob", 0) / content
        uniq = len(set(s["lines"])) / max(len(s["lines"]), 1)
        synth = s.get("synthetic_turns", 0) / s["episodes"]
        # Junk mass: bytes of the session discounted by how conversational,
        # non-repetitive, and owner-driven it is. High = big, machine-shaped,
        # machine-driven.
        junk = max((1.0 - prose) * (2.0 - uniq) / 2.0, synth)
        rows.append({
            "session": sid,
            "harness": s["harness"],
            "lane": s["lane"],
            "outcome": s["outcome"],
            "dates": "..".join(sorted(d for d in s["dates"] if d)[:1] + sorted(s["dates"])[-1:]),
            "episodes": s["episodes"],
            "kb": s["bytes"] / 1024,
            "prose": prose,
            "tool": tool,
            "blob": blob,
            "dup": 1.0 - uniq,
            "synth_turns": synth,
            "junk": junk,
            "junk_kb": junk * s["bytes"] / 1024,
            "sample": next((l.strip()[:100] for l in s["lines"]
                            if classify_line(l) in ("tool", "blob")), ""),
            "paths": s["paths"],
        })

    rows.sort(key=lambda r: r["junk_kb"], reverse=True)

    print(f"{n_episodes} episodes in {len(sessions)} sessions under {root}")
    print(f"top {min(args.top, len(rows))} by junk mass "
          f"(junk = (1-prose)·(2-uniqueness)/2, mass = junk·size):\n")
    hdr = (f"{'junk_kb':>8} {'kb':>7} {'eps':>4} {'prose':>6} {'tool':>5} {'blob':>5} "
           f"{'dup':>5} {'synth':>5}  {'harness':<11} {'lane':<15} {'dates':<21} session")
    print(hdr)
    for r in rows[: args.top]:
        print(f"{r['junk_kb']:8.1f} {r['kb']:7.1f} {r['episodes']:4d} "
              f"{r['prose']:6.0%} {r['tool']:5.0%} {r['blob']:5.0%} {r['dup']:5.0%} "
              f"{r['synth_turns']:5.0%}  "
              f"{r['harness']:<11} {r['lane']:<15} {r['dates']:<21} {r['session']}")
        if r["sample"]:
            print(f"{'':8} sample: {r['sample']}")

    if args.json:
        with open(args.json, "w") as fh:
            json.dump(rows, fh, indent=1, default=str)
        print(f"\nfull detail -> {args.json}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
