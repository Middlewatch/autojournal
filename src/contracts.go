// Closed, versioned capture contracts and typed outcomes.
//
// The wire payload is one JSON object. The schema is closed: unknown fields,
// duplicate fields, missing fields, and over-budget values are all rejected
// with typed reasons rather than best-effort acceptance.
//
// JSON strictness note: this payload is a closed wire format, the documented
// exception to the house encoding/json default. encoding/json alone cannot
// reject duplicate object keys and silently replaces invalid UTF-8, so
// ParsePayload runs a strict token walk before typed extraction. Field
// metadata still lives on the wire names; there is no escaping layer.

package autojournal

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// PayloadSchemaVersion is the only capture wire version accepted.
const PayloadSchemaVersion = 1

// EpisodeSchema stamps every rendered episode's frontmatter.
const EpisodeSchema = "aj-episode.v1"

// Size and shape budgets.
const (
	MaxPayloadBytes = 4 * 1024 * 1024
	MaxContentBytes = 2 * 1024 * 1024
	// MaxEpisodeFileBytes is the read budget for one rendered episode file:
	// the file carries frontmatter and escaping overhead on top of payload
	// content, so it may legitimately exceed MaxPayloadBytes.
	MaxEpisodeFileBytes = MaxPayloadBytes * 2
	// CorpusWalkDepth bounds directory descent below the journal root. The
	// deepest supported layout is
	// worlds/<world>/scopes/<scope>/lanes/<lane>/YYYY/MM/DD/file.
	CorpusWalkDepth = 10
	MaxWorldLen     = 64
	MaxTokenLen     = 128
	MaxTools        = 256
	// MaxPathLen bounds the optional provenance paths (workspace root,
	// branch-of).
	MaxPathLen = 512
)

// Retrieval bounds. Search returns ranked references and bounded snippets;
// Get opens one bounded span. Neither ever returns an unbounded episode body.
const (
	MaxQueryBytes       = 4096
	MaxQueryTerms       = 64
	MaxSnippetLineBytes = 400
	MaxSnippetBytes     = 4096
	MaxGetLines         = 400
	MaxGetBytes         = 64 * 1024
	MaxResultsLimit     = 100
)

// Lane distinguishes normal conversation, delegated work, evaluation, and
// explicit imported legacy source. It is a system record type, never a
// user folder choice.
type Lane string

const (
	LaneConversation   Lane = "conversation"
	LaneDelegatedWork  Lane = "delegated_work"
	LaneEvaluation     Lane = "evaluation"
	LaneImportedLegacy Lane = "imported_legacy"
)

// ValidLane reports whether l is one of the four contract lanes. The
// library refuses an unknown lane itself rather than relying on any one
// caller to have checked first.
func ValidLane(l Lane) bool {
	switch l {
	case LaneConversation, LaneDelegatedWork, LaneEvaluation, LaneImportedLegacy:
		return true
	}
	return false
}

// Event times outside this window are refused at Validate with
// ErrImplausibleEventTime: a wrapped or garbage timestamp would
// otherwise shard the episode into a nonsense date directory. The bounds
// are deliberately wide — the epoch through 9999-12-31T23:59:59Z — because
// the contract's job is refusing nonsense, not judging clocks.
const (
	MinEventTimeMs uint64 = 0
	MaxEventTimeMs uint64 = 253402300799000
)

// CaptureOutcome is the result vocabulary reported to adapters. Published,
// Duplicate, and Superseded are success; everything else is a distinct
// typed failure. Consumers must tolerate values they do not know: the
// vocabulary is an interface-tier contract and grows by minor version.
type CaptureOutcome string

const (
	CapturePublished CaptureOutcome = "published"
	CaptureDuplicate CaptureOutcome = "duplicate"
	// CaptureSuperseded: a redelivery of this episode's own identity was
	// proven to contain the stored content, and the episode's bytes
	// were replaced in place at its own path. Success: exit 0,
	// indexed on the same branch as published and duplicate.
	CaptureSuperseded       CaptureOutcome = "superseded"
	CaptureConflict         CaptureOutcome = "conflict"
	CaptureMalformed        CaptureOutcome = "malformed"
	CapturePermissionDenied CaptureOutcome = "permission_denied"
	CaptureUnavailable      CaptureOutcome = "unavailable"
	CaptureInternalError    CaptureOutcome = "internal_error"
)

// IndexFreshness is reported independently of source publication.
type IndexFreshness string

const (
	IndexFresh       IndexFreshness = "fresh"
	IndexStale       IndexFreshness = "stale"
	IndexNotBuilt    IndexFreshness = "not_built"
	IndexUnavailable IndexFreshness = "unavailable"
)

// Outcome is the retrieval vocabulary shared by memory_search and
// memory_get. NoMatch is a valid result, not a failure: empty, stale,
// unavailable, malformed, and timed-out are all distinct typed outcomes.
type Outcome string

const (
	OutcomeMatch            Outcome = "match"
	OutcomeNoMatch          Outcome = "no_match"
	OutcomeStaleRevision    Outcome = "stale_revision"
	OutcomeGone             Outcome = "gone"
	OutcomeIndexStale       Outcome = "index_stale"
	OutcomeTimeout          Outcome = "timeout"
	OutcomeUnavailable      Outcome = "unavailable"
	OutcomePermissionDenied Outcome = "permission_denied"
	OutcomeMalformed        Outcome = "malformed"
	OutcomeConflict         Outcome = "conflict"
	OutcomeInternalError    Outcome = "internal_error"
)

// Validation failure vocabulary. ParsePayload returns only ErrMalformed;
// Validate returns one of these typed sentinels so the CLI can map each to
// its contract outcome without string matching.
var (
	ErrUnsupportedSchemaVersion = errors.New("unsupported schema_version")
	ErrInvalidWorld             = errors.New("invalid world")
	ErrInvalidScope             = errors.New("invalid scope")
	ErrInvalidLane              = errors.New("invalid lane")
	ErrInvalidHarness           = errors.New("invalid harness")
	ErrInvalidAdapterVersion    = errors.New("invalid adapter_version")
	ErrInvalidSessionID         = errors.New("invalid session_id")
	ErrInvalidTurnID            = errors.New("invalid turn_id")
	ErrInvalidCapturePolicy     = errors.New("invalid capture_policy")
	ErrInvalidTurnOutcome       = errors.New("invalid turn_outcome")
	ErrImplausibleEventTime     = errors.New("implausible event_time_ms")
	ErrEmptyUserContent         = errors.New("empty user_content")
	ErrEmptyAssistantResult     = errors.New("empty assistant_result")
	ErrOversizedContent         = errors.New("oversized content")
	ErrInvalidUTF8              = errors.New("invalid utf-8")
	ErrTooManyTools             = errors.New("too many tools")
	ErrInvalidToolName          = errors.New("invalid tool name")
	ErrInvalidWorkspaceRoot     = errors.New("invalid workspace_root")
	ErrInvalidBranchOf          = errors.New("invalid branch_of")
	ErrInvalidHost              = errors.New("invalid host")
	// ErrMalformed covers every parse-level rejection: over-budget bytes,
	// invalid JSON, duplicate or unknown fields, missing required fields,
	// and wrong value types.
	ErrMalformed = errors.New("malformed payload")
)

// Tool is the allowlisted safe metadata for one tool call: its name only.
type Tool struct {
	Name string
}

// RawPayload is the wire shape prior to validation. World and Scope may be
// omitted and filled from owner defaults; a first-class host may provide
// them when transporting an explicit owner session selection. Every other
// field is a lifecycle fact the adapter knows.
type RawPayload struct {
	SchemaVersion   uint64
	World           *string
	Scope           *string
	Lane            string
	Harness         string
	AdapterVersion  string
	SessionID       string
	TurnID          string
	EventTimeMs     uint64
	CapturePolicy   string
	TurnOutcome     string
	UserContent     string
	AssistantResult string
	Tools           []Tool // nil when the wire omitted the field
	// Optional session provenance: where the turn happened. Omitted when
	// the adapter does not know them, and excluded from the payload digest
	// like other capture-source metadata, so a faithful re-delivery still
	// dedupes.
	WorkspaceRoot *string
	BranchOf      *string
	Host          *string
}

// Payload is a validated capture payload.
type Payload struct {
	World           string
	Scope           string
	Lane            Lane
	Harness         string
	AdapterVersion  string
	SessionID       string
	TurnID          string
	EventTimeMs     uint64
	CapturePolicy   string
	TurnOutcome     string
	UserContent     string
	AssistantResult string
	Tools           []Tool
	WorkspaceRoot   *string
	BranchOf        *string
	Host            *string
}

// ValidWorld reports whether s names a directory component: lowercase
// alphanumeric plus '-', bounded, never starting with '.' (enforced by
// charset).
func ValidWorld(s string) bool {
	if len(s) == 0 || len(s) > MaxWorldLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
		default:
			return false
		}
	}
	return true
}

// ValidToken reports whether s is a safe identity token (session, turn,
// harness, policy, scope, outcome, adapter version): printable, no
// whitespace/control bytes, so it embeds safely in frontmatter lines and
// canonical digest input.
func ValidToken(s string) bool {
	if len(s) == 0 || len(s) > MaxTokenLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '.', '_', '-', ':', '+', '/', '@':
			continue
		}
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		default:
			return false
		}
	}
	return true
}

// ValidPath reports whether s is safe as a frontmatter line value.
// Provenance paths are never directory components or digest input, so the
// rule is line safety, not a charset: bounded, valid UTF-8, and free of
// control bytes (which is what keeps the line-oriented frontmatter grammar
// unbreakable). Spaces and non-ASCII are legitimate in real filesystem
// paths and are allowed.
func ValidPath(s string) bool {
	if len(s) == 0 || len(s) > MaxPathLen {
		return false
	}
	if !utf8.ValidString(s) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// ValidScope reports whether s is a usable scope name. Scopes are both
// frontmatter tokens and directory components; unlike general identity
// tokens they cannot contain a path separator, name a traversal
// component, or start with '.' — WalkCorpus skips dot-directories as
// foreign tooling state, so a dot-led scope would publish episodes the
// corpus walk (sync, freshness, reseal) could never see. Worlds enforce
// the same invariant through their charset.
func ValidScope(s string) bool {
	if !ValidToken(s) || s[0] == '.' {
		return false
	}
	return !strings.Contains(s, "/")
}

// requiredKeys and optionalKeys define the closed wire object.
var requiredKeys = []string{
	"schema_version", "lane", "harness", "adapter_version",
	"session_id", "turn_id", "event_time_ms", "capture_policy",
	"turn_outcome", "user_content", "assistant_result",
}

var optionalKeys = map[string]bool{
	"world": true, "scope": true, "tools": true,
	"workspace_root": true, "branch_of": true, "host": true,
}

// ParsePayload parses the wire bytes into a RawPayload. Every parse-level
// problem — over budget, invalid JSON, duplicate or unknown fields, missing
// required fields, wrong value types — collapses to ErrMalformed. One typed
// outcome is the honest answer because every one of them has the same remedy
// for the adapter that sent it: fix the payload.
func ParsePayload(b []byte) (RawPayload, error) {
	var raw RawPayload
	if len(b) > MaxPayloadBytes {
		return raw, ErrMalformed
	}
	if err := rejectDuplicateKeys(b); err != nil {
		return raw, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return raw, ErrMalformed
	}
	for _, k := range requiredKeys {
		if _, ok := fields[k]; !ok {
			return raw, ErrMalformed
		}
	}
	for k := range fields {
		if !optionalKeys[k] && !contains(requiredKeys, k) {
			return raw, ErrMalformed
		}
	}
	var err error
	if raw.SchemaVersion, err = reqUint(fields, "schema_version", 32); err != nil {
		return raw, err
	}
	if raw.EventTimeMs, err = reqUint(fields, "event_time_ms", 64); err != nil {
		return raw, err
	}
	if raw.Lane, err = reqString(fields, "lane"); err != nil {
		return raw, err
	}
	if raw.Harness, err = reqString(fields, "harness"); err != nil {
		return raw, err
	}
	if raw.AdapterVersion, err = reqString(fields, "adapter_version"); err != nil {
		return raw, err
	}
	if raw.SessionID, err = reqString(fields, "session_id"); err != nil {
		return raw, err
	}
	if raw.TurnID, err = reqString(fields, "turn_id"); err != nil {
		return raw, err
	}
	if raw.CapturePolicy, err = reqString(fields, "capture_policy"); err != nil {
		return raw, err
	}
	if raw.TurnOutcome, err = reqString(fields, "turn_outcome"); err != nil {
		return raw, err
	}
	if raw.UserContent, err = reqString(fields, "user_content"); err != nil {
		return raw, err
	}
	if raw.AssistantResult, err = reqString(fields, "assistant_result"); err != nil {
		return raw, err
	}
	if raw.World, err = optString(fields, "world"); err != nil {
		return raw, err
	}
	if raw.Scope, err = optString(fields, "scope"); err != nil {
		return raw, err
	}
	if raw.WorkspaceRoot, err = optString(fields, "workspace_root"); err != nil {
		return raw, err
	}
	if raw.BranchOf, err = optString(fields, "branch_of"); err != nil {
		return raw, err
	}
	if raw.Host, err = optString(fields, "host"); err != nil {
		return raw, err
	}
	if raw.Tools, err = optTools(fields); err != nil {
		return raw, err
	}
	return raw, nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// reqString extracts a required string field, rejecting JSON null and
// non-strings (encoding/json would otherwise silently zero-value them).
func reqString(fields map[string]json.RawMessage, key string) (string, error) {
	raw := fields[key]
	if bytes.Equal(raw, []byte("null")) {
		return "", ErrMalformed
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", ErrMalformed
	}
	return s, nil
}

// optString extracts an optional string field: absent or JSON null maps to
// nil, anything else must be a string.
func optString(fields map[string]json.RawMessage, key string) (*string, error) {
	raw, ok := fields[key]
	if !ok || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, ErrMalformed
	}
	return &s, nil
}

// reqUint extracts a required unsigned integer field. Only bare decimal
// digits are accepted — no sign, fraction, or exponent. These fields feed
// identity and digest derivation, so each value must have exactly one textual
// form; accepting 1.0e3 for 1000 would make identity depend on formatting.
func reqUint(fields map[string]json.RawMessage, key string, bitSize int) (uint64, error) {
	raw := bytes.TrimSpace(fields[key])
	for _, c := range raw {
		if c < '0' || c > '9' {
			return 0, ErrMalformed
		}
	}
	if len(raw) == 0 {
		return 0, ErrMalformed
	}
	n, err := strconv.ParseUint(string(raw), 10, bitSize)
	if err != nil {
		return 0, ErrMalformed
	}
	return n, nil
}

// optTools extracts the optional tools array. Each element must be an
// object carrying exactly one "name" string — the closed schema does not
// admit future tool metadata without a schema version bump.
func optTools(fields map[string]json.RawMessage) ([]Tool, error) {
	raw, ok := fields["tools"]
	if !ok || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil, ErrMalformed
	}
	tools := make([]Tool, 0, len(elems))
	for _, e := range elems {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(e, &obj); err != nil {
			return nil, ErrMalformed
		}
		if len(obj) != 1 {
			return nil, ErrMalformed
		}
		nameRaw, ok := obj["name"]
		if !ok || bytes.Equal(nameRaw, []byte("null")) {
			return nil, ErrMalformed
		}
		var name string
		if err := json.Unmarshal(nameRaw, &name); err != nil {
			return nil, ErrMalformed
		}
		tools = append(tools, Tool{Name: name})
	}
	return tools, nil
}

// rejectDuplicateKeys walks the JSON token stream and fails on any
// repeated key inside any object. encoding/json's Unmarshal keeps the last
// duplicate silently, which would leave identity derivation depending on
// which repetition happened to win. A payload that says two things cannot be
// signed as though it said one.
func rejectDuplicateKeys(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := walkValue(dec); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		// Trailing garbage after the one top-level value.
		return ErrMalformed
	}
	return nil
}

func walkValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return ErrMalformed
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil // scalar
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return ErrMalformed
			}
			key, ok := kt.(string)
			if !ok {
				return ErrMalformed
			}
			if seen[key] {
				return ErrMalformed
			}
			seen[key] = true
			if err := walkValue(dec); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil { // closing '}'
			return ErrMalformed
		}
	case '[':
		for dec.More() {
			if err := walkValue(dec); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil { // closing ']'
			return ErrMalformed
		}
	default:
		return ErrMalformed
	}
	return nil
}

// Validate checks a parsed payload against the closed contract and returns
// the capture-ready Payload. The check order is fixed so that a payload with
// several problems always reports the same first failure, which is what lets
// an adapter test against a stable error name.
func Validate(raw RawPayload) (Payload, error) {
	var p Payload
	if raw.SchemaVersion != PayloadSchemaVersion {
		return p, ErrUnsupportedSchemaVersion
	}
	if raw.World == nil {
		return p, ErrInvalidWorld
	}
	if raw.Scope == nil {
		return p, ErrInvalidScope
	}
	if !ValidWorld(*raw.World) {
		return p, ErrInvalidWorld
	}
	if !ValidScope(*raw.Scope) {
		return p, ErrInvalidScope
	}
	if !ValidLane(Lane(raw.Lane)) {
		return p, ErrInvalidLane
	}
	p.Lane = Lane(raw.Lane)
	if raw.EventTimeMs < MinEventTimeMs || raw.EventTimeMs > MaxEventTimeMs {
		return p, ErrImplausibleEventTime
	}
	if !ValidToken(raw.Harness) {
		return p, ErrInvalidHarness
	}
	if !ValidToken(raw.AdapterVersion) {
		return p, ErrInvalidAdapterVersion
	}
	if !ValidToken(raw.SessionID) {
		return p, ErrInvalidSessionID
	}
	if !ValidToken(raw.TurnID) {
		return p, ErrInvalidTurnID
	}
	if !ValidToken(raw.CapturePolicy) {
		return p, ErrInvalidCapturePolicy
	}
	if !ValidToken(raw.TurnOutcome) {
		return p, ErrInvalidTurnOutcome
	}
	if len(raw.UserContent) == 0 {
		return p, ErrEmptyUserContent
	}
	if len(raw.AssistantResult) == 0 {
		return p, ErrEmptyAssistantResult
	}
	if len(raw.UserContent) > MaxContentBytes || len(raw.AssistantResult) > MaxContentBytes {
		return p, ErrOversizedContent
	}
	if !utf8.ValidString(raw.UserContent) || !utf8.ValidString(raw.AssistantResult) {
		return p, ErrInvalidUTF8
	}
	if len(raw.Tools) > MaxTools {
		return p, ErrTooManyTools
	}
	for _, t := range raw.Tools {
		if !ValidToken(t.Name) {
			return p, ErrInvalidToolName
		}
	}
	if raw.WorkspaceRoot != nil && !ValidPath(*raw.WorkspaceRoot) {
		return p, ErrInvalidWorkspaceRoot
	}
	if raw.BranchOf != nil && !ValidPath(*raw.BranchOf) {
		return p, ErrInvalidBranchOf
	}
	if raw.Host != nil && !ValidToken(*raw.Host) {
		return p, ErrInvalidHost
	}
	p.World = *raw.World
	p.Scope = *raw.Scope
	p.Harness = raw.Harness
	p.AdapterVersion = raw.AdapterVersion
	p.SessionID = raw.SessionID
	p.TurnID = raw.TurnID
	p.EventTimeMs = raw.EventTimeMs
	p.CapturePolicy = raw.CapturePolicy
	p.TurnOutcome = raw.TurnOutcome
	p.UserContent = raw.UserContent
	p.AssistantResult = raw.AssistantResult
	p.Tools = raw.Tools
	if p.Tools == nil {
		p.Tools = []Tool{}
	}
	p.WorkspaceRoot = raw.WorkspaceRoot
	p.BranchOf = raw.BranchOf
	p.Host = raw.Host
	return p, nil
}

// CaptureErrorName renders the failure vocabulary carried in a capture
// report's `detail`. These CamelCase names are an Interface-tier contract that
// adapters match on; they are deliberately not snake_cased to match the
// surrounding JSON, because renaming them would break those consumers.
func CaptureErrorName(err error) string {
	for _, m := range []struct {
		err  error
		name string
	}{
		{ErrMalformed, "Malformed"},
		{ErrUnsupportedSchemaVersion, "UnsupportedSchemaVersion"},
		{ErrInvalidWorld, "InvalidWorld"},
		{ErrInvalidScope, "InvalidScope"},
		{ErrInvalidLane, "InvalidLane"},
		{ErrInvalidHarness, "InvalidHarness"},
		{ErrInvalidAdapterVersion, "InvalidAdapterVersion"},
		{ErrInvalidSessionID, "InvalidSessionId"},
		{ErrInvalidTurnID, "InvalidTurnId"},
		{ErrInvalidCapturePolicy, "InvalidCapturePolicy"},
		{ErrInvalidTurnOutcome, "InvalidTurnOutcome"},
		{ErrImplausibleEventTime, "ImplausibleEventTime"},
		{ErrEmptyUserContent, "EmptyUserContent"},
		{ErrEmptyAssistantResult, "EmptyAssistantResult"},
		{ErrOversizedContent, "OversizedContent"},
		{ErrInvalidUTF8, "InvalidUtf8"},
		{ErrTooManyTools, "TooManyTools"},
		{ErrInvalidToolName, "InvalidToolName"},
		{ErrInvalidWorkspaceRoot, "InvalidWorkspaceRoot"},
		{ErrInvalidBranchOf, "InvalidBranchOf"},
		{ErrInvalidHost, "InvalidHost"},
		{ErrContainmentViolation, "ContainmentViolation"},
		{ErrPermissionDenied, "PermissionDenied"},
		{ErrStoreUnavailable, "Unavailable"},
	} {
		if errors.Is(err, m.err) {
			return m.name
		}
	}
	return "Unavailable"
}
