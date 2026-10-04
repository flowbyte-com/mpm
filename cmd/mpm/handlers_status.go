package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/config"

	"github.com/flowbyte-com/mpm-core/usererror"

	"github.com/flowbyte-com/mpm/cmd/mpm/render"
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
	modeSource    string   // 2026-09-14: resolution source tag ("[fallback]" / "[explicit]" / "")
	personaState  string   // none | one | auto
	personaValues []string // empty when "none", one entry when "one"/"auto"
	personaSource string   // 2026-09-14: resolution source tag
	memTotal      int
	memLTM        int
	directives    int
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
	// Defect N (2026-09-13 acceptance): pre-fix this used
	// `formatUptime(time.Since(startTime))` which is the CLI
	// process start time — every short-lived invocation saw
	// "Uptime: 0s" regardless of how long the substrate had been
	// alive. The fix reads scheduler.state's process_started_unix
	// (the same source `mpm doctor` uses) so the dashboard shows
	// the canonical scheduler uptime. If the state file is
	// missing or unreadable, fall back to CLI process uptime
	// with a clearly-labelled "process uptime" so the operator
	// isn't misled.
	d.uptime = schedulerUptimeOrFallback(dm, startTime)
	d.memTotal, _ = countMemories(dm, "")
	d.memLTM, _ = countMemories(dm, "weight >= 10")
	// Directives live in the memories table but are surfaced as
	// their own row on the dashboard. countMemories already
	// excludes them from the memory totals above, so this is
	// the authoritative count for the Directives row.
	d.directives, _ = countDirectives(dm)
	d.theoryTotal, _ = countMemories(dm, "collection = 'theories'")
	d.decisions, _ = countMemories(dm, "collection = 'decisions'")
	d.theoryPend, _ = countTheoriesByStatus(dm, "pending")
	// D-5.1: theories have terminal statuses 'proven' / 'disproven'
	// (with 'challenged' as a non-terminal variant on the way to terminal).
	// The literal status='resolved' is never written. Count proven +
	// disproven so the "resolved" line on the dashboard reflects the actual
	// terminal population.
	proven, _ := countTheoriesByStatus(dm, "proven")
	disproven, _ := countTheoriesByStatus(dm, "disproven")
	d.theoryResolv = proven + disproven
	d.synthMerged, d.synthLast = getSynthesisStats(dm)
	d.recentEvents = getRecentWatchdogEvents(dm, 3)

	// Defect M (2026-09-13 acceptance): pre-fix this used a parallel
	// fallback ("none" when active.json was empty) that disagreed with
	// the canonical resolver used by `mpm info` and the wake context.
	// Operators saw `mpm status` show "Mode: none" while wake payload
	// showed `active_mode: default`. The fix uses the same canonical
	// resolvers so all three surfaces agree on what the active value
	// is — including the "default" fallback when active.json is
	// uninitialised.
	d.modeState, d.modeValues, d.modeSource = resolveStatusMode(dm)
	d.personaState, d.personaValues, d.personaSource = resolveStatusPersona(dm)
	return d
}

// schedulerUptimeOrFallback returns the scheduler uptime from the
// canonical scheduler.state.json file (the same source
// `mpm doctor`'s Scheduler row reads via service_doctor.go's
// schedulerStatePath). Falls back to the CLI process uptime
// (clearly labelled) if the state file is missing or unreadable.
//
// 2026-09-14 release-pass: the pre-fix implementation read
// `dm.WatchdogPath()` (which is the per-DB watchdog.jsonl — a
// query-observability log, NOT the scheduler heartbeat). The
// watchdog.jsonl format has no `process_started_unix` field, so
// the unmarshal always failed and the function always fell back
// to "(process)" uptime — producing the long-standing
// `Uptime : 0s (process)` regression. The fix reads the
// canonical scheduler.state.json instead.
func schedulerUptimeOrFallback(dm *mpminternal.DatabaseManager, startTime time.Time) string {
	statePath := schedulerStatePath()
	if statePath != "" {
		if data, err := os.ReadFile(statePath); err == nil {
			var snap struct {
				ProcessStartedUnix int64 `json:"process_started_unix"`
			}
			if err := json.Unmarshal(data, &snap); err == nil && snap.ProcessStartedUnix > 0 {
				return formatUptime(time.Since(time.Unix(snap.ProcessStartedUnix, 0)))
			}
		}
	}
	return formatUptime(time.Since(startTime)) + " (process)"
}

// resolveStatusMode uses the canonical mode resolver and returns
// the {state, values} pair the dashboard expects. Empty active.json
// falls back to "default" via ResolveActiveMode, matching wake and
// info surfaces.
//
// 2026-09-14 release-pass: returns the ModeResolution so the
// caller can label the value with the resolution source
// ([fallback] for default-bootstrap, [explicit] for a real user
// selection). This matches the `mpm info` presentation
// (`active modes : default [fallback]`) so the two surfaces do
// not disagree about which mode is in effect.
func resolveStatusMode(dm *mpminternal.DatabaseManager) (state string, values []string, sourceLabel string) {
	resolution := mpminternal.ResolveActiveModes(dm, nil)
	names := resolution.Names()
	switch resolution.Source {
	case mpminternal.SourceEmpty:
		return "none", []string{}, ""
	case mpminternal.SourceFallback:
		if len(names) > 0 {
			return "one", names, "[fallback]"
		}
		return "none", []string{}, ""
	case mpminternal.SourceExplicit:
		if len(names) == 0 {
			return "none", []string{}, ""
		}
		if len(names) == 1 {
			return "one", names, "[explicit]"
		}
		return "many", names, "[explicit]"
	default:
		if len(names) == 0 {
			return "none", []string{}, ""
		}
		return "one", names, ""
	}
}

// resolveStatusPersona uses the canonical persona resolver and
// returns the {state, values, sourceLabel} triple the dashboard
// expects. 2026-09-14 release-pass: the source label
// (`[fallback]` / `[explicit]`) is appended to the rendered line
// so status and info agree on which persona is in effect.
func resolveStatusPersona(dm *mpminternal.DatabaseManager) (state string, values []string, sourceLabel string) {
	// Mirror the mode resolver's canonical-source approach by
	// reading the canonical resolver's ModeResolution-style output.
	// ResolveActivePersona's contract is a single-string return, so
	// we classify the result based on active.json presence.
	if active, err := mpminternal.LoadActiveJSON(); err == nil {
		var explicit string
		if active.Persona != nil {
			explicit = *active.Persona
		}
		resolved := mpminternal.ResolveActivePersona(dm, "")
		if explicit == "" && resolved != "" {
			// No explicit persona in active.json — the value is
			// the resolver's default fallback.
			if resolved == "auto" || resolved == "ephemeral" {
				return "auto", []string{resolved}, ""
			}
			return "one", []string{resolved}, "[fallback]"
		}
		if resolved == "auto" || resolved == "ephemeral" {
			return "auto", []string{resolved}, ""
		}
		return "one", []string{resolved}, "[explicit]"
	}
	resolved := mpminternal.ResolveActivePersona(dm, "")
	if resolved == "" {
		return "none", []string{}, ""
	}
	if resolved == "auto" || resolved == "ephemeral" {
		return "auto", []string{resolved}, ""
	}
	return "one", []string{resolved}, ""
}

// trimBrackets strips the surrounding `[...]` decoration from a
// source label so the JSON wire shape emits a clean enum
// (`fallback` / `explicit` / `auto` / `ephemeral`).
func trimBrackets(s string) string {
	if len(s) >= 2 && s[0] == '[' && s[len(s)-1] == ']' {
		return s[1 : len(s)-1]
	}
	return s
}

// formatModeLine renders the dashboard's "Mode: ..." line from a
// {state, values} pair. Cardinality is explicit in the text so a
// reader doesn't have to count entries.
//
// 2026-09-14 release-pass: includes the resolution source label
// (e.g. "[fallback]" or "[explicit]") so the line matches `mpm
// info`'s `active modes : default [fallback]` wording. Without
// the label, status showed `Mode: one (default)` while info
// showed `active modes : default [fallback]` — same value, two
// different presentations.
func formatModeLine(state string, values []string, sourceLabel string) string {
	switch state {
	case "none":
		return "Mode: none"
	case "one":
		name := values[0]
		if sourceLabel != "" {
			return fmt.Sprintf("Mode: %s %s", name, sourceLabel)
		}
		return fmt.Sprintf("Mode: %s", name)
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
//
// 2026-09-14 release-pass: includes the resolution source label so
// the line matches `mpm info`'s `active persona : default
// [fallback]` wording.
func formatPersonaLine(state string, values []string, sourceLabel string, dm *mpminternal.DatabaseManager) string {
	switch state {
	case "none":
		return "Persona: none"
	case "one":
		name := values[0]
		if sourceLabel != "" {
			return fmt.Sprintf("Persona: %s %s", name, sourceLabel)
		}
		return fmt.Sprintf("Persona: %s", name)
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

	// Rendered through the shared render package — no inline lipgloss,
	// no ad-hoc styling. Same heading token as doctor/why.
	render.Heading(os.Stdout, "System Status")
	render.Divider(os.Stdout)
	render.KeyValue(os.Stdout, "Uptime", d.uptime)
	render.Plain(os.Stdout, "  "+formatModeLine(d.modeState, d.modeValues, d.modeSource))
	render.Plain(os.Stdout, "  "+formatPersonaLine(d.personaState, d.personaValues, d.personaSource, dm))
	// Models health row — read-only on the probe cache; never network IO.
	if cfg, err := config.LoadConfig(); err == nil && cfg != nil {
		renderModelsDashboard(os.Stdout, dm, cfg)
	} else {
		renderModelsDashboard(os.Stdout, nil, nil)
	}
	render.KeyValue(os.Stdout, "Memories", fmt.Sprintf("%d total | %d LTM", d.memTotal, d.memLTM))
	render.KeyValue(os.Stdout, "Directives", fmt.Sprintf("%d active", d.directives))
	render.KeyValue(os.Stdout, "Theories", fmt.Sprintf("%d total | %d pending | %d resolved", d.theoryTotal, d.theoryPend, d.theoryResolv))
	render.KeyValue(os.Stdout, "Decisions", fmt.Sprintf("%d total", d.decisions))
	render.KeyValue(os.Stdout, "Synthesis", fmt.Sprintf("%d merged | last: %s", d.synthMerged, d.synthLast))
	if len(d.recentEvents) > 0 {
		render.Divider(os.Stdout)
		render.Section(os.Stdout, "Recent events")
		for _, e := range d.recentEvents {
			render.Plainf(os.Stdout, "  %s %s\n", e.op, e.detail)
		}
	}
	render.Divider(os.Stdout)
	render.Hint(os.Stdout, "Run `mpm help` for daily commands.")
	render.Hint(os.Stdout, "Run `mpm ops help` for engine room.")
}

// modeStateJSON / personaStateJSON are the wire shape for the
// status JSON output. Both use the {state, values} form so consumers
// can branch on cardinality without parsing the string.
//
// 2026-09-14 release-pass: `source` carries the resolution source
// (`fallback` / `explicit` / `auto` / `ephemeral`) so consumers
// can distinguish a real user selection from the resolver's
// default fallback. The legacy JSON shape was additive —
// consumers reading only `state` / `values` continue to work.
type modeStateJSON struct {
	State  string   `json:"state"`
	Values []string `json:"values"`
	Source string   `json:"source,omitempty"`
}

type personaStateJSON struct {
	State  string   `json:"state"`
	Values []string `json:"values"`
	Source string   `json:"source,omitempty"`
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
		Directives   int              `json:"directives"`
		Theories     thCounts         `json:"theories"`
		Decisions    int              `json:"decisions"`
		Synthesis    synthCounts      `json:"synthesis"`
		RecentEvents []jsonEvent      `json:"recent_events,omitempty"`
	}{
		Uptime:     d.uptime,
		Mode:       modeStateJSON{State: d.modeState, Values: d.modeValues, Source: trimBrackets(d.modeSource)},
		Persona:    personaStateJSON{State: d.personaState, Values: d.personaValues, Source: trimBrackets(d.personaSource)},
		Memories:   memCounts{Total: d.memTotal, LTM: d.memLTM},
		Directives: d.directives,
		Theories:   thCounts{Total: d.theoryTotal, Pending: d.theoryPend, Resolved: d.theoryResolv},
		Decisions:  d.decisions,
		Synthesis:  synthCounts{Merged: d.synthMerged, Last: d.synthLast},
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
	// Directives live in the `memories` table alongside ordinary
	// memories (collection='directives' for the canonical path,
	// is_prime_directive=1 for legacy rows). The memory counts on
	// the status dashboard must exclude them — they are reported
	// separately on the dashboard's `Directives : N active` row.
	//
	// Exclusion is NULL-safe: COALESCE(is_prime_directive, 0) = 1
	// treats a NULL flag as "not a directive" so imported,
	// legacy, or manually-created rows with NULL in the flag
	// column still count as ordinary memories. The previous
	// `is_prime_directive != 1` form was NOT NULL-safe: a row
	// with `is_prime_directive = NULL` evaluated the comparison
	// to UNKNOWN, the whole AND predicate to UNKNOWN, and the
	// row was silently hidden from memory counts.
	//
	// Excluding them at the countMemories boundary keeps every
	// existing call-site (memTotal, memLTM, theoryTotal,
	// decisions) correct without each having to repeat the
	// filter — the only call that explicitly includes directives
	// is the dedicated countDirectives function below.
	var query string
	var args []interface{}
	directiveExclusion := `NOT (collection = 'directives' OR COALESCE(is_prime_directive, 0) = 1)`
	if where == "" {
		// D-5.2: apply the canonical EXPIRE filter (expires_at IS NULL
		// OR expires_at > now). The prior code only filtered deleted_at,
		// so an expired memory continued to count toward memTotal.
		query = `SELECT COUNT(*) FROM memories
			WHERE deleted_at IS NULL
			AND (expires_at IS NULL OR expires_at > strftime('%s','now'))
			AND ` + directiveExclusion
	} else {
		query = `SELECT COUNT(*) FROM memories
			WHERE deleted_at IS NULL
			AND (expires_at IS NULL OR expires_at > strftime('%s','now'))
			AND ` + directiveExclusion + ` AND ` + where
	}
	var count int
	if err := dm.SQLDB().QueryRow(query, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("countMemories: %w", err)
	}
	return count, nil
}

// countDirectives returns the number of active (non-deleted,
// non-expired) directives in the memories table. The canonical
// identifier is `collection = 'directives'` (the MCP path);
// the legacy `is_prime_directive` column is also included via
// COALESCE so pre-F19 rows with the flag set AND imported
// rows with NULL flags are both visible. Mirrors the query
// used by `mpm call mpm_context --payload
// '{"action":"read_directives"}'` so the dashboard's
// `Directives : N active` count matches what the agent sees
// via the wake-context read path.
func countDirectives(dm *mpminternal.DatabaseManager) (int, error) {
	var count int
	const query = `SELECT COUNT(*) FROM memories
		WHERE (collection = 'directives' OR COALESCE(is_prime_directive, 0) = 1)
		  AND deleted_at IS NULL
		  AND (expires_at IS NULL OR expires_at > strftime('%s','now'))`
	if err := dm.SQLDB().QueryRow(query).Scan(&count); err != nil {
		return 0, fmt.Errorf("countDirectives: %w", err)
	}
	return count, nil
}

func countTheoriesByStatus(dm *mpminternal.DatabaseManager, status string) (int, error) {
	var count int
	query := `SELECT COUNT(*) FROM memories
		WHERE collection = 'theories' AND deleted_at IS NULL
		AND (expires_at IS NULL OR expires_at > strftime('%s','now'))
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
		SELECT COUNT(*), COALESCE(MAX(json_extract(metadata, '$.synthesized_at')), '')
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
		// `op` and `detail` are the v2 keys; `operation`/`query` and
		// `reason` are the v1 ones. This tail reads the live file rather
		// than going through RecentWatchdogOps, so it applies the same
		// tolerance locally. Old lines keep rendering after the cutover.
		op, _ := m["op"].(string)
		if op == "" {
			op, _ = m["operation"].(string)
		}
		detail, _ := m["detail"].(string)
		if detail == "" {
			detail, _ = m["reason"].(string)
		}
		if detail == "" {
			detail, _ = m["error"].(string)
		}
		if op != "" {
			events = append(events, watchdogEvent{op: op, detail: detail})
		}
	}
	return events
}
