package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// round 9 T46-T48: extend the `mpm ops confidence` subcommand to
// expose `explain` and `history` as thin CLI adapters over the
// underlying mpm_confidence functionality (the substrate tool at
// internal/core/tools handles action=explain|history directly). The
// pre-fix surface offered only show/recompute/changes/trend — a
// smoke test running `mpm ops confidence explain <id>` got "unknown
// subcommand" rather than the parity-with-mpm_call surface. The fix
// routes explain/history through the same actions the MCP/CALL
// surface accepts and updates the usage strings/help to advertise
// the full surface.
func parseOpsConfidenceArgs(args []string) (map[string]interface{}, string, error) {
	if len(args) == 0 {
		return nil, "", fmt.Errorf("subcommand required: show|recompute|changes|trend|explain|history")
	}
	sub := args[0]
	switch sub {
	case "show", "recompute", "changes", "trend", "explain", "history":
	default:
		return nil, "", fmt.Errorf("unknown subcommand: %q (want show|recompute|changes|trend|explain|history)", sub)
	}
	fs := flag.NewFlagSet("ops-confidence-"+sub, flag.ContinueOnError)
	artifactID := fs.String("artifact", "", "artifact id (required)")
	artifactType := fs.String("artifact-type", "memory", "artifact type")
	limit := fs.Int("limit", 10, "history rows to show (show|history only)")
	windowDays := fs.Int("window-days", 30, "trend window in days (trend only)")
	if err := fs.Parse(args[1:]); err != nil {
		return nil, "", err
	}
	if *artifactID == "" {
		return nil, "", fmt.Errorf("--artifact is required")
	}
	payload := map[string]interface{}{
		"artifact_id":   *artifactID,
		"artifact_type": *artifactType,
		"limit":         *limit,
		"window_days":   *windowDays,
	}
	return payload, sub, nil
}

// printOpsConfidenceHelp renders the `mpm ops confidence` surface. The
// canonical machine surface remains `mpm call mpm_confidence`; this help
// keeps the friendly alias discoverable without duplicating the engine.
func printOpsConfidenceHelp() {
	fmt.Print(`mpm ops confidence — Confidence / evidence engine (friendly alias)

Usage:
  mpm ops confidence <subcommand> --artifact <id> [--artifact-type memory] [--limit N] [--window-days N]

Subcommands:
  show        Current confidence snapshot for an artifact
  recompute   Recompute confidence from evidence now
  changes     Recent confidence changes (last 24h)
  trend       Confidence trend over --window-days (default 30)
  explain     Evidence breakdown + reasoning trace
  history     Full confidence_history timeline

Canonical machine surface:
  mpm call mpm_confidence --payload '{"action":"show","params":{"artifact_id":"<id>"}}'

Examples:
  mpm ops confidence show --artifact abc123
  mpm ops confidence explain --artifact abc123
  mpm ops confidence history --artifact abc123 --limit 20
`)
}

func handleOpsConfidence(args []string) int {
	// R3: --help / -h / "help" short-circuit. Without this, `mpm ops
	// confidence --help` fell into parseOpsConfidenceArgs as sub="--help"
	// ("unknown subcommand") and `mpm ops confidence show --help`
	// surfaced the misleading "--artifact is required". Same defect
	// class fixed on the memory/topic/lesson leaves; the ops surface
	// was missed. Rough-edge closure 2026-09-12 (item 6).
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "help" {
			printOpsConfidenceHelp()
			return 0
		}
	}
	payload, sub, err := parseOpsConfidenceArgs(args)
	if err != nil {
		printError("%v", err)
		return 1
	}
	dm := getDBConcrete()
	if dm == nil {
		return 1
	}

	artifactID, _ := payload["artifact_id"].(string)
	artifactType, _ := payload["artifact_type"].(string)

	var res interface{}
	switch sub {
	case "show":
		limit, _ := payload["limit"].(int)
		snap, err := mpminternal.GetConfidenceForArtifact(dm, artifactID, artifactType, limit)
		if err != nil {
			printError("ops confidence show: %v", err)
			return 1
		}
		res = snap
	case "recompute":
		if err := mpminternal.RecomputeConfidence(dm, artifactID, artifactType, mpminternal.RecomputeReasonManual); err != nil {
			printError("ops confidence recompute: %v", err)
			return 1
		}
		res = map[string]interface{}{"success": true, "artifact_id": artifactID}
	case "changes":
		limit, _ := payload["limit"].(int)
		filter := mpminternal.ConfidenceChangesFilter{
			Since:        time.Now().Add(-24 * time.Hour),
			Limit:        limit,
			ArtifactID:   artifactID,
			ArtifactType: artifactType,
		}
		changes, err := mpminternal.QueryConfidenceChanges(dm, filter)
		if err != nil {
			printError("ops confidence changes: %v", err)
			return 1
		}
		res = map[string]interface{}{"changes": changes, "count": len(changes)}
	case "trend":
		windowDays, _ := payload["window_days"].(int)
		trend, err := mpminternal.QueryConfidenceTrend(dm, artifactID, artifactType, windowDays)
		if err != nil {
			printError("ops confidence trend: %v", err)
			return 1
		}
		res = trend
	case "explain":
		// T47: route through the substrate confidence engine so the
		// CLI surface stays in parity with `mpm call mpm_confidence
		// --payload '{"action":"explain",...}'`. The handler returns
		// the reasoning trace (evidence breakdown + recompute trace)
		// for the artifact.
		explain, err := dm.ExplainConfidence(artifactID, artifactType)
		if err != nil {
			printError("ops confidence explain: %v", err)
			return 1
		}
		res = explain
	case "history":
		// T48: history is the full confidence_history timeline. The
		// underlying tool accepts `action=history`; we go through
		// the canonical QueryConfidenceHistory so a CLI call and
		// `mpm call mpm_confidence --payload '{"action":"history"}'`
		// return identical shapes.
		limit, _ := payload["limit"].(int)
		history, err := dm.QueryConfidenceHistory(artifactID, artifactType, limit)
		if err != nil {
			printError("ops confidence history: %v", err)
			return 1
		}
		res = history
	}
	out, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(out))
	return 0
}
