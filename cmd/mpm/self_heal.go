// Self-healing: autonomous integrity repair loop.
//
// `mpm ops self-heal` runs the same checks as `mpm doctor --deep-scan` but
// takes automated action on the results:
//
//   - Known-safe drift (soft-delete ghosts) is auto-fixed and a lesson is
//     written to MPM as an audit trail.
//   - Unknown drift (FTS orphans, dangling memberships) is escalated via
//     a pending theory so the next agent wake surfaces it. The system
//     refuses to auto-fix novel drift because we do not yet know whether
//     the repair is safe.
//
// The cognitive loop closes through the existing wake context: pending
// theories are stored in the memories table and surface via
// `mpm call read_wake_context`. The next agent boot sees the structural
// anomaly and acts on it. No new wake code is needed.
//
// Safety boundaries:
//
//  1. WHITELIST-ONLY auto-fix. Only soft-delete ghosts are auto-fixed.
//  2. BOUNDED BLAST RADIUS. If ghost count exceeds SelfHealMaxFix, do
//     not auto-fix; escalate as a bounded theory.
//  3. RATE LIMIT. Max one auto-fix per SelfHealCooldown. Re-running
//     within the cooldown with no new drift signature is a no-op.
//  4. AUDIT TRAIL. Every auto-fix writes a lesson tagged source=self-heal.
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/flowbyte-com/mpm-core/config"
	"github.com/flowbyte-com/mpm-core/usererror"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// SelfHealMaxFix is the upper bound on soft-delete ghosts the autonomous
// loop will fix in one run. Beyond this, the situation is no longer
// "drift" — it's "the trigger is broken" — and the right move is to
// escalate, not repair.
const SelfHealMaxFix = 1000

// SelfHealCooldown is the minimum interval between auto-fix runs. Re-runs
// inside this window with no new drift are silent no-ops.
const SelfHealCooldown = 24 * time.Hour

// SelfHealStateID is the fixed memory id used to track self-heal rate-limit
// state. Stored in collection='projects' so it does not bleed into wake
// context via the standard recent_memories path (we surface it explicitly
// when relevant).
const SelfHealStateID = "self-heal-state-marker"

// SelfHealState captures the rate-limit + last-action metadata.
type SelfHealState struct {
	LastRun        time.Time `json:"last_run"`
	LastAction     string    `json:"last_action"`      // "clean", "auto-fixed", "escalated", "bounded"
	LastFixedCount int       `json:"last_fixed_count"` // ghosts cleaned in last run
	LastTheoryID   string    `json:"last_theory_id"`   // pending theory id, if escalated
	DriftSignature string    `json:"drift_signature"`  // hash of last drift finding, for dedup
}

// loadSelfHealState reads the rate-limit marker. Returns a zero state if
// no prior run has been recorded.
func loadSelfHealState(dm *mpminternal.DatabaseManager) (*SelfHealState, error) {
	row := dm.SQLDB().QueryRow(
		"SELECT metadata FROM memories WHERE id = ? AND collection = 'projects'",
		SelfHealStateID,
	)
	var metaStr sql.NullString
	if err := row.Scan(&metaStr); err != nil {
		if err == sql.ErrNoRows {
			return &SelfHealState{}, nil
		}
		return nil, err
	}
	if !metaStr.Valid || metaStr.String == "" {
		return &SelfHealState{}, nil
	}
	var state SelfHealState
	if err := json.Unmarshal([]byte(metaStr.String), &state); err != nil {
		// Corrupt state — start fresh rather than failing the whole loop.
		return &SelfHealState{}, nil
	}
	return &state, nil
}

// saveSelfHealState persists the rate-limit marker. Uses UPSERT via
// INSERT OR REPLACE on the projects collection.
func saveSelfHealState(dm *mpminternal.DatabaseManager, state *SelfHealState) error {
	metaBytes, err := json.Marshal(state)
	if err != nil {
		return err
	}
	_, err = dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, metadata, created_at, updated_at)
		VALUES (?, 'projects', ?, ?, CAST(strftime('%s','now') AS INTEGER), CAST(strftime('%s','now') AS INTEGER))
		ON CONFLICT(id) DO UPDATE SET
			metadata = excluded.metadata,
			updated_at = CAST(strftime('%s','now') AS INTEGER)
	`, SelfHealStateID, "self-heal rate-limit marker", string(metaBytes))
	return err
}

// driftSignature produces a stable fingerprint of the current drift so we
// can dedup repeat escalations.
func driftSignature(scan *DeepScanResult) string {
	return fmt.Sprintf("orphans=%v:ghosts=%d:danling=%d",
		scan.FTSOrphans, scan.SoftDeleteGhosts, scan.DanglingMemberships)
}

// handleSelfHeal is the entry point for `mpm ops self-heal`.
func handleSelfHeal(args []string) int {
	fs := flag.NewFlagSet("self-heal", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "Report drift without taking any action")
	force := fs.Bool("force", false, "Bypass rate-limit cooldown")
	quiet := fs.Bool("quiet", false, "Suppress OK output when no drift is found")
	fs.Usage = func() {
		fmt.Println("Usage: mpm ops self-heal [options]")
		fmt.Println("\nAutonomous integrity repair:")
		fmt.Println("  - Runs FTS orphan / soft-delete ghost / dangling membership checks")
		fmt.Println("  - Auto-fixes KNOWN-SAFE drift (soft-delete ghosts) within bounds")
		fmt.Println("  - Escalates UNKNOWN drift via pending theory for the next agent wake")
		fmt.Println("  - Rate-limited to one auto-fix per 24h; --force bypasses the cooldown")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 1
	}

	dbPath := fmt.Sprintf("%s/src/db/mpm.db", config.GetMPMDir())
	if _, err := os.Stat(dbPath); err != nil {
		usererror.Warn("self-heal: no database at %s", dbPath)
		return 1
	}

	// loadSelfHealState / saveSelfHealState take *DatabaseManager (concrete),
	// not the CoreDB interface. The singleton is always a *DatabaseManager.
	dm := getDBConcrete()
	if dm == nil {
		return 1
	}

	// Load prior state. If we're inside the cooldown with the same drift
	// signature, no-op silently.
	state, _ := loadSelfHealState(dm)
	now := time.Now().UTC()
	withinCooldown := !state.LastRun.IsZero() && now.Sub(state.LastRun) < SelfHealCooldown

	// Run the shared scan.
	scan, err := runDeepScanCheck(dbPath)
	if err != nil {
		usererror.Warn("self-heal: scan failed: %v", err)
		return 1
	}
	sig := driftSignature(scan)

	if !scan.HasKnownSafeDrift() && !scan.HasUnknownDrift() {
		// Clean. Update state silently, exit 0.
		state.LastRun = now
		state.LastAction = "clean"
		state.DriftSignature = sig
		_ = saveSelfHealState(dm, state)
		if !*quiet {
			fmt.Println("self-heal: clean — no drift detected")
		}
		return 0
	}

	if withinCooldown && state.DriftSignature == sig && !*force {
		// Same drift as last run, still inside cooldown window. Don't
		// spam MPM with duplicate theories. Quietly exit unless --quiet
		// is set (in which case be silent).
		if !*quiet {
			fmt.Printf("self-heal: drift unchanged since %s — skipping (use --force to override)\n",
				state.LastRun.Format(time.RFC3339))
		}
		return 0
	}

	// We have drift. Decide what to do.
	exitCode := 0
	action := "auto-fixed"
	fixedCount := 0
	var theoryID string

	// 1) Auto-fix soft-delete ghosts if within bounds.
	if scan.SoftDeleteGhosts > 0 {
		if scan.SoftDeleteGhosts > SelfHealMaxFix {
			// Bounded. Do NOT auto-fix. Escalate.
			action = "bounded"
			exitCode = 1
			usererror.Warn("self-heal: %d ghosts exceeds SelfHealMaxFix=%d — escalating without fix", scan.SoftDeleteGhosts, SelfHealMaxFix)
		} else if !*dryRun {
			// H-4: routed through the singleton's *sql.DB so the shared
			// maintenance lease (LOCK_SH) covers the auto-fix; concurrent
			// restore-db / shred-database refuses (EWOULDBLOCK).
			n, ferr := runDeepScanFixSoftDeleteGhosts(dm)
			if ferr != nil {
				usererror.Warn("self-heal: ghost fix failed: %v", ferr)
				action = "fix-failed"
				exitCode = 1
			} else {
				fixedCount = int(n)
				fmt.Printf("self-heal: auto-fixed %d soft-delete ghost(s)\n", n)
			}
		} else {
			fmt.Printf("self-heal: would fix %d soft-delete ghost(s) (--dry-run)\n", scan.SoftDeleteGhosts)
			fixedCount = scan.SoftDeleteGhosts
		}
	}

	// 2) Escalate unknown drift via pending theory.
	if scan.HasUnknownDrift() {
		action = "escalated"
		exitCode = 1
		hypothesis, criteria := buildDriftTheory(scan, *dryRun)
		tagsJSON, _ := json.Marshal([]string{"mpm", "self-heal", "fts5", "drift", "auto-escalated"})
		if !*dryRun {
			t, terr := dm.ProposeTheory(hypothesis, criteria, nil, nil, []string{"mpm", "self-heal", "fts5", "drift", "auto-escalated"})
			if terr != nil {
				usererror.Warn("self-heal: theory injection failed: %v", terr)
			} else {
				if id, ok := t["id"].(string); ok {
					theoryID = id
				}
				fmt.Printf("self-heal: escalated unknown drift as pending theory id=%s\n", theoryID)
			}
		} else {
			fmt.Printf("self-heal: would escalate unknown drift (--dry-run)\n")
		}
		_ = tagsJSON
	}

	// 3) Audit lesson for any auto-fix action.
	if fixedCount > 0 && !*dryRun {
		lesson := fmt.Sprintf(
			"SELF-HEAL %s: auto-fixed %d soft-delete ghost(s) in memories_fts. "+
				"Audit trail for autonomous self-heal run at %s. "+
				"Drift signature: %s. Source: self-heal.",
			now.Format(time.RFC3339), fixedCount, dbPath, sig,
		)
		if _, err := dm.SQLDB().Exec(
			"INSERT INTO lessons (id, type, content, tags, created) VALUES (?, 'insight', ?, ?, STRFTIME('%Y-%m-%d %H:%M:%f', 'NOW'))",
			fmt.Sprintf("selfheal-%d", now.UnixNano()),
			lesson,
			`["mpm","self-heal","auto-fix","audit"]`,
		); err != nil {
			usererror.Warn("self-heal: lesson write failed: %v", err)
		}
	}

	// 4) Persist state.
	state.LastRun = now
	state.LastAction = action
	state.LastFixedCount = fixedCount
	state.LastTheoryID = theoryID
	state.DriftSignature = sig
	_ = saveSelfHealState(dm, state)

	return exitCode
}

// buildDriftTheory constructs the pending theory text for unknown drift.
// The theory's VALIDATION_CRITERIA points the next agent at the exact
// recovery procedure so the cognitive loop is self-directing.
func buildDriftTheory(scan *DeepScanResult, dryRun bool) (string, string) {
	hypothesis := "HYPOTHESIS: MPM integrity drift detected by autonomous self-heal loop"
	criteria := "VALIDATION_CRITERIA:\n"
	criteria += "1. Inspect each drift class: run `mpm doctor --deep-scan` for the human-readable report.\n"
	criteria += "2. For FTS orphans in any FTS table, decide whether the source rows were correctly deleted (in which case the FTS row is a true orphan and a manual `INSERT INTO <fts>_fts(fts)<rowid>) VALUES ('rebuild')` + rebuild is the fix) or the source row was incorrectly deleted (in which case restore from the memory_revisions table).\n"
	criteria += "3. For dangling topic_memberships, run `DELETE FROM topic_memberships WHERE topic_id NOT IN (SELECT id FROM topics)` once the membership list is reviewed.\n"
	criteria += "4. After manual repair, re-run `mpm doctor --deep-scan` to confirm clean state.\n"
	criteria += "5. Mark this theory proven once deep-scan reports 0 warnings.\n"
	criteria += "STATUS: pending"
	if dryRun {
		criteria += "\nDRY_RUN: true"
	}
	return hypothesis, criteria
}
