// Re-parses rendered episode frontmatter for index sync and rebuild.
//
// Stored data is untrusted at the read boundary: a hand-edited or corrupt
// file yields nil and is excluded with visible diagnostics, never a crash
// and never a merged-by-filename guess.

package autojournal

import (
	"strconv"
	"strings"
)

// Episode is the parsed frontmatter view of one stored episode file. All
// strings borrow from the content passed to ParseEpisode.
type Episode struct {
	EpisodeID     string
	World         string
	Scope         string
	Lane          Lane
	Harness       string
	SessionID     string
	TurnID        string
	EventTimeMs   uint64
	CaptureTimeMs uint64
	CapturePolicy string
	TurnOutcome   string
	DigestHex     string
	// BodyLine is the 1-based line number of the first line after the
	// closing `---`. Frontmatter is metadata, not memory: indexing and
	// snippet clamping start here.
	BodyLine uint32
	// BodyOffset is the byte offset of that same first body line.
	BodyOffset int
}

// ParseEpisode re-reads the leading frontmatter block of a stored episode.
// Unknown keys are tolerated on read (a newer writer may add fields); any
// missing, malformed, or contract-violating required value yields nil.
func ParseEpisode(content string) *Episode {
	if !strings.HasPrefix(content, "---\n") {
		return nil
	}
	var (
		schema        string
		episodeID     string
		world         string
		scope         string
		lane          Lane
		harness       string
		sessionID     string
		turnID        string
		eventTimeMs   uint64
		captureTimeMs uint64
		capturePolicy string
		turnOutcome   string
		digestHex     string
	)
	seen := map[string]bool{}

	rest := content[len("---\n"):]
	offset := len("---\n")
	lineNo := uint32(1) // the opening `---` is line 1
	closed := false
	for len(rest) > 0 {
		lineEnd := strings.IndexByte(rest, '\n')
		if lineEnd < 0 {
			break
		}
		line := rest[:lineEnd]
		rest = rest[lineEnd+1:]
		offset += lineEnd + 1
		lineNo++
		if line == "---" {
			closed = true
			break
		}
		sep := strings.Index(line, ": ")
		if sep < 0 {
			return nil
		}
		key, value := line[:sep], line[sep+2:]
		switch key {
		case "schema":
			schema = value
		case "episode_id":
			episodeID = value
		case "world":
			world = value
		case "scope":
			scope = value
		case "lane":
			switch Lane(value) {
			case LaneConversation, LaneDelegatedWork, LaneEvaluation, LaneImportedLegacy:
				lane = Lane(value)
			default:
				return nil
			}
		case "harness":
			harness = value
		case "session_id":
			sessionID = value
		case "turn_id":
			turnID = value
		case "event_time_ms":
			n, ok := parseFrontmatterUint(value)
			if !ok {
				return nil
			}
			eventTimeMs = n
		case "capture_time_ms":
			n, ok := parseFrontmatterUint(value)
			if !ok {
				return nil
			}
			captureTimeMs = n
		case "capture_policy":
			capturePolicy = value
		case "turn_outcome":
			turnOutcome = value
		case "payload_digest":
			if !strings.HasPrefix(value, DigestPrefix) {
				return nil
			}
			hexPart := value[len(DigestPrefix):]
			if len(hexPart) != DigestHexLen {
				return nil
			}
			digestHex = hexPart
		default:
			// Unknown keys are tolerated on read.
		}
		seen[key] = true
	}
	if !closed {
		return nil
	}
	if schema != EpisodeSchema {
		return nil
	}
	for _, k := range []string{
		"episode_id", "world", "scope", "lane", "harness", "session_id",
		"turn_id", "event_time_ms", "capture_time_ms", "capture_policy",
		"turn_outcome", "payload_digest",
	} {
		if !seen[k] {
			return nil
		}
	}
	ep := &Episode{
		EpisodeID:     episodeID,
		World:         world,
		Scope:         scope,
		Lane:          lane,
		Harness:       harness,
		SessionID:     sessionID,
		TurnID:        turnID,
		EventTimeMs:   eventTimeMs,
		CaptureTimeMs: captureTimeMs,
		CapturePolicy: capturePolicy,
		TurnOutcome:   turnOutcome,
		DigestHex:     digestHex,
		BodyLine:      lineNo + 1,
		BodyOffset:    offset,
	}
	// The read boundary revalidates the charsets capture enforced; the
	// scope check is deliberately the looser token rule, matching the Zig
	// reference (a legacy file may predate the stricter write-side rule).
	if !ValidWorld(ep.World) {
		return nil
	}
	for _, s := range []string{
		ep.Scope, ep.Harness, ep.SessionID, ep.TurnID,
		ep.CapturePolicy, ep.TurnOutcome,
	} {
		if !ValidToken(s) {
			return nil
		}
	}
	return ep
}

// parseFrontmatterUint parses a frontmatter integer value: bare decimal
// digits only. Rendered files never carry signs or whitespace; a
// hand-edited value that does is treated as corruption, not interpreted.
func parseFrontmatterUint(s string) (uint64, bool) {
	if len(s) == 0 {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
