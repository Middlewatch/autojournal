// End-to-end retrieval tests: corpus on disk, SQLite projection, search
// orchestration, evidence opening. Frozen clock throughout.

package autojournal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const searchTestNowMs uint64 = 1785240000000

type searchFixture struct {
	rootPath  string
	root      *os.Root
	idx       *Index
	published [4]*Published
	emptyMap  *AliasMap
}

func (fx *searchFixture) request(query string) SearchRequest {
	return SearchRequest{Query: query, World: "testworld", NowMs: searchTestNowMs}
}

// setupSearchCorpus publishes four episodes: two conversation, one
// delegated, one evaluation, with distinct multi-line bodies so ranking,
// context windows, and lane filters are all observable.
func setupSearchCorpus(t *testing.T) *searchFixture {
	t.Helper()
	rootPath, root := testCorpus(t)
	base := mustValidate(t, testPayloadJSON)

	p1 := base
	p1.TurnID = "turn-2001"
	p1.UserContent = "context before\nthe quokka enclosure needed reindexing today\ncontext after"
	p1.AssistantResult = "Noted the fwupd firmware refresh too."
	a := mustPublish(t, root, p1)

	p2 := base
	p2.TurnID = "turn-2002"
	p2.UserContent = "a quokka appeared in the pi-web-access logs"
	p2.AssistantResult = "Filed it."
	b := mustPublish(t, root, p2)

	p3 := base
	p3.TurnID = "turn-2003"
	p3.Lane = LaneDelegatedWork
	p3.UserContent = "delegated wombat census"
	p3.AssistantResult = "Census complete."
	c := mustPublish(t, root, p3)

	p4 := base
	p4.TurnID = "turn-2004"
	p4.Lane = LaneEvaluation
	p4.UserContent = "sealed evaluation phrase quokka"
	p4.AssistantResult = "Sealed."
	d := mustPublish(t, root, p4)

	idx := openMemoryIndex(t)
	if _, err := idx.SyncFromCorpus(root); err != nil {
		t.Fatalf("SyncFromCorpus: %v", err)
	}

	return &searchFixture{
		rootPath:  rootPath,
		root:      root,
		idx:       idx,
		published: [4]*Published{a, b, c, d},
		emptyMap:  LoadAliasMapFromBytes([]byte("{}")),
	}
}

func TestSearchRanksMultiTermHitsFirstWithProvenanceAndSnippets(t *testing.T) {
	fx := setupSearchCorpus(t)

	out := Search(fx.root, fx.idx, fx.emptyMap, fx.request("quokka enclosure"))
	if out.Outcome != OutcomeMatch {
		t.Fatalf("outcome = %q (%s)", out.Outcome, out.Detail)
	}
	if out.Total != 2 || len(out.Hits) != 2 {
		t.Fatalf("total = %d, hits = %d", out.Total, len(out.Hits))
	}

	top := out.Hits[0]
	if top.EpisodeID != fx.published[0].EpisodeID {
		t.Errorf("top episode = %q, want %q", top.EpisodeID, fx.published[0].EpisodeID)
	}
	if !strings.HasPrefix(top.Revision, "sha256:") {
		t.Errorf("revision = %q", top.Revision)
	}
	if !strings.HasPrefix(top.Path, "worlds/testworld/") {
		t.Errorf("path = %q", top.Path)
	}
	if top.Lane != LaneConversation {
		t.Errorf("lane = %q", top.Lane)
	}
	if top.Scope != "workspace:demo" {
		t.Errorf("scope = %q", top.Scope)
	}
	if top.Score <= out.Hits[1].Score {
		t.Errorf("top score %v not above second %v", top.Score, out.Hits[1].Score)
	}
	if len(top.MatchedTerms) != 2 {
		t.Errorf("matched terms = %v", top.MatchedTerms)
	}
	// The snippet carries the matched line plus context, never frontmatter.
	if !strings.Contains(top.Snippet, "quokka enclosure") {
		t.Errorf("snippet missing matched line: %q", top.Snippet)
	}
	if !strings.Contains(top.Snippet, "context before") {
		t.Errorf("snippet missing context: %q", top.Snippet)
	}
	if strings.Contains(top.Snippet, "payload_digest") {
		t.Errorf("snippet leaks frontmatter: %q", top.Snippet)
	}
	if top.SnippetStart < 19 { // body starts after frontmatter
		t.Errorf("snippet start = %d", top.SnippetStart)
	}
	if top.Line < top.SnippetStart || top.Line > top.SnippetEnd {
		t.Errorf("line %d outside snippet [%d,%d]", top.Line, top.SnippetStart, top.SnippetEnd)
	}
	// Line numbering stays aligned through blank lines: walking the snippet
	// to the hit's line index lands on the matched text.
	{
		lineNo := top.SnippetStart
		matchLine := ""
		found := false
		for _, line := range strings.Split(top.Snippet, "\n") {
			if lineNo == top.Line {
				matchLine = line
				found = true
				break
			}
			lineNo++
		}
		if !found {
			t.Fatalf("hit line %d not reachable in snippet starting at %d", top.Line, top.SnippetStart)
		}
		if !strings.Contains(matchLine, "quokka enclosure") {
			t.Errorf("snippet line %d = %q", top.Line, matchLine)
		}
	}
	if out.Freshness != IndexFresh {
		t.Errorf("freshness = %q", out.Freshness)
	}

	// Determinism: an identical call returns identical results.
	again := Search(fx.root, fx.idx, fx.emptyMap, fx.request("quokka enclosure"))
	if again.Hits[0].EpisodeID != top.EpisodeID {
		t.Errorf("rerun episode = %q", again.Hits[0].EpisodeID)
	}
	if again.Hits[0].Line != top.Line {
		t.Errorf("rerun line = %d", again.Hits[0].Line)
	}
	if again.Hits[0].Score != top.Score {
		t.Errorf("rerun score = %v, want %v", again.Hits[0].Score, top.Score)
	}
}

func TestInfixRecallIsSubstringModeOnlyWordStartDefaultDropsIt(t *testing.T) {
	fx := setupSearchCorpus(t)

	// "index" appears only inside "reindexing". v1-parity substring
	// crediting still surfaces it.
	parityReq := fx.request("index")
	parityReq.CreditMode = CreditSubstring
	parity := Search(fx.root, fx.idx, fx.emptyMap, parityReq)
	if parity.Outcome != OutcomeMatch {
		t.Fatalf("parity outcome = %q", parity.Outcome)
	}
	if parity.Hits[0].EpisodeID != fx.published[0].EpisodeID {
		t.Errorf("parity episode = %q", parity.Hits[0].EpisodeID)
	}
	if parity.Hits[0].MatchedTerms[0] != "index" {
		t.Errorf("parity matched = %v", parity.Hits[0].MatchedTerms)
	}

	// The word_start default refuses the infix credit ("index" mid-word),
	// trading it away to stop "hang"-in-"changed" false credits; curated
	// aliases ("index" -> "reindex") recover wanted infix families.
	strict := Search(fx.root, fx.idx, fx.emptyMap, fx.request("index"))
	if strict.Outcome != OutcomeNoMatch {
		t.Errorf("strict outcome = %q", strict.Outcome)
	}
}

func TestEvaluationLaneInvisibleUntilExplicitlyRequested(t *testing.T) {
	fx := setupSearchCorpus(t)

	hidden := Search(fx.root, fx.idx, fx.emptyMap, fx.request("sealed"))
	if hidden.Outcome != OutcomeNoMatch {
		t.Fatalf("hidden outcome = %q", hidden.Outcome)
	}

	req := fx.request("sealed")
	req.Lanes = []Lane{LaneEvaluation}
	shown := Search(fx.root, fx.idx, fx.emptyMap, req)
	if shown.Outcome != OutcomeMatch {
		t.Fatalf("shown outcome = %q", shown.Outcome)
	}
	if shown.Hits[0].Lane != LaneEvaluation {
		t.Errorf("lane = %q", shown.Hits[0].Lane)
	}
}

func TestAliasesRescueVocabularyMismatchQueriesIncludingPhraseValues(t *testing.T) {
	fx := setupSearchCorpus(t)

	// Without the alias the casual word misses.
	miss := Search(fx.root, fx.idx, fx.emptyMap, fx.request("portal"))
	if miss.Outcome != OutcomeNoMatch {
		t.Fatalf("miss outcome = %q", miss.Outcome)
	}

	m := LoadAliasMapFromBytes([]byte(`{"portal": ["pi-web-access"], "refresh": ["fwupd"]}`))
	hit := Search(fx.root, fx.idx, m, fx.request("portal"))
	if hit.Outcome != OutcomeMatch {
		t.Fatalf("hit outcome = %q (%s)", hit.Outcome, hit.Detail)
	}
	if hit.Hits[0].EpisodeID != fx.published[1].EpisodeID {
		t.Errorf("episode = %q, want %q", hit.Hits[0].EpisodeID, fx.published[1].EpisodeID)
	}
	if len(hit.AliasTerms) != 1 {
		t.Errorf("alias terms = %v", hit.AliasTerms)
	}
	if hit.Hits[0].MatchedTerms[0] != "pi-web-access" {
		t.Errorf("matched = %v", hit.Hits[0].MatchedTerms)
	}
	// Alias identity differs from the empty map and stamps the output.
	if hit.AliasDigest == miss.AliasDigest {
		t.Errorf("alias digest %q did not change with the map", hit.AliasDigest)
	}
}

func TestNoiseQueriesProduceTypedNoMatchNotWeakResults(t *testing.T) {
	fx := setupSearchCorpus(t)

	noise := Search(fx.root, fx.idx, fx.emptyMap, fx.request("xyzzyplugh frobnicate"))
	if noise.Outcome != OutcomeNoMatch {
		t.Errorf("noise outcome = %q", noise.Outcome)
	}
	if noise.Total != 0 {
		t.Errorf("noise total = %d", noise.Total)
	}

	stops := Search(fx.root, fx.idx, fx.emptyMap, fx.request("the of and it"))
	if stops.Outcome != OutcomeNoMatch {
		t.Errorf("stops outcome = %q", stops.Outcome)
	}
}

func TestPaginationPagesDeterministicallyWithGuardedCursor(t *testing.T) {
	fx := setupSearchCorpus(t)

	req := fx.request("quokka")
	req.Limit = 1
	page1 := Search(fx.root, fx.idx, fx.emptyMap, req)
	if page1.Total != 2 {
		t.Fatalf("total = %d", page1.Total)
	}
	if len(page1.Hits) != 1 {
		t.Fatalf("page1 hits = %d", len(page1.Hits))
	}
	cursor := page1.NextCursor
	if cursor == "" {
		t.Fatal("page1 has no next cursor")
	}

	req2 := req
	req2.Cursor = &cursor
	page2 := Search(fx.root, fx.idx, fx.emptyMap, req2)
	if len(page2.Hits) != 1 {
		t.Fatalf("page2 hits = %d", len(page2.Hits))
	}
	if page2.NextCursor != "" {
		t.Errorf("page2 cursor = %q, want none", page2.NextCursor)
	}
	if page1.Hits[0].EpisodeID == page2.Hits[0].EpisodeID {
		t.Errorf("pages repeat episode %q", page1.Hits[0].EpisodeID)
	}

	// A cursor replayed against a different query is malformed.
	req3 := fx.request("enclosure")
	req3.Cursor = &cursor
	replay := Search(fx.root, fx.idx, fx.emptyMap, req3)
	if replay.Outcome != OutcomeMalformed {
		t.Errorf("replay outcome = %q", replay.Outcome)
	}
}

func TestEmptyProjectionOverNonemptyCorpusIsIndexStaleNotNoMatch(t *testing.T) {
	fx := setupSearchCorpus(t)

	freshIdx := openMemoryIndex(t)
	out := Search(fx.root, freshIdx, fx.emptyMap, fx.request("quokka"))
	if out.Outcome != OutcomeIndexStale {
		t.Errorf("outcome = %q", out.Outcome)
	}
	if out.Freshness != IndexStale {
		t.Errorf("freshness = %q", out.Freshness)
	}
}

func TestEpisodeEditedAfterIndexingIsExcludedFromEvidence(t *testing.T) {
	fx := setupSearchCorpus(t)

	// Replace episode 2's file with a re-rendered revision (new body, new
	// digest) without re-syncing: the projection still holds the old
	// revision, so the candidate must be dropped, not served.
	p2 := mustValidate(t, testPayloadJSON)
	p2.TurnID = "turn-2002"
	p2.UserContent = "the logs were rotated away"
	p2.AssistantResult = "Filed it."
	id := EpisodeID(&p2)
	digest := PayloadDigestHex(&p2)
	content := Render(RenderInput{
		Payload:       &p2,
		EpisodeID:     id,
		DigestHex:     digest,
		CaptureTimeMs: searchTestNowMs,
	})
	writeCorpusFile(t, fx.rootPath, fx.published[1].RelPath, string(content))

	out := Search(fx.root, fx.idx, fx.emptyMap, fx.request("quokka"))
	if out.Outcome != OutcomeMatch {
		t.Fatalf("outcome = %q", out.Outcome)
	}
	if out.Total != 1 {
		t.Errorf("total = %d", out.Total)
	}
	if out.EditedExcluded != 1 {
		t.Errorf("edited_excluded = %d", out.EditedExcluded)
	}
	if out.Hits[0].EpisodeID != fx.published[0].EpisodeID {
		t.Errorf("episode = %q", out.Hits[0].EpisodeID)
	}
}

func TestMemoryGetOpensExactBoundedEvidenceAndTracksRevisions(t *testing.T) {
	fx := setupSearchCorpus(t)

	found := Search(fx.root, fx.idx, fx.emptyMap, fx.request("wombat"))
	if found.Outcome != OutcomeMatch {
		t.Fatalf("search outcome = %q", found.Outcome)
	}
	hit := found.Hits[0]

	// Happy path: hint + matching revision.
	got := Get(fx.root, fx.idx, GetRequest{
		EpisodeID: hit.EpisodeID,
		Revision:  hit.Revision,
		PathHint:  &hit.Path,
	})
	if got.Outcome != OutcomeMatch {
		t.Fatalf("get outcome = %q (%s)", got.Outcome, got.Detail)
	}
	if !strings.Contains(got.Content, "delegated wombat census") {
		t.Errorf("content missing body: %q", got.Content)
	}
	if strings.Contains(got.Content, "payload_digest") {
		t.Errorf("content leaks frontmatter: %q", got.Content)
	}
	if got.LineStart < 19 {
		t.Errorf("line_start = %d", got.LineStart)
	}
	if got.Trust != "untrusted_evidence" {
		t.Errorf("trust = %q", got.Trust)
	}
	if !got.Resolved || got.Lane != LaneDelegatedWork {
		t.Errorf("lane = %q (resolved %v)", got.Lane, got.Resolved)
	}

	// Without the hint, the index resolves the path.
	noHint := Get(fx.root, fx.idx, GetRequest{
		EpisodeID: hit.EpisodeID,
		Revision:  hit.Revision,
	})
	if noHint.Outcome != OutcomeMatch {
		t.Errorf("no-hint outcome = %q", noHint.Outcome)
	}

	// A revision that no longer matches the file is stale, with the
	// current revision reported for re-request.
	stale := Get(fx.root, fx.idx, GetRequest{
		EpisodeID: hit.EpisodeID,
		Revision:  "sha256:" + strings.Repeat("ab", 32),
		PathHint:  &hit.Path,
	})
	if stale.Outcome != OutcomeStaleRevision {
		t.Errorf("stale outcome = %q", stale.Outcome)
	}
	if stale.Revision != hit.Revision {
		t.Errorf("stale revision = %q, want %q", stale.Revision, hit.Revision)
	}

	// Malformed references are typed, not crashes.
	badID := Get(fx.root, fx.idx, GetRequest{
		EpisodeID: "not-an-id",
		Revision:  hit.Revision,
	})
	if badID.Outcome != OutcomeMalformed {
		t.Errorf("bad id outcome = %q", badID.Outcome)
	}
	badRev := Get(fx.root, fx.idx, GetRequest{
		EpisodeID: hit.EpisodeID,
		Revision:  "sha256:short",
	})
	if badRev.Outcome != OutcomeMalformed {
		t.Errorf("bad revision outcome = %q", badRev.Outcome)
	}

	// Deleted evidence is gone.
	if err := os.Remove(filepath.Join(fx.rootPath, filepath.FromSlash(hit.Path))); err != nil {
		t.Fatal(err)
	}
	gone := Get(fx.root, fx.idx, GetRequest{
		EpisodeID: hit.EpisodeID,
		Revision:  hit.Revision,
		PathHint:  &hit.Path,
	})
	if gone.Outcome != OutcomeGone {
		t.Errorf("gone outcome = %q", gone.Outcome)
	}

	// Escaping path hints are rejected by containment, not resolved.
	if ContainedPath("../outside.md") {
		t.Error("ContainedPath accepted ../outside.md")
	}
	if ContainedPath("/etc/passwd") {
		t.Error("ContainedPath accepted /etc/passwd")
	}
	if !ContainedPath("worlds/w/2026/07/28/aj1-x.md") {
		t.Error("ContainedPath rejected a valid journal path")
	}
}

func TestLineBoundsClampToBodyAndHonorExplicitSpans(t *testing.T) {
	fx := setupSearchCorpus(t)

	found := Search(fx.root, fx.idx, fx.emptyMap, fx.request("enclosure"))
	if found.Outcome != OutcomeMatch {
		t.Fatalf("search outcome = %q", found.Outcome)
	}
	hit := found.Hits[0]

	// Requesting frontmatter lines clamps to the body start.
	clamped := Get(fx.root, fx.idx, GetRequest{
		EpisodeID: hit.EpisodeID,
		Revision:  hit.Revision,
		PathHint:  &hit.Path,
		LineStart: 1,
		LineEnd:   100,
	})
	if clamped.Outcome != OutcomeMatch {
		t.Fatalf("clamped outcome = %q", clamped.Outcome)
	}
	if clamped.LineStart < 19 {
		t.Errorf("clamped line_start = %d", clamped.LineStart)
	}

	// A single-line span serves exactly that line.
	single := Get(fx.root, fx.idx, GetRequest{
		EpisodeID: hit.EpisodeID,
		Revision:  hit.Revision,
		PathHint:  &hit.Path,
		LineStart: hit.Line,
		LineEnd:   hit.Line,
	})
	if single.LineStart != hit.Line || single.LineEnd != hit.Line {
		t.Errorf("span = [%d,%d], want [%d,%d]", single.LineStart, single.LineEnd, hit.Line, hit.Line)
	}
	if !strings.Contains(single.Content, "quokka enclosure") {
		t.Errorf("single-line content = %q", single.Content)
	}
	if strings.ContainsRune(single.Content, '\n') {
		t.Errorf("single-line content spans lines: %q", single.Content)
	}
}

// Regression: a single-component path descends zero directories, so the
// handle being read through is still the caller's root — it must survive
// the call, whether the entry is a directory (refused) or a regular file
// (served). Before the fix, the deferred close poisoned the root and every
// later read failed.
func TestReadContainedSingleComponentPathLeavesRootOpen(t *testing.T) {
	fx := setupSearchCorpus(t)

	if _, err := ReadContained(fx.root, "worlds"); err == nil {
		t.Error("reading a directory did not fail")
	}
	if _, err := ReadContained(fx.root, fx.published[0].RelPath); err != nil {
		t.Fatalf("root unusable after single-component directory read: %v", err)
	}

	writeCorpusFile(t, fx.rootPath, "loose.md", "stray but readable")
	if content, err := ReadContained(fx.root, "loose.md"); err != nil || content != "stray but readable" {
		t.Errorf("single-component file read = %q, %v", content, err)
	}
	if _, err := ReadContained(fx.root, fx.published[0].RelPath); err != nil {
		t.Fatalf("root unusable after single-component file read: %v", err)
	}
}

func TestCreditLineBoundaryRulesPerMode(t *testing.T) {
	cases := []struct {
		line, term string
		mode       CreditMode
		want       bool
	}{
		// The motivating false positive: "hang" inside "changed".
		{"we changed the config", "hang", CreditSubstring, true},
		{"we changed the config", "hang", CreditWordStart, false},
		{"we changed the config", "hang", CreditWholeWord, false},

		// Prefix inflections survive word_start but not whole_word.
		{"the server was hanging", "hang", CreditWordStart, true},
		{"the server was hanging", "hang", CreditWholeWord, false},
		{"config", "config", CreditWholeWord, true},
		{"configuration drift", "config", CreditWordStart, true},
		{"configuration drift", "config", CreditWholeWord, false},

		// Infix recall exists only under substring parity.
		{"reindexing finished", "index", CreditSubstring, true},
		{"reindexing finished", "index", CreditWordStart, false},

		// Case-insensitive in every mode.
		{"Hang detected", "hang", CreditWholeWord, true},

		// A later occurrence can credit after an earlier bounded one fails.
		{"changed, then a hang", "hang", CreditWholeWord, true},

		// Phrase terms: boundaries apply at the edges of the whole
		// occurrence; interior punctuation and spaces need no special
		// handling.
		{"ran out of memory today", "out of memory", CreditWholeWord, true},
		{"timeout of memory", "out of memory", CreditWordStart, false},
		{"built llama.cpp again", "llama.cpp", CreditWholeWord, true},

		// Term at line edges has an implicit boundary.
		{"hang", "hang", CreditWholeWord, true},
		{"hangs", "hang", CreditWholeWord, false},
		{"hangs", "hang", CreditWordStart, true},

		// Underscore is a token byte: "foo_hang" does not word_start-credit
		// "hang".
		{"foo_hang", "hang", CreditWordStart, false},
		{"hang_foo", "hang", CreditWholeWord, false},
		{"hang_foo", "hang", CreditWordStart, true},
	}
	for _, tc := range cases {
		if got := CreditLine(tc.line, tc.term, tc.mode); got != tc.want {
			t.Errorf("CreditLine(%q, %q, %s) = %v, want %v", tc.line, tc.term, tc.mode, got, tc.want)
		}
	}
}
