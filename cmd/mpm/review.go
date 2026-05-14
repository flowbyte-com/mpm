package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
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
		// Spec: WHERE deleted_at IS NULL AND last_accessed_at IS NOT NULL
		//       AND (reinforcement_count > 0 OR weight > 1) ORDER BY last_accessed_at DESC
		rows, err := dm.SQLDB().Query(`
			SELECT id, collection, content,
			       COALESCE(reinforcement_count, 0) as reinforcement_count,
			       COALESCE(weight, 1) as weight,
			       last_accessed_at,
			       created_at
			FROM memories
			WHERE deleted_at IS NULL
			  AND last_accessed_at IS NOT NULL
			  AND (reinforcement_count > 0 OR weight > 1)
			ORDER BY last_accessed_at DESC
			LIMIT ?
		`, resultLimit)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Review query failed: %v\n", err)
			return 1
		}
		defer rows.Close()

		memories, err = scanMemoriesFromRows(rows)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Review scan failed: %v\n", err)
			return 1
		}
	} else {
		// Stale: LTM/high-weight memories not accessed in N+ days
		memories, err = dm.GetSpacedReinforcementReview(daysFilter, 20)
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
		modeStr := "promoted"
		if !showPromoted {
			modeStr = "stale"
		}
		data, _ := json.Marshal(map[string]interface{}{
			"mode":     modeStr,
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

		fmt.Printf("%s%d.%s %s[%s]%s %s%s%s weight\n",
			cyan, i+1, reset,
			dim, id, reset,
			magenta, recallFreq, reset)
		fmt.Printf("   %s%s  weight %s%+d%s\n",
			magenta, age, dim, weightDelta, reset)
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

// scanMemoriesFromRows scans SQL rows into a memory list
func scanMemoriesFromRows(rows *sql.Rows) ([]map[string]interface{}, error) {
	var result []map[string]interface{}
	for rows.Next() {
		var id, collection, content string
		var reinforcementCount, weight int64
		var lastAccess, createdAt time.Time

		if err := rows.Scan(&id, &collection, &content, &reinforcementCount, &weight, &lastAccess, &createdAt); err != nil {
			continue
		}
		result = append(result, map[string]interface{}{
			"id":                  id,
			"collection":          collection,
			"content":             content,
			"reinforcement_count": reinforcementCount,
			"weight":              weight,
			"last_accessed_at":    lastAccess,
			"created_at":          createdAt,
		})
	}
	return result, rows.Err()
}