// Parse-boundary fuzzing: the five functions that turn bytes this
// package did not produce into structured values, asserted against
// round-trip and containment invariants rather than crash-freedom — every
// defect found at these boundaries so far parsed cleanly and produced a
// wrong value. Seeds are the fixture corpus plus one named regression
// seed per defect found at each boundary, under testdata/fuzz.

package autojournal

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// seedFiles f.Adds every file matching the glob, so the pinned fixture
// corpus is the baseline population for a target.
func seedFiles(f *testing.F, glob string) {
	f.Helper()
	names, err := filepath.Glob(glob)
	if err != nil || len(names) == 0 {
		f.Fatalf("seed corpus missing: %s (%v)", glob, err)
	}
	for _, name := range names {
		b, err := os.ReadFile(name)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
}

// FuzzParsePayload: a payload that validates derives a prefixed episode id
// and layout components that stay inside the corpus root — and the date
// shard is exactly four, two and two digits naming a year in 0001–9999.
// The digit bound is the invariant, not decoration: containment alone
// holds over a wrapped timestamp (2^63 derives a negative year and
// MaxEventTimeMs+1s derives 10000/01/01, both inside the root), so only
// the shape assertion can catch a reverted event_time_ms bound.
func FuzzParsePayload(f *testing.F) {
	seedFiles(f, filepath.Join(payloadsDir, "*.json"))
	f.Fuzz(func(t *testing.T, data []byte) {
		raw, err := ParsePayload(data)
		if err != nil {
			return
		}
		if raw.World == nil {
			s := "main"
			raw.World = &s
		}
		if raw.Scope == nil {
			s := "default"
			raw.Scope = &s
		}
		p, err := Validate(raw)
		if err != nil {
			return
		}
		id := EpisodeID(&p)
		if !strings.HasPrefix(id, IDPrefix) {
			t.Errorf("episode id %q lacks the %q prefix", id, IDPrefix)
		}
		comps := layoutComponents(&p)
		if len(comps) < 3 {
			t.Fatalf("layout %v is too shallow to carry a date shard", comps)
		}
		for _, c := range comps {
			if c == "" || c == "." || c == ".." || strings.ContainsAny(c, "/\\\x00") {
				t.Errorf("layout component %q escapes containment (layout %v)", c, comps)
			}
		}
		date := comps[len(comps)-3:]
		for i, want := range []int{4, 2, 2} {
			part := date[i]
			if len(part) != want {
				t.Errorf("date shard %v: component %q is not %d digits", date, part, want)
				continue
			}
			for _, r := range part {
				if r < '0' || r > '9' {
					t.Errorf("date shard %v: component %q is not numeric", date, part)
					break
				}
			}
		}
		if date[0] == "0000" {
			t.Errorf("date shard %v names year zero", date)
		}
	})
}

// FuzzParseConfig: a config that parses re-emits stably through the
// rewrite path — the canonical emission parses, and re-emitting it is
// byte-identical, so an owner rewrite can never publish a config this
// package refuses or one that keeps churning.
func FuzzParseConfig(f *testing.F) {
	seedFiles(f, filepath.Join(goldenDir, "config", "*.json"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if _, err := ParseConfig(data); err != nil {
			return
		}
		root, err := parseOrderedJSON(data)
		if err != nil || root.kind != kindObject {
			t.Fatalf("config parsed but the ordered reader refuses it: %v", err)
		}
		var b strings.Builder
		writeCanonicalJSON(&b, root, 0)
		first := b.String()
		cfg2, err := ParseConfig([]byte(first))
		if err != nil {
			t.Fatalf("rewrite emitted a config this package refuses to load: %v", err)
		}
		root2, err := parseOrderedJSON([]byte(first))
		if err != nil {
			t.Fatalf("rewrite emitted unreadable JSON: %v", err)
		}
		var b2 strings.Builder
		writeCanonicalJSON(&b2, root2, 0)
		if b2.String() != first {
			t.Errorf("rewrite is not byte-stable:\nfirst %q\nagain %q", first, b2.String())
		}
		// The emission carries the same typed values, not merely parseable
		// ones: silent drift above float64's integer range would satisfy
		// every byte assertion while changing what the owner configured.
		cfg, _ := ParseConfig(data)
		if cfg != cfg2 {
			t.Errorf("rewrite changed typed values:\nbefore %+v\nafter %+v", cfg, cfg2)
		}
		// An accepted config is finite (the ratified F-contract): stated
		// here independently of the parser's internal rejection path so
		// the config_non_finite regression seed can fire if that path is
		// ever narrowed away.
		for name, v := range map[string]float64{
			"recency_boost":    cfg.RecencyBoost,
			"min_score":        cfg.MinScore,
			"confidence_floor": cfg.ConfidenceFloor,
		} {
			if math.IsInf(v, 0) || math.IsNaN(v) {
				t.Errorf("accepted config carries non-finite %s = %v", name, v)
			}
		}
	})
}

// FuzzParseEpisode: an episode that parses carries only contract-clean
// identity fields (the read boundary revalidates what capture enforced),
// and one that verifies re-renders its body to identical bytes — body
// only, for the same reason TestVerifyEpisodeRoundTripsEveryGoldenFixture
// is: provenance keys are rendered but not parsed.
func FuzzParseEpisode(f *testing.F) {
	seedFiles(f, filepath.Join(goldenDir, "episodes", "*.md"))
	f.Fuzz(func(t *testing.T, data []byte) {
		content := string(data)
		ep := ParseEpisode(content)
		if ep == nil {
			return
		}
		if !ValidWorld(ep.World) || !ValidScope(ep.Scope) {
			t.Errorf("parsed episode carries contract-violating identity: world %q scope %q", ep.World, ep.Scope)
		}
		// A parsed episode binds each required key exactly once: with a
		// duplicate, readers could disagree about which line wins (the P9
		// MAJOR). Counted textually over the frontmatter region — values
		// are single-line, so every "\n<key>: " is a key line — which is
		// deliberately asymmetric: duplicated unknown keys bind nothing
		// and stay tolerated, so no generic no-duplicates oracle applies.
		fm := content[:ep.BodyOffset]
		for key := range requiredEpisodeKeys {
			if n := strings.Count(fm, "\n"+key+": "); n != 1 {
				t.Errorf("parsed episode carries %d %q lines, want exactly 1", n, key)
			}
		}
		v, err := VerifyEpisode(content)
		if err != nil {
			return
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
			t.Errorf("verified body does not re-render:\n got %q\nwant %q", got, want)
		}
	})
}

// FuzzLoadAliasMapFromBytes: loading never fails — a broken thesaurus
// degrades recall, never search itself — and merging is idempotent:
// re-serializing the loaded entries and loading again yields the same
// digest with nothing left to merge.
func FuzzLoadAliasMapFromBytes(f *testing.F) {
	f.Add([]byte(`{"portal": ["gateway"], "refresh": ["fwupd"]}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`not json`))
	f.Fuzz(func(t *testing.T, data []byte) {
		m := LoadAliasMapFromBytes(data)
		if m == nil {
			t.Fatal("LoadAliasMapFromBytes returned nil")
		}
		// Totality: an entry the tolerance rules give no grounds to drop
		// must survive loading. Expectation comes from an independent
		// encoding/json parse restricted to unambiguously valid entries,
		// so this is what catches the former behavior of disabling the
		// whole file over one duplicated key.
		if expected := expectedAliasKeys(data); expected != nil {
			loaded := map[string]bool{}
			for _, entry := range m.Entries() {
				loaded[entry.Key] = true
			}
			for k := range expected {
				if !loaded[k] {
					t.Errorf("valid alias entry %q vanished on load", k)
				}
			}
		}
		var b strings.Builder
		b.WriteString("{")
		for i, entry := range m.Entries() {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(quoteJSON(entry.Key))
			b.WriteString(":[")
			for j, v := range entry.Values {
				if j > 0 {
					b.WriteString(",")
				}
				b.WriteString(quoteJSON(v))
			}
			b.WriteString("]")
		}
		b.WriteString("}")
		again := LoadAliasMapFromBytes([]byte(b.String()))
		if again.DigestHex() != m.DigestHex() {
			t.Errorf("reload of the re-serialized map changed the digest: %s -> %s", m.DigestHex(), again.DigestHex())
		}
		if again.MergedKeys() != 0 {
			t.Errorf("re-serialized map still merges %d keys; the collapse is not idempotent", again.MergedKeys())
		}
	})
}

// FuzzCursorDecode: a cursor decodes only against the inputs it was
// minted with — an arbitrary token either fails typed or re-mints
// byte-identically, a minted cursor round-trips its offset, and any
// change to the minting inputs makes the same cursor malformed.
func FuzzCursorDecode(f *testing.F) {
	f.Add("aj1.3.00000000", "quokka fence", "main", "", "conversation", "e3b0", uint64(3))
	f.Fuzz(func(t *testing.T, cursor, query, world, scope, lanes, aliasDigest string, offset uint64) {
		inputs := CursorInputs{Query: query, World: world, Scope: scope, Lanes: lanes, AliasDigest: aliasDigest}
		if off, err := CursorDecode(cursor, inputs); err == nil {
			if CursorEncode(off, inputs) != cursor {
				t.Errorf("cursor %q decoded but does not re-mint identically", cursor)
			}
		}
		minted := CursorEncode(offset, inputs)
		off, err := CursorDecode(minted, inputs)
		if err != nil || off != offset {
			t.Errorf("minted cursor did not round-trip: off %d err %v", off, err)
		}
		// Every guard field is binding. The assertion is conditional on
		// the 8-hex guards actually differing: the guard is 32 bits by
		// design, so a hunted hash collision is accepted behavior, not a
		// finding for the fuzzer to spend a weekly run discovering.
		for _, mutate := range []func(*CursorInputs){
			func(i *CursorInputs) { i.Query += "\x00m" },
			func(i *CursorInputs) { i.World += "\x00m" },
			func(i *CursorInputs) { i.Scope += "\x00m" },
			func(i *CursorInputs) { i.Lanes += "\x00m" },
			func(i *CursorInputs) { i.AliasDigest += "\x00m" },
		} {
			other := inputs
			mutate(&other)
			if CursorGuardHex(other) == CursorGuardHex(inputs) {
				continue
			}
			if _, err := CursorDecode(minted, other); err == nil {
				t.Errorf("cursor decoded against inputs it was not minted with (%+v)", other)
			}
		}
	})
}

// expectedAliasKeys derives, from an independent encoding/json parse, the
// normalized keys the tolerant loader has no grounds to drop: the document
// is an object of string arrays, the normalized key is valid, and every
// value is already in canonical (lowercase, valid) form. Entries with any
// doubt are excluded from the expectation rather than guessed at, so the
// oracle under-approximates and never flakes; nil means no expectation.
func expectedAliasKeys(data []byte) map[string]bool {
	// encoding/json quietly replaces invalid UTF-8 with U+FFFD where the
	// product's parser refuses the whole document — a legitimate
	// file-level refusal, found by this oracle's own first gate run
	// (alias_invalid_utf8_key). Only well-encoded documents carry an
	// expectation.
	if !utf8.Valid(data) {
		return nil
	}
	var m map[string][]string
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	keys := map[string]bool{}
	for k, vs := range m {
		nk := normalizeAliasKey(k)
		if !validAliasKey(nk) || len(vs) == 0 {
			continue
		}
		clean := true
		for _, v := range vs {
			if v != lowerASCII(v) || !validAliasValue(v) {
				clean = false
				break
			}
		}
		if clean {
			keys[nk] = true
		}
	}
	return keys
}

// quoteJSON is the minimal JSON string quoting the alias round-trip
// needs; alias keys and values are validated charsets, but the fuzzer
// hands us arbitrary survivors of the tolerant loader.
func quoteJSON(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				b.WriteString(`\u00`)
				const hex = "0123456789abcdef"
				b.WriteByte(hex[r>>4])
				b.WriteByte(hex[r&0xf])
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
