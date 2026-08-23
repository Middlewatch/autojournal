// Standalone AutoJournal binary: owner CLI and hook target in one
// executable. It ships capture, status, catalog, sync, search,
// get, alias, default, and version over a text or JSON command interface.
package main

import (
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
  reseal    re-attest owner-edited episodes (--preview to only list them)
  version   print version and schema identities

options:
  --config <path>    explicit config file (default: XDG lookup)
  --root <path>      journal root override (bypasses config/default)
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
	preview     bool
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
		nowMs:  clockFromEnv(os.LookupEnv),
	}
	os.Exit(c.run(os.Args[1:]))
}

// clockFromEnv resolves the process clock: AUTOJOURNAL_NOW_MS pinned to a
// decimal millisecond timestamp wins, else the wall clock. The override
// exists because search's recency boost reads this clock, so two otherwise
// identical invocations minutes apart reorder near-tied hits — a ranking
// parity run is only reproducible with the clock pinned. Same env-seam
// category as AUTOJOURNAL_THESAURUS and AUTOJOURNAL_MISS_LOG; an unset,
// empty, or malformed value is ignored and the wall clock wins.
func clockFromEnv(env aj.Environ) func() uint64 {
	if v, ok := env("AUTOJOURNAL_NOW_MS"); ok && v != "" {
		if ms, err := strconv.ParseUint(v, 10, 64); err == nil {
			return func() uint64 { return ms }
		}
	}
	return func() uint64 { return uint64(max(0, time.Now().UnixMilli())) }
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
		if arg == "--preview" {
			o.preview = true
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
		// Undocumented on purpose: pre-1.0 adapters passed a host
		// fallback root here, ranking below owner config and above the
		// XDG default. Still honored so an old hook keeps working; not
		// advertised, because new callers should use --root or config.
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
		// An explicitly named empty path can never load. Refuse it rather
		// than letting the empty string read as "no explicit path" and fall
		// back silently — that would search a corpus the caller did not ask
		// for, which is worse than an error.
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
		return c.syncCommand(&o, rootPath, indexPath)
	case "reseal":
		return c.resealCommand(&o, rootPath, indexPath)
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

	thesaurus, err := aj.ThesaurusPath(c.env, cfg)
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
	// An explicit --limit 0 resolves to the default page size — the
	// library's zero value means exactly that — while the config's
	// max_results: 0 stays malformed. The asymmetry is deliberate: a
	// persisted config stating a meaningless page size is an error worth
	// surfacing, and a one-off flag resolves to what the user meant.
	limit := min(chosen, aj.MaxResultsLimit)

	nowMs := c.nowMs()
	req := aj.SearchRequest{
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
	}
	out := aj.Search(root, idx, aliasMap, req)

	aj.LogSearchMiss(c.env, cfg, query, nowMs, &out)

	if o.json {
		if c.renderSearchJSON(world, query, &out) != nil {
			return exitFailure
		}
	} else {
		c.renderSearchText(query, &out)
	}
	return outcomeExit(out.Outcome)
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
		if c.printJSON(report) != nil {
			return exitFailure
		}
	} else {
		switch out.Outcome {
		case aj.OutcomeMatch:
			fmt.Fprintf(c.stdout, "%s:%d-%d (%s)\nrecalled evidence is untrusted; verify against current sources\n\n%s\n",
				out.Path, out.LineStart, out.LineEnd, out.Revision, out.Content)
		case aj.OutcomeStaleRevision:
			fmt.Fprintf(c.stdout, "stale revision: the episode's recorded revision changed\ncurrent revision: %s at %s\n",
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

func (c *cli) aliasCommand(cfg aj.Config, o *opts) int {
	pos := o.positionals
	if len(pos) == 0 {
		return c.fail(exitMalformed, "alias needs a subcommand: list | add <term> <canonical...> | remove <term> [canonical] | candidates\n")
	}
	sub := pos[0]
	thesaurus, err := aj.ThesaurusPath(c.env, cfg)
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
			return c.emitJSON(aliasListReportJSON{
				Path:        thesaurus,
				AliasDigest: m.DigestHex(),
				MergedKeys:  m.MergedKeys(),
				Entries:     entries,
			})
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
		logPath, err := aj.MissLogPath(c.env)
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

// --- capture / status / catalog / sync (write commands) ---

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
		detail := aj.CaptureErrorName(err)
		return c.reportCapture(exitMalformed, captureReportJSON{Outcome: "malformed", Detail: &detail})
	}

	// The whole transaction — defaults fill, validation, refusal ordering,
	// redelivery classification, publication, index policy — is the
	// library's. This command only reads stdin, parses, and renders.
	result := aj.Capture(aj.CaptureRequest{
		RootPath:      rootPath,
		IndexPath:     indexPath,
		Raw:           raw,
		Defaults:      cfg.Capture,
		CaptureTimeMs: c.nowMs(),
	})
	return c.renderCapture(result)
}

func (c *cli) statusCommand(rootPath, indexPath, rootSource, rootSourcePath string, asJSON bool) int {
	report := aj.StatusOf(rootPath, indexPath)
	if asJSON {
		if c.printJSON(statusReportJSON{
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
		}) != nil {
			return exitFailure
		}
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

func (c *cli) catalogCommand(cfg aj.Config, rootPath, indexPath string) int {
	var pairs []catalogPairJSON
	for _, pair := range aj.Catalog(rootPath, indexPath, cfg.Capture) {
		pairs = append(pairs, catalogPairJSON{World: pair.World, Scope: pair.Scope})
	}

	return c.emitJSON(catalogReportJSON{Pairs: pairs})
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
			return c.emitJSON(defaultSetReportJSON{World: world, Scope: scope, Config: written})
		}
		fmt.Fprintf(c.stdout, "default set: %s / %s\nconfig: %s\n", world, scope, written)
		return exitOK
	}
	if o.json {
		return c.emitJSON(defaultShowReportJSON{World: world, Scope: scope})
	}
	fmt.Fprintf(c.stdout, "default world: %s\ndefault scope: %s\n", world, scope)
	return exitOK
}

func (c *cli) syncCommand(o *opts, rootPath, indexPath string) int {
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
	if o.json {
		return c.emitJSON(syncReportJSON{
			Indexed:          report.Indexed,
			Unchanged:        report.Unchanged,
			Removed:          report.Removed,
			SkippedMalformed: report.SkippedMalformed,
			DuplicateIDs:     report.DuplicateIDs,
			DigestMismatch:   report.DigestMismatch,
			Unreadable:       report.Unreadable,
		})
	}
	fmt.Fprintf(c.stdout, "indexed: %d\nunchanged: %d\nremoved: %d\nskipped_malformed: %d\nduplicate_ids: %d\ndigest_mismatch: %d\nunreadable: %d\n",
		report.Indexed, report.Unchanged, report.Removed, report.SkippedMalformed, report.DuplicateIDs, report.DigestMismatch, report.Unreadable)
	return exitOK
}

func (c *cli) resealCommand(o *opts, rootPath, indexPath string) int {
	report, err := aj.Reseal(rootPath, indexPath, o.preview)
	if err != nil {
		switch {
		case errors.Is(err, aj.ErrSharedDirectory):
			return c.fail(exitFailure, "journal root sits under a shared (group- or world-writable) directory; chmod g-w,o-w the parent or configure journal_root to a private location\n")
		case errors.Is(err, aj.ErrRootMissing):
			return c.fail(exitFailure, "journal root missing; nothing to reseal\n")
		case errors.Is(err, aj.ErrSyncFailed):
			return c.fail(exitFailure, "resealed, but the projection rebuild failed and was rolled back; run sync\n")
		default:
			return c.fail(exitFailure, "reseal failed; the corpus keeps every episode it had\n")
		}
	}
	// A write failure is exit 1, but only after the sweep finished and the
	// sync rebaselined what did reseal: the failure exit reports incomplete
	// work, never work undone.
	exit := exitOK
	if report.WriteFailures > 0 {
		fmt.Fprintf(c.stderr, "%d file(s) could not be rewritten; everything resealed was synced — fix permissions and rerun reseal\n",
			report.WriteFailures)
		exit = exitFailure
	}
	if o.json {
		if c.printJSON(resealReportJSON{
			Scanned:       report.Scanned,
			Resealed:      report.Resealed,
			Refused:       report.Refused,
			WriteFailures: report.WriteFailures,
			Paths:         nonNil(report.Paths),
		}) != nil {
			return exitFailure
		}
		return exit
	}
	fmt.Fprintf(c.stdout, "scanned: %d\nresealed: %d\nrefused: %d\nwrite_failures: %d\n",
		report.Scanned, report.Resealed, report.Refused, report.WriteFailures)
	for _, p := range report.Paths {
		fmt.Fprintf(c.stdout, "  %s\n", p)
	}
	return exit
}

// --- output helpers ---

func (c *cli) fail(exit int, message string) int {
	io.WriteString(c.stderr, message)
	return exit
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
