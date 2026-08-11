// Owner-curated thesaurus (alias map) and the weak-query miss log.
//
// The thesaurus file is the authority and stays a flat, hand-editable JSON
// object mapping a casual query word to the canonical journal terms it
// should also search: {"firmware": ["fwupd", "polkit"]}. It is
// byte-compatible with the deployed v1 map, loaded fresh on every search
// invocation (editor changes apply immediately, no cache to invalidate),
// and never projected into SQLite. Its canonical digest is computed and
// stamped on results as the alias identity.
//
// Curation is manual by design: the engine never writes an alias itself.
// The miss log is the raw material for growing the map from real recall
// misses; it is opt-in, owner-private, bounded, and best-effort.
//
// Duplicate and case-variant keys merge on load: keys normalize —
// whitespace trimmed, Unicode-lowercased — and entries that collapse to
// one key combine, value order preserved and repeats dropped, so one
// duplicated key never disables the whole thesaurus. `alias list` reports
// how many keys were collapsed (merged_keys); the file itself is
// normalized only by the commands that already rewrite it.

package autojournal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MaxThesaurusBytes bounds the hand-editable map file.
const MaxThesaurusBytes = 256 * 1024

// AliasEntry maps one casual query word to its canonical terms. Keys and
// values are lowercased; values keep file order.
type AliasEntry struct {
	Key    string
	Values []string
}

// AliasMap is the loaded thesaurus: entries sorted by key plus the
// canonical digest that stamps search results.
type AliasMap struct {
	entries    []AliasEntry
	mergedKeys int
	digestHex  string
}

// Entries returns the sorted entries (for the CLI's alias list).
func (m *AliasMap) Entries() []AliasEntry { return m.entries }

// MergedKeys reports how many entries collapsed into another during load: a
// duplicate key no longer disables the file, but it remains a fact about
// the file the owner may want to repair at the source.
func (m *AliasMap) MergedKeys() int { return m.mergedKeys }

// DigestHex is the SHA-256 hex of the canonical form (sorted keys, sorted
// deduped values) — independent of file formatting and key order.
func (m *AliasMap) DigestHex() string { return m.digestHex }

// Get returns the canonical terms for one query term, or nil.
func (m *AliasMap) Get(term string) []string {
	i := sort.Search(len(m.entries), func(i int) bool { return m.entries[i].Key >= term })
	if i < len(m.entries) && m.entries[i].Key == term {
		return m.entries[i].Values
	}
	return nil
}

func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		b[i] = lowerByte(c)
	}
	return string(b)
}

// LoadAliasMapFromBytes is the tolerant load: only object entries whose value
// is an array become aliases (array items that are not strings are skipped
// item by item); keys normalize and duplicate or case-variant entries merge
// rather than disabling the file. Anything unreadable or unparseable
// is a valid empty configuration — recall degrades but never fails because
// the thesaurus is malformed.
func LoadAliasMapFromBytes(data []byte) *AliasMap {
	var entries []AliasEntry
	merged := 0
	// encoding/json silently replaces invalid UTF-8 with U+FFFD, which would
	// turn a corrupt byte into a plausible-looking alias key. Validate first
	// so a damaged file reads as empty rather than as subtly wrong data.
	if utf8.Valid(data) {
		if parsed, ok := parseAliasEntries(data); ok {
			entries, merged = mergeAliasEntries(parsed)
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	return &AliasMap{entries: entries, mergedKeys: merged, digestHex: aliasDigest(entries)}
}

// normalizeAliasKey canonicalizes a thesaurus key: surrounding whitespace
// dropped, then full Unicode lowercasing — not the ASCII-only fold values
// use — so "Firmware" and " firmware " are one key wherever they appear.
func normalizeAliasKey(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// mergeAliasEntries collapses entries whose normalized keys coincide,
// preserving first-appearance value order and dropping repeated values.
func mergeAliasEntries(parsed []AliasEntry) ([]AliasEntry, int) {
	index := map[string]int{}
	var out []AliasEntry
	merged := 0
	for _, entry := range parsed {
		at, ok := index[entry.Key]
		if !ok {
			index[entry.Key] = len(out)
			out = append(out, entry)
			continue
		}
		merged++
		for _, value := range entry.Values {
			seen := false
			for _, have := range out[at].Values {
				if have == value {
					seen = true
					break
				}
			}
			if !seen {
				out[at].Values = append(out[at].Values, value)
			}
		}
	}
	return out, merged
}

// parseAliasEntries walks the top-level object token by token so every
// occurrence of a duplicated key is seen: map decoding would keep the last
// one and discard the evidence, while the merge that follows load combines
// them instead of letting one hand-edit disable the whole file.
func parseAliasEntries(data []byte) ([]AliasEntry, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, false
	}
	var entries []AliasEntry
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, _ := keyTok.(string) // inside an object, keys are strings
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, false
		}
		// Only arrays become aliases; null, scalars, and objects are skipped
		// whole. The alternative is guessing what a scalar meant, and a
		// thesaurus that guesses is worse than one that ignores.
		if len(raw) == 0 || raw[0] != '[' {
			continue
		}
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, false
		}
		entry := AliasEntry{Key: normalizeAliasKey(key), Values: []string{}}
		for _, item := range items {
			// Non-string items — numbers (even overflowing ones), bools,
			// nested containers — are skipped item by item; the key stays.
			var text string
			if err := json.Unmarshal(item, &text); err != nil {
				continue
			}
			if len(text) == 0 || len(text) > MaxTokenLen {
				continue
			}
			entry.Values = append(entry.Values, lowerASCII(text))
		}
		entries = append(entries, entry)
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF { // no trailing garbage
		return nil, false
	}
	return entries, true
}

// LoadAliasMapFile loads the map from disk; a missing or unreadable map
// is a valid empty configuration.
func LoadAliasMapFile(path string) *AliasMap {
	data, err := os.ReadFile(path)
	if err != nil || len(data) > MaxThesaurusBytes {
		return LoadAliasMapFromBytes([]byte("{}"))
	}
	return LoadAliasMapFromBytes(data)
}

// aliasDigest hashes the canonical form: keys sorted, each entry framed as
// length\x00key then \x00-prefixed length\x00value pairs (values sorted and
// deduped) with a '\n' terminator, so the digest tracks meaning rather than
// file formatting. The length prefix frames the key the same way the payload
// digest frames its fields: without it, a key containing the separator byte
// could make two different maps hash identically. Every value participates —
// a digest that ignored any of them could call two different maps identical.
func aliasDigest(entries []AliasEntry) string {
	h := sha256.New()
	frame := func(s string) {
		h.Write([]byte(strconv.Itoa(len(s))))
		h.Write([]byte{0})
		h.Write([]byte(s))
	}
	for _, entry := range entries {
		sorted := append([]string(nil), entry.Values...)
		sort.Strings(sorted)
		frame(entry.Key)
		prev := ""
		for i, v := range sorted {
			if i > 0 && v == prev {
				continue
			}
			prev = v
			h.Write([]byte{0})
			frame(v)
		}
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}
