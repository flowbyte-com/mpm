package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"time"

	"github.com/flowbyte-com/mpm-core/usererror"
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
	jsonFlagSeen, preprocessed := ExtractJSONFlag(args[1:])

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

	dm := getDB()
	if dm == nil {
		return 1
	}

	var memories []map[string]interface{}

	// `err` is reused across both branches below (`showPromoted` and the
	// `GetSpacedReinforcementReview` path). It was previously declared by
	// `dm, err := mpminternal.NewDatabaseManager("")`; that line is now
	// the `getDB()` singleton lookup, so declare `err` explicitly.
	var err error

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
			usererror.Error("Review query failed: %v", err)
			return 1
		}
		defer rows.Close()

		memories, err = scanMemoriesFromRows(rows)
		if err != nil {
			usererror.Error("Review scan failed: %v", err)
			return 1
		}
	} else {
		// Stale: LTM/high-weight memories not accessed in N+ days
		memories, err = dm.GetSpacedReinforcementReview(daysFilter, 20)
		if err != nil {
			usererror.Error("Stale query failed: %v", err)
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
				Weight:             toInt(m["weight"]),
				ReinforcementCount: toInt(m["reinforcement_count"]),
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
		rc := toInt(m["reinforcement_count"])
		weight := toInt(m["weight"])
		lastAccess := toTime(m["last_accessed_at"])
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

// toInt safely extracts an int from interface{} handling both int and int64.
func toInt(v interface{}) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}

// toTime safely extracts a time.Time from interface{}.
//
// Handles:
//   - time.Time
//   - string (RFC3339)
//   - int / int64 / float64 (Unix-epoch seconds — the canonical
//     INTEGER timestamp shape after the 2026-08 timestamps_unified_v1
//     migration; see internal/core/memory.go)
//
// D-009 (alpha-4.1.1): the int64 branch is load-bearing. The pre-fix
// code only handled time.Time and string — any integer timestamp fell
// through to the zero-time default, which `time.Since` rendered as
// "106751d ago" (≈292 years, the time from year 1 to now) for every
// newly-created memory in the spaced review report. The fix routes the
// integer shape through time.Unix so the relative-age display reads
// sensibly.
//
// Future timestamp shapes (RFC3339Nano, fractional seconds) are out of
// scope — the substrate standardises on INTEGER Unix-epoch seconds.
func toTime(v interface{}) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t
	case string:
		parsed, _ := time.Parse(time.RFC3339, t)
		return parsed
	case int:
		if t <= 0 {
			return time.Time{}
		}
		return time.Unix(int64(t), 0)
	case int64:
		if t <= 0 {
			return time.Time{}
		}
		return time.Unix(t, 0)
	case float64:
		if t <= 0 {
			return time.Time{}
		}
		return time.Unix(int64(t), 0)
	default:
		return time.Time{}
	}
}

// scanMemoriesFromRows scans SQL rows into a memory list.
//
// Timestamp fields are stored as INTEGER Unix-epoch seconds (see migration
// timestamps_unified_v1).
func scanMemoriesFromRows(rows *sql.Rows) ([]map[string]interface{}, error) {
	var result []map[string]interface{}
	for rows.Next() {
		var id, collection, content string
		var reinforcementCount int64
		var weight float64
		var lastAccess, createdAt int64

		if err := rows.Scan(&id, &collection, &content, &reinforcementCount, &weight, &lastAccess, &createdAt); err != nil {
			return nil, fmt.Errorf("scanning memory row during spaced review: %w", err)
		}
		result = append(result, map[string]interface{}{
			"id":                  id,
			"collection":          collection,
			"content":             content,
			"reinforcement_count": reinforcementCount,
			"weight":              int(weight),
			"last_accessed_at":    lastAccess,
			"created_at":          createdAt,
		})
	}
	return result, rows.Err()
}
