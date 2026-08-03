package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
// testdata/golden/ops-samples with the archived Zig binary (tag
// zig-final): capture the whole payload matrix into a fresh root, then
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
		if vec["outcome"] == "malformed" {
			wantCode = 2
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
}

func TestGoldenVersionLine(t *testing.T) {
	code, stdout, _ := runCLI(t, t.TempDir(), nil, "version")
	if code != 0 {
		t.Errorf("version exit = %d", code)
	}
	// The package version moves past the oracle's; every schema identity
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
