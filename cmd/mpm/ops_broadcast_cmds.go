// ops_broadcast_cmds.go — `mpm ops broadcast` engine-room command.
//
// Arc 2's operator surface. Fans out one row in shared.event_wakes
// per active session (within the 24h heartbeat window), so receiving
// agents see the new rule/resolution in their wake context the next
// time they call any MPM tool.
//
// Locked design choices (v 2026-07-07):
//   - Topology: heartbeat-discovered via shared.sessions.
//   - Dedup: deterministic sha256 prefix as wake_id (PRIMARY KEY)
//     + INSERT OR IGNORE for O(1) no-op re-broadcasts.
//   - Self-skip: the broadcasting session is NOT in the target list.
//   - Payload: must include rationale (kind=resolution/arbitration
//     auto-extracts; everything else requires --rationale).
//   - Auto-broadcast: only as a strict side-effect of an explicit
//     operator command (record_global_rule --confirm,
//     resolve-contradictions --apply, resolve-theory --winner).
//     Internal agent writes do NOT trigger broadcasts.
//
// Operator's-eye view:
//   mpm ops broadcast <memory_id>                           # full fan-out
//   mpm ops broadcast <memory_id> --kind=rule              # explicit kind
//   mpm ops broadcast <memory_id> --rationale="..."        # for non-auto kinds
//   mpm ops broadcast <memory_id> --to=808,claude          # restricted fan-out
//   mpm ops broadcast <memory_id> --dry-run                # compute targets, no inserts
//   mpm ops broadcast <memory_id> --json                   # machine-readable
//   mpm ops active-sessions                                # see who's listening

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// handleOpsBroadcast dispatches both `mpm ops broadcast` and
// handleOpsBroadcast handles `mpm ops broadcast <memory_id> ...`.
// The router already stripped the "broadcast" subcommand; args here
// is everything AFTER it: <memory_id>, --kind, --rationale, --to,
// --dry-run, --json (in any order).
func handleOpsBroadcast(args []string) int {
	dryRun := false
	jsonOut := false
	var positional []string
	for _, a := range args {
		switch a {
		case "--dry-run":
			dryRun = true
		case "--json":
			jsonOut = true
		case "--help", "-h", "help":
			fmt.Println("Usage: mpm ops broadcast <memory_id> [--kind=rule|resolution|arbitration] [--rationale=\"...\"] [--to=agent_id,...] [--dry-run] [--json]")
			fmt.Println("")
			fmt.Println("Arc 2 fan-out. Broadcasts a memory to every active session (or a --to list).")
			fmt.Println("Receiving agents see the wake in their WakesPending block on the next MPM call.")
			fmt.Println("")
			fmt.Println("Flags:")
			fmt.Println("  --kind=...        Override the auto-detected kind (default: collection name).")
			fmt.Println("  --rationale=...   The WHY. Required unless kind=resolution or arbitration (auto-extracted).")
			fmt.Println("  --to=a,b,c        Restrict fan-out to these agent_ids. Refuses if any target is offline.")
			fmt.Println("  --dry-run         Compute the target list + payload, but don't INSERT.")
			fmt.Println("  --json            Machine-readable output.")
			return 0
		default:
			positional = append(positional, a)
		}
	}
	return doOpsBroadcast(positional, dryRun, jsonOut)
}

// handleOpsActiveSessions handles `mpm ops active-sessions [--json]`.
// The router already stripped the "active-sessions" subcommand.
func handleOpsActiveSessions(args []string) int {
	jsonOut := false
	for _, a := range args {
		switch a {
		case "--json":
			jsonOut = true
		case "--help", "-h", "help":
			fmt.Println("Usage: mpm ops active-sessions [--json]")
			fmt.Println("")
			fmt.Println("List every session active within the heartbeat window (default 24h).")
			fmt.Println("Useful as the operator's preview before `mpm ops broadcast`.")
			return 0
		default:
			_ = a // be lenient
		}
	}
	return doOpsActiveSessions(jsonOut)
}

// doOpsBroadcast runs the actual broadcast. Args is positional[1:]
// (everything after `broadcast`).
//
// Flag parsing for --kind, --rationale, --to is inline (not via
// router.parseFlags) because these flags have = values AND are
// optional. The router's flag parser doesn't support = values;
// it treats the whole --kind=rule token as a single argument.
func doOpsBroadcast(args []string, dryRun, jsonOut bool) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usererror.Error("missing memory_id"))
		return 1
	}
	memoryID := args[0]

	var kind, rationale string
	var toAgents []string
	for _, a := range args[1:] {
		switch {
		case strings.HasPrefix(a, "--kind="):
			kind = strings.TrimPrefix(a, "--kind=")
		case strings.HasPrefix(a, "--rationale="):
			rationale = strings.TrimPrefix(a, "--rationale=")
		case strings.HasPrefix(a, "--to="):
			raw := strings.TrimPrefix(a, "--to=")
			for _, ag := range strings.Split(raw, ",") {
				if ag = strings.TrimSpace(ag); ag != "" {
					toAgents = append(toAgents, ag)
				}
			}
		default:
			fmt.Fprintln(os.Stderr, usererror.Errorf(fmt.Sprintf("unknown flag %q", a)))
			return 1
		}
	}

	dm := getDB()
	report, err := dm.BroadcastMemory(memoryID, mpminternal.BroadcastOpts{
		Kind:           kind,
		Rationale:      rationale,
		ToAgents:       toAgents,
		DryRun:         dryRun,
		SourceAgent:    resolveAgentID(),
		SourceSessionID: getOrMakeSessionID(),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, usererror.Errorf(err.Error()))
		return 1
	}

	if jsonOut {
		b, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(b))
		return 0
	}

	// Human-readable output.
	if dryRun {
		fmt.Println("DRY RUN: no rows inserted.")
	}
	fmt.Printf("Memory:   %s\n", report.MemoryID)
	fmt.Printf("Kind:     %s\n", report.Kind)
	fmt.Printf("Hash:     %s\n", report.ContentHash)
	fmt.Printf("Rationale: %s\n", report.Rationale)
	fmt.Printf("Targets:  %d (new=%d, deduped=%d)\n\n",
		len(report.Targets), report.NewWakes, report.DedupedWakes)
	for _, t := range report.Targets {
		fmt.Printf("  [%s] %s (agent=%s) wake_id=%s\n",
			t.Status, t.SessionID, t.AgentID, t.WakeID)
	}
	return 0
}

// doOpsActiveSessions lists every session within the heartbeat
// window. The operator's "who would I broadcast to?" companion to
// `mpm ops broadcast`.
func doOpsActiveSessions(jsonOut bool) int {
	dm := getDB()
	sessions, err := dm.DiscoverActiveSessions()
	if err != nil {
		fmt.Fprintln(os.Stderr, usererror.Errorf(err.Error()))
		return 1
	}

	if jsonOut {
		b, _ := json.MarshalIndent(map[string]interface{}{
			"active_sessions": sessions,
			"count":           len(sessions),
			"window_hours":    mpminternal.HeartbeatWindowHours,
		}, "", "  ")
		fmt.Println(string(b))
		return 0
	}

	fmt.Printf("Active sessions in last %dh (heartbeat window):\n\n", mpminternal.HeartbeatWindowHours)
	if len(sessions) == 0 {
		fmt.Println("  (none — no agents have hit the shared DB recently)")
		return 0
	}
	for _, s := range sessions {
		fmt.Printf("  %s (agent=%s) host=%s last_heartbeat=%s\n",
			s.SessionID, s.AgentID, s.Hostname, s.LastHeartbeat)
	}
	return 0
}