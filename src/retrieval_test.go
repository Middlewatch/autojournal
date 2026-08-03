package autojournal

import (
	"fmt"
	"strings"
	"testing"
)

func TestTokenizerParityWithV1LexicalContract(t *testing.T) {
	terms := ExtractTerms("What did the Cinder routing-mode use?")
	want := []string{"cinder", "routing", "mode"}
	if len(terms.Items) != len(want) {
		t.Fatalf("terms = %v", terms.Items)
	}
	for i, w := range want {
		if terms.Items[i] != w {
			t.Errorf("term %d = %q, want %q", i, terms.Items[i], w)
		}
	}
	if terms.Truncated {
		t.Error("unexpected truncation")
	}
}

func TestTokenizerDropsShortTokensStopWordsAndNonASCII(t *testing.T) {
	if terms := ExtractTerms("naïve — ✓ it we is"); len(terms.Items) != 0 {
		t.Errorf("terms = %v, want none", terms.Items)
	}
	// Underscores join tokens (\w parity); duplicates are preserved.
	terms := ExtractTerms("gguf gguf snake_case")
	want := []string{"gguf", "gguf", "snake_case"}
	if len(terms.Items) != len(want) {
		t.Fatalf("terms = %v", terms.Items)
	}
	for i, w := range want {
		if terms.Items[i] != w {
			t.Errorf("term %d = %q, want %q", i, terms.Items[i], w)
		}
	}
}

func TestIndexSideTokenizerLowercasesAndFiltersLikeQuerySide(t *testing.T) {
	got := TokenizeLine("The Cinder ROUTING-mode? naïve Q8 x " + strings.Repeat("a", 200))
	// Two-byte tokens are indexed (alias values like "q8" need them);
	// "naïve" splits at the non-ASCII bytes into fragments; "x" is short;
	// the 200-byte run exceeds the token cap.
	want := []string{"cinder", "routing", "mode", "na", "ve", "q8"}
	if len(got) != len(want) {
		t.Fatalf("tokens = %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("token %d = %q, want %q", i, got[i], w)
		}
	}
}

func TestTermCapTruncatesWithFlag(t *testing.T) {
	var parts []string
	for i := 0; i < MaxQueryTerms+5; i++ {
		parts = append(parts, fmt.Sprintf("term%03d", i))
	}
	terms := ExtractTerms(strings.Join(parts, " "))
	if len(terms.Items) != MaxQueryTerms {
		t.Errorf("len = %d, want %d", len(terms.Items), MaxQueryTerms)
	}
	if !terms.Truncated {
		t.Error("expected truncation flag")
	}
}

func TestRecencyVectorsMatchV1(t *testing.T) {
	var now uint64 = 1785240000000
	if got := RecencyMultiplier(now, now, 1.0); got != 2.0 {
		t.Errorf("same-day = %v, want 2.0", got)
	}
	if got := RecencyMultiplier(now-msPerDay, now, 1.0); got != 1.5 {
		t.Errorf("one day = %v, want 1.5", got)
	}
	if got := RecencyMultiplier(now+msPerDay, now, 1.0); got != 1.0 {
		t.Errorf("future = %v, want 1.0", got)
	}
	// Sub-day age floors to zero days.
	if got := RecencyMultiplier(now-(msPerDay-1), now, 1.0); got != 2.0 {
		t.Errorf("sub-day = %v, want 2.0", got)
	}
}

func TestIDFWeightAbsentAndUbiquitousTermsContributeNothing(t *testing.T) {
	if got := IDFWeight(87, 0); got != 0.0 {
		t.Errorf("df 0 = %v", got)
	}
	if got := IDFWeight(87, 87); got != 0.0 {
		t.Errorf("df N = %v", got)
	}
	if got := IDFWeight(87, 1); got <= 4.4 {
		t.Errorf("df 1 = %v, want > 4.4", got)
	}
}

// rankFixture builds two episodes; episode 0 is older. Term 0 is rare
// (idf 2.0), term 1 common (idf 0.1).
func rankFixture() Ranked {
	episodes := []EpisodeInfo{
		{EpisodeID: "aj1-old", RelPath: "worlds/w/2026/07/01/aj1-old.md", EventTimeMs: 0},
		{EpisodeID: "aj1-new", RelPath: "worlds/w/2026/07/28/aj1-new.md", EventTimeMs: 1785240000000},
	}
	candidates := []Candidate{
		{EpisodeOrd: 0, LineNo: 20, MatchedMask: 0b01}, // rare, old
		{EpisodeOrd: 1, LineNo: 20, MatchedMask: 0b10}, // common, new
		{EpisodeOrd: 1, LineNo: 22, MatchedMask: 0b11}, // both, new, same bucket as above
		{EpisodeOrd: 1, LineNo: 40, MatchedMask: 0b10}, // common, new, distinct bucket
	}
	idf := []float64{2.0, 0.1}
	return Rank(candidates, episodes, idf, RankParams{
		NowMs:         1785240000000,
		RecencyBoost:  1.0,
		ContextWindow: 3,
	})
}

func TestRankOrdersByRarityTimesRecencyAndDedupsSpans(t *testing.T) {
	ranked := rankFixture()
	// Candidate 2 scores (2.0+0.1)*2.0 = 4.2; candidate 0 scores 2.0*1.0;
	// candidate 1 (0.2) is deduped away by candidate 2 (same 6-line bucket);
	// candidate 3 (0.2) survives in its own bucket.
	if len(ranked.Order) != 3 {
		t.Fatalf("order = %v", ranked.Order)
	}
	if ranked.Order[0] != 2 || ranked.Order[1] != 0 || ranked.Order[2] != 3 {
		t.Errorf("order = %v, want [2 0 3]", ranked.Order)
	}
	if ranked.Scores[2] != 4.2 {
		t.Errorf("score[2] = %v, want 4.2", ranked.Scores[2])
	}
}

func TestRankTiesBreakOnPathThenLineDeterministically(t *testing.T) {
	episodes := []EpisodeInfo{
		{EpisodeID: "aj1-b", RelPath: "worlds/w/2026/07/02/aj1-b.md", EventTimeMs: 5},
		{EpisodeID: "aj1-a", RelPath: "worlds/w/2026/07/01/aj1-a.md", EventTimeMs: 5},
	}
	// Arrival order is b-then-a; ranking must not preserve it.
	candidates := []Candidate{
		{EpisodeOrd: 0, LineNo: 30, MatchedMask: 0b1},
		{EpisodeOrd: 1, LineNo: 30, MatchedMask: 0b1},
		{EpisodeOrd: 1, LineNo: 90, MatchedMask: 0b1},
	}
	ranked := Rank(candidates, episodes, []float64{1.0}, RankParams{NowMs: 5})
	if len(ranked.Order) != 3 {
		t.Fatalf("order = %v", ranked.Order)
	}
	if ranked.Order[0] != 1 || ranked.Order[1] != 2 || ranked.Order[2] != 0 {
		t.Errorf("order = %v, want [1 2 0]", ranked.Order)
	}
}

func TestMinScoreFloorDropsWeakResultsFromOrderButKeepsScores(t *testing.T) {
	episodes := []EpisodeInfo{
		{EpisodeID: "aj1-x", RelPath: "worlds/w/e/aj1-x.md", EventTimeMs: 5},
	}
	candidates := []Candidate{
		{EpisodeOrd: 0, LineNo: 20, MatchedMask: 0b1},
		{EpisodeOrd: 0, LineNo: 90, MatchedMask: 0b10},
	}
	// Second candidate scores 0.4 * 2.0 = 0.8 < 1.0 and is floored out; a
	// score exactly at the floor would survive (legacy >= semantics).
	ranked := Rank(candidates, episodes, []float64{3.0, 0.4}, RankParams{NowMs: 5, MinScore: 1.0})
	if len(ranked.Order) != 1 || ranked.Order[0] != 0 {
		t.Errorf("order = %v, want [0]", ranked.Order)
	}
}

func TestConfidenceBandsOffTheFloor(t *testing.T) {
	cases := []struct {
		score, floor float64
		want         Confidence
	}{
		{2.9, 3.0, ConfidenceLow},
		{3.0, 3.0, ConfidenceMedium},
		{6.0, 3.0, ConfidenceHigh},
		{0.0, 0.0, ConfidenceHigh},
	}
	for _, c := range cases {
		if got := ConfidenceOf(c.score, c.floor); got != c.want {
			t.Errorf("ConfidenceOf(%v, %v) = %q, want %q", c.score, c.floor, got, c.want)
		}
	}
}

func TestCursorRoundTripAndGuardMismatch(t *testing.T) {
	inputs := CursorInputs{
		Query:       "cinder routing",
		World:       "willow",
		Scope:       "",
		Lanes:       "conversation,delegated_work,imported_legacy",
		AliasDigest: "abc123",
	}
	cursor := CursorEncode(20, inputs)
	if !strings.HasPrefix(cursor, "aj1.20.") {
		t.Errorf("cursor = %q", cursor)
	}
	offset, err := CursorDecode(cursor, inputs)
	if err != nil || offset != 20 {
		t.Errorf("decode = %d, %v", offset, err)
	}

	other := inputs
	other.Query = "different query"
	if _, err := CursorDecode(cursor, other); err == nil {
		t.Error("guard mismatch decoded")
	}
	if _, err := CursorDecode("aj1.x.deadbeef", inputs); err == nil {
		t.Error("bad offset decoded")
	}
	if _, err := CursorDecode("nonsense", inputs); err == nil {
		t.Error("garbage decoded")
	}
}
