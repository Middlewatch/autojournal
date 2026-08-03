package autojournal

import (
	"bytes"
	"encoding/json"
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
