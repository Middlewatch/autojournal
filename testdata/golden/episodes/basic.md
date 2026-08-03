---
schema: aj-episode.v1
episode_id: aj1-2b51a0c261ddfe3de551ddcd9bf03a7d
world: testworld
scope: workspace:demo
lane: conversation
harness: claude-code
adapter_version: 0.1.0
session_id: sess-01
turn_id: turn-0007
event_time: 2026-07-12T13:20:00Z
event_time_ms: 1783862400123
capture_time: 2026-08-03T18:55:24Z
capture_time_ms: 1785783324800
capture_policy: default-v1
turn_outcome: completed
payload_digest: sha256:c3664aa5f523351edd8c571dd5cf8f7be02ae9df77c57d4a98a8dcebe40e3dce
---

## User

How do the naïve tests behave? — ✓

## Assistant

They pass.

```zig
const x = 1;
```

## Tools

- Bash
- Read
