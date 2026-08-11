package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	aj "github.com/Middlewatch/autojournal/src"
)

const (
	payloadsDir = "../../../testdata/payloads"
	goldenDir   = "../../../testdata/golden"
)

// runCLI drives one command through the cli struct with an isolated fake
// environment (only HOME set) and a frozen clock.
func runCLI(t *testing.T, home string, stdin []byte, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut strings.Builder
	c := &cli{
		env: func(key string) (string, bool) {
			if key == "HOME" {
				return home, true
			}
			return "", false
		},
		stdin:  bytes.NewReader(stdin),
		stdout: &out,
		stderr: &errOut,
		nowMs:  func() uint64 { return 1785240000000 },
	}
	code = c.run(args)
	return code, out.String(), errOut.String()
}

func readGolden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(goldenDir, "ops-samples", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return string(b)
}

// TestGoldenOpsSamples replays the exact procedure that generated
// testdata/golden/ops-samples pins the CLI's machine surface: capture the
// whole payload matrix into a fresh root, then
// redeliver basic.json unchanged (duplicate) and with changed content
// (conflict), then take status and catalog. The Go CLI's stdout must be
// byte-identical after substituting the scratch paths.
func TestGoldenOpsSamples(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	rootPath := filepath.Join(base, "root")
	indexPath := filepath.Join(base, "idx.sqlite")
	captureArgs := []string{"capture", "--root", rootPath, "--index", indexPath, "--json"}

	vectorBytes, err := os.ReadFile(filepath.Join(goldenDir, "capture-vectors.json"))
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var vectors map[string]map[string]any
	if err := json.Unmarshal(vectorBytes, &vectors); err != nil {
		t.Fatalf("parse vectors: %v", err)
	}

	payloads, err := filepath.Glob(filepath.Join(payloadsDir, "*.json"))
	if err != nil || len(payloads) == 0 {
		t.Fatalf("payload matrix missing: %v", err)
	}
	for _, path := range payloads {
		name := strings.TrimSuffix(filepath.Base(path), ".json")
		payload, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		code, stdout, _ := runCLI(t, home, payload, captureArgs...)
		var report map[string]any
		if err := json.Unmarshal([]byte(stdout), &report); err != nil {
			t.Fatalf("%s: capture stdout is not JSON: %q", name, stdout)
		}
		vec, ok := vectors[name]
		if !ok {
			t.Fatalf("%s: no capture vector", name)
		}
		for _, field := range []string{"outcome", "episode_id", "payload_digest", "path"} {
			if report[field] != vec[field] {
				t.Errorf("%s: %s = %v, want %v", name, field, report[field], vec[field])
			}
		}
		wantCode := 0
		switch vec["outcome"] {
		case "malformed":
			wantCode = 2
		case "conflict":
			// The one capture outcome that exits non-zero without being a
			// failure report; superseded is a success and stays 0.
			wantCode = 3
		}
		if code != wantCode {
			t.Errorf("%s: exit = %d, want %d", name, code, wantCode)
		}
	}

	// Redelivering basic.json unchanged is a duplicate (exit 0).
	basic, err := os.ReadFile(filepath.Join(payloadsDir, "basic.json"))
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, _ := runCLI(t, home, basic, captureArgs...)
	if code != 0 {
		t.Errorf("duplicate exit = %d", code)
	}
	if stdout != readGolden(t, "capture-duplicate.json") {
		t.Errorf("duplicate report:\n got %q\nwant %q", stdout, readGolden(t, "capture-duplicate.json"))
	}

	// The same identity with changed content is a conflict (exit 3).
	var conflictPayload map[string]any
	if err := json.Unmarshal(basic, &conflictPayload); err != nil {
		t.Fatal(err)
	}
	conflictPayload["user_content"] = "changed content but same identity"
	conflictBytes, err := json.Marshal(conflictPayload)
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, _ = runCLI(t, home, conflictBytes, captureArgs...)
	if code != 3 {
		t.Errorf("conflict exit = %d", code)
	}
	if stdout != readGolden(t, "capture-conflict.json") {
		t.Errorf("conflict report:\n got %q\nwant %q", stdout, readGolden(t, "capture-conflict.json"))
	}

	// Status and catalog over the built corpus, path-substituted.
	code, stdout, _ = runCLI(t, home, nil, "status", "--root", rootPath, "--index", indexPath, "--json")
	if code != 0 {
		t.Errorf("status exit = %d", code)
	}
	wantStatus := readGolden(t, "status.json")
	wantStatus = strings.ReplaceAll(wantStatus, "/tmp/aj-golden.rTwa4g/root", rootPath)
	wantStatus = strings.ReplaceAll(wantStatus, "/tmp/aj-golden.rTwa4g/idx.sqlite", indexPath)
	if stdout != wantStatus {
		t.Errorf("status report:\n got %q\nwant %q", stdout, wantStatus)
	}

	code, stdout, _ = runCLI(t, home, nil, "catalog", "--root", rootPath, "--index", indexPath, "--json")
	if code != 0 {
		t.Errorf("catalog exit = %d", code)
	}
	if stdout != readGolden(t, "catalog.json") {
		t.Errorf("catalog report:\n got %q\nwant %q", stdout, readGolden(t, "catalog.json"))
	}

	code, stdout, _ = runCLI(t, home, nil, "sync", "--root", rootPath, "--index", indexPath, "--json")
	if code != 0 {
		t.Errorf("sync exit = %d", code)
	}
	if stdout != readGolden(t, "sync.json") {
		t.Errorf("sync report:\n got %q\nwant %q", stdout, readGolden(t, "sync.json"))
	}

	// A clean corpus reseals nothing, and says so in the pinned shape.
	code, stdout, _ = runCLI(t, home, nil, "reseal", "--root", rootPath, "--index", indexPath, "--json")
	if code != 0 {
		t.Errorf("reseal exit = %d", code)
	}
	if stdout != readGolden(t, "reseal.json") {
		t.Errorf("reseal report:\n got %q\nwant %q", stdout, readGolden(t, "reseal.json"))
	}
}

// TestResealCommandJSONShape pins the reseal --json machine surface byte
// for byte against the golden sample, over the same payload matrix the
// other ops samples use, from a fresh corpus of its own.
func TestResealCommandJSONShape(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	rootPath := filepath.Join(base, "root")
	indexPath := filepath.Join(base, "idx.sqlite")
	payloads, err := filepath.Glob(filepath.Join(payloadsDir, "*.json"))
	if err != nil || len(payloads) == 0 {
		t.Fatalf("payload matrix missing: %v", err)
	}
	for _, path := range payloads {
		payload, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		runCLI(t, home, payload, "capture", "--root", rootPath, "--index", indexPath, "--json")
	}
	code, stdout, _ := runCLI(t, home, nil, "reseal", "--root", rootPath, "--index", indexPath, "--json")
	if code != 0 {
		t.Errorf("reseal exit = %d", code)
	}
	if stdout != readGolden(t, "reseal.json") {
		t.Errorf("reseal report:\n got %q\nwant %q", stdout, readGolden(t, "reseal.json"))
	}
}

func TestGoldenVersionLine(t *testing.T) {
	code, stdout, _ := runCLI(t, t.TempDir(), nil, "version")
	if code != 0 {
		t.Errorf("version exit = %d", code)
	}
	// The package version moves with releases; every schema identity
	// in the parenthesis stays pinned to it.
	golden := readGolden(t, "version.txt")
	fields := strings.SplitN(golden, " ", 3)
	if len(fields) != 3 {
		t.Fatalf("unparseable golden version line: %q", golden)
	}
	want := fields[0] + " " + aj.PackageVersion + " " + fields[2]
	if stdout != want {
		t.Errorf("version:\n got %q\nwant %q", stdout, want)
	}
}

func TestLaneListParsing(t *testing.T) {
	lanes := parseLanes("conversation, evaluation")
	if len(lanes) != 2 || lanes[1] != "evaluation" {
		t.Errorf("lanes = %v", lanes)
	}
	if deduped := parseLanes("conversation,conversation"); len(deduped) != 1 {
		t.Errorf("deduped = %v", deduped)
	}
	if parseLanes("gossip") != nil {
		t.Error("accepted unknown lane")
	}
	if parseLanes("") != nil {
		t.Error("accepted empty lane list")
	}
}

func TestLineSpanParsing(t *testing.T) {
	span := parseLineSpan("19-40")
	if span == nil || span.start != 19 || span.end != 40 {
		t.Errorf("span = %+v", span)
	}
	single := parseLineSpan("21")
	if single == nil || single.start != 21 || single.end != 21 {
		t.Errorf("single = %+v", single)
	}
	for _, bad := range []string{"40-19", "0", "abc"} {
		if parseLineSpan(bad) != nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// TestSyncJSONMatchesTextAccounting: the two sync forms report the same
// numbers over the same corpus state, including the digest-mismatch count.
func TestSyncJSONMatchesTextAccounting(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	rootPath := filepath.Join(base, "root")
	indexPath := filepath.Join(base, "idx.sqlite")

	payload, err := os.ReadFile(filepath.Join(payloadsDir, "basic.json"))
	if err != nil {
		t.Fatal(err)
	}
	if code, _, _ := runCLI(t, home, payload, "capture", "--root", rootPath, "--index", indexPath, "--json"); code != 0 {
		t.Fatalf("capture exit = %d", code)
	}

	// Hand-edit the published body, digest line untouched, so the report
	// has a non-zero digest_mismatch to compare across forms.
	var published string
	filepath.Walk(rootPath, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(p, ".md") {
			published = p
		}
		return nil
	})
	if published == "" {
		t.Fatal("no published episode found")
	}
	b, err := os.ReadFile(published)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(b), "## Assistant\n\n", "## Assistant\n\nedited: ", 1)
	if edited == string(b) {
		t.Fatal("edit did not apply")
	}
	if err := os.WriteFile(published, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}

	code, jsonOut, _ := runCLI(t, home, nil, "sync", "--root", rootPath, "--index", indexPath, "--json")
	if code != 0 {
		t.Fatalf("sync --json exit = %d", code)
	}
	var got map[string]uint64
	if err := json.Unmarshal([]byte(jsonOut), &got); err != nil {
		t.Fatalf("sync --json is not JSON: %q", jsonOut)
	}

	code, textOut, _ := runCLI(t, home, nil, "sync", "--root", rootPath, "--index", indexPath)
	if code != 0 {
		t.Fatalf("sync exit = %d", code)
	}
	fromText := map[string]uint64{}
	for _, line := range strings.Split(strings.TrimSpace(textOut), "\n") {
		var k string
		var v uint64
		if _, err := fmt.Sscanf(line, "%s %d", &k, &v); err != nil {
			t.Fatalf("unparseable text line %q", line)
		}
		fromText[strings.TrimSuffix(k, ":")] = v
	}
	if len(fromText) != len(got) {
		t.Fatalf("field sets differ: text %v vs json %v", fromText, got)
	}
	// The corpus did not change between the runs, so every count must agree
	// except indexed/unchanged, which trade places once the first sync has
	// stamped the content hashes.
	if got["digest_mismatch"] != 1 || fromText["digest_mismatch"] != 1 {
		t.Errorf("digest_mismatch: json %d, text %d, want 1", got["digest_mismatch"], fromText["digest_mismatch"])
	}
	for _, k := range []string{"removed", "skipped_malformed", "duplicate_ids", "digest_mismatch"} {
		if got[k] != fromText[k] {
			t.Errorf("%s: json %d != text %d", k, got[k], fromText[k])
		}
	}
	if got["indexed"]+got["unchanged"] != fromText["indexed"]+fromText["unchanged"] {
		t.Errorf("indexed+unchanged: json %d != text %d",
			got["indexed"]+got["unchanged"], fromText["indexed"]+fromText["unchanged"])
	}
}

func TestPrintJSONFailsOnEncodeError(t *testing.T) {
	var out, errOut strings.Builder
	c := &cli{stdout: &out, stderr: &errOut}
	// A channel is unserializable; the failure must reach the exit code and
	// leave stdout empty — zero bytes with a success exit is the one
	// combination no consumer can detect.
	if err := c.printJSON(make(chan int)); err == nil {
		t.Fatal("printJSON accepted an unserializable value")
	}
	if out.Len() != 0 {
		t.Errorf("stdout carries %d bytes after a failed encode", out.Len())
	}
	if errOut.Len() == 0 {
		t.Error("stderr silent on a failed encode")
	}
	if code := c.emitJSON(make(chan int)); code != exitFailure {
		t.Errorf("emitJSON exit = %d, want %d", code, exitFailure)
	}
}

func TestLimitZeroResolvesToDefaultPageSize(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	rootPath := filepath.Join(base, "root")
	indexPath := filepath.Join(base, "idx.sqlite")
	for i, turn := range []string{"turn-a", "turn-b"} {
		payload := fmt.Sprintf(`{
			"schema_version": 1, "world": "limitworld", "scope": "global",
			"lane": "conversation", "harness": "verify", "adapter_version": "0.0.0",
			"session_id": "sess-limit", "turn_id": %q, "event_time_ms": %d,
			"capture_policy": "default-v1", "turn_outcome": "completed",
			"user_content": "the quokka census run %d", "assistant_result": "counted."
		}`, turn, 1785240000000+int64(i), i)
		code, _, _ := runCLI(t, home, []byte(payload),
			"capture", "--root", rootPath, "--index", indexPath, "--json")
		if code != 0 {
			t.Fatalf("capture %s exit = %d", turn, code)
		}
	}
	// --limit 0 means the default page size (10), not a clamp to one: both
	// matching episodes come back.
	code, stdout, _ := runCLI(t, home, nil, "search", "quokka",
		"--root", rootPath, "--index", indexPath,
		"--world", "limitworld", "--scope", "global", "--limit", "0", "--json")
	if code != 0 {
		t.Fatalf("search exit = %d (%s)", code, stdout)
	}
	var report struct {
		Results []any  `json:"results"`
		Total   uint64 `json:"total"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("search stdout is not JSON: %q", stdout)
	}
	if len(report.Results) != 2 || report.Total != 2 {
		t.Errorf("results = %d (total %d), want both episodes on the default page",
			len(report.Results), report.Total)
	}
}

func TestAliasListReportsMergedKeys(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	thesaurusDir := filepath.Join(home, ".config", "autojournal")
	if err := os.MkdirAll(thesaurusDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(thesaurusDir, "thesaurus.json"),
		[]byte(`{"FW": ["fwupd"], "fw": ["polkit"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, _ := runCLI(t, home, nil, "alias", "list", "--json")
	if code != 0 {
		t.Fatalf("alias list exit = %d (%s)", code, stdout)
	}
	var report struct {
		MergedKeys int `json:"merged_keys"`
		Entries    []struct {
			Key    string   `json:"key"`
			Values []string `json:"values"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("alias list stdout is not JSON: %q", stdout)
	}
	if report.MergedKeys != 1 {
		t.Errorf("merged_keys = %d, want 1", report.MergedKeys)
	}
	if len(report.Entries) != 1 || report.Entries[0].Key != "fw" ||
		len(report.Entries[0].Values) != 2 {
		t.Errorf("entries = %+v, want one merged fw entry", report.Entries)
	}
}

// TestClockFromEnvPinsAndFallsBack pins the parity-clock seam: a decimal
// AUTOJOURNAL_NOW_MS wins verbatim, and every other spelling — unset,
// empty, junk, a sign, a fraction — is ignored so the wall clock wins.
func TestClockFromEnvPinsAndFallsBack(t *testing.T) {
	envWith := func(value string, present bool) aj.Environ {
		return func(key string) (string, bool) {
			if key == "AUTOJOURNAL_NOW_MS" && present {
				return value, true
			}
			return "", false
		}
	}

	if got := clockFromEnv(envWith("1785326400000", true))(); got != 1785326400000 {
		t.Errorf("pinned clock = %d, want 1785326400000", got)
	}

	before := uint64(max(0, time.Now().UnixMilli()))
	for _, tc := range []struct {
		name    string
		value   string
		present bool
	}{
		{"unset", "", false},
		{"empty", "", true},
		{"junk", "not-a-number", true},
		{"signed", "+1785326400000", true},
		{"fraction", "1785326400000.5", true},
	} {
		got := clockFromEnv(envWith(tc.value, tc.present))()
		if got < before {
			t.Errorf("%s: clock = %d, want wall clock >= %d", tc.name, got, before)
		}
	}
}
