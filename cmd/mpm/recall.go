package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	mpminternal "mpm/internal"
)

// Pre-compiled regexes for stripMarkdown (avoid repeated recompilation)
var (
	stripMarkdownHeadings = regexp.MustCompile(`(?m)^#+\s*`)
	stripMarkdownBold     = regexp.MustCompile(`\*\*(.*?)\*\*`)
	stripMarkdownItalic   = regexp.MustCompile(`\*(.*?)\*`)
	stripMarkdownCode     = regexp.MustCompile("`([^`]+)`")
)

// =============================================================================
// mpm recall <query> — Search memories for relevant context
// =============================================================================

func handleRecall(args []string) int {
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
	asOf := fs.String("as-of", "", "Point-in-time reconstruction: retrieve memory state as of this timestamp (RFC3339)")
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
		fmt.Fprintf(os.Stderr, "Usage: mpm recall [options] <query>\n")
		return 1
	}
	query = strings.TrimSpace(query)

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ DB open failed: %v\n", err)
		return 1
	}
	defer dm.Close()

	db := dm.SQLDB()
	returnedIDs := make(map[string]bool)

	// Hybrid semantic search: FTS5 + vector embeddings blended
	if *semantic {
		cfg := mpminternal.DefaultHybridConfig()
		cfg.Limit = *limit
		cfg.VectorWeight = *vectorWeight
		hybridResults, err := mpminternal.HybridSearch(dm, query, *collection, cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Semantic search failed: %v\n", err)
			return 1
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
		fmt.Fprintf(os.Stderr, "❌ Search failed: %v\n", err)
		return 1
	}
	defer rows.Close()

	type recallEntry struct {
		id                 string
		content            string
		metadata           string
		sessionID          string
		createdAt          time.Time
		tags               string
		synthesized        bool
		reinforcementCount int
		weight             int
		lastAccessedAt     time.Time
		referenceID        string
	}
	var entries []recallEntry
	for rows.Next() {
		var id, content, createdAt string
		var nullableSessionID, nullableTags sql.NullString
		var reinforcementCount, weight int64
		var nullableLastAccessed, nullableRefID, nullableMetadata sql.NullString

		if err := rows.Scan(&id, &content, &nullableSessionID, &nullableTags, &nullableMetadata, &createdAt,
			&reinforcementCount, &weight, &nullableLastAccessed, &nullableRefID); err != nil {
			continue
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
			reinforcementCount: int(reinforcementCount),
			weight:             int(weight),
			referenceID:        refID,
		}
		if t, err := time.Parse(time.RFC3339, createdAt); err == nil {
			entry.createdAt = t
		}
		if nullableLastAccessed.Valid && nullableLastAccessed.String != "" {
			if t, err := time.Parse(time.RFC3339, nullableLastAccessed.String); err == nil {
				entry.lastAccessedAt = t
			}
		}
		if strings.Contains(nullableTags.String, "synthesized") {
			entry.synthesized = true
		}
		returnedIDs[id] = true
		entries = append(entries, entry)
	}

	// Sort by recency — must happen before empty check for JSON mode
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].createdAt.After(entries[j].createdAt)
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
		asOfTime, err := time.Parse(time.RFC3339, *asOf)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: --as-of must be an RFC3339 timestamp, got %q\n", *asOf)
			return 1
		}

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
			lastAccessStr := ""
			if !e.lastAccessedAt.IsZero() {
				lastAccessStr = e.lastAccessedAt.Format(time.RFC3339)
			}
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
				CreatedAt:            e.createdAt.Format(time.RFC3339),
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
		if !e.lastAccessedAt.IsZero() {
			accessAge = formatAge(e.lastAccessedAt)
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
			    last_accessed_at = CURRENT_TIMESTAMP
			WHERE id IN (%s)
			  AND weight >= 1
			  AND (last_accessed_at IS NULL OR last_accessed_at < datetime('now', '-1 hour'))
		`, strings.Join(ids, ",")), vals...); err != nil {
			slog.Warn("implicit reinforcement failed", "error", err)
		}
	}

	return 0
}

func keywordSearch(db *sql.DB, query string, limit int) (*sql.Rows, error) {
	// Try FTS5 first — use table-level MATCH with JOIN pattern (same as QueryMemory)
	ftsQuery := `
		SELECT m.id, m.content, m.session_id, m.tags, m.created_at
		FROM memories m
		JOIN memories_fts fts ON m.rowid = fts.rowid
		WHERE memories_fts MATCH ? AND m.deleted_at IS NULL
		ORDER BY fts.rank
		LIMIT ?`

	rows, err := db.Query(ftsQuery, query, limit)
	if err == nil {
		return rows, nil
	}

	// FTS5 failed (malformed query or unavailable) — fallback to LIKE search
	fmt.Fprintf(os.Stderr, "⚠️ FTS5 query failed (query=%q): %v — falling back to LIKE\n", query, err)
	likePattern := "%" + query + "%"
	likeQuery := `
		SELECT id, content, session_id, tags, created_at
		FROM memories
		WHERE deleted_at IS NULL
		  AND (content LIKE ? OR tags LIKE ?)
		ORDER BY created_at DESC
		LIMIT ?`
	return db.Query(likeQuery, likePattern, likePattern, limit)
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
func formatRationale(rc, weight int, lastAccessed time.Time) string {
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

	if !lastAccessed.IsZero() {
		parts = append(parts, "accessed "+formatAge(lastAccessed))
	}

	return strings.Join(parts, " · ")
}

// isMemoryStale returns true if the memory has not been accessed within staleDays.
func isMemoryStale(createdAt, lastAccessed time.Time, staleDays int) bool {
	if staleDays <= 0 {
		return false // feature disabled
	}
	threshold := time.Duration(staleDays) * 24 * time.Hour

	if !lastAccessed.IsZero() {
		return time.Since(lastAccessed) > threshold
	}
	if !createdAt.IsZero() {
		return time.Since(createdAt) > threshold
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
			Weight             int     `json:"weight"`
			Score              float64 `json:"score"`
			Source             string  `json:"source"` // "fts5", "vector", "hybrid"
			FTS5Score          float64 `json:"fts5_score,omitempty"`
			VectorSimilarity   float64 `json:"vector_similarity,omitempty"`
			Rationale          string  `json:"rationale"`
			IsStale            bool    `json:"is_stale"`
		}
		entries := make([]hybridEntry, 0, len(results))
		for _, r := range results {
			createdAt := time.Time{}
			if t, err := time.Parse(time.RFC3339, r.CreatedAt); err == nil {
				createdAt = t
			}
			entries = append(entries, hybridEntry{
				ID:                 shortID(r.ID),
				Content:            r.Content,
				Tags:               r.Tags,
				CreatedAt:          r.CreatedAt,
				ReinforcementCount: r.ReinforcementCount,
				Weight:             r.Weight,
				Score:              r.CombinedScore,
				Source:             r.Source,
				FTS5Score:          r.FTS5Score,
				VectorSimilarity:   r.VectorSimilarity,
				Rationale:          fmt.Sprintf("hybrid fts5+vec weight=%.2f", r.CombinedScore),
				IsStale:            isMemoryStale(createdAt, time.Time{}, staleDays),
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
		createdAt := time.Time{}
		if t, err := time.Parse(time.RFC3339, r.CreatedAt); err == nil {
			createdAt = t
		}
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
