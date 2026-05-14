package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	mpminternal "mpm/internal"
)

// handleReview implements `mpm review [flags]` for spaced reinforcement review.
// --promoted (default): show recently elevated memories, ordered by last_accessed_at DESC, limited to 20
// --stale --days N: show LTM/high-weight memories not accessed in N+ days
// --json: output JSON for tool integration
func handleReview(args []string) int {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	promoted := fs.Bool("promoted", false, "Show recently elevated/promoted memories (default)")
	stale := fs.Bool("stale", false, "Show stale memories (LTM/high-weight not accessed in --days)")
	days := fs.Int("days", 30, "Number of days for stale filter")
	jsonOutput := fs.Bool("json", false, "Output JSON for tool integration")
	limit := fs.Int("limit", 20, "Maximum results to return")
	fs.Usage = func() {
		fmt.Println("Usage: mpm review [options]")
		fmt.Println("\nReview options:")
		fs.PrintDefaults()
	}

	// Handle --json anywhere in args (may follow command name)
	preprocessed := make([]string, 0, len(args))
	jsonFlagSeen := false
	for _, arg := range args[1:] {
		if arg == "--json" || arg == "-j" {
			jsonFlagSeen = true
			continue
		}
		preprocessed = append(preprocessed, arg)
	}

	if err := fs.Parse(preprocessed); err != nil {
		return 1
	}

	if jsonFlagSeen {
		*jsonOutput = true
	}

	// Default to promoted if neither flag is set
	showPromoted := *promoted || (!*stale)
	daysFilter := *days
	resultLimit := *limit
	if resultLimit <= 0 {
		resultLimit = 20
	}

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "DB open failed: %v\n", err)
		return 1
	}
	defer dm.Close()

	var memories []map[string]interface{}

	if showPromoted {
		// Default: recently elevated memories for spaced reinforcement review
		memories, err = dm.GetSpacedReinforcementReview(0, resultLimit)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Review query failed: %v\n", err)
			return 1
		}
		// Sort by last_accessed_at DESC (already sorted by the method, but ensure)
		sortByLastAccessed(memories)
	} else {
		// Stale: LTM/high-weight memories not accessed in N+ days
		memories, err = getStaleMemories(dm.SQLDB(), daysFilter, resultLimit)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Stale query failed: %v\n", err)
			return 1
		}
	}

	if len(memories) == 0 {
		if showPromoted {
			fmt.Println("No promoted memories found.")
		} else {
			fmt.Printf("No stale memories (unused for %d+ days)\n", daysFilter)
		}
		return 0
	}

	// JSON output
	if *jsonOutput {
		type memoryEntry struct {
			ID                 string `json:"id"`
			Content            string `json:"content"`
			Weight             int    `json:"weight"`
			ReinforcementCount int    `json:"reinforcement_count"`
			LastAccessedAt     string `json:"last_accessed_at"`
			Collection         string `json:"collection"`
		}
		result := make([]memoryEntry, 0, len(memories))
		for _, m := range memories {
			lastAccess := ""
			if v, ok := m["last_accessed_at"].(time.Time); ok {
				lastAccess = v.Format(time.RFC3339)
			}
			result = append(result, memoryEntry{
				ID:                 shortID(m["id"].(string)),
				Content:            m["content"].(string),
				Weight:             int(m["weight"].(int64)),
				ReinforcementCount: int(m["reinforcement_count"].(int64)),
				LastAccessedAt:     lastAccess,
				Collection:         m["collection"].(string),
			})
		}
		data, _ := json.Marshal(map[string]interface{}{
			"mode":     map[bool]bool{true: "promoted", false: "stale"}[showPromoted],
			"days":     daysFilter,
			"count":    len(result),
			"memories": result,
		})
		fmt.Println(string(data))
		return 0
	}

	// Human-readable output
	cyan := "\033[36m"
	magenta := "\033[35m"
	reset := "\033[0m"
	bold := "\033[1m"
	dim := "\033[2m"

	if showPromoted {
		fmt.Printf("%s%sSpaced Reinforcement Review%s\n", bold, cyan, reset)
	} else {
		fmt.Printf("%s%sStale Memory Review (%d+ days)%s\n", bold, cyan, daysFilter, reset)
	}
	fmt.Println()

	for i, m := range memories {
		id := shortID(m["id"].(string))
		rc := int(m["reinforcement_count"].(int64))
		weight := int(m["weight"].(int64))
		lastAccess := m["last_accessed_at"].(time.Time)
		content := m["content"].(string)

		age := formatAge(lastAccess)

		// Content snippet (strip markdown, truncate)
		snippet := stripMarkdown(content)
		if len(snippet) > 120 {
			snippet = snippet[:120] + "..."
		}

		// Weight delta shows how far from LTM threshold (10)
		weightDelta := weight - 10

		// Recall frequency
		recallFreq := fmt.Sprintf("%dx", rc)

		fmt.Printf("%s%d.%s %s[%s]%s %s%dd%s weight\n",
			cyan, i+1, reset,
			dim, id, reset,
			magenta, recallFreq, reset)
		fmt.Printf("   %s%dd%s ago  %s%+d%s\n",
			magenta, age, reset,
			dim, weightDelta, reset)
		fmt.Printf("   %s\n\n", snippet)
	}

	fmt.Printf("%s%d memories%s\n", cyan, len(memories), reset)
	return 0
}

// shortID returns a shortened memory ID (first 8 chars)
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// sortByLastAccessed sorts memories by last_accessed_at DESC
func sortByLastAccessed(memories []map[string]interface{}) {
	// GetSpacedReinforcementReview already returns sorted by last_accessed_at DESC
	// But we sort again to be safe in case the order changes
	for i := 0; i < len(memories)-1; i++ {
		for j := i + 1; j < len(memories); j++ {
			ti := memories[i]["last_accessed_at"].(time.Time)
			tj := memories[j]["last_accessed_at"].(time.Time)
			if tj.After(ti) {
				memories[i], memories[j] = memories[j], memories[i]
			}
		}
	}
}

// getStaleMemories returns LTM/high-weight memories not accessed in N+ days
func getStaleMemories(db *sql.DB, days int, limit int) ([]map[string]interface{}, error) {
	cutoff := time.Now().AddDate(0, 0, -days)
	query := `
		SELECT id, content, weight, reinforcement_count, last_accessed_at, collection, tags
		FROM memories
		WHERE deleted_at IS NULL
		  AND (weight >= 10 OR collection = 'ltm')
		  AND last_accessed_at < ?
		ORDER BY last_accessed_at ASC
		LIMIT ?`

	rows, err := db.Query(query, cutoff.Format(time.RFC3339), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []map[string]interface{}
	for rows.Next() {
		var id, content, collection, tags string
		var weight, reinforcementCount int64
		var lastAccess time.Time

		if err := rows.Scan(&id, &content, &weight, &reinforcementCount, &lastAccess, &collection, &tags); err != nil {
			continue
		}
		result = append(result, map[string]interface{}{
			"id":                  id,
			"content":             content,
			"weight":              weight,
			"reinforcement_count": reinforcementCount,
			"last_accessed_at":    lastAccess,
			"collection":          collection,
			"tags":                tags,
		})
	}
	return result, nil
}