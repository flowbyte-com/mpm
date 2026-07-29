// cmd/mpm/handlers_info.go — `mpm info` identity printout.
//
// Architecture session Wed 2026-07-29: the cognitive-interface RFC
// ships three flagship inspectors:
//
//   mpm status   — live substrate dashboard
//   mpm doctor   — trust signals (✓/⚠/✗)
//   mpm info     — identity (what installation am I talking to?)
//
// `mpm info` answers a different question from `status` / `doctor`.
// The other two answer "is the substrate healthy right now?". `info`
// answers "what is this installation's identity?" — version,
// database location, model wiring, registered personas / skills,
// memory counts. The git-version / docker-info / kubectl-version
// shape: a single screen of static-or-slowly-changing facts.
//
// Architecture: handlers_info.go composes existing helpers —
// buildVersion (package var), dm.DBPath, dm.SQLDB(), GetMemoryStats,
// LoadActiveJSON, the scheduled_tasks table — without duplicating
// any query logic. It does NOT own SQL.

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/flowbyte-com/mpm-core/config"
	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// handleInfo prints installation identity to stdout.
//
//   mpm info [--json]
//
// --json reserved for a follow-up RFC; the dashboard output is the
// canonical form for human operators. The composed identity surface
// (version, db, models, scheduler, skills, persona, counts) is
// intentionally static — it answers "what installation am I talking
// to?" rather than "is the substrate healthy right now?" (that is
// what `mpm doctor` answers).
func handleInfo(args []string) int {
	for _, a := range args {
		switch a {
		case "--json", "-j":
			usererror.Error("--json not yet implemented for info (planned for follow-up RFC)")
			return 1
		default:
			usererror.Error("info: unknown flag %q", a)
			return 1
		}
	}

	dm := getDBConcrete()
	if dm == nil {
		usererror.Error("database unavailable")
		return 1
	}

	fmt.Println("MPM · Installation identity")
	fmt.Println()

	// Identity — version + paths.
	fmt.Println("Identity")
	fmt.Printf("  version          : %s\n", buildVersion)
	fmt.Printf("  data directory   : %s\n", config.GetMPMDir())
	fmt.Printf("  database path    : %s\n", dm.DBPath())
	fmt.Println()

	// Workspace — active mode / persona from active.json.
	fmt.Println("Workspace")
	if active, err := mpminternal.LoadActiveJSON(); err == nil {
		modes := "(none)"
		if len(active.Modes) > 0 {
			modes = strings.Join(active.Modes, ", ")
		}
		persona := "(none)"
		if active.Persona != "" {
			persona = active.Persona
		}
		fmt.Printf("  active modes     : %s\n", modes)
		fmt.Printf("  active persona   : %s\n", persona)
		fmt.Printf("  active updated   : %s\n", active.Updated)
	} else {
		fmt.Println("  active modes     : (no active.json — defaults apply)")
		fmt.Println("  active persona   : (default)")
	}
	fmt.Println()

	// Database shape + memory counts.
	fmt.Println("Database")
	fmt.Printf("  path             : %s\n", dm.DBPath())
	if _, err := dm.HealthCheck(); err != nil {
		fmt.Printf("  health           : ⚠ %v\n", err)
	} else {
		fmt.Printf("  health           : ✓ ok\n")
	}
	stats, err := dm.GetMemoryStats()
	if err != nil {
		fmt.Println("  stats            : (unavailable)")
	} else {
		for _, k := range []string{"total", "active", "ltm", "deleted", "never_accessed", "expired"} {
			if v, ok := stats[k]; ok && v != nil {
				fmt.Printf("  %-16s : %v\n", k, v)
			}
		}
	}
	fmt.Println()

	// Skills.
	fmt.Println("Skills")
	skills := listSkillsForInfo(dm)
	if len(skills) == 0 {
		fmt.Println("  registered skills: (none)")
	} else {
		fmt.Printf("  registered skills: %d\n", len(skills))
		for _, s := range skills {
			fmt.Printf("    - %s\n", s)
		}
	}
	fmt.Println()

	// Scheduler.
	fmt.Println("Scheduler")
	pid := readSchedulerPID()
	if pid > 0 {
		fmt.Printf("  mpm-scheduler    : running (pid %d)\n", pid)
	} else {
		fmt.Println("  mpm-scheduler    : not running (start with 'systemctl --user start mpm-scheduler')")
	}
	wakes := listScheduledTasksForInfo(dm)
	if len(wakes) == 0 {
		fmt.Println("  scheduled tasks  : (none)")
	} else {
		fmt.Printf("  scheduled tasks  : %d\n", len(wakes))
		for _, w := range wakes {
			fmt.Printf("    - %s (next: %s)\n", w.id, w.nextRun)
		}
	}
	fmt.Println()

	// Runtime identity.
	host, _ := os.Hostname()
	if host == "" {
		host = "(unknown)"
	}
	fmt.Println("Runtime")
	fmt.Printf("  hostname         : %s\n", host)
	fmt.Printf("  pid              : %d\n", os.Getpid())
	fmt.Println()
	return 0
}

// listSkillsForInfo returns skill names (best-effort; never blocks).
func listSkillsForInfo(dm *mpminternal.DatabaseManager) []string {
	if dm == nil {
		return nil
	}
	rows, err := dm.QueryTracked(
		`SELECT id, name FROM memories WHERE collection = 'skills' AND deleted_at IS NULL
		 ORDER BY created_at DESC LIMIT 25`,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err == nil {
			if name != "" {
				out = append(out, name)
			} else {
				out = append(out, id)
			}
		}
	}
	return out
}

// scheduledTaskInfo is the small shape for the info display.
type scheduledTaskInfo struct {
	id      string
	nextRun string
}

// listScheduledTasksForInfo returns the configured scheduled tasks.
// Best-effort — fails silently if the table isn't there yet.
func listScheduledTasksForInfo(dm *mpminternal.DatabaseManager) []scheduledTaskInfo {
	if dm == nil {
		return nil
	}
	rows, err := dm.QueryTracked(
		`SELECT id, next_run_at FROM scheduled_tasks WHERE status = 'active'
		 ORDER BY next_run_at ASC LIMIT 25`,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []scheduledTaskInfo{}
	for rows.Next() {
		var id, nextRun string
		if err := rows.Scan(&id, &nextRun); err == nil {
			out = append(out, scheduledTaskInfo{id: id, nextRun: nextRun})
		}
	}
	return out
}

// readSchedulerPID introspects the user-level systemd unit to read
// the current mpm-scheduler MainPID. Returns 0 on failure (caller
// renders the not-running state).
func readSchedulerPID() int {
	out, err := exec.Command("systemctl", "--user", "is-active", "mpm-scheduler").Output()
	if err != nil || strings.TrimSpace(string(out)) != "active" {
		return 0
	}
	pidOut, err := exec.Command("systemctl", "--user", "show",
		"mpm-scheduler", "--property=MainPID", "--value").Output()
	if err != nil {
		return 0
	}
	var pid int
	_, _ = fmt.Sscanf(strings.TrimSpace(string(pidOut)), "%d", &pid)
	return pid
}
