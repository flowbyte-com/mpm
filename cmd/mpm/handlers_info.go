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

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/config"
	"github.com/flowbyte-com/mpm-core/usererror"

	"github.com/flowbyte-com/mpm/cmd/mpm/render"
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
	Version   string        `json:"version"`
	DataDir   string        `json:"data_directory"`
	DBPath    string        `json:"db_path"`
	Workspace infoWorkspace `json:"workspace"`
	Database  infoDatabase  `json:"database"`
	Skills    infoSkills    `json:"skills"`
	Scheduler infoScheduler `json:"scheduler"`
	Runtime   infoRuntime   `json:"runtime"`
}

type infoWorkspace struct {
	// ActiveModes is the canonical plural representation of the
	// multi-mode selection. Replaces the pre-2026-09-11 comma-joined
	// singular string. New consumers should use this list directly.
	ActiveModes []string `json:"active_modes"`
	// ActiveModeSource is the aggregate resolver source for the
	// multi-mode selection ("explicit" | "fallback" | "empty").
	ActiveModeSource    string `json:"active_mode_source,omitempty"`
	ActivePersona       string `json:"active_persona"`
	ActivePersonaSource string `json:"active_persona_source,omitempty"`
	ActiveUpdated       string `json:"active_updated,omitempty"`
	ActiveJSONLoad      string `json:"active_json_load,omitempty"` // error string when active.json failed
}

type infoDatabase struct {
	Path   string                 `json:"path"`
	Health string                 `json:"health"` // "ok" or "degraded"
	Error  string                 `json:"error,omitempty"`
	Stats  map[string]interface{} `json:"stats,omitempty"` // may be nil if unavailable
	// Directives is the live count of active directives. Computed
	// via the canonical countDirectives helper so the human form
	// agrees with `mpm status`'s `Directives : N active` row.
	// Go-only: NOT serialized to JSON (json:"-") because adding a
	// new key to the existing stats map would change the machine-
	// readable shape. Future consumers that need the directives
	// count programmatically can call `mpm call mpm_context
	// read_directives` directly.
	Directives int `json:"-"`
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
	// Active-directives count via the same canonical helper
	// `mpm status` uses (cmd/mpm/handlers_status.go::countDirectives).
	// Reusing it here means the two surfaces cannot drift on
	// directive accounting — same NULL-safe predicate, same
	// scope (active = non-deleted + non-expired).
	if n, err := countDirectives(dm); err == nil {
		out.Database.Directives = n
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
// two surfaces stay co-aligned. Rendered through the shared render
// package — no inline lipgloss, no ad-hoc styling.
func renderInfoHuman(out infoOutput) {
	render.Heading(os.Stdout, "Installation identity")
	render.BlankLine(os.Stdout)

	render.Section(os.Stdout, "Identity")
	render.Label(os.Stdout, "version", out.Version)
	render.Label(os.Stdout, "data directory", out.DataDir)
	render.Label(os.Stdout, "database path", out.DBPath)
	render.BlankLine(os.Stdout)

	render.Section(os.Stdout, "Workspace")
	if len(out.Workspace.ActiveModes) == 0 {
		render.Label(os.Stdout, "active modes", "<none>"+sourceTag(out.Workspace.ActiveModeSource))
	} else {
		render.Label(os.Stdout, "active modes", strings.Join(out.Workspace.ActiveModes, ", ")+sourceTag(out.Workspace.ActiveModeSource))
	}
	if out.Workspace.ActivePersona == "" {
		render.Label(os.Stdout, "active persona", "<none>"+sourceTag(out.Workspace.ActivePersonaSource))
	} else {
		render.Label(os.Stdout, "active persona", out.Workspace.ActivePersona+sourceTag(out.Workspace.ActivePersonaSource))
	}
	if out.Workspace.ActiveUpdated != "" {
		render.Label(os.Stdout, "active updated", out.Workspace.ActiveUpdated)
	} else if out.Workspace.ActiveJSONLoad != "" {
		render.Label(os.Stdout, "active.json load", "⚠ "+out.Workspace.ActiveJSONLoad)
	}
	render.BlankLine(os.Stdout)

	render.Section(os.Stdout, "Database")
	render.Label(os.Stdout, "path", out.Database.Path)
	if out.Database.Health == "ok" {
		render.Label(os.Stdout, "health", "✓ ok")
	} else {
		render.Label(os.Stdout, "health", "⚠ "+out.Database.Error)
	}
	// Memory stats use the humanized labels and combined
	// "N total | N active | N LTM" line so the bare `total`
	// count doesn't look like "the database is empty" when it
	// really means "no ordinary memories" — they're two
	// different things. Directives are a separate artifact
	// class with their own count line, mirroring the dashboard.
	if out.Database.Stats != nil {
		total, _ := out.Database.Stats["total"].(int)
		active, _ := out.Database.Stats["active"].(int)
		ltm, _ := out.Database.Stats["ltm"].(int)
		deleted, _ := out.Database.Stats["deleted"].(int)
		neverAccessed, _ := out.Database.Stats["never_accessed"].(int)
		expired, _ := out.Database.Stats["expired"].(int)
		render.Label(os.Stdout, "memories", fmt.Sprintf("%d total | %d active | %d LTM", total, active, ltm))
		render.Label(os.Stdout, "directives", fmt.Sprintf("%d active", out.Database.Directives))
		render.Label(os.Stdout, "deleted memories", fmt.Sprintf("%d", deleted))
		render.Label(os.Stdout, "never accessed", fmt.Sprintf("%d", neverAccessed))
		render.Label(os.Stdout, "expired", fmt.Sprintf("%d", expired))
	} else {
		render.Label(os.Stdout, "stats", "(unavailable)")
	}
	render.BlankLine(os.Stdout)

	render.Section(os.Stdout, "Skills")
	if out.Skills.Count == 0 {
		render.Label(os.Stdout, "registered skills", "(none)")
	} else {
		render.Label(os.Stdout, "registered skills", fmt.Sprintf("%d", out.Skills.Count))
		for _, s := range out.Skills.Names {
			render.Plainf(os.Stdout, "    - %s\n", s)
		}
	}
	render.BlankLine(os.Stdout)

	render.Section(os.Stdout, "Scheduler")
	if out.Scheduler.Running {
		render.Label(os.Stdout, "mpm-scheduler", fmt.Sprintf("running (pid %d)", out.Scheduler.PID))
	} else {
		render.Label(os.Stdout, "mpm-scheduler", "not running (start with 'systemctl --user start mpm-scheduler')")
	}
	if len(out.Scheduler.ScheduledTasks) == 0 {
		render.Label(os.Stdout, "scheduled tasks", "(none)")
	} else {
		render.Label(os.Stdout, "scheduled tasks", fmt.Sprintf("%d", len(out.Scheduler.ScheduledTasks)))
		for _, t := range out.Scheduler.ScheduledTasks {
			render.Plainf(os.Stdout, "    - %s (next: %s)\n", t.ID, t.NextRun)
		}
	}
	render.BlankLine(os.Stdout)

	render.Section(os.Stdout, "Runtime")
	render.Label(os.Stdout, "hostname", out.Runtime.Hostname)
	render.Label(os.Stdout, "pid", fmt.Sprintf("%d", out.Runtime.PID))
	render.BlankLine(os.Stdout)
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
//
// 2026-09-14 release-pass: now uses `dm.ListSkills(scope)` — the
// same parse-aware, dedup-by-name source that `mpm skill list`
// uses. Pre-fix this function issued a raw SQL query that returned
// the stored row count without parsing frontmatter, so `mpm info`
// reported `registered skills: 0` while `mpm skill list` showed 1.
// The two surfaces must agree; this is the canonical resolution.
func listSkillsForInfo(dm *mpminternal.DatabaseManager) []string {
	if dm == nil {
		return nil
	}
	skills, err := dm.ListSkills("all")
	if err != nil {
		usererror.Warn("listSkillsForInfo: %v", err)
		return nil
	}
	out := make([]string, 0, len(skills))
	for _, s := range skills {
		if s.Name != "" {
			out = append(out, s.Name)
		} else {
			out = append(out, s.ID)
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
