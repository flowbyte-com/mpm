package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"github.com/flowbyte-com/mpm-core/usererror"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"

	_ "github.com/mattn/go-sqlite3"
)

// Pre-compiled regexes for stripMarkdown (avoid repeated recompilation)
var (
	stripMarkdownHeadings = regexp.MustCompile(`(?m)^#+\s*`)
	stripMarkdownBold     = regexp.MustCompile(`\*\*(.*?)\*\*`)
	stripMarkdownItalic   = regexp.MustCompile(`\*(.*?)\*`)
	stripMarkdownCode     = regexp.MustCompile("`([^`]+)`")
)

// lastQuery is the most recent query passed to handleRecall. Captured so
// the --why provenance helper can extract matched FTS5 terms without
// re-running the search. Cleared at the start of each handleRecall call.
var lastQuery string

// recallEntry is the in-memory shape used to render each memory in the
// recall result list. Promoted to package scope so the --why provenance
// helper (printProvenance) can take a value rather than reach into
// handleRecall's local scope.
type recallEntry struct {
	id                 string
	content            string
	metadata           string
	sessionID          string
	createdAt          int64
	tags               string
	synthesized        bool
	reinforcementCount int
	weight             int
	lastAccessedAt     *int64
	referenceID        string
}

var _ = recallEntry{} // ensure the type is "used" even if --why is off

// =============================================================================
// mpm recall <query> — Search memories for relevant context
// =============================================================================

func handleRecall(args []string) int {
	lastQuery = "" // reset; the search code sets this once the query is known
	fs := flag.NewFlagSet("recall", flag.ContinueOnError)
	since := fs.String("since", "", "Search memories since date (YYYY-MM-DD)")
	until := fs.String("until", "", "Search memories until date (YYYY-MM-DD)")
	limit := fs.Int("limit", 15, "Maximum results to return")
	staleDays := fs.Int("stale-days", 14, "Days threshold for stale flag (0=disabled)")
	jsonOutput := fs.Bool("json", false, "Output JSON for tool integration")
	collection := fs.String("collection", "memories", "Collection to search")
	tokenBudget := fs.Int("token-budget", 0, "Max cumulative tokens before truncation (0=unlimited)")
	weightBelow := fs.Int("weight-below", 0, "Only results with weight below this value (0=no filter)")
	before := fs.String("before", "", "Only results created before this date (YYYY-MM-DD)")
	semantic := fs.Bool("semantic", false, "Use hybrid semantic search (FTS5 + embeddings)")
	vectorWeight := fs.Float64("vector-weight", 0.5, "Vector weight in hybrid search (0=FTS5-only, 1=vector-only)")
	asOf := fs.String("as-of", "", "Point-in-time reconstruction: retrieve memory state as of this timestamp (unix-epoch seconds or RFC3339)")
	why := fs.Bool("why", false, "Show why each memory was retrieved (provenance: score factors, FTS terms, source engine)")
	fs.Usage = func() {
		fmt.Println("Usage: mpm recall [options] <query>")
		fmt.Println("\nRecall options:")
		fs.PrintDefaults()
	}

	// Go's flag.Parse stops at the first non-flag positional arg.
	// Pre-scan for --json, --stale-days, and --token-budget since callers
	// may place these flags after the query.
	preprocessed := make([]string, 0, len(args))
	jsonFlagSeen := false
	tokenBudgetVal := 0
	weightBelowVal := 0
	beforeVal := ""

	// Collect indices to remove
	removeIdxs := make(map[int]bool)

	for i, arg := range args[1:] {
		realIdx := i + 1 // account for args[0] being the command name
		if arg == "--json" || arg == "-j" {
			jsonFlagSeen = true
			removeIdxs[realIdx] = true
			continue
		}
		if arg == "--stale-days" {
			removeIdxs[realIdx] = true
			if realIdx+1 < len(args) {
				removeIdxs[realIdx+1] = true // consume its value
			}
			continue
		}
		if strings.HasPrefix(arg, "--stale-days=") {
			removeIdxs[realIdx] = true
			continue
		}
		if arg == "--token-budget" {
			removeIdxs[realIdx] = true
			if realIdx+1 < len(args) {
				removeIdxs[realIdx+1] = true
				if v, err := strconv.Atoi(args[realIdx+1]); err == nil && v > 0 {
					tokenBudgetVal = v
				}
			}
			continue
		}
		if strings.HasPrefix(arg, "--token-budget=") {
			removeIdxs[realIdx] = true
			if v, err := strconv.Atoi(strings.TrimPrefix(arg, "--token-budget=")); err == nil && v > 0 {
				tokenBudgetVal = v
			}
			continue
		}
		if arg == "--weight-below" {
			removeIdxs[realIdx] = true
			if realIdx+1 < len(args) {
				removeIdxs[realIdx+1] = true
				if v, err := strconv.Atoi(args[realIdx+1]); err == nil && v > 0 {
					weightBelowVal = v
				}
			}
			continue
		}
		if strings.HasPrefix(arg, "--weight-below=") {
			removeIdxs[realIdx] = true
			if v, err := strconv.Atoi(strings.TrimPrefix(arg, "--weight-below=")); err == nil && v > 0 {
				weightBelowVal = v
			}
			continue
		}
		if arg == "--before" {
			removeIdxs[realIdx] = true
			if realIdx+1 < len(args) {
				removeIdxs[realIdx+1] = true
				beforeVal = args[realIdx+1]
			}
			continue
		}
		if strings.HasPrefix(arg, "--before=") {
			removeIdxs[realIdx] = true
			beforeVal = strings.TrimPrefix(arg, "--before=")
			continue
		}
	}

	for i, arg := range args {
		if !removeIdxs[i] {
			preprocessed = append(preprocessed, arg)
		}
	}

	if err := fs.Parse(preprocessed[1:]); err != nil {
		return 1
	}

	// Apply pre-scanned flags
	if jsonFlagSeen {
		*jsonOutput = true
	}
	if tokenBudgetVal > 0 {
		*tokenBudget = tokenBudgetVal
	}
	if weightBelowVal > 0 {
		*weightBelow = weightBelowVal
	}
	if beforeVal != "" {
		*before = beforeVal
	}

	hasFilter := *weightBelow > 0 || *before != "" || *since != "" || *until != "" || *collection != "memories"
	query := fs.Arg(0)
	if query == "" && !hasFilter {
		usererror.Usage("mpm recall [options] <query>")
		return 1
	}
	query = strings.TrimSpace(query)
	lastQuery = query // captured for the --why provenance helper

	dm := getDBConcrete()
	if dm == nil {
		return 1
	}

	db := dm.SQLDB()
	returnedIDs := make(map[string]bool)

	// Hybrid semantic search: FTS5 + vector embeddings blended
	if *semantic {
		// UX guard: --semantic needs an embedding provider. Without one, the
		// probe falls back to NullProvider and HybridSearch degrades to a
		// noisy FTS5-only result that misleads the user. Fail clean instead.
		embedCfg := mpminternal.DefaultEmbeddingConfig()
		if embedCfg.ProviderName == "null" {
			return usererror.Error("No embedding provider configured for semantic search.\n" +
				"       Configure one with `mpm config profile set` (api_key + base_url),\n" +
				"       set OLLAMA_ENDPOINT, or omit --semantic for standard lexical recall.")
		}
		cfg := mpminternal.DefaultHybridConfig()
		cfg.Limit = *limit
		cfg.VectorWeight = *vectorWeight
		hybridResults, err := mpminternal.HybridSearch(dm, query, *collection, cfg)
		if err != nil {
			return usererror.Error("Semantic search failed: %v", err)
		}
		if len(hybridResults) == 0 {
			fmt.Printf("No memories found for: %s\n", query)
			return 0
		}
		// Render hybrid results as recall entries and output
		return renderHybridResults(hybridResults, query, *jsonOutput, *tokenBudget, *staleDays, dm)
	}

	// Keyword search using LIKE + FTS5 fallback with time and weight filters
	rows, err := keywordSearchWithTime(db, query, *collection, *since, *until, *weightBelow, *before, *limit)
	if err != nil {
		usererror.Error("Search failed: %v", err)
	}
	defer rows.Close()

	var entries []recallEntry
	scanErr := func() error {
		for rows.Next() {
			var id, content string
			var createdAt int64
			var nullableSessionID, nullableTags sql.NullString
			var reinforcementCount, weight float64
			var nullableLastAccessed, nullableRefID, nullableMetadata sql.NullString
			if err := rows.Scan(&id, &content, &nullableSessionID, &nullableTags, &nullableMetadata, &createdAt,
				&reinforcementCount, &weight, &nullableLastAccessed, &nullableRefID); err != nil {
				return fmt.Errorf("scanning recall entry row: %w", err)
			}
			if content == "" {
				continue
			}
			sessionID := ""
			if nullableSessionID.Valid {
				sessionID = nullableSessionID.String
			}
			refID := ""
			if nullableRefID.Valid {
				refID = nullableRefID.String
			}
		entry := recallEntry{
			id:                 id,
			content:            content,
			metadata:           nullableMetadata.String,
			sessionID:          sessionID,
			tags:               nullableTags.String,
			createdAt:          createdAt,
			reinforcementCount: int(reinforcementCount),
			weight:             int(weight + 0.5), // round to nearest int for display
			referenceID:        refID,
		}
		if nullableLastAccessed.Valid && nullableLastAccessed.String != "" {
			if v, err := strconv.ParseInt(nullableLastAccessed.String, 10, 64); err == nil {
				entry.lastAccessedAt = &v
			}
		}
		if strings.Contains(nullableTags.String, "synthesized") {
			entry.synthesized = true
		}
		returnedIDs[id] = true
		entries = append(entries, entry)
		}
		return nil
	}()
	if scanErr != nil {
		usererror.Warn("handleRecall: %v", scanErr)
		return 1
	}

	// Sort by recency — must happen before empty check for JSON mode
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].createdAt > entries[j].createdAt
	})

	// ── Point-in-Time Reconstruction (--as-of) ─────────────────────────
	//
	// NOTE: FTS5/keyword search operates on the *current* indexed text only.
	// When --as-of is active, results are filtered and their content substituted
	// from memory_revisions after the initial search pass. This means:
	//   - A memory whose current text no longer matches the query may still be
	//     returned if it matched at the --as-of timestamp (the FTS match is
	//     against current indexed content; reconstruction happens post-selection).
	//   - A memory that was created after --as-of is silently dropped.
	//   - The reconstructed content reflects the historical text, not current.
	var timeTravelVersionMap map[string]int
	if *asOf != "" {
		asOfUnix, err := mpminternal.ParseTimestampArg(*asOf)
		if err != nil {
			usererror.Error("--as-of must be a unix-epoch integer or RFC3339 timestamp, got %q", *asOf)
		}
		asOfTime := time.Unix(asOfUnix, 0)

		timeTravelVersionMap = make(map[string]int, len(entries))
		var filtered []recallEntry
		for _, e := range entries {
			rev, err := dm.GetMemoryRevisionAtTime(e.id, asOfTime)
			if err != nil || rev == nil {
				continue // memory didn't exist yet at that time — drop silently
			}
			e.content = rev.Content
			e.weight = rev.Weight
			timeTravelVersionMap[e.id] = rev.Version
			filtered = append(filtered, e)
		}
		entries = filtered
	}

	// JSON output for tool integration — even with zero results, return valid JSON
	if *jsonOutput {
		type memoryEntry struct {
			ID                   string  `json:"id"`
			Content              string  `json:"content"`
			Tags                 string  `json:"tags"`
			SessionID            string  `json:"session_id,omitempty"`
			CreatedAt            string  `json:"created_at"`
			ReinforcementCount   int     `json:"reinforcement_count"`
			Weight               int     `json:"weight"`
			LastAccessedAt       string  `json:"last_accessed_at,omitempty"`
			Score                float64 `json:"score"`
			Rationale            string  `json:"rationale"`
			IsStale              bool    `json:"is_stale"`
			ReconstructedVersion *int    `json:"reconstructed_version,omitempty"`
			CrossReferences      struct {
				Topics       []mpminternal.TopicRef       `json:"topics"`
				ReferenceDoc *mpminternal.ReferenceDocRef `json:"reference_doc"`
			} `json:"cross_references"`
		}
		result := make([]memoryEntry, 0, len(entries))
		accumulatedTokens := 0
		budget := *tokenBudget
		truncated := false
		for _, e := range entries {
			lastAccessStr := mpminternal.FormatOptionalUnixSeconds(e.lastAccessedAt)
			score := computeScore(e.reinforcementCount, e.weight)
			rationale := formatRationale(e.reinforcementCount, e.weight, e.lastAccessedAt)

			isStale := isMemoryStale(e.createdAt, e.lastAccessedAt, *staleDays)

			// Fetch cross-references
			topics, _ := dm.GetMemoryTopics(e.id)
			var refDoc *mpminternal.ReferenceDocRef
			if e.referenceID != "" {
				refDoc, _ = dm.GetReferenceDoc(e.referenceID)
			}

			crossRefs := struct {
				Topics       []mpminternal.TopicRef       `json:"topics"`
				ReferenceDoc *mpminternal.ReferenceDocRef `json:"reference_doc"`
			}{
				Topics:       topics,
				ReferenceDoc: refDoc,
			}

			// Token budget check
			if budget > 0 {
				tokens, tokErr := mpminternal.CountTokens(e.content)
				if tokErr == nil {
					if accumulatedTokens+tokens > budget {
						truncated = true
						break
					}
					accumulatedTokens += tokens
				}
			}

			var recVer *int
			if timeTravelVersionMap != nil {
				if v, ok := timeTravelVersionMap[e.id]; ok {
					recVer = &v
				}
			}

			jsonContent := e.content
			if preamble := mpminternal.ProvenancePreamble(e.metadata); preamble != "" {
				jsonContent = preamble + "\n" + jsonContent
			}

			result = append(result, memoryEntry{
				ID:                   shortID(e.id),
				Content:              jsonContent,
				Tags:                 e.tags,
				SessionID:            e.sessionID,
				CreatedAt:            mpminternal.FormatUnixSeconds(e.createdAt),
				ReinforcementCount:   e.reinforcementCount,
				Weight:               e.weight,
				LastAccessedAt:       lastAccessStr,
				Score:                score,
				Rationale:            rationale,
				IsStale:              isStale,
				ReconstructedVersion: recVer,
				CrossReferences:      crossRefs,
			})
		}
		output := map[string]interface{}{
			"query":    query,
			"memories": result,
		}
		if *asOf != "" {
			output["reconstructed_as_of"] = *asOf
		}
		if truncated {
			output["truncated"] = true
			output["truncated_notice"] = fmt.Sprintf("[Truncated: token budget of %d reached — showing %d of %d results]", budget, len(result), len(entries))
		}
		data, _ := json.Marshal(output)
		fmt.Println(string(data))
		return 0
	}

	if len(entries) == 0 {
		fmt.Printf("No memories found for: %s\n", query)
		return 0
	}

	cyan := "\033[36m"
	magenta := "\033[35m"
	yellow := "\033[33m"
	reset := "\033[0m"
	bold := "\033[1m"

	header := fmt.Sprintf("%s%sRecall — %s%s", bold, cyan, query, reset)
	if *asOf != "" {
		header = fmt.Sprintf("%s%sReconstructed Past State as of %s%s", bold, yellow, *asOf, reset)
	}
	fmt.Printf("%s\n\n", header)

	dim := "\033[2m"
	accumulatedTokens := 0
	budget := *tokenBudget
	humanTruncated := false
	truncateIdx := len(entries)
	for i, e := range entries {
		if budget > 0 {
			tokens, tokErr := mpminternal.CountTokens(e.content)
			if tokErr == nil {
				if accumulatedTokens+tokens > budget {
					humanTruncated = true
					truncateIdx = i
					break
				}
				accumulatedTokens += tokens
			}
		}

		content := e.content
		if preamble := mpminternal.ProvenancePreamble(e.metadata); preamble != "" {
			content = preamble + "\n" + content
		}
		if strings.Contains(e.metadata, `"status":"challenged"`) {
			content = "[Note: This memory is challenged — treat as unverified]\n" + content
		}
		if len(content) > 250 {
			content = content[:250] + "..."
		}
		content = stripMarkdown(content)

		// Build chip list
		chips := []string{}

		// Reinforcement count
		if e.reinforcementCount > 0 {
			chips = append(chips, fmt.Sprintf("%dx ref", e.reinforcementCount))
		}

		// Weight (skip if ≤ 1 since that's default)
		if e.weight > 1 {
			chips = append(chips, fmt.Sprintf("weight %d", e.weight))
		}

		// Stale indicator
		isStale := isMemoryStale(e.createdAt, e.lastAccessedAt, *staleDays)
		if isStale {
			chips = append(chips, fmt.Sprintf("%s⚠️ STALE%s", yellow, reset))
		}

		// LTM flag
		ltmTag := ""
		if e.weight >= 10 {
			ltmTag = fmt.Sprintf(" %sLTM%s", magenta, reset)
		}

		// Access age (use lastAccessedAt, not createdAt)
		accessAge := ""
		if e.lastAccessedAt != nil {
			accessAge = formatAgeUnix(*e.lastAccessedAt)
		}

		// ID chip
		shortID := shortID(e.id)
		idChip := fmt.Sprintf("%s[%s]%s", dim, shortID, reset)

		// Assemble chip line
		chipParts := []string{idChip}
		for _, c := range chips {
			chipParts = append(chipParts, fmt.Sprintf("%s%s%s", magenta, c, reset))
		}
		if accessAge != "" {
			chipParts = append(chipParts, fmt.Sprintf("%s%s%s", magenta, accessAge, reset))
		}

		chipsLine := strings.Join(chipParts, " · ")
		if ltmTag != "" {
			chipsLine += ltmTag
		}

		synthTag := ""
		if e.synthesized {
			synthTag = fmt.Sprintf(" %s[synth]%s", magenta, reset)
		}

		versionNote := ""
		if timeTravelVersionMap != nil {
			if v, ok := timeTravelVersionMap[e.id]; ok {
				versionNote = fmt.Sprintf(" %s[Reconstructed Past State: Version %d as of %s]%s", yellow, v, *asOf, reset)
			}
		}

		fmt.Printf("%s%d.%s %s%s%s%s\n    %s\n\n",
			cyan, i+1, reset,
			chipsLine, synthTag,
			versionNote, reset,
			content)

		// --why provenance: show the score breakdown for this memory.
		// Useful for "why did the agent retrieve this?" introspection.
		if *why {
			printProvenance(os.Stdout, e, content)
		}

		// Fetch and display cross-references
		topics, _ := dm.GetMemoryTopics(e.id)
		var refTitle string
		if e.referenceID != "" {
			if refDoc, err := dm.GetReferenceDoc(e.referenceID); err == nil && refDoc != nil {
				refTitle = refDoc.Title
			}
		}

		if len(topics) > 0 || refTitle != "" {
			topicChips := []string{}
			for _, t := range topics {
				topicChips = append(topicChips, fmt.Sprintf("%s[%s]%s", magenta, t.Name, reset))
			}

			fmt.Printf("\n%s─ Related ────────────────────────────────%s\n", bold, reset)
			if len(topics) > 0 {
				fmt.Printf("  Topics: %s\n", strings.Join(topicChips, " "))
			}
			if refTitle != "" {
				fmt.Printf("  Ref:    %s\n", refTitle)
			}
			fmt.Printf("%s─────────────────────────────────────────%s\n", bold, reset)
		}
	}

	shown := len(entries)
	if humanTruncated {
		shown = truncateIdx
		fmt.Printf("%s%s[Truncated: token budget of %d reached — showing %d of %d results]%s\n", yellow, bold, budget, shown, len(entries), reset)
	}
	fmt.Printf("%s%d results%s\n", cyan, shown, reset)

	// Implicit reinforcement: bump weight +0.5 for returned memories (capped +1/hr)
	// Only reinforce memories with meaningful weight (>=1) to avoid artificially boosting
	// casual mentions or memories that have decayed to near-zero weight
	if len(returnedIDs) > 0 {
		ids := make([]string, 0, len(returnedIDs))
		vals := make([]interface{}, 0, len(returnedIDs))
		for id := range returnedIDs {
			ids = append(ids, "?")
			vals = append(vals, id)
		}
		if _, err := dm.SQLDB().Exec(fmt.Sprintf(`
			UPDATE memories
			SET weight = MIN(weight + 0.5, 100.0),
			    last_accessed_at = CAST(strftime('%%s','now') AS INTEGER)
			WHERE id IN (%s)
			  AND weight >= 1
			  AND (last_accessed_at IS NULL OR last_accessed_at < CAST(strftime('%%s','now', '-1 hour') AS INTEGER))
		`, strings.Join(ids, ",")), vals...); err != nil {
			slog.Warn("implicit reinforcement failed", "error", err)
		}
	}

	return 0
}

func keywordSearchWithTime(db *sql.DB, query, collection, since, until string, weightBelow int, before string, limit int) (*sql.Rows, error) {
	if collection == "" {
		collection = "memories"
	}

	var args []interface{}
	useFTS := false

	ftsQuery := `
		SELECT m.id, m.content, m.session_id, m.tags, m.metadata, m.created_at,
		       COALESCE(m.reinforcement_count, 0) as reinforcement_count,
		       COALESCE(m.weight, 1) as weight,
		       m.last_accessed_at,
		       m.reference_id
		FROM memories m
		JOIN memories_fts fts ON m.rowid = fts.rowid
		WHERE m.deleted_at IS NULL`

	if query != "" {
		ftsQuery += " AND memories_fts MATCH ?"
		args = append(args, query)
		useFTS = true
	} else {
		ftsQuery += " AND memories_fts MATCH '*'"
	}

	ftsQuery += " AND m.collection = ?"
	args = append(args, collection)

	if since != "" {
		ftsQuery += " AND m.created_at >= ?"
		args = append(args, since)
	}
	if until != "" {
		ftsQuery += " AND m.created_at <= ?"
		args = append(args, until+" 23:59:59")
	}
	if before != "" {
		ftsQuery += " AND m.created_at < ?"
		args = append(args, before)
	}
	if weightBelow > 0 {
		ftsQuery += " AND m.weight < ?"
		args = append(args, weightBelow)
	}

	ftsQuery += " ORDER BY fts.rank LIMIT ?"
	args = append(args, limit)

	rows, err := db.Query(ftsQuery, args...)
	if err == nil && useFTS {
		return rows, nil
	}
	if err == nil && !useFTS {
		// FTS5 MATCH '*' succeeded — return results
		return rows, nil
	}

	// FTS5 failed (malformed query, MATCH '*' not supported, or unavailable)
	// Fallback to LIKE — only if there's a text query to match
	if query == "" {
		return rows, err // return the FTS5 error or empty results
	}

	likePattern := "%" + query + "%"
	likeQuery := `
		SELECT id, content, session_id, tags, created_at,
		       COALESCE(reinforcement_count, 0) as reinforcement_count,
		       COALESCE(weight, 1) as weight,
		       last_accessed_at,
		       reference_id
		FROM memories
		WHERE deleted_at IS NULL AND collection = ?
		  AND (content LIKE ? OR tags LIKE ?)`

	args = []interface{}{collection, likePattern, likePattern}

	if since != "" {
		likeQuery += " AND created_at >= ?"
		args = append(args, since)
	}
	if until != "" {
		likeQuery += " AND created_at <= ?"
		args = append(args, until+" 23:59:59")
	}
	if before != "" {
		likeQuery += " AND created_at < ?"
		args = append(args, before)
	}
	if weightBelow > 0 {
		likeQuery += " AND weight < ?"
		args = append(args, weightBelow)
	}

	likeQuery += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)

	return db.Query(likeQuery, args...)
}

func stripMarkdown(s string) string {
	s = stripMarkdownHeadings.ReplaceAllString(s, "")
	s = stripMarkdownBold.ReplaceAllString(s, "$1")
	s = stripMarkdownItalic.ReplaceAllString(s, "$1")
	s = stripMarkdownCode.ReplaceAllString(s, "$1")
	return strings.TrimSpace(s)
}

func formatAge(t time.Time) string {
	age := time.Since(t)
	switch {
	case age < time.Minute:
		return "just now"
	case age < time.Hour:
		return fmt.Sprintf("%dm ago", int(age.Minutes()))
	case age < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(age.Hours()))
	default:
		days := int(age.Hours() / 24)
		if days == 1 {
			return "yesterday"
		}
		return fmt.Sprintf("%dd ago", days)
	}
}

// formatAgeUnix renders an int64 Unix-epoch-seconds value as a relative
// age string ("just now" / "5m ago" / "3h ago" / "2d ago" / "yesterday").
// Used at the display boundary for migrated-timestamp fields whose callers
// no longer carry a time.Time. Zero values render as "(never)".
func formatAgeUnix(sec int64) string {
	if sec <= 0 {
		return "(never)"
	}
	return formatAge(time.Unix(sec, 0))
}

// formatExpiresInFromNowUnix renders the seconds-until-expiry for an int64
// expiry value. Mirrors formatExpiresInFromNow but operates on the int64
// field directly.
func formatExpiresInFromNowUnix(sec int64) string {
	if sec <= 0 {
		return "(none)"
	}
	d := time.Until(time.Unix(sec, 0))
	switch {
	case d <= 0:
		return "expired"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// computeScore returns a fractional score 0-1 based on reinforcement count and weight.
// Formula: clamp((rc * 2 + weight * 1.5) / 55, 0, 1)
func computeScore(rc, weight int) float64 {
	raw := float64(rc*2) + float64(weight*3)/2
	result := raw / 55.0
	if result > 1.0 {
		result = 1.0
	}
	if result < 0.0 {
		result = 0.0
	}
	return result
}

// formatRationale returns a one-line string describing why this memory matters.
func formatRationale(rc, weight int, lastAccessed *int64) string {
	parts := []string{}

	if rc > 0 {
		parts = append(parts, fmt.Sprintf("%dx ref", rc))
	}

	if weight > 1 {
		parts = append(parts, fmt.Sprintf("weight %d", weight))
	}

	if weight >= 10 {
		parts = append(parts, "LTM")
	}

	if lastAccessed != nil {
		parts = append(parts, "accessed "+formatAgeUnix(*lastAccessed))
	}

	return strings.Join(parts, " · ")
}

// isMemoryStale returns true if the memory has not been accessed within staleDays.
func isMemoryStale(createdAt int64, lastAccessed *int64, staleDays int) bool {
	if staleDays <= 0 {
		return false // feature disabled
	}
	threshold := time.Duration(staleDays) * 24 * time.Hour

	if lastAccessed != nil {
		return time.Since(time.Unix(*lastAccessed, 0)) > threshold
	}
	if createdAt > 0 {
		return time.Since(time.Unix(createdAt, 0)) > threshold
	}
	return false
}

// renderHybridResults outputs hybrid search results in the same format as keyword recall.
func renderHybridResults(results []mpminternal.HybridResult, query string, jsonOutput bool, tokenBudget, staleDays int, dm *mpminternal.DatabaseManager) int {
	if jsonOutput {
		type hybridEntry struct {
			ID                 string  `json:"id"`
			Content            string  `json:"content"`
			Tags               string  `json:"tags"`
			CreatedAt          string  `json:"created_at"`
			ReinforcementCount int     `json:"reinforcement_count"`
			Weight             float64 `json:"weight"`
			Score              float64 `json:"score"`
			Source             string  `json:"source"` // "fts5", "vector", "hybrid"
			FTS5Score          float64 `json:"fts5_score,omitempty"`
			VectorSimilarity   float64 `json:"vector_similarity,omitempty"`
			Rationale          string  `json:"rationale"`
			IsStale            bool    `json:"is_stale"`
		}
		entries := make([]hybridEntry, 0, len(results))
		for _, r := range results {
			entries = append(entries, hybridEntry{
				ID:                 shortID(r.ID),
				Content:            r.Content,
				Tags:               r.Tags,
				CreatedAt:          mpminternal.FormatUnixSeconds(r.CreatedAt),
				ReinforcementCount: r.ReinforcementCount,
				Weight:             r.Weight,
				Score:              r.CombinedScore,
				Source:             r.Source,
				FTS5Score:          r.FTS5Score,
				VectorSimilarity:   r.VectorSimilarity,
				Rationale:          fmt.Sprintf("hybrid fts5+vec weight=%.2f", r.CombinedScore),
				IsStale:            isMemoryStale(r.CreatedAt, nil, staleDays),
			})
		}
		data, _ := json.Marshal(map[string]interface{}{"query": query, "memories": entries, "search_mode": "hybrid"})
		fmt.Println(string(data))
		return 0
	}

	cyan := "\033[36m"
	reset := "\033[0m"
	bold := "\033[1m"
	fmt.Printf("%s%sHybrid Recall — %s%s\n\n", bold, cyan, query, reset)

	for i, r := range results {
		createdAt := time.Unix(r.CreatedAt, 0)
		score := r.CombinedScore
		source := r.Source
		content := r.Content
		if len(content) > 80 {
			content = content[:80] + "..."
		}
		fmt.Printf("%d. [%.2f] [%s] %s\n   %s\n\n", i+1, score, source, createdAt.Format("2006-01-02"), content)
	}
	return 0
}

// printProvenance prints the "why was this retrieved?" breakdown for a
// recall result. Shows the score components (reinforcement_count × 2 +
// weight × 1.5 + recency bonus) and any matched FTS5 terms. Designed to
// answer "why did the agent pick this memory?" without re-running the
// query against a debugger.
//
// The output is plain text with a dim prefix so it stays unobtrusive
// when stacked between result cards.
func printProvenance(w io.Writer, e recallEntry, content string) {
	// ANSI codes are hardcoded rather than reading the package-level
	// dim/reset vars (which are local to handleRecall). Keeps the helper
	// self-contained and easy to call from other handlers.
	const dim = "\033[2m"
	const reset = "\033[0m"

	// Score components per internal/hybrid_search.go.
	// score = (reinforcement_count × 2) + (weight × 1.5) + recency_bonus
	reinforcement := float64(e.reinforcementCount) * 2
	weightContrib := float64(e.weight) * 1.5
	// Recency bonus is harder to reverse-engineer; show the access age
	// instead and let the operator intuit it.
	recency := ""
	if e.lastAccessedAt != nil {
		recency = formatAgeUnix(*e.lastAccessedAt) + " ago"
	} else {
		recency = formatAgeUnix(e.createdAt) + " old"
	}

	fmt.Fprintf(w, "    %s[why]%s reinforcement=%d (×2 = %.1f)  weight=%d (×1.5 = %.1f)  recency=%s\n",
		dim, reset,
		e.reinforcementCount, reinforcement,
		e.weight, weightContrib,
		recency,
	)

	// FTS5 hit terms: extract quoted query terms from the content. This
	// is a heuristic — for proper FTS5 term highlighting we'd query
	// the snippet() function, but that requires re-running the search.
	// The heuristic is "good enough" for the agent's introspection.
	if hits := ftsHitTerms(content, lastQuery); len(hits) > 0 {
		fmt.Fprintf(w, "    %s[why]%s matched terms: %s\n", dim, reset, strings.Join(hits, ", "))
	}

	if e.synthesized {
		fmt.Fprintf(w, "    %s[why]%s synthesized from prior memories\n", dim, reset)
	}
}

// ftsHitTerms extracts likely FTS5 hit terms by lowercasing both the
// content and the query, then returning query words that appear as
// substrings in the content. Stopwords ("the", "a", "of", ...) are
// filtered. This is a heuristic, not a real FTS5 parser — it gives
// the operator a starting point for "why did this match?" without
// re-running the search.
func ftsHitTerms(content, query string) []string {
	if query == "" {
		return nil
	}
	stop := map[string]bool{
		"a": true, "an": true, "and": true, "are": true, "as": true,
		"at": true, "be": true, "by": true, "for": true, "from": true,
		"has": true, "have": true, "in": true, "is": true, "it": true,
		"of": true, "on": true, "or": true, "that": true, "the": true,
		"to": true, "was": true, "were": true, "will": true, "with": true,
	}
	lowerContent := strings.ToLower(content)
	var hits []string
	seen := map[string]bool{}
	for _, word := range strings.Fields(strings.ToLower(query)) {
		word = strings.TrimFunc(word, func(r rune) bool {
			return r == '"' || r == '*' || r == '(' || r == ')' || r == ','
		})
		if len(word) < 3 || stop[word] || seen[word] {
			continue
		}
		if strings.Contains(lowerContent, word) {
			hits = append(hits, word)
			seen[word] = true
		}
	}
	return hits
}
