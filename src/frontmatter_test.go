package autojournal

import (
	"strings"
	"testing"
)

func TestFrontmatterRoundTrip(t *testing.T) {
	p := mustValidate(t, testPayloadJSON)
	id, digest := EpisodeID(&p), PayloadDigestHex(&p)
	content := Render(RenderInput{
		Payload:       &p,
		EpisodeID:     id,
		DigestHex:     digest,
		CaptureTimeMs: 1785240000000,
	})
	ep := ParseEpisode(string(content))
	if ep == nil {
		t.Fatal("ParseEpisode rejected a rendered episode")
	}
	if ep.EpisodeID != id {
		t.Errorf("episode id = %q, want %q", ep.EpisodeID, id)
	}
	if ep.World != p.World {
		t.Errorf("world = %q, want %q", ep.World, p.World)
	}
	if ep.Lane != p.Lane {
		t.Errorf("lane = %q, want %q", ep.Lane, p.Lane)
	}
	if ep.EventTimeMs != p.EventTimeMs {
		t.Errorf("event_time_ms = %d, want %d", ep.EventTimeMs, p.EventTimeMs)
	}
	if ep.DigestHex != digest {
		t.Errorf("digest = %q, want %q", ep.DigestHex, digest)
	}
	// Rendered frontmatter is `---` + 16 keys + `---`, so the body starts
	// on line 19, at the blank line before `## User`.
	if ep.BodyLine != 19 {
		t.Errorf("body_line = %d, want 19", ep.BodyLine)
	}
	if !strings.HasPrefix(string(content)[ep.BodyOffset:], "\n## User\n") {
		t.Errorf("body_offset lands on %q", string(content)[ep.BodyOffset:ep.BodyOffset+12])
	}
}

func TestBodyLineDoesNotShiftWhenBodyMimicsFrontmatter(t *testing.T) {
	raw, err := ParsePayload([]byte(testPayloadJSON))
	if err != nil {
		t.Fatalf("ParsePayload: %v", err)
	}
	raw.UserContent = "---\nworld: fake\n---"
	p, err := Validate(raw)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	content := Render(RenderInput{
		Payload:       &p,
		EpisodeID:     EpisodeID(&p),
		DigestHex:     PayloadDigestHex(&p),
		CaptureTimeMs: 1785240000000,
	})
	ep := ParseEpisode(string(content))
	if ep == nil {
		t.Fatal("ParseEpisode rejected spoofed body")
	}
	if ep.BodyLine != 19 {
		t.Errorf("body_line = %d, want 19", ep.BodyLine)
	}
}

func TestCorruptFrontmatterYieldsNilNotCrash(t *testing.T) {
	cases := []string{
		"not an episode",
		"---\nschema: aj-episode.v1\n",
		"---\nschema: wrong.v9\n---\n",
		"---\nschema: aj-episode.v1\nepisode_id: x\n---\n",
		"---\nschema: aj-episode.v1\nno-separator-line\n---\n",
	}
	for _, c := range cases {
		if ep := ParseEpisode(c); ep != nil {
			t.Errorf("ParseEpisode(%q) = %+v, want nil", c, ep)
		}
	}
}
