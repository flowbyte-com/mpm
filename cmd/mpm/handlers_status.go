package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

func handleStatus() int {
	dm := getDBConcrete()
	if dm == nil {
		return 1
	}
	printStatusDashboard(dm, startTime)
	return 0
}

func printStatusDashboard(dm *mpminternal.DatabaseManager, startTime time.Time) {
	uptime := formatUptime(time.Since(startTime))
	totalMemories, _ := countMemories(dm, "")
	ltmCount, _ := countMemories(dm, "weight >= 10")
	theoriesCount, _ := countMemories(dm, "collection = 'theories'")
	decisionsCount, _ := countMemories(dm, "collection = 'decisions'")
	activeTheories, _ := countTheoriesByStatus(dm, "pending")
	resolvedTheories, _ := countTheoriesByStatus(dm, "resolved")

	daemonStatus := getDaemonStatus()

	synthCount, lastSynth := getSynthesisStats(dm)

	recentEvents := getRecentWatchdogEvents(dm, 3)

	// Auto-mode transparency
	modeLine := ""
	personaLine := ""
	active, err := mpminternal.LoadActiveJSON()
	if err == nil {
		if len(active.Modes) > 0 && active.Modes[0] == "auto" {
			modeLine = "Mode: auto (loaded: auto)"
		} else if len(active.Modes) > 0 {
			modeLine = fmt.Sprintf("Mode: %s", strings.Join(active.Modes, ", "))
		}
		if active.Persona == "auto" {
			personaLine = "Persona: auto (loaded: auto)"
		} else if active.Persona == "ephemeral" {
			if ep, epErr := mpminternal.GetEphemeralPersona(dm); epErr == nil {
				displayName := ep.Name
				if ep.Title != "" {
					displayName = ep.Title
				}
				personaLine = fmt.Sprintf("Persona: auto (loaded: ephemeral - %q)", displayName)
			} else {
				personaLine = "Persona: auto (loaded: ephemeral)"
			}
		} else if active.Persona != "" {
			personaLine = fmt.Sprintf("Persona: %s", active.Persona)
		}
	}

	fmt.Println("⚡ MPM · System Status")
	fmt.Println("────────────────────────────────────")
	fmt.Printf("Uptime:    %s\n", uptime)
	if modeLine != "" {
		fmt.Printf("  %s\n", modeLine)
	}
	if personaLine != "" {
		fmt.Printf("  %s\n", personaLine)
	}
	fmt.Printf("Memories:  %d total | %d LTM\n", totalMemories, ltmCount)
	fmt.Printf("Theories:  %d total | %d pending | %d resolved\n", theoriesCount, activeTheories, resolvedTheories)
	fmt.Printf("Decisions: %d total\n", decisionsCount)
	fmt.Printf("Watcher:   %s\n", daemonStatus)
	fmt.Printf("Synthesis: %d merged | last: %s\n", synthCount, lastSynth)
	if len(recentEvents) > 0 {
		fmt.Println("────────────────────────────────────")
		fmt.Println("Recent events:")
		for _, e := range recentEvents {
			fmt.Printf("  %s %s\n", e.op, e.detail)
		}
	}
	fmt.Println("────────────────────────────────────")
	fmt.Println("Run `mpm help` for daily commands.")
	fmt.Println("Run `mpm ops help` for engine room.")
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
