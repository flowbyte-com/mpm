package main

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	mpminternal "mpm/internal"
)

// handleStats shows memory statistics
func handleStats(args []string) int {
	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to open database: %v\n", err)
		return 1
	}
	defer dm.Close()

	stats, err := dm.GetMemoryStats()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to get stats: %v\n", err)
		return 1
	}

	printStats(stats)
	return 0
}

func printStats(s map[string]interface{}) {
	fmt.Println("\n📊 MPM Memory Statistics")
	fmt.Println("══════════════════════════════════════════")

	fmt.Printf("\n  Total Memories:     %v\n", s["total"])
	fmt.Printf("  Active:            %v\n", s["active"])
	fmt.Printf("  Deleted:           %v\n", s["deleted"])
	fmt.Printf("  LTM (Long-Term):   %v\n", s["ltm"])
	fmt.Printf("  Reinforced:        %v\n", s["reinforced"])
	fmt.Printf("  Never Accessed:    %v ⚠️\n", s["never_accessed"])
	fmt.Printf("  Expired:           %v\n", s["expired"])

	fmt.Printf("\n  By Collection:\n")
	if collections, ok := s["by_collection"].([]map[string]interface{}); ok {
		for _, c := range collections {
			fmt.Printf("    • %-20s %v\n", c["collection"], c["count"])
		}
	}

	fmt.Printf("\n  By Tag (top 10):\n")
	tags, _ := s["by_tag"].([]map[string]interface{})
	if len(tags) == 0 {
		fmt.Printf("    • None\n")
	} else {
		for i, t := range tags {
			if i >= 10 {
				fmt.Printf("    ... and %d more tags\n", len(tags)-10)
				break
			}
			fmt.Printf("    • %-20s %v uses\n", t["tag"], t["count"])
		}
	}

	fmt.Printf("\n  Reinforcement Distribution:\n")
	reinforceDist, _ := s["reinforce_dist"].([]map[string]interface{})
	for _, r := range reinforceDist {
		fmt.Printf("    • %d memories with rc=%v\n", r["count"], r["reinforcement_count"])
	}

	fmt.Print("\n══════════════════════════════════════════\n")
}

// handlePrune removes old/expired memories
func handlePrune(args []string) int {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	olderThan := fs.String("older-than", "", "Delete memories older than duration (e.g., 90d, 30d)")
	neverAccessed := fs.Bool("never-accessed", false, "Delete memories never accessed")
	fs.Usage = func() {
		fmt.Println("Usage: mpm prune [options]")
		fmt.Println("\nPrune options:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args[1:]); err != nil {
		return 1
	}

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to open database: %v\n", err)
		return 1
	}
	defer dm.Close()

	var count int
	var pruneErr error

	if *olderThan != "" {
		duration, parseErr := parseDuration(*olderThan)
		if parseErr != nil {
			fmt.Fprintf(os.Stderr, "Error: Invalid duration '%s': %v\n", *olderThan, parseErr)
			return 1
		}
		cutoff := time.Now().Add(-duration)
		count, pruneErr = dm.PruneOlderThan(cutoff)
		if pruneErr != nil {
			fmt.Fprintf(os.Stderr, "Error: Prune failed: %v\n", pruneErr)
			return 1
		}
		fmt.Printf("Pruned %d memories older than %s (before %s)\n", count, *olderThan, cutoff.Format(time.RFC3339))
	} else if *neverAccessed {
		count, pruneErr = dm.PruneNeverAccessed()
		if pruneErr != nil {
			fmt.Fprintf(os.Stderr, "Error: Prune failed: %v\n", pruneErr)
			return 1
		}
		fmt.Printf("Pruned %d memories never accessed\n", count)
	} else {
		// Default: prune expired
		count, pruneErr = dm.PruneExpired()
		if pruneErr != nil {
			fmt.Fprintf(os.Stderr, "Error: Prune failed: %v\n", pruneErr)
			return 1
		}
		fmt.Printf("Pruned %d expired memories\n", count)
	}

	return 0
}

func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "d") {
		days := strings.TrimSuffix(s, "d")
		d, err := strconv.Atoi(days)
		if err != nil {
			return 0, err
		}
		return time.Duration(d) * 24 * time.Hour, nil
	}
	if strings.HasSuffix(s, "h") {
		hours := strings.TrimSuffix(s, "h")
		h, err := strconv.Atoi(hours)
		if err != nil {
			return 0, err
		}
		return time.Duration(h) * time.Hour, nil
	}
	if strings.HasSuffix(s, "m") {
		minutes := strings.TrimSuffix(s, "m")
		m, err := strconv.Atoi(minutes)
		if err != nil {
			return 0, err
		}
		return time.Duration(m) * time.Minute, nil
	}
	return time.ParseDuration(s)
}

// handleExport exports memories to JSON/CSV
func handleExport(args []string) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	format := fs.String("format", "json", "Output format: json, csv")
	collection := fs.String("collection", "", "Filter by collection")
	since := fs.String("since", "", "Export memories since date (YYYY-MM-DD)")
	until := fs.String("until", "", "Export memories until date (YYYY-MM-DD)")
	output := fs.String("output", "", "Output file (default: stdout)")
	fs.Usage = func() {
		fmt.Println("Usage: mpm export [options]")
		fmt.Println("\nExport options:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args[1:]); err != nil {
		return 1
	}

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to open database: %v\n", err)
		return 1
	}
	defer dm.Close()

	memories, err := dm.GetMemoriesForExport(*collection, *since, *until)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Export failed: %v\n", err)
		return 1
	}

	var writer *csv.Writer
	var outputFile *os.File

	if *output != "" {
		outputFile, err = os.Create(*output)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: Cannot create output file: %v\n", err)
			return 1
		}
		defer outputFile.Close()

		if *format == "csv" {
			writer = csv.NewWriter(outputFile)
		}
	}

	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(memories); err != nil {
			fmt.Fprintf(os.Stderr, "Error: JSON encode failed: %v\n", err)
			return 1
		}
	} else if *format == "csv" {
		if writer == nil {
			writer = csv.NewWriter(os.Stdout)
		}
		defer writer.Flush()

		// CSV header
		csvErr := writer.Write([]string{"id", "collection", "content", "tags", "created_at", "reinforcement_count", "weight", "is_long_term"})
		if csvErr == nil {
			for _, m := range memories {
				id, _ := m["id"].(string)
				coll, _ := m["collection"].(string)
				content, _ := m["content"].(string)
				created, _ := m["created_at"].(string)
				tags := ""
				if t, ok := m["tags"].(string); ok {
					tags = t
				}
				rc := 0
				if r, ok := m["reinforcement_count"].(int64); ok {
					rc = int(r)
				}
				w := 1
				if we, ok := m["weight"].(int64); ok {
					w = int(we)
				}
				lt := 0
				if l, ok := m["is_long_term"].(int64); ok {
					lt = int(l)
				}
				if err := writer.Write([]string{id, coll, content, tags, created, fmt.Sprintf("%d", rc), fmt.Sprintf("%d", w), fmt.Sprintf("%d", lt)}); err != nil {
					fmt.Fprintf(os.Stderr, "CSV write error: %v\n", err)
					break
				}
			}
		}
	}

	fmt.Fprintf(os.Stderr, "Exported %d memories\n", len(memories))
	return 0
}

// handleMaintain runs self-improvement maintenance
func handleMaintain(args []string) int {
	fs := flag.NewFlagSet("maintain", flag.ContinueOnError)
	review := fs.Bool("review", false, "Show memories for spaced reinforcement review")
	days := fs.Int("days", 14, "Days since access for spaced reinforcement review")
	fs.Usage = func() {
		fmt.Println("Usage: mpm maintain [options]")
		fmt.Println("\nSelf-improvement options:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args[1:]); err != nil {
		return 1
	}

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	defer dm.Close()

	if *review {
		// Show memories for spaced reinforcement review
		memories, err := dm.GetSpacedReinforcementReview(*days, 20)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}

		if len(memories) == 0 {
			fmt.Println("No memories need reinforcement review")
			return 0
		}

		fmt.Printf("\n📋 Memories for Spaced Reinforcement Review (not accessed in %d days):\n", *days)
		fmt.Println("───────────────────────────────────────────────────────────────")
		for i, m := range memories {
			content := m["content"].(string)
			if len(content) > 80 {
				content = content[:80] + "..."
			}
			w := safeInt(m["weight"])
			rc := safeInt(m["reinforcement_count"])
			la := ""
			if lat, ok := m["last_accessed_at"].(string); ok && lat != "" {
				la = lat
			}
			fmt.Printf("%d. [w=%d rc=%d last=%s] %s\n", i+1, w, rc, la, content)
		}
		return 0
	}

	fmt.Println("\n🧠 Running self-maintenance...")

	// Run full self-maintenance
	stats, err := dm.RunSelfMaintenance()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	fmt.Println("\n✅ Self-maintenance complete:")
	fmt.Printf("   Decayed weights:      %d\n", stats["decayed_weights"])
	fmt.Printf("   Pruned total:        %d\n", stats["pruned_total"])
	fmt.Printf("   Consolidated:         %d\n", stats["consolidated"])
	fmt.Printf("   Never accessed:      %d\n", stats["never_accessed"])
	fmt.Printf("   Low weight:          %d\n", stats["low_weight"])
	fmt.Printf("   For review:          %d\n", stats["for_spaced_reinforcement"])

	// Deferred VACUUM: reclaim space from hard deletes during idle maintenance
	var deletedCount int
	dm.SQLDB().QueryRow("SELECT COUNT(*) FROM memories WHERE deleted_at IS NOT NULL").Scan(&deletedCount)
	if deletedCount > 0 {
		if _, vacErr := dm.SQLDB().Exec("PRAGMA incremental_vacuum"); vacErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: incremental_vacuum failed: %v\n", vacErr)
		} else {
			fmt.Printf("   Vacuum reclaimed space (%d deleted records)\n", deletedCount)
		}
	}

	fmt.Println()

	// Suggest spaced reinforcement review if there are memories needing it
	if reviewCount, ok := stats["for_spaced_reinforcement"].(int); ok && reviewCount > 0 {
		fmt.Printf("💡 Run 'mpm maintain --review' to see %d memories that could use reinforcement\n", reviewCount)
	}

	return 0
}

// safeInt safely extracts an int from interface{} that may be int, int64, or float64.
// Returns 0 on type mismatch.
func safeInt(v interface{}) int {
	switch val := v.(type) {
	case int:
		return val
	case int64:
		return int(val)
	case float64:
		return int(val)
	default:
		return 0
	}
}
