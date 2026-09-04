---
schema: aj-episode.v1
episode_id: aj1-daff9910cf3e098a01e14e49e10812e9
world: main
scope: default
lane: conversation
harness: pi
adapter_version: 2.1.0
session_id: sess-files
turn_id: turn-0001
event_time: 2026-09-04T00:00:00Z
event_time_ms: 1788480000000
capture_time: 2026-09-04T05:33:20Z
capture_time_ms: 1788500000000
capture_policy: pi-visible-v3
turn_outcome: completed
payload_digest: sha256:8e4b608d2fd0832541f5a14ae27ebc4d85e8940f5b2e07afa2d0cc09a931cc42
---

## User

which adr covers the journal format?

## Assistant

ADR 0015 does; the ledger agrees.

Quoted heading in prose: ## Files

## Tools

- read
- bash
- memory_get
- web_fetch
- delegate

## Files

- read ~/.agents/skills/adr/SKILL.md
- read! docs/adr/0016-missing.md
- read ~/.agents/wiki/reference/structural-code-maps-for-agents.md:32-40
- bash ~/.agents/skills/harvest/LEDGER.md
- memory_get aj1-b6cc4862b87e23fd0cddb26f764cb613
- web_fetch docs.example.org
- child:read ~/.agents/wiki/knowledge/zig notes.md
- child:read! ~/.agents/wiki/knowledge/gone.md
