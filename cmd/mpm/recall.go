package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
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
	jsonOutput := fs.Bool("json", false, "Output JSON for tool integration")
	collection := fs.String("collection", "memories", "Collection to search")
	fs.Usage = func() {
		fmt.Println("Usage: mpm recall [options] <query>")
		fmt.Println("\nRecall options:")
		fs.PrintDefaults()
	}

	// Go's flag.Parse stops at the first non-flag positional arg.
	// Pre-scan for --json since callers may place it after the query.
	preprocessed := make([]string, 0, len(args))
	jsonFlagSeen := false
	for _, arg := range args[1:] {
		if arg == "--json" || arg == "-j" {
			jsonFlagSeen = true
			continue // drop from processed args, we'll set it directly
		}
		preprocessed = append(preprocessed, arg)
	}

	if err := fs.Parse(preprocessed); err != nil {
		return 1
	}

	// Apply pre-scanned json flag
	if jsonFlagSeen {
		*jsonOutput = true
	}

	query := fs.Arg(0)
	if query == "" {
		fmt.Fprintf(os.Stderr, "Usage: mpm recall [options] <query>\n")
		return 1
	}
	query = strings.TrimSpace(query)
	if query == "" {
		fmt.Fprintf(os.Stderr, "Empty query.\n")
		return 1
	}

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ DB open failed: %v\n", err)
		return 1
	}
	defer dm.Close()

	db := dm.SQLDB()
	sessionAccessCounts := make(map[string]int)

	// Keyword search using LIKE + FTS5 fallback with time filters
	rows, err := keywordSearchWithTime(db, query, *collection, *since, *until, *limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Search failed: %v\n", err)
		return 1
	}
	defer rows.Close()

	type recallEntry struct {
		id                   string
		content              string
		sessionID            string
		createdAt            time.Time
		tags                 string
		synthesized          bool
		reinforcementCount   int
		weight               int
		lastAccessedAt       time.Time
	}
	var entries []recallEntry
	for rows.Next() {
		var id, content, createdAt string
		var nullableSessionID, nullableTags sql.NullString
		var reinforcementCount, weight int64
		var nullableLastAccessed sql.NullTime

		if err := rows.Scan(&id, &content, &nullableSessionID, &nullableTags, &createdAt,
			&reinforcementCount, &weight, &nullableLastAccessed); err != nil {
			continue
		}
		if content == "" {
			continue
		}
		sessionID := ""
		if nullableSessionID.Valid {
			sessionID = nullableSessionID.String
		}
		entry := recallEntry{
			id:                  id,
			content:             content,
			sessionID:           sessionID,
			tags:                nullableTags.String,
			reinforcementCount:  int(reinforcementCount),
			weight:              int(weight),
		}
		if t, err := time.Parse(time.RFC3339, createdAt); err == nil {
			entry.createdAt = t
		}
		if nullableLastAccessed.Valid {
			entry.lastAccessedAt = nullableLastAccessed.Time
		}
		if strings.Contains(nullableTags.String, "synthesized") {
			entry.synthesized = true
		}
		// Per-call access deduplication: reinforce only on first access in this call
		if sessionAccessCounts[id] == 0 {
			if err := dm.ReinforceMemory(id, 1); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to reinforce memory %s: %v\n", id, err)
			}
		}
		sessionAccessCounts[id]++
		entries = append(entries, entry)
	}

	if len(entries) == 0 {
		fmt.Printf("No memories found for: %s\n", query)
		return 0
	}

	// Sort by recency
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].createdAt.After(entries[j].createdAt)
	})

	// JSON output for tool integration
	if *jsonOutput {
		type memoryEntry struct {
			ID        string `json:"id"`
			Content   string `json:"content"`
			Tags      string `json:"tags"`
			SessionID string `json:"session_id,omitempty"`
			CreatedAt string `json:"created_at"`
		}
		result := make([]memoryEntry, 0, len(entries))
		for _, e := range entries {
			result = append(result, memoryEntry{
				Content:   e.content,
				Tags:      e.tags,
				SessionID: e.sessionID,
				CreatedAt: e.createdAt.Format(time.RFC3339),
			})
		}
		data, _ := json.Marshal(map[string]interface{}{
			"query":    query,
			"memories": result,
		})
		fmt.Println(string(data))
		return 0
	}

	cyan := "\033[36m"
	magenta := "\033[35m"
	reset := "\033[0m"
	bold := "\033[1m"

	fmt.Printf("%s%sRecall — %s%s\n\n", bold, cyan, query, reset)

	dim := "\033[2m"
	for i, e := range entries {
		content := e.content
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

		fmt.Printf("%s%d.%s %s%s%s\n    %s\n\n",
			cyan, i+1, reset,
			chipsLine, synthTag,
			reset,
			content)
	}

	fmt.Printf("%s%d results%s\n", cyan, len(entries), reset)
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

func keywordSearchWithTime(db *sql.DB, query, collection, since, until string, limit int) (*sql.Rows, error) {
	if collection == "" {
		collection = "memories"
	}

	// Try FTS5 first
	ftsQuery := `
		SELECT m.id, m.content, m.session_id, m.tags, m.created_at,
		       COALESCE(m.reinforcement_count, 0) as reinforcement_count,
		       COALESCE(m.weight, 1) as weight,
		       m.last_accessed_at
		FROM memories m
		JOIN memories_fts fts ON m.rowid = fts.rowid
		WHERE memories_fts MATCH ? AND m.deleted_at IS NULL AND m.collection = ?`

	args := []interface{}{query, collection}

	if since != "" {
		ftsQuery += " AND m.created_at >= ?"
		args = append(args, since)
	}
	if until != "" {
		ftsQuery += " AND m.created_at <= ?"
		args = append(args, until+" 23:59:59")
	}

	ftsQuery += " ORDER BY fts.rank LIMIT ?"
	args = append(args, limit)

	rows, err := db.Query(ftsQuery, args...)
	if err == nil {
		return rows, nil
	}

	// FTS5 failed — fallback to LIKE
	likePattern := "%" + query + "%"
	likeQuery := `
		SELECT id, content, session_id, tags, created_at,
		       COALESCE(reinforcement_count, 0) as reinforcement_count,
		       COALESCE(weight, 1) as weight,
		       last_accessed_at
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
