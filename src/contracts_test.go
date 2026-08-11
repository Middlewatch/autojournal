package autojournal

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// testPayloadJSON mirrors testdata/payloads/basic.json, the contract fixture.
const testPayloadJSON = `{
  "schema_version": 1,
  "world": "testworld",
  "scope": "workspace:demo",
  "lane": "conversation",
  "harness": "claude-code",
  "adapter_version": "0.1.0",
  "session_id": "sess-01",
  "turn_id": "turn-0007",
  "event_time_ms": 1783862400123,
  "capture_policy": "default-v1",
  "turn_outcome": "completed",
  "user_content": "How do the naïve tests behave? — ✓",
  "assistant_result": "They pass.\n\n` + "```" + `zig\nconst x = 1;\n` + "```" + `",
  "tools": [{"name": "Bash"}, {"name": "Read"}]
}`

func mustValidate(t *testing.T, jsonText string) Payload {
	t.Helper()
	raw, err := ParsePayload([]byte(jsonText))
	if err != nil {
		t.Fatalf("ParsePayload: %v", err)
	}
	p, err := Validate(raw)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return p
}

func TestValidPayloadParsesAndValidates(t *testing.T) {
	p := mustValidate(t, testPayloadJSON)
	if p.Lane != LaneConversation {
		t.Errorf("lane = %q", p.Lane)
	}
	if p.World != "testworld" {
		t.Errorf("world = %q", p.World)
	}
	if len(p.Tools) != 2 {
		t.Errorf("tools = %v", p.Tools)
	}
}

func TestClosedSchemaRejections(t *testing.T) {
	cases := []struct {
		name string
		json string
	}{
		{"unknown field", `{"schema_version":1,"world":"w","scope":"s","lane":"conversation",
"adapter_version":"1","session_id":"a","turn_id":"b","harness":"h",
"event_time_ms":1,"capture_policy":"p","turn_outcome":"ok",
"user_content":"u","assistant_result":"a","surprise":true}`},
		{"missing required field", `{"schema_version":1,"world":"w"}`},
		{"duplicate key", `{"schema_version":1,"schema_version":1,"world":"w","scope":"s","lane":"conversation",
"harness":"h","adapter_version":"1","session_id":"a","turn_id":"b",
"event_time_ms":1,"capture_policy":"p","turn_outcome":"ok",
"user_content":"u","assistant_result":"a"}`},
		{"duplicate key in tool object", `{"schema_version":1,"world":"w","scope":"s","lane":"conversation",
"harness":"h","adapter_version":"1","session_id":"a","turn_id":"b",
"event_time_ms":1,"capture_policy":"p","turn_outcome":"ok",
"user_content":"u","assistant_result":"a","tools":[{"name":"a","name":"b"}]}`},
		{"null required string", `{"schema_version":1,"world":"w","scope":"s","lane":null,
"harness":"h","adapter_version":"1","session_id":"a","turn_id":"b",
"event_time_ms":1,"capture_policy":"p","turn_outcome":"ok",
"user_content":"u","assistant_result":"a"}`},
		{"fractional event_time_ms", `{"schema_version":1,"world":"w","scope":"s","lane":"conversation",
"harness":"h","adapter_version":"1","session_id":"a","turn_id":"b",
"event_time_ms":1.5,"capture_policy":"p","turn_outcome":"ok",
"user_content":"u","assistant_result":"a"}`},
		{"negative event_time_ms", `{"schema_version":1,"world":"w","scope":"s","lane":"conversation",
"harness":"h","adapter_version":"1","session_id":"a","turn_id":"b",
"event_time_ms":-1,"capture_policy":"p","turn_outcome":"ok",
"user_content":"u","assistant_result":"a"}`},
		{"string event_time_ms", `{"schema_version":1,"world":"w","scope":"s","lane":"conversation",
"harness":"h","adapter_version":"1","session_id":"a","turn_id":"b",
"event_time_ms":"1","capture_policy":"p","turn_outcome":"ok",
"user_content":"u","assistant_result":"a"}`},
		{"trailing garbage", `{"schema_version":1,"world":"w","scope":"s","lane":"conversation",
"harness":"h","adapter_version":"1","session_id":"a","turn_id":"b",
"event_time_ms":1,"capture_policy":"p","turn_outcome":"ok",
"user_content":"u","assistant_result":"a"} extra`},
		{"tool with extra key", `{"schema_version":1,"world":"w","scope":"s","lane":"conversation",
"harness":"h","adapter_version":"1","session_id":"a","turn_id":"b",
"event_time_ms":1,"capture_policy":"p","turn_outcome":"ok",
"user_content":"u","assistant_result":"a","tools":[{"name":"a","args":{}}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParsePayload([]byte(tc.json)); !errors.Is(err, ErrMalformed) {
				t.Fatalf("ParsePayload error = %v, want ErrMalformed", err)
			}
		})
	}
}

func TestTypedValidationFailures(t *testing.T) {
	base, err := ParsePayload([]byte(testPayloadJSON))
	if err != nil {
		t.Fatalf("ParsePayload: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*RawPayload)
		want   error
	}{
		{"unsupported schema version", func(r *RawPayload) { r.SchemaVersion = 2 }, ErrUnsupportedSchemaVersion},
		{"invalid lane", func(r *RawPayload) { r.Lane = "gossip" }, ErrInvalidLane},
		{"invalid world", func(r *RawPayload) { s := "Bad World"; r.World = &s }, ErrInvalidWorld},
		{"scope with slash", func(r *RawPayload) { s := "a/b"; r.Scope = &s }, ErrInvalidScope},
		{"dot scope", func(r *RawPayload) { s := ".."; r.Scope = &s }, ErrInvalidScope},
		{"empty assistant", func(r *RawPayload) { r.AssistantResult = "" }, ErrEmptyAssistantResult},
		{"empty user", func(r *RawPayload) { r.UserContent = "" }, ErrEmptyUserContent},
		{"invalid harness", func(r *RawPayload) { r.Harness = "two words" }, ErrInvalidHarness},
		{"invalid tool name", func(r *RawPayload) { r.Tools = []Tool{{Name: "has space"}} }, ErrInvalidToolName},
		{"invalid host", func(r *RawPayload) { s := "two words"; r.Host = &s }, ErrInvalidHost},
		{"workspace root with newline", func(r *RawPayload) { s := "/has\nnewline"; r.WorkspaceRoot = &s }, ErrInvalidWorkspaceRoot},
		{"empty branch_of", func(r *RawPayload) { s := ""; r.BranchOf = &s }, ErrInvalidBranchOf},
		{"oversized workspace root", func(r *RawPayload) {
			s := strings.Repeat("a", MaxPathLen+1)
			r.WorkspaceRoot = &s
		}, ErrInvalidWorkspaceRoot},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := base
			tc.mutate(&raw)
			if _, err := Validate(raw); !errors.Is(err, tc.want) {
				t.Fatalf("Validate error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestOversizedContent(t *testing.T) {
	base, err := ParsePayload([]byte(testPayloadJSON))
	if err != nil {
		t.Fatalf("ParsePayload: %v", err)
	}
	base.UserContent = strings.Repeat("x", MaxContentBytes+1)
	if _, err := Validate(base); !errors.Is(err, ErrOversizedContent) {
		t.Fatalf("Validate error = %v, want ErrOversizedContent", err)
	}
}

func TestOmittedWorldScopeParseAsNullAndFailUnfilled(t *testing.T) {
	bare := `{"schema_version":1,"lane":"conversation","harness":"pi",
"adapter_version":"1","session_id":"s","turn_id":"t",
"event_time_ms":1,"capture_policy":"p","turn_outcome":"completed",
"user_content":"u","assistant_result":"a"}`
	raw, err := ParsePayload([]byte(bare))
	if err != nil {
		t.Fatalf("ParsePayload: %v", err)
	}
	if raw.World != nil || raw.Scope != nil {
		t.Fatalf("world/scope = %v/%v, want nil", raw.World, raw.Scope)
	}
	if _, err := Validate(raw); !errors.Is(err, ErrInvalidWorld) {
		t.Fatalf("Validate error = %v, want ErrInvalidWorld", err)
	}
	// The capture host's config merge makes the same payload valid.
	world, scope := "main", "default"
	raw.World, raw.Scope = &world, &scope
	p, err := Validate(raw)
	if err != nil {
		t.Fatalf("Validate after fill: %v", err)
	}
	if p.World != "main" || p.Scope != "default" {
		t.Errorf("filled world/scope = %q/%q", p.World, p.Scope)
	}
}

func TestProvenanceFieldsValidateAndPassThrough(t *testing.T) {
	base, err := ParsePayload([]byte(testPayloadJSON))
	if err != nil {
		t.Fatalf("ParsePayload: %v", err)
	}
	p, err := Validate(base)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if p.WorkspaceRoot != nil || p.BranchOf != nil || p.Host != nil {
		t.Fatal("provenance should be nil when omitted")
	}
	root := "/home/user/projects/my repo"
	branch := "/home/user/.evoker/sessions/parent-a1b2.jsonl"
	host := "buildbox-01"
	base.WorkspaceRoot, base.BranchOf, base.Host = &root, &branch, &host
	p, err = Validate(base)
	if err != nil {
		t.Fatalf("Validate with provenance: %v", err)
	}
	if *p.WorkspaceRoot != root || *p.BranchOf != branch || *p.Host != host {
		t.Errorf("provenance = %q/%q/%q", *p.WorkspaceRoot, *p.BranchOf, *p.Host)
	}
}

func TestPayloadOverBudget(t *testing.T) {
	big := make([]byte, MaxPayloadBytes+1)
	if _, err := ParsePayload(big); !errors.Is(err, ErrMalformed) {
		t.Fatalf("ParsePayload error = %v, want ErrMalformed", err)
	}
}

// TestCaptureErrorNameCoversEverySentinel asserts every sentinel in the
// capture failure vocabulary maps to its own distinct CamelCase name, and an
// error the vocabulary does not know maps to Unavailable.
func TestCaptureErrorNameCoversEverySentinel(t *testing.T) {
	sentinels := []error{
		ErrMalformed, ErrUnsupportedSchemaVersion, ErrInvalidWorld,
		ErrInvalidScope, ErrInvalidLane, ErrInvalidHarness,
		ErrInvalidAdapterVersion, ErrInvalidSessionID, ErrInvalidTurnID,
		ErrInvalidCapturePolicy, ErrInvalidTurnOutcome, ErrEmptyUserContent,
		ErrEmptyAssistantResult, ErrOversizedContent, ErrInvalidUTF8,
		ErrTooManyTools, ErrInvalidToolName, ErrInvalidWorkspaceRoot,
		ErrInvalidBranchOf, ErrInvalidHost, ErrContainmentViolation,
		ErrPermissionDenied, ErrStoreUnavailable,
	}
	seen := map[string]error{}
	for _, s := range sentinels {
		name := CaptureErrorName(s)
		if name == "" {
			t.Errorf("%v has no name", s)
		}
		if prev, dup := seen[name]; dup {
			t.Errorf("%v and %v share the name %q", prev, s, name)
		}
		seen[name] = s
		// A wrapped sentinel keeps its name: adapters match on the report,
		// not on how deep the wrap is.
		if got := CaptureErrorName(fmt.Errorf("context: %w", s)); got != name {
			t.Errorf("wrapped %v = %q, want %q", s, got, name)
		}
	}
	if got := CaptureErrorName(errors.New("never seen before")); got != "Unavailable" {
		t.Errorf("unrecognized error = %q, want Unavailable", got)
	}
}

func TestValidateRejectsImplausibleEventTime(t *testing.T) {
	raw, err := ParsePayload([]byte(testPayloadJSON))
	if err != nil {
		t.Fatal(err)
	}
	raw.EventTimeMs = MaxEventTimeMs + 1
	if _, err := Validate(raw); !errors.Is(err, ErrImplausibleEventTime) {
		t.Errorf("beyond the window: err = %v, want ErrImplausibleEventTime", err)
	}
	if CaptureErrorName(ErrImplausibleEventTime) != "ImplausibleEventTime" {
		t.Errorf("error name = %q", CaptureErrorName(ErrImplausibleEventTime))
	}
	// The boundary itself is inside the window.
	raw.EventTimeMs = MaxEventTimeMs
	if _, err := Validate(raw); err != nil {
		t.Errorf("at the boundary: err = %v", err)
	}
	raw.EventTimeMs = MinEventTimeMs
	if _, err := Validate(raw); err != nil {
		t.Errorf("at the epoch: err = %v", err)
	}
}

func TestValidateRejectsUnknownLane(t *testing.T) {
	raw, err := ParsePayload([]byte(testPayloadJSON))
	if err != nil {
		t.Fatal(err)
	}
	raw.Lane = "scratch"
	if _, err := Validate(raw); !errors.Is(err, ErrInvalidLane) {
		t.Errorf("unknown lane: err = %v, want ErrInvalidLane", err)
	}
	for _, lane := range []Lane{LaneConversation, LaneDelegatedWork, LaneEvaluation, LaneImportedLegacy} {
		if !ValidLane(lane) {
			t.Errorf("ValidLane(%q) = false", lane)
		}
	}
	if ValidLane("scratch") || ValidLane("") {
		t.Error("ValidLane accepted a lane outside the contract")
	}
}

func TestValidScopeRejectsDotLedAndPathScopes(t *testing.T) {
	// A dot-led scope would publish into a directory WalkCorpus skips as
	// foreign tooling state: captured fine, then invisible to sync,
	// freshness, and reseal forever. Worlds already enforce this through
	// their charset; scopes enforce it here.
	for _, bad := range []string{"", ".", "..", ".hidden", ".work", "a/b"} {
		if ValidScope(bad) {
			t.Errorf("ValidScope(%q) = true, want false", bad)
		}
	}
	for _, good := range []string{"default", "workspace:demo", "a.b", "x..y", "global"} {
		if !ValidScope(good) {
			t.Errorf("ValidScope(%q) = false, want true", good)
		}
	}
}
