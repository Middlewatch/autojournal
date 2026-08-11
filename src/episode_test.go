package autojournal

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFrontmatterRoundTrip(t *testing.T) {
	p := mustValidate(t, testPayloadJSON)
	id, digest := EpisodeID(&p), PayloadDigestHex(&p)
	content := Render(RenderInput{
		Payload:       &p,
		EpisodeID:     id,
		DigestHex:     digest,
		CaptureTimeMs: 1785240000000,
	})
	ep := ParseEpisode(string(content))
	if ep == nil {
		t.Fatal("ParseEpisode rejected a rendered episode")
	}
	if ep.EpisodeID != id {
		t.Errorf("episode id = %q, want %q", ep.EpisodeID, id)
	}
	if ep.World != p.World {
		t.Errorf("world = %q, want %q", ep.World, p.World)
	}
	if ep.Lane != p.Lane {
		t.Errorf("lane = %q, want %q", ep.Lane, p.Lane)
	}
	if ep.EventTimeMs != p.EventTimeMs {
		t.Errorf("event_time_ms = %d, want %d", ep.EventTimeMs, p.EventTimeMs)
	}
	if ep.DigestHex != digest {
		t.Errorf("digest = %q, want %q", ep.DigestHex, digest)
	}
	// Rendered frontmatter is `---` + 16 keys + `---`, so the body starts
	// on line 19, at the blank line before `## User`.
	if ep.BodyLine != 19 {
		t.Errorf("body_line = %d, want 19", ep.BodyLine)
	}
	if !strings.HasPrefix(string(content)[ep.BodyOffset:], "\n## User\n") {
		t.Errorf("body_offset lands on %q", string(content)[ep.BodyOffset:ep.BodyOffset+12])
	}
}

func TestBodyLineDoesNotShiftWhenBodyMimicsFrontmatter(t *testing.T) {
	raw, err := ParsePayload([]byte(testPayloadJSON))
	if err != nil {
		t.Fatalf("ParsePayload: %v", err)
	}
	raw.UserContent = "---\nworld: fake\n---"
	p, err := Validate(raw)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	content := Render(RenderInput{
		Payload:       &p,
		EpisodeID:     EpisodeID(&p),
		DigestHex:     PayloadDigestHex(&p),
		CaptureTimeMs: 1785240000000,
	})
	ep := ParseEpisode(string(content))
	if ep == nil {
		t.Fatal("ParseEpisode rejected spoofed body")
	}
	if ep.BodyLine != 19 {
		t.Errorf("body_line = %d, want 19", ep.BodyLine)
	}
}

func TestCorruptFrontmatterYieldsNilNotCrash(t *testing.T) {
	cases := []string{
		"not an episode",
		"---\nschema: aj-episode.v1\n",
		"---\nschema: wrong.v9\n---\n",
		"---\nschema: aj-episode.v1\nepisode_id: x\n---\n",
		"---\nschema: aj-episode.v1\nno-separator-line\n---\n",
	}
	for _, c := range cases {
		if ep := ParseEpisode(c); ep != nil {
			t.Errorf("ParseEpisode(%q) = %+v, want nil", c, ep)
		}
	}
}

// TestVerifyEpisodeRoundTripsEveryGoldenFixture: every pinned episode
// verifies, and re-rendering the body from its recovered fields yields bytes
// identical to that file's body region. Body-only, deliberately: Render
// emits adapter_version and the optional provenance keys, which Episode does
// not carry (correctly — the digest does not cover them), so a whole-file
// re-render is impossible for provenance.md and would assert the wrong
// thing.
func TestVerifyEpisodeRoundTripsEveryGoldenFixture(t *testing.T) {
	names, err := filepath.Glob(filepath.Join(goldenDir, "episodes", "*.md"))
	if err != nil || len(names) == 0 {
		t.Fatalf("golden episodes missing: %v", err)
	}
	for _, path := range names {
		name := strings.TrimSuffix(filepath.Base(path), ".md")
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			content := string(b)
			v, err := VerifyEpisode(content)
			if err != nil {
				t.Fatalf("VerifyEpisode: %v", err)
			}
			var body strings.Builder
			body.WriteString("\n## User\n\n")
			body.WriteString(v.UserContent)
			body.WriteString("\n\n## Assistant\n\n")
			body.WriteString(v.AssistantResult)
			body.WriteString("\n")
			if len(v.Tools) > 0 {
				body.WriteString("\n## Tools\n\n")
				for _, tool := range v.Tools {
					body.WriteString("- ")
					body.WriteString(tool.Name)
					body.WriteString("\n")
				}
			}
			if got, want := body.String(), content[v.BodyOffset:]; got != want {
				t.Errorf("re-rendered body differs:\n got %q\nwant %q", got, want)
			}
		})
	}
}

// TestVerifyEpisodeAcceptsBodyContainingSeparators: the body-spoof shape,
// whose content embeds frontmatter-shaped and tools-shaped blocks, verifies.
// The fixture that would fail without the candidate design already ships.
func TestVerifyEpisodeAcceptsBodyContainingSeparators(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(goldenDir, "episodes", "body-spoof.md"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyEpisode(string(b)); err != nil {
		t.Fatalf("body-spoof does not verify: %v", err)
	}

	// A synthetic case whose user content contains the assistant separator
	// itself, so the first candidate split is the wrong one.
	p := mustValidate(t, testPayloadJSON)
	p.UserContent = "before\n\n## Assistant\n\nafter"
	p.AssistantResult = "the real answer"
	content := string(Render(RenderInput{
		Payload:       &p,
		EpisodeID:     EpisodeID(&p),
		DigestHex:     PayloadDigestHex(&p),
		CaptureTimeMs: 1785240000000,
	}))
	v, err := VerifyEpisode(content)
	if err != nil {
		t.Fatalf("separator-bearing content does not verify: %v", err)
	}
	if v.UserContent != p.UserContent || v.AssistantResult != p.AssistantResult {
		t.Errorf("recovered reading = %q / %q, want the original", v.UserContent, v.AssistantResult)
	}
}

// TestVerifyEpisodeRejectsBodyEdit: one byte changed in the body, digest
// line untouched, is ErrDigestMismatch.
func TestVerifyEpisodeRejectsBodyEdit(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(goldenDir, "episodes", "basic.md"))
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(b), "They pass.", "They fail.", 1)
	if edited == string(b) {
		t.Fatal("edit did not apply; fixture wording changed?")
	}
	if _, err := VerifyEpisode(edited); !errors.Is(err, ErrDigestMismatch) {
		t.Errorf("err = %v, want ErrDigestMismatch", err)
	}
}

// TestVerifyEpisodeRejectsUnparseableFile: not an episode at all.
func TestVerifyEpisodeRejectsUnparseableFile(t *testing.T) {
	for _, content := range []string{
		"",
		"not an episode",
		"---\nschema: aj-episode.v1\nunclosed: yes\n",
	} {
		if _, err := VerifyEpisode(content); !errors.Is(err, ErrEpisodeMalformed) {
			t.Errorf("%q: err = %v, want ErrEpisodeMalformed", content, err)
		}
	}

	// Parses as frontmatter but the body has no structurally valid reading:
	// also malformed — reseal must refuse it, not re-attest it.
	b, err := os.ReadFile(filepath.Join(goldenDir, "episodes", "basic.md"))
	if err != nil {
		t.Fatal(err)
	}
	headerless := strings.Replace(string(b), "\n## User\n\n", "\nUser said:\n\n", 1)
	if headerless == string(b) {
		t.Fatal("header replacement did not apply")
	}
	if _, err := VerifyEpisode(headerless); !errors.Is(err, ErrEpisodeMalformed) {
		t.Errorf("headerless body: err = %v, want ErrEpisodeMalformed", err)
	}
	if _, ok := ResealDigestHex(headerless); ok {
		t.Error("ResealDigestHex re-attested a body with no valid reading")
	}
}

// TestVerifyEpisodeBoundsInterpretations: a body whose candidate cross
// product exceeds MaxBodyInterpretations returns ErrDigestMismatch promptly
// rather than searching further.
func TestVerifyEpisodeBoundsInterpretations(t *testing.T) {
	p := mustValidate(t, testPayloadJSON)
	// Nine embedded assistant separators and nine embedded tools separators,
	// every tools section structurally valid, so the evaluated pair count
	// crosses the 64 cap. The recorded digest is then made stale by editing
	// one byte, so no reading can match and the cap is what stops the search.
	sep := strings.Repeat("x\n\n## Assistant\n\nx", 9)
	tools := strings.Repeat("x\n\n## Tools\n\n- tool_a\n- tool_b", 9)
	p.UserContent = sep
	p.AssistantResult = "start" + tools + "\nend"
	content := string(Render(RenderInput{
		Payload:       &p,
		EpisodeID:     EpisodeID(&p),
		DigestHex:     PayloadDigestHex(&p),
		CaptureTimeMs: 1785240000000,
	}))
	// Sanity: the untouched file must exceed the cap too, so first check the
	// edited one fails fast with the right error.
	edited := strings.Replace(content, "start", "staRt", 1)
	done := make(chan error, 1)
	go func() {
		_, err := VerifyEpisode(edited)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrDigestMismatch) {
			t.Errorf("err = %v, want ErrDigestMismatch", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("VerifyEpisode did not return promptly on an over-cap body")
	}
}

// TestVerifyEpisodeRejectsForgedEpisodeID: an edited episode_id line — the
// one identity field the payload digest does not cover — fails verification
// and is refused by reseal, so a forged id cannot shadow another episode.
func TestVerifyEpisodeRejectsForgedEpisodeID(t *testing.T) {
	p := mustValidate(t, testPayloadJSON)
	content := string(Render(RenderInput{
		Payload:       &p,
		EpisodeID:     EpisodeID(&p),
		DigestHex:     PayloadDigestHex(&p),
		CaptureTimeMs: 1785240000000,
	}))
	other := p
	other.TurnID = "some-other-turn"
	forged := strings.Replace(content, EpisodeID(&p), EpisodeID(&other), 1)
	if forged == content {
		t.Fatal("forgery did not apply")
	}
	if _, err := VerifyEpisode(forged); !errors.Is(err, ErrDigestMismatch) {
		t.Errorf("forged id: err = %v, want ErrDigestMismatch", err)
	}
	if _, ok := ResealDigestHex(forged); ok {
		t.Error("ResealDigestHex re-attested a forged identity line")
	}
	// The untouched file still verifies.
	if _, err := VerifyEpisode(content); err != nil {
		t.Errorf("untouched file: %v", err)
	}
}

// TestVerifyEpisodeCapIsExactlySixtyFour pins the MaxBodyInterpretations
// value: a true reading evaluated as the 64th candidate verifies, and one
// evaluated as the 65th is unreachable and reports ErrDigestMismatch even
// though a matching reading exists past the cap.
func TestVerifyEpisodeCapIsExactlySixtyFour(t *testing.T) {
	render := func(embedded int) (string, Payload) {
		p := mustValidate(t, testPayloadJSON)
		// Each embedded separator creates one earlier assistant split whose
		// no-tools reading is structurally valid, costing one evaluated
		// candidate before the true split is reached.
		p.UserContent = strings.Repeat("x\n\n## Assistant\n\nx", embedded)
		p.AssistantResult = "the true answer"
		p.Tools = nil // one candidate per split: the no-tools reading
		return string(Render(RenderInput{
			Payload:       &p,
			EpisodeID:     EpisodeID(&p),
			DigestHex:     PayloadDigestHex(&p),
			CaptureTimeMs: 1785240000000,
		})), p
	}

	// 63 embedded separators: the true reading is candidate 64 — verifies.
	content, p := render(63)
	v, err := VerifyEpisode(content)
	if err != nil {
		t.Fatalf("candidate 64: %v", err)
	}
	if v.UserContent != p.UserContent {
		t.Error("candidate 64 recovered the wrong reading")
	}
	// 64 embedded separators: the true reading is candidate 65 — past the
	// cap, so the file is reported unverifiable rather than searched further.
	content, _ = render(64)
	if _, err := VerifyEpisode(content); !errors.Is(err, ErrDigestMismatch) {
		t.Errorf("candidate 65: err = %v, want ErrDigestMismatch", err)
	}
}

// TestVerifyEpisodeBoundsCorruptEnumeration: a body with many assistant
// separators and no structurally valid reading at all returns promptly —
// the split bound, not just the evaluated-pair cap, is what stops it.
func TestVerifyEpisodeBoundsCorruptEnumeration(t *testing.T) {
	p := mustValidate(t, testPayloadJSON)
	p.AssistantResult = "tail"
	p.Tools = nil // a tools tail would leave a valid trailing newline
	content := string(Render(RenderInput{
		Payload:       &p,
		EpisodeID:     EpisodeID(&p),
		DigestHex:     PayloadDigestHex(&p),
		CaptureTimeMs: 1785240000000,
	}))
	// Blow up the body: thousands of separators, then strip the trailing
	// newline so no split has a valid no-tools reading and no tools section
	// parses. Every candidate is structurally invalid, so only the split
	// bound — not the evaluated-pair cap — can stop the scan.
	corrupt := strings.Replace(content, "tail\n",
		strings.Repeat("filler text\n\n## Assistant\n\nmore", 20000), 1)
	corrupt = strings.TrimSuffix(corrupt, "\n")
	done := make(chan error, 1)
	go func() {
		_, err := VerifyEpisode(corrupt)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrDigestMismatch) && !errors.Is(err, ErrEpisodeMalformed) {
			t.Errorf("err = %v, want a typed verification error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("corrupt enumeration did not return promptly")
	}
}

func TestParseEpisodeRejectsInvalidScope(t *testing.T) {
	// The read boundary applies the same scope rule as the write
	// boundary. A traversal-shaped scope passes the general token rule, so
	// only ValidScope stands between it and the --json surface.
	p := mustValidate(t, testPayloadJSON)
	content := string(Render(RenderInput{
		Payload:       &p,
		EpisodeID:     EpisodeID(&p),
		DigestHex:     PayloadDigestHex(&p),
		CaptureTimeMs: 1785240000000,
	}))
	if ParseEpisode(content) == nil {
		t.Fatal("rendered episode does not parse")
	}
	for _, scope := range []string{"../escape", "a/b", ".", ".."} {
		edited := strings.Replace(content, "scope: workspace:demo", "scope: "+scope, 1)
		if edited == content {
			t.Fatal("scope line not found in rendered episode")
		}
		if ParseEpisode(edited) != nil {
			t.Errorf("scope %q accepted at the read boundary", scope)
		}
	}
}

// TestParseEpisodeRejectsDuplicatedRequiredKey: a duplicated required
// frontmatter key leaves readers free to disagree about which line binds
// — for payload_digest that disagreement once turned reseal's success
// report into a lie — so the file refuses to parse. Duplicated unknown
// keys bind nothing and stay tolerated.
func TestParseEpisodeRejectsDuplicatedRequiredKey(t *testing.T) {
	p := mustValidate(t, testPayloadJSON)
	content := string(Render(RenderInput{
		Payload:       &p,
		EpisodeID:     EpisodeID(&p),
		DigestHex:     PayloadDigestHex(&p),
		CaptureTimeMs: 1785240000000,
	}))
	if ParseEpisode(content) == nil {
		t.Fatal("baseline render does not parse")
	}

	digestLine := "payload_digest: " + DigestPrefix
	idx := strings.Index(content, digestLine)
	if idx < 0 {
		t.Fatal("no digest line in render")
	}
	lineEnd := idx + strings.IndexByte(content[idx:], '\n')
	line := content[idx:lineEnd]
	doubled := content[:lineEnd] + "\n" + line + content[lineEnd:]
	if ParseEpisode(doubled) != nil {
		t.Error("duplicated payload_digest parsed; the record is ambiguous")
	}

	worldIdx := strings.Index(content, "world: ")
	worldEnd := worldIdx + strings.IndexByte(content[worldIdx:], '\n')
	doubledWorld := content[:worldEnd] + "\n" + content[worldIdx:worldEnd] + content[worldEnd:]
	if ParseEpisode(doubledWorld) != nil {
		t.Error("duplicated world parsed")
	}

	unknown := strings.Replace(content, line, line+"\nx_note: a\nx_note: b", 1)
	if ParseEpisode(unknown) == nil {
		t.Error("duplicated unknown key was refused; tolerance narrowed too far")
	}
}
