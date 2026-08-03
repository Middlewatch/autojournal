package autojournal

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Golden parity harness. testdata/payloads is the capture contract matrix;
// testdata/golden/capture-vectors.json records what the archived Zig binary
// (git tag zig-final) did with each payload — outcome, episode id, payload
// digest, and published path — and testdata/golden/episodes holds the exact
// episode bytes it wrote. The Go port is done when every module's output is
// indistinguishable from that oracle.

const (
	payloadsDir = "../testdata/payloads"
	goldenDir   = "../testdata/golden"
)

type captureVector struct {
	Outcome       string  `json:"outcome"`
	EpisodeID     *string `json:"episode_id"`
	PayloadDigest *string `json:"payload_digest"`
	Path          *string `json:"path"`
}

func loadVectors(t *testing.T) map[string]captureVector {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(goldenDir, "capture-vectors.json"))
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var m map[string]captureVector
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("parse vectors: %v", err)
	}
	return m
}

// validateAsCaptureHost mirrors the CLI's config merge: omitted world/scope
// are filled from owner defaults (main/default here) before validation.
func validateAsCaptureHost(t *testing.T, raw RawPayload) (Payload, error) {
	t.Helper()
	if raw.World == nil {
		s := "main"
		raw.World = &s
	}
	if raw.Scope == nil {
		s := "default"
		raw.Scope = &s
	}
	return Validate(raw)
}

func TestGoldenCaptureVectors(t *testing.T) {
	for name, vec := range loadVectors(t) {
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join(payloadsDir, name+".json"))
			if err != nil {
				t.Fatalf("read payload: %v", err)
			}
			raw, parseErr := ParsePayload(b)
			if vec.Outcome == string(CaptureMalformed) {
				if parseErr == nil {
					if _, valErr := Validate(raw); valErr == nil {
						t.Fatal("payload the oracle rejected validated cleanly")
					}
				}
				return
			}
			if vec.Outcome != string(CapturePublished) {
				t.Fatalf("unhandled vector outcome %q", vec.Outcome)
			}
			if parseErr != nil {
				t.Fatalf("ParsePayload: %v", parseErr)
			}
			p, err := validateAsCaptureHost(t, raw)
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if got := EpisodeID(&p); got != *vec.EpisodeID {
				t.Errorf("episode id = %q, oracle %q", got, *vec.EpisodeID)
			}
			if got := DigestPrefix + PayloadDigestHex(&p); got != *vec.PayloadDigest {
				t.Errorf("payload digest = %q, oracle %q", got, *vec.PayloadDigest)
			}
		})
	}
}

// TestGoldenEpisodeBytes re-renders every oracle episode with the capture
// time read out of the oracle file itself and demands byte identity.
func TestGoldenEpisodeBytes(t *testing.T) {
	for name, vec := range loadVectors(t) {
		if vec.Outcome != string(CapturePublished) {
			continue
		}
		t.Run(name, func(t *testing.T) {
			golden, err := os.ReadFile(filepath.Join(goldenDir, "episodes", name+".md"))
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}
			ep := ParseEpisode(string(golden))
			if ep == nil {
				t.Fatal("ParseEpisode rejected oracle episode")
			}
			b, err := os.ReadFile(filepath.Join(payloadsDir, name+".json"))
			if err != nil {
				t.Fatalf("read payload: %v", err)
			}
			raw, err := ParsePayload(b)
			if err != nil {
				t.Fatalf("ParsePayload: %v", err)
			}
			p, err := validateAsCaptureHost(t, raw)
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			rendered := Render(RenderInput{
				Payload:       &p,
				EpisodeID:     ep.EpisodeID,
				DigestHex:     ep.DigestHex,
				CaptureTimeMs: ep.CaptureTimeMs,
			})
			if !bytes.Equal(rendered, golden) {
				t.Fatalf("rendered episode differs from oracle\n--- oracle ---\n%s\n--- rendered ---\n%s", golden, rendered)
			}
			// The oracle's frontmatter facts must agree with the vectors.
			if ep.EpisodeID != *vec.EpisodeID {
				t.Errorf("frontmatter id = %q, vector %q", ep.EpisodeID, *vec.EpisodeID)
			}
			if DigestPrefix+ep.DigestHex != *vec.PayloadDigest {
				t.Errorf("frontmatter digest = %q, vector %q", ep.DigestHex, *vec.PayloadDigest)
			}
		})
	}
}

// TestGoldenPublishPaths replays every oracle capture through Publish and
// demands the same journal-relative path and episode bytes the oracle
// wrote. The path is an index and evidence-reference key, so a layout
// drift here would silently fork the corpus.
func TestGoldenPublishPaths(t *testing.T) {
	for name, vec := range loadVectors(t) {
		if vec.Outcome != string(CapturePublished) {
			continue
		}
		t.Run(name, func(t *testing.T) {
			golden, err := os.ReadFile(filepath.Join(goldenDir, "episodes", name+".md"))
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}
			ep := ParseEpisode(string(golden))
			if ep == nil {
				t.Fatal("ParseEpisode rejected oracle episode")
			}
			b, err := os.ReadFile(filepath.Join(payloadsDir, name+".json"))
			if err != nil {
				t.Fatalf("read payload: %v", err)
			}
			raw, err := ParsePayload(b)
			if err != nil {
				t.Fatalf("ParsePayload: %v", err)
			}
			p, err := validateAsCaptureHost(t, raw)
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			root, err := OpenJournalRoot(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			pub, err := Publish(root, &p, ep.CaptureTimeMs)
			if err != nil {
				t.Fatalf("Publish: %v", err)
			}
			if pub.Outcome != CapturePublished {
				t.Errorf("outcome = %q", pub.Outcome)
			}
			if pub.RelPath != *vec.Path {
				t.Errorf("rel path = %q, oracle %q", pub.RelPath, *vec.Path)
			}
			if !bytes.Equal(pub.Content, golden) {
				t.Error("published bytes differ from oracle episode")
			}
			info, err := root.Lstat(pub.RelPath)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Errorf("episode mode = %o", info.Mode().Perm())
			}
			// Redelivery against the oracle-shaped corpus dedupes.
			again, err := Publish(root, &p, ep.CaptureTimeMs+1)
			if err != nil {
				t.Fatalf("redeliver: %v", err)
			}
			if again.Outcome != CaptureDuplicate || again.RelPath != pub.RelPath {
				t.Errorf("redelivery = %q at %q", again.Outcome, again.RelPath)
			}
		})
	}
}

// configVector describes one oracle-pinned config rewrite: the before
// bytes (absent for a creation), the default-command arguments, and the
// expected outcome. after bytes live next to the before file.
type configVector struct {
	World   string `json:"world"`
	Scope   string `json:"scope"`
	Before  bool   `json:"before"`
	Outcome string `json:"outcome"`
}

// TestGoldenConfigVectors replays SaveCaptureDefaults against the byte
// fixtures the archived Zig binary produced for the same inputs
// (testdata/golden/config/). The rewritten config file is a frozen
// contract: key order, number normalization, escaping, and indentation
// must be indistinguishable from the oracle's.
func TestGoldenConfigVectors(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(goldenDir, "config-vectors.json"))
	if err != nil {
		t.Fatalf("read config vectors: %v", err)
	}
	var vectors map[string]configVector
	if err := json.Unmarshal(b, &vectors); err != nil {
		t.Fatalf("parse config vectors: %v", err)
	}
	for name, vec := range vectors {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			var before []byte
			if vec.Before {
				var err error
				before, err = os.ReadFile(filepath.Join(goldenDir, "config", name+".before.json"))
				if err != nil {
					t.Fatalf("read before: %v", err)
				}
				if err := os.WriteFile(path, before, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			env := mapEnviron()
			_, err := SaveCaptureDefaults(env, path, vec.World, vec.Scope)
			switch vec.Outcome {
			case "ok":
				if err != nil {
					t.Fatalf("SaveCaptureDefaults: %v", err)
				}
				want, err := os.ReadFile(filepath.Join(goldenDir, "config", name+".after.json"))
				if err != nil {
					t.Fatalf("read after: %v", err)
				}
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("rewritten config differs from oracle\n--- oracle ---\n%s\n--- got ---\n%s", want, got)
				}
			case "malformed":
				if !errors.Is(err, ErrConfigMalformed) {
					t.Fatalf("err = %v, want ErrConfigMalformed", err)
				}
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, before) {
					t.Error("malformed config was modified")
				}
			default:
				t.Fatalf("unhandled vector outcome %q", vec.Outcome)
			}
		})
	}
}
