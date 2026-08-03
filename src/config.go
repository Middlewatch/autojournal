// Owner configuration. AutoJournal owns its configuration; harness-side
// wiring passes no policy. Resolution order: explicit `--config` path,
// `$AUTOJOURNAL_CONFIG`, `$XDG_CONFIG_HOME/autojournal/config.json`,
// `$HOME/.config/autojournal/config.json`.
//
// The config file is a frozen on-disk contract. Two behaviors carry the
// weight of that freeze:
//
//   - ParseConfig is a closed schema with the Zig reference's exact
//     acceptance rules, including std.json's numeric coercions: integer
//     fields also accept strings and integral floats ("5", 3.0, 3e0), and
//     float fields accept strings ("1.5", "inf"). Unknown keys, duplicate
//     keys, and wrong shapes are malformed.
//   - SaveCaptureDefaults rewrites the file byte-identically to the
//     reference: key order preserved, `world_root` migrated to
//     `journal_root` (removed from its old position, appended at the
//     end), numbers re-emitted with std.json's normalization (1.0 -> 1,
//     1e-10 -> 0.0000000001, over-i64 integers verbatim), and std.json's
//     escaping table (only control bytes, '"' and '\' escaped; UTF-8,
//     DEL and HTML-significant characters raw). This is the house
//     encoding/json deviation the Go guide carves out: the bytes on disk
//     are themselves the contract, so an explicit writer replaces struct
//     tags. Golden proof: testdata/golden/config-vectors.json.
//
// One deliberate deviation: std.json's numeric coercion also accepts
// Zig-style underscore separators inside string values ("1_0"); the Go
// port rejects them (fail closed). No real config carries them and no
// frozen byte sequence is affected.

package autojournal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MaxConfigBytes bounds the owner config file.
const MaxConfigBytes = 64 * 1024

// Config is the resolved owner configuration. Absent keys take the
// defaults baked into the field values below.
type Config struct {
	// JournalRoot is the absolute path to the owner-controlled Markdown
	// journal. Empty when the config names none: the host-neutral XDG
	// data default applies, so a config may hold only capture defaults
	// or retrieval knobs. (The wire value is optional; Go collapses
	// absent to the zero value because nothing downstream distinguishes
	// them.)
	JournalRoot     string
	DefaultWorld    string // world searched when the caller names none; recall-side convenience only
	ThesaurusPath   string // absolute path to the owner-edited alias map
	ContextWindow   uint32 // snippet context lines on each side of a matched line
	MaxResults      uint32 // default memory_search result page size
	RecencyBoost    float64
	MinScore        float64 // relevance floor; 0 disables it (legacy parity)
	ConfidenceFloor float64
	MissLog         bool
	MissLogMaxBytes uint64
	Capture         Capture
}

// Capture holds completed-turn defaults. A host adapter may override
// world/scope only when transporting an explicit per-session owner
// selection.
type Capture struct {
	World string // world that completed-turn capture publishes into
	Scope string // scope token recorded on captured episodes
}

// DefaultConfig is the configuration every absent key falls back to.
func DefaultConfig() Config {
	return Config{
		ContextWindow:   3,
		MaxResults:      10,
		RecencyBoost:    1.0,
		MinScore:        0.0,
		ConfidenceFloor: 3.0,
		MissLog:         false,
		MissLogMaxBytes: 1024 * 1024,
		Capture:         Capture{World: "main", Scope: "default"},
	}
}

// Config load failures. Malformed covers every schema violation;
// NotFound means no config could be resolved or the resolved file is
// absent; Unavailable is an I/O failure (including an over-budget file).
var (
	ErrConfigNotFound    = errors.New("config not found")
	ErrConfigMalformed   = errors.New("config malformed")
	ErrConfigUnavailable = errors.New("config unavailable")
)

// ResolvePath resolves the owner config path without reading it:
// explicit `--config`, `$AUTOJOURNAL_CONFIG`, XDG config dir,
// `$HOME/.config`. An empty explicitPath means "not provided".
func ResolvePath(env Environ, explicitPath string) (string, error) {
	if explicitPath != "" {
		return explicitPath, nil
	}
	if p, ok := env("AUTOJOURNAL_CONFIG"); ok && p != "" {
		return p, nil
	}
	// Empty or relative XDG values are invalid per the spec and ignored.
	if xdg, ok := env("XDG_CONFIG_HOME"); ok && xdg != "" && filepath.IsAbs(xdg) {
		return xdg + "/autojournal/config.json", nil
	}
	home, ok := env("HOME")
	if !ok {
		return "", ErrConfigNotFound
	}
	return home + "/.config/autojournal/config.json", nil
}

// LoadedConfig is a parsed config plus the path it came from.
type LoadedConfig struct {
	Config
	SourcePath string
}

// LoadConfig resolves, reads, and parses the owner config.
func LoadConfig(env Environ, explicitPath string) (*LoadedConfig, error) {
	path, err := ResolvePath(env, explicitPath)
	if err != nil {
		return nil, err
	}
	data, err := readConfigFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := ParseConfig(data)
	if err != nil {
		return nil, err
	}
	return &LoadedConfig{Config: cfg, SourcePath: path}, nil
}

// readConfigFile reads up to MaxConfigBytes; an over-budget file is
// Unavailable, matching the reference's limited-read error mapping.
func readConfigFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrConfigNotFound
		}
		return nil, fmt.Errorf("%w: %v", ErrConfigUnavailable, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxConfigBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConfigUnavailable, err)
	}
	if len(data) > MaxConfigBytes {
		return nil, fmt.Errorf("%w: config exceeds %d bytes", ErrConfigUnavailable, MaxConfigBytes)
	}
	return data, nil
}

// ParseConfig validates config bytes against the closed schema and
// returns the resolved Config. The acceptance rules mirror the Zig
// reference's std.json typed parse exactly, including its lenient
// numeric coercions for integer and float fields.
func ParseConfig(data []byte) (Config, error) {
	cfg := DefaultConfig()
	v, err := parseOrderedJSON(data)
	if err != nil {
		return cfg, ErrConfigMalformed
	}
	if v.kind != kindObject {
		return cfg, ErrConfigMalformed
	}
	for _, kv := range v.obj.pairs {
		switch kv.k {
		case "journal_root", "world_root", "default_world", "thesaurus_path",
			"context_window", "max_results", "recency_boost", "min_score",
			"confidence_floor", "miss_log", "miss_log_max_bytes", "capture":
			// known keys extracted below
		default:
			return cfg, ErrConfigMalformed
		}
	}
	get := func(key string) (configValue, bool) { return v.obj.get(key) }

	journalRoot, jrOK, err := optStringField(get, "journal_root")
	if err != nil {
		return cfg, err
	}
	worldRoot, wrOK, err := optStringField(get, "world_root")
	if err != nil {
		return cfg, err
	}
	// Compatibility for pre-release owner configurations: world_root
	// names the journal root. Both set to different values is malformed.
	if jrOK && wrOK && journalRoot != worldRoot {
		return cfg, ErrConfigMalformed
	}
	rootSet := jrOK || wrOK
	if jrOK {
		cfg.JournalRoot = journalRoot
	} else if wrOK {
		cfg.JournalRoot = worldRoot
	}
	var dwOK, tpOK bool
	if cfg.DefaultWorld, dwOK, err = optStringField(get, "default_world"); err != nil {
		return cfg, err
	}
	if cfg.ThesaurusPath, tpOK, err = optStringField(get, "thesaurus_path"); err != nil {
		return cfg, err
	}
	var u uint64
	var f float64
	var b bool
	if u, err = optUintField(get, "context_window", 32, uint64(cfg.ContextWindow)); err != nil {
		return cfg, err
	}
	cfg.ContextWindow = uint32(u)
	if u, err = optUintField(get, "max_results", 32, uint64(cfg.MaxResults)); err != nil {
		return cfg, err
	}
	cfg.MaxResults = uint32(u)
	if f, err = optFloatField(get, "recency_boost", cfg.RecencyBoost); err != nil {
		return cfg, err
	}
	cfg.RecencyBoost = f
	if f, err = optFloatField(get, "min_score", cfg.MinScore); err != nil {
		return cfg, err
	}
	cfg.MinScore = f
	if f, err = optFloatField(get, "confidence_floor", cfg.ConfidenceFloor); err != nil {
		return cfg, err
	}
	cfg.ConfidenceFloor = f
	if b, err = optBoolField(get, "miss_log", cfg.MissLog); err != nil {
		return cfg, err
	}
	cfg.MissLog = b
	if u, err = optUintField(get, "miss_log_max_bytes", 64, cfg.MissLogMaxBytes); err != nil {
		return cfg, err
	}
	cfg.MissLogMaxBytes = u
	if raw, ok := get("capture"); ok {
		cap, err := parseCapture(raw)
		if err != nil {
			return cfg, err
		}
		cfg.Capture = cap
	}

	// Validation, mirroring the reference's check order. Presence is
	// tracked separately from the value: an explicit empty string is
	// present, and fails the absolute-path and world-token checks.
	if rootSet && !filepath.IsAbs(cfg.JournalRoot) {
		return cfg, ErrConfigMalformed
	}
	if tpOK && !filepath.IsAbs(cfg.ThesaurusPath) {
		return cfg, ErrConfigMalformed
	}
	if dwOK && !ValidWorld(cfg.DefaultWorld) {
		return cfg, ErrConfigMalformed
	}
	// Snippets stay bounded: 10 context lines each side already triples
	// the default and approaches the whole-snippet byte cap.
	if cfg.ContextWindow == 0 || cfg.ContextWindow > 10 {
		return cfg, ErrConfigMalformed
	}
	if cfg.MaxResults == 0 {
		return cfg, ErrConfigMalformed
	}
	// The !(x >= 0) shape is deliberate: it rejects NaN, like the
	// reference. (+Inf passes, as it did there.)
	if !(cfg.RecencyBoost >= 0) || !(cfg.MinScore >= 0) || !(cfg.ConfidenceFloor >= 0) {
		return cfg, ErrConfigMalformed
	}
	if !ValidWorld(cfg.Capture.World) {
		return cfg, ErrConfigMalformed
	}
	if !ValidScope(cfg.Capture.Scope) {
		return cfg, ErrConfigMalformed
	}
	return cfg, nil
}

// parseCapture parses the nested capture object: world/scope strings with
// defaults, closed key set, null and non-strings rejected.
func parseCapture(v configValue) (Capture, error) {
	cap := Capture{World: "main", Scope: "default"}
	if v.kind != kindObject {
		return cap, ErrConfigMalformed
	}
	for _, kv := range v.obj.pairs {
		if kv.k != "world" && kv.k != "scope" {
			return cap, ErrConfigMalformed
		}
		if kv.v.kind != kindString {
			return cap, ErrConfigMalformed
		}
		if kv.k == "world" {
			cap.World = kv.v.s
		} else {
			cap.Scope = kv.v.s
		}
	}
	return cap, nil
}

// optStringField extracts an optional string field: absent or JSON null
// leaves the zero value; anything non-string is malformed.
func optStringField(get func(string) (configValue, bool), key string) (string, bool, error) {
	v, ok := get(key)
	if !ok || v.kind == kindNull {
		return "", false, nil
	}
	if v.kind != kindString {
		return "", false, ErrConfigMalformed
	}
	return v.s, true, nil
}

// optUintField extracts an unsigned integer field with the reference's
// sliceToInt coercions: integer-shaped literals parse directly; strings
// and float-shaped literals are accepted when they are exactly integral
// and in range.
func optUintField(get func(string) (configValue, bool), key string, bitSize int, def uint64) (uint64, error) {
	v, ok := get(key)
	if !ok {
		return def, nil
	}
	var lit string
	switch v.kind {
	case kindNumber, kindString:
		lit = v.s
	default:
		return 0, ErrConfigMalformed
	}
	n, ok := zigCoerceUint(lit, bitSize)
	if !ok {
		return 0, ErrConfigMalformed
	}
	return n, nil
}

// optFloatField extracts a float field: number or string literals,
// strconv grammar. Overflow to ±Inf is accepted (ErrRange tolerated),
// matching the reference's parseFloat, which returns inf for "1e999".
func optFloatField(get func(string) (configValue, bool), key string, def float64) (float64, error) {
	v, ok := get(key)
	if !ok {
		return def, nil
	}
	var lit string
	switch v.kind {
	case kindNumber, kindString:
		lit = v.s
	default:
		return 0, ErrConfigMalformed
	}
	f, err := strconv.ParseFloat(lit, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, ErrConfigMalformed
	}
	return f, nil
}

// optBoolField extracts a bool field: JSON true/false only.
func optBoolField(get func(string) (configValue, bool), key string, def bool) (bool, error) {
	v, ok := get(key)
	if !ok {
		return def, nil
	}
	if v.kind != kindBool {
		return false, ErrConfigMalformed
	}
	return v.b, nil
}

// isIntegerShaped reports whether a JSON number literal has no fraction
// or exponent and is not "-0" — the reference's
// isNumberFormattedLikeAnInteger.
func isIntegerShaped(lit string) bool {
	return lit != "-0" && !strings.ContainsAny(lit, ".eE")
}

// zigCoerceUint mirrors std.json's sliceToInt for unsigned targets:
// integer-shaped input goes through strict decimal parsing; anything else
// (strings, float-shaped literals) is parsed at f128 precision and
// accepted only when exactly integral and in range. Zig's parseFloat also
// accepts "inf"/"nan" strings; those are out of range here and rejected.
func zigCoerceUint(lit string, bitSize int) (uint64, bool) {
	if isIntegerShaped(lit) {
		n, err := strconv.ParseUint(lit, 10, bitSize)
		return n, err == nil
	}
	// f128 has a 113-bit mantissa; parsing at that precision reproduces
	// the reference's coercion boundary (e.g. 18446744073709551615.0 is
	// exactly u64 max and accepted, ...616.0 overflows and is rejected).
	f, _, err := big.ParseFloat(lit, 10, 113, big.ToNearestEven)
	if err != nil || !f.IsInt() || f.Sign() < 0 {
		return 0, false
	}
	max := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), uint(bitSize)), big.NewInt(1))
	i, _ := f.Int(nil)
	if i.Cmp(max) > 0 {
		return 0, false
	}
	return i.Uint64(), true
}

// SaveCaptureDefaults persists new capture defaults into the owner config
// with an atomic rewrite that preserves every other key the owner wrote
// (the pre-release `world_root` key is migrated to `journal_root` on the
// way). A missing config file is created holding only the capture
// defaults — the journal root may stay implicit now that the host-neutral
// default exists. A file that fails closed-schema validation is left
// untouched. Returns the path written.
//
// The output bytes are a frozen contract: key order, number
// normalization, escaping, and indentation match the Zig reference
// exactly, pinned by testdata/golden/config-vectors.json.
func SaveCaptureDefaults(env Environ, explicitPath, world, scope string) (string, error) {
	if !ValidWorld(world) || !ValidScope(scope) {
		return "", ErrConfigMalformed
	}
	path, err := ResolvePath(env, explicitPath)
	if err != nil {
		return "", err
	}

	data, err := readConfigFile(path)
	if err != nil {
		if !errors.Is(err, ErrConfigNotFound) {
			return "", err
		}
		data = []byte("{}") // a missing file is created holding only the capture defaults
	}
	root, err := parseOrderedJSON(data)
	if err != nil || root.kind != kindObject {
		return "", ErrConfigMalformed
	}

	// Migrate world_root: the value moves to a journal_root key appended
	// at the end (unless one is already present, even as null), and
	// world_root is removed from its old position.
	if legacy, ok := root.obj.get("world_root"); ok {
		if !root.obj.has("journal_root") {
			root.obj.set("journal_root", legacy)
		}
		root.obj.remove("world_root")
	}
	previousWorld := "main"
	if cap, ok := root.obj.get("capture"); ok && cap.kind == kindObject {
		if w, ok := cap.obj.get("world"); ok && w.kind == kindString {
			previousWorld = w.s
		}
	}
	capture := configValue{kind: kindObject, obj: &orderedObject{pairs: []pair{
		{k: "world", v: configValue{kind: kindString, s: world}},
		{k: "scope", v: configValue{kind: kindString, s: scope}},
	}}}
	root.obj.set("capture", capture)
	// `default_world` is the recall-side override; it follows only an
	// actual world change, so a scope-only update touches nothing else.
	if root.obj.has("default_world") && world != previousWorld {
		root.obj.set("default_world", configValue{kind: kindString, s: world})
	}

	var b strings.Builder
	writeZigJSON(&b, root, 0)
	text := b.String()
	// Never publish a config this package would refuse to load.
	if _, err := ParseConfig([]byte(text)); err != nil {
		return "", err
	}
	if err := writeAtomicConfig(path, text); err != nil {
		return "", err
	}
	return path, nil
}

// writeAtomicConfig writes text plus a trailing newline via a sibling
// temp file and rename, creating the parent directory if needed.
func writeAtomicConfig(path, text string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("%w: %v", ErrConfigUnavailable, err)
	}
	tmpPath := filepath.Join(dir, "."+filepath.Base(path)+".tmp")
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrConfigUnavailable, err)
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(tmpPath)
		}
	}()
	if _, err := f.WriteString(text + "\n"); err != nil {
		return fmt.Errorf("%w: %v", ErrConfigUnavailable, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("%w: %v", ErrConfigUnavailable, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("%w: %v", ErrConfigUnavailable, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("%w: %v", ErrConfigUnavailable, err)
	}
	ok = true
	return nil
}

// --- Ordered JSON model -------------------------------------------------
//
// The config rewrite must preserve the owner's key order and re-emit
// numbers with the reference's normalization, neither of which
// encoding/json's map-based decoding can do. This is a minimal ordered
// document model: parsing rejects duplicate keys, trailing garbage, and
// invalid UTF-8 in strings (all malformed in the reference), and writing
// reproduces std.json's indent_2 byte format.

type valueKind int

const (
	kindNull valueKind = iota
	kindBool
	kindString
	kindNumber
	kindObject
	kindArray
)

type pair struct {
	k string
	v configValue
}

// orderedObject is a key-ordered JSON object. set replaces in place when
// the key exists and appends otherwise — the exact semantics of the
// reference's array hash map, which the frozen byte order depends on.
type orderedObject struct {
	pairs []pair
}

func (o *orderedObject) get(key string) (configValue, bool) {
	for _, p := range o.pairs {
		if p.k == key {
			return p.v, true
		}
	}
	return configValue{}, false
}

func (o *orderedObject) has(key string) bool {
	_, ok := o.get(key)
	return ok
}

func (o *orderedObject) set(key string, v configValue) {
	for i := range o.pairs {
		if o.pairs[i].k == key {
			o.pairs[i].v = v
			return
		}
	}
	o.pairs = append(o.pairs, pair{k: key, v: v})
}

func (o *orderedObject) remove(key string) {
	for i := range o.pairs {
		if o.pairs[i].k == key {
			o.pairs = append(o.pairs[:i], o.pairs[i+1:]...)
			return
		}
	}
}

// configValue is one JSON value. Numbers keep their raw literal; the
// reference's normalization is applied at write time, and typed
// extraction (zigCoerceUint) needs the literal for its coercions.
type configValue struct {
	kind valueKind
	b    bool
	s    string // string value, or raw number literal
	obj  *orderedObject
	arr  []configValue
}

// parseOrderedJSON decodes one complete JSON document into the ordered
// model, rejecting duplicate keys, trailing garbage, and invalid UTF-8.
func parseOrderedJSON(data []byte) (configValue, error) {
	// The decoder silently replaces invalid UTF-8 with U+FFFD, so the
	// check has to happen on the raw document: outside strings JSON is
	// all-ASCII, and invalid bytes anywhere mean a corrupt string —
	// malformed in the reference (its scanner validates UTF-8).
	if !utf8.Valid(data) {
		return configValue{}, ErrConfigMalformed
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber() // keep number literals raw for classification/coercion
	v, err := parseOrderedValue(dec)
	if err != nil {
		return configValue{}, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return configValue{}, ErrConfigMalformed
	}
	return v, nil
}

func parseOrderedValue(dec *json.Decoder) (configValue, error) {
	tok, err := dec.Token()
	if err != nil {
		return configValue{}, ErrConfigMalformed
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := &orderedObject{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return configValue{}, ErrConfigMalformed
				}
				key, ok := kt.(string)
				if !ok || !utf8.ValidString(key) {
					return configValue{}, ErrConfigMalformed
				}
				if obj.has(key) {
					return configValue{}, ErrConfigMalformed
				}
				val, err := parseOrderedValue(dec)
				if err != nil {
					return configValue{}, err
				}
				obj.pairs = append(obj.pairs, pair{k: key, v: val})
			}
			if _, err := dec.Token(); err != nil { // closing '}'
				return configValue{}, ErrConfigMalformed
			}
			return configValue{kind: kindObject, obj: obj}, nil
		case '[':
			var arr []configValue
			for dec.More() {
				val, err := parseOrderedValue(dec)
				if err != nil {
					return configValue{}, err
				}
				arr = append(arr, val)
			}
			if _, err := dec.Token(); err != nil { // closing ']'
				return configValue{}, ErrConfigMalformed
			}
			return configValue{kind: kindArray, arr: arr}, nil
		}
		return configValue{}, ErrConfigMalformed
	case nil:
		return configValue{kind: kindNull}, nil
	case bool:
		return configValue{kind: kindBool, b: t}, nil
	case string:
		if !utf8.ValidString(t) {
			return configValue{}, ErrConfigMalformed
		}
		return configValue{kind: kindString, s: t}, nil
	case json.Number:
		return configValue{kind: kindNumber, s: string(t)}, nil
	}
	return configValue{}, ErrConfigMalformed
}

// writeZigJSON serializes v in std.json's indent_2 format: two spaces per
// level, `"key": value`, empty containers as `{}`/`[]`.
func writeZigJSON(b *strings.Builder, v configValue, indent int) {
	switch v.kind {
	case kindNull:
		b.WriteString("null")
	case kindBool:
		if v.b {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case kindString:
		writeZigJSONString(b, v.s)
	case kindNumber:
		b.WriteString(formatConfigNumber(v.s))
	case kindObject:
		if len(v.obj.pairs) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteByte('{')
		for i, p := range v.obj.pairs {
			if i > 0 {
				b.WriteByte(',')
			}
			writeIndent(b, indent+1)
			writeZigJSONString(b, p.k)
			b.WriteString(": ")
			writeZigJSON(b, p.v, indent+1)
		}
		writeIndent(b, indent)
		b.WriteByte('}')
	case kindArray:
		if len(v.arr) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteByte('[')
		for i, e := range v.arr {
			if i > 0 {
				b.WriteByte(',')
			}
			writeIndent(b, indent+1)
			writeZigJSON(b, e, indent+1)
		}
		writeIndent(b, indent)
		b.WriteByte(']')
	}
}

func writeIndent(b *strings.Builder, indent int) {
	b.WriteByte('\n')
	for i := 0; i < 2*indent; i++ {
		b.WriteByte(' ')
	}
}

// writeZigJSONString applies the reference's escaping table: only control
// bytes below 0x20, '"', and '\' are escaped (\b \f \n \r \t short forms,
// \u00xx lowercase otherwise); every other byte — UTF-8 continuation
// bytes, DEL, and the HTML-significant characters encoding/json would
// escape — passes through raw.
func writeZigJSONString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\':
			b.WriteString("\\\\")
		case '"':
			b.WriteString("\\\"")
		case 0x08:
			b.WriteString("\\b")
		case 0x0C:
			b.WriteString("\\f")
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		case '\t':
			b.WriteString("\\t")
		default:
			if c < 0x20 {
				fmt.Fprintf(b, "\\u%04x", c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
}

// formatConfigNumber re-emits a JSON number literal the way the
// reference's Value round-trip does: integer-shaped literals that fit an
// i64 print as-is (they are already canonical), over-i64 integer literals
// and non-finite floats print verbatim (number_string), and everything
// else is parsed to f64 and printed in full decimal notation — shortest
// round-trip digits placed positionally, never scientific (1e-10 becomes
// 0.0000000001, 1e300 becomes 1 followed by 300 zeros).
func formatConfigNumber(lit string) string {
	if isIntegerShaped(lit) {
		// Integer-shaped literals always print verbatim: fitting an i64
		// they are already canonical (JSON forbids leading zeros and
		// "-0" is excluded); overflowing an i64 they are kept as a
		// number_string, also verbatim.
		return lit
	}
	f, err := strconv.ParseFloat(lit, 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return lit // non-finite: kept verbatim
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}
