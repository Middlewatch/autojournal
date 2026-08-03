package autojournal

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMinimalConfigParsesWithDefaults(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{"journal_root": "/tmp/journals"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ContextWindow != 3 || cfg.MaxResults != 10 || cfg.MissLog {
		t.Errorf("defaults = %+v", cfg)
	}
	if cfg.DefaultWorld != "" || cfg.ThesaurusPath != "" {
		t.Errorf("unset optionals = %q/%q", cfg.DefaultWorld, cfg.ThesaurusPath)
	}
	if cfg.Capture.World != "main" || cfg.Capture.Scope != "default" {
		t.Errorf("capture defaults = %+v", cfg.Capture)
	}
	if cfg.RecencyBoost != 1.0 || cfg.MinScore != 0.0 || cfg.ConfidenceFloor != 3.0 {
		t.Errorf("retrieval floats = %+v", cfg)
	}
	if cfg.MissLogMaxBytes != 1024*1024 {
		t.Errorf("miss log cap = %d", cfg.MissLogMaxBytes)
	}
}

func TestClosedConfigSchemaRejections(t *testing.T) {
	cases := []struct {
		name string
		json string
	}{
		{"unknown key", `{"journal_root": "/j", "surprise": 1}`},
		{"duplicate key", `{"journal_root": "/j", "journal_root": "/k"}`},
		{"relative journal root", `{"journal_root": "relative/path"}`},
		{"relative thesaurus path", `{"journal_root": "/j", "thesaurus_path": "thesaurus.json"}`},
		{"invalid default world", `{"journal_root": "/j", "default_world": "Bad World"}`},
		{"zero context window", `{"journal_root": "/j", "context_window": 0}`},
		{"over-budget context window", `{"journal_root": "/j", "context_window": 11}`},
		{"zero max results", `{"journal_root": "/j", "max_results": 0}`},
		{"invalid capture world", `{"journal_root": "/j", "capture": {"world": "Bad World"}}`},
		{"invalid capture scope", `{"journal_root": "/j", "capture": {"scope": "has space"}}`},
		{"capture null", `{"journal_root": "/j", "capture": null}`},
		{"capture unknown key", `{"journal_root": "/j", "capture": {"world": "main", "zz": 1}}`},
		{"capture world number", `{"capture": {"world": 5}}`},
		{"conflicting roots", `{"journal_root": "/new", "world_root": "/old"}`},
		{"non-object root", `[]`},
		{"number for string field", `{"journal_root": 5}`},
		{"string for bool field", `{"journal_root": "/j", "miss_log": "true"}`},
		{"fractional int", `{"journal_root": "/j", "context_window": 3.5}`},
		{"u32 overflow", `{"journal_root": "/j", "context_window": 4294967296}`},
		{"negative unsigned", `{"journal_root": "/j", "context_window": -1}`},
		{"negative float unsigned", `{"journal_root": "/j", "context_window": -1.0}`},
		{"null for int field", `{"journal_root": "/j", "context_window": null}`},
		{"null capture world", `{"capture": {"world": null}}`},
		{"empty journal root", `{"journal_root": ""}`},
		{"empty world root", `{"world_root": ""}`},
		{"empty thesaurus path", `{"thesaurus_path": ""}`},
		{"empty default world", `{"default_world": ""}`},
		{"empty capture world", `{"capture": {"world": ""}}`},
		{"trailing garbage", `{"journal_root": "/j"} extra`},
		{"invalid utf-8 in string", "{\"journal_root\": \"/j\xff\"}"},
		{"negative min score", `{"journal_root": "/j", "min_score": -0.5}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseConfig([]byte(tc.json)); !errors.Is(err, ErrConfigMalformed) {
				t.Fatalf("ParseConfig error = %v, want ErrConfigMalformed", err)
			}
		})
	}
}

// The reference's std.json typed parse is lenient about numeric shapes in
// ways encoding/json is not; these acceptances are frozen behavior.
func TestConfigNumericCoercions(t *testing.T) {
	cases := []struct {
		name  string
		json  string
		check func(t *testing.T, cfg Config)
	}{
		{"int from string", `{"context_window": "5"}`, func(t *testing.T, c Config) {
			if c.ContextWindow != 5 {
				t.Errorf("context_window = %d", c.ContextWindow)
			}
		}},
		{"int from integral float", `{"context_window": 3.0}`, func(t *testing.T, c Config) {
			if c.ContextWindow != 3 {
				t.Errorf("context_window = %d", c.ContextWindow)
			}
		}},
		{"int from exponent form", `{"context_window": 3e0}`, func(t *testing.T, c Config) {
			if c.ContextWindow != 3 {
				t.Errorf("context_window = %d", c.ContextWindow)
			}
		}},
		{"int from float string", `{"context_window": "3.0"}`, func(t *testing.T, c Config) {
			if c.ContextWindow != 3 {
				t.Errorf("context_window = %d", c.ContextWindow)
			}
		}},
		{"u64 max from string", `{"miss_log_max_bytes": "18446744073709551615"}`, func(t *testing.T, c Config) {
			if c.MissLogMaxBytes != 18446744073709551615 {
				t.Errorf("miss_log_max_bytes = %d", c.MissLogMaxBytes)
			}
		}},
		{"float from string", `{"recency_boost": "1.5"}`, func(t *testing.T, c Config) {
			if c.RecencyBoost != 1.5 {
				t.Errorf("recency_boost = %v", c.RecencyBoost)
			}
		}},
		{"float overflow is inf and accepted", `{"recency_boost": 1e999}`, func(t *testing.T, c Config) {
			if !math.IsInf(c.RecencyBoost, 1) {
				t.Errorf("recency_boost = %v", c.RecencyBoost)
			}
		}},
		{"inf string accepted", `{"recency_boost": "inf"}`, func(t *testing.T, c Config) {
			if !math.IsInf(c.RecencyBoost, 1) {
				t.Errorf("recency_boost = %v", c.RecencyBoost)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseConfig([]byte(tc.json))
			if err != nil {
				t.Fatalf("ParseConfig: %v", err)
			}
			tc.check(t, cfg)
		})
	}
	// -0 coerces to 0 through the float path (it is deliberately not
	// "integer-shaped"), which the context_window validation then rejects.
	if _, err := ParseConfig([]byte(`{"context_window": -0}`)); !errors.Is(err, ErrConfigMalformed) {
		t.Errorf("-0 context_window error = %v, want ErrConfigMalformed (coerces to 0)", err)
	}
	// One past u64 max overflows the coercion range even at f128 precision.
	if _, err := ParseConfig([]byte(`{"miss_log_max_bytes": 18446744073709551616.0}`)); !errors.Is(err, ErrConfigMalformed) {
		t.Errorf("u64 max+1 float error = %v, want ErrConfigMalformed", err)
	}
}

func TestLegacyWorldRootAccepted(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{"world_root": "/tmp/legacy-journals"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.JournalRoot != "/tmp/legacy-journals" {
		t.Errorf("journal root = %q", cfg.JournalRoot)
	}
}

func TestConfigWithoutJournalRootIsValid(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{"capture": {"world": "team", "scope": "default"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.JournalRoot != "" {
		t.Errorf("journal root = %q", cfg.JournalRoot)
	}
	if cfg.Capture.World != "team" {
		t.Errorf("capture world = %q", cfg.Capture.World)
	}
}

func TestRetrievalKnobsHonored(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{"journal_root": "/j", "default_world": "willow", "confidence_floor": 2.5,
"miss_log": true, "thesaurus_path": "/home/x/thesaurus.json"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultWorld != "willow" || cfg.ConfidenceFloor != 2.5 || !cfg.MissLog {
		t.Errorf("cfg = %+v", cfg)
	}
	if cfg.ThesaurusPath != "/home/x/thesaurus.json" {
		t.Errorf("thesaurus path = %q", cfg.ThesaurusPath)
	}
}

func TestResolvePathOrder(t *testing.T) {
	env := mapEnviron(
		"HOME", "/home/x",
		"AUTOJOURNAL_CONFIG", "/env/config.json",
		"XDG_CONFIG_HOME", "/xdg",
	)
	p, err := ResolvePath(env, "/explicit/config.json")
	if err != nil || p != "/explicit/config.json" {
		t.Errorf("explicit = %q, %v", p, err)
	}
	p, err = ResolvePath(env, "")
	if err != nil || p != "/env/config.json" {
		t.Errorf("env = %q, %v", p, err)
	}
	p, err = ResolvePath(mapEnviron("HOME", "/home/x", "XDG_CONFIG_HOME", "/xdg"), "")
	if err != nil || p != "/xdg/autojournal/config.json" {
		t.Errorf("xdg = %q, %v", p, err)
	}
	// A relative XDG value is invalid and ignored.
	p, err = ResolvePath(mapEnviron("HOME", "/home/x", "XDG_CONFIG_HOME", "rel"), "")
	if err != nil || p != "/home/x/.config/autojournal/config.json" {
		t.Errorf("home fallback = %q, %v", p, err)
	}
	if _, err := ResolvePath(mapEnviron(), ""); !errors.Is(err, ErrConfigNotFound) {
		t.Errorf("no HOME err = %v, want ErrConfigNotFound", err)
	}
}

func TestSaveCaptureDefaultsSemantics(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	env := mapEnviron()

	// A missing file is created with only the capture defaults.
	written, err := SaveCaptureDefaults(env, path, "team", "default")
	if err != nil {
		t.Fatal(err)
	}
	if written != path {
		t.Errorf("written = %q", written)
	}
	loaded, err := LoadConfig(env, path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Config.JournalRoot != "" || loaded.Config.Capture.World != "team" {
		t.Errorf("created config = %+v", loaded.Config)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("config mode = %o", info.Mode().Perm())
	}

	// A legacy config keeps its other keys, migrates the root key, and
	// the recall-side override follows the new default.
	if err := os.WriteFile(path, []byte(`{"world_root": "/j", "default_world": "old", "miss_log": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveCaptureDefaults(env, path, "willow", "global"); err != nil {
		t.Fatal(err)
	}
	loaded, err = LoadConfig(env, path)
	if err != nil {
		t.Fatal(err)
	}
	c := loaded.Config
	if c.JournalRoot != "/j" || c.DefaultWorld != "willow" ||
		c.Capture.World != "willow" || c.Capture.Scope != "global" || !c.MissLog {
		t.Errorf("migrated config = %+v", c)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "world_root") {
		t.Error("world_root key survived migration")
	}

	// A scope-only update leaves a diverged recall-side override
	// untouched.
	if err := os.WriteFile(path, []byte(`{"default_world": "willow", "capture": {"world": "main", "scope": "default"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveCaptureDefaults(env, path, "main", "work"); err != nil {
		t.Fatal(err)
	}
	loaded, err = LoadConfig(env, path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Config.DefaultWorld != "willow" || loaded.Config.Capture.Scope != "work" {
		t.Errorf("scope-only config = %+v", loaded.Config)
	}

	// Invalid identities and a malformed existing file are refused, and
	// the file is left untouched.
	if _, err := SaveCaptureDefaults(env, path, "Bad World", "default"); !errors.Is(err, ErrConfigMalformed) {
		t.Errorf("bad world err = %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"surprise": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveCaptureDefaults(env, path, "team", "default"); !errors.Is(err, ErrConfigMalformed) {
		t.Errorf("malformed existing err = %v", err)
	}
	raw, _ = os.ReadFile(path)
	if string(raw) != `{"surprise": 1}` {
		t.Errorf("malformed file was modified: %q", raw)
	}
}

// The save path validates the *rewritten* document, not the input — the
// reference parses the existing file as a generic JSON value, mutates,
// and only then applies the closed schema. Inputs whose malformed parts
// are exactly the ones the mutation replaces are therefore repaired, not
// rejected. (The CLI never reaches this: it loads the config, with the
// closed schema, before dispatching `default`.) These cases pin the
// reference's library semantics so they are not "fixed" into divergence.
func TestSaveCaptureDefaultsRepairsWhatTheMutationReplaces(t *testing.T) {
	env := mapEnviron()
	cases := []struct {
		name   string
		before string
		world  string
		scope  string
		check  func(t *testing.T, cfg Config)
	}{
		{"null capture", `{"journal_root":"/j","capture":null}`, "team", "default",
			func(t *testing.T, c Config) {
				if c.Capture.World != "team" {
					t.Errorf("capture = %+v", c.Capture)
				}
			}},
		{"conflicting roots lose world_root", `{"journal_root":"/new","world_root":"/old"}`, "team", "default",
			func(t *testing.T, c Config) {
				if c.JournalRoot != "/new" {
					t.Errorf("journal root = %q", c.JournalRoot)
				}
			}},
		{"invalid default_world follows world change", `{"default_world":"Bad World"}`, "team", "default",
			func(t *testing.T, c Config) {
				if c.DefaultWorld != "team" {
					t.Errorf("default world = %q", c.DefaultWorld)
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tc.before), 0o600); err != nil {
				t.Fatal(err)
			}
			// The input does not pass the closed schema on its own.
			if _, err := ParseConfig([]byte(tc.before)); !errors.Is(err, ErrConfigMalformed) {
				t.Fatalf("before config parses: %v", err)
			}
			if _, err := SaveCaptureDefaults(env, path, tc.world, tc.scope); err != nil {
				t.Fatalf("SaveCaptureDefaults: %v", err)
			}
			loaded, err := LoadConfig(env, path)
			if err != nil {
				t.Fatalf("LoadConfig after repair: %v", err)
			}
			tc.check(t, loaded.Config)
		})
	}

	// But a malformed part the mutation does NOT replace still refuses,
	// leaving the file untouched: a scope-only save does not touch a
	// diverged default_world, so the invalid value survives to validation
	// and the whole save fails.
	path := filepath.Join(t.TempDir(), "config.json")
	before := `{"default_world":"Bad World","capture":{"world":"main","scope":"default"}}`
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveCaptureDefaults(env, path, "main", "work"); !errors.Is(err, ErrConfigMalformed) {
		t.Fatalf("scope-only save err = %v, want ErrConfigMalformed", err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != before {
		t.Error("refused save modified the file")
	}
}
