package autojournal

import (
	"fmt"
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

// Tolerance profile of the thesaurus parser: a duplicate key merges into
// its first occurrence (one hand-edit must not disable the file), a
// null value drops its key, and a non-string array item — even an
// overflowing number — is skipped item by item with the key kept.
func TestAliasLoadToleranceMatchesReferenceParser(t *testing.T) {
	dup := LoadAliasMapFromBytes([]byte(`{"a": ["x"], "a": ["y"]}`))
	if got := dup.Get("a"); len(got) != 2 || got[0] != "x" || got[1] != "y" {
		t.Errorf("duplicate-key map = %v, want the merged entry", got)
	}
	if dup.MergedKeys() != 1 {
		t.Errorf("merged keys = %d, want 1", dup.MergedKeys())
	}

	nullValue := LoadAliasMapFromBytes([]byte(`{"firmware": null, "portal": ["pi-web-access"]}`))
	if got := len(nullValue.Entries()); got != 1 {
		t.Fatalf("entries = %d, want the null key dropped", got)
	}
	if nullValue.Entries()[0].Key != "portal" {
		t.Errorf("kept key = %q", nullValue.Entries()[0].Key)
	}
	if nullValue.Get("firmware") != nil {
		t.Error("null-valued key resolves")
	}

	overflow := LoadAliasMapFromBytes([]byte(`{"nums": [1e999, "kept"]}`))
	if got := overflow.Get("nums"); len(got) != 1 || got[0] != "kept" {
		t.Errorf("overflow-number entry = %v", got)
	}

	trailing := LoadAliasMapFromBytes([]byte(`{"a": ["x"]} extra`))
	if len(trailing.Entries()) != 0 {
		t.Error("trailing garbage still produced entries")
	}
}

func TestLoadAliasMapMergesDuplicateKeys(t *testing.T) {
	m := LoadAliasMapFromBytes([]byte(`{"fw": ["fwupd", "polkit"], "fw": ["polkit", "dbus"]}`))
	if got := m.Get("fw"); len(got) != 3 ||
		got[0] != "fwupd" || got[1] != "polkit" || got[2] != "dbus" {
		t.Errorf("fw = %v, want merged values in first-appearance order", got)
	}
	if m.MergedKeys() != 1 {
		t.Errorf("merged keys = %d, want 1", m.MergedKeys())
	}
	if len(m.Entries()) != 1 {
		t.Errorf("entries = %d, want 1", len(m.Entries()))
	}
}

func TestLoadAliasMapMergesCaseVariantKeys(t *testing.T) {
	m := LoadAliasMapFromBytes([]byte(`{"Firmware": ["fwupd"], "firmware": ["polkit"], " firmware ": ["fwupd", "udev"]}`))
	if got := m.Get("firmware"); len(got) != 3 ||
		got[0] != "fwupd" || got[1] != "polkit" || got[2] != "udev" {
		t.Errorf("firmware = %v, want merged values with repeats dropped", got)
	}
	if m.MergedKeys() != 2 {
		t.Errorf("merged keys = %d, want 2", m.MergedKeys())
	}
	// A clean file reports zero.
	if clean := LoadAliasMapFromBytes([]byte(`{"a": ["b"]}`)); clean.MergedKeys() != 0 {
		t.Errorf("clean merged keys = %d", clean.MergedKeys())
	}
}

func TestAliasDigestFramesKeysAgainstSeparatorCollisions(t *testing.T) {
	// Without length framing, a key carrying the separator byte hashes
	// identically to a key/value split at the same position: the NUL-bearing
	// key "a\x00b" with no values and the entry {"a": ["b"]} would share
	// one digest, and the alias identity stamped on results could call two
	// different maps the same.
	withNulKey := LoadAliasMapFromBytes([]byte(`{"a\u0000b": []}`))
	keyAndValue := LoadAliasMapFromBytes([]byte(`{"a": ["b"]}`))
	if len(withNulKey.Entries()) != 1 || len(keyAndValue.Entries()) != 1 {
		t.Fatalf("entries = %d and %d, want 1 and 1",
			len(withNulKey.Entries()), len(keyAndValue.Entries()))
	}
	if withNulKey.DigestHex() == keyAndValue.DigestHex() {
		t.Error("two different maps share one alias digest")
	}
}
