// handlers_milestones.go — `mpm ops milestones` engine-room command.
//
// Surfaces the milestone landscape to the human operator: lists recent
// commitments by flavor (shipped / insight), scoped to the same 30-day
// rolling window the wake-context uses, with the same JSON-encoded
// tag-substring query. Symmetric with the MCP wire surface: anything
// visible here is also visible in read_wake_context's Recent Milestones
// block, and vice versa.
//
// The verb set is intentionally minimal: list is enough for now. If a
// milestone needs to be retracted, the operator uses shred_memory with
// the returned id — same primitive the rest of the memory surface uses.
// No bespoke "revert milestone" path: a milestone is a memory with a
// particular tag, and the memory substrate handles mutation.

package main

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	mpminternal "mpm/internal"
	"mpm/internal/usererror"
)

// MilestoneDaysWindow is the rolling-window scope for the listing.
// Matches the wake-context query in internal/wake_context.go so the CLI
// and the agent's wake surface see exactly the same horizon. Tunable from
// one place if the envelope ever needs to grow.
const MilestoneDaysWindow = 30

// MilestoneMaxList is the hard cap on rows returned. Matches the wake-
// context budget (5 tactical + 5 strategic — milestones get the strategic
// half). Larger lists belong in a paginated query, which doesn't exist
// yet. Operators who need >5 should narrow with `mpm ops milestones --flavor X`
// or query FTS5 directly via search_lessons / query_long_term_memory.
const MilestoneMaxList = 5

// milestoneFlavorPattern extracts the flavor suffix from a stored tag.
// Captures everything after `type:milestone-`. Invalid flavors (e.g.
// `type:milestone-` with no suffix) pass through as the empty string and
// the row is reported as "[untagged]".
var milestoneFlavorPattern = regexp.MustCompile(`type:milestone-([a-zA-Z0-9_-]+)`)

func handleOpsMilestones(args []string) int {
	// Tiny flag parser — by-flavor filter is the only operator need.
	// Pattern matches the rest of the codebase: short flags, no external
	// flag library, defaults that work for the common case.
	flavorFilter := ""
	daysWindow := MilestoneDaysWindow
	maxRows := MilestoneMaxList

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h" || a == "--help" || a == "help":
			printMilestonesHelp()
			return 0
		case a == "--flavor" && i+1 < len(args):
			flavorFilter = args[i+1]
			i++
		case strings.HasPrefix(a, "--flavor="):
			flavorFilter = strings.TrimPrefix(a, "--flavor=")
		case a == "--days" && i+1 < len(args):
			fmt.Sscanf(args[i+1], "%d", &daysWindow)
			i++
		case a == "--limit" && i+1 < len(args):
			fmt.Sscanf(args[i+1], "%d", &maxRows)
			i++
		}
	}

	if maxRows > 50 {
		usererror.Warn("--limit capped at 50 (got %d)", maxRows)
		maxRows = 50
	}
	if maxRows <= 0 {
		maxRows = MilestoneMaxList
	}

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		usererror.Error("DB open failed: %v", err)
	}
	defer dm.Close()

	rows, err := dm.SQLDB().Query(`
		SELECT id, content, created_at, tags
		FROM memories
		WHERE deleted_at IS NULL
		  AND tags LIKE ?
		  AND created_at > datetime('now', ?)
		ORDER BY created_at DESC
		LIMIT ?`,
		`%type:milestone-%`+`"`+`%`,
		fmt.Sprintf("-%d days", daysWindow),
		maxRows,
	)
	if err != nil {
		usererror.Error("milestone query failed: %v", err)
	}
	defer rows.Close()

	type entry struct {
		id        string
		content   string
		createdAt string
		flavor    string
	}
	var entries []entry
	for rows.Next() {
		var e entry
		var tagsJSON string
		if err := rows.Scan(&e.id, &e.content, &e.createdAt, &tagsJSON); err != nil {
			continue
		}
		e.flavor = extractFlavor(tagsJSON)
		if flavorFilter != "" && e.flavor != flavorFilter {
			continue
		}
		entries = append(entries, e)
	}

	fmt.Println("Recent Milestones")
	fmt.Println("─────────────────")
	if len(entries) == 0 {
		fmt.Printf("  (none in the last %d days)\n", daysWindow)
		if flavorFilter != "" {
			fmt.Printf("  filter: flavor=%s\n", flavorFilter)
		}
		fmt.Println()
		fmt.Println("Milestones are written via `commit_milestone` (MCP) or `mpm call commit_milestone` (CLI).")
		fmt.Println("Each one carries a type:milestone-* tag; this view queries that anchor.")
		return 0
	}

	fmt.Printf("  window:  %d days\n", daysWindow)
	fmt.Printf("  showing: %d milestone(s)\n", len(entries))
	if flavorFilter != "" {
		fmt.Printf("  filter:  flavor=%s\n", flavorFilter)
	}
	fmt.Println()
	for i, e := range entries {
		_ = i
		// Content truncation symmetric with wake-context (120 chars + ellipsis).
		content := e.content
		if len(content) > 120 {
			content = content[:120] + "…"
		}
		// Age suffix from created_at. Format: keep date only unless
		// exactly today, in which case include time too.
		age := formatMilestoneAge(e.createdAt)
		fmt.Printf("  [%s]  %s  (%s)\n", e.flavor, content, age)
		fmt.Printf("          id=%s\n", e.id)
	}
	fmt.Println()
	fmt.Println("Tactical:  delete via `mpm call shred_memory --payload '{\"memory_id\": \"<id>\"}'`.")
	fmt.Println("Strategic: same primitive — a milestone is a memory with a tag.")

	return 0
}

func printMilestonesHelp() {
	fmt.Println("Usage: mpm ops milestones [--flavor shipped|insight] [--days N] [--limit N]")
	fmt.Println()
	fmt.Println("Lists recent narrative milestones — memories tagged with type:milestone-*.")
	fmt.Println("Mirrors the wake-context 'Recent Milestones' block exactly: same 30-day")
	fmt.Println("rolling window, same JSON-tag anchor, same flavor taxonomy.")
	fmt.Println()
	fmt.Println("Flags:")
	fmt.Println("  --flavor <shipped|insight>  filter by milestone flavor")
	fmt.Println("  --days    <N>               rolling window in days (default 30)")
	fmt.Println("  --limit   <N>               max rows to return (default 5, capped at 50)")
	fmt.Println("  -h, --help                  show this help")
	fmt.Println()
	fmt.Println("Write counterpart: `mpm call commit_milestone --payload '{\"summary\":\"...\",\"flavor\":\"shipped\"}'`")
	fmt.Println("Summary must be ≥50 chars; flavor defaults to 'shipped' if omitted.")
}

// extractFlavor pulls the flavor suffix from a JSON-encoded tags column.
// The stored format is `["type:milestone-<flavor>", ...]` — a JSON array
// of strings. Returns "untagged" if no `type:milestone-` tag is present
// (e.g. a manually-crafted memory row).
func extractFlavor(tagsJSON string) string {
	if tagsJSON == "" {
		return "untagged"
	}
	m := milestoneFlavorPattern.FindStringSubmatch(tagsJSON)
	if len(m) < 2 {
		return "untagged"
	}
	return m[1]
}

// formatMilestoneAge returns a human-readable age string from an ISO-8601
// timestamp. "today HH:MM", "yesterday", "Nd ago", or the bare date for
// older entries. Drops to seconds precision only when it's the same day;
// the wake-context renderer needs only date precision and this CLI can
// afford the extra detail.
//
// Named formatMilestoneAge (not formatAge) to avoid symbol collision with
// recall.go which already exports a formatAge taking time.Time.
func formatMilestoneAge(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		// Fall back to the date prefix — matches what wake-context shows.
		if len(ts) >= 10 {
			return ts[:10]
		}
		return ts
	}
	now := time.Now()
	dur := now.Sub(t)
	switch {
	case dur < 24*time.Hour && t.Day() == now.Day() && t.Month() == now.Month() && t.Year() == now.Year():
		return fmt.Sprintf("today %s", t.Format("15:04"))
	case dur < 48*time.Hour:
		return "yesterday"
	case dur < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(dur.Hours()/24))
	default:
		return t.Format("2006-01-02")
	}
}
