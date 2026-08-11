// The capability manifest and the AST check that holds the tree to it.
//
// DESIGN.md's Structure section names ten capabilities; this manifest states
// which src/ files implement each and which top-level symbols each file owns,
// and the tests below parse the tree with go/ast and fail on a symbol
// declared in the wrong file, on a symbol no capability claims, and on a
// manifest entry the tree no longer declares. The manifest covers
// non-_test.go files only: a test file belongs beside the file it
// covers by naming convention, which the compiler and the reader already
// enforce.
//
// A change that adds, moves, or removes a top-level declaration updates this
// manifest in the same commit. The manifest is not documentation of the tree;
// it is the statement the tree is held to.

package autojournal

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// capability is one DESIGN.md Structure entry: its number, its name exactly
// as Structure prints it, and the files that implement it with the symbols
// each owns. Methods are keyed Receiver.Method.
type capability struct {
	num   int
	name  string
	files map[string][]string
}

var capabilityManifest = []capability{
	{
		num:  1,
		name: "Contracts",
		files: map[string][]string{
			"contracts.go": {
				"PayloadSchemaVersion", "EpisodeSchema", "MaxPayloadBytes", "MaxContentBytes",
				"MaxEpisodeFileBytes", "CorpusWalkDepth", "MaxWorldLen", "MaxTokenLen",
				"MaxTools", "MaxPathLen", "MaxQueryBytes", "MaxQueryTerms",
				"MaxSnippetLineBytes", "MaxSnippetBytes", "MaxGetLines", "MaxGetBytes",
				"MaxResultsLimit", "Lane", "LaneConversation", "LaneDelegatedWork",
				"LaneEvaluation", "LaneImportedLegacy", "ValidLane",
				"MinEventTimeMs", "MaxEventTimeMs", "ErrImplausibleEventTime",
				"CaptureOutcome", "CapturePublished",
				"CaptureDuplicate", "CaptureSuperseded", "CaptureConflict", "CaptureMalformed",
				"CapturePermissionDenied", "CaptureUnavailable", "CaptureInternalError",
				"IndexFreshness", "IndexFresh", "IndexStale", "IndexNotBuilt",
				"IndexUnavailable", "Outcome", "OutcomeMatch", "OutcomeNoMatch",
				"OutcomeStaleRevision", "OutcomeGone", "OutcomeIndexStale", "OutcomeTimeout",
				"OutcomeUnavailable", "OutcomePermissionDenied", "OutcomeMalformed",
				"OutcomeConflict", "OutcomeInternalError", "ErrUnsupportedSchemaVersion",
				"ErrInvalidWorld", "ErrInvalidScope", "ErrInvalidLane", "ErrInvalidHarness",
				"ErrInvalidAdapterVersion", "ErrInvalidSessionID", "ErrInvalidTurnID",
				"ErrInvalidCapturePolicy", "ErrInvalidTurnOutcome", "ErrEmptyUserContent",
				"ErrEmptyAssistantResult", "ErrOversizedContent", "ErrInvalidUTF8",
				"ErrTooManyTools", "ErrInvalidToolName", "ErrInvalidWorkspaceRoot",
				"ErrInvalidBranchOf", "ErrInvalidHost", "ErrMalformed", "Tool", "RawPayload",
				"Payload", "ValidWorld", "ValidToken", "CaptureErrorName", "ValidPath", "ValidScope",
				"requiredKeys", "optionalKeys", "ParsePayload", "contains", "reqString",
				"optString", "reqUint", "optTools", "rejectDuplicateKeys", "walkValue",
				"Validate",
			},
		},
	},
	{
		num:  2,
		name: "Identity and rendering",
		files: map[string][]string{
			"identity.go": {
				"IDPrefix", "EpisodeIDLen", "DigestPrefix", "DigestHexLen", "EpisodeID",
				"PayloadDigestHex", "hashField",
			},
			"render.go": {
				"RenderInput", "Render", "ISOFromMs", "FrontmatterDigestHex",
			},
			"episode.go": {
				"Episode", "ParseEpisode", "requiredEpisodeKeys", "parseFrontmatterUint",
				"bodyUserHeader", "bodyAssistantSep", "bodyToolsSep", "bodyToolLinePrefix",
				"MaxBodyInterpretations", "VerifiedEpisode", "ErrEpisodeMalformed",
				"ErrDigestMismatch", "bodyReading", "digestPayload", "parseToolsSection",
				"enumerateReadings", "recordedIdentityAgrees", "VerifyEpisode", "ResealDigestHex",
			},
		},
	},
	{
		num:  3,
		name: "Paths and containment",
		files: map[string][]string{
			"paths.go": {
				"Environ", "ErrMissingHome", "homeDir", "xdgBase", "StateDir", "DefaultJournalRoot",
				"IndexDigestNameLen", "RootDigestHex", "DefaultIndexPath",
				"RootInSharedDirectory", "ThesaurusPath", "MissLogPath", "ResolveJournalRoot",
			},
			"corpus.go": {
				"corpusDirPermissions", "corpusFilePermissions", "ErrContainmentViolation",
				"ErrPermissionDenied", "ErrStoreUnavailable", "errTempCollision",
				"ErrEvidenceUnavailable", "corpusError", "OpenJournalRoot", "layoutComponents",
				"openComponents", "openOrCreateChild", "syncCreatedDir", "writeTemp", "syncDir", "ContainedPath",
				"ReadContained", "WalkKind", "WalkEpisode", "WalkUnreadableDir", "WalkShardDir", "WalkCorpus",
				"CorpusSignature", "CorpusSignatureOf",
				"readRootFile",
			},
		},
	},
	{
		num:  4,
		name: "Configuration",
		files: map[string][]string{
			"config.go": {
				"MaxConfigBytes", "Config", "CaptureDefaults", "DefaultConfig",
				"ErrConfigNotFound", "ErrConfigMalformed", "ErrConfigUnavailable",
				"ResolvePath", "LoadedConfig", "LoadConfig", "readConfigFile", "ParseConfig",
				"parseCapture", "optStringField", "optUintField", "optFloatField",
				"optBoolField", "isIntegerShaped", "coerceConfigUint", "SaveCaptureDefaults",
				"writeAtomicConfig", "valueKind", "kindNull", "kindBool", "kindString",
				"kindNumber", "kindObject", "kindArray", "pair", "orderedObject",
				"orderedObject.get", "orderedObject.has", "orderedObject.set",
				"orderedObject.remove", "configValue", "parseOrderedJSON", "parseOrderedValue",
				"writeCanonicalJSON", "writeIndent", "writeCanonicalJSONString",
				"formatConfigNumber",
			},
			"doc.go": {
				"PackageVersion",
			},
		},
	},
	{
		num:  5,
		name: "Store",
		files: map[string][]string{
			"store.go": {
				"Published", "Publish", "classifyExisting", "supersedes",
				"publishWriteTemp", "publishSyncDir", "publishRename",
				"Redelivery", "CheckRedelivery", "CaptureRequest", "CaptureResult", "Capture",
			},
		},
	},
	{
		num:  6,
		name: "Index",
		files: map[string][]string{
			"db.go": {
				"ErrSQLiteBusy", "ErrSQLiteCorrupt", "ErrSQLiteReadOnly", "ErrSQLiteCantOpen",
				"ErrSQLiteMisuse", "ErrSQLiteNoMemory", "ErrSQLite", "sqliteBusy",
				"sqliteLocked", "sqliteNoMemory", "sqliteReadOnly", "sqliteCantOpen",
				"sqliteCorrupt", "sqliteNotADB", "sqliteMisuse", "mapDBError", "busyTimeoutMs", "sqliteURIPath", "openSQLite",
			},
			"index.go": {
				"syncHashKeyPrefix", "IndexSchemaVersion", "createIndexSQL", "ErrForeignIndex",
				"ErrIndexMalformed", "Index", "sqlQuerier", "OpenIndex", "Index.Close",
				"OpenIndexHardened", "HardenIndexFiles", "Index.metaGet", "Index.metaGetInt",
				"Index.ExcludedCount", "Index.excludedCount", "Index.CorpusMatches",
				"Index.metaSet", "Index.writeIdentity", "Index.disposeAllTables", "EpisodeRow",
				"clampMillis", "nonNeg", "Index.Upsert", "Index.upsert", "Index.IndexEpisode",
				"Index.indexEpisodeInTx", "Index.deindexEpisodeInTx", "Index.EpisodeCount",
				"Index.StatsEpisodeCount", "MaxVocabMatches", "Index.VocabTerms",
				"trigramsOf", "Index.VocabCandidates", "PostingRow",
				"postingsTermChunk", "PostingPair", "Index.PostingPairs",
				"Index.postingPairsChunk", "Index.EpisodeMetadata",
				"Index.episodeMetadataChunk", "Index.PostingsForTerm",
				"Index.LookupEpisode", "WorldScope", "Index.WorldScopePairs", "SyncReport",
				"Index.SyncFromCorpus", "Index.syncFromCorpus", "repairShardDir",
				"metaFreshnessVerdict", "metaFreshnessEpisodes", "metaFreshnessMaxMtime",
				"metaFreshnessIndexed", "metaFreshnessExcluded", "FreshnessResult",
				"Index.Freshness", "Index.freshnessMemo", "Index.stampFreshness",
			},
		},
	},
	{
		num:  7,
		name: "Retrieval",
		files: map[string][]string{
			"retrieval.go": {
				"TokenizerVersion", "ScorerVersion", "ConfidencePolicyVersion", "msPerDay",
				"MaxPerEpisodeDefault", "ConfidenceCoverageAlpha", "stopWords", "IsStopWord",
				"isTokenByte", "IsIndexTokenByte", "lowerByte", "Terms", "ExtractTerms",
				"TokenizeLine", "RecencyMultiplier", "IDFWeight", "Candidate", "EpisodeInfo",
				"RankParams", "Ranked", "Rank", "Confidence", "ConfidenceLow",
				"ConfidenceMedium", "ConfidenceHigh", "ConfidenceWithCoverage", "ConfidenceOf",
				"CursorPrefix", "CursorGuardHexLen", "CursorMaxLen", "CursorInputs",
				"CursorGuardHex", "CursorEncode", "ErrCursorMalformed", "errCursorMalformed",
				"errCursorMalformed.Error", "CursorDecode",
			},
			"aliases.go": {
				"MaxThesaurusBytes", "AliasEntry", "AliasMap", "AliasMap.Entries",
				"AliasMap.MergedKeys", "AliasMap.DigestHex", "AliasMap.Get", "lowerASCII",
				"normalizeAliasKey", "mergeAliasEntries", "LoadAliasMapFromBytes",
				"parseAliasEntries", "LoadAliasMapFile", "aliasDigest",
			},
		},
	},
	{
		num:  8,
		name: "Search",
		files: map[string][]string{
			"search.go": {
				"DefaultLanes", "DefaultResultsLimit", "MinNeedleLen",
				"CreditMode", "CreditSubstring", "CreditWordStart", "CreditWholeWord", "Knobs",
				"DefaultKnobs", "SearchRequest", "singularVariants", "Hit", "SearchOutput",
				"dbErrorName", "outcomeForError", "Search", "searchInner", "readContained", "stringSet",
				"newStringSet", "stringSet.has", "stringSet.add", "trailingZeros",
				"CreditLine", "indexIgnoreCase", "lanesTag", "snippet", "snippetSpec",
				"renderSnippet", "satSub", "capAtCodepoint", "GetRequest", "GetOutput", "Get",
				"getInner", "validEpisodeID",
			},
		},
	},
	{
		num:  9,
		name: "Operations",
		files: map[string][]string{
			"ops.go": {
				"Status", "Status.Healthy", "StatusOf", "ErrSharedDirectory", "ErrRootMissing",
				"ErrIndexUnavailable", "ErrSyncFailed", "Sync", "CountEpisodes", "Catalog",
				"ResealReport", "Reseal", "rewriteDigestLine", "resealWrite",
			},
			"ops_alias.go": {
				"ErrAliasInvalidTerm", "ErrAliasInvalidValue", "ErrAliasMalformed",
				"ErrAliasNotFound", "ErrAliasUnavailable", "AddAlias", "AliasRemoved",
				"RemovedEntry", "RemovedValue", "RemoveAlias", "validAliasKey",
				"validAliasValue", "canonicalizeAliasKeys", "readEditableThesaurus", "writeThesaurusAtomic",
				"MissRecord", "AppendMiss", "MissCandidate", "AggregateMisses", "LogSearchMiss",
			},
		},
	},
	{
		num:  10,
		name: "CLI wiring",
		files: map[string][]string{
			"cmd/autojournal/main.go": {
				"usageText", "exitOK", "exitFailure", "exitMalformed", "exitConflict", "opts",
				"cli", "main", "clockFromEnv", "cli.run", "openIndex", "parseLanes", "cli.searchCommand",
				"lineSpan", "parseLineSpan", "cli.getCommand", "cli.aliasCommand",
				"cli.captureCommand", "cli.statusCommand", "cli.catalogCommand",
				"cli.defaultCommand", "cli.syncCommand", "cli.resealCommand", "cli.fail", "readLimited",
			},
			"cmd/autojournal/report.go": {
				"outcomeExit", "hitJSON", "searchIdentitiesJSON", "searchIndexJSON",
				"searchReportJSON", "cli.renderSearchJSON", "cli.renderSearchText",
				"matchLine", "getReportJSON", "aliasEntryJSON", "captureReportJSON",
				"captureOutcomeExit", "cli.reportCapture", "cli.renderCapture", "syncReportJSON", "resealReportJSON", "statusIndexJSON",
				"statusReportJSON", "catalogPairJSON", "catalogReportJSON", "aliasListReportJSON",
				"defaultSetReportJSON", "defaultShowReportJSON", "cli.printJSON", "cli.emitJSON", "optString", "nonNil",
			},
		},
	},
}

// declaredSymbols parses every non-test .go file under src/ (this package's
// directory and cmd/autojournal) and returns symbol -> declaring file, with
// methods keyed Receiver.Method.
func declaredSymbols(t *testing.T) map[string]string {
	t.Helper()
	decls := map[string]string{}
	fset := token.NewFileSet()
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		af, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(path)
		// Symbols are package-scoped: the same name may exist in this
		// package and in cmd/autojournal, so keys carry the directory.
		scope := filepath.ToSlash(filepath.Dir(path)) + ":"
		for _, d := range af.Decls {
			switch dd := d.(type) {
			case *ast.FuncDecl:
				name := dd.Name.Name
				if dd.Recv != nil && len(dd.Recv.List) > 0 {
					rt := dd.Recv.List[0].Type
					if star, ok := rt.(*ast.StarExpr); ok {
						rt = star.X
					}
					if id, ok := rt.(*ast.Ident); ok {
						name = id.Name + "." + name
					}
				}
				decls[scope+name] = rel
			case *ast.GenDecl:
				for _, spec := range dd.Specs {
					switch sp := spec.(type) {
					case *ast.TypeSpec:
						decls[scope+sp.Name.Name] = rel
					case *ast.ValueSpec:
						for _, n := range sp.Names {
							if n.Name != "_" {
								decls[scope+n.Name] = rel
							}
						}
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("parse src: %v", err)
	}
	if len(decls) == 0 {
		t.Fatal("no declarations found; wrong working directory?")
	}
	return decls
}

// manifestClaims flattens the manifest into symbol -> claiming file.
func manifestClaims(t *testing.T) map[string]string {
	t.Helper()
	claims := map[string]string{}
	for _, c := range capabilityManifest {
		for file, syms := range c.files {
			scope := filepath.ToSlash(filepath.Dir(file)) + ":"
			for _, sym := range syms {
				if prev, dup := claims[scope+sym]; dup {
					t.Fatalf("manifest claims %s in both %s and %s", sym, prev, file)
				}
				claims[scope+sym] = file
			}
		}
	}
	return claims
}

// TestCapabilityOwnership fails on a symbol declared in a different file
// than the manifest assigns it, naming both files, and on a manifest entry
// the tree no longer declares.
func TestCapabilityOwnership(t *testing.T) {
	decls := declaredSymbols(t)
	claims := manifestClaims(t)
	for sym, declFile := range decls {
		claimFile, ok := claims[sym]
		if !ok {
			continue // TestCapabilityOwnershipHasNoOrphans owns this failure
		}
		if claimFile != declFile {
			t.Errorf("%s is declared in %s but the manifest places it in %s", sym, declFile, claimFile)
		}
	}
	for sym, claimFile := range claims {
		if _, ok := decls[sym]; !ok {
			t.Errorf("manifest claims %s in %s but the tree does not declare it", sym, claimFile)
		}
	}
}

// TestCapabilityOwnershipHasNoOrphans fails on any top-level declaration no
// capability claims, so a new file or symbol cannot appear silently.
func TestCapabilityOwnershipHasNoOrphans(t *testing.T) {
	decls := declaredSymbols(t)
	claims := manifestClaims(t)
	for sym, declFile := range decls {
		if _, ok := claims[sym]; !ok {
			t.Errorf("%s (declared in %s) is claimed by no capability", sym, declFile)
		}
	}
}

// TestCapabilityManifestMatchesDesignStructure asserts every capability
// number in the manifest appears in DESIGN.md's Structure section with the
// same name, so the manifest and the document cannot drift apart silently.
func TestCapabilityManifestMatchesDesignStructure(t *testing.T) {
	b, err := os.ReadFile("../DESIGN.md")
	if err != nil {
		t.Fatalf("read DESIGN.md: %v", err)
	}
	text := string(b)
	idx := strings.Index(text, "\n## Structure")
	if idx < 0 {
		t.Fatal("DESIGN.md has no Structure section")
	}
	section := text[idx:]
	if end := strings.Index(section[1:], "\n## "); end >= 0 {
		section = section[:end+1]
	}
	for _, c := range capabilityManifest {
		want := fmt.Sprintf("%d. **%s**", c.num, c.name)
		if !strings.Contains(section, want) {
			t.Errorf("Structure section does not list %q", want)
		}
	}
}
