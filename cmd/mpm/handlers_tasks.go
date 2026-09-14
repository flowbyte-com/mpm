// handlers_tasks.go — mpm tasks CLI for Agentic Cron (Phase 4).
//
// Usage:
//   mpm tasks upsert <id> <name> <cron_expr> <directive_id> [status]
//   mpm tasks list
//   mpm tasks delete <id>
//
// Mirrors the three split MCP tools (upsert_scheduled_task,
// list_scheduled_tasks, delete_scheduled_task) so the CLI surface and
// the agent surface stay in lockstep. The fail-fast directive check
// from the MCP handler runs here too.
//
// Output conventions: structured help text uses fmt.Println (it's not
// an error message — it's documentation). Error paths use
// usererror.{Error,Warn,Usage} per the cmd/mpm TestNoNewDirectStderrWrites
// regression guard.

package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/usererror"

	"github.com/flowbyte-com/mpm/cmd/mpm/render"
)

// handleTasksCommand is the router entry for `mpm tasks <subcommand>`.
func handleTasksCommand(args []string) int {
	if len(args) < 1 {
		printTasksHelp()
		return 0
	}
	switch args[0] {
	case "upsert":
		return handleTasksUpsert(args[1:])
	case "list":
		return handleTasksList(args[1:])
	case "delete", "rm":
		return handleTasksDelete(args[1:])
	case "help", "-h", "--help":
		printTasksHelp()
		return 0
	default:
		usererror.Error("Unknown tasks subcommand: %q", args[0])
		return 1
	}
}

func printTasksHelp() {
	fmt.Println("Usage: mpm tasks <subcommand> [args]")
	fmt.Println()
	fmt.Println("Subcommands:")
	fmt.Println("  upsert <id> <name> <cron_expr> <directive_id> [status]")
	fmt.Println("      Create or update a recurring task. Status defaults to 'active';")
	fmt.Println("      set to 'paused' to halt execution without losing the schedule.")
	fmt.Println("  list")
	fmt.Println("      List all scheduled tasks ordered by next_run_at ASC.")
	fmt.Println("  delete <id>")
	fmt.Println("      Hard-delete a task. Prefer 'upsert ... paused' for soft-stop.")
	fmt.Println()
	fmt.Println("Cron syntax: standard 5-field (minute hour dom month dow).")
	fmt.Println("Examples:")
	fmt.Println("  '0 3 * * *'    Daily at 03:00 UTC")
	fmt.Println("  '0 0 * * 1'    Weekly on Monday at midnight")
	fmt.Println("  '*/15 * * * *' Every 15 minutes")
}

func handleTasksUpsert(args []string) int {
	if len(args) < 4 {
		usererror.Usage("mpm tasks upsert <id> <name> <cron_expr> <directive_id> [status]")
		return 1
	}
	id := args[0]
	name := args[1]
	cronExpr := args[2]
	directiveID := args[3]
	status := mpminternal.ScheduledTaskActive
	if len(args) >= 5 {
		status = args[4]
		if status != mpminternal.ScheduledTaskActive && status != mpminternal.ScheduledTaskPaused {
			usererror.Error("invalid status %q: must be 'active' or 'paused'", status)
			return 1
		}
	}

	dm := getDB()
	if dm == nil {
		return 1
	}

	// Fail-fast directive check (mirrors the MCP handler).
	sqlDB := dm.SQLDB()
	var found string
	row := sqlDB.QueryRow(
		`SELECT id FROM memories WHERE id = ? AND collection = 'directives' AND deleted_at IS NULL LIMIT 1`,
		directiveID,
	)
	if err := row.Scan(&found); err != nil {
		usererror.Error("directive_id %q not found in memories where collection='directives'", directiveID)
		return 1
	}

	task := mpminternal.ScheduledTask{
		ID:          id,
		Name:        name,
		CronExpr:    cronExpr,
		DirectiveID: directiveID,
		Status:      status,
	}
	if err := dm.UpsertScheduledTask(task); err != nil {
		usererror.Error("%v", err)
		return 1
	}
	fmt.Printf("✓ %s: %s (cron=%s, directive=%s, status=%s)\n",
		id, name, cronExpr, directiveID, status)
	return 0
}

func handleTasksList(args []string) int {
	dm := getDB()
	if dm == nil {
		return 1
	}
	tasks, err := dm.ListScheduledTasks()
	if err != nil {
		usererror.Error("%v", err)
		return 1
	}
	if len(tasks) == 0 {
		// 2026-09-14 release-pass: canonical visual grammar.
		render.Heading(os.Stdout, "Tasks")
		render.Plain(os.Stdout, "No scheduled tasks. Use `mpm tasks upsert` to add one.")
		return 0
	}

	// 2026-09-14 release-pass: heading via the canonical renderer,
	// tabwriter retained for column alignment. The release-pass
	// requirement is "preserve existing JSON contracts, not
	// manufacture new ones" — no --json flag is added here.
	render.Heading(os.Stdout, "Tasks")
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tCRON\tDIRECTIVE_ID\tSTATUS\tNEXT RUN (UTC)\tLAST RUN (UTC)")
	fmt.Fprintln(w, "----\t----\t----\t-----------\t------\t--------------\t--------------")
	for _, t := range tasks {
		// Defect K (2026-09-13 acceptance): pre-fix the renderer
		// overwrote NEXT RUN with LAST RUN when the task had ever
		// fired. Operators saw "next run" actually be the past
		// execution time, with a "(last)" suffix that read as a
		// label rather than a correction. The fix: NEXT RUN shows
		// NextRunAt always; LAST RUN is a separate column shown
		// only when the task has fired.
		nextRun := mpminternal.FormatUnixSeconds(t.NextRunAt)
		var lastRun string
		if t.LastRunAt != nil {
			lastRun = mpminternal.FormatOptionalUnixSeconds(t.LastRunAt)
		} else {
			lastRun = "—"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			t.ID, t.Name, t.CronExpr, t.DirectiveID, t.Status, nextRun, lastRun)
	}
	w.Flush()
	return 0
}

func handleTasksDelete(args []string) int {
	if len(args) < 1 {
		usererror.Usage("mpm tasks delete <id>")
		return 1
	}
	id := args[0]
	dm := getDB()
	if dm == nil {
		return 1
	}
	if err := dm.DeleteScheduledTask(id); err != nil {
		if strings.Contains(err.Error(), "no such") {
			usererror.Error("task %q not found", id)
		} else {
			usererror.Error("%v", err)
		}
		return 1
	}
	fmt.Printf("✓ deleted %s\n", id)
	return 0
}