// Owner-curated thesaurus (alias map) and the weak-query miss log.
//
// The thesaurus file is the authority and stays a flat, hand-editable JSON
// object mapping a casual query word to the canonical journal terms it
// should also search: {"firmware": ["fwupd", "polkit"]}. It is
// byte-compatible with the deployed v1 map, loaded fresh on every search
// invocation (editor changes apply immediately, no cache to invalidate),
// and never projected into SQLite — only its canonical digest is recorded
// and stamped on results as the alias identity.
//
// Curation is manual by design: the engine never writes an alias itself.
// The miss log is the raw material for growing the map from real recall
// misses; it is opt-in, owner-private, bounded, and best-effort.
//
// Corrupt-file tolerance note: a hand-edit that introduces a duplicate
// JSON key is rejected as ErrAliasMalformed on edit and reads as an
// empty map on load — matching the reference parser, which fails the
// whole document on a duplicate key. Refusing to interpret the ambiguous
// file protects it from being clobbered, the same intent as the
// non-object Malformed guardrail.

package autojournal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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
	entries   []AliasEntry
	digestHex string
}

// Entries returns the sorted entries (for the CLI's alias list).
func (m *AliasMap) Entries() []AliasEntry { return m.entries }

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

// LoadAliasMapFromBytes is the tolerant load, v1 parity: only object
// entries whose value is an array become aliases (array items that are
// not strings are skipped item by item); keys and values are lowercased.
// Anything unreadable or unparseable — including a duplicate key, which
// the reference's parser rejects wholesale — is a valid empty
// configuration: recall never fails because the thesaurus does.
func LoadAliasMapFromBytes(data []byte) *AliasMap {
	var entries []AliasEntry
	// encoding/json silently replaces invalid UTF-8 with U+FFFD; the
	// reference's scanner validates it, and a corrupt document is an
	// empty map there — so it is here too.
	if utf8.Valid(data) {
		if parsed, ok := parseAliasEntries(data); ok {
			entries = parsed
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	return &AliasMap{entries: entries, digestHex: aliasDigest(entries)}
}

// parseAliasEntries walks the top-level object token by token so a
// duplicate key is seen and rejected — the reference parser fails the
// whole document there, and an ambiguous hand-edit must not silently
// resolve to either value. Map decoding cannot detect this.
func parseAliasEntries(data []byte) ([]AliasEntry, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, false
	}
	seen := map[string]struct{}{}
	var entries []AliasEntry
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, _ := keyTok.(string) // inside an object, keys are strings
		if _, dup := seen[key]; dup {
			return nil, false
		}
		seen[key] = struct{}{}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, false
		}
		// Only arrays become aliases; null, scalars, and objects are
		// skipped whole, like the reference's non-array branch.
		if len(raw) == 0 || raw[0] != '[' {
			continue
		}
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, false
		}
		entry := AliasEntry{Key: lowerASCII(key), Values: []string{}}
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

// aliasDigest hashes the canonical form: keys sorted, each line
// key\x00value\x00value…\n with values sorted and deduped, so the digest
// tracks meaning rather than file formatting. Every value participates —
// a digest that ignored any of them could call two different maps
// identical.
func aliasDigest(entries []AliasEntry) string {
	h := sha256.New()
	for _, entry := range entries {
		sorted := append([]string(nil), entry.Values...)
		sort.Strings(sorted)
		h.Write([]byte(entry.Key))
		prev := ""
		for i, v := range sorted {
			if i > 0 && v == prev {
				continue
			}
			prev = v
			h.Write([]byte{0})
			h.Write([]byte(v))
		}
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// --- Owner edits (CLI `alias add` / `alias remove`) ---

// Edit failure vocabulary.
var (
	// ErrAliasInvalidTerm: the key would never fire — it must survive
	// query tokenization (length > 2, [a-z0-9_], not a stop word).
	ErrAliasInvalidTerm = errors.New("invalid alias term")
	// ErrAliasInvalidValue: a value must be a searchable token or phrase —
	// 2..128 bytes from the identity-token charset.
	ErrAliasInvalidValue = errors.New("invalid alias value")
	// ErrAliasMalformed: the file exists but is not a JSON object;
	// refusing to rewrite it protects a hand-edit gone wrong from being
	// clobbered.
	ErrAliasMalformed = errors.New("thesaurus is not a JSON object")
	// ErrAliasNotFound: no such entry, or no such value in the entry.
	ErrAliasNotFound = errors.New("alias not found")
	// ErrAliasUnavailable is any I/O failure reading or rewriting the file.
	ErrAliasUnavailable = errors.New("thesaurus unavailable")
)

// AddAlias adds (or extends) one alias entry and atomically rewrites the
// file, preserving every entry it does not touch. Values already present
// are not duplicated.
func AddAlias(path, term string, canonicals []string) error {
	key := lowerASCII(term)
	if !validAliasKey(key) {
		return ErrAliasInvalidTerm
	}
	if len(canonicals) == 0 {
		return ErrAliasInvalidValue
	}
	root, err := readEditableThesaurus(path)
	if err != nil {
		return err
	}

	var values []configValue
	if existing, ok := root.obj.get(key); ok {
		if existing.kind != kindArray {
			return ErrAliasMalformed
		}
		values = existing.arr
	}
	for _, raw := range canonicals {
		value := lowerASCII(raw)
		if !validAliasValue(value) {
			return ErrAliasInvalidValue
		}
		already := false
		for _, v := range values {
			if v.kind == kindString && v.s == value {
				already = true
				break
			}
		}
		if !already {
			values = append(values, configValue{kind: kindString, s: value})
		}
	}
	root.obj.set(key, configValue{kind: kindArray, arr: values})
	return writeThesaurusAtomic(path, root)
}

// AliasRemoved distinguishes a whole-entry removal from a single value.
type AliasRemoved string

const (
	RemovedEntry AliasRemoved = "entry"
	RemovedValue AliasRemoved = "value"
)

// RemoveAlias removes a whole entry, or one value from an entry (dropping
// the entry when its last value goes), and atomically rewrites the file.
func RemoveAlias(path, term string, canonical *string) (AliasRemoved, error) {
	key := lowerASCII(term)
	root, err := readEditableThesaurus(path)
	if err != nil {
		return "", err
	}
	existing, ok := root.obj.get(key)
	if !ok {
		return "", ErrAliasNotFound
	}

	removed := RemovedEntry
	if canonical != nil {
		value := lowerASCII(*canonical)
		if existing.kind != kindArray {
			return "", ErrAliasMalformed
		}
		at := -1
		for i, v := range existing.arr {
			if v.kind == kindString && v.s == value {
				at = i
				break
			}
		}
		if at < 0 {
			return "", ErrAliasNotFound
		}
		values := append(existing.arr[:at], existing.arr[at+1:]...)
		if len(values) == 0 {
			root.obj.remove(key)
		} else {
			root.obj.set(key, configValue{kind: kindArray, arr: values})
			removed = RemovedValue
		}
	} else {
		root.obj.remove(key)
	}
	return removed, writeThesaurusAtomic(path, root)
}

func validAliasKey(key string) bool {
	if len(key) <= 2 || len(key) > MaxTokenLen {
		return false
	}
	for i := 0; i < len(key); i++ {
		if !isTokenByte(key[i]) {
			return false
		}
	}
	return !IsStopWord(key)
}

func validAliasValue(value string) bool {
	if len(value) < 2 {
		return false
	}
	return ValidToken(value)
}

// readEditableThesaurus reads the file as a mutable ordered JSON object;
// a missing file starts empty, but an existing non-object file is
// Malformed, never overwritten.
func readEditableThesaurus(path string) (configValue, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return configValue{kind: kindObject, obj: &orderedObject{}}, nil
		}
		return configValue{}, fmt.Errorf("%w: %v", ErrAliasUnavailable, err)
	}
	if len(data) > MaxThesaurusBytes {
		return configValue{}, fmt.Errorf("%w: exceeds %d bytes", ErrAliasUnavailable, MaxThesaurusBytes)
	}
	root, err := parseOrderedJSON(data)
	if err != nil || root.kind != kindObject {
		return configValue{}, ErrAliasMalformed
	}
	return root, nil
}

// writeThesaurusAtomic rewrites the file in the reference's indent_2
// byte format via a sibling temp file and rename.
func writeThesaurusAtomic(path string, root configValue) error {
	var b strings.Builder
	writeZigJSON(&b, root, 0)
	if err := writeAtomicConfig(path, b.String()); err != nil {
		if errors.Is(err, ErrConfigUnavailable) {
			return fmt.Errorf("%w: %v", ErrAliasUnavailable, err)
		}
		return err
	}
	return nil
}

// --- Miss log ---

// MissRecord is one weak-query record, appended as a JSON line.
type MissRecord struct {
	TS    string
	Query string
	Terms []string
	Best  float64
	Top   *string
}

// AppendMiss appends one record as a JSON line. Best-effort by contract:
// every failure is swallowed, and the log stops growing at maxBytes.
func AppendMiss(path string, rec MissRecord, maxBytes uint64) {
	// The log is self-produced and self-consumed, so field order and
	// float formatting need no oracle parity; compact encoding/json is
	// exactly what AggregateMisses reads back.
	line, err := json.Marshal(struct {
		TS    string   `json:"ts"`
		Query string   `json:"query"`
		Terms []string `json:"terms"`
		Best  float64  `json:"best"`
		Top   *string  `json:"top"`
	}{rec.TS, rec.Query, rec.Terms, rec.Best, rec.Top})
	if err != nil {
		return
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return
	}
	info, err := f.Stat()
	if err != nil || uint64(info.Size()) >= maxBytes {
		return
	}
	f.Write(append(line, '\n')) //nolint:errcheck // best-effort by contract
}

// MissCandidate is one reviewed candidate: a distinct weak query, its
// frequency, and the union of extracted terms across its misses.
type MissCandidate struct {
	Query string
	Count uint64
	Terms []string
}

// AggregateMisses aggregates the miss log for review: dedupes by
// lowercased query, ranks by frequency (ties alphabetical). Malformed
// lines are skipped — the log is best-effort on the write side too.
func AggregateMisses(data []byte) []MissCandidate {
	type agg struct {
		count uint64
		terms map[string]struct{}
	}
	byQuery := map[string]*agg{}
	var order []string // first-seen order; sort decides presentation
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		// The reference trims exactly space/tab/CR — not the wider
		// Unicode space set — so a query differing only in NBSP stays
		// distinct on both sides.
		trimmed := bytes.Trim(line, " \t\r")
		if len(trimmed) == 0 {
			continue
		}
		var record struct {
			Query *string         `json:"query"`
			Terms json.RawMessage `json:"terms"`
		}
		if err := json.Unmarshal(trimmed, &record); err != nil || record.Query == nil {
			continue
		}
		query := lowerASCII(strings.Trim(*record.Query, " \t"))
		slot, ok := byQuery[query]
		if !ok {
			slot = &agg{terms: map[string]struct{}{}}
			byQuery[query] = slot
			order = append(order, query)
		}
		slot.count++
		// terms is optional and tolerated field by field: a mistyped or
		// missing terms value never discards the counted query, and
		// non-string items are skipped individually.
		var rawTerms []json.RawMessage
		if record.Terms != nil && json.Unmarshal(record.Terms, &rawTerms) == nil {
			for _, item := range rawTerms {
				var term string
				if json.Unmarshal(item, &term) == nil {
					slot.terms[term] = struct{}{}
				}
			}
		}
	}

	items := make([]MissCandidate, 0, len(byQuery))
	for _, query := range order {
		slot := byQuery[query]
		terms := make([]string, 0, len(slot.terms))
		for term := range slot.terms {
			terms = append(terms, term)
		}
		sort.Strings(terms)
		items = append(items, MissCandidate{Query: query, Count: slot.count, Terms: terms})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Count != items[j].Count {
			return items[i].Count > items[j].Count
		}
		return items[i].Query < items[j].Query
	})
	return items
}
