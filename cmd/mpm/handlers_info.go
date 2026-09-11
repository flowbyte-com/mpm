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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/flowbyte-com/mpm-core/config"
	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// infoOutput is the JSON shape for `mpm info --json`. The human-readable
// rendering and the JSON payload share a single data-collection pass
// (collectInfo / renderInfoHuman) so the two cannot drift.
//
// Fields mirror the section order of the human output: identity,
// workspace, database, skills, scheduler, runtime. Stats are kept
// as a generic map because they evolve as new counters land; the
// `null` for "unavailable" matches the human "(unavailable)" line.
type infoOutput struct {
	Version    string                 `json:"version"`
	DataDir    string                 `json:"data_directory"`
	DBPath     string                 `json:"db_path"`
	Workspace  infoWorkspace          `json:"workspace"`
	Database   infoDatabase           `json:"database"`
	Skills     infoSkills             `json:"skills"`
	Scheduler  infoScheduler          `json:"scheduler"`
	Runtime    infoRuntime            `json:"runtime"`
}

type infoWorkspace struct {
	// ActiveModes is the canonical plural representation of the
	// multi-mode selection. Replaces the pre-2026-09-11 comma-joined
	// singular string. New consumers should use this list directly.
	ActiveModes    []string `json:"active_modes"`
	// ActiveModeSource is the aggregate resolver source for the
	// multi-mode selection ("explicit" | "fallback" | "empty").
	ActiveModeSource   string   `json:"active_mode_source,omitempty"`
	ActivePersona  string   `json:"active_persona"`
	ActivePersonaSource string `json:"active_persona_source,omitempty"`
	ActiveUpdated  string   `json:"active_updated,omitempty"`
	ActiveJSONLoad string   `json:"active_json_load,omitempty"` // error string when active.json failed
}

type infoDatabase struct {
	Path   string                 `json:"path"`
	Health string                 `json:"health"`            // "ok" or "degraded"
	Error  string                 `json:"error,omitempty"`
	Stats  map[string]interface{} `json:"stats,omitempty"`   // may be nil if unavailable
}

type infoSkills struct {
	Count int      `json:"count"`
	Names []string `json:"names"`
}

type infoScheduler struct {
	Running        bool                `json:"running"`
	PID            int                 `json:"pid,omitempty"`
	ScheduledTasks []infoScheduledTask `json:"scheduled_tasks"`
}

type infoScheduledTask struct {
	ID      string `json:"id"`
	NextRun string `json:"next_run"`
}

type infoRuntime struct {
	Hostname string `json:"hostname"`
	PID      int    `json:"pid"`
}

// handleInfo prints installation identity to stdout.
//
//	mpm info [--json]
//
// The JSON payload and the human-readable form share one data
// collection pass; the two cannot drift. Unknown flags other than
// --json / -j still hard-fail (consistent with the pre-alpha
// behavior) so a typo doesn't silently no-op.
func handleInfo(args []string) int {
	jsonFlagSeen, preprocessed := ExtractJSONFlag(args)
	jsonOutput := jsonFlagSeen
	for _, a := range preprocessed {
		if a == "--help" || a == "-h" {
			fmt.Println("Usage: mpm info [--json]")
			fmt.Println()
			fmt.Println("Print installation identity (version, paths, active mode/persona,")
			fmt.Println("database stats, registered skills, scheduler status, runtime).")
			fmt.Println()
			fmt.Println("Flags:")
			fmt.Println("  --json, -j    Emit JSON to stdout for machine consumption.")
			return 0
		}
		usererror.Error("info: unknown flag %q", a)
		return 1
	}

	dm := getDBConcrete()
	if dm == nil {
		usererror.Error("database unavailable")
		return 1
	}

	out := collectInfo(dm)

	if jsonOutput {
		return emitInfoJSON(out)
	}
	renderInfoHuman(out)
	return 0
}

// collectInfo gathers every fact the info command renders. The
// helper is the single source of truth for both the human and
// JSON renderers — adding a new field happens here, once.
func collectInfo(dm *mpminternal.DatabaseManager) infoOutput {
	host, _ := os.Hostname()
	if host == "" {
		host = "(unknown)"
	}

	out := infoOutput{
		Version: buildVersion,
		DataDir: config.GetMPMDir(),
		DBPath:  dm.DBPath(),
		Workspace: infoWorkspace{
			ActiveModes:   []string{},
			ActivePersona: "",
		},
		Database: infoDatabase{
			Path:   dm.DBPath(),
			Health: "ok",
		},
		Skills: infoSkills{
			Names: []string{},
		},
		Scheduler: infoScheduler{
			ScheduledTasks: []infoScheduledTask{},
		},
		Runtime: infoRuntime{
			Hostname: host,
			PID:      os.Getpid(),
		},
	}

	// Workspace — active mode / persona from active.json, resolved via
	// the canonical resolver. Plural modes + per-field source vocab.
	if active, err := mpminternal.LoadActiveJSON(); err == nil {
		modeRes := mpminternal.ResolveActiveModes(dm, active.Modes)
		out.Workspace.ActiveModes = modeRes.Names()
		out.Workspace.ActiveModeSource = modeRes.Source
		personaRes := mpminternal.ResolveActivePersonaIntent(dm, active.Persona)
		out.Workspace.ActivePersona = personaRes.Name
		out.Workspace.ActivePersonaSource = personaRes.Source
		out.Workspace.ActiveUpdated = active.Updated
	} else {
		out.Workspace.ActiveJSONLoad = err.Error()
	}

	// Database shape + memory counts.
	if _, err := dm.HealthCheck(); err != nil {
		out.Database.Health = "degraded"
		out.Database.Error = err.Error()
	}
	if stats, err := dm.GetMemoryStats(); err == nil {
		out.Database.Stats = stats
	}

	// Skills.
	if names := listSkillsForInfo(dm); len(names) > 0 {
		out.Skills.Count = len(names)
		out.Skills.Names = names
	}

	// Scheduler.
	if pid := readSchedulerPID(); pid > 0 {
		out.Scheduler.Running = true
		out.Scheduler.PID = pid
	}
	if tasks := listScheduledTasksForInfo(dm); len(tasks) > 0 {
		for _, t := range tasks {
			out.Scheduler.ScheduledTasks = append(out.Scheduler.ScheduledTasks, infoScheduledTask{
				ID:      t.id,
				NextRun: t.nextRun,
			})
		}
	}

	return out
}

// renderInfoHuman prints the canonical human-readable form. It
// intentionally mirrors the JSON section ordering one-to-one so the
// two surfaces stay co-aligned.
func renderInfoHuman(out infoOutput) {
	fmt.Println("MPM · Installation identity")
	fmt.Println()

	fmt.Println("Identity")
	fmt.Printf("  version          : %s\n", out.Version)
	fmt.Printf("  data directory   : %s\n", out.DataDir)
	fmt.Printf("  database path    : %s\n", out.DBPath)
	fmt.Println()

	fmt.Println("Workspace")
	if len(out.Workspace.ActiveModes) == 0 {
		fmt.Printf("  active modes     : <none>%s\n", sourceTag(out.Workspace.ActiveModeSource))
	} else {
		fmt.Printf("  active modes     : %s%s\n",
			strings.Join(out.Workspace.ActiveModes, ", "),
			sourceTag(out.Workspace.ActiveModeSource))
	}
	if out.Workspace.ActivePersona == "" {
		fmt.Printf("  active persona   : <none>%s\n", sourceTag(out.Workspace.ActivePersonaSource))
	} else {
		fmt.Printf("  active persona   : %s%s\n",
			out.Workspace.ActivePersona,
			sourceTag(out.Workspace.ActivePersonaSource))
	}
	if out.Workspace.ActiveUpdated != "" {
		fmt.Printf("  active updated   : %s\n", out.Workspace.ActiveUpdated)
	} else if out.Workspace.ActiveJSONLoad != "" {
		fmt.Printf("  active.json load : ⚠ %s\n", out.Workspace.ActiveJSONLoad)
	}
	fmt.Println()

	fmt.Println("Database")
	fmt.Printf("  path             : %s\n", out.Database.Path)
	if out.Database.Health == "ok" {
		fmt.Printf("  health           : ✓ ok\n")
	} else {
		fmt.Printf("  health           : ⚠ %s\n", out.Database.Error)
	}
	if out.Database.Stats != nil {
		for _, k := range []string{"total", "active", "ltm", "deleted", "never_accessed", "expired"} {
			if v, ok := out.Database.Stats[k]; ok && v != nil {
				fmt.Printf("  %-16s : %v\n", k, v)
			}
		}
	} else {
		fmt.Println("  stats            : (unavailable)")
	}
	fmt.Println()

	fmt.Println("Skills")
	if out.Skills.Count == 0 {
		fmt.Println("  registered skills: (none)")
	} else {
		fmt.Printf("  registered skills: %d\n", out.Skills.Count)
		for _, s := range out.Skills.Names {
			fmt.Printf("    - %s\n", s)
		}
	}
	fmt.Println()

	fmt.Println("Scheduler")
	if out.Scheduler.Running {
		fmt.Printf("  mpm-scheduler    : running (pid %d)\n", out.Scheduler.PID)
	} else {
		fmt.Println("  mpm-scheduler    : not running (start with 'systemctl --user start mpm-scheduler')")
	}
	if len(out.Scheduler.ScheduledTasks) == 0 {
		fmt.Println("  scheduled tasks  : (none)")
	} else {
		fmt.Printf("  scheduled tasks  : %d\n", len(out.Scheduler.ScheduledTasks))
		for _, t := range out.Scheduler.ScheduledTasks {
			fmt.Printf("    - %s (next: %s)\n", t.ID, t.NextRun)
		}
	}
	fmt.Println()

	fmt.Println("Runtime")
	fmt.Printf("  hostname         : %s\n", out.Runtime.Hostname)
	fmt.Printf("  pid              : %d\n", out.Runtime.PID)
	fmt.Println()
}

// emitInfoJSON renders the typed payload as indented JSON on stdout.
// Machine consumers pipe this directly; human readers see indented
// output via `jq .` or `cat`.
func emitInfoJSON(out infoOutput) int {
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		usererror.Error("marshal info: %v", err)
		return 1
	}
	fmt.Println(string(b))
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
		if err := rows.Scan(&id, &name); err != nil {
			usererror.Warn("listSkillsForInfo: failed to scan skill row, skipping: %v", err)
			continue
		}
		if name != "" {
			out = append(out, name)
		} else {
			out = append(out, id)
		}
	}
	return out
}

// scheduledTaskInfo is the small shape for the info display.
type scheduledTaskInfo struct {
	id      string
	nextRun string
}

// sourceTag formats the resolver source for human display. Empty or
// "explicit" source renders as no tag (the common case is no tag).
func sourceTag(s string) string {
	if s == "" || s == "explicit" {
		return ""
	}
	return " [" + s + "]"
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
		if err := rows.Scan(&id, &nextRun); err != nil {
			usererror.Warn("listScheduledTasksForInfo: failed to scan task row, skipping: %v", err)
			continue
		}
		out = append(out, scheduledTaskInfo{id: id, nextRun: nextRun})
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
