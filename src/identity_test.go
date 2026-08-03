package autojournal

import "testing"

func TestEpisodeIDStableAndPrefixed(t *testing.T) {
	p := mustValidate(t, testPayloadJSON)
	a, b := EpisodeID(&p), EpisodeID(&p)
	if a != b {
		t.Fatalf("unstable id: %q vs %q", a, b)
	}
	if len(a) != EpisodeIDLen || a[:len(IDPrefix)] != IDPrefix {
		t.Fatalf("id %q lacks %q prefix or wrong length", a, IDPrefix)
	}
}

func TestIdentityFieldsChangeIDBodyDoesNot(t *testing.T) {
	p := mustValidate(t, testPayloadJSON)
	base := EpisodeID(&p)
	other := p
	other.TurnID = "turn-0008"
	if EpisodeID(&other) == base {
		t.Error("turn_id change did not change episode id")
	}
	other = p
	other.AssistantResult = "different body"
	if EpisodeID(&other) != base {
		t.Error("body change changed episode id")
	}
}

func TestDigestTracksBodyAndIdentityNotCaptureRunMetadata(t *testing.T) {
	p := mustValidate(t, testPayloadJSON)
	base := PayloadDigestHex(&p)
	other := p
	other.AssistantResult = "changed"
	if PayloadDigestHex(&other) == base {
		t.Error("body change did not change digest")
	}
	other = p
	other.AdapterVersion = "9.9.9"
	if PayloadDigestHex(&other) != base {
		t.Error("adapter_version change changed digest")
	}
}

func TestFieldBoundaryShiftsChangeDigest(t *testing.T) {
	p := mustValidate(t, testPayloadJSON)
	a := p
	a.UserContent, a.AssistantResult = "ab", "c"
	b := p
	b.UserContent, b.AssistantResult = "a", "bc"
	if PayloadDigestHex(&a) == PayloadDigestHex(&b) {
		t.Error("field-boundary shift did not change digest")
	}
}
