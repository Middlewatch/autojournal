package autojournal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAliasLoadToleratesJunkLowercasesAndDigestIgnoresFormatting(t *testing.T) {
	m := LoadAliasMapFromBytes([]byte(
		`{"Firmware": ["FWUPD", "polkit"], "quant": ["gguf", "q8"], "junk": "not-an-array", "numbers": [1, 2]}`))
	fw := m.Get("firmware")
	if len(fw) != 2 || fw[0] != "fwupd" {
		t.Errorf("firmware = %v", fw)
	}
	if got := m.Get("junk"); got != nil {
		t.Errorf("junk = %v, want nil", got)
	}
	// "numbers" keeps its key with no valid values.
	if got := m.Get("numbers"); got == nil || len(got) != 0 {
		t.Errorf("numbers = %v, want empty non-nil", got)
	}

	reordered := LoadAliasMapFromBytes([]byte(`{
  "numbers": [],
  "quant": ["q8", "gguf", "q8"],
  "firmware": ["polkit", "fwupd"]
}`))
	if m.DigestHex() != reordered.DigestHex() {
		t.Error("digest tracks formatting, not meaning")
	}
	different := LoadAliasMapFromBytes([]byte(`{"firmware": ["fwupd"]}`))
	if m.DigestHex() == different.DigestHex() {
		t.Error("different maps share a digest")
	}
}

func TestAliasDigestCoversEveryValueIncludingPastThe64th(t *testing.T) {
	// The two maps agree on the 64 lexically-smallest values and differ
	// only in the last one, so any digest that caps the sorted value list
	// would wrongly call them identical.
	build := func(last string) string {
		var sb strings.Builder
		sb.WriteString(`{"term": [`)
		for i := 0; i < 65; i++ {
			if i > 0 {
				sb.WriteByte(',')
			}
			v := last
			if i < 64 {
				v = fmt.Sprintf("v%03d", i)
			}
			sb.WriteString(`"` + v + `"`)
		}
		sb.WriteString(`]}`)
		return sb.String()
	}
	ma := LoadAliasMapFromBytes([]byte(build("v064")))
	mb := LoadAliasMapFromBytes([]byte(build("v999")))
	if ma.DigestHex() == mb.DigestHex() {
		t.Error("digest ignored a value past the 64th")
	}
}

func TestCorruptOrMissingThesaurusIsEmptyMapNeverError(t *testing.T) {
	corrupt := LoadAliasMapFromBytes([]byte("not json at all {{{"))
	if len(corrupt.Entries()) != 0 {
		t.Error("corrupt map has entries")
	}
	empty := LoadAliasMapFromBytes([]byte("{}"))
	if empty.DigestHex() != corrupt.DigestHex() {
		t.Error("corrupt and empty digests differ")
	}
	missing := LoadAliasMapFile(filepath.Join(t.TempDir(), "nonexistent", "thesaurus.json"))
	if len(missing.Entries()) != 0 {
		t.Error("missing map has entries")
	}
}

func TestAliasEditsRewriteAtomicallyAndPreserveForeignEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thesaurus.json")
	// Foreign key shapes survive edits untouched.
	if err := os.WriteFile(path, []byte(`{"keep-me": {"nested": true}, "firmware": ["fwupd"]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := AddAlias(path, "Firmware", []string{"polkit", "FWUPD"}); err != nil {
		t.Fatal(err)
	}
	if err := AddAlias(path, "planner", []string{"plannotator"}); err != nil {
		t.Fatal(err)
	}
	m := LoadAliasMapFile(path)
	if fw := m.Get("firmware"); len(fw) != 2 { // fwupd deduped
		t.Errorf("firmware = %v", fw)
	}
	if got := m.Get("planner"); len(got) != 1 || got[0] != "plannotator" {
		t.Errorf("planner = %v", got)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "keep-me") || !strings.Contains(string(raw), "nested") {
		t.Error("foreign entry lost:\n" + string(raw))
	}

	if r, err := RemoveAlias(path, "firmware", strptr("polkit")); err != nil || r != RemovedValue {
		t.Errorf("remove value = %q, %v", r, err)
	}
	if r, err := RemoveAlias(path, "firmware", strptr("fwupd")); err != nil || r != RemovedEntry {
		t.Errorf("remove last value = %q, %v", r, err)
	}
	if _, err := RemoveAlias(path, "firmware", nil); !errors.Is(err, ErrAliasNotFound) {
		t.Errorf("remove gone entry = %v", err)
	}
	if r, err := RemoveAlias(path, "planner", nil); err != nil || r != RemovedEntry {
		t.Errorf("remove entry = %q, %v", r, err)
	}

	// Guardrails: keys must be able to fire; values must be searchable.
	if err := AddAlias(path, "the", []string{"fwupd"}); !errors.Is(err, ErrAliasInvalidTerm) {
		t.Errorf("stop-word key = %v", err)
	}
	if err := AddAlias(path, "no spaces", []string{"x2"}); !errors.Is(err, ErrAliasInvalidTerm) {
		t.Errorf("spaced key = %v", err)
	}
	if err := AddAlias(path, "weight", []string{"q"}); !errors.Is(err, ErrAliasInvalidValue) {
		t.Errorf("short value = %v", err)
	}
}

func TestAliasEditingRefusesToClobberNonObjectFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thesaurus.json")
	if err := os.WriteFile(path, []byte("[1, 2, 3]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AddAlias(path, "weight", []string{"gguf"}); !errors.Is(err, ErrAliasMalformed) {
		t.Errorf("err = %v, want ErrAliasMalformed", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "[1, 2, 3]" {
		t.Errorf("file clobbered: %q", raw)
	}
}

func TestMissLogAppendsBoundsAndAggregates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "misses.jsonl")
	rec := MissRecord{
		TS:    "2026-07-28T12:00:00Z",
		Query: "Weight Format",
		Terms: []string{"weight", "format"},
		Best:  1.75,
		Top:   nil,
	}
	AppendMiss(path, rec, 1024*1024)
	AppendMiss(path, rec, 1024*1024)
	other := rec
	other.Query = "vpn setup"
	other.Terms = []string{"vpn", "setup"}
	AppendMiss(path, other, 1024*1024)
	// A full log stops growing but never errors.
	AppendMiss(path, other, 1)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	agg := AggregateMisses(data)
	if len(agg) != 2 {
		t.Fatalf("candidates = %v", agg)
	}
	if agg[0].Query != "weight format" || agg[0].Count != 2 {
		t.Errorf("agg[0] = %+v", agg[0])
	}
	if agg[1].Query != "vpn setup" || agg[1].Count != 1 {
		t.Errorf("agg[1] = %+v", agg[1])
	}
	if len(agg[0].Terms) != 2 {
		t.Errorf("terms = %v", agg[0].Terms)
	}
}

func strptr(s string) *string { return &s }
