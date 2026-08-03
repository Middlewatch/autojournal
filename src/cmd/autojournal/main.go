// Standalone AutoJournal binary: owner CLI and hook target in one
// executable. This slice ships capture, status, catalog, sync, search,
// get, alias, default, and version; the framed protocol follows.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	aj "github.com/Middlewatch/autojournal/src"
)

const usageText = `usage: autojournal <command> [options]

commands:
  capture   read one completed-turn JSON payload on stdin and publish it
  search    ranked, bounded recall: autojournal search <query words...>
  get       open one evidence reference exactly
  alias     thesaurus upkeep: list | add <term> <canonical...> |
            remove <term> [canonical] | candidates
  default   show or set the owner default world/scope (--world/--scope)
  status    report journal root, corpus, and index health
  catalog   list discovered worlds and scopes
  sync      rebuild/repair the index projection from the Markdown corpus
  version   print version and schema identities

options:
  --config <path>    explicit config file (default: XDG lookup)
  --root <path>      journal root override (bypasses config/default)
  --default-root <p> deprecated host fallback retained for adapters
  --index <path>     index database override (default: XDG state dir)
  --world <id>       world to search (default: config default_world,
                     else the capture world)
  --scope <token>    restrict search to one scope
  --lanes <a,b>      lanes to search (default:
                     conversation,delegated_work,imported_legacy)
  --limit <n>        page size (default from config, cap 100)
  --cursor <c>       continue a previous search page
  --credit-mode <m>  term crediting: substring | word_start | whole_word
                     (default word_start; see docs/SEARCH_TUNING.md)
  --episode <id>     (get) evidence episode id
  --revision <r>     (get) sha256:<hex> revision the evidence had
  --path <rel>       (get) path hint from a search result
  --lines <a-b>      (get) explicit line bounds
  --json             machine-readable output
`

const (
	exitOK        = 0
	exitFailure   = 1
	exitMalformed = 2
	exitConflict  = 3
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

type opts struct {
	config      *string
	root        *string
	defaultRoot *string
	index       *string
	world       *string
	scope       *string
	lanes       *string
	limit       *uint32
	cursor      *string
	episode     *string
	revision    *string
	path        *string
	lines       *string
	creditMode  *string
	json        bool
	positionals []string
}

// cli carries the process boundary so command logic is testable without
// exec: environment lookup, streams, and the wall clock.
type cli struct {
	env    aj.Environ
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	nowMs  func() uint64
}

func main() {
	c := &cli{
		env:    os.LookupEnv,
		stdin:  os.Stdin,
		stdout: os.Stdout,
		stderr: os.Stderr,
		nowMs:  func() uint64 { return uint64(max(0, time.Now().UnixMilli())) },
	}
	os.Exit(c.run(os.Args[1:]))
}

func (c *cli) run(args []string) int {
	if len(args) == 0 {
		return c.fail(exitMalformed, usageText)
	}
	command := args[0]

	var o opts
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		arg := rest[i]
		if arg == "--json" {
			o.json = true
			continue
		}
		if !strings.HasPrefix(arg, "--") {
			o.positionals = append(o.positionals, arg)
			continue
		}
		i++
		if i >= len(rest) {
			return c.fail(exitMalformed, arg+" requires a value\n")
		}
		value := rest[i]
		var slot **string
		switch arg {
		case "--config":
			slot = &o.config
		case "--root":
			slot = &o.root
		case "--default-root":
			slot = &o.defaultRoot
		case "--index":
			slot = &o.index
		case "--world":
			slot = &o.world
		case "--scope":
			slot = &o.scope
		case "--lanes":
			slot = &o.lanes
		case "--cursor":
			slot = &o.cursor
		case "--episode":
			slot = &o.episode
		case "--revision":
			slot = &o.revision
		case "--path":
			slot = &o.path
		case "--lines":
			slot = &o.lines
		case "--credit-mode":
			slot = &o.creditMode
		case "--limit":
			n, err := strconv.ParseUint(value, 10, 32)
			if err != nil {
				return c.fail(exitMalformed, "--limit must be a positive integer\n")
			}
			v := uint32(n)
			o.limit = &v
			continue
		default:
			return c.fail(exitMalformed, usageText)
		}
		*slot = &value
	}

	if command == "version" {
		fmt.Fprintf(c.stdout, "autojournal %s (payload schema v%d, episode schema %s, index schema v%d, %s, %s, %s)\n",
			aj.PackageVersion, aj.PayloadSchemaVersion, aj.EpisodeSchema, aj.IndexSchemaVersion,
			aj.TokenizerVersion, aj.ScorerVersion, aj.ConfidencePolicyVersion)
		return exitOK
	}

	// Configuration is optional because commands can use the host-neutral
	// journal default when neither config nor --root is present.
	explicitConfig := o.config != nil
	if !explicitConfig {
		if p, ok := c.env("AUTOJOURNAL_CONFIG"); ok && p != "" {
			explicitConfig = true
		}
	}
	explicitConfigPath := ""
	if o.config != nil {
		// An explicitly named empty path can never load (the reference
		// opens "" and fails); refuse it rather than letting the empty
		// string read as "no explicit path" and fall back silently.
		if *o.config == "" {
			return c.fail(exitFailure, "explicit AutoJournal config was not found\n")
		}
		explicitConfigPath = *o.config
	}
	cfg := aj.DefaultConfig()
	var loaded *aj.LoadedConfig
	if l, err := aj.LoadConfig(c.env, explicitConfigPath); err == nil {
		loaded = l
		cfg = l.Config
	} else {
		switch {
		case errors.Is(err, aj.ErrConfigNotFound):
			if explicitConfig {
				return c.fail(exitFailure, "explicit AutoJournal config was not found\n")
			}
		case errors.Is(err, aj.ErrConfigMalformed):
			return c.fail(exitFailure, "config is malformed (see config.json schema)\n")
		default:
			return c.fail(exitFailure, "config unavailable\n")
		}
	}

	switch command {
	case "alias":
		return c.aliasCommand(cfg, &o)
	case "default":
		return c.defaultCommand(cfg, &o)
	}

	// Root resolution: explicit command override, an owner configuration
	// that names a root, a deprecated host fallback for pre-release
	// adapters, then AutoJournal's host-neutral XDG data default.
	rootSource := "autojournal_default"
	var rootPath string
	switch {
	case o.root != nil:
		rootPath = *o.root
		rootSource = "explicit"
	case cfg.JournalRoot != "":
		rootPath = cfg.JournalRoot
		rootSource = "owner_config"
	case o.defaultRoot != nil:
		rootPath = *o.defaultRoot
		rootSource = "host_default"
	default:
		p, err := aj.DefaultJournalRoot(c.env)
		if err != nil {
			return c.fail(exitFailure, "cannot resolve the default journal root (no HOME)\n")
		}
		rootPath = p
	}
	indexPath := ""
	if o.index != nil {
		indexPath = *o.index
	} else {
		p, err := aj.DefaultIndexPath(c.env, rootPath)
		if err != nil {
			return c.fail(exitFailure, "cannot resolve the default index path (no HOME)\n")
		}
		indexPath = p
	}

	switch command {
	case "capture":
		return c.captureCommand(cfg, rootPath, indexPath)
	case "status":
		rootSourcePath := ""
		if rootSource == "owner_config" && loaded != nil {
			rootSourcePath = loaded.SourcePath
		}
		return c.statusCommand(rootPath, indexPath, rootSource, rootSourcePath, o.json)
	case "catalog":
		return c.catalogCommand(cfg, rootPath, indexPath)
	case "sync":
		return c.syncCommand(rootPath, indexPath)
	case "search":
		return c.searchCommand(cfg, rootPath, indexPath, &o)
	case "get":
		return c.getCommand(rootPath, indexPath, &o)
	}
	return c.fail(exitMalformed, usageText)
}

// --- Paths ---
//
// The derivations themselves live in the library's paths module so this
// CLI and an embedding host cannot drift apart on where the journal and
// its index are.

// thesaurusPath resolves the thesaurus: owner config first, the legacy
// environment override second, the XDG default last. The file is
// hand-editable and hot-loads on every invocation.
func (c *cli) thesaurusPath(cfg aj.Config) (string, error) {
	if cfg.ThesaurusPath != "" {
		return cfg.ThesaurusPath, nil
	}
	if p, ok := c.env("AUTOJOURNAL_THESAURUS"); ok && p != "" {
		return p, nil
	}
	if xdg, ok := c.env("XDG_CONFIG_HOME"); ok && xdg != "" {
		return xdg + "/autojournal/thesaurus.json", nil
	}
	home, ok := c.env("HOME")
	if !ok {
		return "", aj.ErrMissingHome
	}
	return home + "/.config/autojournal/thesaurus.json", nil
}

func (c *cli) missLogPath() (string, error) {
	if p, ok := c.env("AUTOJOURNAL_MISS_LOG"); ok && p != "" {
		return p, nil
	}
	state, err := aj.StateDir(c.env)
	if err != nil {
		return "", err
	}
	return state + "/autojournal/thesaurus-candidates.jsonl", nil
}

// openIndex opens the projection, creating its parent directory
// owner-only on the first run. Never touches the journal root. When
// expectedRoot is set, an index recording another root's identity is
// rejected as ErrForeignIndex instead of being misread as empty memory;
// sync passes none because it rebuilds and re-stamps the identity itself.
func openIndex(indexPath, expectedRoot string) (*aj.Index, error) {
	var digest *string
	if expectedRoot != "" {
		d := aj.RootDigestHex(expectedRoot)
		digest = &d
	}
	return aj.OpenIndexHardened(indexPath, digest)
}

// --- search ---

func parseLanes(text string) []aj.Lane {
	var lanes []aj.Lane
	for _, tag := range strings.Split(text, ",") {
		trimmed := strings.Trim(tag, " ")
		if trimmed == "" {
			continue
		}
		lane := aj.Lane(trimmed)
		switch lane {
		case aj.LaneConversation, aj.LaneDelegatedWork, aj.LaneEvaluation, aj.LaneImportedLegacy:
		default:
			return nil
		}
		dup := false
		for _, seen := range lanes {
			if seen == lane {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		if len(lanes) >= 4 {
			return nil
		}
		lanes = append(lanes, lane)
	}
	if len(lanes) == 0 {
		return nil
	}
	return lanes
}

func (c *cli) searchCommand(cfg aj.Config, rootPath, indexPath string, o *opts) int {
	if len(o.positionals) == 0 {
		return c.fail(exitMalformed, "search needs query words: autojournal search <query...>\n")
	}
	query := strings.Join(o.positionals, " ")
	if len(query) > aj.MaxQueryBytes {
		return c.fail(exitMalformed, "query exceeds max_query_bytes\n")
	}
	// World fallback mirrors capture: an unconfigured install searches the
	// world capture publishes into ("main" unless the config says otherwise).
	world := cfg.Capture.World
	if cfg.DefaultWorld != "" {
		world = cfg.DefaultWorld
	}
	if o.world != nil {
		world = *o.world
	}

	lanes := aj.DefaultLanes
	if o.lanes != nil {
		lanes = parseLanes(*o.lanes)
		if lanes == nil {
			return c.fail(exitMalformed, "--lanes takes a comma list of: conversation, delegated_work, evaluation, imported_legacy\n")
		}
	}

	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return c.fail(exitFailure, "journal root missing or unreadable\n")
	}
	defer root.Close()
	idx, err := openIndex(indexPath, rootPath)
	if err != nil {
		if errors.Is(err, aj.ErrForeignIndex) {
			return c.fail(exitFailure, "index at this path belongs to a different journal root; run sync to rebuild it\n")
		}
		return c.fail(exitFailure, "cannot open index database\n")
	}
	defer idx.Close()

	thesaurus, err := c.thesaurusPath(cfg)
	if err != nil {
		return c.fail(exitFailure, "cannot resolve the thesaurus path (no HOME)\n")
	}
	aliasMap := aj.LoadAliasMapFile(thesaurus)

	creditMode := aj.CreditWordStart
	if o.creditMode != nil {
		creditMode = aj.CreditMode(*o.creditMode)
		switch creditMode {
		case aj.CreditSubstring, aj.CreditWordStart, aj.CreditWholeWord:
		default:
			return c.fail(exitMalformed, "--credit-mode takes: substring, word_start, whole_word\n")
		}
	}

	chosen := cfg.MaxResults
	if o.limit != nil {
		chosen = *o.limit
	}
	// The reference request clamps 0 to one result; the Go library's zero
	// value means "default page size", so resolve here.
	limit := min(chosen, aj.MaxResultsLimit)
	if limit == 0 {
		limit = 1
	}

	nowMs := c.nowMs()
	out := aj.Search(root, idx, aliasMap, aj.SearchRequest{
		Query:      query,
		World:      world,
		Scope:      o.scope,
		Lanes:      lanes,
		CreditMode: creditMode,
		Limit:      limit,
		Cursor:     o.cursor,
		NowMs:      nowMs,
		Knobs: aj.Knobs{
			ContextWindow:   cfg.ContextWindow,
			RecencyBoost:    cfg.RecencyBoost,
			MinScore:        cfg.MinScore,
			ConfidenceFloor: cfg.ConfidenceFloor,
		},
	})

	// Weak-query miss logging: opt-in, bounded, best-effort, and only for
	// real (non-error) recall outcomes.
	if cfg.MissLog && (out.Outcome == aj.OutcomeMatch || out.Outcome == aj.OutcomeNoMatch) &&
		out.BestScore < cfg.ConfidenceFloor {
		if logPath, err := c.missLogPath(); err == nil {
			var top *string
			if len(out.Hits) > 0 {
				top = &out.Hits[0].EpisodeID
			}
			aj.AppendMiss(logPath, aj.MissRecord{
				TS:    aj.ISOFromMs(nowMs),
				Query: query,
				Terms: out.QueryTerms,
				Best:  out.BestScore,
				Top:   top,
			}, cfg.MissLogMaxBytes)
		}
	}

	if o.json {
		c.renderSearchJSON(world, query, &out)
	} else {
		c.renderSearchText(query, &out)
	}
	return outcomeExit(out.Outcome)
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
	Outcome    string               `json:"outcome"`
	Query      string               `json:"query"`
	QueryTerms []string             `json:"query_terms"`
	AliasTerms []string             `json:"alias_terms"`
	Results    []hitJSON            `json:"results"`
	Total      uint64               `json:"total"`
	Cursor     *string              `json:"cursor"`
	Identities searchIdentitiesJSON `json:"identities"`
	Index      searchIndexJSON      `json:"index"`
	Detail     *string              `json:"detail"`
}

func (c *cli) renderSearchJSON(world, query string, out *aj.SearchOutput) {
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
	c.printJSON(searchReportJSON{
		Outcome:    string(out.Outcome),
		Query:      query,
		QueryTerms: nonNil(out.QueryTerms),
		AliasTerms: nonNil(out.AliasTerms),
		Results:    results,
		Total:      out.Total,
		Cursor:     optString(out.NextCursor),
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

// --- get ---

type lineSpan struct {
	start, end uint32
}

func parseLineSpan(text string) *lineSpan {
	parseU32 := func(s string) (uint32, bool) {
		n, err := strconv.ParseUint(s, 10, 32)
		return uint32(n), err == nil
	}
	if dash := strings.IndexByte(text, '-'); dash >= 0 {
		start, okS := parseU32(text[:dash])
		end, okE := parseU32(text[dash+1:])
		if !okS || !okE || start == 0 || end < start {
			return nil
		}
		return &lineSpan{start: start, end: end}
	}
	line, ok := parseU32(text)
	if !ok || line == 0 {
		return nil
	}
	return &lineSpan{start: line, end: line}
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

func (c *cli) getCommand(rootPath, indexPath string, o *opts) int {
	if o.episode == nil {
		return c.fail(exitMalformed, "get needs --episode <id> and --revision <sha256:hex>\n")
	}
	if o.revision == nil {
		return c.fail(exitMalformed, "get needs --revision <sha256:hex> (from a search result)\n")
	}
	span := &lineSpan{}
	if o.lines != nil {
		span = parseLineSpan(*o.lines)
		if span == nil {
			return c.fail(exitMalformed, "--lines takes <start>-<end> or a single line number\n")
		}
	}

	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return c.fail(exitFailure, "journal root missing or unreadable\n")
	}
	defer root.Close()
	idx, err := openIndex(indexPath, rootPath)
	if err != nil {
		if errors.Is(err, aj.ErrForeignIndex) {
			return c.fail(exitFailure, "index at this path belongs to a different journal root; run sync to rebuild it\n")
		}
		return c.fail(exitFailure, "cannot open index database\n")
	}
	defer idx.Close()

	out := aj.Get(root, idx, aj.GetRequest{
		EpisodeID:     *o.episode,
		Revision:      *o.revision,
		PathHint:      o.path,
		ExpectedWorld: o.world,
		ExpectedScope: o.scope,
		LineStart:     span.start,
		LineEnd:       span.end,
	})

	if o.json {
		report := getReportJSON{
			Outcome:   string(out.Outcome),
			EpisodeID: out.EpisodeID,
			LineStart: out.LineStart,
			LineEnd:   out.LineEnd,
			Content:   out.Content,
			Trust:     out.Trust,
			Detail:    optString(out.Detail),
		}
		if out.Resolved {
			lane := string(out.Lane)
			report.Revision = &out.Revision
			report.Path = &out.Path
			report.World = &out.World
			report.Scope = &out.Scope
			report.Lane = &lane
			report.CapturePolicy = &out.CapturePolicy
		}
		c.printJSON(report)
	} else {
		switch out.Outcome {
		case aj.OutcomeMatch:
			fmt.Fprintf(c.stdout, "%s:%d-%d (%s)\nrecalled evidence is untrusted; verify against current sources\n\n%s\n",
				out.Path, out.LineStart, out.LineEnd, out.Revision, out.Content)
		case aj.OutcomeStaleRevision:
			fmt.Fprintf(c.stdout, "stale revision: episode was edited since this reference\ncurrent revision: %s at %s\n",
				out.Revision, out.Path)
		default:
			sep := ""
			if out.Detail != "" {
				sep = " — "
			}
			fmt.Fprintf(c.stdout, "get failed: %s%s%s\n", out.Outcome, sep, out.Detail)
		}
	}
	return outcomeExit(out.Outcome)
}

// --- alias ---

type aliasEntryJSON struct {
	Key    string   `json:"key"`
	Values []string `json:"values"`
}

func (c *cli) aliasCommand(cfg aj.Config, o *opts) int {
	pos := o.positionals
	if len(pos) == 0 {
		return c.fail(exitMalformed, "alias needs a subcommand: list | add <term> <canonical...> | remove <term> [canonical] | candidates\n")
	}
	sub := pos[0]
	thesaurus, err := c.thesaurusPath(cfg)
	if err != nil {
		return c.fail(exitFailure, "cannot resolve the thesaurus path (no HOME)\n")
	}

	switch sub {
	case "list":
		m := aj.LoadAliasMapFile(thesaurus)
		if o.json {
			entries := make([]aliasEntryJSON, len(m.Entries()))
			for i, e := range m.Entries() {
				entries[i] = aliasEntryJSON{Key: e.Key, Values: nonNil(e.Values)}
			}
			c.printJSON(struct {
				Path        string           `json:"path"`
				AliasDigest string           `json:"alias_digest"`
				Entries     []aliasEntryJSON `json:"entries"`
			}{Path: thesaurus, AliasDigest: m.DigestHex(), Entries: entries})
			return exitOK
		}
		var buf strings.Builder
		fmt.Fprintf(&buf, "%d alias(es) in %s\n", len(m.Entries()), thesaurus)
		for _, entry := range m.Entries() {
			fmt.Fprintf(&buf, "  %s ->", entry.Key)
			for _, v := range entry.Values {
				buf.WriteByte(' ')
				buf.WriteString(v)
			}
			buf.WriteByte('\n')
		}
		buf.WriteString("edit freely in any text editor; changes apply on the next search\n")
		io.WriteString(c.stdout, buf.String())
		return exitOK

	case "add":
		if len(pos) < 3 {
			return c.fail(exitMalformed, "alias add <term> <canonical...>\n")
		}
		if err := aj.AddAlias(thesaurus, pos[1], pos[2:]); err != nil {
			switch {
			case errors.Is(err, aj.ErrAliasInvalidTerm):
				return c.fail(exitMalformed, "term must be a searchable word: longer than 2 letters, [a-z0-9_], not a stop word\n")
			case errors.Is(err, aj.ErrAliasInvalidValue):
				return c.fail(exitMalformed, "canonical values must be 2..128 chars of [A-Za-z0-9._:+/@-]\n")
			case errors.Is(err, aj.ErrAliasMalformed):
				return c.fail(exitFailure, "thesaurus file is not a JSON object; fix it by hand first\n")
			default:
				return c.fail(exitFailure, "cannot write thesaurus file\n")
			}
		}
		fmt.Fprintf(c.stdout, "added: %s -> %s (%s)\n", pos[1], strings.Join(pos[2:], " "), thesaurus)
		return exitOK

	case "remove":
		if len(pos) < 2 || len(pos) > 3 {
			return c.fail(exitMalformed, "alias remove <term> [canonical]\n")
		}
		var canonical *string
		if len(pos) == 3 {
			canonical = &pos[2]
		}
		removed, err := aj.RemoveAlias(thesaurus, pos[1], canonical)
		if err != nil {
			switch {
			case errors.Is(err, aj.ErrAliasNotFound):
				return c.fail(exitFailure, "no such alias entry\n")
			case errors.Is(err, aj.ErrAliasMalformed):
				return c.fail(exitFailure, "thesaurus file is not a JSON object; fix it by hand first\n")
			default:
				return c.fail(exitFailure, "cannot write thesaurus file\n")
			}
		}
		fmt.Fprintf(c.stdout, "removed %s: %s\n", string(removed), pos[1])
		return exitOK

	case "candidates":
		logPath, err := c.missLogPath()
		if err != nil {
			return c.fail(exitFailure, "cannot resolve the miss-log path (no HOME)\n")
		}
		data, err := readLimited(logPath, 16*1024*1024)
		if err != nil {
			fmt.Fprintf(c.stdout, "no candidates yet (%s); enable with \"miss_log\": true in config.json\n", logPath)
			return exitOK
		}
		agg := aj.AggregateMisses(data)
		limit := uint32(20)
		if o.limit != nil {
			limit = *o.limit
		}
		shown := min(int(limit), len(agg))
		var buf strings.Builder
		plural := "ies"
		if len(agg) == 1 {
			plural = "y"
		}
		fmt.Fprintf(&buf, "%d distinct weak quer%s, most frequent first:\n", len(agg), plural)
		for _, cand := range agg[:shown] {
			fmt.Fprintf(&buf, "  [%dx] %s\n        terms:", cand.Count, cand.Query)
			for _, t := range cand.Terms {
				buf.WriteByte(' ')
				buf.WriteString(t)
			}
			buf.WriteByte('\n')
		}
		buf.WriteString("for each: if the journal really covers it, promote with\n  autojournal alias add <casual term> <canonical term>\n")
		io.WriteString(c.stdout, buf.String())
		return exitOK
	}

	return c.fail(exitMalformed, "unknown alias subcommand; use list | add | remove | candidates\n")
}

// --- capture / status / catalog / sync (write slice) ---

type captureReportJSON struct {
	Outcome       string  `json:"outcome"`
	EpisodeID     *string `json:"episode_id"`
	PayloadDigest *string `json:"payload_digest"`
	Path          *string `json:"path"`
	Index         string  `json:"index"`
	Detail        *string `json:"detail"`
}

func (c *cli) reportCapture(exit int, report captureReportJSON) int {
	if report.Index == "" {
		report.Index = string(aj.IndexNotBuilt)
	}
	c.printJSON(report)
	return exit
}

func (c *cli) captureCommand(cfg aj.Config, rootPath, indexPath string) int {
	payloadBytes, err := io.ReadAll(io.LimitReader(c.stdin, aj.MaxPayloadBytes+2))
	if err != nil {
		return c.fail(exitFailure, "failed reading stdin\n")
	}
	if len(payloadBytes) > aj.MaxPayloadBytes+1 {
		detail := "payload exceeds max_payload_bytes"
		return c.reportCapture(exitMalformed, captureReportJSON{Outcome: "malformed", Detail: &detail})
	}

	raw, err := aj.ParsePayload(payloadBytes)
	if err != nil {
		detail := zigErrorName(err)
		return c.reportCapture(exitMalformed, captureReportJSON{Outcome: "malformed", Detail: &detail})
	}
	// An omitted world/scope falls back to owner capture defaults. A host may
	// provide explicit values only when transporting an owner session choice.
	if raw.World == nil {
		raw.World = &cfg.Capture.World
	}
	if raw.Scope == nil {
		raw.Scope = &cfg.Capture.Scope
	}
	payload, err := aj.Validate(raw)
	if err != nil {
		detail := zigErrorName(err)
		return c.reportCapture(exitMalformed, captureReportJSON{Outcome: "malformed", Detail: &detail})
	}

	if aj.RootInSharedDirectory(rootPath) {
		detail := "journal root sits under a shared (group- or world-writable) directory; chmod g-w,o-w the parent or configure journal_root to a private location"
		return c.reportCapture(exitFailure, captureReportJSON{Outcome: "permission_denied", Detail: &detail})
	}
	root, err := aj.OpenJournalRoot(rootPath)
	if err != nil {
		detail := "cannot open journal root"
		return c.reportCapture(exitFailure, captureReportJSON{Outcome: "unavailable", Detail: &detail})
	}
	defer root.Close()

	// Identity is corpus-wide, but the store's duplicate detection is
	// path-local; consult the index first so a redelivery whose event time
	// shards to another date is still recognized. Best-effort: an absent or
	// unhelpful index falls through to publication as before.
	if idx, err := openIndex(indexPath, rootPath); err == nil {
		existing := aj.CheckRedelivery(root, idx, &payload)
		idx.Close()
		if existing != nil {
			episodeID := aj.EpisodeID(&payload)
			digest := aj.DigestPrefix + aj.PayloadDigestHex(&payload)
			exit := exitOK
			indexState := "fresh"
			if existing.Outcome != aj.CaptureDuplicate {
				exit = exitConflict
				indexState = "stale"
			}
			return c.reportCapture(exit, captureReportJSON{
				Outcome:       string(existing.Outcome),
				EpisodeID:     &episodeID,
				PayloadDigest: &digest,
				Path:          &existing.RelPath,
				Index:         indexState,
			})
		}
	}

	published, err := aj.Publish(root, &payload, c.nowMs())
	if err != nil {
		outcome := aj.CaptureUnavailable
		switch {
		case errors.Is(err, aj.ErrContainmentViolation):
			outcome = aj.CaptureInternalError
		case errors.Is(err, aj.ErrPermissionDenied):
			outcome = aj.CapturePermissionDenied
		}
		detail := zigErrorName(err)
		return c.reportCapture(exitFailure, captureReportJSON{Outcome: string(outcome), Detail: &detail})
	}

	// Source publication is already durable; the index is best-effort here
	// and repairable via sync, so its failure downgrades freshness only.
	indexState := aj.IndexStale
	switch published.Outcome {
	case aj.CaptureConflict:
	case aj.CapturePublished, aj.CaptureDuplicate:
		if idx, err := openIndex(indexPath, rootPath); err != nil {
			if errors.Is(err, aj.ErrForeignIndex) {
				indexState = aj.IndexUnavailable
			}
		} else {
			indexed := idx.IndexEpisode(published.RelPath, string(published.Content)) == nil
			idx.Close()
			if indexed && aj.HardenIndexFiles(indexPath) == nil {
				indexState = aj.IndexFresh
			}
		}
	}

	exit := exitOK
	if published.Outcome == aj.CaptureConflict {
		exit = exitConflict
	}
	digest := aj.DigestPrefix + published.DigestHex
	return c.reportCapture(exit, captureReportJSON{
		Outcome:       string(published.Outcome),
		EpisodeID:     &published.EpisodeID,
		PayloadDigest: &digest,
		Path:          &published.RelPath,
		Index:         string(indexState),
	})
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

func (c *cli) statusCommand(rootPath, indexPath, rootSource, rootSourcePath string, asJSON bool) int {
	report := aj.StatusOf(rootPath, indexPath)
	if asJSON {
		c.printJSON(statusReportJSON{
			JournalRoot:    rootPath,
			RootSource:     rootSource,
			RootSourcePath: optString(rootSourcePath),
			RootOK:         report.RootOK,
			Episodes:       report.Episodes,
			Index: statusIndexJSON{
				Freshness: string(report.Freshness),
				Indexed:   report.Indexed,
				Path:      indexPath,
			},
		})
	} else if !report.RootOK {
		fmt.Fprintf(c.stdout, "journal_root: %s (missing)\nepisodes: 0\nindex: not_built\n", rootPath)
	} else {
		fmt.Fprintf(c.stdout, "journal_root: %s (ok)\nepisodes: %d\nindex: %s (%d indexed, %s)\n",
			rootPath, report.Episodes, report.Freshness, report.Indexed, indexPath)
	}
	if !report.RootOK || report.Freshness == aj.IndexStale || report.Freshness == aj.IndexUnavailable {
		return exitFailure
	}
	return exitOK
}

type catalogPairJSON struct {
	World string `json:"world"`
	Scope string `json:"scope"`
}

func (c *cli) catalogCommand(cfg aj.Config, rootPath, indexPath string) int {
	pairs := []catalogPairJSON{{World: cfg.Capture.World, Scope: cfg.Capture.Scope}}

	if _, err := os.Stat(indexPath); err == nil {
		if idx, err := openIndex(indexPath, rootPath); err == nil {
			if rows, err := idx.WorldScopePairs(); err == nil {
				for _, row := range rows {
					exists := false
					for _, pair := range pairs {
						if pair.World == row.World && pair.Scope == row.Scope {
							exists = true
							break
						}
					}
					if !exists {
						pairs = append(pairs, catalogPairJSON{World: row.World, Scope: row.Scope})
					}
				}
			}
			idx.Close()
		}
	}

	c.printJSON(struct {
		Pairs []catalogPairJSON `json:"pairs"`
	}{Pairs: pairs})
	return exitOK
}

// defaultCommand shows or persists the owner's default world/scope. With
// neither --world nor --scope this prints the effective capture defaults;
// with either it rewrites the owner config atomically, so future sessions
// in every conforming harness start there.
func (c *cli) defaultCommand(cfg aj.Config, o *opts) int {
	world := cfg.Capture.World
	if o.world != nil {
		world = *o.world
	}
	scope := cfg.Capture.Scope
	if o.scope != nil {
		scope = *o.scope
	}
	if o.world != nil || o.scope != nil {
		explicitPath := ""
		if o.config != nil {
			explicitPath = *o.config
		}
		written, err := aj.SaveCaptureDefaults(c.env, explicitPath, world, scope)
		if err != nil {
			switch {
			case errors.Is(err, aj.ErrConfigMalformed):
				return c.fail(exitMalformed, "cannot save defaults: invalid world/scope, or the existing config is malformed\n")
			case errors.Is(err, aj.ErrConfigNotFound):
				return c.fail(exitFailure, "cannot resolve a config path (no HOME)\n")
			default:
				return c.fail(exitFailure, "cannot write the owner config\n")
			}
		}
		if o.json {
			c.printJSON(struct {
				World  string `json:"world"`
				Scope  string `json:"scope"`
				Config string `json:"config"`
			}{World: world, Scope: scope, Config: written})
		} else {
			fmt.Fprintf(c.stdout, "default set: %s / %s\nconfig: %s\n", world, scope, written)
		}
		return exitOK
	}
	if o.json {
		c.printJSON(struct {
			World string `json:"world"`
			Scope string `json:"scope"`
		}{World: world, Scope: scope})
	} else {
		fmt.Fprintf(c.stdout, "default world: %s\ndefault scope: %s\n", world, scope)
	}
	return exitOK
}

func (c *cli) syncCommand(rootPath, indexPath string) int {
	report, err := aj.Sync(rootPath, indexPath)
	if err != nil {
		switch {
		case errors.Is(err, aj.ErrSharedDirectory):
			return c.fail(exitFailure, "journal root sits under a shared (group- or world-writable) directory; chmod g-w,o-w the parent or configure journal_root to a private location\n")
		case errors.Is(err, aj.ErrRootMissing):
			return c.fail(exitFailure, "journal root missing; nothing to sync\n")
		case errors.Is(err, aj.ErrSyncFailed):
			return c.fail(exitFailure, "sync failed; projection rolled back\n")
		default:
			return c.fail(exitFailure, "cannot open index database\n")
		}
	}
	fmt.Fprintf(c.stdout, "indexed: %d\nremoved: %d\nskipped_malformed: %d\nduplicate_ids: %d\n",
		report.Indexed, report.Removed, report.SkippedMalformed, report.DuplicateIDs)
	return exitOK
}

// --- output helpers ---

func (c *cli) fail(exit int, message string) int {
	io.WriteString(c.stderr, message)
	return exit
}

// printJSON renders like the reference's std.json.Stringify: declaration
// field order, raw UTF-8, and no HTML escaping.
func (c *cli) printJSON(v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(c.stderr, "internal error rendering JSON: %v\n", err)
		return
	}
	c.stdout.Write(buf.Bytes()) // Encode appends the trailing newline
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// nonNil keeps empty slices rendering as [] — the reference never emits
// null for a list field.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func readLimited(path string, budget int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, budget+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > budget {
		return nil, errors.New("over budget")
	}
	return data, nil
}

// zigErrorName renders the reference CLI's failure vocabulary
// (@errorName strings) for capture report details.
func zigErrorName(err error) string {
	for _, m := range []struct {
		err  error
		name string
	}{
		{aj.ErrMalformed, "Malformed"},
		{aj.ErrUnsupportedSchemaVersion, "UnsupportedSchemaVersion"},
		{aj.ErrInvalidWorld, "InvalidWorld"},
		{aj.ErrInvalidScope, "InvalidScope"},
		{aj.ErrInvalidLane, "InvalidLane"},
		{aj.ErrInvalidHarness, "InvalidHarness"},
		{aj.ErrInvalidAdapterVersion, "InvalidAdapterVersion"},
		{aj.ErrInvalidSessionID, "InvalidSessionId"},
		{aj.ErrInvalidTurnID, "InvalidTurnId"},
		{aj.ErrInvalidCapturePolicy, "InvalidCapturePolicy"},
		{aj.ErrInvalidTurnOutcome, "InvalidTurnOutcome"},
		{aj.ErrEmptyUserContent, "EmptyUserContent"},
		{aj.ErrEmptyAssistantResult, "EmptyAssistantResult"},
		{aj.ErrOversizedContent, "OversizedContent"},
		{aj.ErrInvalidUTF8, "InvalidUtf8"},
		{aj.ErrTooManyTools, "TooManyTools"},
		{aj.ErrInvalidToolName, "InvalidToolName"},
		{aj.ErrInvalidWorkspaceRoot, "InvalidWorkspaceRoot"},
		{aj.ErrInvalidBranchOf, "InvalidBranchOf"},
		{aj.ErrInvalidHost, "InvalidHost"},
		{aj.ErrContainmentViolation, "ContainmentViolation"},
		{aj.ErrPermissionDenied, "PermissionDenied"},
		{aj.ErrStoreUnavailable, "Unavailable"},
	} {
		if errors.Is(err, m.err) {
			return m.name
		}
	}
	return "Unavailable"
}
