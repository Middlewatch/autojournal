// Parses stored episodes at the read boundary: frontmatter for index sync
// and rebuild, and — because evidence is verified against content, not
// against a recorded claim — the body, re-deriving the canonical digest from
// what the file actually says before that content is served.
//
// Stored data is untrusted here: a hand-edited or corrupt file yields a nil
// parse or a typed verification error and is excluded with visible
// diagnostics, never a crash and never a merged-by-filename guess.

package autojournal

import (
	"errors"
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

// requiredEpisodeKeys are the frontmatter keys every episode must carry
// exactly once: absence and duplication are both refused, since a
// duplicated required key leaves readers free to disagree about which
// line binds.
var requiredEpisodeKeys = map[string]bool{
	"episode_id": true, "world": true, "scope": true, "lane": true,
	"harness": true, "session_id": true, "turn_id": true,
	"event_time_ms": true, "capture_time_ms": true, "capture_policy": true,
	"turn_outcome": true, "payload_digest": true,
}

// ParseEpisode re-reads the leading frontmatter block of a stored episode.
// Unknown keys are tolerated on read (a newer writer may add fields); any
// missing, duplicated, malformed, or contract-violating required value
// yields nil.
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
		// A duplicated required key makes the record ambiguous — readers
		// could disagree about which line wins, and for payload_digest
		// that disagreement turns reseal's success report into a lie. No
		// file this product wrote has one; refuse rather than pick.
		// Duplicated unknown keys stay tolerated: they bind nothing.
		if requiredEpisodeKeys[key] && seen[key] {
			return nil
		}
		seen[key] = true
	}
	if !closed {
		return nil
	}
	if schema != EpisodeSchema {
		return nil
	}
	for k := range requiredEpisodeKeys {
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
	// The read boundary revalidates exactly the charsets capture enforced:
	// scope through ValidScope, matching the write boundary. No
	// episode this product wrote can fail it, so a file that does is a
	// visible, located problem — skipped_malformed — not a tolerated one.
	if !ValidWorld(ep.World) {
		return nil
	}
	if !ValidScope(ep.Scope) {
		return nil
	}
	for _, s := range []string{
		ep.Harness, ep.SessionID, ep.TurnID,
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

// --- Body parsing and digest verification ---

// Body separators, exactly as Render emits them. They are ordinary text an
// owner's content may also contain, which is why parsing enumerates
// candidate splits instead of trusting the first occurrence.
const (
	bodyUserHeader     = "\n## User\n\n"
	bodyAssistantSep   = "\n\n## Assistant\n\n"
	bodyToolsSep       = "\n\n## Tools\n\n"
	bodyToolLinePrefix = "- "
)

// MaxBodyInterpretations caps the candidate readings of one body. A rendered
// body is not injectively decodable: the "## Assistant" and "## Tools"
// separators are ordinary text that owner content may also contain, so one
// byte sequence can be the rendering of several distinct payloads. The cap
// bounds the search; a body exceeding it is reported as unverifiable rather
// than guessed at.
//
// The unit is evaluated candidate *pairs*, not occurrences of either
// separator. The candidate space is the cross product of assistant-separator
// positions and tools-separator positions plus the no-tools reading, so nine
// of each already exceeds this. Enumeration is lazy in render order and stops
// at the cap.
const MaxBodyInterpretations = 64

// VerifiedEpisode is a parsed episode together with the body reading whose
// recomputed digest equals the digest the file records about itself.
type VerifiedEpisode struct {
	Episode
	UserContent     string
	AssistantResult string
	Tools           []Tool
}

var (
	// ErrEpisodeMalformed: the content is not a parseable episode.
	ErrEpisodeMalformed = errors.New("not a parseable episode")
	// ErrDigestMismatch: the content parses, but no reading of its body
	// recomputes to the digest the file records — or the recorded episode
	// id disagrees with the identity its own fields derive, which would
	// otherwise let an edited id line shadow another episode's recall.
	// This is the edited-episode state: excluded from search,
	// stale_revision for get, counted by sync.
	ErrDigestMismatch = errors.New("recorded digest disagrees with content")
)

// recordedIdentityAgrees re-derives the episode id from the five identity
// fields the frontmatter carries and compares it to the recorded id line.
// The payload digest does not cover the id itself — it covers the fields the
// id derives from — so without this check an edited id line would verify
// clean and serve one episode's content under another's identity.
func recordedIdentityAgrees(ep *Episode) bool {
	return EpisodeID(&Payload{
		World:         ep.World,
		Harness:       ep.Harness,
		SessionID:     ep.SessionID,
		TurnID:        ep.TurnID,
		CapturePolicy: ep.CapturePolicy,
	}) == ep.EpisodeID
}

// bodyReading is one candidate decomposition of a body region.
type bodyReading struct {
	userContent     string
	assistantResult string
	tools           []Tool
}

// digestPayload assembles the Payload the digest derivation covers from the
// parsed frontmatter plus one candidate reading.
func digestPayload(ep *Episode, r *bodyReading) *Payload {
	return &Payload{
		World:           ep.World,
		Scope:           ep.Scope,
		Lane:            ep.Lane,
		Harness:         ep.Harness,
		SessionID:       ep.SessionID,
		TurnID:          ep.TurnID,
		EventTimeMs:     ep.EventTimeMs,
		CapturePolicy:   ep.CapturePolicy,
		TurnOutcome:     ep.TurnOutcome,
		UserContent:     r.userContent,
		AssistantResult: r.assistantResult,
		Tools:           r.tools,
	}
}

// parseToolsSection reads a candidate tools section: one or more lines,
// each exactly "- <name>\n" with a name satisfying the tool-name rule.
// Render never emits an empty section, so zero lines is not a reading.
func parseToolsSection(section string) ([]Tool, bool) {
	if section == "" {
		return nil, false
	}
	var tools []Tool
	for len(section) > 0 {
		lineEnd := strings.IndexByte(section, '\n')
		if lineEnd < 0 {
			return nil, false
		}
		line := section[:lineEnd]
		section = section[lineEnd+1:]
		if !strings.HasPrefix(line, bodyToolLinePrefix) {
			return nil, false
		}
		name := line[len(bodyToolLinePrefix):]
		if !ValidToken(name) {
			return nil, false
		}
		tools = append(tools, Tool{Name: name})
	}
	return tools, true
}

// enumerateReadings walks every candidate decomposition of the body region
// in render order — earliest assistant separator first, and within one, the
// no-tools reading before any tools reading — calling visit for each
// structurally valid candidate until visit returns true (stop, found) or the
// enumeration ends. The int return counts the candidates visited.
func enumerateReadings(body string, visit func(*bodyReading) bool) (int, bool) {
	visited := 0
	if !strings.HasPrefix(body, bodyUserHeader) {
		return 0, false
	}
	region := body[len(bodyUserHeader):]
	// Splits examined are bounded too, valid or not: a corrupt body with
	// many assistant separators and no valid reading must return promptly,
	// not scan quadratically. The bound cannot strand a legitimate file —
	// every rendered region ends in a newline, so each earlier split yields
	// a countable no-tools candidate and the pair cap fires first.
	splits := 0
	for from := 0; ; {
		i := strings.Index(region[from:], bodyAssistantSep)
		if i < 0 {
			return visited, false
		}
		splits++
		if splits > MaxBodyInterpretations {
			return visited, false
		}
		split := from + i
		userContent := region[:split]
		rest := region[split+len(bodyAssistantSep):]

		// The no-tools reading: everything up to a final newline.
		if strings.HasSuffix(rest, "\n") {
			visited++
			if visit(&bodyReading{userContent: userContent, assistantResult: rest[:len(rest)-1]}) {
				return visited, true
			}
			if visited >= MaxBodyInterpretations {
				return visited, false
			}
		}
		// Tools readings, earliest separator first.
		for tfrom := 0; ; {
			j := strings.Index(rest[tfrom:], bodyToolsSep)
			if j < 0 {
				break
			}
			tsplit := tfrom + j
			tools, ok := parseToolsSection(rest[tsplit+len(bodyToolsSep):])
			if ok {
				visited++
				if visit(&bodyReading{userContent: userContent, assistantResult: rest[:tsplit], tools: tools}) {
					return visited, true
				}
				if visited >= MaxBodyInterpretations {
					return visited, false
				}
			}
			tfrom = tsplit + 1
		}
		from = split + 1
	}
}

// VerifyEpisode parses content and returns the reading of its body that
// agrees with the recorded digest. Existence, not uniqueness, is the test:
// an unedited file's true reading is always among the candidates, and an
// edited file has no agreeing candidate short of a SHA-256 collision. The
// no-tools reading is a candidate in its own right, not a fallback — a body
// ending in something tools-shaped may be an assistant result that happens to
// look like one.
func VerifyEpisode(content string) (*VerifiedEpisode, error) {
	ep := ParseEpisode(content)
	if ep == nil {
		return nil, ErrEpisodeMalformed
	}
	if !recordedIdentityAgrees(ep) {
		return nil, ErrDigestMismatch
	}
	body := content[ep.BodyOffset:]
	var found *bodyReading
	visited, ok := enumerateReadings(body, func(r *bodyReading) bool {
		if PayloadDigestHex(digestPayload(ep, r)) == ep.DigestHex {
			found = &bodyReading{userContent: r.userContent, assistantResult: r.assistantResult, tools: r.tools}
			return true
		}
		return false
	})
	if ok {
		return &VerifiedEpisode{
			Episode:         *ep,
			UserContent:     found.userContent,
			AssistantResult: found.assistantResult,
			Tools:           found.tools,
		}, nil
	}
	if visited == 0 {
		// No structurally valid reading at all: this body is not a rendering
		// of anything, which is a different state than an edit the digest
		// catches — reseal refuses it rather than re-attesting it.
		return nil, ErrEpisodeMalformed
	}
	return nil, ErrDigestMismatch
}

// ResealDigestHex returns the digest of the first structurally valid candidate
// reading in render order — earliest assistant separator, then the no-tools
// reading before any tools reading — for reseal to write back. Structurally
// valid means the tools section, if taken as one, is entirely `- <name>` lines
// whose names satisfy the tool-name rule. Returns false when the content does
// not parse as an episode at all: reseal re-attests a well-formed edit and
// never repairs a broken file.
//
// On an ambiguous body the chosen reading may not be the decomposition the
// owner had in mind, and after reseal that reading is what VerifyEpisode
// returns to supersede and to get. The alternative — refusing to reseal an
// ambiguous body — would strand exactly the episodes whose content made the
// ambiguity, which is the wrong side to fail on.
func ResealDigestHex(content string) (string, bool) {
	ep := ParseEpisode(content)
	if ep == nil {
		return "", false
	}
	if !recordedIdentityAgrees(ep) {
		// A lying identity line is not re-attestable: identity is
		// corpus-durable and reseal rewrites only the digest line.
		return "", false
	}
	digest := ""
	_, found := enumerateReadings(content[ep.BodyOffset:], func(r *bodyReading) bool {
		digest = PayloadDigestHex(digestPayload(ep, r))
		return true
	})
	return digest, found
}
