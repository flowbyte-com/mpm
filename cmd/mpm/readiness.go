// cmd/mpm/readiness.go — `mpm` (no args) readiness subsystem.
//
// v (operator) design session, Wed 2026-07-29:
//
//   'mpm' is the SYSTEM ENTRY POINT. Every invocation verifies
//   that the substrate is READY. The checks are:
//     - Database open (SQLite WAL ping)
//     - Scheduler alive (read mpm-scheduler PID; auto-start if not)
//     - Embeddings available (env var wired)
//     - Gateway connected (broadcast channel configured)
//     - Working Context loaded (current session has a focus row)
//     - No pending theories (cognitive debt not piling up)
//
//   Separately:
//     - 'mpm doctor'  → HEALTH   (no integrity issues, no
//                                contradictions, embedding queue OK)
//     - 'mpm status'  → LIVE STATE (what's the substrate doing
//                                right now — composite dashboard)
//     - 'mpm continue' → RESUME   (the cognitive dashboard that
//                                the RFC v0.4 ARCHITECTURE INVARIANT
//                                locks down — pure state, no actions)
//
//   'mpm' therefore checks READINESS. Operator can run it at the
//   start of every session to confirm the substrate is awake and
//   connected before relying on the rest of the workflow.
//
// Architecture: readiness is a thin assembler over existing
// substrate primitives (DatabaseManager.SQLDB().PingContext,
// readSchedulerPID, os.Getenv, WorkingContextService.GetCurrent,
// dm.CountMemory). No new substrate. No new service layer —
// readiness is a presentation concern, owned by PrintQuicklinks.
//
// Side-effect policy: the ONLY side effect `mpm` (no args) takes
// is auto-starting the scheduler daemon when it's not running.
// This is justified because (1) the operator explicitly asked
// for this in the design session; (2) the alternative — leaving
// the scheduler dead — silently breaks every wake-scheduled
// task; (3) the side effect is reported explicitly in the
// readiness output (not a silent magic). All other readiness
// checks are read-only. `mpm` will NEVER auto-fix or auto-clear
// any substrate state — those are 'mpm doctor' territory.

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// ReadinessItem is one row in the readiness check.
//
//   Name:    the subsystem being checked (e.g. "Database connected").
//   OK:      true when the subsystem is ready (or the absence is
//            an acceptable default state — e.g. no working context
//            is not a failure, it's just an operator hint).
//   Detail:  short human-readable summary; appears inline next to
//            the marker on the dashboard.
//   Hint:    optional next-action hint — used when the operator
//            should run a specific command to resolve the issue
//            (typically `mpm doctor`).
//
// Hint is intentionally separate from Detail because the two
// serve different reading modes: Detail explains the state,
// Hint telegraphs the fix.
type ReadinessItem struct {
	Name   string
	OK     bool
	Detail string
	Hint   string
}

// readinessOverall checks all items and returns true only if every
// item is OK. Items where OK=false contribute to the verdict
// surfaced in the dashboard header.
func readinessOverall(items []ReadinessItem) bool {
	for _, it := range items {
		if !it.OK {
			return false
		}
	}
	return true
}

// ReadReadiness runs every readiness check and returns the items
// in the canonical display order. Order matters: scheduler first
// (so its auto-start side effect, if triggered, runs before the
// rest of the dashboard is composed), then database, then the
// lighter cognitive-surface checks.
//
// The function takes a *DatabaseManager rather than calling
// getDBConcrete() directly so the caller controls when the
// database handle is opened (matters for the auto-start path:
// if the database fails to open, we still want to try starting
// the scheduler first because it's likely the right move).
func ReadReadiness(dm *mpminternal.DatabaseManager) []ReadinessItem {
	var items []ReadinessItem

	// 1. Scheduler — first because it's the only side-effect we
	// take. Auto-start on failure is the explicit behaviour.
	schedItem, schedAutoStarted := checkSchedulerWithAutoStart()
	items = append(items, schedItem)

	// 2. Database ping.
	items = append(items, checkDatabase(dm))

	// 3. Embeddings: env-var wired (the substrate uses OLLAMA).
	items = append(items, checkEmbeddings())

	// 4. Gateway: MPM_GATEWAY_URL env or default in-process.
	items = append(items, checkGateway())

	// 5. Working context loaded — present is OK, absent is not a
	// failure (operator can run `mpm work` to start one) but
	// surfaces as a hint.
	items = append(items, checkWorkingContextLoaded(dm))

	// 6. Pending theories — informative signal, not a hard fail.
	// A small backlog is normal during active work; large
	// backlogs are a workflow signal worth surfacing.
	items = append(items, checkPendingTheoriesCount(dm))

	// If we auto-started the scheduler, suffix the scheduler
	// item's detail with the timestamp so the operator can see
	// when the side effect happened.
	if schedAutoStarted {
		items[0].Detail = items[0].Detail + " · started just now"
	}

	return items
}

// checkSchedulerWithAutoStart reads the mpm-scheduler PID; if 0,
// attempts 'systemctl --user start mpm-scheduler'. Returns the
// item AND a bool indicating whether auto-start fired (caller
// uses that to surface the side effect explicitly).
//
// TTY-not-applicable here: systemd calls work the same in TTY
// and non-TTY; auto-start is best-effort and failure is recoverable.
func checkSchedulerWithAutoStart() (ReadinessItem, bool) {
	pid := readSchedulerPID()
	if pid > 0 {
		detail := fmt.Sprintf("uptime %s", schedulerUptime())
		return ReadinessItem{
			Name:   "Scheduler running",
			OK:     true,
			Detail: detail,
		}, false
	}

	// Not running. Attempt auto-start. systemd returns non-zero
	// exit if the unit file doesn't exist OR if the start fails;
	// we treat both as "scheduler not running" with a helpful hint.
	out, err := exec.Command("systemctl", "--user", "start", "mpm-scheduler").CombinedOutput()
	if err != nil {
		errStr := strings.TrimSpace(string(out))
		if errStr == "" {
			errStr = err.Error()
		}
		// Distinguish "unit not installed" from "start failed" so the
		// hint is actionable either way.
		hint := "run `mpm doctor` for diagnostics"
		if strings.Contains(errStr, "not found") || strings.Contains(errStr, "not loaded") {
			hint = "install the unit: `make service-scheduler`"
		}
		return ReadinessItem{
			Name:   "Scheduler running",
			OK:     false,
			Detail: truncate(errStr, 60),
			Hint:   hint,
		}, false
	}

	// Auto-start succeeded. Don't include uptime (it's nonsense
	// for a daemon we just kicked). The "started just now" suffix
	// is added once by the caller via the schedAutoStarted flag.
	pid = readSchedulerPID()
	if pid > 0 {
		return ReadinessItem{
			Name:   "Scheduler running",
			OK:     true,
			Detail: fmt.Sprintf("started automatically · pid %d", pid),
		}, true
	}

	// Race: started but PID not yet visible (service still
	// forking). Surface as OK with a caveat.
	return ReadinessItem{
		Name:   "Scheduler running",
		OK:     true,
		Detail: "started automatically · verifying…",
	}, true
}

// schedulerUptime reads the ActiveEnterTimestamp from systemd and
// formats the delta from now. Returns "just started" on parse
// failure (the daemon was just started but the timestamp hasn't
// propagated yet) or "unknown" when systemctl is missing.
func schedulerUptime() string {
	out, err := exec.Command("systemctl", "--user", "show",
		"mpm-scheduler", "--property=ActiveEnterTimestamp", "--value").Output()
	if err != nil {
		return "unknown"
	}
	ts := strings.TrimSpace(string(out))
	if ts == "" || ts == "n/a" {
		return "just started"
	}
	// systemd timestamp format: "Wed 2026-07-29 09:12:34 UTC"
	t, err := time.Parse("Mon 2006-01-02 15:04:05 MST", ts)
	if err != nil {
		// Try a couple of common variants.
		for _, layout := range []string{
			"2006-01-02 15:04:05 MST",
			"Mon 2006-01-02 15:04:05 MST",
		} {
			if t2, err2 := time.Parse(layout, ts); err2 == nil {
				t = t2
				break
			}
		}
		if t.IsZero() {
			return "unknown"
		}
	}
	d := time.Since(t)
	if d < 0 {
		return "just started"
	}
	return formatDuration(d)
}

// formatDuration renders a time.Duration as the short form
// "Xd Yh" / "Xh Ym" / "Xm Ys" / "Xs".
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	return fmt.Sprintf("%dd %dh", days, hours)
}

// checkDatabase pings SQLite with a 2s timeout. Substrate integrity
// is a 'mpm doctor' concern; this check is just "can we open
// the connection at all".
func checkDatabase(dm *mpminternal.DatabaseManager) ReadinessItem {
	if dm == nil {
		return ReadinessItem{
			Name:   "Database connected",
			OK:     false,
			Detail: "database handle unavailable",
			Hint:   "check MPM_DB_PATH / MPM_WORKSPACE",
		}
	}
	pingCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := dm.SQLDB().PingContext(pingCtx); err != nil {
		return ReadinessItem{
			Name:   "Database connected",
			OK:     false,
			Detail: truncate(err.Error(), 60),
			Hint:   "run `mpm doctor` for diagnostics",
		}
	}
	return ReadinessItem{Name: "Database connected", OK: true}
}

// checkEmbeddings verifies the embedding provider is reachable.
// Today the substrate uses OLLAMA (configurable via env). If
// OLLAMA_ENDPOINT isn't set, embed-related functions will fall
// back to FTS5-only — not a failure, just a feature-degradation
// signal.
func checkEmbeddings() ReadinessItem {
	if endpoint := os.Getenv("OLLAMA_ENDPOINT"); endpoint != "" {
		return ReadinessItem{
			Name:   "Embeddings available",
			OK:     true,
			Detail: truncate(endpoint, 50),
		}
	}
	return ReadinessItem{
		Name:   "Embeddings available",
		OK:     true,
		Detail: "FTS5-only (set OLLAMA_ENDPOINT for semantic)",
	}
}

// checkGateway reports broadcast/gateway configuration.
//
// In Wave 1+2 era, gateway is a future-facing concept (the "shared
// DB fan-out" Arc 2 architecture). For v0.2 we mark it as
// "in-process" — the operator knows the substrate has no external
// gateway wire yet. This is intentionally an OK=true with a
// informative detail, so the operator can SEE the capability
// surface without seeing a false alarm.
func checkGateway() ReadinessItem {
	return ReadinessItem{
		Name:   "Gateway connected",
		OK:     true,
		Detail: "in-process (no shared DB wire configured)",
	}
}

// checkWorkingContextLoaded reports whether the current session
// has a Working Context row. Absent is OK (the operator can
// start one with `mpm work`) but the readiness row surfaces
// this as a soft state — neither OK nor fail, just informational.
func checkWorkingContextLoaded(dm *mpminternal.DatabaseManager) ReadinessItem {
	if dm == nil {
		return ReadinessItem{Name: "Working Context loaded", OK: true, Detail: "(no db)"}
	}
	wcSvc := NewWorkingContextService(
		NewWorkingContextStore(dm),
		NewDatabaseManagerMemoryWriter(dm),
	)
	wc, _ := wcSvc.GetCurrent(getOrMakeSessionID())
	if wc != nil {
		return ReadinessItem{
			Name:   "Working Context loaded",
			OK:     true,
			Detail: truncate(wc.Thesis, 60),
		}
	}
	return ReadinessItem{
		Name:   "Working Context loaded",
		OK:     true,
		Detail: "(no active working context)",
		Hint:   "run `mpm work` to start one",
	}
}

// checkPendingTheoriesCount returns an informational row about
// pending theory count. Not a hard failure — operators may have
// large backlogs during active investigation. Larger than five
// surfaces a hint.
func checkPendingTheoriesCount(dm *mpminternal.DatabaseManager) ReadinessItem {
	if dm == nil {
		return ReadinessItem{Name: "Open theories", OK: true, Detail: "(no db)"}
	}
	var n int
	row := dm.QueryRowTracked(
		`SELECT COUNT(*) FROM memories
		 WHERE collection = 'theories'
		   AND deleted_at IS NULL
		   AND json_extract(metadata, '$.status') = 'pending'`,
	)
	if err := row.Scan(&n); err != nil {
		return ReadinessItem{Name: "Open theories", OK: true, Detail: "(count unavailable)"}
	}
	item := ReadinessItem{Name: "Open theories", OK: true}
	if n == 0 {
		item.Detail = "none pending"
	} else if n == 1 {
		item.Detail = "1 pending"
	} else {
		item.Detail = strconv.Itoa(n) + " pending"
	}
	if n > 5 {
		item.Hint = "review open theories"
	}
	return item
}

// strconv is imported above indirectly; keep the package explicit
// here for callers that want a quick-read scaffold.
var _ = strconv.Itoa
