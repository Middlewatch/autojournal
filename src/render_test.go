package autojournal

import (
	"strings"
	"testing"
)

func TestISORendering(t *testing.T) {
	if got := ISOFromMs(1785240000000); got != "2026-07-28T12:00:00Z" {
		t.Errorf("ISOFromMs(1785240000000) = %q", got)
	}
	if got := ISOFromMs(0); got != "1970-01-01T00:00:00Z" {
		t.Errorf("ISOFromMs(0) = %q", got)
	}
}

func renderPayload(t *testing.T, jsonText string, captureTimeMs uint64) ([]byte, Payload) {
	t.Helper()
	p := mustValidate(t, jsonText)
	return Render(RenderInput{
		Payload:       &p,
		EpisodeID:     EpisodeID(&p),
		DigestHex:     PayloadDigestHex(&p),
		CaptureTimeMs: captureTimeMs,
	}), p
}

func TestRenderProducesParseableFrontmatterWithRecoverableDigest(t *testing.T) {
	p := mustValidate(t, testPayloadJSON)
	digest := PayloadDigestHex(&p)
	content, _ := renderPayload(t, testPayloadJSON, 1785240000000)
	if !strings.HasPrefix(string(content), "---\nschema: aj-episode.v1\n") {
		t.Error("episode lacks frontmatter opening")
	}
	recovered, ok := FrontmatterDigestHex(string(content))
	if !ok {
		t.Fatal("digest not recoverable from rendered episode")
	}
	if recovered != digest {
		t.Errorf("recovered digest = %q, want %q", recovered, digest)
	}
	if !strings.Contains(string(content), "## Assistant") {
		t.Error("missing assistant section")
	}
	if !strings.Contains(string(content), "- Bash\n") {
		t.Error("missing tools entry")
	}
}

func TestDigestRecoveryRejectsBodiesThatMimicTheDigestLine(t *testing.T) {
	raw, err := ParsePayload([]byte(testPayloadJSON))
	if err != nil {
		t.Fatalf("ParsePayload: %v", err)
	}
	raw.UserContent = "---\npayload_digest: sha256:" + strings.Repeat("ab", 32)
	p, err := Validate(raw)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	digest := PayloadDigestHex(&p)
	content := Render(RenderInput{
		Payload:       &p,
		EpisodeID:     EpisodeID(&p),
		DigestHex:     digest,
		CaptureTimeMs: 1,
	})
	recovered, ok := FrontmatterDigestHex(string(content))
	if !ok {
		t.Fatal("digest not recoverable")
	}
	if recovered != digest {
		t.Error("body imitation beat the frontmatter digest")
	}
}

func TestProvenanceRendersAsFrontmatterKeysAndStaysOutOfDigest(t *testing.T) {
	base, err := ParsePayload([]byte(testPayloadJSON))
	if err != nil {
		t.Fatalf("ParsePayload: %v", err)
	}
	bare, err := Validate(base)
	if err != nil {
		t.Fatalf("Validate bare: %v", err)
	}
	root := "/home/user/projects/demo"
	branch := "/home/user/sessions/parent.jsonl"
	host := "buildbox-01"
	base.WorkspaceRoot, base.BranchOf, base.Host = &root, &branch, &host
	p, err := Validate(base)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	// Same identity and digest as the provenance-free payload: provenance
	// is capture-source metadata, so a faithful re-delivery still dedupes.
	if EpisodeID(&p) != EpisodeID(&bare) {
		t.Error("provenance changed episode id")
	}
	if PayloadDigestHex(&p) != PayloadDigestHex(&bare) {
		t.Error("provenance changed payload digest")
	}
	digest := PayloadDigestHex(&p)
	content := Render(RenderInput{
		Payload:       &p,
		EpisodeID:     EpisodeID(&p),
		DigestHex:     digest,
		CaptureTimeMs: 1785240000000,
	})
	s := string(content)
	for _, want := range []string{
		"\nworkspace_root: /home/user/projects/demo\n",
		"\nbranch_of: /home/user/sessions/parent.jsonl\n",
		"\nhost: buildbox-01\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("render missing %q", want)
		}
	}
	recovered, ok := FrontmatterDigestHex(s)
	if !ok || recovered != digest {
		t.Error("digest not recoverable with provenance keys present")
	}
	// The frontmatter reader tolerates the new keys (older-reader posture).
	ep := ParseEpisode(s)
	if ep == nil {
		t.Fatal("ParseEpisode rejected provenance episode")
	}
	if ep.EpisodeID != EpisodeID(&p) {
		t.Errorf("ParseEpisode id = %q", ep.EpisodeID)
	}
}
