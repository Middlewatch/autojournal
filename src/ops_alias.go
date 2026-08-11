// Alias maintenance and the weak-query miss log: the thesaurus write path
// and its aggregation, exposed to the owner CLI as `alias add`, `alias
// remove`, and `alias review`. aliases.go owns the read path search uses;
// the operations here rewrite the same hand-editable file atomically and
// never run on a recall path.

package autojournal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

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
	key := normalizeAliasKey(term)
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
	// An existing case-variant entry is extended rather than shadowed, and
	// the rewrite below persists the collapse.
	canonicalizeAliasKeys(root)

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
	key := normalizeAliasKey(term)
	root, err := readEditableThesaurus(path)
	if err != nil {
		return "", err
	}
	// Same collapse as AddAlias: the entry being removed may exist only as
	// a case variant, and the rewrite persists the normalization.
	canonicalizeAliasKeys(root)
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

// writeThesaurusAtomic rewrites the file via a sibling temp file and rename,
// in the same two-space canonical format the owner config uses. The thesaurus
// is hand-edited, so it gets the same stable byte shape: an `alias add` should
// produce a one-line diff, not reflow the file.
func writeThesaurusAtomic(path string, root configValue) error {
	var b strings.Builder
	writeCanonicalJSON(&b, root, 0)
	if err := writeAtomicConfig(path, b.String()); err != nil {
		if errors.Is(err, ErrConfigUnavailable) {
			return fmt.Errorf("%w: %v", ErrAliasUnavailable, err)
		}
		return err
	}
	return nil
}

// canonicalizeAliasKeys rewrites the editable document's keys to their
// normalized form and merges entries that collapse to one key — value
// order preserved, repeats dropped, first entry's position kept. The
// caller's atomic rewrite persists the collapse, so a file carrying
// case-variant duplicates converges the first time it is edited.
func canonicalizeAliasKeys(root configValue) {
	merged := &orderedObject{}
	for _, kv := range root.obj.pairs {
		key := normalizeAliasKey(kv.k)
		existing, ok := merged.get(key)
		if !ok {
			merged.set(key, kv.v)
			continue
		}
		if existing.kind == kindArray && kv.v.kind == kindArray {
			for _, value := range kv.v.arr {
				dup := false
				for _, have := range existing.arr {
					if have.kind == kindString && value.kind == kindString && have.s == value.s {
						dup = true
						break
					}
				}
				if !dup {
					existing.arr = append(existing.arr, value)
				}
			}
			merged.set(key, existing)
		}
		// A duplicate whose value is not an array keeps the first entry:
		// arrays are the only alias shape, and guessing is worse.
	}
	*root.obj = *merged
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
	// The log is self-produced and self-consumed, so it carries no external
	// format obligation; compact encoding/json is exactly what AggregateMisses
	// reads back.
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
		// Trim exactly space/tab/CR, not the wider Unicode space set: two
		// queries differing only by a non-breaking space are different
		// queries, and merging them would hide a real vocabulary miss.
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

// LogSearchMiss appends one miss-log record when a search was weak enough
// to be worth reviewing: opt-in (cfg.MissLog), only for real recall outcomes
// (a match or a typed no-match, never an error), and only below the owner's
// confidence floor. One record per weak-scoring search. Best-effort by the
// same contract as AppendMiss — recall is never failed by its own
// diagnostics. This is miss-log policy, owned here beside the log it feeds,
// so the CLI and an embedding host cannot disagree about what a weak query
// is.
func LogSearchMiss(env Environ, cfg Config, query string, nowMs uint64, out *SearchOutput) {
	if !cfg.MissLog {
		return
	}
	if out.Outcome != OutcomeMatch && out.Outcome != OutcomeNoMatch {
		return
	}
	if out.BestScore >= cfg.ConfidenceFloor {
		return
	}
	logPath, err := MissLogPath(env)
	if err != nil {
		return
	}
	var top *string
	if len(out.Hits) > 0 {
		top = &out.Hits[0].EpisodeID
	}
	AppendMiss(logPath, MissRecord{
		TS:    ISOFromMs(nowMs),
		Query: query,
		Terms: out.QueryTerms,
		Best:  out.BestScore,
		Top:   top,
	}, cfg.MissLogMaxBytes)
}
