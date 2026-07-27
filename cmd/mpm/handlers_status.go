package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"

	"github.com/flowbyte-com/mpm-core/usererror"
)

// handleStatus shows the system status dashboard. Supports --json / -j
// for machine-readable output; the JSON shape mirrors the text dashboard
// field-for-field.
func handleStatus(args []string) int {
	dm := getDBConcrete()
	if dm == nil {
		return 1
	}
	jsonOutput, _ := ExtractJSONFlag(args)
	if jsonOutput {
		return printStatusJSON(dm, startTime)
	}
	printStatusDashboard(dm, startTime)
	return 0
}

// statusData is the canonical view of the daemon's state used by both
// the text dashboard (printStatusDashboard) and the JSON output
// (printStatusJSON). Single source of truth so the two views cannot drift.
type statusData struct {
	uptime       string
	modeLine     string
	personaLine  string
	memTotal     int
	memLTM       int
	theoryTotal  int
	theoryPend   int
	theoryResolv int
	decisions    int
	watcher      string
	synthMerged  int
	synthLast    string
	recentEvents []watchdogEvent
}

func buildStatusData(dm *mpminternal.DatabaseManager, startTime time.Time) statusData {
	var d statusData
	d.uptime = formatUptime(time.Since(startTime))
	d.memTotal, _ = countMemories(dm, "")
	d.memLTM, _ = countMemories(dm, "weight >= 10")
	d.theoryTotal, _ = countMemories(dm, "collection = 'theories'")
	d.decisions, _ = countMemories(dm, "collection = 'decisions'")
	d.theoryPend, _ = countTheoriesByStatus(dm, "pending")
	d.theoryResolv, _ = countTheoriesByStatus(dm, "resolved")
	d.watcher = getDaemonStatus()
	d.synthMerged, d.synthLast = getSynthesisStats(dm)
	d.recentEvents = getRecentWatchdogEvents(dm, 3)

	// Mode/persona — same parsing as the dashboard's pre-refactor inline logic.
	active, err := mpminternal.LoadActiveJSON()
	if err == nil {
		if len(active.Modes) > 0 && active.Modes[0] == "auto" {
			d.modeLine = "Mode: auto (loaded: auto)"
		} else if len(active.Modes) > 0 {
			d.modeLine = fmt.Sprintf("Mode: %s", strings.Join(active.Modes, ", "))
		}
		if active.Persona == "auto" {
			d.personaLine = "Persona: auto (loaded: auto)"
		} else if active.Persona == "ephemeral" {
			if ep, epErr := mpminternal.GetEphemeralPersona(dm); epErr == nil {
				displayName := ep.Name
				if ep.Title != "" {
					displayName = ep.Title
				}
				d.personaLine = fmt.Sprintf("Persona: auto (loaded: ephemeral - %q)", displayName)
			} else {
				d.personaLine = "Persona: auto (loaded: ephemeral)"
			}
		} else if active.Persona != "" {
			d.personaLine = fmt.Sprintf("Persona: %s", active.Persona)
		}
	}
	return d
}

func printStatusDashboard(dm *mpminternal.DatabaseManager, startTime time.Time) {
	d := buildStatusData(dm, startTime)

	fmt.Println("⚡ MPM · System Status")
	fmt.Println("────────────────────────────────────")
	fmt.Printf("Uptime:    %s\n", d.uptime)
	if d.modeLine != "" {
		fmt.Printf("  %s\n", d.modeLine)
	}
	if d.personaLine != "" {
		fmt.Printf("  %s\n", d.personaLine)
	}
	fmt.Printf("Memories:  %d total | %d LTM\n", d.memTotal, d.memLTM)
	fmt.Printf("Theories:  %d total | %d pending | %d resolved\n", d.theoryTotal, d.theoryPend, d.theoryResolv)
	fmt.Printf("Decisions: %d total\n", d.decisions)
	fmt.Printf("Watcher:   %s\n", d.watcher)
	fmt.Printf("Synthesis: %d merged | last: %s\n", d.synthMerged, d.synthLast)
	if len(d.recentEvents) > 0 {
		fmt.Println("────────────────────────────────────")
		fmt.Println("Recent events:")
		for _, e := range d.recentEvents {
			fmt.Printf("  %s %s\n", e.op, e.detail)
		}
	}
	fmt.Println("────────────────────────────────────")
	fmt.Println("Run `mpm help` for daily commands.")
	fmt.Println("Run `mpm ops help` for engine room.")
}

// printStatusJSON emits the status dashboard as a JSON object.
// Field names mirror the dashboard labels where reasonable; values
// are emitted with `omitempty` so empty optional fields (mode,
// persona, recent_events) are skipped rather than emitted as "".
func printStatusJSON(dm *mpminternal.DatabaseManager, startTime time.Time) int {
	d := buildStatusData(dm, startTime)

	type jsonEvent struct {
		Op     string `json:"op"`
		Detail string `json:"detail"`
	}
	out := struct {
		Uptime       string      `json:"uptime"`
		Mode         string      `json:"mode,omitempty"`
		Persona      string      `json:"persona,omitempty"`
		Memories     memCounts   `json:"memories"`
		Theories     thCounts    `json:"theories"`
		Decisions    int         `json:"decisions"`
		Watcher      string      `json:"watcher"`
		Synthesis    synthCounts `json:"synthesis"`
		RecentEvents []jsonEvent `json:"recent_events,omitempty"`
	}{
		Uptime:    d.uptime,
		Mode:      stripPrefix(d.modeLine, "Mode: "),
		Persona:   stripPrefix(d.personaLine, "Persona: "),
		Memories:  memCounts{Total: d.memTotal, LTM: d.memLTM},
		Theories:  thCounts{Total: d.theoryTotal, Pending: d.theoryPend, Resolved: d.theoryResolv},
		Decisions: d.decisions,
		Watcher:   d.watcher,
		Synthesis: synthCounts{Merged: d.synthMerged, Last: d.synthLast},
	}
	for _, e := range d.recentEvents {
		out.RecentEvents = append(out.RecentEvents, jsonEvent{Op: e.op, Detail: e.detail})
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return usererror.Error("json encode failed: %v", err)
	}
	return 0
}

// stripPrefix removes `prefix` from `s` if present. Used to peel the
// "Mode: " / "Persona: " label off the dashboard's pre-formatted lines
// so the JSON view exposes the bare value.
func stripPrefix(s, prefix string) string {
	if strings.HasPrefix(s, prefix) {
		return strings.TrimPrefix(s, prefix)
	}
	return s
}

type memCounts struct {
	Total int `json:"total"`
	LTM   int `json:"ltm"`
}

type thCounts struct {
	Total    int `json:"total"`
	Pending  int `json:"pending"`
	Resolved int `json:"resolved"`
}

type synthCounts struct {
	Merged int    `json:"merged"`
	Last   string `json:"last"`
}

func countMemories(dm *mpminternal.DatabaseManager, where string) (int, error) {
	var query string
	var args []interface{}
	if where == "" {
		query = "SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL"
	} else {
		query = "SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL AND " + where
	}
	var count int
	err := dm.SQLDB().QueryRow(query, args...).Scan(&count)
	return count, err
}

func countTheoriesByStatus(dm *mpminternal.DatabaseManager, status string) (int, error) {
	var count int
	query := `SELECT COUNT(*) FROM memories
		WHERE collection = 'theories' AND deleted_at IS NULL
		AND json_extract(metadata, '$.status') = ?`
	err := dm.SQLDB().QueryRow(query, status).Scan(&count)
	return count, err
}

// getDaemonStatus returns the watcher daemon status. Deprecated 2026-06-26 —
// the watcher is gone, so this always reports "not running". Kept as a stable
// string contract for the status dashboard so callers don't have to special-case
// a missing function.
func getDaemonStatus() string {
	return "not running (watcher deprecated 2026-06-26)"
}

func getSynthesisStats(dm *mpminternal.DatabaseManager) (int, string) {
	var count int
	var lastTime string
	dm.SQLDB().QueryRow(`
		SELECT COUNT(*), MAX(json_extract(metadata, '$.synthesized_at'))
		FROM memories
		WHERE deleted_at IS NULL
		AND json_extract(metadata, '$.synthesized') = 'true'
	`).Scan(&count, &lastTime)
	if lastTime == "" {
		lastTime = "never"
	}
	return count, lastTime
}

type watchdogEvent struct {
	op     string
	detail string
}

func getRecentWatchdogEvents(dm *mpminternal.DatabaseManager, limit int) []watchdogEvent {
	path := dm.WatchdogPath()
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	var events []watchdogEvent
	for _, line := range lines {
		if line == "" {
			continue
		}
		var m map[string]interface{}
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		op := ""
		if v, ok := m["op"].(string); ok {
			op = v
		}
		detail := ""
		if v, ok := m["reason"].(string); ok {
			detail = v
		} else if v, ok := m["error"].(string); ok {
			detail = v
		}
		if op != "" {
			events = append(events, watchdogEvent{op: op, detail: detail})
		}
	}
	return events
}
