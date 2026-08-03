// Episode file rendering: closed frontmatter plus Markdown body.
//
// The rendered file is the authoritative artifact. Frontmatter values are
// restricted to validated charsets — identity fields to token charsets,
// provenance paths to control-free single-line text — so no quoting or
// escaping layer is needed; body content is arbitrary validated UTF-8 and
// is never inspected by the frontmatter parser, which stops at the closing
// delimiter.
//
// The byte layout below is frozen and verified byte-for-byte against the
// Zig-produced episodes in testdata/golden/episodes.

package autojournal

import (
	"fmt"
	"strings"
	"time"
)

// RenderInput carries everything render needs beyond the payload itself.
type RenderInput struct {
	Payload       *Payload
	EpisodeID     string
	DigestHex     string
	CaptureTimeMs uint64
}

// Render produces the complete episode file content. It cannot fail:
// every input was already validated into frontmatter-safe charsets.
func Render(in RenderInput) []byte {
	p := in.Payload
	b := new(strings.Builder)
	b.Grow(len(p.UserContent) + len(p.AssistantResult) + 512)
	fmt.Fprintf(b, `---
schema: %s
episode_id: %s
world: %s
scope: %s
lane: %s
harness: %s
adapter_version: %s
session_id: %s
turn_id: %s
event_time: %s
event_time_ms: %d
capture_time: %s
capture_time_ms: %d
capture_policy: %s
turn_outcome: %s
`,
		EpisodeSchema,
		in.EpisodeID,
		p.World,
		p.Scope,
		string(p.Lane),
		p.Harness,
		p.AdapterVersion,
		p.SessionID,
		p.TurnID,
		ISOFromMs(p.EventTimeMs),
		p.EventTimeMs,
		ISOFromMs(in.CaptureTimeMs),
		in.CaptureTimeMs,
		p.CapturePolicy,
		p.TurnOutcome,
	)
	// Optional provenance keys render only when the payload carried them,
	// so episodes from adapters that do not know them stay byte-identical
	// to the pre-provenance rendering.
	if p.WorkspaceRoot != nil {
		fmt.Fprintf(b, "workspace_root: %s\n", *p.WorkspaceRoot)
	}
	if p.BranchOf != nil {
		fmt.Fprintf(b, "branch_of: %s\n", *p.BranchOf)
	}
	if p.Host != nil {
		fmt.Fprintf(b, "host: %s\n", *p.Host)
	}
	fmt.Fprintf(b, `payload_digest: %s%s
---

## User

%s

## Assistant

%s
`,
		DigestPrefix, in.DigestHex,
		p.UserContent,
		p.AssistantResult,
	)
	if len(p.Tools) > 0 {
		b.WriteString("\n## Tools\n\n")
		for _, t := range p.Tools {
			fmt.Fprintf(b, "- %s\n", t.Name)
		}
	}
	return []byte(b.String())
}

// ISOFromMs renders epoch milliseconds as UTC ISO-8601 to second
// precision. Input is unsigned, so pre-epoch times cannot occur by
// construction.
func ISOFromMs(ms uint64) string {
	return time.UnixMilli(int64(ms)).UTC().Format("2006-01-02T15:04:05Z")
}

// FrontmatterDigestHex extracts the digest hex from a rendered episode's
// frontmatter, for the duplicate-vs-conflict decision on redelivery. The
// second return is false when the file has no parseable digest line in its
// leading frontmatter block.
func FrontmatterDigestHex(content string) (string, bool) {
	const key = "payload_digest: " + DigestPrefix
	if !strings.HasPrefix(content, "---\n") {
		return "", false
	}
	rest := content[len("---\n"):]
	for len(rest) > 0 {
		lineEnd := strings.IndexByte(rest, '\n')
		if lineEnd < 0 {
			lineEnd = len(rest)
		}
		line := rest[:lineEnd]
		if line == "---" {
			return "", false
		}
		if strings.HasPrefix(line, key) {
			hexPart := line[len(key):]
			if len(hexPart) != DigestHexLen {
				return "", false
			}
			return hexPart, true
		}
		if lineEnd == len(rest) {
			return "", false
		}
		rest = rest[lineEnd+1:]
	}
	return "", false
}
