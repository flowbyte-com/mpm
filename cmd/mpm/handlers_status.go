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
	uptime        string
	modeState     string   // none | one | many | auto
	modeValues    []string // empty when state="none", one entry when "one"/"auto", many when "many"
	personaState  string   // none | one | auto
	personaValues []string // empty when "none", one entry when "one"/"auto"
	memTotal      int
	memLTM        int
	theoryTotal   int
	theoryPend    int
	theoryResolv  int
	decisions     int
	synthMerged   int
	synthLast     string
	recentEvents  []watchdogEvent
}

// stateForMode classifies active.json's modes slice into a {state, values}
// pair. state ∈ {none, one, many, auto} — the auto state means the user
// has handed off mode selection to the substrate (current code keeps "auto"
// as the literal first-slot value).
func stateForMode(activeModes []string) (state string, values []string) {
	switch len(activeModes) {
	case 0:
		return "none", []string{}
	case 1:
		if activeModes[0] == "auto" {
			return "auto", []string{"auto"}
		}
		return "one", activeModes
	default:
		return "many", activeModes
	}
}

// stateForPersona classifies active.json's persona string into a {state, values}
// pair. state ∈ {none, one, auto}. "ephemeral" is surfaced as state="auto"
// with values=["ephemeral"] — it's a kind of auto-loaded persona, not a
// separate cardinality.
func stateForPersona(activePersona string) (state string, values []string) {
	switch activePersona {
	case "":
		return "none", []string{}
	case "auto", "ephemeral":
		return "auto", []string{activePersona}
	default:
		return "one", []string{activePersona}
	}
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
	d.synthMerged, d.synthLast = getSynthesisStats(dm)
	d.recentEvents = getRecentWatchdogEvents(dm, 3)

	// Default to "none" — the absence of an active selection is always
	// visible (never silent). Overridden below if a real value is found
	// or if LoadActiveJSON fails (in which case the defaults persist).
	d.modeState, d.modeValues = "none", []string{}
	d.personaState, d.personaValues = "none", []string{}

	active, err := mpminternal.LoadActiveJSON()
	if err == nil {
		d.modeState, d.modeValues = stateForMode(active.Modes)
		d.personaState, d.personaValues = stateForPersona(active.Persona)
	}
	return d
}

// formatModeLine renders the dashboard's "Mode: ..." line from a
// {state, values} pair. Cardinality is explicit in the text so a
// reader doesn't have to count entries.
func formatModeLine(state string, values []string) string {
	switch state {
	case "none":
		return "Mode: none"
	case "one":
		return fmt.Sprintf("Mode: one (%s)", values[0])
	case "many":
		return fmt.Sprintf("Mode: many (%s)", strings.Join(values, ", "))
	case "auto":
		return "Mode: auto (loaded: auto)"
	}
	return fmt.Sprintf("Mode: %s", state)
}

// formatPersonaLine renders the dashboard's "Persona: ..." line.
// For auto-state with ephemeral values, looks up the ephemeral persona's
// display name to give the operator a more useful "loaded:" hint.
func formatPersonaLine(state string, values []string, dm *mpminternal.DatabaseManager) string {
	switch state {
	case "none":
		return "Persona: none"
	case "one":
		return fmt.Sprintf("Persona: one (%s)", values[0])
	case "auto":
		if len(values) > 0 && values[0] == "ephemeral" {
			if ep, epErr := mpminternal.GetEphemeralPersona(dm); epErr == nil {
				displayName := ep.Name
				if ep.Title != "" {
					displayName = ep.Title
				}
				return fmt.Sprintf("Persona: auto (loaded: ephemeral - %q)", displayName)
			}
			return "Persona: auto (loaded: ephemeral)"
		}
		return "Persona: auto (loaded: auto)"
	}
	return fmt.Sprintf("Persona: %s", state)
}

func printStatusDashboard(dm *mpminternal.DatabaseManager, startTime time.Time) {
	d := buildStatusData(dm, startTime)

	fmt.Println("⚡ MPM · System Status")
	fmt.Println("────────────────────────────────────")
	fmt.Printf("Uptime:    %s\n", d.uptime)
	fmt.Printf("  %s\n", formatModeLine(d.modeState, d.modeValues))
	fmt.Printf("  %s\n", formatPersonaLine(d.personaState, d.personaValues, dm))
	fmt.Printf("Memories:  %d total | %d LTM\n", d.memTotal, d.memLTM)
	fmt.Printf("Theories:  %d total | %d pending | %d resolved\n", d.theoryTotal, d.theoryPend, d.theoryResolv)
	fmt.Printf("Decisions: %d total\n", d.decisions)
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

// modeStateJSON / personaStateJSON are the wire shape for the
// status JSON output. Both use the {state, values} form so consumers
// can branch on cardinality without parsing the string.
type modeStateJSON struct {
	State  string   `json:"state"`
	Values []string `json:"values"`
}

type personaStateJSON struct {
	State  string   `json:"state"`
	Values []string `json:"values"`
}

// printStatusJSON emits the status dashboard as a JSON object.
// Mode and persona are first-class {state, values} objects —
// cardinality (none|one|many|auto for mode, none|one|auto for persona)
// is the discriminator; values carry the actual selections.
func printStatusJSON(dm *mpminternal.DatabaseManager, startTime time.Time) int {
	d := buildStatusData(dm, startTime)

	type jsonEvent struct {
		Op     string `json:"op"`
		Detail string `json:"detail"`
	}
	out := struct {
		Uptime       string           `json:"uptime"`
		Mode         modeStateJSON    `json:"mode"`
		Persona      personaStateJSON `json:"persona"`
		Memories     memCounts        `json:"memories"`
		Theories     thCounts         `json:"theories"`
		Decisions    int              `json:"decisions"`
		Synthesis    synthCounts      `json:"synthesis"`
		RecentEvents []jsonEvent      `json:"recent_events,omitempty"`
	}{
		Uptime:    d.uptime,
		Mode:      modeStateJSON{State: d.modeState, Values: d.modeValues},
		Persona:   personaStateJSON{State: d.personaState, Values: d.personaValues},
		Memories:  memCounts{Total: d.memTotal, LTM: d.memLTM},
		Theories:  thCounts{Total: d.theoryTotal, Pending: d.theoryPend, Resolved: d.theoryResolv},
		Decisions: d.decisions,
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
	if err := dm.SQLDB().QueryRow(query, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("countMemories: %w", err)
	}
	return count, nil
}

func countTheoriesByStatus(dm *mpminternal.DatabaseManager, status string) (int, error) {
	var count int
	query := `SELECT COUNT(*) FROM memories
		WHERE collection = 'theories' AND deleted_at IS NULL
		AND json_extract(metadata, '$.status') = ?`
	if err := dm.SQLDB().QueryRow(query, status).Scan(&count); err != nil {
		return 0, fmt.Errorf("countTheoriesByStatus: %w", err)
	}
	return count, nil
}

func getSynthesisStats(dm *mpminternal.DatabaseManager) (int, string) {
	var count int
	var lastTime string
	if err := dm.SQLDB().QueryRow(`
		SELECT COUNT(*), MAX(json_extract(metadata, '$.synthesized_at'))
		FROM memories
		WHERE deleted_at IS NULL
		AND json_extract(metadata, '$.synthesized') = 'true'
	`).Scan(&count, &lastTime); err != nil {
		usererror.Warn("getSynthesisStats: failed to query synthesis stats, defaulting to (0, never): %v", err)
	}
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
