// Every --json shape and every renderer the CLI emits, in one file: the
// --json surface is the Interface-tier contract, and a contract spread
// through the whole of main.go is not reviewable. main.go keeps flag
// parsing, resolution, dispatch, and the exit-code constants; everything
// that decides what bytes a command prints lives here.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	aj "github.com/Middlewatch/autojournal/src"
)

func outcomeExit(outcome aj.Outcome) int {
	switch outcome {
	// A typed empty result is a successful answer, not an error.
	case aj.OutcomeMatch, aj.OutcomeNoMatch:
		return exitOK
	case aj.OutcomeMalformed:
		return exitMalformed
	case aj.OutcomeConflict:
		return exitConflict
	default:
		return exitFailure
	}
}

type hitJSON struct {
	EpisodeID     string   `json:"episode_id"`
	Revision      string   `json:"revision"`
	Path          string   `json:"path"`
	World         string   `json:"world"`
	Scope         string   `json:"scope"`
	Lane          string   `json:"lane"`
	CapturePolicy string   `json:"capture_policy"`
	EventTime     string   `json:"event_time"`
	Line          uint32   `json:"line"`
	SnippetStart  uint32   `json:"snippet_start"`
	SnippetEnd    uint32   `json:"snippet_end"`
	Snippet       string   `json:"snippet"`
	MatchedTerms  []string `json:"matched_terms"`
	Score         float64  `json:"score"`
	Confidence    string   `json:"confidence"`
}

type searchIdentitiesJSON struct {
	Scorer           string `json:"scorer"`
	Tokenizer        string `json:"tokenizer"`
	ConfidencePolicy string `json:"confidence_policy"`
	AliasDigest      string `json:"alias_digest"`
	IndexSchema      uint32 `json:"index_schema"`
}

type searchIndexJSON struct {
	Freshness      string `json:"freshness"`
	Indexed        uint64 `json:"indexed"`
	Source         uint64 `json:"source"`
	EditedExcluded uint64 `json:"edited_excluded"`
}

type searchReportJSON struct {
	Outcome     string               `json:"outcome"`
	Query       string               `json:"query"`
	QueryTerms  []string             `json:"query_terms"`
	AliasTerms  []string             `json:"alias_terms"`
	FoldedTerms []string             `json:"folded_terms"`
	Results     []hitJSON            `json:"results"`
	Total       uint64               `json:"total"`
	Cursor      *string              `json:"cursor"`
	Identities  searchIdentitiesJSON `json:"identities"`
	Index       searchIndexJSON      `json:"index"`
	Detail      *string              `json:"detail"`
}

func (c *cli) renderSearchJSON(world, query string, out *aj.SearchOutput) error {
	results := make([]hitJSON, len(out.Hits))
	for i, hit := range out.Hits {
		results[i] = hitJSON{
			EpisodeID:     hit.EpisodeID,
			Revision:      hit.Revision,
			Path:          hit.Path,
			World:         world,
			Scope:         hit.Scope,
			Lane:          string(hit.Lane),
			CapturePolicy: hit.CapturePolicy,
			EventTime:     aj.ISOFromMs(hit.EventTimeMs),
			Line:          hit.Line,
			SnippetStart:  hit.SnippetStart,
			SnippetEnd:    hit.SnippetEnd,
			Snippet:       hit.Snippet,
			MatchedTerms:  nonNil(hit.MatchedTerms),
			Score:         hit.Score,
			Confidence:    string(hit.Confidence),
		}
	}
	return c.printJSON(searchReportJSON{
		Outcome:     string(out.Outcome),
		Query:       query,
		QueryTerms:  nonNil(out.QueryTerms),
		AliasTerms:  nonNil(out.AliasTerms),
		FoldedTerms: nonNil(out.FoldedTerms),
		Results:     results,
		Total:       out.Total,
		Cursor:      optString(out.NextCursor),
		Identities: searchIdentitiesJSON{
			Scorer:           aj.ScorerVersion,
			Tokenizer:        aj.TokenizerVersion,
			ConfidencePolicy: aj.ConfidencePolicyVersion,
			AliasDigest:      out.AliasDigest,
			IndexSchema:      aj.IndexSchemaVersion,
		},
		Index: searchIndexJSON{
			Freshness:      string(out.Freshness),
			Indexed:        out.Indexed,
			Source:         out.Source,
			EditedExcluded: out.EditedExcluded,
		},
		Detail: optString(out.Detail),
	})
}

func (c *cli) renderSearchText(query string, out *aj.SearchOutput) {
	var buf strings.Builder
	switch out.Outcome {
	case aj.OutcomeMatch: // fall through to results
	case aj.OutcomeNoMatch:
		fmt.Fprintf(&buf, "no match for \"%s\" (index %s, %d indexed)\n", query, out.Freshness, out.Indexed)
		if out.EditedExcluded > 0 {
			fmt.Fprintf(&buf, "note: %d candidate(s) excluded as edited since indexing; run sync\n", out.EditedExcluded)
		}
		io.WriteString(c.stdout, buf.String())
		return
	default:
		fmt.Fprintf(&buf, "search failed: %s", out.Outcome)
		if out.Detail != "" {
			fmt.Fprintf(&buf, " (%s)", out.Detail)
		}
		buf.WriteByte('\n')
		io.WriteString(c.stdout, buf.String())
		return
	}

	fmt.Fprintf(&buf, "%d of %d result(s) for \"%s\" — index %s\n", len(out.Hits), out.Total, query, out.Freshness)
	if len(out.AliasTerms) > 0 {
		buf.WriteString("aliases applied:")
		for _, t := range out.AliasTerms {
			buf.WriteByte(' ')
			buf.WriteString(t)
		}
		buf.WriteByte('\n')
	}
	for i, hit := range out.Hits {
		fmt.Fprintf(&buf, "%2d. [%.2f %s] %s:%d (%s)\n",
			i+1, hit.Score, hit.Confidence, hit.Path, hit.Line, aj.ISOFromMs(hit.EventTimeMs)[:10])
		fmt.Fprintf(&buf, "    %s\n", matchLine(hit))
		fmt.Fprintf(&buf, "    id %s rev %s\n", hit.EpisodeID, hit.Revision)
	}
	if out.NextCursor != "" {
		fmt.Fprintf(&buf, "more: add --cursor %s\n", out.NextCursor)
	}
	if out.Detail != "" {
		fmt.Fprintf(&buf, "note: %s\n", out.Detail)
	}
	io.WriteString(c.stdout, buf.String())
}

// matchLine extracts the matched line from a hit's snippet (the snippet
// spans context lines; the hit's own line is the evidence).
func matchLine(hit aj.Hit) string {
	if hit.Snippet == "" {
		return "(source changed since indexing)"
	}
	lineNo := hit.SnippetStart
	for _, line := range strings.Split(hit.Snippet, "\n") {
		if lineNo == hit.Line {
			return line
		}
		lineNo++
	}
	return hit.Snippet
}

type getReportJSON struct {
	Outcome       string  `json:"outcome"`
	EpisodeID     string  `json:"episode_id"`
	Revision      *string `json:"revision"`
	Path          *string `json:"path"`
	World         *string `json:"world"`
	Scope         *string `json:"scope"`
	Lane          *string `json:"lane"`
	CapturePolicy *string `json:"capture_policy"`
	LineStart     uint32  `json:"line_start"`
	LineEnd       uint32  `json:"line_end"`
	Content       string  `json:"content"`
	Trust         string  `json:"trust"`
	Detail        *string `json:"detail"`
}

type aliasEntryJSON struct {
	Key    string   `json:"key"`
	Values []string `json:"values"`
}

type captureReportJSON struct {
	Outcome       string  `json:"outcome"`
	EpisodeID     *string `json:"episode_id"`
	PayloadDigest *string `json:"payload_digest"`
	Path          *string `json:"path"`
	Index         string  `json:"index"`
	Detail        *string `json:"detail"`
}

// captureOutcomeExit maps a capture outcome to the process exit code: a
// conflict is the one capture outcome that exits non-zero without being a
// failure report.
func captureOutcomeExit(outcome aj.CaptureOutcome) int {
	if outcome == aj.CaptureConflict {
		return exitConflict
	}
	return exitOK
}

func (c *cli) reportCapture(exit int, report captureReportJSON) int {
	if report.Index == "" {
		report.Index = string(aj.IndexNotBuilt)
	}
	if c.printJSON(report) != nil {
		return exitFailure
	}
	return exit
}

type syncReportJSON struct {
	Indexed          uint64 `json:"indexed"`
	Unchanged        uint64 `json:"unchanged"`
	Removed          uint64 `json:"removed"`
	SkippedMalformed uint64 `json:"skipped_malformed"`
	DuplicateIDs     uint64 `json:"duplicate_ids"`
	DigestMismatch   uint64 `json:"digest_mismatch"`
	Unreadable       uint64 `json:"unreadable"`
}

type resealReportJSON struct {
	Scanned       uint64   `json:"scanned"`
	Resealed      uint64   `json:"resealed"`
	Refused       uint64   `json:"refused"`
	WriteFailures uint64   `json:"write_failures"`
	Paths         []string `json:"paths"`
}

type statusIndexJSON struct {
	Freshness string `json:"freshness"`
	Indexed   uint64 `json:"indexed"`
	Path      string `json:"path"`
}

type statusReportJSON struct {
	JournalRoot    string          `json:"journal_root"`
	RootSource     string          `json:"root_source"`
	RootSourcePath *string         `json:"root_source_path"`
	RootOK         bool            `json:"root_ok"`
	Episodes       uint64          `json:"episodes"`
	Index          statusIndexJSON `json:"index"`
}

type catalogPairJSON struct {
	World string `json:"world"`
	Scope string `json:"scope"`
}

type catalogReportJSON struct {
	Pairs []catalogPairJSON `json:"pairs"`
}

type aliasListReportJSON struct {
	Path        string `json:"path"`
	AliasDigest string `json:"alias_digest"`
	// MergedKeys counts duplicate or case-variant keys collapsed on load:
	// the file still works, and this is where the owner learns it
	// needs tidying.
	MergedKeys int              `json:"merged_keys"`
	Entries    []aliasEntryJSON `json:"entries"`
}

type defaultSetReportJSON struct {
	World  string `json:"world"`
	Scope  string `json:"scope"`
	Config string `json:"config"`
}

type defaultShowReportJSON struct {
	World string `json:"world"`
	Scope string `json:"scope"`
}

// printJSON renders the machine surface: declaration field order, raw UTF-8,
// and no HTML escaping, so the output is stable and readable.
//
// An encode failure narrates to stderr and returns the error, and every call
// site turns that into a non-zero exit — an unserializable value produces zero
// bytes of stdout and a failure exit, never a silent success.
func (c *cli) printJSON(v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(c.stderr, "internal error rendering JSON: %v\n", err)
		return err
	}
	c.stdout.Write(buf.Bytes()) // Encode appends the trailing newline
	return nil
}

// emitJSON is printJSON for callers whose only remaining act is to exit: an
// unserializable value must never produce zero bytes of stdout with a
// success exit, which is the one combination no consumer can detect.
func (c *cli) emitJSON(v any) int {
	if c.printJSON(v) != nil {
		return exitFailure
	}
	return exitOK
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// nonNil keeps empty slices rendering as [] rather than null, so a consumer
// never has to treat two spellings of "nothing" as the same thing.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// renderCapture maps one CaptureResult onto the capture report's wire shape
// and exit code. The shared-directory refusal keeps its long-standing human
// wording here — wording is rendering, and the typed result carries the
// sentinel needed to recognize the case.
func (c *cli) renderCapture(res aj.CaptureResult) int {
	report := captureReportJSON{
		Outcome: string(res.Outcome),
		Index:   string(res.IndexState),
	}
	if res.EpisodeID != "" {
		id := res.EpisodeID
		report.EpisodeID = &id
	}
	if res.DigestHex != "" {
		digest := aj.DigestPrefix + res.DigestHex
		report.PayloadDigest = &digest
	}
	if res.RelPath != "" {
		rel := res.RelPath
		report.Path = &rel
	}
	if res.Err != nil {
		detail := res.Detail
		if errors.Is(res.Err, aj.ErrSharedDirectory) {
			detail = "journal root sits under a shared (group- or world-writable) directory; chmod g-w,o-w the parent or configure journal_root to a private location"
		}
		report.Detail = &detail
	}

	exit := captureOutcomeExit(res.Outcome)
	switch res.Outcome {
	case aj.CaptureMalformed:
		exit = exitMalformed
	case aj.CapturePermissionDenied, aj.CaptureUnavailable, aj.CaptureInternalError:
		exit = exitFailure
	}
	return c.reportCapture(exit, report)
}
