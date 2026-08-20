package main

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"github.com/flowbyte-com/mpm-core/usererror"
	"os"
	"strconv"
	"strings"
	"time"
)

// handleStats shows memory statistics
func handleStats(args []string) int {
	dm := getDB()
	if dm == nil {
		return 1
	}

	stats, err := dm.GetMemoryStats()
	if err != nil {
		usererror.Error("Failed to get stats: %v", err)
	}

	printStats(stats)
	return 0
}

func printStats(s map[string]interface{}) {
	total := safeInt(s["total"])
	active := safeInt(s["active"])
	deleted := safeInt(s["deleted"])
	expired := safeInt(s["expired"])
	live := active - expired
	fmt.Println("\n📊 MPM Memory Statistics")
	fmt.Println("══════════════════════════════════════════")

	fmt.Printf("\n  Total Memories:          %d\n", total)
	fmt.Printf("    ├── Active:            %d\n", active)
	fmt.Printf("    │     ├── Live:        %d\n", live)
	fmt.Printf("    │     └── Expired:     %d\n", expired)
	fmt.Printf("    └── Deleted:           %d\n", deleted)

	fmt.Printf("\n  Active detail:\n")
	fmt.Printf("    LTM (Long-Term):       %d\n", safeInt(s["ltm"]))
	fmt.Printf("    Reinforced:            %d\n", safeInt(s["reinforced"]))
	fmt.Printf("    Never Accessed:        %d ⚠️\n", safeInt(s["never_accessed"]))

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

	// ── Epistemic Provenance Registry ─────────────────────────────────
	if registry, ok := s["provenance_registry"].([]map[string]interface{}); ok && len(registry) > 0 {
		fmt.Println("\n== Epistemic Provenance Registry (Agent → Model → Persona) ==")
		for _, agentEntry := range registry {
			agent, _ := agentEntry["agent"].(string)
			fmt.Printf("\n  Agent: %s\n", agent)
			models, _ := agentEntry["models"].([]map[string]interface{})
			for mi, modelEntry := range models {
				model, _ := modelEntry["model"].(string)
				compute, _ := modelEntry["compute"].(string)
				prefix := "  ├─ "
				if mi == len(models)-1 {
					prefix = "  └─ "
				}
				fmt.Printf("%sModel: %s (Compute: %s)\n", prefix, model, compute)
				personas, _ := modelEntry["personas"].([]map[string]interface{})
				for pi, personaEntry := range personas {
					persona, _ := personaEntry["persona"].(string)
					active, _ := personaEntry["active_memories"].(int)
					total, _ := personaEntry["total_memories"].(int)
					decayed, _ := personaEntry["decayed_to_floor"].(int)
					isr, _ := personaEntry["isr"].(float64)
					pPrefix := "     ├─ "
					if pi == len(personas)-1 {
						pPrefix = "     └─ "
					}
					fmt.Printf("%sPersona: %s\n", pPrefix, persona)
					fmt.Printf("        Total: %d | Active: %d | Decayed: %d | ISR: %.1f%%\n", total, active, decayed, isr)
				}
			}
		}
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

	dm := getDB()
	if dm == nil {
		return 1
	}

	var count int
	var pruneErr error

	if *olderThan != "" {
		duration, parseErr := parseDuration(*olderThan)
		if parseErr != nil {
			usererror.Error("Invalid duration '%s': %v", *olderThan, parseErr)
		}
		cutoff := time.Now().Add(-duration)
		count, pruneErr = dm.PruneOlderThan(cutoff.Unix())
		if pruneErr != nil {
			usererror.Error("Prune failed: %v", pruneErr)
		}
		fmt.Printf("Pruned %d memories older than %s (before %s)\n", count, *olderThan, cutoff.Format(time.RFC3339))
	} else if *neverAccessed {
		count, pruneErr = dm.PruneNeverAccessed()
		if pruneErr != nil {
			usererror.Error("Prune failed: %v", pruneErr)
		}
		fmt.Printf("Pruned %d memories never accessed\n", count)
	} else {
		// Default: prune expired
		count, pruneErr = dm.PruneExpired()
		if pruneErr != nil {
			usererror.Error("Prune failed: %v", pruneErr)
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

	dm := getDB()
	if dm == nil {
		return 1
	}

	memories, err := dm.GetMemoriesForExport(*collection, *since, *until)
	if err != nil {
		usererror.Error("Export failed: %v", err)
	}

	var writer *csv.Writer
	var outputFile *os.File

	if *output != "" {
		outputFile, err = os.Create(*output)
		if err != nil {
			usererror.Error("Cannot create output file: %v", err)
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
			usererror.Error("JSON encode failed: %v", err)
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
					usererror.Error("CSV write error: %v", err)
					break
				}
			}
		}
	}

	usererror.Notice("Exported %d memories", len(memories))
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

	dm := getDB()
	if dm == nil {
		return 1
	}

	if *review {
		// Show memories for spaced reinforcement review
		memories, err := dm.GetSpacedReinforcementReview(*days, 20)
		if err != nil {
			usererror.Error("%v", err)
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
		usererror.Error("%v", err)
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
	if err := dm.SQLDB().QueryRow("SELECT COUNT(*) FROM memories WHERE deleted_at IS NOT NULL").Scan(&deletedCount); err != nil {
		usererror.Warn("handleMaintain: failed to count deleted memories for vacuum, defaulting to 0: %v", err)
	}
	if deletedCount > 0 {
		if _, vacErr := dm.SQLDB().Exec("PRAGMA incremental_vacuum"); vacErr != nil {
			slog.Warn("incremental_vacuum failed", "error", vacErr.Error())
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
