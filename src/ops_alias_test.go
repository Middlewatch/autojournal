package autojournal

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

// A mistyped terms field never discards the counted query: the
// The miss-log reader takes query first and consumes terms only when it is an
// array, skipping non-string items individually.
func TestAggregateMissesToleratesMistypedTerms(t *testing.T) {
	log := []byte(`{"query":"vpn","terms":"vpn"}` + "\n" +
		`{"query":"vpn","terms":["tail",7]}` + "\n" +
		`{"query":"vpn"}` + "\n")
	agg := AggregateMisses(log)
	if len(agg) != 1 || agg[0].Query != "vpn" || agg[0].Count != 3 {
		t.Fatalf("agg = %+v", agg)
	}
	if len(agg[0].Terms) != 1 || agg[0].Terms[0] != "tail" {
		t.Errorf("terms = %v", agg[0].Terms)
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

func TestAliasAddExtendsCaseVariantKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thesaurus.json")
	if err := os.WriteFile(path, []byte(`{
  "Firmware": ["fwupd"]
}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AddAlias(path, "firmware", []string{"polkit"}); err != nil {
		t.Fatalf("AddAlias: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "Firmware") {
		t.Errorf("case-variant key survived the rewrite: %s", data)
	}
	m := LoadAliasMapFromBytes(data)
	if got := m.Get("firmware"); len(got) != 2 || got[0] != "fwupd" || got[1] != "polkit" {
		t.Errorf("firmware = %v, want the variant extended, not shadowed", got)
	}
	if m.MergedKeys() != 0 {
		t.Errorf("merged keys after rewrite = %d, want 0 (collapsed on disk)", m.MergedKeys())
	}
}
