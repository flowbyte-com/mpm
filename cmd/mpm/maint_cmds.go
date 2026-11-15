package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/flowbyte-com/mpm-core/usererror"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
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
			return 1
		}
		cutoff := time.Now().Add(-duration)
		count, pruneErr = dm.PruneOlderThan(cutoff.Unix())
		if pruneErr != nil {
			usererror.Error("Prune failed: %v", pruneErr)
			return 1
		}
		fmt.Printf("Pruned %d memories older than %s (before %s)\n", count, *olderThan, cutoff.Format(time.RFC3339))
	} else if *neverAccessed {
		count, pruneErr = dm.PruneNeverAccessed()
		if pruneErr != nil {
			usererror.Error("Prune failed: %v", pruneErr)
			return 1
		}
		fmt.Printf("Pruned %d memories never accessed\n", count)
	} else {
		// Default: prune expired
		count, pruneErr = dm.PruneExpired()
		if pruneErr != nil {
			usererror.Error("Prune failed: %v", pruneErr)
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

// exportUsage is the usage line the export help page prints.
const exportUsage = "mpm export [options]"

// exportProse is the part of the export help page that is not an option
// list.
//
// It is where the JSON document's SHAPE is documented, because the shape
// is a contract a consumer has to know and the option list cannot carry
// it. The prose is reachable from `mpm export --help`, `mpm export -h`
// and `mpm help export` — the router's own help interception reaches this
// page for all three. (The handler's own fs.Usage is NOT the place: the
// router validates the options before the handler is entered, so an
// fs.Usage attached to the handler's flagset is never printed.)
const exportProse = `The JSON document is an OBJECT, not a bare array:

  {"memories": [ ... ],
   "packages_omitted": { "count": N, "packages": [ ... ], "scope": { ... } },
   "package_archives": { "count": N, "directory": "...", "archives": [ ... ] }}

"packages_omitted" is ALWAYS present. It names every skill package that
was selected and could NOT be carried, each with its reason. A count of 0
means the export carried every package it selected - read it against
"scope", which says how many were selected, how many were carried, and
how many the store holds.

"package_archives" is ALWAYS present. It is the index of the .skill
archives the export produced: each archive's file name, skill id, package
hash and archive digest. The archives themselves are written to a SIBLING
DIRECTORY named for the export - "<output-path>.packages".

"memories" is the same key mpm migrate --from reads, so an export file
remains a valid input for "mpm migrate --from <file>" - but a document
that carries package archives is REFUSED by the migration, because the
migration reads only the projected rows and would drop the packages'
content silently.

The CSV format is unchanged. The loss marker is a notice on standard error,
so it never corrupts the document on standard output.

A package-bearing export needs a destination that can carry the archives:
standard output, CSV and the deprecated bare array are REFUSED for it,
before anything is written. An export that carries no package is
unaffected.

--format json-array is the DEPRECATED legacy shape: a bare JSON array,
with no room for the loss marker or the archive index. It is refused
whenever the export would carry a package, because emitting the old shape
then would drop package content silently - the one thing R11 exists to
prevent. It is kept for one release for consumers that cannot yet read the
object, and it will be removed.`

// exportOpts holds the export command's parsed options.
//
// The handler's flag set and the help page are built from ONE declaration
// (exportOptions), so the advertised option set and the accepted option
// set cannot disagree — which is what the command's own help contract
// requires.
type exportOpts struct {
	format     string
	collection string
	since      string
	until      string
	output     string
}

// exportOptions declares every option the export command accepts.
func exportOptions(o *exportOpts) []OptionSpec {
	return []OptionSpec{
		HelpOption,
		{Name: "format", Usage: "Output format", Kind: OptString, String: &o.format,
			ValueName: "json|csv|json-array", DefaultText: "default json"},
		{Name: "collection", Usage: "Filter by collection", Kind: OptString, String: &o.collection, ValueName: "name"},
		{Name: "since", Usage: "Export memories since date", Kind: OptString, String: &o.since, ValueName: "YYYY-MM-DD"},
		{Name: "until", Usage: "Export memories until date", Kind: OptString, String: &o.until, ValueName: "YYYY-MM-DD"},
		{Name: "output", Usage: "Output file", Kind: OptString, String: &o.output, ValueName: "path"},
	}
}

// printExportHelp renders the export command's own help page.
func printExportHelp() int {
	o := &exportOpts{format: "json"}
	return PrintHelpPage(exportUsage, exportOptions(o), exportProse)
}

// exportLegacyArrayFormat is the DEPRECATED bare-array JSON format.
//
// It exists for one release, for consumers that cannot yet read the
// structured object. It is REFUSED whenever the export would omit a
// package's content, because emitting the legacy shape then would drop
// those bytes silently — the exact loss R11 exists to make loud.
const exportLegacyArrayFormat = "json-array"

// handleExport exports memories to JSON/CSV
func handleExport(args []string) int {
	// A help request must never export, and must never mutate. The
	// router intercepts the top-level `mpm export --help`, but the nested
	// `mpm ops export --help` reaches this handler with the router's
	// rewritten "help" token — so the handler short-circuits on it too.
	if requireHelpShortCircuit(args, func() { printExportHelp() }) {
		return 0
	}

	var o exportOpts
	o.format = "json"
	opts := exportOptions(&o)

	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(discardWriter{})
	RegisterOptions(fs, opts)

	// The router has already consumed -h/--help.
	if rc := RejectUnknownOptionInArgs("export", exportUsage, opts, args[1:]); rc != 0 {
		return rc
	}

	if err := fs.Parse(args[1:]); err != nil {
		return usererror.Error("export: %v", err)
	}

	switch o.format {
	case "json", "csv", exportLegacyArrayFormat:
	default:
		return usererror.Error("Unknown export format %q: expected json, csv or %s", o.format, exportLegacyArrayFormat)
	}

	dm := getDB()
	if dm == nil {
		return 1
	}

	memories, err := dm.GetMemoriesForExport(o.collection, o.since, o.until)
	if err != nil {
		return usererror.Error("Export failed: %v", err)
	}

	// ── The packages this export is ABOUT. ──────────────────────────────
	//
	// The scope mirrors the memory selection exactly — the same
	// collection, since and until — so the two surfaces cannot disagree
	// about which packages this export covers.
	//
	// Before P7 this was the LOSS marker: the skills export carried the
	// PROJECTED SKILL ROW only, so every package's reference files,
	// scripts and assets were dropped, and the marker named them.
	// P7 makes the export COMPLETE — the archives are written beside
	// the document — so the scope is what the export is ABOUT, and the
	// marker (built from the export's own omissions, below) is what it
	// could NOT carry.
	//
	// The marker is carried structurally in the JSON response AND printed
	// as a loud notice. There is no configuration in which a loss is
	// silent.
	//
	// The concrete manager is used rather than the CoreDB interface: the
	// scope is a property of the export, not a substrate read surface,
	// and the CLI-only operations stay off CoreDB on purpose (the same
	// boundary TestPurge_IsNotOnCoreDB pins).
	scope := &mpminternal.SkillPackageExportScopeResult{}
	if concrete := getDBConcrete(); concrete != nil {
		selected, err := concrete.SkillPackageExportScope(o.collection, o.since, o.until)
		if err != nil {
			return usererror.Error("Export failed: %v", err)
		}
		scope = selected
	}
	packageBearing := len(scope.Packages) > 0

	// ── The DESTINATION policy. ────────────────────────────────────────
	//
	// A package-bearing export needs a destination that can carry the
	// archives AND the index that names them. The refusal happens
	// BEFORE anything is written, so a refused export has written
	// nothing at all — not a truncated document, not a half-published
	// directory.
	//
	// Standard output cannot carry the archives, and neither can the
	// CSV format or the DEPRECATED bare-array format: neither has room
	// for the index, and an export that dropped the archives silently
	// would be the exact loss R11 exists to prevent.
	if packageBearing {
		switch {
		case o.output == "":
			return usererrorf(
				"this export is about %d skill package(s), and their .skill archives cannot be carried to standard output. "+
					"Re-run with --output <path>: the export writes the document to that path and the archives to the sibling directory %s.",
				len(scope.Packages), packageExportDirectorySuffix)
		case o.format == "csv":
			return usererrorf(
				"the CSV format cannot carry the .skill archives of the %d skill package(s) in scope, nor the index that names them. "+
					"Use --format json with --output <path>. The CSV format is unchanged for an export that carries no package.",
				len(scope.Packages))
		case o.format == exportLegacyArrayFormat:
			return usererrorf(
				"the legacy %q format cannot carry the .skill archives of the %d skill package(s) in scope, nor the index that names them. "+
					"Use --format json with --output <path>.",
				exportLegacyArrayFormat, len(scope.Packages))
		}
	}

	// ── Export the packages. ───────────────────────────────────────────
	//
	// The export is driven by the SCOPE, not by the exported rows: the
	// scope is the list of packages this export is ABOUT. An ordinary
	// skill is not in the scope — it has no package — so it is never
	// offered to the exporter, and it is never named as an omission.
	//
	// The scope's packages whose skill row is NOT among the exported
	// rows are named as omissions too: the export's two surfaces would
	// otherwise disagree about what the export covers, and a package
	// that is in scope but not carried is exactly the silent loss R11
	// exists to prevent.
	//
	// The archives are produced BEFORE anything is published, so the
	// document that names them is complete before it is written.
	var (
		archives []*mpminternal.PackageExportArchive
		omitted  []mpminternal.SkillPackageExportOmitted
	)
	if packageBearing {
		concrete := getDBConcrete()
		if concrete == nil {
			return usererrorf("Export failed: the package export needs the concrete database manager")
		}
		exportedRows := skillIDsAmong(memories)
		toExport := make([]string, 0, len(scope.Packages))
		for _, candidate := range scope.Packages {
			if exportedRows[candidate.SkillID] {
				toExport = append(toExport, candidate.SkillID)
				continue
			}
			omitted = append(omitted, mpminternal.SkillPackageExportOmitted{
				SkillID:     candidate.SkillID,
				Name:        candidate.Name,
				Version:     candidate.Version,
				FileCount:   candidate.FileCount,
				PackageHash: candidate.PackageHash,
				Reason:      "skill_row_not_exported",
				Detail: "the package is in the export's scope, but its skill row is not among " +
					"the exported rows; the archive was not carried",
			})
		}
		produced, failures, err := concrete.ExportSkillPackages(context.Background(), toExport)
		if err != nil {
			return usererrorf("Export failed: %v", err)
		}
		archives = produced
		omitted = append(omitted, failures...)
	}

	// ── The LOSS MARKER. ───────────────────────────────────────────────
	//
	// Before P7 the marker named every package the export did not
	// carry, because the export carried none. Now the export CARRIES
	// the packages, so the marker names only the ones that could not be
	// carried — and the scope says how many were in scope, so a count
	// of zero is a positive statement about a KNOWN denominator rather
	// than an empty claim.
	packageLoss := &mpminternal.SkillPackageExportLoss{
		Count:    len(omitted),
		Packages: omitted,
		Scope: mpminternal.SkillPackageExportScope{
			Total:    scope.Total,
			Selected: len(scope.Packages),
			Carried:  len(archives),
		},
	}
	if packageLoss.Packages == nil {
		packageLoss.Packages = []mpminternal.SkillPackageExportOmitted{}
	}
	// The omissions are sorted GLOBALLY, by skill id. They arrive from
	// two sources — the scope's own omissions and the exporter's
	// failures — and each is sorted on its own, so without this the
	// two lists would interleave differently depending on which source
	// an omission came from. One order for the whole list makes the
	// report reproducible.
	sort.SliceStable(packageLoss.Packages, func(i, j int) bool {
		return packageLoss.Packages[i].SkillID < packageLoss.Packages[j].SkillID
	})

	// ── The DOCUMENT. ──────────────────────────────────────────────────
	//
	// It is rendered in full, in memory, BEFORE anything is published —
	// so a document is never truncated before its content is known to be
	// complete, and a failure leaves the previous output intact.
	var document []byte
	switch o.format {
	case "json":
		index := &packageExportIndex{
			Count:    len(archives),
			Archives: archives,
		}
		if packageBearing {
			index.Directory = filepath.Base(resolvePackageExportTarget(o.output).DirectoryPath)
		}
		document, err = encodePackageExportDocument(memories, packageLoss, index)
		if err != nil {
			return usererrorf("Export failed: %v", err)
		}
	case exportLegacyArrayFormat:
		// The DEPRECATED bare array — the shape the export had before
		// the loss marker existed. It is only reachable when there is
		// nothing to lose (the refusal above), so it is never a silent
		// loss.
		document, err = encodeExportJSON(nonNilMemories(memories))
		if err != nil {
			return usererrorf("Export failed: %v", err)
		}
	default:
		// CSV.
		document, err = encodeExportCSV(memories)
		if err != nil {
			return usererrorf("Export failed: %v", err)
		}
	}

	// ── PUBLISH. ───────────────────────────────────────────────────────
	//
	// The package-bearing path publishes the ARCHIVES and the document
	// together, through the staging-and-rename machinery. The ordinary
	// path writes the document alone — still atomically, so an existing
	// output is never truncated before its content is complete.
	if packageBearing {
		target := resolvePackageExportTarget(o.output)
		if err := publishPackageExport(target, archives, document); err != nil {
			return usererrorf("Export failed: %v", err)
		}
		usererror.Notice("Exported %d memories and %d package archive(s) to %s (archives in %s)",
			len(memories), len(archives), o.output, target.DirectoryPath)
	} else if o.output != "" {
		if err := writeFileAtomically(o.output, document); err != nil {
			return usererrorf("Export failed: %v", err)
		}
		usererror.Notice("Exported %d memories to %s", len(memories), o.output)
	} else {
		if _, err := os.Stdout.Write(document); err != nil {
			return usererrorf("Export failed: %v", err)
		}
		usererror.Notice("Exported %d memories", len(memories))
	}

	// The loss marker, on standard error so it never corrupts the
	// document on standard output — and loud enough that a caller who
	// never read the response still sees it.
	if notice := packageLoss.Notice(); notice != "" {
		usererror.Notice("%s", notice)
	}

	// The deprecation notice for the legacy shape. It goes to standard
	// error for the same reason the loss marker does: standard output
	// carries the document, and the document must stay parseable.
	if o.format == exportLegacyArrayFormat {
		usererror.Notice(
			"WARNING: --format %s is DEPRECATED and will be removed in a future release. "+
				"It emits the legacy bare array, which cannot carry the package loss marker. "+
				"Use --format json, which names every omitted package.",
			exportLegacyArrayFormat)
	}
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
		// Show memories for spaced reinforcement review.
		//
		// The canonical rule (internal/core/review.go) — the same one
		// the doctor and `mpm ops review` use. This used to call
		// GetSpacedReinforcementReview(*days, 20), which applied a
		// different eligibility rule (a never-accessed memory was due
		// immediately) and reported a nullable timestamp through a
		// formatter that rendered it as "106751d ago".
		memories, err := dm.MemoriesDueForReview(*days, 20)
		if err != nil {
			usererror.Error("%v", err)
			return 1
		}

		if len(memories) == 0 {
			fmt.Printf("No memories need reinforcement review (window: %d days)\n", *days)
			return 0
		}

		fmt.Printf("\n📋 Memories for Spaced Reinforcement Review (window: %d days):\n", *days)
		fmt.Println("───────────────────────────────────────────────────────────────")
		for i, m := range memories {
			content := m.Content
			if len(content) > 80 {
				content = content[:80] + "..."
			}
			// The age is honest: a memory that was never accessed says
			// so, and names its creation date instead of inventing an
			// age for it.
			last := "never accessed"
			if m.LastAccessedAt != nil {
				last = humanDaysAgo(*m.LastAccessedAt)
			} else {
				last = fmt.Sprintf("never accessed (created %s)", humanDaysAgo(m.CreatedAt))
			}
			fmt.Printf("%d. [w=%d reinforcement=%d last=%s] %s\n",
				i+1, m.Weight, m.ReinforcementCount, last, content)
		}
		return 0
	}

	fmt.Println("\n🧠 Running self-maintenance...")

	// Run full self-maintenance.
	//
	// The statistics and the error are NOT exclusive. A failed operation does
	// not make the numbers that DID succeed useless, so the partial statistics
	// are printed either way and the EXIT CODE carries the failure. This used
	// to print "complete" and exit 0 while decay, pruning or consolidation had
	// not run at all.
	stats, err := dm.RunSelfMaintenance()

	if err != nil && stats == nil {
		return usererror.Error("self-maintenance failed: %v", err)
	}

	if err != nil {
		fmt.Println("\n⚠ Self-maintenance completed WITH ERRORS:")
	} else {
		fmt.Println("\n✅ Self-maintenance complete:")
	}
	fmt.Printf("   Decayed weights:      %d\n", stats["decayed_weights"])
	fmt.Printf("   Pruned total:        %d\n", stats["pruned_total"])
	fmt.Printf("   Consolidated:         %d\n", stats["consolidated"])
	fmt.Printf("   Never accessed:      %d\n", stats["never_accessed"])
	fmt.Printf("   Low weight:          %d\n", stats["low_weight"])
	fmt.Printf("   For review:          %d\n", stats["for_spaced_reinforcement"])

	if err != nil {
		if failed, ok := stats["errors"].([]string); ok && len(failed) > 0 {
			fmt.Printf("   Failed operations:   %s\n", strings.Join(failed, ", "))
		}
		fmt.Println()
		return usererror.Error("self-maintenance failed: %v", err)
	}

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
